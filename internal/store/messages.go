package store

import (
	"context"
	"time"
)

// DirectMessage 是一条私信。字段名与前端契约逐字一致（id/sender_id/recipient_id/body/created_at）；
// read_at（已读回执）不在单条消息的对外形状里：回执是**收到侧**的状态，本批次只在会话列表上
// 以 unread_count 表达，读单条消息不会顺带读出"对方读没读"（发送方看不到回执）。
type DirectMessage struct {
	ID          string    `json:"id"`
	SenderID    string    `json:"sender_id"`
	RecipientID string    `json:"recipient_id"`
	Body        string    `json:"body"`
	CreatedAt   time.Time `json:"created_at"`
}

// Conversation 是收件箱里的一行：对方 id + 最近一条消息 + 我未读的条数。
//
// 不含对方用户名：账号资料归账号服务，本服务不查它的库（与 000005 的归属边界一致）。
// 让每一行都做一次出站调用去补用户名，等于把收件箱变成"账号服务可用才可用"，
// 且是 N 次出站；调用方拿 peer_id 自己去 /api/users/{id} 取资料更便宜（前端按页并发取）。
type Conversation struct {
	PeerID      string        `json:"peer_id"`
	LastMessage DirectMessage `json:"last_message"`
	UnreadCount int           `json:"unread_count"`
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

// ListConversations 取收件箱一页：我参与的每一段会话的对方、最近一条消息与我的未读数。
//
// 形状（**一条语句，不是每条会话一次查询**）：
//
//	recent  —— 两个分支各取"我这一侧按对方分组的最新行"：
//	            收到侧 DISTINCT ON (sender_id)   WHERE recipient_id = $1
//	                     ORDER BY sender_id, created_at DESC, id DESC   → direct_messages_inbox
//	            发出侧 DISTINCT ON (recipient_id) WHERE sender_id = $1
//	                     ORDER BY recipient_id, created_at DESC, id DESC → direct_messages_outbox
//	           DISTINCT ON 的排序键与索引列顺序逐字对齐，两个分支都是索引倒序扫描，
//	           没有"读全表再排序"；UNION ALL 之后每个对方至多剩两行（我发的/他发的各一）。
//	latest  —— 把这两行并成一段会话的最近一条（每个 peer 一行）。
//	unread  —— 按 sender_id 分组数"我还没读的"，一次 GROUP BY 出全部会话的未读数。
//
// 为什么不用窗口函数 row_number() 一把梭：那要把两个分支的结果全部物化再排序，
// 而这里每个分支的排序都由索引直接给出，成本与"我参与的会话数"同阶。
//
// total 单独一条查询：`count(*) OVER ()` 在"页码越界、这一页为空"时拿不到总数
// （窗口函数没有行就没有输出），而调用方要靠 total 判断"还有没有下一页"。
func (s *Store) ListConversations(ctx context.Context, userID string, limit, offset int) ([]Conversation, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH recent AS (
		  (SELECT DISTINCT ON (sender_id) sender_id AS peer, id, sender_id, recipient_id, body, created_at
		     FROM community.direct_messages
		    WHERE recipient_id = $1::uuid
		    ORDER BY sender_id, created_at DESC, id DESC)
		  UNION ALL
		  (SELECT DISTINCT ON (recipient_id) recipient_id AS peer, id, sender_id, recipient_id, body, created_at
		     FROM community.direct_messages
		    WHERE sender_id = $1::uuid
		    ORDER BY recipient_id, created_at DESC, id DESC)
		), latest AS (
		  SELECT DISTINCT ON (peer) peer, id, sender_id, recipient_id, body, created_at
		    FROM recent
		   ORDER BY peer, created_at DESC, id DESC
		), unread AS (
		  SELECT sender_id AS peer, count(*) AS n
		    FROM community.direct_messages
		   WHERE recipient_id = $1::uuid AND read_at IS NULL
		   GROUP BY sender_id
		)
		SELECT l.peer::text, l.id::text, l.sender_id::text, l.recipient_id::text, l.body, l.created_at,
		       COALESCE(u.n, 0)
		  FROM latest l LEFT JOIN unread u ON u.peer = l.peer
		 ORDER BY l.created_at DESC, l.id DESC
		 LIMIT $2 OFFSET $3`,
		userID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Conversation{}
	for rows.Next() {
		var c Conversation
		if err = rows.Scan(&c.PeerID, &c.LastMessage.ID, &c.LastMessage.SenderID, &c.LastMessage.RecipientID,
			&c.LastMessage.Body, &c.LastMessage.CreatedAt, &c.UnreadCount); err != nil {
			return nil, 0, err
		}
		items = append(items, c)
	}
	if err = rows.Err(); err != nil {
		return nil, 0, err
	}

	var total int
	if err = s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM (
		  SELECT sender_id AS peer FROM community.direct_messages WHERE recipient_id = $1::uuid
		  UNION
		  SELECT recipient_id AS peer FROM community.direct_messages WHERE sender_id = $1::uuid
		) peers`, userID).Scan(&total); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

// UnreadCount 是我全部未读的私信条数（导航栏角标用）：一条走 direct_messages_inbox 的计数。
// 收件箱列表里的 unread_count 是**按对方**的同一个口径，两处不会互相矛盾。
func (s *Store) UnreadCount(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM community.direct_messages
		WHERE recipient_id = $1::uuid AND read_at IS NULL`, userID).Scan(&n)
	return n, err
}

// MarkConversationRead 把"peerID 发给我、我还没读的"全部置为已读，返回本次影响的条数。
//
// 可见性同样是结构性的：WHERE 里 recipient_id 恒为当前用户，标记不到别人的行；
// 不存在的会话（没有未读、甚至从来没有消息）落到 0 行，调用方按"没有可标记的"处理，
// 而不是拿它当"会话不存在"——本服务没有"会话"这张表，会话是 (我, 对方) 这一对。
//
// 幂等：已经读过的行不参与更新（read_at IS NULL 过滤），所以重复调用第二次返回 0，
// 且不会刷新已读时间戳（回执时间应停在第一次读到的那一刻）。
func (s *Store) MarkConversationRead(ctx context.Context, userID, peerID string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE community.direct_messages
		   SET read_at = now()
		 WHERE recipient_id = $1::uuid AND sender_id = $2::uuid AND read_at IS NULL`,
		userID, peerID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
