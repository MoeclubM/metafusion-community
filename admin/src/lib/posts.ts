// 帖子（短评与楼中回复）的纯逻辑：两类内容的响应解析。
// 契约来源：
//   internal/handler/community.go:23-108 —— /community/feed 返回 {items:[{id,entity_id,author_id,
//     author_name,body,created_at,entity_title,entity_kind}]}，只有 limit 与 q，没有 total/offset
//   internal/handler/forum.go:724-755    —— 主题详情的 posts 是 [{id,topic_id,user_id,author_name,
//     content,post_number,reply_to_post_number,created_at}]
export interface CommentRow {
  id: string;
  entity_id: string;
  author_id: string;
  author_name: string;
  body: string;
  created_at: string;
  entity_title?: string;
  entity_kind?: string;
}

export interface ReplyRow {
  id: string;
  topic_id: string;
  user_id: string;
  author_name: string;
  content: string;
  post_number: number;
  reply_to_post_number?: number | null;
  created_at: string;
}

export const COMMENT_PAGE_SIZE = 50;

export function parseComments(raw: unknown): CommentRow[] {
  const obj = (raw ?? {}) as { items?: unknown };
  return Array.isArray(obj.items) ? (obj.items as CommentRow[]) : [];
}

/** 主题详情里的楼层：字段缺失（评论板块的详情就没有 posts）按空列表处理。 */
export function parseReplies(raw: unknown): ReplyRow[] {
  const obj = (raw ?? {}) as { posts?: unknown };
  const rows = Array.isArray(obj.posts) ? (obj.posts as ReplyRow[]) : [];
  return [...rows].sort((a, b) => (a.post_number ?? 0) - (b.post_number ?? 0));
}

export function floorLabel(number: number | undefined | null): string {
  return typeof number === "number" && number > 0 ? `#${number}` : "#?";
}
