package handler

import (
	"math"
	"sort"
	"sync"
	"time"
)

// 发私信的**两档**按账号限流 —— 反骚扰的最小约束（拉黑/举报/静默期留给 F3）。
//
// 为什么必须在应用层补：网关的 api_limit 是**按 IP** 的（30r/s，burst 50），
// 对"一个账号每分钟刷上千条"毫无约束力——一个 IP 完全发得起；而本服务此前没有任何限流
// （2026-09 审计 ops 表：community 全仓限流 0 命中）。这里补两档：
//
//  1. **总体发送频率**（messageSendBurst / 窗口）：所有收件人合计，桶容量 20、每分钟回满。
//  2. **陌生人新会话额度**（messageStrangerBurst / 窗口）：只对"向还没有既有会话的人发起新会话"计费，
//     桶容量 5、每小时回满。它存在的前提是收件人侧的开关默认**允许**陌生人发信（见 000010 迁移）：
//     只靠"默认允许"会让任何人给任意 uuid 刷信，所以用发送侧的额度把默认允许的滥用面收窄。
//     同一对之间继续发（已有会话）不受这一档限制，只受第 1 档约束。
//
// 两档是**交集**：额度够但总体超频仍 429，反之亦然。超限一律 429 + 稳定机器码 rate_limited
// + Retry-After（秒）；是哪一档拦的在审计行 changes 里区分（limit=sender_window / limit=new_stranger），
// 机器码统一，免得调用方为两档各写一条分支。
//
// 已知边界（本次刻意不做，写清楚免得被当成"已经防住了"）：
//   - 桶在**进程内存**里：多副本部署时每个副本各有一份，实际放行量是副本数的倍数。
//     **当前线上是单副本，因此行为正确；扩容到多副本前必须先解决这一条**（换共享存储，或按实例数
//     折算额度），否则扩容会把限流额度成倍放大——那是隐性的安全退化，不是"性能优化"。
//   - 只限"发多快"与"能开多少新会话"，不限"发给谁"：对同一个人反复发的收敛（拉黑、举报、静默期）
//     需要收件人侧的设置，留给 F3。
//   - 只挂在发信（POST /api/messages/with/:id）上。标记已读本身是幂等的
//     （UPDATE ... WHERE read_at IS NULL），重复调用第二次影响 0 行，不需要限。
const (
	// messageSendBurst 是总体桶容量 = 一次突发能连发的条数，也是每分钟的回满量。
	messageSendBurst = 20
	// messageSendWindow 是总体桶回满一个空桶的时间。
	messageSendWindow = time.Minute
	// messageStrangerBurst 是"陌生人新会话"桶容量 = 每小时最多向几个没有既有会话的人发起会话。
	messageStrangerBurst = 5
	// messageStrangerWindow 是陌生人桶回满一个空桶的时间。
	messageStrangerWindow = time.Hour
	// messageLimiterMaxKeys 是**每个桶**的 key 数量上限：桶按账号建，不清理的话长跑实例会攒
	// "每个访客一个桶"。超过上限时顺手清一遍空闲超过 messageLimiterIdle 的桶——本服务没有后台 ticker
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

// bucketMap 是一组同容量、同窗口的令牌桶。两档限流共用这一份实现，避免两份相似的桶代码漂移。
type bucketMap struct {
	capacity int
	window   time.Duration
	buckets  map[string]*messageBucket
}

func newBucketMap(capacity int, window time.Duration) bucketMap {
	return bucketMap{capacity: capacity, window: window, buckets: map[string]*messageBucket{}}
}

// allow 判定某个 key 这一次能不能过，并给出过不去时"多久之后再有额度"。
// 额度不足时**不扣令牌**（否则连续请求会把等待时间越推越长，Retry-After 失真）。
// 空 key（身份缺失）直接放行：限流是**账号**维度的，而发信路由都在登录门槛之后。
func (m *bucketMap) allow(now time.Time, key string) (bool, time.Duration) {
	if m == nil || key == "" {
		return true, 0
	}
	if len(m.buckets) >= messageLimiterMaxKeys {
		m.evict(now)
	}
	b, ok := m.buckets[key]
	if !ok {
		b = &messageBucket{tokens: float64(m.capacity), last: now}
		m.buckets[key] = b
	}
	refill := float64(m.capacity) / m.window.Seconds() // 每秒回满的令牌数
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(float64(m.capacity), b.tokens+elapsed*refill)
		b.last = now
	}
	if b.tokens < 1 {
		// 再等 (1 - tokens) 个令牌，向上取整到秒：Retry-After 只说秒。
		return false, time.Duration(math.Ceil((1-b.tokens)/refill)) * time.Second
	}
	b.tokens--
	return true, 0
}

// evict 清桶，且只在 key 数量触顶时执行，因此正常流量下不会退化成"每个请求都扫一遍全部桶"。
//
// 两步：先清空闲超过 messageLimiterIdle 的；如果清完仍然触顶（短时间内出现大量互不相同的账号，
// 也就是"有人在拿账号 id 灌水"），按最后一次使用时间丢掉最旧的一半——**硬上限优先于保留每个桶**。
// 代价说清楚：被清掉的桶会重新拿到满额令牌，所以这一步是内存安全阀，不是精确限流；
// 宁可放过一个刚被清掉的账号，也不能让一个未认证过的字节流把进程内存吃光。
func (m *bucketMap) evict(now time.Time) {
	for k, b := range m.buckets {
		if now.Sub(b.last) > messageLimiterIdle {
			delete(m.buckets, k)
		}
	}
	if len(m.buckets) < messageLimiterMaxKeys {
		return
	}
	type entry struct {
		key  string
		last time.Time
	}
	all := make([]entry, 0, len(m.buckets))
	for k, b := range m.buckets {
		all = append(all, entry{k, b.last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].last.Before(all[j].last) })
	for _, e := range all[:len(all)/2] {
		delete(m.buckets, e.key)
	}
}

// messageLimiter 是并发安全的两档令牌桶。now 可注入，用例据此把时间做成确定性的
// （用真实时钟写"一分钟后再试"的用例只能靠 sleep，慢且不稳）。
type messageLimiter struct {
	mu sync.Mutex
	// general 是所有收件人合计的发送频率（第 1 档）。
	general bucketMap
	// stranger 只对"向没有既有会话的人发起新会话"计费（第 2 档）。
	stranger bucketMap
	now      func() time.Time
}

func newMessageLimiter() *messageLimiter {
	return &messageLimiter{
		general:  newBucketMap(messageSendBurst, messageSendWindow),
		stranger: newBucketMap(messageStrangerBurst, messageStrangerWindow),
		now:      time.Now,
	}
}

// allow 判总体发送频率（第 1 档）。
func (l *messageLimiter) allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.general.allow(l.now(), key)
}

// allowStranger 判"陌生人新会话"额度（第 2 档）。调用方只在**确实没有既有会话**时扣它。
func (l *messageLimiter) allowStranger(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.stranger.allow(l.now(), key)
}
