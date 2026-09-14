package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"
)

// FavoriteKinds 是收藏目标类型 → 实体 kind 的映射：键就是固定八种实体骨架本身，
// 落库与读取不做词表映射（与主仓库同一口径）。
var FavoriteKinds = map[string]string{
	"agent":        "agent",
	"collection":   "collection",
	"work":         "work",
	"content_unit": "content_unit",
	"expression":   "expression",
	"release":      "release",
	"medium":       "medium",
	"track":        "track",
}

// Favorite 是一条收藏记录。Entity 由调用方（HTTP 层）经目录服务解析后填入，
// 因此这里只负责本服务的自有数据。
type Favorite struct {
	ID         string    `json:"id"`
	TargetType string    `json:"target_type"`
	TargetID   string    `json:"target_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// KindFor 校验目标类型并返回对应的实体 kind；未知类型返回错误。
func KindFor(targetType string) (string, error) {
	if kind, ok := FavoriteKinds[strings.TrimSpace(targetType)]; ok {
		return kind, nil
	}
	return "", errors.New("invalid_target_type")
}

// ToggleFavorite 切换收藏状态并返回切换后是否已收藏。
// 主键 (user_id,target_type,target_id) 保证并发下不会重复收藏：
// 先删命中即取消，未命中再插入，两步在同一事务内完成。
func (s *Store) ToggleFavorite(ctx context.Context, userID, targetType, targetID string) (bool, error) {
	kind, err := KindFor(targetType)
	if err != nil {
		return false, err
	}
	_ = kind
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "DELETE FROM community.favorites WHERE user_id=$1 AND target_type=$2 AND target_id=$3", userID, targetType, targetID)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO community.favorites(user_id,target_type,target_id) VALUES($1,$2,$3) ON CONFLICT DO NOTHING", userID, targetType, targetID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// FavoriteStatus 返回给定 ID 集合中已收藏的部分（按 target_type 限定）。
func (s *Store) FavoriteStatus(ctx context.Context, userID, targetType string, targetIDs []string) ([]string, error) {
	if _, err := KindFor(targetType); err != nil {
		return nil, err
	}
	if len(targetIDs) == 0 {
		return []string{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT target_id::text FROM community.favorites
		WHERE user_id=$1 AND target_type=$2 AND target_id = ANY($3::uuid[])`, userID, targetType, pq.Array(targetIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListFavorites 按收藏时间倒序返回某用户的收藏，同时返回总数。
// 目标实体的可见性由调用方（HTTP 层）判定：不可见或已删除的条目跳过展示，
// 但不清空记录，也不并入 total——与主仓库口径一致。
func (s *Store) ListFavorites(ctx context.Context, ownerID, targetType string, limit, offset int) ([]Favorite, int, error) {
	if strings.TrimSpace(targetType) != "" {
		if _, err := KindFor(targetType); err != nil {
			return nil, 0, err
		}
	}
	args := []any{ownerID}
	where := "user_id=$1"
	if strings.TrimSpace(targetType) != "" {
		args = append(args, targetType)
		where += fmt.Sprintf(" AND target_type=$%d", len(args))
	}
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM community.favorites WHERE "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	args = append(args, limit, offset)
	// 表主键是 (user_id,target_type,target_id)，没有独立 id 列；
	// 前端需要稳定 id，用三元组拼一个合成 ID。
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT target_type || ':' || target_id::text, target_type, target_id::text, created_at
		FROM community.favorites WHERE %s ORDER BY created_at DESC, target_id LIMIT $%d OFFSET $%d`, where, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Favorite{}
	for rows.Next() {
		var f Favorite
		if err = rows.Scan(&f.ID, &f.TargetType, &f.TargetID, &f.CreatedAt); err != nil {
			return nil, 0, err
		}
		items = append(items, f)
	}
	return items, total, rows.Err()
}

var _ = sql.ErrNoRows
