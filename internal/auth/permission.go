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
	// PermissionTopicPin 置顶主题：PUT /api/community/topics/:id/pin 用它（handler/forum.go）。
	PermissionTopicPin = "community.topic.pin"
	// PermissionBoardManage 管理板块：PUT /api/community/boards/:code 用它（handler/board.go），
	// 当前只覆盖"改一个已存在的板块"（names/descriptions/color/icon/排序/开关），
	// 板块的新增与删除仍由种子与后台完成。
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

// legacyOpenCodes 是老令牌（claims 里没有 permissions）在角色兜底之外仍然放行的码。
//
// 只有发帖码在这一列：在这条码成为闸门之前，发主题/回帖/短评**只要求登录**，任何已登录用户
// 都能发。账号服务尚未升级、令牌还不带 permissions 的实例如果按"角色兜底只认 admin"处理，
// 就会变成"除了管理员谁都不能发帖"——那是把兼容策略做成了故障。
// 治理类码（moderate / pin / board.manage）不在此列：改造前它们同样只有 admin 能过，
// 现在保持 admin 兜底，边界不变。
var legacyOpenCodes = []string{PermissionPostCreate}

// HasPermission 是**纯权限码集合判定**：只看 permissions 里的码（* 通配即全权），
// 不做任何角色或历史边界兜底。PAT 身份（FromPAT）一律走它——PAT 的权限集合可能为空
// （scopes 里没有本服务的任何码），空集合必须表现为"什么都不许"：若顺着 legacyOpenCodes
// 兜底，一个 scopes=[] 的 PAT 就能发帖，收窄 scopes 也就形同虚设。
func (p *Principal) HasPermission(code string) bool {
	if p == nil {
		return false
	}
	return hasCode(p.Permissions, permissionWildcard) || hasCode(p.Permissions, code)
}

// Can 报告身份是否持有某权限码；身份为 nil（匿名）一律不放行。
//
// 令牌带 permissions 时**一律以码为准**（* 通配即全权）：拆服务后这是唯一的授权来源，
// 此时角色不再额外放行，否则「角色兜底」会变成绕过权限组的后门。
// 只有令牌完全没有 permissions 声明时（老令牌，或尚未按权限组配置的实例）才按历史边界兜底：
// 发帖类码见 legacyOpenCodes（收口前就是"登录即可"），其余码只认 admin —— 与本服务改造前一致。
//
// FromPAT（身份来自 PAT 内省）时**永不**回落到兜底：PAT 的权限就是账号服务算好的
// "用户自身权限 ∩ scopes"，scopes 空时就是空。若把它当"没有 permissions 声明"处理，
// 一个 scopes=[] 的 PAT 会顺着 legacyOpenCodes 拿到发帖权——收窄 scopes 也就形同虚设。
func (p *Principal) Can(code string) bool {
	if p == nil {
		return false
	}
	if len(p.Permissions) > 0 || p.FromPAT {
		return p.HasPermission(code)
	}
	if hasCode(legacyOpenCodes, code) {
		return true
	}
	return p.Role == adminRole && hasCode(communityPermissionCodes, code)
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
