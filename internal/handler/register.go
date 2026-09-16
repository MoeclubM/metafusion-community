package handler

import (
	"context"
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// Handler 承载互动服务的 HTTP 契约：论坛（/api/community/*）与用户互动记录（/api/records/*）。
// 路径与请求/响应形状与主仓库 modules 包逐字一致，切流时前端不需要任何改动。
type Handler struct {
	// db 供迁移自单体的实现直接执行 SQL（与主仓库逐字一致，便于对照回归）。
	db *sql.DB
	// store 是新实现在用的自有 schema 访问层。
	store    *store.Store
	catalog  *catalog.Client
	verifier *auth.Verifier
}

func New(s *store.Store, cat *catalog.Client, verifier *auth.Verifier) *Handler {
	return &Handler{db: s.DB(), store: s, catalog: cat, verifier: verifier}
}

// Register 挂载全部路由。/api 前缀下统一先挂身份中间件：
// 读接口匿名可用（可见性由目录实体决定），写接口再各自要求登录。
func (h *Handler) Register(r *gin.Engine) {
	api := r.Group("/api")
	api.Use(h.verifier.Middleware())
	h.registerForum(api)
	h.registerCommunity(api)
	h.registerFavorites(api)
}

// guard 是写操作的门槛：互动服务读接口对可见实体开放，写接口必须登录。
func (h *Handler) guard(write bool) gin.HandlerFunc {
	if write {
		return h.verifier.Required()
	}
	return func(c *gin.Context) { c.Next() }
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

func fail(c *gin.Context, status int, code string) {
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
