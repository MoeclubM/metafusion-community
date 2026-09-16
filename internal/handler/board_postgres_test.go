package handler

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
	"github.com/MoeclubM/metafusion-community/internal/testutil"
)

// opsFixture 是运营写接口（置顶、板块配置）的夹具：真库 + 目录桩 + 可验签的 router。
// 板块会被用例改写，因此每次从"清空后重新播种"的状态开始，避免用例互相依赖。
func opsFixture(t *testing.T) (context.Context, *sql.DB, http.Handler, *rsa.PrivateKey, string) {
	t.Helper()
	dsn := testutil.DSN(t)
	db := testutil.Database(t)
	ctx := context.Background()

	s, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err = s.Init(ctx); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	for _, stmt := range []string{
		"DELETE FROM community.topic_tags",
		"DELETE FROM community.tags",
		"DELETE FROM community.posts",
		"DELETE FROM community.topics",
		"DELETE FROM community.boards",
	} {
		if _, err = db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("cleanup %s: %v", stmt, err)
		}
	}
	if err = Seed(ctx, db); err != nil {
		t.Fatalf("seed boards: %v", err)
	}

	// 目录桩：实体一律可见（发主题要锚定可见实体）。
	entityID := uuid.NewString()
	catalogStub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": entityID, "kind": "work", "title": "测试作品", "status": "published"})
	}))
	t.Cleanup(catalogStub.Close)

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	srv, kid := jwksServer(t, key)
	router := gin.New()
	New(s, catalog.New(catalogStub.URL, 2*time.Second), newVerifier(t, srv.URL)).Register(router)
	return ctx, db, router, key, kid
}

func opsCall(t *testing.T, router http.Handler, method, path, payload, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func opsErrorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	code, _ := out["error"].(string)
	return code
}

// 置顶是运营动作：无 community.topic.pin 一律 403 且**不写库**，持码则写既有 is_pinned 列。
// 响应沿用主题形状（含 is_pinned），前端不需要为置顶另写解析。
func TestTopicPinRequiresCodeAndPersists(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	authorToken := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"member"}, []string{auth.PermissionPostCreate})
	pinToken := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"community_admin"}, []string{auth.PermissionPostCreate, auth.PermissionTopicPin})

	w := opsCall(t, router, http.MethodPost, "/api/community/topics", `{"board_code":"qa","title":"置顶用例","content":"正文"}`, authorToken)
	if w.Code != 200 {
		t.Fatalf("发主题 HTTP %d: %s", w.Code, w.Body.String())
	}
	created := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	topicID, _ := created["id"].(string)
	if topicID == "" {
		t.Fatalf("发主题未返回 id: %s", w.Body.String())
	}
	isPinned := func() bool {
		t.Helper()
		var pinned bool
		if err := db.QueryRowContext(ctx, "SELECT is_pinned FROM community.topics WHERE id=$1", topicID).Scan(&pinned); err != nil {
			t.Fatalf("查 is_pinned: %v", err)
		}
		return pinned
	}

	// 1) 只有发帖码：403 forbidden，且库里的置顶状态不变（判定在写之前）。
	if w = opsCall(t, router, http.MethodPut, "/api/community/topics/"+topicID+"/pin", `{"pinned":true}`, authorToken); w.Code != 403 || opsErrorCode(t, w) != "forbidden" {
		t.Fatalf("无置顶码应 403 forbidden，实际 %d（%s）", w.Code, w.Body.String())
	}
	if isPinned() {
		t.Fatal("无码请求不得写库")
	}

	// 2) 持置顶码：200，响应与库都要变成已置顶。
	w = opsCall(t, router, http.MethodPut, "/api/community/topics/"+topicID+"/pin", `{"pinned":true}`, pinToken)
	if w.Code != 200 {
		t.Fatalf("持码置顶 HTTP %d: %s", w.Code, w.Body.String())
	}
	out := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if out["id"] != topicID || out["is_pinned"] != true {
		t.Fatalf("置顶响应形状不符: %s", w.Body.String())
	}
	if !isPinned() {
		t.Fatal("置顶未落库")
	}

	// 3) 取消置顶：同一个端点，显式 pinned=false。
	w = opsCall(t, router, http.MethodPut, "/api/community/topics/"+topicID+"/pin", `{"pinned":false}`, pinToken)
	if w.Code != 200 || isPinned() {
		t.Fatalf("取消置顶失败: %d %s", w.Code, w.Body.String())
	}

	// 4) 缺 pinned 字段：400 invalid_payload（缺字段与 false 不可混淆）。
	if w = opsCall(t, router, http.MethodPut, "/api/community/topics/"+topicID+"/pin", `{}`, pinToken); w.Code != 400 || opsErrorCode(t, w) != "invalid_payload" {
		t.Fatalf("缺 pinned 应 400 invalid_payload，实际 %d（%s）", w.Code, w.Body.String())
	}

	// 5) 不存在的主题与非法 uuid 都按不存在处理（不泄露存在性，也不把 pq 解析错误当 500）。
	for _, path := range []string{"/api/community/topics/" + uuid.NewString() + "/pin", "/api/community/topics/not-a-uuid/pin"} {
		if w = opsCall(t, router, http.MethodPut, path, `{"pinned":true}`, pinToken); w.Code != 404 {
			t.Fatalf("PUT %s 应 404，实际 %d（%s）", path, w.Code, w.Body.String())
		}
	}

	// 6) 匿名：401 authentication_required（与其它写接口同一口径）。
	if w = opsCall(t, router, http.MethodPut, "/api/community/topics/"+topicID+"/pin", `{"pinned":true}`, ""); w.Code != 401 || opsErrorCode(t, w) != "authentication_required" {
		t.Fatalf("匿名置顶应 401，实际 %d（%s）", w.Code, w.Body.String())
	}
}

// boardNames 造一份四语齐备的语种 map：接口只收多语言 map，缺语种一律 400。
func boardNames(zhCN, zhTW, jaJP, enUS string) string {
	return `{"zh-CN":"` + zhCN + `","zh-TW":"` + zhTW + `","ja-JP":"` + jaJP + `","en-US":"` + enUS + `"}`
}

// responseLocales 断言响应里的语种 map 字段确实是 map 形状（前端按 locale 取键，不接受单值）。
func responseLocales(t *testing.T, board map[string]any, field string) map[string]string {
	t.Helper()
	raw, ok := board[field]
	if !ok {
		t.Fatalf("板块响应缺少 %s 字段: %v", field, board)
	}
	values, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("%s 应是语种 map，实际 %T: %v", field, raw, raw)
	}
	out := map[string]string{}
	for k, v := range values {
		s, _ := v.(string)
		out[k] = s
	}
	return out
}

// 板块配置：无 community.board.manage 一律 403；名称与描述是**四语 map**（缺语种 400
// four_locale_names_required）；单值 name/description 只作为回退值由多语言 map 派生；只改传入字段。
func TestBoardUpdateRequiresCodeAndValidatesLocales(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	manageToken := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"community_admin"}, []string{auth.PermissionBoardManage})
	postToken := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"member"}, []string{auth.PermissionPostCreate})

	// 库侧形状：多语言列是权威，单值列是与 zh-CN 同步的回退值。
	readBoard := func() (string, string, string, string, bool, bool, int) {
		t.Helper()
		var names, descriptions, name, description string
		var enabled, inFeed bool
		var order int
		if err := db.QueryRowContext(ctx,
			"SELECT names::text,descriptions::text,name,description,is_enabled,show_in_feed,sort_order FROM community.boards WHERE code=$1", "qa").Scan(
			&names, &descriptions, &name, &description, &enabled, &inFeed, &order); err != nil {
			t.Fatalf("查板块: %v", err)
		}
		return names, descriptions, name, description, enabled, inFeed, order
	}

	// 1) 只有发帖码：403 且不改库。
	if w := opsCall(t, router, http.MethodPut, "/api/community/boards/qa", `{"color":"sky","is_enabled":false}`, postToken); w.Code != 403 || opsErrorCode(t, w) != "forbidden" {
		t.Fatalf("无板块码应 403 forbidden，实际 %d（%s）", w.Code, w.Body.String())
	}
	if _, _, _, _, enabled, _, _ := readBoard(); !enabled {
		t.Fatal("无码请求不得写库")
	}

	// 2) 持码：四语名称 + 四语描述 + 颜色/图标/排序/两个开关，响应与库都要变。
	payload := `{"names":` + boardNames("问答", "問答", "質問", "Q&A") +
		`,"descriptions":` + boardNames("使用问题", "使用問題", "使い方の質問", "Usage questions") +
		`,"color":"sky","icon":"LifeBuoy","sort_order":35,"is_enabled":false,"show_in_feed":false}`
	w := opsCall(t, router, http.MethodPut, "/api/community/boards/qa", payload, manageToken)
	if w.Code != 200 {
		t.Fatalf("持码改板块 HTTP %d: %s", w.Code, w.Body.String())
	}
	board := map[string]any{}
	_ = json.Unmarshal(w.Body.Bytes(), &board)
	if board["code"] != "qa" || board["color"] != "sky" || board["icon"] != "LifeBuoy" || board["is_enabled"] != false || board["show_in_feed"] != false {
		t.Fatalf("板块响应不符: %s", w.Body.String())
	}
	names := responseLocales(t, board, "names")
	if names["zh-CN"] != "问答" || names["zh-TW"] != "問答" || names["ja-JP"] != "質問" || names["en-US"] != "Q&A" {
		t.Fatalf("响应 names 未按四语原样返回: %v（%s）", names, w.Body.String())
	}
	descriptions := responseLocales(t, board, "descriptions")
	if descriptions["ja-JP"] != "使い方の質問" || descriptions["zh-CN"] != "使用问题" {
		t.Fatalf("响应 descriptions 未按四语返回: %v（%s）", descriptions, w.Body.String())
	}
	// 单值列是回退值：由多语言 map 的 zh-CN 派生，老读方拿到的不再是空串。
	if board["name"] != "问答" || board["description"] != "使用问题" {
		t.Fatalf("单值回退字段应由 zh-CN 派生: %s", w.Body.String())
	}
	storedNames, storedDescriptions, name, description, enabled, inFeed, order := readBoard()
	if !strings.Contains(storedNames, "質問") || !strings.Contains(storedDescriptions, "使い方の質問") {
		t.Fatalf("多语言列未按载荷落库: names=%s descriptions=%s", storedNames, storedDescriptions)
	}
	if name != "问答" || description != "使用问题" || enabled || inFeed || order != 35 {
		t.Fatalf("板块未按载荷落库: name=%q description=%q enabled=%v inFeed=%v order=%d", name, description, enabled, inFeed, order)
	}

	// 3) 只改传入字段：再发一个只带开关的载荷，多语言列、回退单值与排序必须原样保留。
	w = opsCall(t, router, http.MethodPut, "/api/community/boards/qa", `{"is_enabled":true}`, manageToken)
	if w.Code != 200 {
		t.Fatalf("局部更新 HTTP %d: %s", w.Code, w.Body.String())
	}
	if storedNames, _, name, description, enabled, _, order = readBoard(); !enabled || name != "问答" || description != "使用问题" || order != 35 || !strings.Contains(storedNames, "Q&A") {
		t.Fatalf("局部更新不应清空其它字段: name=%q description=%q enabled=%v order=%d names=%s", name, description, enabled, order, storedNames)
	}

	// 4) 缺语种：400 four_locale_names_required，错误信息里列出缺的语种（与目录侧同一标识）。
	//    单值 name 不再是可接受的输入形态：不给"绕过四语校验"的后门，也不猜语种。
	partialNames := `{"names":` + boardNames("问答", "問答", "  ", "Q&A") + `}`
	for _, payload := range []string{
		`{"names":{"zh-CN":"问答","zh-TW":"問答","en-US":"Q&A"}}`, // 缺 ja-JP
		`{"names":{"zh-CN":"问答"}}`,                            // 只给一个语种
		`{"name":"问答"}`,                                       // 单值形态不再可写
		partialNames,                                          // ja-JP 只给了空白
		`{"descriptions":{"zh-CN":"使用问题"}}`,                   // 描述同样要求四语
	} {
		w = opsCall(t, router, http.MethodPut, "/api/community/boards/qa", payload, manageToken)
		if w.Code != 400 {
			t.Fatalf("载荷 %s 应 400，实际 %d（%s）", payload, w.Code, w.Body.String())
		}
		code := opsErrorCode(t, w)
		// 单值形态连语言维度都没有，落到"空载荷"这条通用校验；其余一律是四语校验的专属错误码。
		want := "four_locale_names_required"
		if !strings.Contains(payload, `"names":{`) && !strings.Contains(payload, `"descriptions":{`) {
			want = "invalid_payload"
		}
		if !strings.Contains(code, want) {
			t.Fatalf("载荷 %s 的错误码 = %q，期望含 %q", payload, code, want)
		}
	}
	if storedNames, storedDescriptions, name, description, _, _, _ = readBoard(); name != "问答" || description != "使用问题" || !strings.Contains(storedNames, "Q&A") || !strings.Contains(storedDescriptions, "使い方の質問") {
		t.Fatalf("被拒的请求不得写库: name=%q description=%q names=%s descriptions=%s", name, description, storedNames, storedDescriptions)
	}

	// 5) 描述允许显式清空：四语全传空串即清空（名称不受影响）。
	if w = opsCall(t, router, http.MethodPut, "/api/community/boards/qa", `{"descriptions":`+boardNames("", "", "", "")+`}`, manageToken); w.Code != 200 {
		t.Fatalf("清空描述 HTTP %d: %s", w.Code, w.Body.String())
	}
	if storedNames, storedDescriptions, name, description, _, _, _ = readBoard(); description != "" || name != "问答" || !strings.Contains(storedNames, "Q&A") || strings.Contains(storedDescriptions, "使い方の質問") {
		t.Fatalf("描述应被清空且名称保留: name=%q description=%q descriptions=%s", name, description, storedDescriptions)
	}

	// 6) 空载荷、空颜色/图标、未知板块、code 不可改（与名称/描述无关的通用边界）。
	if w = opsCall(t, router, http.MethodPut, "/api/community/boards/qa", `{}`, manageToken); w.Code != 400 || opsErrorCode(t, w) != "invalid_payload" {
		t.Fatalf("空载荷应 400 invalid_payload，实际 %d（%s）", w.Code, w.Body.String())
	}
	if w = opsCall(t, router, http.MethodPut, "/api/community/boards/qa", `{"color":"  "}`, manageToken); w.Code != 400 || opsErrorCode(t, w) != "invalid_payload" {
		t.Fatalf("空颜色应 400 invalid_payload，实际 %d（%s）", w.Code, w.Body.String())
	}
	if w = opsCall(t, router, http.MethodPut, "/api/community/boards/qa", `{"icon":""}`, manageToken); w.Code != 400 || opsErrorCode(t, w) != "invalid_payload" {
		t.Fatalf("空图标应 400 invalid_payload，实际 %d（%s）", w.Code, w.Body.String())
	}
	if w = opsCall(t, router, http.MethodPut, "/api/community/boards/nope", `{"is_enabled":true}`, manageToken); w.Code != 404 || opsErrorCode(t, w) != "not_found" {
		t.Fatalf("未知板块应 404 not_found，实际 %d（%s）", w.Code, w.Body.String())
	}
	// code 取路径值：载荷里再带一个 code 也不会改到别的板块（gin 忽略未声明字段）。
	if w = opsCall(t, router, http.MethodPut, "/api/community/boards/qa", `{"code":"casual","is_enabled":false}`, manageToken); w.Code != 200 || !strings.Contains(w.Body.String(), `"code":"qa"`) {
		t.Fatalf("板块 code 不可改，实际 %d（%s）", w.Code, w.Body.String())
	}

	// 7) 匿名：401。
	if w = opsCall(t, router, http.MethodPut, "/api/community/boards/qa", `{"is_enabled":true}`, ""); w.Code != 401 {
		t.Fatalf("匿名改板块应 401，实际 %d（%s）", w.Code, w.Body.String())
	}
}
