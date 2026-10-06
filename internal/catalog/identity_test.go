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
		"complete":     true,
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
			"/api/catalog/entities/" + txAliasA + "/identity": identityPayload(txAliasD, txAliasA, txAliasB, txAliasC),
			"/api/catalog/entities/" + txAliasB + "/identity": identityPayload(txAliasD, txAliasA, txAliasB, txAliasC),
			"/api/catalog/entities/" + txAliasC + "/identity": identityPayload(txAliasD, txAliasA, txAliasB, txAliasC),
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
				case txAliasA, txAliasB, txAliasC, txAliasD:
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
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
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
		txAliasA: {txAliasA, txAliasB, txAliasC, txAliasD},
		txAliasB: {txAliasA, txAliasB, txAliasC, txAliasD},
		txAliasC: {txAliasA, txAliasB, txAliasC, txAliasD},
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

// forwardOnlyStub 模拟旧目录的前向别名响应，没有 complete 全集确认。
func forwardOnlyStub() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/catalog/entities/" + txAliasA + "/identity":
			_, _ = w.Write([]byte(`{"canonical_id":"` + txAliasD + `","aliases":["` + txAliasA + `","` + txAliasC + `"],"entity":{"id":"` + txAliasD + `"}}`))
		case "/api/catalog/entities/" + txAliasD + "/identity":
			_, _ = w.Write([]byte(`{"canonical_id":"` + txAliasD + `","aliases":[],"entity":{"id":"` + txAliasD + `"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not_found"}`))
		}
	}))
}

func TestResolveAliasSetRejectsForwardOnlyResponse(t *testing.T) {
	srv := forwardOnlyStub()
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	ctx := context.Background()

	for _, requested := range []string{txAliasA, txAliasD} {
		if canonical, set, err := c.ResolveAliasSet(ctx, requested); !upstream.IsUnavailable(err) || canonical != "" || len(set) != 0 {
			t.Fatalf("旧前向响应必须拒绝聚合：canonical=%q set=%v err=%v", canonical, set, err)
		}
	}
}

func TestIdentityRejectsIncompleteAliasSet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := map[string]any{
			"canonical_id": txAliasD,
			"aliases":      []string{txAliasA},
			"complete":     false,
			"entity":       map[string]any{"id": txAliasD},
		}
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": map[string]any{txAliasA: payload}, "missing": []string{}})
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	if v, err := c.Identity(context.Background(), txAliasA); !upstream.IsUnavailable(err) || v.CanonicalID != "" {
		t.Fatalf("单条不完整响应必须回依赖错误：v=%+v err=%v", v, err)
	}
	if items, err := c.IdentityMany(context.Background(), []string{txAliasA}); !upstream.IsUnavailable(err) || items != nil {
		t.Fatalf("批量不完整响应不得返回部分结果：items=%v err=%v", items, err)
	}
}

// legacyStub 模拟缺少身份契约的目录实例。
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

func TestIdentityDoesNotFallbackWithoutContract(t *testing.T) {
	srv := legacyStub()
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	ctx := context.Background()

	if v, err := c.Identity(ctx, txAliasA); err == nil || v.CanonicalID != "" {
		t.Fatalf("缺少身份契约应返回上游错误：%+v err=%v", v, err)
	}
	if canonical, set, err := c.ResolveAliasSet(ctx, txAliasA); err == nil || canonical != "" || len(set) != 0 {
		t.Fatalf("缺少身份契约不得拼别名：canonical=%q set=%v err=%v", canonical, set, err)
	}
	if v, err := c.Identity(ctx, txMissing); err == nil || v.CanonicalID != "" {
		t.Fatalf("旧目录缺失接口应返回上游错误：%+v err=%v", v, err)
	}

	if _, err := c.IdentityMany(ctx, []string{txAliasA, txAliasD, txMissing}); err == nil {
		t.Fatal("批量身份接口缺失应返回契约错误")
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
