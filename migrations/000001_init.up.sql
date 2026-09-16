-- 互动服务自有 community schema 的基线结构（000001）。
--
-- 这里是 community schema 的**唯一**结构来源：服务启动（internal/store.Init）与任何迁移入口
-- 读的都是这份文件，不再有 Go 里的内联 DDL 副本。
--
-- 表结构与主仓库 modules 包里的 forum_* / records 表逐列一致，因此切流前可以用 cmd/migrate
-- 做一次性数据搬运，不需要任何字段映射（逐列一致性由 internal/store/schema_parity_test.go 钉住）。
-- 全部语句都是幂等的（IF NOT EXISTS）：对已经由旧内联 DDL 建过表的实例，首次执行只是登记版本。

CREATE SCHEMA IF NOT EXISTS community;

CREATE TABLE IF NOT EXISTS community.boards(
  code text PRIMARY KEY,
  names jsonb NOT NULL DEFAULT '{}'::jsonb,
  descriptions jsonb NOT NULL DEFAULT '{}'::jsonb,
  color text NOT NULL DEFAULT 'emerald',
  icon text NOT NULL DEFAULT 'BookOpen',
  sort_order int NOT NULL DEFAULT 0,
  is_enabled boolean NOT NULL DEFAULT true,
  show_in_feed boolean NOT NULL DEFAULT true
);
CREATE TABLE IF NOT EXISTS community.topics(
  id uuid PRIMARY KEY,
  board_code text NOT NULL REFERENCES community.boards(code),
  author_id uuid NOT NULL,
  author_name text NOT NULL DEFAULT '',
  title text NOT NULL,
  body text NOT NULL,
  language text NOT NULL DEFAULT '',
  entity_id uuid,
  is_pinned boolean NOT NULL DEFAULT false,
  is_locked boolean NOT NULL DEFAULT false,
  view_count int NOT NULL DEFAULT 0,
  reply_count int NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  last_activity_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS community.posts(
  id uuid PRIMARY KEY,
  topic_id uuid NOT NULL REFERENCES community.topics(id) ON DELETE CASCADE,
  author_id uuid NOT NULL,
  author_name text NOT NULL DEFAULT '',
  body text NOT NULL,
  post_number int NOT NULL,
  reply_to_post_number int,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(topic_id,post_number)
);
CREATE TABLE IF NOT EXISTS community.tags(id bigserial PRIMARY KEY,name text NOT NULL,slug text NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS community.topic_tags(
  topic_id uuid NOT NULL REFERENCES community.topics(id) ON DELETE CASCADE,
  tag_id bigint NOT NULL REFERENCES community.tags(id) ON DELETE CASCADE,
  PRIMARY KEY(topic_id,tag_id)
);
CREATE INDEX IF NOT EXISTS topics_board ON community.topics(board_code,last_activity_at DESC);
CREATE INDEX IF NOT EXISTS topics_entity ON community.topics(entity_id);
CREATE INDEX IF NOT EXISTS posts_topic ON community.posts(topic_id,post_number);
-- 收藏：目标类型就是实体 kind（固定八种骨架），落库与读取不做词表映射，
-- 表结构与主仓库 catalog.favorites 逐列一致，便于一次性导入。
CREATE TABLE IF NOT EXISTS community.favorites(
  user_id uuid NOT NULL,
  target_type text NOT NULL CHECK (target_type IN ('agent','collection','work','content_unit','expression','release','medium','track')),
  target_id uuid NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY(user_id,target_type,target_id)
);
CREATE INDEX IF NOT EXISTS favorites_target ON community.favorites(target_type, target_id);
CREATE INDEX IF NOT EXISTS favorites_user_created ON community.favorites(user_id, created_at DESC);
-- 用户互动数据（评分、进度、持有）：属互动系统，不属目录元数据。
CREATE TABLE IF NOT EXISTS community.records(
  owner_id uuid NOT NULL,
  entity_id uuid NOT NULL,
  document jsonb NOT NULL,
  PRIMARY KEY(owner_id,entity_id)
);
