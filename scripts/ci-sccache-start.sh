#!/usr/bin/env bash
# Starts the sccache server the job's RUSTC_WRAPPER talks to, before the first cargo call.
#
# GitHub's cache service can stay unreachable for an hour. On 2026-11-23 six jobs in three workflows failed with
# "sccache: error: Timed out waiting for server startup" before compiling anything. So: three tries with the GitHub
# cache, then a server with only the runner's local disk cache. Cargo's clients connect to whichever server is
# running, whatever their own SCCACHE_GHA_ENABLED says, so the job builds without the shared cache, slower but not
# red. SCCACHE_IDLE_TIMEOUT=0 keeps the server alive through long steps that don't compile, so a later cargo call
# never has to start a new server against the GitHub cache.
set -u

for try in 1 2 3; do
  if SCCACHE_IDLE_TIMEOUT=0 sccache --start-server; then
    exit 0
  fi
  sccache --stop-server >/dev/null 2>&1 || true
  sleep $((try * 10))
done

echo "::warning::sccache could not reach GitHub's cache service; this job compiles with a local-only cache"
SCCACHE_GHA_ENABLED=false SCCACHE_IDLE_TIMEOUT=0 sccache --start-server
