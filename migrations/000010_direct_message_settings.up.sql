-- 私信收件人侧开关：是否接收**陌生人**发来的私信（默认接收）。
--
-- 为什么落在本服务、而不是账号服务：私信投递是互动服务的业务，发信路径必须在**不依赖任何
-- 外部服务可用性**的前提下就能判定"要不要拒收"；账号服务目前也没有用户级设置表
-- （GET /api/auth/settings 与 PUT /api/admin/settings 都是实例级；favorites_public 这类
-- 用户级隐私列在 auth.users 里并不存在，README「职责边界」已写明"落地该开关时应由本服务承担"）。
--
-- "陌生人"的定义（平台没有关注/好友关系，必须给一个可判定的口径）：
-- **这一对用户之间还没有任何私信**（双向都不存在）。因此关闭开关的效果是
-- "只接收已经聊过的人的私信"，不会掐断任何既有对话；已经给我发过信的人不是陌生人。
--
-- 默认值 = 接收（true）：不建行即为默认。新功能一上线不能让既有用户突然发不出信，
-- 而且平台没有"先成为好友"的路径可走；滥用面交给发送侧的**陌生人新会话限额**
-- （internal/handler/message_limit.go，每小时 5 个新会话）与收件人自己的这个开关一起收窄。
--
-- 幂等：CREATE TABLE IF NOT EXISTS；runner 应用过本版本后直接跳过。
CREATE TABLE IF NOT EXISTS community.direct_message_settings(
  user_id uuid PRIMARY KEY,
  accept_from_strangers boolean NOT NULL DEFAULT true,
  updated_at timestamptz NOT NULL DEFAULT now()
);
