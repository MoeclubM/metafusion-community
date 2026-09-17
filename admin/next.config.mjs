/** @type {import('next').NextConfig} */
const basePath = "/admin/community";

// 网关按路径把它交给本应用（community-admin:3000），因此 basePath 必须与网关矩阵逐字一致：
// 改这里要同步 deploy/nginx.conf 与本文件顶部的常量，否则页面能起、入口 404。
const nextConfig = {
  reactStrictMode: true,
  basePath,
  output: "standalone",
  // 页面壳是纯客户端应用：会话与权限码都来自 /api/auth/me，不在服务端持有身份。
  // 开发时把 /api/* 转给网关（默认本机 8080），生产走同源网关，无需 rewrite。
  async rewrites() {
    return [
      {
        source: "/api/:path*",
        destination: `${process.env.API_ORIGIN || "http://127.0.0.1:8080"}/api/:path*`,
      },
    ];
  },
};

export default nextConfig;
