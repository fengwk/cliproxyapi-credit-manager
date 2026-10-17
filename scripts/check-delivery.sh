#!/usr/bin/env bash
# Dependency-free delivery checks for the build/release tooling itself.
#
# Runs without the Go toolchain or a C compiler, so it is valid on a tree whose
# implementation slices have not landed yet. It verifies that
#   * every shell script parses and every Python script compiles,
#   * no goreleaser configuration was introduced,
#   * the workflow set stays ci.yml + release.yml with no auto-release,
#     auto-push or scheduled controller, least-privilege permissions and
#     native-only three-platform builds, and
#   * the Makefile keeps the c-shared/ldflags/package contract.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

failures=0
note() { printf '%s\n' "$*" >&2; }
pass() { printf 'ok   - %s\n' "$1"; }
fail() {
	printf 'FAIL - %s\n' "$1" >&2
	failures=$((failures + 1))
}

require() {
	local description="$1" file="$2" needle="$3"
	if [[ ! -f "$file" ]]; then
		fail "${description} (missing file ${file})"
		return
	fi
	if grep -Fq -- "$needle" "$file"; then
		pass "$description"
	else
		fail "${description} (${file} is missing '${needle}')"
	fi
}

forbid() {
	local description="$1" file="$2" needle="$3"
	if [[ ! -f "$file" ]]; then
		fail "${description} (missing file ${file})"
		return
	fi
	if grep -Fq -- "$needle" "$file"; then
		fail "${description} (${file} must not contain '${needle}')"
	else
		pass "$description"
	fi
}

# --- shell and python syntax -------------------------------------------------

for script in scripts/*.sh; do
	if bash -n "$script"; then
		pass "bash -n ${script}"
	else
		fail "bash -n ${script}"
	fi
done

if ! python3 - <<'PY'
import ast
import glob
import sys

bad = 0
for path in sorted(glob.glob("scripts/*.py")):
    try:
        with open(path, encoding="utf-8") as handle:
            ast.parse(handle.read(), path)
    except SyntaxError as err:
        print("python syntax error: {}".format(err), file=sys.stderr)
        bad = 1
    else:
        print("ok   - python syntax {}".format(path))
sys.exit(bad)
PY
then
	fail "python syntax check"
fi

# --- retired architecture keys ----------------------------------------------
# The post-usage architecture removed the admission backlog gate and the host
# model-execution (warmup) bridge. Their configuration keys must not come back:
# they are a hard config error now, so shipping them in the example config would
# advertise an unsupported setting, and the harness only uses them to assert the
# rejection path. The check is YAML-key anchored so the explanatory comments and
# the harness' retired-setting fixtures do not trip it.
retired_key_re='^[[:space:]]*(warmup|max_pending_requests):'
if grep -Eq -- "$retired_key_re" config.example.yaml; then
	fail "config.example.yaml carries no retired architecture key"
else
	pass "config.example.yaml carries no retired architecture key"
fi
require "the acceptance harness still rejects retired settings" \
	tests/integration/acceptance/cases/passive.py "RETIRED_SETTINGS"

# --- no goreleaser -----------------------------------------------------------

goreleaser="$(find . -maxdepth 2 -name '.goreleaser*' -not -path './.workspace/*' -not -path './.git/*' -print -quit)"
if [[ -n "$goreleaser" ]]; then
	fail "no goreleaser configuration (found ${goreleaser})"
else
	pass "no goreleaser configuration"
fi

# --- workflow set ------------------------------------------------------------

workflow_dir=".github/workflows"
if [[ ! -d "$workflow_dir" ]]; then
	fail "workflow directory ${workflow_dir} exists"
else
	mapfile -t workflows < <(cd "$workflow_dir" && ls -1 *.yml *.yaml 2>/dev/null | LC_ALL=C sort)
	if [[ "${#workflows[@]}" -eq 2 && "${workflows[0]}" == "ci.yml" && "${workflows[1]}" == "release.yml" ]]; then
		pass "workflow set is exactly ci.yml + release.yml"
	else
		fail "workflow set must be exactly ci.yml + release.yml (got: ${workflows[*]:-none})"
	fi
fi

for workflow in "$workflow_dir"/*.yml; do
	[[ -f "$workflow" ]] || continue
	for banned in 'schedule:' 'workflow_run:' 'git push' '--clobber' '--cleanup-tag'; do
		forbid "${workflow} has no '${banned}'" "$workflow" "$banned"
	done
done

if [[ -f "$workflow_dir/ci.yml" ]]; then
	forbid "ci.yml does not publish releases" "$workflow_dir/ci.yml" "gh release create"
fi

# --- ci.yml ------------------------------------------------------------------

ci="$workflow_dir/ci.yml"
for needle in \
	'push:' \
	'branches: [main]' \
	'pull_request:' \
	'workflow_dispatch:' \
	'contents: read' \
	'go-version-file: go.mod' \
	'node-version: "24"' \
	'go vet ./...' \
	'go test ./...' \
	'go test -race ./...' \
	'scripts/web-tests.sh' \
	'runner: ubuntu-24.04' \
	'runner: ubuntu-24.04-arm' \
	'runner: macos-latest' \
	'goos: linux' \
	'goarch: amd64' \
	'goarch: arm64' \
	'goos: darwin' \
	'make build' \
	'actions/upload-artifact@'; do
	require "ci.yml contains '${needle}'" "$ci" "$needle"
done
forbid "ci.yml has no write permission" "$ci" "contents: write"

# --- release.yml -------------------------------------------------------------

release="$workflow_dir/release.yml"
for needle in \
	'tags:' \
	'"v*"' \
	'workflow_dispatch:' \
	'RELEASE_TAG: ${{ inputs.tag || github.ref_name }}' \
	'cancel-in-progress: false' \
	'contents: read' \
	'contents: write' \
	'actions/download-artifact@' \
	'checksums.txt' \
	'scripts/verify-release-assets.py' \
	'gh release create' \
	'--verify-tag' \
	'--draft' \
	'gh release edit' \
	'needs: verify' \
	'needs: build' \
	'make race' \
	'make static' \
	'refusing' \
	'runner: ubuntu-24.04' \
	'runner: ubuntu-24.04-arm' \
	'runner: macos-latest'; do
	require "release.yml contains '${needle}'" "$release" "$needle"
done

# --- Makefile ----------------------------------------------------------------

makefile="Makefile"
for needle in \
	'PLUGIN_ID := cliproxyapi-credit-manager' \
	'github.com/fengwk/cliproxyapi-credit-manager/internal/plugin.PluginVersion' \
	'github.com/fengwk/cliproxyapi-credit-manager/internal/plugin.GitCommit' \
	'github.com/fengwk/cliproxyapi-credit-manager/internal/plugin.SDKVersion' \
	'-buildmode=c-shared' \
	'-trimpath' \
	'vet:' \
	'test: vet' \
	'race:' \
	'static:' \
	'package: build' \
	'scripts/package-plugin.sh'; do
	require "Makefile contains '${needle}'" "$makefile" "$needle"
done

if [[ "$failures" -ne 0 ]]; then
	note ""
	note "${failures} delivery check(s) failed"
	exit 1
fi
note ""
note "all delivery checks passed"
