"use client";

import { ApiError, fetchApi } from "./client";

/** 账号服务 /api/auth/me 的形状（只取本后台用得到的字段）。 */
export interface SessionUser {
  id: string;
  username: string;
  display_name?: string | null;
  groups?: string[] | null;
  permissions?: string[] | null;
}

/**
 * getCurrentUser 是"当前是谁、有什么权限"的唯一来源：账号服务下发 permissions，
 * 各服务按码判定（本后台同样只认码，见 lib/permissions.ts 的 can）。
 * 401/403 视为匿名（没登录就是没登录，不是错误）；其余失败抛出，由界面如实显示。
 */
export async function getCurrentUser(): Promise<SessionUser | null> {
  try {
    const user = await fetchApi<SessionUser>("/api/auth/me");
    if (!user || typeof user.id !== "string" || user.id === "") return null;
    return user;
  } catch (err) {
    if (err instanceof ApiError && (err.status === 401 || err.status === 403)) return null;
    throw err;
  }
}

export function displayNameOf(user: SessionUser): string {
  const name = (user.display_name ?? "").trim();
  return name !== "" ? name : user.username;
}
