package audit

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	"github.com/MoeclubM/metafusion-community/migrations"
)

// auditMigrationFile 是审计表的迁移落点（契约 §6.1 给互动服务指定的版本号）。
// 只追加：这一版之后的结构变更必须新增 000008_*.up.sql，不能改本文件（改已应用的文件会被
// internal/migrator 的 checksum 守卫拒绝启动）。本版本发布前用过一次这套例外：
// DDL 从"顶层 CREATE ... IF NOT EXISTS"改成 to_regclass 守卫（否则非 owner 角色启动即 42501），
// 见 TestAuditSchemaUsesExistenceGuard。
const auditMigrationFile = "000007_audit_log.up.sql"

// TestSchemaMatchesMigrationFile：包里的 Schema 常量与迁移文件必须**逐字一致**。
//
// 两边漂移的后果很具体：服务启动实际执行的是迁移文件，而四个服务之间对照的是各自的 Schema 常量。
// 少一列/改一个默认值都不会在编译或其它用例里报错，只会在第一次写审计行时以"列不存在"炸掉。
func TestSchemaMatchesMigrationFile(t *testing.T) {
	raw, err := fs.ReadFile(migrations.FS, auditMigrationFile)
	if err != nil {
		t.Fatalf("读取 migrations/%s: %v", auditMigrationFile, err)
	}
	file := stripSQLComments(string(raw))
	if !strings.Contains(file, strings.TrimSpace(Schema)) {
		t.Fatalf("迁移文件没有逐字包含 audit.Schema：\n--- 文件（去注释后）---\n%s\n--- 期望包含 ---\n%s",
			file, strings.TrimSpace(Schema))
	}
	// 四个服务共用同一个 advisory 锁键（740205）串行建表：键不同会让两条 CREATE TABLE 撞在
	// pg_type 的唯一索引上；该键与各服务自己的迁移锁（catalog 740202 / auth 740203 /
	// storage 740204）区分开——互动服务自己的迁移锁恰好也是 740205，同一事务内重复获取是安全的。
	for _, want := range []string{
		"DO $audit_ddl$",
		"IF to_regclass('audit.audit_log') IS NULL THEN",
		"PERFORM pg_advisory_xact_lock(740205)",
		"CREATE SCHEMA IF NOT EXISTS audit",
		"CREATE TABLE audit.audit_log (",
	} {
		if !strings.Contains(Schema, want) {
			t.Fatalf("Schema 缺少 %q", want)
		}
	}
}

// TestAuditSchemaUsesExistenceGuard 是回归守卫：建表段必须整段包在 to_regclass 判断里，
// 且**不能**再用 CREATE INDEX IF NOT EXISTS；锁必须在守卫内、建表之前取。
//
// 理由（真库实测）：PostgreSQL 的 CREATE INDEX IF NOT EXISTS **先做表所有权检查、再看索引是否存在**
// （CREATE TABLE IF NOT EXISTS 不同，它只要求 schema 的 CREATE）。表由部署时的 mf_audit_owner 预建时，
// 非 owner 的运行角色执行这段 DDL 会拿到 42501 must be owner of table audit_log，四个服务全部起不来。
// 有人把守卫拆掉、或把索引写回 IF NOT EXISTS（哪怕放在守卫外面）这条用例就会红。
func TestAuditSchemaUsesExistenceGuard(t *testing.T) {
	const guard = "IF to_regclass('audit.audit_log') IS NULL THEN"
	guardAt := strings.Index(Schema, guard)
	if guardAt < 0 {
		t.Fatal("建表段必须用 to_regclass 守卫：非 owner 角色执行 CREATE INDEX 会 42501（见 Schema 注释）")
	}
	if strings.Contains(Schema, "CREATE INDEX IF NOT EXISTS") {
		t.Fatal("索引不能写成 CREATE INDEX IF NOT EXISTS：它会先查表所有权，非 owner 角色启动即失败")
	}
	if strings.Contains(Schema, "CREATE TABLE IF NOT EXISTS audit.audit_log") {
		t.Fatal("建表不再需要 IF NOT EXISTS：守卫内那条本来就是刚建表，写着它反而让人以为守卫可以被去掉")
	}
	lockAt := strings.Index(Schema, "PERFORM pg_advisory_xact_lock(740205)")
	if lockAt < 0 {
		t.Fatal("跨服务建表锁必须在守卫内、建表之前取（PERFORM），才能串行化四个服务的建表")
	}
	if lockAt > guardAt {
		t.Fatal("锁必须在 to_regclass 判断之前取：先取锁再判断存在性，四个服务才不会撞在 CREATE TABLE 上")
	}
	if tableAt := strings.Index(Schema, "CREATE TABLE audit.audit_log ("); tableAt < guardAt {
		t.Fatal("建表语句必须在守卫内（表已存在的实例上不允许再执行任何 DDL）")
	}
	if !strings.Contains(Schema[guardAt:], "END IF;") || !strings.HasSuffix(strings.TrimSpace(Schema), "$audit_ddl$;") {
		t.Fatal("守卫必须是完整的 DO $audit_ddl$ ... END IF; END $audit_ddl$; 块")
	}
}

// frozenAuditColumns 是契约 §1 的 18 列（列名 → 归一后的定义）。
// 这份清单跨四个服务相同：任何一列被改名、改类型、丢默认值或丢 CHECK，都应当在这里失败。
var frozenAuditColumns = map[string]string{
	"id":               "id uuid primary key",
	"occurred_at":      "occurred_at timestamptz not null default now()",
	"service":          "service text not null",
	"action":           "action text not null",
	"actor_user_id":    "actor_user_id uuid",
	"actor_username":   "actor_username text not null default ''",
	"credential_type":  "credential_type text not null default ''",
	"actor_ip":         "actor_ip text not null default ''",
	"actor_user_agent": "actor_user_agent text not null default ''",
	"target_type":      "target_type text not null default ''",
	"target_id":        "target_id text not null default ''",
	"changes":          "changes jsonb not null default '{}'::jsonb",
	"result":           "result text not null default 'success' check (result in ('success','failure'))",
	"error_code":       "error_code text not null default ''",
	"request_method":   "request_method text not null default ''",
	"route":            "route text not null default ''",
	"http_status":      "http_status int not null default 0",
	"request_id":       "request_id text not null default ''",
}

// frozenAuditIndexes 是契约 §1 的四条索引（名字 → "表 (表达式清单)"）。
// 索引被删、表达式换写法或 DESC 写成 ASC，查询都会退化成全表扫描，而编译与功能用例都不报错。
var frozenAuditIndexes = map[string]string{
	"audit_log_occurred_at_idx":    "audit.audit_log (occurred_at desc)",
	"audit_log_service_action_idx": "audit.audit_log (service, action, occurred_at desc)",
	"audit_log_actor_idx":          "audit.audit_log (actor_user_id, occurred_at desc)",
	"audit_log_target_idx":         "audit.audit_log (target_type, target_id, occurred_at desc)",
}

// TestAuditTableColumnsAreFrozen：从 Schema 解析出 audit.audit_log 的列，与契约 §1 逐列比对。
// 多一列也要失败：多出来的列没有任何写入方，只会在排障时让人以为"这里存过东西"。
func TestAuditTableColumnsAreFrozen(t *testing.T) {
	got := guardedAuditTable(t, Schema)
	for col, want := range frozenAuditColumns {
		if got[col] != want {
			t.Fatalf("audit.audit_log.%s 定义漂移：\n  期望 %s\n  实际 %s", col, want, got[col])
		}
	}
	for col := range got {
		if _, ok := frozenAuditColumns[col]; !ok {
			t.Fatalf("audit.audit_log 多出列 %s：审计表的列形状跨四个服务共用，新增列要同步契约与四个服务", col)
		}
	}
	if len(got) != len(frozenAuditColumns) {
		t.Fatalf("列数 = %d，期望 %d", len(got), len(frozenAuditColumns))
	}
}

// TestAuditIndexesAreFrozen：四条索引的名字、目标表与表达式清单逐字冻结。
func TestAuditIndexesAreFrozen(t *testing.T) {
	re := regexp.MustCompile(`(?is)create index\s+([a-z_]+)\s+on\s+([a-z_.]+)\s*\(([^)]*)\)`)
	got := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(Schema, -1) {
		got[m[1]] = strings.ToLower(strings.TrimSpace(m[2])) + " (" +
			strings.Join(strings.Fields(strings.ToLower(m[3])), " ") + ")"
	}
	for name, want := range frozenAuditIndexes {
		if got[name] != want {
			t.Fatalf("索引 %s 定义漂移：\n  期望 %s\n  实际 %s", name, want, got[name])
		}
	}
	if len(got) != len(frozenAuditIndexes) {
		t.Fatalf("索引数 = %d，期望 %d（%v）", len(got), len(frozenAuditIndexes), got)
	}
}

// auditCreateTableRe 从守卫里取建表段：DDL 现在整段包在 DO $audit_ddl$ 里，
// 「顶层 CREATE TABLE IF NOT EXISTS」那种解析方式（internal/store 的 parseSchema）在它上面失效，
// 因此这里直接按建表语句的头尾取列清单。
var auditCreateTableRe = regexp.MustCompile(`(?is)CREATE TABLE audit\.audit_log\s*\((.*?)\n\s*\);`)

// guardedAuditTable 取出 audit.audit_log 的列集合（列名 → 归一后的定义）。
func guardedAuditTable(t *testing.T, ddl string) map[string]string {
	t.Helper()
	m := auditCreateTableRe.FindStringSubmatch(stripSQLComments(ddl))
	if m == nil {
		t.Fatalf("审计 DDL 里找不到 CREATE TABLE audit.audit_log (...)：\n%s", ddl)
	}
	cols := map[string]string{}
	for _, entry := range splitTopLevel(m[1]) {
		def := normalizeDef(entry)
		if def == "" {
			continue
		}
		name := strings.Fields(def)[0]
		switch name {
		case "primary", "unique", "foreign", "check", "constraint":
			continue
		}
		cols[name] = def
	}
	return cols
}

// splitTopLevel 按顶级逗号切分，忽略括号内的逗号（列定义里有 CHECK (result IN (...))）。
func splitTopLevel(s string) []string {
	out := []string{}
	depth, start := 0, 0
	for i, ch := range s {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// normalizeDef 把一段列定义归一成"小写 + 单空格"：排版差异（缩进、换行）不参与比对。
func normalizeDef(def string) string {
	if i := strings.Index(def, "--"); i >= 0 {
		def = def[:i]
	}
	return strings.ToLower(strings.Join(strings.Fields(def), " "))
}

// stripSQLComments 去掉 -- 行注释：迁移文件的说明文字不参与"逐字一致"的比对。
func stripSQLComments(ddl string) string {
	lines := strings.Split(ddl, "\n")
	for i, line := range lines {
		if j := strings.Index(line, "--"); j >= 0 {
			lines[i] = line[:j]
		}
	}
	return strings.Join(lines, "\n")
}
