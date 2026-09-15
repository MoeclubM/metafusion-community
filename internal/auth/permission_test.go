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

// 兼容：老令牌（没有 permissions 声明）与尚未按权限组配置的实例按角色兜底 ——
// admin 放行本服务全部码，其它角色与匿名不放行，边界与本服务改造前一致。
func TestCanFallsBackToRoleForLegacyTokens(t *testing.T) {
	legacyAdmin := &Principal{ID: "u-5", Username: "kana", Role: "admin"}
	for _, code := range communityPermissionCodes {
		if !legacyAdmin.Can(code) {
			t.Fatalf("老令牌的 admin 应放行本服务全部码，%s 未放行", code)
		}
	}
	// 角色兜底只覆盖本服务声明的码，别的子系统的码不归论坛判。
	for _, foreign := range []string{"catalog.entity.edit", "storage.asset.upload", "auth.groups.manage", "community.something.else"} {
		if legacyAdmin.Can(foreign) {
			t.Fatalf("角色兜底不得放行非本服务权限码：%s", foreign)
		}
	}
	for _, role := range []string{"editor", "user", ""} {
		p := &Principal{ID: "u-6", Role: role}
		for _, code := range communityPermissionCodes {
			if p.Can(code) {
				t.Fatalf("角色 %q 的老令牌不得凭角色放行 %s", role, code)
			}
		}
	}
	if (&Principal{}).Can(PermissionPostModerate) {
		t.Fatal("没有任何权限与角色的令牌不得放行")
	}
	var anon *Principal
	if anon.Can(PermissionPostModerate) {
		t.Fatal("匿名（nil）不得放行")
	}
}
