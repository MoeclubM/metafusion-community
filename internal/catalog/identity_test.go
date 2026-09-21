package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeclubM/metafusion-community/internal/upstream"
)

// X01 菱形合并聚合：A→C、B→C、C→D 后，从 D 页必须能展开 {A,B,C,D}。
// 这是目录反向契约落地后的形状（查存活 D 直接回全量历史别名）；社区侧经
// ResolveAliasSet 一处消费，SQL 与调用方都不用改。
const (
	txAliasA  = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	txAliasB  = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	txAliasC  = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	txAliasD  = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	txMissing = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"
)

func identityPayload(canonical string, aliases ...string) string {
	raw, _ := json.Marshal(map[string]any{
		"canonical_id": canonical,
		"aliases":      aliases,
		"entity":       map[string]any{"id": canonical, "kind": "work", "title": "D", "status": "published"},
	})
	return string(raw)
}

func sameSet(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("集合大小不符：got=%v want=%v", got, want)
	}
	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Fatalf("集合缺 %q：got=%v want=%v", id, got, want)
		}
	}
}

// reverseStub 模拟反向契约已落地的目录：查存活 D 直接回全量历史 [A,B,C]。
func reverseStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := map[string]string{
			"/api/catalog/entities/" + txAliasA + "/identity": identityPayload(txAliasD, txAliasA, txAliasC),
			"/api/catalog/entities/" + txAliasB + "/identity": identityPayload(txAliasD, txAliasB, txAliasC),
			"/api/catalog/entities/" + txAliasC + "/identity": identityPayload(txAliasD, txAliasC),
			"/api/catalog/entities/" + txAliasD + "/identity": identityPayload(txAliasD, txAliasA, txAliasB, txAliasC),
		}
		if body, ok := identity[r.URL.Path]; ok {
			_, _ = w.Write([]byte(body))
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/catalog/entities/identity" {
			var in struct {
				IDs []string `json:"ids"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			items := map[string]json.RawMessage{}
			missing := []string{}
			for _, id := range in.IDs {
				var aliases []string
				switch id {
				case txAliasA:
					aliases = []string{txAliasA, txAliasC}
				case txAliasB:
					aliases = []string{txAliasB, txAliasC}
				case txAliasC:
					aliases = []string{txAliasC}
				case txAliasD:
					aliases = []string{txAliasA, txAliasB, txAliasC}
				default:
					missing = append(missing, id)
					continue
				}
				items[id] = json.RawMessage(identityPayload(txAliasD, aliases...))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "missing": missing})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestResolveAliasSetAggregatesMergeDiamond(t *testing.T) {
	srv := reverseStub()
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	ctx := context.Background()

	// D 页聚合：此前 A/B/C 上的评论、收藏、通知参与人都要被覆盖，且去重正确。
	canonical, set, err := c.ResolveAliasSet(ctx, txAliasD)
	if err != nil || canonical != txAliasD {
		t.Fatalf("D 应归一到自身：canonical=%q err=%v", canonical, err)
	}
	sameSet(t, set, txAliasA, txAliasB, txAliasC, txAliasD)

	for requested, want := range map[string][]string{
		txAliasA: {txAliasA, txAliasC, txAliasD},
		txAliasB: {txAliasB, txAliasC, txAliasD},
		txAliasC: {txAliasC, txAliasD},
	} {
		canonical, set, err := c.ResolveAliasSet(ctx, requested)
		if err != nil || canonical != txAliasD {
			t.Fatalf("请求 %s 应归一到 D：canonical=%q err=%v", requested, canonical, err)
		}
		sameSet(t, set, want...)
	}

	if _, set, err := c.ResolveAliasSet(ctx, txMissing); err != nil || len(set) != 0 {
		t.Fatalf("不可见应回空集合无错误：set=%v err=%v", set, err)
	}
}

// forwardOnlyStub 模拟今天的目录（只有前向别名）：查存活 D 拿不到历史 A/B。
// X01-compat：此时读 D 只含 D（历史行待回填），读 A 覆盖前向链 {A,C,D} 正确。
func forwardOnlyStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/catalog/entities/" + txAliasA + "/identity":
			_, _ = w.Write([]byte(identityPayload(txAliasD, txAliasA, txAliasC)))
		case "/api/catalog/entities/" + txAliasD + "/identity":
			_, _ = w.Write([]byte(identityPayload(txAliasD)))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestResolveAliasSetForwardOnlyCompat(t *testing.T) {
	srv := forwardOnlyStub()
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	ctx := context.Background()

	if canonical, set, err := c.ResolveAliasSet(ctx, txAliasA); err != nil || canonical != txAliasD {
		t.Fatalf("读 A 应归一到 D：canonical=%q err=%v", canonical, err)
	} else {
		sameSet(t, set, txAliasA, txAliasC, txAliasD)
	}
	// 反向契约未落地：读 D 暂只含 D，不是正确结果，是已标注的兼容行为。
	if canonical, set, err := c.ResolveAliasSet(ctx, txAliasD); err != nil || canonical != txAliasD {
		t.Fatalf("读 D 应归一到自身：canonical=%q err=%v", canonical, err)
	} else {
		sameSet(t, set, txAliasD)
	}
}

// legacyStub 模拟还没有 /identity 路由的旧目录：只有实体与 /resolve。
// X01-compat：回退到 Lookup 拼 {canonical + 请求 ID}，调用方不动。
func legacyStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/catalog/entities/" + txAliasA:
			w.WriteHeader(http.StatusNotFound)
		case "/api/catalog/entities/" + txAliasA + "/resolve":
			_, _ = w.Write([]byte(`{"id":"` + txAliasD + `","kind":"work","title":"D","status":"published"}`))
		case "/api/catalog/entities/" + txAliasD:
			_, _ = w.Write([]byte(`{"id":"` + txAliasD + `","kind":"work","title":"D","status":"published"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestIdentityFallbackWithoutContract(t *testing.T) {
	srv := legacyStub()
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	ctx := context.Background()

	v, err := c.Identity(ctx, txAliasA)
	if err != nil || v.CanonicalID != txAliasD {
		t.Fatalf("旧目录应回退到 Lookup 归一：%+v err=%v", v, err)
	}
	sameSet(t, append(append([]string{}, v.Aliases...), v.CanonicalID), txAliasA, txAliasD)

	if canonical, set, err := c.ResolveAliasSet(ctx, txAliasA); err != nil || canonical != txAliasD {
		t.Fatalf("旧目录展开应含双 ID：canonical=%q err=%v", canonical, err)
	} else {
		sameSet(t, set, txAliasA, txAliasD)
	}
	if v, err := c.Identity(ctx, txMissing); err != nil || v.CanonicalID != "" {
		t.Fatalf("旧目录不可见应回零值无错误：%+v err=%v", v, err)
	}

	m, err := c.IdentityMany(ctx, []string{txAliasA, txAliasD, txMissing})
	if err != nil {
		t.Fatalf("旧目录批量应回退不断言：%v", err)
	}
	if m[txAliasA].CanonicalID != txAliasD || m[txAliasD].CanonicalID != txAliasD {
		t.Fatalf("旧目录批量映射不符：%v", m)
	}
	if _, ok := m[txMissing]; ok {
		t.Fatalf("不可见不应进批量结果：%v", m)
	}
}

func TestIdentityManyBatch(t *testing.T) {
	srv := reverseStub()
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)

	m, err := c.IdentityMany(context.Background(), []string{txAliasA, txAliasD, txMissing})
	if err != nil {
		t.Fatalf("批量解析不应错：%v", err)
	}
	if m[txAliasA].CanonicalID != txAliasD || m[txAliasD].CanonicalID != txAliasD {
		t.Fatalf("批量映射不符：%v", m)
	}
	sameSet(t, append(append([]string{}, m[txAliasD].Aliases...), m[txAliasD].CanonicalID), txAliasA, txAliasB, txAliasC, txAliasD)
	if _, ok := m[txMissing]; ok {
		t.Fatalf("不可见不应进批量结果：%v", m)
	}
}

func TestIdentityReportsUpstreamUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	ctx := context.Background()

	if _, err := c.Identity(ctx, txAliasA); !upstream.IsUnavailable(err) {
		t.Fatalf("单条应回上游不可用：%v", err)
	}
	if _, err := c.IdentityMany(ctx, []string{txAliasA}); !upstream.IsUnavailable(err) {
		t.Fatalf("批量应回上游不可用：%v", err)
	}
	if _, _, err := c.ResolveAliasSet(ctx, txAliasA); !upstream.IsUnavailable(err) {
		t.Fatalf("展开应回上游不可用：%v", err)
	}
}
