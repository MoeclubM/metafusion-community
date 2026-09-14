# MetaFusion Community

MetaFusion 社区互动服务：论坛（板块/主题/回复/标签）、条目短评与用户互动记录（收藏、评分、进度、持有）。

拆分基准见主仓库 [docs/architecture/service-split-migration.md](https://github.com/MoeclubM/MetaFusion/blob/main/docs/architecture/service-split-migration.md) 的 P2 阶段。

## 职责边界

- **拥有**：论坛板块/主题/回复/标签、条目短评（评论板块）、用户互动记录（`community.records`）。
- **不拥有**：账号与令牌（令牌只由账号服务签发，本服务只验签；存量不透明令牌也问账号服务）、
  实体元数据（问目录服务，不复制、不 JOIN）。
- **收藏**：`community.favorites`（表结构与原 `catalog.favorites` 逐列一致，便于一次性导入），
  前端 `/api/favorites/*`、`/api/users/{id}/favorites` 由本服务承载。**合并旧身份的收藏不再由目录改写**：
  读取时经 `/api/catalog/entities/{id}/resolve` 跟随重定向，因此收藏不会因实体合并而消失。
- **遗留**：收藏"是否公开"目前只有前端只读占位（`frontend/src/app/settings/page.tsx` 的开关是
  `disabled readOnly`，目录侧无对应字段），接口恒返回 `visible: true`；落地该开关时应由本服务承担。

## HTTP 契约

路径与请求/响应形状与原单体 `modules` 包**逐字一致**，切流时前端零改动。已切流（2026-09-14，开发实例）：
网关把 `/api/community/*`、`/api/favorites/*`、`/api/records/*`、`^/api/users/[^/]+/favorites$` 指到本服务。

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| GET | `/api/community/boards` | 匿名 | 板块列表（后台可增删改） |
| GET | `/api/community/topics` | 匿名 | 主题列表：板块/标签/语言/关键词筛选、置顶优先、分页 |
| GET | `/api/community/topic-tags` | 匿名 | 标签清单（`[{id,name}]`，供前端按 id 筛选） |
| GET | `/api/community/topics/{id}` | 匿名 | 主题详情（含回复、标签、锚定实体题名）；浏览量自增 |
| POST | `/api/community/topics` | 登录 | 发主题（可锚定实体、可带标签） |
| POST | `/api/community/topics/{id}/posts` | 登录 | 回帖（`post_number` 楼层、可引用楼号） |
| DELETE | `/api/community/topics/{id}` | 作者/管理员 | 删主题（级联回复） |
| DELETE | `/api/community/topics/{id}/posts/{postId}` | 作者/管理员 | 删回复 |
| GET | `/api/community/feed` | 匿名 | 站点级评论流（跨实体聚合，带条目标题；`q` 有界窗口过滤） |
| GET | `/api/community/entities/{id}/posts` | 匿名 | 某实体下的短评 |
| POST | `/api/community/entities/{id}/posts` | 登录 | 发表短评 |
| GET | `/api/community/posts/{id}` | 匿名 | 单条短评（稳定 permalink） |
| DELETE | `/api/community/posts/{id}` | 作者/管理员 | 删短评（仅评论板块） |
| GET | `/api/community/entities/{id}/collections` | 匿名 | 关联的合集（经目录关系接口，不 JOIN 目录表） |
| POST | `/api/favorites/toggle` | 登录 | 切换收藏（目标必须是可见实体，且 kind 与 `target_type` 相符） |
| GET | `/api/favorites/status` | 匿名 | 批量查询收藏状态（未登录返回空集合） |
| GET | `/api/favorites/mine` | 登录 | 我的收藏（分页，目标按请求者可见性过滤） |
| GET | `/api/users/{id}/favorites` | 匿名 | 指定用户的收藏列表（公开读，目标按可见性过滤） |
| GET | `/api/records/entities/{id}` | 登录 | 本人的互动记录 |
| PUT | `/api/records/entities/{id}` | 登录 | 写入互动记录（评分/进度/持有） |

论坛主题与"实体短评"共用同一张 `community.topics`，靠板块区分语义：评论锚定实体、无独立标题、不进信息流；
主题有标题、可独立成文、进信息流（`show_in_feed`）。

## 数据与迁移

本服务拥有 `community` schema，表结构与主仓库 `modules` 包中的 `forum_*` / `records` **逐列一致**，
因此切流前可用附带的一次性导入工具搬运数据，不需要字段映射：

```bash
# 切换前：先看规模（不写入），再搬运
go run cmd/migrate -direction forward -dry-run
go run cmd/migrate -direction forward

# 回滚时：先把服务期间写入的行搬回单体，再改网关指回单体
go run cmd/migrate -direction back -dry-run
go run cmd/migrate -direction back
```

两个方向都存在，切流才是真的可回滚：**先搬数据再改网关**，否则回滚窗口内的新帖在单体侧会"消失"。
完整步骤与逐步验证见主仓库 [docs/architecture/cutover-runbook.md](https://github.com/MoeclubM/MetaFusion/blob/main/docs/architecture/cutover-runbook.md)。

- 幂等：全部 `ON CONFLICT DO NOTHING`，失败重跑安全；
- **只读旧表**：不删除、不修改 `modules.*`，因此切流前随时可以取消，回滚只需把网关指回单体；
- 顺序 `boards → topics → posts → tags → topic_tags → records → favorites`，满足外键依赖；
  唯一跨 schema 的步骤是收藏（源表在主仓库的 `catalog.favorites`）；
- 迁移窗口：切流前单体仍在写入，因此**切流时再跑一次**补齐增量。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `PORT` | `8083` | 监听端口 |
| `DATABASE_URL` | 由 `DB_*` 拼装 | PostgreSQL 连接串（本服务只使用 `community` schema） |
| `COMMUNITY_JWKS_URL` | `http://auth:8081/api/oidc/jwks` | 验签公钥来源：账号服务是唯一签发方 |
| `AUTH_URL` | 空 | 账号服务地址，仅用于存量不透明会话令牌的兜底解析（`GET /api/auth/me`）；留空即"只接受 JWT" |
| `AUTH_JWT_PUBLIC_KEY` | 空 | 静态公钥（PEM 或 base64 PEM）；设置后不再请求 JWKS |
| `AUTH_JWT_ISSUER` / `AUTH_JWT_AUDIENCE` | `https://findverse.cc/api` / `metafusion` | 与主仓库一致，避免存量令牌失效 |
| `CATALOG_URL` | `http://backend:8080` | 目录服务地址（可见性、标题、关系邻居） |
| `COMMUNITY_CATALOG_TIMEOUT_MS` | `5000` | 单次目录调用超时 |

## 运行

```bash
go run cmd/server/main.go
go test ./... && go vet ./...
```

## 迁移状态

- 主仓库仍提供 `/api/community/*` 与 `/api/records/*`（当前线上流量入口），本服务为切流目标；
  两者共用同一份表结构的复制体，切流前靠 `cmd/migrate` 同步，切流后旧实现随 `modules` 包下线。
- 实体合并（`entity.merged`）后的引用改写：旧实现由单体订阅 outbox 完成；本服务的增量消费
  在 P4 与跨服务事件通道一起确定，当前不消费事件。
