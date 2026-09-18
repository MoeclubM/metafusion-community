package handler

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/audit"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// 举报与申诉的**管理端**契约：处理队列、详情、三种处置动作、申诉队列与处理。
// 用户端（提交举报 / 我的举报 / 提交申诉）与请求形状、校验工具在 reports.go；
// 两条队列共用一张社区权限码 community.report.review（见 auth.PermissionReportReview）。
//
// 处置动作不新造能力：下线内容走既有删除端点（这里只复核内容确实已不存在），
// 封禁用户走账号服务的既有封禁端点。

// ── 管理端 ──

// listReports 是处理队列：状态（可多选，逗号分隔）+ 类型 + 关键词过滤，page/page_size 分页。
// 取不到数据回 500（不是空列表）：管理端把"队列空了"与"取数失败"分开显示的前提是
// 服务端把失败如实回成失败。
func (h *Handler) listReports(c *gin.Context) {
	filter := store.ReportFilter{
		Statuses:   parseStatuses(c.Query("status")),
		TargetType: strings.TrimSpace(c.Query("target_type")),
		Query:      strings.TrimSpace(c.Query("q")),
	}
	if filter.TargetType != "" && !store.ValidReportTargetType(filter.TargetType) {
		fail(c, 400, "invalid_target_type")
		return
	}
	for _, s := range filter.Statuses {
		if !validReportStatus(s) {
			fail(c, 400, "invalid_status")
			return
		}
	}
	limit, offset := pagingPageSize(c, moderationDefaultPageSize)
	items, total, err := h.store.ListReports(c.Request.Context(), filter, limit, offset)
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	appeals, err := h.store.AppealsForReports(c.Request.Context(), reportIDs(items))
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	out := make([]gin.H, 0, len(items))
	for _, r := range items {
		row := gin.H{"report": r, "appeal": nil}
		if list := appeals[r.ID]; len(list) > 0 {
			row["appeal"] = newAppealItem(list[0])
		}
		out = append(out, row)
	}
	c.JSON(200, gin.H{"items": out, "total": total})
}

// reportDetail 是详情：报告本体 + 时间线 + 该报告下的申诉。
// 时间线让"受理了但没有处置"这种中间态可见——只看报告的 updated_at 看不出经历过什么。
func (h *Handler) reportDetail(c *gin.Context) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		fail(c, 404, "not_found")
		return
	}
	ctx := c.Request.Context()
	report, err := h.store.ReportByID(ctx, id)
	if errors.Is(err, store.ErrReportNotFound) {
		fail(c, 404, "not_found")
		return
	}
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	events, err := h.store.ReportEvents(ctx, id)
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	appeals, err := h.store.ListReportAppeals(ctx, id)
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	items := make([]appealItem, 0, len(appeals))
	for _, a := range appeals {
		items = append(items, *newAppealItem(a))
	}
	// 目标此刻是否还在（本地内容）：处理人据此判断"下线内容"是否还能做、
	// 以及"内容已不存在"是被人删了还是从来没存在过。
	present, err := h.targetPresent(c, report)
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	c.JSON(200, gin.H{"item": report, "events": events, "appeals": items, "target_present": present})
}

// acceptReport：受理（pending → accepted），说明可选。
func (h *Handler) acceptReport(c *gin.Context) {
	note, ok := reviewNote(c, false)
	if !ok {
		return
	}
	h.applyTransition(c, store.ReportAccepted, []string{store.ReportPending}, "", note)
}

// rejectReport：驳回（pending / accepted → rejected），说明必填——
// "为什么不成立"是驳回唯一要向举报人交代的东西。
func (h *Handler) rejectReport(c *gin.Context) {
	note, ok := reviewNote(c, true)
	if !ok {
		return
	}
	h.applyTransition(c, store.ReportRejected, []string{store.ReportPending, store.ReportAccepted}, "", note)
}

// resolveReport：处置（pending / accepted → resolved），需要处置结论 + 说明。
//
// 两条复核（都不是"新造动作"，而是确认既有动作确实发生过）：
//   - enforcement=content_removed：目标必须是本地内容，且**此刻已经不存在**——
//     下线由既有删除端点完成（评论 / 帖子面板就在隔壁页签）。还在 → 409 content_still_present。
//   - enforcement=user_banned：必须有可封禁的对端（本地能解析出作者，或目标就是用户本人）。
//     封禁动作在账号服务执行，这里不做也无法代查（本服务不持有账号库、也没有封禁权限）。
//
// enforcement=none：警告 / 转交等不带强制动作的处置，只留结论与说明。
func (h *Handler) resolveReport(c *gin.Context) {
	var in struct {
		Enforcement string `json:"enforcement"`
		Note        string `json:"note"`
	}
	if !body(c, &in) {
		return
	}
	enforcement := strings.TrimSpace(in.Enforcement)
	switch enforcement {
	case store.EnforcementNone, store.EnforcementContentRemoved, store.EnforcementUserBanned:
	default:
		fail(c, 400, "invalid_enforcement")
		return
	}
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		fail(c, 404, "not_found")
		return
	}
	report, err := h.store.ReportByID(c.Request.Context(), id)
	if errors.Is(err, store.ErrReportNotFound) {
		fail(c, 404, "not_found")
		return
	}
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	switch enforcement {
	case store.EnforcementContentRemoved:
		if !localContentTarget(report.TargetType) {
			fail(c, 400, "enforcement_not_supported")
			return
		}
		present, err := h.targetPresent(c, report)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		if present {
			fail(c, 409, "content_still_present")
			return
		}
	case store.EnforcementUserBanned:
		if report.TargetAuthorID == "" {
			fail(c, 400, "enforcement_not_supported")
			return
		}
	}
	note := strings.TrimSpace(in.Note)
	if note == "" {
		fail(c, 400, "note_required")
		return
	}
	if utf8.RuneCountInString(note) > reportNoteRunes {
		fail(c, 400, "note_too_long")
		return
	}
	h.applyTransition(c, store.ReportResolved, []string{store.ReportPending, store.ReportAccepted}, enforcement, note)
}

// reviewNote 读处置请求体里的说明并校验（**整个请求只在这里读一次 body**：
// body() 会消耗掉请求体，调两次第二次必然解析失败——429/400 的分支都是这么踩出来的）。
// 返回 ok=false 表示已经写过错误响应，调用方直接返回。
func reviewNote(c *gin.Context, required bool) (string, bool) {
	var in struct {
		Note string `json:"note"`
	}
	// 受理允许空对象（不带 body 或 {}），因此 ContentLength 为 0 时按"没写说明"处理。
	if c.Request.ContentLength != 0 {
		if !body(c, &in) {
			return "", false
		}
	}
	note := strings.TrimSpace(in.Note)
	if required && note == "" {
		fail(c, 400, "note_required")
		return "", false
	}
	if utf8.RuneCountInString(note) > reportNoteRunes {
		fail(c, 400, "note_too_long")
		return "", false
	}
	return note, true
}

// applyTransition 是三种处置动作的唯一写入口：调存储层的状态机 → 按稳定错误码回响应 → 记审计。
// 状态机（哪些来源态允许推进到哪）在存储层用行锁原子判定，这里不重复实现一份，
// 否则两处会在并发下打架。
func (h *Handler) applyTransition(c *gin.Context, status string, from []string, enforcement, note string) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		fail(c, 404, "not_found")
		return
	}
	p := h.principal(c)
	updated, err := h.store.TransitionReport(c.Request.Context(), store.ReportTransition{
		ReportID: id, Status: status, From: from, Enforcement: enforcement,
		ReviewerID: p.ID, ReviewerName: authorName(p), Note: note,
	}, time.Now())
	switch {
	case errors.Is(err, store.ErrReportNotFound):
		fail(c, 404, "not_found")
		return
	case errors.Is(err, store.ErrReportState):
		fail(c, 409, "invalid_report_state")
		return
	case err != nil:
		fail(c, 500, "module_error")
		return
	}
	changes := map[string]any{"status": status}
	if enforcement != "" {
		changes["enforcement"] = enforcement
	}
	if note != "" {
		changes["note"] = note
	}
	audit.Describe(c, audit.Detail{TargetType: "report", TargetID: id, Changes: changes})
	c.JSON(200, gin.H{"ok": true, "item": updated})
}

// listAppeals 是申诉队列：默认只看待处理，可用 status 显式取别的档
// （与举报队列同一形状：items + total，page/page_size 分页）。
func (h *Handler) listAppeals(c *gin.Context) {
	statuses := parseStatuses(c.Query("status"))
	if len(statuses) == 0 {
		statuses = []string{store.AppealPending}
	}
	for _, s := range statuses {
		if s != store.AppealPending && s != store.AppealAccepted && s != store.AppealRejected {
			fail(c, 400, "invalid_status")
			return
		}
	}
	limit, offset := pagingPageSize(c, moderationDefaultPageSize)
	items, total, err := h.store.ListAppealsQueue(c.Request.Context(), statuses, limit, offset)
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	c.JSON(200, gin.H{"items": items, "total": total})
}

// reviewAppeal 处理一条申诉：只允许 pending → accepted / rejected（终态不再改）。
func (h *Handler) reviewAppeal(c *gin.Context) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		fail(c, 404, "not_found")
		return
	}
	var in struct {
		Status string `json:"status"`
		Note   string `json:"note"`
	}
	if !body(c, &in) {
		return
	}
	status := strings.TrimSpace(in.Status)
	if status != store.AppealAccepted && status != store.AppealRejected {
		fail(c, 400, "invalid_status")
		return
	}
	note := strings.TrimSpace(in.Note)
	if note == "" {
		fail(c, 400, "note_required")
		return
	}
	if utf8.RuneCountInString(note) > reportNoteRunes {
		fail(c, 400, "note_too_long")
		return
	}
	p := h.principal(c)
	updated, err := h.store.ReviewAppeal(c.Request.Context(), id, status, note, p.ID, authorName(p), time.Now())
	switch {
	case errors.Is(err, store.ErrAppealNotFound):
		fail(c, 404, "not_found")
		return
	case errors.Is(err, store.ErrAppealState):
		fail(c, 409, "invalid_appeal_state")
		return
	case err != nil:
		fail(c, 500, "module_error")
		return
	}
	audit.Describe(c, audit.Detail{TargetType: "report", TargetID: updated.ReportID, Changes: map[string]any{
		"appeal_id": updated.ID, "appeal_status": status, "note": note,
	}})
	c.JSON(200, gin.H{"ok": true, "item": updated})
}

// targetPresent 复核本地目标此刻是否还在。非本地类型恒为 false（"本地没有这份内容"）。
func (h *Handler) targetPresent(c *gin.Context, r store.Report) (bool, error) {
	if !localContentTarget(r.TargetType) {
		return false, nil
	}
	content, err := h.store.ResolveReportedContent(c.Request.Context(), r.TargetType, r.TargetID, commentBoard)
	if err != nil {
		return false, err
	}
	return content.Found, nil
}

// parseStatuses 解析 status= 查询参数（逗号分隔多选）：空值与全空白项都忽略，
// 非法项由调用方判定（这里不静默丢掉，否则"拼错状态名"会变成"看起来没过滤"）。
func parseStatuses(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func validReportStatus(s string) bool {
	return s == store.ReportPending || s == store.ReportAccepted || s == store.ReportRejected || s == store.ReportResolved
}
