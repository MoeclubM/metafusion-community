-- S4 可靠送达接通（000012）。
--
-- 背景：000011 用 event_id 单列唯一做幂等，但同一业务事件要通知多人
-- （一条论坛回帖同时通知被回复楼层作者与主题作者，共用 post id 做 event_id）。
-- 单列唯一会让第二收件人入队时被 ON CONFLICT 静默丢掉——“通知了 A 就永远通知不到 B”。
-- 改为 (recipient_id, event_id) 复合唯一：同一事件不同收件人各存一行，
-- 同一收件人同一事件重复入队仍幂等丢弃。目录侧按 (收件人, dedupe_key) 聚合、
-- 按 last_event_id 判重试幂等，与本约束同向（见主仓 backend/migrations/000003）。
--
-- 并发领取不需要新列做租约：worker 用 SELECT ... FOR UPDATE SKIP LOCKED 原子领取，
-- 领取时把 next_retry_at 推后一个租期（见 store.ClaimDueOutbox），崩溃的 worker
-- 占住的行租期一过自然重新到期；终态更新带 status='pending' 条件，不覆盖已发送。
--
-- 幂等：DROP CONSTRAINT IF EXISTS + CREATE UNIQUE INDEX IF NOT EXISTS；
-- runner 应用过本版本后直接跳过。历史表为空（000011 落地后尚无业务入队），无数据迁移。
ALTER TABLE IF EXISTS community.notification_outbox DROP CONSTRAINT IF EXISTS notification_outbox_event_id_key;
CREATE UNIQUE INDEX IF NOT EXISTS notification_outbox_recipient_event ON community.notification_outbox(recipient_id, event_id);
