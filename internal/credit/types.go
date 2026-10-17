// Package credit owns transactional credit state and disposable usage history.
package credit

import (
	"context"
	"errors"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var (
	ErrInvalid      = errors.New("invalid argument")
	ErrUnauthorized = errors.New("invalid or inactive key")
	ErrNotFound     = errors.New("not found")
	ErrDenied       = errors.New("permission denied")
	ErrQuota        = errors.New("quota exceeded")
	ErrConcurrency  = errors.New("concurrency exceeded")
	ErrUnpriced     = errors.New("model has no price")
	ErrConflict     = errors.New("conflicting event")
)

type Options struct {
	DataDir         string
	RetentionMonths int
	Now             func() time.Time
}

type KeySpec struct {
	Label    string `json:"label"`
	Material string `json:"key,omitempty"`
	Enabled  bool   `json:"enabled"`
	// CallerID is an optional attribution to a caller record. It is metadata
	// only: it never changes the key's identity, scope or scheduling.
	CallerID string `json:"caller_id,omitempty"`
	// QuotaMicroUSD, DailyMicroUSD, WeeklyMicroUSD and MonthlyMicroUSD are
	// display-only budget figures. They are stored and reported so an operator
	// can compare a key against its intended budget, but they never gate a
	// request: admission reads prices and amounts nowhere. A zero budget means
	// "no budget recorded", not "unlimited allowance".
	QuotaMicroUSD   int64 `json:"quota_micro_usd"`
	DailyMicroUSD   int64 `json:"daily_micro_usd"`
	WeeklyMicroUSD  int64 `json:"weekly_micro_usd"`
	MonthlyMicroUSD int64 `json:"monthly_micro_usd"`
	// MaxConcurrent and ModelTokenLimits are the only admission limits.
	MaxConcurrent    int64             `json:"max_concurrent"`
	AllowedModels    []string          `json:"allowed_models"`
	ModelTokenLimits []ModelTokenLimit `json:"model_token_limits,omitempty"`
	ExpiresAt        *time.Time        `json:"expires_at,omitempty"`
}

type ModelTokenLimit struct {
	Model   string `json:"model"`
	Total   int64  `json:"total"`
	Daily   int64  `json:"daily"`
	Weekly  int64  `json:"weekly"`
	Monthly int64  `json:"monthly"`
}

type Key struct {
	ID          string  `json:"id"`
	Principal   string  `json:"principal"`
	Scope       string  `json:"scope"`
	Fingerprint string  `json:"fingerprint"`
	Spec        KeySpec `json:"spec"`
	// CallerID is the attribution copied from the spec for convenience.
	CallerID string `json:"caller_id,omitempty"`
	// DeletedAt marks a soft deleted key. A soft deleted key keeps its records,
	// token counters and unresolved requests but can no longer be re-enabled,
	// rotated or authenticated.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type Rates struct {
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	Reasoning  *int64 `json:"reasoning,omitempty"`
	CacheRead  int64  `json:"cache_read"`
	CacheWrite int64  `json:"cache_write"`
}

// Price amounts are integer microUSD per million tokens, not USD floats.
//
// BillingMode selects how a request is billed: the empty string and "token"
// bill the canonical token buckets through Rates; "per_image" bills
// PerImageMicroUSD once per requested image. Optional rate tiers are carried by
// the pricing rule that selects this price (see PricingRule.Tiers), so a price
// value and its tiers can never be duplicated or disagree.
type Price struct {
	Model            string `json:"model"`
	Rates            Rates  `json:"rates"`
	BillingMode      string `json:"billing_mode,omitempty"`
	PerImageMicroUSD int64  `json:"per_image_micro_usd,omitempty"`
	Version          int64  `json:"version"`
}

// PriceTier is an optional rate overlay carried by a pricing rule.
//
// A context tier applies when the canonical prompt size (uncached input plus
// cache read and cache write) reaches Threshold; the highest matching
// threshold wins. A service tier applies when the request's normalized service
// tier is named in the comma separated Service list. A matching service tier
// takes precedence over every context tier. Each rate that is zero (or, for
// reasoning, nil) inherits the base price.
type PriceTier struct {
	Kind      string `json:"kind"`
	Threshold int64  `json:"threshold,omitempty"`
	Service   string `json:"service,omitempty"`
	Label     string `json:"label,omitempty"`
	Rates     Rates  `json:"rates"`
}

type Tokens struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
	Total      int64 `json:"total"`
}

// AdmitInput carries the facts an admission needs to authenticate the key,
// persist the request/key/trace correlation and take a concurrency slot before
// execution starts. It carries no amount, price or token estimate: admission
// only checks permissions and already-consumed effective usage, and nothing in
// it is priced.
type AdmitInput struct {
	ID             string
	TraceID        string
	KeyID          string
	RequestedModel string
	// ServiceTier is the request fact read from the request body (for example
	// "priority"). It is normalized and persisted on the request; a usage
	// event that omits its own tier inherits it. It never gates admission.
	ServiceTier string
	At          time.Time
}

type Usage struct {
	ID            string                `json:"id"`
	RequestID     string                `json:"request_id"`
	TraceID       string                `json:"trace_id"`
	KeyID         string                `json:"key_id"`
	Provider      string                `json:"provider"`
	AuthID        string                `json:"auth_id"`
	AuthIndex     string                `json:"auth_index"`
	Executor      string                `json:"executor"`
	Model         string                `json:"model"`
	ResponseModel string                `json:"response_model,omitempty"`
	Raw           pluginapi.UsageDetail `json:"raw"`
	Tokens        Tokens                `json:"tokens"`
	Quality       string                `json:"quality"`
	// ServiceTier is the normalized service tier under which this execution
	// was reported: the host response tier, else the reported requested tier,
	// else the tier recorded on the request. It is the tier used when a
	// query-time price is applied to these tokens.
	ServiceTier string `json:"service_tier,omitempty"`
	// CostMicroUSD is purely query derived: it is computed from this
	// execution's actual normalized tokens, its exact actual Model and the
	// current price book at read time. It is never persisted, so a price added,
	// edited or deleted after the fact immediately changes it, and it is null
	// (never zero) when the execution cannot be priced: no price, unusable
	// accounting, or a per-image price whose real image count the host does not
	// report.
	CostMicroUSD *int64    `json:"cost_micro_usd"`
	Failed       bool      `json:"failed"`
	StatusCode   int       `json:"status_code,omitempty"`
	At           time.Time `json:"at"`
	LatencyMS    int64     `json:"latency_ms"`
	TTFTMS       int64     `json:"ttft_ms"`
}

type Request struct {
	ID          string     `json:"id"`
	TraceID     string     `json:"trace_id"`
	KeyID       string     `json:"key_id"`
	Model       string     `json:"model"`
	Outcome     string     `json:"outcome"`
	State       string     `json:"state"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	// Executions carry the per-execution query-time cost; the request itself
	// freezes no price, so nothing here is a durable amount.
	Executions []Usage `json:"executions"`
	// ServiceTier is the request-time tier fact reported by the caller. It is
	// not a frozen price input: a usage event that omits its tier inherits it.
	ServiceTier string `json:"service_tier,omitempty"`
}

type Completion struct {
	ID         string
	TraceID    string
	Outcome    string
	StatusCode int
	At         time.Time
}

type Filter struct {
	From     time.Time
	To       time.Time
	KeyID    string
	Model    string
	Provider string
	AuthID   string
	Result   string
	Search   string
	Page     int
	PageSize int
	Bucket   string
}

// Summary is the aggregate of one selection. Requests, executions and counts
// come from SQL aggregation over the selection; CostMicroUSD and
// UnpricedExecutions are derived from the same executions through the current
// price book, so no summary can observe a stale or durable amount.
type Summary struct {
	Requests   int64 `json:"requests"`
	Executions int64 `json:"executions"`
	Failed     int64 `json:"failed"`
	Pending    int64 `json:"pending"`
	// CostMicroUSD is the strict sum of the priced executions only. An
	// execution is priced when its actual Model has a usable price in the
	// current price book and its accounting is trustworthy; every other
	// execution contributes nothing here and is counted by
	// UnpricedExecutions instead. Cost is never zero-filled for an unpriced
	// execution.
	CostMicroUSD int64 `json:"cost_micro_usd"`
	// UnpricedExecutions counts executions of the selection that carry no cost
	// (null), so the dashboard can distinguish "free" (priced zero) from "not
	// priced".
	UnpricedExecutions int64    `json:"unpriced_executions"`
	Tokens             Tokens   `json:"tokens"`
	CompleteExecutions int64    `json:"complete_executions"`
	CacheReadRatio     *float64 `json:"cache_read_ratio,omitempty"`
}

type Point struct {
	Time time.Time `json:"time"`
	Summary
}

type Report struct {
	Summary       Summary   `json:"summary"`
	Trend         []Point   `json:"trend"`
	Items         []Request `json:"items"`
	Total         int64     `json:"total"`
	Page          int       `json:"page"`
	PageSize      int       `json:"page_size"`
	RetentionFrom time.Time `json:"retention_from"`
	// Models is populated only for customer lookup, from the same snapshot.
	Models []ModelMetrics `json:"models,omitempty"`
}

type Maintenance struct {
	Deleted       int64 `json:"deleted"`
	DatabaseBytes int64 `json:"database_bytes"`
	WALBytes      int64 `json:"wal_bytes"`
}

// Backend is the application-facing contract; Store is its SQLite implementation.
type Backend interface {
	Close() error
	// HasActive reports whether any execution is still in flight. Callers use it
	// to refuse disruptive reconfiguration while business requests are running.
	HasActive(context.Context) (bool, error)
	CreateKey(context.Context, KeySpec) (Key, string, error)
	UpdateKey(context.Context, string, KeySpec) (Key, error)
	RotateKey(context.Context, string, string) (Key, string, error)
	ListKeys(context.Context) ([]Key, error)
	GetKey(context.Context, string) (Key, error)
	Authenticate(context.Context, string) (Key, error)
	KeyByScope(context.Context, string) (Key, error)
	KeyByPrincipal(context.Context, string) (Key, error)
	ListPrices(context.Context) ([]Price, error)
	PutPrice(context.Context, Price) error
	Admit(context.Context, AdmitInput) (Request, error)
	RecordUsage(context.Context, Usage) error
	Complete(context.Context, Completion) error
	Report(context.Context, Filter) (Report, error)
	Cleanup(context.Context) (Maintenance, error)
}
