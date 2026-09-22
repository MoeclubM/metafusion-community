-- A03 受限服务身份：异步投递不再转发用户令牌，作者以产生端确认的事件数据落库。
--
-- 约束：actor_id/actor_name 是回帖事务内已确认的原始作者快照（不存令牌、不延长寿命）；
-- 后台与管理员重试只读这两列，不取调用者身份，因此重试不改作者。
-- 幂等：ADD COLUMN IF NOT EXISTS；runner 已应用版本直接跳过。
ALTER TABLE community.notification_outbox ADD COLUMN IF NOT EXISTS actor_id uuid;
ALTER TABLE community.notification_outbox ADD COLUMN IF NOT EXISTS actor_name text NOT NULL DEFAULT '';
