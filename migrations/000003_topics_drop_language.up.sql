-- 论坛内容不再带语言维度（用户决议：论坛不再分语言）：主题的 language 列退役。
--
-- 为什么单独一个版本、而不是并进 000002：000002 已经提交并可能已在某些实例应用过，
-- runner 用 checksum 守"已应用迁移不得改动"（改了就拒绝启动），因此新结构变更一律新增版本。
--
-- 幂等：DROP COLUMN IF EXISTS；runner 应用过本版本后直接跳过。
-- 破坏性列变更：language 里的历史值不再保留（旧单体的 modules.forum_topics 仍是四语时代的表，
-- 反向搬运工具已改成给老表补空语言值）。
ALTER TABLE community.topics DROP COLUMN IF EXISTS language;
