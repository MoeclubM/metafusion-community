package store

import (
	"context"
	"testing"

	"github.com/MoeclubM/metafusion-community/internal/testutil"
)

// 需要真实 PostgreSQL 的回归：schema 建表 + 话题/回复约束 + 收藏往返。
// 未设置 COMMUNITY_TEST_DSN 时整体跳过；切流前用它在真实库上跑一次。
func TestSchemaTopicsAndFavoritesAgainstPostgres(t *testing.T) {
	dsn := testutil.DSN(t)
	db := testutil.Database(t)
	ctx := context.Background()

	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	if err = s.Init(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	const topicID = "11111111-1111-1111-1111-111111111111"
	const authorID = "22222222-2222-2222-2222-222222222222"
	// 板块、话题与首条回复用一条语句建出：本仓库的用例共用一个测试库、包与包之间并行执行，
	// 而 handler 包的用例会在准备阶段清空这些表——分成多条语句时，"话题刚建好就被删掉"
	// 会变成偶发的外键失败（Flaky）。ON CONFLICT 让这条语句可重复执行。
	// 板块定义在 handler 包（seedForum），这里按同一 SQL 建一块用于话题测试。
	if _, err = db.ExecContext(ctx, `
		WITH b AS (
			INSERT INTO community.boards(code,name) VALUES('qa','问答')
			ON CONFLICT (code) DO UPDATE SET code=EXCLUDED.code
			RETURNING code
		), t AS (
			INSERT INTO community.topics(id,board_code,author_id,title,body)
			SELECT $1, b.code, $2, 't', 'b' FROM b
			ON CONFLICT (id) DO UPDATE SET updated_at=now()
			RETURNING id
		)
		INSERT INTO community.posts(id,topic_id,author_id,body,post_number)
		SELECT '33333333-3333-3333-3333-333333333333', t.id, $2, 'p1', 1 FROM t`, topicID, authorID); err != nil {
		t.Fatalf("insert topic and first post: %v", err)
	}
	if _, err = db.ExecContext(ctx, "INSERT INTO community.posts(id,topic_id,author_id,body,post_number) VALUES('44444444-4444-4444-4444-444444444444',$1,$2,'p2',1)", topicID, authorID); err == nil {
		t.Fatal("同一话题内重复楼层号必须被唯一约束拒绝")
	}

	// 收藏：切换 → 状态 → 列表 → 取消 → 再收藏 → 清理。
	const target = "55555555-5555-5555-5555-555555555555"
	if _, err = db.ExecContext(ctx, "DELETE FROM community.favorites WHERE user_id=$1", authorID); err != nil {
		t.Fatalf("cleanup favorites: %v", err)
	}
	on, err := s.ToggleFavorite(ctx, authorID, "work", target)
	if err != nil || !on {
		t.Fatalf("首次切换应为已收藏: on=%v err=%v", on, err)
	}
	if on, err = s.ToggleFavorite(ctx, authorID, "work", target); err != nil || on {
		t.Fatalf("二次切换应取消收藏: on=%v err=%v", on, err)
	}
	if _, err = s.ToggleFavorite(ctx, authorID, "work", target); err != nil {
		t.Fatalf("再次收藏: %v", err)
	}
	ids, err := s.FavoriteStatus(ctx, authorID, "work", []string{target, "66666666-6666-6666-6666-666666666666"})
	if err != nil || len(ids) != 1 || ids[0] != target {
		t.Fatalf("收藏状态 = %v, err=%v", ids, err)
	}
	items, total, err := s.ListFavorites(ctx, authorID, "", 20, 0)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("收藏列表 total=%d items=%d err=%v", total, len(items), err)
	}
	if items[0].ID != "work:"+target {
		t.Fatalf("合成 id 不符: %s", items[0].ID)
	}
	if _, err = s.ToggleFavorite(ctx, authorID, "work", target); err != nil {
		t.Fatalf("清理收藏: %v", err)
	}
	if _, err = s.ToggleFavorite(ctx, authorID, "canonical_entry", target); err == nil {
		t.Fatal("已退役的目标类型必须被拒绝")
	}

	// 级联：删话题应带走回复。
	if _, err = db.ExecContext(ctx, "DELETE FROM community.topics WHERE id=$1", topicID); err != nil {
		t.Fatalf("delete topic: %v", err)
	}
	var left int
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM community.posts WHERE topic_id=$1", topicID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("回复未被级联删除: left=%d err=%v", left, err)
	}
}
