package handler

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// 标签 slug 是标签的身份：同名标签必须得到同一个 slug，大小写与空白不产生新标签。
func TestTagSlugIsStable(t *testing.T) {
	cases := map[string]string{
		"原盘":         "原盘",
		"FLAC":       "flac",
		"Hi-Res ":    "hi-res",
		"  Hi Res  ": "hi-res",
		"a/b":        "a-b",
	}
	for in, want := range cases {
		if got := tagSlug(in); got != want {
			t.Fatalf("tagSlug(%q) = %q, 期望 %q", in, got, want)
		}
	}
	if tagSlug("Tag") != tagSlug(" tag ") {
		t.Fatal("大小写与空白不得产生不同 slug")
	}
}

// 评论与主题的作者展示名：登录用户用用户名，缺失时才回退占位，绝不写成空串。
func TestAuthorNameFallsBack(t *testing.T) {
	if got := authorName(&auth.Principal{ID: "u1", Username: "kana"}); got != "kana" {
		t.Fatalf("authorName = %q", got)
	}
	if got := authorName(&auth.Principal{ID: "u1"}); got != "User" {
		t.Fatalf("authorName without username = %q", got)
	}
	if got := authorName(nil); got != "User" {
		t.Fatalf("authorName(nil) = %q", got)
	}
}

// 评论板块必须是"不进信息流"的专用板块：这是评论与论坛主题的语义分界。
func TestCommentBoardExcludedFromFeed(t *testing.T) {
	found := false
	for _, b := range defaultBoards {
		if b.Code == commentBoard {
			found = true
			if b.InFeed {
				t.Fatal("评论板块不得进入信息流")
			}
		}
	}
	if !found {
		t.Fatal("默认板块缺少评论板块")
	}
}

// 仓储层错误到 HTTP 的映射：只有"目标类型不合法"是 400，
// 其余一律 500 module_error —— 数据库原文绝不能冒充错误码回给客户端。
func TestStoreErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"目标类型不合法", store.ErrInvalidTargetType, 400, "invalid_target_type"},
		{"包装后的目标类型不合法", fmt.Errorf("wrap: %w", store.ErrInvalidTargetType), 400, "invalid_target_type"},
		{"数据库故障", errors.New(`pq: invalid input syntax for type uuid: "x" (22P02)`), 500, "module_error"},
	}
	for _, tc := range cases {
		if got := storeErrorStatus(tc.err); got != tc.status {
			t.Fatalf("%s 状态码 = %d，期望 %d", tc.name, got, tc.status)
		}
		if got := storeErrorCode(tc.err); got != tc.code {
			t.Fatalf("%s 错误码 = %q，期望 %q", tc.name, got, tc.code)
		}
		if code := storeErrorCode(tc.err); strings.Contains(code, "pq:") {
			t.Fatalf("%s 错误码回显了数据库原文：%q", tc.name, code)
		}
	}
}

// 板块名称的四语校验用例已随"论坛不再分语言"移除（2026-09-16）：板块只有单语言 name，
// 校验退化为"非空"，覆盖在 board_postgres_test.go 的 TestBoardUpdateRequiresCodeAndValidatesNames 里。
// 四语齐备规则仍然适用于目录侧的 definitions / shelves / external_databases，那些在 catalog 侧校验。
