package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/MoeclubM/metafusion-community/internal/audit"
	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
	"github.com/MoeclubM/metafusion-community/internal/testutil"
)

// 真库用例（契约 §6.2）：每个被审计动作**恰好一行**、敏感值整行零命中、失败路径留痕、
// X-Request-Id 透传。这些都是"异步写入 + 中间件"共同决定的，只有打真库才验得到。
//
// 审计是旁路异步写入（Record 入队，后台 goroutine 落库），因此请求返回时行可能还没落库：
// 这里用 waitAuditRows 轮询到"行数稳定"，而不是 sleep 一个猜出来的时长。
// 用例跑完按 request_id 清掉自己写的行，不给同库的其它用例留噪音。

// auditRow 是审计行的投影。18 列全读出来：契约 §6.2 的敏感值断言按"整行"判定，
// 只看 changes 会漏掉 UA / target 这些同样由请求内容决定的列。
type auditRow struct {
	ID             string
	OccurredAt     time.Time
	Service        string
	Action         string
	ActorUserID    string
	ActorUsername  string
	CredentialType string
	ActorIP        string
	ActorUserAgent string
	TargetType     string
	TargetID       string
	Changes        string
	Result         string
	ErrorCode      string
	RequestMethod  string
	Route          string
	HTTPStatus     int
	RequestID      string
}

const auditCols = `id::text,occurred_at,service,action,COALESCE(actor_user_id::text,''),actor_username,
	credential_type,actor_ip,actor_user_agent,target_type,target_id,changes::text,result,error_code,
	request_method,route,http_status,request_id`

// text 是一行的全文（所有列拼在一起），敏感值断言就打在它上面。
func (r auditRow) text() string {
	return fmt.Sprintf("%s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %s %d %s",
		r.ID, r.OccurredAt, r.Service, r.Action, r.ActorUserID, r.ActorUsername, r.CredentialType,
		r.ActorIP, r.ActorUserAgent, r.TargetType, r.TargetID, r.Changes, r.Result, r.ErrorCode,
		r.RequestMethod, r.Route, r.HTTPStatus, r.RequestID)
}

// sensitivePattern 是契约 §6.2 的敏感值判据：口令/令牌/密钥的字样、PAT 前缀、完整邮箱。
// 命中即失败——审计行里不允许出现这些。
var sensitivePattern = regexp.MustCompile(`(?i)password|token|secret|mfp_|mf_pat_|sk_|` + fullEmailInRow)

var fullEmailInRow = `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+.[A-Za-z]{2,}`

func queryAuditRows(t *testing.T, ctx context.Context, db *sql.DB, requestID string) []auditRow {
	t.Helper()
	rows, err := db.QueryContext(ctx, "SELECT "+auditCols+" FROM audit.audit_log WHERE request_id=$1 ORDER BY occurred_at", requestID)
	if err != nil {
		t.Fatalf("查审计行: %v", err)
	}
	defer rows.Close()
	out := []auditRow{}
	for rows.Next() {
		var r auditRow
		if err = rows.Scan(&r.ID, &r.OccurredAt, &r.Service, &r.Action, &r.ActorUserID, &r.ActorUsername,
			&r.CredentialType, &r.ActorIP, &r.ActorUserAgent, &r.TargetType, &r.TargetID, &r.Changes,
			&r.Result, &r.ErrorCode, &r.RequestMethod, &r.Route, &r.HTTPStatus, &r.RequestID); err != nil {
			t.Fatalf("扫描审计行: %v", err)
		}
		out = append(out, r)
	}
	if err = rows.Err(); err != nil {
		t.Fatalf("读审计行: %v", err)
	}
	return out
}

// waitAuditRows 等到 request_id 下的审计行数稳定在 want 条：先等到看见 want 条，
// 再等一小会儿确认没有第 want+1 条姗姗来迟（"恰好一行"是契约 §6.2 的断言项）。
func waitAuditRows(t *testing.T, ctx context.Context, db *sql.DB, requestID string, want int) []auditRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := queryAuditRows(t, ctx, db, requestID)
		if len(rows) == want {
			time.Sleep(150 * time.Millisecond)
			settled := queryAuditRows(t, ctx, db, requestID)
			if len(settled) != want {
				t.Fatalf("request_id=%s 在稳定后又多出了审计行：%d 条（期望 %d）", requestID, len(settled), want)
			}
			return settled
		}
		if time.Now().After(deadline) {
			t.Fatalf("request_id=%s 的审计行数 = %d，期望 %d", requestID, len(rows), want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// auditHarness 是这套用例的夹具：真库 + 目录桩 + 可验签 router + request_id 记账。
type auditHarness struct {
	ctx       context.Context
	db        *sql.DB
	router    http.Handler
	token     string
	tokenSub  string
	entityID  string
	peerID    string
	requestID []string
}

func newAuditHarness(t *testing.T) *auditHarness {
	t.Helper()
	ctx, db, router, key, kid := opsFixture(t)
	h := &auditHarness{ctx: ctx, db: db, router: router, entityID: uuid.NewString(), peerID: uuid.NewString()}
	h.tokenSub = uuid.NewString()
	// 一个持全部社区权限码的身份：每一步都用它，因此审计行里的 actor 可以逐字段核对。
	h.token = signTokenWith(t, key, kid, h.tokenSub, "user", []string{"community_admin"}, []string{
		auth.PermissionPostCreate, auth.PermissionPostModerate, auth.PermissionTopicPin, auth.PermissionBoardManage,
	})
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM audit.audit_log WHERE request_id = ANY($1)", pq.Array(h.requestID))
	})
	return h
}

// call 打一次请求并带上固定的 X-Request-Id（为空时用响应头回写的那个），返回响应与 request_id。
func (h *auditHarness) call(t *testing.T, method, path, payload, requestID string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mf-community-audit-test/1.0")
	if requestID != "" {
		req.Header.Set("X-Request-Id", requestID)
	}
	if h.token != "" {
		req.Header.Set("Authorization", "Bearer "+h.token)
	}
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	effective := requestID
	if effective == "" {
		effective = w.Header().Get("X-Request-Id")
	}
	h.requestID = append(h.requestID, effective)
	return w, effective
}

// auditCall 打一次**已认证**的写请求，并断言 request_id 下恰好一行、基础字段一致。
func (h *auditHarness) auditCall(t *testing.T, method, path, payload, requestID, action string, status int) auditRow {
	t.Helper()
	w, rid := h.call(t, method, path, payload, requestID)
	if w.Code != status {
		t.Fatalf("%s %s HTTP %d，期望 %d：%s", method, path, w.Code, status, w.Body.String())
	}
	if got := w.Header().Get("X-Request-Id"); got != rid {
		t.Fatalf("%s %s 响应头 X-Request-Id = %q，期望 %q（必须原样透传）", method, path, got, rid)
	}
	rows := waitAuditRows(t, h.ctx, h.db, rid, 1)
	row := rows[0]
	if row.Action != action {
		t.Fatalf("%s %s 的动作码 = %q，期望 %q", method, path, row.Action, action)
	}
	if row.Service != audit.ServiceName || row.RequestMethod != method {
		t.Fatalf("%s %s 的服务/方法不符：%+v", method, path, row)
	}
	// 路由存的是**模板**（/api/community/topics/:id），不是原始路径：原始路径没有额外信息，
	// 模板才聚合得起来（契约 §1 的 route 列）。所以这里断言"是登记过的模板且与请求路径匹配"。
	if _, ok := auditActions[row.RequestMethod+" "+row.Route]; !ok || !templateMatches(row.Route, path) {
		t.Fatalf("%s %s 的审计路由 %q 不是登记过的模板或与请求路径不匹配：%+v", method, path, row.Route, row)
	}
	if status < 400 {
		if row.Result != audit.ResultSuccess || row.ErrorCode != "" || row.HTTPStatus != status {
			t.Fatalf("%s %s 的成功行不符：%+v", method, path, row)
		}
	} else if row.Result != audit.ResultFailure || row.ErrorCode == "" {
		t.Fatalf("%s %s 的失败行必须带 result=failure + error_code：%+v", method, path, row)
	}
	if row.ActorUserID != h.tokenSub || row.ActorUsername != "kana" || row.CredentialType != audit.CredentialSession {
		t.Fatalf("%s %s 的操作者不符：%+v", method, path, row)
	}
	if row.OccurredAt.IsZero() || row.RequestID != rid {
		t.Fatalf("%s %s 的 occurred_at / request_id 不符：%+v", method, path, row)
	}
	return row
}

// templateMatches 报告请求路径是否匹配一条路由模板（:param 匹配恰好一个路径段）。
func templateMatches(template, path string) bool {
	tParts := strings.Split(strings.Trim(template, "/"), "/")
	pParts := strings.Split(strings.Trim(path, "/"), "/")
	if len(tParts) != len(pParts) {
		return false
	}
	for i := range tParts {
		if strings.HasPrefix(tParts[i], ":") {
			continue
		}
		if tParts[i] != pParts[i] {
			return false
		}
	}
	return true
}

// TestAuditLogAgainstPostgres：逐个走完本服务的 10 条写路由，每条断言"恰好一行 + 正确的
// 动作码 / 被动对象 / 变更摘要"，最后对全部行做一次整行敏感值扫描。
func TestAuditLogAgainstPostgres(t *testing.T) {
	h := newAuditHarness(t)

	// 1) 发主题：标题里塞一个真邮箱，验证"值里的邮箱"在入库前被遮罩。
	w, rid := h.call(t, http.MethodPost, "/api/community/topics",
		`{"board_code":"qa","title":"审计邮箱 user@example.com","content":"正文不进审计","tag_names":["原盘"]}`, "rid-audit-topic")
	if w.Code != 200 {
		t.Fatalf("发主题 HTTP %d：%s", w.Code, w.Body.String())
	}
	created := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	topicID, _ := created["id"].(string)
	if topicID == "" {
		t.Fatalf("发主题未返回 id：%s", w.Body.String())
	}
	row := waitAuditRows(t, h.ctx, h.db, rid, 1)[0]
	if row.Action != "topic.created" || row.TargetType != "topic" || row.TargetID != topicID {
		t.Fatalf("发主题的审计行不符：%+v", row)
	}
	for _, want := range []string{"u***@example.com", `"board_code": "qa"`, "原盘"} {
		if !strings.Contains(row.Changes, want) {
			t.Fatalf("changes 缺少 %q：%s", want, row.Changes)
		}
	}
	// 正文是内容，不进审计（审计表不存请求体原文，契约 §1）。
	if strings.Contains(row.Changes, "正文不进审计") {
		t.Fatalf("正文进了审计摘要：%s", row.Changes)
	}
	if m := regexp.MustCompile(fullEmailInRow).FindString(row.Changes); m != "" {
		t.Fatalf("changes 里出现完整邮箱 %q：%s", m, row.Changes)
	}

	// 2) 回帖。
	w, pid := h.call(t, http.MethodPost, "/api/community/topics/"+topicID+"/posts", `{"content":"一楼"}`, "rid-audit-post")
	if w.Code != 200 {
		t.Fatalf("回帖 HTTP %d：%s", w.Code, w.Body.String())
	}
	posted := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &posted)
	postID, _ := posted["id"].(string)
	row = waitAuditRows(t, h.ctx, h.db, pid, 1)[0]
	if row.Action != "post.created" || row.TargetType != "post" || row.TargetID != postID {
		t.Fatalf("回帖的审计行不符：%+v", row)
	}
	// 楼层号按接口返回的值断言，不写死：当前实现的第一个楼层号是 2
	// （SQL 是 COALESCE(MAX(post_number),1)+1，空主题表里 MAX 为 NULL → 1+1）。
	// 审计用例不该把这条既有行为钉死，它只负责保证"记下来的与真实写入的一致"。
	postNumber, _ := posted["post_number"].(float64)
	wantPostNumber := fmt.Sprintf(`"post_number": %.0f`, postNumber)
	if !strings.Contains(row.Changes, `"topic_id": "`+topicID) || !strings.Contains(row.Changes, wantPostNumber) {
		t.Fatalf("回帖 changes 不符：%s（期望 %s）", row.Changes, wantPostNumber)
	}

	// 3) 置顶：变更前后都要记（UPDATE ... RETURNING 只给得出"之后"）。
	row = h.auditCall(t, http.MethodPut, "/api/community/topics/"+topicID+"/pin", `{"pinned":true}`, "rid-audit-pin", "topic.pinned", 200)
	if row.TargetType != "topic" || row.TargetID != topicID {
		t.Fatalf("置顶的被动对象不符：%+v", row)
	}
	for _, want := range []string{`"from": false`, `"to": true`, `"board_code": "qa"`} {
		if !strings.Contains(row.Changes, want) {
			t.Fatalf("置顶 changes 缺少 %q：%s", want, row.Changes)
		}
	}

	// 4) 条目短评。
	w, cid := h.call(t, http.MethodPost, "/api/community/entities/"+h.entityID+"/posts", `{"body":"短评正文不进审计"}`, "rid-audit-comment")
	if w.Code != 200 {
		t.Fatalf("发短评 HTTP %d：%s", w.Code, w.Body.String())
	}
	comment := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &comment)
	item, _ := comment["item"].(map[string]any)
	commentID, _ := item["id"].(string)
	row = waitAuditRows(t, h.ctx, h.db, cid, 1)[0]
	if row.Action != "comment.created" || row.TargetType != "comment" || row.TargetID != commentID {
		t.Fatalf("发短评的审计行不符：%+v", row)
	}
	if !strings.Contains(row.Changes, h.entityID) || strings.Contains(row.Changes, "短评正文不进审计") {
		t.Fatalf("发短评 changes 不符（应记锚点、不记正文）：%s", row.Changes)
	}

	// 5) 收藏切换：被动对象是被收藏的实体。
	row = h.auditCall(t, http.MethodPost, "/api/favorites/toggle",
		`{"target_type":"work","target_id":"`+h.entityID+`"}`, "rid-audit-favorite", "favorite.toggled", 200)
	if row.TargetType != "entity" || row.TargetID != h.entityID {
		t.Fatalf("收藏的被动对象不符：%+v", row)
	}
	if !strings.Contains(row.Changes, `"favorited": true`) {
		t.Fatalf("收藏 changes 不符：%s", row.Changes)
	}

	// 6) 发私信：正文（私有内容）绝不进审计。
	msgBody := "私信正文-mf_pat_should_not_appear"
	row = h.auditCall(t, http.MethodPost, "/api/messages/with/"+h.peerID, `{"body":"`+msgBody+`"}`, "rid-audit-message", "message.sent", 200)
	if row.TargetType != "user" || row.TargetID != h.peerID {
		t.Fatalf("私信的被动对象不符：%+v", row)
	}
	if strings.Contains(row.text(), msgBody) || strings.Contains(row.text(), "mf_pat_") {
		t.Fatalf("私信正文进了审计行：%s", row.Changes)
	}

	// 7) 板块配置：只记确实变了的字段，且带变更前后值。
	row = h.auditCall(t, http.MethodPut, "/api/community/boards/qa",
		`{"color":"sky","is_enabled":false}`, "rid-audit-board", "board.updated", 200)
	if row.TargetType != "board" || row.TargetID != "qa" {
		t.Fatalf("板块配置的被动对象不符：%+v", row)
	}
	for _, want := range []string{`"from": "teal"`, `"to": "sky"`, `"from": true`, `"to": false`} {
		if !strings.Contains(row.Changes, want) {
			t.Fatalf("板块 changes 缺少 %q：%s", want, row.Changes)
		}
	}
	if strings.Contains(row.Changes, "sort_order") || strings.Contains(row.Changes, "names") {
		t.Fatalf("载荷里没传的字段不该出现在 changes：%s", row.Changes)
	}

	// 8) 删回复 / 删短评 / 删主题：三条删除路由各一行，被动对象是"被删掉的东西"。
	row = h.auditCall(t, http.MethodDelete, "/api/community/topics/"+topicID+"/posts/"+postID, "", "rid-audit-delpost", "post.deleted", 200)
	if row.TargetID != postID || !strings.Contains(row.Changes, wantPostNumber) {
		t.Fatalf("删回复的审计行不符（应含变更前楼层号 %s）：%+v", wantPostNumber, row)
	}
	row = h.auditCall(t, http.MethodDelete, "/api/community/posts/"+commentID, "", "rid-audit-delcomment", "comment.deleted", 200)
	if row.TargetID != commentID || !strings.Contains(row.Changes, h.entityID) {
		t.Fatalf("删短评的审计行不符：%+v", row)
	}
	row = h.auditCall(t, http.MethodDelete, "/api/community/topics/"+topicID, "", "rid-audit-deltopic", "topic.deleted", 200)
	if row.TargetID != topicID || !strings.Contains(row.Changes, `"board_code": "qa"`) {
		t.Fatalf("删主题的审计行不符：%+v", row)
	}

	// 9) X-Request-Id 缺省时自己生成并回写，且审计行用的是同一个 id。
	//    用 casual 而不是 qa：上一步刚把 qa 停用，往停用板块发主题会被 400 invalid_board 拒掉。
	w, generated := h.call(t, http.MethodPost, "/api/community/topics",
		`{"board_code":"casual","title":"无 request id","content":"正文"}`, "")
	if w.Code != 200 || generated == "" {
		t.Fatalf("缺省 request_id 的请求应 200 且回写 header：%d %q", w.Code, generated)
	}
	if _, err := uuid.Parse(generated); err != nil {
		t.Fatalf("回写的 X-Request-Id 不是 uuid：%q", generated)
	}
	row = waitAuditRows(t, h.ctx, h.db, generated, 1)[0]
	if row.RequestID != generated || row.Action != "topic.created" {
		t.Fatalf("缺省 request_id 的审计行不符：%+v", row)
	}

	// 10) 整行敏感值扫描：契约 §6.2 的硬要求，一次扫完本次写入的全部行。
	for _, rid := range h.requestID {
		for _, row := range queryAuditRows(t, h.ctx, h.db, rid) {
			if m := sensitivePattern.FindString(row.text()); m != "" {
				t.Fatalf("request_id=%s 的审计行命中敏感值 %q：%s", rid, m, row.text())
			}
		}
	}
}

// TestAuditFailurePathsAgainstPostgres：失败路径也要留痕——匿名写、缺码、非法载荷、目标不存在。
// error_code 必须与响应体的 error 字段一致（handler 的 fail() 与 require/guard 都登记了码）。
func TestAuditFailurePathsAgainstPostgres(t *testing.T) {
	h := newAuditHarness(t)

	// 1) 非法板块：400 invalid_board（handler 的 fail() 登记了码）。
	row := h.auditCall(t, http.MethodPost, "/api/community/topics", `{"board_code":"nope","title":"x","content":"y"}`, "rid-audit-badboard", "topic.created", 400)
	if row.Result != audit.ResultFailure || row.ErrorCode != "invalid_board" || row.HTTPStatus != 400 {
		t.Fatalf("非法板块的失败行不符：%+v", row)
	}
	if row.TargetType != "" || row.TargetID != "" {
		t.Fatalf("失败在业务写入之前的行不该有被动对象：%+v", row)
	}

	// 2) 目标不存在：404 not_found（预读与删除都没命中）。
	missing := uuid.NewString()
	row = h.auditCall(t, http.MethodDelete, "/api/community/topics/"+missing, "", "rid-audit-missing", "topic.deleted", 404)
	if row.ErrorCode != "not_found" || row.TargetType != "" {
		t.Fatalf("删除不存在主题的失败行不符：%+v", row)
	}

	// 3) 匿名写（require 的 401）：照样留痕，操作者是 anonymous、没有 actor_user_id。
	h.token = ""
	w, rid := h.call(t, http.MethodPost, "/api/community/topics", `{"board_code":"qa","title":"匿名","content":"y"}`, "rid-audit-anon")
	if w.Code != 401 || !strings.Contains(w.Body.String(), "authentication_required") {
		t.Fatalf("匿名发主题应 401 authentication_required：%d %s", w.Code, w.Body.String())
	}
	row = waitAuditRows(t, h.ctx, h.db, rid, 1)[0]
	if row.Action != "topic.created" || row.Result != audit.ResultFailure || row.ErrorCode != "authentication_required" {
		t.Fatalf("匿名写主题的审计行不符：%+v", row)
	}
	if row.CredentialType != audit.CredentialAnonymous || row.ActorUserID != "" || row.ActorUsername != "" {
		t.Fatalf("匿名行不该有身份：%+v", row)
	}

	// 4) 匿名写走 guard(true) 的路由（auth.Required 的 401）：语义同上，码也必须是 body 里的那个。
	w, rid = h.call(t, http.MethodPost, "/api/favorites/toggle", `{"target_type":"work","target_id":"`+h.entityID+`"}`, "rid-audit-anon-fav")
	if w.Code != 401 {
		t.Fatalf("匿名收藏应 401：%d %s", w.Code, w.Body.String())
	}
	row = waitAuditRows(t, h.ctx, h.db, rid, 1)[0]
	if row.Action != "favorite.toggled" || row.ErrorCode != "authentication_required" || row.Result != audit.ResultFailure {
		t.Fatalf("匿名收藏的审计行不符：%+v", row)
	}

}

// 缺权限码（403 forbidden）是第三条失败路径：它既不是匿名（401，见上一个用例），
// 也不是业务校验失败（400/404），而"有人试过但被拒"同样要有痕迹。
// 这条路由的闸门是 require(community.board.manage)，判定在中间件里、写库之前。
func TestAuditPermissionDeniedAgainstPostgres(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	sub := uuid.NewString()
	token := signTokenWith(t, key, kid, sub, "user", []string{"member"}, []string{auth.PermissionPostCreate})
	req := httptest.NewRequest(http.MethodPut, "/api/community/boards/qa", strings.NewReader(`{"is_enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Request-Id", "rid-audit-forbidden")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("缺 community.board.manage 应 403：%d %s", w.Code, w.Body.String())
	}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM audit.audit_log WHERE request_id=$1", "rid-audit-forbidden")
	})
	row := waitAuditRows(t, ctx, db, "rid-audit-forbidden", 1)[0]
	if row.Action != "board.updated" || row.Result != audit.ResultFailure || row.ErrorCode != "forbidden" {
		t.Fatalf("缺码请求的审计行不符：%+v", row)
	}
	if row.ActorUserID != sub || row.CredentialType != audit.CredentialSession {
		t.Fatalf("缺码请求仍要记操作者：%+v", row)
	}
}

// TestAuditLogForRejectedCredentialsAgainstPostgres：**凭据被拒的写请求也要留痕**。
//
// 这条用例钉住中间件顺序：审计中间件挂在身份中间件之前（handler.Register 的注释写了原因）。
// 身份中间件（auth.Verifier.Middleware）只对 PAT 路径 abort——形态非法/已吊销/过期 → 401
// invalid_token，内省不可达（含未配置 AUTH_URL）→ 503 auth_unavailable；缺 Authorization 或
// 无效 JWT 一律**按匿名继续**，由路由闸门回 401 authentication_required。因此：
//   - 审计中间件挂在身份之后：PAT 的两种拒绝一行审计都没有（覆盖缺口）；
//   - 挂在身份之前：两种拒绝各落一行，记 anonymous + http_<status>（响应里就是这个码，是事实），
//     而"无效 JWT"那条走的是闸门登记的稳定码 authentication_required。
func TestAuditLogForRejectedCredentialsAgainstPostgres(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	s, err := store.Open(ctx, testutil.DSN(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.Init(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	db := testutil.Database(t)
	cleaned := []string{}
	t.Cleanup(func() {
		_, _ = db.ExecContext(ctx, "DELETE FROM audit.audit_log WHERE request_id = ANY($1)", pq.Array(cleaned))
	})

	// do 打一次请求，返回响应与 request_id（本用例每步都显式带 X-Request-Id）。
	do := func(router http.Handler, method, path, payload, bearer, requestID string) (*httptest.ResponseRecorder, string) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", requestID)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		cleaned = append(cleaned, requestID)
		return w, requestID
	}
	// assertRejected 断言"响应是这条拒绝 + 恰好一行审计"，并返回该行供进一步断言。
	assertRejected := func(w *httptest.ResponseRecorder, rid, code string, status int, action, errorCode string) auditRow {
		t.Helper()
		if w.Code != status || !strings.Contains(w.Body.String(), code) {
			t.Fatalf("应 %d %s，实际 %d：%s", status, code, w.Code, w.Body.String())
		}
		row := waitAuditRows(t, ctx, db, rid, 1)[0]
		if row.Action != action || row.Result != audit.ResultFailure || row.ErrorCode != errorCode || row.HTTPStatus != status {
			t.Fatalf("被拒请求的审计行不符：%+v", row)
		}
		if row.CredentialType != audit.CredentialAnonymous || row.ActorUserID != "" || row.ActorUsername != "" {
			t.Fatalf("身份没解析出来的行只能记匿名：%+v", row)
		}
		return row
	}

	// 1) 形态非法 PAT（mfp_ + 非 43 位 base62）：内省器的本地预检直接判"明确无效"，不打账号服务。
	//    必须装配内省器——未装配（AUTH_URL 未配置）时任何 mfp_ 前缀都按依赖不可用回 503，见第 2 条。
	withPAT := newVerifier(t, "http://127.0.0.1:1/jwks")
	withPAT.SetPAT(auth.NewPATIntrospector("http://127.0.0.1:1"))
	patRouter := gin.New()
	New(s, catalog.New("", 0), withPAT).Register(patRouter)
	w, rid := do(patRouter, http.MethodPost, "/api/community/topics", `{"board_code":"qa","title":"x","content":"y"}`,
		auth.PATPrefix+"not-a-real-token", "rid-audit-badpat")
	row := assertRejected(w, rid, auth.CodeInvalidToken, http.StatusUnauthorized, "topic.created", "http_401")
	// 被身份拒绝的请求压根没进处理器，因此 route 仍是模板、也没有被动对象与摘要。
	if _, ok := auditActions[row.RequestMethod+" "+row.Route]; !ok {
		t.Fatalf("被拒请求的 route 也必须是登记过的模板：%+v", row)
	}
	if row.TargetType != "" {
		t.Fatalf("身份拒绝发生在业务之前，不该有被动对象：%+v", row)
	}

	// 2) 账号服务不可达（这里等价于"未配置 AUTH_URL / 未装配内省器"）：形态合法的 PAT 回 503。
	bare := newVerifier(t, "http://127.0.0.1:1/jwks")
	bareRouter := gin.New()
	New(s, catalog.New("", 0), bare).Register(bareRouter)
	w, rid = do(bareRouter, http.MethodPost, "/api/favorites/toggle",
		`{"target_type":"work","target_id":"`+uuid.NewString()+`"}`, patBearer('q'), "rid-audit-pat503")
	assertRejected(w, rid, auth.CodeAuthUnavailable, http.StatusServiceUnavailable, "favorite.toggled", "http_503")

	// 3) 无效 JWT 不 abort：按匿名继续，由路由闸门回 401，审计行记的是闸门登记的稳定码
	//    （不是 http_401）——这条把"身份中间件只对 PAT 路径 abort"钉住。
	w, rid = do(bareRouter, http.MethodPost, "/api/community/topics",
		`{"board_code":"qa","title":"x","content":"y"}`, "not-a-jwt", "rid-audit-badjwt")
	assertRejected(w, rid, "authentication_required", http.StatusUnauthorized, "topic.created", "authentication_required")
}
