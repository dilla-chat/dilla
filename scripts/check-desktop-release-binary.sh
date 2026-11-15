#!/usr/bin/env bash
# Fails when a dilla-desktop release binary carries the test-only code: the `automation` feature (the stdin/stdout
# automation interface, entered with `--automation`, on GPUI's test-support renderer) or the `test-hooks` feature
# (`AppView::forget_from_forced`, which signs out and wipes). A release builds `-p dilla-desktop` with default features
# only; this checks the result instead of assuming it (GPUI release plan, Task 1).
#
# The markers are searched in the raw bytes. Symbol names (`automation::run`, legacy-mangled `10automation3run`,
# `forget_from_forced`) exist only in an unstripped binary, so run this before `strip` too. The automation module's
# own strings survive strip, measured on a stripped `--features automation` build (2026-11-15): its source path in
# panic locations, its type names, its protocol name and its environment variables. `forget_from_forced` has no
# caller in the app, so a release build's linker drops it whatever the features.
# Usage: check-desktop-release-binary.sh <binary>
# Exit: 0 clean, 1 a marker is present, 2 usage or tooling error (the same contract as check-glibc-max.sh).
set -uo pipefail
[ "$#" -eq 1 ] || { echo "check-desktop-release-binary: usage: check-desktop-release-binary.sh <binary>" >&2; exit 2; }
bin=$1
[ -f "$bin" ] || { echo "check-desktop-release-binary: $bin does not exist" >&2; exit 2; }
[ -r "$bin" ] || { echo "check-desktop-release-binary: $bin is not readable" >&2; exit 2; }
command -v grep >/dev/null 2>&1 || { echo "check-desktop-release-binary: grep is not installed" >&2; exit 2; }
status=0
for marker in 'automation::run' '10automation3run' 'test_support' 'forget_from_forced' '--automation' \
  'dilla_desktop::automation::' 'dilla-desktop/src/automation.rs' 'dilla-automation-interface' 'DILLA_AUTOMATION_'; do
  grep -aqF -e "$marker" -- "$bin"
  case $? in
    0) echo "check-desktop-release-binary: $bin carries \`$marker\` (a test-only feature is compiled in)" >&2; status=1 ;;
    1) ;;
    *) echo "check-desktop-release-binary: grep failed on $bin" >&2; exit 2 ;;
  esac
done
[ "$status" -ne 0 ] || echo "check-desktop-release-binary: $bin carries no test-only code"
exit "$status"
