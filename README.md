# MetaFusion Community

MetaFusion 社区互动服务：论坛（板块/主题/回复/标签）、条目短评、收藏与私信。

拆分基准见主仓库 [docs/architecture/service-split-migration.md](https://github.com/MoeclubM/MetaFusion/blob/main/docs/architecture/service-split-migration.md) 的 P2 阶段。

## 职责边界

- **拥有**：论坛板块/主题/回复/标签、条目短评（评论板块）、
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
网关把 `/api/community/*`、`/api/favorites/*`、`^/api/users/[^/]+/favorites$` 指到本服务。

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| GET | `/api/community/boards` | 匿名 | 板块列表（只含 `is_enabled=true`；持 `community.board.manage` 时含停用板块，见「板块」） |
| GET | `/api/community/topics` | 匿名 | 主题列表：板块/标签/关键词筛选、置顶优先、分页；停用板块的主题不在其中 |
| GET | `/api/community/topic-tags` | 匿名 | 标签清单（`[{id,name}]`，供前端按 id 筛选） |
| GET | `/api/community/topics/{id}` | 匿名 | 主题详情（含回复、标签、锚定实体题名）；浏览量自增；停用板块的主题按不存在处理（404 `not_found`，且不自增） |
| POST | `/api/community/topics` | `community.post.create` | 发主题（可锚定实体、可带标签） |
| POST | `/api/community/topics/{id}/posts` | `community.post.create` | 回帖（`post_number` 楼层、可引用楼号） |
| DELETE | `/api/community/topics/{id}` | 作者 / `community.post.moderate` | 删主题（级联回复） |
| DELETE | `/api/community/topics/{id}/posts/{postId}` | 作者 / `community.post.moderate` | 删回复 |
| GET | `/api/community/posts` | `community.post.moderate` | 帖子治理列表：跨主题列楼中回复，`q` 匹配主题标题或回复正文，`page`/`page_size`（缺省 20、上限 100）→ `{"items":[…],"total":N}`；**只读，不改 `view_count`**（见「帖子治理列表」） |
| GET | `/api/community/feed` | 匿名 | 站点级评论流（跨实体聚合，带条目标题；`q` 有界窗口过滤；停用的评论板块不出现） |
| GET | `/api/community/entities/{id}/posts` | 匿名 | 某实体下的短评（停用的评论板块返回空列表） |
| POST | `/api/community/entities/{id}/posts` | `community.post.create` | 发表短评 |
| GET | `/api/community/posts/{id}` | 匿名 | 单条短评（稳定 permalink）；停用的评论板块 404 |
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
| POST | `/api/messages/with/{id}` | 登录 | 发私信（`{"body":"…"}`；裁剪两侧空白后必须非空、不超过 4000 **字符**，否则 400 `invalid_body`；给自己发 400 `invalid_recipient`；超过发送频率 429 `rate_limited`）→ `{"message":{…}}` |
| GET | `/api/messages/conversations` | 登录 | 收件箱会话列表（按对方分组，`page`/`page_size` 与其它列表同口径，按最近一条**倒序**）；`{"items":[{peer_id,last_message,unread_count}],"total":N}`；口径见「私信（DM）」 |
| GET | `/api/messages/unread` | 登录 | 我的未读总数（导航栏角标）；`{"unread_count":N}`，与收件箱每行的 `unread_count` 同口径 |
| PUT | `/api/messages/with/{id}/read` | 登录 | 标记"对方发给我"的未读为已读 → `{"marked":N}`（幂等：重复调用为 0，且不刷新回执时间） |

论坛主题与"实体短评"共用同一张 `community.topics`，靠板块区分语义：评论锚定实体、无独立标题、不进信息流；
主题有标题、可独立成文、进信息流（`show_in_feed`）。

### 跨服务依赖不可用（目录 / 账号）

读路径要先问目录（实体可见性、标题、关系邻居）与账号（会话兜底、PAT 内省）。这两类出站调用走
`internal/upstream`（超时分层 + 有界重试 + 熔断），失败**不再折成空结果**：

- 目录侧取不到（超时 / 连接失败 / 5xx / 429 / 熔断打开）→ `503` + `{"error":"upstream_unavailable"}`。
  受影响的是所有需要目录才能成形响应的端点：`GET /api/community/feed`（评论流）、主题列表与详情
  （`entity_title`）、`GET /api/community/posts/{id}`、`GET /api/community/entities/{id}/collections`、
  `POST /api/favorites/toggle`、`GET /api/favorites/mine`、`GET /api/users/{id}/favorites`。
  目录**明确回答**"不可见/不存在"（404）时口径不变：仍是 404 `not_found`，或跳过该条目标。
  唯一例外：`PUT /api/community/topics/{id}/pin` 已经落库，取不到题名只影响装饰字段，仍回 200
  （把一次已生效的写回成 503 会让调用方重试一次已经发生的写操作）。
- 账号侧沿用既有机器码（见「权限」）：无效/吊销仍是 401 `invalid_token`，账号服务不可达是
  503 `auth_unavailable`；会话兜底失败仍按匿名继续，只有 PAT 内省才回 503。
- 策略口径：目录 3 次尝试 / 单次 2s / 总预算 7s / 连续 5 次失败熔断 10s；账号 2 次尝试 / 单次 1.5s /
  总预算 4s / 熔断 10s。调用方自己取消（关页面）不算上游故障，也不进熔断计数。
- `GET /ready` 仍是毫秒级浅探针（只探 PG）；`GET /ready?deep=1` 并发探 `CATALOG_URL/ready` 与
  `AUTH_URL/ready`（总预算 3s），响应带 `upstreams`，任一上游不可用时回 503 + `status:"degraded"`
  （未配置地址记 `not_configured`，那是部署态，不算故障）。

### 板块：`is_enabled`（停用）与 `show_in_feed`（信息流）

两个开关分工不同，不能互相替代：

- **`is_enabled=false` 是"整个板块停用"**：公开读路径一律不再出现该板块及其内容 ——
  `GET /api/community/boards`（只列已启用板块）、按板块的主题列表 `GET /api/community/topics`
  （显式 `board_code=<停用板块>` 返回 200 + 空 `items`/`total=0`，**不是 404**：调用方给的是合法筛选，
  事实就是"没有内容"）、主题详情 `GET /api/community/topics/{id}`（按"不存在"处理，404 `not_found`，
  且不自增 `view_count`），以及以 `board_code` 限定的评论读路径（`/api/community/feed`、
  `/api/community/entities/{id}/posts`、`/api/community/posts/{id}`）。
  向停用板块**发新主题仍被拒**（400 `invalid_board`，与改造前同一判据），权限语义不变。
- **`show_in_feed` 只管"已启用板块的主题要不要进站点信息流"**：它由前端消费（主站信息流按它过滤），
  服务端既不解释它、也不因它过滤任何读路径。板块停用不等于退订信息流，两者各管一段。
- **运营例外（刻意）**：持 `community.board.manage` 的调用者不受上述读过滤影响，仍能看到停用板块及其内容。
  原因很具体：社区管理台与公开前端读的是**同一个** `GET /api/community/boards`，对运营也过滤会让管理台
  看不到被停用的板块，也就没有把它切回来的入口——停用会变成单向操作。
  刻意**不用查询参数**（如 `?include_disabled=1`）表达这一点：任何忘记带参数的运营客户端都会静默
  "少看到板块"，而"少一块的列表"看起来仍然正常；授权例外只挂在权限码上，至少还能被权限审计发现。
  运营类端点（板块配置、帖子治理列表）本来就按权限码开放，不受该过滤约束。

实现上，读路径共用 `internal/handler/forum.go` 的 `enabledBoardGuard(alias)` 生成
`NOT EXISTS (... b.code=<alias>.board_code AND NOT b.is_enabled)` 谓词，可见性判据共用 `Handler.seesDisabledBoards`；
写路径与权限判定一概不动。真库用例见 `internal/handler/board_disabled_postgres_test.go`。

### 私信（DM）

前端 `DirectMessageModal`（用户主页弹窗）与主站 `/messages` 收件箱页读的是同一批端点：
单会话 `/api/messages/with/{id}`、收件箱 `/api/messages/conversations`、未读 `/api/messages/unread`、
标记已读 `PUT /api/messages/with/{id}/read`。

- **可见性是查询结构保证的**：会话由 `(当前用户, 对方)` 一对参与者决定，SQL 用
  `LEAST/GREATEST` 归一后等值匹配（与 `direct_messages_conversation` 索引表达式逐字一致），
  请求里也没有"会话 id"这种能指向别人会话的输入，所以第三者的私信查出来就是空页。
- **对方 id 只是外部引用**：账号数据归账号服务，本服务不查它的库、也不校验对方是否存在（只校验是不是 uuid）。
  代价是收件人被删除后这些私信仍在。
- **不能给自己发**：写接口 400 `invalid_recipient`，`CHECK(sender_id <> recipient_id)` 是同一口径的兜底；
  读自己的会话不报错，恒为空会话。
- **已读回执是收信人的动作、不是读接口的副作用**：`read_at`（000005 预留的列）由
  `PUT /api/messages/with/{id}/read` 置位，`GET` 会话**不**顺手写它——读接口带写副作用会让缓存、
  重试与审计都说不清（一个 GET 既读又写还回写 `X-Request-Id`），而这个动作有它自己的动作码 `message.read`。
  幂等：`UPDATE … WHERE read_at IS NULL`，重复调用第二次影响 0 行、也不刷新回执时间。
  发送方**看不到**回执（对外形状里没有"对方读没读"），本批次只在收件箱侧以 `unread_count` 表达。
- **会话列表不含对方用户名**：账号资料归账号服务，本服务不查它的库；让每行都做出站调用去补用户名，
  等于把收件箱变成"账号服务可用才可用"，而且是 N 次出站。列表只给 `peer_id`，调用方按自己的
  缓存/并发策略去 `GET /api/users/{id}` 取（主站收件箱页按页有界并发取一次并缓存）。
- **未读数只有一套口径**：`recipient_id = 我 AND read_at IS NULL`（000009 的索引服务它），
  收件箱每行的 `unread_count` 与 `GET /api/messages/unread` 都是它；自己发出的消息从不计入。
- **分页窗口**：单会话第一页是**最近**的 20 条（按 `created_at DESC, id DESC`），往后翻是更早的，
  `total` 是整段会话的条数、不随窗口变化；收件箱同样倒序（按最近一条），`total` 是"我参与了多少段会话"，
  **页码越界时该页为空但 `total` 仍正确**（前端靠它判断还有没有下一页）。
- **收件箱列表是两条查询，不是 N+1**：000009 的两条索引分别服务"我收到的按对方取最近一条"与
  "我发出的按对方取最近一条"，两个分支都是索引倒序扫描（`DISTINCT ON` 的排序键与索引列顺序逐字对齐），
  未读由一次 `GROUP BY sender_id` 出全部会话；真库用例在 `enable_seqscan=off` 下断言命中的正是这两条索引。
- **反骚扰只做到"发送频率"这一层**：网关按 IP 限流（30r/s）拦不住"一个账号刷量"，因此发信另有一条
  **按账号**的令牌桶（容量 20、每分钟回满，见 `internal/handler/message_limit.go`），超限回 429 `rate_limited`
  + `Retry-After`。已知边界：桶在进程内存里（多副本时额度按副本数放大，要跨副本一致得上共享存储）；
  只限"发多快"、不限"发给谁"。**拉黑 / 举报 / 静默期仍不存在**（留给 F3）——本服务目前没有任何
  收件人侧设置，也没有可以阻断投递的名单。

### 帖子治理列表

`GET /api/community/posts` 是运营后台（本仓库 `admin/`，网关路径 `/admin/community`）用来**跨主题巡检回复**的端点。
此前回复只有"删"的入口（`DELETE /api/community/topics/{id}/posts/{postId}`），要处置一条回复得先知道它在哪个主题；
而唯一能列出某主题楼层的是主题详情 `GET /api/community/topics/{id}` —— 它会顺手把 `view_count` +1，
拿它当检索入口等于每次排查都在篡改统计。因此本端点只读，并一次带上治理所需上下文。

- **闸门**：`community.post.moderate`（与处置内容的其它入口同一码），匿名 401 `authentication_required`、缺码 403 `forbidden`。
- **分页**：`page`/`page_size` 写法（缺省 20、上限 100，越界静默收敛），与 `/api/messages/*`、`/api/favorites/*` 同口径；
  响应形状同样是 `{"items":[…],"total":N}`。`/api/community/topics` 的 `limit`/`offset` 属兼容期内的另一套口径
  （见 `internal/handler/paging.go` 的说明），本次不动它，也没有在本端点上另开口子。
- **关键词**：`q` 走 `ILIKE` 子串匹配主题标题或回复正文，与主题列表的搜索同口径。
  **不引入 `to_tsvector`**：默认分词配置对中文按词切分的假设不成立（中文没有空格边界），
  全文索引只会把"搜不到"变成"看起来支持却搜不到"。与 topics 列表一样，`q` 里的 `%`/`_` 会被当通配符。
- **每一项带**：`id`、`topic_id`、`topic_title`、`board_code`、`author_id`/`author_name`（写入时的快照，
  空快照回落 `Anonymous`）、`post_number`、`reply_to_post_number`、`excerpt` + `truncated`
  （摘要按 **rune** 截断到 200 字，不把整篇长文搬进列表）、`created_at`/`updated_at`。
- **与 `/community/feed` 的分工**：feed 读的是**评论板块的短评**（存在 `community.topics` 里的行，带条目标题），
  本端点读的是**楼中回复**（`community.posts`）。两者不是同一张表——治理台的两块列表分别对应它们，不互相替代。
- **不碰 `view_count`** 是真库用例的断言项之一（`internal/handler/posts_postgres_test.go`）。

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
- **停用板块不影响统计**：`topics_created` 数的是"我发过多少主题"这一事实，不是公开读路径，
  因此不过滤 `is_enabled`（与"停用板块的存量主题仍归作者所有"同一口径）。
- 不存在的用户与"没有互动记录的用户"都返回 0：账号数据不归本服务，这里不查账号库（只看 uuid 字面量）。

**网关分流已就位**（主仓库 `deploy/nginx.conf`，提交 `7ab2f97`）：`/api/community/*`、`/api/favorites/*`、
`^/api/users/[^/]+/favorites$`、`^/api/users/[^/]+/stats$` 与 `/api/messages/` 全部分流到本服务
（`community:8083`）；`/api/users/:id` 归账号服务、`/api/users/:id/contributions` 落目录服务兜底。
主仓库的 `scripts/check_gateway_matrix.py` 会强制矩阵与契约表对齐，漏一条会红，所以这里不再需要人工提醒。

**语言维度只去接口层，不去字段**（用户决议 2026-09-17）：

- 主题与回复的接口没有语言维度：`GET /api/community/topics` 不读 `?language=`、发帖/改帖请求体没有 `language`、
  SELECT 列清单里也没有它（老前端传了只被忽略，不报错）；
- 板块名与描述是**多语言 map**：列表与管理接口收发 `names` / `descriptions`（`{"zh-CN":…,"zh-TW":…,"ja-JP":…,"en-US":…}`），
  服务端不做单语解析，前端按显示语言取键、缺键走自己的回退链；
- 管理接口收 `names` / `descriptions` 时要求**四语齐备**，缺语种返回 400 `four_locale_names_required: <缺的语种>`
  （与目录侧 definitions / shelves / external_databases 同一标识，前端复用同一套错误文案）；描述允许四语全传空串来清空；
- 库里的列全部保留，见下一节。

## 审计留痕（写入侧）

本服务的**全部 11 条写路由**都会往跨服务共用的 `audit.audit_log` 写一行审计（谁、什么时候、
对什么、做了什么、结果如何）。契约是主仓库 [docs/architecture/audit-log.md](https://github.com/MoeclubM/MetaFusion/blob/main/docs/architecture/audit-log.md)：
表结构、动作码命名、写入语义、脱敏规则四个服务共用（各仓各存一份同源代码，没有共享 module）。

| 方法 | 路由 | 动作码 | 被动对象（`target_type` = `target_id`） |
| --- | --- | --- | --- |
| POST | `/api/community/topics` | `topic.created` | `topic` = 新主题 id |
| POST | `/api/community/topics/{id}/posts` | `post.created` | `post` = 新回复 id |
| POST | `/api/community/entities/{id}/posts` | `comment.created` | `comment` = 新短评 id |
| PUT | `/api/community/topics/{id}/pin` | `topic.pinned` | `topic` |
| PUT | `/api/community/boards/{code}` | `board.updated` | `board` = code |
| DELETE | `/api/community/topics/{id}` | `topic.deleted` | `topic` |
| DELETE | `/api/community/topics/{id}/posts/{postId}` | `post.deleted` | `post` |
| DELETE | `/api/community/posts/{id}` | `comment.deleted` | `comment` |
| POST | `/api/favorites/toggle` | `favorite.toggled` | `entity` = 被收藏的实体 |
| POST | `/api/messages/with/{id}` | `message.sent` | `user` = 收件人 |
| PUT | `/api/messages/with/{id}/read` | `message.read` | `user` = 会话另一端 |

三条不变式（实现 `internal/audit`，注册表与接线 `internal/handler/audit.go`）：

- **审计是旁路，不参与业务事务**：非阻塞入队 + 单个后台 goroutine 落库；队列满或落库失败只记
  error 日志并丢这一行，业务不回滚、响应不等待（丢行有 `Recorder.Dropped()` 计数）。
- **写入前统一脱敏**：键名两级黑名单（`password`/`token`/`secret`/`hash` 等）→ `[redacted]`；
  值里的邮箱遮罩成 `j***@example.com`；单值 512 字符、`changes` 序列化后 8KB 截断。
  **正文、私信内容与请求体原文一概不进审计**：摘要只放身份级字段（板块、标题、锚点、楼层、
  变更前后值），要看内容去业务表。
- **只记登记过的写路由**：新增写端点必须在注册表里登记动作码，否则「写路由覆盖守卫」
  （`internal/handler/audit_coverage_test.go`）失败——漏一条不会有任何其它用例报出来。

四处需要知道的口径：

- **本服务没有的写能力**：板块只有"改已有板块"（新增与删除由种子与后台完成，无端点）；
  没有封禁端点（封禁归账号服务）；私信的已读回执**已经有了**（`message.read`），
  但拉黑 / 举报仍没有（留给 F3）。这些在任务清单里点名核过，都是"端点不存在"，不是漏接线。
- **收件箱的两条读接口是纯读**（`GET /api/messages/conversations`、`GET /api/messages/unread`）：
  不置位 `read_at`，因此按契约 §7「审计只记写操作」不进注册表。
- **GET 不记**：`GET /api/community/topics/{id}` 会自增 `view_count`（读接口的副作用），
  契约 §7 明确"审计只记写操作"，因此它不在注册表里；这条读接口也不回写 `X-Request-Id`。
- **`credential_type` 是近似值**：本服务只验签与内省，分不清会话令牌与 OAuth 令牌，
  只能给 `pat`（`Principal.FromPAT`）或 `session`（契约 §7 已记录）。
- **凭据被拒的写请求也留痕**：审计中间件挂在身份中间件**之前**——身份中间件会对被拒的 PAT
  （`401 invalid_token` / `503 auth_unavailable`）直接 abort 掉请求，挂在它之后这类写请求就一行审计
  都没有，而"凭据被拒"恰恰是最该留痕的一类。被拒的行记 `credential_type=anonymous` +
  `error_code=http_<status>`（响应里就是这个码，是事实）；缺 Authorization 或无效 JWT 不 abort，
  仍按匿名进路由闸门，那类行记闸门登记的稳定码（`authentication_required` / `forbidden`）。
  顺序不能反——真库用例 `TestAuditLogForRejectedCredentialsAgainstPostgres` 会红。

读取面**不在本服务**：唯一的读取端点是账号服务的 `GET /api/admin/audit-logs`（权限码
`auth.audit.read`），可按 `service=community`、`action`、`actor`、`target_type`/`target_id` 过滤。
审计行与请求日志的关联键是 `X-Request-Id`：写路由缺省生成 uuid 并回写同名响应头（调用方带了就
原样透传），同一个 id 也写进审计行的 `request_id`。表由迁移 `000007_audit_log.up.sql` 建在
`audit` schema（跨服务共用，不属于 `community`）。

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

**个人访问令牌（PAT）**：`Authorization: Bearer mfp_…`（`mfp_` + 43 位 base62）由账号服务的
`POST /api/auth/tokens/introspect` 判定，本服务**不读账号库、不签发、不落盘凭据**。内省拿到的身份与 JWT 同形
（id / username / role / permissions），但**权限一律按 permissions 里的码判定**：即使权限集合为空也绝不回落到
角色兜底或历史的"登录即可"边界——PAT 的权限就是账号服务算好的"用户自身权限 ∩ scopes"，否则 `scopes=[]`
的管理员令牌会变成全权令牌（创建端点已禁止空 scopes，这是第二道防线）。

- 内省结果按明文 sha256 **进程内缓存 60 秒**（同键并发只打一次账号服务，缓存有上限与逐出），
  因此**吊销与过期最长 60 秒后才在本服务生效**；
- 本地先做形态预检（`mfp_` + 43 位 base62），明显非法的明文直接 `401 invalid_token`，不打账号服务；
- 令牌无效 / 已吊销 / 已过期 / 账号被封禁 → `401 invalid_token`（共用一个稳定机器码，不细分原因）；
- 账号服务不可达、内省端点未上线或未配置 `AUTH_URL` → `503 auth_unavailable`，**不是 401**：
  那是依赖故障，回 401 会让 bot/CI 以为凭据有问题去换令牌（换令牌解决不了，重试才行）；
- 状态码映射的边界（别按字面"非 200 都当不认"改回去）：只有 `401` / `403` 是账号服务对**令牌本身**的判定，
  才回 `401 invalid_token`；`503`（账号服务读不动库）、`404`（内省端点还没上线，滚动部署期）、
  `429`（内省限流）与 5xx / 网络超时都**不是**"令牌无效"的证据，一律回 `503 auth_unavailable`——
  照字面把它们也回 401，会让 bot/CI 把有效令牌当废令牌丢掉（换令牌解决不了这些故障，重试才行）；
- PAT 请求不回落 `mf_session` Cookie（浏览器里可能同时有另一个用户的会话），也不产出 Cookie。
  实现与回归见 `internal/auth/pat.go`；三处（目录 / 互动 / 存储）必须同改，口径见主仓库 README 的同名字段。

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

本服务拥有 `community` schema，表结构与主仓库 `modules` 包中的 `forum_*` **逐列一致**，
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
- 顺序 `boards → topics → posts → tags → topic_tags → favorites`，满足外键依赖；
  唯一跨 schema 的步骤是收藏（源表在主仓库的 `catalog.favorites`）；
- 迁移窗口：切流前单体仍在写入，因此**切流时再跑一次**补齐增量。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `PORT` | `8083` | 监听端口 |
| `DATABASE_URL` | 由 `DB_*` 拼装 | PostgreSQL 连接串（业务表在 `community` schema；审计表在跨服务共用的 `audit`，见「审计留痕」） |
| `COMMUNITY_JWKS_URL` | `http://auth:8081/api/oidc/jwks` | 验签公钥来源：账号服务是唯一签发方 |
| `AUTH_URL` | 空 | 账号服务地址：存量不透明会话令牌的兜底解析（`GET /api/auth/me`）与 PAT 内省（`POST /api/auth/tokens/introspect`）；留空即"只接受 JWT"且 PAT 一律 `503 auth_unavailable` |
| `AUTH_JWT_PUBLIC_KEY` | 空 | 静态公钥（PEM 或 base64 PEM）；设置后不再请求 JWKS |
| `AUTH_JWT_ISSUER` / `AUTH_JWT_AUDIENCE` | `https://findverse.cc/api` / `metafusion` | 与主仓库一致，避免存量令牌失效 |
| `CATALOG_URL` | `http://backend:8080` | 目录服务地址（可见性、标题、关系邻居） |
| `COMMUNITY_CATALOG_TIMEOUT_MS` | `5000` | 目录调用总预算的**下限**（策略自带 7s，含 3 次尝试）；调小不生效——预算被调小会砍掉重试，见「跨服务依赖不可用」 |
| `TRUSTED_PROXIES` | 空 | 应用层信任的反向代理范围（IP/CIDR 逗号分隔；留空 = 回环 + RFC1918 私网 = 网关容器所在网段，`none` = 入口链上没有代理）。决定审计 `actor_ip` 与按 IP 限流所用的 `ClientIP()`；非法项直接拒绝启动 |

## 运行

```bash
go run cmd/server/main.go
go vet ./...
# 带真库的用例共用一个测试库（COMMUNITY_TEST_DSN），包与包之间并行执行会互相清表：
# 与 CI 一致用 -p 1 串行跑，否则会出现偶发的外键失败。
COMMUNITY_TEST_DSN='postgres://user:pass@127.0.0.1:5432/metafusion_community_test?sslmode=disable' go test -p 1 ./...
```

## 迁移状态

- 主仓库仍提供 `/api/community/*`（当前线上流量入口），本服务为切流目标；
  两者共用同一份表结构的复制体，切流前靠 `cmd/migrate` 同步，切流后旧实现随 `modules` 包下线。
- 实体合并（`entity.merged`）后的引用改写：旧实现由单体订阅 outbox 完成；本服务的增量消费
  在 P4 与跨服务事件通道一起确定，当前不消费事件。
