package store

import (
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/MoeclubM/metafusion-community/migrations"
)

// schemaDDL 返回建表语句的唯一来源：**全部迁移文件按版本拼接**（Go 里没有内联副本）。
//
// 000001 是基线，之后的版本都是增量迁移（000002 板块名单值化、000003 主题去 language、
// 000004 前向恢复多语言列），因此只读 000001 会让"结构测试看到的库"与"实际建出来的库"不是同一个东西。
// 文件缺失/读不到必须让用例失败——"测试拿不到结构"和"结构漂移"一样危险。
func schemaDDL(t *testing.T) string {
	t.Helper()
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("读取 migrations 目录: %v", err)
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("migrations 目录里没有 .up.sql 文件")
	}
	sort.Strings(names) // 文件名前缀即版本号，字典序等于版本序
	out := make([]string, 0, len(names))
	for _, name := range names {
		raw, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("读取 migrations/%s: %v", name, err)
		}
		out = append(out, string(raw))
	}
	return strings.Join(out, "\n")
}
