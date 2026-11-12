#!/usr/bin/env bash
# Runs the whole proof for the current OS: beta.5 reference (Chromium, hinted and unhinted), the GPUI render, the diffs.
# Usage: run-proof.sh <os-label> [outDir]   (needs: node_modules with playwright + chromium, a release build of this crate)
set -euo pipefail
cd "$(dirname "$0")"
os="${1:?os label: linux | mac | win}"
out="${2:-out/$os}"
bin="target/release/gpui-pixel-proof"
[ -x "$bin" ] || bin="target/release/gpui-pixel-proof.exe"
mkdir -p "$out"
node render-reference.mjs "$out" "$os"
node render-reference.mjs "$out" "$os" nohint
for s in 1 2; do "$bin" render "$out/$os-gpui-${s}x.png" "$s"; done
: > "$out/results.txt"
for s in 1 2; do
  "$bin" diff "$out/$os-beta5-${s}x.png" "$out/$os-gpui-${s}x.png" "$out" "$os-${s}x" "$os" | tee -a "$out/results.txt"
  "$bin" diff "$out/$os-beta5-nohint-${s}x.png" "$out/$os-gpui-${s}x.png" "$out" "$os-nohint-${s}x" "$os" | tee -a "$out/results.txt"
done
