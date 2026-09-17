package handler

import (
	"strings"
	"testing"
)

// 私信正文的校验是纯函数：这里把边界逐个钉住，真库用例只管往返、可见性与分页。
func TestNormalizeMessageBody(t *testing.T) {
	long := strings.Repeat("汉", maxMessageBodyRunes)
	cases := []struct {
		name string
		in   string
		want string
		code string
	}{
		{"普通文本", "你好", "你好", ""},
		{"裁剪两侧空白", "  第一条\n", "第一条", ""},
		{"保留正文内部换行", "第一行\n第二行", "第一行\n第二行", ""},
		{"空串", "", "", "invalid_body"},
		{"纯空白", " \n\t\u3000 ", "", "invalid_body"},
		{"刚好 4000 个汉字", long, long, ""},
		{"超一个汉字", strings.Repeat("汉", maxMessageBodyRunes+1), "", "invalid_body"},
		{"刚好 4000 个 ASCII", strings.Repeat("a", maxMessageBodyRunes), strings.Repeat("a", maxMessageBodyRunes), ""},
		{"超一个 ASCII", strings.Repeat("a", maxMessageBodyRunes+1), "", "invalid_body"},
	}
	for _, tc := range cases {
		got, code := normalizeMessageBody(tc.in)
		if code != tc.code {
			t.Fatalf("%s：错误码 = %q，期望 %q", tc.name, code, tc.code)
		}
		if got != tc.want {
			t.Fatalf("%s：正文 = %q（%d 字符），期望 %q", tc.name, got, len([]rune(got)), tc.want)
		}
	}

	// 上限按**字符**算而不是按字节：4000 个汉字有 12000 字节，按字节判定会把这份合法正文误杀。
	if len(long) <= maxMessageBodyRunes {
		t.Fatalf("用例前提不成立：上限 %d 个汉字的字节数 %d 应大于上限本身", maxMessageBodyRunes, len(long))
	}
}
