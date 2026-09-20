package handler

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/audit"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// registerFavorites 挂载收藏与收藏列表。收藏是用户行为，属互动系统，不属目录元数据；
// 路径与分页口径与主仓库 catalog 的 /favorites/* 逐字一致，切流时前端零改动。
func (h *Handler) registerFavorites(api *gin.RouterGroup) {
	// 分页走 page/page_size 写法（口径、换算与兼容期见 paging.go）。

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
		// 取不到目录是 503（依赖故障），不是 404：把自己的故障说成"目标不存在"，
		// 用户会以为条目被删了，运维在监控里也看不到这次故障。
		entity, err := h.catalog.Lookup(c.Request.Context(), strings.TrimSpace(in.TargetID))
		if err != nil {
			failUpstream(c)
			return
		}
		if entity.ID == "" || entity.Kind != targetType {
			fail(c, 404, "not_found")
			return
		}
		// X01：Lookup 已跟随 /resolve，entity.ID 即 canonical；新写归一 canonical。
		targetID := entity.ID
		favorited, err := h.store.ToggleFavorite(c.Request.Context(), h.principal(c).ID, targetType, targetID)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		// 被动对象是**被收藏的实体**（target_type 进 changes）：收藏是用户对实体的关系，
		// 审计行按 target 查得到"这个条目被谁收藏过"。toggle 一次只有一个动作码，
		// 收藏还是取消由 changes.favorited 表达（路由是先于响应决定的，没有第二个码可用）。
		audit.Describe(c, audit.Detail{TargetType: "entity", TargetID: targetID, Changes: map[string]any{
			"target_type": targetType, "favorited": favorited,
		}})
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
		// X01：按 canonical 查状态（请求别名 A 归一到 B 后查 B，再映射回请求 ID）。
		// 非法 UUID 直接视为未收藏：送进 uuid 列只会拿到 pq 解析错误（旧实现曾 500 回显）。
		valid := []string{}
		for _, id := range ids {
			if _, err := uuid.Parse(id); err == nil {
				valid = append(valid, id)
			}
		}
		resolved, err := h.catalog.ResolveMany(c.Request.Context(), valid)
		if err != nil {
			failUpstream(c)
			return
		}
		canonicals := []string{}
		seen := map[string]bool{}
		for _, id := range ids {
			if canonical := resolved[id]; canonical != "" && !seen[canonical] {
				seen[canonical] = true
				canonicals = append(canonicals, canonical)
			}
		}
		v, err := h.store.FavoriteStatus(c.Request.Context(), p.ID, c.Query("target_type"), canonicals)
		if err != nil {
			fail(c, storeErrorStatus(err), storeErrorCode(err))
			return
		}
		hit := map[string]bool{}
		for _, id := range v {
			hit[id] = true
		}
		out := []string{}
		for _, id := range ids {
			if canonical := resolved[id]; canonical != "" && hit[canonical] {
				out = append(out, id)
			}
		}
		c.JSON(200, gin.H{"favorited": out})
	})

	// 我的收藏需登录：普通用户可列出自己的收藏。
	api.GET("/favorites/mine", h.guard(true), func(c *gin.Context) {
		limit, offset := pagingPageSize(c, 20)
		h.respondFavorites(c, h.principal(c).ID, c.Query("target_type"), limit, offset)
	})

	// 指定用户收藏列表：公开读，但目标实体仍按请求者可见性过滤；
	// 不可见或已删除的目标跳过展示，不泄露其存在性。
	api.GET("/users/:id/favorites", func(c *gin.Context) {
		// 用户 id 是 uuid 主键：非法字面量按"没有这个人"处理（404），
		// 不能送进 uuid 列——那只会拿到 pq 的解析错误，再被回显给客户端。
		ownerID := c.Param("id")
		if _, err := uuid.Parse(ownerID); err != nil {
			fail(c, 404, "not_found")
			return
		}
		limit, offset := pagingPageSize(c, 20)
		h.respondFavorites(c, ownerID, c.Query("target_type"), limit, offset)
	})
}

// storeErrorCode/Status 把仓储层的错误翻成对外错误码与状态：
// 只有"目标类型不合法"是调用方的输入错误（400 invalid_target_type），
// 其余（连接断了、列不存在、uuid 解析失败）都是本服务的故障，统一 500 module_error。
// 直接把 err.Error() 当错误码回给客户端会把数据库原文（含 SQL 片段）吐出去。
func storeErrorCode(err error) string {
	if errors.Is(err, store.ErrInvalidTargetType) {
		return "invalid_target_type"
	}
	return "module_error"
}

func storeErrorStatus(err error) int {
	if errors.Is(err, store.ErrInvalidTargetType) {
		return 400
	}
	return 500
}

// respondFavorites 组装收藏列表：本服务只提供自有数据，实体摘要经目录服务透传，
// 因此不会出现"互动服务重新定义了一遍目录 DTO"的字段漂移。
func (h *Handler) respondFavorites(c *gin.Context, ownerID, targetType string, limit, offset int) {
	items, total, err := h.store.ListFavorites(c.Request.Context(), ownerID, targetType, limit, offset)
	if err != nil {
		fail(c, storeErrorStatus(err), storeErrorCode(err))
		return
	}
	out := []map[string]any{}
	for _, f := range items {
		raw, err := h.catalog.LookupRaw(c.Request.Context(), f.TargetID)
		if err != nil {
			// 上游不可用：整请求 503。继续拼下去会返回一份"少了几条"的收藏列表，
			// 而调用方无从分辨是目标不可见还是目录挂了。
			failUpstream(c)
			return
		}
		if raw == nil {
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
