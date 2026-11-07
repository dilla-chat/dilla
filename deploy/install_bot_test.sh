#!/usr/bin/env bash
# Rootless integration tests for install-bot.sh, the Bot's upgrade path. Since Bot v2 the Bot ships with dillad and
# runs as its supervised child; install-bot.sh moves a v1 installation (dillabot.service, /etc/dillabot) under dillad
# and upgrades the Bot binaries there. Host commands are stubbed, release assets come from a local file:// directory,
# and /proc and the cgroup tree are fakes under the install root that the systemctl stub fills in when dilla restarts.
#
# SC2016: the stub bodies are single-quoted on purpose, to expand when the stub runs.
# shellcheck disable=SC2016
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
# The Bot must keep its pid this long under dilla.service to count as healthy, and may take this long to appear; keep
# the tests quick.
export DILLA_BOT_HEALTH_SECONDS=1 DILLA_BOT_START_SECONDS=1
# The dillabot account resolves to the uid that owns the files the tests create, so ownership checks are real.
BOT_TEST_UID="$(id -u)"
export BOT_TEST_UID

stub() {
	printf '#!/bin/sh\n%s\n' "$2" >"$STUBS/$1"
	chmod 0755 "$STUBS/$1"
}
# State the stubs share lives in $BOT_TEST_STATE: whether the dillabot account exists, whether dillad is in its group,
# and the v1 unit's enabled/active state.
stub id 'case "$*" in
"-u") echo 0 ;;
"-u dillabot") [ -f "$BOT_TEST_STATE/account" ] || exit 1; echo "$BOT_TEST_UID" ;;
dillabot) [ -f "$BOT_TEST_STATE/account" ] ;;
dillad) exit 0 ;;
"-nG dillad") if [ -f "$BOT_TEST_STATE/group" ]; then echo "dillad dillabot"; else echo dillad; fi ;;
*) exit 1 ;;
esac'
stub useradd 'echo "useradd $*" >>"$BOT_TEST_LOG"; touch "$BOT_TEST_STATE/account"'
stub usermod 'echo "usermod $*" >>"$BOT_TEST_LOG"; touch "$BOT_TEST_STATE/group"'
stub chown 'echo "chown $*" >>"$BOT_TEST_LOG"'
# install -o/-g need root; the stub logs them and installs without.
stub install 'echo "install $*" >>"$BOT_TEST_LOG"
args=""
while [ "$#" -gt 0 ]; do
	case "$1" in
	-o | -g) shift 2 ;;
	*) args="$args '"'"'$1'"'"'"; shift ;;
	esac
done
eval exec /usr/bin/install $args'
# runuser -u USER -- CMD ARGS: runs CMD ARGS.
stub runuser 'echo "runuser $*" >>"$BOT_TEST_LOG"; while [ "$#" -gt 0 ] && [ "$1" != -- ]; do shift; done; shift; exec "$@"'
# The fake dillad: `admin bot-runtime create --out=F` writes a credential, `doctor` reports its config and Bot legs.
stub dillad 'echo "dillad $*" >>"$BOT_TEST_LOG"
case "$1 $2" in
"admin bot-runtime")
	for a in "$@"; do case "$a" in --out=*) out="${a#*=}" ;; --config=*) ;; esac; done
	printf "dbr_fresh-runtime-secret-value\n" >"$out"
	echo "created bot runtime 02020202020202020202020202020202; secret written to protected file"
	exit 0 ;;
"media --rooms")
	if [ -n "${BOT_TEST_ROOMS:-}" ]; then cat "$BOT_TEST_ROOMS"; else echo "no live rooms"; fi
	exit 0 ;;
esac
[ "$1" = doctor ] || exit 64
for a in "$@"; do case "$a" in --config=*) cfg="${a#*=}" ;; esac; done
echo "OK    config       parsed and validated"
if [ -n "${BOT_TEST_BOT_LEG_FAIL:-}" ]; then echo "FAIL  bot          helper_executable /usr/local/lib/dilla/dilla-bot-core: group or others can write /usr/local/lib/dilla"; exit 1; fi
if awk "/^\\[bot\\]/{b=1;next} /^\\[/{b=0} b && /enabled = true/{f=1} END{exit !f}" "$cfg"; then
	echo "OK    bot          runs /usr/local/bin/dillabot as user dillabot"
else
	echo "OK    bot          disabled ([bot] enabled = false)"
fi
echo "FAIL  certificate  no DNS yet"
exit 1'
# systemctl: the v1 unit'"'"'s state lives in $BOT_TEST_STATE. A restart of dilla starts the supervised Bot: it writes
# dilla.service'"'"'s control group and the /proc entries of dillad, the Bot ($BOT_TEST_BOT: ok, caps, none or
# restarting) and the Bot'"'"'s helper.
stub systemctl 'echo "systemctl $*" >>"$BOT_TEST_LOG"
S="$BOT_TEST_STATE"
case "$1" in
is-active)
	case "$*" in
	*dillabot*) [ -f "$S/v1-active" ] ;;
	*) [ "${BOT_TEST_DILLA_ACTIVE:-1}" = 1 ] ;;
	esac
	exit ;;
is-enabled) case "$*" in *dillabot*) [ -f "$S/v1-enabled" ] ;; *) exit 0 ;; esac; exit ;;
disable) rm -f "$S/v1-enabled"; [ "$2" != --now ] || rm -f "$S/v1-active" ;;
enable) case "$*" in *dillabot*) touch "$S/v1-enabled" ;; esac ;;
start) case "$*" in *dillabot*) touch "$S/v1-active" ;; esac ;;
show) [ "$2 $3 $4 $5" = "-p ControlGroup --value dilla" ] && echo /system.slice/dilla.service ;;
restart)
	[ "$2" = dilla ] || exit 0
	[ -z "${BOT_TEST_RESTART_FAIL:-}" ] || exit 1
	R="$DILLA_INSTALL_ROOT"
	n=0; [ ! -f "$S/restarts" ] || n="$(cat "$S/restarts")"; n=$((n + 1)); echo "$n" >"$S/restarts"
	rm -rf "$R/proc" "$R/sys"
	mkdir -p "$R/sys/fs/cgroup/system.slice/dilla.service"
	proc() { # PID UID CAPBND ARGV0
		mkdir -p "$R/proc/$1"
		printf "%s\\0--flag\\0" "$4" >"$R/proc/$1/cmdline"
		ln -sfn "$4" "$R/proc/$1/exe"
		printf "Name:\\tx\\nUid:\\t%s\\t%s\\t%s\\t%s\\nCapInh:\\t0000000000000000\\nCapPrm:\\t0000000000000000\\nCapEff:\\t0000000000000000\\nCapBnd:\\t%s\\nCapAmb:\\t0000000000000000\\nNoNewPrivs:\\t1\\n" "$2" "$2" "$2" "$2" "$3" >"$R/proc/$1/status"
		echo "$1" >>"$R/sys/fs/cgroup/system.slice/dilla.service/cgroup.procs"
	}
	proc 100 999 00000000000000c0 /usr/local/bin/dillad
	case "${BOT_TEST_BOT:-ok}" in
	ok) proc 101 "$BOT_TEST_UID" 0000000000000000 /usr/local/bin/dillabot ;;
	caps) proc 101 "$BOT_TEST_UID" 000001ffffffffff /usr/local/bin/dillabot ;;
	none) ;;
	esac
	# The Bot'"'"'s helper and its sandbox run as dillabot too; a bubblewrap child holds capabilities in its own
	# user namespace, which is not the Bot'"'"'s and is not checked.
	proc 102 "$BOT_TEST_UID" 000001ffffffffff /usr/local/lib/dilla/dilla-bot-core
	;;
esac
exit 0'

REL="$TMP/release"
mkdir -p "$REL"
printf 'bot-binary-v2\n' >"$REL/dillabot-linux-amd64"
printf 'native-helper-v2\n' >"$REL/dilla-bot-core-linux-amd64"
(cd "$REL" && sha256sum dillabot-linux-amd64 dilla-bot-core-linux-amd64 >SHA256SUMS)

SECRET_TOKEN='dbr_v1-runtime-secret-value'
SECRET_KEY='database-key-secret-material'
INST=01010101010101010101010101010101

# v1_root ROOT: a host with dillad (no [bot] in dilla.toml, the pre-v2 dilla.service) and a v1 Bot: dillabot.service
# (enabled and active), /etc/dillabot/{bot.toml,runtime.token}, the v1 binaries, and one installation's state with its
# database key, plus an older installer's pre-upgrade key copy.
v1_root() {
	r="$1"
	mkdir -p "$r/usr/local/bin" "$r/usr/local/libexec" "$r/usr/local/lib/dilla" "$r/etc/dilla" "$r/etc/systemd/system" "$r.state"
	cp "$STUBS/dillad" "$r/usr/local/bin/dillad"
	printf 'core-v2\n' >"$r/usr/local/bin/dilla_core_wasi.wasm"
	printf "[instance]\n  domain = 'chat.example.org'\n\n[livekit]\n  mode = 'sidecar'\n" >"$r/etc/dilla/dilla.toml"
	chmod 0640 "$r/etc/dilla/dilla.toml"
	printf '[Service]\nExecStart=/usr/local/bin/dillad serve\n' >"$r/etc/systemd/system/dilla.service"
	printf '[Unit]\nDescription=the v1 Bot unit\n[Service]\nUser=dillabot\n' >"$r/etc/systemd/system/dillabot.service"
	install -d -m 0750 "$r/etc/dillabot"
	printf 'base_url = "https://chat.example.org"\nruntime_credential_file = "/etc/dillabot/runtime.token"\nstate_dir = "/var/lib/dillabot"\n' >"$r/etc/dillabot/bot.toml"
	chmod 0640 "$r/etc/dillabot/bot.toml"
	printf '%s\n' "$SECRET_TOKEN" >"$r/etc/dillabot/runtime.token"
	chmod 0600 "$r/etc/dillabot/runtime.token"
	printf 'bot-binary-v1\n' >"$r/usr/local/bin/dillabot"
	printf 'native-helper-v1\n' >"$r/usr/local/libexec/dilla-bot-core"
	printf 'module-v1\n' >"$r/usr/local/lib/dilla/dilla_core_wasi.wasm"
	install -d -m 0700 "$r/var/lib/dillabot" "$r/var/lib/dillabot/$INST"
	printf 'sqlcipher pages\n' >"$r/var/lib/dillabot/$INST/state.db"
	printf '%s\n' "$SECRET_KEY" >"$r/var/lib/dillabot/$INST/database.key"
	chmod 0600 "$r/var/lib/dillabot/$INST/state.db" "$r/var/lib/dillabot/$INST/database.key"
	install -d -m 0700 "$r/var/backups/dillabot/20200101T000000Z-aaaaaaa1/state/$INST"
	printf '%s\n' "$SECRET_KEY" >"$r/var/backups/dillabot/20200101T000000Z-aaaaaaa1/state/$INST/database.key"
	touch "$r.state/account" "$r.state/v1-enabled" "$r.state/v1-active"
}

# inst ROOT ARGS...: runs install-bot.sh against ROOT; leaves RC, $TMP/out and $TMP/err.
inst() {
	r="$1"
	shift
	: >"$LOG"
	RC=0
	PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$r" BOT_TEST_LOG="$LOG" BOT_TEST_STATE="$r.state" "$INSTALL" "$@" >"$TMP/out" 2>"$TMP/err" || RC=$?
}
# tree DIR: every path under DIR with its mode, owner and (for files) contents hash.
tree() {
	(cd "$1" && find . -path ./proc -prune -o -path ./sys -prune -o -print | LC_ALL=C sort | while IFS= read -r p; do
		if [ -f "$p" ] && [ ! -L "$p" ]; then h="$(sha256sum <"$p" | cut -c1-16)"; else h=-; fi
		printf '%s %s %s\n' "$p" "$(stat -c '%a %u' "$p")" "$h"
	done)
}
no_secret_printed() { # NAME
	for s in "$SECRET_TOKEN" "$SECRET_KEY" dbr_fresh-runtime-secret-value; do
		if grep -qF "$s" "$TMP/out" "$TMP/err" "$LOG"; then fail "$1: a secret reached the output or a command line"; fi
	done
}

# ---------------------------------------------------------------- arguments ----
inst "$TMP/args" --upgrade --release-url="file://$REL" --check-args
[ "$RC" = 0 ] || fail "--check-args rejected valid arguments: $(cat "$TMP/err")"
[ ! -s "$LOG" ] || fail "--check-args ran a host command"
pass "--check-args validates without host changes"
for bad in "--base-url=https://chat.example.org" "--instance-id=$INST" "--module=$REL/x.wasm" "--keep-backups=3"; do
	inst "$TMP/args" --upgrade --release-url="file://$REL" "$bad" --check-args
	[ "$RC" != 0 ] || fail "$bad was accepted"
	[ ! -s "$LOG" ] || fail "$bad ran a host command"
done
inst "$TMP/args" --upgrade --base-url=https://chat.example.org --check-args
grep -q 'learns the instance from dillad' "$TMP/err" || fail "--base-url was not explained: $(cat "$TMP/err")"
pass "the v1 first-install flags (--base-url, --instance-id, --module) and --keep-backups are refused"

# ---------------------------------------------- upgrade moves dillabot under dillad ----
R="$TMP/v1"
v1_root "$R"
STATE_BEFORE="$(tree "$R/var/lib/dillabot/$INST")"
UNIT_V1="$(cat "$R/etc/systemd/system/dillabot.service")"
TOML_V1="$(cat "$R/etc/dillabot/bot.toml")"
TOKEN_SUM="$(sha256sum <"$R/etc/dillabot/runtime.token")"
inst "$R" --upgrade --release-url="file://$REL"
[ "$RC" = 0 ] || fail "upgrade moves dillabot under dillad: exit $RC: $(cat "$TMP/err")"
# Stops and disables dillabot.service.
grep -qx 'systemctl disable --now dillabot' "$LOG" || fail "dillabot.service was not stopped and disabled: $(cat "$LOG")"
[ ! -f "$R.state/v1-active" ] && [ ! -f "$R.state/v1-enabled" ] || fail "dillabot.service is still active or enabled"
# Backs up the unit and bot.toml to /var/backups/dilla/bot-v1-<timestamp>/, root-only, and nothing secret.
BK="$(find "$R/var/backups/dilla" -mindepth 1 -maxdepth 1 -type d -name 'bot-v1-[0-9]*T[0-9]*Z')"
[ -n "$BK" ] && [ "$(printf '%s\n' "$BK" | wc -l)" = 1 ] || fail "no single bot-v1-<timestamp> backup: $(find "$R/var/backups" | head)"
[ "$(stat -c %a "$R/var/backups/dilla")" = 700 ] && [ "$(stat -c %a "$BK")" = 700 ] || fail "the backup is not root-only (0700)"
[ "$(cat "$BK/dillabot.service")" = "$UNIT_V1" ] || fail "the backup does not hold the v1 unit"
[ "$(cat "$BK/bot.toml")" = "$TOML_V1" ] || fail "the backup does not hold bot.toml"
for f in "$BK"/*; do
	case "${f##*/}" in dillabot.service | bot.toml | dilla.service) ;; *) fail "the backup holds ${f##*/}; it holds only the units and bot.toml" ;; esac
done
if grep -rqF -e "$SECRET_TOKEN" -e "$SECRET_KEY" "$R/var/backups/dilla"; then fail "the backup holds the runtime credential or the database key"; fi
[ -z "$(find "$R/var/backups/dilla" -name 'database.key' -o -name 'runtime.token' -o -name 'state.db')" ] || fail "the backup holds a key, credential or database file"
# Removes both.
[ ! -e "$R/etc/systemd/system/dillabot.service" ] || fail "dillabot.service was not removed"
[ ! -e "$R/etc/dillabot" ] || fail "/etc/dillabot was not removed: $(ls -la "$R/etc/dillabot")"
# Leaves /var/lib/dillabot/<installation> byte-identical, owned by dillabot.
[ "$(tree "$R/var/lib/dillabot/$INST")" = "$STATE_BEFORE" ] || fail "the installation's state changed"
[ "$(stat -c '%a %u' "$R/var/lib/dillabot")" = "700 $BOT_TEST_UID" ] || fail "/var/lib/dillabot is not 0700 dillabot"
[ "$(stat -c %u "$R/var/lib/dillabot/$INST")" = "$BOT_TEST_UID" ] || fail "the installation's state is not owned by dillabot"
# The runtime credential moves into the state dir unchanged, 0600 dillabot.
[ "$(sha256sum <"$R/var/lib/dillabot/runtime.token")" = "$TOKEN_SUM" ] || fail "the runtime credential changed on its move"
[ "$(stat -c '%a %u' "$R/var/lib/dillabot/runtime.token")" = "600 $BOT_TEST_UID" ] || fail "runtime.token is not 0600 dillabot"
grep -q "^install -T -m 0600 -o dillabot -g dillabot $R/etc/dillabot/runtime.token $R/var/lib/dillabot/runtime.token.new$" "$LOG" || fail "the credential was not installed -T 0600 dillabot:dillabot"
if grep -q '^chown -R\|^chown [^-]' "$LOG"; then fail "root ran a chown that follows links in the state dir: $(grep '^chown' "$LOG")"; fi
[ ! -e "$R/etc/dilla/dilla.toml.bot-prev" ] || fail "the previous dilla.toml outlived the confirmed upgrade"
if grep -q '^dillad admin bot-runtime create' "$LOG"; then fail "the upgrade created a new runtime credential"; fi
# Writes [bot] enabled = true with state_dir.
[ "$(sed -n '/^\[bot\]$/,/^\[/p' "$R/etc/dilla/dilla.toml")" = "$(printf "[bot]\n  enabled = true\n  state_dir = '/var/lib/dillabot'")" ] || fail "dilla.toml has no [bot] enabled = true with state_dir: $(cat "$R/etc/dilla/dilla.toml")"
grep -q "^  domain = 'chat.example.org'$" "$R/etc/dilla/dilla.toml" || fail "the rest of dilla.toml was lost"
[ "$(stat -c %a "$R/etc/dilla/dilla.toml")" = 640 ] || fail "dilla.toml lost its mode"
# The Bot's files: root-owned binaries where [bot] expects them, the release's dilla.service, dillad in the group.
[ "$(cat "$R/usr/local/bin/dillabot")" = bot-binary-v2 ] || fail "dillabot was not upgraded"
[ "$(cat "$R/usr/local/lib/dilla/dilla-bot-core")" = native-helper-v2 ] || fail "dilla-bot-core is not at /usr/local/lib/dilla"
grep -q "^install -m 0755 -o root -g root .*/dillabot-linux-amd64 $R/usr/local/bin/dillabot.new$" "$LOG" || fail "dillabot was not installed root:root 0755"
grep -q "^install -m 0755 -o root -g root .*/dilla-bot-core-linux-amd64 $R/usr/local/lib/dilla/dilla-bot-core.new$" "$LOG" || fail "dilla-bot-core was not installed root:root 0755"
grep -q "^install -d -m 0755 -o root -g root $R/usr/local/lib/dilla$" "$LOG" || fail "/usr/local/lib/dilla was not made root:root 0755"
[ ! -e "$R/usr/local/libexec/dilla-bot-core" ] && [ ! -e "$R/usr/local/lib/dilla/dilla_core_wasi.wasm" ] || fail "the v1 helper and module were left behind"
cmp -s "$HERE/dilla.service" "$R/etc/systemd/system/dilla.service" || fail "dilla.service is not the release's deploy/dilla.service"
grep -qx 'usermod -aG dillabot dillad' "$LOG" || fail "dillad was not added to the dillabot group"
# The old installer's key copy goes (Task 8: no indefinite key copies).
[ ! -e "$R/var/backups/dillabot/20200101T000000Z-aaaaaaa1" ] || fail "an older installer's key copy survived"
# Restarts dilla.service, once, after doctor passed.
[ "$(grep -c '^systemctl restart dilla$' "$LOG")" = 1 ] || fail "dilla.service was not restarted exactly once"
grep -q 'runuser -u dillad -- .*dillad doctor' "$LOG" || fail "doctor did not run as dillad"
[ "$(grep -n 'runuser -u dillad -- .*dillad doctor' "$LOG" | head -1 | cut -d: -f1)" -lt "$(grep -n '^systemctl restart dilla$' "$LOG" | cut -d: -f1)" ] || fail "doctor did not run before the restart"
# Logs one line per step.
for line in \
	"verified Bot assets against SHA256SUMS" \
	"backed up dillabot.service, bot.toml and dilla.service to $BK (no keys, no credential)" \
	"stopped and disabled dillabot.service" \
	"added dillad to the dillabot group" \
	"moved the runtime credential to /var/lib/dillabot/runtime.token, unchanged" \
	"installed dillabot and dilla-bot-core (root:root 0755)" \
	"installed dilla.service from this release" \
	"removed dillabot.service and /etc/dillabot/bot.toml" \
	"set [bot] enabled = true, state_dir = '/var/lib/dillabot' in dilla.toml" \
	"dillad doctor: bot OK, runs /usr/local/bin/dillabot as user dillabot" \
	"restarted dilla.service" \
	"the Bot runs under dilla.service as dillabot (pid 101), holding no capabilities" \
	"removed the v1 credential copy /etc/dillabot/runtime.token" \
	"removed the v1 helper and module" \
	"removed pre-upgrade copy $R/var/backups/dillabot/20200101T000000Z-aaaaaaa1"; do
	grep -qxF "install-bot: $line" "$TMP/err" || fail "the log lacks the step line \"$line\"; it was:
$(cat "$TMP/err")"
done
no_secret_printed "upgrade moves dillabot under dillad"
pass "upgrade moves dillabot under dillad"

# ---------------------------------- upgrade run twice is a no-op the second time ----
BEFORE="$(tree "$R")"
inst "$R" --upgrade --release-url="file://$REL"
[ "$RC" = 0 ] || fail "the second run failed: $(cat "$TMP/err")"
[ "$(tree "$R")" = "$BEFORE" ] || fail "the second run changed files: $(diff <(printf '%s\n' "$BEFORE") <(tree "$R"))"
if grep -E '^(systemctl (restart|stop|start|enable|disable|daemon-reload)|useradd|usermod|chown|install|dillad admin)' "$LOG"; then fail "the second run changed the host"; fi
grep -qx 'install-bot: nothing to change: the Bot already runs under dillad from this release' "$TMP/err" || fail "the second run did not say it changed nothing: $(cat "$TMP/err")"
grep -qx 'install-bot: the Bot runs under dilla.service as dillabot (pid 101), holding no capabilities' "$TMP/err" || fail "the second run did not check the Bot"
no_secret_printed "the second run"
pass "upgrade run twice is a no-op the second time"

# -------------------------------------------- fresh install has no dillabot.service ----
# dillad installed without a Bot (no [bot] enabled, no account): install-bot.sh puts the Bot under it.
F="$TMP/fresh"
mkdir -p "$F/usr/local/bin" "$F/etc/dilla" "$F/etc/systemd/system" "$F.state"
cp "$STUBS/dillad" "$F/usr/local/bin/dillad"
printf 'core-v2\n' >"$F/usr/local/bin/dilla_core_wasi.wasm"
printf "[instance]\n  domain = 'chat.example.org'\n\n[livekit]\n  mode = 'sidecar'\n\n[bot]\n  enabled = false\n  executable = '/usr/local/bin/dillabot'\n  state_dir = '/var/lib/dillabot'\n  user = 'dillabot'\n" >"$F/etc/dilla/dilla.toml"
cp "$HERE/dilla.service" "$F/etc/systemd/system/dilla.service"
inst "$F" --upgrade --release-url="file://$REL"
[ "$RC" = 0 ] || fail "fresh install has no dillabot.service: exit $RC: $(cat "$TMP/err")"
[ ! -e "$F/etc/systemd/system/dillabot.service" ] || fail "a fresh install wrote dillabot.service"
[ ! -e "$F/etc/dillabot" ] || fail "a fresh install wrote /etc/dillabot"
[ ! -e "$F/var/backups/dilla" ] || fail "a fresh install made a backup of nothing"
if grep -q 'dillabot.service\|systemctl [a-z-]* .*dillabot' "$LOG"; then fail "a fresh install touched a dillabot unit: $(cat "$LOG")"; fi
grep -qx 'useradd --system --user-group --no-create-home --home-dir /var/lib/dillabot --shell /usr/sbin/nologin dillabot' "$LOG" || fail "the dillabot user was not created"
grep -qx 'usermod -aG dillabot dillad' "$LOG" || fail "dillad was not added to the dillabot group"
grep -q "^install -d -m 0700 -o dillabot -g dillabot $F/var/lib/dillabot$" "$LOG" || fail "the state dir was not created 0700 dillabot"
grep -q "^dillad admin bot-runtime create --config=$F/etc/dilla/dilla.toml --out=" "$LOG" || fail "no runtime credential was registered"
[ "$(stat -c %a "$F/var/lib/dillabot/runtime.token")" = 600 ] || fail "the new credential is not 0600"
grep -qx "install-bot: dillad registered a new runtime credential: created bot runtime 02020202020202020202020202020202; secret written to protected file" "$TMP/err" || fail "the new runtime id was not logged"
[ "$(sed -n '/^\[bot\]$/,/^\[/p' "$F/etc/dilla/dilla.toml" | sed -n 2p)" = "  enabled = true" ] || fail "[bot] was not enabled in place: $(cat "$F/etc/dilla/dilla.toml")"
[ "$(grep -c '^\[bot\]$' "$F/etc/dilla/dilla.toml")" = 1 ] || fail "a second [bot] table was appended"
if grep -q 'daemon-reload' "$LOG"; then fail "an unchanged dilla.service was reloaded"; fi
no_secret_printed "fresh install has no dillabot.service"
pass "fresh install has no dillabot.service"

# ------------------------------------------------ failures restore the v1 Bot ----
# expect_v1_restored NAME ROOT: the failed upgrade put every v1 file back and the v1 Bot runs again.
expect_v1_restored() {
	[ "$RC" != 0 ] || fail "$1: the upgrade succeeded"
	r="$2"
	[ "$(cat "$r/etc/systemd/system/dillabot.service")" = "$UNIT_V1" ] || fail "$1: dillabot.service was not restored"
	[ "$(cat "$r/etc/dillabot/bot.toml")" = "$TOML_V1" ] || fail "$1: bot.toml was not restored"
	[ "$(sha256sum <"$r/etc/dillabot/runtime.token")" = "$TOKEN_SUM" ] || fail "$1: the v1 credential is not in place"
	[ ! -e "$r/var/lib/dillabot/runtime.token" ] || fail "$1: the moved credential copy was left in the state dir"
	[ "$(cat "$r/usr/local/bin/dillabot")" = bot-binary-v1 ] || fail "$1: dillabot was not restored"
	[ "$(cat "$r/usr/local/libexec/dilla-bot-core")" = native-helper-v1 ] || fail "$1: the v1 helper is gone"
	[ ! -e "$r/usr/local/lib/dilla/dilla-bot-core" ] || fail "$1: the new helper was left behind"
	[ "$(cat "$r/etc/systemd/system/dilla.service")" = "$(printf '[Service]\nExecStart=/usr/local/bin/dillad serve')" ] || fail "$1: dilla.service was not restored"
	if grep -q '^\[bot\]' "$r/etc/dilla/dilla.toml"; then fail "$1: dilla.toml was not restored"; fi
	[ "$(tree "$r/var/lib/dillabot/$INST")" = "$STATE_BEFORE" ] || fail "$1: the state changed"
	{ [ -f "$r.state/v1-enabled" ] && [ -f "$r.state/v1-active" ]; } || fail "$1: dillabot.service was not enabled and started again"
	grep -q "put back the v1 Bot's files and unit (dillabot.service and /etc/dillabot) and the previous dilla.toml, dilla.service and Bot binaries. The v1 Bot cannot connect to a wire-6 dillad until this upgrade succeeds" "$TMP/err" ||
		fail "$1: the rollback was not reported as files and unit put back: $(cat "$TMP/err")"
	if grep -q 'incomplete' "$TMP/err"; then fail "$1: a complete rollback said incomplete"; fi
	# A complete rollback leaves no rollback material behind.
	[ -z "$(find "$r/var/backups" -path '*/dilla/bot-*' 2>/dev/null)" ] || fail "$1: the rollback material outlived a complete rollback: $(find "$r/var/backups/dilla")"
	[ ! -e "$r/etc/dilla/dilla.toml.bot-prev" ] || fail "$1: dilla.toml.bot-prev outlived a complete rollback"
	no_secret_printed "$1"
}
R2="$TMP/caps"
v1_root "$R2"
BOT_TEST_BOT=caps inst "$R2" --upgrade --release-url="file://$REL"
expect_v1_restored "a Bot holding capabilities" "$R2"
grep -q 'install-bot: RED bot: the Bot (pid 101) holds capabilities: CapBnd=000001ffffffffff; it must hold none' "$TMP/err" || fail "the capability failure was not named: $(cat "$TMP/err")"
[ "$(grep -c '^systemctl restart dilla$' "$LOG")" = 2 ] || fail "dilla was not restarted on the restored config"
[ -e "$R2/var/backups/dillabot/20200101T000000Z-aaaaaaa1" ] || fail "a rolled-back upgrade removed an older copy"
grep -qF "the v1 installer's key copies are still there (they hold the database key; delete them once the Bot is upgraded): $R2/var/backups/dillabot/20200101T000000Z-aaaaaaa1" "$TMP/err" ||
	fail "the rollback did not name the kept key copies: $(cat "$TMP/err")"
pass "a Bot that holds any capability fails the upgrade, which restores the v1 Bot"

R3="$TMP/nobot"
v1_root "$R3"
BOT_TEST_BOT=none inst "$R3" --upgrade --release-url="file://$REL"
expect_v1_restored "a Bot that never starts" "$R3"
grep -q 'no Bot process runs under dilla.service' "$TMP/err" || fail "the missing Bot was not named: $(cat "$TMP/err")"
pass "a Bot that never starts under dillad fails the upgrade, which restores the v1 Bot"

R4="$TMP/leg"
v1_root "$R4"
BOT_TEST_BOT_LEG_FAIL=1 inst "$R4" --upgrade --release-url="file://$REL"
expect_v1_restored "a red doctor Bot leg" "$R4"
if grep -q '^systemctl restart dilla$' "$LOG"; then fail "dilla was restarted although doctor's Bot leg was red"; fi
grep -q 'group or others can write /usr/local/lib/dilla' "$TMP/err" || fail "the doctor's reason was not shown"
pass "a red doctor Bot leg stops the upgrade before dilla restarts, and restores the v1 Bot"

R5="$TMP/restart"
v1_root "$R5"
BOT_TEST_RESTART_FAIL=1 inst "$R5" --upgrade --release-url="file://$REL"
expect_v1_restored "a dilla that does not restart" "$R5"
pass "a dilla that does not restart restores the v1 Bot"

# ------------------------------------------------------------- refusals ----
R6="$TMP/elsewhere"
v1_root "$R6"
sed -i 's|^state_dir = .*|state_dir = "/srv/bot"|' "$R6/etc/dillabot/bot.toml"
SNAP="$(tree "$R6")"
inst "$R6" --upgrade --release-url="file://$REL"
[ "$RC" != 0 ] || fail "a v1 state_dir outside /var/lib/dillabot was accepted"
grep -q 'state_dir is /srv/bot' "$TMP/err" || fail "the state_dir refusal was not explained: $(cat "$TMP/err")"
[ "$(tree "$R6")" = "$SNAP" ] || fail "the refusal changed files"
if grep -q '^systemctl \(stop\|disable\|restart\)' "$LOG"; then fail "the refusal stopped something"; fi
pass "a v1 state_dir outside /var/lib/dillabot is refused before anything changes"

R7="$TMP/twocreds"
v1_root "$R7"
printf 'dbr_another\n' >"$R7/var/lib/dillabot/runtime.token"
SNAP="$(tree "$R7")"
inst "$R7" --upgrade --release-url="file://$REL"
{ [ "$RC" != 0 ] && grep -q 'two different runtime credentials' "$TMP/err"; } || fail "two different credentials were not refused: $(cat "$TMP/err")"
[ "$(tree "$R7")" = "$SNAP" ] || fail "the refusal changed files"
pass "two different runtime credentials are refused before anything changes"

BAD="$TMP/bad-release"
cp -a "$REL" "$BAD"
printf 'tamper' >>"$BAD/dillabot-linux-amd64"
R8="$TMP/tamper"
v1_root "$R8"
SNAP="$(tree "$R8")"
inst "$R8" --upgrade --release-url="file://$BAD"
{ [ "$RC" != 0 ] && grep -q 'checksum mismatch' "$TMP/err"; } || fail "a tampered release was accepted"
[ "$(tree "$R8")" = "$SNAP" ] || fail "a checksum failure changed files"
if grep -q '^systemctl' "$LOG"; then fail "a checksum failure ran systemctl"; fi
pass "a tampered release changes nothing"

# ------------------------------- I-1: root never follows a link in the state dir ----
# Every entry looks foreign here (dillabot resolves to uid 4242, which owns nothing), as after a restore done as root.
# A hardlink or a symlink among them is refused before anything changes: chown would reach the inode outside.
for kind in hardlink symlink; do
	RL="$TMP/links-$kind"
	v1_root "$RL"
	printf 'root-owned target\n' >"$TMP/target-$kind"
	TARGET_OWNER="$(stat -c %u "$TMP/target-$kind")"
	case "$kind" in
	hardlink) ln "$TMP/target-$kind" "$RL/var/lib/dillabot/$INST/planted" ;;
	symlink) ln -s "$TMP/target-$kind" "$RL/var/lib/dillabot/$INST/planted" ;;
	esac
	SNAP="$(tree "$RL")"
	BOT_TEST_UID=4242 inst "$RL" --upgrade --release-url="file://$REL"
	[ "$RC" != 0 ] || fail "a $kind in the state dir was accepted"
	grep -qF "$RL/var/lib/dillabot/$INST/planted" "$TMP/err" || fail "the planted $kind was not named: $(cat "$TMP/err")"
	[ "$(tree "$RL")" = "$SNAP" ] || fail "the $kind refusal changed files"
	[ "$(stat -c %u "$TMP/target-$kind")" = "$TARGET_OWNER" ] || fail "the $kind's target changed owner"
	if grep -q '^chown\|^systemctl \(stop\|disable\|restart\)' "$LOG"; then fail "the $kind refusal ran chown or stopped something: $(cat "$LOG")"; fi
done
pass "a hardlink or symlink that dillabot does not own in the state dir is refused, and root never chowns through it"

# Foreign directories and single-link files are given to dillabot with chown -h, one filesystem, after dilla stopped.
RO="$TMP/owners"
v1_root "$RO"
BOT_TEST_UID=4242 inst "$RO" --upgrade --release-url="file://$REL"
[ "$RC" = 0 ] || fail "the ownership repair failed: $(cat "$TMP/err")"
grep -q "^chown -h dillabot:dillabot .*$RO/var/lib/dillabot/$INST/database.key" "$LOG" || fail "the foreign key file was not given to dillabot with chown -h: $(grep chown "$LOG")"
if grep -q '^chown -R\|^chown [^-]' "$LOG"; then fail "a chown that follows links ran"; fi
[ "$(grep -n '^systemctl stop dilla$' "$LOG" | cut -d: -f1)" -lt "$(grep -n '^chown -h dillabot:dillabot .*database.key' "$LOG" | cut -d: -f1)" ] || fail "dilla was not stopped before the owners were fixed"
grep -qx 'install-bot: gave dillabot the files under /var/lib/dillabot it did not own' "$TMP/err" || fail "the ownership repair was not logged"
pass "foreign directories and single-link files are given to dillabot with chown -h, with no Bot running"

# ------------------------------------------------- I-2: interrupted runs ----
# inst_bg ROOT ARGS...: starts install-bot.sh with a 30 s health wait and returns once it is in that wait (dilla has
# restarted with the Bot enabled); BGPID is the script.
inst_bg() {
	r="$1"
	shift
	: >"$LOG"
	: >"$TMP/err"
	env PATH="$STUBS:$PATH" DILLA_INSTALL_ROOT="$r" BOT_TEST_LOG="$LOG" BOT_TEST_STATE="$r.state" DILLA_BOT_HEALTH_SECONDS=30 \
		"$INSTALL" "$@" >"$TMP/out" 2>"$TMP/err" &
	BGPID=$!
	for _ in $(seq 150); do
		if grep -q 'the Bot runs under\|restarted dilla.service' "$TMP/err"; then sleep 0.3; return 0; fi
		sleep 0.1
	done
	fail "the run never reached the health wait: $(cat "$TMP/err")"
}
# An interrupt (SIGTERM, like Ctrl-C's SIGINT) during the health wait rolls back at once.
RT="$TMP/term"
v1_root "$RT"
inst_bg "$RT" --upgrade --release-url="file://$REL"
kill -TERM "$BGPID"
RC=0
wait "$BGPID" || RC=$?
grep -q 'install-bot: error: interrupted' "$TMP/err" || fail "SIGTERM did not roll back: $(cat "$TMP/err")"
expect_v1_restored "an interrupt during the health wait" "$RT"
pass "an interrupt during the health wait rolls back to the v1 Bot's files and unit"

# A SIGKILL leaves the run pending; its rollback material is in the backup and beside dilla.toml, not in a temp dir.
killed_root() { # ROOT
	v1_root "$1"
	inst_bg "$1" --upgrade --release-url="file://$REL"
	kill -KILL "$BGPID"
	# bash reports the kill on wait's stderr.
	{ wait "$BGPID"; } 2>/dev/null || :
	PBK="$(find "$1/var/backups/dilla" -mindepth 1 -maxdepth 1 -name 'bot-v1-*')"
	[ -f "$PBK/pending" ] || fail "a killed run left no pending backup"
	[ "$(cat "$PBK/prev-dillabot")" = bot-binary-v1 ] || fail "the killed run's backup does not hold the previous dillabot"
	{ grep -qx 'was_enabled=1' "$PBK/state" && grep -qx 'was_active=1' "$PBK/state"; } || fail "the v1 unit's enable state is not in the backup"
	[ -f "$1/etc/dilla/dilla.toml.bot-prev" ] || fail "the previous dilla.toml is not beside dilla.toml"
	[ "$(stat -c %a "$1/etc/dilla/dilla.toml.bot-prev")" = 640 ] || fail "dilla.toml.bot-prev is not 0640"
	[ ! -e "$1/etc/systemd/system/dillabot.service" ] || fail "the killed run had not reached the health wait"
}
RK1="$TMP/killed-ok"
killed_root "$RK1"
inst "$RK1" --upgrade --release-url="file://$REL"
[ "$RC" = 0 ] || fail "the re-run after a kill failed: $(cat "$TMP/err")"
grep -qxF "install-bot: resuming the interrupted upgrade recorded in $PBK" "$TMP/err" || fail "the re-run did not resume the pending backup"
[ "$(find "$RK1/var/backups/dilla" -mindepth 1 -maxdepth 1 | wc -l)" = 1 ] || fail "the re-run made a second backup"
[ "$(find "$PBK" -mindepth 1 -printf '%f\n' | LC_ALL=C sort | tr '\n' ' ')" = "bot.toml dilla.service dillabot.service " ] || fail "the confirmed backup holds $(find "$PBK" -mindepth 1 -printf '%f ')"
{ [ ! -e "$RK1/etc/dillabot" ] && [ ! -e "$RK1/etc/dilla/dilla.toml.bot-prev" ]; } || fail "the resumed upgrade left v1 leftovers"
pass "a re-run after a killed upgrade resumes its pending backup and completes"

RK2="$TMP/killed-fail"
killed_root "$RK2"
BOT_TEST_BOT=caps inst "$RK2" --upgrade --release-url="file://$REL"
expect_v1_restored "a failing re-run after a kill" "$RK2"
pass "a failing re-run after a killed upgrade restores the v1 Bot's files, binaries and enable state from the pending backup"

RK3="$TMP/killed-missing"
killed_root "$RK3"
rm -f "$PBK/prev-dillabot"
BOT_TEST_BOT=caps inst "$RK3" --upgrade --release-url="file://$REL"
[ "$RC" != 0 ] || fail "a rollback with missing material succeeded"
grep -qF "the rollback was incomplete: the previous dillabot is missing ($PBK/prev-dillabot). The rollback material is kept in $PBK" "$TMP/err" ||
	fail "the incomplete rollback did not name what is missing: $(cat "$TMP/err")"
if grep -q "put back the v1 Bot" "$TMP/err"; then fail "an incomplete rollback claimed the v1 Bot was put back"; fi
[ -d "$PBK" ] || fail "an incomplete rollback deleted its material"
pass "a rollback that misses its material says incomplete and names exactly what is missing"

# --------------------------------- I-3: key copies go only after a healthy Bot ----
# R is on this release; a key copy an earlier run could not delete is kept while the Bot is RED, and goes once it is not.
install -d -m 0700 "$R/var/backups/dillabot/20200102T000000Z-bbbbbbb1/state"
printf '%s\n' "$SECRET_KEY" >"$R/var/backups/dillabot/20200102T000000Z-bbbbbbb1/state/database.key"
cp "$R/proc/101/status" "$TMP/status.ok"
sed -i 's/^CapEff:.*/CapEff:\t0000000000000400/' "$R/proc/101/status"
inst "$R" --upgrade --release-url="file://$REL"
[ "$RC" != 0 ] || fail "a no-op run with a RED Bot succeeded"
[ -d "$R/var/backups/dillabot/20200102T000000Z-bbbbbbb1" ] || fail "the key copy was deleted while the Bot was RED"
grep -qF "the v1 installer's key copies are kept until the Bot is healthy: $R/var/backups/dillabot/20200102T000000Z-bbbbbbb1" "$TMP/err" || fail "the kept key copy was not named"
cp "$TMP/status.ok" "$R/proc/101/status"
inst "$R" --upgrade --release-url="file://$REL"
[ "$RC" = 0 ] || fail "the healthy no-op run failed: $(cat "$TMP/err")"
[ ! -e "$R/var/backups/dillabot/20200102T000000Z-bbbbbbb1" ] || fail "the key copy survived a healthy run"
[ "$(grep -n 'removed pre-upgrade copy' "$TMP/err" | cut -d: -f1)" -gt "$(grep -n 'the Bot runs under dilla.service' "$TMP/err" | cut -d: -f1)" ] || fail "the key copy went before the Bot was checked"
pass "a run that changes nothing else deletes the v1 key copies only after doctor and the Bot check pass"

# --------------------------------------------------- I-4: live calls ----
printf 'room-a\t2 participants\n  alice\n  bob\n' >"$TMP/rooms.live"
RL4="$TMP/live"
v1_root "$RL4"
SNAP="$(tree "$RL4")"
BOT_TEST_ROOMS="$TMP/rooms.live" inst "$RL4" --upgrade --release-url="file://$REL"
[ "$RC" != 0 ] || fail "an upgrade over live calls went ahead"
grep -q 'there are live calls, and restarting the SFU disconnects everyone in them. Nothing was changed' "$TMP/err" || fail "the live calls were not the reason: $(cat "$TMP/err")"
grep -q 'room-a: 2 participants' "$TMP/err" || fail "the live room was not listed"
if grep -q 'alice' "$TMP/err"; then fail "a participant's identity was listed"; fi
[ "$(tree "$RL4")" = "$SNAP" ] || fail "the live-call refusal changed files"
if grep '^systemctl' "$LOG" | grep -vq '^systemctl is-active'; then fail "the live-call refusal ran systemctl: $(grep systemctl "$LOG")"; fi
BOT_TEST_ROOMS="$TMP/rooms.live" inst "$RL4" --upgrade --yes --release-url="file://$REL"
[ "$RC" = 0 ] || fail "--yes over live calls failed: $(cat "$TMP/err")"
grep -q 'warning: there are live calls, and restarting the SFU disconnects everyone in them; continuing because of --yes' "$TMP/err" || fail "--yes did not warn"
pass "live calls refuse the restart unless --yes, as install.sh --upgrade does"

# --------------------------------------- M-3: the Bot check, case by case ----
# health_case NAME WANT_RC [TEXT]: runs the shared bot_health against the fake /proc and cgroup under $HR.
HR="$TMP/health"
mkdir -p "$HR.state"
touch "$HR.state/account"
hproc() { # PID UID ARGV0 EXE CAPS("inh prm eff bnd amb", "-" drops a line) NNP IN_CGROUP
	mkdir -p "$HR/proc/$1"
	printf '%s\0' "$3" >"$HR/proc/$1/cmdline"
	ln -sfn "$4" "$HR/proc/$1/exe"
	{
		printf 'Uid:\t%s\t%s\t%s\t%s\n' "$2" "$2" "$2" "$2"
		i=0
		for v in $5; do
			i=$((i + 1))
			name="$(echo "CapInh CapPrm CapEff CapBnd CapAmb" | cut -d' ' -f"$i")"
			[ "$v" = - ] || printf '%s:\t%s\n' "$name" "$v"
		done
		printf 'NoNewPrivs:\t%s\n' "$6"
	} >"$HR/proc/$1/status"
	[ "$7" != 1 ] || echo "$1" >>"$HR/sys/fs/cgroup/system.slice/dilla.service/cgroup.procs"
}
Z=0000000000000000
ZERO="$Z $Z $Z $Z $Z"
B=/usr/local/bin/dillabot
health_case() { # NAME WANT_RC TEXT; then resets the fake tree
	rc=0
	# The eval'd block reads ROOT, the two timings and log.
	# shellcheck disable=SC2034,SC2329
	(
		ROOT="$HR"
		BOT_START_SECONDS=0
		BOT_HEALTH_SECONDS=0
		log() { printf 'install-bot: %s\n' "$*" >&2; }
		eval "$(health_block "$INSTALL")"
		PATH="$STUBS:$PATH" BOT_TEST_STATE="$HR.state" BOT_TEST_LOG="$TMP/health.log" bot_health
	) 2>"$TMP/health.err" || rc=$?
	[ "$rc" = "$2" ] || fail "bot check, $1: exit $rc, want $2: $(cat "$TMP/health.err")"
	grep -qF "$3" "$TMP/health.err" || fail "bot check, $1: no \"$3\" in: $(cat "$TMP/health.err")"
	rm -rf "$HR/proc" "$HR/sys"
	mkdir -p "$HR/sys/fs/cgroup/system.slice/dilla.service"
	: >"$HR/sys/fs/cgroup/system.slice/dilla.service/cgroup.procs"
}
health_block() { sed -n '/^# --- bot health: begin/,/^# --- bot health: end/p' "$1"; }
mkdir -p "$HR/sys/fs/cgroup/system.slice/dilla.service"
: >"$HR/sys/fs/cgroup/system.slice/dilla.service/cgroup.procs"
U="$BOT_TEST_UID"
hproc 7 "$U" $B $B "$ZERO" 1 1
health_case "a clean Bot" 0 "the Bot runs under dilla.service as dillabot (pid 7), holding no capabilities"
hproc 7 "$U" $B $B "$Z $Z 0000000000000400 $Z $Z" 1 1
health_case "CapEff set" 1 "holds capabilities: CapEff=0000000000000400; it must hold none"
hproc 7 "$U" $B $B "$Z $Z $Z $Z 0000000000000080" 1 1
health_case "CapAmb set" 1 "holds capabilities: CapAmb=0000000000000080; it must hold none"
hproc 7 "$U" $B $B "$ZERO" 0 1
health_case "NoNewPrivs 0" 1 "runs without no_new_privs"
hproc 7 "$U" $B $B "$Z $Z $Z $Z -" 1 1
health_case "a capability line missing" 1 "could not read the Bot's (pid 7) five capability sets"
hproc 7 "$U" $B $B "$ZERO" 1 1
hproc 8 "$U" $B $B "$Z $Z 000001ffffffffff 000001ffffffffff $Z" 1 0
health_case "a capable dillabot process outside the cgroup is not the Bot" 0 "(pid 7), holding no capabilities"
hproc 8 "$U" $B $B "$ZERO" 1 0
health_case "a Bot outside the cgroup only" 1 "no Bot process runs under dilla.service"
hproc 7 "$U" $B $B "$ZERO" 1 1
hproc 9 4343 $B $B "$Z $Z 000001ffffffffff 000001ffffffffff $Z" 1 1
health_case "a process of another uid named dillabot is not the Bot" 0 "(pid 7), holding no capabilities"
hproc 9 4343 $B $B "$ZERO" 1 1
health_case "another uid only" 1 "no Bot process runs under dilla.service"
hproc 7 "$U" $B $B "$ZERO" 1 1
hproc 10 "$U" $B $B "$ZERO" 1 1
health_case "two Bots" 1 "more than one Bot process runs under dilla.service: 7 10"
hproc 11 "$U" $B /var/lib/dillabot/evil "$ZERO" 1 1
health_case "argv[0] dillabot, another executable" 1 "pid 11 calls itself /usr/local/bin/dillabot but runs /var/lib/dillabot/evil; it is not the Bot"
pass "the Bot check: CapEff, CapAmb, NoNewPrivs, a missing set, the cgroup, the uid, two Bots and /proc/<pid>/exe"

# ------------------------------------------------------------ the scripts ----
# The live-call preflight is install.sh's, verbatim.
for fn in livekit_mode live_calls_preflight; do
	[ "$(sed -n "/^$fn() {/,/^}/p" "$INSTALL")" = "$(sed -n "/^$fn() {/,/^}/p" "$HERE/install.sh")" ] || fail "$fn differs between install.sh and install-bot.sh"
	[ -n "$(sed -n "/^$fn() {/,/^}/p" "$INSTALL")" ] || fail "install-bot.sh has no $fn"
done
pass "install-bot.sh's live-call preflight is install.sh's"
# The Bot health check is the same code in install.sh (fresh install) and install-bot.sh (upgrade).
health_block() { sed -n '/^# --- bot health: begin/,/^# --- bot health: end/p' "$1"; }
[ -n "$(health_block "$INSTALL")" ] || fail "install-bot.sh has no bot health block"
[ "$(health_block "$INSTALL")" = "$(health_block "$HERE/install.sh")" ] || fail "the bot health blocks of install.sh and install-bot.sh differ"
pass "install.sh and install-bot.sh share one Bot health check"
# Nothing that touches a secret runs under xtrace.
if grep -nE '^[[:space:]]*set -[a-wyz]*x|set -o xtrace' "$INSTALL" "$HERE/install.sh"; then fail "an installer turns on xtrace"; fi
pass "neither installer turns on xtrace"
