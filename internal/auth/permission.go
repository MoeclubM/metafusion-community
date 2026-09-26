package auth

// 论坛侧授权的集中判定：以权限码为准（S01：显式空权限不回落 admin，管理 API 默认拒第三方）。
//
// 权限码由账号服务（metafusion-auth）装进权限组，随访问令牌的 claims 与 /api/auth/me 下发
// （admin 组带 * 通配）。码名必须与账号服务的权限清单逐字一致
// （metafusion-auth/internal/store/access.go 的 PermissionCatalog 与 seed_groups.go 播种的
// 系统组）：两边都只读令牌、谁也不查对方的库，唯一需要对齐的就是码的拼写。
//
// 收敛计划：这些码的单一来源将是协议层仓库 metafusion-sdk（常量 + 生成物），本服务改为依赖它，
// 不再靠"逐字抄写 + 代码评审"对齐（见 docs/architecture/decoupling-audit-2026-09.md §3）。
// 本批次只登记这条路径，不引入依赖：码表仍由本文件声明，且必须与账号服务逐字一致。
// 组码（community_moderator 等）与角色都不参与判定——散落的角色比较正是
// 「后台分配了权限组、本服务却不认」的成因。

const (
	// PermissionPostCreate 发帖：发主题、回帖与发表短评（member 组即持有）。
	PermissionPostCreate = "community.post.create"
	// PermissionPostModerate 管理帖子：处置**他人**的主题、回复与短评
	// （community_moderator 与 community_admin 都持有）。
	PermissionPostModerate = "community.post.moderate"
	// PermissionTopicPin 置顶主题：PUT /api/community/topics/:id/pin 用它（handler/forum.go）。
	PermissionTopicPin = "community.topic.pin"
	// PermissionBoardManage 管理板块：PUT /api/community/boards/:code 用它（handler/board.go），
	// 当前只覆盖"改一个已存在的板块"（names/descriptions/color/icon/排序/开关），
	// 板块的新增与删除仍由种子与后台完成。
	PermissionBoardManage = "community.board.manage"
	// PermissionReportReview 审阅举报与申诉：/api/community/admin/reports* 与
	// /api/community/admin/appeals* 用它（handler/reports.go）。
	//
	// 与 community.post.moderate 的分界：post.moderate 管"内容本身能不能留"（删主题/回复/短评），
	// 举报码管"这条举报怎么处置"（受理/驳回/记录处置结论）。两者不互相蕴含：
	// 只有 community_admin 与 community_moderator 同时持有两个码（见账号服务的系统组播种）。
	PermissionReportReview = "community.report.review"

	// permissionWildcard 是账号服务给的「全部权限」码（admin 组）。
	permissionWildcard = "*"
)

// communityPermissionCodes 是本服务声明的全部论坛权限码。
var communityPermissionCodes = []string{
	PermissionPostCreate,
	PermissionPostModerate,
	PermissionTopicPin,
	PermissionBoardManage,
	PermissionReportReview,
}

// HasPermission 只看权限码；* 通配即全权，空集合不授予任何权限。
func (p *Principal) HasPermission(code string) bool {
	if p == nil {
		return false
	}
	return hasCode(p.Permissions, permissionWildcard) || hasCode(p.Permissions, code)
}

// Can 报告身份是否持有某权限码；匿名和第三方治理请求不放行。
func (p *Principal) Can(code string) bool {
	if p == nil {
		return false
	}
	if p.IsThirdParty && isGovernanceCode(code) {
		return false
	}
	return p.HasPermission(code)
}

// isGovernanceCode 报告是否为治理类（管理）权限码：发帖（community.post.create）是用户
// 行为，不在此列；其余本服务声明的码都是管理动作，第三方令牌默认拒绝。
func isGovernanceCode(code string) bool {
	if code == PermissionPostCreate {
		return false
	}
	return hasCode(communityPermissionCodes, code)
}

// hasCode 是权限码集合的成员判定：逐字比较，不做前缀或大小写归一 ——
// 码由账号服务下发，拼写不一致必须表现为"不放行"，而不是被兜底掩盖。
func hasCode(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}
