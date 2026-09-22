// 站内通知的投递端（互动服务 → 目录服务）。
//
// 为什么由互动服务投递：评论/回帖的写路径在互动服务，而收件箱存在目录库
// （目录是事件产生端的主系统与前端 /api 的主入口，见主仓库
// backend/migrations/000003_notifications.up.sql）。这是全系统**唯一**一条跨服务写。
//
// X02 依赖方向整理：收件箱归目录所有（元数据主系统），互动服务依赖目录的投递端点；
// 这只是边界整理，不拆第五个微服务——通知量级与语义用目录收件箱 + 本服务待投递表即够，
// 不引入 Kafka（必须送达的重试/过期/失败查询见 community.notification_outbox）。
//
// 三条纪律：
//  1. 走 internal/upstream（超时分层 + 有界重试 + 熔断），不自己起 http.Client；
//  2. 投递失败**不影响**评论/回帖本身——用户的回复已经落库，通知是旁路。
//     调用方拿到的 err 只用来记日志，绝不改响应状态码（与审计旁路同一哲学）；
//  3. 未配置密钥时**不发也不报错**（ErrNotConfigured 是部署态）：把评论功能
//     押在"通知密钥配没配"上是不对的。
//
// 鉴权是受限服务身份：X-Internal-Token 证明"这是受信任服务生成的"，不转发用户令牌；
// 约束：actor 以产生端确认的事件数据为准（actor_id/actor_name 随业务事务落库），后台与管理员重试不取调用者身份。
package catalog

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/MoeclubM/metafusion-community/internal/upstream"
)

// InternalTokenHeader 与目录侧 catalog.InternalTokenHeader 逐字一致。
const InternalTokenHeader = "X-Internal-Token"

// NotificationCommentReplied 是互动服务唯一能产生的事件类型。类型码是跨服务契约，
// 只增不改；写错会被目录侧以 400 invalid_notification_type 拒掉（不会被静默丢弃）。
const NotificationCommentReplied = "comment.replied"

// Notification 是投递体，字段与目录侧 POST /api/notifications/internal 的请求体逐字对应。
// 约束：同一业务事件的每次投递必须携带相同 EventID（重试/立即/后台路径共用）；
// ActorID/ActorName 是产生端已确认的原始作者快照，目录侧收据表按 (收件人, 事件) 判重时不改作者。
type Notification struct {
	RecipientID string         `json:"recipient_id"`
	Type        string         `json:"type"`
	SubjectType string         `json:"subject_type"`
	SubjectID   string         `json:"subject_id"`
	Payload     map[string]any `json:"payload"`
	// DedupeKey 决定聚合：同一收件人同一键只保留一行（count 累加）。
	// 评论回复按"被回复的落点"聚合（同一主题的多条回复是一行 + count）。
	DedupeKey string `json:"dedupe_key"`
	// EventID 是稳定事件身份：传新建回复/评论的 id，同一事件通知多人时各收件人共用同一个。
	// 约束：同一事件的每次投递必须相同；目录侧收据表按 (收件人, 事件) 去重，领取语义为同一事件只投递一次。
	EventID string `json:"event_id"`
	// ActorID 是原始作者（产生端确认，不取调用者身份）；ActorName 是写入时的展示快照。
	ActorID   string `json:"actor_id"`
	ActorName string `json:"actor_name"`
}

// notifyPolicy 是**尽力而为**的写投递策略：比读路径（3 次尝试 / 7s 预算）更短。
// 通知迟到没有价值——用户已经在看页面了；而它挂在回帖请求的尾巴上，
// 拖长的是真人等待的写请求。所以一次重试、总预算 1.5s 封顶。
func notifyPolicy() upstream.Policy {
	p := upstream.DefaultPolicy("catalog")
	p.Attempts = 2
	p.AttemptTimeout = time.Second
	p.Budget = 1500 * time.Millisecond
	p.BaseBackoff = 50 * time.Millisecond
	p.MaxBackoff = 200 * time.Millisecond
	p.Jitter = 0.5
	p.BreakerThreshold = 5
	p.BreakerOpenFor = 10 * time.Second
	return p
}

// ErrNotConfigured 表示 INTERNAL_API_TOKEN 未配置（部署态，不是故障）。
var ErrNotConfigured = errNotConfigured{}

type errNotConfigured struct{}

func (errNotConfigured) Error() string {
	return "notification delivery is not configured (INTERNAL_API_TOKEN is empty)"
}

// SetInternalToken 由启动路径注入共享密钥。空值 = 关闭通知投递。
func (c *Client) SetInternalToken(token string) { c.internalToken = strings.TrimSpace(token) }

// NotificationsConfigured 让启动日志能说清"通知到底能不能发"。
func (c *Client) NotificationsConfigured() bool {
	return c != nil && c.base != "" && c.internalToken != ""
}

// Notify 投递一条通知（尽力路径：实体短评参与式广播）。err != nil 只表示"这次没送达"，不代表事件没发生。
// 约束：调用者凭据仍随 ctx 转发（目录侧旧契约按令牌取 actor）；必须送达路径改用 NotifyService。
func (c *Client) Notify(ctx context.Context, msg Notification) error {
	if c == nil || c.base == "" || c.internalToken == "" {
		return ErrNotConfigured
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := outboundHeaders(ctx)
	header.Set(InternalTokenHeader, c.internalToken)
	header.Set("Content-Type", "application/json")
	resp, err := c.notify.Do(ctx, upstream.Request{
		Method: http.MethodPost,
		URL:    c.base + "/api/notifications/internal",
		Header: header,
		Body:   raw,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 非 200 一律按"上游不可用"上报（含 401 invalid_internal_token 与 503 internal_api_disabled）：
		// 前者是密钥配错、后者是目录侧没配，都属于部署问题，不该被静默吞掉。
		return upstreamError(c.up.Name(), statusReason(resp.StatusCode), resp.StatusCode, nil)
	}
	return nil
}

// NotifyService 以受限服务身份投递必须送达通知（待投递表路径）。
// 约束：不转发调用者凭据（ctx 中的令牌被忽略），作者只取 msg.ActorID/ActorName（产生端落库快照）；
// 后台 worker 与管理员重试因此不改作者，用户退出后仍可送达；无用户令牌不延长寿命。
func (c *Client) NotifyService(ctx context.Context, msg Notification) error {
	if c == nil || c.base == "" || c.internalToken == "" {
		return ErrNotConfigured
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	header := http.Header{}
	header.Set(InternalTokenHeader, c.internalToken)
	header.Set("Content-Type", "application/json")
	resp, err := c.notify.Do(ctx, upstream.Request{
		Method: http.MethodPost,
		URL:    c.base + "/api/notifications/internal",
		Header: header,
		Body:   raw,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return upstreamError(c.up.Name(), statusReason(resp.StatusCode), resp.StatusCode, nil)
	}
	return nil
}
