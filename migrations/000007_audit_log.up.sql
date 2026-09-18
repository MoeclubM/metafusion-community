-- 统一审计留痕（audit log）：跨四个服务共用的 audit schema 与 audit.audit_log 表。
--
-- 契约的唯一来源是主仓库 docs/architecture/audit-log.md（§1 表结构、§4 脱敏、§6 迁移与测试）。
-- 四个服务（catalog / auth / community / storage）各自实现一份**逐字相同**的 DDL：四仓是独立
-- module，没有跨仓依赖通道，所以是四份副本而不是共享模块；谁先启动谁建表，其余走 to_regclass 守卫。
--
-- 为什么单独用 audit schema 而不是 community schema：审计表不属于任何单个服务的领域数据
-- （读取面只在账号服务的 GET /api/admin/audit-logs），放在某个业务的 schema 里会让"谁能读"变成
-- 幻觉。为什么不建外键：账号删了审计还得在（与 auth.oauth_audit.client_id 同一理由）。
--
-- 为什么在迁移文件里再取一次 advisory lock（守卫内、建表之前）：四个服务的迁移可能同时首次启动，
-- 建表用同一个键（740205）串行化。本服务的迁移器把**整个文件一次 ExecContext**（internal/migrator 的单文件事务），
-- 一次 Exec 里的多条语句由 lib/pq 作为隐式事务批处理发送，因此 xact 锁覆盖到建表结束；
-- 该键与本服务自己的迁移锁（同样是 740205）相同：同一事务内重复获取是安全的。
--
-- **为什么整段包在 to_regclass 守卫里**（真库实测，2026-09-19）：PostgreSQL 的
-- CREATE INDEX IF NOT EXISTS 会**先做表所有权检查、再看索引是否存在**（CREATE TABLE IF NOT EXISTS
-- 不同，它只要求 schema 的 CREATE）。四个服务启动都会执行这段 DDL，而表由部署时的 mf_audit_owner
-- 预建——不做守卫的话，非 owner 的运行角色会拿到 42501 must be owner of table audit_log，
-- 四个服务全部起不来（实测：预建后四服务全挂；不预建让 community 先建，另三个全挂）。
-- 索引因此不再写 IF NOT EXISTS（在守卫内，表是刚建的），锁用 PERFORM 放进守卫里先取。
--
-- 幂等：表已存在的实例上整段是**纯空转**（所有 DDL 与所有权检查都不会被触发）；
-- 应用过本版本后 runner 直接跳过（community.schema_migrations 记 checksum，本文件不可再改）。
-- 审计行本身由应用侧写入（internal/audit），本迁移只建表、不碰数据。

DO $audit_ddl$
BEGIN
  PERFORM pg_advisory_xact_lock(740205);
  IF to_regclass('audit.audit_log') IS NULL THEN
    CREATE SCHEMA IF NOT EXISTS audit;
    CREATE TABLE audit.audit_log (
      id               uuid PRIMARY KEY,
      occurred_at      timestamptz NOT NULL DEFAULT now(),
      service          text NOT NULL,
      action           text NOT NULL,
      actor_user_id    uuid,
      actor_username   text NOT NULL DEFAULT '',
      credential_type  text NOT NULL DEFAULT '',
      actor_ip         text NOT NULL DEFAULT '',
      actor_user_agent text NOT NULL DEFAULT '',
      target_type      text NOT NULL DEFAULT '',
      target_id        text NOT NULL DEFAULT '',
      changes          jsonb NOT NULL DEFAULT '{}'::jsonb,
      result           text NOT NULL DEFAULT 'success' CHECK (result IN ('success','failure')),
      error_code       text NOT NULL DEFAULT '',
      request_method   text NOT NULL DEFAULT '',
      route            text NOT NULL DEFAULT '',
      http_status      int NOT NULL DEFAULT 0,
      request_id       text NOT NULL DEFAULT ''
    );
    CREATE INDEX audit_log_occurred_at_idx ON audit.audit_log(occurred_at DESC);
    CREATE INDEX audit_log_service_action_idx ON audit.audit_log(service, action, occurred_at DESC);
    CREATE INDEX audit_log_actor_idx ON audit.audit_log(actor_user_id, occurred_at DESC);
    CREATE INDEX audit_log_target_idx ON audit.audit_log(target_type, target_id, occurred_at DESC);
  END IF;
END
$audit_ddl$;
