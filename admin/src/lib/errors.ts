// 服务端错误码 → 人话。只覆盖本后台会遇到的码，未覆盖的**原样带出**，
// 不把没解释过的码伪装成已解释（排查时能直接搜到码）。
import { formatLocaleList, orderLocales, parseMissingLocales } from "./locales.ts";

export { formatLocaleList, orderLocales, parseMissingLocales };

export type Translate = (key: string, vars?: Record<string, string | number>) => string;

export interface ErrorLike {
  status: number;
  code: string;
  message?: string;
}

/** 兜底形状：任何异常都能换算成 {status, code, message}，网络类错误没有 HTTP 状态。 */
export function toErrorLike(err: unknown): ErrorLike {
  if (typeof err === "object" && err !== null) {
    const anyErr = err as { status?: unknown; code?: unknown; message?: unknown };
    const status = typeof anyErr.status === "number" ? anyErr.status : 0;
    const code = typeof anyErr.code === "string" ? anyErr.code : "";
    const message = typeof anyErr.message === "string" ? anyErr.message : String(err);
    if (status > 0 || code !== "") return { status, code, message };
    return { status: 0, code: "", message };
  }
  return { status: 0, code: "", message: String(err) };
}

const CODE_KEYS: Record<string, string> = {
  authentication_required: "admin.err.unauthorized",
  forbidden: "admin.err.forbidden",
  not_found: "admin.err.notFound",
  invalid_payload: "admin.err.invalidPayload",
  invalid_board: "admin.err.invalidBoard",
  topic_locked: "admin.err.topicLocked",
  module_error: "admin.err.module",
};

/**
 * describeError 把错误翻成一句可执行的话：
 * 缺语种单独解析（"还缺 zh-TW、ja-JP"）、403 点名缺哪个权限码、
 * 5xx/网络失败说清是互动服务不可达，其余把原始码与状态码原样带出。
 */
export function describeError(
  err: ErrorLike,
  t: Translate,
  locale: string,
  requiredPermission?: string
): string {
  const code = (err.code ?? "").trim();
  const missing = parseMissingLocales(code);
  if (missing) {
    return t("admin.err.fourLocales", { locales: formatLocaleList(orderLocales(missing), locale) });
  }
  if (err.status === 403 || code === "forbidden") {
    return t("admin.err.forbidden", { code: requiredPermission ?? t("admin.err.unknownPermission") });
  }
  const key = CODE_KEYS[code];
  if (key) return t(key);
  if (err.status === 401) return t("admin.err.unauthorized");
  if (err.status === 404) return t("admin.err.notFound");
  if (err.status >= 500 && err.status <= 504) {
    return t("admin.err.upstream", { status: err.status });
  }
  if (err.status === 0) {
    return t("admin.err.network", { message: err.message || code });
  }
  return t("admin.err.unknownCode", { status: err.status, code: code || "-" });
}
