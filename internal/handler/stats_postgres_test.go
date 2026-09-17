package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// 用户互动统计的真库用例：造数据 → 打端点 → 断言三个数字，并把"哪些行不算"的反例一起钉住。
// 夹具复用 opsFixture（真库 + 目录桩 + 路由），身份用全新的 uuid，跑完清干净。
func TestUserStatsAgainstPostgres(t *testing.T) {
	ctx, db, router, _, _ := opsFixture(t)
	probe, other := uuid.NewString(), uuid.NewString()
	cleanup := func() { cleanupStats(ctx, db, probe, other) }
	cleanup()
	t.Cleanup(cleanup)

	// 造数据（板块用 opsFixture 播种好的 qa / comment）：
	//   * 3 个论坛主题 + 1 条实体短评（评论板块，与主题共用 community.topics）；
	//   * 5 条楼中回复挂在第一个主题上；
	//   * 4 条收藏（target_id 各不相同：主键是 (user_id,target_type,target_id)）；
	//   * 另有一个用户的 1 个主题 + 1 条回复 + 1 条收藏，用来确认不串号。
	topicIDs := []string{}
	for i := 0; i < 3; i++ {
		id := uuid.NewString()
		topicIDs = append(topicIDs, id)
		exec(t, ctx, db, `INSERT INTO community.topics(id,board_code,author_id,author_name,title,body)
			VALUES($1,'qa',$2,'tester',$3,'正文')`, id, probe, fmt.Sprintf("统计-主题-%d", i))
	}
	// 短评锚定的实体 id 只是外部引用：统计不查目录服务，也不该因为实体不可见而改变。
	exec(t, ctx, db, `INSERT INTO community.topics(id,board_code,author_id,author_name,title,body,entity_id)
		VALUES($1,'comment',$2,'tester','','统计-短评',$3)`, uuid.NewString(), probe, uuid.NewString())
	for i := 0; i < 5; i++ {
		exec(t, ctx, db, `INSERT INTO community.posts(id,topic_id,author_id,author_name,body,post_number)
			VALUES($1,$2,$3,'tester',$4,$5)`, uuid.NewString(), topicIDs[0], probe, fmt.Sprintf("统计-回复-%d", i), i+1)
	}
	for i := 0; i < 4; i++ {
		exec(t, ctx, db, `INSERT INTO community.favorites(user_id,target_type,target_id) VALUES($1,'work',$2)`, probe, uuid.NewString())
	}
	otherTopic := uuid.NewString()
	exec(t, ctx, db, `INSERT INTO community.topics(id,board_code,author_id,author_name,title,body)
		VALUES($1,'qa',$2,'tester','别人的主题','正文')`, otherTopic, other)
	exec(t, ctx, db, `INSERT INTO community.posts(id,topic_id,author_id,author_name,body,post_number)
		VALUES($1,$2,$3,'tester','别人的回复',1)`, uuid.NewString(), otherTopic, other)
	exec(t, ctx, db, `INSERT INTO community.favorites(user_id,target_type,target_id) VALUES($1,'work',$2)`, other, uuid.NewString())

	// 匿名可读（用户主页对未登录访客也展示），形状 {"stats":{topics_created,comments_created,favorites_count}}。
	stats := fetchStats(t, router, probe)
	want := map[string]float64{"topics_created": 3, "comments_created": 5, "favorites_count": 4}
	if len(stats) != len(want) {
		t.Fatalf("stats 字段集合 = %v，期望恰好 %v", stats, want)
	}
	for key, value := range want {
		got, ok := stats[key].(float64)
		if !ok {
			t.Fatalf("stats.%s 缺失或不是数字：%v", key, stats)
		}
		if got != value {
			t.Fatalf("stats.%s = %v，期望 %v（全部读数：%v）", key, got, value, stats)
		}
	}

	// 口径自证：favorites_count 必须与公开收藏列表的 total 一致（同一个数字不能有两套口径）。
	w := opsCall(t, router, http.MethodGet, "/api/users/"+probe+"/favorites", "", "")
	if w.Code != 200 {
		t.Fatalf("公开收藏列表 HTTP %d：%s", w.Code, w.Body.String())
	}
	list := struct {
		Total int `json:"total"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析收藏列表: %v（%s）", err, w.Body.String())
	}
	if float64(list.Total) != stats["favorites_count"] {
		t.Fatalf("统计的收藏数 %v 与公开列表 total %d 不一致", stats["favorites_count"], list.Total)
	}

	// 短评不计入任何一个数字（评论不是主题、也不在 posts 表里）：宁可少算，也不重复计数。
	exec(t, ctx, db, `DELETE FROM community.topics WHERE board_code='comment' AND author_id=$1`, probe)
	if got := fetchStats(t, router, probe); got["topics_created"].(float64) != 3 || got["comments_created"].(float64) != 5 {
		t.Fatalf("删掉短评后主题/回复数不该变：%v", got)
	}

	// 非法 uuid：404（不把 pq 解析错误兜成 500）；没有任何互动记录的 id：三个 0
	// （账号不归本服务，无法区分"没有记录"与"没有这个人"，与收藏列表用空列表表示同一个意思）。
	if w := opsCall(t, router, http.MethodGet, "/api/users/not-a-uuid/stats", "", ""); w.Code != 404 {
		t.Fatalf("非法用户 id 应 404，实际 %d（%s）", w.Code, w.Body.String())
	}
	empty := fetchStats(t, router, uuid.NewString())
	for key, value := range empty {
		if value.(float64) != 0 {
			t.Fatalf("没有互动记录的 id 应全为 0，实际 %s=%v", key, value)
		}
	}
}

// fetchStats 打统计端点并解出 stats 对象：信封形状不符直接失败（前端按 stats 取键）。
func fetchStats(t *testing.T, router http.Handler, userID string) map[string]any {
	t.Helper()
	w := opsCall(t, router, http.MethodGet, "/api/users/"+userID+"/stats", "", "")
	if w.Code != 200 {
		t.Fatalf("GET /api/users/%s/stats HTTP %d：%s", userID, w.Code, w.Body.String())
	}
	envelope := struct {
		Stats map[string]any `json:"stats"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Stats == nil {
		t.Fatalf("统计响应应为 {\"stats\":{…}}：%s", w.Body.String())
	}
	return envelope.Stats
}

// cleanupStats 清掉这两个身份造出来的行：用例共用一个测试库，不能留残数据影响别的用例。
func cleanupStats(ctx context.Context, db *sql.DB, ids ...string) {
	for _, stmt := range []string{
		"DELETE FROM community.posts WHERE author_id = ANY($1::uuid[])",
		"DELETE FROM community.topics WHERE author_id = ANY($1::uuid[])",
		"DELETE FROM community.favorites WHERE user_id = ANY($1::uuid[])",
	} {
		_, _ = db.ExecContext(ctx, stmt, pq.Array(ids))
	}
}

func exec(t *testing.T, ctx context.Context, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("造数据 %.48s…: %v", query, err)
	}
}
