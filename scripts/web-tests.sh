#!/usr/bin/env bash
# Runs the repository's dependency-free Node web tests.
#
# The exact test file names are owned by the frontend slice, so this script
# discovers every *.test.js / *.test.cjs in the repository instead of naming
# one. Workspace records (.workspace/) and build output (dist/, node_modules/)
# are excluded: they are not part of the delivered module.
#
# When no test file exists yet the script reports that nothing was run and
# exits 0, so CI stays green on a tree whose frontend has not landed. It never
# claims a test passed -- only `node --test` decides that.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! command -v node >/dev/null 2>&1; then
	echo "web-tests: node is required" >&2
	exit 2
fi

files=()
while IFS= read -r file; do
	files+=("$file")
done < <(
	find "$root" \
		\( -name .git -o -name .workspace -o -name node_modules -o -name dist \) -prune -o \
		-type f \( -name '*.test.js' -o -name '*.test.cjs' \) -print |
		LC_ALL=C sort
)

if [[ "${#files[@]}" -eq 0 ]]; then
	echo "web-tests: no *.test.js/*.test.cjs files found; nothing to run" >&2
	exit 0
fi

printf 'web-tests: node --test %s\n' "${files[*]}" >&2
node --test "${files[@]}"
