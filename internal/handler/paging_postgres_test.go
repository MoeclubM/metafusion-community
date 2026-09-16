package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
	"github.com/MoeclubM/metafusion-community/internal/testutil"
)

// 真库上的窗口等价性：两套分页写法必须落在同一个窗口。
//
// 做法是把"未分页"的响应当基准，再断言分页请求拿到的是基准的同一个切片：
// 同一个逻辑窗口（第 2 页、每页 2 条 = 第 3、4 条）在两套参数下必须一致。
// 期望值全部来自被测端点自身，因此不依赖表里的既有数据，也不受其它用例残留影响。
//
// 隔离：主题写进本用例专用的板块 code，收藏挂在专用的 sub 上，跑完删干净。
func TestPaginationWindowsAgainstPostgres(t *testing.T) {
	dsn := testutil.DSN(t)
	db := testutil.Database(t)
	ctx := context.Background()

	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	if err = s.Init(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	const probeBoard = "paging_probe"
	probeUser := uuid.NewString()
	cleanup := func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM community.favorites WHERE user_id=$1", probeUser)
		_, _ = db.ExecContext(ctx, "DELETE FROM community.topics WHERE board_code=$1", probeBoard)
		_, _ = db.ExecContext(ctx, "DELETE FROM community.boards WHERE code=$1", probeBoard)
	}
	cleanup()
	t.Cleanup(cleanup)

	if _, err = db.ExecContext(ctx, `INSERT INTO community.boards(code,name,description,color,icon,sort_order,is_enabled,show_in_feed)
		VALUES($1,'分页探针板块','','slate','Hash',999,true,false)`, probeBoard); err != nil {
		t.Fatalf("插入探针板块: %v", err)
	}
	// 主题列表按 last_activity_at DESC 排序：让越靠前的越新，顺序确定。
	for i := 0; i < 5; i++ {
		if _, err = db.ExecContext(ctx, `INSERT INTO community.topics(id,board_code,author_id,author_name,title,body,language,created_at,updated_at,last_activity_at)
			VALUES($1,$2,$3,'tester',$4,'body','zh',now(),now(),now() - make_interval(mins => $5))`,
			uuid.NewString(), probeBoard, probeUser, fmt.Sprintf("分页窗口-主题-%d", i), i); err != nil {
			t.Fatalf("插入主题 %d: %v", i, err)
		}
	}
	for i := 0; i < 5; i++ {
		if _, err = db.ExecContext(ctx, `INSERT INTO community.favorites(user_id,target_type,target_id,created_at)
			VALUES($1,'work',$2,now() - make_interval(mins => $3))`, probeUser, uuid.NewString(), i); err != nil {
			t.Fatalf("插入收藏 %d: %v", i, err)
		}
	}

	// 目录桩：收藏列表逐条问实体可见性，这里把请求路径里的实体 id 原样回显。
	catalogStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimSuffix(r.URL.Path, "/resolve")
		id := path[strings.LastIndex(path, "/")+1:]
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "kind": "work", "title": "探针作品", "status": "published"})
	}))
	defer catalogStub.Close()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	router := gin.New()
	New(s, catalog.New(catalogStub.URL, 2*time.Second), newVerifier(t, srv.URL)).Register(router)
	token := signTokenWith(t, key, kid, probeUser, "editor", nil, nil)

	items := func(path, bearer string) []map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("GET %s => HTTP %d: %s", path, w.Code, w.Body.String())
		}
		var payload map[string]any
		if err = json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("GET %s 响应不是 JSON 对象: %v %s", path, err, w.Body.String())
		}
		rawItems, ok := payload["items"].([]any)
		if !ok {
			t.Fatalf("GET %s 响应缺少 items 数组: %s", path, w.Body.String())
		}
		out := []map[string]any{}
		for _, raw := range rawItems {
			if m, ok := raw.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	idsOf := func(rows []map[string]any, key string) []string {
		out := []string{}
		for _, row := range rows {
			if v, ok := row[key].(string); ok {
				out = append(out, v)
			}
		}
		return out
	}
	assertWindow := func(name string, full, window []string) {
		t.Helper()
		if len(full) != 5 {
			t.Fatalf("%s：基准（未分页）条数 = %d，期望 5（%v）", name, len(full), full)
		}
		want := full[2:4]
		if len(window) != len(want) {
			t.Fatalf("%s：分页窗口条数 = %d，期望 %d", name, len(window), len(want))
		}
		for i := range want {
			if window[i] != want[i] {
				t.Fatalf("%s：第 2 页第 %d 条 = %s，基准第 %d 条 = %s（两套写法必须给出同一个窗口）",
					name, i+1, window[i], i+3, want[i])
			}
		}
	}

	// 写法一：limit/offset（/api/community/topics），缺省页宽 30、上限 100。
	fullTopics := idsOf(items("/api/community/topics?board_code="+probeBoard+"&limit=100&offset=0", ""), "id")
	windowTopics := idsOf(items("/api/community/topics?board_code="+probeBoard+"&limit=2&offset=2", ""), "id")
	assertWindow("limit/offset（主题列表）", fullTopics, windowTopics)

	// 写法二：page/page_size（/api/favorites/mine），缺省页宽 20。
	fullFavs := idsOf(items("/api/favorites/mine?page=1&page_size=100", token), "target_id")
	windowFavs := idsOf(items("/api/favorites/mine?page=2&page_size=2", token), "target_id")
	assertWindow("page/page_size（我的收藏）", fullFavs, windowFavs)
}
