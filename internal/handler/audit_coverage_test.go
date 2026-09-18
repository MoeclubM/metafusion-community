package handler

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/config"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// writeMethods 是"写操作"的判据：审计只记这四种方法（契约 §7 明确 GET 不记）。
var writeMethods = map[string]bool{
	"POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// actionCodePattern 是动作码的命名（契约 §2）：<域>.<过去式动作>，全小写 + 下划线。
var actionCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*.[a-z][a-z0-9_]*$`)

// TestEveryWriteRouteIsAuditedOrExempt 是「写路由覆盖守卫」：
//
// 遍历 gin 路由树里的全部写路由，断言每条要么登记了动作码，要么在豁免表里且带一句理由。
// 新增写端点忘了登记动作码 → 这里失败；这条用例是"操作零留痕"的唯一防线（漏一条不会有任何
// 其它用例或编译错误报出来，只会安静地少一行审计）。
//
// 反向也要查：注册表里的路由必须真实存在，否则删掉/改名端点后注册表会留一条永不命中的死条目，
// 而覆盖守卫却以为"已经覆盖了"。
func TestEveryWriteRouteIsAuditedOrExempt(t *testing.T) {
	routes := writeRoutesOfRegisteredRouter(t)
	if len(routes) == 0 {
		t.Fatal("路由树里没有写路由：路由根本没注册上，覆盖守卫等于空转")
	}
	seen := map[string]bool{}
	for _, route := range routes {
		seen[route] = true
		action, audited := auditActions[route]
		reason, exempt := auditExempt[route]
		switch {
		case audited && exempt:
			t.Fatalf("写路由 %s 同时出现在注册表与豁免表里：只能二选一", route)
		case audited:
			if !actionCodePattern.MatchString(action) {
				t.Fatalf("写路由 %s 的动作码 %q 不符合契约 §2 的 <域>.<动作> 命名", route, action)
			}
		case exempt:
			if strings.TrimSpace(reason) == "" {
				t.Fatalf("写路由 %s 在豁免表里但没有理由：无理由的豁免等于漏登记", route)
			}
		default:
			t.Fatalf("写路由 %s 既没登记动作码也不在豁免表里：新增写端点必须留痕（auditActions / auditExempt）", route)
		}
	}
	for route := range auditActions {
		if !seen[route] {
			t.Fatalf("注册表里的 %s 不是一条真实路由：端点改名或删除后没有同步注册表", route)
		}
	}
	for route := range auditExempt {
		if !seen[route] {
			t.Fatalf("豁免表里的 %s 不是一条真实路由：端点改名或删除后没有同步豁免表", route)
		}
	}
}

// TestAuditActionCodesAreDistinctPerRoute：一个路由对应一个动作码（反向不要求唯一：
// 同一个业务动作将来可能有多个入口）。重复登记同一个路由会让两处定义漂移。
func TestAuditActionCodesAreDistinctPerRoute(t *testing.T) {
	byAction := map[string][]string{}
	for route, action := range auditActions {
		byAction[action] = append(byAction[action], route)
	}
	for action, routes := range byAction {
		if len(routes) > 1 {
			sort.Strings(routes)
			t.Fatalf("动作码 %s 被 %d 条路由共用（%s）：一条路由一个码，共用会让审计看不出走的哪个入口",
				action, len(routes), strings.Join(routes, "、"))
		}
	}
}

// writeRoutesOfRegisteredRouter 从真实注册结果里取写路由（"METHOD /path"，路径带 /api 前缀），
// 用的是生产同一条注册路径（handler.Register），因此新增端点会自动出现在这里。
func writeRoutesOfRegisteredRouter(t *testing.T) []string {
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
		if writeMethods[route.Method] {
			out = append(out, route.Method+" "+route.Path)
		}
	}
	sort.Strings(out)
	return out
}
