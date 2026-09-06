package main

import (
    "log"
    "os"
    "github.com/gin-gonic/gin"
    "github.com/MoeclubM/metafusion-community/internal/handler"
)

func main() {
    port := os.Getenv("PORT")
    if port == "" {
        port = "8083"
    }

    r := gin.Default()

    r.GET("/health", func(c *gin.Context) {
        c.JSON(200, gin.H{"status": "ok", "service": "metafusion-community"})
    })

    api := r.Group("/api/community")
    {
        api.GET("/categories", handler.ListCategories)
        api.GET("/threads", handler.ListThreads)
        api.POST("/threads", handler.CreateThread)
        api.GET("/threads/:id", handler.GetThread)
        api.POST("/threads/:id/posts", handler.CreatePost)
        api.GET("/entities/:id/threads", handler.GetEntityThreads)
        api.POST("/entities/:id/rate", handler.RateEntity)
    }

    log.Printf("MetaFusion Community Service listening on port %s", port)
    if err := r.Run(":" + port); err != nil {
        log.Fatalf("Failed to run server: %v", err)
    }
}
