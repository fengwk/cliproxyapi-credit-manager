# Delivery scripts

Build and release tooling for the `cliproxyapi-credit-manager` native plugin.
Nothing here compiles or imports the plugin packages, so these scripts run while
the implementation slices are still being merged.

| Script | Purpose |
|---|---|
| `plugin-version.sh` | Plugin version for ldflags/metadata: `PLUGIN_VERSION` env, exact git tag, else `0.1.0-dev+<short-sha>`. |
| `plugin-sdk.sh` | SDK module/version read from `go.mod` (`--module` prints `<module> <version>`). |
| `build-commit.sh` | Full commit hash embedded in the build (empty outside a git checkout). |
| `package-plugin.sh` | Packages one native build as `<id>_<version>_<goos>_<goarch>.zip` + `.sha256`. |
| `verify-release-assets.py` | Verifies a release asset directory: SHA256, flat unique ZIP layout, metadata identity, native headers. Stdlib only. |
| `web-tests.sh` | Discovers and runs every `*.test.js` / `*.test.cjs` with `node --test`. |
| `check-delivery.sh` | Static delivery gate: script syntax, no goreleaser, workflow and Makefile contracts. |
| `test-package-plugin.sh` | Regression tests for `package-plugin.sh` on a throwaway repository skeleton. |
| `test-verify-release-assets.py` | Unit tests for `verify-release-assets.py` using synthetic ELF/Mach-O headers. |
| `test-release-pipeline.py` | End-to-end test: the real packager output must pass the real verifier, and a mutated archive must be rejected. |

Entry points: `make static` (all checks above), `make test` (Go + Node tests),
`make build`, `make package`.

## Release archive contract

Verified against `internal/pluginstore` of the pinned SDK (v8.0.30) and the
published `cliproxyapi-commandcode-provider` release files:

* release assets are `<plugin-id>_<version>_<goos>_<goarch>.zip` plus one
  `checksums.txt` holding `<sha256>  <archive>` lines;
* each ZIP is flat and contains exactly one dynamic library named
  `<plugin-id>.so` / `.dylib`, plus `LICENSE`, `README.md` and `metadata.json`;
* `metadata.json` records the reproducible build identity: `id`, `name`,
  `version`, `goos`, `goarch`, `sdk`, `commit` (full 40-hex) and `abi`.

The host installer only reads the dynamic library; `metadata.json` is the
offline build record that `verify-release-assets.py` enforces.
