package handler

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/audit"
	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// Handler 承载互动服务的 HTTP 契约：论坛（/api/community/*）、收藏与用户统计
// （/api/favorites/*、/api/users/:id/{favorites,stats}）与私信（/api/messages/*）。
// 路径与请求/响应形状与主仓库 modules 包逐字一致，切流时前端不需要任何改动。
type Handler struct {
	// db 供迁移自单体的实现直接执行 SQL（与主仓库逐字一致，便于对照回归）。
	db *sql.DB
	// store 是新实现在用的自有 schema 访问层。
	store    *store.Store
	catalog  *catalog.Client
	verifier *auth.Verifier
	// audit 是写操作的审计写入器（注册表与接线见 audit.go）。db 为 nil 时它是 nil，
	// 中间件退化为空操作。
	audit *audit.Recorder
}

func New(s *store.Store, cat *catalog.Client, verifier *auth.Verifier) *Handler {
	h := &Handler{db: s.DB(), store: s, catalog: cat, verifier: verifier}
	// 写入器就在这里建，而不是要求调用方注入：注册表能拦住"新增写端点忘了登记动作码"，
	// 但拦不住"忘了把写入器接上"——那会让全部写操作静默不留痕，比漏一条端点严重得多。
	if h.db != nil {
		h.audit = audit.NewRecorder(h.db, audit.ServiceName)
	}
	return h
}

// Close 排空审计队列并停掉后台 goroutine。**必须在 http.Server.Shutdown 之后调用**：
// 关闭队列时仍在入队的请求会丢行（见 audit.Recorder.Close 的约定）。进程被 kill 时不必调，
// 队列里未落库的行随进程一起丢（审计是旁路，不给业务收尾加钩子）。
func (h *Handler) Close() {
	if h.audit != nil {
		h.audit.Close()
	}
}

// Register 挂载全部路由。/api 前缀下统一先挂身份中间件：
// 读接口匿名可用（可见性由目录实体决定），写接口要求登录，运营类写接口再要求具体权限码。
func (h *Handler) Register(r *gin.Engine) {
	api := r.Group("/api")
	api.Use(h.verifier.Middleware())
	// 审计中间件必须挂在任何路由注册之前（gin 的 Use 只对之后注册的路由生效），
	// 且挂在身份中间件之后：它要在 c.Next() 之后读 Principal 才能记下操作者。
	api.Use(h.auditMiddleware())
	h.registerForum(api)
	h.registerCommunity(api)
	h.registerFavorites(api)
	h.registerMessages(api)
	h.registerBoards(api)
	h.registerStats(api)
	h.registerModeration(api)
}

// guard 是写操作的门槛：互动服务读接口对可见实体开放，写接口必须登录。
//
// 写分支在 auth.Required 之外多做一件事：把它的 401 落成审计的 error_code。审计中间件读不到
// 已经被 abort 掉的响应体，而匿名写请求恰恰是最该留痕的一类；让审计行的 error_code 与响应体的
// error 字段一致，读审计的人就不必去猜。语义与状态码由 auth.Required 全权决定（PAT 的 401/503
// 判定也在它里面），这里只补一个码，不改响应。
func (h *Handler) guard(write bool) gin.HandlerFunc {
	if !write {
		return func(c *gin.Context) { c.Next() }
	}
	required := h.verifier.Required()
	return func(c *gin.Context) {
		required(c)
		if c.Writer.Status() == http.StatusUnauthorized {
			audit.Fail(c, "authentication_required")
		}
	}
}

// require 是"已登录 + 持有权限码"的门槛：匿名 401 authentication_required（与 guard 同一口径），
// 已登录但缺码 403 forbidden。身份由组上的 Middleware 预解析，这里不重复解析令牌。
// 判定放在中间件而不是处理器里：缺码的请求不该进入业务逻辑（更不该先读一次库再看权限）。
func (h *Handler) require(code string) gin.HandlerFunc {
	return func(c *gin.Context) {
		p := h.principal(c)
		if p == nil {
			audit.Fail(c, "authentication_required")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication_required"})
			return
		}
		if !p.Can(code) {
			fail(c, http.StatusForbidden, "forbidden")
			c.Abort()
			return
		}
		c.Next()
	}
}

func (h *Handler) principal(c *gin.Context) *auth.Principal { return auth.Current(c) }

// entity 确认实体对调用者可见；不可见一律 404，不区分"不存在"与"无权限"。
func (h *Handler) entity(c *gin.Context, id string) bool {
	if _, ok := h.catalog.Lookup(c.Request.Context(), id); !ok {
		fail(c, 404, "not_found")
		return false
	}
	return true
}

// Seed 播种默认板块：首次运行写入，已存在的不覆盖，保留运营在后台的调整。
// 转发到论坛实现里的 seedForum，避免同一份板块定义出现两份。
func Seed(ctx context.Context, db *sql.DB) error { return seedForum(ctx, db) }

// fail 是全部业务失败的唯一出口：同一个稳定码既进响应体，也进审计行的 error_code
// （中间件在 c.Next() 之后读它）。失败却查不到错误码的审计行，等于把排障成本留给下一个人。
func fail(c *gin.Context, status int, code string) {
	audit.Fail(c, code)
	c.JSON(status, gin.H{"error": code})
}

// body 统一写接口的请求解析：2MB 上限，错误码与主仓库一致。
// 上限是必须的——网关的 client_max_body_size 是 1G，没有它一个写请求就能让本服务
// 把整份载荷读进内存；字段集合由各请求结构体决定（gin 默认忽略未知字段）。
func body(c *gin.Context, v any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	if err := c.ShouldBindJSON(v); err != nil {
		fail(c, 400, "invalid_payload")
		return false
	}
	return true
}

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}
