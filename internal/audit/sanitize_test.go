package audit

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fullEmailPattern 是"完整邮箱"的判据，与契约 §6.2 的真库用例同一形状。
// 它**必须**匹配不到遮罩后的串（j***@example.com：@ 前的 * 不在本地部分字符集里）。
var fullEmailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+.[A-Za-z]{2,}`)

// TestSanitizeChangesRedactsSecretKeys：契约 §4 的两张键名黑名单（精确 + 子串）都必须整键替换成
// [redacted]。子串黑名单是"宁可少记"的那一层——漏一个键名等于把凭据写进审计表。
func TestSanitizeChangesRedactsSecretKeys(t *testing.T) {
	in := map[string]any{
		"password": "hunter2", "old_password": "a", "new_password": "b", "password_hash": "$2a$10$x",
		"token": "t", "access_token": "t", "refresh_token": "t", "token_hash": "t",
		"secret": "s", "client_secret": "s", "secret_hash": "s", "api_key": "k",
		"authorization": "Bearer x", "cookie": "mf_session=y", "code_verifier": "v",
		// 子串命中：键名里出现 password / secret / token / hash 一律整键丢弃
		"user_password": "p", "Access-Token": "t", "client_secret_v2": "s", "sha256_hash": "h",
		// 大小写与空白不参与判定
		" PASSWORD ": "p", "Token": "t",
	}
	out := SanitizeChanges(in)
	for k := range in {
		if got := out[k]; got != Redacted {
			t.Fatalf("%q 未被脱敏：%#v", k, got)
		}
	}
}

// TestSanitizeChangesMasksEmails：值里的邮箱、键名含 email 的字段都必须遮罩，
// 遮罩后的串不能再被完整邮箱正则命中（这是真库用例断言的同一件事）。
func TestSanitizeChangesMasksEmails(t *testing.T) {
	in := map[string]any{
		"title":      "联系 kana@example.com 或 ops@sub.example.co.jp",
		"email":      "kana@example.com",
		"user_email": "a@b.com",
		"nested": map[string]any{
			"note": "抄送 cc@example.org",
		},
		"list":  []any{"x@y.com", map[string]any{"email": "z@w.net"}},
		"plain": "没有邮箱的普通文本",
	}
	out := SanitizeChanges(in)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if m := fullEmailPattern.FindAllString(string(raw), -1); len(m) != 0 {
		t.Fatalf("脱敏后仍能匹配到完整邮箱：%v（%s）", m, raw)
	}
	for _, want := range []string{"k***@example.com", "o***@sub.example.co.jp", "a***@b.com", "c***@example.org", "x***@y.com", "z***@w.net"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("缺少遮罩形式 %q：%s", want, raw)
		}
	}
	if out["plain"] != "没有邮箱的普通文本" {
		t.Fatalf("普通文本不该被动：%#v", out["plain"])
	}
}

// TestSanitizeChangesTruncatesLongValues：单个字符串值 512（按 rune 截断，多字节不留半个字）。
func TestSanitizeChangesTruncatesLongValues(t *testing.T) {
	long := strings.Repeat("汉", 600)
	out := SanitizeChanges(map[string]any{"body": long})
	got, _ := out["body"].(string)
	runes := []rune(got)
	if len(runes) != maxValueLen+1 || !strings.HasSuffix(got, "…") {
		t.Fatalf("长度 = %d 个 rune（尾部 %q），期望 %d 个字 + 省略号", len(runes), string(runes[len(runes)-1]), maxValueLen)
	}
	if !strings.HasPrefix(got, strings.Repeat("汉", 10)) {
		t.Fatalf("截断把多字节字符切坏了：%q", got[:min(12, len(got))])
	}
	// 恰好等于上限的值不动：截断只在超限时发生。
	exact := strings.Repeat("a", maxValueLen)
	if got := SanitizeChanges(map[string]any{"body": exact})["body"]; got != exact {
		t.Fatalf("恰好 %d 字符的值被改动了：%d 字符", maxValueLen, len([]rune(got.(string))))
	}
}

// TestSanitizeChangesRecursesAndBoundsDepth：嵌套结构要逐层脱敏；深度超限时整块替换成 [redacted]，
// 不能在请求路径上被自引用结构拖住。
func TestSanitizeChangesRecursesAndBoundsDepth(t *testing.T) {
	var deep any = map[string]any{"token": "leak"}
	for i := 0; i < 12; i++ {
		deep = map[string]any{"level": deep}
	}
	out := SanitizeChanges(map[string]any{"deep": deep, "locales": map[string]string{"zh-CN": strings.Repeat("字", 600)}})
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "leak") {
		t.Fatalf("嵌套里的凭据泄漏了：%s", raw)
	}
	if !strings.Contains(string(raw), Redacted) {
		t.Fatalf("深度超限应整块替换成 %s：%s", Redacted, raw)
	}
	locales, ok := out["locales"].(map[string]any)
	if !ok {
		t.Fatalf("map[string]string 应递归成 map[string]any：%#v", out["locales"])
	}
	if len([]rune(locales["zh-CN"].(string))) != maxValueLen+1 {
		t.Fatalf("语种 map 的值没有按 512 截断：%d", len([]rune(locales["zh-CN"].(string))))
	}
}

// TestMarshalChangesCapsAtEightKB：changes 序列化后 8KB，超限时换成一个可读的截断标记
// （留下 _keys 才能知道"本该记什么"），而不是半截 JSON。
func TestMarshalChangesCapsAtEightKB(t *testing.T) {
	big := map[string]any{}
	for i := 0; i < 100; i++ {
		big["field_"+strconv.Itoa(i)] = strings.Repeat("x", maxValueLen)
	}
	raw, err := marshalChanges(SanitizeChanges(big))
	if err != nil {
		t.Fatalf("marshalChanges: %v", err)
	}
	if len(raw) > maxChangesLen {
		t.Fatalf("序列化后 %d 字节，超过 8KB", len(raw))
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("截断结果必须是合法 JSON：%v（%s）", err, raw)
	}
	if parsed["_truncated"] != true {
		t.Fatalf("缺少 _truncated 标记：%s", raw)
	}
	if keys, _ := parsed["_keys"].([]any); len(keys) != len(big) {
		t.Fatalf("_keys 应列出全部键名，实际 %d 个（期望 %d）", len(keys), len(big))
	}
	// 未超限的摘要原样序列化，不带标记。
	small, err := marshalChanges(SanitizeChanges(map[string]any{"a": "b"}))
	if err != nil || strings.Contains(small, "_truncated") {
		t.Fatalf("小摘要不该被截断：%s（%v）", small, err)
	}
	if empty, err := marshalChanges(nil); err != nil || empty != "{}" {
		t.Fatalf("空摘要应写成 {}：%q（%v）", empty, err)
	}
}

// TestPrepareFillsDefaultsAndSanitizes：prepare 是入库前的唯一加工点——补齐 id/时间/service/result，
// 并再脱敏一次（调用点忘了脱敏也不会写进库）。
func TestPrepareFillsDefaultsAndSanitizes(t *testing.T) {
	r := NewRecorder(nil, ServiceName)
	defer r.Close()
	e := r.prepare(Entry{
		Action:         "topic.created",
		ActorUserAgent: strings.Repeat("a", 900),
		ActorUsername:  "kana",
		Changes:        map[string]any{"token": "t", "title": "标题"},
	})
	if e.ID == "" || e.OccurredAt.IsZero() || e.Service != ServiceName || e.Result != ResultSuccess {
		t.Fatalf("默认值没补齐：%#v", e)
	}
	if len([]rune(e.ActorUserAgent)) != maxUserAgent+1 {
		t.Fatalf("UA 未按 512 截断：%d", len([]rune(e.ActorUserAgent)))
	}
	if e.Changes["token"] != Redacted || e.Changes["title"] != "标题" {
		t.Fatalf("changes 未脱敏：%#v", e.Changes)
	}
	// 调用方给过 result 的不覆盖（失败行不能被悄悄改写成成功）。
	if got := r.prepare(Entry{Result: ResultFailure}).Result; got != ResultFailure {
		t.Fatalf("result 被覆盖成 %q", got)
	}
	// 时间用 UTC：审计行的 occurred_at 要与日志时间轴对得上。
	if e.OccurredAt.Location() != time.UTC {
		t.Fatalf("occurred_at 时区 = %v，期望 UTC", e.OccurredAt.Location())
	}
}

// TestMaskEmailAndMaskSecret：两个导出遮罩器的边界（契约 §4 的例子逐字实现）。
func TestMaskEmailAndMaskSecret(t *testing.T) {
	for in, want := range map[string]string{
		"j@example.com":           "j***@example.com",
		"kana.liu@sub.example.jp": "k***@sub.example.jp",
		"没有邮箱":                    "没有邮箱",
		"前缀 a@b.com 后缀 c@d.com":   "前缀 a***@b.com 后缀 c***@d.com",
	} {
		if got := MaskEmail(in); got != want {
			t.Fatalf("MaskEmail(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := MaskEmail(MaskEmail("j@example.com")); got != "j***@example.com" {
		t.Fatalf("遮罩不幂等：%q", got)
	}
	for in, want := range map[string]string{
		"abcd1234": "abcd…",
		"a":        "*",
		"abcd":     "****",
		"":         "",
	} {
		if got := MaskSecret(in); got != want {
			t.Fatalf("MaskSecret(%q) = %q，期望 %q", in, got, want)
		}
	}
}
