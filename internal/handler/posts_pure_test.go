package handler

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MoeclubM/metafusion-community/internal/store"
)

// 摘要的边界：折叠空白、按 rune 截断（不是按字节），恰好等于上限时不加省略号。
// 按字节截断会把中文切成半个字符（非法 UTF-8），这个用例就是防这件事。
func TestExcerptRunesCollapsesWhitespaceAndTruncates(t *testing.T) {
	if got, cut := excerptRunes("  hello\n\nworld\t!", 200); got != "hello world !" || cut {
		t.Fatalf("折叠空白后应为 %q / 未截断，实际 %q / %v", "hello world !", got, cut)
	}
	if got, cut := excerptRunes("", 200); got != "" || cut {
		t.Fatalf("空正文应为空串且不截断，实际 %q / %v", got, cut)
	}
	if got, cut := excerptRunes("   \n\t ", 200); got != "" || cut {
		t.Fatalf("纯空白应折叠成空串，实际 %q / %v", got, cut)
	}

	exact := strings.Repeat("字", moderationExcerptRunes)
	if got, cut := excerptRunes(exact, moderationExcerptRunes); got != exact || cut {
		t.Fatalf("恰好到上限不应截断：长度 %d，截断 %v", utf8.RuneCountInString(got), cut)
	}

	long := strings.Repeat("字", moderationExcerptRunes+50)
	got, cut := excerptRunes(long, moderationExcerptRunes)
	if !cut {
		t.Fatal("超过上限必须标记截断")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("截断结果必须是合法 UTF-8（按 rune 截断）：%q", got)
	}
	if n := utf8.RuneCountInString(got); n != moderationExcerptRunes+1 {
		t.Fatalf("摘要长度应为 %d（上限 + 省略号），实际 %d", moderationExcerptRunes+1, n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("截断后应以省略号结尾：%q", got)
	}

	// max<=0 不做截断（调用方的取值由常量决定，这里只钉住"不 panic、不返回空"）。
	if got, cut := excerptRunes("abc", 0); got != "abc" || cut {
		t.Fatalf("max<=0 应原样返回，实际 %q / %v", got, cut)
	}
}

// storeModerationRow 造一行存储层结果：带作者快照、未引用任何楼层。
// 作者为空的回落（Anonymous）由 store 的 SQL 负责，本用例只钉字段映射——真库用例覆盖回落本身。
func storeModerationRow() store.ModerationPost {
	return store.ModerationPost{
		ID: "post-1", TopicID: "topic-1", TopicTitle: "标题", BoardCode: "qa",
		AuthorID: "author-1", AuthorName: "kana", Body: "正文", PostNumber: 2,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
}

// 对外形状：字段名与裁剪后的正文口径固定，改动会破坏治理台与第三方消费者。
func TestModerationPostItemShape(t *testing.T) {
	item := newModerationPostItem(storeModerationRow())
	if item.ID != "post-1" || item.TopicID != "topic-1" || item.TopicTitle != "标题" || item.BoardCode != "qa" {
		t.Fatalf("治理上下文字段映射不对：%+v", item)
	}
	if item.AuthorID != "author-1" || item.AuthorName != "kana" || item.PostNumber != 2 {
		t.Fatalf("作者与楼层字段映射不对：%+v", item)
	}
	if item.ReplyTo != nil {
		t.Fatalf("未引用楼层时应为 null，实际 %v", *item.ReplyTo)
	}
	if item.Excerpt != "正文" || item.Truncated {
		t.Fatalf("短正文不应被截断：%q / %v", item.Excerpt, item.Truncated)
	}
}
