package handler

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/audit"
	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// 举报与申诉的 HTTP 契约。缺口背景：审计 2026-09-19 S-30 —— 站点此前没有任何举报载体，
// 用户无处提交、管理员也没有处置队列与留痕。本文件是那条缺口的最小闭环：
//
//	用户端（登录即可，不需要额外权限码）：
//	  POST /api/community/reports               提交举报
//	  GET  /api/community/reports/mine          我的举报（状态 / 处理人 / 处置结论 / 能否申诉）
//	  POST /api/community/reports/:id/appeal    被处置方提交一次申诉
//	管理端（闸门 community.report.review）：
//	  GET  /api/community/admin/reports                队列（状态 / 类型 / 关键词过滤 + 分页）
//	  GET  /api/community/admin/reports/:id            详情（目标快照 + 时间线 + 申诉）
//	  POST /api/community/admin/reports/:id/accept     受理
//	  POST /api/community/admin/reports/:id/reject     驳回
//	  POST /api/community/admin/reports/:id/resolve    处置（记录处置结论）
//	  GET  /api/community/admin/appeals                申诉队列
//	  POST /api/community/admin/appeals/:id/review     处理申诉
//
// 为什么举报提交**不设权限码**：举报是任何登录用户的基本权利。收口成权限码意味着
// "没有那个码的用户无处举报"——正是本轮要修的缺口；滥用防线改用配额 + 未终结重复拦截（见下）。
//
// 下线内容与封禁用户都**不由本服务新造动作**：
//   - 下线内容走既有删除端点（DELETE /community/posts/{id}、/community/topics/{id}、
//     /community/topics/{id}/posts/{postId}）；本服务的 resolve 只**复核**内容确实已经下线
//     （复核不通过回 409 content_still_present），不在这里再写一套删除。
//   - 封禁用户在账号服务（PUT /api/admin/users/{id}/ban，auth.users.manage）。本服务没有、
//     也不应该有一套自己的封禁：处置结论记 user_banned 只表达"这次处置的结果是封禁"，
//     真正的封禁动作与它的权限判定留在账号服务（队列页据此提示去账号控制台执行）。

// reportQuotaWindow / reportQuota 是举报的滥用防线：同一举报人在滚动 24 小时内最多提交 20 条。
// 取值刻意宽松（正常用户一年也用不到），目的是拦住脚本刷举报把队列灌满。
const (
	reportQuotaWindow = 24 * time.Hour
	reportQuota       = 20
)

// 文本上限：与库里的 text 列配合，避免一次请求写进 MB 级正文。
// 说明（detail）按 rune 计：中文一字一 rune，按字节会把中文用户的上限砍到三分之一。
const (
	reportDetailRunes = 2000
	reportAppealRunes = 2000
	reportNoteRunes   = 2000
	reportIDMaxRunes  = 200
	reportURLMaxRunes = 500
	// reportExcerptRunes 是快照里正文/标题摘要的 rune 上限：与帖子治理列表同一档（200），
	// 快照要能塞进列表响应与审计行，长文全文不是它的职责。
	reportExcerptRunes = 200
)

// registerReports 挂载举报与申诉的全部路由（写路由的动作码登记见 audit.go）。
func (h *Handler) registerReports(api *gin.RouterGroup) {
	api.POST("/community/reports", h.guard(true), h.createReport)
	api.GET("/community/reports/mine", h.guard(true), h.listMyReports)
	api.POST("/community/reports/:id/appeal", h.guard(true), h.createReportAppeal)

	api.GET("/community/admin/reports", h.require(auth.PermissionReportReview), h.listReports)
	api.GET("/community/admin/reports/:id", h.require(auth.PermissionReportReview), h.reportDetail)
	api.POST("/community/admin/reports/:id/accept", h.require(auth.PermissionReportReview), h.acceptReport)
	api.POST("/community/admin/reports/:id/reject", h.require(auth.PermissionReportReview), h.rejectReport)
	api.POST("/community/admin/reports/:id/resolve", h.require(auth.PermissionReportReview), h.resolveReport)
	api.GET("/community/admin/appeals", h.require(auth.PermissionReportReview), h.listAppeals)
	api.POST("/community/admin/appeals/:id/review", h.require(auth.PermissionReportReview), h.reviewAppeal)
}

// ── 对外形状 ──

// reportItem 是**用户可见**的举报形状：只给"我提交了什么、现在怎么样、处理人是谁"，
// 不外泄举报人 id / 目标作者 id / 处理人 id（这些是管理端与审计要用的内部标识）。
type reportItem struct {
	ID            string         `json:"id"`
	TargetType    string         `json:"target_type"`
	TargetID      string         `json:"target_id"`
	TargetContext map[string]any `json:"target_context"`
	Reason        string         `json:"reason"`
	Detail        string         `json:"detail,omitempty"`
	EvidenceURL   string         `json:"evidence_url,omitempty"`
	Status        string         `json:"status"`
	ReviewerName  string         `json:"reviewer_name,omitempty"`
	ReviewNote    string         `json:"review_note,omitempty"`
	Enforcement   string         `json:"enforcement,omitempty"`
	ReviewedAt    *time.Time     `json:"reviewed_at,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	CanAppeal     bool           `json:"can_appeal"`
	Appeal        *appealItem    `json:"appeal,omitempty"`
}

// appealItem 是申诉的对外形状（用户端与管理端共用一个形状：
// 申诉正文本来就是被处置方自己写的，处理人与其结论对双方都要可见）。
type appealItem struct {
	ID           string     `json:"id"`
	Body         string     `json:"body"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"created_at"`
	ReviewerName string     `json:"reviewer_name,omitempty"`
	ReviewNote   string     `json:"review_note,omitempty"`
	ReviewedAt   *time.Time `json:"reviewed_at,omitempty"`
}

func newAppealItem(a store.ReportAppeal) *appealItem {
	return &appealItem{
		ID: a.ID, Body: a.Body, Status: a.Status, CreatedAt: a.CreatedAt,
		ReviewerName: a.ReviewerName, ReviewNote: a.ReviewNote, ReviewedAt: a.ReviewedAt,
	}
}

// canAppeal 是"被处置方能否提交申诉"的**唯一判据**（用户端标按钮、服务端把闸门都用它）：
// 只有被处置方本人、报告已经受理或处置、且还没有申诉过，才允许提交一次。
func canAppeal(r store.Report, viewerID string, hasAppeal bool) bool {
	if hasAppeal || viewerID == "" || r.TargetAuthorID == "" {
		return false
	}
	if r.TargetAuthorID != viewerID {
		return false
	}
	return r.Status == store.ReportAccepted || r.Status == store.ReportResolved
}

func newReportItem(r store.Report, viewerID string, appeal *store.ReportAppeal) reportItem {
	item := reportItem{
		ID: r.ID, TargetType: r.TargetType, TargetID: r.TargetID, TargetContext: r.TargetContext,
		Reason: r.Reason, Detail: r.Detail, EvidenceURL: r.EvidenceURL, Status: r.Status,
		ReviewerName: r.ReviewerName, ReviewNote: r.ReviewNote, Enforcement: r.Enforcement,
		ReviewedAt: r.ReviewedAt, CreatedAt: r.CreatedAt,
	}
	if appeal != nil {
		item.Appeal = newAppealItem(*appeal)
	}
	item.CanAppeal = canAppeal(r, viewerID, appeal != nil)
	return item
}

// ── 用户端 ──

// createReport 提交一条举报。
//
// 三道防线（顺序即代价从低到高）：
//  1. 入参校验：类型 / 理由必须在词表内，文本与 URL 有长度与协议上限；
//  2. 配额：同一人滚动 24 小时最多 reportQuota 条，超限回 429 rate_limited + Retry-After
//     （与站点既有限流的错误码和头一致，调用方按同一套分支处理）；
//  3. 未终结重复：同一个人对同一个对象的未终结举报只允许一条，重复回 409 duplicate_report
//     （由部分唯一索引兜底，见迁移 000008 的注释）。**已驳回或已处置之后可以再举报一次**。
func (h *Handler) createReport(c *gin.Context) {
	var in struct {
		TargetType  string `json:"target_type"`
		TargetID    string `json:"target_id"`
		Reason      string `json:"reason"`
		Detail      string `json:"detail"`
		EvidenceURL string `json:"evidence_url"`
	}
	if !body(c, &in) {
		return
	}
	targetType := strings.TrimSpace(in.TargetType)
	targetID := strings.TrimSpace(in.TargetID)
	reason := strings.TrimSpace(in.Reason)
	detail := strings.TrimSpace(in.Detail)
	evidence := strings.TrimSpace(in.EvidenceURL)

	if !store.ValidReportTargetType(targetType) {
		fail(c, 400, "invalid_target_type")
		return
	}
	if targetID == "" || utf8.RuneCountInString(targetID) > reportIDMaxRunes {
		fail(c, 400, "invalid_target_id")
		return
	}
	if !store.ValidReportReason(reason) {
		fail(c, 400, "invalid_reason")
		return
	}
	if utf8.RuneCountInString(detail) > reportDetailRunes {
		fail(c, 400, "detail_too_long")
		return
	}
	if evidence != "" && !validEvidenceURL(evidence) {
		fail(c, 400, "invalid_evidence_url")
		return
	}

	p := h.principal(c)
	ctx := c.Request.Context()

	used, retryAfter, err := h.store.ReportQuota(ctx, p.ID, reportQuotaWindow, time.Now())
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	if used >= reportQuota {
		if retryAfter <= 0 {
			retryAfter = int(reportQuotaWindow.Seconds())
		}
		c.Header("Retry-After", strconv.Itoa(retryAfter))
		fail(c, http.StatusTooManyRequests, "rate_limited")
		return
	}

	// 目标解析：本地内容（短评 / 帖子）提交时必须真实存在——否则"下线内容"这条处置
	// 会对着一个从来不存在的 id 也判定成功。实体 / 用户 / 资源不在本地，只存不透明引用，
	// 这里不做存在性判断（那是别的服务的数据，查不到不等于不存在）。
	content, err := h.store.ResolveReportedContent(ctx, targetType, targetID, commentBoard)
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	if localContentTarget(targetType) && !content.Found {
		fail(c, 404, "not_found")
		return
	}

	now := time.Now()
	report := store.Report{
		ID:               uuid.NewString(),
		TargetType:       targetType,
		TargetID:         targetID,
		TargetAuthorID:   content.AuthorID,
		TargetAuthorName: content.AuthorName,
		TargetContext:    reportContext(content),
		Reason:           reason,
		Detail:           detail,
		EvidenceURL:      evidence,
		ReporterID:       p.ID,
		ReporterName:     authorName(p),
		Status:           store.ReportPending,
	}
	if err = h.store.CreateReport(ctx, report, now); err != nil {
		if errors.Is(err, store.ErrDuplicateReport) {
			fail(c, 409, "duplicate_report")
			return
		}
		fail(c, 500, "module_error")
		return
	}
	// 举报正文不进审计（与发主题/短评同一口径：审计表不存请求体原文），只记"举报了什么、以什么理由"。
	audit.Describe(c, audit.Detail{TargetType: "report", TargetID: report.ID, Changes: map[string]any{
		"target_type": targetType, "target_id": targetID, "reason": reason,
	}})
	c.JSON(200, gin.H{"ok": true, "item": newReportItem(report, p.ID, nil)})
}

// listMyReports 列"我的举报"：含状态、处理人与处置结论，并标出能否申诉。
// 分页走 page/page_size（与 favorites / messages / 帖子治理列表同口径）。
func (h *Handler) listMyReports(c *gin.Context) {
	p := h.principal(c)
	limit, offset := pagingPageSize(c, moderationDefaultPageSize)
	items, total, err := h.store.ListMyReports(c.Request.Context(), p.ID, limit, offset)
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	appeals, err := h.store.AppealsForReports(c.Request.Context(), reportIDs(items))
	if err != nil {
		fail(c, 500, "module_error")
		return
	}
	out := make([]reportItem, 0, len(items))
	for _, r := range items {
		var appeal *store.ReportAppeal
		if list := appeals[r.ID]; len(list) > 0 {
			appeal = &list[0]
		}
		out = append(out, newReportItem(r, p.ID, appeal))
	}
	c.JSON(200, gin.H{"items": out, "total": total})
}

// createReportAppeal 是"被处置方提交一次申诉"的唯一入口。
//
// 身份与状态都在这里判定（存储层的唯一索引只负责"不写出第二条"）：
//   - 没有本地可申诉方（实体 / 资源）→ 403 appeal_not_available；
//   - 不是被处置方本人 → 403 not_appealed_party；
//   - 报告还没被受理 / 处置（待处理或已驳回）→ 409 report_not_disposed；
//   - 已经申诉过 → 409 duplicate_appeal。
func (h *Handler) createReportAppeal(c *gin.Context) {
	id := c.Param("id")
	if _, err := uuid.Parse(id); err != nil {
		fail(c, 404, "not_found")
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if !body(c, &in) {
		return
	}
	text := strings.TrimSpace(in.Body)
	if text == "" {
		fail(c, 400, "invalid_payload")
		return
	}
	if utf8.RuneCountInString(text) > reportAppealRunes {
		fail(c, 400, "appeal_too_long")
		return
	}

	p := h.principal(c)
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
	if report.TargetAuthorID == "" {
		fail(c, 403, "appeal_not_available")
		return
	}
	// 不是被处置方：回 403 但**不区分**"这条处置不是你"与"你没有申诉权"，
	// 免得把"谁被处置了"透给无关的人。
	if report.TargetAuthorID != p.ID {
		fail(c, 403, "not_appealed_party")
		return
	}
	if report.Status != store.ReportAccepted && report.Status != store.ReportResolved {
		fail(c, 409, "report_not_disposed")
		return
	}

	appeal := store.ReportAppeal{
		ID: uuid.NewString(), ReportID: id,
		AppellantID: p.ID, AppellantName: authorName(p), Body: text,
	}
	if err = h.store.CreateReportAppeal(ctx, appeal, time.Now()); err != nil {
		if errors.Is(err, store.ErrDuplicateAppeal) {
			fail(c, 409, "duplicate_appeal")
			return
		}
		fail(c, 500, "module_error")
		return
	}
	audit.Describe(c, audit.Detail{TargetType: "report", TargetID: id, Changes: map[string]any{
		"appeal_id": appeal.ID, "target_type": report.TargetType, "target_id": report.TargetID,
	}})
	appeal.Status = store.AppealPending
	c.JSON(200, gin.H{"ok": true, "item": newAppealItem(appeal)})
}

// ── 小工具 ──

// localContentTarget 报告该类型的内容是否**在本服务里**（可解析作者、可下线）。
// 实体 / 资源 / 用户不在本地：它们的处置动作分别归目录、存储与账号服务。
func localContentTarget(targetType string) bool {
	return targetType == store.ReportTargetComment || targetType == store.ReportTargetPost
}

// reportContext 是提交时刻的目标快照：正文摘要按 rune 截断，
// 因为快照要能塞进审计与列表响应里，长文全文不是它的职责（全文还在原页面）。
func reportContext(content store.ReportedContent) map[string]any {
	out := map[string]any{}
	if !content.Found {
		return out
	}
	if content.Kind != "" {
		out["content_kind"] = content.Kind
	}
	if content.BoardCode != "" {
		out["board_code"] = content.BoardCode
	}
	if content.TopicID != "" {
		out["topic_id"] = content.TopicID
	}
	if content.TopicTitle != "" {
		out["topic_title"] = excerptField(content.TopicTitle)
	}
	if content.EntityID != "" {
		out["entity_id"] = content.EntityID
	}
	if content.AuthorID != "" {
		out["author_id"] = content.AuthorID
	}
	if excerpt, truncated := excerptRunes(content.Body, reportExcerptRunes); excerpt != "" {
		out["excerpt"] = excerpt
		if truncated {
			out["excerpt_truncated"] = true
		}
	}
	return out
}

// excerptField 对快照里的标题做同样的截断（长度上限与正文摘要共用）。
func excerptField(s string) string {
	out, _ := excerptRunes(s, reportExcerptRunes)
	return out
}

// validEvidenceURL 只接受 http/https 绝对地址：证据链要能被处理人点开，
// javascript: / data: 这类 scheme 在后台点一下就是一次注入。
func validEvidenceURL(raw string) bool {
	if utf8.RuneCountInString(raw) > reportURLMaxRunes {
		return false
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != ""
}

func reportIDs(items []store.Report) []string {
	out := make([]string, 0, len(items))
	for _, r := range items {
		out = append(out, r.ID)
	}
	return out
}
