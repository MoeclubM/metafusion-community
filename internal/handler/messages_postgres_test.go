package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// 收件箱的真库语义：会话列表（按对方分组 + 每段会话的未读）→ 标记已读归零 →
// 越权（第三者既看不见别人的会话、也不能替别人标记已读）→ 分页 → 索引与查询形状 → 发信限流。
//
// 与上一条用例共用夹具与清理口径：身份全是新建 uuid，跑完按参与者删行，不给同库的其它用例留噪音。
func TestMessageInboxAgainstPostgres(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	alice, bob, carol := uuid.NewString(), uuid.NewString(), uuid.NewString()
	dave, erin := uuid.NewString(), uuid.NewString()
	cleanup := func() { cleanupDirectMessages(ctx, db, alice, bob, carol, dave, erin) }
	cleanup()
	t.Cleanup(cleanup)
	token := func(id string) string {
		return signTokenWith(t, key, kid, id, "user", []string{"member"}, []string{auth.PermissionPostCreate})
	}
	send := func(from, to, body string) *httptest.ResponseRecorder {
		t.Helper()
		return opsCall(t, router, http.MethodPost, "/api/messages/with/"+to, `{"body":"`+body+`"}`, token(from))
	}

	// 1) 三条新端点都在登录门槛之后：匿名 401 authentication_required（与既有两条同一口径）。
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/messages/conversations"},
		{http.MethodGet, "/api/messages/unread"},
		{http.MethodPut, "/api/messages/with/" + bob + "/read"},
	} {
		w := opsCall(t, router, tc.method, tc.path, "", "")
		if w.Code != 401 || opsErrorCode(t, w) != "authentication_required" {
			t.Fatalf("%s %s 匿名应 401 authentication_required，实际 %d（%s）", tc.method, tc.path, w.Code, w.Body.String())
		}
	}

	// 2) 非法 uuid：404 not_found（与读/写同一口径，不把 pq 的解析错误兜成 500）。
	if w := opsCall(t, router, http.MethodPut, "/api/messages/with/not-a-uuid/read", "", token(alice)); w.Code != 404 || opsErrorCode(t, w) != "not_found" {
		t.Fatalf("非法 id 标记已读应 404 not_found，实际 %d（%s）", w.Code, w.Body.String())
	}

	// 3) 空收件箱：items 必须是 [] 而不是 null（前端按数组解析），total / 未读都是 0——
	//    "没有会话"与"取不到"在服务端是两件事，这一条只钉前者。
	items, total := listConversations(t, router, token(carol), "")
	if total != 0 || len(items) != 0 {
		t.Fatalf("空收件箱应为 0 会话，实际 total=%d items=%d", total, len(items))
	}
	if n := unreadCount(t, router, token(carol)); n != 0 {
		t.Fatalf("没有消息时未读数 = %d，期望 0", n)
	}

	// 4) A 给 B 发 2 条 → B 的收件箱：1 个会话 / 未读 2 / 最近一条是第二条。
	for _, body := range []string{"第一条", "第二条"} {
		if w := send(alice, bob, body); w.Code != 200 {
			t.Fatalf("发信 %q HTTP %d：%s", body, w.Code, w.Body.String())
		}
	}
	items, total = listConversations(t, router, token(bob), "")
	if total != 1 || len(items) != 1 {
		t.Fatalf("B 的收件箱应 1 个会话，实际 total=%d items=%d（%v）", total, len(items), items)
	}
	if items[0]["peer_id"] != alice {
		t.Fatalf("会话的对方 = %v，期望 %v", items[0]["peer_id"], alice)
	}
	if last, _ := items[0]["last_message"].(map[string]any); last == nil || last["body"] != "第二条" {
		t.Fatalf("会话的最近一条应是后发的那条，实际 %v", items[0]["last_message"])
	}
	if items[0]["unread_count"] != float64(2) {
		t.Fatalf("B 的未读数 = %v，期望 2", items[0]["unread_count"])
	}
	if n := unreadCount(t, router, token(bob)); n != 2 {
		t.Fatalf("未读总数 = %d，期望 2（与列表里的 unread_count 必须同口径）", n)
	}
	// 同一条会话在**发送方**视角也只剩一段，但未读为 0：自己发的不是自己没读的。
	sent, sentTotal := listConversations(t, router, token(alice), "")
	if sentTotal != 1 || len(sent) != 1 || sent[0]["peer_id"] != bob {
		t.Fatalf("A 的收件箱应含与 B 的一段会话，实际 total=%d items=%v", sentTotal, sent)
	}
	if sent[0]["unread_count"] != float64(0) {
		t.Fatalf("自己发出的消息不该算未读，实际 unread_count=%v", sent[0]["unread_count"])
	}

	// 5) 越权写入：**发送方**标记已读影响 0 行（read_at 是收信人的状态），B 的未读不变；
	//    第三者拿 A 的 id 标记同样 0 行，且这一对的行一条都没被置位。
	if marked := markRead(t, router, token(alice), bob); marked != 0 {
		t.Fatalf("发送方标记已读应影响 0 行，实际 %d", marked)
	}
	if marked := markRead(t, router, token(carol), alice); marked != 0 {
		t.Fatalf("第三者标记已读应影响 0 行，实际 %d", marked)
	}
	if n := unreadCount(t, router, token(bob)); n != 2 {
		t.Fatalf("越权标记改动了 B 的未读：%d，期望仍是 2", n)
	}
	if n := readFlaggedRows(t, db, alice, bob, carol); n != 0 {
		t.Fatalf("越权调用把 %d 行置成了已读，期望 0", n)
	}
	// 第三者读不到别人的会话（结构保证）：这一条已由 TestDirectMessagesAgainstPostgres 覆盖单会话，
	// 这里补收件箱视角——对方的会话不会出现在第三者的列表里。
	if items, total := listConversations(t, router, token(carol), ""); total != 0 || len(items) != 0 {
		t.Fatalf("第三者的收件箱不该有别人的会话，实际 total=%d items=%v", total, items)
	}

	// 6) 收信人标记已读：影响 2 行 → 未读归零；重复调用影响 0 行且**不刷新回执时间**。
	if marked := markRead(t, router, token(bob), alice); marked != 2 {
		t.Fatalf("标记已读应影响 2 行，实际 %d", marked)
	}
	if n := unreadCount(t, router, token(bob)); n != 0 {
		t.Fatalf("标记已读后未读数 = %d，期望 0", n)
	}
	items, _ = listConversations(t, router, token(bob), "")
	if items[0]["unread_count"] != float64(0) {
		t.Fatalf("标记已读后列表里的 unread_count = %v，期望 0", items[0]["unread_count"])
	}
	first := readReceiptAt(t, db, bob, alice)
	if first == "" {
		t.Fatal("标记已读没有写 read_at：回执列没有接上线")
	}
	if marked := markRead(t, router, token(bob), alice); marked != 0 {
		t.Fatalf("重复标记已读应影响 0 行（幂等），实际 %d", marked)
	}
	if again := readReceiptAt(t, db, bob, alice); again != first {
		t.Fatalf("重复标记刷新了回执时间：%s → %s（应停在第一次读到的那一刻）", first, again)
	}

	// 7) 收件箱分页：25 个不同对方各给 dave 发一条（时间显式错开，同一事务里 now() 会撞在一起）。
	//    这里直接插库而不是走接口：25 个对方是 25 个身份，用 HTTP 造要先签 25 个令牌，与分页无关。
	for i := 0; i < 25; i++ {
		exec(t, ctx, db, `INSERT INTO community.direct_messages(id,sender_id,recipient_id,body,created_at)
			VALUES($1,$2,$3,$4,now() - make_interval(secs => $5))`,
			uuid.NewString(), uuid.NewString(), dave, fmt.Sprintf("会话-%02d", i), i)
	}
	page1, total1 := listConversations(t, router, token(dave), "page=1&page_size=20")
	if total1 != 25 || len(page1) != 20 {
		t.Fatalf("第 1 页应 20 个会话 / total=25，实际 %d 个 / total=%d", len(page1), total1)
	}
	if bodyOf(t, page1[0]) != "会话-00" || bodyOf(t, page1[19]) != "会话-19" {
		t.Fatalf("会话应按最近一条的时间倒序：首 %q 末 %q", bodyOf(t, page1[0]), bodyOf(t, page1[19]))
	}
	page2, total2 := listConversations(t, router, token(dave), "page=2&page_size=20")
	if total2 != 25 || len(page2) != 5 {
		t.Fatalf("第 2 页应 5 个会话 / total=25，实际 %d 个 / total=%d", len(page2), total2)
	}
	if bodyOf(t, page2[0]) != "会话-20" || bodyOf(t, page2[4]) != "会话-24" {
		t.Fatalf("第 2 页应是更旧的 5 段：首 %q 末 %q", bodyOf(t, page2[0]), bodyOf(t, page2[4]))
	}
	// 空页也要给出正确的 total：前端靠它决定"还有没有下一页"，折成 0 会把入口掐掉。
	if page3, total3 := listConversations(t, router, token(dave), "page=3&page_size=20"); total3 != 25 || len(page3) != 0 {
		t.Fatalf("第 3 页应为空且 total 仍为 25，实际 %d 个 / total=%d", len(page3), total3)
	}
	if sized, _ := listConversations(t, router, token(dave), "page=1&page_size=0"); len(sized) != 20 {
		t.Fatalf("page_size 越界应取缺省 20，实际 %d 个", len(sized))
	}

	// 8) 索引真的能被这两条查询用上（不是"建了索引但查询对不上"）：
	//    关掉 seqscan 之后，两个分支必须各自命中收件箱/发件箱索引，且都不需要额外排序。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务: %v", err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("关掉 seqscan: %v", err)
	}
	assertPlanUsesIndex(t, tx, "direct_messages_inbox", dave, `SELECT id FROM community.direct_messages
		WHERE recipient_id = $1::uuid ORDER BY sender_id, created_at DESC, id DESC`)
	assertPlanUsesIndex(t, tx, "direct_messages_outbox", dave, `SELECT id FROM community.direct_messages
		WHERE sender_id = $1::uuid ORDER BY recipient_id, created_at DESC, id DESC`)

	// 9) 发信限流：同一账号连发 messageSendBurst 条之后，第 burst+1 条 429 rate_limited
	//    （带 Retry-After），且**不落库**；这一行失败也照样留痕（error_code 与响应体一致）。
	for i := 0; i < messageSendBurst; i++ {
		if w := send(erin, bob, fmt.Sprintf("限流-%d", i)); w.Code != 200 {
			t.Fatalf("限流前第 %d 条应放行，实际 HTTP %d：%s", i+1, w.Code, w.Body.String())
		}
	}
	limited := send(erin, bob, "限流-超限")
	if limited.Code != http.StatusTooManyRequests || opsErrorCode(t, limited) != "rate_limited" {
		t.Fatalf("超限应 429 rate_limited，实际 %d（%s）", limited.Code, limited.Body.String())
	}
	if retryAfter := limited.Header().Get("Retry-After"); retryAfter == "" {
		t.Fatal("429 必须带 Retry-After，否则调用方只能猜")
	}
	if n := countDirectMessages(t, db, erin); n != messageSendBurst {
		t.Fatalf("被限流的请求落库了：erin 参与 %d 行，期望 %d", n, messageSendBurst)
	}
	if rid := limited.Header().Get("X-Request-Id"); rid != "" {
		row := waitAuditRows(t, ctx, db, rid, 1)[0]
		if row.Action != "message.sent" || row.Result != "failure" || row.ErrorCode != "rate_limited" {
			t.Fatalf("被限流的发信审计行不符：%+v", row)
		}
		_, _ = db.ExecContext(ctx, "DELETE FROM audit.audit_log WHERE request_id=$1", rid)
	}
}

// listConversations 打收件箱端点并解出 items/total：形状不符直接失败（前端按这两个键解析）。
// 空列表必须是 [] 而不是 null——折成 null 的话前端会把它当"取不到"。
func listConversations(t *testing.T, router http.Handler, bearer, query string) ([]map[string]any, int) {
	t.Helper()
	path := "/api/messages/conversations"
	if query != "" {
		path += "?" + query
	}
	w := opsCall(t, router, http.MethodGet, path, "", bearer)
	if w.Code != 200 {
		t.Fatalf("GET 收件箱 HTTP %d：%s", w.Code, w.Body.String())
	}
	payload := struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析收件箱响应: %v（%s）", err, w.Body.String())
	}
	if payload.Items == nil {
		t.Fatalf("items 必须是数组（空收件箱也要是 [] 而不是 null）：%s", w.Body.String())
	}
	return payload.Items, payload.Total
}

// unreadCount 打未读总数端点（导航栏角标的形状）。
func unreadCount(t *testing.T, router http.Handler, bearer string) int {
	t.Helper()
	w := opsCall(t, router, http.MethodGet, "/api/messages/unread", "", bearer)
	if w.Code != 200 {
		t.Fatalf("GET 未读 HTTP %d：%s", w.Code, w.Body.String())
	}
	payload := struct {
		UnreadCount *int `json:"unread_count"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload.UnreadCount == nil {
		t.Fatalf("未读响应应为 {\"unread_count\":N}：%s", w.Body.String())
	}
	return *payload.UnreadCount
}

// markRead 标记会话已读并返回影响条数。
func markRead(t *testing.T, router http.Handler, bearer, peer string) int {
	t.Helper()
	w := opsCall(t, router, http.MethodPut, "/api/messages/with/"+peer+"/read", "", bearer)
	if w.Code != 200 {
		t.Fatalf("标记已读 HTTP %d：%s", w.Code, w.Body.String())
	}
	payload := struct {
		Marked *int64 `json:"marked"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload.Marked == nil {
		t.Fatalf("标记已读响应应为 {\"marked\":N}：%s", w.Body.String())
	}
	return int(*payload.Marked)
}

// bodyOf 取一条会话的最近一条正文。
func bodyOf(t *testing.T, conv map[string]any) string {
	t.Helper()
	last, _ := conv["last_message"].(map[string]any)
	body, _ := last["body"].(string)
	return body
}

// readFlaggedRows 数这几个身份之间已经被置为已读的行（用来断言越权调用没有改到任何一行）。
func readFlaggedRows(t *testing.T, db *sql.DB, ids ...string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM community.direct_messages
		WHERE read_at IS NOT NULL
		  AND (sender_id = ANY($1::uuid[]) OR recipient_id = ANY($1::uuid[]))`, pq.Array(ids)).Scan(&n); err != nil {
		t.Fatalf("数已读行: %v", err)
	}
	return n
}

// readReceiptAt 取"peer 发给 user 的"已读时间戳（最新那一个），空串表示一条都没读过。
func readReceiptAt(t *testing.T, db *sql.DB, user, peer string) string {
	t.Helper()
	var at sql.NullString
	if err := db.QueryRow(`SELECT max(read_at)::text FROM community.direct_messages
		WHERE recipient_id=$1::uuid AND sender_id=$2::uuid`, user, peer).Scan(&at); err != nil {
		t.Fatalf("读回执时间: %v", err)
	}
	return at.String
}

// assertPlanUsesIndex 关掉 seqscan 后跑一次 EXPLAIN，断言规划器选的确实是这条索引，
// 且没有落到额外排序上（索引列顺序或方向写错都会在这里失败——表太小，别的用例看不出来）。
func assertPlanUsesIndex(t *testing.T, tx *sql.Tx, index, userID, query string) {
	t.Helper()
	rows, err := tx.Query("EXPLAIN "+query, userID)
	if err != nil {
		t.Fatalf("EXPLAIN %.60s…: %v", query, err)
	}
	defer rows.Close()
	plan := []string{}
	for rows.Next() {
		var line string
		if err = rows.Scan(&line); err != nil {
			t.Fatalf("读执行计划: %v", err)
		}
		plan = append(plan, line)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, index) {
		t.Fatalf("这条查询没有走 %s：索引列顺序与查询的 WHERE/ORDER BY 对不上了\n%s", index, joined)
	}
	if strings.Contains(joined, "Seq Scan") {
		t.Fatalf("关掉 seqscan 后仍在全表扫：\n%s", joined)
	}
}

// 陌生人私信开关（收件人侧）与两档限流的真库语义：
// 默认接收 → 关闭后陌生人 403 recipient_not_accepting_messages（留痕、不落库）→ 已有会话不受影响 →
// 重新打开可发 → 设置项进审计 → 陌生人新会话额度第 6 个 429 → 收件箱未读优先排序。
//
// 夹具与清理口径同上：身份全是新建 uuid，跑完按参与者删行（设置行一并删）。
func TestMessageStrangerSwitchAgainstPostgres(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	alice, bob := uuid.NewString(), uuid.NewString()
	carol, dave, erin, frank := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	ids := []string{alice, bob, carol, dave, erin, frank}
	cleanup := func() {
		cleanupDirectMessages(ctx, db, ids...)
		if _, err := db.ExecContext(ctx, "DELETE FROM community.direct_message_settings WHERE user_id = ANY($1::uuid[])", pq.Array(ids)); err != nil {
			t.Fatalf("清理设置行: %v", err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	token := func(id string) string {
		return signTokenWith(t, key, kid, id, "user", []string{"member"}, []string{auth.PermissionPostCreate})
	}
	send := func(from, to, body string) *httptest.ResponseRecorder {
		t.Helper()
		return opsCall(t, router, http.MethodPost, "/api/messages/with/"+to, `{"body":"`+body+`"}`, token(from))
	}
	pairRows := func(a, b string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM community.direct_messages
			WHERE (sender_id=$1::uuid AND recipient_id=$2::uuid) OR (sender_id=$2::uuid AND recipient_id=$1::uuid)`,
			a, b).Scan(&n); err != nil {
			t.Fatalf("数这一对的行: %v", err)
		}
		return n
	}

	// 1) 设置端点：匿名 401；字段缺失/换型 400 invalid_payload（*bool 区分"没传"与"传了 false"）。
	for _, tc := range []struct{ method, path, payload string }{
		{http.MethodGet, "/api/messages/settings", ""},
		{http.MethodPut, "/api/messages/settings", `{"accept_from_strangers":false}`},
	} {
		if w := opsCall(t, router, tc.method, tc.path, tc.payload, ""); w.Code != 401 || opsErrorCode(t, w) != "authentication_required" {
			t.Fatalf("%s %s 匿名应 401 authentication_required，实际 %d（%s）", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
	for _, payload := range []string{`{}`, `{"accept_from_strangers":"yes"}`} {
		w := opsCall(t, router, http.MethodPut, "/api/messages/settings", payload, token(bob))
		if w.Code != 400 || opsErrorCode(t, w) != "invalid_payload" {
			t.Fatalf("载荷 %s 应 400 invalid_payload，实际 %d（%s）", payload, w.Code, w.Body.String())
		}
	}

	// 2) 默认接收：没有设置行的用户，陌生人（这一对之间没有任何私信）能发进来。
	if got := messageSettings(t, router, token(bob)); got != true {
		t.Fatalf("没设置过应是默认接收，实际 %v", got)
	}
	if w := send(alice, bob, "陌生人第一条"); w.Code != 200 {
		t.Fatalf("默认接收时陌生人应能发，实际 %d（%s）", w.Code, w.Body.String())
	}

	// 3) 关闭开关：carol 与 bob 之间没有任何私信 → 403 稳定机器码，**不落库**，且留痕。
	if got := setMessageSettings(t, router, token(bob), false); got != false {
		t.Fatalf("PUT 之后应回 false，实际 %v", got)
	}
	blocked := send(carol, bob, "陌生人被拒")
	if blocked.Code != http.StatusForbidden || opsErrorCode(t, blocked) != "recipient_not_accepting_messages" {
		t.Fatalf("关闭后陌生人应 403 recipient_not_accepting_messages，实际 %d（%s）", blocked.Code, blocked.Body.String())
	}
	if n := pairRows(carol, bob); n != 0 {
		t.Fatalf("被拒的发送不得落库，实际 %d 行", n)
	}
	if rid := blocked.Header().Get("X-Request-Id"); rid != "" {
		row := waitAuditRows(t, ctx, db, rid, 1)[0]
		if row.Action != "message.sent" || row.Result != "failure" || row.ErrorCode != "recipient_not_accepting_messages" {
			t.Fatalf("被拒发送的审计行不符（应留痕且带稳定码）：%+v", row)
		}
		if row.TargetType != "user" || row.TargetID != bob || !strings.Contains(row.Changes, "recipient_disallows_strangers") {
			t.Fatalf("被拒发送的被动对象/摘要不符：%+v", row)
		}
		_, _ = db.ExecContext(ctx, "DELETE FROM audit.audit_log WHERE request_id=$1", rid)
	} else {
		t.Fatal("被拒的发送响应缺少 X-Request-Id：审计行无法定位")
	}

	// 4) 已有会话不受开关影响：alice 在第 2 步已经给 bob 发过信，她不是陌生人。
	if w := send(alice, bob, "聊过的人仍能发"); w.Code != 200 {
		t.Fatalf("已有会话的一方不该被开关拦住，实际 %d（%s）", w.Code, w.Body.String())
	}

	// 5) 重新打开：陌生人又能发；设置值是持久的（GET 回读）。
	if got := setMessageSettings(t, router, token(bob), true); got != true {
		t.Fatalf("重新打开应回 true，实际 %v", got)
	}
	if w := send(carol, bob, "重新打开后可以发"); w.Code != 200 {
		t.Fatalf("重新打开后陌生人应能发，实际 %d（%s）", w.Code, w.Body.String())
	}

	// 6) 设置只写自己的：alice 关闭开关不能影响 bob 的值（请求里没有可以指向别人的输入）。
	if got := setMessageSettings(t, router, token(alice), false); got != false {
		t.Fatalf("alice 的设置应回 false，实际 %v", got)
	}
	if got := messageSettings(t, router, token(bob)); got != true {
		t.Fatalf("别人的设置被改动了：bob = %v，期望仍是 true", got)
	}

	// 7) 设置项进审计：被动对象是设置所有者本人，changes 记前后值。
	ridProbe := send(alice, bob, "审计探针")
	if ridProbe.Code != 200 {
		t.Fatalf("探针发送应 200，实际 %d（%s）", ridProbe.Code, ridProbe.Body.String())
	}
	putW := opsCall(t, router, http.MethodPut, "/api/messages/settings", `{"accept_from_strangers":false}`, token(dave))
	if putW.Code != 200 {
		t.Fatalf("PUT 设置应 200，实际 %d（%s）", putW.Code, putW.Body.String())
	}
	if rid := putW.Header().Get("X-Request-Id"); rid != "" {
		row := waitAuditRows(t, ctx, db, rid, 1)[0]
		if row.Action != "message.settings_updated" || row.TargetType != "user" || row.TargetID != dave {
			t.Fatalf("设置项审计行不符：%+v", row)
		}
		for _, want := range []string{`"from": true`, `"to": false`} {
			if !strings.Contains(row.Changes, want) {
				t.Fatalf("设置审计 changes 缺少 %q：%s", want, row.Changes)
			}
		}
		_, _ = db.ExecContext(ctx, "DELETE FROM audit.audit_log WHERE request_id=$1", rid)
	} else {
		t.Fatal("PUT 设置响应缺少 X-Request-Id：审计行无法定位")
	}

	// 8) 陌生人新会话额度：erin 连开 5 个新会话放行，第 6 个 429 rate_limited（额度是"新会话"，
	//    不是"发信"——所以这里每个收件人都是全新的 uuid，且这一档与总体 20/分钟的桶互不串账）。
	for i := 0; i < messageStrangerBurst; i++ {
		peer := uuid.NewString()
		t.Cleanup(func() { cleanupDirectMessages(ctx, db, peer) })
		if w := send(erin, peer, fmt.Sprintf("新会话-%d", i)); w.Code != 200 {
			t.Fatalf("第 %d 个新会话应放行，实际 %d（%s）", i+1, w.Code, w.Body.String())
		}
	}
	sixth := uuid.NewString()
	t.Cleanup(func() { cleanupDirectMessages(ctx, db, sixth) })
	limited := send(erin, sixth, "第 6 个新会话")
	if limited.Code != http.StatusTooManyRequests || opsErrorCode(t, limited) != "rate_limited" {
		t.Fatalf("第 6 个新会话应 429 rate_limited，实际 %d（%s）", limited.Code, limited.Body.String())
	}
	if limited.Header().Get("Retry-After") == "" {
		t.Fatal("429 必须带 Retry-After")
	}
	if n := pairRows(erin, sixth); n != 0 {
		t.Fatalf("被限流的新会话不得落库，实际 %d 行", n)
	}
	if rid := limited.Header().Get("X-Request-Id"); rid != "" {
		row := waitAuditRows(t, ctx, db, rid, 1)[0]
		if row.ErrorCode != "rate_limited" || !strings.Contains(row.Changes, "new_stranger") {
			t.Fatalf("被限流的新会话审计行应能区分是哪一档拦的：%+v", row)
		}
		_, _ = db.ExecContext(ctx, "DELETE FROM audit.audit_log WHERE request_id=$1", rid)
	}

	// 9) 陌生人额度只对"新会话"计费：erin 已经与前面 5 个人聊过，继续发不扣这一档，
	//    因此即使额度已耗尽，她**继续**跟那 5 个人中的任意一个也能发出去（只受总体 20/分钟约束）。
	var knownPeer string
	if err := db.QueryRowContext(ctx, `SELECT recipient_id::text FROM community.direct_messages
		WHERE sender_id=$1::uuid ORDER BY created_at LIMIT 1`, erin).Scan(&knownPeer); err != nil {
		t.Fatalf("取 erin 的一个已有收件人: %v", err)
	}
	if w := send(erin, knownPeer, "额度耗尽后继续聊"); w.Code != 200 {
		t.Fatalf("已有会话的继续发送不该被陌生人额度拦住，实际 %d（%s）", w.Code, w.Body.String())
	}

	// 10) 收件箱未读优先：frank 收到两条新消息（未读）与一条已被标记已读的旧会话，
	//     未读的两段必须排在已读那段之前（组内仍按最近一条倒序）。
	readPeer, unreadOld, unreadNew := uuid.NewString(), uuid.NewString(), uuid.NewString()
	t.Cleanup(func() { cleanupDirectMessages(ctx, db, readPeer, unreadOld, unreadNew) })
	exec(t, ctx, db, `INSERT INTO community.direct_messages(id,sender_id,recipient_id,body,created_at,read_at)
		VALUES($1,$2,$3,'已读会话',now() - make_interval(secs => 600), now())`,
		uuid.NewString(), readPeer, frank)
	exec(t, ctx, db, `INSERT INTO community.direct_messages(id,sender_id,recipient_id,body,created_at)
		VALUES($1,$2,$3,'未读较早',now() - make_interval(secs => 300))`,
		uuid.NewString(), unreadOld, frank)
	exec(t, ctx, db, `INSERT INTO community.direct_messages(id,sender_id,recipient_id,body,created_at)
		VALUES($1,$2,$3,'未读较新',now() - make_interval(secs => 60))`,
		uuid.NewString(), unreadNew, frank)
	convs, total := listConversations(t, router, token(frank), "")
	if total != 3 || len(convs) != 3 {
		t.Fatalf("frank 应有 3 段会话，实际 total=%d items=%d", total, len(convs))
	}
	if bodyOf(t, convs[0]) != "未读较新" || bodyOf(t, convs[1]) != "未读较早" || bodyOf(t, convs[2]) != "已读会话" {
		t.Fatalf("未读优先排序不符：%q / %q / %q（期望 未读较新 / 未读较早 / 已读会话）",
			bodyOf(t, convs[0]), bodyOf(t, convs[1]), bodyOf(t, convs[2]))
	}

	// 11) 未读优先没有改掉"每个分支走索引"的形状：关掉 seqscan 后两个分支仍各自命中收件箱/发件箱索引。
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("开启事务: %v", err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("关掉 seqscan: %v", err)
	}
	assertPlanUsesIndex(t, tx, "direct_messages_inbox", frank, `SELECT id FROM community.direct_messages
		WHERE recipient_id = $1::uuid ORDER BY sender_id, created_at DESC, id DESC`)
	assertPlanUsesIndex(t, tx, "direct_messages_outbox", frank, `SELECT id FROM community.direct_messages
		WHERE sender_id = $1::uuid ORDER BY recipient_id, created_at DESC, id DESC`)
}

// messageSettings 读私信收件设置端点（扁平布尔）。
func messageSettings(t *testing.T, router http.Handler, bearer string) bool {
	t.Helper()
	w := opsCall(t, router, http.MethodGet, "/api/messages/settings", "", bearer)
	if w.Code != 200 {
		t.Fatalf("GET 私信设置 HTTP %d：%s", w.Code, w.Body.String())
	}
	payload := struct {
		Accept *bool `json:"accept_from_strangers"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload.Accept == nil {
		t.Fatalf("设置响应应为 {\"accept_from_strangers\":bool}：%s", w.Body.String())
	}
	return *payload.Accept
}

// setMessageSettings 写私信收件设置并回读响应里的值。
func setMessageSettings(t *testing.T, router http.Handler, bearer string, accept bool) bool {
	t.Helper()
	payload := fmt.Sprintf(`{"accept_from_strangers":%v}`, accept)
	w := opsCall(t, router, http.MethodPut, "/api/messages/settings", payload, bearer)
	if w.Code != 200 {
		t.Fatalf("PUT 私信设置 HTTP %d：%s", w.Code, w.Body.String())
	}
	out := struct {
		Accept *bool `json:"accept_from_strangers"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Accept == nil {
		t.Fatalf("设置写入响应应为 {\"accept_from_strangers\":bool}：%s", w.Body.String())
	}
	return *out.Accept
}
