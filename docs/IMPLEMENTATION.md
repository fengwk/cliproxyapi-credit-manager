# Current implementation contract

Module: github.com/fengwk/cliproxyapi-credit-manager. CPA v8.0.30, Go 1.26.
There is no compatibility layer for earlier ledger formats and no CPA/provider
edits.

## Ownership and boundaries

- `credit/`: one SQLite-backed transactional package for keys, token counters,
  prices, usage logs and query-time cost projection. `types.go` defines the
  shared API. Implement `Open(Options) (*Store, error)` and Backend.
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
  RetentionMonths, Capture (capture.Config). Unknown fields are a hard parse
  error; only the documented configuration is accepted.
- Frontend writes only `internal/web/assets/` and `internal/web/assets_test.go`.

## Credit semantics

Key material HMAC verified, private pepper persisted mode0600. Internal Principal
`cmk:<UUID>` and SDK session.CallerScope stable. Material omitted from all
stored/returned KeySpec objects. Generate sk- plus 48 unbiased alphanumerics.
Custom material is opaque, 8..4096 UTF-8 bytes, with no control characters or
leading/trailing whitespace. Creation and rotation share the same validation.

`KeySpec` keeps the four canonical fields `quota_micro_usd`, `daily_micro_usd`,
`weekly_micro_usd` and `monthly_micro_usd` (and the Go fields), but they are
display-only budget figures. `0` means "no budget recorded", not unlimited
spending and not an admission allowance. No synonymous aliases are added.
`MaxConcurrent` 0 means unlimited. Enabled is explicit true/false. AllowedModels
empty=all, otherwise exact IDs (no fuzzy/slash-prefix inference). ModelTokenLimits
match exact requested IDs, 0=unlimited. Durable lifetime/UTC period token
counters and token reset baselines survive history cleanup too.

A Key has no `spent_micro_usd`, `quota_used_micro_usd`, daily/weekly/monthly
used amount or other permanent monetary fields. Cost is reported only from the
selected retained log window by the usage/report APIs.

Prices are integer microUSD per million tokens. `Rates` are canonical:
`Input` is uncached input; `Output` excludes Reasoning; Reasoning nil inherits
Output; explicit all-zero rates mean priced-free. Tiers are carried by pricing
rules; a zero tier rate inherits the rule price. A price may also be
`billing_mode: "per_image"` with `per_image_micro_usd`, but see the image
limitation below.

Price and display-budget configuration is persisted, but execution amounts,
adjustments and request price snapshots are not. `AdmitInput`
carries only the lifecycle ID, trace, key, exact requested model, request
service tier and time. It carries no amount, price, image count or token
estimate. Admission checks only key permissions/expiry, exact allowed model,
actual-used token quotas and explicitly configured concurrency. It never reads
prices and never estimates, reserves or holds money or in-flight tokens.

Token quotas are soft: with a configured limit > 0, `actual effective used >=
limit` rejects the next request; a positive remaining quota admits even a very
large declared output limit or input payload. Lifetime and UTC
day/week/month token counters are independent of historical rows, charged to the
request admission period. Concurrent/streaming/in-flight requests may exceed
quotas; their reliable actual token usage is recorded in full, never capped,
guessed, skipped, duplicated, cancelled, retried or rerouted for quota reasons.

Lifecycle `active`, `pending` and `settled` describe usage completeness only:
UI "executing / usage incomplete / complete". They never mean charge or
settlement. Price cannot change state. `pending` is reserved for missing or
unreliable usage; a complete but unpriced execution settles normally.

Usage arrives before/after completion. One execution UUID per event, unique
persistent dedup; same UUID with different material content conflicts. Complete
releases execution concurrency once. Unknown/failed-with-unprovable-usage/no
event => pending, never fabricated zero. Missing usage never becomes free.
Restart marks unfinished executions stopped/pending, not free. There is no
per-Key pending cap, no independent backlog gate and no money hold.

There is no administrator monetary resolve and no manual adjustment. Deleted
API/routes include `POST /api/resolve`, `POST /api/reset-spend`, Resolution,
`Backend.Resolve` and the release/charge actions. `ResetSpec`/`ResetResult`
remain, but `Store.ResetTokens` and `POST /api/reset-tokens` only snapshot the
selected Token effective-usage baselines. They never modify logs, derived cost
or dollar budgets. The audit action may remain the string `reset` with detail
noting tokens.

Only associated requests may accept usage; after retention purge, unknown old
events cannot resurrect requests. Unresolved requests retain necessary rows.
Deduplication is limited to retained history; replaying an entire expired
Admit/Usage/Complete lifecycle is unsupported. No permanent event journal is
kept.

One complete DDL initializes an empty SQLite ledger in a single transaction and
stamps `PRAGMA user_version=2` last. The schema stores no computed execution
amount, request price snapshot or adjustment; deleted fields/tables include `keys.spent_micro_usd`,
`requests.price_snapshot/charge_micro_usd/resolved`,
`usages.cost_micro_usd/charged`, `key_money_counters`, `key_money_baselines` and
`adjustments`. Same-format ledgers reopen without replaying or repairing DDL.
A nonempty unmarked ledger, format 1 (v0.1.5) or any other version is refused
and left untouched: no automatic migration, no downgrade, no legacy fallback,
no ALTER/DROP path and no silent erase. Operators must use a new empty
`data_dir`, keep the old directory as a backup and must not be advised to delete
the old database. Every key requires encrypted material; there is no
HMAC-only fallback.

Cleanup default 3 calendar months UTC (clamp day at month end), batch deletes
closed history, keeps durable token counters/baselines. `incremental_vacuum` +
WAL TRUNCATE; busy must be observable. One DB connection, bounded transactions
and no external MQ.

## Cost projection

Cost is purely query-derived. For every execution, use the exact actual
`usage.model` (full id), its normalized five token buckets and its normalized
service tier, resolve the current price book and compute an exact integer
microUSD amount. Request aliases, `RequestedModel` and `ResponseModel` are never
used to guess a price. Editing, adding or deleting a price/rule immediately
changes the report of retained history; nothing is frozen per request.

The price book is loaded once per read snapshot and matching is cached per
actual model. The whole selection is projected in one additional lean execution
scan (bucket, request model, actual model, reliable flag, service tier and token
buckets) so summary, trend and model totals share the same pass. Memory grows
with groups and the current page, not with the number of executions; this is not
a durable Observer-style pre-aggregation. Pagination only hydrates the current
page, and the summary/trend/model totals are never limited to that page.

An execution's `cost_micro_usd` is `null` (not zero, JSON explicit, no
`omitempty`) when usage is unreliable/incomplete, the actual model has no
current price, the winning price is per-image (the host reports no real image
count), or the exact product overflows. Such executions count in
`Summary.unpriced_executions`; `Summary.cost_micro_usd` is the strict subtotal of
priced executions. An explicitly all-zero complete rate is a priced zero (free)
and is not counted as unpriced. `CompleteExecutions` comes from usage quality,
not pricing coverage; the two counts may happen to be equal.

Per-image prices, rules and preview may still exist, but a log's cost must be
`null` because SDK v8.0.30 `UsageDetail` exposes tokens only and no actual image
count. The request's declared image count (or `n`) must never be treated as
actual produced images.

Cost reflects only logs still retained locally and the selected time range. It
does not claim lifetime total spend. Token counters/baselines remain independent
of log cleanup.

## Pricing rules

Rules are evaluated deterministically: priority descending, then match kind
(exact before glob before regexp), then rule id ascending. The exact price book
is a priority-zero exact candidate; a same-priority explicit exact rule wins.
A disabled rule is skipped entirely: it neither participates in pricing nor
denies a model, so it is not a model permission blacklist. No candidate or an
an amount that cannot be computed means the execution is unpriced; it never
causes a request rejection.

A context tier applies when canonical prompt size (uncached input plus cache
read and cache write) reaches its threshold; the highest matching threshold
wins. A service tier applies when the normalized actual service tier is named
in the rule; service precedence is above context. The billed tier is the host
response tier, else the reported usage tier, else the request-time tier.
Reported default/auto overrides an inherited priority tier rather than
incorrectly inheriting it.

"Price preview" computes a preview from supplied input tokens (and optional
tier) against the current book. It is not a persisted log and may differ from
actual query-time cost, which uses real usage tokens and service tier.

The model catalog uses fixed HTTPS sources, bounded fetches, no redirects and a
bounded cache; missing rates stay missing. Refresh only updates the reference
catalog. Import is an explicit administrator write that opens an exact-rule
draft; it never changes prices automatically, never associates aliases and never
performs network access on the inference/admission path.

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

RPC schema6/ABI1. A missing/zero/older schema is refused, not negotiated down.
Exclusive frontend auth, RequestInterceptor+Lifecycle+Usage,
Management+resource routes only. CPA candidates first valid in native order.
Caller_scope trusted SDK metadata => internal Key; NEVER reparse Bearer to bill.
Full TraceID connects lifecycle and usage execution IDs. There is no input/output
estimate of any kind: the interceptor never sizes the request to admit it -- a
positive remaining quota admits even a very large declared output limit or body.
Never modify execution body/headers/selection. RequestedModel is mandatory and
is the id used for permissions, token limits, request lists and model grouping.
The actual `usage.model` is the price key; ResponseModel is display-only and
never a price fallback.
Normalize standard SDK tokens; exact commandcode/opencode-go executorAdapter
cohorts use Observer's strict complete unique normalization (copy with license).
Usage.APIKey is principal, AccessProvider must belong to plugin.
Maintenance and notification errors are reported through health; each admission
fails only on its own validation/storage error (`ErrQuota`/`ErrConcurrency` =>
429) and no sticky global admissionBlocked/recordFinanceFault may permanently
block every key after one transient error or malformed/conflicting event. Never
weaken auth/management/CSRF safeguards.
No background execution: the plugin registers no executor/router/scheduler and
issues no outbound host model-execution callback; the
ABI probe asserts zero host callbacks.
Advanced pricing is non-invasive: the interceptor only reads the request body's
service tier (`service_tier`/`tier`) and never rewrites the request, its
headers, its selection or the response. It does not read or freeze an image
count for billing.

## Management HTTP contract

Base `/v0/management/plugins/credit-manager` (the plugin's own fixed, declared
route segment, distinct from the host plugin instance/library id), all API paths
management-authenticated.
The only library id is `cliproxyapi-credit-manager`, derived from the published
filename. A different host resource base stops admissions rather than allowing
execution with unowned usage notifications. Resources
`/v0/resource/plugins/cliproxyapi-credit-manager/` serve an exact embedded-asset
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
- POST `/api/reveal` `{"id":id}` => id,key; an audited disclosure of verified
  encrypted material. Invalid ciphertext fails without disclosing a key.
- POST `/api/delete-key` `{"id":id,"reason":"mandatory"}` => ok; permanent soft
  deletion, never a log/history deletion or forgiveness of unresolved usage.
- POST `/api/reset-tokens` `{id|ids|all,total,daily,weekly,monthly,reason}` =>
  `{keys,windows}`. Exactly one scope, at least one window, mandatory reason.
  Resets token baselines only; logs, derived cost and dollar budgets remain.
  `POST /api/reset-spend` is not registered.
- GET/POST/PUT `/api/callers` => items/item; `{id,display_name,enabled}` is
  attribution metadata, never an authentication, concurrency or routing policy.
- GET `/api/audit?key_id=&caller_id=&page=&page_size=` => items,total,page,page_size.
  Key/caller mutations, disclosures, token resets and pricing edits append audit
  events atomically with the action; no key material is included.
- GET `/api/prices` => `{"items":[Price]}`; PUT `/api/prices` Price => `{"ok":true}`
  (`Price` adds optional `billing_mode` `""|"token"|"per_image"` and
  `per_image_micro_usd`; a per-image price ignores token rates).
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
  higher-priority rule shadows. A disabled rule is skipped and does not deny the
  model. Admission does not resolve or freeze a price.
- GET `/api/usage` query from/to RFC3339,key_id,model,provider,auth_id,result,
  search,page,page_size,bucket(hour/day/month) => Report (summary/trend/items/all-match total).
  Summary contains the priced subtotal and `unpriced_executions`.
- GET `/api/overview` shares usage filters, ignores pagination, and returns
  summary,trend,models,retention_from from one read snapshot. Models are grouped
  by requested model, while each execution is priced by its own actual model.
  Output paired with measured latency is `measured_output_tokens`; zero samples
  mean undefined, not zero efficiency.
- GET `/api/model-catalog?refresh=1` => items,source,fetched_at,cached,stale,error.
  Fixed HTTPS sources, bounded fetches, no redirects; missing rates stay missing.
  Import is an explicit administrator write, not a background price update.
- GET `/api/exchange-rate?refresh=1` => base,quote,rate,source,fetched_at,cached,stale,error.
  Fixed USD/CNY source, display only. Failure never changes prices or cost.
- GET `/api/capture?id=<lifecycle-id>` => Detail. Click only, never list body.
- GET `/api/health` => `{"status":"ok|degraded","version":string,"capture":Stats,
  "retention_months":3,"capture_enabled":bool,"last_error":string}`
- `POST /api/resolve` is not registered (404); manual release/charge amounts do
  not exist.

Frontend plain HTML/CSS/JS, no bundler/CDN; ECharts 5.6.0 is vendored with license.
Chinese. Responsive tabs: overview,usage,keys,callers,prices,audit,credentials,health.
Credentials default to per-tab sessionStorage; explicit remember mirrors to
localStorage. Management and customer keys use separate storage prefixes;
never use Cookie/URL or consume CPA's stored login. Logout/auth failure clears
private DOM and chart instances; epochs reject late responses from old sessions.
Static shell never embeds credentials. Key plaintext is shown only in explicit
create/rotate/reveal dialogs and removed when the dialog or session ends.
Charts/totals/list share Report; include explicit pending and quality states.
All clients use the current snake_case fields only. Dollar budgets are labeled
display-only; a zero budget means unset. Cost is the sum of priced executions
only and must be rendered as unpriced (`null` with the unpriced count) when not
determinable, never as zero.
TPS uses measured_output_tokens with paired latency, not all output tokens.
Hour/day/month buckets are UTC; month means calendar month, not 30 days.
Token and money display/tooltip values use exact integers/decimal arithmetic;
only chart geometry may use floats. USD/CNY is display-only at a manual or
explicitly refreshed reference rate.
Request model top, execution model and actual declared response model separately;
only known opencode-go/commandcode namespace is normalized for display, never
bill/access control. Body modal lazy fetch, textContent, lexical JSON formatting
preserves 64bit integer text. API prices microUSD/1M labels unambiguous.
Credentials tab calls CPA `/v8/management/credentials` and corresponding plugin
token-reset POST dynamically, never excludes monthly/balance/currency buckets.
Do not implement key import/OAuth form changes or modify other provider pages.

## Release

Release only after unit/race/vet, real isolated native CPA/mock
auth+retry+failover+cancel+providers probes (including the post-usage admission,
passive-boundary and retired-config cases) and browser checks. ZIPs linux/amd64,
linux/arm64,darwin/arm64 + checksums.txt. Registration version is bare release
number for CPA store comparison; health and package metadata retain full commit/sdk.
Curated notes live at `docs/release-notes-<tag>.md`; the release workflow refuses
to publish a tag without them. Config example and operations guidance live in
`config.example.yaml` and `docs/operations.md`.
Do not publish placeholders or hide incomplete requirements. Event delivery is
memory-only CPA queue: no end-to-end crash-safe exactly-once claim.
A local version label alone never implies a published release or completed
verification. Installable artifacts must come from the verified tagged release.
