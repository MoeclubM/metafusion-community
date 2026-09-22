package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// NotifyService 以受限服务身份投递：不转发用户凭据，作者取事件数据。
func TestNotifyServiceUsesServiceIdentity(t *testing.T) {
	var gotAuth, gotToken string
	var gotBody Notification
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotToken = r.Header.Get(InternalTokenHeader)
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()
	c := New(srv.URL, 2*time.Second)
	c.SetInternalToken("s3cr3t")
	// ctx 带用户凭据，但服务身份必须忽略它。
	ctx := auth.WithCredentials(context.Background(), "user-bearer", "cookie-val")
	err := c.NotifyService(ctx, Notification{
		RecipientID: "11111111-1111-1111-1111-111111111111",
		Type: NotificationCommentReplied, SubjectType: "topic", SubjectID: "t",
		DedupeKey: "k", EventID: "e1", ActorID: "22222222-2222-2222-2222-222222222222", ActorName: "bob",
		Payload: map[string]any{"via": "topic_reply"},
	})
	if err != nil {
		t.Fatalf("NotifyService: %v", err)
	}
	if gotToken != "s3cr3t" {
		t.Fatalf("必须带共享密钥: %q", gotToken)
	}
	if gotAuth != "" {
		t.Fatalf("不得转发用户令牌: %q", gotAuth)
	}
	if gotBody.ActorID != "22222222-2222-2222-2222-222222222222" || gotBody.ActorName != "bob" {
		t.Fatalf("作者快照必须原样透传: %+v", gotBody)
	}
	if gotBody.EventID != "e1" {
		t.Fatalf("事件身份必须稳定透传: %+v", gotBody)
	}
}

// 未配置密钥不发。
func TestNotifyServiceNotConfigured(t *testing.T) {
	c := New("http://127.0.0.1:1", time.Second)
	c.SetInternalToken("")
	if err := c.NotifyService(context.Background(), Notification{}); err != ErrNotConfigured {
		t.Fatalf("应 ErrNotConfigured，实际 %v", err)
	}
}
