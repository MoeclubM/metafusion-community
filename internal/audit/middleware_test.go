package audit

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
)

// capture 是一个把审计行收进内存的落库函数：让中间件的用例不需要真库就能断言整条 entry。
type capture struct {
	mu      sync.Mutex
	entries []Entry
}

func newCapture(t *testing.T) (*Recorder, *capture) {
	t.Helper()
	c := &capture{}
	r := NewRecorder(nil, ServiceName)
	r.sink = func(e Entry) error {
		c.mu.Lock()
		c.entries = append(c.entries, e)
		c.mu.Unlock()
		return nil
	}
	t.Cleanup(r.Close)
	return r, c
}

func (c *capture) all(t *testing.T, r *Recorder) []Entry {
	t.Helper()
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Entry(nil), c.entries...)
}

// auditTestRouter 造一个最小路由组：一条登记的写路由、一条没登记的写路由、一条登记的失败路由。
func auditTestRouter(t *testing.T, r *Recorder) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api")
	api.Use(Middleware(Options{
		Recorder: r,
		Actions: map[string]string{
			"PUT /api/things/:id":    "thing.updated",
			"DELETE /api/things/:id": "thing.deleted",
			"PATCH /api/things/:id":  "thing.patched",
		},
		Actor: func(c *gin.Context) Actor {
			// 互动服务的 Actor 就是 Principal 的投影：FromPAT → pat，否则 session（近似值，契约 §7）。
			if c.GetHeader("X-Test-Anonymous") == "1" {
				return Actor{}
			}
			return Actor{UserID: "11111111-1111-1111-1111-111111111111", Username: "kana", CredentialType: CredentialSession}
		},
	}))
	api.PUT("/things/:id", func(c *gin.Context) {
		Describe(c, Detail{TargetType: "thing", TargetID: c.Param("id"), Changes: map[string]any{"name": "新名"}})
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	// 没登记的写路由：不该有任何审计行，也不该被塞上 X-Request-Id。
	api.POST("/things", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	// 失败路径：处理器登记错误码 → result=failure + error_code 与响应体一致。
	api.DELETE("/things/:id", func(c *gin.Context) {
		Fail(c, "not_found")
		c.JSON(http.StatusNotFound, gin.H{"error": "not_found"})
	})
	// 失败路径但没有登记错误码 → 按契约 §3 回落 http_<status>。
	api.PATCH("/things/:id", func(c *gin.Context) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication_required"})
	})
	return engine
}

// TestMiddlewareRecordsAuditedWriteRoute：登记过的写路由产生**恰好一行**，且模板、actor、
// 补充对象、request_id 透传全部到位（路由存模板不存原始路径，原始 id 只在 target_id 里）。
func TestMiddlewareRecordsAuditedWriteRoute(t *testing.T) {
	r, cap := newCapture(t)
	engine := auditTestRouter(t, r)

	thingID := "22222222-2222-2222-2222-222222222222"
	req := httptest.NewRequest(http.MethodPut, "/api/things/"+thingID, strings.NewReader("{}"))
	req.Header.Set("X-Request-Id", "rid-echo")
	req.Header.Set("User-Agent", "audit-test/1.0")
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)

	if got := w.Header().Get("X-Request-Id"); got != "rid-echo" {
		t.Fatalf("响应头未透传 X-Request-Id：%q", got)
	}
	entries := cap.all(t, r)
	if len(entries) != 1 {
		t.Fatalf("审计行数 = %d，期望恰好 1", len(entries))
	}
	e := entries[0]
	for field, want := range map[string]any{
		"service":    ServiceName,
		"action":     "thing.updated",
		"method":     http.MethodPut,
		"route":      "/api/things/:id",
		"request_id": "rid-echo",
		// httptest.NewRequest 的 RemoteAddr 是 192.0.2.1:1234，gin 的 ClientIP 去掉端口。
		"host":            "192.0.2.1",
		"user_agent":      "audit-test/1.0",
		"actor_id":        "11111111-1111-1111-1111-111111111111",
		"actor_username":  "kana",
		"credential_type": CredentialSession,
		"target_type":     "thing",
		"target_id":       thingID,
		"result":          ResultSuccess,
		"error_code":      "",
		"http_status":     200,
		"changes_name":    "新名",
	} {
		got := map[string]any{
			"service": e.Service, "action": e.Action, "method": e.RequestMethod, "route": e.Route,
			"request_id": e.RequestID, "host": e.ActorIP, "user_agent": e.ActorUserAgent,
			"actor_id": e.ActorUserID, "actor_username": e.ActorUsername, "credential_type": e.CredentialType,
			"target_type": e.TargetType, "target_id": e.TargetID, "result": e.Result,
			"error_code": e.ErrorCode, "http_status": e.HTTPStatus, "changes_name": e.Changes["name"],
		}[field]
		if got != want {
			t.Fatalf("%s = %#v，期望 %#v（整行 %#v）", field, got, want, e)
		}
	}
	if e.ID == "" || e.OccurredAt.IsZero() {
		t.Fatalf("id / occurred_at 未生成：%#v", e)
	}
}

// TestMiddlewareSkipsUnregisteredAndUnknownRoutes：没登记的写路由与未匹配路径都不写审计，
// 也不给响应塞 X-Request-Id（读接口的响应形状一动不动）。
func TestMiddlewareSkipsUnregisteredAndUnknownRoutes(t *testing.T) {
	r, cap := newCapture(t)
	engine := auditTestRouter(t, r)

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/things"},      // 写路由但没登记动作码
		{http.MethodPost, "/api/not-a-route"}, // 未匹配：FullPath 为空，回落原始路径也不该命中注册表
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("X-Request-Id", "rid-skip")
		w := httptest.NewRecorder()
		engine.ServeHTTP(w, req)
		if got := w.Header().Get("X-Request-Id"); got != "" {
			t.Fatalf("%s %s 不该回写 X-Request-Id，实际 %q", tc.method, tc.path, got)
		}
	}
	if entries := cap.all(t, r); len(entries) != 0 {
		t.Fatalf("未登记的路由写了 %d 行审计：%#v", len(entries), entries)
	}
}

// TestMiddlewareRequestIDGenerationAndFailureResult：没有 X-Request-Id 时自己生成并回写；
// 失败路径写 result=failure，错误码优先取处理器登记的稳定码，没有才回落 http_<status>。
func TestMiddlewareRequestIDGenerationAndFailureResult(t *testing.T) {
	r, cap := newCapture(t)
	engine := auditTestRouter(t, r)
	thingID := "33333333-3333-3333-3333-333333333333"

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/things/"+thingID, nil))
	generated := w.Header().Get("X-Request-Id")
	if len(generated) != 36 {
		t.Fatalf("缺省应生成 uuid 并回写，实际 %q", generated)
	}

	w = httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/api/things/"+thingID, nil))

	entries := cap.all(t, r)
	if len(entries) != 2 {
		t.Fatalf("审计行数 = %d，期望 2", len(entries))
	}
	if entries[0].RequestID != generated || entries[0].Result != ResultFailure || entries[0].ErrorCode != "not_found" {
		t.Fatalf("登记错误码的失败行不符：%#v（生成的 request_id %q）", entries[0], generated)
	}
	if entries[0].HTTPStatus != http.StatusNotFound || entries[0].TargetType != "" {
		t.Fatalf("失败路径的 http_status / 未补充的 target 不符：%#v", entries[0])
	}
	if entries[1].Result != ResultFailure || entries[1].ErrorCode != "http_401" {
		t.Fatalf("未登记错误码应回落 http_<status>：%#v", entries[1])
	}
}

// TestMiddlewareWithoutRecorderIsNoop：Recorder 为 nil（不建库的进程/用例）时中间件退化为空操作，
// 不能 panic，也不能给响应加头。
func TestMiddlewareWithoutRecorderIsNoop(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	api := engine.Group("/api")
	api.Use(Middleware(Options{Recorder: nil, Actions: map[string]string{"PUT /api/things/:id": "thing.updated"}}))
	api.PUT("/things/:id", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	w := httptest.NewRecorder()
	engine.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/things/44444444-4444-4444-4444-444444444444", nil))
	if w.Code != http.StatusOK || w.Header().Get("X-Request-Id") != "" {
		t.Fatalf("nil Recorder 应为空操作：%d %q", w.Code, w.Header().Get("X-Request-Id"))
	}
}

// TestActorCredentialType：没有身份时 credential_type=anonymous（互动服务的写接口都要求登录，
// 这条对应"身份中间件没解析出人"的异常路径，审计仍要留下痕迹）。
func TestActorCredentialType(t *testing.T) {
	r, cap := newCapture(t)
	engine := auditTestRouter(t, r)
	req := httptest.NewRequest(http.MethodPut, "/api/things/55555555-5555-5555-5555-555555555555", nil)
	req.Header.Set("X-Test-Anonymous", "1")
	engine.ServeHTTP(httptest.NewRecorder(), req)

	entries := cap.all(t, r)
	if len(entries) != 1 || entries[0].CredentialType != CredentialAnonymous || entries[0].ActorUserID != "" {
		t.Fatalf("匿名行的 credential_type 不符：%#v", entries)
	}
}
