package handler

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/config"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

const (
	testIssuer   = "https://findverse.cc/api"
	testAudience = "metafusion"
)

func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// 起一个与账号服务同格式的 JWKS：kid 取公钥 SPKI 的 SHA-256 前 8 字节，
// n/e 用 base64url。本服务只按这份格式验签，因此这里同时验证了跨服务的契约。
func jwksServer(t *testing.T, key *rsa.PrivateKey) (*httptest.Server, string) {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	sum := sha256.Sum256(der)
	kid := b64url(sum[:8])
	doc := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "use": "sig", "alg": "RS256", "kid": kid,
		"n": b64url(key.PublicKey.N.Bytes()),
		"e": b64url(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv, kid
}

// testSubject 是测试令牌的固定 sub：老令牌用例与它无关，但权限用例需要多个身份。
const testSubject = "11111111-1111-1111-1111-111111111111"

// signToken 签发**老令牌**：claims 里没有 groups / permissions，本服务只能按角色兜底。
func signToken(t *testing.T, key *rsa.PrivateKey, kid, role string) string {
	t.Helper()
	return signTokenWith(t, key, kid, testSubject, role, nil, nil)
}

// signTokenWith 签发带权限组的令牌：groups / permissions 的 claims 名与账号服务逐字一致，
// 用来验证"后台分配的权限组能被本服务读到"这条链路。
func signTokenWith(t *testing.T, key *rsa.PrivateKey, kid, sub, role string, groups, permissions []string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":                sub,
		"preferred_username": "kana",
		"token_use":          "session",
		"role":               role,
		"iss":                testIssuer,
		"aud":                testAudience,
		"exp":                time.Now().Add(10 * time.Minute).Unix(),
		"iat":                time.Now().Unix(),
		"jti":                "test-jti",
	}
	if len(groups) > 0 {
		claims["groups"] = groups
	}
	if len(permissions) > 0 {
		claims["permissions"] = permissions
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

func newVerifier(t *testing.T, jwksURL string) *auth.Verifier {
	t.Helper()
	v, err := auth.New(config.Config{JWKSURL: jwksURL, JWTIssuer: testIssuer, JWTAudience: testAudience})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	return v
}

// 跨服务验签：账号服务签发的令牌必须能被互动服务用 JWKS 本地验签，
// 并还原出 sub/role（切流后每个业务服务都靠这条链路识别身份）。
func TestMiddlewareResolvesPrincipalFromJWKS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	verifier := newVerifier(t, srv.URL)

	r := gin.New()
	api := r.Group("/api")
	api.Use(verifier.Middleware())
	api.GET("/probe", func(c *gin.Context) {
		p := auth.Current(c)
		if p == nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "anonymous"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"id": p.ID, "username": p.Username,
			"groups": strings.Join(p.Groups, ","), "permissions": strings.Join(p.Permissions, ","),
		})
	})

	// 匿名：应视为未登录（而不是报错）。
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/probe", nil))
	if w.Code != 401 {
		t.Fatalf("匿名应为 401，实际 %d", w.Code)
	}

	// 带令牌：应还原出身份。
	req := httptest.NewRequest(http.MethodGet, "/api/probe", nil)
	req.Header.Set("Authorization", "Bearer "+signToken(t, key, kid, "editor"))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("带令牌应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	var got map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["id"] != testSubject || got["username"] != "kana" {
		t.Fatalf("身份还原不符: %v", got)
	}

	// 带权限组的令牌：groups / permissions 必须原样落到 Principal（否则后台分配的权限组白给）。
	req = httptest.NewRequest(http.MethodGet, "/api/probe", nil)
	req.Header.Set("Authorization", "Bearer "+signTokenWith(t, key, kid, testSubject, "user",
		[]string{"community_moderator"}, []string{auth.PermissionPostCreate, auth.PermissionPostModerate}))
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("带权限组的令牌应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	got = map[string]string{}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["groups"] != "community_moderator" {
		t.Fatalf("权限组未还原: %v", got)
	}
	if got["permissions"] != auth.PermissionPostCreate+","+auth.PermissionPostModerate {
		t.Fatalf("权限码未还原: %v", got)
	}

	// 错误受众的令牌必须被拒（防止跨依赖方混用）。
	bad := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"sub": "x", "iss": testIssuer, "aud": "other", "exp": time.Now().Add(time.Minute).Unix(),
	})
	bad.Header["kid"] = kid
	signed, _ := bad.SignedString(key)
	req = httptest.NewRequest(http.MethodGet, "/api/probe", nil)
	req.Header.Set("Authorization", "Bearer "+signed)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatalf("受众不符的令牌必须被拒，实际 %d", w.Code)
	}
}

// 鉴权边界与可见性判定都必须在触碰数据库之前完成：
// 匿名写操作 401；不可见/不存在实体一律 404（不泄露存在性）。
func TestAuthBoundaryBeforeDatabase(t *testing.T) {
	gin.SetMode(gin.TestMode)
	verifier := newVerifier(t, "http://127.0.0.1:1/jwks")
	r := gin.New()
	New(&store.Store{}, catalog.New("", 0), verifier).Register(r)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/community/topics"},
		{http.MethodPost, "/api/community/entities/" + uuid.NewString() + "/posts"},
		{http.MethodPost, "/api/favorites/toggle"},
		{http.MethodGet, "/api/favorites/mine"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != 401 {
			t.Fatalf("%s %s 匿名应 401，实际 %d", tc.method, tc.path, w.Code)
		}
	}

	// 匿名读：实体经目录接口判定不可见（目录地址为空）时一律 404，且不查库。
	for _, path := range []string{
		"/api/community/entities/" + uuid.NewString() + "/posts",
		"/api/community/entities/" + uuid.NewString() + "/collections",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != 404 {
			t.Fatalf("GET %s 不可见实体应 404，实际 %d", path, w.Code)
		}
	}
}

// 删除接口的 uuid 校验必须发生在查库之前：非法字面量回 404，
// 既不是 500（pq 解析错误被兜成 500），也不能靠空库 panic 混过去。
func TestDeleteEndpointsRejectMalformedIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	verifier := newVerifier(t, srv.URL)
	r := gin.New()
	New(&store.Store{}, catalog.New("", 0), verifier).Register(r)

	token := signTokenWith(t, key, kid, testSubject, "user",
		[]string{"community_moderator"}, []string{auth.PermissionPostModerate})
	for _, path := range []string{
		"/api/community/topics/not-a-uuid",
		"/api/community/topics/not-a-uuid/posts/" + uuid.NewString(),
		"/api/community/topics/" + uuid.NewString() + "/posts/not-a-uuid",
		"/api/community/posts/not-a-uuid",
	} {
		req := httptest.NewRequest(http.MethodDelete, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != 404 {
			t.Fatalf("DELETE %s 非法 id 应 404，实际 %d（%s）", path, w.Code, w.Body.String())
		}
	}
}

// 公开收藏列表的用户 id 是 uuid：非法字面量必须在查库之前按"没有这个人"处理，
// 否则 pq 的解析错误会被回显给客户端（线上曾返回 400 与整条 SQL 错误原文）。
func TestUserFavoritesRejectsMalformedOwnerID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	verifier := newVerifier(t, "http://127.0.0.1:1/jwks")
	r := gin.New()
	New(&store.Store{}, catalog.New("", 0), verifier).Register(r)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/users/not-a-uuid/favorites", nil))
	if w.Code != 404 {
		t.Fatalf("非法用户 id 应 404，实际 %d（%s）", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "pq:") {
		t.Fatalf("响应不得回显数据库错误：%s", w.Body.String())
	}
}

// 写接口的请求体统一走 body()：超过 2MB 直接 400，不能把整份载荷读进来
// （网关给的上限是 1G，服务侧不收口就等于没有上限）。
func TestTopicCreateRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	verifier := newVerifier(t, srv.URL)
	r := gin.New()
	New(&store.Store{}, catalog.New("", 0), verifier).Register(r)

	// 载荷超限但字段本身都合法：这样"返回 400"只能由 body() 的体积上限解释，
	// 而不是被标题/正文长度校验顺带拦下。
	big := `{"board_code":"casual","title":"x","content":"ok","padding":"` + strings.Repeat("a", 3<<20) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/community/topics", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+signTokenWith(t, key, kid, testSubject, "user", nil, []string{auth.PermissionPostCreate}))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_payload") {
		t.Fatalf("超限请求体应 400 invalid_payload，实际 %d（%s）", w.Code, w.Body.String())
	}
}
