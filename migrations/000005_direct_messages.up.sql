-- 私信（DM）：前端 DirectMessageModal 已经在调 GET/POST /api/messages/with/{id}，
-- 但四仓都没有这张表，因此两个端点此前必然 404。私信是用户之间的互动内容，
-- 与论坛、收藏同属互动系统，因此落在本服务自有的 community schema 里。
--
-- 归属边界：对方用户 id 只当**外部引用**存，不建跨库外键、也不在写入前查账号库
-- （账号数据归账号服务）。代价是收件人被删除后这些私信仍在，由调用方自行处置。
--
-- id 由应用层生成（uuid.NewString()，与 topics/posts 同一口径），刻意**不写默认值**：
-- 编排与 CI 用的都是 PostgreSQL 16（deploy/docker-compose.yml 的 postgres:16-alpine），
-- uuidv7() 到 18 才有，写 DEFAULT uuidv7() 会让这一版迁移在目标实例上直接失败；
-- gen_random_uuid() 虽然 13 起可用，但与既有两张表的写法不一致，没必要多一条口径。
--
-- read_at 是已读回执的预留列（NULL = 未读）：当前两个端点都不读不写它
-- （对外形状里没有"已读"字段），落地回执时由收信人读会话时置位。
--
-- 幂等：CREATE TABLE/INDEX IF NOT EXISTS；runner 应用过本版本后直接跳过。
CREATE TABLE IF NOT EXISTS community.direct_messages(
  id uuid PRIMARY KEY,
  sender_id uuid NOT NULL,
  recipient_id uuid NOT NULL,
  body text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  read_at timestamptz,
  CHECK (sender_id <> recipient_id)
);

-- 会话索引：两人之间的消息**按时间倒序**取（最新在前）。列顺序按"等值列在前、排序列在后"排，
-- 前两列由 LEAST/GREATEST 归一成一对参与者——接口的 WHERE 用逐字相同的表达式，
-- 规划器才能拿它做范围扫描 + 倒序，而不是全表扫完再排序。
-- 排序键再带上 id：created_at 只有微秒精度，同一事务里插入的多行会撞在一起（now() 是事务时间），
-- 分页要有确定的次序就必须有第二排序键。
CREATE INDEX IF NOT EXISTS direct_messages_conversation
  ON community.direct_messages (LEAST(sender_id,recipient_id), GREATEST(sender_id,recipient_id), created_at DESC, id DESC);
