package handler

import (
	"strings"
	"testing"

	"github.com/MoeclubM/metafusion-community/internal/store"
)

// 举报与申诉的离线用例：不需要数据库，钉住的是"词表/判据/形状"这类纯逻辑。
// 真库链路由 reports_postgres_test.go 覆盖（未设置 COMMUNITY_TEST_DSN 时跳过）。

func TestCanAppealRules(t *testing.T) {
	me, other := "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	cases := []struct {
		name      string
		report    store.Report
		viewer    string
		hasAppeal bool
		want      bool
	}{
		{"待处理的举报还不能申诉（还没被处置）", store.Report{Status: store.ReportPending, TargetAuthorID: me}, me, false, false},
		{"被驳回的举报不能申诉（处置结论是举报不成立）", store.Report{Status: store.ReportRejected, TargetAuthorID: me}, me, false, false},
		{"已受理 + 本人 + 未申诉 → 可申诉", store.Report{Status: store.ReportAccepted, TargetAuthorID: me}, me, false, true},
		{"已处置 + 本人 + 未申诉 → 可申诉", store.Report{Status: store.ReportResolved, TargetAuthorID: me}, me, false, true},
		{"不是被处置方 → 不可申诉", store.Report{Status: store.ReportResolved, TargetAuthorID: other}, me, false, false},
		{"已经申诉过 → 不可再申诉", store.Report{Status: store.ReportResolved, TargetAuthorID: me}, me, true, false},
		{"实体/资源类目标没有本地被处置方 → 不可申诉", store.Report{Status: store.ReportResolved}, me, false, false},
		{"匿名（没有身份）→ 不可申诉", store.Report{Status: store.ReportResolved, TargetAuthorID: me}, "", false, false},
	}
	for _, tc := range cases {
		if got := canAppeal(tc.report, tc.viewer, tc.hasAppeal); got != tc.want {
			t.Fatalf("%s：canAppeal = %v，期望 %v", tc.name, got, tc.want)
		}
	}
}

func TestParseStatusesIgnoresBlanksButKeepsUnknown(t *testing.T) {
	if got := parseStatuses(""); len(got) != 0 {
		t.Fatalf("空串应解析成空列表：%v", got)
	}
	got := parseStatuses("pending, ,accepted,")
	if len(got) != 2 || got[0] != "pending" || got[1] != "accepted" {
		t.Fatalf("应忽略空白项并保留顺序：%v", got)
	}
	// 非法项**不在这里丢弃**：丢了它就变成"看起来没过滤"，调用方（listReports）负责回 400。
	if got := parseStatuses("nonsense"); len(got) != 1 || got[0] != "nonsense" {
		t.Fatalf("非法项必须原样返回给调用方判定：%v", got)
	}
}

func TestValidEvidenceURL(t *testing.T) {
	good := []string{"https://example.com/a", "http://example.com/证据"}
	for _, url := range good {
		if !validEvidenceURL(url) {
			t.Fatalf("%s 应被接受", url)
		}
	}
	bad := []string{"", "javascript:alert(1)", "data:text/html;base64,AAAA", "ftp://example.com/x", "/relative/path", "https://" + strings.Repeat("a", reportURLMaxRunes)}
	for _, url := range bad {
		if validEvidenceURL(url) {
			t.Fatalf("%s 不该被接受", url)
		}
	}
}

func TestReportContextSnapshot(t *testing.T) {
	// 本地没有这份内容（实体 / 资源）：快照为空对象，而不是伪造字段。
	if got := reportContext(store.ReportedContent{}); len(got) != 0 {
		t.Fatalf("非本地内容的快照应为空：%v", got)
	}
	body := strings.Repeat("长", reportExcerptRunes+10)
	got := reportContext(store.ReportedContent{
		Kind: "comment", Found: true, AuthorID: "u1", BoardCode: "comment",
		TopicID: "t1", TopicTitle: "标题", EntityID: "e1", Body: body,
	})
	if got["content_kind"] != "comment" || got["board_code"] != "comment" || got["topic_id"] != "t1" || got["entity_id"] != "e1" {
		t.Fatalf("快照字段不全：%v", got)
	}
	if got["excerpt_truncated"] != true {
		t.Fatalf("超长正文应标记截断：%v", got)
	}
	excerpt, _ := got["excerpt"].(string)
	if n := len([]rune(excerpt)); n > reportExcerptRunes+1 {
		t.Fatalf("摘要长度 %d 超过上限：%q", n, excerpt)
	}
}

func TestLocalContentTargetOnlyCommentsAndPosts(t *testing.T) {
	if !localContentTarget(store.ReportTargetComment) || !localContentTarget(store.ReportTargetPost) {
		t.Fatal("短评与帖子是本地内容（可解析作者、可下线）")
	}
	for _, target := range []string{store.ReportTargetEntity, store.ReportTargetUser, store.ReportTargetResource} {
		if localContentTarget(target) {
			t.Fatalf("%s 不在本服务：不能按本地内容处置", target)
		}
	}
}
