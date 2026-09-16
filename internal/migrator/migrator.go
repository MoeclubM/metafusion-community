// Package migrator 按版本应用 migrations/ 下的嵌入迁移。
//
// 与主仓库 backend/internal/migrator 的差别：这里只有 up（互动服务的结构变更都是增量的幂等 DDL），
// 且版本表放在本服务自有的 community schema 里——互动服务不写别人的 schema。
//
// 单文件事务：一个迁移文件在自己的事务里执行并登记版本，失败即整体回滚，
// 不会留下"改了一半结构、版本却没登记"的中间态；已登记的版本直接跳过，因此重复执行是幂等的。
package migrator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/MoeclubM/metafusion-community/migrations"
)

// versionDDL 是迁移登记表本身：它不随业务结构演进，因此写在代码里而不是当作一个迁移文件。
const versionDDL = `
CREATE SCHEMA IF NOT EXISTS community;
CREATE TABLE IF NOT EXISTS community.schema_migrations(
  version bigint PRIMARY KEY,
  name text NOT NULL,
  applied_at timestamptz NOT NULL DEFAULT now(),
  checksum text NOT NULL
);`

// migrationLockKey 是迁移期间的事务级 advisory lock 键。与目录服务（740202）、
// 账号服务（740203）、存储服务（740204）都不同：多副本同时启动时只让一个实例执行 DDL，
// 其余实例等它提交后重查账本空转，避免两条 CREATE TABLE 撞在 pg_type 的唯一索引上。
const migrationLockKey = 740205

type migration struct {
	version  int64
	name     string
	filename string
	content  string
	checksum string
}

// load 读取嵌入的 *.up.sql，按版本号升序返回。文件名约定 NNNNNN_名称.up.sql。
func load() ([]migration, error) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil, err
	}
	out := []migration{}
	seen := map[int64]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		namePart := strings.TrimSuffix(e.Name(), ".up.sql")
		parts := strings.SplitN(namePart, "_", 2)
		version, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("迁移文件名 %s 缺少 NNNNNN_ 前缀: %w", e.Name(), err)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("迁移版本 %d 重复：%s 与 %s", version, prev, e.Name())
		}
		seen[version] = e.Name()
		raw, err := fs.ReadFile(migrations.FS, e.Name())
		if err != nil {
			return nil, err
		}
		name := namePart
		if len(parts) > 1 {
			name = parts[1]
		}
		sum := sha256.Sum256(raw)
		out = append(out, migration{
			version:  version,
			name:     name,
			filename: filepath.Base(e.Name()),
			content:  string(raw),
			checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// Apply 应用所有未应用的迁移；已应用的版本跳过（幂等）。
//
// 已应用版本在校验和被改动时直接报错：迁移文件是历史，改它意味着"库里的结构与文件描述的不再是同一件事"，
// 此时应当新增一个迁移，而不是改旧的那个。
func Apply(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, versionDDL); err != nil {
		return fmt.Errorf("初始化迁移登记表: %w", err)
	}
	files, err := load()
	if err != nil {
		return err
	}
	applied := map[int64]string{}
	rows, err := db.QueryContext(ctx, "SELECT version, checksum FROM community.schema_migrations")
	if err != nil {
		return fmt.Errorf("读取迁移登记表: %w", err)
	}
	for rows.Next() {
		var version int64
		var checksum string
		if err = rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return err
		}
		applied[version] = checksum
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}

	for _, m := range files {
		if checksum, ok := applied[m.version]; ok {
			if checksum != m.checksum {
				return fmt.Errorf("迁移 %s 已应用但内容被改动（登记 checksum %s，当前 %s）：结构变更请新增迁移文件", m.filename, checksum, m.checksum)
			}
			continue
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", migrationLockKey); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("获取迁移锁: %w", err)
		}
		// 取锁后重查账本：等锁期间另一个实例可能已经把这一版应用了，此时必须空转，
		// 否则会重复执行 DDL（幂等语句本身安全，但账本插入会撞主键）。
		var concurrentlyApplied bool
		if err = tx.QueryRowContext(ctx,
			"SELECT EXISTS(SELECT 1 FROM community.schema_migrations WHERE version=$1)", m.version).Scan(&concurrentlyApplied); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("重查迁移登记表 %s: %w", m.filename, err)
		}
		if concurrentlyApplied {
			_ = tx.Rollback()
			continue
		}
		if _, err = tx.ExecContext(ctx, m.content); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("迁移 %s 执行失败: %w", m.filename, err)
		}
		if _, err = tx.ExecContext(ctx,
			"INSERT INTO community.schema_migrations(version,name,checksum) VALUES($1,$2,$3)",
			m.version, m.name, m.checksum); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("登记迁移 %s: %w", m.filename, err)
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
