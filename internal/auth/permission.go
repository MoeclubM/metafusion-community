package auth

// 论坛侧授权的集中判定：以权限码为准，角色只在老令牌上兜底。
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
	// PermissionTopicPin 置顶主题。本服务当前没有置顶写接口，码先声明以便接口落地时直接用。
	PermissionTopicPin = "community.topic.pin"
	// PermissionBoardManage 管理板块：板块增删改与开关。
	// 本服务当前没有板块管理接口（板块是运营配置，由种子与后台写入），码先声明。
	PermissionBoardManage = "community.board.manage"

	// permissionWildcard 是账号服务给的「全部权限」码（admin 组）。
	permissionWildcard = "*"
	// adminRole 是完全没有权限声明时的历史角色兜底判据。
	adminRole = "admin"
)

// communityPermissionCodes 是本服务声明的全部论坛权限码：角色兜底只认这些码，
// 不会因为角色是 admin 就放行别的子系统的码（catalog.* / storage.* / auth.* 归各自服务）。
var communityPermissionCodes = []string{
	PermissionPostCreate,
	PermissionPostModerate,
	PermissionTopicPin,
	PermissionBoardManage,
}

// Can 报告身份是否持有某权限码；身份为 nil（匿名）一律不放行。
//
// 令牌带 permissions 时**一律以码为准**（* 通配即全权）：拆服务后这是唯一的授权来源，
// 此时角色不再额外放行，否则「角色兜底」会变成绕过权限组的后门。
// 只有令牌完全没有 permissions 声明时（老令牌，或尚未按权限组配置的实例）才按历史角色兜底：
// admin 放行本服务全部码，其它角色与匿名不放行——保证老令牌与本服务现有边界一致，不被打死。
func (p *Principal) Can(code string) bool {
	if p == nil {
		return false
	}
	if len(p.Permissions) > 0 {
		for _, perm := range p.Permissions {
			if perm == permissionWildcard || perm == code {
				return true
			}
		}
		return false
	}
	if p.Role != adminRole {
		return false
	}
	for _, allowed := range communityPermissionCodes {
		if allowed == code {
			return true
		}
	}
	return false
}
