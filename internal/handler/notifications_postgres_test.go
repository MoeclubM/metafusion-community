package handler

// 通知产生端的真库用例：实体短评（参与式关注）与论坛回帖（回复目标 → 主题作者）。
// 目录侧只做两件事：答一次实体投影（要标题）、收一次投递体；因此这里用 httptest 桩，
// 断言的是"互动服务发了什么"，目录侧的收件箱语义在它自己的仓库里验。
// COMMUNITY_TEST_DSN 未设置时 testutil.Database 自动跳过。

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
	"github.com/MoeclubM/metafusion-community/internal/testutil"
)

const testInternalToken = "test-internal-token"

// delivery 是桩收到的投递体（与目录侧 POST /api/notifications/internal 请求体同形）。
type delivery struct {
	Token       string         `json:"-"`
	RecipientID string         `json:"recipient_id"`
	Type        string         `json:"type"`
	SubjectType string         `json:"subject_type"`
	SubjectID   string         `json:"subject_id"`
	Payload     map[string]any `json:"payload"`
	DedupeKey   string         `json:"dedupe_key"`
	EventID     string         `json:"event_id"`
}

type notifyHarness struct {
	ctx      context.Context
	db       *store.Store
	router   http.Handler
	client   *catalog.Client
	entityID string
	key      *rsa.PrivateKey
	kid      string
	mu       sync.Mutex
	sent     []delivery
}

// sentWith 复制已收到的投递（并发安全：投递发生在请求 goroutine 内，但读要加锁）。
func (h *notifyHarness) sentWith() []delivery {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]delivery{}, h.sent...)
}

func newNotifyHarness(t *testing.T) *notifyHarness {
	t.Helper()
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
	for _, stmt := range []string{"DELETE FROM community.posts", "DELETE FROM community.topics", "DELETE FROM community.boards"} {
		if _, err = db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("cleanup %s: %v", stmt, err)
		}
	}
	if err = Seed(ctx, db); err != nil {
		t.Fatalf("seed boards: %v", err)
	}

	h := &notifyHarness{ctx: ctx, db: s, entityID: uuid.NewString()}
	catalogStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/notifications/internal") {
			var d delivery
			if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			d.Token = r.Header.Get("X-Internal-Token")
			h.mu.Lock()
			h.sent = append(h.sent, d)
			h.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "unread": 1})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": h.entityID, "kind": "work", "title": "测试作品", "status": "published"})
	}))
	t.Cleanup(catalogStub.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	client := catalog.New(catalogStub.URL, 2*time.Second)
	client.SetInternalToken(testInternalToken)
	router := gin.New()
	New(s, client, newVerifier(t, srv.URL)).Register(router)
	h.router = router
	h.client = client
	h.kid = kid
	h.key = key
	return h
}

// call 用给定身份的令牌打一次真实的 HTTP 路由。
func (h *notifyHarness) call(t *testing.T, sub, payload, method, path string) *httptest.ResponseRecorder {
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

func (h *notifyHarness) comment(t *testing.T, sub, body string) *httptest.ResponseRecorder {
	return h.call(t, sub, "{\"body\":\""+body+"\"}", http.MethodPost, "/api/community/entities/"+h.entityID+"/posts")
}

// 核心场景：两个用户 + 一条评论 → 先评论的人收到恰好一条投递。
func TestEntityCommentNotifiesOtherParticipants(t *testing.T) {
	h := newNotifyHarness(t)
	userA := uuid.NewString()
	userB := uuid.NewString()

	if w := h.comment(t, userA, "第一条评论"); w.Code != 200 {
		t.Fatalf("A 发短评: %d %s", w.Code, w.Body.String())
	}
	if sent := h.sentWith(); len(sent) != 0 {
		t.Fatalf("评论区里只有自己时不该通知任何人: %+v", sent)
	}

	if w := h.comment(t, userB, "我也想问这个"); w.Code != 200 {
		t.Fatalf("B 发短评: %d %s", w.Code, w.Body.String())
	}
	sent := h.sentWith()
	if len(sent) != 1 {
		t.Fatalf("应先评论的人收到恰好一条: %+v", sent)
	}
	d := sent[0]
	if d.RecipientID != userA {
		t.Fatalf("收件人应是先评论的人: %+v", d)
	}
	if d.Type != catalog.NotificationCommentReplied || d.SubjectType != "entity" || d.SubjectID != h.entityID {
		t.Fatalf("投递体形状不符: %+v", d)
	}
	if d.DedupeKey != "comment.replied:entity:"+h.entityID {
		t.Fatalf("聚合键应按条目: %q", d.DedupeKey)
	}
	if d.EventID == "" {
		t.Fatal("必须带稳定事件 id（上游重试要幂等）")
	}
	if d.Token != testInternalToken {
		t.Fatalf("必须带共享密钥头: %q", d.Token)
	}
	if got, _ := d.Payload["entity_title"].(string); got != "测试作品" {
		t.Fatalf("载荷应带条目题名（来自目录投影）: %+v", d.Payload)
	}
	if got, _ := d.Payload["excerpt"].(string); got != "我也想问这个" {
		t.Fatalf("载荷应带正文摘要: %+v", d.Payload)
	}
}

// 论坛回帖：收件人是被回复楼层作者；没有回复目标时是主题作者。
func TestTopicReplyNotifiesRepliedToAndTopicAuthor(t *testing.T) {
	h := newNotifyHarness(t)
	userA := uuid.NewString()
	userB := uuid.NewString()
	topicID := uuid.NewString()
	if _, err := h.db.DB().ExecContext(h.ctx,
		"INSERT INTO community.topics(id,board_code,author_id,author_name,title,body) VALUES($1,$2,$3,$4,$5,$6)",
		topicID, "qa", userA, "author-a", "求助：怎么编目", "正文"); err != nil {
		t.Fatalf("插入主题: %v", err)
	}

	// B 回帖（无回复目标）→ 主题作者 A 收到。
	path := "/api/community/topics/" + topicID + "/posts"
	if w := h.call(t, userB, "{\"content\":\"试试这样\"}", http.MethodPost, path); w.Code != 200 {
		t.Fatalf("B 回帖: %d %s", w.Code, w.Body.String())
	}
	sent := h.sentWith()
	if len(sent) != 1 || sent[0].RecipientID != userA {
		t.Fatalf("无回复目标时应通知主题作者: %+v", sent)
	}
	if sent[0].SubjectType != "topic" || sent[0].SubjectID != topicID || sent[0].DedupeKey != "comment.replied:topic:"+topicID {
		t.Fatalf("回帖投递体形状不符: %+v", sent[0])
	}

	// A 回复 B 的楼层（post_number=2）→ 只有 B 收到（A 是操作者，不给自己发）。
	h.mu.Lock()
	h.sent = nil
	h.mu.Unlock()
	if w := h.call(t, userA, "{\"content\":\"谢谢\",\"reply_to_post_number\":2}", http.MethodPost, path); w.Code != 200 {
		t.Fatalf("A 回帖: %d %s", w.Code, w.Body.String())
	}
	sent = h.sentWith()
	if len(sent) != 1 || sent[0].RecipientID != userB {
		t.Fatalf("带回复目标时应通知被回复楼层作者，且不给自己发: %+v", sent)
	}
	if got, ok := sent[0].Payload["reply_to_post_number"]; !ok || got != float64(2) {
		t.Fatalf("载荷应带被回复楼层号: %+v", sent[0].Payload)
	}
}

// 未配置共享密钥：一条也不投递（评论照常成功）——部署态不该把评论功能一起拖垮。
func TestEntityCommentWithoutInternalTokenStillSucceeds(t *testing.T) {
	h := newNotifyHarness(t)
	userA := uuid.NewString()
	userB := uuid.NewString()
	if w := h.comment(t, userA, "第一条评论"); w.Code != 200 {
		t.Fatalf("A 发短评: %d %s", w.Code, w.Body.String())
	}
	// 把密钥清掉后再评论：请求仍 200，只是没有投递。
	h.client.SetInternalToken("")
	if w := h.comment(t, userB, "第二条评论"); w.Code != 200 {
		t.Fatalf("未配置密钥时短评仍应成功: %d %s", w.Code, w.Body.String())
	}
	if sent := h.sentWith(); len(sent) != 0 {
		t.Fatalf("未配置密钥不该投递: %+v", sent)
	}
}
