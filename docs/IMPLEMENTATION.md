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
Custom material is opaque, 8..4096 UTF-8 bytes, with no control characters or
leading/trailing whitespace. Creation and rotation share the same validation.
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
mandatory reason; ledger adjustments append, no silent reprice, and the report
shows execution cost and administrator adjustments as separate figures (an
adjustment is never merged into an execution's cost).
Resolve only accepts pending requests, never active executions. A manually closed
request stays closed after late completion. An automatically closed request that
receives late unknown usage reopens pending and restores its original hold.
Only associated requests may accept usage; after retention purge, unknown old
events cannot resurrect requests. Unresolved requests retain necessary rows.
Deduplication is limited to retained history; replaying an entire expired
Reserve/Usage/Complete lifecycle is unsupported. No permanent event journal is kept.
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
Full TraceID connects lifecycle and usage execution IDs. Input estimate is raw
request byte length (conservative estimate only), output estimate uses declared
max token fields or4096. Never modify execution body/headers/selection.
Normalize standard SDK tokens; exact commandcode/opencode-go executorAdapter
cohorts use Observer's strict complete unique normalization (copy with license).
Usage.APIKey is principal, AccessProvider must belong to plugin. ResponseModel
only actual record.ResponseModel, never record.Model fallback. No guessed joins.
Maintenance failures sticky health; finance failure blocks new Reserve, usage
errors observable. Source internal/warmup must not attach to customer records.
Advanced pricing is non-invasive: the interceptor only reads the request body
(service_tier/tier, and n/n_images/num_images/image_count clamped to 1..32,
default 1) and never rewrites the request, its headers, its selection or the
response. The billed service tier is `UsageRecord.ResponseServiceTier`, else
`UsageRecord.ServiceTier`, else the tier frozen at admission. LIMITATION: the
SDK `UsageRecord` exposes no image counters, so per-image billing charges the
frozen requested image count once on a proven successful lifecycle and cannot
reconcile an upstream that returns a different number of images; that requested
count is intentionally not an "actual images returned" claim.

## Warmup (optional, disabled by default)

Plugin-local YAML only: `warmup.enabled` (default false), `warmup.interval`
(default 1h) and `warmup.targets[]` of `auth_id`, `provider`, `model`,
`protocol` (openai|claude|gemini, default openai). auth_id, provider and model
are mandatory so a probe can never fall back to host credential or provider
selection. No credential material, hold, cap or cooldown is expressed here.

One serial worker starts only when enabled AND targets are non-empty; it issues
one `host.model.execute` per target immediately and then once per interval, at
most one in flight. Every probe pins AuthID+ForcedProvider, is non-streaming,
carries a legal single-turn "max_tokens 1" body, a fresh random proprietary
session header marker and no caller identity or host callback id. It uses a
fresh host context, so the host skips this plugin's own interceptors for it: a
probe never reserves, never settles and never touches the customer ledger.
Usage attribution does not depend on the marker (a forgeable header is never a
billing gate): a background execution has no client AccessProvider, so
`isOwnAccessProvider` rejects its usage before any ledger row is considered, and
a customer request that forges the marker is still billed in full.

Lifecycle limits, stated accurately: quiesce, a disabled configuration and
shutdown stop new probes, including the rest of a running pass. A probe already
inside the synchronous, non-cancellable host callback can not be aborted, so a
reconfigure waits `warmupSwapGrace` (5s) for the pass to go idle and otherwise
refuses with 409 while the previous configuration and worker keep running: a
refused reload never leaves the previous periodic warmup dead, never abandons a
probe and never starts a second loop. A reload refused because requests are
still settling restores the state it started from - a quiesced plugin stays
quiesced, and only a plugin that goes back to live restarts the previous
periodic warmup, with exactly one worker. Re-sending the configuration already
applied resumes the attached store instead of reopening it, so requests that
reserved money before a quiesce can still be settled. `plugin.shutdown` waits for
that callback without a bound before returning, because the host frees the
callback context and dlclose()s the library immediately afterwards; with warmup
enabled and an upstream that hangs, plugin shutdown and the hot reload behind it
can therefore take as long as that request. Neither wait is held across the call
gate, so customer requests are never blocked behind a probe. Enabling warmup is
a real request against a real credential: it can observe upstream failures,
which is the point, and it never excludes or reserves an account on its own.

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
  Baselines affect admission usage only; durable spend, history and holds remain.
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
  model (`400`/`403`), it never falls through to a cheaper price. A reservation
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

v0.1.0 initial release only after unit/race/vet, real isolated native CPA/mock
auth+retry+cancel+providers probes and browser checks. ZIPs linux/amd64,
linux/arm64,darwin/arm64 + checksums.txt. Registration version is bare release
number for CPA store comparison; health and package metadata retain full commit/sdk.
Curated notes live at `docs/release-notes-<tag>.md`; the release workflow refuses
to publish a tag without them. Config example and operations guidance live in
`config.example.yaml` and `docs/operations.md`.
Do not publish placeholders or hide incomplete requirements. Event delivery is
memory-only CPA queue: no end-to-end crash-safe exactly-once billing claim.
