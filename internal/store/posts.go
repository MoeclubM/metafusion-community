package store

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

// ModerationPost 是帖子治理列表的一行。治理需要的上下文（属于哪个主题、哪个板块、谁写的、
// 第几楼）都在这里一次带走：调用方不必再回查主题，也不会因为"先列回复再逐条问主题"而放大往返。
//
// 与 community.topics 表里"评论板块的短评"不是同一类内容：那些行由 /community/feed 承载，
// 本结构只描述 community.posts（楼中回复），两者不互相替代。
type ModerationPost struct {
	ID         string
	TopicID    string
	TopicTitle string
	BoardCode  string
	AuthorID   string
	AuthorName string
	Body       string
	PostNumber int
	// 可空的引用楼号：NULL 就是"没有引用"，用 sql.NullInt64 与 forum.go 的楼层读取同口径。
	ReplyToPostNumber sql.NullInt64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// ListModerationPosts 跨主题列社区回复，按 (created_at DESC, id DESC) 排序——最新的在前。
//
// 关键词口径与主题列表（handler/forum.go 的 /community/topics）保持一致：ILIKE 子串匹配
// 主题标题或回复正文，**不引入 to_tsvector**——默认分词配置对中文按词切分的假设不成立
// （中文没有空格边界），全文索引在这里只会把"搜不到"变成"看起来支持却搜不到"；
// 子串匹配与既有列表同口径，也支持中文直接搜索。
// 与 topics 列表一样，q 里的 % / _ 会被当通配符（ILIKE 的原义），这是本服务既有的搜索语义。
//
// id 参与排序的第二关键字：created_at 只到微秒但同一批写入的行没有稳定顺序时，
// 分页窗口之间会串行（同一行出现两次或被跳过）。
func (s *Store) ListModerationPosts(ctx context.Context, search string, limit, offset int) ([]ModerationPost, int, error) {
	selects := "SELECT p.id::text, p.topic_id::text, t.title, t.board_code, " +
		"p.author_id::text, COALESCE(NULLIF(p.author_name,''),'Anonymous'), " +
		"p.body, p.post_number, p.reply_to_post_number, p.created_at, p.updated_at " +
		"FROM community.posts p JOIN community.topics t ON t.id = p.topic_id"
	counts := "SELECT count(*) FROM community.posts p JOIN community.topics t ON t.id = p.topic_id"

	args := []any{}
	where := ""
	if search != "" {
		// 两个字段用同一个参数：位置占位符的编号由参数个数决定，写成同一个 $1 不必再拼第二份。
		args = append(args, "%"+search+"%")
		where = " WHERE (t.title ILIKE $1 OR p.body ILIKE $1)"
	}

	out := []ModerationPost{}
	var total int
	if err := s.db.QueryRowContext(ctx, counts+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.QueryContext(ctx, selects+where+
		" ORDER BY p.created_at DESC, p.id DESC LIMIT $"+strconv.Itoa(len(pageArgs)-1)+
		" OFFSET $"+strconv.Itoa(len(pageArgs)), pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ModerationPost
		if err := rows.Scan(&item.ID, &item.TopicID, &item.TopicTitle, &item.BoardCode,
			&item.AuthorID, &item.AuthorName, &item.Body, &item.PostNumber,
			&item.ReplyToPostNumber, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, item)
	}
	return out, total, rows.Err()
}
