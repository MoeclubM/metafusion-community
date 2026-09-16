package store

import (
	"context"
	"database/sql"

	"github.com/MoeclubM/metafusion-community/internal/migrator"
)

type Store struct{ db *sql.DB }

func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) DB() *sql.DB  { return s.db }

// Init 应用版本化迁移：表结构的唯一来源是 migrations/*.up.sql（启动与迁移入口读同一份文件），
// 已应用的版本按 community.schema_migrations 跳过，因此重复调用是幂等的、不会改已存在的表。
func (s *Store) Init(ctx context.Context) error { return migrator.Apply(ctx, s.db) }
