/** @type {import('next').NextConfig} */
const basePath = "/admin/community";

// 网关按路径把它交给本应用（community-admin:3000），因此 basePath 必须与网关矩阵逐字一致：
// 改这里要同步 nginx 的 location 与 README「宿主需要接入的东西」，否则页面能起、入口 404。
const nextConfig = {
  reactStrictMode: true,
  // 默认会带 X-Powered-By: Next.js（线上实测本管理台响应里就有），关掉它不改变任何行为。
  poweredByHeader: false,
  basePath,
  // 网关对无尾斜杠的入口做 301 → /admin/community/，而 Next 默认把带尾斜杠的地址 308 回无斜杠，
  // 两者对扯就是无限重定向（ERR_TOO_MANY_REDIRECTS）。这里认领带尾斜杠的规范形式，
  // 并关掉 Next 自己的归一化，让 /admin/community 与 /admin/community/ 都直接 200
  // （健康端点同理：/admin/community/api/health 与 .../health/ 都必须按契约字面路径返回 200，
  //  不能依赖探针跟随重定向）。
  trailingSlash: true,
  skipTrailingSlashRedirect: true,
  output: "standalone",
  // 页面壳是纯客户端应用：会话与权限码都来自 /api/auth/me，不在服务端持有身份。
  //
  // 这里**没有** rewrites：客户端的请求是根路径 /api/*（生产形态就是同源网关），
  // 而 basePath 会给 rewrite 的 source 加上前缀，写一条 "/api/:path*" 只会匹配到
  // /admin/community/api/*（那是本应用自己的路由空间），看着配了、实际不生效——
  // 本机单跑 next dev 时 /api/* 会 404，联调请在网关后面跑（见 README「本地开发」）。
};

export default nextConfig;
