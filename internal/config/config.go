package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是互动服务的运行配置，全部来自环境变量。
// 服务只依赖 PostgreSQL 与两个上游：目录（可见性/元信息）与账号（验签公钥）。
type Config struct {
	Port string
	// DatabaseURL 为 PostgreSQL 连接串；为空时由 DB_* 拼装。
	DatabaseURL string
	// JWKSURL 验签公钥来源。账号服务上线前指向目录服务的 /api/oidc/jwks。
	JWKSURL string
	// JWTPublicKeyPEM 静态公钥（PEM 或 base64 PEM）；设置后不再请求 JWKS。
	JWTPublicKeyPEM string
	JWTIssuer       string
	JWTAudience     string
	// CatalogURL 元数据目录地址：实体可见性与标题一律问它，本服务不复制目录数据。
	CatalogURL string
	// CatalogTimeout 单次目录调用的超时。
	CatalogTimeout time.Duration
}

func Load() Config {
	c := Config{
		Port:            env("PORT", "8083"),
		DatabaseURL:     env("DATABASE_URL", ""),
		JWKSURL:         env("COMMUNITY_JWKS_URL", env("STORAGE_JWKS_URL", "http://catalog:8080/api/oidc/jwks")),
		JWTPublicKeyPEM: env("AUTH_JWT_PUBLIC_KEY", ""),
		JWTIssuer:       env("AUTH_JWT_ISSUER", "https://findverse.cc/api"),
		JWTAudience:     env("AUTH_JWT_AUDIENCE", "metafusion"),
		CatalogURL:      env("CATALOG_URL", "http://catalog:8080"),
		CatalogTimeout:  time.Duration(envInt("COMMUNITY_CATALOG_TIMEOUT_MS", 5000)) * time.Millisecond,
	}
	if c.DatabaseURL == "" {
		c.DatabaseURL = buildDSN()
	}
	return c
}

func buildDSN() string {
	host := env("DB_HOST", "localhost")
	port := env("DB_PORT", "5432")
	name := env("DB_NAME", "metafusion_db")
	user := env("DB_USER", "metafusion")
	pass := os.Getenv("DB_PASSWORD")
	ssl := env("DB_SSLMODE", "disable")
	return "postgres://" + user + ":" + pass + "@" + host + ":" + port + "/" + name + "?sslmode=" + ssl
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
