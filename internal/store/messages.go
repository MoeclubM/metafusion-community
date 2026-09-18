package store

import (
	"context"
	"database/sql"
	"errors"
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
// **排序是"未读优先、组内按最近一条倒序"**：有未读的会话排在最前（收件箱的首要用途是
// "还有谁在等我回"），组内仍按 last_message.created_at DESC, id DESC。
// 这条排序多了一个"未读"这个可变键，已知影响写在 ListConversations 的注释里。
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
// **排序是"未读优先"**：ORDER BY (未读数 > 0) DESC, created_at DESC, id DESC。它的成本只加在
// 最后那次"对已归并的会话行排序"上（每个会话至多两行归并成一行），两个分支的索引倒序扫描
// 与"每个对方取最近一条"的查询形状都没变；真库用例在 enable_seqscan=off 下继续断言两个分支
// 命中 direct_messages_inbox / direct_messages_outbox（计划对比见 docs-local 报告）。
// 已知影响：未读是**可变**排序键，配合 OFFSET 分页时"读到一半把上面的会话标记成已读"会让它
// 往后挪，翻页可能重复或跳过一行——与所有"排序键可变 + 偏移分页"的列表同理，本次接受。
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
		 ORDER BY (COALESCE(u.n, 0) > 0) DESC, l.created_at DESC, l.id DESC
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

// MessagePolicy 是发信前的两个判定，一次往返取回：收件人是否接收陌生人私信、这一对之间是否已有会话。
//
// 为什么合并成一条查询：发信是热路径，而这两个判定都只跟"收件人"与"这一对"有关，
// 分成两次往返只会多一次网络等待。第二条用的是既有的 direct_messages_conversation 索引
// （EXISTS + LIMIT 语义：找到第一行就停），不需要为它新建索引。
type MessagePolicy struct {
	// AcceptsFromStrangers 是收件人的开关。**没有设置行 = 默认接收**（见 000010 迁移的理由）。
	AcceptsFromStrangers bool
	// HasConversation 为真表示这一对之间已经有过私信（任一方向），因此发送者不是"陌生人"。
	HasConversation bool
}

// SendPolicy 取发信前的判定。收件人 id 只当外部引用（不查账号库），因此不存在的用户
// 与"没设置过的用户"都是默认接收——本服务无法区分二者，也不该假装能区分。
func (s *Store) SendPolicy(ctx context.Context, senderID, recipientID string) (MessagePolicy, error) {
	var p MessagePolicy
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE((SELECT accept_from_strangers
		                   FROM community.direct_message_settings
		                  WHERE user_id = $2::uuid), true),
		       EXISTS (SELECT 1 FROM community.direct_messages
		                WHERE LEAST(sender_id,recipient_id)=LEAST($1::uuid,$2::uuid)
		                  AND GREATEST(sender_id,recipient_id)=GREATEST($1::uuid,$2::uuid))`,
		senderID, recipientID).Scan(&p.AcceptsFromStrangers, &p.HasConversation)
	return p, err
}

// MessageSettings 读我自己的收件设置。没有行即默认接收（true）：默认值不落库，
// 因此"用户从没进过设置页"与"用户明确选了接收"在库里是同一种状态，这是刻意的——
// 默认值改了（例如将来改成"只接收既有会话"）时，不会有存量行把旧默认冻结住。
func (s *Store) MessageSettings(ctx context.Context, userID string) (bool, error) {
	var accept bool
	err := s.db.QueryRowContext(ctx, `
		SELECT accept_from_strangers FROM community.direct_message_settings WHERE user_id = $1::uuid`,
		userID).Scan(&accept)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	return accept, err
}

// SetMessageSettings 写我自己的收件设置（UPSERT：首次写入建行，之后改值并刷新 updated_at）。
// 只写自己的行：user_id 恒为当前用户，请求里没有可以指向别人的输入。
func (s *Store) SetMessageSettings(ctx context.Context, userID string, accept bool) (bool, error) {
	var out bool
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO community.direct_message_settings(user_id, accept_from_strangers)
		VALUES($1::uuid, $2)
		ON CONFLICT (user_id) DO UPDATE
		   SET accept_from_strangers = EXCLUDED.accept_from_strangers,
		       updated_at = now()
		RETURNING accept_from_strangers`, userID, accept).Scan(&out)
	return out, err
}
