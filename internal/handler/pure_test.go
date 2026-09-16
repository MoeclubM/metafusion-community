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

// 板块名称与目录侧定义名称同一口径：zh-CN / zh-TW / en-US 逐个必需，
// 日文接受 ja 或 ja-JP 其中之一。缺失语种要能被逐个列出（前端据此提示补哪几语）。
func TestBoardNameLocalesRequireFourLanguages(t *testing.T) {
	full := map[string]string{"zh-CN": "问答", "zh-TW": "問答", "ja-JP": "質問", "en-US": "Q&A"}
	if missing := missingBoardNameLocales(full); len(missing) != 0 {
		t.Fatalf("四语齐备不应报缺失，实际 %v", missing)
	}
	// ja 与 ja-JP 等价：只写 ja 也算齐备（存量键名两种都有）。
	withJa := map[string]string{"zh-CN": "问答", "zh-TW": "問答", "ja": "質問", "en-US": "Q&A"}
	if missing := missingBoardNameLocales(withJa); len(missing) != 0 {
		t.Fatalf("只写 ja 也应算齐备，实际 %v", missing)
	}
	partial := map[string]string{"zh-CN": "问答", "en-US": "Q&A"}
	got := strings.Join(missingBoardNameLocales(partial), ",")
	if got != "zh-TW,ja-JP" {
		t.Fatalf("缺失语种 = %q，期望 zh-TW,ja-JP", got)
	}
	if got := strings.Join(missingBoardNameLocales(nil), ","); got != "zh-CN,zh-TW,en-US,ja-JP" {
		t.Fatalf("空名称应报全部语种缺失，实际 %q", got)
	}
	// 只有空白字符等于没填：不能因为键存在就放过。
	blank := map[string]string{"zh-CN": "  ", "zh-TW": "問答", "ja-JP": "質問", "en-US": "Q&A"}
	if got := strings.Join(missingBoardNameLocales(blank), ","); got != "zh-CN" {
		t.Fatalf("空白值应判为缺失，实际 %q", got)
	}
}
