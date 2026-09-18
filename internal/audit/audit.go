// Package audit 是互动服务的审计写入端：把「谁、在什么时候、对什么、做了什么、结果如何」
// 写进跨服务共用的 audit.audit_log（字段级契约见主仓库 docs/architecture/audit-log.md）。
//
// 三条不变式（改代码时先看这三条，它们解释了下面所有反直觉的写法）：
//
//  1. **审计是旁路**：落库失败只打 error 级日志并丢弃该行，绝不让业务写入回滚，也绝不阻塞响应
//     （Record 是非阻塞入队 + 后台 goroutine 落库）。审计的用途是事后追责与排障，不是业务约束，
//     所以"业务成功、审计丢一行"比"审计写失败导致业务回滚"可接受；丢行有计数器可观测。
//  2. **写入前必须脱敏**：口令、令牌、密钥、完整邮箱一律不进库（见 SanitizeChanges / MaskEmail）。
//     脱敏在 Recorder 内部做，而不是指望每个调用点记得——调用点只要把语义字段丢进 changes 即可。
//  3. **只记登记过的写路由**：动作码注册表（Options.Actions，本服务的表在 handler 包的
//     internal/handler/audit.go）是唯一开关，新增写端点忘了登记会被 handler 包的
//     「写路由覆盖守卫」测试拦住。
//
// 本文件与账号 / 目录 / 存储三个服务的 internal/audit **同源**（四仓是独立 module，
// 没有跨仓依赖通道，所以是四份副本而不是共享模块）：函数名与语义保持一致，便于交叉审查；
// 各仓的差异只有 ServiceName 与注释里的例子。
package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// ServiceName 是本服务在 audit.audit_log.service 里的取值。
const ServiceName = "community"

// Schema 是审计表的建表语句：与契约 §1 **逐字一致**（四个服务各存一份同样的 DDL，
// 谁先启动谁建表，其余服务走 to_regclass 守卫的幂等路径）。
//
// 为什么用 audit 独立 schema：它不是任何单个服务的领域数据（互动服务的库里有 community 与 audit
// 两个 schema，读得到 catalog/auth 的行是刻意的，见契约 §5）；为什么不建外键：账号删了审计还得在。
// pg_advisory_xact_lock(740205) 让四个服务首次启动时串行建表：一次 Exec 里的多条语句由
// lib/pq 作为隐式事务批处理发送，锁因此覆盖到建表结束，避免两条 CREATE TABLE 撞在 pg_type
// 的唯一索引上。互动服务的迁移器（internal/migrator）把整个迁移文件一次 ExecContext，
// 本服务的建表与它外层的事务级迁移锁是同一个键，同事务内重复获取是安全的。
//
// **为什么整段包在 to_regclass 守卫里**（真库实测，2026-09-19）：PostgreSQL 的
// `CREATE INDEX IF NOT EXISTS` 会**先做表所有权检查、再看索引是否存在**（`CREATE TABLE IF NOT EXISTS`
// 不同，它只要求 schema 的 CREATE）。四个服务启动都会执行这段 DDL，而表由部署时的 mf_audit_owner
// 预建——不做守卫的话，非 owner 的运行角色会拿到 `42501 must be owner of table audit_log`，
// 四个服务全部起不来（实测：预建后四服务全挂；不预建让 community 先建，另三个全挂）。
// 守卫让"表已存在"的实例上整段变成纯空转：所有 DDL 与所有权检查都不会被触发。
// 索引因此不再写 IF NOT EXISTS（它们在守卫内，表是刚建的）；锁用 PERFORM 放进守卫里，
// 仍然在建表之前取。拆掉守卫或把索引写回 IF NOT EXISTS 会被
// TestAuditSchemaUsesExistenceGuard 拦下。
const Schema = `
DO $audit_ddl$
BEGIN
  PERFORM pg_advisory_xact_lock(740205);
  IF to_regclass('audit.audit_log') IS NULL THEN
    CREATE SCHEMA IF NOT EXISTS audit;
    CREATE TABLE audit.audit_log (
      id               uuid PRIMARY KEY,
      occurred_at      timestamptz NOT NULL DEFAULT now(),
      service          text NOT NULL,
      action           text NOT NULL,
      actor_user_id    uuid,
      actor_username   text NOT NULL DEFAULT '',
      credential_type  text NOT NULL DEFAULT '',
      actor_ip         text NOT NULL DEFAULT '',
      actor_user_agent text NOT NULL DEFAULT '',
      target_type      text NOT NULL DEFAULT '',
      target_id        text NOT NULL DEFAULT '',
      changes          jsonb NOT NULL DEFAULT '{}'::jsonb,
      result           text NOT NULL DEFAULT 'success' CHECK (result IN ('success','failure')),
      error_code       text NOT NULL DEFAULT '',
      request_method   text NOT NULL DEFAULT '',
      route            text NOT NULL DEFAULT '',
      http_status      int NOT NULL DEFAULT 0,
      request_id       text NOT NULL DEFAULT ''
    );
    CREATE INDEX audit_log_occurred_at_idx ON audit.audit_log(occurred_at DESC);
    CREATE INDEX audit_log_service_action_idx ON audit.audit_log(service, action, occurred_at DESC);
    CREATE INDEX audit_log_actor_idx ON audit.audit_log(actor_user_id, occurred_at DESC);
    CREATE INDEX audit_log_target_idx ON audit.audit_log(target_type, target_id, occurred_at DESC);
  END IF;
END
$audit_ddl$;
`

// 凭据类型取值（契约 §1 的 credential_type 列）。互动服务只验签 / 内省，分不清"会话令牌"与
// "OAuth 令牌"，因此实际只会写 session 与 pat（近似值，契约 §7 已记录）。
const (
	CredentialSession   = "session"
	CredentialPAT       = "pat"
	CredentialOAuth     = "oauth"
	CredentialAnonymous = "anonymous"
	CredentialSystem    = "system"
)

// Result 取值。
const (
	ResultSuccess = "success"
	ResultFailure = "failure"
)

// 长度上限（契约 §4）：单值 512、UA 512、changes 序列化后 8KB。
const (
	maxValueLen   = 512
	maxUserAgent  = 512
	maxChangesLen = 8 << 10
	// maxSanitizeDepth 是递归上限：审计写入在请求路径上，遇到自引用结构不能在这里卡住。
	maxSanitizeDepth = 6
)

// Entry 是一条待写入的审计行。
type Entry struct {
	ID             string
	OccurredAt     time.Time
	Service        string
	Action         string
	ActorUserID    string
	ActorUsername  string
	CredentialType string
	ActorIP        string
	ActorUserAgent string
	TargetType     string
	TargetID       string
	Changes        map[string]any
	Result         string
	ErrorCode      string
	RequestMethod  string
	Route          string
	HTTPStatus     int
	RequestID      string
}

// Detail 是处理器补充的被动对象与变更摘要。
type Detail struct {
	TargetType string
	TargetID   string
	Changes    map[string]any
}

// Actor 是操作者身份投影。
type Actor struct {
	UserID         string
	Username       string
	CredentialType string
}

// Recorder 是审计写入器：非阻塞入队 + 单个后台 goroutine 串行落库。
//
// 单个 writer 有两个好处：不与连接池抢并发（库连接是受限资源），以及 Flush 能用一个
// 屏障 job 精确表示"前面的都写完了"。
type Recorder struct {
	db      *sql.DB
	service string

	jobs    chan job
	closed  atomic.Bool
	wg      sync.WaitGroup
	dropped atomic.Int64
	written atomic.Int64

	// sink 是本条行的落库函数，默认走 insert(db)。用例用它注入"卡住/失败"的写入，
	// 从而在不建真库的前提下验证 Record 的非阻塞与队列满时的丢弃。
	sink func(Entry) error
}

// job 是队列元素：entry 为 nil 时是屏障（Flush 用）。
type job struct {
	entry   *Entry
	barrier chan struct{}
}

// NewRecorder 建一个写入器并启动后台 goroutine。db 为 nil 时退化为"只计数不落库"，
// 供不建库的用例使用（不静默假装成功：Written 计数不会增长）。
func NewRecorder(db *sql.DB, service string) *Recorder {
	r := &Recorder{db: db, service: service, jobs: make(chan job, 1024)}
	r.sink = r.insert
	r.wg.Add(1)
	go r.loop()
	return r
}

func (r *Recorder) loop() {
	defer r.wg.Done()
	for j := range r.jobs {
		if j.entry != nil {
			if err := r.sink(*j.entry); err != nil {
				// 只有这一处会丢掉审计行，所以必须吵：丢行意味着"发生了没人知道的写操作"。
				slog.Error("互动服务：审计写入失败（该行已丢弃，业务不受影响）", "action", j.entry.Action,
					"service", r.service, "request_id", j.entry.RequestID, "err", err.Error())
			} else {
				r.written.Add(1)
			}
		}
		if j.barrier != nil {
			close(j.barrier)
		}
	}
}

// Record 入队一条审计行。**非阻塞**：队列满时打 error 日志并丢弃，绝不等待、绝不阻塞调用方。
func (r *Recorder) Record(e Entry) {
	if r == nil || r.closed.Load() {
		return
	}
	e = r.prepare(e)
	select {
	case r.jobs <- job{entry: &e}:
	default:
		r.dropped.Add(1)
		slog.Error("互动服务：审计队列已满，丢弃一条审计行", "action", e.Action, "request_id", e.RequestID)
	}
}

// RecordSync 同步写入一条审计行（测试与真库用例用；业务路径一律走非阻塞的 Record）。
// 返回错误表示这一行没落库；调用方自行决定是否影响业务结果。
func (r *Recorder) RecordSync(ctx context.Context, e Entry) error {
	if r == nil {
		return nil
	}
	return r.insert(r.prepare(e))
}

// Flush 等队列里已有的行全部落库（测试断言用）。屏障 job 与审计行走同一条 FIFO 队列，
// 因此屏障被关掉时它前面的行必定已经处理完。
func (r *Recorder) Flush(ctx context.Context) error {
	if r == nil || r.closed.Load() {
		return nil
	}
	barrier := make(chan struct{})
	select {
	case r.jobs <- job{barrier: barrier}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-barrier:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close 排空队列并停掉后台 goroutine（测试收尾与进程退出时调用）。
// 调用方负责保证没有并发的 Record：本服务在 http.Server.Shutdown 返回（在途请求都已收尾）之后调用。
func (r *Recorder) Close() {
	if r == nil || !r.closed.CompareAndSwap(false, true) {
		return
	}
	close(r.jobs)
	r.wg.Wait()
}

// Dropped / Written 是观测计数：丢行必须能被看见。
func (r *Recorder) Dropped() int64 { return r.dropped.Load() }
func (r *Recorder) Written() int64 { return r.written.Load() }

// prepare 补齐默认值并脱敏：这是一条审计行进入库前的唯一加工点。
func (r *Recorder) prepare(e Entry) Entry {
	if e.ID == "" {
		e.ID = uuid.NewString()
	}
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	if e.Service == "" {
		e.Service = r.service
	}
	if e.Result == "" {
		e.Result = ResultSuccess
	}
	e.ActorUserAgent = truncate(e.ActorUserAgent, maxUserAgent)
	e.ActorUsername = truncate(e.ActorUsername, maxValueLen)
	e.ActorIP = truncate(e.ActorIP, 64)
	e.TargetID = truncate(e.TargetID, maxValueLen)
	e.ErrorCode = truncate(e.ErrorCode, maxValueLen)
	e.Changes = SanitizeChanges(e.Changes)
	return e
}

// insert 落库一行。occurred_at 由应用侧给定（动作发生的时刻，比数据库 now() 更贴近事实，
// 也避免批量重放时所有行落在同一时刻）。actor_user_id 经 NULLIF 转换后落库：
// 匿名与"身份里没有 id"都落 NULL，而不是让空串把整行写失败。
func (r *Recorder) insert(e Entry) error {
	if r.db == nil {
		return nil
	}
	changes, err := marshalChanges(e.Changes)
	if err != nil {
		return err
	}
	_, err = r.db.ExecContext(context.Background(), `
INSERT INTO audit.audit_log(
  id, occurred_at, service, action, actor_user_id, actor_username, credential_type,
  actor_ip, actor_user_agent, target_type, target_id, changes, result, error_code,
  request_method, route, http_status, request_id)
VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
		e.ID, e.OccurredAt, e.Service, e.Action, e.ActorUserID, e.ActorUsername, e.CredentialType,
		e.ActorIP, e.ActorUserAgent, e.TargetType, e.TargetID, changes, e.Result, e.ErrorCode,
		e.RequestMethod, e.Route, e.HTTPStatus, e.RequestID)
	return err
}

// marshalChanges 把摘要序列化成 jsonb 的文本参数；超限时换成"截断标记 + 键名清单"，
// 保证库里永远有一条可读的痕迹，而不是半截 JSON。
func marshalChanges(changes map[string]any) (string, error) {
	if len(changes) == 0 {
		return "{}", nil
	}
	raw, err := json.Marshal(changes)
	if err != nil {
		return "", err
	}
	if len(raw) <= maxChangesLen {
		return string(raw), nil
	}
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	trimmed, err := json.Marshal(map[string]any{
		"_truncated": true,
		"_bytes":     len(raw),
		"_keys":      keys,
	})
	if err != nil {
		return "", err
	}
	return string(trimmed), nil
}

// redactedExactKeys 是整键丢弃的精确黑名单（契约 §4）。
var redactedExactKeys = map[string]bool{
	"password": true, "old_password": true, "new_password": true, "password_hash": true,
	"token": true, "access_token": true, "refresh_token": true, "token_hash": true,
	"secret": true, "client_secret": true, "secret_hash": true, "api_key": true,
	"authorization": true, "cookie": true, "code_verifier": true,
}

// redactedKeyParts 是整键丢弃的子串黑名单（宁可少记，不可泄漏）。
var redactedKeyParts = []string{"password", "secret", "token", "hash"}

// emailPattern 用于"值里顺手带了邮箱"的情形。末段的点号必须转义：
// 写成 .[A-Za-z]{2,} 会把 "a@b_cd" 这类非邮箱也当成邮箱去遮罩。
var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+.[A-Za-z]{2,}`)

// Redacted 是脱敏后的占位值：留在库里能看出"这里原本有个值"，但拿不到值本身。
const Redacted = "[redacted]"

// SanitizeChanges 递归脱敏变更摘要：调用点只负责把语义字段放进来，脱敏由这里统一兜底。
//
// 三层防线：键名黑名单 → 键名含 email → 值里出现邮箱形状的串。深度上限见 maxSanitizeDepth，
// 防止有人塞进自引用结构（审计写入在请求路径上，不能在这里卡住）。
func SanitizeChanges(in map[string]any) map[string]any { return sanitizeChangesDepth(in, 0) }

func sanitizeChangesDepth(in map[string]any, depth int) map[string]any {
	if len(in) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if isRedactedKey(k) {
			out[k] = Redacted
			continue
		}
		out[k] = sanitizeValue(strings.ToLower(strings.TrimSpace(k)), v, depth+1)
	}
	return out
}

func isRedactedKey(k string) bool {
	lower := strings.ToLower(strings.TrimSpace(k))
	if redactedExactKeys[lower] {
		return true
	}
	for _, part := range redactedKeyParts {
		if strings.Contains(lower, part) {
			return true
		}
	}
	return false
}

func sanitizeValue(key string, v any, depth int) any {
	if depth > maxSanitizeDepth {
		return Redacted
	}
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		return sanitizeString(key, t)
	case bool, float64, float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return v
	case []string:
		out := make([]any, 0, len(t))
		for _, s := range t {
			out = append(out, sanitizeString(key, s))
		}
		return out
	case []any:
		out := make([]any, 0, len(t))
		for _, item := range t {
			out = append(out, sanitizeValue(key, item, depth+1))
		}
		return out
	case map[string]any:
		return sanitizeChangesDepth(t, depth)
	case map[string]string:
		// 板块的语种 map 就是这一型：不展开会让语种值绕过长度截断与邮箱遮罩。
		inner := make(map[string]any, len(t))
		for k, s := range t {
			inner[k] = s
		}
		return sanitizeChangesDepth(inner, depth)
	case time.Time:
		return t.UTC().Format(time.RFC3339)
	default:
		// 其它类型（含指针/结构体）一律转成字符串再按文本脱敏：审计接受的信息面刻意窄。
		return sanitizeString(key, fmt.Sprint(v))
	}
}

func sanitizeString(key, s string) string {
	// 键名含 email，或值里出现邮箱形状的串：一律遮罩（域名保留，便于排障时判断"哪个域"）。
	s = MaskEmail(s)
	return truncate(s, maxValueLen)
}

// MaskEmail 把字符串里每个邮箱遮罩成 j***@example.com：保留首字母与域名，去掉可识别的人名部分。
// 值里没有邮箱形状的串时按长度截断原样返回——它不是通用脱敏器（那是 SanitizeChanges 的活）。
func MaskEmail(s string) string {
	return emailPattern.ReplaceAllStringFunc(s, maskEmailMatch)
}

func maskEmailMatch(m string) string {
	at := strings.LastIndex(m, "@")
	if at <= 0 || at == len(m)-1 {
		return m
	}
	return string([]rune(m[:at])[:1]) + "***@" + m[at+1:]
}

// MaskSecret 把准凭据（邀请码、一次性码）写成 abcd…：保留前 4 位供人工比对，其余抹掉。
// 互动服务当前没有这类字段，但四个服务的 API 形状一致，账号服务的邀请码必须经它再进 changes。
func MaskSecret(s string) string {
	runes := []rune(s)
	if len(runes) <= 4 {
		return strings.Repeat("*", len(runes))
	}
	return string(runes[:4]) + "…"
}

func truncate(s string, limit int) string {
	// 按 rune 边界截断，避免把多字节字符切成半个（标题可能是中文）。
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit]) + "…"
}

// ---- gin 中间件 ----

// ErrorCodeKey 是处理器写错误码的 gin 上下文键（audit.Fail 写入，中间件读取）。
const ErrorCodeKey = "audit_error_code"

const draftKey = "audit_draft"

// Options 是中间件参数。
type Options struct {
	// Recorder 为 nil 时中间件退化为空操作（不建库的进程与不建库的用例都能跑）。
	Recorder *Recorder
	// Actions 是「HTTP 方法 + 路由模板」→ 动作码的注册表；没登记的路由不写审计。
	Actions map[string]string
	// Exempt 是写路由的豁免表：route → 理由。它只被覆盖守卫测试使用（运行时不用），
	// 放在这里是为了让"为什么这条写路由不审计"和注册表挨着，改的时候一眼能看见。
	Exempt map[string]string
	// Actor 解析调用者身份；在 c.Next() 之后调用，因此处理器可以用 SetActor 覆盖。
	Actor func(*gin.Context) Actor
}

// draft 是请求内的审计草稿。
type draft struct {
	entry  Entry
	actor  *Actor
	detail Detail
}

// Middleware 把写请求的审计留痕挂到路由组上。
//
// **必须挂在组上、且在任何路由注册之前**：gin 的 RouterGroup.Use 只对之后注册的路由生效
// （注册时把当时的 handler 链复制进路由表）；也要挂在身份中间件之后，Actor 才读得到 Principal。
func Middleware(o Options) gin.HandlerFunc {
	return func(c *gin.Context) {
		if o.Recorder == nil {
			c.Next()
			return
		}
		// 路由模板：gin 在进入 handler 链之前就把 fullPath 设好了（gin.go 的 handleHTTPRequest）。
		// 取不到（NoRoute / 未匹配）时回落原始路径——它不会是注册表里的模板键，因此仍然不写审计。
		route := c.FullPath()
		if route == "" {
			route = c.Request.URL.Path
		}
		action, ok := o.Actions[c.Request.Method+" "+route]
		if !ok {
			c.Next()
			return
		}
		// request_id 先落：处理器与下游日志都要能用同一个 id 串起来；调用方自带的一律透传。
		requestID := strings.TrimSpace(c.GetHeader("X-Request-Id"))
		if requestID == "" {
			requestID = uuid.NewString()
		}
		requestID = truncate(requestID, maxValueLen)
		c.Writer.Header().Set("X-Request-Id", requestID)

		d := &draft{entry: Entry{
			Action:         action,
			RequestMethod:  c.Request.Method,
			Route:          route,
			ActorIP:        c.ClientIP(),
			ActorUserAgent: truncate(c.Request.UserAgent(), maxUserAgent),
			RequestID:      requestID,
		}}
		c.Set(draftKey, d)

		c.Next()

		// 身份与补充信息都在 c.Next() 之后读：本中间件挂在身份中间件之后，Principal 早已在上下文里；
		// 放在之后读还能覆盖"处理器在请求内改写身份"的情形（见 SetActor）。
		actor := d.actor
		if actor == nil && o.Actor != nil {
			a := o.Actor(c)
			actor = &a
		}
		if actor != nil {
			d.entry.ActorUserID = actor.UserID
			d.entry.ActorUsername = actor.Username
			d.entry.CredentialType = actor.CredentialType
		}
		if d.entry.CredentialType == "" {
			d.entry.CredentialType = CredentialAnonymous
		}
		d.entry.TargetType = d.detail.TargetType
		d.entry.TargetID = d.detail.TargetID
		d.entry.Changes = d.detail.Changes

		status := c.Writer.Status()
		if status == 0 {
			status = http.StatusOK
		}
		d.entry.HTTPStatus = status
		if status < 400 {
			d.entry.Result = ResultSuccess
		} else {
			d.entry.Result = ResultFailure
			// 错误码取处理器登记的稳定码；没登记时回落 http_<status>（契约 §3）。
			// 互动服务的写路径要么走 handler 的 fail()，要么走 require/guard 的 401，都已登记。
			code := c.GetString(ErrorCodeKey)
			if code == "" {
				code = "http_" + strconv.Itoa(status)
			}
			d.entry.ErrorCode = code
		}
		o.Recorder.Record(d.entry)
	}
}

// SetActor 覆盖本条请求的操作者（登录、注册这类"操作的瞬间才知道是谁"的端点用）。
// 互动服务的写接口都要求已登录，Actor 在进入时就能读到身份，因此没有调用点；
// 保留它是为了与另外三个服务的同源代码保持同一形状。
func SetActor(c *gin.Context, a Actor) {
	if d := currentDraft(c); d != nil {
		d.actor = &a
	}
}

// Describe 补充被动对象与变更摘要。**changes 只放语义字段**：口令/令牌/密钥/邮箱明文由
// Recorder 统一脱敏（SanitizeChanges），不要试图在这里自己拼 JSON 字符串。
// 多次调用按字段合并（后写的键覆盖同名键）。
func Describe(c *gin.Context, detail Detail) {
	d := currentDraft(c)
	if d == nil {
		return
	}
	if detail.TargetType != "" {
		d.detail.TargetType = detail.TargetType
	}
	if detail.TargetID != "" {
		d.detail.TargetID = detail.TargetID
	}
	if len(detail.Changes) > 0 {
		if d.detail.Changes == nil {
			d.detail.Changes = map[string]any{}
		}
		for k, v := range detail.Changes {
			d.detail.Changes[k] = v
		}
	}
}

// Fail 记下稳定错误码：中间件据此写 result=failure + error_code（与响应体的 error 字段一致）。
func Fail(c *gin.Context, code string) {
	if strings.TrimSpace(code) == "" {
		return
	}
	c.Set(ErrorCodeKey, code)
}

func currentDraft(c *gin.Context) *draft {
	if v, ok := c.Get(draftKey); ok {
		if d, ok := v.(*draft); ok {
			return d
		}
	}
	return nil
}
