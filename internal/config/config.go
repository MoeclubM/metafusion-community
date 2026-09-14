package config

import (
	"net/url"
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
	// JWKSURL 验签公钥来源：账号服务是令牌的唯一签发方，因此指向它。
	JWKSURL string
	// JWTPublicKeyPEM 静态公钥（PEM 或 base64 PEM）；设置后不再请求 JWKS。
	JWTPublicKeyPEM string
	JWTIssuer       string
	JWTAudience     string
	// AuthURL 账号服务地址，仅用于存量不透明会话令牌的兜底解析（GET /api/auth/me）。
	// 留空即"只接受 JWT"：身份问题只问账号服务，不查任何人的库。
	AuthURL string
	// CatalogURL 元数据目录地址：实体可见性与标题一律问它，本服务不复制目录数据。
	CatalogURL string
	// CatalogTimeout 单次目录调用的超时。
	CatalogTimeout time.Duration
}

func Load() Config {
	c := Config{
		Port:            env("PORT", "8083"),
		DatabaseURL:     env("DATABASE_URL", ""),
		JWKSURL:         env("COMMUNITY_JWKS_URL", "http://auth:8081/api/oidc/jwks"),
		JWTPublicKeyPEM: env("AUTH_JWT_PUBLIC_KEY", ""),
		JWTIssuer:       env("AUTH_JWT_ISSUER", "https://findverse.cc/api"),
		JWTAudience:     env("AUTH_JWT_AUDIENCE", "metafusion"),
		AuthURL:         env("AUTH_URL", ""),
		CatalogURL:      env("CATALOG_URL", "http://backend:8080"),
		CatalogTimeout:  time.Duration(envInt("COMMUNITY_CATALOG_TIMEOUT_MS", 5000)) * time.Millisecond,
	}
	if c.DatabaseURL == "" {
		c.DatabaseURL = buildDSN()
	}
	return c
}

// buildDSN 用 url.URL 拼连接串：口令里的 @ : / ? # 等字符必须转义，
// 直接字符串拼接会在这些字符上拼出非法 DSN（或连错主机）。
func buildDSN() string {
	u := url.URL{
		Scheme: "postgres",
		Host:   env("DB_HOST", "localhost") + ":" + env("DB_PORT", "5432"),
		Path:   env("DB_NAME", "metafusion_db"),
		User:   url.UserPassword(env("DB_USER", "metafusion"), os.Getenv("DB_PASSWORD")),
	}
	q := u.Query()
	q.Set("sslmode", env("DB_SSLMODE", "disable"))
	u.RawQuery = q.Encode()
	return u.String()
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
