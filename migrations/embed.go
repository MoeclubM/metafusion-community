// Package migrations 嵌入版本化迁移文件：二进制在任何环境都能独立建库，
// 不需要把 .sql 单独拷进镜像。
//
// 服务启动（internal/store.Init）与任何显式迁移入口读的是**同一份文件**，
// 因此不存在"启动执行一份 DDL、迁移工具执行另一份"的双份结构（见
// docs/architecture/decoupling-audit-2026-09.md §4）。
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
