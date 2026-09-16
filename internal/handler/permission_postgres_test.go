package handler

import (
	"context"
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

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
	"github.com/MoeclubM/metafusion-community/internal/testutil"
)

// 需要真实 PostgreSQL：验证"后台分配的权限组"在本服务真的生效 —— 持 community.post.moderate
// 的成员可处置他人的主题与短评，只有 community.post.create 的成员不行，而**老令牌**
// （claims 里没有 permissions）仍按角色兜底（admin 可治理）。未设置 COMMUNITY_TEST_DSN 时跳过。
func TestModerationEndpointsHonourPermissionCodes(t *testing.T) {
	dsn := testutil.DSN(t)
	db := testutil.Database(t)
	ctx := context.Background()

	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	if err = s.Init(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	for _, stmt := range []string{"DELETE FROM community.topic_tags", "DELETE FROM community.tags", "DELETE FROM community.posts", "DELETE FROM community.topics", "DELETE FROM community.boards"} {
		if _, err = db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("cleanup %s: %v", stmt, err)
		}
	}
	if err = Seed(ctx, db); err != nil {
		t.Fatalf("seed boards: %v", err)
	}

	entityID := uuid.NewString()
	catalogStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/relations") {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "entities": map[string]any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": entityID, "kind": "work", "title": "测试作品", "status": "published"})
	}))
	defer catalogStub.Close()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	verifier := newVerifier(t, srv.URL)
	router := gin.New()
	New(s, catalog.New(catalogStub.URL, 2*time.Second), verifier).Register(router)

	// 四个身份：发帖作者、只有发帖码的成员、持治理码的版主，以及没有 permissions 声明的老令牌管理员。
	authorID := uuid.NewString()
	authorToken := signTokenWith(t, key, kid, authorID, "user", []string{"member"}, []string{auth.PermissionPostCreate})
	memberToken := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"member"}, []string{auth.PermissionPostCreate})
	moderatorToken := signTokenWith(t, key, kid, uuid.NewString(), "user",
		[]string{"community_moderator"}, []string{auth.PermissionPostCreate, auth.PermissionPostModerate})
	legacyAdminToken := signToken(t, key, kid, "admin")

	call := func(method, path, body, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		return w
	}
	newTopic := func() string {
		t.Helper()
		w := call(http.MethodPost, "/api/community/topics", `{"board_code":"qa","title":"权限码校验","content":"正文"}`, authorToken)
		if w.Code != 200 {
			t.Fatalf("发主题 HTTP %d: %s", w.Code, w.Body.String())
		}
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		id, _ := out["id"].(string)
		if id == "" {
			t.Fatalf("发主题未返回 id: %s", w.Body.String())
		}
		return id
	}
	topicExists := func(id string) bool {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM community.topics WHERE id=$1", id).Scan(&n); err != nil {
			t.Fatalf("查主题: %v", err)
		}
		return n == 1
	}

	// 1) 只有 community.post.create 的成员删不了他人的主题（404 = 未命中，也不泄露存在性）。
	topic := newTopic()
	if w := call(http.MethodDelete, "/api/community/topics/"+topic, "", memberToken); w.Code != 404 {
		t.Fatalf("无治理码删他人主题应 404，实际 %d（%s）", w.Code, w.Body.String())
	}
	if !topicExists(topic) {
		t.Fatal("无治理码的删除请求不应真的删掉主题")
	}

	// 2) 持 community.post.moderate 的版主（角色仍是 user）可以删他人的主题。
	if w := call(http.MethodDelete, "/api/community/topics/"+topic, "", moderatorToken); w.Code != 200 {
		t.Fatalf("持治理码删他人主题应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	if topicExists(topic) {
		t.Fatal("持治理码的删除请求未生效")
	}

	// 3) 老令牌（没有 permissions 声明）按角色兜底：admin 仍可治理。
	legacyTopic := newTopic()
	if w := call(http.MethodDelete, "/api/community/topics/"+legacyTopic, "", legacyAdminToken); w.Code != 200 {
		t.Fatalf("老令牌 admin 删他人主题应 200，实际 %d（%s）", w.Code, w.Body.String())
	}

	newComment := func() string {
		t.Helper()
		w := call(http.MethodPost, "/api/community/entities/"+entityID+"/posts", `{"body":"短评"}`, authorToken)
		if w.Code != 200 {
			t.Fatalf("发短评 HTTP %d: %s", w.Code, w.Body.String())
		}
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		item, _ := out["item"].(map[string]any)
		id, _ := item["id"].(string)
		if id == "" {
			t.Fatalf("发短评未返回 id: %s", w.Body.String())
		}
		return id
	}

	// 4) 短评走同一套判定：无码 404、作者本人可删、持码可删他人的。
	comment := newComment()
	if w := call(http.MethodDelete, "/api/community/posts/"+comment, "", memberToken); w.Code != 404 {
		t.Fatalf("无治理码删他人短评应 404，实际 %d（%s）", w.Code, w.Body.String())
	}
	if w := call(http.MethodDelete, "/api/community/posts/"+comment, "", authorToken); w.Code != 200 {
		t.Fatalf("作者删自己短评应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	moderatedComment := newComment()
	if w := call(http.MethodDelete, "/api/community/posts/"+moderatedComment, "", moderatorToken); w.Code != 200 {
		t.Fatalf("持治理码删他人短评应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
}

// 发帖码是**真实闸门**：令牌带 permissions 却不含 community.post.create 时，三个写入口
// （发主题 / 回帖 / 短评）一律 403 forbidden 且不写库；持码则 200；
// 而老令牌（claims 里没有 permissions）按历史边界仍可发帖（见 auth.legacyOpenCodes）。
func TestPostCreateRequiresPermissionCode(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	authorToken := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"member"}, []string{auth.PermissionPostCreate})
	moderatorOnly := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"community_moderator"}, []string{auth.PermissionPostModerate})
	legacyToken := signToken(t, key, kid, "editor")

	topicCount := func() int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM community.topics").Scan(&n); err != nil {
			t.Fatalf("统计主题: %v", err)
		}
		return n
	}

	// 作者（持发帖码）先建一个主题，让"回帖"路由有真实目标。
	w := opsCall(t, router, http.MethodPost, "/api/community/topics", `{"board_code":"qa","title":"发帖码用例","content":"正文"}`, authorToken)
	if w.Code != 200 {
		t.Fatalf("持码发主题 HTTP %d: %s", w.Code, w.Body.String())
	}
	created := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	topicID, _ := created["id"].(string)
	if topicID == "" {
		t.Fatalf("发主题未返回 id: %s", w.Body.String())
	}

	// 1) 有 permissions、但只有治理码：三个入口都 403 forbidden，且主题数不变。
	before := topicCount()
	for _, tc := range []struct{ path, payload string }{
		{"/api/community/topics", `{"board_code":"qa","title":"无权发帖","content":"正文"}`},
		{"/api/community/topics/" + topicID + "/posts", `{"content":"无权回帖"}`},
		{"/api/community/entities/" + uuid.NewString() + "/posts", `{"body":"无权短评"}`},
	} {
		w = opsCall(t, router, http.MethodPost, tc.path, tc.payload, moderatorOnly)
		if w.Code != 403 || opsErrorCode(t, w) != "forbidden" {
			t.Fatalf("POST %s 缺发帖码应 403 forbidden，实际 %d（%s）", tc.path, w.Code, w.Body.String())
		}
	}
	if got := topicCount(); got != before {
		t.Fatalf("缺码的写请求不得落库：主题数 %d -> %d", before, got)
	}

	// 2) 持码：三个入口都放行（回帖与短评各写一行）。
	for _, tc := range []struct{ path, payload string }{
		{"/api/community/topics", `{"board_code":"qa","title":"有权发帖","content":"正文"}`},
		{"/api/community/topics/" + topicID + "/posts", `{"content":"有权回帖"}`},
		{"/api/community/entities/" + uuid.NewString() + "/posts", `{"body":"有权短评"}`},
	} {
		if w = opsCall(t, router, http.MethodPost, tc.path, tc.payload, authorToken); w.Code != 200 {
			t.Fatalf("POST %s 持码应 200，实际 %d（%s）", tc.path, w.Code, w.Body.String())
		}
	}

	// 3) 老令牌（没有 permissions）沿用"登录即可发帖"的历史边界，不能因为收口而断掉。
	if w = opsCall(t, router, http.MethodPost, "/api/community/topics", `{"board_code":"qa","title":"老令牌发帖","content":"正文"}`, legacyToken); w.Code != 200 {
		t.Fatalf("老令牌发主题应 200，实际 %d（%s）", w.Code, w.Body.String())
	}

	// 4) 匿名：401（鉴权先于权限判定）。
	if w = opsCall(t, router, http.MethodPost, "/api/community/topics", `{"board_code":"qa","title":"匿名","content":"正文"}`, ""); w.Code != 401 {
		t.Fatalf("匿名发主题应 401，实际 %d（%s）", w.Code, w.Body.String())
	}
}
