#!/usr/bin/env bash
# Packages one native plugin build as a CLIProxyAPI plugin-store archive.
#
#   package-plugin.sh <version> <goos> <goarch> <outdir> [repo-root]
#
# Produces <outdir>/cliproxyapi-credit-manager_<version>_<goos>_<goarch>.zip
# plus a matching .zip.sha256 sidecar.
#
# The layout mirrors what the CPA v8 host plugin store installer accepts
# (internal/pluginstore.InstallArchive / SelectReleaseAssets in the SDK): the
# single dynamic library at the archive root, named with the plugin id, next to
# LICENSE, README.md and the build metadata manifest (metadata.json). The host
# only reads the library; metadata.json is the reproducible build record that
# scripts/verify-release-assets.py checks.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
default_root="$(cd "${script_dir}/.." && pwd)"

version="${1:-}"
goos="${2:-}"
goarch="${3:-}"
outdir="${4:-}"
root="${5:-$default_root}"

if [[ -z "$version" || -z "$goos" || -z "$goarch" || -z "$outdir" ]]; then
	echo "usage: package-plugin.sh <version> <goos> <goarch> <outdir> [repo-root]" >&2
	exit 2
fi

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.+-]+)?$ ]]; then
	echo "package-plugin: refusing unknown version '$version'" >&2
	exit 1
fi

plugin_id="cliproxyapi-credit-manager"
# Mirrors internal/plugin.PluginName and sdk/pluginabi.ABIVersion of the SDK the
# plugin builds against; the host reads the runtime name from the RPC
# registration, this manifest is the offline build record.
plugin_name="Credit Manager"
plugin_abi=1

case "$goos" in
linux) ext="so" ;;
darwin) ext="dylib" ;;
*)
	echo "package-plugin: unsupported goos '$goos'" >&2
	exit 1
	;;
esac

lib="${root}/dist/${goos}/${goarch}/${plugin_id}.${ext}"
if [[ ! -f "$lib" ]]; then
	echo "package-plugin: native library not found at '${lib}'" >&2
	exit 1
fi

for asset in LICENSE README.md; do
	if [[ ! -f "${root}/${asset}" ]]; then
		echo "package-plugin: required archive asset '${asset}' is missing" >&2
		exit 1
	fi
done

if ! sdk="$(bash "${script_dir}/plugin-sdk.sh" --module)"; then
	echo "package-plugin: cannot resolve the CLIProxyAPI SDK version" >&2
	exit 1
fi

commit="${COMMIT:-${GITHUB_SHA:-}}"
if [[ -z "$commit" ]]; then
	commit="$(git -C "$root" rev-parse HEAD 2>/dev/null || true)"
fi
if [[ ! "$commit" =~ ^[0-9a-f]{40}$ ]]; then
	echo "package-plugin: full 40-character commit id unavailable (set COMMIT or GITHUB_SHA)" >&2
	exit 1
fi

archive="${plugin_id}_${version}_${goos}_${goarch}.zip"
mkdir -p "$outdir"
# zip runs from inside the staging directory, so a relative outdir (e.g.
# "dist/pkg" from `make package`) would be resolved against the stage and the
# archive would be written to the wrong place or lost with the stage. Pin the
# output directory to an absolute path before archiving.
outdir="$(cd "$outdir" && pwd)"
out="${outdir}/${archive}"

stage="$(mktemp -d)"
trap 'rm -rf "$stage"' EXIT

cp "$lib" "${stage}/${plugin_id}.${ext}"
cp "${root}/LICENSE" "${root}/README.md" "$stage/"

cat >"${stage}/metadata.json" <<EOF
{
  "id": "${plugin_id}",
  "name": "${plugin_name}",
  "version": "${version}",
  "goos": "${goos}",
  "goarch": "${goarch}",
  "sdk": "${sdk}",
  "commit": "${commit}",
  "abi": ${plugin_abi}
}
EOF

rm -f "$out"
( cd "$stage" && zip -X -q "$out" "${plugin_id}.${ext}" LICENSE README.md metadata.json )

if command -v sha256sum >/dev/null 2>&1; then
	digest="$(sha256sum "$out" | awk '{print $1}')"
else
	digest="$(shasum -a 256 "$out" | awk '{print $1}')"
fi
printf '%s  %s\n' "$digest" "$archive" >"${out}.sha256"

printf 'package-plugin: %s\n' "$out" >&2
