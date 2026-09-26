package handler

// 上游不可用的对外形状：目录取不到时读/写路径都必须回 503 + 稳定机器码 upstream_unavailable，
// 而不是把依赖故障折成 404（用户以为条目被删了）或空列表（条目凭空消失）。
// 这里选不需要真库的路径：判定都发生在访问数据库之前（先问目录，再落库）。

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
	"github.com/MoeclubM/metafusion-community/internal/upstream"
)

// assertUpstreamUnavailable 钉住"依赖故障"的对外形状：503 + error=upstream_unavailable。
func assertUpstreamUnavailable(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("上游不可用应回 503，实际 %d（%s）", w.Code, w.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是 JSON 对象: %v（%s）", err, w.Body.String())
	}
	if got["error"] != upstream.CodeUpstreamUnavailable {
		t.Fatalf("机器码 = %q，期望 %q", got["error"], upstream.CodeUpstreamUnavailable)
	}
}

// 目录服务 503 时：收藏切换（写路径，判定在落库之前）与关联合集（读路径）都必须 503，
// 而不是 not_found / 空 items。
func TestCatalogOutageIsReportedAsUpstreamUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	verifier := newVerifier(t, srv.URL)

	// 假目录：一律 503（目录服务挂了，不是"这个实体不可见"）。
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()

	router := gin.New()
	New(&store.Store{}, catalog.New(down.URL, 2*time.Second), verifier).Register(router)

	req := httptest.NewRequest(http.MethodPost, "/api/favorites/toggle",
		strings.NewReader(`{"target_type":"work","target_id":"`+uuid.NewString()+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+signToken(t, key, kid, "user"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	assertUpstreamUnavailable(t, w)

	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/community/entities/"+uuid.NewString()+"/collections", nil))
	assertUpstreamUnavailable(t, w)
}

// 目录明确回答"不可见/不存在"时仍然是 404：降级不能把 404 也一起变成 503。
func TestCatalogNotFoundStaysNotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	jwks, _ := jwksServer(t, key)
	verifier := newVerifier(t, jwks.URL)

	missing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	}))
	defer missing.Close()

	router := gin.New()
	New(&store.Store{}, catalog.New(missing.URL, 2*time.Second), verifier).Register(router)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/api/community/entities/"+uuid.NewString()+"/collections", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("不可见实体应回 404，实际 %d（%s）", w.Code, w.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应体不是 JSON 对象: %v", err)
	}
	if got["error"] != "not_found" {
		t.Fatalf("错误码 = %q，期望 not_found", got["error"])
	}
}
