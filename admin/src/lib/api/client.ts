// 网关请求层：与站点同源（生产里本应用就在网关后面），开发时由 next.config.mjs 的
// rewrite 把 /api/* 转给网关。**路径一律以 / 开头且不含 basePath**（basePath 只影响本应用的页面），
// 例如 /api/community/boards；写成 "api/xxx" 会被解析成 /admin/community/api/xxx，直接 404。
"use client";

const API_BASE = (process.env.NEXT_PUBLIC_API_BASE ?? "").replace(/\/$/, "");

/** 令牌沿用站点约定：localStorage 的 metafusion_token；没有它也能工作（HttpOnly cookie 随同源请求发出）。 */
const TOKEN_KEY = "metafusion_token";
const REFRESH_KEY = "metafusion_refresh_token";

export function getAccessToken(): string | null {
  if (typeof window === "undefined") return null;
  return window.localStorage.getItem(TOKEN_KEY);
}

export function setAuthTokens(accessToken: string, refreshToken?: string | null): void {
  if (typeof window === "undefined") return;
  window.localStorage.setItem(TOKEN_KEY, accessToken);
  if (refreshToken) window.localStorage.setItem(REFRESH_KEY, refreshToken);
}

export function clearAuthTokens(): void {
  if (typeof window === "undefined") return;
  window.localStorage.removeItem(TOKEN_KEY);
  window.localStorage.removeItem(REFRESH_KEY);
}

function readLocale(): string | null {
  if (typeof document === "undefined") return null;
  const m = document.cookie.match(/(?:^|;\s*)NEXT_LOCALE=([^;]+)/);
  return m ? decodeURIComponent(m[1]!) : null;
}

/**
 * ApiError 保留 HTTP 状态码与**服务端错误码**：错误文案由 lib/errors.ts 按码翻译，
 * 不做字符串匹配（服务端错误的形状是 {error: "机器码"}）。
 * status = 0 表示请求没能到达服务端（网络/CORS），此时 code 为空。
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string, message?: string) {
    super(message || code || "HTTP " + status);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    Object.setPrototypeOf(this, ApiError.prototype);
  }
}

let refreshPromise: Promise<string | null> | null = null;

/** 静默续期一次：/api/auth/refresh 以 Bearer 或 HttpOnly cookie 识别调用方，不读 body。 */
async function requestTokenRefresh(): Promise<string | null> {
  if (refreshPromise) return refreshPromise;
  refreshPromise = (async () => {
    try {
      const res = await fetch(API_BASE + "/api/auth/refresh", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({}),
      });
      if (!res.ok) {
        clearAuthTokens();
        return null;
      }
      const data = (await res.json()) as { access_token?: string; token?: string; refresh_token?: string };
      const token = data.access_token || data.token || null;
      if (token) setAuthTokens(token, data.refresh_token);
      else clearAuthTokens();
      return token;
    } catch {
      return null;
    } finally {
      refreshPromise = null;
    }
  })();
  return refreshPromise;
}

function buildHeaders(init: RequestInit, token: string | null): Record<string, string> {
  const headers: Record<string, string> = { ...((init.headers as Record<string, string>) ?? {}) };
  if (!(init.body instanceof FormData) && headers["Content-Type"] === undefined) {
    headers["Content-Type"] = "application/json";
  }
  if (token) headers["Authorization"] = "Bearer " + token;
  const locale = readLocale();
  if (locale) {
    if (headers["X-Locale"] === undefined && headers["x-locale"] === undefined) headers["x-locale"] = locale;
    if (headers["Accept-Language"] === undefined) headers["Accept-Language"] = locale;
  }
  return headers;
}

/** 服务端错误体：{"error":"机器码"}；读不出 JSON 时按状态码兜底，不吞掉状态。 */
async function toApiError(res: Response): Promise<ApiError> {
  let code = "";
  try {
    const data = (await res.json()) as { error?: unknown };
    if (typeof data?.error === "string") code = data.error;
  } catch {
    code = "";
  }
  return new ApiError(res.status, code);
}

/**
 * fetchApi 是唯一的请求出口：401 时静默续期并重试一次（与站点行为一致），
 * 其余非 2xx 一律抛 ApiError（带状态码与错误码），空响应体返回 undefined。
 */
export async function fetchApi<T>(path: string, init: RequestInit = {}): Promise<T> {
  if (!path.startsWith("/")) {
    throw new ApiError(0, "", "API 路径必须以 / 开头（不含 basePath）：" + path);
  }
  const isAuthEndpoint =
    path.startsWith("/api/auth/login") ||
    path.startsWith("/api/auth/refresh") ||
    path.startsWith("/api/auth/logout");

  let token = getAccessToken();
  let res: Response;
  try {
    res = await fetch(API_BASE + path, { ...init, credentials: "same-origin", headers: buildHeaders(init, token) });
  } catch (err) {
    throw new ApiError(0, "", err instanceof Error ? err.message : String(err));
  }

  if (res.status === 401 && !isAuthEndpoint) {
    const fresh = await requestTokenRefresh();
    if (fresh) {
      token = fresh;
      try {
        res = await fetch(API_BASE + path, { ...init, credentials: "same-origin", headers: buildHeaders(init, token) });
      } catch (err) {
        throw new ApiError(0, "", err instanceof Error ? err.message : String(err));
      }
    }
  }

  if (!res.ok) throw await toApiError(res);
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  if (text.trim() === "") return undefined as T;
  return JSON.parse(text) as T;
}
