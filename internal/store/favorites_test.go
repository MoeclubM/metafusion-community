package store

import (
	"errors"
	"testing"
)

// 收藏目标类型就是固定八种实体骨架：多一个少一个都会让收藏指向不存在的维度，
// 因此这里逐个锁定，而不是只测"某个字符串能用"。
func TestFavoriteKindsCoverEntitySkeleton(t *testing.T) {
	want := []string{"agent", "collection", "work", "content_unit", "expression", "release", "medium", "track"}
	if len(FavoriteKinds) != len(want) {
		t.Fatalf("收藏目标类型数量 = %d, 期望 %d", len(FavoriteKinds), len(want))
	}
	for _, k := range want {
		kind, err := KindFor(k)
		if err != nil || kind != k {
			t.Fatalf("KindFor(%q) = %q, %v", k, kind, err)
		}
	}
}

// 已退役的历史类型与空值必须拒绝，避免收藏指向不再存在的维度。
func TestKindForRejectsUnknownAndEmpty(t *testing.T) {
	for _, bad := range []string{"", "canonical_entry", "artist", "WORK"} {
		// 必须是可判定的哨兵错误：HTTP 层靠它把"输入错"（400）与"库故障"（500）分开。
		if _, err := KindFor(bad); !errors.Is(err, ErrInvalidTargetType) {
			t.Fatalf("KindFor(%q) 本应返回 ErrInvalidTargetType，实际 %v", bad, err)
		}
	}
	if kind, err := KindFor("  work  "); err != nil || kind != "work" {
		t.Fatalf("两端空白应被容忍: %q, %v", kind, err)
	}
}
