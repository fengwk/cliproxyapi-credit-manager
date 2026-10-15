// Package capture stores bounded diagnostic snapshots independently of money.
package capture

import (
	"net/http"
	"time"
)

type Config struct {
	Enabled         bool          `yaml:"enabled" json:"enabled"`
	Retention       time.Duration `yaml:"-" json:"-"`
	RetentionText   string        `yaml:"retention" json:"retention"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes" json:"max_body_bytes"`
	MaxStorageBytes int64         `yaml:"max_storage_bytes" json:"max_storage_bytes"`
}

type Snapshot struct {
	RequestID string
	Headers   http.Header
	Body      []byte
	At        time.Time
}

type Detail struct {
	RequestID string      `json:"request_id"`
	Status    string      `json:"status"`
	Headers   http.Header `json:"headers,omitempty"`
	Body      string      `json:"body,omitempty"`
	At        time.Time   `json:"at"`
	ExpiresAt time.Time   `json:"expires_at"`
}

type Stats struct {
	DatabaseBytes int64  `json:"database_bytes"`
	WALBytes      int64  `json:"wal_bytes"`
	QueueBytes    int64  `json:"queue_bytes"`
	Dropped       int64  `json:"dropped"`
	LastError     string `json:"last_error,omitempty"`
}
