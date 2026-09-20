package auth

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/MoeclubM/metafusion-community/internal/config"
	"github.com/MoeclubM/metafusion-community/internal/upstream"
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
	// Scope/ClientID 是第三方 OAuth 授权的绑定（与签发侧对齐）：站内会话令牌恒不携带
	// 这两项（签发侧 Sign 会清零，会话用途以 token_use=session 为准，见 S01）。
	// 不进 JSON 输出、不暴露给调用方。
	Scope    string `json:"-"`
	ClientID string `json:"-"`
	// IsThirdParty 标记身份来自第三方 OAuth 授权（scope/client_id/token_use 任一非空）。
	// 管理 API 默认拒绝此类令牌（见 register.go 的 require），不进 JSON 输出。
	IsThirdParty bool `json:"-"`
	// PermissionsSet 标记令牌是否显式携带 permissions 声明（含空数组）：携带即以码为准，
	// 显式空集合不得回落角色；缺字段才是老令牌，走 Can 的历史边界兜底。不进 JSON 输出。
	PermissionsSet bool `json:"-"`
	// FromPAT 标记身份来自 PAT 内省（而不是签发的 JWT）。它参与授权判定
	// （见 permission.go：PAT 身份永不回落角色兜底），不进 JSON 输出、不暴露给调用方。
	FromPAT bool `json:"-"`
}

// SessionResolver 是存量令牌的兜底：用户可能还持有登录时发的不透明会话令牌（不是 JWT）。
// 解析**必须问账号服务**（会话表在它那里）；本服务不查任何人的库。
type SessionResolver interface {
	Resolve(ctx context.Context, bearer, cookie string) (*Principal, bool)
}

// authPolicy 是账号侧出站调用的策略（会话兜底与 PAT 内省共用同一套口径）：
// 两次尝试、单次 1.5s、总预算 4s、退避 100ms/400ms、连续 5 次失败熔断 10s。
// 账号服务在鉴权关键路径上，多打一次比"把有效凭据判成无效"便宜；但不允许无限重试。
func authPolicy() upstream.Policy {
	p := upstream.DefaultPolicy("auth")
	p.Attempts = 2
	// 单次尝试的超时与 PAT 内省共用同一个常量：两个客户端的口径必须一致。
	p.AttemptTimeout = PATIntrospectTimeout
	p.Budget = 4 * time.Second
	p.BaseBackoff = 100 * time.Millisecond
	p.MaxBackoff = 400 * time.Millisecond
	p.Jitter = 0.5
	p.BreakerThreshold = 5
	p.BreakerOpenFor = 10 * time.Second
	return p
}

// SessionClient 是与账号服务约定的兜底解析实现：把原样的 Bearer/Cookie 转给
// `GET /api/auth/me`，由账号服务验签或查会话表后返回身份。
// 账号服务是唯一身份来源——目录服务不参与身份判定，因此这里不指向 CATALOG_URL。
// 出站走 internal/upstream：账号服务抖动时有界重试、连续失败则熔断快速失败，
// 而不是每个请求各自死等一个固定超时。
type SessionClient struct {
	base string
	up   *upstream.Client
}

func NewSessionClient(baseURL string) *SessionClient {
	return &SessionClient{base: strings.TrimRight(strings.TrimSpace(baseURL), "/"), up: upstream.New(authPolicy())}
}

// Upstream 返回出站执行器：/ready?deep=1 的深探针与请求路径共用它（同一份熔断状态）。
func (c *SessionClient) Upstream() *upstream.Client { return c.up }

func (c *SessionClient) Resolve(ctx context.Context, bearer, cookie string) (*Principal, bool) {
	if c.base == "" || (bearer == "" && cookie == "") {
		return nil, false
	}
	header := http.Header{}
	if bearer != "" {
		header.Set("Authorization", "Bearer "+bearer)
	}
	if cookie != "" {
		header.Add("Cookie", (&http.Cookie{Name: "mf_session", Value: cookie}).String())
	}
	resp, err := c.up.Do(ctx, upstream.Request{Method: http.MethodGet, URL: c.base + "/api/auth/me", Header: header})
	if err != nil {
		// 兜底解析失败 = "这条令牌不是会话"，按匿名继续（401 由 Required 决定）。
		// 这里刻意不回 503：会话兜底是存量令牌的兼容路径，账号服务抖动不该把普通匿名读请求
		// 一律变成 503——401/503 的机器码契约只在 PAT 内省那条路径上（见 pat.go）。
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var raw json.RawMessage
	if err = json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, false
	}
	var user struct {
		ID          string   `json:"id"`
		Username    string   `json:"username"`
		Role        string   `json:"role"`
		Groups      []string `json:"groups"`
		Permissions []string `json:"permissions"`
	}
	if err = json.Unmarshal(raw, &user); err != nil || user.ID == "" {
		return nil, false
	}
	// 权限组随 /api/auth/me 一起下发：是否出现 permissions 键决定走“以码为准”还是
	// 历史角色兜底（显式空集合不得回落 admin，见 permission.go 的 Can）。
	var keys map[string]json.RawMessage
	_, permissionsSet := map[string]json.RawMessage{}, false
	if err := json.Unmarshal(raw, &keys); err == nil {
		_, permissionsSet = keys["permissions"]
	}
	return &Principal{
		ID: user.ID, Username: user.Username, Role: user.Role,
		Groups: user.Groups, Permissions: user.Permissions,
		PermissionsSet: permissionsSet,
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

	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	// refreshMu 只用来串行化"出站刷新"：flight 非空表示已有一次刷新在飞行中，
	// 等待者复用它的结果；flightErr 是最近一次飞行的结果。
	refreshMu sync.Mutex
	flight    chan struct{}
	flightErr error
}

// 令牌用途取值，与账号服务 store.TokenUse 同源（见 S01 矩阵 L3）：判定只认这三个值
// 与空串（历史令牌缺键，按会话语义兼容）；未知取值在 Verify 直接拒收。
const (
	TokenUseSession = "session"
	TokenUseOAuth   = "oauth"
	TokenUseIDToken = "id_token"
)

// isThirdPartyUse 报告该用途是否为第三方（oauth 访问令牌或发给当事客户端的 id_token）。
// 这类身份只代表“谁”，不代表“能做什么”：管理授权一律拒绝（见 permission.go 的 Can）。
func isThirdPartyUse(use string) bool {
	return use == TokenUseOAuth || use == TokenUseIDToken
}

// validTokenUse 判定载荷里的用途声明是否合法：未知取值直接拒收，防止将来新增用途的
// 令牌被当成已知用途放行（与签发侧 validTokenUse 同口径）。
func validTokenUse(use string) bool {
	switch use {
	case "", TokenUseSession, TokenUseOAuth, TokenUseIDToken:
		return true
	default:
		return false
	}
}

type claims struct {
	Username string `json:"preferred_username"`
	Role     string `json:"role"`
	// 权限组与权限码：与账号服务 store.Claims 的 json 名逐字一致，否则后台分配的
	// 权限组到了本服务就是空的（老令牌不带这两项，走 Can 的角色兜底）。
	Groups      []string `json:"groups"`
	Permissions []string `json:"permissions"`
	// Scope/ClientID/TokenUse 与签发侧对齐（auth 7e5bd35）：会话 JWT 恒带
	// token_use=session（无 scope/client_id），第三方 OAuth 带 token_use=oauth
	// （+scope/client_id），id_token 带 token_use=id_token（aud 指向客户端，
	// 平台受众的验签天然拒收）。audience 收口（Verify 的 WithAudience）不变。
	// 用途判定只认 IsThirdPartyUse（见 Verify），不把 token_use=session 误判为第三方。
	Scope     string `json:"scope"`
	ClientID  string `json:"client_id"`
	Cid       string `json:"cid"`
	TokenUse  string `json:"token_use"`
	TokenType string `json:"token_type"`
	// permissionsPresent 记录载荷里是否出现 permissions 键（含空数组）：显式空集合
	// 不得回落角色，只有缺字段的老令牌才走兜底（见 permission.go 的 Can）。
	permissionsPresent bool
	jwt.RegisteredClaims
}

// UnmarshalJSON 在标准 claims 解析之外多记一笔 permissions 键是否存在：
// encoding/json 无法区分“缺字段”与“显式空数组”（两者都解成 len==0），
// 而 S01 要求这两者走不同分支（显式空不得回落 admin）。
func (c *claims) UnmarshalJSON(raw []byte) error {
	type plain claims
	var p plain
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	*c = claims(p)
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		return nil
	}
	_, c.permissionsPresent = keys["permissions"]
	return nil
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
	clientID := c.ClientID
	if clientID == "" {
		clientID = c.Cid
	}
	// 用途隔离（与签发侧 7e5bd35 对齐）：会话 JWT 恒带 token_use=session，必须放行，
	// 只有 oauth/id_token 才是第三方；缺省（空串）是历史令牌，按会话语义兼容。
	// 缺省用途下若仍带 scope/client_id（签发侧过渡态），视为第三方——会话签发恒清零
	// 这两项。未知用途 fail closed（直接拒收，不按匿名放行）。
	use := strings.TrimSpace(c.TokenUse)
	if !validTokenUse(use) {
		return nil, errors.New("bad token_use")
	}
	thirdParty := isThirdPartyUse(use)
	if use == "" && !thirdParty {
		thirdParty = strings.TrimSpace(c.Scope) != "" || strings.TrimSpace(clientID) != "" ||
			strings.TrimSpace(c.TokenType) != ""
	}
	return &Principal{
		ID: c.Subject, Username: c.Username, Role: c.Role,
		Groups: c.Groups, Permissions: c.Permissions,
		Scope: strings.TrimSpace(c.Scope), ClientID: strings.TrimSpace(clientID),
		IsThirdParty: thirdParty, PermissionsSet: c.permissionsPresent,
	}, nil
}

// publicKey 按 kid 取验签公钥：命中未过期缓存直接返回；TTL 过期或 kid 未知时刷新一次
// （轮换期间签名密钥可能刚换过）。三条约束与目录/存储侧同构（2026-09-19 第二轮架构报告 #22）：
//
//  1. 出站请求不持缓存锁——JWKS 慢不会让本服务把请求堵在验签上；
//  2. 同一时刻只允许一次出站刷新（并发未知 kid 共享结果），出站请求数有界；
//  3. 刷新失败不直接判死：缓存里仍有该 kid 的公钥（TTL 过期也算——公钥本身没有有效期，
//     TTL 只是"多久去确认一次轮换"）就继续用它验签并记告警。账号服务抖动超过 TTL 不该让
//     本服务把所有已签发的有效令牌判成 401。
//
// 缓存里没有该 kid 时仍然 fail closed。
func (v *Verifier) publicKey(kid string) (*rsa.PublicKey, error) {
	if v.static != nil {
		return v.static, nil
	}
	if key, ok := v.freshKey(kid); ok {
		return key, nil
	}
	if err := v.refreshJWKS(kid); err != nil {
		if key, ok := v.cachedKey(kid); ok {
			slog.Warn("community: JWKS 刷新失败，回落到缓存公钥（可能错过一次轮换确认）", "kid", kid, "err", err.Error())
			return key, nil
		}
		return nil, err
	}
	if key, ok := v.cachedKey(kid); ok {
		return key, nil
	}
	return nil, errors.New("unknown signing key")
}

// freshKey 返回缓存里命中且未过期的公钥。
func (v *Verifier) freshKey(kid string) (*rsa.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if time.Since(v.fetchedAt) >= keyCacheTTL {
		return nil, false
	}
	return lookupKey(v.keys, kid)
}

// cachedKey 返回缓存里该 kid 的公钥，不看是否过期：刷新失败时的回落来源。
func (v *Verifier) cachedKey(kid string) (*rsa.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return lookupKey(v.keys, kid)
}

// lookupKey 按 kid 取键；只有一个密钥且令牌头没带 kid 时也命中（账号服务总是带 kid，这条只是容错）。
func lookupKey(keys map[string]*rsa.PublicKey, kid string) (*rsa.PublicKey, bool) {
	if key, ok := keys[kid]; ok {
		return key, true
	}
	if kid == "" && len(keys) == 1 {
		for _, key := range keys {
			return key, true
		}
	}
	return nil, false
}

// refreshJWKS 拉一次 JWKS。出站请求不持 mu；refreshMu + flight 让同一时刻只有一次出站请求，
// 等待者复用同一次结果，不再各自打一次网络。未知 kid 强刷、命中缓存不刷的行为保持不变
// （因此这里不再需要旧实现那次"再取一次"的额外出站）。
//
// 这里的出站**刻意不进 internal/upstream**（不加重试/熔断）：JWKS 是公钥的滚动更新源，
// 刷不到时已有"继续用缓存公钥验签"的降级路径（见 publicKey），再加一层重试只会把验签路径拖慢。
// 这条不在本轮跨服务降级的范围内。
func (v *Verifier) refreshJWKS(kid string) error {
	v.refreshMu.Lock()
	if ch := v.flight; ch != nil {
		v.refreshMu.Unlock()
		<-ch
		v.refreshMu.Lock()
		err := v.flightErr
		v.refreshMu.Unlock()
		return err
	}
	// 拿到刷新权时缓存可能刚被刷好：命中就直接复用，不必再打一次网络。
	if key, ok := v.freshKey(kid); ok && key != nil {
		v.refreshMu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	v.flight = ch
	v.refreshMu.Unlock()

	err := v.fetch()

	v.refreshMu.Lock()
	v.flightErr, v.flight = err, nil
	v.refreshMu.Unlock()
	close(ch)
	return err
}

// fetch 拉取 JWKS 并整体替换缓存。非 200、解析失败或没有可用 RSA 公钥都算失败，
// 此时旧缓存保持不动（signingKey 会按回落规则继续使用它）。出站期间不持 mu。
func (v *Verifier) fetch() error {
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
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = time.Now()
	v.mu.Unlock()
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
