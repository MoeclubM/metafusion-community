// 主题与回复的纯逻辑：列表响应解析、翻页换算、摘要。
// 契约来源：internal/handler/forum.go:280-356（{items,total}、q/board_code/limit/offset、置顶优先）。
export interface ForumTag {
  id: number;
  name: string;
}

export interface TopicRow {
  id: string;
  board_code: string;
  user_id: string;
  author_name: string;
  title: string;
  content: string;
  is_pinned: boolean;
  is_locked: boolean;
  view_count: number;
  reply_count: number;
  created_at: string;
  updated_at: string;
  last_activity_at: string;
  entity_id?: string;
  entity_title?: string;
  entity_kind?: string;
  tags?: ForumTag[];
}

export interface TopicPage {
  items: TopicRow[];
  total: number;
}

export const TOPIC_PAGE_SIZE = 20;
export const COMMENT_BOARD = "comment";

/** 主题列表响应解析：服务端是 {items,total}；裸数组属异常输入，按空页处理而不是崩掉。 */
export function parseTopicPage(raw: unknown): TopicPage {
  const obj = (raw ?? {}) as { items?: unknown; total?: unknown };
  const items = Array.isArray(obj.items) ? (obj.items as TopicRow[]) : [];
  const total = typeof obj.total === "number" ? obj.total : items.length;
  return { items, total };
}

export function pageCount(total: number, pageSize = TOPIC_PAGE_SIZE): number {
  if (total <= 0) return 1;
  return Math.max(1, Math.ceil(total / pageSize));
}

export function clampPage(page: number, total: number, pageSize = TOPIC_PAGE_SIZE): number {
  const max = pageCount(total, pageSize);
  if (!Number.isFinite(page) || page < 1) return 1;
  return Math.min(Math.floor(page), max);
}

/** 标题留白兜底：评论板块的行没有标题，用正文首行代替，避免列表出现空白行。 */
export function topicTitle(topic: Pick<TopicRow, "title" | "content">): string {
  const title = (topic.title ?? "").trim();
  if (title) return title;
  const first = (topic.content ?? "").split("\n")[0]?.trim() ?? "";
  return first.length > 60 ? `${first.slice(0, 60)}…` : first;
}

export function excerpt(text: string | undefined | null, max = 160): string {
  const value = (text ?? "").trim().replace(/\s+/g, " ");
  return value.length > max ? `${value.slice(0, max)}…` : value;
}

/** 置顶/锁定这类开关的展示值：缺字段（老响应）按 false，不猜。 */
export function booleanFlag(value: unknown): boolean {
  return value === true;
}
