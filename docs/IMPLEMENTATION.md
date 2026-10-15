# Initial implementation contract

Module: github.com/fengwk/cliproxyapi-credit-manager. CPA v8.0.30, Go 1.26.
No compatibility with the prior implementation. No CPA/provider edits.

## Ownership and boundaries

- `credit/`: one SQLite-backed transactional accounting package. `types.go`
  defines the shared API. Implement `Open(Options) (*Store, error)` and Backend.
- `capture/`: separate disposable SQLite, bounded async gzip snapshots.
  Implement `Open(dataDir string, cfg Config) (*Store,error)`, `Close() error`,
  `Enqueue(Snapshot)`, `Get(string) (Detail,error)`, `Cleanup() error`,
  `Stats() Stats`. UTC times; immutable snapshots; sanitize before queueing.
- `plugin/`, `main.go`: native ABI adapter. Depends on config, credit, capture,
  web. `NewManager()` and `HandleCall(method string, raw []byte)([]byte,error)`.
  Constructor must not open files. No Executor/Router/Scheduler.
- `web/`: backend `New(credit.Backend,*capture.Store,config.Config) *Handler`;
  `Handle(pluginapi.ManagementRequest) pluginapi.ManagementResponse`.
  `Asset(path string)([]byte,string,bool)` embeds frontend files.
- `config/`: `Parse([]byte)(Config,error)`, Config has DataDir,
  RetentionMonths, MaxPendingRequests, Capture (capture.Config).
- Frontend writes only `internal/web/assets/` and `internal/web/assets_test.go`.

## Credit semantics

Key material HMAC verified, private pepper persisted mode0600. Internal Principal
`cmk:<UUID>` and SDK session.CallerScope stable. Material omitted from all
stored/returned KeySpec objects. Generate sk- plus 48 unbiased alphanumerics.
0 monetary limit/concurrency means unlimited. Enabled explicit true/false.
AllowedModels empty=all, otherwise exact IDs (no fuzzy/slash-prefix inference).
ModelTokenLimits exact requested IDs, 0=unlimited. Durable lifetime/UTC period
token counters plus admission estimates must survive history cleanup too.
Prices exact requested model; explicit all-zero rates mean free; absence denies.
Input is canonical UNCACHED input; Output excludes Reasoning. Reasoning nil rate
uses Output. Integer cost rounds up once over the sum of canonical buckets.
Raw tokens, normalization quality and price snapshot retained. Unknown accounting
does not settle as zero. Protect overflow.

Reserve is idempotent by lifecycle ID, conflicting IDs/traces rejected; persist
trace+key before execution. Lifetime and UTC day/week/month counters independent
of historical rows, charged to request admission period. One execution UUID per
event, unique persistent dedup. Same UUID/different material content conflicts.
Usage arrives before/after completion. Complete releases execution concurrency
once; known complete usages charge incrementally, monetary hold remains until
completion with at least one reliable success (or proven rejection before any
execution). Unknown/failed-zero/no event => pending not fake zero billing.
Outstanding pending per Key is bounded. Restart marks unfinished execution
stopped/pending, not free. Administrator Resolve accepts release or charge with a
mandatory reason; ledger adjustments append, no silent reprice.
Only associated requests may accept usage; after retention purge, unknown old
events cannot resurrect requests. Unresolved requests retain necessary rows.
Cleanup default 3 calendar months UTC (clamp day at month end), batch deletes
closed history, keeps durable counters. incremental_vacuum + WAL TRUNCATE; busy
must be observable. One DB connection, bounded transactions and no external MQ.

## Capture

Execution-entry snapshot, NOT final upstream HTTP/network-raw bytes.
Disabled default. Retention24h, uncompressed10MiB, physical1GiB incl WAL+indexes.
Queue bounded in BYTES, skip oversize before copying; never mutate forwarded data.
Redact Authorization/X-Api-Key/X-Goog-Api-Key/Cookie/Set-Cookie and obvious secret
JSON fields recursively. Metadata small and bounded. Get missing returns status
unavailable (do not claim definitely expired without evidence); disabled returns
disabled. gzip size not TTL and not SQLite compaction. Storage-budget headroom,
evict early or pause; no periodic full VACUUM. Enqueue failure never inference
failure. Tests must check actual DB+WAL shrinking and pressure/busy cases.

## Native integration

RPC schema6/ABI1. exclusive frontend auth, RequestInterceptor+Lifecycle+Usage,
Management+resource routes only. CPA candidates first valid in native order.
Caller_scope trusted SDK metadata => internal Key; NEVER reparse Bearer to bill.
Full TraceID connects lifecycle and usage execution IDs. Input estimate is raw
request byte length (conservative estimate only), output estimate uses declared
max token fields or4096. Never modify execution body/headers/selection.
Normalize standard SDK tokens; exact commandcode/opencode-go executorAdapter
cohorts use Observer's strict complete unique normalization (copy with license).
Usage.APIKey is principal, AccessProvider must belong to plugin. ResponseModel
only actual record.ResponseModel, never record.Model fallback. No guessed joins.
Maintenance failures sticky health; finance failure blocks new Reserve, usage
errors observable. Source internal/warmup must not attach to customer records.
Warmup deferred until core verified; don't advertise if absent.

## Management HTTP contract

Base `/v0/management/plugins/credit-manager`, all API paths management-authenticated.
Resources `/v0/resource/plugins/credit-manager/` serve public static shell ONLY.
Never put financial APIs in resources or localStorage any credentials.
Envelope errors `{"error":"safe message"}`.
- GET `/api/keys` => `{"items":[Key]}`
- POST `/api/keys` KeySpec => `{"item":Key,"key":"one-time plaintext"}`
- PUT `/api/keys` `{"id":id,"spec":KeySpec}` => `{"item":Key}`
- POST `/api/rotate` `{"id":id,"key":"optional custom"}` => item,key
- GET `/api/prices` => `{"items":[Price]}`; PUT `/api/prices` Price => `{"ok":true}`
- GET `/api/usage` query from/to RFC3339,key_id,model,provider,auth_id,result,
  search,page,page_size,bucket(hour/day) => Report (summary/trend/items/all-match total).
- GET `/api/capture?id=<lifecycle-id>` => Detail. Click only, never list body.
- POST `/api/resolve` `{"id":id,"action":"release|charge","amount_micro_usd":N,
  "reason":"mandatory"}` => `{"ok":true}`
- GET `/api/health` => `{"status":"ok|degraded","version":string,"capture":Stats,
  "retention_months":3,"capture_enabled":bool,"last_error":string}`

Frontend plain HTML/CSS/JS, no bundler/runtime/CDN. Chinese. Responsive tabs:
usage,keys,prices,credentials,health. UI accepts management key only in memory.
Static shell never embeds credentials. One-time key dialog.
Charts/totals/list share Report; include explicit pending and quality states.
Request model top, execution model and actual declared response model separately;
only known opencode-go/commandcode namespace is normalized for display, never
bill/access control. Body modal lazy fetch, textContent, lexical JSON formatting
preserves 64bit integer text. API prices microUSD/1M labels unambiguous.
Credentials tab calls CPA `/v8/management/credentials` and corresponding plugin
quota POST dynamically, never excludes monthly/balance/currency buckets.
Do not implement key import/OAuth form changes or modify other provider pages.

## Release

v0.1.0 initial release only after unit/race/vet, real isolated native CPA/mock
auth+retry+cancel+providers probes and browser checks. ZIPs linux/amd64,
linux/arm64,darwin/arm64 + checksums.txt. Metadata version/full commit/sdk.
Do not publish placeholders or hide incomplete requirements. Event delivery is
memory-only CPA queue: no end-to-end crash-safe exactly-once billing claim.
