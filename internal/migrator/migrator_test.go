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
