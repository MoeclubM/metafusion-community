package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

// 必须送达通知的待投递表（X02，见 migrations/000011、000012）。
//
// 分工（S4 接通后）：
//   - 必须送达：论坛定向回帖 comment.replied（被回复楼层作者 / 主题作者，目录侧支持该类型）——
//     在回帖原事务中入队，worker 领取重试直到送达/过期/耗尽。
//   - 尽力投递：实体短评的参与式广播（最多 10 人，迟到的提醒没有价值）仍经 deliverAll
//     同步投递、失败只记日志，不进本表（见 handler/notifications.go）。
//   - 举报处置结论（受理/驳回/处置/申诉处理）不进本表：目录收件箱暂无 report.* 类型，
//     跨服务投递会被目录侧以 invalid_notification_type 拒收；结论经报告行 + report_events
//     同事务落库，经举报/申诉队列与审计留痕（见 store/reports.go 的 withTx），目录加类型后再接。
//
// 并发重试的重复投递由目录侧收据表按 (收件人, 事件) 去重兜底（见 catalog/notify.go 的 EventID）。
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
// 约束：EventID 是同一业务事件在重试/立即/后台路径间的稳定身份，每次投递必须原样携带；
// ActorID/ActorName 是产生端已确认的原始作者快照，随业务事务落库，不存用户令牌。
type OutboxItem struct {
	ID          string         `json:"id"`
	RecipientID string         `json:"recipient_id"`
	Type        string         `json:"type"`
	SubjectType string         `json:"subject_type"`
	SubjectID   string         `json:"subject_id"`
	DedupeKey   string         `json:"dedupe_key"`
	EventID     string         `json:"event_id"`
	ActorID     string         `json:"actor_id"`
	ActorName   string         `json:"actor_name"`
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

// EnqueueNotification 幂等入队：同一 (收件人, 事件) 只保留一行（重复调用返回 inserted=false）。
// 同一业务事件通知多人时各收件人各存一行（000012 复合唯一），目录侧按收件人分别聚合。
// expiresAt 为零值时按 now+OutboxTTL 落库——调用方不传即默认存活 7 天。
func (s *Store) EnqueueNotification(ctx context.Context, item OutboxItem, now time.Time) (bool, error) {
	return enqueueOutbox(ctx, s.db, item, now)
}

// outboxExec 是入队语句的执行面：*sql.DB 与 *sql.Tx 都满足，业务写事务内入队走它
// （回帖行 + 待投递行同事务提交，崩溃不丢“已落库未入队”的半截状态）。
type outboxExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// EnqueueNotificationTx 在调用方已开启的业务事务内幂等入队（语义同 EnqueueNotification）。
func (s *Store) EnqueueNotificationTx(ctx context.Context, tx *sql.Tx, item OutboxItem, now time.Time) (bool, error) {
	return enqueueOutbox(ctx, tx, item, now)
}

func enqueueOutbox(ctx context.Context, db outboxExec, item OutboxItem, now time.Time) (bool, error) {
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
	res, err := db.ExecContext(ctx, `INSERT INTO community.notification_outbox(
			id,recipient_id,type,subject_type,subject_id,dedupe_key,event_id,actor_id,actor_name,payload,
			status,attempts,next_retry_at,expires_at,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,'')::uuid,$9,$10::jsonb,'pending',0,$11,$12,$11,$11)
			ON CONFLICT (recipient_id, event_id) DO NOTHING`,
		item.ID, item.RecipientID, item.Type, item.SubjectType, item.SubjectID,
		item.DedupeKey, item.EventID, item.ActorID, item.ActorName, string(raw), now, item.ExpiresAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const outboxColumns = `id::text,recipient_id::text,type,subject_type,subject_id,dedupe_key,event_id,COALESCE(actor_id::text,''),actor_name,payload,status,attempts,next_retry_at,expires_at,last_error,created_at,updated_at`

func scanOutbox(scanner interface{ Scan(...any) error }) (OutboxItem, error) {
	var it OutboxItem
	var raw []byte
	if err := scanner.Scan(&it.ID, &it.RecipientID, &it.Type, &it.SubjectType, &it.SubjectID,
		&it.DedupeKey, &it.EventID, &it.ActorID, &it.ActorName, &raw, &it.Status, &it.Attempts,
		&it.NextRetryAt, &it.ExpiresAt, &it.LastError, &it.CreatedAt, &it.UpdatedAt); err != nil {
		return OutboxItem{}, err
	}
	it.Payload = map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &it.Payload)
	}
	return it, nil
}

// ClaimDueOutbox 原子领取已到重试时间的待投递行（多 worker 安全）：
// SELECT ... FOR UPDATE SKIP LOCKED 抢行，抢到即把 next_retry_at 推后一个租期——
// 租期是“崩溃自愈”：占住行后崩溃的 worker 不用显式释放，租期一过该行自然重新到期。
// 租期应大于单次触发的最大投递耗时（50 条 × 每条 5s ≈ 250s，默认 5min 覆盖）。
func (s *Store) ClaimDueOutbox(ctx context.Context, now time.Time, limit int, lease time.Duration) ([]OutboxItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM community.notification_outbox
		WHERE status='pending' AND next_retry_at <= $1 AND expires_at > $1
		ORDER BY next_retry_at, id LIMIT $2 FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return []OutboxItem{}, nil
	}
	out := []OutboxItem{}
	claimed, err := tx.QueryContext(ctx, `UPDATE community.notification_outbox
		SET next_retry_at=$1, updated_at=$1 WHERE id = ANY($2::uuid[]) RETURNING `+outboxColumns, now.Add(lease), pq.Array(ids))
	if err != nil {
		return nil, err
	}
	for claimed.Next() {
		it, err := scanOutbox(claimed)
		if err != nil {
			claimed.Close()
			return nil, err
		}
		out = append(out, it)
	}
	claimed.Close()
	if err := claimed.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// ClaimOutboxByIDs 按 ID 原子领取指定行（立即投递与后台投递共用领取约定）：
// 约束：仅 pending 且未被租约占住（next_retry_at<=now）的行可被领走，领走即推后租期；
// 同一事件只投递一次，未领到的一方必须跳过投递（另一方会投递）。
func (s *Store) ClaimOutboxByIDs(ctx context.Context, ids []string, now time.Time, lease time.Duration) ([]OutboxItem, error) {
	if len(ids) == 0 {
		return []OutboxItem{}, nil
	}
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM community.notification_outbox
		WHERE id = ANY($2::uuid[]) AND status='pending' AND next_retry_at <= $1 AND expires_at > $1
		FOR UPDATE SKIP LOCKED`, now, pq.Array(ids))
	if err != nil {
		return nil, err
	}
	claimedIDs := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		claimedIDs = append(claimedIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(claimedIDs) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return []OutboxItem{}, nil
	}
	out := []OutboxItem{}
	claimed, err := tx.QueryContext(ctx, `UPDATE community.notification_outbox
		SET next_retry_at=$1, updated_at=$1 WHERE id = ANY($2::uuid[]) RETURNING `+outboxColumns, now.Add(lease), pq.Array(claimedIDs))
	if err != nil {
		return nil, err
	}
	for claimed.Next() {
		it, err := scanOutbox(claimed)
		if err != nil {
			claimed.Close()
			return nil, err
		}
		out = append(out, it)
	}
	claimed.Close()
	if err := claimed.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// ListDueOutbox 取出已到重试时间的待投递行（pending + next_retry_at<=now + 未过期），
// 按重试时间排序。worker 与管理端手动触发走 ClaimDueOutbox（行锁 + 租约）；
// 本函数只留给排障时只读查看，不用于领取——无锁并发领取会多投（目录侧可去重，但浪费配额）。
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

// MarkOutboxSent 标记投递成功（终态）：只从 pending 推进，
// 已是 sent/failed/expired 的行不再覆盖——并发领取导致重复投递时，
// 先成功的那台定终态，后到的只记“已有人定过”，不把成功改成失败。
func (s *Store) MarkOutboxSent(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE community.notification_outbox
		SET status='sent', updated_at=$2 WHERE id=$1 AND status='pending'`, id, now)
	return err
}

// MarkOutboxAttempt 记录一次失败并安排下次重试：attempts+1，next_retry_at 按退避后移；
// 次数耗尽（>=OutboxMaxAttempts）则置 failed（等人工查询与重放，不再自动重试）。
// 整段在行锁事务内“先判状态再写”：行已不在 pending（被别的 worker 置 sent/failed/expired）
// 则直接返回，不把别人的终态覆盖成自己的失败。
func (s *Store) MarkOutboxAttempt(ctx context.Context, id, errText string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current string
	var attempts int
	if err := tx.QueryRowContext(ctx,
		`SELECT status, attempts FROM community.notification_outbox WHERE id=$1 FOR UPDATE`, id).Scan(&current, &attempts); err != nil {
		return err
	}
	if current != OutboxPending {
		return tx.Commit()
	}
	attempts++
	if errText == "" {
		errText = "delivery failed"
	}
	if len(errText) > 1000 {
		errText = errText[:1000]
	}
	if attempts >= OutboxMaxAttempts {
		_, err := tx.ExecContext(ctx, `UPDATE community.notification_outbox
			SET status='failed', attempts=$2, last_error=$3, updated_at=$4 WHERE id=$1 AND status='pending'`,
			id, attempts, errText, now)
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	_, err = tx.ExecContext(ctx, `UPDATE community.notification_outbox
		SET attempts=$2, last_error=$3, next_retry_at=$4, updated_at=$4 WHERE id=$1 AND status='pending'`,
		id, attempts, errText, now.Add(OutboxRetryDelay(attempts)))
	if err != nil {
		return err
	}
	return tx.Commit()
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
