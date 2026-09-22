package handler

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/audit"
	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// 必须送达通知的管理面（X02）：失败查询与手动重试触发。
//
// 边界（与 notifications.go 的尽力投递对照，S4 接通后）：
//   - 必须送达：论坛定向回帖 comment.replied（被回复楼层作者 / 主题作者）在回帖原事务中
//     入队（(收件人, 事件) 幂等），再由 worker 经 RetryDueOutbox 投递到目录收件箱；
//     退避见 store.OutboxRetryDelay，过期 7 天，次数耗尽置 failed。不引入 Kafka：
//     量级与语义用本库表即够。
//   - 尽力投递：实体短评的参与式广播仍经 deliverAll 同步投递（整批 2s 预算，失败只记日志）。
//   - 举报处置结论不进本表：目录收件箱暂无 report.* 类型，投递会被拒收；结论经报告行 +
//     report_events 同事务落库，经举报/申诉队列与审计留痕（见 store/reports.go 的 withTx）。
//
// 依赖方向：收件箱仍在目录库（目录是事件产生端的主系统与前端 /api 主入口），互动服务
// 依赖目录的投递端点；这只是边界整理，不拆第五个微服务（见 catalog/notify.go 头注释）。
//
// 闸门复用 community.report.review（举报/申诉管理码）：待投递行今天承载的是回帖通知的
// 可靠投递，不新开权限码（新码要账号服务同步播种，跨仓对齐另起一行变更）。
func (h *Handler) registerNotificationsOutbox(api *gin.RouterGroup) {
	api.GET("/community/admin/notifications/outbox", h.require(auth.PermissionReportReview), func(c *gin.Context) {
		status := strings.TrimSpace(c.Query("status"))
		if status != "" && status != store.OutboxPending && status != store.OutboxSent &&
			status != store.OutboxFailed && status != store.OutboxExpired {
			fail(c, 400, "invalid_status")
			return
		}
		limit, offset := pagingPageSize(c, moderationDefaultPageSize)
		items, total, err := h.store.ListOutbox(c.Request.Context(), status, limit, offset)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{"items": items, "total": total})
	})
	api.POST("/community/admin/notifications/outbox/retry", h.require(auth.PermissionReportReview), func(c *gin.Context) {
		result, err := h.RetryDueOutbox(c.Request.Context(), 50)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		audit.Describe(c, audit.Detail{TargetType: "notification", Changes: map[string]any{
			"retried": result.Retried, "sent": result.Sent, "expired": result.Expired,
		}})
		c.JSON(200, gin.H{"ok": true, "retried": result.Retried, "sent": result.Sent, "expired": result.Expired})
	})
}

// OutboxRetryResult 是一次重试触发的结果：只计数，不透传正文与收件人（管理端按需查列表）。
type OutboxRetryResult struct {
	Retried int   `json:"retried"`
	Sent    int   `json:"sent"`
	Expired int64 `json:"expired"`
}

// outboxClaimLease 是 worker 单次领取的租期：领取时把 next_retry_at 推后这么久，
// 其它 worker（与下一次触发）在这段时间内看不到这些行；占住后崩溃的行租期一过
// 自然重新到期。取值覆盖单次触发的最大投递耗时（50 条 × 每条 5s ≈ 250s）。
const outboxClaimLease = 5 * time.Minute

// RetryDueOutbox 触发一次到期重试：先过期、再原子领取（行锁 + 租约，多 worker 安全）、
// 逐条投递（每条独立 5s 超时，不共享尽力投递的 2s 批预算——必须送达的语义是“最终到”，
// 不是“这次请求内到”）。调用方是管理端重试端点与 server main 的常驻 worker；
// 投递失败只记行状态，不改触发者的响应码。
func (h *Handler) RetryDueOutbox(ctx context.Context, limit int) (OutboxRetryResult, error) {
	var out OutboxRetryResult
	now := time.Now()
	expired, err := h.store.ExpireOutbox(ctx, now)
	if err != nil {
		return out, err
	}
	out.Expired = expired
	due, err := h.store.ClaimDueOutbox(ctx, now, limit, outboxClaimLease)
	if err != nil {
		return out, err
	}
	for _, item := range due {
		out.Retried++
		if h.deliverOutboxItem(ctx, item) {
			out.Sent++
		}
	}
	return out, nil
}

// enqueueTopicReply 在回帖事务内为定向收件人入队（S4 必须送达的唯一产生端）：
// 约束：收件人 = 被回复楼层作者 → 主题作者（去掉操作者本人，去重后最多两人）；
// 同一事件各收件人各存一行（(收件人, 事件) 复合幂等），EventID 取 postID 且每次投递原样携带；
// ActorID/ActorName 是原始作者快照，随业务事务落库，不存令牌。
// 调用方在回帖事务提交前调它：入队失败返回 err，调用方让回帖一起回滚。
// 返回的 items（含预生成的行 ID）供提交后领取试投（claimOutboxAndDeliver 按租约置终态）。
func (h *Handler) enqueueTopicReply(ctx context.Context, tx *sql.Tx, actorID, actorName, topicID, postID string, replyTo *int, topicAuthor, topicTitle, topicEntity, body string) ([]store.OutboxItem, error) {
	repliedTo := ""
	if replyTo != nil {
		_ = tx.QueryRowContext(ctx,
			`SELECT COALESCE(author_id::text,'') FROM community.posts WHERE topic_id=$1 AND post_number=$2`,
			topicID, *replyTo).Scan(&repliedTo)
	}
	recipients := []string{}
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || id == actorID || seen[id] {
			return
		}
		seen[id] = true
		recipients = append(recipients, id)
	}
	add(repliedTo)
	add(topicAuthor)
	if len(recipients) == 0 {
		return nil, nil
	}
	payload := map[string]any{
		"topic_id":    topicID,
		"topic_title": topicTitle,
		"excerpt":     notifyExcerpt(body),
		"via":         "topic_reply",
	}
	if topicEntity != "" {
		payload["entity_id"] = topicEntity
	}
	if replyTo != nil {
		payload["reply_to_post_number"] = *replyTo
	}
	now := time.Now()
	out := make([]store.OutboxItem, 0, len(recipients))
	for _, rid := range recipients {
		item := store.OutboxItem{
			ID:          uuid.NewString(),
			RecipientID: rid,
			Type:        catalog.NotificationCommentReplied,
			SubjectType: "topic",
			SubjectID:   topicID,
			DedupeKey:   "comment.replied:topic:" + topicID,
			EventID:     postID,
			ActorID:     actorID,
			ActorName:   actorName,
			Payload:     payload,
		}
		if _, err := h.store.EnqueueNotificationTx(ctx, tx, item, now); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

// deliverOutboxItem 投递单条待投递行：成功置 sent，失败记 attempts/退避/错误。
// 约束：以受限服务身份投递（不转发调用者凭据），作者只取行内快照，重试不改作者；
// 未配置投递密钥（ErrNotConfigured）是部署态：不计失败、不推退避，留待配置后下次触发。
func (h *Handler) deliverOutboxItem(ctx context.Context, item store.OutboxItem) bool {
	deliverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := h.catalog.NotifyService(deliverCtx, catalog.Notification{
		RecipientID: item.RecipientID,
		Type:        item.Type,
		SubjectType: item.SubjectType,
		SubjectID:   item.SubjectID,
		DedupeKey:   item.DedupeKey,
		EventID:     item.EventID,
		ActorID:     item.ActorID,
		ActorName:   item.ActorName,
		Payload:     item.Payload,
	})
	now := time.Now()
	if err == nil {
		_ = h.store.MarkOutboxSent(ctx, item.ID, now)
		return true
	}
	if err == catalog.ErrNotConfigured {
		return false
	}
	_ = h.store.MarkOutboxAttempt(ctx, item.ID, err.Error(), now)
	return false
}

// claimOutboxAndDeliver 按 ID 领取后投递（立即投递与后台投递共用领取约定）。
// 约束：同一事件只投递一次，未领到的一方必须跳过（另一方会投递）。
func (h *Handler) claimOutboxAndDeliver(ctx context.Context, item store.OutboxItem) bool {
	now := time.Now()
	claimed, err := h.store.ClaimOutboxByIDs(ctx, []string{item.ID}, now, outboxClaimLease)
	if err != nil || len(claimed) == 0 {
		return false
	}
	return h.deliverOutboxItem(ctx, claimed[0])
}
