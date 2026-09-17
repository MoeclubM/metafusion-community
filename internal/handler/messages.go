package handler

import (
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// 私信（DM）：两个端点都在登录门槛（guard(true)）之后，与其它私有数据接口同一口径。
//
// "只能看见/发送自己参与的会话"不是处理器里的一条 if，而是查询本身的结构：
// 会话由 (当前用户, 对方) 两个参与者决定（见 store.ListConversation），
// 请求里也没有"会话 id"这种可以指向别人会话的输入，因此越权访问没有入口——
// 第三者的私信查出来就是空列表（回到 404/空页，而不是 403 之外的另一种形状）。
//
// 对方 id 只当外部引用用：账号数据归账号服务，本服务不查它的库、也不校验对方是否存在，
// 只校验字面量是不是 uuid。
//
// 契约（与前端调用的形状逐字一致）：
//   GET  /api/messages/with/{id}?page=1&page_size=20 -> {"items":[...],"total":N}
//   POST /api/messages/with/{id} {"body":"..."}      -> {"message":{...}}

// maxMessageBodyRunes 是一条私信的正文上限，按**字符数**（rune）算而不是字节数。
// 契约里写的是"4000 字符"：按字节算会把中文上限压到约 1/3（4000 字节 ≈ 1333 个汉字），
// 同一段内容在纯中文与纯 ASCII 下拿到不同的长度判定，对用户是说不清的。
const maxMessageBodyRunes = 4000

// normalizeMessageBody 是正文的纯校验（无 IO，便于用例直接打边界）：裁掉两侧空白后必须非空，
// 长度按 rune 计。空/纯空白与超限都回 invalid_body —— 对调用方是同一件事：这个 body 不能发。
// 入库的是裁剪后的文本：私信的空白只在正文内部有意义，两侧空白是编辑器残留。
func normalizeMessageBody(raw string) (string, string) {
	body := strings.TrimSpace(raw)
	if body == "" {
		return "", "invalid_body"
	}
	if utf8.RuneCountInString(body) > maxMessageBodyRunes {
		return "", "invalid_body"
	}
	return body, ""
}

func (h *Handler) registerMessages(api *gin.RouterGroup) {
	// 会话读：分页走 page/page_size 写法（口径与换算见 paging.go），缺省页宽 20，
	// 排序**按时间倒序**——第一页是最近的 20 条，往后翻是更早的。
	api.GET("/messages/with/:id", h.guard(true), func(c *gin.Context) {
		peerID, ok := h.messagePeer(c)
		if !ok {
			return
		}
		limit, offset := pagingPageSize(c, 20)
		items, total, err := h.store.ListConversation(c.Request.Context(), h.principal(c).ID, peerID, limit, offset)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{"items": items, "total": total})
	})

	// 发信：sender 恒为当前用户，收件人只有"别人"一种可能。
	api.POST("/messages/with/:id", h.guard(true), func(c *gin.Context) {
		peerID, ok := h.messagePeer(c)
		if !ok {
			return
		}
		p := h.principal(c)
		// 不能给自己发：库里的 CHECK(sender_id <> recipient_id) 是同一口径的兜底，
		// 判定放在正文校验之前——收件人非法时，"body 写错了"不是调用方需要先知道的事。
		if peerID == p.ID {
			fail(c, 400, "invalid_recipient")
			return
		}
		var in struct {
			Body string `json:"body"`
		}
		if !body(c, &in) {
			return
		}
		text, code := normalizeMessageBody(in.Body)
		if code != "" {
			fail(c, 400, code)
			return
		}
		msg, err := h.store.SendMessage(c.Request.Context(), uuid.NewString(), p.ID, peerID, text)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{"message": msg})
	})
}

// messagePeer 解析并校验路径里的对方用户 id，非法字面量按"没有这个人"处理（404）：
// 送进 uuid 列只会拿到 pq 的解析错误，再被兜成 500 回显给客户端（与 /users/{id}/favorites 同一口径）。
//
// 读接口**不**额外拒绝 :id == 自己：库里不可能存在自己发给自己的行（CHECK 约束），
// 因此那只是一个恒空的会话；只有写接口按 400 invalid_recipient 拒掉。
func (h *Handler) messagePeer(c *gin.Context) (string, bool) {
	peerID := strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(peerID); err != nil {
		fail(c, 404, "not_found")
		return "", false
	}
	return peerID, true
}
