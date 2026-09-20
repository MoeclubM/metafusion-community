package catalog

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// X01：请求别名 A 合并到 B 后，归一必须落到 B（新写归一），读取集合必须含双 ID。
func TestResolveCanonicalFollowsMerge(t *testing.T) {
	const oldID = "11111111-1111-1111-1111-111111111111"
	const newID = "22222222-2222-2222-2222-222222222222"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/catalog/entities/" + oldID:
			w.WriteHeader(http.StatusNotFound)
		case "/api/catalog/entities/" + oldID + "/resolve":
			_, _ = w.Write([]byte(`{"id":"` + newID + `","kind":"work","title":"B","status":"published"}`))
		case "/api/catalog/entities/" + newID:
			_, _ = w.Write([]byte(`{"id":"` + newID + `","kind":"work","title":"B","status":"published"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	got, err := c.ResolveCanonical(context.Background(), oldID)
	if err != nil || got != newID {
		t.Fatalf("别名应归一到 canonical：got=%q err=%v", got, err)
	}
	got, err = c.ResolveCanonical(context.Background(), newID)
	if err != nil || got != newID {
		t.Fatalf("canonical 自身应保持：got=%q err=%v", got, err)
	}
	if set := AliasSet(newID, oldID); len(set) != 2 || set[0] != oldID || set[1] != newID {
		t.Fatalf("读取集合应含请求别名与 canonical：%v", set)
	}
	if set := AliasSet(newID, newID); len(set) != 1 || set[0] != newID {
		t.Fatalf("同 ID 去重：%v", set)
	}
}

// 不可见返回空串（调用方按 404），上游故障返回错误（调用方按 503）。
func TestResolveCanonicalInvisibleAndUnavailable(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	c := New(notFound.URL, 2*time.Second)
	if got, err := c.ResolveCanonical(context.Background(), "33333333-3333-3333-3333-333333333333"); err != nil || got != "" {
		t.Fatalf("不可见应回空串无错误：got=%q err=%v", got, err)
	}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer broken.Close()
	c2 := New(broken.URL, 2*time.Second)
	if _, err := c2.ResolveCanonical(context.Background(), "33333333-3333-3333-3333-333333333333"); err == nil {
		t.Fatal("上游故障必须回错误（调用方按 503）")
	}
}

// 批量归一：可见的落 canonical，不可见的记空串；上游故障整批报错。
func TestResolveManyMapsToCanonical(t *testing.T) {
	const oldID = "11111111-1111-1111-1111-111111111111"
	const newID = "22222222-2222-2222-2222-222222222222"
	const missing = "33333333-3333-3333-3333-333333333333"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/catalog/entities/" + oldID:
			w.WriteHeader(http.StatusNotFound)
		case "/api/catalog/entities/" + oldID + "/resolve":
			_, _ = w.Write([]byte(`{"id":"` + newID + `","kind":"work","title":"B","status":"published"}`))
		case "/api/catalog/entities/" + newID:
			_, _ = w.Write([]byte(`{"id":"` + newID + `","kind":"work","title":"B","status":"published"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	got, err := c.ResolveMany(context.Background(), []string{oldID, newID, missing})
	if err != nil {
		t.Fatalf("批量归一不应错：%v", err)
	}
	if got[oldID] != newID || got[newID] != newID || got[missing] != "" {
		t.Fatalf("批量映射不符：%v", got)
	}
}
