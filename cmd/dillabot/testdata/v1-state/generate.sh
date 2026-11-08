#!/usr/bin/env bash
# Regenerates rows.json from the v1 Bot's own stores (Bot v2 Task 17, G7): it extracts tag v0.1.0-signal2.1, the last
# v1 release, into a temporary directory, runs generate.go there against that code, and writes the rows here.
#   cmd/dillabot/testdata/v1-state/generate.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
root="$(git -C "$here" rev-parse --show-toplevel)"
tag="${V1_TAG:-v0.1.0-signal2.1}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
git -C "$root" archive "$tag" | tar -x -C "$work"
mkdir -p "$work/cmd/v1stategen"
cp "$here/generate.go" "$work/cmd/v1stategen/main.go"
(cd "$work" && go run ./cmd/v1stategen/main.go) >"$here/rows.json.new"
mv "$here/rows.json.new" "$here/rows.json"
echo "wrote $here/rows.json from $tag"
