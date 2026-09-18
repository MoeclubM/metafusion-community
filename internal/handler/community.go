package handler

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// registerCommunity 挂载短评（评论流/条目评论）。
// 与论坛的区别：评论锚定实体、无独立标题、不进信息流；也不承载任何私有个人数据。
func (h *Handler) registerCommunity(api *gin.RouterGroup) {
	// 站点级评论流：跨实体聚合评论（评论板块），并带上被评论条目的题名。
	// 条目元信息经目录接口批量获取，不直接 JOIN 目录表（解耦边界）。
	// 支持 sort=recent（默认，最新在前）/ oldest；entity_id 限定单个条目；q 匹配正文或条目标题。
	api.GET("/community/feed", h.guard(false), func(c *gin.Context) {
		limit, _ := pagingLimitOffset(c, 50)
		args := []any{commentBoard}
		where := []string{"t.board_code = $1"}
		// 评论板块被停用时，评论流同样不再公开（持 community.board.manage 的运营仍可见，
		// 判据与板块列表共用，见 forum.go 的 seesDisabledBoards）。
		if !h.seesDisabledBoards(c) {
			where = append(where, enabledBoardGuard("t"))
		}
		// entity_id 必须是合法 UUID，否则直接判为空结果，而不是把非法字面量送进查询。
		if raw := strings.TrimSpace(c.Query("entity_id")); raw != "" {
			if _, err := uuid.Parse(raw); err != nil {
				c.JSON(200, gin.H{"items": []any{}})
				return
			}
			args = append(args, raw)
			where = append(where, fmt.Sprintf("t.entity_id = $%d", len(args)))
		}
		// q 需同时匹配正文与条目标题，而标题不属本 schema、无法在 SQL 内完成；
		// 因此带 q 时取一个有界窗口后在 Go 侧过滤，无 q 时把 LIMIT 下推。
		q := strings.TrimSpace(c.Query("q"))
		scan := limit
		if q != "" {
			scan = feedScanCap
		}
		order := "DESC"
		if c.Query("sort") == "oldest" {
			order = "ASC"
		}
		args = append(args, scan)
		rows, err := h.db.QueryContext(c.Request.Context(), `
			SELECT t.id::text, t.entity_id::text, t.author_id::text,
			       COALESCE(NULLIF(t.author_name, ''), 'Anonymous'), t.body, t.created_at
			FROM community.topics t
			WHERE `+strings.Join(where, " AND ")+`
			ORDER BY t.created_at `+order+`, t.id
			LIMIT $`+strconv.Itoa(len(args)), args...)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		type feedRow struct {
			id, entityID, authorID, authorName, body string
			at                                       time.Time
		}
		raw := []feedRow{}
		ids := []string{}
		seen := map[string]bool{}
		for rows.Next() {
			var r feedRow
			if rows.Scan(&r.id, &r.entityID, &r.authorID, &r.authorName, &r.body, &r.at) != nil {
				rows.Close()
				fail(c, 500, "module_error")
				return
			}
			raw = append(raw, r)
			if r.entityID != "" && !seen[r.entityID] {
				seen[r.entityID] = true
				ids = append(ids, r.entityID)
			}
		}
		rows.Close()
		// 一次性批量取元信息与可见性；不可见或已删除的条目，其评论不再展示。
		meta := h.catalog.LookupMany(c.Request.Context(), ids)
		needle := strings.ToLower(q)
		items := []map[string]any{}
		for _, r := range raw {
			title, kind := "", ""
			if r.entityID != "" {
				e, ok := meta[r.entityID]
				if !ok {
					continue
				}
				title, kind = e.Title, e.Kind
			}
			if needle != "" &&
				!strings.Contains(strings.ToLower(r.body), needle) &&
				!strings.Contains(strings.ToLower(title), needle) {
				continue
			}
			items = append(items, map[string]any{
				"id": r.id, "entity_id": r.entityID, "author_id": r.authorID,
				"author_name": r.authorName, "body": r.body, "created_at": r.at,
				"entity_title": title, "entity_kind": kind,
			})
			if len(items) >= limit {
				break
			}
		}
		c.JSON(200, gin.H{"items": items})
	})

	// 实体评论：语义上是"文章下的评论"，与论坛主题（独立成文的帖子）区分开。
	// 存储复用 topics 的评论板块：锚定实体、无独立标题、不进信息流。
	// URL 契约沿用 /community/entities/:id/posts，前端无需改动。
	api.GET("/community/entities/:id/posts", h.guard(false), func(c *gin.Context) {
		id := c.Param("id")
		if !h.entity(c, id) {
			return
		}
		// 评论板块停用时，条目下的短评同样不再公开（谓词与其余读路径同一份，见 enabledBoardGuard）。
		query := `SELECT id::text,author_id::text,COALESCE(NULLIF(author_name, ''), 'Anonymous'),body,created_at FROM community.topics t WHERE t.board_code=$1 AND t.entity_id=$2`
		if !h.seesDisabledBoards(c) {
			query += " AND " + enabledBoardGuard("t")
		}
		rows, err := h.db.QueryContext(c.Request.Context(), query+" ORDER BY created_at DESC LIMIT 100", commentBoard, id)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var id, author, authorName, body string
			var at time.Time
			if rows.Scan(&id, &author, &authorName, &body, &at) != nil {
				fail(c, 500, "module_error")
				return
			}
			items = append(items, map[string]any{"id": id, "author_id": author, "author_name": authorName, "body": body, "created_at": at})
		}
		c.JSON(200, gin.H{"items": items})
	})

	// 短评也是发帖：与发主题/回帖共用 community.post.create，避免"能发主题但不能短评"的缺口。
	api.POST("/community/entities/:id/posts", h.require(auth.PermissionPostCreate), func(c *gin.Context) {
		id := c.Param("id")
		if !h.entity(c, id) {
			return
		}
		var in struct {
			Body string `json:"body"`
		}
		if !body(c, &in) || len(strings.TrimSpace(in.Body)) == 0 || len(in.Body) > 20000 {
			return
		}
		p := h.principal(c)
		pid := uuid.NewString()
		_, err := h.db.ExecContext(c.Request.Context(), `INSERT INTO community.topics(id,board_code,author_id,author_name,title,body,entity_id) VALUES($1,$2,$3,$4,'',$5,$6)`, pid, commentBoard, p.ID, authorName(p), in.Body, id)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{
			"ok": true,
			"item": map[string]any{
				"id":          pid,
				"author_id":   p.ID,
				"author_name": authorName(p),
				"body":        in.Body,
				"created_at":  time.Now().Format(time.RFC3339),
			},
		})
	})

	// 评论删除：只作用于评论板块，避免仅凭 id 误删论坛主题。
	api.DELETE("/community/posts/:id", h.guard(true), func(c *gin.Context) {
		id := c.Param("id")
		if _, err := uuid.Parse(id); err != nil {
			fail(c, 404, "not_found")
			return
		}
		p := h.principal(c)
		query := "DELETE FROM community.topics WHERE id=$1 AND board_code=$2 AND author_id=$3"
		args := []any{id, commentBoard, p.ID}
		// 短评与帖子同属"内容治理"：删他人的短评用 community.post.moderate（原判据是 admin 角色）。
		if p.Can(auth.PermissionPostModerate) {
			query = "DELETE FROM community.topics WHERE id=$1 AND board_code=$2"
			args = args[:2]
		}
		res, err := h.db.ExecContext(c.Request.Context(), query, args...)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		// 没删到行（不存在、不是评论板块、或不是本人且无治理码）必须回 404：
		// 恒回 {"ok":true} 会让调用方以为越权删除成功了，与主题/回复删除的口径也不一致。
		if n, _ := res.RowsAffected(); n == 0 {
			fail(c, 404, "not_found")
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})

	// 单条评论：为评论提供稳定的直达链接（permalink）。同样限定评论板块。
	api.GET("/community/posts/:id", h.guard(false), func(c *gin.Context) {
		id := c.Param("id")
		if _, err := uuid.Parse(id); err != nil {
			fail(c, 404, "not_found")
			return
		}
		var postID, entityID, author, authorName, body string
		var at time.Time
		// 评论板块停用时直达链接按"不存在"处理（404 与其它读路径同一口径）。
		where := "WHERE t.id = $1 AND t.board_code = $2"
		if !h.seesDisabledBoards(c) {
			where += " AND " + enabledBoardGuard("t")
		}
		err := h.db.QueryRowContext(c.Request.Context(), `
			SELECT t.id::text, COALESCE(t.entity_id::text,''), t.author_id::text,
			       COALESCE(NULLIF(t.author_name, ''), 'Anonymous'), t.body, t.created_at
			FROM community.topics t `+where, id, commentBoard).
			Scan(&postID, &entityID, &author, &authorName, &body, &at)
		if err == sql.ErrNoRows {
			fail(c, 404, "not_found")
			return
		}
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		// 关联实体的可见性：匿名只应看到 published 条目的评论。
		title, kind := "", ""
		if entityID != "" {
			meta, ok := h.catalog.Lookup(c.Request.Context(), entityID)
			if !ok {
				fail(c, 404, "not_found")
				return
			}
			title, kind = meta.Title, meta.Kind
		}
		c.JSON(200, gin.H{
			"id": postID, "entity_id": entityID, "author_id": author,
			"author_name": authorName, "body": body, "created_at": at,
			"entity_title": title, "entity_kind": kind,
		})
	})

	// 关联合集：经目录接口取关系邻居，不直接 JOIN 目录表（解耦边界）。
	api.GET("/community/entities/:id/collections", h.guard(false), func(c *gin.Context) {
		id := c.Param("id")
		if !h.entity(c, id) {
			return
		}
		cols := h.catalog.Related(c.Request.Context(), id, []string{"collection"})
		if cols == nil {
			c.JSON(200, gin.H{"items": []any{}})
			return
		}
		if len(cols) > 20 {
			cols = cols[:20]
		}
		items := []map[string]any{}
		for _, col := range cols {
			if col.Status != "published" {
				continue
			}
			items = append(items, map[string]any{"id": col.ID, "title": col.Title})
		}
		c.JSON(200, gin.H{"items": items})
	})

}
