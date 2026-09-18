package audit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// quietLogs 把本文件的 slog 输出丢掉：队列满与落库失败都会被 Error 级记录，
// 那正是用例要验证的行为，但刷屏会淹没真正的失败信息。
func quietLogs(t *testing.T) {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
}

// TestRecordNeverBlocksWhenQueueIsFull 是契约 §3 的硬要求：队列满时 Record 必须立即丢弃并计数，
// **绝不等待**业务响应。这里把落库函数卡住（sink 阻塞）让队列一定被填满，再灌两倍容量：
// 如果 Record 会阻塞，这个用例会直接挂死或超时。
func TestRecordNeverBlocksWhenQueueIsFull(t *testing.T) {
	quietLogs(t)
	release := make(chan struct{})
	r := NewRecorder(nil, ServiceName)
	r.sink = func(Entry) error { <-release; return nil }

	const attempts = 2048 // 队列容量 1024 的两倍
	start := time.Now()
	for i := 0; i < attempts; i++ {
		r.Record(Entry{Action: "topic.created"})
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Record 被阻塞了 %s：队列满时应当立即丢弃", elapsed)
	}
	if r.Dropped() == 0 {
		t.Fatal("队列已满但 Dropped() = 0：丢弃没有被计数（丢行必须可观测）")
	}
	close(release)
	r.Close()
}

// TestRecorderDrainsQueueOnClose：Close 要排空队列再停 goroutine（测试收尾依赖它，
// 否则"写完就断言"会看到随机行数）。
func TestRecorderDrainsQueueOnClose(t *testing.T) {
	var mu sync.Mutex
	got := []Entry{}
	r := NewRecorder(nil, ServiceName)
	r.sink = func(e Entry) error {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
		return nil
	}
	for i := 0; i < 50; i++ {
		r.Record(Entry{Action: "topic.created"})
	}
	r.Close()
	if len(got) != 50 || r.Written() != 50 {
		t.Fatalf("排空后落库 %d 行 / Written=%d，期望 50", len(got), r.Written())
	}
	if r.Dropped() != 0 {
		t.Fatalf("队列没满却丢了 %d 行", r.Dropped())
	}
	// 关闭之后再 Record 不能 panic（也不该再入队）。
	r.Record(Entry{Action: "topic.created"})
	if len(got) != 50 {
		t.Fatal("Close 之后仍在写审计")
	}
}

// TestRecordWriteFailureIsLoggedAndNotCounted：落库失败只记日志、丢这一行，不计入 Dropped
// （Dropped 是"队列满"的计数，两者分开才看得清是"写不动"还是"来不及写"）。
func TestRecordWriteFailureIsLoggedAndNotCounted(t *testing.T) {
	quietLogs(t)
	r := NewRecorder(nil, ServiceName)
	r.sink = func(Entry) error { return errors.New("boom") }
	r.Record(Entry{Action: "topic.created"})
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if r.Written() != 0 {
		t.Fatalf("写入计数 = %d，期望 0（sink 一直失败）", r.Written())
	}
	if r.Dropped() != 0 {
		t.Fatalf("落库失败不该计入 Dropped：%d", r.Dropped())
	}
	r.Close()
}

// TestRecordSyncAndNilRecorder：RecordSync 是同步路径（真库用例与"必须强一致"的少量动作用）；
// nil Recorder 与 nil db 都必须安全空转，不能 panic——不建库的进程与用例会走到这里。
func TestRecordSyncAndNilRecorder(t *testing.T) {
	r := NewRecorder(nil, ServiceName)
	defer r.Close()
	if err := r.RecordSync(context.Background(), Entry{Action: "topic.created"}); err != nil {
		t.Fatalf("nil db 的 RecordSync 应当空转成功：%v", err)
	}
	var nilRecorder *Recorder
	if err := nilRecorder.RecordSync(context.Background(), Entry{Action: "x"}); err != nil {
		t.Fatalf("nil Recorder 的 RecordSync 应为 no-op：%v", err)
	}
	nilRecorder.Record(Entry{Action: "x"}) // 不应 panic
	if r.Written() != 0 {
		t.Fatal("nil db 的空转路径不该产生写入计数")
	}
	if err := r.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
}
