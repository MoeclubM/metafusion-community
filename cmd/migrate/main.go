// Command migrate 在互动服务的 community schema 与主仓库的旧表之间做一次性数据搬运。
//
// 两个方向都要有，切流才是真的可回滚：
//
//	forward（默认）主仓库 → 互动服务：切流前把 modules.forum_* / modules.records / catalog.favorites
//	                 搬到 community.*，切流后互动服务成为唯一写入方；
//	back            互动服务 → 主仓库：切流后若要回退，先把互动服务这几小时写入的行搬回去，
//	                 再把网关指回单体，避免"回滚后新数据看不见"。
//
// 设计要点：
//   - 幂等：全部 ON CONFLICT DO NOTHING，可重复运行，失败后重跑安全；
//   - 只读源表：不删除、不修改源数据，因此两个方向都可以随时取消；
//   - 顺序满足外键依赖（boards → topics → posts → tags → topic_tags）；
//   - 搬运本身不停止写入，因此必须在切换前（forward）或切换后立刻（back）执行，
//     并配合切流窗口使用：切流那一刻起只有一侧在写。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"time"

	_ "github.com/lib/pq"

	"github.com/MoeclubM/metafusion-community/internal/config"
)

// step 描述一步搬运：源 schema + 源表 + 目标 SQL。
type step struct {
	name string
	// schema 是**源**表所在 schema；留空表示 modules（论坛与记录的老家）。
	schema string
	// table 是源表名；留空表示与 name 同名（反向搬运时生效）。
	table string
	sql   string
	after string
}

const setTagsSeq = `SELECT setval(pg_get_serial_sequence('%s.tags','id'), GREATEST(COALESCE((SELECT max(id) FROM %s.tags),1),1))`

// forwardSteps：主仓库 → 互动服务。目标表名不变，只有 schema 变化；
// 唯一跨 schema 的源是收藏（主仓库把它放在 catalog schema 里）。
var forwardSteps = []step{
	{name: "boards", table: "forum_boards", sql: `INSERT INTO community.boards(code,names,descriptions,color,icon,sort_order,is_enabled,show_in_feed)
		SELECT code,names,descriptions,color,icon,sort_order,is_enabled,show_in_feed FROM modules.forum_boards
		ON CONFLICT (code) DO NOTHING`},
	{name: "topics", table: "forum_topics", sql: `INSERT INTO community.topics(id,board_code,author_id,author_name,title,body,language,entity_id,is_pinned,is_locked,view_count,reply_count,created_at,updated_at,last_activity_at)
		SELECT id,board_code,author_id,author_name,title,body,language,entity_id,is_pinned,is_locked,view_count,reply_count,created_at,updated_at,last_activity_at FROM modules.forum_topics
		ON CONFLICT (id) DO NOTHING`},
	{name: "posts", table: "forum_posts", sql: `INSERT INTO community.posts(id,topic_id,author_id,author_name,body,post_number,reply_to_post_number,created_at,updated_at)
		SELECT id,topic_id,author_id,author_name,body,post_number,reply_to_post_number,created_at,updated_at FROM modules.forum_posts
		ON CONFLICT (id) DO NOTHING`},
	{name: "tags", table: "forum_tags", sql: `INSERT INTO community.tags(id,name,slug) SELECT id,name,slug FROM modules.forum_tags
		ON CONFLICT (id) DO NOTHING`,
		after: fmt.Sprintf(setTagsSeq, "community", "community")},
	{name: "topic_tags", table: "forum_topic_tags", sql: `INSERT INTO community.topic_tags(topic_id,tag_id) SELECT topic_id,tag_id FROM modules.forum_topic_tags
		ON CONFLICT DO NOTHING`},
	{name: "records", sql: `INSERT INTO community.records(owner_id,entity_id,document) SELECT owner_id,entity_id,document FROM modules.records
		ON CONFLICT (owner_id,entity_id) DO NOTHING`},
	{name: "favorites", schema: "catalog", sql: `INSERT INTO community.favorites(user_id,target_type,target_id,created_at)
		SELECT user_id,target_type,target_id,created_at FROM catalog.favorites
		ON CONFLICT (user_id,target_type,target_id) DO NOTHING`},
	// 更早的实体短评表（modules.posts）已被单体并入论坛评论板块；这里对尚未执行的部署补做一次。
	{name: "legacy_posts", table: "posts", sql: `INSERT INTO community.topics(id,board_code,author_id,author_name,title,body,entity_id,created_at,updated_at,last_activity_at)
		SELECT p.id,'comment',p.author_id,COALESCE(NULLIF(p.author_name,''),'Anonymous'),'',p.body,p.entity_id,p.created_at,p.created_at,p.created_at
		FROM modules.posts p WHERE NOT EXISTS (SELECT 1 FROM community.topics t WHERE t.id = p.id)
		ON CONFLICT (id) DO NOTHING`},
}

// backSteps：互动服务 → 主仓库（回滚用）。顺序与外键一致，源表都在 community schema。
var backSteps = []step{
	{name: "boards", schema: "community", sql: `INSERT INTO modules.forum_boards(code,names,descriptions,color,icon,sort_order,is_enabled,show_in_feed)
		SELECT code,names,descriptions,color,icon,sort_order,is_enabled,show_in_feed FROM community.boards
		ON CONFLICT (code) DO NOTHING`},
	{name: "topics", schema: "community", sql: `INSERT INTO modules.forum_topics(id,board_code,author_id,author_name,title,body,language,entity_id,is_pinned,is_locked,view_count,reply_count,created_at,updated_at,last_activity_at)
		SELECT id,board_code,author_id,author_name,title,body,language,entity_id,is_pinned,is_locked,view_count,reply_count,created_at,updated_at,last_activity_at FROM community.topics
		ON CONFLICT (id) DO NOTHING`},
	{name: "posts", schema: "community", sql: `INSERT INTO modules.forum_posts(id,topic_id,author_id,author_name,body,post_number,reply_to_post_number,created_at,updated_at)
		SELECT id,topic_id,author_id,author_name,body,post_number,reply_to_post_number,created_at,updated_at FROM community.posts
		ON CONFLICT (id) DO NOTHING`},
	{name: "tags", schema: "community", sql: `INSERT INTO modules.forum_tags(id,name,slug) SELECT id,name,slug FROM community.tags
		ON CONFLICT (id) DO NOTHING`,
		after: fmt.Sprintf(setTagsSeq, "modules", "modules")},
	{name: "topic_tags", schema: "community", sql: `INSERT INTO modules.forum_topic_tags(topic_id,tag_id) SELECT topic_id,tag_id FROM community.topic_tags
		ON CONFLICT DO NOTHING`},
	{name: "records", schema: "community", sql: `INSERT INTO modules.records(owner_id,entity_id,document) SELECT owner_id,entity_id,document FROM community.records
		ON CONFLICT (owner_id,entity_id) DO NOTHING`},
	{name: "favorites", schema: "community", sql: `INSERT INTO catalog.favorites(user_id,target_type,target_id,created_at)
		SELECT user_id,target_type,target_id,created_at FROM community.favorites
		ON CONFLICT (user_id,target_type,target_id) DO NOTHING`},
}

func main() {
	// 默认连接串与常驻服务同一条装配路径（config.Load：先 DATABASE_URL，再用 DB_* 拼），
	// 因此编排里只给 DB_* 就能跑，不需要为这个一次性工具额外配一份连接串。
	dsn := flag.String("dsn", config.Load().DatabaseURL, "PostgreSQL 连接串（默认取 DATABASE_URL，其次用 DB_* 拼装）")
	direction := flag.String("direction", "forward", "forward=主仓库→互动服务；back=互动服务→主仓库（回滚）")
	dryRun := flag.Bool("dry-run", false, "只统计将要搬运的行数，不写入")
	flag.Parse()
	if *dsn == "" {
		log.Fatal("missing -dsn or DATABASE_URL")
	}
	var steps []step
	switch *direction {
	case "forward":
		steps = forwardSteps
	case "back":
		steps = backSteps
	default:
		log.Fatalf("unknown -direction %q (want forward|back)", *direction)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	db, err := sql.Open("postgres", *dsn)
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}
	defer db.Close()

	fmt.Printf("direction=%s\n", *direction)
	for _, s := range steps {
		schema := s.schema
		if schema == "" {
			schema = "modules"
		}
		table := s.table
		if table == "" {
			table = s.name
		}
		source := schema + "." + table
		exists, err := tableExists(ctx, db, schema, table)
		if err != nil {
			log.Fatalf("inspect %s failed: %v", source, err)
		}
		if !exists {
			fmt.Printf("%-12s skip (source %s missing)\n", s.name, source)
			continue
		}
		if *dryRun {
			var n int
			if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+source).Scan(&n); err != nil {
				log.Fatalf("count %s failed: %v", source, err)
			}
			fmt.Printf("%-12s source %s rows: %d\n", s.name, source, n)
			continue
		}
		res, err := db.ExecContext(ctx, s.sql)
		if err != nil {
			log.Fatalf("move %s failed: %v", s.name, err)
		}
		affected, _ := res.RowsAffected()
		fmt.Printf("%-12s copied rows: %d\n", s.name, affected)
		if s.after != "" {
			if _, err = db.ExecContext(ctx, s.after); err != nil {
				log.Fatalf("post-step %s failed: %v", s.name, err)
			}
		}
	}
	fmt.Println("done: source tables untouched; rerun to pick up the delta.")
}

func tableExists(ctx context.Context, db *sql.DB, schema, table string) (bool, error) {
	var exists bool
	err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=$1 AND table_name=$2)", schema, table).Scan(&exists)
	return exists, err
}
