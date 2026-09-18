package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/store"
	"github.com/MoeclubM/metafusion-community/internal/testutil"
)

// 举报与申诉的真库端到端回归（缺口来源：审计 2026-09-19 S-30 / round2-func-gaps 第 3 条）。
//
// 走的是"用户举报一条评论 → 管理端队列出现 → 受理 → 被处置方申诉 → 申诉进队列 →
// 下线内容 → 处置完成"这条完整链路，用的都是前端真正会打的端点。
// 未设置 COMMUNITY_TEST_DSN 时整体跳过（与同包其它真库用例同口径）。
//
// 三条边界单独验证：
//   - 重复举报：同一人对同一对象的未终结举报回 409 duplicate_report，换个人举报同一条内容是允许的；
//   - 越权处置：没有 community.report.review 的身份打管理端一律 403，匿名 401；
//   - 下线内容不新造动作：resolve(enforcement=content_removed) 只**复核**内容已不存在，
//     内容还在时回 409 content_still_present（真正的删除走既有 DELETE /community/posts/{id}）。

func reportsCall(t *testing.T, router http.Handler, method, path, payload, bearer, requestID string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

// reportItemOf 从 {"item":{...}} 形状里取对象。
func reportItemOf(t *testing.T, body map[string]any, what string) map[string]any {
	t.Helper()
	item, ok := body["item"].(map[string]any)
	if !ok {
		t.Fatalf("%s 的响应缺少 item：%v", what, body)
	}
	return item
}

func TestReportsLifecycleAgainstPostgres(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)

	// request_id 带一次运行的随机前缀：审计断言是"这个 request_id 下恰好一行"，
	// 固定 id 会让上一轮运行的残留行在第 2 次运行时把断言打红（代码其实没错）。
	runID := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	rid := func(name string) string { return "rid-f3-" + runID + "-" + name }

	// 三个身份：举报人 / 内容作者（被处置方）/ 处理人（持 community.report.review）。
	reporterID, authorID, reviewerID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	reporter := signTokenWith(t, key, kid, reporterID, "user", []string{"member"}, []string{auth.PermissionPostCreate})
	author := signTokenWith(t, key, kid, authorID, "user", []string{"member"}, []string{auth.PermissionPostCreate})
	reviewer := signTokenWith(t, key, kid, reviewerID, "user", []string{"community_moderator"}, []string{
		auth.PermissionPostCreate, auth.PermissionPostModerate, auth.PermissionReportReview,
	})
	entityID := uuid.NewString()

	t.Cleanup(func() {
		// 只清本用例自己写的行：reports 的级联会带走 events / appeals。
		_, _ = db.ExecContext(ctx, "DELETE FROM community.reports WHERE reporter_id = ANY($1)", []string{reporterID, authorID, reviewerID})
		// 审计行也按本次运行的 request_id 前缀清掉：这套用例自己写的行不给同库的其它用例留噪音。
		_, _ = db.ExecContext(ctx, "DELETE FROM audit.audit_log WHERE request_id LIKE $1", "rid-f3-"+runID+"-%")
		_, _ = db.ExecContext(ctx, "DELETE FROM community.topics WHERE author_id = ANY($1)", []string{authorID, reporterID})
	})

	// 1) 作者发一条评论（被举报的对象）。
	w, posted := reportsCall(t, router, http.MethodPost, "/api/community/entities/"+entityID+"/posts",
		`{"body":"这是一条会被举报的评论内容"}`, author, rid("comment"))
	if w.Code != 200 {
		t.Fatalf("发评论 HTTP %d：%s", w.Code, w.Body.String())
	}
	commentID := reportItemOf(t, posted, "发评论")["id"].(string)

	// 2) 举报人提交举报（带证据 URL）。
	w, created := reportsCall(t, router, http.MethodPost, "/api/community/reports",
		`{"target_type":"comment","target_id":"`+commentID+`","reason":"spam","detail":"重复刷屏","evidence_url":"https://example.com/evidence"}`,
		reporter, rid("report"))
	if w.Code != 200 {
		t.Fatalf("提交举报 HTTP %d：%s", w.Code, w.Body.String())
	}
	report := reportItemOf(t, created, "提交举报")
	reportID := report["id"].(string)
	if report["status"] != store.ReportPending {
		t.Fatalf("新举报状态应为 pending，实际 %v", report["status"])
	}
	if report["can_appeal"] != false {
		t.Fatalf("待处理的举报不该显示可申诉：%v", report["can_appeal"])
	}
	// 举报正文不进审计（同一口径见 handler/community.go 的短评）：只记"举报了什么、以什么理由"。
	rows := waitAuditRows(t, ctx, db, rid("report"), 1)
	if rows[0].Action != "report.created" || rows[0].TargetID != reportID || rows[0].Result != auditSuccess {
		t.Fatalf("举报提交的审计行不符：%+v", rows[0])
	}
	if strings.Contains(rows[0].Changes, "重复刷屏") {
		t.Fatalf("举报说明不该进审计 changes：%s", rows[0].Changes)
	}

	// 3) 重复举报（同一人同一对象，未终结）→ 409，并且这一行审计记成失败。
	w, dup := reportsCall(t, router, http.MethodPost, "/api/community/reports",
		`{"target_type":"comment","target_id":"`+commentID+`","reason":"spam"}`, reporter, rid("dup"))
	if w.Code != 409 || dup["error"] != "duplicate_report" {
		t.Fatalf("重复举报应 409 duplicate_report，实际 %d / %s", w.Code, w.Body.String())
	}
	if failed := waitAuditRows(t, ctx, db, rid("dup"), 1); failed[0].Result != "failure" || failed[0].ErrorCode != "duplicate_report" {
		t.Fatalf("被拒的重复举报审计行应记失败码：%+v", failed[0])
	}

	// 4) 另一个人举报同一条内容不是重复（唯一索引按举报人 + 对象）。
	otherReporter, otherToken := uuid.NewString(), signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"member"}, []string{auth.PermissionPostCreate})
	w, second := reportsCall(t, router, http.MethodPost, "/api/community/reports",
		`{"target_type":"comment","target_id":"`+commentID+`","reason":"abuse"}`, otherToken, rid("second"))
	if w.Code != 200 {
		t.Fatalf("他人举报同一对象应放行，实际 %d：%s", w.Code, w.Body.String())
	}
	secondReportID := reportItemOf(t, second, "第二份举报")["id"].(string)
	_ = otherReporter

	// 5) 我的举报：状态与处理人可见，且带上目标快照（审核时正文可能已经不在）。
	w, mine := reportsCall(t, router, http.MethodGet, "/api/community/reports/mine?page=1&page_size=20", "", reporter, "")
	if w.Code != 200 {
		t.Fatalf("我的举报 HTTP %d：%s", w.Code, w.Body.String())
	}
	if total, _ := mine["total"].(float64); total != 1 {
		t.Fatalf("我的举报 total = %v，期望 1：%s", mine["total"], w.Body.String())
	}
	items := mine["items"].([]any)
	first := items[0].(map[string]any)
	if first["id"] != reportID {
		t.Fatalf("我的举报里没有刚提交的那条：%v", first["id"])
	}
	snapshot, _ := first["target_context"].(map[string]any)
	if snapshot["content_kind"] != "comment" || snapshot["excerpt"] == nil {
		t.Fatalf("目标快照缺少内容类型/摘要：%v", snapshot)
	}

	// 6) 匿名读自己的举报 → 401；无权限码打管理端 → 403（越权处置的判据在码上）。
	if w, _ := reportsCall(t, router, http.MethodGet, "/api/community/reports/mine", "", "", ""); w.Code != 401 {
		t.Fatalf("匿名读我的举报应 401，实际 %d", w.Code)
	}
	if w, _ := reportsCall(t, router, http.MethodGet, "/api/community/admin/reports", "", reporter, ""); w.Code != 403 {
		t.Fatalf("无 community.report.review 打队列应 403，实际 %d", w.Code)
	}
	if w, _ := reportsCall(t, router, http.MethodPost, "/api/community/admin/reports/"+reportID+"/accept", "{}", reporter, ""); w.Code != 403 {
		t.Fatalf("无权限码受理应 403，实际 %d", w.Code)
	}
	if w, _ := reportsCall(t, router, http.MethodGet, "/api/community/admin/reports", "", "", ""); w.Code != 401 {
		t.Fatalf("匿名打队列应 401，实际 %d", w.Code)
	}

	// 7) 管理端队列：默认列出全部，带过滤条件也能命中。
	w, queue := reportsCall(t, router, http.MethodGet, "/api/community/admin/reports?status=pending&target_type=comment&page=1&page_size=20", "", reviewer, "")
	if w.Code != 200 {
		t.Fatalf("队列 HTTP %d：%s", w.Code, w.Body.String())
	}
	if !containsReportID(queue, reportID) {
		t.Fatalf("队列里没有刚提交的举报：%s", w.Body.String())
	}
	w, filtered := reportsCall(t, router, http.MethodGet, "/api/community/admin/reports?q="+reportID, "", reviewer, "")
	if w.Code != 200 || !containsReportID(filtered, reportID) {
		t.Fatalf("按关键词过滤没命中：%d %s", w.Code, w.Body.String())
	}
	if w, _ := reportsCall(t, router, http.MethodGet, "/api/community/admin/reports?status=nonsense", "", reviewer, ""); w.Code != 400 {
		t.Fatalf("非法状态过滤应 400，实际 %d", w.Code)
	}

	// 8) 详情：时间线有 created、目标此刻还在、还没有申诉。
	w, detail := reportsCall(t, router, http.MethodGet, "/api/community/admin/reports/"+reportID, "", reviewer, "")
	if w.Code != 200 {
		t.Fatalf("详情 HTTP %d：%s", w.Code, w.Body.String())
	}
	if present, _ := detail["target_present"].(bool); !present {
		t.Fatalf("目标内容此刻应还在：%s", w.Body.String())
	}
	if events := detail["events"].([]any); len(events) != 1 || events[0].(map[string]any)["kind"] != "created" {
		t.Fatalf("时间线应只有一条 created：%v", detail["events"])
	}
	if appeals := detail["appeals"].([]any); len(appeals) != 0 {
		t.Fatalf("此刻不该有申诉：%v", appeals)
	}

	// 9) 受理：状态推进 + 处理人落库 + 审计留痕。
	w, accepted := reportsCall(t, router, http.MethodPost, "/api/community/admin/reports/"+reportID+"/accept", `{"note":"已受理，等待处置"}`, reviewer, rid("accept"))
	if w.Code != 200 {
		t.Fatalf("受理 HTTP %d：%s", w.Code, w.Body.String())
	}
	acceptedItem := reportItemOf(t, accepted, "受理")
	if acceptedItem["status"] != store.ReportAccepted || acceptedItem["reviewer_name"] != "kana" {
		t.Fatalf("受理后的状态/处理人不符：%v", acceptedItem)
	}
	if row := waitAuditRows(t, ctx, db, rid("accept"), 1)[0]; row.Action != "report.accepted" {
		t.Fatalf("受理的动作码应为 report.accepted：%+v", row)
	}
	// 终态不可再受理（already accepted）。
	if w, _ := reportsCall(t, router, http.MethodPost, "/api/community/admin/reports/"+reportID+"/accept", "{}", reviewer, ""); w.Code != 409 {
		t.Fatalf("重复受理应 409，实际 %d", w.Code)
	}

	// 10) 被处置方申诉：一次成功、第二次 409，非被处置方 403。
	w, appealCreated := reportsCall(t, router, http.MethodPost, "/api/community/reports/"+reportID+"/appeal",
		`{"body":"这条评论没有违规，请复核"}`, author, rid("appeal"))
	if w.Code != 200 {
		t.Fatalf("申诉 HTTP %d：%s", w.Code, w.Body.String())
	}
	appealID := reportItemOf(t, appealCreated, "申诉")["id"].(string)
	if w, dup := reportsCall(t, router, http.MethodPost, "/api/community/reports/"+reportID+"/appeal", `{"body":"再来一次"}`, author, ""); w.Code != 409 || dup["error"] != "duplicate_appeal" {
		t.Fatalf("重复申诉应 409 duplicate_appeal，实际 %d / %s", w.Code, w.Body.String())
	}
	if w, forbidden := reportsCall(t, router, http.MethodPost, "/api/community/reports/"+reportID+"/appeal", `{"body":"我不是被处置方"}`, reporter, ""); w.Code != 403 || forbidden["error"] != "not_appealed_party" {
		t.Fatalf("非被处置方申诉应 403 not_appealed_party，实际 %d / %s", w.Code, w.Body.String())
	}

	// 11) 申诉进队列（默认只看待处理），并带上所属举报的上下文。
	w, appeals := reportsCall(t, router, http.MethodGet, "/api/community/admin/appeals?page=1&page_size=20", "", reviewer, "")
	if w.Code != 200 {
		t.Fatalf("申诉队列 HTTP %d：%s", w.Code, w.Body.String())
	}
	if !containsAppealID(appeals, appealID) {
		t.Fatalf("申诉队列里没有刚提交的申诉：%s", w.Body.String())
	}
	for _, raw := range appeals["items"].([]any) {
		item := raw.(map[string]any)
		if item["id"] == appealID {
			if item["report_id"] != reportID || item["target_id"] != commentID || item["status"] != store.AppealPending {
				t.Fatalf("申诉队列行缺少举报上下文：%v", item)
			}
		}
	}

	// 12) 处理申诉：终态不可再改。
	w, reviewed := reportsCall(t, router, http.MethodPost, "/api/community/admin/appeals/"+appealID+"/review",
		`{"status":"rejected","note":"维持原判"}`, reviewer, rid("appeal-review"))
	if w.Code != 200 {
		t.Fatalf("处理申诉 HTTP %d：%s", w.Code, w.Body.String())
	}
	if reportItemOf(t, reviewed, "处理申诉")["status"] != store.AppealRejected {
		t.Fatalf("申诉状态应为 rejected：%v", reviewed)
	}
	if w, _ := reportsCall(t, router, http.MethodPost, "/api/community/admin/appeals/"+appealID+"/review", `{"status":"accepted","note":"改判"}`, reviewer, ""); w.Code != 409 {
		t.Fatalf("终态申诉再审应 409，实际 %d", w.Code)
	}
	// 缺说明回 400 note_required：请求体校验先于状态机判定，
	// 调用方的错误不因"这条申诉已经是终态"而改变码。
	if w, missing := reportsCall(t, router, http.MethodPost, "/api/community/admin/appeals/"+appealID+"/review", `{"status":"accepted"}`, reviewer, ""); w.Code != 400 || missing["error"] != "note_required" {
		t.Fatalf("缺说明应 400 note_required，实际 %d / %s", w.Code, w.Body.String())
	}

	// 13) 处置：内容还在时不允许记"已下线"（409），必须先用既有删除端点下线。
	w, blocked := reportsCall(t, router, http.MethodPost, "/api/community/admin/reports/"+reportID+"/resolve",
		`{"enforcement":"content_removed","note":"已下线"}`, reviewer, rid("resolve-blocked"))
	if w.Code != 409 || blocked["error"] != "content_still_present" {
		t.Fatalf("内容还在时应 409 content_still_present，实际 %d / %s", w.Code, w.Body.String())
	}
	w, _ = reportsCall(t, router, http.MethodDelete, "/api/community/posts/"+commentID, "", reviewer, rid("delete"))
	if w.Code != 200 {
		t.Fatalf("既有删除端点删除短评 HTTP %d：%s", w.Code, w.Body.String())
	}
	w, resolved := reportsCall(t, router, http.MethodPost, "/api/community/admin/reports/"+reportID+"/resolve",
		`{"enforcement":"content_removed","note":"内容已下线"}`, reviewer, rid("resolve"))
	if w.Code != 200 {
		t.Fatalf("处置 HTTP %d：%s", w.Code, w.Body.String())
	}
	resolvedItem := reportItemOf(t, resolved, "处置")
	if resolvedItem["status"] != store.ReportResolved || resolvedItem["enforcement"] != store.EnforcementContentRemoved {
		t.Fatalf("处置后的状态/结论不符：%v", resolvedItem)
	}
	if row := waitAuditRows(t, ctx, db, rid("resolve"), 1)[0]; row.Action != "report.resolved" {
		t.Fatalf("处置的动作码应为 report.resolved：%+v", row)
	}

	// 14) 举报人侧：状态、处理人、处置结论与时间线都可见（申诉结果同样回给举报人）。
	w, mine = reportsCall(t, router, http.MethodGet, "/api/community/reports/mine", "", reporter, "")
	first = mine["items"].([]any)[0].(map[string]any)
	if first["status"] != store.ReportResolved || first["reviewer_name"] != "kana" || first["review_note"] != "内容已下线" {
		t.Fatalf("举报人看到的处置结果不完整：%v", first)
	}
	if appeal, _ := first["appeal"].(map[string]any); appeal == nil || appeal["status"] != store.AppealRejected {
		t.Fatalf("举报人应看到申诉结论：%v", first["appeal"])
	}
	if first["can_appeal"] != false {
		t.Fatalf("举报人（非被处置方）任何时候都不该显示可申诉：%v", first["can_appeal"])
	}

	// 15) 驳回：说明必填；驳回后不能再申诉（报告已经是终态）。
	w, _ = reportsCall(t, router, http.MethodPost, "/api/community/admin/reports/"+secondReportID+"/reject", `{}`, reviewer, "")
	if w.Code != 400 {
		t.Fatalf("驳回缺说明应 400，实际 %d", w.Code)
	}
	w, rejected := reportsCall(t, router, http.MethodPost, "/api/community/admin/reports/"+secondReportID+"/reject",
		`{"note":"不构成违规"}`, reviewer, rid("reject"))
	if w.Code != 200 {
		t.Fatalf("驳回 HTTP %d：%s", w.Code, w.Body.String())
	}
	if reportItemOf(t, rejected, "驳回")["status"] != store.ReportRejected {
		t.Fatalf("驳回后状态应为 rejected：%v", rejected)
	}
	if w, blockedAppeal := reportsCall(t, router, http.MethodPost, "/api/community/reports/"+secondReportID+"/appeal",
		`{"body":"不服"}`, author, ""); w.Code != 409 || blockedAppeal["error"] != "report_not_disposed" {
		t.Fatalf("对已驳回的举报申诉应 409 report_not_disposed，实际 %d / %s", w.Code, w.Body.String())
	}
	if row := waitAuditRows(t, ctx, db, rid("reject"), 1)[0]; row.Action != "report.rejected" {
		t.Fatalf("驳回的动作码应为 report.rejected：%+v", row)
	}

	// 16) 非本地对象（实体）：可以举报（只存不透明引用），但不能记"已下线内容"。
	w, entityReport := reportsCall(t, router, http.MethodPost, "/api/community/reports",
		`{"target_type":"entity","target_id":"`+entityID+`","reason":"misinformation"}`, reporter, rid("entity"))
	if w.Code != 200 {
		t.Fatalf("举报实体 HTTP %d：%s", w.Code, w.Body.String())
	}
	entityReportID := reportItemOf(t, entityReport, "举报实体")["id"].(string)
	if w, unsupported := reportsCall(t, router, http.MethodPost, "/api/community/admin/reports/"+entityReportID+"/resolve",
		`{"enforcement":"content_removed","note":"下线实体"}`, reviewer, ""); w.Code != 400 || unsupported["error"] != "enforcement_not_supported" {
		t.Fatalf("实体目标记内容已下线应 400 enforcement_not_supported，实际 %d / %s", w.Code, w.Body.String())
	}
	// 目标不存在（短评）：回 404，而不是插一条永远无法下线的举报。
	if w, _ := reportsCall(t, router, http.MethodPost, "/api/community/reports",
		`{"target_type":"comment","target_id":"`+uuid.NewString()+`","reason":"spam"}`, reporter, ""); w.Code != 404 {
		t.Fatalf("举报不存在的短评应 404，实际 %d", w.Code)
	}
	// 非法理由 / 非法证据 URL：回 400（词表由服务端持有）。
	if w, _ := reportsCall(t, router, http.MethodPost, "/api/community/reports",
		`{"target_type":"entity","target_id":"`+entityID+`","reason":"not-a-reason"}`, reporter, ""); w.Code != 400 {
		t.Fatalf("非法理由应 400，实际 %d", w.Code)
	}
	if w, _ := reportsCall(t, router, http.MethodPost, "/api/community/reports",
		`{"target_type":"entity","target_id":"`+entityID+`","reason":"spam","evidence_url":"javascript:alert(1)"}`, reporter, ""); w.Code != 400 {
		t.Fatalf("非法证据 URL 应 400，实际 %d", w.Code)
	}

	// 17) 滥用防线：同一人滚动 24 小时内超过配额回 429 + Retry-After。
	flooder := uuid.NewString()
	floodToken := signTokenWith(t, key, kid, flooder, "user", []string{"member"}, []string{auth.PermissionPostCreate})
	for i := 0; i < reportQuota; i++ {
		w, body := reportsCall(t, router, http.MethodPost, "/api/community/reports",
			`{"target_type":"entity","target_id":"`+uuid.NewString()+`","reason":"other"}`, floodToken, "")
		if w.Code != 200 {
			t.Fatalf("第 %d 条举报应放行，实际 %d：%s", i+1, w.Code, body)
		}
	}
	w, throttled := reportsCall(t, router, http.MethodPost, "/api/community/reports",
		`{"target_type":"entity","target_id":"`+uuid.NewString()+`","reason":"other"}`, floodToken, rid("quota"))
	if w.Code != http.StatusTooManyRequests || throttled["error"] != "rate_limited" {
		t.Fatalf("超配额应 429 rate_limited，实际 %d / %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After（与站点既有限流的契约一致）")
	}
	if row := waitAuditRows(t, ctx, db, rid("quota"), 1)[0]; row.Result != "failure" || row.ErrorCode != "rate_limited" {
		t.Fatalf("被限流的举报审计行应记 rate_limited：%+v", row)
	}
}

// 队列取数失败必须回 500（而不是空列表）：管理台把"队列空了"与"取数失败"分开显示，
// 前提是服务端把失败如实回成失败。这里用一个**已关闭连接**的存储层造出真实故障：
// 空列表与故障在响应上必须可区分（契约漂移被讲成"队列已清空"正是本轮踩过的坑）。
func TestQueueFailsLoudlyWhenStoreUnavailable(t *testing.T) {
	dsn := testutil.DSN(t)
	s, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err = s.Init(context.Background()); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	h := New(s, nil, nil)
	router := gin.New()
	router.GET("/reports", h.listReports)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/reports", nil))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "module_error") {
		t.Fatalf("存储层故障应回 500 module_error，实际 %d / %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/reports?status=pending", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("带过滤条件的故障请求同样应 500，实际 %d", w.Code)
	}
}

// auditSuccess 是成功审计行的 result 取值（这里只用来读断言，避免再引一个包名）。
const auditSuccess = "success"

// containsReportID / containsAppealID 判定列表响应里是否含某条 id。
func containsReportID(body map[string]any, id string) bool {
	items, _ := body["items"].([]any)
	for _, raw := range items {
		row, _ := raw.(map[string]any)
		report, _ := row["report"].(map[string]any)
		if report != nil && report["id"] == id {
			return true
		}
		if row["id"] == id {
			return true
		}
	}
	return false
}

func containsAppealID(body map[string]any, id string) bool {
	items, _ := body["items"].([]any)
	for _, raw := range items {
		if row, _ := raw.(map[string]any); row != nil && row["id"] == id {
			return true
		}
	}
	return false
}
