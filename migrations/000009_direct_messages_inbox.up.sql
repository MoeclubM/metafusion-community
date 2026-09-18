-- 私信收件箱（会话列表 / 未读数 / 标记已读）：只加索引，不动表结构。
--
-- 为什么不动结构：read_at 在 000005 就为已读回执预留好了（"落地回执时由收信人读会话时置位"），
-- 这一版就是把那一列接上线——收信人主动标记已读时置位，未读数由它的 IS NULL 决定。
--
-- 000005 的 direct_messages_conversation 服务不了收件箱：它的前两列是 LEAST/GREATEST 归一后的
-- 一对参与者，只回答"两个已知参与者之间的整段会话"。收件箱要问的是"我的那一侧"——
-- 我收到的、按对方分组的最近一条，以及我发出的那一侧，都不是它的列顺序能表达的形状。
--
-- 三条查询的形状（实现见 internal/store/messages.go，列顺序必须与下面两行逐字对齐）：
--   1) 我收到的按对方取最近一条：DISTINCT ON (sender_id) WHERE recipient_id = me
--      ORDER BY sender_id, created_at DESC, id DESC        → direct_messages_inbox
--   2) 我发出的按对方取最近一条：DISTINCT ON (recipient_id) WHERE sender_id = me
--      ORDER BY recipient_id, created_at DESC, id DESC     → direct_messages_outbox
--   3) 未读计数与标记已读：WHERE recipient_id = me [AND sender_id = peer] AND read_at IS NULL
--      → 同样走 direct_messages_inbox 的前两列；read_at 不在索引里，这两条查询要回表取它，
--        换来的是不必再维护一条只服务读回执的索引（未读计数是导航栏角标的查询，
--        但它的过滤面已经由前两列收窄到"某人的来信"，回表代价与收益不成比例）。
--   **收件箱列表是两条查询而不是 N+1**：列表本身一条（两个分支各走一条索引），
--   总数一条（两个分支都是索引扫描 + UNION 去重）；前端一次请求拿到的就是整页。
--
-- 排序键都带上 id：created_at 只有微秒精度，同一事务里插入的多行会撞在一起（now() 是事务时间）。
--
-- 幂等：CREATE INDEX IF NOT EXISTS；runner 应用过本版本后直接跳过。
CREATE INDEX IF NOT EXISTS direct_messages_inbox
  ON community.direct_messages (recipient_id, sender_id, created_at DESC, id DESC);

CREATE INDEX IF NOT EXISTS direct_messages_outbox
  ON community.direct_messages (sender_id, recipient_id, created_at DESC, id DESC);
