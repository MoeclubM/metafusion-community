"use client";

import { ApiError, fetchApi } from "./client";

// 举报与申诉的接口层（服务端契约：../internal/handler/reports.go 与 reports_admin.go）。
//
// 两条队列 + 三种处置 + 申诉处理；**下线内容与封禁用户没有新端点**：
//   - 下线内容复用既有的内容删除入口（DELETE /api/community/posts/{id}、
//     /api/community/topics/{id}、/api/community/topics/{id}/posts/{postId}）；
//   - 封禁用户在账号服务（PUT /api/admin/users/{id}/ban），本后台只把它记成处置结论。

/** 举报对象类型（服务端词表，顺序与迁移的 CHECK 一致）。 */
export type ReportTargetType = "entity" | "comment" | "post" | "user" | "resource";

/** 一条举报（管理端形状：比用户端多出 reporter_id / target_author_id 等内部标识）。 */
export interface ReportRow {
  id: string;
  target_type: ReportTargetType;
  target_id: string;
  target_author_id?: string;
  target_author_name?: string;
  /** 提交时刻的目标快照：content_kind(comment|topic|reply|user) / board_code / topic_id / entity_id / excerpt … */
  target_context: Record<string, unknown>;
  reason: string;
  detail?: string;
  evidence_url?: string;
  reporter_id: string;
  reporter_name: string;
  status: string;
  reviewer_id?: string;
  reviewer_name?: string;
  review_note?: string;
  enforcement?: string;
  reviewed_at?: string;
  created_at: string;
  updated_at: string;
}

export interface AppealRow {
  id: string;
  report_id: string;
  appellant_id: string;
  appellant_name: string;
  body: string;
  status: string;
  reviewer_id?: string;
  reviewer_name?: string;
  review_note?: string;
  reviewed_at?: string;
  created_at: string;
  /** 队列行会带上所属举报的关键字段，处理人不必再点一次才能判断。 */
  target_type?: ReportTargetType;
  target_id?: string;
  reason?: string;
  report_status?: string;
  target_author_id?: string;
}

export interface ReportEvent {
  at: string;
  kind: string;
  actor_id?: string;
  actor_name?: string;
  actor_role: string;
  note?: string;
  meta?: Record<string, unknown>;
}

export interface ReportQueueRow {
  report: ReportRow;
  appeal: AppealRow | null;
}

export interface ReportQueuePage {
  items: ReportQueueRow[];
  total: number;
}

export interface ReportDetail {
  item: ReportRow;
  events: ReportEvent[];
  appeals: AppealRow[];
  /** 本地目标此刻是否还在（非本地类型恒 false）：决定"下线内容"这条处置还能不能做。 */
  target_present: boolean;
}

export interface AppealQueuePage {
  items: AppealRow[];
  total: number;
}

export interface ReportQueueParams {
  statuses?: string[];
  targetType?: string;
  q?: string;
  page: number;
  pageSize: number;
}

function page3(x: number): number { return Math.max(1, Math.floor(x)); }

/** 队列：GET /api/community/admin/reports（状态多选逗号分隔 + 类型 + 关键词 + 分页）。 */
export function fetchReportQueue(params: ReportQueueParams): Promise<ReportQueuePage> {
  const query = new URLSearchParams();
  if (params.statuses && params.statuses.length > 0) query.set("status", params.statuses.join(","));
  if (params.targetType && params.targetType !== "all") query.set("target_type", params.targetType);
  if (params.q && params.q.trim() !== "") query.set("q", params.q.trim());
  query.set("page", String(page3(params.page)));
  query.set("page_size", String(params.pageSize));
  return fetchApi<ReportQueuePage>("/api/community/admin/reports?" + query.toString());
}

/** 详情：GET /api/community/admin/reports/{id}（报告 + 时间线 + 申诉 + 目标是否还在）。 */
export function fetchReportDetail(id: string): Promise<ReportDetail> {
  return fetchApi<ReportDetail>("/api/community/admin/reports/" + encodeURIComponent(id));
}

/** 受理：pending → accepted（说明可选）。 */
export function acceptReport(id: string, note: string): Promise<ReportRow> {
  return fetchApi<ReportRow>("/api/community/admin/reports/" + encodeURIComponent(id) + "/accept", {
    method: "POST",
    body: JSON.stringify({ note }),
  }).then((raw) => (raw as { item?: ReportRow }).item ?? (raw as unknown as ReportRow));
}

/** 驳回：pending / accepted → rejected（说明必填：驳回要向举报人交代理由）。 */
export function rejectReport(id: string, note: string): Promise<ReportRow> {
  return fetchApi<ReportRow>("/api/community/admin/reports/" + encodeURIComponent(id) + "/reject", {
    method: "POST",
    body: JSON.stringify({ note }),
  }).then((raw) => (raw as { item?: ReportRow }).item ?? (raw as unknown as ReportRow));
}

/**
 * 处置：pending / accepted → resolved，带处置结论 + 说明。
 *
 * enforcement=content_removed 时服务端会**复核**本地内容确实已经不在了：
 * 所以调用方必须先用既有删除入口把内容下线（见 removeReportedContent），
 * 否则服务端回 409 content_still_present。
 */
export function resolveReport(id: string, enforcement: string, note: string): Promise<ReportRow> {
  return fetchApi<ReportRow>("/api/community/admin/reports/" + encodeURIComponent(id) + "/resolve", {
    method: "POST",
    body: JSON.stringify({ enforcement, note }),
  }).then((raw) => (raw as { item?: ReportRow }).item ?? (raw as unknown as ReportRow));
}

/** 申诉队列：GET /api/community/admin/appeals（默认只看待处理）。 */
export function fetchAppealQueue(params: { statuses?: string[]; page: number; pageSize: number }): Promise<AppealQueuePage> {
  const query = new URLSearchParams();
  if (params.statuses && params.statuses.length > 0) query.set("status", params.statuses.join(","));
  query.set("page", String(page3(params.page)));
  query.set("page_size", String(params.pageSize));
  return fetchApi<AppealQueuePage>("/api/community/admin/appeals?" + query.toString());
}

/** 处理申诉：pending → accepted / rejected（说明必填）。 */
export function reviewAppeal(id: string, status: string, note: string): Promise<AppealRow> {
  return fetchApi<AppealRow>("/api/community/admin/appeals/" + encodeURIComponent(id) + "/review", {
    method: "POST",
    body: JSON.stringify({ status, note }),
  }).then((raw) => (raw as { item?: AppealRow }).item ?? (raw as unknown as AppealRow));
}

/**
 * 下线被举报的内容：**复用既有删除端点**，不新造一套。
 * 按快照 content_kind 选入口（comment / reply / topic），与帖子面板点"删除"走的是同一条路。
 * 支持的类型之外一律抛 not_removable：实体 / 资源 / 用户的下线归各自服务，本后台不假装能做。
 */
export function removeReportedContent(row: ReportRow): Promise<void> {
  const kind = String((row.target_context?.content_kind as string) ?? "");
  const topicId = String((row.target_context?.topic_id as string) ?? "");
  if (kind === "comment") {
    return fetchApi<void>("/api/community/posts/" + encodeURIComponent(row.target_id), { method: "DELETE" });
  }
  if (kind === "reply") {
    if (!topicId) throw new ApiError(0, "not_removable", "缺少 topic_id，无法下线该回复");
    return fetchApi<void>(
      "/api/community/topics/" + encodeURIComponent(topicId) + "/posts/" + encodeURIComponent(row.target_id),
      { method: "DELETE" },
    );
  }
  if (kind === "topic") {
    return fetchApi<void>("/api/community/topics/" + encodeURIComponent(row.target_id), { method: "DELETE" });
  }
  throw new ApiError(0, "not_removable", "该类型不在本服务：下线动作归拥有它的服务");
}
