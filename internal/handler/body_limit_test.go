package handler

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// 回帖端点此前用 c.ShouldBindJSON 直接解析，绕过了本服务统一的 body() 助手，
// 而网关的 client_max_body_size 是 1G——一个超大请求体足以让本服务把整份载荷读进内存。
// 上限判定发生在解析阶段，因此这条用例不需要数据库。
func TestReplyRejectsOversizedPayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	verifier := newVerifier(t, srv.URL)
	r := gin.New()
	New(&store.Store{}, catalog.New("", 0), verifier).Register(r)
	token := signTokenWith(t, key, kid, testSubject, "user", nil, []string{auth.PermissionPostCreate})

	// 载荷本身是合法 JSON、正文很短，体积全在未被声明的字段上（gin 默认忽略未知字段）：
	// 这样"被上限拦住"与"被正文长度校验拦住"不会撞成同一个 400——若上限失效，
	// 解析会成功并继续走到数据库（用例会以 panic 而不是 400 暴露）。
	payload := `{"content":"ok","padding":"` + strings.Repeat("a", 2<<20) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/community/topics/"+uuid.NewString()+"/posts", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "invalid_payload") {
		t.Fatalf("超限回帖应 400 invalid_payload，实际 %d（%s）", w.Code, w.Body.String())
	}
}
