-- 板块名与描述改为单语言（用户决议 2026-09-16：论坛不再分语言）。
--
-- 幂等：ADD COLUMN IF NOT EXISTS / DROP COLUMN IF EXISTS 自身幂等；回填只在 jsonb 旧列还在时执行，
-- 因此"已经跑过 000001 的实例"与"已经跑过本迁移的实例"重复执行都不报错、不改已有数据。
-- 回填优先级（与前端/文档同一口径）：zh-CN → en-US → zh-TW → ja-JP → jsonb 里 key 排序后的第一个值；
-- name 全空时兜底用 code（板块身份不能为空），description 允许为空串。
--
-- 旧列的 DROP 刻意放在 DO 之外：既保持幂等，也让 internal/store 的结构测试能静态解析出终态结构。

ALTER TABLE community.boards ADD COLUMN IF NOT EXISTS name text NOT NULL DEFAULT '';
ALTER TABLE community.boards ADD COLUMN IF NOT EXISTS description text NOT NULL DEFAULT '';

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = 'community' AND table_name = 'boards' AND column_name = 'names') THEN
    UPDATE community.boards SET name = COALESCE(
      NULLIF(btrim(names ->> 'zh-CN'), ''),
      NULLIF(btrim(names ->> 'en-US'), ''),
      NULLIF(btrim(names ->> 'zh-TW'), ''),
      NULLIF(btrim(names ->> 'ja-JP'), ''),
      (SELECT NULLIF(btrim(value), '') FROM jsonb_each_text(names) ORDER BY key LIMIT 1),
      code)
    WHERE COALESCE(btrim(name), '') = '';
  END IF;

  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = 'community' AND table_name = 'boards' AND column_name = 'descriptions') THEN
    UPDATE community.boards SET description = COALESCE(
      NULLIF(btrim(descriptions ->> 'zh-CN'), ''),
      NULLIF(btrim(descriptions ->> 'en-US'), ''),
      NULLIF(btrim(descriptions ->> 'zh-TW'), ''),
      NULLIF(btrim(descriptions ->> 'ja-JP'), ''),
      (SELECT NULLIF(btrim(value), '') FROM jsonb_each_text(descriptions) ORDER BY key LIMIT 1),
      '')
    WHERE COALESCE(btrim(description), '') = '';
  END IF;
END $$;

ALTER TABLE community.boards DROP COLUMN IF EXISTS names;
ALTER TABLE community.boards DROP COLUMN IF EXISTS descriptions;
