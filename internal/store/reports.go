package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// 举报与申诉的存储层：表结构见 migrations/000008_reports.up.sql，口径与取舍写在那个文件的注释里。
//
// 本层只碰 community schema：跨服务的举报对象（实体 / 用户 / 资源）在这里是**不透明引用**，
// 既不建外键也不跨库查询——"这个 id 在别的服务里存不存在"不是本服务能回答的问题，
// 硬答（查不到就当不存在）会把依赖故障误判成用户输入错误。

// 举报目标类型。四类对象在本服务的解析能力不同：
//   - comment：评论板块（commentBoard）的短评，本地可解析作者、可下线；
//   - post：论坛主题或楼中回复，本地可解析作者、可下线；
//   - user：被举报的用户本人，本地只存 id（用户数据在账号服务）；
//   - entity / resource：本地无内容，只有不透明引用（元数据在目录服务、文件在存储服务）。
const (
	ReportTargetEntity   = "entity"
	ReportTargetComment  = "comment"
	ReportTargetPost     = "post"
	ReportTargetUser     = "user"
	ReportTargetResource = "resource"
)

// ReportTargetTypes 是全部合法举报类型，顺序与迁移里的 CHECK 约束一致。
// 词表由服务端持有、前端只做展示，理由与状态同理（四语名称在前端字典里）。
var ReportTargetTypes = []string{
	ReportTargetEntity, ReportTargetComment, ReportTargetPost, ReportTargetUser, ReportTargetResource,
}

// 举报理由枚举。**不落库为用户可读文案**：库里存码，四语名称由前端字典解析，
// 这样加语种、改措辞都不需要迁移数据。
const (
	ReasonSpam           = "spam"
	ReasonAbuse          = "abuse"
	ReasonHarassment     = "harassment"
	ReasonIllegal        = "illegal"
	ReasonCopyright      = "copyright"
	ReasonPrivacy        = "privacy"
	ReasonMisinformation = "misinformation"
	ReasonOther          = "other"
)

// ReportReasons 是全部合法理由，顺序即管理端与提交表单的展示顺序（由重到轻）。
var ReportReasons = []string{
	ReasonIllegal, ReasonCopyright, ReasonPrivacy, ReasonAbuse, ReasonHarassment, ReasonSpam, ReasonMisinformation, ReasonOther,
}

// 举报状态机：pending → accepted → resolved；pending → rejected。
// rejected / resolved 是终态（再次处置回 ErrReportState），accepted 是"已受理待处置"的中间态。
const (
	ReportPending  = "pending"
	ReportAccepted = "accepted"
	ReportRejected = "rejected"
	ReportResolved = "resolved"
)

// 处置结论。content_removed 与 user_banned 都**不由本服务执行强制动作**：
// 下线内容走既有删除端点、封禁走账号服务的既有封禁端点，这里记录的是处置结论与依据。
const (
	EnforcementNone           = "none"
	EnforcementContentRemoved = "content_removed"
	EnforcementUserBanned     = "user_banned"
)

// 申诉状态：申诉只有"待处理 / 受理 / 驳回"三态，不引入仲裁流程。
const (
	AppealPending  = "pending"
	AppealAccepted = "accepted"
	AppealRejected = "rejected"
)

// 举报与申诉的稳定错误：HTTP 层据此映射状态码，不靠字符串匹配。
var (
	// ErrDuplicateReport：同一举报人对同一对象的**未终结**举报已存在（部分唯一索引 reports_open_unique）。
	ErrDuplicateReport = errors.New("duplicate_report")
	// ErrDuplicateAppeal：被处置方对同一条处置已经申诉过（唯一索引 report_appeals_once）。
	ErrDuplicateAppeal = errors.New("duplicate_appeal")
	// ErrReportNotFound：报告不存在。
	ErrReportNotFound = errors.New("report_not_found")
	// ErrAppealNotFound：申诉不存在。
	ErrAppealNotFound = errors.New("appeal_not_found")
	// ErrReportState：报告当前状态不允许这次处置（例如对已驳回的报告再受理）。
	ErrReportState = errors.New("invalid_report_state")
	// ErrAppealState：申诉当前状态不允许这次处理。
	ErrAppealState = errors.New("invalid_appeal_state")
)

// Report 是一条举报。target_context 是提交时刻的目标快照，字段名不固定（jsonb）：
// 内容被下线或删除之后，队列与详情仍要能看出"当时被举报的是什么"。
type Report struct {
	ID               string         `json:"id"`
	TargetType       string         `json:"target_type"`
	TargetID         string         `json:"target_id"`
	TargetAuthorID   string         `json:"target_author_id,omitempty"`
	TargetAuthorName string         `json:"target_author_name,omitempty"`
	TargetContext    map[string]any `json:"target_context"`
	Reason           string         `json:"reason"`
	Detail           string         `json:"detail,omitempty"`
	EvidenceURL      string         `json:"evidence_url,omitempty"`
	ReporterID       string         `json:"reporter_id"`
	ReporterName     string         `json:"reporter_name"`
	Status           string         `json:"status"`
	ReviewerID       string         `json:"reviewer_id,omitempty"`
	ReviewerName     string         `json:"reviewer_name,omitempty"`
	ReviewNote       string         `json:"review_note,omitempty"`
	Enforcement      string         `json:"enforcement,omitempty"`
	ReviewedAt       *time.Time     `json:"reviewed_at,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	UpdatedAt        time.Time      `json:"updated_at"`
}

// ReportEvent 是时间线上的一行。kind 取 created / accepted / rejected / resolved /
// appealed / appeal_reviewed，actor_role 取 reporter / reviewer / appellant。
type ReportEvent struct {
	ID        int64          `json:"-"`
	ReportID  string         `json:"-"`
	At        time.Time      `json:"at"`
	Kind      string         `json:"kind"`
	ActorID   string         `json:"actor_id,omitempty"`
	ActorName string         `json:"actor_name,omitempty"`
	ActorRole string         `json:"actor_role"`
	Note      string         `json:"note,omitempty"`
	Meta      map[string]any `json:"meta,omitempty"`
}

// ReportAppeal 是被处置方提交的申诉。队列查询会把所属报告的关键字段一并带出来
// （TargetType / TargetID / Reason / ReportStatus）：只给一个 report_id，处理人要再点一次才能判断。
type ReportAppeal struct {
	ID             string     `json:"id"`
	ReportID       string     `json:"report_id"`
	AppellantID    string     `json:"appellant_id"`
	AppellantName  string     `json:"appellant_name"`
	Body           string     `json:"body"`
	Status         string     `json:"status"`
	ReviewerID     string     `json:"reviewer_id,omitempty"`
	ReviewerName   string     `json:"reviewer_name,omitempty"`
	ReviewNote     string     `json:"review_note,omitempty"`
	ReviewedAt     *time.Time `json:"reviewed_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	TargetType     string     `json:"target_type,omitempty"`
	TargetID       string     `json:"target_id,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	ReportStatus   string     `json:"report_status,omitempty"`
	TargetAuthorID string     `json:"target_author_id,omitempty"`
}

// ReportedContent 是举报对象在本服务里的样子。Found=false 有两种含义，调用方必须分开处理：
// 本地内容查不到（已下线 / 从未存在），或该类型本来就不在本地（entity / resource）。
// Kind 区分它们：comment / topic / reply / user 四类本地可达，none 表示本地没有这份内容。
type ReportedContent struct {
	Kind       string
	Found      bool
	AuthorID   string
	AuthorName string
	BoardCode  string
	TopicID    string
	TopicTitle string
	EntityID   string
	Body       string
}

// 举报状态与理由的合法性判定放在服务端：库里的 CHECK 是最后一道防线，
// 但把非法值送进 SQL 会让"输入错误"以 500 的形式返回。
func ValidReportTargetType(v string) bool { return containsString(ReportTargetTypes, v) }
func ValidReportReason(v string) bool     { return containsString(ReportReasons, v) }

func containsString(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

// reportColumns 是报告行的唯一列清单：读路径与 RETURNING 共用，避免两处漂移。
const reportColumns = `id::text,target_type,target_id,COALESCE(target_author_id::text,''),
	target_author_name,target_context,reason,detail,evidence_url,reporter_id::text,reporter_name,status,
	COALESCE(reviewer_id::text,''),reviewer_name,review_note,enforcement,reviewed_at,created_at,updated_at`

func scanReport(scanner interface{ Scan(...any) error }) (Report, error) {
	var r Report
	var raw []byte
	var reviewedAt sql.NullTime
	if err := scanner.Scan(&r.ID, &r.TargetType, &r.TargetID, &r.TargetAuthorID, &r.TargetAuthorName,
		&raw, &r.Reason, &r.Detail, &r.EvidenceURL, &r.ReporterID, &r.ReporterName, &r.Status,
		&r.ReviewerID, &r.ReviewerName, &r.ReviewNote, &r.Enforcement, &reviewedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return Report{}, err
	}
	r.TargetContext = decodeContext(raw)
	if reviewedAt.Valid {
		at := reviewedAt.Time
		r.ReviewedAt = &at
	}
	return r, nil
}

// decodeContext 把 jsonb 解成 map：解不出来（历史行被外部改坏）不报错，退回空 map——
// 队列不该因为一条快照读不出来就整页 500，快照本身是**装饰性**信息。
func decodeContext(raw []byte) map[string]any {
	out := map[string]any{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}

func encodeContext(v map[string]any) ([]byte, error) {
	if len(v) == 0 {
		return []byte("{}"), nil
	}
	return json.Marshal(v)
}

// isUniqueViolation 判定唯一索引冲突（PQ 23505）：重复举报与重复申诉都要映射成稳定错误码，
// 而不是把驱动错误直接回给用户。
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// CreateReport 写入一条举报，并在同一事务里补一行 created 事件。
//
// 重复举报（同一人对同一对象的未终结举报）由部分唯一索引拦下并映射成 ErrDuplicateReport：
// 先查再插在并发下会漏，索引才是唯一可靠的判据。
func (s *Store) CreateReport(ctx context.Context, r Report, at time.Time) error {
	raw, err := encodeContext(r.TargetContext)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO community.reports(
			id,target_type,target_id,target_author_id,target_author_name,target_context,
			reason,detail,evidence_url,reporter_id,reporter_name,status,created_at,updated_at)
			VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13)`,
			r.ID, r.TargetType, r.TargetID, r.TargetAuthorID, r.TargetAuthorName, raw,
			r.Reason, r.Detail, r.EvidenceURL, r.ReporterID, r.ReporterName, ReportPending, at)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrDuplicateReport
			}
			return err
		}
		return appendReportEvent(ctx, tx, ReportEvent{
			ReportID: r.ID, At: at, Kind: "created",
			ActorID: r.ReporterID, ActorName: r.ReporterName, ActorRole: "reporter",
			Meta: map[string]any{"target_type": r.TargetType, "target_id": r.TargetID, "reason": r.Reason},
		})
	})
}

// ReportQuota 一次查出"滚动窗口内该举报人已提交的条数"与"最早一条滚出窗口还需多少秒"。
//
// 反滥用的取值口径：配额判定与 429 的 Retry-After 用同一份数据，避免两次查询之间窗口滑动
// 导致"配额说超了、Retry-After 说再等 0 秒"。计数直接走 community.reports 本身——
// 举报量级远低于限流器要处理的请求量，不值得为它引入计数器表或 Redis。
func (s *Store) ReportQuota(ctx context.Context, reporterID string, window time.Duration, now time.Time) (int, int, error) {
	var total, retryAfter int
	err := s.db.QueryRowContext(ctx, `SELECT count(*),
		COALESCE(CEIL(EXTRACT(EPOCH FROM (min(created_at) + make_interval(secs => $3) - $2::timestamptz))), 0)::int
		FROM community.reports
		WHERE reporter_id=$1 AND created_at >= $2::timestamptz - make_interval(secs => $3)`,
		reporterID, now, int(window.Seconds())).Scan(&total, &retryAfter)
	if retryAfter < 0 {
		retryAfter = 0
	}
	return total, retryAfter, err
}

// ReportByID 读一条报告；不存在回 ErrReportNotFound。
func (s *Store) ReportByID(ctx context.Context, id string) (Report, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+reportColumns+" FROM community.reports WHERE id=$1", id)
	r, err := scanReport(row)
	if err == sql.ErrNoRows {
		return Report{}, ErrReportNotFound
	}
	if err != nil {
		return Report{}, err
	}
	return r, nil
}

// ReportFilter 是队列的过滤条件。空值即"不过滤"：状态多选、类型单选、关键词匹配
// 举报 id / 目标 id / 举报人 / 处理人 / 说明——举报 id 也在里面，因为处理人拿到最多的是
// "用户贴过来的那条举报号"，查不到它就得回列表里翻。
// 关键词用 ILIKE 子串，与社区既有列表同口径
// （不引入 to_tsvector：中文没有空格边界，全文索引只会把"搜不到"变成"看起来支持却搜不到"）。
type ReportFilter struct {
	Statuses   []string
	TargetType string
	Query      string
}

func (f ReportFilter) where(prefix string, args []any) (string, []any) {
	parts := []string{}
	if len(f.Statuses) > 0 {
		args = append(args, pq.Array(f.Statuses))
		parts = append(parts, prefix+"status = ANY($"+strconv.Itoa(len(args))+")")
	}
	if strings.TrimSpace(f.TargetType) != "" {
		args = append(args, f.TargetType)
		parts = append(parts, prefix+"target_type = $"+strconv.Itoa(len(args)))
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		args = append(args, "%"+q+"%")
		idx := strconv.Itoa(len(args))
		parts = append(parts, "("+prefix+"id::text ILIKE $"+idx+" OR "+prefix+"target_id ILIKE $"+idx+
			" OR "+prefix+"reporter_name ILIKE $"+idx+" OR "+prefix+"reviewer_name ILIKE $"+idx+
			" OR "+prefix+"detail ILIKE $"+idx+")")
	}
	if len(parts) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(parts, " AND "), args
}

// ListReports 是管理端队列：按提交时间倒序列出，同时返回满足条件的总数。
// 排序第二关键字是 id：同一批写入的行 created_at 相同，缺了它分页窗口之间会串行。
func (s *Store) ListReports(ctx context.Context, f ReportFilter, limit, offset int) ([]Report, int, error) {
	where, args := f.where("", []any{})
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM community.reports"+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pageArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.QueryContext(ctx, "SELECT "+reportColumns+" FROM community.reports"+where+
		" ORDER BY created_at DESC, id DESC LIMIT $"+strconv.Itoa(len(pageArgs)-1)+" OFFSET $"+strconv.Itoa(len(pageArgs)), pageArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out, err := scanReports(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// ListMyReports 是"我的举报"：只按举报人过滤，不分状态。
func (s *Store) ListMyReports(ctx context.Context, reporterID string, limit, offset int) ([]Report, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM community.reports WHERE reporter_id=$1", reporterID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+reportColumns+" FROM community.reports WHERE reporter_id=$1"+
		" ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3", reporterID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out, err := scanReports(rows)
	if err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func scanReports(rows *sql.Rows) ([]Report, error) {
	out := []Report{}
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReportEvents 读时间线（提交、受理、驳回、处置、申诉、申诉处理）。
func (s *Store) ReportEvents(ctx context.Context, reportID string) ([]ReportEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,report_id::text,at,kind,COALESCE(actor_id::text,''),
		actor_name,actor_role,note,meta FROM community.report_events WHERE report_id=$1 ORDER BY at, id`, reportID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ReportEvent{}
	for rows.Next() {
		var e ReportEvent
		var raw []byte
		if err = rows.Scan(&e.ID, &e.ReportID, &e.At, &e.Kind, &e.ActorID, &e.ActorName, &e.ActorRole, &e.Note, &raw); err != nil {
			return nil, err
		}
		e.Meta = decodeContext(raw)
		out = append(out, e)
	}
	return out, rows.Err()
}

// ReportTransition 是一次处置请求（受理 / 驳回 / 处置）的输入。
// From 是允许的来源状态：状态机本身由存储层用 SELECT ... FOR UPDATE 原子校验，
// 放在 handler 里"先读状态再写"会在并发下把两次处置都放行。
type ReportTransition struct {
	ReportID     string
	Status       string
	From         []string
	Enforcement  string
	ReviewerID   string
	ReviewerName string
	Note         string
}

// TransitionReport 原子地推进状态并落一行事件，返回推进后的报告。
func (s *Store) TransitionReport(ctx context.Context, t ReportTransition, at time.Time) (Report, error) {
	var out Report
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var current string
		err := tx.QueryRowContext(ctx, "SELECT status FROM community.reports WHERE id=$1 FOR UPDATE", t.ReportID).Scan(&current)
		if err == sql.ErrNoRows {
			return ErrReportNotFound
		}
		if err != nil {
			return err
		}
		if !containsString(t.From, current) {
			return ErrReportState
		}
		row := tx.QueryRowContext(ctx, `UPDATE community.reports
			SET status=$2, enforcement=$3, review_note=$4, reviewer_id=NULLIF($5,'')::uuid, reviewer_name=$6,
			    reviewed_at=$7, updated_at=$7
			WHERE id=$1 RETURNING `+reportColumns,
			t.ReportID, t.Status, t.Enforcement, t.Note, t.ReviewerID, t.ReviewerName, at)
		updated, err := scanReport(row)
		if err != nil {
			return err
		}
		out = updated
		meta := map[string]any{"from": current, "to": t.Status}
		if t.Enforcement != "" && t.Enforcement != EnforcementNone {
			meta["enforcement"] = t.Enforcement
		}
		return appendReportEvent(ctx, tx, ReportEvent{
			ReportID: t.ReportID, At: at, Kind: t.Status,
			ActorID: t.ReviewerID, ActorName: t.ReviewerName, ActorRole: "reviewer",
			Note: t.Note, Meta: meta,
		})
	})
	if err != nil {
		return Report{}, err
	}
	return out, nil
}

// ResolveReportedContent 解析举报对象在本服务里的样子（作者、所属主题/板块、正文）。
//
// commentBoard 由调用方传入而不是在这里写死：板块码属于 handler 的领域常量，
// store 反向依赖 handler 会成环，而这个码只有一个来源（handler 的 commentBoard）。
//
// entity / resource 一律返回 Found=false + Kind=none：本地没有这份内容，
// 也**不能**因为查不到就判定"目标不存在"——那是别的服务的数据。
func (s *Store) ResolveReportedContent(ctx context.Context, targetType, targetID, commentBoard string) (ReportedContent, error) {
	switch targetType {
	case ReportTargetComment:
		return s.reportedTopic(ctx, targetID, commentBoard, true)
	case ReportTargetPost:
		// 帖子（论坛主题或楼中回复）：先按主题查，再按回复查。两张表的 id 都是 uuid，
		// 且回复经 topic_id 关联主题，因此命中哪张表就是哪类内容。
		topic, err := s.reportedTopic(ctx, targetID, commentBoard, false)
		if err != nil {
			return ReportedContent{}, err
		}
		if topic.Found {
			return topic, nil
		}
		return s.reportedReply(ctx, targetID)
	case ReportTargetUser:
		// 被举报的用户本人：被处置方就是他，因此作者就是目标本身。
		return ReportedContent{Kind: "user", Found: true, AuthorID: targetID}, nil
	default:
		return ReportedContent{Kind: "none"}, nil
	}
}

// reportedTopic 读 community.topics 里的一行。commentOnly=true 时限定评论板块，
// false 时排除评论板块（评论与论坛主题共用一张表，只有 board_code 的区别）。
func (s *Store) reportedTopic(ctx context.Context, id, commentBoard string, commentOnly bool) (ReportedContent, error) {
	query := `SELECT board_code,author_id::text,COALESCE(NULLIF(author_name,''),'Anonymous'),title,body,
		COALESCE(entity_id::text,'') FROM community.topics WHERE id=$1`
	args := []any{id}
	if commentBoard != "" {
		if commentOnly {
			query += " AND board_code=$2"
		} else {
			query += " AND board_code<>$2"
		}
		args = append(args, commentBoard)
	}
	var board, authorID, authorName, title, body, entityID string
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&board, &authorID, &authorName, &title, &body, &entityID)
	if err == sql.ErrNoRows {
		return ReportedContent{Kind: "none"}, nil
	}
	if err != nil {
		return ReportedContent{}, err
	}
	kind := "topic"
	if commentOnly {
		kind = "comment"
	}
	return ReportedContent{Kind: kind, Found: true, AuthorID: authorID, AuthorName: authorName,
		BoardCode: board, TopicID: id, TopicTitle: title, EntityID: entityID, Body: body}, nil
}

// reportedReply 读 community.posts 里的一行回复，并带上所属主题的标题与板块（治理上下文）。
func (s *Store) reportedReply(ctx context.Context, id string) (ReportedContent, error) {
	var authorID, authorName, body, topicID, title, board string
	err := s.db.QueryRowContext(ctx, `SELECT p.author_id::text,COALESCE(NULLIF(p.author_name,''),'Anonymous'),
		p.body,p.topic_id::text,t.title,t.board_code
		FROM community.posts p JOIN community.topics t ON t.id = p.topic_id WHERE p.id=$1`, id).
		Scan(&authorID, &authorName, &body, &topicID, &title, &board)
	if err == sql.ErrNoRows {
		return ReportedContent{Kind: "none"}, nil
	}
	if err != nil {
		return ReportedContent{}, err
	}
	return ReportedContent{Kind: "reply", Found: true, AuthorID: authorID, AuthorName: authorName,
		Body: body, TopicID: topicID, TopicTitle: title, BoardCode: board}, nil
}

// appendReportEvent 是事件行的唯一写入口（时间线只有一种形状）。
func appendReportEvent(ctx context.Context, tx *sql.Tx, e ReportEvent) error {
	raw, err := encodeContext(e.Meta)
	if err != nil {
		return err
	}
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO community.report_events(report_id,at,kind,actor_id,actor_name,actor_role,note,meta)
		VALUES($1,$2,$3,NULLIF($4,'')::uuid,$5,$6,$7,$8)`,
		e.ReportID, at, e.Kind, e.ActorID, e.ActorName, e.ActorRole, e.Note, raw)
	return err
}

// withTx 是存储层内部的小事务封装：举报/申诉/处置都要"业务行 + 事件行"一起成功。
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
