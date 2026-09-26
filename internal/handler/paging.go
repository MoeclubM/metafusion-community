package handler

import (
	"strconv"

	"github.com/gin-gonic/gin"
)

// maxPageSize 是分页窗口上限。越界时取缺省值。
const maxPageSize = 100

func legacyPaging(c *gin.Context) bool {
	_, hasLimit := c.GetQuery("limit")
	_, hasOffset := c.GetQuery("offset")
	return hasLimit || hasOffset
}

// pagingPageSize 解析 page/page_size，返回存储层使用的 (limit, offset)。
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
