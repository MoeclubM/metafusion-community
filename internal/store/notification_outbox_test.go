package store

import (
	"testing"
	"time"
)

// 退避必须是单调非减：重试间隔只增不减，避免失败风暴里越重试越密。
func TestOutboxRetryDelayBacksOff(t *testing.T) {
	prev := time.Duration(0)
	for attempts := 1; attempts <= 10; attempts++ {
		got := OutboxRetryDelay(attempts)
		if got <= 0 {
			t.Fatalf("attempts=%d 退避必须为正：%v", attempts, got)
		}
		if got < prev {
			t.Fatalf("attempts=%d 退避回退：%v < %v", attempts, got, prev)
		}
		prev = got
	}
	if got := OutboxRetryDelay(1); got != time.Minute {
		t.Fatalf("首次重试应 1 分钟：%v", got)
	}
	if OutboxMaxAttempts < 3 || OutboxTTL < 24*time.Hour {
		t.Fatalf("上限太小会让必须送达名存实亡：max=%d ttl=%v", OutboxMaxAttempts, OutboxTTL)
	}
}
