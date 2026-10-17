# Native credit-manager acceptance harness

Isolated, reproducible end-to-end acceptance for the `credit-manager` CLIProxyAPI
native plugin. It launches a **real** CLIProxyAPI 8.0.30 host, loads the **real**
plugin shared library, and drives it over HTTP against a **loopback** upstream
mock.

## Accounting architecture pinned here

Admission is a *correlation* step, not a reservation and not a price check: the
plugin authenticates the client key, checks key permissions, the actual-used
model-token balances and the explicitly configured concurrency, then persists the
request/key/trace association before execution. Prices and the four dollar
budgets (`quota_micro_usd`/`daily`/`weekly`/`monthly_micro_usd`) are display-only;
they never admit or deny a request. Costs are **derived at query time** from each
recorded execution's actual `usage.model` and token counts times the price book in
force when the report is read, so adding, changing or deleting a price reprices
history immediately. A key no longer carries `spent_micro_usd` or any
`*_used_micro_usd`, a request carries no frozen `price`/`price_tiers`/
`pricing_rule_id`/`image_count`, and a summary carries no
`adjustment_micro_usd`; a request/report never carries
`held_micro_usd`/`initial_hold_micro_usd`/`token_estimate` either. An execution
without a resolvable price (missing price, incomplete usage, or a `per_image`
price whose actual image count SDK usage cannot prove) has `cost_micro_usd:null`
and counts in `summary.unpriced_executions`; an explicit all-zero token price is
a priced zero. Unknown usage stays visibly pending and never becomes free, and no
independent backlog gate (`max_pending_requests`) blocks later admissions. The
plugin runs no background model execution and declares no host model-execution
callback.

## Safety and isolation

- Everything is synthetic. The harness never reads real profiles, `auths/`,
  databases or credentials, and never contacts an external account.
- Client keys, upstream keys and the management key are synthetic literals
  generated in-process; the upstream mock runs on `127.0.0.1` on a random port.
- Each case runs against its own freshly booted host with its own `data_dir`,
  SQLite databases, config and log, all under `--artifacts`.
- Every child process is stopped in a `finally` block.

## Dependencies

- Go 1.26 with CGO (to build the plugin and the CPA host). `go`, `gcc` on PATH.
- A CLIProxyAPI 8.0.30 source checkout (for the host binary). No CPA repo files
  are written by this harness.
- Python 3.11+ (standard library only).
- Optional: a locally cached Playwright Chromium (`python3 -m playwright install
  chromium` or an existing `~/.cache/ms-playwright`) for the `browser` module.
  Set `CHROME_BIN=/absolute/path/to/chrome` to explicitly use an installed Chrome
  instead. No automatic fallback occurs; an invalid executable fails the cases.
  The harness never installs anything into the repo.

## Build

```sh
# 1. Build the plugin library (from this worktree root). VERSION is the bare
#    release version the harness asserts in the registration metadata; the build
#    commit (GIT_COMMIT, auto-derived) is asserted in the health diagnostic
#    version. Keep VERSION in sync with --expected-version.
env CGO_ENABLED=1 make build VERSION=0.1.0
# -> dist/linux/amd64/cliproxyapi-credit-manager.so

# 2. Build the CPA host into a scratch dir (no CPA repo files are created).
#    Keep GOTMPDIR off a quota-limited tmpfs.
mkdir -p "$SCRATCH"
env CGO_ENABLED=1 GOTMPDIR=/home/$USER/.cache/go-tmp \
    go build -o "$SCRATCH/cpa" ./cmd/server      # run inside the CPA checkout
```

## Run

```sh
cd tests/integration
PYTHONDONTWRITEBYTECODE=1 python3 run_acceptance.py \
    --cpa "$SCRATCH/cpa" \
    --plugin "$PWD/../../dist/linux/amd64/cliproxyapi-credit-manager.so" \
    --artifacts "$SCRATCH/run" \
    --plugin-id cliproxyapi-credit-manager \
    --expected-version 0.1.0 \
    --expected-commit "$(git rev-parse HEAD)"
```

`git rev-parse HEAD` still resolves correctly from inside `tests/integration`:
git walks up to the repository root, so the command needs no extra `cd`. Pass
the **plugin source** commit, which is the commit the library was built from --
if the harness itself changes afterwards, `HEAD` moves but the built library's
commit does not.

Options:

- `--only <substr>` run only cases whose name contains the substring.
- `--modules core,retry` restrict to specific case modules (default: all).
- `--plugin-id cliproxyapi-credit-manager` override the plugin library/instance
  id. The default is the released library id, which is what the host derives
  from the installed library filename.
- `--expected-version <v>` the exact bare release version the registration
  metadata must report (default `0.1.0`); must be non-empty.
- `--expected-commit <40hex>` an explicit build commit the health diagnostic
  version must carry. When given, the health version must equal
  `<version>+<commit>` exactly. When omitted, the health version's release part
  must equal `--expected-version` and any commit part must still be a 40-hex
  commit (this project always injects the build commit, so an unexpected suffix
  is a misbuild). The registration metadata version never carries the commit.
  Must be 40 hex characters.

Exit code: `0` all cases pass, `1` at least one case failed, `2` fail-closed: the
host failed to boot, a requested module does not exist, the filter matched no
case, or `--expected-version`/`--expected-commit` is malformed. A zero-case or
unresolvable run is never reported as green. Build/run status is reported
separately from case status, and `build` in `result.json` records the expected
build identity.

The plugin involves two distinct ids: the plugin's *declared* management route
prefix (fixed at `/v0/management/plugins/credit-manager`, serving `/api/*`) and
the host plugin *instance* id (the loaded library id, e.g.
`--plugin-id cliproxyapi-credit-manager`, naming the host's generic
`/v0/management/plugins/<id>/config` route and the public resource base). The
default instance id is the released library id the host derives from the library
filename; the public resource base follows that id, so the short declared route
prefix never names the resource directory.

## Output

`<artifacts>/result.json` contains, per case: the expectation (`expects`), the
observed result, the upstream call count/deltas and the upstream call details
(path + upstream credential + streaming), plus `build` metadata, `elapsed_s`,
`host_root` and `host_log`. Passing is never decided by an HTTP 200 alone: each
case asserts the actual usage, the derived cost and the lifecycle state.

Each case's host lives at `<artifacts>/host-<n>-<case>/` with `config.yaml`,
`host.log`, `data/credit.db` and `data/request-captures.db`.

## Cases

| Module | Coverage |
| --- | --- |
| `abi` | Direct `.so` probe via ctypes: ABI 1 / RPC schema 6 negotiation, an undefined (missing/0) or older host schema refused with `unsupported_host`, passive-only capabilities (no executor/router/scheduler), no host model-execution callback bridge at any lifecycle point. |
| `core` | Host lists the plugin (enabled, bare numeric metadata version, published menu); health (build-identity diagnostic version carrying the commit); management auth gate (401 without/with wrong key). |
| `accounting` | Query-time cost (input 10 / output 4 / total 14 = 14 microUSD), canonical buckets, complete quality, distinct execution id, same-key history total, all-zero price is a priced free execution, a fresh key on an empty price book runs chat **and** Responses with `cost_micro_usd:null` and an explicit unpriced count, later add/change/delete of the only effective price (via a pricing rule) reprices the same history to 14 / 24 / null, a real usage whose upstream account label is the literal `warmup` is still priced in full. No settled request, summary or key row exposes a hold, a ledger or a frozen price. |
| `admission` | Money is display-only: a key whose four dollar budgets are all exceeded keeps executing and derives every cost; an unpriced model is admitted despite an exhausted budget; actual-used model-token limits still return 429 before upstream; `max_concurrent` still rejects a second in-flight request and releases the slot on settle; `allowed_models` and `expires_at` still deny; 20 unresolved unknown-usage requests stay pending (no cost, no unpriced execution) without blocking the same or another key; every admitted request keeps one unique execution and the summary equals the exact derived sum. |
| `auth` | Five credential locations, invalid/absent denial with no upstream call, invalid Bearer + valid Google fallback, first-candidate-wins on conflict, original CPA key rejected while exclusive, custom (non `sk-`) prefix key. Deployment fail-closed: a plugin-managed key is accepted at all five locations while the plugin is loaded, and with `plugins.enabled: false` (library not loaded) all five are denied with no upstream call while the plugin instance stays unregistered. |
| `capture` | Disabled vs unavailable vs captured states, lazy fetch, header + JSON redaction, 64-bit integer fidelity, adjustable body limit (oversize skipped). |
| `report` | `from/to/key_id/model/result/search/page/page_size/bucket` filters, pagination, trend sums equal the summary, full-UUID trace ids searchable. |
| `resources` | Public static assets are embedded and credential-free; resource paths cannot reach administrative APIs. |
| `lookup` | Explicit customer Bearer, own-key-only reports, foreign/unknown snapshot 404 equivalence, natural-month trends and exact amounts. |
| `namespace` | Request model vs actual `usage.model` are kept distinct for an aliased model; pricing uses the actual model (pricing only the request alias leaves the execution `null`, and pricing the actual model reprices it to 14). |
| `lifecycle` | Unknown usage stays pending (never free) with no hold, and it does not block the next admission; client cancel does not hang or fabricate a charge; restart retains the derived cost and keeps unfinished work pending. |
| `retry` | A 429 on the first upstream credential fails over, one client request, two independent per-attempt usage rows (failed-unknown not written off); all-failures never bill as free. |
| `passive` | No background model execution: no upstream activity or credential selection without a client request. CPA can save `warmup`/`max_pending_requests` with HTTP 200, but the subsequent plugin registration refuses these fields and the native guard denies client keys. Removing the fields restores registration and the original usage history and derived cost. A deployment configured with `warmup` also refuses registration with the field named in the host log. |
| `affinity` | Native CPA session-affinity scheduling (`routing.session-affinity: true`, default round-robin, two synthetic credentials): an explicit `X-Session-ID` shared by three requests with different user content, and a grown conversation prefix (system+user stable, assistant+user appended) with **no** session header, each keep every request on one credential (identical `AuthID` and upstream key) and bill 3 x 14 = 42. Unrelated requests without a session do alternate the two credentials, so the binding is not a default. A third case binds an explicit session, then reassigns the key's caller and rotates the key material, and asserts the same session stays on the same `AuthID`/upstream key. |
| `gemini` | Native `generateContent` and `streamGenerateContent` (`alt=sse`) authenticate, forward the upstream Google key and bill. |
| `browser` | Real isolated Chromium: per-tab session login, optional remembered reload, logout clearing storage/charts, real ECharts/month query, lazy capture-body modal with redaction/64-bit fidelity, customer own-key snapshot lookup, the default overview tab's real ECharts aggregate, an advanced rule saved through the DOM with a lossless 2^53+1 rate, and customer model-data isolation. |
| `management` | Every declared management route denies a missing and a wrong credential without leaking data; the read routes succeed with the management secret; the retired `/api/resolve` and `/api/reset-spend` routes answer 404. |
| `keyops` | Caller attribution metadata (server principal/scope/fingerprint, stable across reads; a disabled caller never disables its key) and reveal/rotate/soft-delete keeping the derived cost, the history and an unresolved pending request. |
| `reset` | `reset-tokens` re-opens only the consumed model-token baseline: it changes neither the usage log nor the derived costs nor the pending request, and validates scope/window/reason (`total` window). |
| `audit` | Key, pricing-rule and token-reset operations are all audited (newest first, with identifiers/reasons/details naming the token baseline) and never contain a plaintext key; no manual resolve action exists. |
| `pricing` | Nested rate sheets keep exact signed 64-bit values (2^53+1, int64 max) across save/reload and derive no cost; rule context/service tiers resolve exactly in preview and a rule prices a live execution by its actual usage; a `per_image` price never bills the requested `n` (no actual image count in SDK usage, so the cost is `null`); a disabled rule takes no part in pricing and is not a permission denial. |
| `reference` | `/api/model-catalog` and `/api/exchange-rate` are management-gated and, with host egress pinned to a closed loopback port, answer the safe `unavailable` envelope instead of live prices. |

## Environment notes

- The plugin is a security control, not an optional add-on: a plugin-managed
  client key is only valid while the plugin is loaded. Running CPA with
  `plugins.enabled: false` (or without the library) does not merely stop credit
  accounting -- it denies every plugin key at all five credential locations, so
  the deployment fails closed rather than silently falling back to the original
  CPA `access.api-keys`. The `auth` module's deployment case asserts this.
  The precondition is that at least one *undistributed native guard* key remains
  configured in `access.api-keys` (the native CPA guard is left in place). The
  denial is not plugin "magic": it is the host's own native authentication
  rejecting a key the absent plugin no longer injects.
- Put `GOTMPDIR` on persistent home storage; some hosts run `/tmp` on a
  quota-limited tmpfs and `go build` of the ~90 MB host binary can otherwise fail
  with `disk quota exceeded`.
- The browser module requires a locally cached Playwright Chromium; if it is
  missing the case fails with an explicit message rather than silently passing.
- The host is launched with `HTTP_PROXY`/`HTTPS_PROXY` pinned to a closed
  loopback port, so the plugin's reference catalog and FX routes can never reach
  the public internet from a run (the `reference` module asserts the safe
  `unavailable` envelope); loopback traffic is exempted, so the upstream mock
  and the harness are unaffected.

## Harness checks without a host

`test_browser_runtime.py` and `test_harness_inventory.py` are plain `unittest`
modules (standard library only) that need neither the plugin library nor a CPA
host; they are picked up by `make static`
(`python3 -m unittest discover -s tests/integration -p 'test_*.py'`). The
inventory module keeps `DEFAULT_MODULES` and `acceptance/cases/*.py` in sync,
proves every module declares at least one case, and forbids the case files from
reading a retired estimate/hold field, a key money ledger or a frozen request
price again. A requested module that does not
exist (for example the removed `warmup`) fails the run with exit code 2 instead
of being skipped.
