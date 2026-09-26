package handler

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/audit"
	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// 板块写接口是运营配置：名称与描述（都是**四语 map**，接口不带语言维度，见下）、
// 颜色、图标、排序、两个开关。
//
// 三条边界：code 是 community.topics.board_code 的外键，**不可改**（改码要么让存量主题悬空、
// 要么得级联改主题的板块归属，都不该由"改名"顺带做掉）；本服务不提供创建与删除板块
// （板块由种子播种、运营配置），删除会让存量主题失去归属；描述不是必填，但名称是身份。
//
// 只改传入字段：调用方想单独切 show_in_feed 或 is_enabled 时不必回传整份配置，
// 也就不会因为漏带某个字段而把它清空。
//
// 名称与描述只收多语言 map，不收单值 name/description。

// resolveBoardLocales 规范化传入的语种 map：逐语裁剪空白，四语齐备才算通过，
// 缺语种返回与目录侧同名的错误码（four_locale_names_required: 缺的语种），
// 便于前端复用同一套错误文案。
//
// 显式清空（每个语种都传空串）由调用方另行判定：只有"想清空"才允许全空，
// "漏传某个语种"永远不是清空。
func resolveBoardLocales(locales map[string]string) (map[string]string, string) {
	trimmed := make(map[string]string, len(locales))
	for locale, value := range locales {
		trimmed[locale] = strings.TrimSpace(value)
	}
	missing := []string{}
	for _, locale := range boardLocales {
		if trimmed[locale] == "" {
			missing = append(missing, locale)
		}
	}
	if len(missing) == 0 {
		return trimmed, ""
	}
	// 全部传空等价于"清空这个字段"：调用方据此把 map 落成 {}，而不是被判成缺语种。
	allEmpty := true
	for _, value := range trimmed {
		if value != "" {
			allEmpty = false
			break
		}
	}
	if allEmpty && len(trimmed) > 0 {
		return map[string]string{}, ""
	}
	return nil, "four_locale_names_required: " + strings.Join(missing, ",")
}

// addLocaleUpdate 写入多语言列。
// 允许 value 为空 map：那是显式清空（description/descriptions 不是板块身份）。
//
// 语种 map 编成 JSON 文本再经 ::jsonb 转换：lib/pq 不认 map 类型。编码不可能失败
// （map 的键值都是 string），这里用 encodeLocales 的返回值兜底只是为了不吞掉错误。
func addLocaleUpdate(set *[]string, args *[]any, column string, value map[string]string) {
	encoded, err := encodeLocales(value)
	if err != nil {
		encoded = "{}"
	}
	*args = append(*args, encoded)
	*set = append(*set, fmt.Sprintf("%s=$%d::jsonb", column, len(*args)))
}

func (h *Handler) registerBoards(api *gin.RouterGroup) {
	api.PUT("/community/boards/:code", h.require(auth.PermissionBoardManage), func(c *gin.Context) {
		code := strings.TrimSpace(c.Param("code"))
		if code == "" {
			fail(c, 404, "not_found")
			return
		}
		var in struct {
			Names        map[string]string `json:"names"`
			Descriptions map[string]string `json:"descriptions"`
			Color        *string           `json:"color"`
			Icon         *string           `json:"icon"`
			SortOrder    *int              `json:"sort_order"`
			IsEnabled    *bool             `json:"is_enabled"`
			ShowInFeed   *bool             `json:"show_in_feed"`
		}
		if !body(c, &in) {
			return
		}
		set := []string{}
		args := []any{code}
		// 名称是板块身份：传入就必须四语齐备且非空（清空板块名会让列表显示空白项）。
		if in.Names != nil {
			names, errCode := resolveBoardLocales(in.Names)
			if errCode != "" {
				fail(c, 400, errCode)
				return
			}
			if len(names) == 0 {
				fail(c, 400, "invalid_payload")
				return
			}
			addLocaleUpdate(&set, &args, "names", names)
		}
		// 描述不是身份：允许四语全传空串来清空（严格四语校验由 resolveBoardLocales 负责）。
		if in.Descriptions != nil {
			descriptions, errCode := resolveBoardLocales(in.Descriptions)
			if errCode != "" {
				fail(c, 400, errCode)
				return
			}
			addLocaleUpdate(&set, &args, "descriptions", descriptions)
		}
		// 空串的颜色/图标会把前端渲染成无样式：按非法值拒掉，而不是静默写坏展示。
		if in.Color != nil {
			if strings.TrimSpace(*in.Color) == "" {
				fail(c, 400, "invalid_payload")
				return
			}
			args = append(args, strings.TrimSpace(*in.Color))
			set = append(set, fmt.Sprintf("color=$%d", len(args)))
		}
		if in.Icon != nil {
			if strings.TrimSpace(*in.Icon) == "" {
				fail(c, 400, "invalid_payload")
				return
			}
			args = append(args, strings.TrimSpace(*in.Icon))
			set = append(set, fmt.Sprintf("icon=$%d", len(args)))
		}
		if in.SortOrder != nil {
			args = append(args, *in.SortOrder)
			set = append(set, fmt.Sprintf("sort_order=$%d", len(args)))
		}
		if in.IsEnabled != nil {
			args = append(args, *in.IsEnabled)
			set = append(set, fmt.Sprintf("is_enabled=$%d", len(args)))
		}
		if in.ShowInFeed != nil {
			args = append(args, *in.ShowInFeed)
			set = append(set, fmt.Sprintf("show_in_feed=$%d", len(args)))
		}
		// 空载荷（一个字段都没传）不是"什么也不做"，而是调用方搞错了口径：直接拒绝。
		if len(set) == 0 {
			fail(c, 400, "invalid_payload")
			return
		}
		// 变更前摘要：UPDATE 只给得出"之后"的值，而"把 show_in_feed 关掉"和"把颜色改了"是完全不同的两件事，
		// 只看结果看不出来。写端点独有的那次读，契约 §3 明确允许。
		before, err := scanBoardRow(h.db.QueryRowContext(c.Request.Context(),
			"SELECT "+boardCols+" FROM community.boards WHERE code=$1", code).Scan)
		if err == sql.ErrNoRows {
			fail(c, 404, "not_found")
			return
		}
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		row := h.db.QueryRowContext(c.Request.Context(),
			"UPDATE community.boards SET "+strings.Join(set, ",")+
				" WHERE code=$1 RETURNING "+boardCols,
			args...)
		board, err := scanBoardRow(row.Scan)
		if err == sql.ErrNoRows {
			// 预读之后这一行被并发删掉了：与"不存在"同口径（板块的删除入口不在本服务，几乎不可能走到）。
			fail(c, 404, "not_found")
			return
		}
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		// 只记载荷里出现的字段的差异：没传的字段等于没动，记进去会让"这次改了什么"失真。
		// 语种 map 只记变了的语种（理由是 auditLocaleChanges 的注释）。
		changes := map[string]any{}
		if in.Names != nil {
			auditLocaleChanges("names", before.Names, board.Names, changes)
		}
		if in.Descriptions != nil {
			auditLocaleChanges("descriptions", before.Descriptions, board.Descriptions, changes)
		}
		for field, pair := range map[string][2]any{
			"color":        {before.Color, board.Color},
			"icon":         {before.Icon, board.Icon},
			"sort_order":   {before.SortOrder, board.SortOrder},
			"is_enabled":   {before.IsEnabled, board.IsEnabled},
			"show_in_feed": {before.ShowInFeed, board.ShowInFeed},
		} {
			present := map[string]bool{
				"color": in.Color != nil, "icon": in.Icon != nil, "sort_order": in.SortOrder != nil,
				"is_enabled": in.IsEnabled != nil, "show_in_feed": in.ShowInFeed != nil,
			}[field]
			if present && pair[0] != pair[1] {
				changes[field] = auditChange(pair[0], pair[1])
			}
		}
		audit.Describe(c, audit.Detail{TargetType: "board", TargetID: board.Code, Changes: changes})
		c.JSON(200, board)
	})
}

// scanBoardRow 的列顺序与 boardCols 一致（listBoards 与板块配置共用同一份清单）。
//
// 两个 jsonb 列先读成 string 再解成 map：驱动不认 map 目标（见 encodeLocales）。
func scanBoardRow(scan func(...any) error) (forumBoard, error) {
	var b forumBoard
	var names, descriptions string
	if err := scan(&b.Code, &names, &descriptions, &b.Color, &b.Icon, &b.SortOrder, &b.IsEnabled, &b.ShowInFeed); err != nil {
		return forumBoard{}, err
	}
	var err error
	if b.Names, err = decodeLocales(names); err != nil {
		return forumBoard{}, fmt.Errorf("解析 community.boards.names: %w", err)
	}
	if b.Descriptions, err = decodeLocales(descriptions); err != nil {
		return forumBoard{}, fmt.Errorf("解析 community.boards.descriptions: %w", err)
	}
	return b, nil
}
