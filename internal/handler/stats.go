package handler

import (
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// 用户互动统计：用户主页要的三个数字（主题 / 回复 / 收藏），**匿名可读**——
// 主页对未登录访客也展示；三个数字都来自本服务自有的表，因此这里就是它们的唯一归属
// （目录与账号服务都不提供这些计数）。口径写在 store.StatsFor 的注释里，与列表接口同源。
//
// "有这个人但没有互动记录"与"没有这个人"无法区分：账号数据不归本服务，本服务不查账号库，
// 两者都返回 0——与 GET /users/{id}/favorites 用空列表表达同一个意思保持一致；
// 非法 uuid 仍按 404 处理（送进 uuid 列只会拿到 pq 的解析错误，再被兜成 500 回显）。
func (h *Handler) registerStats(api *gin.RouterGroup) {
	api.GET("/users/:id/stats", func(c *gin.Context) {
		ownerID := c.Param("id")
		if _, err := uuid.Parse(ownerID); err != nil {
			fail(c, 404, "not_found")
			return
		}
		stats, err := h.store.StatsFor(c.Request.Context(), ownerID, commentBoard)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{"stats": stats})
	})
}
