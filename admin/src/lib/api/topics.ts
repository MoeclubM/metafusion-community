"use client";

import type { TopicPage, TopicRow } from "../topics";
import { fetchApi } from "./client";

export interface TopicListParams {
  boardCode?: string;
  q?: string;
  limit: number;
  offset: number;
}

/**
 * 主题列表：limit/offset 写法（本端点第 2 代兼容期内的口径，见服务端 paging.go；
 * 新端点用的是 page/page_size，两套在各自端点内保持单一来源）。
 * board_code=all 与服务端"不带该参数"同义：都会排除评论板块。
 */
export function fetchTopics(params: TopicListParams): Promise<TopicPage> {
  const query = new URLSearchParams();
  if (params.boardCode && params.boardCode !== "all") query.set("board_code", params.boardCode);
  if (params.q && params.q.trim() !== "") query.set("q", params.q.trim());
  query.set("limit", String(params.limit));
  query.set("offset", String(params.offset));
  return fetchApi<unknown>("/api/community/topics?" + query.toString()).then((raw) => {
    const page = raw as { items?: unknown; total?: unknown };
    return {
      items: Array.isArray(page.items) ? (page.items as TopicRow[]) : [],
      total: typeof page.total === "number" ? page.total : 0,
    };
  });
}

/** 置顶 / 取消置顶：需要 community.topic.pin；响应是主题形状。 */
export function setTopicPin(id: string, pinned: boolean): Promise<TopicRow> {
  return fetchApi<TopicRow>("/api/community/topics/" + encodeURIComponent(id) + "/pin", {
    method: "PUT",
    body: JSON.stringify({ pinned }),
  });
}

/** 删除主题（级联回复）：作者本人或持 community.post.moderate。 */
export function deleteTopic(id: string): Promise<void> {
  return fetchApi<void>("/api/community/topics/" + encodeURIComponent(id), { method: "DELETE" });
}
