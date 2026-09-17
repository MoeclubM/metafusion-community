package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// 私信的真库往返：发 → 收 → 第三者看不见 → 分页窗口 → 索引与兜底约束。
// 夹具复用 opsFixture（真库 + 可验签的 router），身份用三个全新的 uuid，跑完清干净。
func TestDirectMessagesAgainstPostgres(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	alice, bob, mallory := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cleanup := func() { cleanupDirectMessages(ctx, db, alice, bob, mallory) }
	cleanup()
	t.Cleanup(cleanup)
	tokenAlice := signTokenWith(t, key, kid, alice, "user", []string{"member"}, []string{auth.PermissionPostCreate})
	tokenBob := signTokenWith(t, key, kid, bob, "user", []string{"member"}, []string{auth.PermissionPostCreate})
	tokenMallory := signTokenWith(t, key, kid, mallory, "user", []string{"member"}, []string{auth.PermissionPostCreate})

	// 1) 匿名：两个端点都 401 authentication_required（与其它私有数据接口同一口径）。
	for _, tc := range []struct{ method, path, payload string }{
		{http.MethodGet, "/api/messages/with/" + bob, ""},
		{http.MethodPost, "/api/messages/with/" + bob, `{"body":"在吗"}`},
	} {
		w := opsCall(t, router, tc.method, tc.path, tc.payload, "")
		if w.Code != 401 || opsErrorCode(t, w) != "authentication_required" {
			t.Fatalf("%s %s 匿名应 401 authentication_required，实际 %d（%s）", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	// 2) 非法 uuid：404。送进 uuid 列只会拿到 pq 的解析错误，再被兜成 500 回显给客户端。
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		w := opsCall(t, router, method, "/api/messages/with/not-a-uuid", `{"body":"x"}`, tokenAlice)
		if w.Code != 404 || opsErrorCode(t, w) != "not_found" {
			t.Fatalf("%s 非法 id 应 404 not_found，实际 %d（%s）", method, w.Code, w.Body.String())
		}
	}

	// 3) 不能给自己发：400 invalid_recipient（不看正文有没有写错）。
	if w := opsCall(t, router, http.MethodPost, "/api/messages/with/"+alice, `{"body":"自说自话"}`, tokenAlice); w.Code != 400 || opsErrorCode(t, w) != "invalid_recipient" {
		t.Fatalf("给自己发应 400 invalid_recipient，实际 %d（%s）", w.Code, w.Body.String())
	}
	// 读自己的会话不报错、恒空：库里不可能有自己发给自己的行（CHECK 约束），那只是没有消息的会话。
	if items, total := listMessages(t, router, tokenAlice, alice, "page=1&page_size=20"); total != 0 || len(items) != 0 {
		t.Fatalf("与自己应是没有消息的会话，实际 total=%d items=%d", total, len(items))
	}

	// 4) 正文边界：空 / 纯空白 / 超 4000 字符都 400 invalid_body，且一行都不落库。
	for _, payload := range []string{
		`{"body":""}`,
		`{"body":"   \n\t "}`,
		`{"body":"` + strings.Repeat("汉", maxMessageBodyRunes+1) + `"}`,
	} {
		w := opsCall(t, router, http.MethodPost, "/api/messages/with/"+bob, payload, tokenAlice)
		if w.Code != 400 || opsErrorCode(t, w) != "invalid_body" {
			t.Fatalf("载荷 %.40s… 应 400 invalid_body，实际 %d（%s）", payload, w.Code, w.Body.String())
		}
	}
	if n := countDirectMessages(t, db, alice); n != 0 {
		t.Fatalf("被拒的请求不得落库，实际 %d 行", n)
	}

	// 5) 发：响应形状是 {"message":{...}}（前端按这个包裹解析，不是裸对象），两侧空白被裁掉。
	w := opsCall(t, router, http.MethodPost, "/api/messages/with/"+bob, `{"body":"  第一条  "}`, tokenAlice)
	if w.Code != 200 {
		t.Fatalf("发信 HTTP %d：%s", w.Code, w.Body.String())
	}
	envelope := struct {
		Message map[string]any `json:"message"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope.Message == nil {
		t.Fatalf("发信响应应为 {\"message\":{…}}：%s", w.Body.String())
	}
	firstID, _ := envelope.Message["id"].(string)
	if firstID == "" {
		t.Fatalf("发信未返回 id：%s", w.Body.String())
	}
	if envelope.Message["sender_id"] != alice || envelope.Message["recipient_id"] != bob {
		t.Fatalf("发信响应的参与者不符：%s", w.Body.String())
	}
	if envelope.Message["body"] != "第一条" {
		t.Fatalf("正文两侧空白未裁剪：%s", w.Body.String())
	}
	if at, _ := envelope.Message["created_at"].(string); at == "" {
		t.Fatalf("发信响应缺少 created_at：%s", w.Body.String())
	}

	// 6) 收：对方读到同一批行（视角不同，行不变），形状 {items:[{id,sender_id,recipient_id,body,created_at}],total}。
	items, total := listMessages(t, router, tokenBob, alice, "page=1&page_size=20")
	if total != 1 || len(items) != 1 {
		t.Fatalf("收件人应看到 1 条、total=1，实际 total=%d items=%d", total, len(items))
	}
	for field, want := range map[string]any{"id": firstID, "sender_id": alice, "recipient_id": bob, "body": "第一条"} {
		if items[0][field] != want {
			t.Fatalf("列表条目 %s = %v，期望 %v（%v）", field, items[0][field], want, items[0])
		}
	}

	// 7) 第三者看不见：mallory 与两人的会话都是空页（"只能看见自己参与的会话"是查询结构保证的）。
	for _, peer := range []string{alice, bob} {
		if items, total := listMessages(t, router, tokenMallory, peer, "page=1&page_size=20"); total != 0 || len(items) != 0 {
			t.Fatalf("第三者看 %s 的会话应为空，实际 total=%d items=%d", peer, total, len(items))
		}
	}

	// 8) 分页：清掉这一对的行，写 25 条时间显式错开的消息再翻页。
	//    时间必须显式错开：now() 是事务时间，同一事务里插入的多行会撞在一起，窗口就不确定了。
	cleanupDirectMessages(ctx, db, alice, bob)
	for i := 0; i < 25; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO community.direct_messages(id,sender_id,recipient_id,body,created_at)
			VALUES($1,$2,$3,$4,now() - make_interval(secs => $5))`,
			uuid.NewString(), bob, alice, fmt.Sprintf("分页-消息-%02d", i), i); err != nil {
			t.Fatalf("插入第 %d 条: %v", i, err)
		}
	}
	// 这 25 条是 bob → alice：同时验证"收件人视角"能看到整段会话，而不只是自己发出的。
	page1, total1 := listMessages(t, router, tokenAlice, bob, "page=1&page_size=20")
	if total1 != 25 || len(page1) != 20 {
		t.Fatalf("第 1 页应 20 条 / total=25，实际 %d 条 / total=%d", len(page1), total1)
	}
	if page1[0]["body"] != "分页-消息-00" || page1[19]["body"] != "分页-消息-19" {
		t.Fatalf("第一页应按时间倒序给最新的 20 条：首条 %v、末条 %v", page1[0]["body"], page1[19]["body"])
	}
	page2, total2 := listMessages(t, router, tokenAlice, bob, "page=2&page_size=20")
	if total2 != 25 || len(page2) != 5 {
		t.Fatalf("第 2 页应 5 条 / total=25，实际 %d 条 / total=%d", len(page2), total2)
	}
	if page2[0]["body"] != "分页-消息-20" || page2[4]["body"] != "分页-消息-24" {
		t.Fatalf("第 2 页应是更早的 5 条：首条 %v、末条 %v", page2[0]["body"], page2[4]["body"])
	}
	if page3, total3 := listMessages(t, router, tokenAlice, bob, "page=3&page_size=20"); total3 != 25 || len(page3) != 0 {
		t.Fatalf("第 3 页应为空且 total 仍为 25，实际 %d 条 / total=%d", len(page3), total3)
	}
	// 越界参数静默收敛成缺省页宽 20（与 /favorites/mine 同一口径，不返回 400）。
	def, _ := listMessages(t, router, tokenAlice, bob, "page=1&page_size=0")
	if len(def) != 20 || def[0]["body"] != page1[0]["body"] {
		t.Fatalf("page_size 越界应取缺省 20，实际 %d 条", len(def))
	}
	// 反向视角（会话的另一头）拿到同一批行。
	back, _ := listMessages(t, router, tokenBob, alice, "page=1&page_size=2")
	if len(back) != 2 || back[0]["body"] != "分页-消息-00" || back[0]["sender_id"] != bob {
		t.Fatalf("会话另一头应看到同一批行，实际 %v", back)
	}

	// 9) 索引真的建出来了：会话的倒序扫描全靠它（表达式本身由 store 的结构用例静态冻结）。
	var indexDef string
	if err := db.QueryRowContext(ctx, `SELECT indexdef FROM pg_indexes
		WHERE schemaname='community' AND tablename='direct_messages' AND indexname='direct_messages_conversation'`).Scan(&indexDef); err != nil {
		t.Fatalf("direct_messages_conversation 索引不存在: %v", err)
	}
	lower := strings.ToLower(indexDef)
	for _, want := range []string{"least(sender_id, recipient_id)", "greatest(sender_id, recipient_id)", "created_at desc", "id desc"} {
		if !strings.Contains(lower, want) {
			t.Fatalf("会话索引缺少 %q：%s", want, indexDef)
		}
	}

	// 索引"建出来了"不等于"用得上"：接口的 WHERE 只有与索引表达式逐字对齐才会命中，
	// 写成 (sender_id=$1 AND recipient_id=$2) OR (…) 就会退化成全表扫 + 排序——
	// 读法直观得多，却没有任何用例会失败。所以这里关掉 seqscan 问一次计划，
	// 断言规划器选的确实是这条索引（删索引、改表达式、改排序方向都会在这里失败）。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务: %v", err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("关掉 seqscan: %v", err)
	}
	planRows, err := tx.QueryContext(ctx, `EXPLAIN SELECT id FROM community.direct_messages
		WHERE LEAST(sender_id,recipient_id)=LEAST($1::uuid,$2::uuid)
		  AND GREATEST(sender_id,recipient_id)=GREATEST($1::uuid,$2::uuid)
		ORDER BY created_at DESC, id DESC LIMIT 20`, alice, bob)
	if err != nil {
		t.Fatalf("EXPLAIN 会话查询: %v", err)
	}
	plan := []string{}
	for planRows.Next() {
		var line string
		if err = planRows.Scan(&line); err != nil {
			planRows.Close()
			t.Fatalf("读执行计划: %v", err)
		}
		plan = append(plan, line)
	}
	planRows.Close()
	if !strings.Contains(strings.Join(plan, "\n"), "direct_messages_conversation") {
		t.Fatalf("会话查询没走 direct_messages_conversation：接口的 WHERE 表达式与索引对不上了\n%s", strings.Join(plan, "\n"))
	}

	// 10) 兜底约束：给自己发的行在库层面也写不进去（接口层挡在前面，但约束不能少）。
	if _, err := db.ExecContext(ctx, `INSERT INTO community.direct_messages(id,sender_id,recipient_id,body) VALUES($1,$2,$2,'x')`, uuid.NewString(), alice); err == nil {
		t.Fatal("CHECK(sender_id <> recipient_id) 未生效：给自己发的行被写进了库")
	}
}

// listMessages 打私信列表端点并解出 items/total：形状不符直接失败（前端按这两个键解析）。
func listMessages(t *testing.T, router http.Handler, bearer, peer, query string) ([]map[string]any, int) {
	t.Helper()
	w := opsCall(t, router, http.MethodGet, "/api/messages/with/"+peer+"?"+query, "", bearer)
	if w.Code != 200 {
		t.Fatalf("GET 会话 HTTP %d：%s", w.Code, w.Body.String())
	}
	payload := struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析会话响应: %v（%s）", err, w.Body.String())
	}
	if payload.Items == nil {
		t.Fatalf("items 必须是数组（空会话也要是 [] 而不是 null）：%s", w.Body.String())
	}
	return payload.Items, payload.Total
}

// cleanupDirectMessages 清掉这几个身份之间的私信：用例共用一个测试库，不能留残行影响别的用例。
func cleanupDirectMessages(ctx context.Context, db *sql.DB, ids ...string) {
	_, _ = db.ExecContext(ctx, `DELETE FROM community.direct_messages
		WHERE sender_id = ANY($1::uuid[]) OR recipient_id = ANY($1::uuid[])`, pq.Array(ids))
}

// countDirectMessages 数某个身份参与的私信行数（用来断言"被拒的请求不落库"）。
func countDirectMessages(t *testing.T, db *sql.DB, id string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM community.direct_messages WHERE sender_id=$1::uuid OR recipient_id=$1::uuid`, id).Scan(&n); err != nil {
		t.Fatalf("数私信行数: %v", err)
	}
	return n
}
