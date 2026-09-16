package migrator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"strings"
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

// 000004 的语义：把 000002/000003 删掉的列恢复成"字段保留、接口去语言"的终态。
//
// 用例从**只应用过 000001 的实例状态**起步：把 000002 引入、000004 恢复的多语言列与
// topics.language 先删掉（IF EXISTS，对已经是终态的库是空转），再执行 000004 的内容，
// 断言三列回来了、默认值正确、重复执行幂等，最后 ROLLBACK——不给同库其它包留下改过的结构。
//
// DDL 在 PostgreSQL 里是事务性的，所以这个做法安全；加列会取表锁，因此整套用例按 README 的
// 口径用 -p 1 串行跑，不要并行跑包。
func TestRestoreMultilingualFieldsMigrationRestoresColumns(t *testing.T) {
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

	// 1) 造 000001 的基线结构：多语言列（000002 删了）与 topics.language（000003 删了）都不在。
	for _, stmt := range []string{
		"ALTER TABLE community.boards DROP COLUMN IF EXISTS names",
		"ALTER TABLE community.boards DROP COLUMN IF EXISTS descriptions",
		"ALTER TABLE community.topics DROP COLUMN IF EXISTS language",
	} {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("造 000001 基线结构（%s）: %v", stmt, err)
		}
	}

	// 2) 执行 000004 的内容（刻意不走 Apply：那一版已在账本里，Apply 会跳过）。
	raw, err := fs.ReadFile(migrations.FS, "000004_restore_multilingual_fields.up.sql")
	if err != nil {
		t.Fatalf("读取 000004: %v", err)
	}
	if _, err = tx.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("执行 000004: %v", err)
	}

	// 3) 断言列回来了，默认值与已应用 000004 的实例逐列一致。
	for _, want := range []struct{ table, column, def string }{
		{"boards", "names", "'{}'::jsonb"},
		{"boards", "descriptions", "'{}'::jsonb"},
		{"topics", "language", "''::text"},
	} {
		var exists bool
		if err = tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='community' AND table_name=$1 AND column_name=$2)",
			want.table, want.column).Scan(&exists); err != nil {
			t.Fatalf("查 community.%s.%s: %v", want.table, want.column, err)
		}
		if !exists {
			t.Fatalf("000004 之后 community.%s.%s 不存在", want.table, want.column)
		}
		if got := columnDefault(t, tx, want.table, want.column); got != want.def {
			t.Fatalf("community.%s.%s 默认值 = %q，期望 %q", want.table, want.column, got, want.def)
		}
	}

	// 4) 幂等：同一份内容再跑一次不能报错。
	if _, err = tx.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("重复执行 000004 必须幂等: %v", err)
	}
}

// 000004 的回填语义：把 000002 收敛出来的单值列反向补进多语言 map，且不覆盖已有语种键。
//
// 从**应用过 000002/000003 的实例状态**起步（多语言列与 topics.language 都不在），
// 先造一条"只丢了中文键"的行（多语言列由 000001 留下、000002 没管它），再执行 000004：
// 断言 zh-CN 由单值列补回、原有 en-US 仍在、language 列回来但历史值不恢复、重复执行不改数据。
func TestRestoreMultilingualFieldsBackfillsFromSingleValues(t *testing.T) {
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

	const probeBoard = "restore-probe"
	const probeTopic = "00000000-0000-0000-0000-0000000000cc"
	var taken bool
	if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM community.boards WHERE code=$1)", probeBoard).Scan(&taken); err != nil {
		t.Fatalf("查探针板块: %v", err)
	}
	if taken {
		t.Fatalf("探针板块 %s 已存在：上一次运行的清理没有落地", probeBoard)
	}

	// 造 000002/000003 之后的结构：多语言列与 topics.language 都不在。
	for _, stmt := range []string{
		"ALTER TABLE community.boards DROP COLUMN IF EXISTS names",
		"ALTER TABLE community.boards DROP COLUMN IF EXISTS descriptions",
		"ALTER TABLE community.topics DROP COLUMN IF EXISTS language",
	} {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("造 000002/000003 结构（%s）: %v", stmt, err)
		}
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO community.boards(code,name,description,color,icon,sort_order)"+
		" VALUES($1,'闲谈','闲聊描述','slate','Hash',997)", probeBoard); err != nil {
		t.Fatalf("插入单值板块: %v", err)
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO community.topics(id,board_code,author_id,author_name,title,body)"+
		" VALUES($1,$2,'00000000-0000-0000-0000-0000000000dd','tester','探针主题','正文')", probeTopic, probeBoard); err != nil {
		t.Fatalf("插入探针主题: %v", err)
	}

	raw, err := fs.ReadFile(migrations.FS, "000004_restore_multilingual_fields.up.sql")
	if err != nil {
		t.Fatalf("读取 000004: %v", err)
	}
	// 多语言列这时还不存在：第一遍只能在"列刚建出来"之后写值，因此先把 en-US 的存在性留给
	// 第二遍——第一遍断言单值列补进了 zh-CN、description 同理。
	if _, err = tx.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("执行 000004: %v", err)
	}
	var names, descriptions string
	if err = tx.QueryRowContext(ctx, "SELECT names::text, descriptions::text FROM community.boards WHERE code=$1", probeBoard).Scan(&names, &descriptions); err != nil {
		t.Fatalf("回读多语言列: %v", err)
	}
	if !strings.Contains(names, `"zh-CN"`) || !strings.Contains(names, "闲谈") {
		t.Fatalf("names 未从单值列补回 zh-CN：%s", names)
	}
	if !strings.Contains(descriptions, `"zh-CN"`) || !strings.Contains(descriptions, "闲聊描述") {
		t.Fatalf("descriptions 未从单值列补回 zh-CN：%s", descriptions)
	}
	// 模拟"000001 时代只丢了中文键"的行：names 里有 en-US、没有 zh-CN，再跑一遍 000004。
	if _, err = tx.ExecContext(ctx, "UPDATE community.boards SET names = names - 'zh-CN' || $2::jsonb WHERE code=$1",
		probeBoard, `{"en-US":"Casual"}`); err != nil {
		t.Fatalf("造缺中文键的行: %v", err)
	}
	if _, err = tx.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("第二遍执行 000004: %v", err)
	}
	var refilled string
	if err = tx.QueryRowContext(ctx, "SELECT names::text FROM community.boards WHERE code=$1", probeBoard).Scan(&refilled); err != nil {
		t.Fatalf("第二遍回读: %v", err)
	}
	for _, want := range []string{`"zh-CN"`, "闲谈", `"en-US"`, "Casual"} {
		if !strings.Contains(refilled, want) {
			t.Fatalf("回填应只补 zh-CN、不覆盖已有语种，缺少 %s：%s", want, refilled)
		}
	}

	// language 列回来，但历史值不恢复（000003 已经丢了，且该列不参与任何读取）。
	var language string
	if err = tx.QueryRowContext(ctx, "SELECT language FROM community.topics WHERE id=$1", probeTopic).Scan(&language); err != nil {
		t.Fatalf("回读 topics.language: %v", err)
	}
	if language != "" {
		t.Fatalf("language = %q，期望空串（历史值已丢，无法恢复）", language)
	}

	// 幂等：重复执行不能报警、也不能再改动已补好的值。
	var before string
	if err = tx.QueryRowContext(ctx, "SELECT names::text FROM community.boards WHERE code=$1", probeBoard).Scan(&before); err != nil {
		t.Fatalf("回读: %v", err)
	}
	if _, err = tx.ExecContext(ctx, string(raw)); err != nil {
		t.Fatalf("重复执行 000004 必须幂等: %v", err)
	}
	var after string
	if err = tx.QueryRowContext(ctx, "SELECT names::text FROM community.boards WHERE code=$1", probeBoard).Scan(&after); err != nil {
		t.Fatalf("重复执行后回读: %v", err)
	}
	if before != after {
		t.Fatalf("重复执行改动了多语言列：\n  前 %s\n  后 %s", before, after)
	}
}

// columnDefault 读 information_schema 里的列默认值，用来断言 000004 加回来的列形状正确。
func columnDefault(t *testing.T, tx *sql.Tx, table, column string) string {
	t.Helper()
	var def sql.NullString
	if err := tx.QueryRow(
		"SELECT column_default FROM information_schema.columns WHERE table_schema='community' AND table_name=$1 AND column_name=$2",
		table, column).Scan(&def); err != nil {
		t.Fatalf("查 community.%s.%s 列定义: %v", table, column, err)
	}
	return def.String
}
