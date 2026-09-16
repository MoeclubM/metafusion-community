-- 互动服务（metafusion-community）独立数据库角色的授权样例。
--
-- **状态：尚未启用。** 这是 docs/architecture/decoupling-audit-2026-09.md §4（数据层边界）
-- 里 B4「先分角色、再分库」的准备件：现在四个服务共用同一个 DB 用户与同一个库，
-- schema 只是命名约定——任何持凭据的进程都能写别人的 schema，误写不会被库拦下。
--
-- 本文件只提供样例与说明，**不改任何启动路径的默认行为，也不改主仓库的编排**。
-- 启用它需要三件事一起做（缺一件就会把服务启动打断）：
--   1) **停机窗口**：改角色后要重启服务并确认连接串用的是新用户；
--   2) **回滚脚本**：见文末（先撤权限再删角色，服务先换回原连接串）；
--   3) **迁移与运行分角色**：迁移/建表用库 owner（下表 defs 里的 metafusion），
--      服务运行用受限角色（metafusion_community）。
--
-- 为什么第 3 件是硬前提：服务的启动路径（internal/store.Init → internal/migrator.Apply）
-- 会执行 CREATE SCHEMA / CREATE TABLE IF NOT EXISTS。PostgreSQL 在 CREATE TABLE IF NOT EXISTS
-- 上**先做 schema 的 CREATE 权限检查、再看表是否存在**，因此受限角色在已建好结构的库上启动
-- 也会被拒绝（permission denied for schema community）。所以启用本样例必须与
-- 「启动只校验不建表 / 迁移单独跑」一起落地，见审计 §4.3 第 3 条与 §4.4。
--
-- 用法（在目标实例上按实际情况调整；下面的名字与现有 compose 默认值一致）：
--   1. 以库 owner（默认 metafusion）执行本文件；
--   2. 把互动服务的连接串换成 metafusion_community（DATABASE_URL 或 DB_USER/DB_PASSWORD）；
--   3. 冒烟：读板块 → 发主题 → 收藏切换，并确认写其它 schema 被拒（见文末校验）。

-- 1. 角色本身：能登录即可，不给建库/建角色/复制等实例级能力。
CREATE ROLE metafusion_community LOGIN PASSWORD 'CHANGE_ME_APP_ROLE_PASSWORD';

-- 2. 公共权限：public schema 的创建权对 PUBLIC 与对本角色都显式撤掉
--    （PG15+ 默认已不再给 PUBLIC，这条是给从老实例升级上来的库兜底）。
REVOKE ALL ON SCHEMA public FROM PUBLIC;
REVOKE ALL ON SCHEMA public FROM metafusion_community;

-- 3. 只授本服务自己的 schema。社区表全部在 community schema 内，读写都只需要这四类权限；
--    不授 CREATE/ALTER/DROP：结构变更走迁移（owner 角色执行），运行角色不该有权改结构。
GRANT USAGE ON SCHEMA community TO metafusion_community;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA community TO metafusion_community;
-- community.tags.id 是 bigserial：没有序列权限就插不进标签（帖子标签会 500）。
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA community TO metafusion_community;

-- 4. 迁移将来新增的表与序列同样授权。默认权限只对"这条语句之后、由指定角色创建的对象"生效，
--    所以必须由建表用 owner 角色执行；换 owner 名时要同步改这两行。
ALTER DEFAULT PRIVILEGES FOR ROLE metafusion IN SCHEMA community
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO metafusion_community;
ALTER DEFAULT PRIVILEGES FOR ROLE metafusion IN SCHEMA community
  GRANT USAGE, SELECT ON SEQUENCES TO metafusion_community;

-- 5. 其它 schema 一律不给（新角色默认也拿不到）。写出来是为了让"授权范围"一眼可见，
--    避免以后有人顺手补一条 GRANT 把边界弄花。目录/账号/存储分别是别人的域。
REVOKE ALL ON SCHEMA catalog FROM metafusion_community;
REVOKE ALL ON SCHEMA auth FROM metafusion_community;
REVOKE ALL ON SCHEMA storage FROM metafusion_community;

-- 6. 校验（授权后逐条确认；返回 false 表示确实没有权限）：
--    SELECT has_schema_privilege('metafusion_community','community','USAGE');        -- true
--    SELECT has_schema_privilege('metafusion_community','catalog','USAGE');          -- false
--    SELECT has_table_privilege('metafusion_community','community.topics','SELECT'); -- true
--    SELECT has_table_privilege('metafusion_community','community.topics','DELETE'); -- true
--    -- 用受限角色连上去试写别人的域，期望 ERROR: permission denied for schema catalog
--    INSERT INTO catalog.outbox(id,type,entity_id,version,payload) VALUES (gen_random_uuid(),'probe',gen_random_uuid(),1,'{}');

-- ============================================================================
-- 回滚（服务先换回原连接串并确认健康，再执行；顺序：先撤权限、再删角色）
-- ============================================================================
-- REVOKE ALL ON ALL TABLES IN SCHEMA community FROM metafusion_community;
-- REVOKE ALL ON ALL SEQUENCES IN SCHEMA community FROM metafusion_community;
-- REVOKE ALL ON SCHEMA community FROM metafusion_community;
-- REVOKE ALL ON SCHEMA public FROM metafusion_community;
-- ALTER DEFAULT PRIVILEGES FOR ROLE metafusion IN SCHEMA community
--   REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM metafusion_community;
-- ALTER DEFAULT PRIVILEGES FOR ROLE metafusion IN SCHEMA community
--   REVOKE USAGE, SELECT ON SEQUENCES FROM metafusion_community;
-- DROP OWNED BY metafusion_community;  -- 清掉该角色持有的对象（本角色不建对象，通常没有）
-- DROP ROLE metafusion_community;
