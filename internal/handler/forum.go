package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// 论坛是本站自建的独立讨论系统：板块（board）→ 主题（topic）→ 回复（post）。
// 话题（主题）与"实体评论"共用 forum_topics 存储，靠板块区分语义：
// 评论锚定实体、无独立标题、不进信息流；主题可独立成文、有标题、进信息流。
//
// 数据只落本服务的 community schema，不触碰目录库实体表。

// boardLocales 是板块名称与描述必须齐备的语种（命名四语铁律），与目录侧的
// requiredNameLocales 同一口径，只是显式写出 ja-JP 而不接受 ja 别名（community 只有这一个写入口）。
var boardLocales = []string{"zh-CN", "zh-TW", "ja-JP", "en-US"}

func newLocalizedText(zhCN, zhTW, jaJP, enUS string) map[string]string {
	return map[string]string{"zh-CN": zhCN, "zh-TW": zhTW, "ja-JP": jaJP, "en-US": enUS}
}

// defaultBoards 是首次运行播种的板块：名称与描述是**多语言 map**（四语齐备）。
//
// 接口不再有语言维度（发帖体与主题列表都没有 language），但字段本身保留：板块名/描述按语种存，
// 前端按显示语言取键、缺键时走自己的回退链；服务端不替客户端解析成单一语言。
var defaultBoards = []struct {
	Code         string
	Names        map[string]string
	Descriptions map[string]string
	Color        string
	Icon         string
	Order        int
	InFeed       bool
}{
	{"announcement", newLocalizedText("站点公告", "站點公告", "サイトのお知らせ", "Site Announcements"), newLocalizedText("站点公告与运营通知", "站點公告與營運通知", "サイトのお知らせと運営からの連絡", "Site announcements and operational notices"), "amber", "Megaphone", 10, true},
	{"casual", newLocalizedText("闲聊杂谈", "閒聊雜談", "雑談・おしゃべり", "Casual Talk"), newLocalizedText("轻松闲聊与日常交流", "輕鬆閒聊與日常交流", "気軽な雑談と日々の交流", "Light chat and everyday conversation"), "purple", "Coffee", 20, true},
	{"qa", newLocalizedText("求助答疑", "求助答疑", "質問・相談", "Questions & Help"), newLocalizedText("使用问题、编目与功能答疑", "使用問題、編目與功能答疑", "使い方・編目・機能についての質問", "Usage, cataloging and feature questions"), "teal", "Hash", 30, true},
	{"reviews", newLocalizedText("考据评注", "考據評註", "考証・評注", "Research & Annotation"), newLocalizedText("版本考证、原盘评析与文献释读", "版本考證、原盤評析與文獻釋讀", "版の考証・原盤評析・文献の読み解き", "Edition research, source analysis and textual annotation"), "emerald", "BookOpen", 40, true},
	{"bug_report", newLocalizedText("反馈与建议", "回饋與建議", "フィードバック", "Feedback & Suggestions"), newLocalizedText("缺陷反馈、功能建议与复现信息", "缺陷回饋、功能建議與重現資訊", "不具合の報告・機能提案・再現情報", "Bug reports, feature requests and reproductions"), "rose", "Bug", 50, true},
	{"comment", newLocalizedText("评论专用", "評論專用", "コメント専用", "Comments Only"), newLocalizedText("作品与讨论的评论承载区，不进入信息流", "作品與討論的評論承載區，不進入動態流", "作品・議論のコメント専用領域（フィードには出さない）", "Comment area for works and discussions; excluded from the feed"), "sky", "MessageCircle", 60, false},
}

// encodeLocales 把语种 map 编成 jsonb 参数：lib/pq 两头都不认 map（写参报 unsupported type，
// 读列报 unsupported Scan … into type *map[string]string），所以读写都过一遍 JSON 文本，
// 落库/读出时由 SQL 的 jsonb 类型与 encoding/json 各自负责解析。
func encodeLocales(value map[string]string) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// decodeLocales 把 jsonb 列读成语种 map。空值（NULL / 空串）与空对象都按空 map 处理：
// 列是 NOT NULL，只可能来自"还没写过值"的历史行。
func decodeLocales(raw string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]string{}
	}
	return out, nil
}

// seedForum 写多语言列，并按同一套派生规则（zh-CN 优先）同步单值回退列——播种也是写入路径，
// 不能让种子板块的单值列永远空着（那会让"单值列是派生回退值"这条不变量出现例外）。
func seedForum(ctx context.Context, db *sql.DB) error {
	for _, b := range defaultBoards {
		names, err := encodeLocales(b.Names)
		if err != nil {
			return err
		}
		descriptions, err := encodeLocales(b.Descriptions)
		if err != nil {
			return err
		}
		if _, err = db.ExecContext(ctx, `
			INSERT INTO community.boards(code,names,descriptions,name,description,color,icon,sort_order,is_enabled,show_in_feed)
			VALUES($1,$2::jsonb,$3::jsonb,$4,$5,$6,$7,$8,true,$9) ON CONFLICT (code) DO NOTHING`,
			b.Code, names, descriptions, aggregateLocale(b.Names), aggregateLocale(b.Descriptions),
			b.Color, b.Icon, b.Order, b.InFeed); err != nil {
			return err
		}
	}
	return nil
}

// commentBoard 是承载"实体评论"的板块。评论与论坛主题共用 forum_topics 存储，
// 靠板块区分语义：评论锚定实体、无独立标题、不进信息流；主题有标题、可独立成文。
const commentBoard = "comment"

// feedScanCap 是带关键词搜索时的扫描上限。评论正文在本服务 schema，而条目
// 标题在 catalog schema，跨 schema 无法在一条 SQL 内完成匹配，因此取一个有界
// 窗口在 Go 侧过滤，避免无上限地把整表读进内存。
const feedScanCap = 500

// forumBoard 是板块的对外形状。names/descriptions 是四语 map，服务端不做单语解析；
// name/description 是**兼容/回退单值列**（容量层保留，值由多语言 map 的 zh-CN 派生），
// 老前端只认单值时仍然有值可显示，但写入只认 names/descriptions。
type forumBoard struct {
	Code         string            `json:"code"`
	Names        map[string]string `json:"names"`
	Descriptions map[string]string `json:"descriptions"`
	Name         string            `json:"name"`
	Description  string            `json:"description"`
	Color        string            `json:"color"`
	Icon         string            `json:"icon"`
	SortOrder    int               `json:"sort_order"`
	IsEnabled    bool              `json:"is_enabled"`
	ShowInFeed   bool              `json:"show_in_feed"`
}

// boardCols 是板块的 SELECT / RETURNING 列清单，列表接口与管理接口共用同一份顺序，
// 避免两处列不同步导致 scanBoardRow 静默串列。
const boardCols = "code,names,descriptions,name,description,color,icon,sort_order,is_enabled,show_in_feed"

func (h *Handler) listBoards(ctx context.Context) ([]forumBoard, error) {
	rows, err := h.db.QueryContext(ctx, "SELECT "+boardCols+" FROM community.boards ORDER BY sort_order,code")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []forumBoard{}
	for rows.Next() {
		b, err := scanBoardRow(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

var slugPattern = regexp.MustCompile(`[^a-z0-9]+`)

// tagSlug 生成标签的唯一键。仅把 ASCII 字母数字规整成 kebab-case；
// 非拉丁标签（如中文）规范化后会变成空串，此时**回退为原名称**——
// 否则纯中文标签会因 slug 为空被静默丢弃（曾因此丢失全部中文标签）。
func tagSlug(name string) string {
	name = strings.TrimSpace(name)
	if s := strings.Trim(slugPattern.ReplaceAllString(strings.ToLower(name), "-"), "-"); s != "" {
		return s
	}
	return name
}

// topicRow 是主题列表/详情的统一扫描结果。tags 由独立查询补齐。
type topicRow struct {
	ID           string         `json:"id"`
	BoardCode    string         `json:"board_code"`
	AuthorID     string         `json:"user_id"`
	AuthorName   string         `json:"author_name"`
	Title        string         `json:"title"`
	Body         string         `json:"content"`
	EntityID     sql.NullString `json:"-"`
	IsPinned     bool           `json:"is_pinned"`
	IsLocked     bool           `json:"is_locked"`
	ViewCount    int            `json:"view_count"`
	ReplyCount   int            `json:"reply_count"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	LastActivity time.Time      `json:"last_activity_at"`
}

func (t topicRow) toMap() map[string]any {
	out := map[string]any{
		"id": t.ID, "board_code": t.BoardCode, "user_id": t.AuthorID,
		"author_name": t.AuthorName, "title": t.Title, "content": t.Body,
		"is_pinned": t.IsPinned, "is_locked": t.IsLocked,
		"view_count": t.ViewCount, "reply_count": t.ReplyCount,
		"created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
		"last_activity_at": t.LastActivity,
		"user":             map[string]any{"id": t.AuthorID, "username": t.AuthorName},
	}
	if t.EntityID.Valid {
		out["entity_id"] = t.EntityID.String
	}
	return out
}

const topicCols = `t.id::text,t.board_code,t.author_id::text,COALESCE(NULLIF(t.author_name,''),'Anonymous'),t.title,t.body,t.entity_id::text,t.is_pinned,t.is_locked,t.view_count,t.reply_count,t.created_at,t.updated_at,t.last_activity_at`

func scanTopic(rows *sql.Rows) (topicRow, error) {
	var t topicRow
	err := rows.Scan(&t.ID, &t.BoardCode, &t.AuthorID, &t.AuthorName, &t.Title, &t.Body,
		&t.EntityID, &t.IsPinned, &t.IsLocked, &t.ViewCount, &t.ReplyCount, &t.CreatedAt, &t.UpdatedAt, &t.LastActivity)
	return t, err
}

// attachTopicEntities 经 Catalog 接口批量补齐主题锚定实体的标题与 kind，
// 与评论信息流用同一边界取元信息，不直接 JOIN catalog 表。不可见或已删除的
// 实体不注入字段，前端据此不渲染关联横幅。
func attachTopicEntities(ctx context.Context, h *Handler, items []map[string]any) {
	ids := []string{}
	seen := map[string]bool{}
	for _, it := range items {
		id, _ := it["entity_id"].(string)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	// 目录没有批量端点：批量取投影时逐条并发，单条失败即视为不可见（跳过该条）。
	meta := h.catalog.LookupMany(ctx, ids)
	for _, it := range items {
		id, _ := it["entity_id"].(string)
		e, ok := meta[id]
		if !ok {
			continue
		}
		it["entity_title"] = e.Title
		it["entity_kind"] = e.Kind
	}
}

// forumTag 是主题标签的对外形状，与 /community/topic-tags（标签清单）一致：
// 前端按 {id,name} 渲染标签并按 id 筛选，后端不得退回裸字符串数组。
type forumTag struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// topicTags 批量取主题标签，避免逐条查询。
func (h *Handler) topicTags(ctx context.Context, ids []string) (map[string][]forumTag, error) {
	out := map[string][]forumTag{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT tt.topic_id::text, g.id, g.name
		FROM community.topic_tags tt JOIN community.tags g ON g.id = tt.tag_id
		WHERE tt.topic_id = ANY($1::uuid[])
		ORDER BY g.name`, pq.Array(ids))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var tid string
		var tg forumTag
		if err = rows.Scan(&tid, &tg.ID, &tg.Name); err != nil {
			return out, err
		}
		out[tid] = append(out[tid], tg)
	}
	return out, rows.Err()
}

func (h *Handler) registerForum(api *gin.RouterGroup) {
	// 板块列表：前端 fetchBoards 期望裸数组。
	api.GET("/community/boards", h.guard(false), func(c *gin.Context) {
		boards, err := h.listBoards(c.Request.Context())
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, boards)
	})

	api.GET("/community/topics", h.guard(false), func(c *gin.Context) {
		limit, offset := pagingLimitOffset(c, 30)
		args := []any{}
		where := []string{"1=1"}
		// 评论与主题共用存储但语义不同：默认只列"主题"（排除评论板块），
		// 仅当显式按评论板块筛选时才返回评论。
		if bc := strings.TrimSpace(c.Query("board_code")); bc != "" && bc != "all" {
			args = append(args, bc)
			where = append(where, fmt.Sprintf("t.board_code=$%d", len(args)))
		} else {
			args = append(args, commentBoard)
			where = append(where, fmt.Sprintf("t.board_code<>$%d", len(args)))
		}
		if q := strings.TrimSpace(c.Query("q")); q != "" {
			args = append(args, "%"+q+"%")
			where = append(where, fmt.Sprintf("(t.title ILIKE $%d OR t.body ILIKE $%d)", len(args), len(args)))
		}
		if raw := strings.TrimSpace(c.Query("entity_id")); raw != "" {
			if _, err := uuid.Parse(raw); err != nil {
				c.JSON(200, gin.H{"items": []any{}, "total": 0})
				return
			}
			args = append(args, raw)
			where = append(where, fmt.Sprintf("t.entity_id=$%d", len(args)))
		}
		// 标签筛选按名称或 id 命中关联表。
		if tagID := strings.TrimSpace(c.Query("tag_id")); tagID != "" {
			args = append(args, tagID)
			where = append(where, fmt.Sprintf("EXISTS (SELECT 1 FROM community.topic_tags tt WHERE tt.topic_id=t.id AND tt.tag_id=$%d)", len(args)))
		} else if tag := strings.TrimSpace(c.Query("tag")); tag != "" {
			args = append(args, tag)
			where = append(where, fmt.Sprintf("EXISTS (SELECT 1 FROM community.topic_tags tt JOIN community.tags g ON g.id=tt.tag_id WHERE tt.topic_id=t.id AND g.name=$%d)", len(args)))
		}
		clause := strings.Join(where, " AND ")

		var total int
		if err := h.db.QueryRowContext(c.Request.Context(), "SELECT count(*) FROM community.topics t WHERE "+clause, args...).Scan(&total); err != nil {
			fail(c, 500, "module_error")
			return
		}
		pageArgs := append(append([]any{}, args...), limit, offset)
		rows, err := h.db.QueryContext(c.Request.Context(),
			"SELECT "+topicCols+" FROM community.topics t WHERE "+clause+
				" ORDER BY t.is_pinned DESC, t.last_activity_at DESC LIMIT $"+strconv.Itoa(len(pageArgs)-1)+" OFFSET $"+strconv.Itoa(len(pageArgs)),
			pageArgs...)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		items := []map[string]any{}
		ids := []string{}
		for rows.Next() {
			t, err := scanTopic(rows)
			if err != nil {
				rows.Close()
				fail(c, 500, "module_error")
				return
			}
			ids = append(ids, t.ID)
			items = append(items, t.toMap())
		}
		rows.Close()
		tags, terr := h.topicTags(c.Request.Context(), ids)
		if terr != nil {
			fail(c, 500, "module_error")
			return
		}
		for _, it := range items {
			if tg, ok := tags[it["id"].(string)]; ok {
				it["tags"] = tg
			} else {
				it["tags"] = []forumTag{}
			}
		}
		attachTopicEntities(c.Request.Context(), h, items)
		c.JSON(200, gin.H{"items": items, "total": total})
	})

	// 标签清单：前端期望裸数组。
	api.GET("/community/topic-tags", h.guard(false), func(c *gin.Context) {
		args := []any{}
		q := "SELECT g.id,g.name FROM community.tags g"
		if s := strings.TrimSpace(c.Query("q")); s != "" {
			args = append(args, "%"+s+"%")
			q += " WHERE g.name ILIKE $1"
		}
		q += " ORDER BY g.name LIMIT 200"
		rows, err := h.db.QueryContext(c.Request.Context(), q, args...)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		defer rows.Close()
		out := []map[string]any{}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				fail(c, 500, "module_error")
				return
			}
			out = append(out, map[string]any{"id": id, "name": name})
		}
		c.JSON(200, out)
	})

	api.GET("/community/topics/:id", h.guard(false), func(c *gin.Context) {
		id := c.Param("id")
		if _, err := uuid.Parse(id); err != nil {
			fail(c, 404, "not_found")
			return
		}
		// 浏览量自增与读取合并：一次 UPDATE ... RETURNING 完成。
		// 排除评论板块：评论不是"文章"，不应有主题详情页（应回到其锚定的条目）。
		rows, err := h.db.QueryContext(c.Request.Context(),
			"UPDATE community.topics t SET view_count=view_count+1 WHERE t.id=$1 AND t.board_code<>$2 RETURNING "+topicCols, id, commentBoard)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		if !rows.Next() {
			rows.Close()
			fail(c, 404, "not_found")
			return
		}
		t, err := scanTopic(rows)
		rows.Close()
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		posts, err := h.topicPosts(c.Request.Context(), id)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		tags, _ := h.topicTags(c.Request.Context(), []string{id})
		out := t.toMap()
		out["posts"] = posts
		out["comments"] = posts
		if tg, ok := tags[id]; ok {
			out["tags"] = tg
		} else {
			out["tags"] = []forumTag{}
		}
		single := []map[string]any{out}
		attachTopicEntities(c.Request.Context(), h, single)
		c.JSON(200, out)
	})

	// 发主题需要 community.post.create（member 组默认持有；自定义组没给该码就不能发帖）。
	api.POST("/community/topics", h.require(auth.PermissionPostCreate), func(c *gin.Context) {
		var in struct {
			BoardCode string   `json:"board_code"`
			Title     string   `json:"title"`
			Content   string   `json:"content"`
			WorkID    string   `json:"work_id"`
			EntityID  string   `json:"entity_id"`
			TagIDs    []int64  `json:"tag_ids"`
			TagNames  []string `json:"tag_names"`
		}
		if !body(c, &in) {
			return
		}
		in.Title = strings.TrimSpace(in.Title)
		in.Content = strings.TrimSpace(in.Content)
		if in.Title == "" || len(in.Title) > 300 || in.Content == "" || len(in.Content) > 50000 {
			fail(c, 400, "invalid_payload")
			return
		}
		// 板块必须存在且启用，避免写入悬空引用。
		var ok bool
		if err := h.db.QueryRowContext(c.Request.Context(), "SELECT is_enabled FROM community.boards WHERE code=$1", in.BoardCode).Scan(&ok); err != nil || !ok {
			fail(c, 400, "invalid_board")
			return
		}
		// 关联实体必须可见，否则视为非法引用。
		entityID := strings.TrimSpace(in.EntityID)
		if entityID == "" {
			entityID = strings.TrimSpace(in.WorkID)
		}
		if entityID != "" {
			if _, err := uuid.Parse(entityID); err != nil {
				fail(c, 400, "invalid_reference")
				return
			}
			if !h.entity(c, entityID) {
				return
			}
		}
		p := h.principal(c)
		tid := uuid.NewString()
		tx, err := h.db.BeginTx(c.Request.Context(), nil)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		defer tx.Rollback()
		var entity any
		if entityID != "" {
			entity = entityID
		}
		if _, err = tx.ExecContext(c.Request.Context(), `
			INSERT INTO community.topics(id,board_code,author_id,author_name,title,body,entity_id)
			VALUES($1,$2,$3,$4,$5,$6,$7)`,
			tid, in.BoardCode, p.ID, authorName(p), in.Title, in.Content, entity); err != nil {
			fail(c, 500, "module_error")
			return
		}
		for _, name := range in.TagNames {
			if err = h.attachTagByName(c.Request.Context(), tx, tid, name); err != nil {
				fail(c, 500, "module_error")
				return
			}
		}
		for _, tagID := range in.TagIDs {
			if _, err = tx.ExecContext(c.Request.Context(), `INSERT INTO community.topic_tags(topic_id,tag_id) SELECT $1,id FROM community.tags WHERE id=$2 ON CONFLICT DO NOTHING`, tid, tagID); err != nil {
				fail(c, 500, "module_error")
				return
			}
		}
		if err = tx.Commit(); err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{
			"id": tid, "board_code": in.BoardCode, "title": in.Title, "content": in.Content,
			"user_id": p.ID, "author_name": authorName(p), "view_count": 0, "reply_count": 0,
			"is_pinned": false, "is_locked": false,
		})
	})

	// 回帖与发主题同一权限码：能发帖就能回帖，二者不拆开。
	api.POST("/community/topics/:id/posts", h.require(auth.PermissionPostCreate), func(c *gin.Context) {
		topicID := c.Param("id")
		if _, err := uuid.Parse(topicID); err != nil {
			fail(c, 404, "not_found")
			return
		}
		var in struct {
			Content           string `json:"content"`
			Body              string `json:"body"`
			ReplyToPostNumber *int   `json:"reply_to_post_number"`
			ReplyToPostID     string `json:"reply_to_post_id"`
		}
		if c.ShouldBindJSON(&in) != nil {
			fail(c, 400, "invalid_payload")
			return
		}
		content := strings.TrimSpace(in.Content)
		if content == "" {
			content = strings.TrimSpace(in.Body)
		}
		if content == "" || len(content) > 50000 {
			fail(c, 400, "invalid_payload")
			return
		}
		var locked bool
		if err := h.db.QueryRowContext(c.Request.Context(), "SELECT is_locked FROM community.topics WHERE id=$1", topicID).Scan(&locked); err == sql.ErrNoRows {
			fail(c, 404, "not_found")
			return
		} else if err != nil {
			fail(c, 500, "module_error")
			return
		}
		if locked {
			fail(c, 403, "topic_locked")
			return
		}
		p := h.principal(c)
		pid := uuid.NewString()
		tx, err := h.db.BeginTx(c.Request.Context(), nil)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		defer tx.Rollback()
		// post_number 在主题内单调递增。必须锁"主题行"而不是聚合查询：
		// PostgreSQL 不允许 FOR UPDATE 与 MAX() 等聚合函数同时出现。
		// 先锁住该主题即可把同一主题的回帖串行化，再取当前最大楼号。
		var lockedNow bool
		if err = tx.QueryRowContext(c.Request.Context(),
			"SELECT is_locked FROM community.topics WHERE id=$1 FOR UPDATE", topicID).Scan(&lockedNow); err == sql.ErrNoRows {
			// 主题在预检与开启事务之间被删除。
			fail(c, 404, "not_found")
			return
		} else if err != nil {
			fail(c, 500, "module_error")
			return
		}
		if lockedNow {
			fail(c, 403, "topic_locked")
			return
		}
		var next int
		if err = tx.QueryRowContext(c.Request.Context(),
			"SELECT COALESCE(MAX(post_number),1)+1 FROM community.posts WHERE topic_id=$1", topicID).Scan(&next); err != nil {
			fail(c, 500, "module_error")
			return
		}
		replyTo := in.ReplyToPostNumber
		if replyTo == nil && in.ReplyToPostID != "" {
			var n int
			if err = tx.QueryRowContext(c.Request.Context(), "SELECT post_number FROM community.posts WHERE id=$1 AND topic_id=$2", in.ReplyToPostID, topicID).Scan(&n); err == nil {
				replyTo = &n
			}
		}
		if _, err = tx.ExecContext(c.Request.Context(), `
			INSERT INTO community.posts(id,topic_id,author_id,author_name,body,post_number,reply_to_post_number)
			VALUES($1,$2,$3,$4,$5,$6,$7)`, pid, topicID, p.ID, authorName(p), content, next, replyTo); err != nil {
			fail(c, 500, "module_error")
			return
		}
		if _, err = tx.ExecContext(c.Request.Context(),
			"UPDATE community.topics SET reply_count=reply_count+1, last_activity_at=now(), updated_at=now() WHERE id=$1", topicID); err != nil {
			fail(c, 500, "module_error")
			return
		}
		if err = tx.Commit(); err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{
			"id": pid, "topic_id": topicID, "user_id": p.ID, "author_name": authorName(p),
			"content": content, "post_number": next, "reply_to_post_number": replyTo,
		})
	})

	// 主题删除：作者本人，或持有帖子治理权限码的人。
	api.DELETE("/community/topics/:id", h.guard(true), func(c *gin.Context) {
		// 非法 uuid 直接按不存在处理：送进 uuid 列只会拿到 pq 的解析错误再兜成 500。
		topicID := c.Param("id")
		if _, err := uuid.Parse(topicID); err != nil {
			fail(c, 404, "not_found")
			return
		}
		p := h.principal(c)
		q := "DELETE FROM community.topics WHERE id=$1 AND author_id=$2"
		args := []any{topicID, p.ID}
		// 删别人的主题属"帖子治理"，对齐账号服务的 community.post.moderate：
		// 这是 moderator 与 community_admin 都持有的实际治理码；topic.pin 只覆盖置顶、
		// board.manage 只管板块结构，都不能替代"处置内容"这一语义。
		if p.Can(auth.PermissionPostModerate) {
			q = "DELETE FROM community.topics WHERE id=$1"
			args = args[:1]
		}
		res, err := h.db.ExecContext(c.Request.Context(), q, args...)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fail(c, 404, "not_found")
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})

	// 回复删除：作者本人，或持有帖子治理权限码的人（同主题删除：删他人的回复是治理行为）。
	api.DELETE("/community/topics/:id/posts/:postId", h.guard(true), func(c *gin.Context) {
		topicID, postID := c.Param("id"), c.Param("postId")
		if _, err := uuid.Parse(topicID); err != nil {
			fail(c, 404, "not_found")
			return
		}
		if _, err := uuid.Parse(postID); err != nil {
			fail(c, 404, "not_found")
			return
		}
		p := h.principal(c)
		q := "DELETE FROM community.posts WHERE id=$1 AND topic_id=$2 AND author_id=$3"
		args := []any{postID, topicID, p.ID}
		if p.Can(auth.PermissionPostModerate) {
			q = "DELETE FROM community.posts WHERE id=$1 AND topic_id=$2"
			args = args[:2]
		}
		res, err := h.db.ExecContext(c.Request.Context(), q, args...)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			fail(c, 404, "not_found")
			return
		}
		if _, err := h.db.ExecContext(c.Request.Context(),
			"UPDATE community.topics SET reply_count=GREATEST(reply_count-1,0) WHERE id=$1", topicID); err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, gin.H{"ok": true})
	})

	// 置顶 / 取消置顶：运营动作，需要 community.topic.pin（member 与普通编辑都不持有）。
	// 只写既有的 is_pinned 列，响应沿用主题列表/详情的形状，前端不需要另写一套解析。
	api.PUT("/community/topics/:id/pin", h.require(auth.PermissionTopicPin), func(c *gin.Context) {
		topicID := c.Param("id")
		if _, err := uuid.Parse(topicID); err != nil {
			fail(c, 404, "not_found")
			return
		}
		var in struct {
			Pinned *bool `json:"pinned"`
		}
		if !body(c, &in) {
			return
		}
		// 指针而非 bool：缺字段与 false 不可区分，静默按 false 会让"漏传的调用"变成取消置顶。
		if in.Pinned == nil {
			fail(c, 400, "invalid_payload")
			return
		}
		// 评论不是文章（与主题详情同一口径）：不给评论板块的条目置顶。
		rows, err := h.db.QueryContext(c.Request.Context(),
			`UPDATE community.topics t SET is_pinned=$2, updated_at=now() WHERE t.id=$1 AND t.board_code<>$3 RETURNING `+topicCols,
			topicID, *in.Pinned, commentBoard)
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		if !rows.Next() {
			rows.Close()
			fail(c, 404, "not_found")
			return
		}
		t, err := scanTopic(rows)
		rows.Close()
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		out := t.toMap()
		attachTopicEntities(c.Request.Context(), h, []map[string]any{out})
		c.JSON(200, out)
	})
}

func authorName(p *auth.Principal) string {
	if p == nil || p.Username == "" {
		return "User"
	}
	return p.Username
}

func (h *Handler) topicPosts(ctx context.Context, topicID string) ([]map[string]any, error) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT id::text, topic_id::text, author_id::text, COALESCE(NULLIF(author_name,''),'Anonymous'),
		       body, post_number, reply_to_post_number, created_at
		FROM community.posts WHERE topic_id=$1 ORDER BY post_number`, topicID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, tid, uid, uname, body string
		var num int
		var replyTo sql.NullInt64
		var at time.Time
		if err := rows.Scan(&id, &tid, &uid, &uname, &body, &num, &replyTo, &at); err != nil {
			return nil, err
		}
		item := map[string]any{
			"id": id, "topic_id": tid, "user_id": uid, "author_name": uname,
			"content": body, "post_number": num, "created_at": at,
			"user": map[string]any{"id": uid, "username": uname},
		}
		if replyTo.Valid {
			item["reply_to_post_number"] = replyTo.Int64
		} else {
			item["reply_to_post_number"] = nil
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// attachTagByName 按名称取或建标签并关联；名称为空则跳过。
func (h *Handler) attachTagByName(ctx context.Context, tx *sql.Tx, topicID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if len(name) > 60 {
		name = name[:60]
	}
	slug := tagSlug(name)
	if slug == "" {
		return nil
	}
	var id int64
	err := tx.QueryRowContext(ctx, `INSERT INTO community.tags(name,slug) VALUES($1,$2) ON CONFLICT (slug) DO UPDATE SET name=EXCLUDED.name RETURNING id`, name, slug).Scan(&id)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO community.topic_tags(topic_id,tag_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, topicID, id)
	return err
}
