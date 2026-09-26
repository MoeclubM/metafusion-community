// 互动服务的权限码：码表来源是服务端 internal/auth/permission.go（本文件只登记，不发明），
// 判定口径与服务的 Can 逐字一致——前台只用来"别把用户引到注定 403 的按钮上"，
// 真正的授权仍在服务端，前端放行不代表服务端会放行。
export const COMMUNITY_POST_CREATE = "community.post.create";
export const COMMUNITY_POST_MODERATE = "community.post.moderate";
export const COMMUNITY_TOPIC_PIN = "community.topic.pin";
export const COMMUNITY_BOARD_MANAGE = "community.board.manage";
export const COMMUNITY_REPORT_REVIEW = "community.report.review";

/** 治理类码，与服务的 communityPermissionCodes 同集合。 */
export const COMMUNITY_GOVERNANCE_CODES = [
  COMMUNITY_POST_MODERATE,
  COMMUNITY_TOPIC_PIN,
  COMMUNITY_BOARD_MANAGE,
  COMMUNITY_REPORT_REVIEW,
] as const;

export type PermissionSubject = {
  permissions?: string[] | null;
} | null | undefined;

/**
 * can 判定身份是否持有权限码：
 *   - 只认 permissions，含 * 通配。
 * 与服务 internal/auth/permission.go 的 Can 一一对应。
 */
export function can(subject: PermissionSubject, code: string): boolean {
  if (!subject) return false;
  const perms = subject.permissions ?? [];
  return perms.includes("*") || perms.includes(code);
}

/** 任一码持有即可（用于"页签是否出现"这种多码入口）。 */
export function canAny(subject: PermissionSubject, codes: readonly string[]): boolean {
  return codes.some((code) => can(subject, code));
}

/** 展示用：权限码缺省提示里点名需要的码，不写成"权限不足"这种没法排查的话。 */
export function formatCodes(codes: readonly string[]): string {
  return codes.join(", ");
}
