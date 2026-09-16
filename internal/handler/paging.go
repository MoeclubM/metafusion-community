package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// maxPageSize 是两套写法共用的窗口上限。越界时取缺省值而不是截断到上限：这是历史行为，
// 前端按"缺省 = 第一页/默认页宽"的假设分页，改成截断会让同一个 URL 在不同版本取到不同窗口。
const maxPageSize = 100

// 分页参数在这个服务里有两种写法并存。这是**历史契约**：本批次只把口径写清、补测试，不改行为。
//
//   - limit/offset：/community/feed（limit 缺省 50）、/community/topics（limit 缺省 30，
//     offset 是起始下标）。limit < 1 或 > maxPageSize 取缺省值，offset < 0 取 0。
//   - page/page_size：/favorites/mine、/api/users/{id}/favorites（page 从 1 起、page_size 缺省 20）。
//     page < 1 取 1，page_size < 1 或 > maxPageSize 取缺省值。
//
// 换算关系：limit = page_size、offset = (page-1) * page_size。两套写法最终都归一成 (limit, offset)
// 交给存储层的 LIMIT/OFFSET，所以"同一个逻辑窗口"在两套写法下必须取到同一批数据
// （换算关系由 internal/handler/paging_test.go 钉住，窗口等价由 paging_postgres_test.go 在真库上验证）。
//
// 越界一律静默收敛、不返回 400：前端会把 URL 参数原样回传，参数抖动（手改地址、跨页带过来的空值）
// 不该变成整页报错；口径与主仓库 catalog 侧一致。
//
// 兼容期：两套写法保持现状语义，直到前端按端点统一到一套为止（见
// docs/architecture/decoupling-audit-2026-09.md 的 B4/B6）。在此之前不要删掉任何一套，
// 也不要在解析层做"写法互推"（例如让 favorites 也接受 limit/offset）——那是改变已公布契约的行为。
func pagingLimitOffset(c *gin.Context, defaultLimit int) (limit, offset int) {
	limit, _ = strconv.Atoi(c.Query("limit"))
	if limit <= 0 || limit > maxPageSize {
		limit = defaultLimit
	}
	offset, _ = strconv.Atoi(c.Query("offset"))
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// pagingPageSize 解析 page/page_size 写法并换算成 (limit, offset)，口径见 pagingLimitOffset 的说明。
func pagingPageSize(c *gin.Context, defaultSize int) (limit, offset int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", strconv.Itoa(defaultSize)))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > maxPageSize {
		size = defaultSize
	}
	return size, (page - 1) * size
}
