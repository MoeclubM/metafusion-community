package handler

import (
	"math"
	"sort"
	"sync"
	"time"
)

// 发私信的按账号限流 —— 反骚扰的**最小约束**（拉黑/举报/静默留给 F3）。
//
// 为什么必须在应用层补：网关的 api_limit 是**按 IP** 的（30r/s，burst 50），
// 对"一个账号每分钟刷上千条"毫无约束力——一个 IP 完全发得起；而本服务此前没有任何限流
// （2026-09 审计 ops 表：community 全仓限流 0 命中）。这里补的是按**发送者账号**的令牌桶：
// 桶容量 messageSendBurst、按 messageSendWindow 匀速回满，每次发信扣一个令牌。
// 超限回 429 + 稳定机器码 rate_limited + Retry-After（秒）——调用方按码分支，不解析文案。
//
// 已知边界（本次刻意不做，写清楚免得被当成"已经防住了"）：
//   - 桶在**进程内存**里：多副本部署时每个副本各有一份，实际放行量是副本数的倍数。
//     要跨副本一致得上共享存储（本服务没接 Redis），那是另一轮的事，不是调大这个常量能解决的。
//   - 只限"发多快"，不限"发给谁"：同一个人被反复发仍能发满这个额度，
//     针对个人的收敛（拉黑、举报、静默期）需要收件人侧的设置，留给 F3。
//   - 只挂在发信（POST /api/messages/with/:id）上。标记已读本身是幂等的
//     （UPDATE ... WHERE read_at IS NULL），重复调用第二次影响 0 行，不需要限。
const (
	// messageSendBurst 是桶容量 = 一次突发能连发的条数，也是每分钟的回满量。
	messageSendBurst = 20
	// messageSendWindow 是回满一个空桶的时间。
	messageSendWindow = time.Minute
	// messageLimiterMaxKeys 是桶数量上限：桶按账号建，不清理的话长跑实例会攒"每个访客一个桶"。
	// 超过上限时顺手清一遍空闲超过 messageLimiterIdle 的桶——本服务没有后台 ticker
	// （见 README「没有的写能力」），所以清理是挂在请求路径上的惰性动作，不是定时任务。
	messageLimiterMaxKeys = 4096
	messageLimiterIdle    = 10 * time.Minute
)

// messageBucket 是一个账号的令牌桶。tokens 用浮点是为了"按经过的时间匀速回满"，
// 而不是整分钟整分钟地跳（租约式的窗口会让"卡在整点前后"的两次发送拿到双份额度）。
type messageBucket struct {
	tokens float64
	last   time.Time
}

// messageLimiter 是并发安全的按账号令牌桶。now 可注入，用例据此把时间做成确定性的
// （用真实时钟写"一分钟后再试"的用例只能靠 sleep，慢且不稳）。
type messageLimiter struct {
	mu      sync.Mutex
	buckets map[string]*messageBucket
	now     func() time.Time
}

func newMessageLimiter() *messageLimiter {
	return &messageLimiter{buckets: map[string]*messageBucket{}, now: time.Now}
}

// allow 判定某个账号这一次能不能发，并给出发不出去时"多久之后再有额度"。
// 额度不足时**不扣令牌**（否则连续请求会把等待时间越推越长，Retry-After 失真）。
func (l *messageLimiter) allow(key string) (bool, time.Duration) {
	if l == nil || key == "" {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) >= messageLimiterMaxKeys {
		l.evictLocked(now)
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &messageBucket{tokens: messageSendBurst, last: now}
		l.buckets[key] = b
	}
	refill := messageSendBurst / messageSendWindow.Seconds() // 每秒回满的令牌数
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(messageSendBurst, b.tokens+elapsed*refill)
		b.last = now
	}
	if b.tokens < 1 {
		// 再等 (1 - tokens) 个令牌，向上取整到秒：Retry-After 只说秒。
		wait := time.Duration(math.Ceil((1-b.tokens)/refill)) * time.Second
		return false, wait
	}
	b.tokens--
	return true, 0
}

// evictLocked 清桶。调用方已持锁，且只在桶数量触顶时执行，因此正常流量下不会退化成
// "每个请求都扫一遍全部桶"。
//
// 两步：先清空闲超过 messageLimiterIdle 的；如果清完仍然触顶（短时间内出现大量互不相同的账号，
// 也就是"有人在拿账号 id 灌水"），按最后一次使用时间丢掉最旧的一半——**硬上限优先于保留每个桶**。
// 代价说清楚：被清掉的桶会重新拿到满额令牌，所以这一步是内存安全阀，不是精确限流；
// 宁可放过一个刚被清掉的账号，也不能让一个未认证过的字节流把进程内存吃光。
func (l *messageLimiter) evictLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > messageLimiterIdle {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) < messageLimiterMaxKeys {
		return
	}
	type entry struct {
		key  string
		last time.Time
	}
	all := make([]entry, 0, len(l.buckets))
	for k, b := range l.buckets {
		all = append(all, entry{k, b.last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
	for _, e := range all[:len(all)/2] {
		delete(l.buckets, e.key)
	}
}
