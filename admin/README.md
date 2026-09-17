# community-admin（互动服务自带后台）

MetaFusion 互动服务（`metafusion-community`）自带的运营后台：**板块管理 / 主题治理 / 帖子治理**。

| 项 | 值 |
| --- | --- |
| 服务名 | `community-admin` |
| 网关路径 | `/admin/community`（Next.js `basePath`，网关矩阵里的前缀必须与它逐字一致） |
| 端口 | `3000` |
| 健康端点 | `/admin/community/api/health`（存活语义，见下） |
| 技术栈 | Next.js 14（App Router）· React 18 · TypeScript · Tailwind；与主仓库 `frontend/` 同栈 |

## 页面与权限码

权限码一律**以账号服务返回的 `permissions` 为准**（`GET /api/auth/me` 的 claims 投影），
判定口径与服务端 `internal/auth/permission.go` 的 `Can` 逐字一致（`*` 通配；令牌完全没有 `permissions` 时
只做历史角色兜底）。前端判定只决定**入口给不给**，真正的授权始终在服务端。

| 页签 | 需要 | 能力 |
| --- | --- | --- |
| 板块管理 | `community.board.manage` | 四语名称/描述、颜色、图标、排序、启用、进信息流（`PUT /api/community/boards/{code}`，补丁语义） |
| 主题治理 | `community.topic.pin` 或 `community.post.moderate` | 列表/搜索、置顶与取消（`PUT /api/community/topics/{id}/pin`）、删除（`DELETE`；作者本人删自己的不需要码） |
| 帖子治理 | `community.post.moderate` | 跨主题列回复（`GET /api/community/posts`）与删除；评论板块的短评列表与删除 |

## 接口事实（写代码前先读这一节）

- **板块 `PUT` 是补丁语义**：只提交改动过的字段（空载荷 400 `invalid_payload`）；
  `names`/`descriptions` 是**四语 map**，名称四语齐备且非空，描述允许四语全空 = 清空；
  缺语种返回 400 `four_locale_names_required: <语种清单>`（界面翻成"还缺哪些语种"）；
  `code` 是主题归属锚点不可改，服务端也不提供新建与删除板块。
- **帖子治理的回复来自 `GET /api/community/posts`**（本次随本后台一起加到服务端）：
  跨主题、`q` 匹配主题标题或回复正文、分页 `page`/`page_size`（缺省 20、上限 100，越界静默收敛），
  响应 `{"items":[…],"total":N}`；每项带 `topic_id`/`topic_title`/`board_code`/作者快照/楼层号/
  `excerpt`+`truncated`（200 字，按 rune 截断）/`created_at`/`updated_at`。
  **只读**：它不会像主题详情 `GET /api/community/topics/{id}` 那样把 `view_count` +1——
  这也是治理台不去拿主题详情当检索入口的原因。
- **短评走 `/api/community/feed`**：短评是 `community.topics` 里评论板块的行，不在 `community.posts` 里，
  所以治理列表端点覆盖不到它。feed 只有 `limit`（**没有 `total`/`offset`**），界面因此固定取最近一页。
- **分页两套写法各自单一来源**：主题列表用 `limit`/`offset`（服务端 `paging.go` 兼容期内的口径），
  治理列表与私信/收藏用 `page`/`page_size`——本应用的请求层按端点选择，不混用。
- **删除的两种授权**：作者本人删自己的内容不需要码；删他人的内容需要 `community.post.moderate`。
  主题删除会级联回复，确认框里写明了这一点。
- 错误码 → 文案的映射在 `src/lib/errors.ts`：已覆盖的码翻成人话（401/403/404/`invalid_payload`/
  `invalid_board`/`topic_locked`/`module_error`/缺语种/5xx/网络），**未覆盖的码连码带状态原样显示**，
  不伪装成已解释。

## 本地开发

```bash
npm ci
npm run dev        # http://127.0.0.1:3000/admin/community
```

客户端的接口请求一律是**根路径** `/api/*`——生产形态就是同源网关，这正是它该有的样子。
因此**本机单跑 `next dev` 只能看 UI**：没有网关时 `/api/*` 会 404（`basePath` 会给 rewrite 源加前缀，
配一条 rewrite 也转不到根路径，所以本应用不配 rewrite）。要联调接口就把整套跑在网关后面
（`deploy/docker-compose.yml` 的 dev 栈），或另想办法把网关挂到同源。

登录态与站点共用：先在站点登录（`mf_session` cookie 或 `localStorage.metafusion_token`），再打开本页。
未登录时页面只显示"去站点登录"，链接会带上一次性的 `?redirect=`（只对本应用路径、且只算一次，
避免登录页回跳套娃）。

### 验证

```bash
npx tsc --noEmit                 # 类型检查
npm test                         # Node 内置测试运行器跑 tests/*.test.ts（纯逻辑，无外部依赖）
npm run build                    # 生产构建（standalone）
```

测试覆盖：权限判定口径、四语校验与补丁差分、错误码翻译、登录回跳地址、四语字典键集合与占位符一致性。

### 路由矩阵（实测，standalone 产物）

按 `Dockerfile` 的 runner 布局补齐 `.next/static`/`public` 后起 `node .next/standalone/server.js`，
逐条打（不跟随重定向）：

| 路径 | 状态 |
| --- | --- |
| `/admin/community` | 200 |
| `/admin/community/` | 200 |
| `/admin/community/api/health` | 200（`{"status":"ok","service":"community-admin",…}`） |
| `/admin/community/api/health/` | 200 |
| `/admin/community/nope`（及带尾斜杠） | 404 |
| `/`、`/admin`、`/api/community/boards` | 404（本应用只认自己的前缀；`/api/*` 归网关） |
| `/admin/community/_next/static/…` | 200 |

`next dev` 起来后 `/admin/community`、`/admin/community` 与健康端点同样 200（UI 可用，接口仍需网关）。

> 注：本应用是 `output: "standalone"`，生产入口是 `node .next/standalone/server.js`（Dockerfile 的 CMD）；
> `next start` 在 standalone 产物上会给出不支持告警，别拿它当生产入口。

界面本身（浏览器里的点击流）尚未实测，见「未验证」。

## 容器

```bash
# 构建上下文是本仓库根目录，Dockerfile 在 admin/
docker build -f admin/Dockerfile -t community-admin:local .
docker run --rm -p 3000:3000 community-admin:local
curl -i http://127.0.0.1:3000/admin/community/api/health
```

`NEXT_PUBLIC_API_BASE` 是**构建期**参数（默认空 = 同源），运行期改它无效。

构建上下文是**仓库根**，所以生效的是仓库根的 `.dockerignore`（`admin/.dockerignore` 只是给
"以 admin/ 为上下文"的临时构建兜底）；它同时管住本仓库 Go 镜像的 `COPY . .`，
因此只排除 `.git`、`admin/node_modules`、`admin/.next` 这些产物与元数据。

## 宿主需要接入的东西（本仓库不改网关与编排）

1. **网关**：`location /admin/community/` → `http://community-admin:3000`；无尾斜杠的
   `/admin/community` 需 301 到带斜杠形式（Next.js `basePath` 只服务带前缀的路径）。
   注意 `location /admin/community/` 不能落到主站前端（`/` 兜底），否则页面 404。
2. **编排**：新增服务 `community-admin`（build context 指向本仓库、`dockerfile: admin/Dockerfile`、
   端口 3000、`depends_on: [community]`）。
3. **探针**：上游互动服务的就绪仍由网关的 `/health/community` 判断；本应用的存活端点是
   `/admin/community/api/health`（**只报本进程存活**，不聚合上游——上游抖动不该让本应用被判死）。

## 未验证

- 界面只在类型检查、单元测试与构建层面验证过，**没有人在浏览器里点过**。
- 未在本机起容器/网关：`docker build`、`location /admin/community/` 的真实转发、
  无尾斜杠 301 都由编排/网关代理实测。
