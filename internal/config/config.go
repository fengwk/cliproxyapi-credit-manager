// Package config defines the single CPA-owned plugin configuration.
package config

import (
	"bytes"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/capture"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled            *bool          `yaml:"enabled" json:"-"`
	Priority           int            `yaml:"priority" json:"-"`
	DataDir            string         `yaml:"data_dir" json:"data_dir"`
	RetentionMonths    int            `yaml:"-" json:"retention_months"`
	MaxPendingRequests int            `yaml:"max_pending_requests" json:"max_pending_requests"`
	History            History        `yaml:"history" json:"history"`
	Capture            capture.Config `yaml:"request_capture" json:"request_capture"`
}

type History struct {
	RetentionMonths int `yaml:"retention_months" json:"retention_months"`
}

func Parse(raw []byte) (Config, error) {
	c := Config{
		DataDir: "./data/cliproxyapi-credit-manager", MaxPendingRequests: 16,
		History: History{RetentionMonths: 3},
		Capture: capture.Config{
			RetentionText: "24h", MaxBodyBytes: 10 << 20, MaxStorageBytes: 1 << 30,
		},
	}
	if len(bytes.TrimSpace(raw)) != 0 {
		d := yaml.NewDecoder(bytes.NewReader(raw))
		d.KnownFields(true)
		if err := d.Decode(&c); err != nil {
			return Config{}, fmt.Errorf("invalid plugin configuration: %w", err)
		}
	}
	if c.DataDir == "" || c.History.RetentionMonths < 1 || c.History.RetentionMonths > 1200 ||
		c.MaxPendingRequests < 1 || c.MaxPendingRequests > 10000 {
		return Config{}, fmt.Errorf("data_dir, retention_months (1..1200), and max_pending_requests (1..10000) are required")
	}
	var err error
	c.DataDir, err = filepath.Abs(c.DataDir)
	if err != nil {
		return Config{}, fmt.Errorf("resolve data directory: %w", err)
	}
	c.RetentionMonths = c.History.RetentionMonths
	c.Capture.Retention, err = time.ParseDuration(c.Capture.RetentionText)
	if err != nil || c.Capture.Retention <= 0 {
		return Config{}, fmt.Errorf("request_capture.retention must be a positive Go duration")
	}
	if c.Capture.MaxBodyBytes < 1 || c.Capture.MaxBodyBytes > 100<<20 ||
		c.Capture.MaxStorageBytes < 1<<20 || c.Capture.MaxStorageBytes > 1<<40 {
		return Config{}, fmt.Errorf("capture body limit must be 1..100MiB and storage limit 1MiB..1TiB")
	}
	return c, nil
}
