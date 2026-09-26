package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

func TestTopicsHaveNoLanguageColumnOrQuery(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	token := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"member"}, []string{auth.PermissionPostCreate})
	w := opsCall(t, router, http.MethodPost, "/api/community/topics",
		`{"board_code":"qa","title":"语言维度退役用例","content":"正文"}`, token)
	if w.Code != 200 {
		t.Fatalf("发帖 HTTP %d：%s", w.Code, w.Body.String())
	}
	created := map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析发帖响应: %v", err)
	}
	if _, ok := created["language"]; ok {
		t.Fatalf("主题对象不应返回 language：%s", w.Body.String())
	}
	w = opsCall(t, router, http.MethodGet, "/api/community/topics?board_code=qa&language=ja", "", "")
	if w.Code != 400 || opsErrorCode(t, w) != "invalid_query_param" {
		t.Fatalf("旧 language 查询应拒绝，实际 %d：%s", w.Code, w.Body.String())
	}
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='community' AND table_name='topics' AND column_name='language')").Scan(&exists); err != nil {
		t.Fatalf("查 language 列: %v", err)
	}
	if exists {
		t.Fatal("community.topics.language 仍存在")
	}
}
