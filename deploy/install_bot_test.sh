#!/usr/bin/env bash
# Rootless integration tests for install-bot.sh. Host commands are stubbed and
# release assets come from a local file:// directory.
set -eu
HERE="$(cd "$(dirname "$0")" && pwd)"
INSTALL="$HERE/install-bot.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
STUBS="$TMP/stubs"
mkdir -p "$STUBS"
LOG="$TMP/calls.log"
fail() { echo "FAIL $*" >&2; exit 1; }
pass() { echo "ok   $*"; }
cat >"$STUBS/id" <<'STUB'
#!/bin/sh
if [ "$1" = -u ]; then echo 0; exit 0; fi
[ "${STUB_ACCOUNT_EXISTS:-}" = 1 ]
STUB
cat >"$STUBS/useradd" <<'STUB'
#!/bin/sh
echo "useradd $*" >>"$BOT_TEST_LOG"
STUB
cat >"$STUBS/install" <<'STUB'
#!/bin/sh
echo "install $*" >>"$BOT_TEST_LOG"
args=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	-o | -g) shift 2 ;;
	*) args="$args '$1'"; shift ;;
esac
done
eval exec /usr/bin/install $args
STUB
cat >"$STUBS/chown" <<'STUB'
#!/bin/sh
echo "chown $*" >>"$BOT_TEST_LOG"
STUB
cat >"$STUBS/runuser" <<'STUB'
echo "runuser $*" >>"$BOT_TEST_LOG"
while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done
shift
case "$1" in */env) shift; export "$1"; shift ;; esac
case "$1" in
*/dillabot) shift; exec "$FAKE_BOT" "$@" ;;
*) exec "$@" ;;
esac
STUB
cat >"$STUBS/systemctl" <<'STUB'
echo "systemctl $*" >>"$BOT_TEST_LOG"
if [ "$1" = start ] && [ -n "${BOT_TEST_FAIL_START_FIRST:-}" ]; then
	n=0; [ ! -f "$BOT_TEST_START_COUNT" ] || n="$(cat "$BOT_TEST_START_COUNT")"
	n=$((n + 1)); echo "$n" >"$BOT_TEST_START_COUNT"
	[ "$n" -gt 1 ] || exit 1
fi
if [ "$1" = show-environment ] && [ -n "${BOT_TEST_SERVICE_PATH:-}" ]; then echo "PATH=$BOT_TEST_SERVICE_PATH"; fi
if [ "$1" = is-active ]; then [ "${BOT_TEST_ACTIVE:-1}" = 1 ]; fi
STUB
cat >"$STUBS/dillad" <<'STUB'
#!/bin/sh
echo "dillad $*" >>"$BOT_TEST_LOG"
[ "$1" = admin ] && [ "$2" = bot-runtime ] && [ "$3" = create ] || exit 2
for arg in "$@"; do case "$arg" in --out=*) out="${arg#*=}" ;; esac; done
printf 'dbr_c29tZS1sb25nLXJhbmRvbS10ZXN0LXZhbHVl\n' >"$out"
echo 'created bot runtime 01010101010101010101010101010101; secret written to protected file'
STUB
cat >"$STUBS/fake-bot" <<'STUB'
#!/bin/sh
[ "$1" = doctor ] || exit 2
if [ -n "${BOT_TEST_DOCTOR_FAIL_AFTER_FIRST:-}" ]; then
	n=0; [ ! -f "$BOT_TEST_DOCTOR_COUNT" ] || n="$(cat "$BOT_TEST_DOCTOR_COUNT")"
	n=$((n + 1)); echo "$n" >"$BOT_TEST_DOCTOR_COUNT"
	[ "$n" -lt 2 ] || exit 1
fi
if [ -n "${BOT_TEST_DOCTOR_PATH_LOG:-}" ]; then
	echo "$PATH" >"$BOT_TEST_DOCTOR_PATH_LOG"
	if command -v yt-dlp >/dev/null 2>&1 && command -v node >/dev/null 2>&1; then echo 'youtube_dependencies OK'; else echo 'youtube_dependencies WARN: yt-dlp unavailable; node unavailable'; fi
fi
echo 'runtime_credential OK'
echo 'native_helper OK'
echo 'wasi_identity_module OK'
echo 'state_directory OK'
STUB
chmod +x "$STUBS"/*

ROOT="$TMP/root"
mkdir -p "$ROOT/etc/dilla" "$ROOT/usr/local/bin" "$ROOT/etc/ssl/certs"
chmod 0750 "$ROOT/etc/dilla"
printf 'server config\n' >"$ROOT/etc/dilla/dilla.toml"
cp "$STUBS/dillad" "$ROOT/usr/local/bin/dillad"
chmod +x "$ROOT/usr/local/bin/dillad"
FAKE_BOT="$STUBS/fake-bot"
export FAKE_BOT
REL="$TMP/release"
mkdir -p "$REL"
printf 'bot-binary\n' >"$REL/dillabot-linux-amd64"
printf 'native-helper\n' >"$REL/dilla-bot-core-linux-amd64"
printf 'module\n' >"$REL/dilla_core_wasi.wasm"
(cd "$REL" && sha256sum dillabot-linux-amd64 dilla-bot-core-linux-amd64 dilla_core_wasi.wasm >SHA256SUMS)

BASE="--base-url=https://chat.example.org --instance-id=01010101010101010101010101010101 --dillad-config=$ROOT/etc/dilla/dilla.toml --release-url=file://$REL"
# shellcheck disable=SC2086
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" "$INSTALL" $BASE --check-args >"$TMP/out" 2>"$TMP/err"; then
	[ ! -s "$LOG" ] || fail "--check-args ran a host command"
	[ ! -e "$ROOT/etc/systemd/system/dillabot.service" ] || fail "--check-args wrote files"
	pass "--check-args validates without host changes"
else fail "--check-args rejected valid arguments: $(cat "$TMP/err")"; fi

reject_args() { # NAME ARGUMENTS...
	name="$1"; shift
	if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" "$INSTALL" "$@" --check-args >"$TMP/out" 2>"$TMP/err"; then
		fail "$name was accepted"
	fi
	[ ! -s "$LOG" ] || fail "$name ran a host command"
	pass "$name is refused before host changes"
}
# The origin is constrained to an HTTPS authority and the id must be a nonzero
# canonical 16-byte hex string so neither value can corrupt generated TOML.
# shellcheck disable=SC2086
reject_args "an instance ID of all zeroes" $BASE --instance-id=00000000000000000000000000000000
# shellcheck disable=SC2086
reject_args "a server URL with a path" $BASE --base-url=https://chat.example.org/v1
# shellcheck disable=SC2086
reject_args "a nonnumeric server URL port" $BASE --base-url=https://chat.example.org:admin
# Full local assets are accepted with checksums without requiring a release tag.
# shellcheck disable=SC2086
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" "$INSTALL" $BASE --binary="$REL/dillabot-linux-amd64" --helper="$REL/dilla-bot-core-linux-amd64" --module="$REL/dilla_core_wasi.wasm" --sums="$REL/SHA256SUMS" --check-args >"$TMP/out" 2>"$TMP/err"; then
	pass "local assets with SHA256SUMS need no release tag"
else fail "local assets with checksums were rejected: $(cat "$TMP/err")"; fi
# shellcheck disable=SC2086
reject_args "local assets without checksums" $BASE --binary="$REL/dillabot-linux-amd64" --helper="$REL/dilla-bot-core-linux-amd64" --module="$REL/dilla_core_wasi.wasm"

# shellcheck disable=SC2086
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" "$INSTALL" $BASE >"$TMP/out" 2>"$TMP/err"; then :; else fail "guided install failed: $(cat "$TMP/err")"; fi
[ -x "$ROOT/usr/local/bin/dillabot" ] || fail "dillabot binary missing"
[ -x "$ROOT/usr/local/libexec/dilla-bot-core" ] || fail "native helper missing"
[ -s "$ROOT/usr/local/lib/dilla/dilla_core_wasi.wasm" ] || fail "WASI module missing"
[ "$(stat -c %a "$ROOT/etc/dillabot/runtime.token")" = 600 ] || fail "runtime credential mode is not 0600"
[ "$(stat -c %a "$ROOT/etc/dillabot")" = 750 ] || fail "Bot config directory mode is not 0750"
[ "$(stat -c %a "$ROOT/etc/dillabot/bot.toml")" = 640 ] || fail "bot config mode is not 0640"
[ "$(stat -c %a "$ROOT/var/lib/dillabot")" = 700 ] || fail "state directory mode is not 0700"
[ "$(stat -c %a "$ROOT/etc/dilla")" = 750 ] || fail "server config directory mode was changed"
grep -q 'install -m 0600 -o dillabot -g dillabot .*runtime.token' "$LOG" || fail "runtime credential ownership flags are wrong"
grep -q 'install -m 0640 -o root -g dillabot .*bot.toml' "$LOG" || fail "Bot config ownership flags are wrong"
grep -q 'useradd --system --user-group --no-create-home --home-dir /var/lib/dillabot --shell /usr/sbin/nologin dillabot' "$LOG" || fail "dedicated service user was not created"
[ "$(cat "$ROOT/etc/dillabot/bot.toml")" = "$(cat <<CONFIG
base_url = "https://chat.example.org"
instance_id = "01010101010101010101010101010101"
runtime_credential_file = "$ROOT/etc/dillabot/runtime.token"
helper_executable = "/usr/local/libexec/dilla-bot-core"
wasi_module = "/usr/local/lib/dilla/dilla_core_wasi.wasm"
state_dir = "/var/lib/dillabot"
reconcile_interval = "30s"
CONFIG
)" ] || fail "Bot config content differs from expected"
cmp -s "$HERE/dillabot.service" "$ROOT/etc/systemd/system/dillabot.service" || fail "installed systemd unit differs from deploy/dillabot.service"
grep -q '^DevicePolicy=closed$' "$ROOT/etc/systemd/system/dillabot.service" || fail "Bot unit does not restrict device access"
if grep -Eq '^(PrivateDevices|RestrictSUIDSGID)=yes$' "$ROOT/etc/systemd/system/dillabot.service"; then fail "Bot unit blocks the nested YouTube sandbox"; fi
grep -q '^dillad admin bot-runtime create --config=' "$LOG" || fail "runtime credential was not created via dillad admin"
grep -q '^systemctl enable --now dillabot$' "$LOG" || fail "service was not enabled"
if grep -q 'dbr_' "$LOG" "$TMP/err" "$TMP/out"; then fail "runtime credential leaked to output"; fi
pass "guided install verifies assets, protects files, creates runtime credential and enables unit"

# Upgrade uses a fresh release but leaves credentials, config and encrypted state untouched.
mkdir -m 0700 "$ROOT/var/lib/dillabot/install-sentinel"
printf 'encrypted-state-marker\n' >"$ROOT/var/lib/dillabot/install-sentinel/keep"
chmod 0600 "$ROOT/var/lib/dillabot/install-sentinel/keep"
CONFIG_BEFORE="$(sha256sum "$ROOT/etc/dillabot/bot.toml")"
CREDENTIAL_BEFORE="$(sha256sum "$ROOT/etc/dillabot/runtime.token")"
STATE_BEFORE="$(sha256sum "$ROOT/var/lib/dillabot/install-sentinel/keep")"
REL2="$TMP/release2"
mkdir -p "$REL2"
printf 'bot-binary-v2\n' >"$REL2/dillabot-linux-amd64"
printf 'native-helper-v2\n' >"$REL2/dilla-bot-core-linux-amd64"
printf 'module-v2\n' >"$REL2/dilla_core_wasi.wasm"
(cd "$REL2" && sha256sum dillabot-linux-amd64 dilla-bot-core-linux-amd64 dilla_core_wasi.wasm >SHA256SUMS)
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" BOT_TEST_DATE_COUNT="$TMP/date-count" STUB_ACCOUNT_EXISTS=1 "$INSTALL" --upgrade --release-url="file://$REL2" >"$TMP/out" 2>"$TMP/err"; then :; else fail "Bot upgrade failed: $(cat "$TMP/err")"; fi
[ "$(cat "$ROOT/usr/local/bin/dillabot")" = bot-binary-v2 ] || fail "upgrade did not replace dillabot"
[ "$(sha256sum "$ROOT/etc/dillabot/bot.toml")" = "$CONFIG_BEFORE" ] || fail "upgrade changed bot.toml"
[ "$(sha256sum "$ROOT/etc/dillabot/runtime.token")" = "$CREDENTIAL_BEFORE" ] || fail "upgrade changed runtime credential"
[ "$(sha256sum "$ROOT/var/lib/dillabot/install-sentinel/keep")" = "$STATE_BEFORE" ] || fail "upgrade changed Bot state"
BACKUPS=("$ROOT"/var/backups/dillabot/*)
[ "${#BACKUPS[@]}" -eq 1 ] || fail "upgrade did not retain one pre-upgrade backup"
[ -s "${BACKUPS[0]}/state/install-sentinel/keep" ] || fail "pre-upgrade backup omitted Bot state"
[ "$(stat -c %a "${BACKUPS[0]}")" = 700 ] || fail "pre-upgrade backup directory is not private"
grep -q '^systemctl start dillabot$' "$LOG" || fail "active service was not restarted after upgrade"
[ "$(grep -c '^dillad admin bot-runtime create ' "$LOG")" = 1 ] || fail "upgrade unexpectedly created another runtime credential"
pass "upgrade preserves config, credential and state with a root-only rollback copy"

# A failed upgraded doctor restores old binaries and state, then restarts the prior service.
REL3="$TMP/release3"
mkdir -p "$REL3"
printf 'bot-binary-v3\n' >"$REL3/dillabot-linux-amd64"
printf 'native-helper-v3\n' >"$REL3/dilla-bot-core-linux-amd64"
printf 'module-v3\n' >"$REL3/dilla_core_wasi.wasm"
(cd "$REL3" && sha256sum dillabot-linux-amd64 dilla-bot-core-linux-amd64 dilla_core_wasi.wasm >SHA256SUMS)
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" BOT_TEST_DATE_COUNT="$TMP/date-count" BOT_TEST_DOCTOR_COUNT="$TMP/doctor-count" BOT_TEST_DOCTOR_FAIL_AFTER_FIRST=1 STUB_ACCOUNT_EXISTS=1 "$INSTALL" --upgrade --release-url="file://$REL3" >"$TMP/out" 2>"$TMP/err"; then fail "upgrade with failed doctor succeeded"; fi
[ "$(cat "$ROOT/usr/local/bin/dillabot")" = bot-binary-v2 ] || fail "failed upgrade did not restore previous binary"
[ "$(cat "$ROOT/usr/local/libexec/dilla-bot-core")" = native-helper-v2 ] || fail "failed upgrade did not restore previous helper"
[ "$(cat "$ROOT/usr/local/lib/dilla/dilla_core_wasi.wasm")" = module-v2 ] || fail "failed upgrade did not restore previous WASI module"
cmp -s "$HERE/dillabot.service" "$ROOT/etc/systemd/system/dillabot.service" || fail "failed upgrade did not restore previous unit"
[ "$(cat "$ROOT/var/lib/dillabot/install-sentinel/keep")" = encrypted-state-marker ] || fail "failed upgrade did not restore Bot state"
grep -q 'restored the previous files and state' "$TMP/err" || fail "failed upgrade did not report rollback: $(cat "$TMP/err")"
pass "doctor failure rolls back Bot binaries and local state"

# A service start failure also restores the old files before starting the prior service.
REL4="$TMP/release4"
mkdir -p "$REL4"
printf 'bot-binary-v4\n' >"$REL4/dillabot-linux-amd64"
printf 'native-helper-v4\n' >"$REL4/dilla-bot-core-linux-amd64"
printf 'module-v4\n' >"$REL4/dilla_core_wasi.wasm"
(cd "$REL4" && sha256sum dillabot-linux-amd64 dilla-bot-core-linux-amd64 dilla_core_wasi.wasm >SHA256SUMS)
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" BOT_TEST_DATE_COUNT="$TMP/date-count" BOT_TEST_START_COUNT="$TMP/start-count" BOT_TEST_FAIL_START_FIRST=1 STUB_ACCOUNT_EXISTS=1 "$INSTALL" --upgrade --release-url="file://$REL4" >"$TMP/out" 2>"$TMP/err"; then fail "upgrade with failed service start succeeded"; fi
[ "$(cat "$ROOT/usr/local/bin/dillabot")" = bot-binary-v2 ] || fail "start failure did not restore previous binary"
[ "$(cat "$ROOT/var/lib/dillabot/install-sentinel/keep")" = encrypted-state-marker ] || fail "start failure did not restore Bot state"
grep -q 'upgraded Bot service did not remain active' "$TMP/err" || fail "start failure did not report rollback"
[ "$(cat "$TMP/start-count")" = 2 ] || fail "old service was not started after rollback"
pass "service start failure rolls back Bot files and state"

# Pre-upgrade copies are pruned only after a successful upgrade: the 3 newest
# stay (the new one included) and only the installer's own plain directories go.
BP="$ROOT/var/backups/dillabot"
rm -rf "$BP"
mkdir -m 0700 "$BP"
for n in 1 2 3 4 5; do mkdir -m 0700 "$BP/2020010${n}T000000Z-aaaaaaa$n"; done
mkdir -m 0700 "$BP/notes-from-operator"
mkdir -m 0700 "$TMP/link-target"
ln -s "$TMP/link-target" "$BP/20200101T000000Z-linkaaaa"
printf 'x\n' >"$BP/20200101T000000Z-fileaaaa"
sleep 1
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" STUB_ACCOUNT_EXISTS=1 "$INSTALL" --upgrade --release-url="file://$REL2" >"$TMP/out" 2>"$TMP/err"; then :; else fail "pruning upgrade failed: $(cat "$TMP/err")"; fi
for n in 1 2 3; do [ ! -e "$BP/2020010${n}T000000Z-aaaaaaa$n" ] || fail "old copy $n was not pruned"; done
for n in 4 5; do [ -d "$BP/2020010${n}T000000Z-aaaaaaa$n" ] || fail "newest old copy $n was pruned"; done
grep -q "removed old pre-upgrade copy $BP/20200101T000000Z-aaaaaaa1" "$TMP/err" || fail "pruned path was not logged"
[ "$(find "$BP" -mindepth 1 -maxdepth 1 -type d -name '2???????T??????Z-????????' | wc -l)" -eq 3 ] || fail "upgrade did not leave exactly 3 copies"
[ -d "$BP/notes-from-operator" ] || fail "non-matching directory was pruned"
[ -L "$BP/20200101T000000Z-linkaaaa" ] && [ -d "$TMP/link-target" ] || fail "symlink or its target was pruned"
[ -f "$BP/20200101T000000Z-fileaaaa" ] || fail "plain file was pruned"
pass "upgrade keeps the 3 newest copies and leaves foreign entries alone"

# A failed upgrade prunes nothing, however many copies exist.
BEFORE_COUNT="$(find "$BP" -mindepth 1 -maxdepth 1 | wc -l)"
rm -f "$TMP/doctor-count"
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" BOT_TEST_DOCTOR_COUNT="$TMP/doctor-count" BOT_TEST_DOCTOR_FAIL_AFTER_FIRST=1 STUB_ACCOUNT_EXISTS=1 "$INSTALL" --upgrade --keep-backups 1 --release-url="file://$REL3" >"$TMP/out" 2>"$TMP/err"; then fail "upgrade with failed doctor succeeded"; fi
[ "$(find "$BP" -mindepth 1 -maxdepth 1 | wc -l)" -eq $((BEFORE_COUNT + 1)) ] || fail "failed upgrade pruned copies"
if grep -q 'removed old pre-upgrade copy' "$TMP/err"; then fail "failed upgrade logged a prune"; fi
pass "failed upgrade prunes nothing"

# --keep-backups 1 keeps only the new copy.
sleep 1
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" STUB_ACCOUNT_EXISTS=1 "$INSTALL" --upgrade --keep-backups 1 --release-url="file://$REL3" >"$TMP/out" 2>"$TMP/err"; then :; else fail "keep-backups 1 upgrade failed: $(cat "$TMP/err")"; fi
[ "$(find "$BP" -mindepth 1 -maxdepth 1 -type d -name '2???????T??????Z-????????' | wc -l)" -eq 1 ] || fail "--keep-backups 1 did not keep only the new copy"
[ "$(cat "$ROOT/usr/local/bin/dillabot")" = bot-binary-v3 ] || fail "keep-backups upgrade did not install v3"
pass "--keep-backups 1 keeps only the new copy"

# The copy this run made is never pruned, even when another copy's name sorts after it
# (two copies in one UTC second order by their random suffix).
mkdir -m 0700 "$BP/20991231T235959Z-zzzzzzzz"
sleep 1
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" STUB_ACCOUNT_EXISTS=1 "$INSTALL" --upgrade --keep-backups 1 --release-url="file://$REL2" >"$TMP/out" 2>"$TMP/err"; then :; else fail "later-sorting copy upgrade failed: $(cat "$TMP/err")"; fi
NEW_COPY="$(sed -n 's|.*cold pre-upgrade copy retained at \([^ ]*\) .*|\1|p' "$TMP/err")"
[ -n "$NEW_COPY" ] && [ -d "$NEW_COPY" ] || fail "the new copy was pruned behind a later-sorting name"
[ ! -e "$BP/20991231T235959Z-zzzzzzzz" ] || fail "the later-sorting old copy was kept over the new one"
[ "$(find "$BP" -mindepth 1 -maxdepth 1 -type d -name '2???????T??????Z-????????' | wc -l)" -eq 1 ] || fail "--keep-backups 1 left more than the new copy"
pass "the new copy survives a later-sorting name"

: >"$LOG"
reject_args "--keep-backups 0" --upgrade --keep-backups 0
reject_args "a negative --keep-backups" --upgrade --keep-backups=-1
reject_args "a nonnumeric --keep-backups" --upgrade --keep-backups many
reject_args "an empty --keep-backups" --upgrade --keep-backups=

# A second guided first-install run must preserve the existing secret/state and refuse.
BEFORE="$(sha256sum "$ROOT/etc/dillabot/runtime.token")"
# shellcheck disable=SC2086
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$ROOT" BOT_TEST_LOG="$LOG" "$INSTALL" $BASE >"$TMP/out" 2>"$TMP/err"; then fail "rerun replaced an existing bot install"; fi
[ "$(sha256sum "$ROOT/etc/dillabot/runtime.token")" = "$BEFORE" ] || fail "refused rerun changed credential"
grep -q 'files or state already exist' "$TMP/err" || fail "rerun refusal was not explained"
pass "rerun refuses without replacing Bot credential"

# The doctor's dependency check runs with the service's PATH, not the caller's:
# `pct exec` has no /usr/local/bin, while systemd's default PATH does.
SVCBIN="$TMP/service-bin"
mkdir -p "$SVCBIN"
printf '#!/bin/sh\n' >"$SVCBIN/yt-dlp"
printf '#!/bin/sh\n' >"$SVCBIN/node"
chmod +x "$SVCBIN/yt-dlp" "$SVCBIN/node"
PROOT="$TMP/path-root"
mkdir -p "$PROOT/etc/dilla" "$PROOT/usr/local/bin" "$PROOT/etc/ssl/certs"
chmod 0750 "$PROOT/etc/dilla"
printf 'server config\n' >"$PROOT/etc/dilla/dilla.toml"
cp "$STUBS/dillad" "$PROOT/usr/local/bin/dillad"
DOCTOR_PATH_LOG="$TMP/doctor-path"
# shellcheck disable=SC2086
if PATH="$STUBS:/usr/bin:/bin" DILLA_INSTALL_ROOT="$PROOT" BOT_TEST_LOG="$LOG" BOT_TEST_DOCTOR_PATH_LOG="$DOCTOR_PATH_LOG" BOT_TEST_SERVICE_PATH="$SVCBIN:/usr/bin" "$INSTALL" --base-url=https://chat.example.org --instance-id=01010101010101010101010101010101 --dillad-config="$PROOT/etc/dilla/dilla.toml" --release-url="file://$REL" >"$TMP/out" 2>"$TMP/err"; then :; else fail "install with a minimal caller PATH failed: $(cat "$TMP/err")"; fi
[ "$(cat "$DOCTOR_PATH_LOG")" = "$SVCBIN:/usr/bin" ] || fail "doctor ran with PATH $(cat "$DOCTOR_PATH_LOG"), not the service PATH"
grep -q "service PATH $SVCBIN:/usr/bin" "$TMP/err" || fail "installer did not name the PATH it used for the doctor"
grep -q '^systemctl show-environment$' "$LOG" || fail "installer did not ask systemd for its default PATH"
pass "the doctor's dependency check uses the service PATH, not the caller's"
# A PATH set by the installed unit wins over systemd's default.
printf '[Service]\nEnvironment="PATH=%s"\n' "$SVCBIN" >"$TMP/unit-with-path"
eval "$(sed -n '/^service_path() {/,/^}/p' "$INSTALL")"
UNIT="$TMP/unit-with-path"
[ "$(PATH="$STUBS:/usr/bin:/bin" BOT_TEST_LOG="$LOG" BOT_TEST_SERVICE_PATH=/elsewhere service_path)" = "$SVCBIN" ] || fail "the unit's Environment=PATH is not read"
UNIT="$TMP/no-such-unit"
[ "$(PATH="$STUBS:/usr/bin:/bin" BOT_TEST_LOG="$LOG" service_path)" = /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin ] || fail "the documented systemd PATH is not the last fallback"
pass "an Environment=PATH in the unit is honoured"

# A tampered release artifact is rejected before account, state or service changes.
BAD="$TMP/bad-release"
cp -a "$REL" "$BAD"
printf 'tamper' >>"$BAD/dillabot-linux-amd64"
BADROOT="$TMP/bad-root"
mkdir -p "$BADROOT/etc/dilla" "$BADROOT/usr/local/bin"
printf 'server config\n' >"$BADROOT/etc/dilla/dilla.toml"
cp "$ROOT/usr/local/bin/dillad" "$BADROOT/usr/local/bin/dillad"
# shellcheck disable=SC2086
if PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$BADROOT" BOT_TEST_LOG="$LOG" "$INSTALL" --base-url=https://chat.example.org --instance-id=01010101010101010101010101010101 --dillad-config="$BADROOT/etc/dilla/dilla.toml" --release-url="file://$BAD" >"$TMP/out" 2>"$TMP/err"; then fail "tampered asset was installed"; fi
[ ! -e "$BADROOT/usr/local/bin/dillabot" ] || fail "checksum failure installed binary"
[ ! -e "$BADROOT/etc/systemd/system/dillabot.service" ] || fail "checksum failure installed service"
grep -q 'checksum mismatch' "$TMP/err" || fail "checksum failure was not reported"
pass "checksum failure changes no service files"
