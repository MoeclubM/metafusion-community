// Command migrate 把主仓库 modules schema 里的论坛与互动记录一次性导入 community schema。
//
// 设计要点：
//   - 幂等：全部走 ON CONFLICT DO NOTHING，可重复运行，失败后重跑安全；
//   - 只读旧表：不删除、不修改 modules.*，因此切流前随时可以取消（回滚只需把网关指回单体）；
//   - 顺序：boards → topics → posts → tags → topic_tags → records，满足外键依赖；
//   - 迁移窗口：切流前旧库仍在写入，因此本工具应在切流时再跑一次（第二次只补增量）。
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"database/sql"

	_ "github.com/lib/pq"
)

type step struct {
	name string
	// schema 是旧表所在 schema；留空表示 modules（论坛与记录的老家）。
	schema string
	// required 为 true 表示旧表必须存在；false 表示旧表不存在就跳过（例如历史表）。
	required bool
	sql      string
	after    string
}

var steps = []step{
	{name: "boards", required: true, sql: `INSERT INTO community.boards(code,names,descriptions,color,icon,sort_order,is_enabled,show_in_feed)
		SELECT code,names,descriptions,color,icon,sort_order,is_enabled,show_in_feed FROM modules.forum_boards
		ON CONFLICT (code) DO NOTHING`},
	{name: "topics", required: true, sql: `INSERT INTO community.topics(id,board_code,author_id,author_name,title,body,language,entity_id,is_pinned,is_locked,view_count,reply_count,created_at,updated_at,last_activity_at)
		SELECT id,board_code,author_id,author_name,title,body,language,entity_id,is_pinned,is_locked,view_count,reply_count,created_at,updated_at,last_activity_at FROM modules.forum_topics
		ON CONFLICT (id) DO NOTHING`},
	{name: "posts", required: false, sql: `INSERT INTO community.posts(id,topic_id,author_id,author_name,body,post_number,reply_to_post_number,created_at,updated_at)
		SELECT id,topic_id,author_id,author_name,body,post_number,reply_to_post_number,created_at,updated_at FROM modules.forum_posts
		ON CONFLICT (id) DO NOTHING`},
	{name: "tags", required: false, sql: `INSERT INTO community.tags(id,name,slug) SELECT id,name,slug FROM modules.forum_tags
		ON CONFLICT (id) DO NOTHING`,
		after: `SELECT setval(pg_get_serial_sequence('community.tags','id'), GREATEST(COALESCE((SELECT max(id) FROM community.tags),1),1))`},
	{name: "topic_tags", required: false, sql: `INSERT INTO community.topic_tags(topic_id,tag_id) SELECT topic_id,tag_id FROM modules.forum_topic_tags
		ON CONFLICT DO NOTHING`},
	{name: "records", required: false, sql: `INSERT INTO community.records(owner_id,entity_id,document) SELECT owner_id,entity_id,document FROM modules.records
		ON CONFLICT (owner_id,entity_id) DO NOTHING`},
	// 收藏在目录 schema 里（catalog.favorites），表结构与 community.favorites 逐列一致。
	{name: "favorites", schema: "catalog", required: false, sql: `INSERT INTO community.favorites(user_id,target_type,target_id,created_at)
		SELECT user_id,target_type,target_id,created_at FROM catalog.favorites
		ON CONFLICT (user_id,target_type,target_id) DO NOTHING`},
	// 更早的实体短评表（modules.posts）已被单体并入论坛评论板块；这里对尚未执行的部署补做一次。
	{name: "legacy_posts", required: false, sql: `INSERT INTO community.topics(id,board_code,author_id,author_name,title,body,entity_id,created_at,updated_at,last_activity_at)
		SELECT p.id,'comment',p.author_id,COALESCE(NULLIF(p.author_name,''),'Anonymous'),'',p.body,p.entity_id,p.created_at,p.created_at,p.created_at
		FROM modules.posts p WHERE NOT EXISTS (SELECT 1 FROM community.topics t WHERE t.id = p.id)
		ON CONFLICT (id) DO NOTHING`},
}

func main() {
	dsn := flag.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL 连接串（默认取 DATABASE_URL）")
	dryRun := flag.Bool("dry-run", false, "只统计将要导入的行数，不写入")
	flag.Parse()
	if *dsn == "" {
		log.Fatal("missing -dsn or DATABASE_URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db, err := sql.Open("postgres", *dsn)
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer db.Close()

	for _, s := range steps {
		schema := s.schema
		if schema == "" {
			schema = "modules"
		}
		legacy := schema + "." + legacyTable(s.name)
		exists, err := tableExists(ctx, db, schema, legacyTable(s.name))
		if err != nil {
			log.Fatalf("inspect %s failed: %v", s.name, err)
		}
		if !exists {
			if s.required {
				log.Fatalf("legacy table %s is missing; run this tool before the source system is retired", legacy)
			}
			fmt.Printf("%-12s skip (legacy table %s missing)\n", s.name, legacy)
			continue
		}
		if *dryRun {
			var n int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+legacy).Scan(&n); err != nil {
				log.Fatalf("count %s failed: %v", s.name, err)
			}
			fmt.Printf("%-12s legacy rows in %s: %d\n", s.name, legacy, n)
			continue
		}
		res, err := db.ExecContext(ctx, s.sql)
		if err != nil {
			log.Fatalf("import %s failed: %v", s.name, err)
		}
		affected, _ := res.RowsAffected()
		fmt.Printf("%-12s imported rows: %d\n", s.name, affected)
		if s.after != "" {
			if _, err = db.ExecContext(ctx, s.after); err != nil {
				log.Fatalf("post-step %s failed: %v", s.name, err)
			}
		}
	}
	fmt.Println("完成：旧表未被修改，可重复运行补齐增量。")
}

// legacyTable 把步骤名映射到旧表名（forum_* 前缀与步骤名不同）。
func legacyTable(step string) string {
	switch step {
	case "boards":
		return "forum_boards"
	case "topics":
		return "forum_topics"
	case "posts":
		return "forum_posts"
	case "tags":
		return "forum_tags"
	case "topic_tags":
		return "forum_topic_tags"
	case "legacy_posts":
		return "posts"
	}
	return step
}

func tableExists(ctx context.Context, db *sql.DB, schema, table string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name=$2)", schema, table).Scan(&exists)
	return exists, err
}
