package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 存量不透明会话令牌的兜底必须问账号服务（会话表在它那里）：
// 把 Bearer 与 Cookie 原样转过去，由账号服务判定身份；
// 权限组与权限码（groups / permissions）必须一并读进 Principal，否则本服务仍只认角色。
func TestSessionClientResolvesThroughAccountService(t *testing.T) {
	var gotAuth, gotCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/me" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		if ck, err := r.Cookie("mf_session"); err == nil {
			gotCookie = ck.Value
		}
		_, _ = w.Write([]byte(`{"id":"u-1","username":"kana","role":"user","groups":["community_moderator"],"permissions":["community.post.create","community.post.moderate"]}`))
	}))
	defer srv.Close()

	c := NewSessionClient(srv.URL, 2*time.Second)
	p, ok := c.Resolve(context.Background(), "opaque-token", "cookie-token")
	if !ok || p == nil || p.ID != "u-1" || p.Role != "user" || p.Username != "kana" {
		t.Fatalf("身份解析失败: %v %+v", ok, p)
	}
	if len(p.Groups) != 1 || p.Groups[0] != "community_moderator" {
		t.Fatalf("权限组未读进 Principal: %v", p.Groups)
	}
	if !p.Can(PermissionPostModerate) {
		t.Fatalf("权限码未读进 Principal: %v", p.Permissions)
	}
	if gotAuth != "Bearer opaque-token" || gotCookie != "cookie-token" {
		t.Fatalf("凭据未原样透传: %q %q", gotAuth, gotCookie)
	}
}

// 未配置账号服务地址时不做任何请求（身份只认 JWT），也不会误判为已登录。
func TestSessionClientWithoutBaseIsInert(t *testing.T) {
	c := NewSessionClient("", time.Second)
	if _, ok := c.Resolve(context.Background(), "t", ""); ok {
		t.Fatal("空地址不应解析出身份")
	}
}
