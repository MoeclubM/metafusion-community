package handler

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/auth"
)

// 板块写接口是运营配置：名称与描述（都是单语言字符串，2026-09-16 起论坛不再分语言）、
// 颜色、图标、排序、两个开关。
//
// 三条边界：code 是 community.topics.board_code 的外键，**不可改**（改码要么让存量主题悬空、
// 要么得级联改主题的板块归属，都不该由"改名"顺带做掉）；本服务不提供创建与删除板块
// （板块由种子播种、运营配置），删除会让存量主题失去归属；描述不是必填，但名称是身份。
//
// 只改传入字段：调用方想单独切 show_in_feed 或 is_enabled 时不必回传整份配置，
// 也就不会因为漏带某个字段而把它清空。

func (h *Handler) registerBoards(api *gin.RouterGroup) {
	api.PUT("/community/boards/:code", h.require(auth.PermissionBoardManage), func(c *gin.Context) {
		code := strings.TrimSpace(c.Param("code"))
		if code == "" {
			fail(c, 404, "not_found")
			return
		}
		var in struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
			Color       *string `json:"color"`
			Icon        *string `json:"icon"`
			SortOrder   *int    `json:"sort_order"`
			IsEnabled   *bool   `json:"is_enabled"`
			ShowInFeed  *bool   `json:"show_in_feed"`
		}
		if !body(c, &in) {
			return
		}
		// 名称是板块身份：传入就必须非空。清空一个板块名会让列表显示空白项，按非法载荷拒掉。
		if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
			fail(c, 400, "invalid_payload")
			return
		}
		set := []string{}
		args := []any{code}
		add := func(column string, value any) {
			args = append(args, value)
			set = append(set, fmt.Sprintf("%s=$%d", column, len(args)))
		}
		if in.Name != nil {
			add("name", strings.TrimSpace(*in.Name))
		}
		if in.Description != nil {
			// 描述不是身份：允许显式清空（传空串即清空），因此只做裁剪不做非空校验。
			add("description", strings.TrimSpace(*in.Description))
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
				" WHERE code=$1 RETURNING code,name,description,color,icon,sort_order,is_enabled,show_in_feed",
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

// scanBoardRow 的列顺序与 listBoards 一致。
func scanBoardRow(scan func(...any) error) (forumBoard, error) {
	var b forumBoard
	if err := scan(&b.Code, &b.Name, &b.Description, &b.Color, &b.Icon, &b.SortOrder, &b.IsEnabled, &b.ShowInFeed); err != nil {
		return forumBoard{}, err
	}
	return b, nil
}
