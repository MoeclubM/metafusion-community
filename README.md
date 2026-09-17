# MetaFusion Community

MetaFusion 社区互动服务：论坛（板块/主题/回复/标签）、条目短评与用户互动记录（收藏、评分、进度、持有）。

拆分基准见主仓库 [docs/architecture/service-split-migration.md](https://github.com/MoeclubM/MetaFusion/blob/main/docs/architecture/service-split-migration.md) 的 P2 阶段。

## 职责边界

- **拥有**：论坛板块/主题/回复/标签、条目短评（评论板块）、用户互动记录（`community.records`）、
  私信（`community.direct_messages`，见「私信」一节）。
- **不拥有**：账号与令牌（令牌只由账号服务签发，本服务只验签；存量不透明令牌也问账号服务）、
  实体元数据（问目录服务，不复制、不 JOIN）。
- **收藏**：`community.favorites`（表结构与原 `catalog.favorites` 逐列一致，便于一次性导入），
  前端 `/api/favorites/*`、`/api/users/{id}/favorites` 由本服务承载。**合并旧身份的收藏不再由目录改写**：
  读取时经 `/api/catalog/entities/{id}/resolve` 跟随重定向，因此收藏不会因实体合并而消失。
- **遗留**：收藏"是否公开"目前只有前端只读占位（`frontend/src/app/settings/page.tsx` 的开关是
  `disabled readOnly`，目录侧无对应字段），接口恒返回 `visible: true`；落地该开关时应由本服务承担。

## HTTP 契约

绝大多数路径与请求/响应形状与原单体 `modules` 包**逐字一致**，切流时前端零改动；已知例外只有一处，
见下面「语言维度只去接口层」。已切流（2026-09-14，开发实例）：
网关把 `/api/community/*`、`/api/favorites/*`、`/api/records/*`、`^/api/users/[^/]+/favorites$` 指到本服务。

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| GET | `/api/community/boards` | 匿名 | 板块列表 |
| GET | `/api/community/topics` | 匿名 | 主题列表：板块/标签/关键词筛选、置顶优先、分页 |
| GET | `/api/community/topic-tags` | 匿名 | 标签清单（`[{id,name}]`，供前端按 id 筛选） |
| GET | `/api/community/topics/{id}` | 匿名 | 主题详情（含回复、标签、锚定实体题名）；浏览量自增 |
| POST | `/api/community/topics` | `community.post.create` | 发主题（可锚定实体、可带标签） |
| POST | `/api/community/topics/{id}/posts` | `community.post.create` | 回帖（`post_number` 楼层、可引用楼号） |
| DELETE | `/api/community/topics/{id}` | 作者 / `community.post.moderate` | 删主题（级联回复） |
| DELETE | `/api/community/topics/{id}/posts/{postId}` | 作者 / `community.post.moderate` | 删回复 |
| GET | `/api/community/feed` | 匿名 | 站点级评论流（跨实体聚合，带条目标题；`q` 有界窗口过滤） |
| GET | `/api/community/entities/{id}/posts` | 匿名 | 某实体下的短评 |
| POST | `/api/community/entities/{id}/posts` | `community.post.create` | 发表短评 |
| GET | `/api/community/posts/{id}` | 匿名 | 单条短评（稳定 permalink） |
| DELETE | `/api/community/posts/{id}` | 作者 / `community.post.moderate` | 删短评（仅评论板块） |
| PUT | `/api/community/topics/{id}/pin` | `community.topic.pin` | 置顶 / 取消置顶（`{pinned: bool}`，写 `is_pinned`；评论板块的条目不可置顶） |
| PUT | `/api/community/boards/{code}` | `community.board.manage` | 板块配置：`names` / `descriptions`（**四语 map**，缺语种 400 `four_locale_names_required`）、`color`、`icon`、`sort_order`、`is_enabled`、`show_in_feed`；只改传入字段，`names` 不可为空，`code` 不可改，不提供新增与删除板块 |
| GET | `/api/community/entities/{id}/collections` | 匿名 | 关联的合集（经目录关系接口，不 JOIN 目录表） |
| POST | `/api/favorites/toggle` | 登录 | 切换收藏（目标必须是可见实体，且 kind 与 `target_type` 相符） |
| GET | `/api/favorites/status` | 匿名 | 批量查询收藏状态（未登录返回空集合） |
| GET | `/api/favorites/mine` | 登录 | 我的收藏（分页，目标按请求者可见性过滤） |
| GET | `/api/users/{id}/favorites` | 匿名 | 指定用户的收藏列表（公开读，目标按可见性过滤） |
| GET | `/api/users/{id}/stats` | 匿名 | 用户互动统计（主题 / 楼中回复 / 收藏），`{"stats":{…}}`；口径见「用户互动统计」 |
| GET | `/api/messages/with/{id}` | 登录 | 与某人的私信会话（`page`/`page_size`，缺省 20、上限 100，按时间**倒序**）；`{"items":[{id,sender_id,recipient_id,body,created_at}],"total":N}` |
| POST | `/api/messages/with/{id}` | 登录 | 发私信（`{"body":"…"}`；裁剪两侧空白后必须非空、不超过 4000 **字符**，否则 400 `invalid_body`；给自己发 400 `invalid_recipient`）→ `{"message":{…}}` |
| GET | `/api/records/entities/{id}` | 登录 | 本人的互动记录 |
| PUT | `/api/records/entities/{id}` | 登录 | 写入互动记录（评分/进度/持有） |

论坛主题与"实体短评"共用同一张 `community.topics`，靠板块区分语义：评论锚定实体、无独立标题、不进信息流；
主题有标题、可独立成文、进信息流（`show_in_feed`）。

### 私信（DM）

前端 `DirectMessageModal` 调的就是上面两条 `/api/messages/with/{id}`；此前四仓都没有实现，两个端点必然 404。

- **可见性是查询结构保证的**：会话由 `(当前用户, 对方)` 一对参与者决定，SQL 用
  `LEAST/GREATEST` 归一后等值匹配（与 `direct_messages_conversation` 索引表达式逐字一致），
  请求里也没有"会话 id"这种能指向别人会话的输入，所以第三者的私信查出来就是空页。
- **对方 id 只是外部引用**：账号数据归账号服务，本服务不查它的库、也不校验对方是否存在（只校验是不是 uuid）。
  代价是收件人被删除后这些私信仍在。
- **不能给自己发**：写接口 400 `invalid_recipient`，`CHECK(sender_id <> recipient_id)` 是同一口径的兜底；
  读自己的会话不报错，恒为空会话。
- **`read_at` 已预留、尚未启用**：两个端点都不读不写它，落地已读回执时由收信人读会话时置位。
- **分页窗口**：第一页是**最近**的 20 条（按 `created_at DESC, id DESC`），往后翻是更早的；
  `total` 是整段会话的条数，不随窗口变化。

### 用户互动统计

用户主页要的三个数字由本服务承载（`GET /api/users/{id}/stats`，匿名可读，返回 `{"stats":{…}}`）——
它们是**给人看的统计**，因此口径写进代码注释（`internal/store.StatsFor`），并在这里同步一份：

| 字段 | 数的是什么 | 过滤条件 |
| --- | --- | --- |
| `topics_created` | 论坛主题 | `community.topics` 里 `author_id` 本人、且 `board_code <> 'comment'` 的行（评论板块的行是实体短评，不是主题，主题列表同样排除它） |
| `comments_created` | 楼中回复（前端标签"互动回复"） | `community.posts` 里 `author_id` 本人的行 |
| `favorites_count` | 公开可见的收藏 | `community.favorites` 里 `user_id` 本人的行 |

两处取舍：

- **短评（评论板块）不计入任何一个数字**：它既不是主题，也不在 `community.posts` 里；
  宁可少算，也不让同一行在两个数字里各出现一次。
- **"公开可见"就是全部收藏行**：`community` 侧没有收藏公开标记、也没有用户设置表（"收藏是否公开"
  目前只是前端只读占位），因此这里与 `GET /api/users/{id}/favorites` 的 `total` 完全同口径
  （目标实体自身的可见性由读取方逐条过滤，不影响计数）；真库用例直接断言两者一致。
- 不存在的用户与"没有互动记录的用户"都返回 0：账号数据不归本服务，这里不查账号库（只看 uuid 字面量）。

**网关还没跟上**：现有的分流规则只覆盖 `/api/community/*`、`/api/favorites/*`、`/api/records/*`
与 `^/api/users/[^/]+/favorites$`；新增的 `/api/messages/*` 与 `^/api/users/[^/]+/stats$`
需要加到本服务（网关规则在主仓库的部署配置里，不在本仓库范围内），
加之前这两组路径会打到旧入口并 404。

**语言维度只去接口层，不去字段**（用户决议 2026-09-17）：

- 主题与回复的接口没有语言维度：`GET /api/community/topics` 不读 `?language=`、发帖/改帖请求体没有 `language`、
  SELECT 列清单里也没有它（老前端传了只被忽略，不报错）；
- 板块名与描述是**多语言 map**：列表与管理接口收发 `names` / `descriptions`（`{"zh-CN":…,"zh-TW":…,"ja-JP":…,"en-US":…}`），
  服务端不做单语解析，前端按显示语言取键、缺键走自己的回退链；
- 管理接口收 `names` / `descriptions` 时要求**四语齐备**，缺语种返回 400 `four_locale_names_required: <缺的语种>`
  （与目录侧 definitions / shelves / external_databases 同一标识，前端复用同一套错误文案）；描述允许四语全传空串来清空；
- 库里的列全部保留，见下一节。

## 权限

写权限以**账号服务下发的权限码**为准（访问令牌 claims 与 `/api/auth/me` 的 `permissions`，admin 组带 `*` 通配），
判定集中在一处：`internal/auth/permission.go` 的 `Can`。组码与角色不参与判定——散落的角色比较正是
「后台给用户分配了子系统权限组、本服务却不认」的成因。

| 权限码 | 本服务用在哪 |
| --- | --- |
| `community.post.create` | 发主题、回帖、短评三处写接口的闸门（member 组默认持有） |
| `community.post.moderate` | 删除**他人**的主题、回复与短评（作者删自己的内容不需要任何码） |
| `community.topic.pin` | 置顶 / 取消置顶主题（`PUT /api/community/topics/{id}/pin`） |
| `community.board.manage` | 板块配置（`PUT /api/community/boards/{code}`） |

**兼容策略**：令牌**完全没有** `permissions` 声明时（老令牌，或尚未按权限组配置的实例）按历史边界兜底：
发帖类码（`community.post.create`）放行——收口前发帖只要求登录，账号服务尚未升级的实例不能因为收口而变成
"除了管理员谁都不能发帖"；治理类码（moderate / pin / board.manage）只认 `role=admin`，与改造前一致。
令牌一旦带 `permissions` 就只认码，角色不再额外放行，避免「角色兜底」变成绕过权限组的后门。

## 数据与迁移

**结构由版本化迁移管理**：`migrations/*.up.sql` 是 `community` schema 的唯一结构来源，服务启动时按版本应用
（已应用则跳过，登记在 `community.schema_migrations`），重复启动不会改动已存在的表；
下面的一次性工具只搬数据、不管结构。已应用的迁移文件按 checksum 校验，**不能改**：
结构要变就新增版本，改了已应用的文件会让服务拒绝启动。

### 语言字段的现状与取舍

版本序列：`000001` 基线（多语言 JSONB + `topics.language`）→ `000002` 板块名收敛为单值列 →
`000003` 主题 language 列退役 → `000004` **前向恢复**多语言列（接口层不引入语言维度）。

| 列 | 状态 | 谁在用 |
| --- | --- | --- |
| `community.boards.names` / `descriptions`（jsonb） | 保留，**权威** | 板块列表与管理接口只收发它 |
| `community.boards.name` / `description`（text） | 保留，**兼容/回退** | 由 `names`/`descriptions` 的 zh-CN 派生；不再接受写入 |
| `community.topics.language`（text） | 保留，恒为空串 | 无：接口不 SELECT、不接受、不返回 |

两处取舍与理由：

- **单值列不 DROP**。000002 已经把它做成了权威字段，000004 之后管理接口改为只写多语言 map，
  单值列退化为"容量层的兼容/回退列"——派生值口径固定（zh-CN 优先），读方（老前端、排查用的 SQL）
  仍能从一个平列取到板块名。DROP 掉反而要让所有只认单值的读方改用 `names ->> 'zh-CN'`，
  收益只是少一列，不划算；`000004` 的回填方向也刻意定成"单值 → 多语言"，让这两列互为一致性校验。
- **`topics.language` 的历史值不恢复**：000003 已经把列连同数据一起 DROP，旧单体的
  `modules.forum_topics.language` 也不保证与切流后的写入同步。000004 只把列加回来并保持空串，
  它不参与任何读取——加回来是为了满足"列必须保留"的库结构口径，不是要让语言维度回到接口里。
- **板块管理接口的四语校验只认 map**：载荷里带单值 `name` 会被当作空载荷拒绝（gin 忽略未声明字段）。
  给单值开一条写入口等于在接口层把语言维度装回来，还会让"这次到底改了哪个语种"说不清；
  前端本来就按 `DynamicNamesEditor` 那类四语编辑器提交 map。

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

工具只对**尚未 retire 的实例**有意义：开发实例已于 2026-09-14 执行
`deploy/sql/retire-legacy-schemas.sql`（`modules` schema 与 `catalog.favorites` 已删除），
在那台实例上两个方向都会因源表/目标表不存在而失败，回滚只剩"改网关上游 + 上一版镜像重建"。

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
go vet ./...
# 带真库的用例共用一个测试库（COMMUNITY_TEST_DSN），包与包之间并行执行会互相清表：
# 与 CI 一致用 -p 1 串行跑，否则会出现偶发的外键失败。
COMMUNITY_TEST_DSN='postgres://user:pass@127.0.0.1:5432/metafusion_community_test?sslmode=disable' go test -p 1 ./...
```

## 迁移状态

- 主仓库仍提供 `/api/community/*` 与 `/api/records/*`（当前线上流量入口），本服务为切流目标；
  两者共用同一份表结构的复制体，切流前靠 `cmd/migrate` 同步，切流后旧实现随 `modules` 包下线。
- 实体合并（`entity.merged`）后的引用改写：旧实现由单体订阅 outbox 完成；本服务的增量消费
  在 P4 与跨服务事件通道一起确定，当前不消费事件。
