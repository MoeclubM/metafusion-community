package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// 论坛内容不再带语言维度（2026-09-17）：主题没有 language 字段，`?language=` 与发帖体里的 language
// 都只被**忽略**、不报错。老前端还在传这类参数，所以这条兼容行为必须钉住——报错会整页打不开。
func TestTopicsIgnoreLanguageDimension(t *testing.T) {
	ctx, db, router, key, kid := opsFixture(t)
	token := signTokenWith(t, key, kid, uuid.NewString(), "user", []string{"member"}, []string{auth.PermissionPostCreate})

	// 1) 发帖体带 language（旧前端行为）：接受、忽略、不落库。
	w := opsCall(t, router, http.MethodPost, "/api/community/topics",
		`{"board_code":"qa","title":"语言维度退役用例","content":"正文","language":"ja"}`, token)
	if w.Code != 200 {
		t.Fatalf("带 language 的发帖应被接受（忽略该字段），实际 %d：%s", w.Code, w.Body.String())
	}
	created := map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析发帖响应: %v", err)
	}
	if _, ok := created["language"]; ok {
		t.Fatalf("主题对象不应再返回 language：%s", w.Body.String())
	}
	if id, _ := created["id"].(string); id == "" {
		t.Fatalf("发帖未返回 id：%s", w.Body.String())
	}

	// 2) 列表：?language= 收到即忽略（200 而不是 400），条目同样没有 language。
	w = opsCall(t, router, http.MethodGet, "/api/community/topics?board_code=qa&language=ja", "", "")
	if w.Code != 200 {
		t.Fatalf("?language= 应被忽略而不是报错，实际 %d：%s", w.Code, w.Body.String())
	}
	list := struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}{}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析列表响应: %v", err)
	}
	if len(list.Items) == 0 {
		t.Fatalf("列表应至少含刚创建的主题：%s", w.Body.String())
	}
	for _, it := range list.Items {
		if _, ok := it["language"]; ok {
			t.Fatalf("列表条目不应再带 language：%v", it)
		}
	}

	// 3) 数据库层面确认迁移 000003 已生效：列不存在。
	var exists bool
	if err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='community' AND table_name='topics' AND column_name='language')").Scan(&exists); err != nil {
		t.Fatalf("查 language 列: %v", err)
	}
	if exists {
		t.Fatal("community.topics.language 仍存在，迁移 000003 未生效")
	}
}
