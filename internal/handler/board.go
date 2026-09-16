package handler

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

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
// 名称与描述**只收多语言 map**，不收单值 name/description：单值列是容量层的兼容/回退列，
// 由多语言 map 派生（见 upsertBoardLocales），不给它一条反向定义多语言值的写入口——
// 那等于在接口层把语言维度又装回来，与"接口去语言维度、字段保留"的决议相悖。
// 旧客户端只传 {"name":...} 会被当成空载荷 400，这是有意的：gin 忽略未知字段，静默接受
// 只会写出一个说不清语种的板块名。

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

// addLocaleUpdate 写入多语言列，并把单值兼容列同步成聚合值。
//
// 同步方向只有一个：多语言 map → 单值列。单值列的值取 zh-CN（缺则第一个有值的语种），
// 因此"库里单值列非空但 names 为空"这种只有 000002 之后的旧实例才有的状态不会再现。
// 允许 value 为空 map：那是显式清空（description/descriptions 不是板块身份）。
//
// 语种 map 编成 JSON 文本再经 ::jsonb 转换：lib/pq 不认 map 类型。编码不可能失败
// （map 的键值都是 string），这里用 encodeLocales 的返回值兜底只是为了不吞掉错误。
func addLocaleUpdate(set *[]string, args *[]any, column, derive string, value map[string]string) {
	encoded, err := encodeLocales(value)
	if err != nil {
		encoded = "{}"
	}
	*args = append(*args, encoded)
	*set = append(*set, fmt.Sprintf("%s=$%d::jsonb", column, len(*args)))
	if derive != "" {
		*args = append(*args, aggregateLocale(value))
		*set = append(*set, fmt.Sprintf("%s=$%d", derive, len(*args)))
	}
}

// aggregateLocale 把多语言 map 收敛成一个回退单值：zh-CN 优先，缺失时按语种清单取第一个非空值。
func aggregateLocale(value map[string]string) string {
	if v := strings.TrimSpace(value["zh-CN"]); v != "" {
		return v
	}
	for _, locale := range boardLocales {
		if v := strings.TrimSpace(value[locale]); v != "" {
			return v
		}
	}
	return ""
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
			addLocaleUpdate(&set, &args, "names", "name", names)
		}
		// 描述不是身份：允许四语全传空串来清空（严格四语校验由 resolveBoardLocales 负责）。
		if in.Descriptions != nil {
			descriptions, errCode := resolveBoardLocales(in.Descriptions)
			if errCode != "" {
				fail(c, 400, errCode)
				return
			}
			addLocaleUpdate(&set, &args, "descriptions", "description", descriptions)
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
		row := h.db.QueryRowContext(c.Request.Context(),
			"UPDATE community.boards SET "+strings.Join(set, ",")+
				" WHERE code=$1 RETURNING "+boardCols,
			args...)
		board, err := scanBoardRow(row.Scan)
		if err == sql.ErrNoRows {
			fail(c, 404, "not_found")
			return
		}
		if err != nil {
			fail(c, 500, "module_error")
			return
		}
		c.JSON(200, board)
	})
}

// scanBoardRow 的列顺序与 boardCols 一致（listBoards 与板块配置共用同一份清单）。
//
// 两个 jsonb 列先读成 string 再解成 map：驱动不认 map 目标（见 encodeLocales）。
func scanBoardRow(scan func(...any) error) (forumBoard, error) {
	var b forumBoard
	var names, descriptions string
	if err := scan(&b.Code, &names, &descriptions, &b.Name, &b.Description, &b.Color, &b.Icon, &b.SortOrder, &b.IsEnabled, &b.ShowInFeed); err != nil {
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
