package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeclubM/metafusion-community/internal/upstream"
)

// 合并只广播事件、不改写别人表里的引用，因此引用方必须自己跟随重定向：
// 旧身份直接取会 404，这时再问 /resolve，否则收藏/互动记录会静默消失。
func TestLookupRawFollowsMergedIdentity(t *testing.T) {
	const oldID, newID = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	var resolved bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/catalog/entities/" + oldID:
			w.WriteHeader(http.StatusNotFound) // 合并后旧 id 不再可见
		case "/api/catalog/entities/" + oldID + "/resolve":
			resolved = true
			_, _ = w.Write([]byte(`{"id":"` + newID + `","kind":"work","title":"存活身份","status":"published"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, 2*time.Second)
	raw, err := c.LookupRaw(context.Background(), oldID)
	if err != nil || !resolved || raw == nil {
		t.Fatalf("未跟随合并重定向: err=%v resolved=%v raw=%v", err, resolved, raw)
	}
	var e Entity
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	if e.ID != newID || e.Title != "存活身份" {
		t.Fatalf("拿到的是旧身份或字段缺失: %+v", e)
	}
}

// 真的不存在/不可见时是"没有结果"而不是错误（不能让 collect 把 404 变成更宽松的可见性，
// 也不能把它说成"上游挂了"）；404 是目录的明确回答，因此不该重试。
func TestLookupRawUnresolvableIsNotAnError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	raw, err := c.LookupRaw(context.Background(), "33333333-3333-3333-3333-333333333333")
	if err != nil || raw != nil {
		t.Fatalf("不可见应是 (nil, nil)，实际 (%v, %v)", raw, err)
	}
	if raw, err = c.LookupRaw(context.Background(), ""); err != nil || raw != nil {
		t.Fatalf("空 id 应是 (nil, nil) 且不发请求，实际 (%v, %v)", raw, err)
	}
	// 一条旧身份只打两次：实体端点 + /resolve。重试只针对超时/连接/5xx/429，404 不重试。
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("404 不该重试：期望 2 次请求（实体 + /resolve），实际 %d 次", n)
	}
}

// 上游 503 时三个读方法都必须给出"上游不可用"，不能折成"不存在"：
// 调用方据此回 503 + upstream_unavailable，而不是渲染 404 或空列表。
func TestLookupReportsUpstreamUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	ctx := context.Background()
	const id = "44444444-4444-4444-4444-444444444444"

	if raw, err := c.LookupRaw(ctx, id); raw != nil || !upstream.IsUnavailable(err) {
		t.Fatalf("LookupRaw 应回上游不可用: raw=%v err=%v", raw, err)
	}
	if e, err := c.Lookup(ctx, id); e.ID != "" || !upstream.IsUnavailable(err) {
		t.Fatalf("Lookup 应回上游不可用: e=%+v err=%v", e, err)
	}
	out, err := c.LookupMany(ctx, []string{id, "55555555-5555-5555-5555-555555555555"})
	if !upstream.IsUnavailable(err) {
		t.Fatalf("LookupMany 必须回错误（否则调用方无从分辨「不可见」与「取不到」）: err=%v", err)
	}
	if len(out) != 0 {
		t.Fatalf("上游不可用时不该有结果: %v", out)
	}
}

// 超时同样是"上游不可用"，且重试**有界**：次数等于策略的 Attempts，不是无限重试、也不是只打一次。
func TestLookupRetriesBoundedOnTimeout(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		<-r.Context().Done() // 不回答，等调用方超时
	}))
	defer srv.Close()

	p := upstream.DefaultPolicy("catalog")
	p.Attempts = 3
	p.AttemptTimeout = 40 * time.Millisecond
	p.Budget = 2 * time.Second
	p.BaseBackoff = 10 * time.Millisecond
	p.MaxBackoff = 20 * time.Millisecond
	p.Jitter = 0 // 退避不计抖动，次数断言才稳定
	c := newClient(srv.URL, upstream.New(p))

	if _, err := c.Lookup(context.Background(), "66666666-6666-6666-6666-666666666666"); !upstream.IsUnavailable(err) {
		t.Fatalf("连续超时应回上游不可用: %v", err)
	}
	if n := atomic.LoadInt32(&calls); n != int32(p.Attempts) {
		t.Fatalf("重试次数应恰为策略的 Attempts=%d，实际 %d 次", p.Attempts, n)
	}
}

// ctx 结束时停止投递，并把"结果不完整"如实报上去。
// 旧的 select { case queue <- id: case <-ctx.Done(): break } 里的 break 只跳出 select，
// 整个 ids 会被继续灌进队列——调用方拿到一个"缺条目但没有任何错误"的结果。
func TestLookupManyStopsWhenContextDone(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(`{"id":"x","kind":"work","title":"t","status":"published"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ids := make([]string, 64)
	for i := range ids {
		ids[i] = "id-" + strconv.Itoa(i)
	}
	out, err := c.LookupMany(ctx, ids)
	if err == nil {
		t.Fatal("ctx 已取消时必须回错误：不能把这些实体都当成不可见")
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Fatalf("ctx 已取消不该再发请求，实际 %d 次", n)
	}
	if len(out) != 0 {
		t.Fatalf("ctx 已取消不该有结果: %v", out)
	}
}
