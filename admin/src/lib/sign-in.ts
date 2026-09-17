// 登录回跳地址的**唯一**构造点。三条规则都来自实测踩坑：
//   1. 不把 /login 本身当成回跳目标——页面若在 /login 上再拼一次 ?redirect=，
//      下一次渲染会把上一次的地址当成"当前地址"再编码一遍，URL 越滚越长（几千字符的套娃）；
//   2. 只回跳**本应用 basePath 内**的路径：回跳到别的应用的路径只会再吃一个 404；
//   3. 同一次页面加载只算一次回跳目标（模块级缓存），刷新页面才重置——
//      避免任何"边渲染边重新计算当前地址"的循环。
export const BASE_PATH = "/admin/community";
export const LOGIN_PATH = "/login";

let cachedTarget: string | null = null;

export function isOwnPath(pathname: string): boolean {
  if (!pathname) return false;
  return pathname === BASE_PATH || pathname === BASE_PATH + "/" || pathname.startsWith(BASE_PATH + "/");
}

/**
 * 当前地址是否就是登录页：站点登录页是 /login，但登录页也可能被挂在别的前缀下
 * （网关换过前缀、或应用自己渲染了一个 /admin/community/login 的兜底页），
 * 因此按"最后一段是 login"判断——只要它本身是登录入口，就绝不再给它拼 redirect。
 */
export function isLoginPath(pathname: string): boolean {
  const trimmed = pathname.replace(/\/+$/, "");
  return trimmed === LOGIN_PATH || trimmed.endsWith(LOGIN_PATH);
}

/**
 * buildSignInHref 返回登录页地址：只有"在本应用内且不是登录页"时才带 redirect 参数。
 * 传入的是**路径 + 查询串**（不含 origin 与 hash）：跨应用跳转只认站内路径。
 */
export function buildSignInHref(pathname: string, search = ""): string {
  if (!isOwnPath(pathname) || isLoginPath(pathname)) return LOGIN_PATH;
  return LOGIN_PATH + "?redirect=" + encodeURIComponent(pathname + search);
}

/** 同一次页面加载内的回跳目标（第一次算出来的那个，之后的调用复用它）。 */
export function signInHrefOnce(): string {
  if (cachedTarget !== null) return cachedTarget;
  if (typeof window === "undefined") return LOGIN_PATH;
  cachedTarget = buildSignInHref(window.location.pathname, window.location.search);
  return cachedTarget;
}

/** 仅测试用：清掉模块级缓存（用例之间互不影响）。 */
export function resetSignInCache(): void {
  cachedTarget = null;
}
