package handler

import (
	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/audit"
	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// 本文件是互动服务的「写路由 → 动作码」注册表与审计接线。
//
// 注册表是审计的唯一开关：没登记的路由一个字节都不写（中间件据此短路）。新增写端点必须在
// auditActions 里登记动作码，否则「写路由覆盖守卫」测试（audit_coverage_test.go）会失败——
// 这是"新写接口忘了留痕"的唯一防线。
//
// 覆盖清单（本服务全部写路由，逐条对着 gin 路由树核过）：
//
//	POST   /api/community/topics                     topic.created     发主题
//	POST   /api/community/topics/:id/posts           post.created      回帖
//	POST   /api/community/entities/:id/posts         comment.created   条目短评
//	PUT    /api/community/topics/:id/pin             topic.pinned      置顶 / 取消置顶
//	PUT    /api/community/boards/:code               board.updated     板块配置
//	DELETE /api/community/topics/:id                 topic.deleted     删主题（评论行也走这条，见下）
//	DELETE /api/community/topics/:id/posts/:postId   post.deleted      删回复
//	DELETE /api/community/posts/:id                  comment.deleted   删短评（仅评论板块）
//	POST   /api/favorites/toggle                     favorite.toggled  收藏切换
//	POST   /api/messages/with/:id                    message.sent      发私信
//
// 两条"看起来像漏登记"的事实，都不是漏：
//   - DELETE /api/community/topics/:id **不排除评论板块**（存量评论行就存在 community.topics 里），
//     所以删除一条短评也可以走这条路由。动作码记的是"从哪条路由进来的"，行的真实归属由
//     changes 里的 board_code 表达；删短评的专用入口是 /api/community/posts/:id（comment.deleted）。
//   - GET /api/community/topics/:id 会把 view_count +1（读接口的副作用）。契约 §7 明确
//     "审计只记写操作、GET 不记"，因此它不进这份按 HTTP 方法判定的清单。
//
// **不存在的写能力**（任务书点名要核的几项）：板块只有"改已有板块"一个写入口，没有创建/删除
// （板块由种子播种，见 README「板块」）；封禁不在本服务（归账号服务），本服务没有任何 ban 端点；
// 私信的 read_at 列已预留，但两个端点都不读不写，因此没有"已读"动作码。
//
// 域名的选取：契约 §2 的清单里没有 favorite / message / comment 三个域（清单是"按业务对象分"
// 的示例）。这三个对象各自独立——收藏是用户行为、私信是私有会话、短评不是主题也不是回复——
// 塞进 entity / topic 只会让按动作码聚合时看不出究竟发生了什么。
var auditActions = map[string]string{
	"POST /api/community/topics":                     "topic.created",
	"POST /api/community/topics/:id/posts":           "post.created",
	"POST /api/community/entities/:id/posts":         "comment.created",
	"PUT /api/community/topics/:id/pin":              "topic.pinned",
	"PUT /api/community/boards/:code":                "board.updated",
	"DELETE /api/community/topics/:id":               "topic.deleted",
	"DELETE /api/community/topics/:id/posts/:postId": "post.deleted",
	"DELETE /api/community/posts/:id":                "comment.deleted",
	"POST /api/favorites/toggle":                     "favorite.toggled",
	"POST /api/messages/with/:id":                    "message.sent",
}

// auditExempt 是写路由的豁免表（路由模板 → 一句理由）。当前**为空**：本服务的 10 条写路由全部
// 登记了动作码，没有需要豁免的。保留这张表是为了让覆盖守卫能表达"这条写路由刻意不审计"，
// 并要求豁免必须写出理由（无理由的豁免等于漏登记）。
var auditExempt = map[string]string{}

// auditMiddleware 组装本服务的审计中间件：注册表 + 身份投影 + 写入器。
func (h *Handler) auditMiddleware() gin.HandlerFunc {
	return audit.Middleware(audit.Options{
		Recorder: h.audit,
		Actions:  auditActions,
		Exempt:   auditExempt,
		Actor:    communityActor,
	})
}

// communityActor 把 Principal 投影成审计行的操作者。
//
// credential_type 是**近似值**（契约 §7 已记录）：本服务只验签与内省，分不清"会话令牌"与
// "OAuth 令牌"，只能给 pat（Principal.FromPAT，即账号服务内省判定的 PAT）或 session（其余）；
// session 这一档把 OAuth 令牌也算了进去，精确区分只有账号服务做得到。
//
// 身份为 nil（匿名）时照样留痕：写路由的 401 是审计最有价值的一类行（谁在什么时候试过什么），
// 此时只有 IP / UA / 路由可记。
func communityActor(c *gin.Context) audit.Actor {
	p := auth.Current(c)
	if p == nil {
		return audit.Actor{CredentialType: audit.CredentialAnonymous}
	}
	kind := audit.CredentialSession
	if p.FromPAT {
		kind = audit.CredentialPAT
	}
	return audit.Actor{UserID: p.ID, Username: p.Username, CredentialType: kind}
}

// auditChange 是"变更前后"的统一形状：读审计的人不必为每个字段猜一次形状。
func auditChange(before, after any) map[string]any {
	return map[string]any{"from": before, "to": after}
}

// auditLocaleChanges 把语种 map 的差异写进 changes：只记真正变了的语种。
//
// 板块一次提交四语，整份记下来会把审计行撑到 8KB 上限（超限后只剩键名清单，反而看不出改了什么）；
// 而"哪个语种从什么变成了什么"才是要看的东西。清空（四语传空串）表现为 from 有值、to 为空串。
func auditLocaleChanges(field string, before, after map[string]string, changes map[string]any) {
	diff := map[string]any{}
	for locale, value := range after {
		if old, ok := before[locale]; !ok || old != value {
			diff[locale] = auditChange(old, value)
		}
	}
	for locale, old := range before {
		if _, ok := after[locale]; !ok {
			diff[locale] = auditChange(old, "")
		}
	}
	if len(diff) > 0 {
		changes[field] = diff
	}
}
