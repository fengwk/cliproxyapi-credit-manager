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
  RetentionMonths, Capture (capture.Config). The removed `warmup.*` and
  `max_pending_requests` keys must be a hard parse error (never ignored).
- Frontend writes only `internal/web/assets/` and `internal/web/assets_test.go`.

## Credit semantics

Key material HMAC verified, private pepper persisted mode0600. Internal Principal
`cmk:<UUID>` and SDK session.CallerScope stable. Material omitted from all
stored/returned KeySpec objects. Generate sk- plus 48 unbiased alphanumerics.
Custom material is opaque, 8..4096 UTF-8 bytes, with no control characters or
leading/trailing whitespace. Creation and rotation share the same validation.
0 monetary limit/concurrency means unlimited. Enabled explicit true/false.
AllowedModels empty=all, otherwise exact IDs (no fuzzy/slash-prefix inference).
ModelTokenLimits exact requested IDs, 0=unlimited. Durable lifetime/UTC period
token counters and actual used amounts must survive history cleanup too.
Prices exact requested model; explicit all-zero rates mean free; absence denies.
Input is canonical UNCACHED input; Output excludes Reasoning. Reasoning nil rate
uses Output. Integer cost rounds up once over the sum of canonical buckets.
Raw tokens, normalization quality and price snapshot retained. Unknown accounting
does not settle as zero. Protect overflow.

`AdmitInput` (renamed from `ReserveInput`; no `InputEstimate`/`OutputEstimate`)
is idempotent by lifecycle ID, conflicting IDs/traces rejected; it persists
request/key/trace association and the frozen effective price snapshot before
execution. This is accounting correlation, NOT a reservation: admission never
estimates, reserves or holds money and never sums in-flight tokens.
Local admission `Admit` (renamed from `Reserve`/`Backend.Reserve`) checks only
key permissions, the requested-model price, actual-used monetary/token quotas
and the explicitly configured Key concurrency. Quotas are soft: with a configured
limit > 0, `actual effective used >= limit` rejects the next request; a positive
remaining quota admits even a very large declared output limit or input payload.
Lifetime and UTC day/week/month counters are independent of historical rows,
charged to the request admission period. One execution UUID per event, unique
persistent dedup. Same UUID/different material content conflicts.
Usage arrives before/after completion. Complete releases execution concurrency
once; known complete usages charge incrementally and in full, even beyond a
quota. Concurrent/streaming/in-flight requests may exceed quotas; actual usage is
never capped, guessed, skipped, double-charged, cancelled, retried or rerouted
for quota reasons. Unknown/failed-zero/no event => pending, never fake zero
billing, and missing usage never becomes fabricated zero. There is no per-Key
pending cap and no independent backlog gate (`ErrPending`/`checkBacklog` are
removed): unresolved accounting never blocks future admission. Restart marks
unfinished execution stopped/pending, not free.
Administrator Resolve accepts release or charge with a mandatory reason; ledger
adjustments append, no silent reprice, and the report shows execution cost and
administrator adjustments as separate figures (an adjustment is never merged into
an execution's cost).
Resolve only accepts pending requests, never active executions. A manually closed
request stays closed after late completion; there is no reopen-money mechanic and
no hold to restore.
Only associated requests may accept usage; after retention purge, unknown old
events cannot resurrect requests. Unresolved requests retain necessary rows.
Deduplication is limited to retained history; replaying an entire expired
Admit/Usage/Complete lifecycle is unsupported. No permanent event journal is kept.
Fresh schema has no hold/estimate columns. Opening an existing SQLite store
migrates transactionally with a guarded DROP COLUMN for `keys.held_micro_usd` and
`requests.held_micro_usd`/`initial_hold_micro_usd`/`token_estimate` (pinned
modernc SQLite). The migration is idempotent and preserves every actual charge,
key, price, snapshot, request, usage, adjustment and reset; reopening old held
data must never invent a charge.
Cleanup default 3 calendar months UTC (clamp day at month end), batch deletes
closed history, keeps durable counters. incremental_vacuum + WAL TRUNCATE; busy
must be observable. One DB connection, bounded transactions and no external MQ.

## Capture

Execution-entry snapshot, NOT final upstream HTTP/network-raw bytes.
Disabled default. Retention24h, uncompressed10MiB, physical1GiB incl WAL+indexes.
Queue bounded in BYTES, skip oversize before copying; never mutate forwarded data.
Redact Authorization/X-Api-Key/X-Goog-Api-Key/Cookie/Set-Cookie and obvious secret
JSON fields recursively. Metadata small and bounded.
JSON-shaped bodies that cannot be decoded as
one complete document are dropped, never retained as unredacted text. Non-JSON
bodies may be retained verbatim; redaction is not a general DLP guarantee.
Get missing returns status unavailable (do not claim expired without evidence); disabled returns
disabled. gzip size not TTL and not SQLite compaction. Storage-budget headroom,
evict early or pause; no periodic full VACUUM. Enqueue failure never inference
failure. Tests must check actual DB+WAL shrinking and pressure/busy cases.

## Native integration

RPC schema6/ABI1. exclusive frontend auth, RequestInterceptor+Lifecycle+Usage,
Management+resource routes only. CPA candidates first valid in native order.
Caller_scope trusted SDK metadata => internal Key; NEVER reparse Bearer to bill.
Full TraceID connects lifecycle and usage execution IDs. There is no input/output
estimate of any kind: the interceptor never sizes the request to admit it -- a
positive remaining quota admits even a very large declared output limit or body.
Never modify execution body/headers/selection.
Normalize standard SDK tokens; exact commandcode/opencode-go executorAdapter
cohorts use Observer's strict complete unique normalization (copy with license).
Usage.APIKey is principal, AccessProvider must belong to plugin. ResponseModel
only actual record.ResponseModel, never record.Model fallback. No guessed joins.
Maintenance and notification errors are reported through health; each admission
fails only on its own validation/storage error (`ErrQuota`/`ErrConcurrency` =>
429) and no sticky global admissionBlocked/recordFinanceFault may permanently
block every key after one transient error or malformed/conflicting event. Never
weaken auth/management/CSRF safeguards.
No background execution: the plugin registers no executor/router/scheduler and
issues no outbound host model-execution callback (the removed warmup bridge); the
ABI probe asserts zero host callbacks.
Advanced pricing is non-invasive: the interceptor only reads the request body
(service_tier/tier, and n/n_images/num_images/image_count clamped to 1..32,
default 1) and never rewrites the request, its headers, its selection or the
response. The billed service tier is `UsageRecord.ResponseServiceTier`, else
`UsageRecord.ServiceTier`, else the tier frozen at admission. LIMITATION: the
SDK `UsageRecord` exposes no image counters, so per-image billing charges the
frozen requested image count once on a proven successful lifecycle and cannot
reconcile an upstream that returns a different number of images; that requested
count is intentionally not an "actual images returned" claim.

## Management HTTP contract

Base `/v0/management/plugins/credit-manager` (the plugin's own fixed, declared
route segment, distinct from the host plugin instance/library id), all API paths
management-authenticated.
Resources `/v0/resource/plugins/<library-id>/` serve an exact embedded-asset
allow list plus GET `lookup/data` and `lookup/capture`. These two read-only
customer endpoints authenticate an explicit customer Bearer key, force the
authenticated KeyID, omit operator credential identifiers, and prove request
ownership before returning a snapshot. Foreign and unknown snapshot IDs share
one 404 response. No resource route exposes administrative writes.
Envelope errors `{"error":"safe message"}`.
- GET `/api/keys` => `{"items":[Key]}`
- POST `/api/keys` KeySpec => `{"item":Key,"key":"plaintext"}`
- PUT `/api/keys` `{"id":id,"spec":KeySpec}` => `{"item":Key}`
- POST `/api/rotate` `{"id":id,"key":"optional custom"}` => item,key
- POST `/api/reveal` `{"id":id}` => id,key; an audited disclosure of encrypted
  material. Legacy HMAC-only keys return 409 until rotated.
- POST `/api/delete-key` `{"id":id,"reason":"mandatory"}` => ok; permanent soft
  deletion, never a ledger/history deletion or forgiveness of pending charges.
- POST `/api/reset-spend` `{id|ids|all,total,daily,weekly,monthly,reason}` =>
  `{keys,windows}`. Exactly one scope, at least one window, mandatory reason.
  Baselines affect admission usage only; durable spend and history remain.
- GET/POST/PUT `/api/callers` => items/item; `{id,display_name,enabled}` is
  attribution metadata, never an authentication, concurrency or routing policy.
- GET `/api/audit?key_id=&caller_id=&page=&page_size=` => items,total,page,page_size.
  Key/caller mutations, disclosures, resets, pricing edits and manual resolutions
  append audit events atomically with the action; no key material is included.
- GET `/api/prices` => `{"items":[Price]}`; PUT `/api/prices` Price => `{"ok":true}`
  (`Price` adds optional `billing_mode` `""|"token"|"per_image"` and
  `per_image_micro_usd`; a per-image price ignores token rates.)
- GET `/api/pricing-rules` => `{"items":[PricingRule]}`; PUT `/api/pricing-rules`
  PricingRule => `{"item":PricingRule}`; POST `/api/delete-price-rule`
  `{"id":id}` => `{"ok":true}`; GET
  `/api/price-preview?model=&service_tier=&input_tokens=` => PriceMatch.
  `PricingRule` is `{id,match_kind:exact|glob|regexp,pattern,priority,enabled,
  price:Price,tiers:[PriceTier]}`; tiers live on the rule only, and
  `PriceTier` is `{kind:context|service,threshold,service,label,rates:Rates}`
  (zero rate inherits the rule price). `enabled` is an explicit bool: an enable
  or disable is a full PUT, never a partial patch. Resolution is deterministic:
  priority descending, then match kind (exact, glob, regexp), then id ascending;
  the exact price book is a priority zero exact candidate that only a
  higher-priority rule shadows. A matching disabled rule wins and denies the
  model (`400`/`403`), it never falls through to a cheaper price. Admission
  freezes the whole rule (price, tiers, service tier, requested image count), so
  editing or deleting a rule never reprices an admitted request.
- GET `/api/usage` query from/to RFC3339,key_id,model,provider,auth_id,result,
  search,page,page_size,bucket(hour/day/month) => Report (summary/trend/items/all-match total).
- GET `/api/overview` shares usage filters, ignores pagination, and returns
  summary,trend,models,retention_from from one read snapshot. Models are grouped
  by requested model, never an upstream alias. Output paired with measured latency
  is `measured_output_tokens`; zero samples mean undefined, not zero efficiency.
- GET `/api/model-catalog?refresh=1` => items,source,fetched_at,cached,stale,error.
  Fixed HTTPS sources, bounded fetches, no redirects; missing rates stay missing.
  Import is an explicit administrator write, not a background price update.
- GET `/api/exchange-rate?refresh=1` => base,quote,rate,source,fetched_at,cached,stale,error.
  Fixed USD/CNY source, display only. Failure never changes financial prices.
- GET `/api/capture?id=<lifecycle-id>` => Detail. Click only, never list body.
- POST `/api/resolve` `{"id":id,"action":"release|charge","amount_micro_usd":N,
  "reason":"mandatory"}` => `{"ok":true}`
- GET `/api/health` => `{"status":"ok|degraded","version":string,"capture":Stats,
  "retention_months":3,"capture_enabled":bool,"last_error":string}`

Frontend plain HTML/CSS/JS, no bundler/CDN; ECharts 5.6.0 is vendored with license.
Chinese. Responsive tabs: overview,usage,keys,callers,prices,audit,credentials,health.
Credentials default to per-tab sessionStorage; explicit remember mirrors to
localStorage. Management and customer keys use separate storage prefixes;
never use Cookie/URL or consume CPA's stored login. Logout/auth failure clears
private DOM and chart instances; epochs reject late responses from old sessions.
Static shell never embeds credentials. Key plaintext is shown only in explicit
create/rotate/reveal dialogs and removed when the dialog or session ends.
Charts/totals/list share Report; include explicit pending and quality states.
Hour/day/month buckets are UTC; month means calendar month, not 30 days.
Token and money display/tooltip values use exact integers/decimal arithmetic;
only chart geometry may use floats. USD/CNY is display-only at a manual or
explicitly refreshed reference rate.
Request model top, execution model and actual declared response model separately;
only known opencode-go/commandcode namespace is normalized for display, never
bill/access control. Body modal lazy fetch, textContent, lexical JSON formatting
preserves 64bit integer text. API prices microUSD/1M labels unambiguous.
Credentials tab calls CPA `/v8/management/credentials` and corresponding plugin
quota POST dynamically, never excludes monthly/balance/currency buckets.
Do not implement key import/OAuth form changes or modify other provider pages.

## Release

Release only after unit/race/vet, real isolated native CPA/mock
auth+retry+failover+cancel+providers probes (including the post-usage admission,
passive-boundary and retired-config cases) and browser checks. ZIPs linux/amd64,
linux/arm64,darwin/arm64 + checksums.txt. Registration version is bare release
number for CPA store comparison; health and package metadata retain full commit/sdk.
Curated notes live at `docs/release-notes-<tag>.md`; the release workflow refuses
to publish a tag without them. Config example and operations guidance live in
`config.example.yaml` and `docs/operations.md`. The candidate local version may
be 0.1.4; no release is authorized by this contract.
Do not publish placeholders or hide incomplete requirements. Event delivery is
memory-only CPA queue: no end-to-end crash-safe exactly-once billing claim.
