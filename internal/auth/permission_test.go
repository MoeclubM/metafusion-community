package auth

import "testing"

// 权限码是拆服务后的授权来源：令牌带上 permissions 后，码说了算——
// 这正是"后台分配了权限组、本服务也该认"的那一条。
func TestCanAllowsByPermissionCode(t *testing.T) {
	moderator := &Principal{
		ID:          "u-1",
		Role:        "user",
		Groups:      []string{"community_moderator"},
		Permissions: []string{PermissionPostCreate, PermissionPostModerate, PermissionTopicPin},
	}
	if !moderator.Can(PermissionPostModerate) {
		t.Fatal("持有 community.post.moderate 的版主应可治理帖子")
	}
	if !moderator.Can(PermissionPostCreate) {
		t.Fatal("持有 community.post.create 的版主应可发帖")
	}
	if moderator.Can(PermissionBoardManage) {
		t.Fatal("版主没有 community.board.manage，不得管理板块")
	}
}

// 拒绝：只发帖的普通成员不得治理他人的帖子；且权限码一旦声明，角色不再额外放行
// （否则"角色兜底"会变成绕过权限组的后门）。
func TestCanDeniesWithoutPermissionCode(t *testing.T) {
	member := &Principal{ID: "u-2", Role: "user", Permissions: []string{PermissionPostCreate}}
	if member.Can(PermissionPostModerate) {
		t.Fatal("只有 community.post.create 的成员不得治理帖子")
	}
	if member.Can(PermissionBoardManage) {
		t.Fatal("普通成员不得管理板块")
	}
	adminWithoutCode := &Principal{ID: "u-3", Role: "admin", Permissions: []string{"community.post.create"}}
	if adminWithoutCode.Can(PermissionPostModerate) {
		t.Fatal("令牌带了 permissions 时以码为准：缺少 community.post.moderate 就不放行，即便角色是 admin")
	}
}

// * 通配即全权（admin 组带的就是这一个码）。
func TestCanHonoursWildcard(t *testing.T) {
	p := &Principal{ID: "u-4", Role: "user", Groups: []string{"admin"}, Permissions: []string{"*"}}
	for _, code := range communityPermissionCodes {
		if !p.Can(code) {
			t.Fatalf("通配符应放行本服务全部码，%s 未放行", code)
		}
	}
}

// 兼容：老令牌（缺 permissions 键、非第三方、非 PAT）仅保留发帖历史边界（S01 已关闭 admin 兜底）。
// S01 起治理码不再设 admin 兜底；发帖码是收口前的“登录即可”，非 admin 角色仍可发（见 legacyOpenCodes）。
func TestCanFallsBackToRoleForLegacyTokens(t *testing.T) {
	legacyAdmin := &Principal{ID: "u-5", Username: "kana", Role: "admin"}
	if !legacyAdmin.Can(PermissionPostCreate) {
		t.Fatal("老令牌仍可发帖：收口前发帖只要求登录")
	}
	for _, code := range []string{PermissionPostModerate, PermissionTopicPin, PermissionBoardManage, PermissionReportReview} {
		if legacyAdmin.Can(code) {
			t.Fatalf("S01 起老令牌的 admin 也不得凭角色放行治理码 %s", code)
		}
	}
	// 角色兜底只覆盖本服务声明的码，别的子系统的码不归论坛判。
	for _, foreign := range []string{"catalog.entity.edit", "storage.asset.upload", "auth.groups.manage", "community.something.else"} {
		if legacyAdmin.Can(foreign) {
			t.Fatalf("角色兜底不得放行非本服务权限码：%s", foreign)
		}
	}
	// 非 admin 的老令牌：能发帖（收口前任何登录用户都能发），但不能治理、置顶或改板块。
	for _, role := range []string{"editor", "user", ""} {
		p := &Principal{ID: "u-6", Role: role}
		if !p.Can(PermissionPostCreate) {
			t.Fatalf("角色 %q 的老令牌应能发帖：收口前发帖只要求登录", role)
		}
		for _, code := range []string{PermissionPostModerate, PermissionTopicPin, PermissionBoardManage} {
			if p.Can(code) {
				t.Fatalf("角色 %q 的老令牌不得凭角色放行 %s", role, code)
			}
		}
	}
	if (&Principal{ID: "u-7", Role: "user", Permissions: []string{}}).Can(PermissionPostModerate) {
		t.Fatal("没有任何权限的老令牌不得放行治理码")
	}
	var anon *Principal
	if anon.Can(PermissionPostCreate) || anon.Can(PermissionPostModerate) {
		t.Fatal("匿名（nil）不得放行")
	}
}

// S01：显式空权限（含空数组、PermissionsSet）不得回落 admin，即使角色是 admin。
func TestCanDeniesExplicitEmptyAdmin(t *testing.T) {
	explicitEmpty := &Principal{ID: "u-8", Role: "admin", Permissions: []string{}, PermissionsSet: true}
	for _, code := range communityPermissionCodes {
		if explicitEmpty.Can(code) {
			t.Fatalf("显式空权限不得回落 admin：%s 不该放行", code)
		}
	}
	nonNilEmpty := &Principal{ID: "u-9", Role: "admin", Permissions: []string{}}
	for _, code := range []string{PermissionPostModerate, PermissionTopicPin, PermissionBoardManage, PermissionReportReview} {
		if nonNilEmpty.Can(code) {
			t.Fatalf("非 nil 空集合不得回落 admin：%s 不该放行", code)
		}
	}
}

// S01：第三方 OAuth 身份在治理码上直接不放行；发帖码仍以码为准（空即不放行）。
func TestCanDeniesThirdPartyGovernance(t *testing.T) {
	thirdPartyAdmin := &Principal{
		ID: "u-10", Role: "admin", Groups: []string{"admin"}, Permissions: []string{"*"},
		Scope: "openid profile", ClientID: "third-party-app", IsThirdParty: true, PermissionsSet: true,
	}
	for _, code := range []string{PermissionPostModerate, PermissionTopicPin, PermissionBoardManage, PermissionReportReview} {
		if thirdPartyAdmin.Can(code) {
			t.Fatalf("第三方令牌不得放行治理码 %s：即使带 * 通配", code)
		}
	}
	thirdPartyPoster := &Principal{
		ID: "u-11", Role: "user", Permissions: []string{PermissionPostCreate},
		Scope: "profile", ClientID: "third-party-app", IsThirdParty: true, PermissionsSet: true,
	}
	if !thirdPartyPoster.Can(PermissionPostCreate) {
		t.Fatal("第三方令牌的发帖码仍以码为准：持有即放行")
	}
	if thirdPartyPoster.Can(PermissionPostModerate) {
		t.Fatal("第三方令牌未持有治理码不得放行")
	}
}
