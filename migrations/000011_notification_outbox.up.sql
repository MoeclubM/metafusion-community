-- 必须送达通知的待投递表（000011，X02）。
--
-- 边界（见 internal/handler/notifications.go 头注释）：
--   - 尽力投递（best-effort）：评论被回复 comment.replied 经 deliverAll 同步投递，
--     整批共享 2s 预算，失败只记日志——迟到的提醒没有价值，不阻塞写请求。
--   - 必须送达（must-deliver）：审核/安全/处置类通知先落本表，再由重试器（幂等、
--     退避、过期、失败查询）投递到目录收件箱。不引入 Kafka：量级与语义用本库表即够。
--
-- 幂等：event_id 唯一（目录侧按它判“同一事件”，见 internal/catalog/notify.go），
-- 重复入队 ON CONFLICT DO NOTHING；并发重试的重复投递由目录侧去重兜底。
-- 过期：expires_at 到期后由 ExpireOutbox 置 expired，不再重试；attempts 耗尽置 failed，
-- 由管理端查询与手动重试（GET/POST /api/community/admin/notifications/outbox*）。
--
-- 依赖方向：收件箱仍在目录库（目录是事件产生端的主系统与前端 /api 主入口），
-- 互动服务依赖目录的投递端点；这只是边界整理，不拆第五个微服务。
--
-- 幂等：CREATE TABLE/INDEX IF NOT EXISTS；runner 应用过本版本后直接跳过。
CREATE TABLE IF NOT EXISTS community.notification_outbox(
  id uuid PRIMARY KEY,
  recipient_id uuid NOT NULL,
  type text NOT NULL,
  subject_type text NOT NULL,
  subject_id text NOT NULL,
  dedupe_key text NOT NULL,
  event_id text NOT NULL UNIQUE,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','sent','failed','expired')),
  attempts int NOT NULL DEFAULT 0,
  next_retry_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  last_error text NOT NULL DEFAULT '',
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS notification_outbox_due ON community.notification_outbox(status, next_retry_at, id);
CREATE INDEX IF NOT EXISTS notification_outbox_recipient ON community.notification_outbox(recipient_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS notification_outbox_dedupe ON community.notification_outbox(dedupe_key);
