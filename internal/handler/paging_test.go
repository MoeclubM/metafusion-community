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

func TestPaginationPageSize(t *testing.T) {
	cases := []struct {
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
	for _, tc := range cases {
		limit, offset := pagingPageSize(pagingContext(t, tc.query), tc.defaultVal)
		if limit != tc.wantLimit || offset != tc.wantOffset {
			t.Fatalf("page/page_size %q（缺省 %d）=> (%d,%d)，期望 (%d,%d)",
				tc.query, tc.defaultVal, limit, offset, tc.wantLimit, tc.wantOffset)
		}
	}
}
