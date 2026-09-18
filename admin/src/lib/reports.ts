"use client";

// 举报与申诉的**纯逻辑**：词表、处置动作的可用性、查询串与字典键的映射。
// 放在这里而不是组件里，是为了能用 node --test 直接测（tests/reports.test.ts）——
// 这些判据错了，界面不会报错，只会静默少一个动作或显示一个原始码。

import type { ReportRow } from "./api/reports";

/** 举报状态（服务端词表：internal/store/reports.go）。 */
export const REPORT_STATUSES = ["pending", "accepted", "rejected", "resolved"] as const;
/** 举报对象类型。 */
export const REPORT_TARGET_TYPES = ["entity", "comment", "post", "user", "resource"] as const;
/** 举报理由：顺序即展示顺序（由重到轻）。 */
export const REPORT_REASONS = [
  "illegal",
  "copyright",
  "privacy",
  "abuse",
  "harassment",
  "spam",
  "misinformation",
  "other",
] as const;
/** 处置结论。 */
export const REPORT_ENFORCEMENTS = ["none", "content_removed", "user_banned"] as const;
/** 申诉状态。 */
export const APPEAL_STATUSES = ["pending", "accepted", "rejected"] as const;

export type BadgeTone = "ok" | "off" | "info" | "warn";

/** 状态 → 徽标配色：待处理要显眼（warn），已处置是终态（ok），已驳回退到灰（off）。 */
export function statusTone(status: string): BadgeTone {
  switch (status) {
    case "pending":
      return "warn";
    case "accepted":
      return "info";
    case "resolved":
      return "ok";
    default:
      return "off";
  }
}

/** 申诉状态 → 徽标配色。 */
export function appealTone(status: string): BadgeTone {
  switch (status) {
    case "pending":
      return "warn";
    case "accepted":
      return "ok";
    default:
      return "off";
  }
}

/** 字典键：凡是枚举值都要经这里取键，界面里不出现原始码。 */
export function statusLabelKey(status: string): string { return "admin.reports.status." + status; }
export function reasonLabelKey(reason: string): string { return "admin.reports.reason." + reason; }
export function enforcementLabelKey(value: string): string {
  return "admin.reports.enforcement." + (value === "" ? "none" : value);
}
export function appealStatusLabelKey(status: string): string { return "admin.reports.appealStatus." + status; }
export function targetTypeLabelKey(target: string): string { return "admin.reports.target." + target; }
export function eventLabelKey(kind: string): string { return "admin.reports.event." + kind; }

/** 快照里的内容类型（comment / topic / reply / user）：下线动作与展示都按它分流。 */
export function contentKindOf(row: ReportRow): string {
  return String((row.target_context?.content_kind as string) ?? "");
}

/**
 * 下线内容这条处置能不能做：只有本服务里的内容（短评 / 主题 / 回复）能下线。
 * 实体 / 资源 / 用户的下线动作归拥有它的服务，这里**不给按钮**（给了也只会 400）。
 */
export function mayRemoveContent(row: ReportRow): boolean {
  const kind = contentKindOf(row);
  return kind === "comment" || kind === "topic" || kind === "reply";
}

/**
 * 一次处置可以选的结论：
 *   - none 任何举报都能选；
 *   - content_removed 只有在本地内容上才出现（且服务端会复核内容确实已下线）；
 *   - user_banned 需要有可封禁的对象（本地解析出的作者，或目标本身就是用户）。
 * 封禁动作在账号服务执行，本后台只记录结论——按钮上的提示文案说明了这一点。
 */
export function allowedEnforcements(row: ReportRow): string[] {
  const out = ["none"];
  if (mayRemoveContent(row)) out.push("content_removed");
  if ((row.target_author_id ?? "") !== "") out.push("user_banned");
  return out;
}

/** 队列查询串：空过滤项不出现在 URL 里（"没过滤"与"过滤成空串"在服务端是两件事）。 */
export function buildQueueQuery(params: {
  statuses?: readonly string[];
  targetType?: string;
  q?: string;
  page: number;
  pageSize: number;
}): string {
  const query = new URLSearchParams();
  if (params.statuses && params.statuses.length > 0) query.set("status", params.statuses.join(","));
  if (params.targetType && params.targetType !== "all") query.set("target_type", params.targetType);
  if (params.q && params.q.trim() !== "") query.set("q", params.q.trim());
  query.set("page", String(Math.max(1, Math.floor(params.page))));
  query.set("page_size", String(params.pageSize));
  return query.toString();
}

/** 列表里"举报对象"的一行摘要：类型 + 快照里的标题/摘要（内容可能已经被删，快照就是留下的证物）。 */
export function targetSummary(row: ReportRow): string {
  const ctx = row.target_context ?? {};
  const title = typeof ctx.topic_title === "string" ? ctx.topic_title : "";
  const excerpt = typeof ctx.excerpt === "string" ? ctx.excerpt : "";
  if (title && excerpt) return title + " · " + excerpt;
  if (title) return title;
  if (excerpt) return excerpt;
  return row.target_id;
}
