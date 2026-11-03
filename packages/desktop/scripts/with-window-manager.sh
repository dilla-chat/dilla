#!/usr/bin/env bash
# Xvfb supplies a display, but minimize, focus and real pointer hit tests also
# require a window manager. Start one only when the test display has none.
set -euo pipefail
desktop_wm_pid=''
desktop_notify_pid=''
desktop_wm_log=$(mktemp "${TMPDIR:-/tmp}/dilla-wm.XXXXXX")
cleanup() {
  if [[ -n "$desktop_notify_pid" ]]; then kill "$desktop_notify_pid" 2>/dev/null || true; wait "$desktop_notify_pid" 2>/dev/null || true; fi
  if [[ -n "$desktop_wm_pid" ]]; then kill "$desktop_wm_pid" 2>/dev/null || true; wait "$desktop_wm_pid" 2>/dev/null || true; fi
  rm -f "$desktop_wm_log"
}
trap cleanup EXIT
has_manager() { [[ $(xprop -root _NET_SUPPORTING_WM_CHECK 2>/dev/null) == *'window id #'* ]]; }
if ! has_manager; then
  desktop_script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
  openbox --sm-disable --config-file "$desktop_script_dir/../test/support/openbox.xml" >"$desktop_wm_log" 2>&1 &
  desktop_wm_pid=$!
  # A loaded runner can take several seconds to map the window manager.
  for ((attempt = 0; attempt < 600; attempt++)); do
    if has_manager; then break; fi
    if ! kill -0 "$desktop_wm_pid" 2>/dev/null; then echo 'openbox exited before managing the display' >&2; cat "$desktop_wm_log" >&2; exit 1; fi
    sleep 0.05
  done
  if ! has_manager; then echo 'openbox did not manage the display within 30 s' >&2; cat "$desktop_wm_log" >&2; exit 1; fi
fi
has_notifications() { [[ $(dbus-send --session --print-reply --dest=org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus.NameHasOwner string:org.freedesktop.Notifications 2>/dev/null) == *'boolean true'* ]]; }
if [[ "${DILLA_E2E_NOTIFICATIONS:-0}" == '1' ]] && ! has_notifications; then
  command -v dunst >/dev/null || { echo 'Native notification acceptance requires dunst'; exit 1; }
  dunst --config /dev/null >>"$desktop_wm_log" 2>&1 &
  desktop_notify_pid=$!
  for ((attempt = 0; attempt < 600; attempt++)); do
    if has_notifications; then break; fi
    if ! kill -0 "$desktop_notify_pid" 2>/dev/null; then echo 'dunst exited before owning org.freedesktop.Notifications' >&2; cat "$desktop_wm_log" >&2; exit 1; fi
    sleep 0.05
  done
  if ! has_notifications; then echo 'dunst did not own org.freedesktop.Notifications within 30 s' >&2; cat "$desktop_wm_log" >&2; exit 1; fi
fi
"$@"
