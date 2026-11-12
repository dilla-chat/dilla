#!/usr/bin/env bash
# Fails when a native release binary needs a glibc symbol version newer than the one the consumers run
# (ubuntu-24.04 ships 2.39; a build on a newer image refuses to start there). Reads the versioned dynamic
# symbols with `objdump -T`, so it is exact about what the dynamic loader will demand.
# Usage: check-glibc-max.sh <max-version, e.g. 2.39> <binary>...
set -euo pipefail
max="${1:?usage: check-glibc-max.sh <max-version> <binary>...}"
shift
[ "$#" -gt 0 ] || { echo "check-glibc-max: no binaries given" >&2; exit 2; }
status=0
for bin in "$@"; do
  [ -f "$bin" ] || { echo "check-glibc-max: $bin does not exist" >&2; exit 2; }
  needed="$(objdump -T "$bin" | grep -o 'GLIBC_[0-9][0-9.]*' | sed 's/^GLIBC_//' | sort -Vu | tail -n 1 || true)"
  if [ -z "$needed" ]; then
    echo "check-glibc-max: $bin has no GLIBC symbol versions (static or not a glibc binary)"
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
