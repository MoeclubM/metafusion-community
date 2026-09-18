package auth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/MoeclubM/metafusion-community/internal/config"
)

// Principal 是验签后的调用者身份。只信令牌里声明的这几项信息；
// 具体能编辑、能下载什么由各服务自己按业务规则判断（一律走 Can，不比较角色字符串）。
type Principal struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	// Groups/Permissions 是账号服务的权限组投影，字段名与签发侧逐字一致
	// （令牌 claims 与 /api/auth/me 都叫 groups / permissions）。
	Groups      []string `json:"groups"`
	Permissions []string `json:"permissions"`
	// FromPAT 标记身份来自 PAT 内省（而不是签发的 JWT）。它参与授权判定
	// （见 permission.go：PAT 身份永不回落角色兜底），不进 JSON 输出、不暴露给调用方。
	FromPAT bool `json:"-"`
}

// SessionResolver 是存量令牌的兜底：用户可能还持有登录时发的不透明会话令牌（不是 JWT）。
// 解析**必须问账号服务**（会话表在它那里）；本服务不查任何人的库。
type SessionResolver interface {
	Resolve(ctx context.Context, bearer, cookie string) (*Principal, bool)
}

// SessionClient 是与账号服务约定的兜底解析实现：把原样的 Bearer/Cookie 转给
// `GET /api/auth/me`，由账号服务验签或查会话表后返回身份。
// 账号服务是唯一身份来源——目录服务不参与身份判定，因此这里不指向 CATALOG_URL。
type SessionClient struct {
	base string
	http *http.Client
}

func NewSessionClient(baseURL string, timeout time.Duration) *SessionClient {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &SessionClient{base: strings.TrimRight(baseURL, "/"), http: &http.Client{Timeout: timeout}}
}

func (c *SessionClient) Resolve(ctx context.Context, bearer, cookie string) (*Principal, bool) {
	if c.base == "" || (bearer == "" && cookie == "") {
		return nil, false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/auth/me", nil)
	if err != nil {
		return nil, false
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "mf_session", Value: cookie})
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var user struct {
		ID          string   `json:"id"`
		Username    string   `json:"username"`
		Role        string   `json:"role"`
		Groups      []string `json:"groups"`
		Permissions []string `json:"permissions"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&user); err != nil || user.ID == "" {
		return nil, false
	}
	// 权限组随 /api/auth/me 一起下发，缺字段（旧账号服务）时为空：判定层据此退回角色兜底。
	return &Principal{
		ID: user.ID, Username: user.Username, Role: user.Role,
		Groups: user.Groups, Permissions: user.Permissions,
	}, true
}

// Verifier 只做一件事：把请求换算成身份。
// 公钥来源优先取静态配置，其次按 JWKS 地址拉取并缓存（未知 kid 会触发一次强制刷新）。
type Verifier struct {
	issuer   string
	audience string
	jwksURL  string
	static   *rsa.PublicKey
	client   *http.Client
	fallback SessionResolver
	// pat 是个人访问令牌的内省器（见 pat.go）。为 nil（未配置 AUTH_URL）时，
	// 带 mfp_ 前缀的请求一律 503 auth_unavailable——身份只能问账号服务。
	pat *PATIntrospector

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

type claims struct {
	Username string `json:"preferred_username"`
	Role     string `json:"role"`
	// 权限组与权限码：与账号服务 store.Claims 的 json 名逐字一致，否则后台分配的
	// 权限组到了本服务就是空的（老令牌不带这两项，走 Can 的角色兜底）。
	Groups      []string `json:"groups"`
	Permissions []string `json:"permissions"`
	jwt.RegisteredClaims
}

const keyCacheTTL = 10 * time.Minute

func New(cfg config.Config) (*Verifier, error) {
	v := &Verifier{
		issuer:   cfg.JWTIssuer,
		audience: cfg.JWTAudience,
		jwksURL:  cfg.JWKSURL,
		client:   &http.Client{Timeout: 5 * time.Second},
		keys:     map[string]*rsa.PublicKey{},
	}
	if cfg.JWTPublicKeyPEM != "" {
		key, err := parsePublicKey(cfg.JWTPublicKeyPEM)
		if err != nil {
			return nil, err
		}
		v.static = key
	}
	return v, nil
}

// SetFallback 注入会话兜底解析器（nil 表示只接受 JWT）。
func (v *Verifier) SetFallback(r SessionResolver) { v.fallback = r }

// SetPAT 注入 PAT 内省器；nil 表示不接 PAT（带 mfp_ 的请求回 503 auth_unavailable）。
func (v *Verifier) SetPAT(p *PATIntrospector) { v.pat = p }

func parsePublicKey(raw string) (*rsa.PublicKey, error) {
	text := strings.TrimSpace(raw)
	if !strings.Contains(text, "BEGIN") {
		if decoded, err := base64.StdEncoding.DecodeString(text); err == nil {
			text = string(decoded)
		}
	}
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		return nil, errors.New("AUTH_JWT_PUBLIC_KEY must be PEM or base64 PEM")
	}
	if key, err := parseRSAPublic(block.Bytes); err == nil {
		return key, nil
	}
	// 私钥也接受：只取其公钥部分，方便与 catalog 共用同一份配置。
	priv, err := parseRSAPrivate(block.Bytes)
	if err != nil {
		return nil, errors.New("AUTH_JWT_PUBLIC_KEY must be an RSA key")
	}
	return &priv.PublicKey, nil
}

func parseRSAPublic(der []byte) (*rsa.PublicKey, error) {
	if parsed, err := x509.ParsePKIXPublicKey(der); err == nil {
		if key, ok := parsed.(*rsa.PublicKey); ok {
			return key, nil
		}
	}
	if key, err := x509.ParsePKCS1PublicKey(der); err == nil {
		return key, nil
	}
	return nil, errors.New("not an RSA public key")
}

func parseRSAPrivate(der []byte) (*rsa.PrivateKey, error) {
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if key, ok := parsed.(*rsa.PrivateKey); ok {
			return key, nil
		}
	}
	return nil, errors.New("not an RSA private key")
}

// Verify 校验令牌并返回身份；失败一律返回错误，调用方自己决定 401 与否。
func (v *Verifier) Verify(token string) (*Principal, error) {
	if token == "" {
		return nil, errors.New("missing token")
	}
	parsed, err := jwt.ParseWithClaims(token, &claims{}, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return v.publicKey(kid)
	}, jwt.WithValidMethods([]string{"RS256"}), jwt.WithIssuer(v.issuer), jwt.WithAudience(v.audience))
	if err != nil {
		return nil, err
	}
	c, ok := parsed.Claims.(*claims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid token")
	}
	if c.Subject == "" {
		return nil, errors.New("token without subject")
	}
	return &Principal{
		ID: c.Subject, Username: c.Username, Role: c.Role,
		Groups: c.Groups, Permissions: c.Permissions,
	}, nil
}

func (v *Verifier) publicKey(kid string) (*rsa.PublicKey, error) {
	if v.static != nil {
		return v.static, nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if key, ok := v.keys[kid]; ok && time.Since(v.fetchedAt) < keyCacheTTL {
		return key, nil
	}
	if err := v.refreshLocked(); err != nil {
		return nil, err
	}
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}
	// 轮换期间签名密钥可能刚换过：清空缓存再取一次。
	if err := v.refreshLocked(); err != nil {
		return nil, err
	}
	if key, ok := v.keys[kid]; ok {
		return key, nil
	}
	return nil, errors.New("unknown signing key")
}

func (v *Verifier) refreshLocked() error {
	if v.jwksURL == "" {
		return errors.New("no jwks url")
	}
	req, err := http.NewRequest(http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("jwks unavailable")
	}
	var doc struct {
		Keys []struct {
			KID string `json:"kid"`
			KTY string `json:"kty"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		if !strings.EqualFold(k.KTY, "RSA") {
			continue
		}
		key, err := rsaFromJWK(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.KID] = key
	}
	if len(keys) == 0 {
		return errors.New("jwks has no usable key")
	}
	v.keys = keys
	v.fetchedAt = time.Now()
	return nil
}

func rsaFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, err
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, errors.New("invalid exponent")
	}
	return &rsa.PublicKey{N: new(big.Int).SetBytes(nBytes), E: e}, nil
}

// Middleware 解析身份但不拦截：带令牌且有效时写入上下文，供需要时读取。
// PAT 路径例外：无效 PAT 401 invalid_token、账号服务不可达 503 auth_unavailable，
// 这两种情况必须在这里结束请求（放行成匿名会让调用方拿到语义错误的 401
// authentication_required，把"依赖故障"误报成"凭据错"）。
func (v *Verifier) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		bearer, cookie := credentials(c.Request)
		c.Request = c.Request.WithContext(WithCredentials(c.Request.Context(), bearer, cookie))
		p, status := v.resolve(c)
		if rejectPAT(c, status) {
			return
		}
		if p != nil {
			c.Set(principalKey, p)
		}
		c.Next()
	}
}

// Required 要求已登录：无有效身份直接 401（PAT 的失败语义见 Middleware）。
func (v *Verifier) Required() gin.HandlerFunc {
	return func(c *gin.Context) {
		bearer, cookie := credentials(c.Request)
		c.Request = c.Request.WithContext(WithCredentials(c.Request.Context(), bearer, cookie))
		p, status := v.resolve(c)
		if rejectPAT(c, status) {
			return
		}
		if p != nil {
			c.Set(principalKey, p)
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication_required"})
	}
}

// resolve 把请求换算成身份与终局状态。
//
// PAT（Authorization: Bearer mfp_...）**只走内省**，且不回落到 cookie：浏览器里可能同时存在
// 另一个用户的 mf_session，回落到它会把"机器身份"悄悄变成"浏览器登录身份"。
// JWT 与不透明会话令牌的路径完全不变：验签失败仍按匿名继续（fail closed），
// 由各端点的 Required/require 决定 401——账号服务挂在 /api/auth/me 兜底上时，
// 这里也不会把普通浏览器的匿名读请求变成 503。
func (v *Verifier) resolve(c *gin.Context) (*Principal, patStatus) {
	bearer, cookie := credentials(c.Request)
	if IsPAT(bearer) {
		ident, err := v.introspectPAT(c.Request.Context(), bearer)
		switch {
		case err != nil:
			return nil, patUnavailable
		case ident == nil:
			return nil, patInvalid
		}
		return ident, patOK
	}
	if p, err := v.Verify(bearer); err == nil {
		return p, patOK
	}
	if v.fallback != nil {
		if p, ok := v.fallback.Resolve(c.Request.Context(), bearer, cookie); ok {
			return p, patOK
		}
	}
	return nil, patOK
}

// introspectPAT 是 PAT 的内省入口：内省器未注入（未配置 AUTH_URL）时按不可用处理。
// 身份只能问账号服务，本服务不查 auth 库，也不在本地缓存明文。
func (v *Verifier) introspectPAT(ctx context.Context, token string) (*Principal, error) {
	if v == nil || v.pat == nil {
		return nil, errPATUnavailable
	}
	ident, err := v.pat.Introspect(ctx, token)
	if err != nil {
		return nil, err
	}
	if ident == nil {
		return nil, nil
	}
	return ident.principal(), nil
}

func credentials(r *http.Request) (string, string) {
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	bearer := ""
	if len(authz) > 7 && strings.EqualFold(authz[:7], "bearer ") {
		bearer = strings.TrimSpace(authz[7:])
	}
	cookie := ""
	if ck, err := r.Cookie("mf_session"); err == nil && ck.Value != "" {
		cookie = ck.Value
	}
	return bearer, cookie
}

const principalKey = "community_principal"

type credsKey struct{}

// WithCredentials 把请求携带的凭据写进 context：本服务调用目录服务时必须原样转发
// 调用者的令牌，否则可见性判定会退化成匿名，草稿/待审条目会被误判为不可见。
func WithCredentials(ctx context.Context, bearer, cookie string) context.Context {
	return context.WithValue(ctx, credsKey{}, [2]string{bearer, cookie})
}

// CredentialsFrom 读出调用者凭据；缺失即匿名（返回空串）。
func CredentialsFrom(ctx context.Context) (string, string) {
	if v, ok := ctx.Value(credsKey{}).([2]string); ok {
		return v[0], v[1]
	}
	return "", ""
}

// Current 返回当前请求者，未登录为 nil。
func Current(c *gin.Context) *Principal {
	if v, ok := c.Get(principalKey); ok {
		if p, ok := v.(*Principal); ok {
			return p
		}
	}
	return nil
}
