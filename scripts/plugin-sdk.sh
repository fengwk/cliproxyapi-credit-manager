#!/usr/bin/env bash
# Reads the CLIProxyAPI SDK version this plugin builds against from go.mod.
#
#   plugin-sdk.sh            -> vX.Y.Z          (bare version, for ldflags)
#   plugin-sdk.sh --module   -> <module> <ver>  (full identity, for metadata)
#
# The SDK module and its version are single-sourced from go.mod so the ldflags
# SDKVersion, the packaged metadata and the host requirement can never drift.
set -euo pipefail

module="github.com/router-for-me/CLIProxyAPI/v8"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

version="$(awk -v m="$module" '$1 == m { print $2; exit }' "${root}/go.mod")"
if [[ -z "$version" ]]; then
	echo "plugin-sdk: ${module} not found in go.mod" >&2
	exit 1
fi
if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]]; then
	echo "plugin-sdk: refusing unknown SDK version '$version'" >&2
	exit 1
fi

case "${1:-}" in
"")
	printf '%s\n' "$version"
	;;
--module)
	printf '%s %s\n' "$module" "$version"
	;;
*)
	echo "usage: plugin-sdk.sh [--module]" >&2
	exit 2
	;;
esac
