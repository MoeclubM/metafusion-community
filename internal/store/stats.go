package store

import "context"

// UserStats 是用户主页的三个互动数字。这份结构就是**对外口径**：前端直接展示，
// 因此"数哪张表、按什么过滤"必须写清楚（见 StatsFor），不接受含糊的"大概就是活跃度"。
type UserStats struct {
	TopicsCreated   int `json:"topics_created"`
	CommentsCreated int `json:"comments_created"`
	FavoritesCount  int `json:"favorites_count"`
}

// StatsFor 用一条 SQL 取回三个数字（三个标量子查询）：三次往返会把一个"给人看的统计"
// 变成三个可能互相不一致的读数，也没有必要。
//
// 口径（每一项都与对应的列表接口同源，不制造第二套数字）：
//
//   - topics_created：community.topics 里 author_id 本人、且**非评论板块**（board_code <> 评论板块）的行数。
//     评论板块的行是"实体短评"，不是主题——主题列表同样把评论排除在外（见 handler 的 commentBoard）。
//   - comments_created：community.posts 里 author_id 本人的行数，即**楼中回复**（前端标签是"互动回复"）。
//     主题正文不算回复（它不在 posts 表里，已计入 topics_created）；实体短评两边都不重复计——
//     宁可少算，也不让同一行在两个数字里各出现一次。
//   - favorites_count：community.favorites 里 user_id 本人的行数。**公开性口径与
//     GET /api/users/{id}/favorites 返回的 total 完全一致**：community 侧没有"收藏公开标记"，
//     也没有用户设置表（"收藏是否公开"目前只是前端只读占位，README「职责边界」有记），
//     因此"公开可见的收藏"就是全部收藏行；目标实体自身的可见性由读取方逐条过滤
//     （列表跳过不可见目标，但同样不影响 total），本服务也不去读 catalog 的库。
//
// commentBoard 由调用方传入（handler 的 commentBoard 常量）：板块码只有一份来源，
// 将来改名不会出现"接口认、统计不认"的两套过滤条件。
func (s *Store) StatsFor(ctx context.Context, userID, commentBoard string) (UserStats, error) {
	var out UserStats
	err := s.db.QueryRowContext(ctx, `
		SELECT
		  (SELECT count(*) FROM community.topics WHERE author_id=$1::uuid AND board_code <> $2),
		  (SELECT count(*) FROM community.posts WHERE author_id=$1::uuid),
		  (SELECT count(*) FROM community.favorites WHERE user_id=$1::uuid)`,
		userID, commentBoard).Scan(&out.TopicsCreated, &out.CommentsCreated, &out.FavoritesCount)
	return out, err
}
