package store

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 举报与申诉的结构不变量（离线）：词表两边一致、重复拦截靠部分唯一索引而不是应用层先查后插。
// 表与索引的形状由 schema_parity_test.go 的冻结清单钉住，这里只补它表达不了的两条。

// TestReportWordListsMatchMigrationChecks：Go 侧词表与迁移里的 CHECK 必须完全一致。
// 两边漂移的后果是不对称的：代码有而库里没有 → 500；库里有而代码没有 → 那个值永远进不来，
// 而两种都不会被编译、vet 或真库用例发现（真库用例只走合法值）。
func TestReportWordListsMatchMigrationChecks(t *testing.T) {
	ddl := schemaDDL(t)
	cases := []struct {
		column string
		want   [][]string
	}{
		{"target_type", [][]string{ReportTargetTypes}},
		{"reason", [][]string{ReportReasons}},
		// status 出现在三张表里，三份词表各不相同：先收齐再按集合比对。
		{"status", [][]string{
			{ReportPending, ReportAccepted, ReportRejected, ReportResolved},
			{AppealPending, AppealAccepted, AppealRejected},
			{OutboxPending, OutboxSent, OutboxFailed, OutboxExpired},
		}},
		{"enforcement", [][]string{{"", EnforcementContentRemoved, EnforcementUserBanned, EnforcementNone}}},
	}
	for _, tc := range cases {
		// 列定义中间允许夹一个 DEFAULT 子句（status / enforcement 都带默认值）。
		re := regexp.MustCompile("(?is)" + tc.column + " text NOT NULL (?:DEFAULT '[^']*' )?CHECK\\(" + tc.column + " IN \\(([^)]*)\\)\\)")
		found := re.FindAllStringSubmatch(ddl, -1)
		if len(found) != len(tc.want) {
			t.Fatalf("%s 的 CHECK 约束找到 %d 条，期望 %d 条", tc.column, len(found), len(tc.want))
		}
		got := [][]string{}
		for _, m := range found {
			list := []string{}
			for _, raw := range strings.Split(m[1], ",") {
				list = append(list, strings.Trim(strings.TrimSpace(raw), "'"))
			}
			// 比集合不比顺序：ReportReasons 的顺序是**展示顺序**（由重到轻），
			// 而 CHECK 约束里的顺序只是书写顺序，两者不必一致。
			sort.Strings(list)
			got = append(got, list)
		}
		sortedWant := [][]string{}
		for _, list := range tc.want {
			copyList := append([]string{}, list...)
			sort.Strings(copyList)
			sortedWant = append(sortedWant, copyList)
		}
		sort.Slice(sortedWant, func(i, j int) bool { return strings.Join(sortedWant[i], ",") < strings.Join(sortedWant[j], ",") })
		sort.Slice(got, func(i, j int) bool { return strings.Join(got[i], ",") < strings.Join(got[j], ",") })
		for i := range sortedWant {
			if strings.Join(got[i], ",") != strings.Join(sortedWant[i], ",") {
				t.Fatalf("%s 的第 %d 份词表与 Go 侧不一致：库 %v / 代码 %v", tc.column, i, got[i], sortedWant[i])
			}
		}
	}
}

// TestReportDuplicateGuardsArePartialUniqueIndexes：重复举报 / 重复申诉由**部分唯一索引**兜底，
// 而不是应用侧的"先查再插"（并发下必漏）。索引被删或 WHERE 条件被改宽/改窄，
// 编译与真库用例都不会报错——只有这里会。
func TestReportDuplicateGuardsArePartialUniqueIndexes(t *testing.T) {
	flat := strings.ToLower(strings.Join(strings.Fields(schemaDDL(t)), " "))
	for _, want := range []string{
		"create unique index if not exists reports_open_unique on community.reports(reporter_id, target_type, target_id) where status in ('pending','accepted');",
		"create unique index if not exists report_appeals_once on community.report_appeals(report_id, appellant_id);",
	} {
		if !strings.Contains(flat, want) {
			t.Fatalf("迁移里缺少这条拦截（重复提交会插出第二行）：%s", want)
		}
	}
}
