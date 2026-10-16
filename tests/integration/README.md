# Native credit-manager acceptance harness

Isolated, reproducible end-to-end acceptance for the `credit-manager` CLIProxyAPI
native plugin. It launches a **real** CLIProxyAPI 8.0.30 host, loads the **real**
plugin shared library, and drives it over HTTP against a **loopback** upstream
mock.

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
- `--plugin-id credit-manager` override the plugin library/instance id.
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
`/v0/management/plugins/<id>/config` route and the public resource base). With
the default `--plugin-id credit-manager` they coincide.

## Output

`<artifacts>/result.json` contains, per case: the expectation (`expects`), the
observed result, the upstream call count/deltas and the upstream call details
(path + upstream credential + streaming), plus `build` metadata, `elapsed_s`,
`host_root` and `host_log`. Passing is never decided by an HTTP 200 alone: each
case asserts the actual ledger, tokens, cost and lifecycle state.

Each case's host lives at `<artifacts>/host-<n>-<case>/` with `config.yaml`,
`host.log`, `data/credit.db` and `data/request-captures.db`.

## Cases

| Module | Coverage |
| --- | --- |
| `abi` | Direct `.so` probe via ctypes: ABI 1 / RPC schema 6 negotiation, passive-only capabilities (no executor/router/scheduler), older-schema host rejected. |
| `core` | Host lists the plugin (enabled, bare numeric metadata version, published menu); health (build-identity diagnostic version carrying the commit); management auth gate (401 without/with wrong key). |
| `accounting` | Exact cost (input 10 / output 4 / total 14 = 14 microUSD), canonical buckets, complete quality, distinct execution id, same-key ledger, all-zero price is free, unpriced model denied with no upstream call and no poisoning of other keys. |
| `auth` | Five credential locations, invalid/absent denial with no upstream call, invalid Bearer + valid Google fallback, first-candidate-wins on conflict, original CPA key rejected while exclusive, custom (non `sk-`) prefix key. Deployment fail-closed: a plugin-managed key is accepted at all five locations while the plugin is loaded, and with `plugins.enabled: false` (library not loaded) all five are denied with no upstream call while the plugin instance stays unregistered. |
| `capture` | Disabled vs unavailable vs captured states, lazy fetch, header + JSON redaction, 64-bit integer fidelity, adjustable body limit (oversize skipped). |
| `report` | `from/to/key_id/model/result/search/page/page_size/bucket` filters, pagination, trend sums equal the summary, full-UUID trace ids searchable. |
| `resources` | Public static assets are embedded and credential-free; resource paths cannot reach administrative APIs. |
| `lookup` | Explicit customer Bearer, own-key-only reports, foreign/unknown snapshot 404 equivalence, natural-month trends and exact amounts. |
| `namespace` | Request model vs execution/response model are kept distinct for an aliased model; billing uses the requested model's price. |
| `lifecycle` | Unknown usage stays pending (never free); client cancel does not hang and keeps the hold; restart retains the settled balance and keeps unfinished work pending. |
| `retry` | A 429 on the first upstream credential fails over, one client request, two independent per-attempt usage rows (failed-unknown not written off), hold retained; all-failures never bill as free. |
| `warmup` | Background warmup: default-off issues no background activity; enabled issues a *real* pinned host model execution (upstream 1, host log shows credential selection with no client scope) that creates no customer ledger row/hold/charge; an unknown `AuthID` fails with no host-side fallback; with a single bound credential a customer request runs alongside the worker on the *exact same AuthID* and is billed; a forged warmup session header is still billed in full; reconfiguring to disabled leaves no residual worker. |
| `affinity` | Native CPA session-affinity scheduling (`routing.session-affinity: true`, default round-robin, two synthetic credentials): an explicit `X-Session-ID` shared by three requests with different user content, and a grown conversation prefix (system+user stable, assistant+user appended) with **no** session header, each keep every request on one credential (identical `AuthID` and upstream key) and bill 3 x 14 = 42. Unrelated requests without a session do alternate the two credentials, so the binding is not a default. |
| `gemini` | Native `generateContent` and `streamGenerateContent` (`alt=sse`) authenticate, forward the upstream Google key and bill. |
| `browser` | Real isolated Chromium: per-tab session login, optional remembered reload, logout clearing storage/charts, real ECharts/month query, lazy capture-body modal with redaction/64-bit fidelity, customer own-key snapshot lookup. |

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
