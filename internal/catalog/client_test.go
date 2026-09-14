package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
	raw, ok := c.LookupRaw(context.Background(), oldID)
	if !ok || !resolved {
		t.Fatalf("未跟随合并重定向: ok=%v resolved=%v", ok, resolved)
	}
	var e Entity
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatal(err)
	}
	if e.ID != newID || e.Title != "存活身份" {
		t.Fatalf("拿到的是旧身份或字段缺失: %+v", e)
	}
}

// 真的不存在/不可见时仍是 false：collect 不能把 404 变成"更宽松的可见性"。
func TestLookupRawStillFailsWhenUnresolvable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	if _, ok := c.LookupRaw(context.Background(), "33333333-3333-3333-3333-333333333333"); ok {
		t.Fatal("不可解析的实体不应返回结果")
	}
	if _, ok := c.LookupRaw(context.Background(), ""); ok {
		t.Fatal("空 id 不应发起请求")
	}
}
