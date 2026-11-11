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

sudo apt-get update
sudo apt-get install -y "$@"
