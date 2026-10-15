#!/usr/bin/env bash
# Regression tests for scripts/package-plugin.sh.
#
# A throwaway repository skeleton (module manifest, archive assets and a fake
# dynamic library) stands in for the real plugin, so packaging is exercised
# without a C toolchain. The relative outdir case mirrors `make package`
# ("dist/pkg"); the absolute outdir case guards the canonicalized path. Both
# verify the flat archive member layout, the metadata identity and the emitted
# sha256 sidecar; the negative cases prove the packager refuses an unknown
# version and a missing library instead of writing a bogus archive.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
packager="${script_dir}/package-plugin.sh"

if ! command -v zip >/dev/null 2>&1 || ! command -v unzip >/dev/null 2>&1; then
	echo "test-package-plugin: zip and unzip are required" >&2
	exit 2
fi

failures=0
case_number=0

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

plugin_id="cliproxyapi-credit-manager"
version="0.1.0"
commit="0123456789abcdef0123456789abcdef01234567"
sdk="github.com/router-for-me/CLIProxyAPI/v8 v8.0.30"
archive="${plugin_id}_${version}_linux_amd64.zip"

# Minimal standalone repository that package-plugin.sh can package. The
# packaging scripts are copied in because they resolve the repository root and
# go.mod from their own location.
repo="${work}/repo"
make_repo() {
	local target="$1" with_lib="$2"
	mkdir -p "${target}/scripts" "${target}/dist/linux/amd64"
	cp "${script_dir}"/*.sh "${target}/scripts/"
	cat >"${target}/go.mod" <<'EOF'
module github.com/fengwk/cliproxyapi-credit-manager

go 1.26.0

require (
	github.com/router-for-me/CLIProxyAPI/v8 v8.0.30
)
EOF
	printf 'license text\n' >"${target}/LICENSE"
	printf '# readme\n' >"${target}/README.md"
	if [[ "$with_lib" == "yes" ]]; then
		printf 'fake dynamic library\n' >"${target}/dist/linux/amd64/${plugin_id}.so"
	fi
}

report() {
	local ok="$1" description="$2" reason="$3"
	if [[ "$ok" -eq 1 ]]; then
		printf 'ok   %d - %s\n' "$case_number" "$description"
	else
		printf 'FAIL %d - %s (%s)\n' "$case_number" "$description" "$reason"
		failures=$((failures + 1))
	fi
}

run_package_case() {
	local description="$1" outdir_arg="$2" outdir_abs="$3"
	case_number=$((case_number + 1))
	local out="${outdir_abs}/${archive}"
	local ok=1 reason=""

	rm -rf "$outdir_abs"
	local rc=0
	(
		cd "$repo" &&
			COMMIT="$commit" bash "$packager" "$version" linux amd64 "$outdir_arg" "$repo"
	) >/dev/null 2>&1 || rc=$?

	if [[ "$rc" -ne 0 ]]; then
		ok=0
		reason="packager exited $rc"
	elif [[ ! -f "$out" ]]; then
		ok=0
		reason="archive missing at ${out}"
	else
		local expected members
		expected=$'LICENSE\nREADME.md\n'"${plugin_id}"$'.so\nmetadata.json'
		members="$(unzip -Z1 "$out" | LC_ALL=C sort)"
		if [[ "$members" != "$expected" ]]; then
			ok=0
			reason="unexpected archive members: ${members//$'\n'/, }"
		fi
	fi

	if [[ "$ok" -eq 1 ]]; then
		if [[ "$(unzip -p "$out" "${plugin_id}.so")" != 'fake dynamic library' ]]; then
			ok=0
			reason="dynamic library payload mismatch"
		fi
	fi

	if [[ "$ok" -eq 1 ]]; then
		local meta
		meta="$(unzip -p "$out" metadata.json)"
		for needle in \
			'"id": "'"${plugin_id}"'"' \
			'"name": "Credit Manager"' \
			'"version": "'"${version}"'"' \
			'"goos": "linux"' \
			'"goarch": "amd64"' \
			'"sdk": "'"${sdk}"'"' \
			'"commit": "'"${commit}"'"' \
			'"abi": 1'; do
			if [[ "$meta" != *"$needle"* ]]; then
				ok=0
				reason="metadata.json missing ${needle}"
				break
			fi
		done
	fi

	if [[ "$ok" -eq 1 ]]; then
		if ! (cd "$outdir_abs" && sha256sum -c "${archive}.sha256") >/dev/null 2>&1; then
			ok=0
			reason="sha256 sidecar verification failed"
		fi
	fi

	report "$ok" "$description" "$reason"
}

make_repo "$repo" yes

# Relative outdir exactly as `make package` passes it: must land in the repo,
# not inside the (removed) staging directory.
run_package_case "relative outdir is written to the requested directory" \
	"dist/pkg" "${repo}/dist/pkg"

# Absolute outdir must keep working.
run_package_case "absolute outdir is written to the requested directory" \
	"${work}/abs/pkg" "${work}/abs/pkg"

# Negative: an unknown version must be refused, never silently relabelled.
case_number=$((case_number + 1))
rc=0
( cd "$repo" && COMMIT="$commit" bash "$packager" "not-a-version" linux amd64 "${work}/bad/pkg" "$repo" ) >/dev/null 2>&1 || rc=$?
if [[ "$rc" -ne 0 && ! -e "${work}/bad/pkg/${plugin_id}_not-a-version_linux_amd64.zip" ]]; then
	report 1 "unknown version is refused" ""
else
	report 0 "unknown version is refused" "packager exited $rc"
fi

# Negative: a missing native library must be refused.
norepo="${work}/norepo"
make_repo "$norepo" no
case_number=$((case_number + 1))
rc=0
( cd "$norepo" && COMMIT="$commit" bash "$norepo/scripts/package-plugin.sh" "$version" linux amd64 "${work}/norepo/pkg" "$norepo" ) >/dev/null 2>&1 || rc=$?
if [[ "$rc" -ne 0 ]]; then
	report 1 "missing native library is refused" ""
else
	report 0 "missing native library is refused" "packager exited 0"
fi

# Negative: an incomplete commit id must be refused rather than packaged as is.
case_number=$((case_number + 1))
rc=0
( cd "$repo" && COMMIT=deadbeef bash "$packager" "$version" linux amd64 "${work}/short/pkg" "$repo" ) >/dev/null 2>&1 || rc=$?
if [[ "$rc" -ne 0 ]]; then
	report 1 "short commit id is refused" ""
else
	report 0 "short commit id is refused" "packager exited 0"
fi

if [[ "$failures" -ne 0 ]]; then
	printf '\n%d test(s) failed\n' "$failures" >&2
	exit 1
fi
printf '\nall %d package-plugin tests passed\n' "$case_number"
