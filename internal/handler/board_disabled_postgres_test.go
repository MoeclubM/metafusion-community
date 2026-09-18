package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// 板块停用（is_enabled=false）此前只在"发新主题"一处生效：读路径不过滤，停用板块的**存量**主题
// 与评论照旧公开，两个开关的分工（is_enabled = 整个板块停用 / show_in_feed = 主题进不进信息流，
// 前端消费）也只有代码注释。本用例逐条钉住整改后的语义，失败信息里写明是哪一条，便于定位回归：
//
//  1. 公开板块列表不含停用板块；持 community.board.manage 的**同一请求**含它（管理台能力回归）；
//  2. 公开主题列表不含停用板块的主题（其它板块不受牵连）；显式 board_code=<停用板块> 是 200 + 空 items；
//  3. 停用板块的主题详情公开 404 not_found（且不自增浏览量）、持 manage 码 200；
//  4. 向停用板块发新主题仍被拒（400 invalid_board，与改造前同一判据，权限语义不变）；
//  5. 停用板块的评论读路径（评论流 / 条目短评列表 / 单条短评）同样不公开，运营仍可见；
//  6. 重新启用后公开读全部恢复（停用必须是可逆的，否则运营不敢用这个开关）。
func TestDisabledBoardHiddenFromPublicReads(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	manageToken := signTokenWith(t, key, kid, uuid.NewString(), "user",
		[]string{"community_admin"}, []string{auth.PermissionBoardManage})
	authorToken := signTokenWith(t, key, kid, uuid.NewString(), "user",
		[]string{"member"}, []string{auth.PermissionPostCreate})
	entityID := uuid.NewString()

	setEnabled := func(code string, enabled bool) {
		t.Helper()
		w := opsCall(t, router, http.MethodPut, "/api/community/boards/"+code,
			fmt.Sprintf(`{"is_enabled":%t}`, enabled), manageToken)
		if w.Code != 200 {
			t.Fatalf("运营开关板块 %s(is_enabled=%t) HTTP %d: %s", code, enabled, w.Code, w.Body.String())
		}
	}
	// boardsByCode 把裸数组响应按 code 索引：列表接口的形状（裸数组）本身也是契约的一部分。
	boardsByCode := func(bearer string) map[string]map[string]any {
		t.Helper()
		w := opsCall(t, router, http.MethodGet, "/api/community/boards", "", bearer)
		if w.Code != 200 {
			t.Fatalf("板块列表 HTTP %d: %s", w.Code, w.Body.String())
		}
		var list []map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
			t.Fatalf("板块列表应为裸数组: %v %s", err, w.Body.String())
		}
		out := map[string]map[string]any{}
		for _, b := range list {
			code, _ := b["code"].(string)
			out[code] = b
		}
		return out
	}
	topicsOf := func(query, bearer string) (int, []map[string]any) {
		t.Helper()
		w := opsCall(t, router, http.MethodGet, "/api/community/topics"+query, "", bearer)
		if w.Code != 200 {
			t.Fatalf("主题列表 %s HTTP %d: %s", query, w.Code, w.Body.String())
		}
		payload := struct {
			Items []map[string]any `json:"items"`
			Total int              `json:"total"`
		}{}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("主题列表 %s 应是带 items/total 的 JSON 对象: %v %s", query, err, w.Body.String())
		}
		return payload.Total, payload.Items
	}
	hasTopic := func(items []map[string]any, id string) bool {
		for _, it := range items {
			if it["id"] == id {
				return true
			}
		}
		return false
	}
	newTopic := func(board, title string) string {
		t.Helper()
		w := opsCall(t, router, http.MethodPost, "/api/community/topics",
			`{"board_code":"`+board+`","title":"`+title+`","content":"正文"}`, authorToken)
		if w.Code != 200 {
			t.Fatalf("在板块 %s 发主题 HTTP %d: %s", board, w.Code, w.Body.String())
		}
		created := map[string]any{}
		_ = json.Unmarshal(w.Body.Bytes(), &created)
		id, _ := created["id"].(string)
		if id == "" {
			t.Fatalf("发主题未返回 id: %s", w.Body.String())
		}
		return id
	}
	feedRaw := func(bearer string) string {
		t.Helper()
		w := opsCall(t, router, http.MethodGet, "/api/community/feed?entity_id="+entityID, "", bearer)
		if w.Code != 200 {
			t.Fatalf("评论流 HTTP %d: %s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	// 存量内容必须在**停用之前**写入：停用后发帖会被拒（见第 4 条），本次要验证的正是存量内容是否还公开。
	qaTopic := newTopic("qa", "停用板块存量主题")
	otherTopic := newTopic("casual", "对照板块主题")
	w := opsCall(t, router, http.MethodPost, "/api/community/entities/"+entityID+"/posts", `{"body":"停用板块存量短评"}`, authorToken)
	if w.Code != 200 {
		t.Fatalf("发短评 HTTP %d: %s", w.Code, w.Body.String())
	}
	created := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	item, _ := created["item"].(map[string]any)
	commentID, _ := item["id"].(string)
	if commentID == "" {
		t.Fatalf("发短评未返回 item.id: %s", w.Body.String())
	}

	// 停用承载主题的 qa 与承载评论的 comment 两个板块。
	setEnabled("qa", false)
	setEnabled(commentBoard, false)
	// 前置事实核对：开关真的落库了。否则下面的"看不到"可能只是因为停用压根没生效。
	for _, code := range []string{"qa", commentBoard} {
		var enabled bool
		if err := db.QueryRowContext(ctx, "SELECT is_enabled FROM community.boards WHERE code=$1", code).Scan(&enabled); err != nil {
			t.Fatalf("查板块 %s 的 is_enabled: %v", code, err)
		}
		if enabled {
			t.Fatalf("前置条件不成立：板块 %s 应已停用", code)
		}
	}

	// 1) 板块列表：公开只列已启用板块；运营（管理台）仍拿到全部，含 is_enabled=false 的那两个。
	publicBoards := boardsByCode("")
	if _, ok := publicBoards["qa"]; ok {
		t.Fatal("语义 1：公开板块列表不得出现停用板块 qa")
	}
	if _, ok := publicBoards[commentBoard]; ok {
		t.Fatalf("语义 1：公开板块列表不得出现停用板块 %s", commentBoard)
	}
	if len(publicBoards) != len(defaultBoards)-2 {
		t.Fatalf("语义 1：公开板块列表条数 = %d，期望 %d（种子 %d 个减去 2 个停用）",
			len(publicBoards), len(defaultBoards)-2, len(defaultBoards))
	}
	opsBoards := boardsByCode(manageToken)
	qa, ok := opsBoards["qa"]
	if !ok {
		t.Fatal("语义 1：持 community.board.manage 的同一请求必须含停用板块 qa（管理台看不到就无法重新启用）")
	}
	if qa["is_enabled"] != false {
		t.Fatalf("语义 1：运营看到的停用板块应带 is_enabled=false，实际 %v", qa["is_enabled"])
	}
	if len(opsBoards) != len(defaultBoards) {
		t.Fatalf("语义 1：运营板块列表条数 = %d，期望全部 %d 个", len(opsBoards), len(defaultBoards))
	}

	// 2) 主题列表：默认列表与显式筛选都要过滤停用板块；其它板块不得受牵连。
	if total, items := topicsOf("?board_code=qa", ""); total != 0 || len(items) != 0 {
		t.Fatalf("语义 2：公开按停用板块筛选应是 200 + 空 items（不是 404），实际 total=%d items=%d", total, len(items))
	}
	total, items := topicsOf("", "")
	if hasTopic(items, qaTopic) {
		t.Fatal("语义 2：公开主题列表不得包含停用板块的主题")
	}
	if !hasTopic(items, otherTopic) || total != 1 {
		t.Fatalf("语义 2：公开主题列表应只剩启用板块的 1 个主题，实际 total=%d items=%v", total, items)
	}
	if _, items := topicsOf("?board_code=qa", manageToken); !hasTopic(items, qaTopic) {
		t.Fatalf("语义 2：持 manage 码时按停用板块筛选仍应看到主题：%v", items)
	}
	if _, items := topicsOf("", manageToken); !hasTopic(items, qaTopic) {
		t.Fatal("语义 2：持 manage 码的默认主题列表仍应含停用板块的主题")
	}

	// 3) 主题详情：停用板块对公开读按"不存在"处理；被过滤的请求也不该产生浏览计数。
	if w = opsCall(t, router, http.MethodGet, "/api/community/topics/"+qaTopic, "", ""); w.Code != 404 || opsErrorCode(t, w) != "not_found" {
		t.Fatalf("语义 3：停用板块的主题详情对公开读应 404 not_found，实际 %d（%s）", w.Code, w.Body.String())
	}
	var views int
	if err := db.QueryRowContext(ctx, "SELECT view_count FROM community.topics WHERE id=$1", qaTopic).Scan(&views); err != nil {
		t.Fatalf("查 view_count: %v", err)
	}
	if views != 0 {
		t.Fatalf("语义 3：不可见的主题不该被计入浏览量，实际 view_count=%d", views)
	}
	if w = opsCall(t, router, http.MethodGet, "/api/community/topics/"+qaTopic, "", manageToken); w.Code != 200 {
		t.Fatalf("语义 3：持 manage 码的主题详情应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	if w = opsCall(t, router, http.MethodGet, "/api/community/topics/"+otherTopic, "", ""); w.Code != 200 {
		t.Fatalf("语义 3：启用板块的主题详情应 200，实际 %d（%s）", w.Code, w.Body.String())
	}

	// 4) 发帖闸门保持原样（改造前就有的判据，本次不动权限语义）。
	if w = opsCall(t, router, http.MethodPost, "/api/community/topics",
		`{"board_code":"qa","title":"停用板块发帖","content":"正文"}`, authorToken); w.Code != 400 || opsErrorCode(t, w) != "invalid_board" {
		t.Fatalf("语义 4：向停用板块发主题应 400 invalid_board，实际 %d（%s）", w.Code, w.Body.String())
	}

	// 5) 评论读路径：评论板块停用后，评论流、条目短评、单条短评一并收敛。
	if body := feedRaw(""); strings.Contains(body, commentID) {
		t.Fatalf("语义 5：停用的评论板块不应出现在评论流：%s", body)
	}
	w = opsCall(t, router, http.MethodGet, "/api/community/entities/"+entityID+"/posts", "", "")
	if w.Code != 200 || strings.Contains(w.Body.String(), commentID) {
		t.Fatalf("语义 5：停用的评论板块不应出现在条目短评列表，实际 %d（%s）", w.Code, w.Body.String())
	}
	if w = opsCall(t, router, http.MethodGet, "/api/community/posts/"+commentID, "", ""); w.Code != 404 || opsErrorCode(t, w) != "not_found" {
		t.Fatalf("语义 5：停用的评论板块的单条短评应 404 not_found，实际 %d（%s）", w.Code, w.Body.String())
	}
	// 运营例外在评论路径上同样成立（否则管理台无法在停用板块里处置内容）。
	if w = opsCall(t, router, http.MethodGet, "/api/community/posts/"+commentID, "", manageToken); w.Code != 200 {
		t.Fatalf("语义 5：持 manage 码仍应能读停用板块的单条短评，实际 %d（%s）", w.Code, w.Body.String())
	}
	if body := feedRaw(manageToken); !strings.Contains(body, commentID) {
		t.Fatalf("语义 5：持 manage 码的评论流仍应含停用板块的短评：%s", body)
	}

	// 6) 停用可逆：重新启用后公开读全部恢复。
	setEnabled("qa", true)
	setEnabled(commentBoard, true)
	if _, ok := boardsByCode("")["qa"]; !ok {
		t.Fatal("语义 6：重新启用后板块必须回到公开列表（停用必须可逆）")
	}
	if w = opsCall(t, router, http.MethodGet, "/api/community/topics/"+qaTopic, "", ""); w.Code != 200 {
		t.Fatalf("语义 6：重新启用后公开主题详情应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	if _, items := topicsOf("?board_code=qa", ""); !hasTopic(items, qaTopic) {
		t.Fatalf("语义 6：重新启用后主题必须回到公开列表：%v", items)
	}
	if body := feedRaw(""); !strings.Contains(body, commentID) {
		t.Fatalf("语义 6：评论板块重新启用后短评应恢复公开：%s", body)
	}
	if w = opsCall(t, router, http.MethodGet, "/api/community/posts/"+commentID, "", ""); w.Code != 200 {
		t.Fatalf("语义 6：评论板块重新启用后单条短评应 200，实际 %d（%s）", w.Code, w.Body.String())
	}
}
