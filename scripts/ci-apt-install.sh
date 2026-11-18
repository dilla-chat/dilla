#!/usr/bin/env bash
# Installs apt packages on a CI runner: `apt-get update`, then `apt-get install -y <packages>`.
#
# Workflows run it through scripts/retry-on.mjs with mirror and DNS failure patterns, so an Ubuntu mirror outage on the
# runner ("Temporary failure resolving 'archive.ubuntu.com'") is retried while a real install error still fails at once.
# `apt-get update` exits 0 when it only warns about unreachable mirrors; the install that follows is what fails, so both
# run in the one retried command.
set -euo pipefail

if [ "$#" -eq 0 ]; then
  echo "ci-apt-install: name at least one package" >&2
  exit 2
fi

# Every attempt is bounded. Run 44253915661 (2026-11-18): two Linux legs pulled the same 310 MB as the others, but from a
# throttled regional mirror at 191-260 kB/s, and sat 20 and 27 minutes in this step (the others: under a minute). apt never
# failed, so no retry ran. A timeout turns a crawl into the "Unable to fetch" failure retry-on.mjs retries; the .deb files
# already downloaded stay in /var/cache/apt/archives, so the next attempt only fetches what is left.
# The limits are far above a healthy run (update 1-7 s, install 8-60 s) and are overridable for the test.
update_limit="${APT_UPDATE_TIMEOUT:-90}"
install_limit="${APT_INSTALL_TIMEOUT:-240}"
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

bounded update "$update_limit" sudo apt-get "${apt_opts[@]}" update
bounded install "$install_limit" sudo apt-get "${apt_opts[@]}" install -y "$@"
