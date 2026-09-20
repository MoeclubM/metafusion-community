package auth

import (
	"crypto/rsa"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// S01 矩阵 L3：token_use 区分会话与第三方（与签发侧 7e5bd35 对齐）。
// 会话 JWT 恒带 token_use=session，必须按第一方放行；只有 oauth/id_token 是第三方；
// 未知取值 fail closed；缺键是历史令牌，按会话语义兼容。
func signTokenUse(t *testing.T, key *rsa.PrivateKey, kid string, extra map[string]any) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":                "aaaaaaaa-0000-0000-0000-000000000001",
		"preferred_username": "kana",
		"role":               "user",
		"iss":                jwksTestIssuer,
		"aud":                jwksTestAudience,
		"exp":                time.Now().Add(10 * time.Minute).Unix(),
		"iat":                time.Now().Unix(),
		"jti":                "token-use-test",
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

func tokenUseVerifier(t *testing.T, key *rsa.PrivateKey) (*Verifier, string) {
	t.Helper()
	var hits int64
	cur := key
	srv := jwksTestServer(t, &hits, func() *rsa.PrivateKey { return cur }, nil, 0)
	return jwksTestVerifier(t, srv.URL), jwksTestKID(t, &key.PublicKey)
}

// 会话用途持治理码：第一方、以码为准，治理放行。
func TestVerifySessionTokenUseIsFirstParty(t *testing.T) {
	key := jwksTestKey(t)
	v, kid := tokenUseVerifier(t, key)
	token := signTokenUse(t, key, kid, map[string]any{
		"token_use":   TokenUseSession,
		"permissions": []string{PermissionPostCreate, PermissionPostModerate},
	})
	p, err := v.Verify(token)
	if err != nil {
		t.Fatalf("会话令牌验签失败: %v", err)
	}
	if p.IsThirdParty {
		t.Fatal("token_use=session 不得判为第三方")
	}
	if !p.PermissionsSet {
		t.Fatal("显式 permissions 必须记为已声明")
	}
	if !p.Can(PermissionPostModerate) {
		t.Fatal("会话持治理码应放行")
	}
	if p.Can(PermissionBoardManage) {
		t.Fatal("未持有的治理码不得放行")
	}
}

// 第三方用途：即使 role 残留 admin，治理码一律不放行（显式空不得回落）。
func TestVerifyOAuthTokenUseIsThirdParty(t *testing.T) {
	key := jwksTestKey(t)
	v, kid := tokenUseVerifier(t, key)
	token := signTokenUse(t, key, kid, map[string]any{
		"token_use": TokenUseOAuth,
		"client_id": "third-party-app",
		"scope":     "openid profile",
		"role":      "admin",
	})
	p, err := v.Verify(token)
	if err != nil {
		t.Fatalf("第三方令牌验签失败: %v", err)
	}
	if !p.IsThirdParty {
		t.Fatal("token_use=oauth 必须判为第三方")
	}
	if p.Scope != "openid profile" || p.ClientID != "third-party-app" {
		t.Fatalf("授权绑定未还原: scope=%q client=%q", p.Scope, p.ClientID)
	}
	for _, code := range communityPermissionCodes {
		if p.Can(code) {
			t.Fatalf("第三方不得放行 %s：即使 role 残留 admin", code)
		}
	}
}

// id_token 用途同样是第三方（aud 指向客户端的真 id_token 在验签时已先被拒，
// 这里断言的是用途位本身的判定）。
func TestVerifyIDTokenUseIsThirdParty(t *testing.T) {
	key := jwksTestKey(t)
	v, kid := tokenUseVerifier(t, key)
	token := signTokenUse(t, key, kid, map[string]any{"token_use": TokenUseIDToken})
	p, err := v.Verify(token)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if !p.IsThirdParty {
		t.Fatal("token_use=id_token 必须判为第三方")
	}
	if p.Can(PermissionPostCreate) {
		t.Fatal("第三方不得放行发帖码（空集合以码为准）")
	}
}

// 未知用途 fail closed：不能把将来新增用途的令牌当成已知用途放行。
func TestVerifyUnknownTokenUseRejected(t *testing.T) {
	key := jwksTestKey(t)
	v, kid := tokenUseVerifier(t, key)
	token := signTokenUse(t, key, kid, map[string]any{
		"token_use":   "superuser",
		"permissions": []string{permissionWildcard},
	})
	if _, err := v.Verify(token); err == nil {
		t.Fatal("未知 token_use 必须拒收")
	}
}

// 历史令牌（无 token_use、无 scope）：按会话语义兼容——发帖走老边界，
// 治理码不再回落 admin（S01），第三方标记为假。
func TestVerifyLegacyWithoutTokenUse(t *testing.T) {
	key := jwksTestKey(t)
	v, kid := tokenUseVerifier(t, key)
	token := signTokenUse(t, key, kid, map[string]any{"role": "admin"})
	p, err := v.Verify(token)
	if err != nil {
		t.Fatalf("历史令牌验签失败: %v", err)
	}
	if p.IsThirdParty {
		t.Fatal("无用途标记不得判为第三方")
	}
	if !p.Can(PermissionPostCreate) {
		t.Fatal("老令牌仍可发帖")
	}
	if p.Can(PermissionPostModerate) {
		t.Fatal("老令牌的 admin 不得凭角色放行治理码")
	}
}

// 缺省用途下仍带 scope/client_id（签发侧过渡态）：视为第三方——会话签发恒清零这两项。
func TestVerifyEmptyUseWithScopeIsThirdParty(t *testing.T) {
	key := jwksTestKey(t)
	v, kid := tokenUseVerifier(t, key)
	token := signTokenUse(t, key, kid, map[string]any{
		"scope":     "profile",
		"client_id": "transitional-app",
	})
	p, err := v.Verify(token)
	if err != nil {
		t.Fatalf("验签失败: %v", err)
	}
	if !p.IsThirdParty {
		t.Fatal("缺省用途下带 scope/client_id 必须判为第三方")
	}
}
