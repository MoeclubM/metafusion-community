// 站内通知的投递端（互动服务 → 目录服务）。
//
// 为什么由互动服务投递：评论/回帖的写路径在互动服务，而收件箱存在目录库
// （目录是事件产生端的主系统与前端 /api 的主入口，见主仓库
// backend/migrations/000003_notifications.up.sql）。这是全系统**唯一**一条跨服务写。
//
// 三条纪律：
//  1. 走 internal/upstream（超时分层 + 有界重试 + 熔断），不自己起 http.Client；
//  2. 投递失败**不影响**评论/回帖本身——用户的回复已经落库，通知是旁路。
//     调用方拿到的 err 只用来记日志，绝不改响应状态码（与审计旁路同一哲学）；
//  3. 未配置密钥时**不发也不报错**（ErrNotConfigured 是部署态）：把评论功能
//     押在"通知密钥配没配"上是不对的。
//
// 鉴权是双凭据：X-Internal-Token 证明"这是受信任服务生成的"，Authorization 里的
// 终端用户令牌提供 actor（目录侧据此写审计行与"谁回复了你"）。
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
type Notification struct {
	RecipientID string         `json:"recipient_id"`
	Type        string         `json:"type"`
	SubjectType string         `json:"subject_type"`
	SubjectID   string         `json:"subject_id"`
	Payload     map[string]any `json:"payload"`
	// DedupeKey 决定聚合：同一收件人同一键只保留一行（count 累加）。
	// 评论回复按"被回复的落点"聚合（同一主题的多条回复是一行 + count）。
	DedupeKey string `json:"dedupe_key"`
	// EventID 让上游重试幂等（目录侧按它判"同一事件"）：传新建回复/评论的 id。
	EventID string `json:"event_id"`
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

// Notify 投递一条通知。err != nil 只表示"这次没送达"，不代表事件没发生。
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
