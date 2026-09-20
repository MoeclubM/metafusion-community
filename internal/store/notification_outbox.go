package store

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// 必须送达通知的待投递表（X02，见 migrations/000011）。
//
// 与尽力投递（comment.replied 经 deliverAll 同步投递、失败只记日志）的分工：
// 本表只承载审核/安全/处置类必须送达的通知——先落库（幂等），再由重试器投递到目录收件箱。
// 并发重试的重复投递由目录侧按 event_id 去重兜底（见 catalog/notify.go 的 EventID）。
// 不引入 Kafka：量级与语义用本库表即够，失败查询与手动重试走管理端点。
const (
	OutboxPending = "pending"
	OutboxSent    = "sent"
	OutboxFailed  = "failed"
	OutboxExpired = "expired"

	// OutboxMaxAttempts 是投递尝试上限：耗尽后置 failed（不再自动重试，等人工查询与重放）。
	OutboxMaxAttempts = 8
	// OutboxTTL 是待投递行的存活期：到期后由 ExpireOutbox 置 expired，不再重试。
	OutboxTTL = 7 * 24 * time.Hour
)

// OutboxItem 是一条待投递记录。Payload 只做透传（目录侧按通知类型解析），
// 本服务不解释它——解释权在收件箱（目录）那边。
type OutboxItem struct {
	ID          string         `json:"id"`
	RecipientID string         `json:"recipient_id"`
	Type        string         `json:"type"`
	SubjectType string         `json:"subject_type"`
	SubjectID   string         `json:"subject_id"`
	DedupeKey   string         `json:"dedupe_key"`
	EventID     string         `json:"event_id"`
	Payload     map[string]any `json:"payload"`
	Status      string         `json:"status"`
	Attempts    int            `json:"attempts"`
	NextRetryAt time.Time      `json:"next_retry_at"`
	ExpiresAt   time.Time      `json:"expires_at"`
	LastError   string         `json:"last_error,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// OutboxRetryDelay 是第 attempts 次失败后的退避：1m/5m/15m/1h/6h/24h/24h…（attempts 从 1 计）。
// 纯函数（不碰库）：退避改动只改这里，重试器与用例共用同一份。
func OutboxRetryDelay(attempts int) time.Duration {
	switch {
	case attempts <= 1:
		return time.Minute
	case attempts == 2:
		return 5 * time.Minute
	case attempts == 3:
		return 15 * time.Minute
	case attempts == 4:
		return time.Hour
	case attempts == 5:
		return 6 * time.Hour
	default:
		return 24 * time.Hour
	}
}

// EnqueueNotification 幂等入队：同一 event_id 只保留一行（重复调用返回 inserted=false）。
// expiresAt 为零值时按 now+OutboxTTL 落库——调用方不传即默认存活 7 天。
func (s *Store) EnqueueNotification(ctx context.Context, item OutboxItem, now time.Time) (bool, error) {
	if item.ID == "" {
		item.ID = uuid.NewString()
	}
	if item.ExpiresAt.IsZero() {
		item.ExpiresAt = now.Add(OutboxTTL)
	}
	raw, err := json.Marshal(item.Payload)
	if err != nil {
		return false, err
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO community.notification_outbox(
			id,recipient_id,type,subject_type,subject_id,dedupe_key,event_id,payload,
			status,attempts,next_retry_at,expires_at,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8::jsonb,'pending',0,$9,$10,$9,$9)
			ON CONFLICT (event_id) DO NOTHING`,
		item.ID, item.RecipientID, item.Type, item.SubjectType, item.SubjectID,
		item.DedupeKey, item.EventID, string(raw), now, item.ExpiresAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const outboxColumns = `id::text,recipient_id::text,type,subject_type,subject_id,dedupe_key,event_id,payload,status,attempts,next_retry_at,expires_at,last_error,created_at,updated_at`

func scanOutbox(scanner interface{ Scan(...any) error }) (OutboxItem, error) {
	var it OutboxItem
	var raw []byte
	if err := scanner.Scan(&it.ID, &it.RecipientID, &it.Type, &it.SubjectType, &it.SubjectID,
		&it.DedupeKey, &it.EventID, &raw, &it.Status, &it.Attempts,
		&it.NextRetryAt, &it.ExpiresAt, &it.LastError, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return OutboxItem{}, err
	}
	it.Payload = map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &it.Payload)
	}
	return it, nil
}

// ListDueOutbox 取出已到重试时间的待投递行（pending + next_retry_at<=now + 未过期），
// 按重试时间排序。并发重试的重复投递由目录侧按 event_id 去重兜底，这里不加行锁——
// 单实例定时触发 + 管理端手动触发不会长期并发，即使并发一次也只是多投一次（可去重）。
func (s *Store) ListDueOutbox(ctx context.Context, now time.Time, limit int) ([]OutboxItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+outboxColumns+` FROM community.notification_outbox
		WHERE status='pending' AND next_retry_at <= $1 AND expires_at > $1
		ORDER BY next_retry_at, id LIMIT $2`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxItem{}
	for rows.Next() {
		it, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkOutboxSent 标记投递成功（终态）。
func (s *Store) MarkOutboxSent(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE community.notification_outbox
		SET status='sent', updated_at=$2 WHERE id=$1`, id, now)
	return err
}

// MarkOutboxAttempt 记录一次失败并安排下次重试：attempts+1，next_retry_at 按退避后移；
// 次数耗尽（>=OutboxMaxAttempts）则置 failed（等人工查询与重放，不再自动重试）。
func (s *Store) MarkOutboxAttempt(ctx context.Context, id, errText string, now time.Time) error {
	var attempts int
	if err := s.db.QueryRowContext(ctx,
		`SELECT attempts FROM community.notification_outbox WHERE id=$1`, id).Scan(&attempts); err != nil {
		return err
	}
	attempts++
	if errText == "" {
		errText = "delivery failed"
	}
	if len(errText) > 1000 {
		errText = errText[:1000]
	}
	if attempts >= OutboxMaxAttempts {
		_, err := s.db.ExecContext(ctx, `UPDATE community.notification_outbox
			SET status='failed', attempts=$2, last_error=$3, updated_at=$4 WHERE id=$1`,
			id, attempts, errText, now)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE community.notification_outbox
		SET attempts=$2, last_error=$3, next_retry_at=$4, updated_at=$4 WHERE id=$1 AND status='pending'`,
		id, attempts, errText, now.Add(OutboxRetryDelay(attempts)))
	return err
}

// ExpireOutbox 把到期的待投递行置 expired，返回影响行数。重试器每次触发前先调它，
// 管理端查询可按 status=expired 看到“为什么没送达”。
func (s *Store) ExpireOutbox(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE community.notification_outbox
		SET status='expired', updated_at=$1
		WHERE status='pending' AND expires_at <= $1`, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListOutbox 按状态列出待投递行（管理端失败查询）：status 为空即全部；排序创建时间倒序。
func (s *Store) ListOutbox(ctx context.Context, status string, limit, offset int) ([]OutboxItem, int, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	where := ""
	args := []any{}
	if status != "" {
		args = append(args, status)
		where = " WHERE status=$1"
	}
	var total int
	if err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM community.notification_outbox"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pageArgs := append(append([]any{}, args...), limit, offset)
	q := "SELECT " + outboxColumns + " FROM community.notification_outbox" + where +
		" ORDER BY created_at DESC, id DESC LIMIT $" + itoa(len(pageArgs)-1) + " OFFSET $" + itoa(len(pageArgs))
	rows, err := s.db.QueryContext(ctx, q, pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []OutboxItem{}
	for rows.Next() {
		it, err := scanOutbox(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, it)
	}
	return out, total, rows.Err()
}

func itoa(n int) string {
	if n < 0 {
		n = 0
	}
	return strconv.Itoa(n)
}
