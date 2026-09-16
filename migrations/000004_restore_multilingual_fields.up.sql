-- 恢复论坛的"字段保留、接口去语言"终态（用户决议 2026-09-17）：把 000002/000003 删掉的列加回来。
--
-- 口径（不要在接口层重新引入语言维度）：接口层已经去掉了语言维度——板块列表/管理接口收发的
-- 是**多语言 map**（names/descriptions，四语齐备）、发帖与主题列表不再带 language；
-- 但数据库列必须保留，所以这里是**前向恢复**而不是回退 000002/000003：
--   * community.boards.names / descriptions  多语言 JSONB（板块名与描述按语种存）
--   * community.topics.language              text，保留列但恒为空串（见下）
-- 000002/000003 已 push 且可能在实例上应用过，runner 用 checksum 守"已应用迁移不得改动"，
-- 因此只能新增版本，绝不能改那两个文件。
--
-- topics.language 的历史值**无法恢复**：000003 已经把列连同数据一起 DROP，旧单体的
-- modules.forum_topics.language 也不保证与切流后的写入同步。这里只把列加回来并给默认空串，
-- 已有主题行的 language 一律为空——它不参与任何读取（SELECT 列清单里没有它），
-- 加回来只是为了让"列必须保留"的库结构与接口层"去语言维度"同时成立。
--
-- 幂等：ADD COLUMN IF NOT EXISTS 自身幂等；回填只在目标 map **缺 zh-CN** 且单值列非空时写。
-- 判据用"缺 zh-CN"而不是"整个 map 为空"：000001 建表时 names 的默认值就是 '{}'，之后没有别的
-- 写入方，因此实际要补的就是"没有中文键"这一种缺口；用"整个 map 为空"会让
-- {"en-US":"Casual"} 这种只丢了中文键的行永远补不上。写入用 jsonb || jsonb_build_object
-- 只并进 zh-CN 一个键，因此在目标 map 已带其它语种时不会覆盖它们，重复执行也是空转
--（合并后 zh-CN 键已存在且非空，条件不再成立）。
-- 兼容两种实例状态：
--   * 只应用过 000001：没有单值列也没有语言值，加列即终态，回填条件空转；
--   * 应用过 000002/000003：单值列（000002 按四语优先级回填过）在，这里把它反向补进 JSONB
--     （topics.language 则只加回列，不回填——历史值已丢）。
-- 单值列 name / description 刻意**不 DROP**：管理接口恢复多语言后，它们退化为容量层的兼容/回退列
-- （管理接口写入时由 names/descriptions 派生），保留可避免"只剩多语言 map、老读方拿不到值"。
-- 取舍与理由见 README「数据与迁移」。
--
-- 回填放在 DO 块里而不是裸 UPDATE：对"只应用过 000001"的实例，单值列 name/description 还不存在
-- （它们由 000002 引入），裸语句会因未知列直接让整份迁移失败。

ALTER TABLE community.boards ADD COLUMN IF NOT EXISTS names jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE community.boards ADD COLUMN IF NOT EXISTS descriptions jsonb NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE community.topics ADD COLUMN IF NOT EXISTS language text NOT NULL DEFAULT '';

DO $$
BEGIN
  -- boards.names 缺少 zh-CN 时用单值列补：000002 已按 zh-CN → en-US → zh-TW → ja-JP → 任一值
  -- 的优先级把旧的四语 map 收敛成单值，因此这一步能把板块名恢复到中文键上（其它语种值 000002 已丢）。
  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = 'community' AND table_name = 'boards' AND column_name = 'name') THEN
    UPDATE community.boards
       SET names = names || jsonb_build_object('zh-CN', btrim(name))
     WHERE name IS NOT NULL AND btrim(name) <> ''
       AND COALESCE(NULLIF(btrim(names ->> 'zh-CN'), ''), '') = '';
  END IF;

  IF EXISTS (SELECT 1 FROM information_schema.columns
             WHERE table_schema = 'community' AND table_name = 'boards' AND column_name = 'description') THEN
    UPDATE community.boards
       SET descriptions = descriptions || jsonb_build_object('zh-CN', btrim(description))
     WHERE description IS NOT NULL AND btrim(description) <> ''
       AND COALESCE(NULLIF(btrim(descriptions ->> 'zh-CN'), ''), '') = '';
  END IF;
END $$;
