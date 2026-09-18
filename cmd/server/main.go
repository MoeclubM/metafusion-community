package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/config"
	"github.com/MoeclubM/metafusion-community/internal/handler"
	"github.com/MoeclubM/metafusion-community/internal/nettrust"
	"github.com/MoeclubM/metafusion-community/internal/store"
	"github.com/MoeclubM/metafusion-community/internal/upstream"
)

// deepProbeBudget 是 /ready?deep=1 的总预算：它是给人看的诊断端点，不能被单个上游拖成慢探针。
const deepProbeBudget = 3 * time.Second

// upstreamReadyURL 是上游的探活地址；地址未配置时返回空串（ProbeAll 记 not_configured，
// 那是部署态而不是故障——深探针不该因为"这个上游还没部署"就报 degraded）。
func upstreamReadyURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	return base + "/ready"
}

func main() {
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("community database connection failed: %v", err)
	}
	defer db.Close()
	if err = db.Init(ctx); err != nil {
		log.Fatalf("community schema initialization failed: %v", err)
	}
	// 板块是运营配置：首次运行播种，已存在的不覆盖，保留后台调整。
	if err = handler.Seed(ctx, db.DB()); err != nil {
		log.Fatalf("community board seeding failed: %v", err)
	}

	cat := catalog.New(cfg.CatalogURL, cfg.CatalogTimeout)
	// 站内通知的投递密钥（目录侧 INTERNAL_API_TOKEN）。未配置时通知不发但评论照常成功：
	// 启动日志必须说清是哪一种状态，否则"通知为什么没来"只能靠读源码猜。
	cat.SetInternalToken(cfg.InternalAPIToken)
	if cat.NotificationsConfigured() {
		log.Print("cross-service notification delivery is enabled (POST " + cfg.CatalogURL + "/api/notifications/internal)")
	} else {
		log.Print("INTERNAL_API_TOKEN is not configured: comment replies will not produce in-app notifications")
	}
	verifier, err := auth.New(cfg)
	if err != nil {
		log.Fatalf("token verifier initialization failed: %v", err)
	}
	// 存量兜底：浏览器可能还持有登录时的不透明会话令牌（非 JWT）。身份只能问账号服务，
	// 因此兜底指向 AUTH_URL；未配置时退化为"只接受 JWT"（fail closed），不会静默放行。
	// PAT（mfp_ 前缀）走内省端点 POST /api/auth/tokens/introspect，结果进程内缓存 60 秒
	// （= 吊销窗口），见 internal/auth/pat.go。内省器无论 AUTH_URL 是否配置都注入：
	// 未配置时它 Enabled()==false，带 mfp_ 的请求一律 503 auth_unavailable（与不注入同一条路径），
	// 同时它也是 /ready?deep=1 探账号服务的执行器。
	pat := auth.NewPATIntrospector(cfg.AuthURL)
	verifier.SetPAT(pat)
	if cfg.AuthURL != "" {
		verifier.SetFallback(auth.NewSessionClient(cfg.AuthURL))
	} else {
		log.Print("AUTH_URL is not configured: personal access tokens (mfp_ prefix) will be rejected with 503 auth_unavailable")
	}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	// 真实客户端 IP：只信任显式声明的来源（TRUSTED_PROXIES，默认回环 + RFC1918 私网 =
	// 网关容器所在网段）。配置非法就拒绝启动——静默退回"谁都不信"会让 ClientIP() 恒等于
	// 网关地址，审计 actor_ip 与按 IP 的限流桶一起失真，而那种退化在功能上"看起来正常"。
	trustedProxies, err := nettrust.Apply(r, cfg.TrustedProxies)
	if err != nil {
		log.Fatalf("community trusted proxy configuration failed: %v", err)
	}
	log.Printf("community trusted proxies in effect: %s", trustedProxies)
	r.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("X-Frame-Options", "SAMEORIGIN")
		// 切流自检用：响应头标明是哪个服务答复的，便于确认网关把前缀切到了目标上游。
		c.Header("X-MetaFusion-Service", "metafusion-community")
		c.Next()
	})

	h := handler.New(db, cat, verifier)
	h.Register(r)
	// 退出时排空审计队列：defer 在 http.Server.Shutdown 之后执行（在途请求都已收尾），
	// 此时关闭不会丢行；defer 顺序在 db.Close() 之前，队列排空时连接还在。
	defer h.Close()

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "live", "service": "metafusion-community"})
	})
	// /ready 是编排器的健康判据：浅探针只探 PG（毫秒级），deep=1 才并发探两个上游的 /ready。
	// 深探针用请求路径上同一份执行器，探测结果因此也喂给同一个熔断器。
	r.GET("/ready", func(c *gin.Context) {
		check, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := db.DB().PingContext(check); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
			return
		}
		if c.Query("deep") != "1" {
			c.JSON(http.StatusOK, gin.H{"status": "ready", "dependencies": []string{"postgres"}})
			return
		}
		results := upstream.ProbeAll(c.Request.Context(), deepProbeBudget, []upstream.ProbeTarget{
			{Client: cat.Upstream(), URL: upstreamReadyURL(cfg.CatalogURL)},
			{Client: pat.Upstream(), URL: upstreamReadyURL(cfg.AuthURL)},
		})
		degraded := false
		for _, res := range results {
			// 未配置地址（not_configured）是部署态，不是故障：它不该让深探针回 503。
			if res.Status != upstream.ProbeReady && res.Reason != upstream.ReasonNotConfigured {
				degraded = true
			}
		}
		body := gin.H{"status": "ready", "dependencies": []string{"postgres"}, "upstreams": results}
		if degraded {
			body["status"] = "degraded"
			c.JSON(http.StatusServiceUnavailable, body)
			return
		}
		c.JSON(http.StatusOK, body)
	})

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	done := make(chan error, 1)
	go func() {
		log.Print("MetaFusion community service ready")
		done <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Fatalf("server shutdown error: %v", err)
		}
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		log.Fatalf("server error: %v", err)
	}
}
