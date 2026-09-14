package handler

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/store"
)

// registerFavorites 挂载收藏与收藏列表。收藏是用户行为，属互动系统，不属目录元数据；
// 路径与分页口径与主仓库 catalog 的 /favorites/* 逐字一致，切流时前端零改动。
func (h *Handler) registerFavorites(api *gin.RouterGroup) {
	// 分页约定（与主仓库一致的静默收敛口径）：page<1 收敛为 1；
	// page_size 越界（<1 或 >100）收敛为 20，不硬拒绝，避免翻页参数抖动直接 400。
	favPage := func(c *gin.Context) (int, int) {
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
		if page < 1 {
			page = 1
		}
		if size < 1 || size > 100 {
			size = 20
		}
		return page, size
	}

	// 收藏切换需登录、不限管理员：普通用户与编辑均可收藏可见实体。
	api.POST("/favorites/toggle", h.guard(true), func(c *gin.Context) {
		var in struct {
			TargetType string `json:"target_type"`
			TargetID   string `json:"target_id"`
		}
		if !body(c, &in) {
			return
		}
		targetType := strings.TrimSpace(in.TargetType)
		if _, err := store.KindFor(targetType); err != nil {
			fail(c, 400, "invalid_target_type")
			return
		}
		// 目标必须存在且对请求者可见，且 kind 与声明的 target_type 相符：
		// 收藏到不可见条目会让"谁收藏了什么"泄露编辑中的条目。
		entity, ok := h.catalog.Lookup(c.Request.Context(), strings.TrimSpace(in.TargetID))
		if !ok || entity.Kind != targetType {
			fail(c, 404, "not_found")
			return
		}
		favorited, err := h.store.ToggleFavorite(c.Request.Context(), h.principal(c).ID, targetType, strings.TrimSpace(in.TargetID))
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{"favorited": favorited})
	})

	// 批量查询收藏状态：匿名返回空集合（未登录自然没有收藏）。
	api.GET("/favorites/status", func(c *gin.Context) {
		p := h.principal(c)
		if p == nil {
			c.JSON(200, gin.H{"favorited": []string{}})
			return
		}
		ids := []string{}
		for _, id := range strings.Split(c.Query("target_ids"), ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		v, err := h.store.FavoriteStatus(c.Request.Context(), p.ID, c.Query("target_type"), ids)
		if err != nil {
			fail(c, 400, err.Error())
			return
		}
		c.JSON(200, gin.H{"favorited": v})
	})

	// 我的收藏需登录：普通用户可列出自己的收藏。
	api.GET("/favorites/mine", h.guard(true), func(c *gin.Context) {
		page, size := favPage(c)
		h.respondFavorites(c, h.principal(c).ID, c.Query("target_type"), size, (page-1)*size)
	})

	// 指定用户收藏列表：公开读，但目标实体仍按请求者可见性过滤；
	// 不可见或已删除的目标跳过展示，不泄露其存在性。
	api.GET("/users/:id/favorites", func(c *gin.Context) {
		page, size := favPage(c)
		h.respondFavorites(c, c.Param("id"), c.Query("target_type"), size, (page-1)*size)
	})
}

// respondFavorites 组装收藏列表：本服务只提供自有数据，实体摘要经目录服务透传，
// 因此不会出现"互动服务重新定义了一遍目录 DTO"的字段漂移。
func (h *Handler) respondFavorites(c *gin.Context, ownerID, targetType string, limit, offset int) {
	items, total, err := h.store.ListFavorites(c.Request.Context(), ownerID, targetType, limit, offset)
	if err != nil {
		fail(c, 400, err.Error())
		return
	}
	out := []map[string]any{}
	for _, f := range items {
		raw, ok := h.catalog.LookupRaw(c.Request.Context(), f.TargetID)
		if !ok {
			continue // 目标不可见/已删除：跳过，不清空记录
		}
		var entity json.RawMessage = raw
		out = append(out, map[string]any{
			"id":          f.ID,
			"target_type": f.TargetType,
			"target_id":   f.TargetID,
			"created_at":  f.CreatedAt,
			"entity":      entity,
		})
	}
	c.JSON(200, gin.H{"items": out, "total": total, "visible": true})
}
