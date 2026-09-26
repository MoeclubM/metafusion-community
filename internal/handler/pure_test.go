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

// 板块名的四语齐备判据（用户决议：论坛只去掉语言维度，不去字段）：板块名与描述是语种 map，
// 缺任一语种都按 four_locale_names_required 拒掉，与目录侧 definitions / shelves /
// external_databases 的标识同名——前端复用同一套错误文案。
func TestResolveBoardLocalesRequiresFourLocales(t *testing.T) {
	full := map[string]string{"zh-CN": "问答", "zh-TW": "問答", "ja-JP": "質問", "en-US": "Q&A"}
	full["zh-CN"] = "  问答  " // 裁剪空白后才判非空
	got, code := resolveBoardLocales(full)
	if code != "" {
		t.Fatalf("四语齐备不应报错：%s", code)
	}
	if got["zh-CN"] != "问答" || got["ja-JP"] != "質問" {
		t.Fatalf("语种值应裁剪空白后原样保留：%v", got)
	}

	// 缺语种：错误码带缺的语种，顺序与 boardLocales 一致。
	if _, code = resolveBoardLocales(map[string]string{"zh-CN": "问答", "en-US": "Q&A"}); code != "four_locale_names_required: zh-TW,ja-JP" {
		t.Fatalf("缺语种错误码 = %q", code)
	}
	// 空白值等同缺该语种，不能被当成"有值"。
	if _, code = resolveBoardLocales(map[string]string{"zh-CN": "问答", "zh-TW": "問答", "ja-JP": "  ", "en-US": "Q&A"}); code != "four_locale_names_required: ja-JP" {
		t.Fatalf("空白语种值的错误码 = %q", code)
	}
	// 四语全空是"显式清空"，由调用方决定是否允许（板块名不允许、描述允许）。
	if emptied, code := resolveBoardLocales(map[string]string{"zh-CN": "", "zh-TW": "", "ja-JP": "", "en-US": ""}); code != "" || len(emptied) != 0 {
		t.Fatalf("四语全空应判为清空：%v / %s", emptied, code)
	}
}
