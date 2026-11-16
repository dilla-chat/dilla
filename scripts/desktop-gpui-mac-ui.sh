#!/usr/bin/env bash
# Drives the packaged macOS app the way a person does (plan 2026-11-16-gpui-macos-ci, Task 5, Layer 3): its window,
# text, accessibility tree, traffic lights, log file, app menu, ⌘Q, single instance, "Start Dilla at login" and the tray.
#
# Usage: desktop-gpui-mac-ui.sh <path/to/Dilla.app> <out-dir> [--connected]
#   Exits 0, or 1 after every check ran, with one `::error::<check>: <what>` line per failure. Leaves the shots
#   (<out-dir>/*.png), their text (<out-dir>/ocr-*.txt), the menus and the AX snapshots (<out-dir>/*.txt) and the app's
#   output (<out-dir>/app-*.log) behind.
#   --connected adds the checks after connect (the 2026-11-16 addendum; not in CI yet): a local dilla-testhost behind
#   dilla-tlsproxy (env DILLA_TESTHOST, DILLA_CORE_WASI, DILLA_TLSPROXY; it uses sudo for /etc/hosts, port 443 and the
#   System keychain's trust), two profiles (DILLA_DATA_DIR) signing up through an https invite link, Copy (pbpaste),
#   Print, the windows after connect, the popovers and the segments.
#
# Every UI access goes through System Events, to the app's process found by bundle identifier and pid (P below), or is
# a real click (mac-click.swift) at an element's frame. The script never sends the app an AppleEvent: the runner image
# grants osascript AppleEvents to System Events only, so `tell application "Dilla"` would block on a consent prompt.
# Every wait has a deadline; on exit the script kills what it started, and the workflow's cleanup step does too.
#
# Expected values come from the app's sources: the manager window's words from screens/instances/copy.rs, the tray and
# autostart rows from native/{tray,autostart}.rs, the update row from strings.rs, the app menu from native/menus.rs
# (`mac_app_items`, the standard macOS app menu Jonas approved on 2026-11-16).
set -uo pipefail

CONNECTED=0
if [ "$#" -eq 3 ] && [ "$3" = --connected ]; then CONNECTED=1; set -- "$1" "$2"; fi
[ "$#" -eq 2 ] || { echo 'usage: desktop-gpui-mac-ui.sh <path/to/Dilla.app> <out-dir> [--connected]' >&2; exit 2; }
[ "$(uname -s)" = Darwin ] || { echo '::error::desktop-gpui-mac-ui: runs on macOS only (System Events, screencapture, Vision)'; exit 1; }
APP=$(cd "$1" && pwd -P) || { echo "::error::desktop-gpui-mac-ui: $1 does not exist"; exit 1; }
mkdir -p "$2"
OUT=$(cd "$2" && pwd -P)
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
SRC=$ROOT/clients/desktop/dilla-desktop/src
BUNDLE=dev.dilla.desktop
EXE=$APP/Contents/MacOS/dilla-desktop
AGENT=$HOME/Library/LaunchAgents/$BUNDLE.plist
FAILED=0

fail() { echo "::error::$1: $2"; FAILED=1; }
note() { echo "--- $*"; }

# deadline <secs> <cmd…>: runs the command, killed after <secs> (exit 124).
deadline() {
  local secs=$1; shift
  "$@" &
  local pid=$! ticks=0
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$ticks" -ge $((secs * 10)) ]; then
      # A process this user cannot kill (sudo's child) is left behind rather than waited for.
      if kill -9 "$pid" 2>/dev/null || sudo -n kill -9 "$pid" 2>/dev/null; then wait "$pid" 2>/dev/null; fi
      echo "desktop-gpui-mac-ui: '$*' did not finish in ${secs}s" >&2
      return 124
    fi
    sleep 0.1
    ticks=$((ticks + 1))
  done
  wait "$pid"
}

# wait_for <secs> <cmd…>: polls the command every half second until it succeeds; 1 at the deadline.
wait_for() {
  local secs=$1; shift
  local end=$((SECONDS + secs))
  until "$@"; do
    [ "$SECONDS" -lt "$end" ] || return 1
    sleep 0.5
  done
}

# se <secs> <body> [args…]: the AppleScript <body> inside `tell application "System Events"`, with P the app's process
# (pid $PID; with two processes of one app System Events answers for the first, so the checks after connect walk the
# tree with mac-ax instead) and argv the args. `s(x)` turns a missing value into "" (AppleScript's `missing value`).
PID=
se() {
  local secs=$1 body=$2; shift 2
  deadline "$secs" osascript -e "on s(x)
if x is missing value then return \"\"
return x as text
end s
on run argv
tell application \"System Events\"
set P to first application process whose unix id is $PID
$body
end tell
end run" "$@"
}

# A Rust `pub const NAME: &str = "…";` from a source file, with its \u{…} escapes decoded.
rust_const() {
  local value
  value=$(sed -n "s/^pub const $2: &str = \"\(.*\)\";\$/\1/p" "$1" | head -n 1)
  [ -n "$value" ] || { echo "::error::sources: $2 is not a string constant in $1" >&2; return 1; }
  printf '%s' "$value" | perl -CS -pe 's/\\u\{([0-9a-fA-F]+)\}/chr(hex($1))/ge'
}

# A strings.rs entry's text.
string_key() {
  sed -n "s/^    (\"$1\", \"\(.*\)\"),\$/\1/p" "$SRC/strings.rs" | head -n 1
}

app_pids() { pgrep -f "$EXE" || true; }
pid_gone() { ! kill -0 "$PID" 2>/dev/null; }
app_running() { [ -n "$(app_pids)" ]; }
app_gone() { ! app_running; }
has_window() { deadline 20 "$BIN/mac-ax" dump "$PID" 2>/dev/null | grep -q '^AXWindow'; }

# The menu's rows, one per line: name<TAB>cmd char<TAB>cmd modifiers<TAB>mark char (a separator's name is "--").
menu_rows() {
  se 30 "set out to \"\"
repeat with m in menu items of $1
  set n to my s(name of m)
  if n is \"\" then set n to \"--\"
  set c to \"\"
  try
    set c to my s(value of attribute \"AXMenuItemCmdChar\" of m)
  end try
  set k to \"\"
  try
    set k to my s(value of attribute \"AXMenuItemCmdModifiers\" of m)
  end try
  set mk to \"\"
  try
    set mk to my s(value of attribute \"AXMenuItemMarkChar\" of m)
  end try
  set out to out & n & tab & c & tab & k & tab & mk & linefeed
end repeat
return out"
}

APP_MENU='menu 1 of menu bar item 2 of menu bar 1 of P'
TRAY_ITEM='menu bar item 1 of menu bar 2 of P'
TRAY_MENU="menu 1 of $TRAY_ITEM"

new_pid() { PID=$(comm -13 <(sort <<< "$1") <(app_pids | sort) | head -n 1); [ -n "$PID" ]; }

# launch <tag> [VAR=value…]: opens the app (a new process, with that environment) and waits for its first window; PID
# is its pid. With no environment it is `open` as a person does it (the default profile).
launch() {
  local tag=$1; shift
  local before args=()
  before=$(app_pids)
  for e in "$@"; do args+=(--env "$e"); done
  if [ "$#" -gt 0 ]; then args+=(-n); fi
  open ${args[@]+"${args[@]}"} --stdout "$OUT/app-$tag.out.log" --stderr "$OUT/app-$tag.err.log" "$APP" || { fail launch "open $APP failed"; return 1; }
  if ! wait_for 15 new_pid "$before"; then
    fail launch "no new $EXE process within 15 s ($tag)"
    tail -n 50 "$OUT/app-$tag.err.log" "$OUT/app-$tag.out.log"
    return 1
  fi
  if ! wait_for 30 has_window; then
    fail launch "no window of pid $PID within 30 s ($tag)"
    tail -n 50 "$OUT/app-$tag.err.log" "$OUT/app-$tag.out.log"
    return 1
  fi
  note "launched ($tag): pid $PID"
}

check_keychain() {
  if pgrep -x SecurityAgent >/dev/null; then fail Keychain 'a SecurityAgent prompt is up after the start'; fi
}

check_text_and_title() {
  local title want pid wid
  title=$(se 10 'return my s(name of window 1 of P)')
  want=$(rust_const "$SRC/screens/instances/copy.rs" WINDOW_TITLE)
  note "AX window title: '$title'"
  [ "$title" = "$want" ] || fail 'Window title' "the AX window title is '$title', not '$want'"
  pid=$PID
  "$BIN/mac-windows" "$pid" | tee "$OUT/windows.txt"
  wid=$(awk 'NR == 1 { print $1 }' "$OUT/windows.txt")
  [ -n "$wid" ] || { fail Text "CGWindowList has no on-screen window of pid $pid"; return; }
  screencapture -x "$OUT/screen.png" || fail Text 'the full-screen screencapture failed'
  screencapture -x -o -l "$wid" "$OUT/window.png" || { fail Text "screencapture of window $wid failed"; return; }
  "$BIN/mac-ocr" "$OUT/window.png" > "$OUT/ocr-window.txt" || fail Text 'OCR of window.png failed'
  [ -f "$OUT/screen.png" ] && "$BIN/mac-ocr" "$OUT/screen.png" > "$OUT/ocr-screen.txt"
  note 'OCR (window):'
  cat "$OUT/ocr-window.txt"
  local text name expect
  text=$(cut -f 2- "$OUT/ocr-window.txt" | tr '\n' ' ')
  for name in HEADING_1 ADDRESS_LABEL CONNECT; do
    expect=$(rust_const "$SRC/screens/instances/copy.rs" "$name")
    grep -qiF -- "$expect" <<< "$text" || fail Text "OCR does not find '$expect' (copy::$name) in the window: no text drawn?"
  done
}

# The manager window's accessibility tree, walked through System Events: the instance address and invite link fields and
# their Connect and Join buttons, by name (the adapter hands its tree over on the first query, so a few seconds' grace).
check_ax_tree() {
  local want=(
    "AXTextField|$(rust_const "$SRC/screens/instances/copy.rs" ADDRESS_LABEL)"
    "AXButton|$(rust_const "$SRC/screens/instances/copy.rs" CONNECT)"
    "AXTextField|$(rust_const "$SRC/screens/instances/copy.rs" INVITE_LABEL)"
    "AXButton|$(rust_const "$SRC/screens/instances/copy.rs" INVITE_CONNECT)"
  )
  local end=$((SECONDS + 10)) missing w
  while :; do
    snap picker || { fail 'AX tree' 'the window could not be walked'; return; }
    missing=
    for w in "${want[@]}"; do has "${w%%|*}" "${w#*|}" || missing="$missing, ${w%%|*} '${w#*|}'"; done
    [ -z "$missing" ] || [ "$SECONDS" -ge "$end" ] || { sleep 1; continue; }
    break
  done
  note 'AX tree of the picker (role, subrole, title, description, value, x, y, w, h, window):'
  cat "$SNAP"
  [ -z "$missing" ] || fail 'AX tree' "the instance picker exposes no ${missing#, } (ax-picker.txt)"
}

# No OCR'd text in the system's close / minimise / zoom buttons.
check_traffic_lights() {
  local buttons win img_w
  buttons=$(se 10 'set out to ""
repeat with b in (buttons of window 1 of P)
  set sr to my s(subrole of b)
  if sr is in {"AXCloseButton", "AXMinimizeButton", "AXZoomButton", "AXFullScreenButton"} then
    set {x, y} to position of b
    set {w, h} to size of b
    set out to out & sr & " " & x & " " & y & " " & w & " " & h & linefeed
  end if
end repeat
return out')
  win=$(se 10 'set {x, y} to position of window 1 of P
set {w, h} to size of window 1 of P
return (x as text) & " " & y & " " & w & " " & h')
  printf '%s\n' "$buttons" > "$OUT/traffic-lights.txt"
  note "window (pt): $win; traffic lights (pt):"
  cat "$OUT/traffic-lights.txt"
  [ -n "$buttons" ] || { fail 'Traffic lights' 'window 1 has no close/minimise/zoom buttons'; return; }
  [ -f "$OUT/window.png" ] || { fail 'Traffic lights' 'no window shot to compare'; return; }
  img_w=$(sips -g pixelWidth "$OUT/window.png" | awk '/pixelWidth/ { print $2 }')
  local hits
  hits=$(awk -v win="$win" -v imgw="$img_w" -v lights="$OUT/traffic-lights.txt" -F '\t' '
    BEGIN {
      split(win, W, " "); s = imgw / W[3]
      x0 = 1e9; y0 = 1e9; x1 = -1e9; y1 = -1e9
      while ((getline line < lights) > 0) {
        if (split(line, b, " ") < 5) continue
        if (b[2] < x0) x0 = b[2]; if (b[3] < y0) y0 = b[3]
        if (b[2] + b[4] > x1) x1 = b[2] + b[4]; if (b[3] + b[5] > y1) y1 = b[3] + b[5]
      }
      X0 = (x0 - W[1]) * s; Y0 = (y0 - W[2]) * s; X1 = (x1 - W[1]) * s; Y1 = (y1 - W[2]) * s
      printf "traffic lights (px, scale %.2f): %d %d %d %d\n", s, X0, Y0, X1, Y1 > "/dev/stderr"
    }
    # Text only: a traffic light itself reads as "•" (no letter or digit).
    $2 ~ /[A-Za-z0-9]/ { split($1, r, " "); if (r[1] < X1 && r[1] + r[3] > X0 && r[2] < Y1 && r[2] + r[4] > Y0) print $2 }
  ' "$OUT/ocr-window.txt")
  [ -z "$hits" ] || fail 'Traffic lights' "text drawn under the traffic lights: $(tr '\n' '|' <<< "$hits")"
}

# menu_has <file> <name> <cmd char> <modifiers> <check>: the row is there with that shortcut.
menu_has() {
  local row
  row=$(awk -F '\t' -v n="$2" '$1 == n' "$1" | head -n 1)
  [ -n "$row" ] || { fail "$5" "'$2' is missing"; return; }
  [ -z "$3" ] && return
  local c k
  c=$(cut -f 2 <<< "$row"); k=$(cut -f 3 <<< "$row")
  [ "$(tr '[:lower:]' '[:upper:]' <<< "$c")" = "$(tr '[:lower:]' '[:upper:]' <<< "$3")" ] && [ "$k" = "$4" ] \
    || fail "$5" "'$2' has the shortcut char '$c' modifiers '$k', not '$3' modifiers $4 (0 = ⌘, 2 = ⌥⌘, 12 = ⌃)"
}

# The standard macOS app menu (menus.rs `mac_app_items`) and the Window menu, with their shortcuts. AXMenuItemCmdModifiers:
# 0 is ⌘ alone, +1 ⇧, +2 ⌥, +4 ⌃, +8 no ⌘.
check_app_menu() {
  local name
  name=$(se 10 'return my s(name of menu bar item 2 of menu bar 1 of P)')
  [ "$name" = Dilla ] || fail 'App menu' "the app menu is '$name', not 'Dilla'"
  menu_rows "$APP_MENU" > "$OUT/menu-app.txt" || fail 'App menu' 'the app menu could not be read'
  se 10 'set out to ""
repeat with m in menu bar items of menu bar 1 of P
  set out to out & my s(name of m) & linefeed
end repeat
return out' > "$OUT/menu-bar.txt"
  menu_rows 'menu 1 of menu bar item "Window" of menu bar 1 of P' > "$OUT/menu-window.txt" || fail 'Window menu' 'the Window menu could not be read'
  note 'menu bar:'; cat "$OUT/menu-bar.txt"
  note 'Dilla menu (name, char, modifiers, mark):'; cat "$OUT/menu-app.txt"
  note 'Window menu:'; cat "$OUT/menu-window.txt"
  local order
  order=$(awk -F '\t' '$1 != "--" { print $1 }' "$OUT/menu-app.txt" \
    | grep -xF -e 'About Dilla' -e 'Settings…' -e 'Services' -e 'Hide Dilla' -e 'Hide Others' -e 'Show All' -e 'Quit Dilla' | tr '\n' '|')
  [ "$order" = 'About Dilla|Settings…|Services|Hide Dilla|Hide Others|Show All|Quit Dilla|' ] \
    || fail 'App menu' "the standard rows are '$order', not 'About Dilla|Settings…|Services|Hide Dilla|Hide Others|Show All|Quit Dilla|'"
  [ "$(awk -F '\t' 'NF { last = $1 } END { print last }' "$OUT/menu-app.txt")" = 'Quit Dilla' ] || fail 'App menu' "'Quit Dilla' is not the last row"
  menu_has "$OUT/menu-app.txt" 'Settings…' ',' 0 'App menu'
  menu_has "$OUT/menu-app.txt" 'Hide Dilla' H 0 'App menu'
  menu_has "$OUT/menu-app.txt" 'Hide Others' H 2 'App menu'
  menu_has "$OUT/menu-app.txt" 'Quit Dilla' Q 0 'App menu'
  menu_has "$OUT/menu-window.txt" 'Minimize' M 0 'Window menu'
  menu_has "$OUT/menu-window.txt" 'Bring All to Front' '' '' 'Window menu'
}

# ⌘Q with nothing to guard (no call, no draft) quits at once: no "Quit Dilla?" window.
check_cmd_q() {
  se 10 'set frontmost of P to true' >/dev/null
  sleep 1
  se 10 'keystroke "q" using command down' >/dev/null
  if ! wait_for 10 pid_gone; then
    fail '⌘Q' "the app still runs 10 s after ⌘Q; its windows: $(se 10 'return name of every window of P' 2>&1)"
    pkill -f "$EXE"
    wait_for 10 app_gone
  fi
}

# One process: `open -n` and a bare dilla:// link hand over to the running app.
check_single_instance() {
  local first now
  first=$(app_pids)
  [ "$(wc -l <<< "$first" | tr -d ' ')" = 1 ] || { fail 'Single instance' "the launch left the pids '$first'"; return; }
  open -n "$APP" || fail 'Single instance' 'open -n failed'
  sleep 5
  now=$(app_pids)
  [ "$now" = "$first" ] || fail 'Single instance' "after open -n the pids are '$(tr '\n' ' ' <<< "$now")', not '$first'"
  open 'dilla://' || fail 'Single instance' 'open dilla:// failed (the scheme is not registered?)'
  sleep 5
  now=$(app_pids)
  [ "$now" = "$first" ] || fail 'Single instance' "after open dilla:// the pids are '$(tr '\n' ' ' <<< "$now")', not '$first'"
}

# real_click <x> <y>: a left click posted as CGEvents (mac-click), else System Events' own `click at`.
# real_click <x> <y> [right]: a click posted as CGEvents (mac-click); a left click falls back to System Events' own
# `click at`.
real_click() {
  deadline 10 "$BIN/mac-click" "$@" && return 0
  [ "$#" -eq 2 ] && se 10 "click P at {$1, $2}" >/dev/null
}

# click_element <element>: a real mouse click at the centre of a System Events element (for views with no AX press).
click_element() {
  local at button=${2:-}
  at=$(se 10 "set pp to position of $1
set zz to size of $1
return (((item 1 of pp) + (item 1 of zz) / 2) as integer as text) & \" \" & (((item 2 of pp) + (item 2 of zz) / 2) as integer as text)") || return 1
  # shellcheck disable=SC2086 # two numbers
  real_click $at $button
}

tray_menu_open() { se 5 "return exists $TRAY_MENU" 2>/dev/null | grep -qx true; }

# Opens the status item's menu with a real click (tray-icon attaches its menu on mouseDown only).
tray_open() {
  note "status item at $(se 10 "set pp to position of $TRAY_ITEM
set zz to size of $TRAY_ITEM
return ((item 1 of pp) as text) & \",\" & (item 2 of pp) & \" \" & (item 1 of zz) & \"x\" & (item 2 of zz)" 2>&1)"
  # The app builds its status item with `with_menu_on_left_click(false)`: a left click shows the app, a right click
  # the menu.
  click_element "$TRAY_ITEM" right || return 1
  wait_for 5 tray_menu_open && return 0
  shot tray-click
  return 1
}

# Clicks the tray's row named $1. The menu closes with the click.
tray_click() {
  tray_open || return 1
  se 10 "click menu item (item 1 of argv) of $TRAY_MENU" "$1" >/dev/null
}

agent_on() { [ -f "$AGENT" ]; }
agent_off() { [ ! -e "$AGENT" ]; }

# "Start Dilla at login": on from the tray, a LaunchAgent for this bundle with --hidden; checked in the menus; off from
# the app menu (which mirrors the tray's rows), and the agent is gone.
check_autostart() {
  local label
  label=$(rust_const "$SRC/native/autostart.rs" START_AT_LOGIN)
  agent_off || { fail Autostart "$AGENT exists before the toggle"; return; }
  if ! tray_click "$label"; then
    fail Autostart "the tray's '$label' could not be clicked; trying the app menu"
    se 10 "click menu item (item 1 of argv) of $APP_MENU" "$label" >/dev/null || fail Autostart "the app menu's '$label' could not be clicked"
  fi
  wait_for 5 agent_on || { fail Autostart "no $AGENT 5 s after '$label'"; return; }
  cp "$AGENT" "$OUT/launch-agent.plist"
  cat "$AGENT"
  plutil -lint "$AGENT" || fail Autostart 'the LaunchAgent is not a valid plist'
  [ "$(plutil -extract Label raw "$AGENT")" = "$BUNDLE" ] || fail Autostart "Label is not $BUNDLE"
  [ "$(plutil -extract ProgramArguments.0 raw "$AGENT")" = "$EXE" ] || fail Autostart "ProgramArguments.0 is not $EXE"
  [ "$(plutil -extract ProgramArguments.1 raw "$AGENT")" = '--hidden' ] || fail Autostart 'ProgramArguments.1 is not --hidden'
  [ "$(plutil -extract RunAtLoad raw "$AGENT")" = true ] || fail Autostart 'RunAtLoad is not true'
  menu_rows "$APP_MENU" > "$OUT/menu-app-autostart.txt"
  [ -n "$(awk -F '\t' -v n="$label" '$1 == n { print $4 }' "$OUT/menu-app-autostart.txt")" ] \
    || fail Autostart "the app menu's '$label' has no check mark while the agent exists"
  se 10 "click menu item (item 1 of argv) of $APP_MENU" "$label" >/dev/null || fail Autostart "the app menu's '$label' could not be clicked"
  wait_for 5 agent_off || fail Autostart "$AGENT is still there 5 s after the second '$label'"
}

# The tray's rows, in tray.rs's slot order: "Your instances…", the open instances, a separator, "Start Dilla at
# login", the update row(s), a separator, "Quit Dilla". Then its Quit Dilla quits through the same guard.
check_tray() {
  local instances autostart check checking retry got
  instances=$(rust_const "$SRC/native/tray.rs" YOUR_INSTANCES)
  autostart=$(rust_const "$SRC/native/autostart.rs" START_AT_LOGIN)
  check=$(string_key desktop.update.check)
  checking=$(string_key desktop.update.checking)
  retry=$(string_key desktop.update.retry)
  [ -n "$check" ] && [ -n "$checking" ] && [ -n "$retry" ] || { fail Tray 'the update rows are not in strings.rs'; return; }
  tray_open || fail Tray 'a click on the status item opened no menu'
  menu_rows "$TRAY_MENU" > "$OUT/menu-tray.txt" || fail Tray 'the status item has no readable menu'
  se 10 'key code 53' >/dev/null
  note 'tray menu:'; cat "$OUT/menu-tray.txt"
  # Instance rows and extra update rows (an available version) are folded into one token each.
  got=$(cut -f 1 "$OUT/menu-tray.txt" | awk -v i="$instances" -v a="$autostart" -v c="$check" -v cg="$checking" -v r="$retry" '
    $0 == i || $0 == a || $0 == "--" || $0 == "Quit Dilla" { print; next }
    $0 == c || $0 == cg || $0 == r { print "<update>"; upd = 1; next }
    !upd { if (last != "<instance>") print "<instance>"; last = "<instance>"; next }
  ' | tr '\n' '|')
  case "$got" in
    "$instances|--|$autostart|<update>|--|Quit Dilla|" | "$instances|<instance>|--|$autostart|<update>|--|Quit Dilla|") ;;
    *) fail Tray "the rows are '$got', not '$instances|[instances|]--|$autostart|<update>|--|Quit Dilla|'" ;;
  esac
  if tray_click 'Quit Dilla'; then
    wait_for 10 pid_gone || fail Tray "the app still runs 10 s after the tray's Quit Dilla"
  else
    fail Tray "the tray's Quit Dilla could not be clicked"
  fi
}

# ---------------------------------------------------------------------------------------------------------------------
# AX snapshots: one walk of every window of P, then bash picks elements from the file
# ---------------------------------------------------------------------------------------------------------------------

# snap <tag>: every element of every window of pid $PID (mac-ax.swift: the AX API by pid; System Events answers for
# the first of two same-named processes), one per line:
#   role<TAB>subrole<TAB>title<TAB>description<TAB>value<TAB>x<TAB>y<TAB>w<TAB>h<TAB>window
# into $OUT/ax-<tag>.txt (SNAP is its path). A window's own line has the role AXWindow.
SNAP=
snap() {
  SNAP=$OUT/ax-$1.txt
  deadline 60 "$BIN/mac-ax" dump "$PID" > "$SNAP" 2> "$SNAP.err" || { cat "$SNAP.err"; return 1; }
}

# pick <role regex> <name> [last]: the first (or last) element of $SNAP with a matching role and that title or
# description; prints its line.
pick() {
  awk -F '\t' -v r="^($1)\$" -v n="$2" -v which="${3:-first}" \
    '$1 ~ r && ($3 == n || $4 == n) { hit = $0; if (which == "first") { print; found = 1; exit } } END { if (!found && hit != "") print hit }' "$SNAP"
}
has() { [ -n "$(pick "$@")" ]; }

# click_line <element line>: a real click at the element's centre.
click_line() {
  local at
  # Fields 6-9 (awk: empty fields kept, unlike `read` with a tab IFS).
  at=$(awk -F '\t' '$8 + 0 > 0 && $9 + 0 > 0 { printf "%d %d", $6 + $8 / 2, $7 + $9 / 2 }' <<< "$1")
  [ -n "$at" ] || { echo "desktop-gpui-mac-ui: no frame to click in '$1'" >&2; return 1; }
  # The process in front first: a click on an inactive app only activates it, and the field would not take the keys.
  [ -z "$PID" ] || deadline 10 "$BIN/mac-ax" front "$PID" >/dev/null 2>&1
  # shellcheck disable=SC2086 # two numbers
  real_click $at
}

# tap <tag> <role regex> <name> [last]: snapshots until the element is there (20 s), then clicks it.
tap() {
  local tag=$1 line
  shift
  local end=$((SECONDS + 20))
  while :; do
    snap "$tag" || return 1
    line=$(pick "$@")
    [ -n "$line" ] && break
    [ "$SECONDS" -lt "$end" ] || { echo "desktop-gpui-mac-ui: no '$2' ($1) in $SNAP" >&2; return 1; }
    sleep 1
  done
  click_line "$line"
}

# await <tag> <secs> <role regex> <name>: snapshots until the element is there.
await() {
  local tag=$1 secs=$2
  shift 2
  local end=$((SECONDS + secs))
  while :; do
    snap "$tag" || return 1
    has "$@" && return 0
    [ "$SECONDS" -lt "$end" ] || return 1
    sleep 1
  done
}

# type_text <text>: brings pid $PID to the front, then types into its focused field.
type_text() {
  deadline 10 "$BIN/mac-ax" front "$PID" || return 1
  sleep 0.3
  deadline 30 osascript -e 'on run argv
tell application "System Events" to keystroke (item 1 of argv)
end run' "$1" >/dev/null
}

shot() { screencapture -x "$OUT/shot-$1.png"; }

# ---------------------------------------------------------------------------------------------------------------------
# The connected checks: a local dilla-testhost behind HTTPS, two profiles (DILLA_DATA_DIR)
# ---------------------------------------------------------------------------------------------------------------------

WORK=
HOST_PID=
PROXY_PID=
INVITES=()
DILLA_TLSPROXY=${DILLA_TLSPROXY:-}

stop_host() {
  [ -n "$HOST_PID" ] && kill -9 "$HOST_PID" 2>/dev/null
  if [ -n "$DILLA_TLSPROXY" ] && pgrep -f "$DILLA_TLSPROXY" >/dev/null; then sudo pkill -9 -f "$DILLA_TLSPROXY"; fi
  # Removing an admin trust setting can wait on an authorization prompt nobody answers: bounded, best effort (the
  # throwaway CA expires within a day, and a hosted runner is discarded after the job).
  if [ -n "$WORK" ] && [ -f "$WORK/ca.pem" ]; then deadline 15 sudo -n security remove-trusted-cert -d "$WORK/ca.pem" 2>/dev/null; fi
  HOST_PID= PROXY_PID=
  return 0
}

# On any exit: no app, host or proxy outlives the script (a helper left running would hold the step, and the macOS
# slot, until the job's timeout).
cleanup() {
  if app_running; then pkill -9 -f "$EXE"; fi
  stop_host
  jobs -p | xargs kill -9 2>/dev/null
  rm -rf "$HOME/dilla-mac-ui-profiles"
  return 0
}
trap cleanup EXIT

invites_ready() { grep -q 'DILLA_TESTKIT_INVITE=' "$WORK/host.log"; }
ca_ready() { sudo test -s "$WORK/ca.pem"; }

# A release build takes https instances only: dilla-testhost on 127.0.0.1:8443, dilla-tlsproxy on dilla.test:443 with a
# throwaway CA the System keychain trusts for the run.
start_host() {
  local missing=
  for v in DILLA_TESTHOST DILLA_CORE_WASI DILLA_TLSPROXY; do [ -x "${!v:-}" ] || [ -f "${!v:-}" ] || missing="$missing $v"; done
  [ -z "$missing" ] || { fail Host "set$missing (the workflow builds dilla-testhost, the WASI core and dilla-tlsproxy)"; return 1; }
  WORK=$(mktemp -d "${TMPDIR:-/tmp}/dilla-mac-ui.XXXXXX")
  grep -q ' dilla.test$' /etc/hosts || echo '127.0.0.1 dilla.test' | sudo tee -a /etc/hosts >/dev/null
  "$DILLA_TESTHOST" -data-dir "$WORK/host" -core "$DILLA_CORE_WASI" -log-level info > "$WORK/host.log" 2>&1 &
  HOST_PID=$!
  wait_for 60 invites_ready || { tail -n 30 "$WORK/host.log"; fail Host 'dilla-testhost printed no invite in 60 s'; return 1; }
  read -r -a INVITES <<< "$(sed -n 's/.*DILLA_TESTKIT_INVITE=\([^ ]*\).*/\1/p' "$WORK/host.log" | head -n 1 | tr ',' ' ')"
  sudo "$DILLA_TLSPROXY" -listen 127.0.0.1:443 -to 127.0.0.1:8443 -ca-out "$WORK/ca.pem" -hosts dilla.test > "$WORK/proxy.log" 2>&1 &
  PROXY_PID=$!
  wait_for 20 ca_ready || { cat "$WORK/proxy.log"; fail Host 'dilla-tlsproxy wrote no CA in 20 s'; return 1; }
  sudo chmod 644 "$WORK/ca.pem"
  sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain "$WORK/ca.pem" \
    || { fail Host 'the test CA could not be trusted'; return 1; }
  note "host up: https://dilla.test/ (${#INVITES[@]} invites)"
}

# The TOTP code for a base32 key (RFC 6238, SHA-1, 6 digits, 30 s) at the instance's time: dilla-testhost runs a fake
# clock (GET /debug/clock on its control listener), which stands still unless a test moves it.
totp() {
  local now
  now=$(curl -fsS --max-time 10 http://127.0.0.1:8444/debug/clock | sed -n 's/.*"now":\([0-9]*\).*/\1/p')
  [ -n "$now" ] || { echo "desktop-gpui-mac-ui: the host's clock could not be read" >&2; return 1; }
  python3 -I -c 'import base64,hmac,struct,sys
k=sys.argv[1].replace(" ","").replace("-","").upper(); k=base64.b32decode(k+"="*(-len(k)%8))
h=hmac.new(k,struct.pack(">Q",int(sys.argv[2])//30),"sha1").digest(); o=h[-1]&15
print("%06d"%((struct.unpack(">I",h[o:o+4])[0]&0x7fffffff)%1000000))' "$1" "$now"
}

# A dialog sheet or a print panel of P (AppKit's print operation runs a sheet on the window, or a panel without one).
print_ui_up() {
  snap print
  awk -F '\t' '$1 == "AXSheet" || ($1 == "AXWindow" && ($2 == "AXDialog" || $3 ~ /Print/)) { f = 1 } END { exit !f }' "$SNAP"
}

# onboard <who> <invite code> <username> <print: 0|1>: the picker's invite link (https), the confirm, the sign-up and
# its second factor, as a person does it. Copy is checked twice through the clipboard (the setup key, the backup codes);
# with print=1 the backup codes' Print opens AppKit's print sheet, and Cancel closes it.
onboard() {
  local who=$1 code=$2 user=$3 print=$4 step key codes host_title
  local L_INVITE L_JOIN
  L_INVITE=$(rust_const "$SRC/screens/instances/copy.rs" INVITE_LABEL)
  L_JOIN=$(rust_const "$SRC/screens/instances/copy.rs" INVITE_CONNECT)
  # The confirm dialog (an AXDialog in the picker): copy.rs NEW_TITLE for an instance this device does not know.
  host_title=$(rust_const "$SRC/screens/instances/copy.rs" NEW_TITLE)
  tap "$who-picker" 'AXTextField' "$L_INVITE" || { fail "$who: invite" "no '$L_INVITE' field"; return 1; }
  type_text "https://dilla.test/i/$code"
  tap "$who-picker-typed" 'AXButton' "$L_JOIN" || { fail "$who: invite" "no '$L_JOIN' button"; return 1; }
  # The https invite link is taken (the bug: the picker refused it): the confirm dialog asks to connect.
  if ! await "$who-confirm" 15 '.*' "$host_title"; then
    shot "$who-confirm"
    fail "$who: https invite" "no '$host_title' after Join with https://dilla.test/i/<code> (ax-$who-confirm.txt)"
    return 1
  fi
  tap "$who-confirm" 'AXButton' "$(rust_const "$SRC/screens/instances/copy.rs" CONFIRM_CONNECT)" last \
    || { fail "$who: https invite" 'no Connect in the confirm'; return 1; }
  # The sign-up, step by step, in whatever order it shows them (each step is recognised by its own control).
  local U P_ CREATE KEYS_ACK KEYS_NEXT APPF NEXT COPY CODE CODES_ACK DONE_ACK OPEN PRINT
  U=$(string_key onboarding.identity.username); P_=$(string_key onboarding.identity.password)
  CREATE=$(string_key onboarding.identity.submit)
  KEYS_ACK=$(string_key onboarding.keys.acknowledge); KEYS_NEXT=$(string_key onboarding.keys.submit)
  APPF=$(string_key factor.choose.app); NEXT=$(string_key factor.choose.next)
  COPY=$(string_key factor.totp.copyKey); CODE=$(string_key factor.totp.code)
  CODES_ACK=$(string_key factor.codes.ack); DONE_ACK=$(string_key onboarding.done.codesAck)
  OPEN=$(string_key onboarding.done.next); PRINT=$(string_key factor.codes.print)
  local done_identity=0 done_keys=0 done_app=0 done_totp=0 done_codes=0 n=0 end=$((SECONDS + 240)) line
  while [ "$SECONDS" -lt "$end" ]; do
    n=$((n + 1))
    snap "$who-signup-$n" || return 1
    note "$who: sign-up snapshot $n: $(awk -F '\t' '$1 == "AXWindow" { printf "[%s] ", $3 }' "$SNAP")$(awk -F '\t' '$1 != "AXWindow" && ($3 != "" || $4 != "") { n++ } END { print n + 0 }' "$SNAP") named elements"
    if has 'AXButton' "$(string_key boot.error.action)" && has 'AXButton' "$(string_key mobile.switchInstance)"; then
      # The failed-start splash ("dilla could not start", Error E_…): no step can follow.
      shot "$who-start-failed"
      "$BIN/mac-ocr" "$OUT/shot-$who-start-failed.png" | cut -f 2 | grep -i 'error' | head -n 3
      grep -i 'start failed\|keychain\|store' "$OUT/app-$who.err.log" | tail -n 5
      security list-keychains; security default-keychain; security show-keychain-info 2>&1
      fail "$who: sign-up" "the window shows a failed start: $("$BIN/mac-ocr" "$OUT/shot-$who-start-failed.png" | cut -f 2 | grep -o 'E_[A-Z_]*' | head -n 1) (shot-$who-start-failed.png)"
      return 1
    elif [ "$done_identity" = 0 ] && has 'AXTextField' "$(string_key onboarding.connect.invite)" && has 'AXButton' "$(string_key onboarding.next)"; then
      # "Join with an invite", the link already in its field (the picker passed it on).
      [ -n "$(pick 'AXTextField' "$(string_key onboarding.connect.invite)" | cut -f 5)" ] \
        || fail "$who: https invite" "the sign-up's invite field is empty: the picker did not pass the link on"
      click_line "$(pick 'AXButton' "$(string_key onboarding.next)")"
    elif [ "$done_identity" = 0 ] && has 'AXTextField' "$U"; then
      click_line "$(pick 'AXTextField' "$U")"; type_text "$user"
      click_line "$(pick 'AXTextField|AXSecureTextField' "$P_")"; type_text "correct horse battery staple"
      snap "$who-signup-$n-filled"
      click_line "$(pick 'AXButton' "$CREATE")" || fail "$who: sign-up" "no '$CREATE'"
      done_identity=1
    elif [ "$done_keys" = 0 ] && has 'AXCheckBox' "$KEYS_ACK"; then
      click_line "$(pick 'AXCheckBox' "$KEYS_ACK")"; sleep 0.5
      tap "$who-keys" 'AXButton' "$KEYS_NEXT" || fail "$who: sign-up" "no '$KEYS_NEXT'"
      done_keys=1
    elif [ "$done_app" = 0 ] && has '.*' "$APPF"; then
      click_line "$(pick 'AXRadioButton|AXButton|AXCheckBox|AXGroup' "$APPF")"; sleep 0.5
      tap "$who-factor" 'AXButton' "$NEXT" || fail "$who: sign-up" "no '$NEXT' after '$APPF'"
      done_app=1
    elif [ "$done_totp" -lt 3 ] && has 'AXTextField' "$CODE" && has 'AXButton' "$COPY"; then
      printf '' | pbcopy
      click_line "$(pick 'AXButton' "$COPY")"; sleep 1
      key=$(pbpaste)
      [[ "$key" =~ ^[A-Z2-7\ =-]{16,}$ ]] || { fail Copy "the setup key's Copy left '$key' on the clipboard"; return 1; }
      note "Copy: the setup key reached the clipboard (${#key} characters)"
      click_line "$(pick 'AXTextField' "$CODE")"
      deadline 10 osascript -e 'tell application "System Events" to keystroke "a" using command down' >/dev/null  # a retry replaces the last code
      type_text "$(totp "$key")"; sleep 0.5
      snap "$who-totp"
      line=$(pick 'AXButton' "$(string_key factor.totp.submit)")
      [ -n "$line" ] || line=$(pick 'AXButton' "$(string_key factor.totp.verify)")
      # Six digits submit the code by themselves; a Continue or Verify left on screen is pressed.
      [ -z "$line" ] || click_line "$line"
      done_totp=$((done_totp + 1))  # again (up to 3 codes) while the code field stays
    elif [ "$done_codes" = 0 ] && { has 'AXCheckBox' "$CODES_ACK" || has 'AXCheckBox' "$DONE_ACK"; }; then
      if has 'AXButton' "$(string_key factor.codes.copy)"; then
        printf '' | pbcopy
        click_line "$(pick 'AXButton' "$(string_key factor.codes.copy)")"; sleep 1
        codes=$(pbpaste)
        [ "$(grep -cE '[A-Za-z0-9]{4}' <<< "$codes")" -ge 4 ] || fail Copy "the backup codes' Copy left '$codes' on the clipboard"
        note "Copy: $(grep -cE '[A-Za-z0-9]{4}' <<< "$codes") backup codes reached the clipboard"
      fi
      if [ "$print" = 1 ]; then
        if has 'AXButton' "$PRINT"; then
          click_line "$(pick 'AXButton' "$PRINT")"
          if wait_for 15 print_ui_up; then
            shot "$who-print"; note 'Print: the print sheet is up'
            line=$(pick 'AXButton' Cancel last)
            if [ -n "$line" ]; then click_line "$line"; else se 10 'key code 53' >/dev/null; fi
            sleep 2; snap "$who-print-closed"
            ! awk -F '\t' '$1 == "AXSheet" { f = 1 } END { exit !f }' "$SNAP" || fail Print 'the print sheet is still up after Cancel'
          else
            shot "$who-print"; fail Print "no print sheet 15 s after '$PRINT' (ax-print.txt)"
          fi
        else
          fail Print "the backup codes have no '$PRINT' button"
        fi
        snap "$who-codes"
      fi
      line=$(pick 'AXCheckBox' "$CODES_ACK"); [ -n "$line" ] || line=$(pick 'AXCheckBox' "$DONE_ACK")
      click_line "$line"; sleep 0.5; snap "$who-codes-ticked"
      line=$(pick 'AXButton' "$OPEN"); [ -n "$line" ] || line=$(pick 'AXButton' "$(string_key factor.codes.next)")
      click_line "$line" || fail "$who: sign-up" 'no Continue after the backup codes'
      done_codes=1
    elif has 'AXButton' "$OPEN"; then
      click_line "$(pick 'AXButton' "$OPEN")"
    elif [ "$done_codes" = 1 ] && ! awk -F '\t' -v m="$(rust_const "$SRC/screens/instances/copy.rs" WINDOW_TITLE)" '$1 == "AXWindow" && $3 == m { f = 1 } END { exit !f }' "$SNAP" \
      && [ "$(awk -F '\t' '$1 == "AXWindow"' "$SNAP" | wc -l | tr -d ' ')" -ge 1 ]; then
      note "$who: signed up as $user"
      return 0
    fi
    sleep 2
  done
  shot "$who-signup-stuck"
  fail "$who: sign-up" "not through the sign-up in 240 s (last snapshot ax-$who-signup-$n.txt)"
  return 1
}

# After connect: one window (the picker closed: connect_closes_the_manager), at least the workspace minimum.
check_windows_after_connect() {
  local min_w min_h wins
  read -r min_w min_h <<< "$(sed -n 's/^pub const WORKSPACE_MIN: (f32, f32) = (\([0-9.]*\), \([0-9.]*\));$/\1 \2/p' "$SRC/screens/shell/window.rs")"
  [ -n "$min_w" ] || { fail Windows 'WORKSPACE_MIN is not in screens/shell/window.rs'; return; }
  sleep 3
  snap "$1-workspace"
  wins=$(awk -F '\t' '$1 == "AXWindow"' "$SNAP")
  note "$1 windows after connect:"; printf '%s\n' "$wins"
  [ "$(wc -l <<< "$wins" | tr -d ' ')" = 1 ] || fail Windows "$1 has $(wc -l <<< "$wins" | tr -d ' ') windows after connect, not 1 (the picker stays open?)"
  awk -F '\t' -v w="${min_w%.*}" -v h="${min_h%.*}" '$1 == "AXWindow" && ($8 + 0 < w || $9 + 0 < h) { bad = bad $3 " " $8 "x" $9 "; " } END { if (bad) { print bad; exit 1 } }' "$SNAP" \
    || fail Windows "$1's window is smaller than the workspace minimum ${min_w%.*}x${min_h%.*}"
  shot "$1-workspace"
}

# The status menu (the avatar block) and the server menu (the chevron) open on a click and expose their items.
check_popovers() {
  local who=$1 status label item
  # The avatar block: "<name>, your status: <status>" (user_panel.rs `status_menu_label`, after the name).
  status=$(awk -F '\t' '$1 == "AXButton" && index($3 $4, "your status: ") > 0 { print; exit }' "$SNAP")
  if [ -z "$status" ]; then
    fail Popovers "$who: no 'your status: …' button (the avatar block)"
  else
    click_line "$status"; sleep 1; snap "$who-status-menu"
    for item in presence.online presence.idle presence.dnd presence.invisible; do
      has 'AXMenuItem|AXMenuItemRadio|AXMenuItemCheckBox|AXMenuButton|AXButton|AXRadioButton|AXCheckBox' "$(string_key "$item")" \
        || fail Popovers "$who: the status menu has no '$(string_key "$item")' item after a click on the avatar (ax-$who-status-menu.txt)"
    done
    awk -F '\t' '$1 == "AXMenu"' "$SNAP" | grep -q . || fail Popovers "$who: no AXMenu after a click on the avatar"
    se 10 'key code 53' >/dev/null; sleep 0.5
  fi
  label=$(string_key admin.open)
  snap "$who-before-server-menu"
  if ! has 'AXButton' "$label" && has 'AXButton' "$(string_key shell.noServer.create)"; then
    # No server yet: start one (create.server.*), as a new account does; its channel list has the chevron.
    click_line "$(pick 'AXButton' "$(string_key shell.noServer.create)")"
    if tap "$who-create" 'AXTextField' "$(string_key create.server.name)"; then
      type_text "$who's server"
      tap "$who-create-named" 'AXButton' "$(string_key create.server.submit)" last || fail Popovers "$who: no '$(string_key create.server.submit)' in the new-server dialog"
      await "$who-before-server-menu" 20 'AXButton' "$label" || true
    else
      fail Popovers "$who: no '$(string_key create.server.name)' field after '$(string_key shell.noServer.create)'"
    fi
  fi
  if has 'AXButton' "$label"; then
    click_line "$(pick 'AXButton' "$label")"; sleep 1; snap "$who-server-menu"
    awk -F '\t' -v n="$label" '$1 == "AXMenu" && ($3 == n || $4 == n)' "$SNAP" | grep -q . \
      || fail Popovers "$who: no '$label' AXMenu after a click on the chevron (ax-$who-server-menu.txt)"
    awk -F '\t' '$1 ~ /^AXMenuItem/' "$SNAP" | grep -q . || fail Popovers "$who: the server menu has no items"
    se 10 'key code 53' >/dev/null; sleep 0.5
  else
    fail Popovers "$who: no '$label' chevron (no server on the start screen?) (ax-$who-before-server-menu.txt)"
  fi
}

# Settings → Appearance → Density: a click on Compact selects it (AXValue 1); Regular goes back.
check_segments() {
  local who=$1 compact regular line
  compact=$(string_key workspace.density.compact); regular=$(string_key workspace.density.regular)
  tap "$who-open-settings" 'AXButton' "$(string_key shell.rail.settings)" || { fail Segments "$who: no '$(string_key shell.rail.settings)' button"; return; }
  tap "$who-settings" 'AXButton|AXLink|AXRadioButton|AXTab|AXRow|AXStaticText|AXCell' "$(string_key settings.nav.appearance)" \
    || fail Segments "$who: no '$(string_key settings.nav.appearance)' in Settings (ax-$who-settings.txt)"
  if ! await "$who-appearance" 15 'AXRadioButton' "$compact"; then
    # The Appearance page by its row title.
    line=$(awk -F '\t' '$3 == "Appearance" || $4 == "Appearance" { print; exit }' "$SNAP")
    [ -n "$line" ] && click_line "$line"
    await "$who-appearance" 10 'AXRadioButton' "$compact" || { fail Segments "$who: no '$compact' segment in Settings (ax-$who-appearance.txt)"; return; }
  fi
  click_line "$(pick 'AXRadioButton' "$compact")"; sleep 1; snap "$who-compact"
  [[ "$(pick 'AXRadioButton' "$compact" | cut -f 5)" =~ ^(1|true)$ ]] || fail Segments "$who: '$compact' is not selected after a click (AXValue '$(pick 'AXRadioButton' "$compact" | cut -f 5)')"
  click_line "$(pick 'AXRadioButton' "$regular")"; sleep 1; snap "$who-regular"
  [[ "$(pick 'AXRadioButton' "$regular" | cut -f 5)" =~ ^(1|true)$ ]] || fail Segments "$who: '$regular' is not selected after a click"
  se 10 'key code 53' >/dev/null
}

# The log file after a launch: ~/Library/Logs/Dilla/dilla-desktop.log with this version's start line.
check_log_file() {
  local log=$HOME/Library/Logs/Dilla/dilla-desktop.log version
  version=$(sed -n 's/^version = "\(.*\)"$/\1/p' "$ROOT/clients/desktop/dilla-desktop/Cargo.toml" | head -n 1)
  [ -f "$log" ] || { fail 'Log file' "no $log after the launch"; return; }
  cp "$log" "$OUT/dilla-desktop.log"
  grep -q "Dilla $version started; log file $log" "$log" || fail 'Log file' "$log has no 'Dilla $version started' line"
}

# alice is the default profile (as a person runs the app); bob a second one beside it (DILLA_DATA_DIR, a private
# directory under the real HOME: a fake HOME breaks the keychain).
check_connected() {
  start_host || return
  local b=$HOME/dilla-mac-ui-profiles/bob pid_a= pid_b=
  rm -rf "$HOME/dilla-mac-ui-profiles"
  mkdir -p "$b"
  chmod 700 "$HOME/dilla-mac-ui-profiles" "$b"
  if launch alice; then
    pid_a=$PID
    check_keychain
    if onboard alice "${INVITES[0]}" alice 1; then
      check_windows_after_connect alice
      check_popovers alice
      check_segments alice
    fi
  fi
  if launch bob "DILLA_DATA_DIR=$b"; then
    pid_b=$PID
    onboard bob "${INVITES[1]}" bob 0 && check_windows_after_connect bob
  fi
  note "profiles: alice $pid_a, bob $pid_b"
  for PID in $pid_a $pid_b; do kill "$PID" 2>/dev/null; done
  stop_host
}

# ---------------------------------------------------------------------------------------------------------------------

note "app $APP, out $OUT, $(sw_vers -productVersion), $(uname -m)"
BIN=$OUT/bin
mkdir -p "$BIN"
swiftc -O -o "$BIN/mac-ocr" "$ROOT/scripts/mac-ocr.swift" || { echo '::error::setup: scripts/mac-ocr.swift does not compile'; exit 1; }
for tool in mac-windows mac-click mac-ax; do
  swiftc -O -o "$BIN/$tool" "$ROOT/scripts/$tool.swift" || { echo "::error::setup: scripts/$tool.swift does not compile"; exit 1; }
done
app_gone || { echo "::error::setup: $EXE already runs"; exit 1; }
/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -f "$APP" \
  || fail setup "lsregister -f $APP failed"

if launch first; then
  check_keychain
  check_log_file
  check_text_and_title
  check_ax_tree
  check_traffic_lights
  check_app_menu
  check_cmd_q
fi
app_gone || { pkill -f "$EXE"; wait_for 10 app_gone; }
if launch second; then
  check_single_instance
  check_autostart
  check_tray
fi
app_gone || { pkill -f "$EXE"; wait_for 10 app_gone; }
[ "$CONNECTED" = 0 ] || check_connected
rm -rf "$BIN"
[ "$FAILED" = 0 ] && note 'all checks passed'
exit "$FAILED"
