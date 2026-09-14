package handler

import (
	"testing"

	"github.com/MoeclubM/metafusion-community/internal/auth"
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
