#!/usr/bin/env bash
# Xvfb supplies a display, but minimize, focus and real pointer hit tests also
# require a window manager. Start one only when the test display has none.
set -euo pipefail
desktop_wm_pid=''
desktop_wm_log=$(mktemp "${TMPDIR:-/tmp}/dilla-wm.XXXXXX")
cleanup() {
  if [[ -n "$desktop_wm_pid" ]]; then kill "$desktop_wm_pid" 2>/dev/null || true; wait "$desktop_wm_pid" 2>/dev/null || true; fi
  rm -f "$desktop_wm_log"
}
trap cleanup EXIT
has_manager() { [[ $(xprop -root _NET_SUPPORTING_WM_CHECK 2>/dev/null) == *'window id #'* ]]; }
if ! has_manager; then
  desktop_script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
  openbox --sm-disable --config-file "$desktop_script_dir/../test/support/openbox.xml" >"$desktop_wm_log" 2>&1 &
  desktop_wm_pid=$!
  for ((attempt = 0; attempt < 100; attempt++)); do
    if has_manager; then break; fi
    if ! kill -0 "$desktop_wm_pid" 2>/dev/null; then cat "$desktop_wm_log"; exit 1; fi
    sleep 0.05
  done
  if ! has_manager; then cat "$desktop_wm_log"; exit 1; fi
fi
"$@"
