package migrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"testing"

	"github.com/MoeclubM/metafusion-community/internal/testutil"
	"github.com/MoeclubM/metafusion-community/migrations"
)

// 迁移文件必须能被解析出版本号：文件名是版本来源，写错前缀会让迁移被静默跳过。
func TestLoadParsesVersionedMigration(t *testing.T) {
	files, err := load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("没有解析到任何迁移文件")
	}
	if files[0].version != 1 {
		t.Fatalf("首个迁移版本 = %d，期望 1", files[0].version)
	}
	for i := 1; i < len(files); i++ {
		if files[i-1].version >= files[i].version {
			t.Fatalf("迁移未按版本升序：%d 后是 %d", files[i-1].version, files[i].version)
		}
	}
	raw, err := fs.ReadFile(migrations.FS, files[0].filename)
	if err != nil {
		t.Fatalf("读回迁移文件: %v", err)
	}
	sum := sha256.Sum256(raw)
	if want := hex.EncodeToString(sum[:]); files[0].checksum != want {
		t.Fatalf("checksum = %s，期望 %s", files[0].checksum, want)
	}
}

// 真库回归：Apply 必须在库上建立社区表、登记版本，并且重复调用是幂等的
// （不重复登记、不报错、不改已存在的表）。
//
// 这条用例是**非破坏性**的：只断言"结构与登记存在且唯一"，不 DROP 任何对象——
// 同一个测试库里还有 handler/store 的用例在并行跑。
func TestApplyIsIdempotentAgainstPostgres(t *testing.T) {
	db := testutil.Database(t)
	ctx := context.Background()

	if err := Apply(ctx, db); err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("重复 Apply 必须幂等: %v", err)
	}

	var rows int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM community.schema_migrations WHERE version=$1", 1).Scan(&rows); err != nil {
		t.Fatalf("读迁移登记表: %v", err)
	}
	if rows != 1 {
		t.Fatalf("版本 1 的登记行数 = %d，期望 1", rows)
	}

	files, err := load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	var name, checksum string
	if err = db.QueryRowContext(ctx,
		"SELECT name, checksum FROM community.schema_migrations WHERE version=$1", 1).Scan(&name, &checksum); err != nil {
		t.Fatalf("读登记行: %v", err)
	}
	if name != files[0].name || checksum != files[0].checksum {
		t.Fatalf("登记内容与迁移文件不一致：name=%s checksum=%s（文件 %s / %s）",
			name, checksum, files[0].name, files[0].checksum)
	}

	for _, table := range []string{"boards", "topics", "posts", "tags", "topic_tags", "favorites", "records"} {
		var exists bool
		if err = db.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='community' AND table_name=$1)",
			table).Scan(&exists); err != nil {
			t.Fatalf("查表 %s: %v", table, err)
		}
		if !exists {
			t.Fatalf("迁移后 community.%s 不存在", table)
		}
	}
}

// 000002 的回填语义：把 jsonb 四语 map 收敛成单语言列。
//
// 全程在一个**回滚事务**里做：先把 boards 表改回旧结构（jsonb 列 + 无单字段列），插入一条双语板块，
// 再执行 000002 的内容，断言回填优先级与旧列消失，最后 ROLLBACK——同一个测试库里还有其它包的用例，
// 不能留下被改过的结构。DDL 在 PostgreSQL 里是事务性的，所以这个做法是安全的。
func TestBoardSingleLanguageMigrationBackfillsAndDropsLegacyColumns(t *testing.T) {
	db := testutil.Database(t)
	ctx := context.Background()
	if err := Apply(ctx, db); err != nil {
		t.Fatalf("先应用全部迁移: %v", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务: %v", err)
	}
	defer tx.Rollback()

	// 1) 造旧结构：加回 jsonb 列、去掉单语言列（模拟"只跑过 000001"的实例）。
	for _, stmt := range []string{
		"ALTER TABLE community.boards ADD COLUMN IF NOT EXISTS names jsonb NOT NULL DEFAULT '{}'::jsonb",
		"ALTER TABLE community.boards ADD COLUMN IF NOT EXISTS descriptions jsonb NOT NULL DEFAULT '{}'::jsonb",
		"ALTER TABLE community.boards DROP COLUMN IF EXISTS name, DROP COLUMN IF EXISTS description",
	} {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("造旧结构（%s）: %v", stmt, err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO community.boards(code,names,descriptions,color,icon,sort_order)
		VALUES('legacy',$1::jsonb,$2::jsonb,'slate','Hash',999)
		ON CONFLICT (code) DO UPDATE SET names=EXCLUDED.names, descriptions=EXCLUDED.descriptions`,
		`{"zh-CN":" 闲聊 ","zh-TW":"閒聊","en-US":"Casual"}`, `{"en-US":"Casual talk","zh-CN":"中文描述"}`); err != nil {
		t.Fatalf("插入旧行: %v", err)
	}

	// 2) 跑 000002 的内容（刻意不走 Apply：那一版已经在账本里，Apply 会跳过）。
	raw, err := fs.ReadFile(migrations.FS, "000002_board_single_language.up.sql")
	if err != nil {
		t.Fatalf("读取 000002: %v", err)
	}
	if _, err = tx.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("执行 000002: %v", err)
	}

	// 3) 断言：name 取 zh-CN（并裁掉两侧空白）、description 同理。
	var name, description string
	if err = tx.QueryRowContext(ctx, "SELECT name, description FROM community.boards WHERE code='legacy'").Scan(&name, &description); err != nil {
		t.Fatalf("回读回填结果: %v", err)
	}
	if name != "闲聊" {
		t.Fatalf("name = %q，期望取 zh-CN 的 %q", name, "闲聊")
	}
	if description != "中文描述" {
		t.Fatalf("description = %q，期望取 zh-CN 的 %q", description, "中文描述")
	}

	// 4) 旧列必须消失。
	for _, col := range []string{"names", "descriptions"} {
		var exists bool
		if err = tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='community' AND table_name='boards' AND column_name=$1)",
			col).Scan(&exists); err != nil {
			t.Fatalf("查列 %s: %v", col, err)
		}
		if exists {
			t.Fatalf("迁移后 community.boards.%s 仍存在", col)
		}
	}

	// 5) 幂等：同一份内容再跑一次不能报错，也不能改动已回填的值。
	if _, err = tx.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("重复执行 000002 必须幂等: %v", err)
	}
	if err = tx.QueryRowContext(ctx, "SELECT name, description FROM community.boards WHERE code='legacy'").Scan(&name, &description); err != nil {
		t.Fatalf("重复执行后回读: %v", err)
	}
	if name != "闲聊" || description != "中文描述" {
		t.Fatalf("重复执行改动了数据: name=%q description=%q", name, description)
	}
}
