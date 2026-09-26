-- 多语言板块字段与无语言维度的主题接口已稳定，移除旧单值副本及无用语言列。
ALTER TABLE community.boards DROP COLUMN IF EXISTS name;
ALTER TABLE community.boards DROP COLUMN IF EXISTS description;

ALTER TABLE community.topics
  DROP COLUMN IF EXISTS language;
