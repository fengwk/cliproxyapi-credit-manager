// Package config defines the single CPA-owned plugin configuration.
package config

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/capture"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled         *bool          `yaml:"enabled" json:"-"`
	Priority        int            `yaml:"priority" json:"-"`
	Store           yaml.Node      `yaml:"store" json:"-"`
	DataDir         string         `yaml:"data_dir" json:"data_dir"`
	RetentionMonths int            `yaml:"-" json:"retention_months"`
	History         History        `yaml:"history" json:"history"`
	Capture         capture.Config `yaml:"request_capture" json:"request_capture"`
}

type History struct {
	RetentionMonths int `yaml:"retention_months" json:"retention_months"`
}

// retiredSettings are configuration keys this plugin no longer supports. The
// removed background warmup and pending-request backlog gate must be refused
// with a clear error instead of being silently ignored, which would leave an
// operator believing the retired behavior is still active.
var retiredSettings = []string{"warmup", "max_pending_requests"}

// rejectRetiredSettings refuses any configuration document that still carries a
// retired top-level key. A document that does not parse as a mapping is left to
// the strict decoder so the operator sees the syntax error instead.
func rejectRetiredSettings(raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var document map[string]yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil
	}
	for _, key := range retiredSettings {
		if _, present := document[key]; present {
			return fmt.Errorf("invalid plugin configuration: %s is no longer supported", key)
		}
	}
	return nil
}

func Parse(raw []byte) (Config, error) {
	c := Config{
		DataDir: "./data/cliproxyapi-credit-manager",
		History: History{RetentionMonths: 3},
		Capture: capture.Config{
			RetentionText: "24h", MaxBodyBytes: 10 << 20, MaxStorageBytes: 1 << 30,
		},
	}
	if err := rejectRetiredSettings(raw); err != nil {
		return Config{}, err
	}
	if len(bytes.TrimSpace(raw)) != 0 {
		d := yaml.NewDecoder(bytes.NewReader(raw))
		d.KnownFields(true)
		if err := d.Decode(&c); err != nil {
			return Config{}, fmt.Errorf("invalid plugin configuration: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return Config{}, fmt.Errorf("plugin configuration must contain one YAML document")
		}
	}
	// data_dir is resolved and used verbatim: a padded value would resolve to a
	// different directory than the health and capture paths point at, so leading
	// or trailing whitespace is refused rather than trimmed.
	if c.DataDir == "" || strings.TrimSpace(c.DataDir) != c.DataDir ||
		c.History.RetentionMonths < 1 || c.History.RetentionMonths > 1200 {
		return Config{}, fmt.Errorf("data_dir must be a non-empty path without leading or trailing whitespace, and " +
			"retention_months (1..1200) is required")
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
