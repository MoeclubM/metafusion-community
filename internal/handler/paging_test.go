package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func pagingContext(t *testing.T, rawQuery string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/probe?"+rawQuery, nil)
	return c
}

// 换算关系是两套写法能共存的前提：同一个逻辑窗口（第 N 页、每页 M 条）在两套参数下
// 必须得到同一个 (limit, offset)，否则"翻到第 3 页"在两套写法下会取到不同数据。
func TestPaginationSpellingsAgreeOnSameWindow(t *testing.T) {
	cases := []struct {
		name       string
		limitQuery string
		pageQuery  string
		wantLimit  int
		wantOffset int
	}{
		{"第一页：offset = (1-1)*页宽", "limit=20&offset=0", "page=1&page_size=20", 20, 0},
		{"第二页：offset = (2-1)*页宽", "limit=20&offset=20", "page=2&page_size=20", 20, 20},
		{"非整页宽：offset = (3-1)*7", "limit=7&offset=14", "page=3&page_size=7", 7, 14},
		{"页宽取上限 100", "limit=100&offset=500", "page=6&page_size=100", 100, 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLimit, gotOffset := pagingLimitOffset(pagingContext(t, tc.limitQuery), 30)
			if gotLimit != tc.wantLimit || gotOffset != tc.wantOffset {
				t.Fatalf("limit/offset 写法 %q => (%d,%d)，期望 (%d,%d)",
					tc.limitQuery, gotLimit, gotOffset, tc.wantLimit, tc.wantOffset)
			}
			gotLimit, gotOffset = pagingPageSize(pagingContext(t, tc.pageQuery), 20)
			if gotLimit != tc.wantLimit || gotOffset != tc.wantOffset {
				t.Fatalf("page/page_size 写法 %q => (%d,%d)，期望 (%d,%d)：两套写法必须落在同一个窗口",
					tc.pageQuery, gotLimit, gotOffset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

// 越界与非法参数一律静默收敛（不报错、不截断到上限）：缺参数取端点缺省值，
// 非法字面量按解析失败处理成缺省值。口径与主仓库 catalog 侧一致，前端依赖它不 400。
func TestPaginationClampsSilently(t *testing.T) {
	limitOffset := []struct {
		query      string
		defaultVal int
		wantLimit  int
		wantOffset int
	}{
		{"", 50, 50, 0},
		{"limit=0", 50, 50, 0},
		{"limit=-3", 50, 50, 0},
		{"limit=101", 50, 50, 0},
		{"limit=abc", 50, 50, 0},
		{"offset=-5", 50, 50, 0},
		{"limit=25&offset=75", 50, 25, 75},
	}
	for _, tc := range limitOffset {
		limit, offset := pagingLimitOffset(pagingContext(t, tc.query), tc.defaultVal)
		if limit != tc.wantLimit || offset != tc.wantOffset {
			t.Fatalf("limit/offset %q（缺省 %d）=> (%d,%d)，期望 (%d,%d)",
				tc.query, tc.defaultVal, limit, offset, tc.wantLimit, tc.wantOffset)
		}
	}

	pageSize := []struct {
		query      string
		defaultVal int
		wantLimit  int
		wantOffset int
	}{
		{"", 20, 20, 0},
		{"page=0", 20, 20, 0},
		{"page=-1", 20, 20, 0},
		{"page=abc", 20, 20, 0},
		{"page_size=0", 20, 20, 0},
		{"page_size=-2", 20, 20, 0},
		{"page_size=101", 20, 20, 0},
		{"page_size=abc", 20, 20, 0},
		{"page=4&page_size=25", 20, 25, 75},
	}
	for _, tc := range pageSize {
		limit, offset := pagingPageSize(pagingContext(t, tc.query), tc.defaultVal)
		if limit != tc.wantLimit || offset != tc.wantOffset {
			t.Fatalf("page/page_size %q（缺省 %d）=> (%d,%d)，期望 (%d,%d)",
				tc.query, tc.defaultVal, limit, offset, tc.wantLimit, tc.wantOffset)
		}
	}
}
