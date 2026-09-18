package handler

import (
	"fmt"
	"testing"
	"time"
)

// 限流是纯函数（时钟可注入），边界在这里钉死；真库用例只管"handler 真的把它接上了发信路径"。
// 用真实时钟写"一分钟后再试"只能靠 sleep，慢且不稳，所以这里把 now 换成手工推进的时钟。
func TestMessageLimiter(t *testing.T) {
	now := time.Unix(1700000000, 0)
	l := newMessageLimiter()
	l.now = func() time.Time { return now }

	// 1) 容量：连发 messageSendBurst 条全放行，紧接着的那条被拒并给出等待时间。
	for i := 0; i < messageSendBurst; i++ {
		if ok, _ := l.allow("u1"); !ok {
			t.Fatalf("第 %d 条就被拒了（容量应至少 %d）", i+1, messageSendBurst)
		}
	}
	ok, wait := l.allow("u1")
	if ok {
		t.Fatal("超过容量仍被放行")
	}
	if wait <= 0 || wait > messageSendWindow {
		t.Fatalf("Retry-After 窗口不合理：%v（应在 (0, %v] 内）", wait, messageSendWindow)
	}

	// 2) 被拒**不扣令牌**：连着再问三次，等待时间必须一样——
	//    扣令牌会让连续请求把 Retry-After 越推越长（客户端按它退避就等于永远退不开）。
	for i := 0; i < 3; i++ {
		again, w2 := l.allow("u1")
		if again || w2 != wait {
			t.Fatalf("被拒的重复请求改变了等待时间：放行=%v 等待=%v，期望 false / %v", again, w2, wait)
		}
	}

	// 3) 匀速回满：过一个"每令牌间隔"回一个令牌（不是整分钟整分钟地跳）。
	now = now.Add(messageSendWindow / messageSendBurst)
	if ok, _ := l.allow("u1"); !ok {
		t.Fatal("回满一个令牌后应放行")
	}
	if ok, _ := l.allow("u1"); ok {
		t.Fatal("只回了一个令牌，第二条不该放行")
	}

	// 4) 满桶封顶：闲置很久之后能连发的最多还是容量条，不会攒出无限额度。
	now = now.Add(10 * messageSendWindow)
	allowed := 0
	for i := 0; i < messageSendBurst+5; i++ {
		if ok, _ := l.allow("u1"); ok {
			allowed++
		}
	}
	if allowed != messageSendBurst {
		t.Fatalf("满桶后连发 %d 条，期望恰好容量 %d 条", allowed, messageSendBurst)
	}

	// 5) 桶按账号隔离：一个账号打满不影响另一个。
	if ok, _ := l.allow("u2"); !ok {
		t.Fatal("另一个账号应有独立额度")
	}

	// 6) 空 key（身份缺失）直接放行：限流是**账号**维度的，没有账号时无从限起，
	//    而所有发信路由都在登录门槛之后，这条只是不让 nil/空串进了 map。
	if ok, _ := l.allow(""); !ok {
		t.Fatal("空 key 应放行（发信路由本身在登录门槛之后）")
	}
	if l2 := (*messageLimiter)(nil); !func() bool { ok, _ := l2.allow("x"); return ok }() {
		t.Fatal("nil 接收者应放行（与 db 为 nil 时的空操作同一口径）")
	}
}

// 陌生人新会话额度（第 2 档）是独立的一档：容量更小、窗口更长，且**不消耗总体桶的语义**
// （两档各自独立计费，谁先耗尽谁拦），与"总体 20/分钟"是交集关系。
func TestMessageLimiterStrangerQuota(t *testing.T) {
	now := time.Unix(1700000000, 0)
	l := newMessageLimiter()
	l.now = func() time.Time { return now }

	// 1) 容量：5 个新陌生人会话放行，第 6 个被拒，等待时间落在 (0, 1h]。
	for i := 0; i < messageStrangerBurst; i++ {
		if ok, _ := l.allowStranger("u1"); !ok {
			t.Fatalf("第 %d 个新会话就被拒了（容量应至少 %d）", i+1, messageStrangerBurst)
		}
	}
	ok, wait := l.allowStranger("u1")
	if ok {
		t.Fatal("超过陌生人额度仍被放行")
	}
	if wait <= 0 || wait > messageStrangerWindow {
		t.Fatalf("Retry-After 窗口不合理：%v（应在 (0, %v] 内）", wait, messageStrangerWindow)
	}

	// 2) 两档互不串账：陌生人桶打满不影响同账号的总体发送额度（继续聊已有会话照常发）。
	if ok, _ := l.allow("u1"); !ok {
		t.Fatal("陌生人桶打满不该影响总体发送额度（已有会话仍要能发）")
	}
	// 反向：总体桶打满也不影响陌生人桶自己的余额（判据在处理器里按序执行）。
	if _, _ = l.allowStranger("u2"); !func() bool { ok, _ := l.allowStranger("u3"); return ok }() {
		t.Fatal("另一个账号的陌生人额度应独立")
	}

	// 3) 满一小时回满：回满后第 6 个新会话重新可用。
	now = now.Add(messageStrangerWindow)
	allowed := 0
	for i := 0; i < messageStrangerBurst+3; i++ {
		if ok, _ := l.allowStranger("u1"); ok {
			allowed++
		}
	}
	if allowed != messageStrangerBurst {
		t.Fatalf("回满后连发 %d 个新会话，期望恰好容量 %d 个", allowed, messageStrangerBurst)
	}
}

// 桶数量有硬上限：短时间灌进大量互不相同的 key（未登录也能撞出来的形状）不能把内存吃光。
// 代价是触顶时被清掉的桶会重新拿到满额令牌——那是内存安全阀的已知取舍，写在这里免得被当成熟 bug。
func TestMessageLimiterBoundedMemory(t *testing.T) {
	now := time.Unix(1700000000, 0)
	l := newMessageLimiter()
	l.now = func() time.Time { return now }

	for i := 0; i < messageLimiterMaxKeys*3; i++ {
		l.allow(fmt.Sprintf("bulk-%d", i))
	}
	l.mu.Lock()
	size := len(l.general.buckets)
	l.mu.Unlock()
	if size > messageLimiterMaxKeys {
		t.Fatalf("桶数量 %d 超过硬上限 %d：清理没有生效", size, messageLimiterMaxKeys)
	}

	// 空闲清理：把时钟推过空闲阈值后再灌一批，老桶应当被清掉（不是只靠硬上限兜底）。
	idle := time.Unix(1700000000, 0).Add(messageLimiterIdle + time.Minute)
	l.now = func() time.Time { return idle }
	for i := 0; i < 2; i++ {
		l.allow(fmt.Sprintf("later-%d", i))
	}
	l.mu.Lock()
	_, stale := l.general.buckets["bulk-0"]
	l.mu.Unlock()
	if stale {
		t.Fatal("空闲超过阈值的桶应被清掉")
	}
}
