package handler

import (
	"sort"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/config"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// frozenRoutes 是切流契约：这些路径与主仓库 modules/catalog 包**逐字一致**，
// 前端与第三方客户端在切流时不需要任何改动。这个测试是"契约冻结"的执行者——
// 任何人改动路由都会在这里失败，而不是等线上 404 才发现。
//
// 例外：本服务新增、主仓库里**没有对应实现**的端点——置顶与板块配置两条运营写接口
// （把"已声明但没落地的权限码"补成真实能力时新增的），以及前端已在调用、四仓都没有实现的
// 私信与用户互动统计。它们不是切流契约的一部分，但同样要进这份清单：
// 路由是"前端调得到"的唯一保证，漏登记就会变成线上 404。
var frozenRoutes = []string{
	"DELETE /api/community/posts/:id",
	"DELETE /api/community/topics/:id",
	"DELETE /api/community/topics/:id/posts/:postId",
	"GET /api/community/boards",
	"GET /api/community/entities/:id/collections",
	"GET /api/community/entities/:id/posts",
	"GET /api/community/feed",
	"GET /api/community/posts/:id",
	"GET /api/community/topic-tags",
	"GET /api/community/topics",
	"GET /api/community/topics/:id",
	"GET /api/favorites/mine",
	"GET /api/favorites/status",
	"GET /api/messages/with/:id",
	"GET /api/records/entities/:id",
	"GET /api/users/:id/favorites",
	"GET /api/users/:id/stats",
	"POST /api/community/entities/:id/posts",
	"POST /api/community/topics",
	"POST /api/community/topics/:id/posts",
	"POST /api/favorites/toggle",
	"POST /api/messages/with/:id",
	"PUT /api/community/boards/:code",
	"PUT /api/community/topics/:id/pin",
	"PUT /api/records/entities/:id",
}

func registeredRoutes(t *testing.T) []string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	verifier, err := auth.New(config.Config{JWTIssuer: "https://findverse.cc/api", JWTAudience: "metafusion"})
	if err != nil {
		t.Fatalf("verifier: %v", err)
	}
	r := gin.New()
	New(&store.Store{}, catalog.New("", 0), verifier).Register(r)
	out := []string{}
	for _, route := range r.Routes() {
		out = append(out, route.Method+" "+route.Path)
	}
	sort.Strings(out)
	return out
}

func TestRoutesMatchFrozenContract(t *testing.T) {
	got := registeredRoutes(t)
	want := append([]string{}, frozenRoutes...)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("路由数量 = %d, 期望 %d\n实际:\n%s", len(got), len(want), join(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 条路由不一致：\n实际 %s\n期望 %s\n实际全集:\n%s", i, got[i], want[i], join(got))
		}
	}
}

func join(items []string) string {
	out := ""
	for _, s := range items {
		out += s + "\n"
	}
	return out
}
