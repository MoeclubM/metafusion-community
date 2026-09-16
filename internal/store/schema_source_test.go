package store

import (
	"io/fs"
	"testing"

	"github.com/MoeclubM/metafusion-community/migrations"
)

// schemaDDL 返回建表语句的唯一来源：迁移文件本身（Go 里不再有内联副本）。
// 文件缺失/读不到必须让用例失败——"测试拿不到结构"和"结构漂移"一样危险。
func schemaDDL(t *testing.T) string {
	t.Helper()
	raw, err := fs.ReadFile(migrations.FS, "000001_init.up.sql")
	if err != nil {
		t.Fatalf("读取 migrations/000001_init.up.sql: %v", err)
	}
	return string(raw)
}
