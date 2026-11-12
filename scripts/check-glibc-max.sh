#!/usr/bin/env bash
# Fails when a native release binary needs a glibc symbol version newer than the one the consumers run
# (ubuntu-24.04 ships 2.39; a build on a newer image refuses to start there). Reads the versioned dynamic
# symbols with `objdump -T`, so it is exact about what the dynamic loader will demand.
# Every binary named here must be a dynamic glibc binary: one with no GLIBC symbol versions fails too (a static
# or musl build would make the check pass without measuring anything).
# Usage: check-glibc-max.sh <max-version, e.g. 2.39> <binary>...
# Exit: 0 all within the ceiling, 1 a binary is too new or has no GLIBC versions, 2 usage or tooling error.
set -euo pipefail
max="${1:?usage: check-glibc-max.sh <max-version> <binary>...}"
shift
[ "$#" -gt 0 ] || { echo "check-glibc-max: no binaries given" >&2; exit 2; }
command -v objdump >/dev/null 2>&1 || { echo "check-glibc-max: objdump is not installed (binutils)" >&2; exit 2; }
status=0
for bin in "$@"; do
  [ -f "$bin" ] || { echo "check-glibc-max: $bin does not exist" >&2; exit 2; }
  symbols="$(objdump -T "$bin")" || { echo "check-glibc-max: objdump -T failed on $bin" >&2; exit 2; }
  needed="$(printf '%s\n' "$symbols" | grep -o 'GLIBC_[0-9][0-9.]*' | sed 's/^GLIBC_//' | sort -Vu | tail -n 1 || true)"
  if [ -z "$needed" ]; then
    echo "check-glibc-max: $bin has no GLIBC symbol versions: not a dynamic glibc binary, nothing was measured" >&2
    status=1
    continue
  fi
  highest="$(printf '%s\n%s\n' "$needed" "$max" | sort -V | tail -n 1)"
  if [ "$highest" != "$max" ]; then
    echo "check-glibc-max: $bin needs GLIBC_$needed, newer than the allowed GLIBC_$max" >&2
    status=1
  else
    echo "check-glibc-max: $bin needs GLIBC_$needed (allowed: $max)"
  fi
done
exit "$status"
