package auth

import "testing"

// 权限码是拆服务后的授权来源：令牌带上 permissions 后，码说了算——
// 这正是"后台分配了权限组、本服务也该认"的那一条。
func TestCanAllowsByPermissionCode(t *testing.T) {
	moderator := &Principal{
		ID:          "u-1",
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
	member := &Principal{ID: "u-2", Permissions: []string{PermissionPostCreate}}
	if member.Can(PermissionPostModerate) {
		t.Fatal("只有 community.post.create 的成员不得治理帖子")
	}
	if member.Can(PermissionBoardManage) {
		t.Fatal("普通成员不得管理板块")
	}
	adminWithoutCode := &Principal{ID: "u-3", Permissions: []string{"community.post.create"}}
	if adminWithoutCode.Can(PermissionPostModerate) {
		t.Fatal("令牌带了 permissions 时以码为准：缺少 community.post.moderate 就不放行，即便角色是 admin")
	}
}

// * 通配即全权（admin 组带的就是这一个码）。
func TestCanHonoursWildcard(t *testing.T) {
	p := &Principal{ID: "u-4", Groups: []string{"admin"}, Permissions: []string{"*"}}
	for _, code := range communityPermissionCodes {
		if !p.Can(code) {
			t.Fatalf("通配符应放行本服务全部码，%s 未放行", code)
		}
	}
}

func TestCanDeniesEmptyPermissions(t *testing.T) {
	plain := &Principal{ID: "u-5", Username: "kana"}
	for _, code := range communityPermissionCodes {
		if plain.Can(code) {
			t.Fatalf("空权限不得放行 %s", code)
		}
	}
	var anon *Principal
	if anon.Can(PermissionPostCreate) || anon.Can(PermissionPostModerate) {
		t.Fatal("匿名（nil）不得放行")
	}
}

// S01：显式空权限（含空数组、PermissionsSet）不得回落 admin，即使角色是 admin。
func TestCanDeniesExplicitEmptyAdmin(t *testing.T) {
	explicitEmpty := &Principal{ID: "u-8", Permissions: []string{}}
	for _, code := range communityPermissionCodes {
		if explicitEmpty.Can(code) {
			t.Fatalf("显式空权限不得回落 admin：%s 不该放行", code)
		}
	}
	nonNilEmpty := &Principal{ID: "u-9", Permissions: []string{}}
	for _, code := range []string{PermissionPostModerate, PermissionTopicPin, PermissionBoardManage, PermissionReportReview} {
		if nonNilEmpty.Can(code) {
			t.Fatalf("非 nil 空集合不得回落 admin：%s 不该放行", code)
		}
	}
}

// S01：第三方 OAuth 身份在治理码上直接不放行；发帖码仍以码为准（空即不放行）。
func TestCanDeniesThirdPartyGovernance(t *testing.T) {
	thirdPartyAdmin := &Principal{
		ID: "u-10", Groups: []string{"admin"}, Permissions: []string{"*"},
		Scope: "openid profile", ClientID: "third-party-app", IsThirdParty: true}
	for _, code := range []string{PermissionPostModerate, PermissionTopicPin, PermissionBoardManage, PermissionReportReview} {
		if thirdPartyAdmin.Can(code) {
			t.Fatalf("第三方令牌不得放行治理码 %s：即使带 * 通配", code)
		}
	}
	thirdPartyPoster := &Principal{
		ID: "u-11", Permissions: []string{PermissionPostCreate},
		Scope: "profile", ClientID: "third-party-app", IsThirdParty: true}
	if !thirdPartyPoster.Can(PermissionPostCreate) {
		t.Fatal("第三方令牌的发帖码仍以码为准：持有即放行")
	}
	if thirdPartyPoster.Can(PermissionPostModerate) {
		t.Fatal("第三方令牌未持有治理码不得放行")
	}
}
