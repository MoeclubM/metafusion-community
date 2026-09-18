package store

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/lib/pq"
)

// 申诉的读写：表结构、状态口径与取舍见 migrations/000008_reports.up.sql。
// 类型与常量（ReportAppeal / AppealPending / ErrDuplicateAppeal 等）与举报同置一处（reports.go），
// 本文件只放申诉的行为，避免举报与申诉的读写堆在同一个文件里。

// CreateReportAppeal 写入一条申诉并落一行 appealed 事件。
// "被处置方可提交一次"由唯一索引 report_appeals_once 兜底（并发下重复提交会撞索引 → ErrDuplicateAppeal）；
// 调用前的状态与身份校验在 handler 里，这里只保证不写出第二条。
func (s *Store) CreateReportAppeal(ctx context.Context, a ReportAppeal, at time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO community.report_appeals(
			id,report_id,appellant_id,appellant_name,body,status,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, a.ID, a.ReportID, a.AppellantID, a.AppellantName, a.Body, AppealPending, at)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrDuplicateAppeal
			}
			return err
		}
		return appendReportEvent(ctx, tx, ReportEvent{
			ReportID: a.ReportID, At: at, Kind: "appealed",
			ActorID: a.AppellantID, ActorName: a.AppellantName, ActorRole: "appellant",
			Meta: map[string]any{"appeal_id": a.ID},
		})
	})
}

// AppealByID 读一条申诉（含所属报告的关键字段，处理人不必再查一次报告）。
func (s *Store) AppealByID(ctx context.Context, id string) (ReportAppeal, error) {
	row := s.db.QueryRowContext(ctx, appealSelect+" WHERE a.id=$1", id)
	a, err := scanAppeal(row)
	if err == sql.ErrNoRows {
		return ReportAppeal{}, ErrAppealNotFound
	}
	if err != nil {
		return ReportAppeal{}, err
	}
	return a, nil
}

// ListReportAppeals 列某条报告下的申诉（当前口径是 0 或 1 条，仍按列表返回：
// 结构上允许将来放开"补充材料"，接口形状不必再改）。
func (s *Store) ListReportAppeals(ctx context.Context, reportID string) ([]ReportAppeal, error) {
	rows, err := s.db.QueryContext(ctx, appealSelect+" WHERE a.report_id=$1 ORDER BY a.created_at DESC, a.id DESC", reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAppeals(rows)
}

// ListAppealsQueue 是申诉队列：accepted / rejected 是终态，队列默认只看 pending（由 filter 决定）。
func (s *Store) ListAppealsQueue(ctx context.Context, statuses []string, limit, offset int) ([]ReportAppeal, int, error) {
	where := ""
	args := []any{}
	if len(statuses) > 0 {
		args = append(args, pq.Array(statuses))
		where = " WHERE a.status = ANY($1)"
	}
	var total int
	if err := s.db.QueryRowContext(ctx, appealFrom+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.QueryContext(ctx, appealSelect+where+
		" ORDER BY a.created_at DESC, a.id DESC LIMIT $"+strconv.Itoa(len(pageArgs)-1)+" OFFSET $"+strconv.Itoa(len(pageArgs)), pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out, err := scanAppeals(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

const appealFrom = "SELECT count(*) FROM community.report_appeals a JOIN community.reports r ON r.id = a.report_id"

// appealSelect 把所属报告的关键字段一起带出来：申诉队列要能直接看出"申诉的是哪条处置"。
const appealSelect = `SELECT a.id::text,a.report_id::text,a.appellant_id::text,a.appellant_name,a.body,a.status,
	COALESCE(a.reviewer_id::text,''),a.reviewer_name,a.review_note,a.reviewed_at,a.created_at,
	r.target_type,r.target_id,r.reason,r.status,COALESCE(r.target_author_id::text,'')
	FROM community.report_appeals a JOIN community.reports r ON r.id = a.report_id`

func scanAppeal(scanner interface{ Scan(...any) error }) (ReportAppeal, error) {
	var a ReportAppeal
	var reviewedAt sql.NullTime
	if err := scanner.Scan(&a.ID, &a.ReportID, &a.AppellantID, &a.AppellantName, &a.Body, &a.Status,
		&a.ReviewerID, &a.ReviewerName, &a.ReviewNote, &reviewedAt, &a.CreatedAt,
		&a.TargetType, &a.TargetID, &a.Reason, &a.ReportStatus, &a.TargetAuthorID); err != nil {
		return ReportAppeal{}, err
	}
	if reviewedAt.Valid {
		at := reviewedAt.Time
		a.ReviewedAt = &at
	}
	return a, nil
}

func scanAppeals(rows *sql.Rows) ([]ReportAppeal, error) {
	out := []ReportAppeal{}
	for rows.Next() {
		a, err := scanAppeal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ReviewAppeal 处理一条申诉：只允许 pending → accepted / rejected（终态不再改，
// 回改会让"当时怎么答复的"无从追溯），并给所属报告追加一行 appeal_reviewed 事件。
func (s *Store) ReviewAppeal(ctx context.Context, id, status, note, reviewerID, reviewerName string, at time.Time) (ReportAppeal, error) {
	var out ReportAppeal
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var current, reportID string
		err := tx.QueryRowContext(ctx, "SELECT status, report_id::text FROM community.report_appeals WHERE id=$1 FOR UPDATE", id).Scan(&current, &reportID)
		if err == sql.ErrNoRows {
			return ErrAppealNotFound
		}
		if err != nil {
			return err
		}
		if current != AppealPending {
			return ErrAppealState
		}
		row := tx.QueryRowContext(ctx, `UPDATE community.report_appeals
			SET status=$2, review_note=$3, reviewer_id=NULLIF($4,'')::uuid, reviewer_name=$5, reviewed_at=$6
			WHERE id=$1 RETURNING id::text,report_id::text,appellant_id::text,appellant_name,body,status,
			COALESCE(reviewer_id::text,''),reviewer_name,review_note,reviewed_at,created_at`,
			id, status, note, reviewerID, reviewerName, at)
		var a ReportAppeal
		var reviewedAt sql.NullTime
		if err = row.Scan(&a.ID, &a.ReportID, &a.AppellantID, &a.AppellantName, &a.Body, &a.Status,
			&a.ReviewerID, &a.ReviewerName, &a.ReviewNote, &reviewedAt, &a.CreatedAt); err != nil {
			return err
		}
		if reviewedAt.Valid {
			when := reviewedAt.Time
			a.ReviewedAt = &when
		}
		out = a
		return appendReportEvent(ctx, tx, ReportEvent{
			ReportID: reportID, At: at, Kind: "appeal_reviewed",
			ActorID: reviewerID, ActorName: reviewerName, ActorRole: "reviewer",
			Note: note, Meta: map[string]any{"appeal_id": id, "appeal_status": status},
		})
	})
	if err != nil {
		return ReportAppeal{}, err
	}
	return out, nil
}

// AppealsForReports 批量取一页报告各自的申诉（key = report_id）。
//
// 列表页要为每一条报告标出"有没有申诉、什么状态"：逐条查会变成 N+1 次往返，
// 一次 ANY($1) 取回整页才是列表该有的形状。没有申诉的报告不出现在返回的 map 里。
func (s *Store) AppealsForReports(ctx context.Context, reportIDs []string) (map[string][]ReportAppeal, error) {
	out := map[string][]ReportAppeal{}
	if len(reportIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.QueryContext(ctx, appealSelect+" WHERE a.report_id = ANY($1::uuid[]) ORDER BY a.created_at, a.id", pq.Array(reportIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items, err := scanAppeals(rows)
	if err != nil {
		return nil, err
	}
	for _, a := range items {
		out[a.ReportID] = append(out[a.ReportID], a)
	}
	return out, nil
}
