package handler

import (
	"context"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/audit"
	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// 必须送达通知的管理面（X02）：失败查询与手动重试触发。
//
// 边界（与 notifications.go 的尽力投递对照）：
//   - 尽力投递：评论被回复 comment.replied 经 deliverAll 同步投递（整批 2s 预算，失败只记日志）。
//   - 必须送达：审核/安全/处置类通知先 EnqueueNotification 落库（event_id 幂等），再由
//     RetryDueOutbox 投递到目录收件箱；退避见 store.OutboxRetryDelay，过期 7 天，次数耗尽置
//     failed。不引入 Kafka：量级与语义用本库表即够。
//
// 依赖方向：收件箱仍在目录库（目录是事件产生端的主系统与前端 /api 主入口），互动服务
// 依赖目录的投递端点；这只是边界整理，不拆第五个微服务（见 catalog/notify.go 头注释）。
//
// 闸门复用 community.report.review（举报/申诉管理码）：必须送达的通知今天就是审核与处置
// 结论的送达，不新开权限码（新码要账号服务同步播种，跨仓对齐另起一行变更）。
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

// RetryDueOutbox 触发一次到期重试：先过期、再逐条投递（每条独立 5s 超时，不共享尽力投递的
// 2s 批预算——必须送达的语义是“最终到”，不是“这次请求内到”）。调用方是管理端重试端点与
// 未来的定时触发（见 cmd/server 的接线注释）；投递失败只记行状态，不改触发者的响应码。
func (h *Handler) RetryDueOutbox(ctx context.Context, limit int) (OutboxRetryResult, error) {
	var out OutboxRetryResult
	now := time.Now()
	expired, err := h.store.ExpireOutbox(ctx, now)
	if err != nil {
		return out, err
	}
	out.Expired = expired
	due, err := h.store.ListDueOutbox(ctx, now, limit)
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

// deliverOutboxItem 投递单条待投递行：成功置 sent，失败记 attempts/退避/错误。
// 未配置投递密钥（ErrNotConfigured）是部署态：不计失败、不推退避，留待配置后下次触发。
func (h *Handler) deliverOutboxItem(ctx context.Context, item store.OutboxItem) bool {
	deliverCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := h.catalog.Notify(deliverCtx, catalog.Notification{
		RecipientID: item.RecipientID,
		Type:        item.Type,
		SubjectType: item.SubjectType,
		SubjectID:   item.SubjectID,
		DedupeKey:   item.DedupeKey,
		EventID:     item.EventID,
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
