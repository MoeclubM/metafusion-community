package handler

import (
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// 帖子治理列表：跨主题巡检 community.posts（楼中回复）。
//
// 为什么需要它：回复本来只有"删"的端点（DELETE /community/topics/{id}/posts/{postId}），
// 要处置一条回复得先知道它在哪个主题；而主题详情（GET /community/topics/{id}）会顺手把
// view_count +1 —— 拿它当巡检入口，等于每次排查都在篡改统计。本端点只读、**不碰 view_count**，
// 并带上治理上下文（所属主题与板块、作者快照、楼层号、正文摘要），让列表本身就能判断。
//
// 与 /community/feed 的分工：feed 读的是**评论板块的短评**（存在 community.topics 里的行），
// 本端点读的是**楼中回复**（community.posts）。二者不是同一张表，也不能互相替代。
//
// 分页走 page/page_size 写法（口径与换算见 paging.go，与 /api/messages/*、/favorites/* 一致），
// 响应 {"items":[...],"total":N} 与同服务的其它列表逐字一致；topics 列表的 limit/offset
// 属兼容期内的另一套口径，本次不动它。
//
// 闸门是 community.post.moderate：这是治理动作的实际权限码（moderator 与 community_admin 都持有），
// topic.pin 只覆盖置顶、board.manage 只管板块结构，都不能替代"处置内容"。匿名 401、缺码 403。

// moderationExcerptRunes 是列表里正文摘要的字符上限（按 rune 算，不是字节）：
// 治理列表要能一眼扫过，不该把整篇长文搬进列表页。
const moderationExcerptRunes = 200

// moderationDefaultPageSize 是缺省页宽（越界值的收敛口径见 pagingPageSize）。
const moderationDefaultPageSize = 20

// excerptRunes 生成展示用摘要：折叠连续空白（含换行）后按 rune 截断。
// 返回 (摘要, 是否截断)；空正文返回空串而不是省略号。
//
// 折叠空白是**展示**行为：回复正文里的换行在表格里会撑破行高，而这里给的是"够判断"的预览，
// 全文仍在主题页（本端点不改变任何存储内容）。
func excerptRunes(raw string, max int) (string, bool) {
	collapsed := strings.Join(strings.Fields(raw), " ")
	if max <= 0 || utf8.RuneCountInString(collapsed) <= max {
		return collapsed, false
	}
	runes := []rune(collapsed)
	return string(runes[:max]) + "…", true
}

// moderationPostItem 是对外形状。excerpt 是摘要、truncated 标明被截断，
// 其余字段名与列表接口的既有命名对齐（author_id/author_name 与短评流一致）。
type moderationPostItem struct {
	ID         string    `json:"id"`
	TopicID    string    `json:"topic_id"`
	TopicTitle string    `json:"topic_title"`
	BoardCode  string    `json:"board_code"`
	AuthorID   string    `json:"author_id"`
	AuthorName string    `json:"author_name"`
	PostNumber int       `json:"post_number"`
	ReplyTo    *int      `json:"reply_to_post_number"`
	Excerpt    string    `json:"excerpt"`
	Truncated  bool      `json:"truncated"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func newModerationPostItem(row store.ModerationPost) moderationPostItem {
	excerpt, truncated := excerptRunes(row.Body, moderationExcerptRunes)
	var replyTo *int
	if row.ReplyToPostNumber.Valid {
		value := int(row.ReplyToPostNumber.Int64)
		replyTo = &value
	}
	return moderationPostItem{
		ID: row.ID, TopicID: row.TopicID, TopicTitle: row.TopicTitle, BoardCode: row.BoardCode,
		AuthorID: row.AuthorID, AuthorName: row.AuthorName, PostNumber: row.PostNumber,
		ReplyTo: replyTo, Excerpt: excerpt, Truncated: truncated,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
}

func (h *Handler) registerModeration(api *gin.RouterGroup) {
	api.GET("/community/posts", h.require(auth.PermissionPostModerate), func(c *gin.Context) {
		limit, offset := pagingPageSize(c, moderationDefaultPageSize)
		search := strings.TrimSpace(c.Query("q"))
		rows, total, err := h.store.ListModerationPosts(c.Request.Context(), search, limit, offset)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		items := make([]moderationPostItem, 0, len(rows))
		for _, row := range rows {
			items = append(items, newModerationPostItem(row))
		}
		c.JSON(200, gin.H{"items": items, "total": total})
	})
}
