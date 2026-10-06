package config

import "testing"

// TRUSTED_PROXIES 必须被真的读到：部署编排与 scripts/check_env_matrix.py 都按这个名字注入/校验，
// 读错了名字的后果是"配了可信代理、服务仍不采信"，按 IP 的限流桶与审计 actor_ip 一起失真。
func TestLoadReadsTrustedProxies(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", " 10.1.0.0/16 , 203.0.113.7 ")
	if got := Load().TrustedProxies; got != "10.1.0.0/16 , 203.0.113.7" {
		t.Fatalf("TrustedProxies = %q", got)
	}
	t.Setenv("TRUSTED_PROXIES", "")
	if got := Load().TrustedProxies; got != "" {
		t.Fatalf("未配置时 TrustedProxies = %q，期望空串（留空由 nettrust 决定保守默认）", got)
	}
}

func TestLoadRequiresExplicitDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("DB_HOST", "db.internal")
	t.Setenv("DB_PORT", "6543")
	t.Setenv("DB_NAME", "metafusion_db")
	t.Setenv("DB_USER", "metafusion")
	t.Setenv("DB_PASSWORD", "p@ss:w/rd?x#1")
	t.Setenv("DB_SSLMODE", "require")

	if Load().DatabaseURL != "" {
		t.Fatal("缺少 DATABASE_URL 时不得从 DB_* 构造连接串")
	}
	const dsn = "postgres://mf_community_app:test-only@db.internal:6543/metafusion_community?sslmode=require"
	t.Setenv("DATABASE_URL", "  "+dsn+"  ")
	if Load().DatabaseURL != dsn {
		t.Fatal("DATABASE_URL 必须直接使用显式本域连接串")
	}
}
