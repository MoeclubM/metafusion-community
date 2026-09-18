package handler

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/audit"
)

// 私信（DM）：五个端点都在登录门槛（guard(true)）之后，与其它私有数据接口同一口径。
//
// "只能看见/发送自己参与的会话"不是处理器里的一条 if，而是查询本身的结构：
// 会话由 (当前用户, 对方) 两个参与者决定（见 store.ListConversation / ListConversations），
// 请求里也没有"会话 id"这种可以指向别人会话的输入，因此越权访问没有入口——
// 第三者的私信查出来就是空列表（回到 404/空页，而不是 403 之外的另一种形状）。
// 收件箱列表同理：它按"我的那一侧"（recipient_id = 我 OR sender_id = 我）分组，
// 别人的会话不在结果集里；标记已读的 UPDATE 也恒带 recipient_id = 我。
//
// 对方 id 只当外部引用用：账号数据归账号服务，本服务不查它的库、也不校验对方是否存在，
// 只校验字面量是不是 uuid。
//
// 契约（与前端调用的形状逐字一致）：
//
//	GET  /api/messages/with/{id}?page=1&page_size=20 -> {"items":[...],"total":N}
//	POST /api/messages/with/{id} {"body":"..."}      -> {"message":{...}}
//	PUT  /api/messages/with/{id}/read                -> {"marked":N}
//	GET  /api/messages/conversations?page&page_size   -> {"items":[{peer_id,last_message,unread_count}],"total":N}
//	GET  /api/messages/unread                        -> {"unread_count":N}
//	GET  /api/messages/settings                      -> {"accept_from_strangers":bool}
//	PUT  /api/messages/settings {"accept_from_strangers":bool} -> {"accept_from_strangers":bool}
//
// 收件人侧开关与发信侧的机器码：
// **收件人关闭"接收陌生人私信"后，陌生人（这一对之间还没有任何私信的人）发信会拿到
// 403 recipient_not_accepting_messages —— 稳定机器码，前端有对应文案；**不再静默丢弃**。
// 已有会话的一方不受影响（关闭开关的效果是"只接收已经聊过的人的私信"）。
// 陌生人**发起新会话**另外受每小时 5 个的额度约束（见 message_limit.go），超限 429 rate_limited。

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
		// 频率限制放在正文校验之前：超限的调用方需要知道的是"慢一点"，
		// 而不是"这条正文格式不对"；先校验会让刷量请求拿到逐条不同的错误码。
		// 审计行用 changes.limit 区分是哪一档拦的（机器码统一为 rate_limited）。
		if ok, retryAfter := h.messages.allow(p.ID); !ok {
			audit.Describe(c, audit.Detail{TargetType: "user", TargetID: peerID, Changes: map[string]any{
				"limit": "sender_window",
			}})
			c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
			fail(c, http.StatusTooManyRequests, "rate_limited")
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
		// 收件人侧的开关与"这一对是不是陌生人"一次问清（store.SendPolicy，一条查询）。
		// 判定放在正文校验之后：正文是本地 CPU 校验，先做它就不必为一份垃圾载荷去读库。
		policy, err := h.store.SendPolicy(c.Request.Context(), p.ID, peerID)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		if !policy.HasConversation && !policy.AcceptsFromStrangers {
			// 稳定机器码，**不是静默丢弃**：发送方要知道自己没发出去、以及为什么。
			// 审计行留痕（被拒的写请求恰恰是最该留痕的一类），被动对象是想发给谁。
			audit.Describe(c, audit.Detail{TargetType: "user", TargetID: peerID, Changes: map[string]any{
				"rejected": "recipient_disallows_strangers",
			}})
			fail(c, http.StatusForbidden, "recipient_not_accepting_messages")
			return
		}
		if !policy.HasConversation {
			// 陌生人新会话额度（第 2 档）：已有会话的一方不扣它，因此"继续聊"永远不会被它拦住。
			if ok, retryAfter := h.messages.allowStranger(p.ID); !ok {
				audit.Describe(c, audit.Detail{TargetType: "user", TargetID: peerID, Changes: map[string]any{
					"limit": "new_stranger",
				}})
				c.Header("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
				fail(c, http.StatusTooManyRequests, "rate_limited")
				return
			}
		}
		msg, err := h.store.SendMessage(c.Request.Context(), uuid.NewString(), p.ID, peerID, text)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		// 私信正文是私有内容，**绝不进审计**（不是"脱敏后可以进"的问题：审计表按设计不存请求体原文）。
		// 被动对象是收件人，changes 只留行 id，够把审计行与 community.direct_messages 对上。
		audit.Describe(c, audit.Detail{TargetType: "user", TargetID: peerID, Changes: map[string]any{
			"message_id": msg.ID,
		}})
		c.JSON(200, gin.H{"message": msg})
	})

	// 收件箱列表：我参与的会话（按对方分组）与每段会话我未读的条数。
	// 一条 SQL 出整页（见 store.ListConversations），不是"先列会话再逐条查最新一条"。
	api.GET("/messages/conversations", h.guard(true), func(c *gin.Context) {
		limit, offset := pagingPageSize(c, 20)
		items, total, err := h.store.ListConversations(c.Request.Context(), h.principal(c).ID, limit, offset)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{"items": items, "total": total})
	})

	// 未读总数：导航栏角标用。与收件箱每一行的 unread_count 同一个口径（read_at IS NULL 计数），
	// 因此两处不会互相矛盾；不存在的用户/没有未读都是 0（账号数据不归本服务）。
	api.GET("/messages/unread", h.guard(true), func(c *gin.Context) {
		n, err := h.store.UnreadCount(c.Request.Context(), h.principal(c).ID)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{"unread_count": n})
	})

	// 标记已读：把"对方发给我、我还没读的"全部置为已读，回本次影响条数。
	//
	// 刻意不做成"GET 会话时顺手置位"：读接口带写副作用会让缓存、重试与审计都说不清
	// （一个 GET 既读又写，还回写 X-Request-Id），而这个动作有它自己的审计动作码 message.read。
	// 幂等：重复调用第二次影响 0 行（read_at IS NULL 过滤），也不会刷新回执时间。
	api.PUT("/messages/with/:id/read", h.guard(true), func(c *gin.Context) {
		peerID, ok := h.messagePeer(c)
		if !ok {
			return
		}
		marked, err := h.store.MarkConversationRead(c.Request.Context(), h.principal(c).ID, peerID)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		// 审计只记"对谁标记了、几条"，不记任何正文；target 是对方（被动对象是会话的另一端）。
		audit.Describe(c, audit.Detail{TargetType: "user", TargetID: peerID, Changes: map[string]any{
			"marked": marked,
		}})
		c.JSON(200, gin.H{"marked": marked})
	})

	// 收件设置（我自己的）：读当前值。
	//
	// 响应是**扁平**的一个布尔，不套信封：只有一个字段的设置再包一层 {"settings":…} 只会让
	// 每个调用点多写一次解包；与 /api/users/{id}/stats 那种"多字段成组"的形状不同，不必强行统一。
	api.GET("/messages/settings", h.guard(true), func(c *gin.Context) {
		accept, err := h.store.MessageSettings(c.Request.Context(), h.principal(c).ID)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{"accept_from_strangers": accept})
	})

	// 收件设置：改自己的值。字段用 *bool 区分"没传"与"传了 false"——
	// Go 的 bool 零值会让"漏传字段"被当成"用户要关闭"，那是最坏的一种默认。
	api.PUT("/messages/settings", h.guard(true), func(c *gin.Context) {
		p := h.principal(c)
		var in struct {
			AcceptFromStrangers *bool `json:"accept_from_strangers"`
		}
		if !body(c, &in) {
			return
		}
		if in.AcceptFromStrangers == nil {
			fail(c, 400, "invalid_payload")
			return
		}
		before, err := h.store.MessageSettings(c.Request.Context(), p.ID)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		after, err := h.store.SetMessageSettings(c.Request.Context(), p.ID, *in.AcceptFromStrangers)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		// 设置项进审计：被动对象是**设置的所有者本人**（不是对方），changes 记前后值。
		// 这类"隐私开关被谁在什么时候改了"的问题，只能靠审计回答。
		audit.Describe(c, audit.Detail{TargetType: "user", TargetID: p.ID, Changes: map[string]any{
			"accept_from_strangers": auditChange(before, after),
		}})
		c.JSON(200, gin.H{"accept_from_strangers": after})
	})
}

// messagePeer 解析并校验路径里的对方用户 id，非法字面量按"没有这个人"处理（404）：
// 送进 uuid 列只会拿到 pq 的解析错误，再被兜成 500 回显给客户端（与 /users/{id}/favorites 同一口径）。
//
// 读接口**不**额外拒绝 :id == 自己：库里不可能存在自己发给自己的行（CHECK 约束），
// 因此那只是一个恒空的会话；只有写接口按 400 invalid_recipient 拒掉
// （标记已读打自己的 id 同样只是 0 行，不是错误）。
func (h *Handler) messagePeer(c *gin.Context) (string, bool) {
	peerID := strings.TrimSpace(c.Param("id"))
	if _, err := uuid.Parse(peerID); err != nil {
		fail(c, 404, "not_found")
		return "", false
	}
	return peerID, true
}
