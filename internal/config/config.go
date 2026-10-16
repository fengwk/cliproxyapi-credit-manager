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
	Enabled            *bool          `yaml:"enabled" json:"-"`
	Priority           int            `yaml:"priority" json:"-"`
	Store              yaml.Node      `yaml:"store" json:"-"`
	DataDir            string         `yaml:"data_dir" json:"data_dir"`
	RetentionMonths    int            `yaml:"-" json:"retention_months"`
	MaxPendingRequests int            `yaml:"max_pending_requests" json:"max_pending_requests"`
	History            History        `yaml:"history" json:"history"`
	Capture            capture.Config `yaml:"request_capture" json:"request_capture"`
	Warmup             Warmup         `yaml:"warmup" json:"warmup"`
}

type History struct {
	RetentionMonths int `yaml:"retention_months" json:"retention_months"`
}

// Warmup configures the optional background account warmup. It is disabled by
// default: with Enabled false, or with no targets, the plugin issues no
// requests at all.
//
// Every value here is plugin-local configuration. An upstream credential is
// referenced only by its CPA credential id; no credential material is stored,
// and no scheduling state (holds, caps, cooldowns) is expressed here.
type Warmup struct {
	Enabled  bool           `yaml:"enabled" json:"enabled"`
	Interval string         `yaml:"interval" json:"interval"`
	Targets  []WarmupTarget `yaml:"targets" json:"targets"`

	// Period is the parsed form of Interval, used by the background worker.
	Period time.Duration `yaml:"-" json:"-"`
}

// WarmupTarget pins one warmup probe to an exact credential, provider and
// request protocol. Both AuthID and Provider are mandatory: a warmup request
// must never fall back to host-side credential or provider selection, because
// that could consume or disturb an account it was not aimed at.
type WarmupTarget struct {
	AuthID   string `yaml:"auth_id" json:"auth_id"`
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
	// Protocol is the request protocol: openai, claude or gemini. Empty means
	// openai.
	Protocol string `yaml:"protocol" json:"protocol"`
}

// defaultWarmupInterval is the warmup period used when none is configured.
const defaultWarmupInterval = "1h"

// warmupProtocols are the request protocols a warmup target may declare.
var warmupProtocols = map[string]struct{}{"openai": {}, "claude": {}, "gemini": {}}

func Parse(raw []byte) (Config, error) {
	c := Config{
		DataDir: "./data/cliproxyapi-credit-manager", MaxPendingRequests: 16,
		History: History{RetentionMonths: 3},
		Capture: capture.Config{
			RetentionText: "24h", MaxBodyBytes: 10 << 20, MaxStorageBytes: 1 << 30,
		},
		Warmup: Warmup{Interval: defaultWarmupInterval},
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
		c.History.RetentionMonths < 1 || c.History.RetentionMonths > 1200 ||
		c.MaxPendingRequests < 1 || c.MaxPendingRequests > 10000 {
		return Config{}, fmt.Errorf("data_dir must be a non-empty path without leading or trailing whitespace, and " +
			"retention_months (1..1200) and max_pending_requests (1..10000) are required")
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
	// Warmup is validated even when disabled so a typo is surfaced at
	// configuration time instead of at the first probe. An enabled warmup with
	// no targets is valid and simply issues no requests.
	interval := strings.TrimSpace(c.Warmup.Interval)
	if interval == "" {
		interval = defaultWarmupInterval
	}
	c.Warmup.Interval = interval
	if c.Warmup.Period, err = time.ParseDuration(interval); err != nil || c.Warmup.Period <= 0 {
		return Config{}, fmt.Errorf("warmup.interval must be a positive Go duration")
	}
	for i := range c.Warmup.Targets {
		target := &c.Warmup.Targets[i]
		target.AuthID = strings.TrimSpace(target.AuthID)
		target.Provider = strings.TrimSpace(target.Provider)
		target.Model = strings.TrimSpace(target.Model)
		target.Protocol = strings.ToLower(strings.TrimSpace(target.Protocol))
		if target.Protocol == "" {
			target.Protocol = "openai"
		}
		if _, ok := warmupProtocols[target.Protocol]; !ok {
			return Config{}, fmt.Errorf("warmup.targets.protocol must be openai, claude or gemini")
		}
		if target.AuthID == "" || target.Provider == "" || target.Model == "" {
			return Config{}, fmt.Errorf("warmup.targets require auth_id, provider and model")
		}
	}
	return c, nil
}
