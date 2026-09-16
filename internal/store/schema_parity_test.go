package store

import (
	"regexp"
	"strings"
	"testing"
)

// frozenTableDefs 是**从主仓库 modules 包逐列抄下来的**社区表定义（列名 + 类型 + 约束 + 默认值）。
//
// 为什么要冻结：互动服务里大量 handler 是从单体直接搬过来的，它们的 SQL 是按老表结构写的。
// 一旦本服务的建表语句与老表有任何漂移（少一列、类型不同、丢了默认值），
// 编译与静态检查都不会报错，却会在切流后的第一次真实写入时 500。
//
// 外键目标 schema 用 SCHEMA. 占位：老表指向 modules.*，新表指向 community.*，
// 这是唯一允许的差异。community.favorites 不在本表内（见 favorites 的对齐测试）。
var frozenTableDefs = map[string]map[string]string{
	"community.boards": {
		"code":         "code text primary key",
		"name":         "name text not null default ''",
		"description":  "description text not null default ''",
		"color":        "color text not null default 'emerald'",
		"icon":         "icon text not null default 'bookopen'",
		"sort_order":   "sort_order int not null default 0",
		"is_enabled":   "is_enabled boolean not null default true",
		"show_in_feed": "show_in_feed boolean not null default true",
	},
	"community.topics": {
		"id":               "id uuid primary key",
		"board_code":       "board_code text not null references SCHEMA.boards(code)",
		"author_id":        "author_id uuid not null",
		"author_name":      "author_name text not null default ''",
		"title":            "title text not null",
		"body":             "body text not null",
		"entity_id":        "entity_id uuid",
		"is_pinned":        "is_pinned boolean not null default false",
		"is_locked":        "is_locked boolean not null default false",
		"view_count":       "view_count int not null default 0",
		"reply_count":      "reply_count int not null default 0",
		"created_at":       "created_at timestamptz not null default now()",
		"updated_at":       "updated_at timestamptz not null default now()",
		"last_activity_at": "last_activity_at timestamptz not null default now()",
	},
	"community.posts": {
		"id":                   "id uuid primary key",
		"topic_id":             "topic_id uuid not null references SCHEMA.topics(id) on delete cascade",
		"author_id":            "author_id uuid not null",
		"author_name":          "author_name text not null default ''",
		"body":                 "body text not null",
		"post_number":          "post_number int not null",
		"reply_to_post_number": "reply_to_post_number int",
		"created_at":           "created_at timestamptz not null default now()",
		"updated_at":           "updated_at timestamptz not null default now()",
	},
	"community.tags": {
		"id":   "id bigserial primary key",
		"name": "name text not null",
		"slug": "slug text not null unique",
	},
	"community.topic_tags": {
		"topic_id": "topic_id uuid not null references SCHEMA.topics(id) on delete cascade",
		"tag_id":   "tag_id bigint not null references SCHEMA.tags(id) on delete cascade",
	},
	"community.records": {
		"owner_id":  "owner_id uuid not null",
		"entity_id": "entity_id uuid not null",
		"document":  "document jsonb not null",
	},
}

var (
	createTableRe = regexp.MustCompile(`(?is)^\s*create table if not exists\s+([a-z_.]+)\s*\(([\s\S]*)\)\s*$`)
	identRe       = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	// 增量迁移（000002 起）用 ALTER 改结构，结构测试必须把 ADD/DROP COLUMN 也算进终态，
	// 否则"测试看到的库"永远停在 000001 的基线形状上。
	alterAddRe  = regexp.MustCompile(`(?is)^\s*alter table\s+([a-z_.]+)\s+add column if not exists\s+([a-z_][a-z0-9_]*)\s+(.+?)\s*$`)
	alterDropRe = regexp.MustCompile(`(?is)^\s*alter table\s+([a-z_.]+)\s+drop column if exists\s+([a-z_][a-z0-9_]*)\s*$`)
	// DO $$ ... $$; 块里是带条件的回填/守卫逻辑，不是结构声明：先整体剥掉再按分号切语句
	// （块内的分号会让简单的切分器把一条语句切成几段）。
	doBlockRe = regexp.MustCompile(`(?is)do\s+\$\$.*?\$\$\s*;`)
)

// splitTopLevel 按顶级分隔符切分，忽略括号内的分隔符（列定义里含 (id) 这类括号）。
func splitTopLevel(s, sep string) []string {
	out := []string{}
	depth := 0
	cur := strings.Builder{}
	for _, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		}
		if string(ch) == sep && depth == 0 {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(ch)
	}
	out = append(out, cur.String())
	return out
}

func normalizeDef(s string) string {
	if i := strings.Index(s, "--"); i >= 0 {
		s = s[:i]
	}
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	s = strings.TrimSuffix(s, ",")
	return strings.ReplaceAll(s, "community.", "SCHEMA.")
}

// stripLineComments 去掉 -- 行注释：注释出现在建表语句前会让"语句以 CREATE 开头"的匹配失效。
func stripLineComments(ddl string) string {
	lines := strings.Split(ddl, "\n")
	for i, line := range lines {
		if j := strings.Index(line, "--"); j >= 0 {
			lines[i] = line[:j]
		}
	}
	return strings.Join(lines, "\n")
}

func parseSchema(t *testing.T, ddl string) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	for _, stmt := range splitTopLevel(doBlockRe.ReplaceAllString(stripLineComments(ddl), ""), ";") {
		if m := alterAddRe.FindStringSubmatch(stmt); m != nil {
			if cols, ok := out[strings.ToLower(strings.TrimSpace(m[1]))]; ok {
				cols[m[2]] = normalizeDef(m[2] + " " + m[3])
			}
			continue
		}
		if m := alterDropRe.FindStringSubmatch(stmt); m != nil {
			if cols, ok := out[strings.ToLower(strings.TrimSpace(m[1]))]; ok {
				delete(cols, m[2])
			}
			continue
		}
		m := createTableRe.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		table := strings.ToLower(strings.TrimSpace(m[1]))
		cols := map[string]string{}
		for _, entry := range splitTopLevel(m[2], ",") {
			def := normalizeDef(entry)
			if def == "" {
				continue
			}
			name := strings.Fields(def)[0]
			switch name {
			case "primary", "unique", "foreign", "check", "constraint":
				continue
			}
			if !identRe.MatchString(name) {
				continue
			}
			cols[name] = def
		}
		out[table] = cols
	}
	return out
}

// 建表语句必须与老表逐列一致：搬运的 SQL、双向导入工具都依赖这一点。
func TestSchemaMatchesLegacyTableShape(t *testing.T) {
	parsed := parseSchema(t, schemaDDL(t))
	for table, want := range frozenTableDefs {
		got, ok := parsed[table]
		if !ok {
			t.Fatalf("建表语句缺少表 %s", table)
		}
		for col, wantDef := range want {
			gotDef, ok := got[col]
			if !ok {
				t.Fatalf("%s 缺少列 %s（老表定义：%s）", table, col, wantDef)
			}
			if gotDef != wantDef {
				t.Fatalf("%s.%s 定义漂移：\n  期望 %s\n  实际 %s", table, col, wantDef, gotDef)
			}
		}
		for col := range got {
			if _, ok := want[col]; !ok {
				t.Fatalf("%s 多出列 %s（老表没有这一列，搬运时要显式决定）", table, col)
			}
		}
	}
}
