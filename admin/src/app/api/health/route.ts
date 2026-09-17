// 健康端点：在 basePath 之下，网关路径是 /admin/community/api/health。
//
// 语义是**存活**（进程能应答），不是"上游也就绪"：本应用是网关后面的静态后台，
// 上游（账号 / 互动）不可用时时它仍然应该被判定为活着——把上游健康混进来会让
// 一个上游抖动把所有探针带红，掩盖本应用自身的状态。上游就绪由网关的
// /health/auth、/health/community 逐条判断。
export const dynamic = "force-dynamic";

export function GET() {
  return Response.json({
    status: "ok",
    service: "community-admin",
    base_path: "/admin/community",
    time: new Date().toISOString(),
  });
}
