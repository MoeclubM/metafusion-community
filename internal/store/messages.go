package store

import (
	"context"
	"time"
)

// DirectMessage 是一条私信。字段名与前端契约逐字一致（id/sender_id/recipient_id/body/created_at）；
// read_at（已读回执）不在对外形状里，列已预留但接口不读不写。
type DirectMessage struct {
	ID          string    `json:"id"`
	SenderID    string    `json:"sender_id"`
	RecipientID string    `json:"recipient_id"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
}

// SendMessage 落一条私信并回读落库后的行（created_at 由数据库 now() 决定，不能由调用方猜）。
// id 由调用方生成：编排里的 PostgreSQL 是 16，没有 uuidv7()（见 000005 迁移的说明）。
// "不能给自己发"由 direct_messages 的 CHECK 兜底，HTTP 层在此之前就会拒掉。
func (s *Store) SendMessage(ctx context.Context, id, senderID, recipientID, body string) (DirectMessage, error) {
	var m DirectMessage
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO community.direct_messages(id,sender_id,recipient_id,body)
		VALUES($1,$2,$3,$4)
		RETURNING id::text,sender_id::text,recipient_id::text,body,created_at`,
		id, senderID, recipientID, body).
		Scan(&m.ID, &m.SenderID, &m.RecipientID, &m.Body, &m.CreatedAt)
	return m, err
}

// ListConversation 取 userID 与 peerID 两人的会话（按时间倒序，最新在前）与总数。
//
// 可见性是**结构性**的：过滤条件只有 (当前用户, 对方) 这一对参与者，别人的私信
// 无论怎么构造请求都进不了结果集——没有"再检查一下这条消息是不是我的"这种可漏的分支。
// WHERE 里的 LEAST/GREATEST 与 direct_messages_conversation 索引表达式逐字一致
// （方向不同也要能命中），因此这是"走索引的倒序扫描"而不是"读全表再排序"。
// 已知取舍：这条索引服务的是"两个已知参与者之间的会话"。将来若要加"我的会话列表"
// （按 sender_id 或 recipient_id 取一侧，是 OR 条件），它帮不上忙，需要另建索引——
// 现在没有那个端点，就先不为想象中的查询付写放大的代价。
func (s *Store) ListConversation(ctx context.Context, userID, peerID string, limit, offset int) ([]DirectMessage, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM community.direct_messages
		WHERE LEAST(sender_id,recipient_id)=LEAST($1::uuid,$2::uuid)
		  AND GREATEST(sender_id,recipient_id)=GREATEST($1::uuid,$2::uuid)`,
		userID, peerID).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT id::text,sender_id::text,recipient_id::text,body,created_at
		FROM community.direct_messages
		WHERE LEAST(sender_id,recipient_id)=LEAST($1::uuid,$2::uuid)
		  AND GREATEST(sender_id,recipient_id)=GREATEST($1::uuid,$2::uuid)
		ORDER BY created_at DESC, id DESC
		LIMIT $3 OFFSET $4`,
		userID, peerID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []DirectMessage{}
	for rows.Next() {
		var m DirectMessage
		if err = rows.Scan(&m.ID, &m.SenderID, &m.RecipientID, &m.Body, &m.CreatedAt); err != nil {
			return nil, 0, err
		}
		items = append(items, m)
	}
	return items, total, rows.Err()
}
