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

	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
	"github.com/MoeclubM/metafusion-community/internal/testutil"
)

// 需要真实 PostgreSQL 的回归：直接打**前端真正调用的那几个端点**
// （板块、主题列表、主题详情、回帖、短评流、收藏），把 handler 的 JSON 形状、
// 状态码与数据库交互一起验证。未设置 COMMUNITY_TEST_DSN 时整体跳过。
func TestForumEndpointsAgainstPostgres(t *testing.T) {
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
	// 清空后重新播种：板块是运营配置，种子必须幂等且包含评论板块。
	for _, stmt := range []string{"DELETE FROM community.topic_tags", "DELETE FROM community.tags", "DELETE FROM community.posts", "DELETE FROM community.topics", "DELETE FROM community.boards"} {
		if _, err = db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("cleanup %s: %v", stmt, err)
		}
	}
	if err = Seed(ctx, db); err != nil {
		t.Fatalf("seed boards: %v", err)
	}

	// 目录桩：实体一律可见，标题固定，用来验证"跨服务取标题"这条链路。
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
	token := signToken(t, key, kid, "editor")

	call := func(method, path, body, bearer string) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		var reader *strings.Reader
		if body == "" {
			reader = strings.NewReader("")
		} else {
			reader = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w, out
	}

	// 1) 板块列表：接口给前端的是**裸数组**（forum.go 的 c.JSON(200, boards)，前端 fetchBoards
	// 按 any[] 解析），不是 {"items": [...]}。种子必须生效，且包含发主题要用的 qa 与承载评论的 comment。
	w, _ := call(http.MethodGet, "/api/community/boards", "", "")
	if w.Code != 200 {
		t.Fatalf("板块列表 HTTP %d: %s", w.Code, w.Body.String())
	}
	var boards []struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &boards); err != nil {
		t.Fatalf("板块列表应为裸数组: %v %s", err, w.Body.String())
	}
	if len(boards) != len(defaultBoards) {
		t.Fatalf("板块列表条数 = %d，期望种子播种的 %d 个: %s", len(boards), len(defaultBoards), w.Body.String())
	}
	codes := map[string]bool{}
	for _, b := range boards {
		codes[b.Code] = true
	}
	for _, want := range []string{"qa", commentBoard} {
		if !codes[want] {
			t.Fatalf("板块列表缺少种子板块 %s: %s", want, w.Body.String())
		}
	}

	// 2) 发主题（带实体锚点与标签）。
	w, created := call(http.MethodPost, "/api/community/topics", `{"board_code":"qa","title":"标题","content":"正文","entity_id":"`+entityID+`","tag_names":["原盘"]}`, token)
	if w.Code != 200 {
		t.Fatalf("发主题 HTTP %d: %s", w.Code, w.Body.String())
	}
	topicID, _ := created["id"].(string)
	if topicID == "" {
		t.Fatalf("发主题未返回 id: %s", w.Body.String())
	}

	// 3) 主题列表：应能看到该主题，且锚定实体标题由目录服务补齐。
	w, list := call(http.MethodGet, "/api/community/topics?board_code=qa", "", "")
	if w.Code != 200 {
		t.Fatalf("主题列表 HTTP %d: %s", w.Code, w.Body.String())
	}
	raw, _ := json.Marshal(list)
	if !strings.Contains(string(raw), topicID) {
		t.Fatalf("主题列表缺少刚发布的主题: %s", string(raw))
	}
	if !strings.Contains(string(raw), "测试作品") {
		t.Fatalf("主题列表未补齐实体标题: %s", string(raw))
	}

	// 4) 回帖：楼层号从 1 开始。
	w, _ = call(http.MethodPost, "/api/community/topics/"+topicID+"/posts", `{"content":"一楼"}`, token)
	if w.Code != 200 {
		t.Fatalf("回帖 HTTP %d: %s", w.Code, w.Body.String())
	}

	// 5) 主题详情：带回复与标签，且浏览量自增。
	w, detail := call(http.MethodGet, "/api/community/topics/"+topicID, "", "")
	if w.Code != 200 {
		t.Fatalf("主题详情 HTTP %d: %s", w.Code, w.Body.String())
	}
	posts, _ := detail["posts"].([]any)
	if len(posts) != 1 {
		t.Fatalf("主题详情回复数 = %d: %s", len(posts), w.Body.String())
	}
	if tagList, _ := detail["tags"].([]any); len(tagList) != 1 {
		t.Fatalf("主题详情标签 = %v", detail["tags"])
	}

	// 6) 短评：写入评论板块，并出现在站点评论流里。
	w, _ = call(http.MethodPost, "/api/community/entities/"+entityID+"/posts", `{"body":"短评内容"}`, token)
	if w.Code != 200 {
		t.Fatalf("发短评 HTTP %d: %s", w.Code, w.Body.String())
	}
	w, feed := call(http.MethodGet, "/api/community/feed?entity_id="+entityID, "", "")
	if w.Code != 200 {
		t.Fatalf("评论流 HTTP %d: %s", w.Code, w.Body.String())
	}
	feedRaw, _ := json.Marshal(feed)
	if !strings.Contains(string(feedRaw), "短评内容") {
		t.Fatalf("评论流缺少刚发布的短评: %s", string(feedRaw))
	}
	// 关键词过滤：命中与不命中。
	if _, hit := call(http.MethodGet, "/api/community/feed?q=短评", "", ""); !strings.Contains(mustJSON(hit), "短评内容") {
		t.Fatal("关键词应命中短评")
	}
	if _, miss := call(http.MethodGet, "/api/community/feed?q=不存在的词", "", ""); strings.Contains(mustJSON(miss), "短评内容") {
		t.Fatal("不匹配的关键词不应返回该短评")
	}

	// 7) 收藏：切换 → 状态 → 我的列表。
	w, toggled := call(http.MethodPost, "/api/favorites/toggle", `{"target_type":"work","target_id":"`+entityID+`"}`, token)
	if w.Code != 200 || toggled["favorited"] != true {
		t.Fatalf("收藏切换失败: %d %s", w.Code, w.Body.String())
	}
	w, mine := call(http.MethodGet, "/api/favorites/mine", "", token)
	if w.Code != 200 || !strings.Contains(mustJSON(mine), entityID) {
		t.Fatalf("我的收藏未包含目标: %d %s", w.Code, mustJSON(mine))
	}

	// 8) 作者可删主题（级联删回复）。
	w, _ = call(http.MethodDelete, "/api/community/topics/"+topicID, "", token)
	if w.Code != 200 {
		t.Fatalf("删主题 HTTP %d: %s", w.Code, w.Body.String())
	}
	var left int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM community.posts WHERE topic_id=$1", topicID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("删主题后回复未级联删除: left=%d err=%v", left, err)
	}
	// 清理收藏，便于重复运行。
	_, _ = db.ExecContext(ctx, "DELETE FROM community.favorites WHERE user_id=$1", "11111111-1111-1111-1111-111111111111")
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
