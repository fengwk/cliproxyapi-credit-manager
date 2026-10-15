#!/usr/bin/env bash
# Prints the full commit hash the current build embeds, or nothing when the
# source tree is not a git checkout (release archives always pass COMMIT).
#
# The value feeds the PluginVersion/GitCommit ldflags contract; a local build
# without git simply leaves GitCommit empty instead of failing.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
git -C "$root" rev-parse HEAD 2>/dev/null || true
