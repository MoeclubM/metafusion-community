package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-community/internal/auth"
	"github.com/MoeclubM/metafusion-community/internal/catalog"
	"github.com/MoeclubM/metafusion-community/internal/config"
	"github.com/MoeclubM/metafusion-community/internal/handler"
	"github.com/MoeclubM/metafusion-community/internal/store"
)

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
	verifier, err := auth.New(cfg)
	if err != nil {
		log.Fatalf("token verifier initialization failed: %v", err)
	}
	// 迁移期兜底：账号服务拆分完成前，浏览器可能持有目录服务签发的不透明会话令牌。
	verifier.SetFallback(cat)

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	r.SetTrustedProxies(nil)
	r.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("X-Frame-Options", "SAMEORIGIN")
		c.Next()
	})

	handler.New(db.DB(), cat, verifier).Register(r)

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "live", "service": "metafusion-community"})
	})
	r.GET("/ready", func(c *gin.Context) {
		check, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := db.DB().PingContext(check); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready", "dependencies": []string{"postgres"}})
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
