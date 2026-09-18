package audit

// 审计 DDL 的真库验证（契约 §6.1 的守卫改动）：守卫要保证两件事——
//   ① 表已存在（部署时由 mf_audit_owner 预建）→ 整段纯空转，且**非 owner 的运行角色**也能执行；
//   ② 表不存在 → 建表 + 四条索引真的建出来（串行化四个服务的首次启动）。
// 未设置 COMMUNITY_TEST_DSN 时整体跳过（与仓库其它真库用例同口径）。

import (
	"context"
	"database/sql"
	"net/url"
	"reflect"
	"testing"

	_ "github.com/lib/pq"

	"github.com/MoeclubM/metafusion-community/internal/testutil"
)

// auditShape 是"库里的审计表长什么样"的快照：列（按序号）与索引定义。
// 用它可以断言"执行 DDL 前后一字不差"，比只看"没报错"强：空转必须真的什么都没改。
type auditShape struct {
	Columns []string
	Indexes []string
}

// snapshotAuditShape 读 information_schema 与 pg_indexes；表不存在时返回空快照。
func snapshotAuditShape(t *testing.T, db *sql.DB, ctx context.Context) auditShape {
	t.Helper()
	out := auditShape{Columns: []string{}, Indexes: []string{}}
	rows, err := db.QueryContext(ctx, `SELECT column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '-')
		FROM information_schema.columns WHERE table_schema='audit' AND table_name='audit_log'
		ORDER BY ordinal_position`)
	if err != nil {
		t.Fatalf("查审计表列: %v", err)
	}
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("扫列: %v", err)
		}
		out.Columns = append(out.Columns, line)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		t.Fatalf("读列: %v", err)
	}
	irows, err := db.QueryContext(ctx,
		"SELECT indexname || ' ' || indexdef FROM pg_indexes WHERE schemaname='audit' AND tablename='audit_log' ORDER BY indexname")
	if err != nil {
		t.Fatalf("查审计表索引: %v", err)
	}
	defer irows.Close()
	for irows.Next() {
		var line string
		if err = irows.Scan(&line); err != nil {
			t.Fatalf("扫索引: %v", err)
		}
		out.Indexes = append(out.Indexes, line)
	}
	return out
}

func execAuditSchema(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx, Schema); err != nil {
		t.Fatalf("执行审计 DDL: %v", err)
	}
}

// TestAuditSchemaReapplyIsPureNoOpAgainstPostgres 是部署主路径的复现：
// 表已建好（迁移先跑过，或部署时由 mf_audit_owner 预建）之后，任何服务再执行整段 DDL
// 都必须是纯空转——否则四个服务的启动都依赖"自己恰好是表 owner"。
func TestAuditSchemaReapplyIsPureNoOpAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	db := testutil.Database(t)
	execAuditSchema(t, db, ctx) // 建出来（幂等：已存在时是空转）

	before := snapshotAuditShape(t, db, ctx)
	// 5 条索引 = 契约 §1 的四条 + 主键隐式建的 audit_log_pkey。
	if len(before.Columns) != 18 || len(before.Indexes) != len(frozenAuditIndexes)+1 {
		t.Fatalf("先决条件：审计表应是 18 列 + %d 索引（四条契约索引 + 主键），实际 %d 列 / %d 索引",
			len(frozenAuditIndexes)+1, len(before.Columns), len(before.Indexes))
	}
	// 表已存在 → 再执行整段（两次，覆盖"并发实例重复启动"的语义）
	for i := 0; i < 2; i++ {
		execAuditSchema(t, db, ctx)
	}
	after := snapshotAuditShape(t, db, ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("重复执行改动了结构：\n前 %+v\n后 %+v", before, after)
	}
}

// TestAuditSchemaRebuildsAfterDropAgainstPostgres 是守卫的另一半：
// 表不存在时必须真的建表并建出四条索引（否则"守卫"就成了"什么都不做"）。
func TestAuditSchemaRebuildsAfterDropAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	db := testutil.Database(t)
	execAuditSchema(t, db, ctx)
	// 无条件还原：本表由已登记的迁移版本建，删掉之后不会再自动重建，用例中途失败会污染整个测试库。
	t.Cleanup(func() { _, _ = db.ExecContext(context.Background(), Schema) })

	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS audit.audit_log"); err != nil {
		t.Fatalf("删表: %v", err)
	}
	if got := snapshotAuditShape(t, db, ctx); len(got.Columns) != 0 || len(got.Indexes) != 0 {
		t.Fatalf("删表后应为空快照，实际 %+v", got)
	}
	execAuditSchema(t, db, ctx)
	after := snapshotAuditShape(t, db, ctx)
	if len(after.Columns) != 18 {
		t.Fatalf("重建后的列数 = %d，期望 18：\n%v", len(after.Columns), after.Columns)
	}
	got := map[string]bool{}
	for _, line := range after.Indexes {
		for name := range frozenAuditIndexes {
			if len(line) > len(name) && line[:len(name)] == name {
				got[name] = true
			}
		}
	}
	for name := range frozenAuditIndexes {
		if !got[name] {
			t.Fatalf("重建后缺少索引 %s：%v", name, after.Indexes)
		}
	}
	if len(after.Indexes) != len(frozenAuditIndexes)+1 {
		t.Fatalf("重建后的索引数 = %d，期望 %d（四条契约索引 + 主键）：%v",
			len(after.Indexes), len(frozenAuditIndexes)+1, after.Indexes)
	}
}

// TestAuditSchemaRunsAsNonOwnerAgainstPostgres 复现部署时的角色分工，直接验证这次修复的目标场景：
// 表由 owner 建好，运行角色只有 USAGE/CREATE on schema audit 与 SELECT/INSERT on audit.audit_log
// （deploy/sql/roles-least-privilege.sql 的同款授权），**没有**表所有权。
// 非 owner 执行整段 DDL 必须成功且纯空转；同一角色若改用旧的 CREATE INDEX IF NOT EXISTS 会 42501
// （根因复现，只记录结论、不改结构）。
//
// 建角色需要 superuser/CREATEROLE：本机（127.0.0.1:15432 的 postgres）可跑；CI 上创建失败即跳过并写明原因。
func TestAuditSchemaRunsAsNonOwnerAgainstPostgres(t *testing.T) {
	ctx := context.Background()
	owner := testutil.Database(t)
	execAuditSchema(t, owner, ctx)
	before := snapshotAuditShape(t, owner, ctx)

	const role = "mf_audit_probe_test"
	// 幂等建角色（上一次运行若在清理前失败，角色可能还在）。
	if _, err := owner.ExecContext(ctx, `DO $$ BEGIN
		IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'mf_audit_probe_test') THEN
			CREATE ROLE mf_audit_probe_test LOGIN;
		END IF;
	END $$;`); err != nil {
		t.Skipf("建不了探针角色（需要 superuser/CREATEROLE）：%v", err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = owner.ExecContext(bg, "DROP INDEX IF EXISTS audit.audit_probe_should_fail_idx")
		_, _ = owner.ExecContext(bg, "REVOKE ALL ON SCHEMA audit FROM "+role)
		_, _ = owner.ExecContext(bg, "REVOKE ALL ON audit.audit_log FROM "+role)
		_, _ = owner.ExecContext(bg, "DROP ROLE IF EXISTS "+role)
	})
	// 部署同款授权：能看见 schema、能建对象（守卫的建表分支需要它），但拿不到表所有权。
	for _, stmt := range []string{
		"GRANT USAGE, CREATE ON SCHEMA audit TO " + role,
		"GRANT SELECT, INSERT ON audit.audit_log TO " + role,
	} {
		if _, err := owner.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("授权失败（%s）：%v", stmt, err)
		}
	}

	probeDSN, err := dsnAsUser(testutil.DSN(t), role)
	if err != nil {
		t.Fatalf("拼探针连接串: %v", err)
	}
	probe, err := sql.Open("postgres", probeDSN)
	if err != nil {
		t.Fatalf("打开探针连接: %v", err)
	}
	defer probe.Close()
	if err = probe.PingContext(ctx); err != nil {
		t.Skipf("探针角色连不上（pg_hba 不接受无口令登录）：%v", err)
	}
	// 确认它确实不是 owner（否则这条用例什么都没验证）。
	var isOwner bool
	if err = probe.QueryRowContext(ctx,
		"SELECT pg_get_userbyid(relowner) = current_user FROM pg_class WHERE oid = 'audit.audit_log'::regclass").Scan(&isOwner); err != nil {
		t.Fatalf("查表 owner: %v", err)
	}
	if isOwner {
		t.Fatalf("探针角色 %s 竟然是表 owner：这条用例的前提不成立", role)
	}

	// ① 非 owner 执行整段 DDL：必须成功且纯空转。
	if _, err = probe.ExecContext(ctx, Schema); err != nil {
		t.Fatalf("非 owner 角色执行审计 DDL 必须纯空转成功（守卫的全部意义），实际：%v", err)
	}
	if after := snapshotAuditShape(t, owner, ctx); !reflect.DeepEqual(before, after) {
		t.Fatalf("非 owner 执行后结构变了：\n前 %+v\n后 %+v", before, after)
	}
	// ② 根因复现（只用来说明"为什么要守卫"，不参与判定）：同一个角色执行旧的索引写法会先撞所有权检查。
	if _, idxErr := probe.ExecContext(ctx,
		"CREATE INDEX IF NOT EXISTS audit_probe_should_fail_idx ON audit.audit_log(id)"); idxErr != nil {
		t.Logf("根因复现：非 owner 的 CREATE INDEX IF NOT EXISTS 失败 → %v", idxErr)
	} else {
		t.Logf("注意：本实例上非 owner 的 CREATE INDEX IF NOT EXISTS 竟然成功——守卫的必要性需按此实例行为复评")
	}
}

// dsnAsUser 把连接串里的用户换成另一个角色（库名与其它参数不变，testutil 的 _test 约束仍然成立）。
func dsnAsUser(dsn, user string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.User = url.User(user)
	return u.String(), nil
}
