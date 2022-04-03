package db

import (
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"time"
)

type Protocol string

const (
	HttpProtocol Protocol = "http"
	TlsProtocol  Protocol = "tls"
)

type Tunnel struct {
	ID            string `gorm:"primaryKey"`
	URL           string
	ClientIP      string
	Protocol      Protocol
	Data          datatypes.JSON
	Metadata      datatypes.JSON
	EstablishedAt time.Time
	Active        bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     gorm.DeletedAt `gorm:"index"`
}
