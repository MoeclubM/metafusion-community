-- 举报与申诉（report / appeal，000008）。
--
-- 缺口（审计 2026-09-19 S-30 / round2-func-gaps 第 3 条）：站点此前**没有任何举报载体**——
-- 用户遇到违规内容或用户无处提交，管理员也没有处置队列与处置留痕。本文件建三张表，
-- 构成"用户提交 → 队列处置 → 被处置方申诉 → 申诉进队列"的最小闭环。
--
-- 为什么放在互动服务：短评（community.topics 的评论板块行）与帖子（主题 / 楼中回复）都在本服务，
-- 举报对象里只有它们能在本地解析出作者、也才谈得上"下线内容"；跨服务的对象
-- （实体 / 用户 / 资源）只存**不透明引用**（target_type + target_id），不建外键、不跨库查询——
-- 本服务既不拥有、也不应假设别的服务的表结构。
--
-- target_id 用 text 而不是 uuid：跨服务对象的 id 未必是 uuid（资源键是路径式字符串），
-- 用 text 存不透明引用，才不会把"别人的 id 形态"写进本服务的约束里。
--
-- 状态机（reports.status）：pending 待处理 → accepted 已受理 → resolved 已处置；
-- pending → rejected 已驳回。rejected / resolved 是终态，再次处置回 409 invalid_report_state。
-- accepted 是"已受理、待处置"的中间态：它让"受理了但还没下线"与"已经处置完"在队列里分得开。
--
-- 同一举报人对同一对象的**未终结**举报只允许一条（partial unique index，见下）：
-- 重复提交回 409 duplicate_report，而不是插一行看起来像新举报的重复行。
-- 口径刻意是"未终结"而非"永久"：被驳回或被处置之后内容若再出问题，同一人应该还能再举报一次；
-- 永久唯一会把"举报过一次"变成"永久失去举报权"。
--
-- 申诉（report_appeals）只做"被处置方可提交一次并进入队列"，不自造完整仲裁流程：
-- 一条报告 + 一个被处置人 = 至多一条申诉（唯一索引）；申诉有自己的状态与处理人，
-- 处理结论**不反向改写报告状态**——报告已终态，回改会让"当时到底处置了什么"无从追溯。
--
-- 时间线（report_events）：提交、受理、驳回、处置、申诉、申诉处理各落一行。
-- 只靠 reports 行上的 reviewed_at 看不出中间态（受理 → 处置是两步，可能还是两个人）。

CREATE TABLE IF NOT EXISTS community.reports(
  id uuid PRIMARY KEY,
  target_type text NOT NULL CHECK(target_type IN ('entity','comment','post','user','resource')),
  target_id text NOT NULL,
  -- 被处置方（可申诉的一方）：短评/帖子提交时从本地行解析，用户对象就是该用户本人；
  -- 实体/资源在本地无法解析，留 NULL —— 这时申诉不可用（口径写在 handler 的注释里）。
  target_author_id uuid,
  target_author_name text NOT NULL DEFAULT '',
  -- 提交时刻的目标快照（板块、所属主题、标题、正文摘要）：内容被下线或删除之后，
  -- 队列与详情仍然要能看出"当时被举报的是什么"，只存 id 会让处置记录变成一串 uuid。
  target_context jsonb NOT NULL DEFAULT '{}'::jsonb,
  reason text NOT NULL CHECK(reason IN ('spam','abuse','harassment','illegal','copyright','privacy','misinformation','other')),
  detail text NOT NULL DEFAULT '',
  evidence_url text NOT NULL DEFAULT '',
  reporter_id uuid NOT NULL,
  reporter_name text NOT NULL DEFAULT '',
  status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','accepted','rejected','resolved')),
  reviewer_id uuid,
  reviewer_name text NOT NULL DEFAULT '',
  review_note text NOT NULL DEFAULT '',
  -- 处置结论：content_removed 下线内容（内容由既有删除端点下线，服务端复核后才记录）、
  -- user_banned 封禁用户（封禁由账号服务的既有动作执行，本服务不新造一套封禁）、
  -- none 无强制动作（例如警告或转交别的服务）。
  enforcement text NOT NULL DEFAULT '' CHECK(enforcement IN ('','content_removed','user_banned','none')),
  reviewed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
-- 未终结的重复举报拦截。索引名带 reports_ 前缀，与社区其余索引同风格。
CREATE UNIQUE INDEX IF NOT EXISTS reports_open_unique
  ON community.reports(reporter_id, target_type, target_id)
  WHERE status IN ('pending','accepted');
CREATE INDEX IF NOT EXISTS reports_queue ON community.reports(status, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS reports_target ON community.reports(target_type, target_id);
CREATE INDEX IF NOT EXISTS reports_reporter ON community.reports(reporter_id, created_at DESC);

CREATE TABLE IF NOT EXISTS community.report_events(
  id bigserial PRIMARY KEY,
  report_id uuid NOT NULL REFERENCES community.reports(id) ON DELETE CASCADE,
  at timestamptz NOT NULL DEFAULT now(),
  -- created / accepted / rejected / resolved / appealed / appeal_reviewed
  kind text NOT NULL,
  actor_id uuid,
  actor_name text NOT NULL DEFAULT '',
  -- reporter / reviewer / appellant：同一个用户在不同事件里可能是不同角色。
  actor_role text NOT NULL DEFAULT '',
  note text NOT NULL DEFAULT '',
  meta jsonb NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS report_events_report ON community.report_events(report_id, at, id);

CREATE TABLE IF NOT EXISTS community.report_appeals(
  id uuid PRIMARY KEY,
  report_id uuid NOT NULL REFERENCES community.reports(id) ON DELETE CASCADE,
  appellant_id uuid NOT NULL,
  appellant_name text NOT NULL DEFAULT '',
  body text NOT NULL,
  status text NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','accepted','rejected')),
  reviewer_id uuid,
  reviewer_name text NOT NULL DEFAULT '',
  review_note text NOT NULL DEFAULT '',
  reviewed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
-- 被处置方对同一条处置只能申诉一次：重复提交回 409 duplicate_appeal。
CREATE UNIQUE INDEX IF NOT EXISTS report_appeals_once
  ON community.report_appeals(report_id, appellant_id);
CREATE INDEX IF NOT EXISTS report_appeals_queue ON community.report_appeals(status, created_at DESC, id DESC);
