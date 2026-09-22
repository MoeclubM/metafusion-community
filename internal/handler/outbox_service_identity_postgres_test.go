package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
	"github.com/MoeclubM/metafusion-community/internal/testutil"
)

// serviceDelivery 是目录桩收到的投递体（含服务身份头与作者快照）。
type serviceDelivery struct {
	Token       string         `json:"-"`
	AuthHeader  string         `json:"-"`
	RecipientID string         `json:"recipient_id"`
	Type        string         `json:"type"`
	SubjectType string         `json:"subject_type"`
	SubjectID   string         `json:"subject_id"`
	Payload     map[string]any `json:"payload"`
	DedupeKey   string         `json:"dedupe_key"`
	EventID     string         `json:"event_id"`
	ActorID     string         `json:"actor_id"`
	ActorName   string         `json:"actor_name"`
}

type serviceHarness struct {
	ctx     context.Context
	db      *store.Store
	router  http.Handler
	client  *catalog.Client
	stubURL string
	key     *rsa.PrivateKey
	kid     string
	mu      sync.Mutex
	sent    []serviceDelivery
	failNext atomic.Int64
	token   string
}

func (h *serviceHarness) sentWith() []serviceDelivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]serviceDelivery{}, h.sent...)
}

func newServiceHarness(t *testing.T) *serviceHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	dsn := testutil.DSN(t)
	db := testutil.Database(t)
	ctx := context.Background()
	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.Init(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	for _, stmt := range []string{
		"DELETE FROM community.notification_outbox",
		"DELETE FROM community.posts",
		"DELETE FROM community.topics",
		"DELETE FROM community.boards",
	} {
		if _, err = db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("cleanup %s: %v", stmt, err)
		}
	}
	if err = Seed(ctx, db); err != nil {
		t.Fatalf("seed boards: %v", err)
	}
	h := &serviceHarness{ctx: ctx, db: s, token: testInternalToken}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/notifications/internal") {
			var d serviceDelivery
			if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			d.Token = r.Header.Get("X-Internal-Token")
			d.AuthHeader = r.Header.Get("Authorization")
			h.mu.Lock()
			h.sent = append(h.sent, d)
			h.mu.Unlock()
			if h.failNext.Load() > 0 {
				h.failNext.Add(-1)
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "boom"})
				return
			}
			if d.Token != h.token {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_internal_token"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "unread": 1})
			return
		}
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/identity") {
			seg := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			id := seg[len(seg)-2]
			_ = json.NewEncoder(w).Encode(map[string]any{
				"canonical_id": id,
				"aliases":      []any{},
				"entity":       map[string]any{"id": id, "kind": "work", "title": "t", "status": "published"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.NewString(), "kind": "work", "title": "t", "status": "published"})
	}))
	t.Cleanup(stub.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	client := catalog.New(stub.URL, 2*time.Second)
	client.SetInternalToken(testInternalToken)
	router := gin.New()
	New(s, client, newVerifier(t, srv.URL)).Register(router)
	h.router = router
	h.client = client
	h.stubURL = stub.URL
	h.kid = kid
	h.key = key
	return h
}

func (h *serviceHarness) call(t *testing.T, sub, payload, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if sub != "" {
		req.Header.Set("Authorization", "Bearer "+signTokenWith(t, h.key, h.kid, sub, "member", nil, []string{auth.PermissionPostCreate}))
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

// A03-1：首次失败后用户退出，后台仍送达且作者正确；投递不带用户令牌。
func TestOutboxWorkerDeliversAfterLogoutWithCorrectAuthor(t *testing.T) {
	h := newServiceHarness(t)
	userA := uuid.NewString()
	userB := uuid.NewString()
	topicID := uuid.NewString()
	if _, err := h.db.DB().ExecContext(h.ctx,
		"INSERT INTO community.topics(id,board_code,author_id,author_name,title,body) VALUES($1,$2,$3,$4,$5,$6)",
		topicID, "qa", userA, "author-a", "求助", "正文"); err != nil {
		t.Fatalf("插入主题: %v", err)
	}
	// 上游执行器自带一次重试（Attempts=2）：逻辑失败需耗尽两次 HTTP 500。
	h.failNext.Store(10)
	path := "/api/community/topics/" + topicID + "/posts"
	if w := h.call(t, userB, "{\"content\":\"试试这样\"}", http.MethodPost, path); w.Code != 200 {
		t.Fatalf("B 回帖: %d %s", w.Code, w.Body.String())
	}
	first := h.sentWith()
	if len(first) == 0 {
		t.Fatalf("首次应试投: %+v", first)
	}
	for _, d := range first {
		if d.EventID == "" {
			t.Fatalf("每次投递必须带稳定事件: %+v", d)
		}
		if d.EventID != first[0].EventID {
			t.Fatalf("同一逻辑投递的多次 HTTP 必须同事件: %+v", first)
		}
	}
	if first[0].AuthHeader != "" {
		t.Fatalf("服务身份投递不得转发用户令牌: %q", first[0].AuthHeader)
	}
	if first[0].ActorID != userB {
		t.Fatalf("作者应为回帖人: %+v", first[0])
	}
	if first[0].EventID == "" {
		t.Fatal("必须带稳定事件 id")
	}
	if _, err := h.db.DB().ExecContext(h.ctx,
		"UPDATE community.notification_outbox SET next_retry_at=now() WHERE status='pending'"); err != nil {
		t.Fatalf("拨到期: %v", err)
	}
	h.failNext.Store(0)
	h.mu.Lock()
	h.sent = nil
	h.mu.Unlock()
	hd := New(h.db, h.client, newVerifier(t, "http://127.0.0.1:1/jwks"))
	if _, err := hd.store.ExpireOutbox(context.Background(), time.Now()); err != nil {
		t.Fatalf("expire: %v", err)
	}
	due, err := hd.store.ClaimDueOutbox(context.Background(), time.Now(), 50, 5*time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("后台应领到一行: %+v", due)
	}
	if due[0].ActorID != userB {
		t.Fatalf("落库快照应为回帖人: %+v", due[0])
	}
	ok := hd.deliverOutboxItem(context.Background(), due[0])
	if !ok {
		t.Fatal("后台应送达成功")
	}
	sent := h.sentWith()
	if len(sent) != 1 {
		t.Fatalf("后台应投递一次: %+v", sent)
	}
	if sent[0].ActorID != userB || sent[0].EventID != first[0].EventID {
		t.Fatalf("后台投递作者与事件必须与首次一致: %+v vs %+v", sent[0], first[0])
	}
	if sent[0].AuthHeader != "" {
		t.Fatalf("后台投递不得带用户令牌: %q", sent[0].AuthHeader)
	}
}

// A03-2：管理员重试不改作者（触发者凭据被忽略）。
func TestAdminRetryDoesNotChangeAuthor(t *testing.T) {
	h := newServiceHarness(t)
	userA := uuid.NewString()
	userB := uuid.NewString()
	topicID := uuid.NewString()
	if _, err := h.db.DB().ExecContext(h.ctx,
		"INSERT INTO community.topics(id,board_code,author_id,author_name,title,body) VALUES($1,$2,$3,$4,$5,$6)",
		topicID, "qa", userA, "author-a", "求助", "正文"); err != nil {
		t.Fatalf("插入主题: %v", err)
	}
	h.failNext.Store(10)
	path := "/api/community/topics/" + topicID + "/posts"
	if w := h.call(t, userB, "{\"content\":\"你好\"}", http.MethodPost, path); w.Code != 200 {
		t.Fatalf("B 回帖: %d %s", w.Code, w.Body.String())
	}
	if _, err := h.db.DB().ExecContext(h.ctx,
		"UPDATE community.notification_outbox SET next_retry_at=now() WHERE status='pending'"); err != nil {
		t.Fatalf("拨到期: %v", err)
	}
	h.failNext.Store(0)
	h.mu.Lock()
	h.sent = nil
	h.mu.Unlock()
	adminCtx := auth.WithCredentials(context.Background(), "admin-bearer-token", "")
	hd := New(h.db, h.client, newVerifier(t, "http://127.0.0.1:1/jwks"))
	res, err := hd.RetryDueOutbox(adminCtx, 50)
	if err != nil {
		t.Fatalf("admin retry: %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("管理员重试应送达: %+v", res)
	}
	sent := h.sentWith()
	if len(sent) != 1 {
		t.Fatalf("应投递一次: %+v", sent)
	}
	if sent[0].ActorID != userB {
		t.Fatalf("作者必须仍是回帖人而非管理员: %+v", sent[0])
	}
	if sent[0].AuthHeader != "" {
		t.Fatalf("管理员重试不得转发管理员令牌: %q", sent[0].AuthHeader)
	}
}

// A03-3：普通客户端伪造内部通知被拒（无共享密钥）。
func TestForgedInternalNotificationRejected(t *testing.T) {
	h := newServiceHarness(t)
	resp, err := http.Post(h.stubURL+"/api/notifications/internal", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post stub: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无共享密钥应 401，实际 %d", resp.StatusCode)
	}
	h.mu.Lock()
	h.sent = nil
	h.mu.Unlock()
	h.client.SetInternalToken("")
	err = h.client.NotifyService(context.Background(), catalog.Notification{RecipientID: uuid.NewString()})
	if err != catalog.ErrNotConfigured {
		t.Fatalf("未配置密钥应 ErrNotConfigured，实际 %v", err)
	}
	if got := h.sentWith(); len(got) != 0 {
		t.Fatalf("未配置密钥不得发出 HTTP: %+v", got)
	}
}
