package handler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// 板块写接口是运营配置：名称与描述（四语 map）、颜色、图标、排序、两个开关。
//
// 三条边界：code 是 community.topics.board_code 的外键，**不可改**（改码要么让存量主题悬空、
// 要么得级联改主题的板块归属，都不该由"改名"顺带做掉）；本服务不提供创建与删除板块
// （板块由种子播种、运营配置），删除会让存量主题失去归属；描述不是必填，但名称是身份。
//
// 只改传入字段：调用方想单独切 show_in_feed 或 is_enabled 时不必回传整份配置，
// 也就不会因为漏带某个字段而把它清空。

// boardNameLocales 是板块名称必须齐备的语种，与目录侧定义名称同一口径：
// zh-CN / zh-TW / en-US 逐个必需，日文接受 ja 或 ja-JP 其中之一（存量两种键名都有）。
var boardNameLocales = []string{"zh-CN", "zh-TW", "en-US"}

// missingBoardNameLocales 返回名称缺失的语种（名称整体为空时即全部缺失）。
func missingBoardNameLocales(names map[string]string) []string {
	missing := []string{}
	for _, loc := range boardNameLocales {
		if strings.TrimSpace(names[loc]) == "" {
			missing = append(missing, loc)
		}
	}
	if strings.TrimSpace(names["ja"]) == "" && strings.TrimSpace(names["ja-JP"]) == "" {
		missing = append(missing, "ja-JP")
	}
	return missing
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
		// 名称一旦传入就必须四语齐备：错误串列出缺失语种，前端据此提示补哪几个。
		if in.Names != nil {
			if missing := missingBoardNameLocales(in.Names); len(missing) > 0 {
				fail(c, 400, "four_locale_names_required: "+strings.Join(missing, ","))
				return
			}
		}
		set := []string{}
		args := []any{code}
		add := func(column string, value any) {
			args = append(args, value)
			set = append(set, fmt.Sprintf("%s=$%d", column, len(args)))
		}
		if in.Names != nil {
			raw, _ := json.Marshal(in.Names)
			add("names", string(raw))
		}
		if in.Descriptions != nil {
			raw, _ := json.Marshal(in.Descriptions)
			add("descriptions", string(raw))
		}
		// 空串的颜色/图标会把前端渲染成无样式：按非法值拒掉，而不是静默写坏展示。
		if in.Color != nil {
			if strings.TrimSpace(*in.Color) == "" {
				fail(c, 400, "invalid_payload")
				return
			}
			add("color", strings.TrimSpace(*in.Color))
		}
		if in.Icon != nil {
			if strings.TrimSpace(*in.Icon) == "" {
				fail(c, 400, "invalid_payload")
				return
			}
			add("icon", strings.TrimSpace(*in.Icon))
		}
		if in.SortOrder != nil {
			add("sort_order", *in.SortOrder)
		}
		if in.IsEnabled != nil {
			add("is_enabled", *in.IsEnabled)
		}
		if in.ShowInFeed != nil {
			add("show_in_feed", *in.ShowInFeed)
		}
		// 空载荷（一个字段都没传）不是"什么也不做"，而是调用方搞错了口径：直接拒绝。
		if len(set) == 0 {
			fail(c, 400, "invalid_payload")
			return
		}
		row := h.db.QueryRowContext(c.Request.Context(),
			"UPDATE community.boards SET "+strings.Join(set, ",")+
				" WHERE code=$1 RETURNING code,names,descriptions,color,icon,sort_order,is_enabled,show_in_feed",
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

// scanBoardRow 的列顺序与 listBoards 一致；names/descriptions 是 JSONB，解成 map 再回给调用方。
func scanBoardRow(scan func(...any) error) (forumBoard, error) {
	var b forumBoard
	var names, descs []byte
	if err := scan(&b.Code, &names, &descs, &b.Color, &b.Icon, &b.SortOrder, &b.IsEnabled, &b.ShowInFeed); err != nil {
		return forumBoard{}, err
	}
	b.Names = map[string]string{}
	b.Descriptions = map[string]string{}
	_ = json.Unmarshal(names, &b.Names)
	_ = json.Unmarshal(descs, &b.Descriptions)
	return b, nil
}
