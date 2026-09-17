package handler

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// 帖子治理列表的真库用例：跨主题巡检、权限闸门、分页边界、关键词，以及一条硬约束——
// **只读不碰 view_count**（这正是它存在的理由：主题详情会把浏览量 +1）。
// 未设置 COMMUNITY_TEST_DSN 时整体跳过。
func TestModerationPostListAgainstPostgres(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	authorID := uuid.NewString()
	authorToken := signTokenWith(t, key, kid, authorID, "user", []string{"member"}, []string{auth.PermissionPostCreate})
	memberToken := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"member"}, []string{auth.PermissionPostCreate})
	moderatorToken := signTokenWith(t, key, kid, uuid.NewString(), "user",
		[]string{"community_moderator"}, []string{auth.PermissionPostCreate, auth.PermissionPostModerate})
	wildcardToken := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"admin"}, []string{"*"})

	newTopic := func(board, title string) string {
		t.Helper()
		w := opsCall(t, router, http.MethodPost, "/api/community/topics",
			`{"board_code":"`+board+`","title":"`+title+`","content":"主题正文"}`, authorToken)
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
	newReply := func(topicID, body string) string {
		t.Helper()
		w := opsCall(t, router, http.MethodPost, "/api/community/topics/"+topicID+"/posts",
			`{"content":"`+body+`"}`, authorToken)
		if w.Code != 200 {
			t.Fatalf("回帖 HTTP %d: %s", w.Code, w.Body.String())
		}
		out := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		id, _ := out["id"].(string)
		if id == "" {
			t.Fatalf("回帖未返回 id: %s", w.Body.String())
		}
		return id
	}

	// 两个板块、两个主题、三条回复（其中一条超长，用来验证摘要截断）。
	qaTopic := newTopic("qa", "阿尔法专辑考据")
	casualTopic := newTopic("casual", "随便聊聊")
	first := newReply(qaTopic, "第一条回复")
	second := newReply(qaTopic, "第二条回复 alphabetical-marker")
	longBody := strings.Repeat("长", moderationExcerptRunes+20)
	third := newReply(casualTopic, longBody)

	// 浏览量先置成已知值：治理列表读完必须一字不变。
	if _, err := db.ExecContext(ctx, "UPDATE community.topics SET view_count=7"); err != nil {
		t.Fatalf("置 view_count: %v", err)
	}
	viewCounts := func() map[string]int {
		t.Helper()
		rows, err := db.QueryContext(ctx, "SELECT id::text, view_count FROM community.topics")
		if err != nil {
			t.Fatalf("查 view_count: %v", err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var id string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				t.Fatalf("扫 view_count: %v", err)
			}
			out[id] = n
		}
		return out
	}
	before := viewCounts()

	type listItem struct {
		ID         string `json:"id"`
		TopicID    string `json:"topic_id"`
		TopicTitle string `json:"topic_title"`
		BoardCode  string `json:"board_code"`
		AuthorID   string `json:"author_id"`
		AuthorName string `json:"author_name"`
		PostNumber int    `json:"post_number"`
		Excerpt    string `json:"excerpt"`
		Truncated  bool   `json:"truncated"`
		CreatedAt  string `json:"created_at"`
		UpdatedAt  string `json:"updated_at"`
	}
	type listResponse struct {
		Items []listItem `json:"items"`
		Total int        `json:"total"`
	}
	list := func(query, bearer string) (int, listResponse, string) {
		t.Helper()
		w := opsCall(t, router, http.MethodGet, "/api/community/posts"+query, "", bearer)
		if w.Code != 200 {
			return w.Code, listResponse{}, w.Body.String()
		}
		var out listResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("解析列表响应: %v %s", err, w.Body.String())
		}
		return w.Code, out, w.Body.String()
	}

	// 1) 闸门：匿名 401、只有发帖码 403、治理码与 * 通配 200。
	if code, _, body := list("", ""); code != 401 || !strings.Contains(body, "authentication_required") {
		t.Fatalf("匿名应 401 authentication_required，实际 %d %s", code, body)
	}
	if code, _, body := list("", memberToken); code != 403 || !strings.Contains(body, "forbidden") {
		t.Fatalf("缺治理码应 403 forbidden，实际 %d %s", code, body)
	}
	code, page, body := list("", moderatorToken)
	if code != 200 {
		t.Fatalf("持治理码应 200，实际 %d %s", code, body)
	}
	if code, _, body := list("", wildcardToken); code != 200 {
		t.Fatalf("* 通配应 200，实际 %d %s", code, body)
	}

	// 2) 治理上下文：跨主题、带所属主题与板块、作者快照、楼层号、摘要。
	if page.Total != 3 || len(page.Items) != 3 {
		t.Fatalf("应返回 3 条回复，实际 total=%d len=%d %s", page.Total, len(page.Items), body)
	}
	if page.Items[0].CreatedAt < page.Items[len(page.Items)-1].CreatedAt {
		t.Fatalf("应 newest-first（created_at 不增）：%v", page.Items)
	}
	byID := map[string]listItem{}
	for _, item := range page.Items {
		byID[item.ID] = item
	}
	second2, ok := byID[second]
	if !ok {
		t.Fatalf("缺少第二条回复 %s：%s", second, body)
	}
	if second2.TopicID != qaTopic || second2.TopicTitle != "阿尔法专辑考据" || second2.BoardCode != "qa" {
		t.Fatalf("治理上下文不完整：%+v", second2)
	}
	// 楼层号只钉"相对递增"：服务端的首楼是 COALESCE(MAX(post_number),1)+1 = 2，
	// 治理台只展示它，不重定义它。
	first2, ok := byID[first]
	if !ok {
		t.Fatalf("缺少第一条回复 %s：%s", first, body)
	}
	if second2.PostNumber != first2.PostNumber+1 {
		t.Fatalf("楼层号应逐条递增：first=%d second=%d", first2.PostNumber, second2.PostNumber)
	}
	if second2.AuthorID != authorID || second2.AuthorName == "" {
		t.Fatalf("作者应为发帖人快照：%+v", second2)
	}
	if second2.CreatedAt == "" || second2.UpdatedAt == "" {
		t.Fatalf("时间字段不应为空：%+v", second2)
	}
	// 作者快照为空（老数据只写了空串）时由 store 的 SQL 回落到 Anonymous。
	if _, err := db.ExecContext(ctx, "UPDATE community.posts SET author_name='' WHERE id=$1", first); err != nil {
		t.Fatalf("清空作者快照: %v", err)
	}
	_, refreshed, _ := list("?page_size=100", moderatorToken)
	for _, item := range refreshed.Items {
		if item.ID == first && item.AuthorName != "Anonymous" {
			t.Fatalf("空作者快照应回落 Anonymous，实际 %q", item.AuthorName)
		}
	}
	long, ok := byID[third]
	if !ok {
		t.Fatalf("缺少超长回复 %s：%s", third, body)
	}
	if !long.Truncated {
		t.Fatalf("超长正文应标记 truncated：%+v", long)
	}
	if n := utf8.RuneCountInString(long.Excerpt); n != moderationExcerptRunes+1 {
		t.Fatalf("摘要应为 %d 个字符（上限 + 省略号），实际 %d", moderationExcerptRunes+1, n)
	}
	if long.BoardCode != "casual" || long.TopicTitle != "随便聊聊" {
		t.Fatalf("超长回复的所属主题不对：%+v", long)
	}

	// 3) 只读：治理列表不得改动 view_count。
	if after := viewCounts(); len(after) != len(before) || after[qaTopic] != before[qaTopic] || after[casualTopic] != before[casualTopic] {
		t.Fatalf("治理列表改动了 view_count：before=%v after=%v", before, after)
	}

	// 4) 关键词：命中主题标题、命中回复正文、空 q 全量、无命中为空且 total=0。
	_, hit, _ := list("?q="+url.QueryEscape("阿尔法"), moderatorToken)
	if hit.Total != 2 || len(hit.Items) != 2 {
		t.Fatalf("按主题标题搜索应命中 2 条，实际 total=%d len=%d", hit.Total, len(hit.Items))
	}
	_, hit, _ = list("?q="+url.QueryEscape("alphabetical-marker"), moderatorToken)
	if hit.Total != 1 || len(hit.Items) != 1 || hit.Items[0].ID != second {
		t.Fatalf("按正文搜索应只命中第二条，实际 %+v", hit)
	}
	_, hit, _ = list("?q="+url.QueryEscape("   "), moderatorToken)
	if hit.Total != 3 {
		t.Fatalf("纯空白 q 应视为空（全量 3 条），实际 %d", hit.Total)
	}
	_, hit, _ = list("?q="+url.QueryEscape("不存在的关键词"), moderatorToken)
	if hit.Total != 0 || len(hit.Items) != 0 {
		t.Fatalf("无命中应为空且 total=0，实际 %+v", hit)
	}

	// 5) 分页边界：窗口不重叠、越界页收敛为空、非法 page/page_size 回落缺省。
	_, p1, _ := list("?page=1&page_size=1", moderatorToken)
	_, p2, _ := list("?page=2&page_size=1", moderatorToken)
	if len(p1.Items) != 1 || len(p2.Items) != 1 || p1.Items[0].ID == p2.Items[0].ID {
		t.Fatalf("page_size=1 的前两页应各 1 条且不重复：%+v / %+v", p1.Items, p2.Items)
	}
	if p1.Total != 3 || p2.Total != 3 {
		t.Fatalf("total 是整段结果数，不随窗口变化：%d / %d", p1.Total, p2.Total)
	}
	_, far, _ := list("?page=99&page_size=2", moderatorToken)
	if far.Total != 3 || len(far.Items) != 0 {
		t.Fatalf("越界页应收敛为空列表且 total 不变，实际 %+v", far)
	}
	_, defaults, _ := list("?page=0&page_size=0", moderatorToken)
	if len(defaults.Items) != 3 {
		t.Fatalf("page=0/page_size=0 应回落缺省（page=1、页宽 20），实际 %d 条", len(defaults.Items))
	}
	_, capped, _ := list("?page_size=101", moderatorToken)
	if len(capped.Items) != 3 {
		t.Fatalf("page_size 超过上限应回落缺省页宽，实际 %d 条", len(capped.Items))
	}

	// 6) 全窗口覆盖与列表口径一致：逐页取回后与一次取全的集合相同。
	_, whole, _ := list("?page_size=100", moderatorToken)
	got := []string{}
	for _, item := range whole.Items {
		got = append(got, item.ID)
	}
	want := []string{first, second, third}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("列表集合应为三条回复：got=%v want=%v", got, want)
	}
}
