package store

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// DDL 里的目标类型集合必须与代码里的 FavoriteKinds 完全一致。
// 两边漂移会导致"代码允许、数据库拒绝"（或反向）——这类错编译期看不出来，
// 只在第一次真实写入时才暴露，因此用一条静态用例把它钉住。
func TestSchemaFavoriteKindsMatchCode(t *testing.T) {
	re := regexp.MustCompile(`target_type text NOT NULL CHECK \(target_type IN \(([^)]*)\)\)`)
	match := re.FindStringSubmatch(schema)
	if match == nil {
		t.Fatal("storage schema 中找不到 favorites.target_type 的 CHECK 约束")
	}
	inSchema := map[string]bool{}
	for _, raw := range strings.Split(match[1], ",") {
		inSchema[strings.Trim(strings.TrimSpace(raw), "'")] = true
	}
	if len(inSchema) == 0 {
		t.Fatal("CHECK 约束里没有解析到任何类型")
	}
	missingInCode := []string{}
	for kind := range inSchema {
		if _, ok := FavoriteKinds[kind]; !ok {
			missingInCode = append(missingInCode, kind)
		}
	}
	missingInSchema := []string{}
	for kind := range FavoriteKinds {
		if !inSchema[kind] {
			missingInSchema = append(missingInSchema, kind)
		}
	}
	sort.Strings(missingInCode)
	sort.Strings(missingInSchema)
	if len(missingInCode) > 0 || len(missingInSchema) > 0 {
		t.Fatalf("收藏目标类型不一致：库里有而代码没有 %v；代码有而库里没有 %v", missingInCode, missingInSchema)
	}
}
