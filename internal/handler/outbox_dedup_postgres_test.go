package handler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

// A04-1：A/B/A 序列同一事件只投递一次（(收件人,事件) 幂等 + 稳定 event_id）。
func TestOutboxStableEventID_ABASequence(t *testing.T) {
	h := newServiceHarness(t)
	ctx := context.Background()
	now := time.Now()
	recipient := uuid.NewString()
	eventA := uuid.NewString()
	eventB := uuid.NewString()
	mk := func(event string) store.OutboxItem {
		return store.OutboxItem{
			ID: uuid.NewString(), RecipientID: recipient,
			Type: catalog.NotificationCommentReplied, SubjectType: "topic", SubjectID: uuid.NewString(),
			DedupeKey: "comment.replied:topic:t", EventID: event,
			ActorID: uuid.NewString(), ActorName: "a",
			Payload: map[string]any{"via": "topic_reply"},
		}
	}
	if ok, err := h.db.EnqueueNotification(ctx, mk(eventA), now); err != nil || !ok {
		t.Fatalf("A 首次入队: ok=%v err=%v", ok, err)
	}
	if ok, err := h.db.EnqueueNotification(ctx, mk(eventB), now); err != nil || !ok {
		t.Fatalf("B 入队: ok=%v err=%v", ok, err)
	}
	if ok, err := h.db.EnqueueNotification(ctx, mk(eventA), now); err != nil || ok {
		t.Fatalf("A 重复入队应幂等丢弃: ok=%v err=%v", ok, err)
	}
	items, total, err := h.db.ListOutbox(ctx, "", 20, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 2 {
		t.Fatalf("A/B/A 应只剩两行: total=%d %+v", total, items)
	}
	// 投递两次携带的 event_id 必须分别是 A 与 B（无新造 ID）。
	hd := New(h.db, h.client, newVerifier(t, "http://127.0.0.1:1/jwks"))
	res, err := hd.RetryDueOutbox(ctx, 50)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if res.Sent != 2 {
		t.Fatalf("应送达两行: %+v", res)
	}
	sent := h.sentWith()
	if len(sent) != 2 {
		t.Fatalf("应投递两次: %+v", sent)
	}
	seen := map[string]bool{}
	for _, d := range sent {
		seen[d.EventID] = true
		if d.EventID != eventA && d.EventID != eventB {
			t.Fatalf("投递携带未知事件: %+v", d)
		}
	}
	if !seen[eventA] || !seen[eventB] {
		t.Fatalf("A/B 各投递一次: %+v", seen)
	}
}

// A04-2：并发同事件只入队一行。
func TestOutboxConcurrentSameEventSingleRow(t *testing.T) {
	h := newServiceHarness(t)
	ctx := context.Background()
	now := time.Now()
	recipient := uuid.NewString()
	event := uuid.NewString()
	const n = 16
	var wg sync.WaitGroup
	oks := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := h.db.EnqueueNotification(ctx, store.OutboxItem{
				ID: uuid.NewString(), RecipientID: recipient,
				Type: catalog.NotificationCommentReplied, SubjectType: "topic", SubjectID: "t",
				DedupeKey: "k", EventID: event,
				ActorID: uuid.NewString(), ActorName: "a",
				Payload: map[string]any{},
			}, now)
			if err != nil {
				t.Errorf("enqueue %d: %v", i, err)
				return
			}
			oks[i] = ok
		}(i)
	}
	wg.Wait()
	count := 0
	for _, ok := range oks {
		if ok {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("并发同事件应恰好一行插入: %d/%d", count, n)
	}
	_, total, err := h.db.ListOutbox(ctx, "", 20, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 {
		t.Fatalf("库里应一行: total=%d", total)
	}
}

// A04-3：响应丢失后重投携带相同 event_id（稳定身份）。
func TestOutboxRedeliveryAfterResponseLostKeepsEventID(t *testing.T) {
	h := newServiceHarness(t)
	ctx := context.Background()
	now := time.Now()
	recipient := uuid.NewString()
	event := uuid.NewString()
	actor := uuid.NewString()
	if ok, err := h.db.EnqueueNotification(ctx, store.OutboxItem{
		ID: uuid.NewString(), RecipientID: recipient,
		Type: catalog.NotificationCommentReplied, SubjectType: "topic", SubjectID: "t",
		DedupeKey: "k", EventID: event, ActorID: actor, ActorName: "a",
		Payload: map[string]any{},
	}, now); err != nil || !ok {
		t.Fatalf("入队: ok=%v err=%v", ok, err)
	}
	// 首次目录已写入但响应丢失（桩 500×2 耗尽上游重试）：本服务记失败。
	h.failNext.Store(10)
	hd := New(h.db, h.client, newVerifier(t, "http://127.0.0.1:1/jwks"))
	res, err := hd.RetryDueOutbox(ctx, 50)
	if err != nil {
		t.Fatalf("first retry: %v", err)
	}
	if res.Sent != 0 || res.Retried != 1 {
		t.Fatalf("首次应失败: %+v", res)
	}
	first := h.sentWith()
	if len(first) == 0 {
		t.Fatalf("首次应有投递: %+v", first)
	}
	for _, d := range first {
		if d.EventID != event {
			t.Fatalf("首次投递事件必须为原事件: %+v", first)
		}
	}
	if _, err := h.db.DB().ExecContext(ctx, "UPDATE community.notification_outbox SET next_retry_at=now() WHERE status='pending'"); err != nil {
		t.Fatalf("拨到期: %v", err)
	}
	h.failNext.Store(0)
	h.mu.Lock()
	h.sent = nil
	h.mu.Unlock()
	res, err = hd.RetryDueOutbox(ctx, 50)
	if err != nil {
		t.Fatalf("second retry: %v", err)
	}
	if res.Sent != 1 {
		t.Fatalf("重投应送达: %+v", res)
	}
	second := h.sentWith()
	if len(second) != 1 || second[0].EventID != event {
		t.Fatalf("重投必须携带相同事件: %+v", second)
	}
	if second[0].EventID != first[0].EventID {
		t.Fatalf("两次投递事件不一致: %q vs %q", first[0].EventID, second[0].EventID)
	}
}

// A04-4：立即与后台共用领取约定（同一事件只投递一次）。
func TestImmediateAndWorkerShareClaim(t *testing.T) {
	h := newServiceHarness(t)
	ctx := context.Background()
	now := time.Now()
	item := store.OutboxItem{
		ID: uuid.NewString(), RecipientID: uuid.NewString(),
		Type: catalog.NotificationCommentReplied, SubjectType: "topic", SubjectID: "t",
		DedupeKey: "k", EventID: uuid.NewString(),
		ActorID: uuid.NewString(), ActorName: "a",
		Payload: map[string]any{},
	}
	if ok, err := h.db.EnqueueNotification(ctx, item, now); err != nil || !ok {
		t.Fatalf("入队: ok=%v err=%v", ok, err)
	}
	// 立即方先领走，后台再领应为空。
	got, err := h.db.ClaimOutboxByIDs(ctx, []string{item.ID}, now, 5*time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("立即应领到: %+v err=%v", got, err)
	}
	due, err := h.db.ClaimDueOutbox(ctx, now, 50, 5*time.Minute)
	if err != nil {
		t.Fatalf("worker claim: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("租约内后台不应再领到: %+v", due)
	}
	// 反向：后台先领走，立即方应跳过。
	item2 := store.OutboxItem{
		ID: uuid.NewString(), RecipientID: uuid.NewString(),
		Type: catalog.NotificationCommentReplied, SubjectType: "topic", SubjectID: "t",
		DedupeKey: "k2", EventID: uuid.NewString(),
		ActorID: uuid.NewString(), ActorName: "a",
		Payload: map[string]any{},
	}
	if ok, err := h.db.EnqueueNotification(ctx, item2, time.Now()); err != nil || !ok {
		t.Fatalf("入队2: ok=%v err=%v", ok, err)
	}
	due, err = h.db.ClaimDueOutbox(ctx, time.Now(), 50, 5*time.Minute)
	if err != nil || len(due) != 1 {
		t.Fatalf("后台应领到第二行: %+v err=%v", due, err)
	}
	got, err = h.db.ClaimOutboxByIDs(ctx, []string{item2.ID}, time.Now(), 5*time.Minute)
	if err != nil {
		t.Fatalf("立即 claim: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("后台已领走时立即方必须跳过: %+v", got)
	}
}
