"use client";

import type { CommentRow } from "../posts";
import { fetchApi } from "./client";

/** 帖子治理列表的一行（服务端 moderationPostItem 的形状）。 */
export interface ModerationPostRow {
  id: string;
  topic_id: string;
  topic_title: string;
  board_code: string;
  author_id: string;
  author_name: string;
  post_number: number;
  reply_to_post_number: number | null;
  excerpt: string;
  truncated: boolean;
  created_at: string;
  updated_at: string;
}

export interface ModerationPostPage {
  items: ModerationPostRow[];
  total: number;
}

export interface ModerationPostParams {
  q?: string;
  page: number;
  pageSize: number;
}

/**
 * 跨主题列回复：GET /api/community/posts?q=&page=&page_size=（page/page_size 是本端点
 * 与服务端 messages/favorites 同口径的写法）。**只读**：服务端不会改动 view_count。
 */
export function fetchModerationPosts(params: ModerationPostParams): Promise<ModerationPostPage> {
  const query = new URLSearchParams();
  if (params.q && params.q.trim() !== "") query.set("q", params.q.trim());
  query.set("page", String(params.page));
  query.set("page_size", String(params.pageSize));
  return fetchApi<ModerationPostPage>("/api/community/posts?" + query.toString());
}

/** 删除回复：作者本人或持 community.post.moderate（主题的 reply_count 由服务端同步减一）。 */
export function deleteReply(topicId: string, postId: string): Promise<void> {
  return fetchApi<void>(
    "/api/community/topics/" + encodeURIComponent(topicId) + "/posts/" + encodeURIComponent(postId),
    { method: "DELETE" },
  );
}

/**
 * 实体短评（评论板块）：走既有的 /api/community/feed —— 短评存在 community.topics 里，
 * 不在治理列表端点覆盖的 community.posts 内，两者不是同一张表。
 * feed 没有 total，因此这里固定取第一页。
 */
export function fetchComments(q: string, limit: number): Promise<CommentRow[]> {
  const query = new URLSearchParams();
  if (q && q.trim() !== "") query.set("q", q.trim());
  query.set("page", "1");
  query.set("page_size", String(limit));
  query.set("sort", "recent");
  return fetchApi<{ items?: CommentRow[] }>("/api/community/feed?" + query.toString()).then((raw) => {
    return Array.isArray(raw?.items) ? raw.items : [];
  });
}

/** 删除短评：仅评论板块，作者本人或持 community.post.moderate。 */
export function deleteComment(id: string): Promise<void> {
  return fetchApi<void>("/api/community/posts/" + encodeURIComponent(id), { method: "DELETE" });
}
