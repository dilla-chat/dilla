#!/usr/bin/env bash
# Installs apt packages on a CI runner: `apt-get update`, then `apt-get install -y <packages>`.
#
# Workflows run it through scripts/retry-on.mjs with mirror and DNS failure patterns, so an Ubuntu mirror outage on the
# runner ("Temporary failure resolving 'archive.ubuntu.com'") is retried while a real install error still fails at once.
# `apt-get update` exits 0 when it only warns about unreachable mirrors; the install that follows is what fails, so both
# run in the one retried command.
#
# Opt-in fetch bound: with DILLA_APT_FETCH_TIMEOUT=<seconds> set, only the network phases are bounded. Run 44253915661
# (2026-11-18): two Linux legs pulled the same 310 MB as the others from a throttled regional mirror at 191-260 kB/s and sat
# 20 and 27 minutes here (the others: under a minute); apt never failed, so no retry ran. With the variable set the script
#   1. repairs an interrupted dpkg state left by an earlier attempt (`dpkg --configure -a`),
#   2. runs `apt-get update` under a 90 s timeout,
#   3. downloads the packages (`install --download-only`) under the given timeout, and
#   4. installs from the cache with NO timeout, so dpkg is never killed mid-unpack or mid-configure.
# A timeout in 2 or 3 prints "Unable to fetch", which retry-on.mjs retries; the .debs already in /var/cache/apt/archives
# stay (partial ones resume), so each attempt makes cumulative progress. Without the variable (the release workflows) the
# script runs exactly `apt-get update` and `apt-get install -y`, unbounded, as it always did.
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "ci-apt-install: name at least one package" >&2
  exit 2
fi

if [ -z "${DILLA_APT_FETCH_TIMEOUT:-}" ]; then
  sudo apt-get update
  sudo apt-get install -y "$@"
  exit 0
fi

fetch_limit="${DILLA_APT_FETCH_TIMEOUT}"
update_limit=90
apt_opts=(-o Acquire::Retries=3 -o Acquire::http::Timeout=20 -o Acquire::https::Timeout=20)

# `timeout` ends the command with 124 (or 137 when it had to be killed); say so in words retry-on.mjs's pattern matches.
bounded() {
  local label="$1" limit="$2"
  shift 2
  local code=0
  timeout --kill-after=10 "$limit" "$@" || code=$?
  if [ "$code" -eq 124 ] || [ "$code" -eq 137 ]; then
    echo "ci-apt-install: Unable to fetch: apt-get ${label} did not finish within ${limit} s (mirror too slow); a retry resumes from the downloaded files" >&2
  fi
  return "$code"
}

if [ -n "$(sudo dpkg --audit 2>/dev/null)" ]; then
  sudo dpkg --configure -a
fi
bounded update "$update_limit" sudo apt-get "${apt_opts[@]}" update
bounded download "$fetch_limit" sudo apt-get "${apt_opts[@]}" install -y --download-only "$@"
sudo apt-get "${apt_opts[@]}" install -y "$@"
