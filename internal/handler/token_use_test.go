package handler

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// S01 矩阵 L3 端到端：会话 JWT（token_use=session）持治理码可过管理闸门；
// 第三方 OAuth JWT（token_use=oauth）被拒；未知用途 fail closed。
// 用置顶闸门（community.topic.pin）做探针：纯中间件判定，不碰数据库。
func TestRequireDistinguishesSessionFromOAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	verifier := newVerifier(t, srv.URL)
	h := New(&store.Store{}, catalog.New("", 0), verifier)
	r := gin.New()
	api := r.Group("/api")
	api.Use(verifier.Middleware())
	api.GET("/probe-pin", h.require(auth.PermissionTopicPin), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	sign := func(extra map[string]any) string {
		t.Helper()
		claims := jwt.MapClaims{
			"sub":                testSubject,
			"preferred_username": "kana",
			"role":               "user",
			"iss":                testIssuer,
			"aud":                testAudience,
			"exp":                time.Now().Add(10 * time.Minute).Unix(),
			"iat":                time.Now().Unix(),
			"jti":                "use-probe",
		}
		for k, v := range extra {
			claims[k] = v
		}
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
		tok.Header["kid"] = kid
		signed, err := tok.SignedString(key)
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return signed
	}
	do := func(token string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/probe-pin", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}

	session := sign(map[string]any{"token_use": "session", "permissions": []string{auth.PermissionTopicPin}})
	if code := do(session); code != http.StatusOK {
		t.Fatalf("会话治理应 200，实际 %d", code)
	}
	oauth := sign(map[string]any{"token_use": "oauth", "scope": "openid profile", "client_id": "third-party-app"})
	if code := do(oauth); code != http.StatusForbidden {
		t.Fatalf("第三方治理应 403，实际 %d", code)
	}
	unknown := sign(map[string]any{"token_use": "superuser", "permissions": []string{auth.PermissionTopicPin}})
	if code := do(unknown); code != http.StatusUnauthorized {
		t.Fatalf("未知用途应 fail closed 401，实际 %d", code)
	}
}
