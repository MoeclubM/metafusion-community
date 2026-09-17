"use client";

import type { BoardPatch, BoardRow } from "../boards";
import { fetchApi } from "./client";

/** 板块列表：匿名可读、裸数组、没有查询参数（搜索在客户端做）。 */
export function fetchBoards(): Promise<BoardRow[]> {
  return fetchApi<BoardRow[]>("/api/community/boards");
}

/**
 * 板块配置：PUT 是**补丁语义**，只发改动过的字段（空载荷会被服务端 400 拒）。
 * 四语校验与"不能改 code"都由服务端决定，这里只负责如实提交与如实报错。
 */
export function updateBoard(code: string, patch: BoardPatch): Promise<BoardRow> {
  return fetchApi<BoardRow>("/api/community/boards/" + encodeURIComponent(code), {
    method: "PUT",
    body: JSON.stringify(patch),
  });
}
