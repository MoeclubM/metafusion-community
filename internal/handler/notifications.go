package handler

// 互动服务的**通知产生端**：把"评论被回复"这件事投递给目录服务的收件箱。
//
// 为什么是这两条路径（证据在 docs-local/report-f1-notifications/REPORT.md）：
//   - 论坛回帖 POST /community/topics/:id/posts 有明确的回复目标（reply_to_post_number /
//     reply_to_post_id），收件人 = 被回复楼层作者，没有回复目标时 = 主题作者。
//   - 实体短评 POST /community/entities/:id/posts **没有回复关系**：短评是
//     community.topics 里 board_code=comment 的行，表里没有 parent 列，写入体也只有 body，
//     前端也没有"回复某条短评"的入口。因此这里用的是**参与式关注**语义：
//     给"同一条目下最近评论过的其他人"各发一条（每个收件人按条目聚合成一行 + count），
//     而不是编造一个并不存在的回复关系。
//
// 投递是旁路：写请求已经提交，通知发不出去只记日志（与审计旁路同一哲学）。
// 扇出有上界（participantFanout）且整批共享一个 deadline（notifyBudget）：
// 热门条目不该让一次评论变成 20 次串行跨服务调用，真人等的还是那个 POST。

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
func (h *Handler) notifyEntityComment(c *gin.Context, entityID, requested, commentID, body string) {
	p := h.principal(c)
	if p == nil {
		return
	}
	recipients := h.commentParticipants(c.Request.Context(), catalog.AliasSet(entityID, requested), p.ID)
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

// notifyTopicReply 是论坛回帖的产生端：收件人 = 被回复楼层作者 →（无回复目标时）主题作者。
// 两位收件人都存在时都发（同一条回复既回答了提问者、也挂在主题作者下面）。
func (h *Handler) notifyTopicReply(c *gin.Context, topicID, postID string, replyTo *int, body string) {
	p := h.principal(c)
	if p == nil {
		return
	}
	ctx := c.Request.Context()
	var topicAuthor, topicTitle, entityID string
	if err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(author_id::text,''),COALESCE(title,''),COALESCE(entity_id::text,'') FROM community.topics WHERE id=$1`,
		topicID).Scan(&topicAuthor, &topicTitle, &entityID); err != nil {
		log.Printf("community notification topic lookup failed: topic=%s err=%v", topicID, err)
		return
	}
	recipients := []string{}
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || id == p.ID || seen[id] {
			return
		}
		seen[id] = true
		recipients = append(recipients, id)
	}
	repliedTo := ""
	if replyTo != nil {
		_ = h.db.QueryRowContext(ctx,
			`SELECT COALESCE(author_id::text,'') FROM community.posts WHERE topic_id=$1 AND post_number=$2`,
			topicID, *replyTo).Scan(&repliedTo)
	}
	add(repliedTo)
	add(topicAuthor)
	if len(recipients) == 0 {
		return
	}
	payload := map[string]any{
		"topic_id":    topicID,
		"topic_title": topicTitle,
		"excerpt":     notifyExcerpt(body),
		"via":         "topic_reply",
	}
	if entityID != "" {
		payload["entity_id"] = entityID
	}
	if replyTo != nil {
		payload["reply_to_post_number"] = *replyTo
	}
	msgs := make([]catalog.Notification, 0, len(recipients))
	for _, rid := range recipients {
		msgs = append(msgs, catalog.Notification{
			RecipientID: rid,
			Type:        catalog.NotificationCommentReplied,
			SubjectType: "topic",
			SubjectID:   topicID,
			DedupeKey:   "comment.replied:topic:" + topicID,
			EventID:     postID,
			Payload:     payload,
		})
	}
	h.deliverAll(c, msgs)
}
