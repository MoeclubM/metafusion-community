package handler

import (
    "net/http"
    "github.com/gin-gonic/gin"
)

func ListCategories(c *gin.Context) {
    c.JSON(http.StatusOK, gin.H{"categories": []any{}})
}

func ListThreads(c *gin.Context) {
    c.JSON(http.StatusOK, gin.H{"threads": []any{}, "total": 0})
}

func CreateThread(c *gin.Context) {
    c.JSON(http.StatusNotImplemented, gin.H{"message": "Community service scaffold ready"})
}

func GetThread(c *gin.Context) {
    c.JSON(http.StatusNotFound, gin.H{"error": "thread_not_found"})
}

func CreatePost(c *gin.Context) {
    c.JSON(http.StatusNotImplemented, gin.H{"message": "Community service scaffold ready"})
}

func GetEntityThreads(c *gin.Context) {
    c.JSON(http.StatusOK, gin.H{"threads": []any{}, "target_entity_id": c.Param("id")})
}

func RateEntity(c *gin.Context) {
    c.JSON(http.StatusOK, gin.H{"status": "rated"})
}
