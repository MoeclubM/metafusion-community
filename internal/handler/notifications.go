package handler

// 互动服务的**尽力投递产生端**：实体短评的参与式广播（S4 明确为尽力，不进 outbox）。
//
// 为什么只剩这一条路径留在这里（证据在 docs-local/report-f1-notifications/REPORT.md）：
// 实体短评 POST /community/entities/:id/posts **没有回复关系**：短评是
// community.topics 里 board_code=comment 的行，表里没有 parent 列，写入体也只有 body，
// 前端也没有"回复某条短评"的入口。因此这里用的是**参与式关注**语义：
// 给"同一条目下最近评论过的其他人"各发一条（每个收件人按条目聚合成一行 + count），
// 而不是编造一个并不存在的回复关系。论坛定向回帖不在这里：收件人明确
// （被回复楼层作者 / 主题作者），走 community.notification_outbox 可靠送达
// （回帖事务内入队 + 提交后试投 + worker 重试，见 notifications_outbox.go）。
//
// 刻意保持尽力投递的三条理由（S4 项 4）：扇出可达 10 人、迟到的提醒没有价值、
// 同一评论为每人各存一行会把一次评论放大成 10 行 DB 写。投递是旁路：
// 写请求已经提交，通知发不出去只记日志（与审计旁路同一哲学）。
// 扇出有上界（participantFanout）且整批共享一个 deadline（notifyBudget）：
// 热门条目不该让一次评论变成 20 次串行跨服务调用，真人等的还是那个 POST。
//
// X02 边界：以上是尽力投递（best-effort，comment.replied）——迟到的提醒没有价值。
// 必须送达的论坛定向回帖走 community.notification_outbox：回帖事务内入队
// （(收件人, 事件) 幂等），提交后立即试投，worker 按退避重试。不引入 Kafka：
// 量级与语义用本库表即够。

import (
	"context"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/lib/pq"

	"github.com/MoeclubM/metafusion-community/internal/catalog"
)

const (
	// participantFanout 是"参与式关注"的扇出上界：只通知最近评论过的这么多人。
	participantFanout = 10
	// notifyBudget 是一整批投递的总预算：超时的收件人直接跳过并记日志。
	notifyBudget = 2 * time.Second
	// notifyExcerptRunes 是塞进通知的正文摘要上限（通知是入口，不是正文副本）。
	notifyExcerptRunes = 120
)

// notifyExcerpt 折叠空白后按 rune 截断，空正文返回空串。
func notifyExcerpt(raw string) string {
	collapsed := strings.Join(strings.Fields(raw), " ")
	if utf8.RuneCountInString(collapsed) <= notifyExcerptRunes {
		return collapsed
	}
	return string([]rune(collapsed)[:notifyExcerptRunes]) + "…"
}

// deliverAll 顺序投递并记日志：失败不冒泡（调用方是已经提交的写路径）。
// 一批共享同一个 deadline——预算用尽后剩下的收件人直接放弃并记一行日志，
// "少发几条通知"远好于"把一个 POST 拖成十几秒"。
func (h *Handler) deliverAll(c *gin.Context, msgs []catalog.Notification) {
	if len(msgs) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), notifyBudget)
	defer cancel()
	for _, msg := range msgs {
		if ctx.Err() != nil {
			log.Printf("community notification skipped (budget exhausted): recipient=%s type=%s", msg.RecipientID, msg.Type)
			return
		}
		if err := h.catalog.Notify(ctx, msg); err != nil {
			log.Printf("community notification delivery failed: recipient=%s type=%s err=%v", msg.RecipientID, msg.Type, err)
		}
	}
}

// commentParticipants 返回同一条目下最近参与评论的其他人（不含作者本人，最多 participantFanout 人）。
// 查询失败返回空表：通知是旁路，不能因为"算不清收件人"让已经成功的短评变成 500。
func (h *Handler) commentParticipants(ctx context.Context, entityIDs []string, actorID string) []string {
	if len(entityIDs) == 0 {
		return nil
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT author_id::text FROM community.topics
		WHERE board_code=$1 AND entity_id = ANY($2::uuid[]) AND author_id <> $3
		GROUP BY author_id ORDER BY max(created_at) DESC LIMIT $4`,
		commentBoard, pq.Array(entityIDs), actorID, participantFanout)
	if err != nil {
		log.Printf("community notification recipients lookup failed: entities=%v err=%v", entityIDs, err)
		return nil
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			continue
		}
		out = append(out, id)
	}
	return out
}

// entityProjectionForNotify 为一个展示字段（题名）和一个跳转字段（kind）多取一次目录投影。
// 取不到就留空：前端按"没有题名/不知道层级"降级渲染（退回通用条目页），
// 而不是拼一个必然 404 的地址——这条通知的语义不受影响。
func (h *Handler) entityProjectionForNotify(ctx context.Context, entityID string) (title, kind string) {
	e, err := h.catalog.Lookup(ctx, entityID)
	if err != nil || e.ID == "" {
		return "", ""
	}
	return e.Title, e.Kind
}

// notifyEntityComment 是短评（参与式关注）的产生端：entityID 必须已归一 canonical，
// requested 保留请求别名以覆盖历史参与人（X01 别名集合，见 commentParticipants）。
// X01-compat：全量别名经 ResolveAliasSet 展开（目录反向契约就绪后自动含全部历史）；
// 展开失败是旁路故障，回退 {canonical + 请求别名} 并记日志，不影响短评本身。
func (h *Handler) notifyEntityComment(c *gin.Context, entityID, requested, commentID, body string) {
	p := h.principal(c)
	if p == nil {
		return
	}
	set := catalog.AliasSet(entityID, requested)
	if _, full, err := h.catalog.ResolveAliasSet(c.Request.Context(), requested); err != nil {
		log.Printf("community notification alias expansion failed, compat fallback: entities=%v err=%v", set, err)
	} else if len(full) > 0 {
		set = catalog.AliasSet(entityID, full...)
	}
	recipients := h.commentParticipants(c.Request.Context(), set, p.ID)
	if len(recipients) == 0 {
		return
	}
	title, kind := h.entityProjectionForNotify(c.Request.Context(), entityID)
	excerpt := notifyExcerpt(body)
	msgs := make([]catalog.Notification, 0, len(recipients))
	for _, rid := range recipients {
		msgs = append(msgs, catalog.Notification{
			RecipientID: rid,
			Type:        catalog.NotificationCommentReplied,
			SubjectType: "entity",
			SubjectID:   entityID,
			DedupeKey:   "comment.replied:entity:" + entityID,
			EventID:     commentID,
			Payload: map[string]any{
				"entity_id":    entityID,
				"entity_kind":  kind,
				"entity_title": title,
				"excerpt":      excerpt,
				"via":          "entity_comment",
			},
		})
	}
	h.deliverAll(c, msgs)
}

// 注：论坛回帖的旧尽力投递路径（notifyTopicReply）已在 S4 接通时移除：
// 回帖通知改为必须送达——forum.go 在回帖事务内入队（enqueueTopicReply），
// 提交后立即试投 + worker 重试，收件人与载荷口径见该函数。本文件只留实体短评的尽力广播。
