#!/usr/bin/env bash
set -euo pipefail

# Keep Electron's real encrypted OS credential backend. This private test bus
# and newly-created password-protected keyring cannot unlock or replace a
# person's login keyring. No password enters arguments, files, or the job log.
if [[ "${1:-}" == '--inside-private-bus' ]]; then
  shift
  : "${DILLA_ACCOUNT_KEYRING_DIR:?private keyring directory required}"
  node -e 'process.stdout.write(require("node:crypto").randomBytes(32).toString("base64")+"\n")' |
    gnome-keyring-daemon --components=secrets --unlock --control-directory "$DILLA_ACCOUNT_KEYRING_DIR/control"
  exec "$@"
fi

keyring_directory=$(mktemp -d "${TMPDIR:-/tmp}/dilla-account-keyring.XXXXXXXX")
trap 'rm -rf -- "$keyring_directory"' EXIT
mkdir -m 700 "$keyring_directory/data" "$keyring_directory/config" "$keyring_directory/control"
export DILLA_ACCOUNT_KEYRING_DIR="$keyring_directory"
export XDG_DATA_HOME="$keyring_directory/data"
export XDG_CONFIG_HOME="$keyring_directory/config"
dbus-run-session -- bash "$0" --inside-private-bus "$@"
