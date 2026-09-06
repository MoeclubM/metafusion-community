package models

import (
    "time"
    "github.com/google/uuid"
)

type Category struct {
    ID          uint      `gorm:"primaryKey" json:"id"`
    Name        string    `gorm:"size:64;not null" json:"name"`
    Slug        string    `gorm:"size:64;uniqueIndex;not null" json:"slug"`
    Description string    `gorm:"size:255" json:"description"`
    SortOrder   int       `gorm:"default:0" json:"sort_order"`
}

type Thread struct {
    ID             uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
    CategoryID     uint       `gorm:"index;not null" json:"category_id"`
    TargetEntityID *uuid.UUID `gorm:"type:uuid;index" json:"target_entity_id,omitempty"`
    AuthorID       uuid.UUID  `gorm:"type:uuid;index;not null" json:"author_id"`
    Title          string     `gorm:"size:255;not null" json:"title"`
    ViewCount      int        `gorm:"default:0" json:"view_count"`
    PostCount      int        `gorm:"default:0" json:"post_count"`
    CreatedAt      time.Time  `json:"created_at"`
    UpdatedAt      time.Time  `json:"updated_at"`
}

type Post struct {
    ID        uuid.UUID `gorm:"type:uuid;primaryKey" json:"id"`
    ThreadID  uuid.UUID `gorm:"type:uuid;index;not null" json:"thread_id"`
    AuthorID  uuid.UUID `gorm:"type:uuid;index;not null" json:"author_id"`
    Content   string    `gorm:"type:text;not null" json:"content"`
    Floor     int       `gorm:"not null" json:"floor"`
    CreatedAt time.Time `json:"created_at"`
    UpdatedAt time.Time `json:"updated_at"`
}
