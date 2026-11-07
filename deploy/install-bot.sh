#!/bin/sh
# Puts the Dilla Bot under dillad, and upgrades it there. Run as root on the host that runs dillad.
#
# Since Bot v2 the Bot ships with dillad: dillad's bot-keeper starts it as the dillabot user inside dilla.service's
# sandbox ([bot] in dilla.toml), and there is no dillabot.service. install.sh sets this up on a fresh install. This
# script is the upgrade path:
#   - it retires a v1 installation: it stops and disables dillabot.service, backs up the unit and
#     /etc/dillabot/bot.toml (never the database key or the runtime credential) to
#     /var/backups/dilla/bot-v1-<UTC timestamp>/, removes both, and moves the runtime credential, unchanged, into the
#     state directory; /var/lib/dillabot/<installation> is left as it is;
#   - it installs this release's dillabot and dilla-bot-core, root-owned, and this release's dilla.service;
#   - it sets [bot] enabled = true with state_dir in dilla.toml, restarts dilla.service, and checks that the Bot runs
#     under it as dillabot with no capabilities. Any failure puts every changed file back.
# Every step logs one line, and a second run changes nothing and says so. No secret is ever printed: the credential
# and the keys are copied by path, never read into the script.
#
# DILLA_INSTALL_ROOT, when set, prefixes every path the script writes or reads (deploy/install_bot_test.sh). The
# release workflow stamps DILLA_BOT_RELEASE_TAG.
set -eu
DILLA_BOT_RELEASE_TAG=""

ROOT="${DILLA_INSTALL_ROOT:-}"
ROOT="${ROOT%/}"
log() { printf 'install-bot: %s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }
usage() {
	cat >&2 <<'USAGE'
usage: install-bot.sh --upgrade [options]

Puts the Dilla Bot under dillad, which runs it as the dillabot user inside dilla.service, and upgrades it there. A v1
Bot (dillabot.service, /etc/dillabot) is retired: its unit and bot.toml are backed up to
/var/backups/dilla/bot-v1-<timestamp>/ and removed, and its runtime credential moves, unchanged, to
/var/lib/dillabot/runtime.token. The Bot's state in /var/lib/dillabot is not touched. A second run changes nothing.

options:
  --upgrade               the upgrade (the only mode; kept so v1 command lines keep working)
  --dillad-config FILE    dillad config (default: /etc/dilla/dilla.toml)
  --version TAG           release tag (default: this release's stamped tag)
  --release-url URL       release asset URL (default: GitHub release URL for TAG)
  --binary FILE           local dillabot binary
  --helper FILE           local dilla-bot-core native helper
  --sums FILE             checksums for supplied local assets
  --insecure-skip-verify  allow supplied local assets without --sums
  --check-args            validate options without changing anything
  -h, --help              show this help

Upgrade dillad first (install.sh --upgrade from the same release). After the upgrade, use Dilla Server Settings >
Bot to enable Bot features for a community.
USAGE
}

DILLAD_CONFIG="$ROOT/etc/dilla/dilla.toml"
VERSION="$DILLA_BOT_RELEASE_TAG"
RELEASE_URL=""
BINARY=""
HELPER=""
SUMS=""
INSECURE=0
CHECK_ARGS=0
# How long the Bot may take to appear under dilla.service, and how long it must then keep its pid to count as healthy.
BOT_START_SECONDS="${DILLA_BOT_START_SECONDS:-30}"
BOT_HEALTH_SECONDS="${DILLA_BOT_HEALTH_SECONDS:-15}"
while [ "$#" -gt 0 ]; do
	flag="$1"; value=""
	case "$flag" in
	--*=*) value="${flag#*=}"; flag="${flag%%=*}"; shift ;;
	--check-args | --insecure-skip-verify | --upgrade | -h | --help) shift ;;
	--base-url | --instance-id | --module | --dillad-config | --version | --release-url | --binary | --helper | --sums)
		[ "$#" -ge 2 ] || die "$flag needs a value"
		value="$2"; shift 2 ;;
	-*) usage; die "unknown option $flag" ;;
	*) usage; die "unexpected argument $flag" ;;
	esac
	case "$flag" in
	--base-url | --instance-id) die "$flag is gone: the Bot runs under dillad now and learns the instance from dillad over its socket" ;;
	--module) die "--module is gone: the Bot loads the core beside dillad (/usr/local/bin/dilla_core_wasi.wasm), which install.sh installs" ;;
	--dillad-config) DILLAD_CONFIG="$value" ;;
	--version) VERSION="$value" ;;
	--release-url) RELEASE_URL="${value%/}" ;;
	--binary) BINARY="$value" ;;
	--helper) HELPER="$value" ;;
	--sums) SUMS="$value" ;;
	--insecure-skip-verify) INSECURE=1 ;;
	--check-args) CHECK_ARGS=1 ;;
	--upgrade) ;;
	-h | --help) usage; exit 0 ;;
	*) usage; die "unknown option $flag" ;;
	esac
done

for seconds in "$BOT_START_SECONDS" "$BOT_HEALTH_SECONDS"; do
	case "$seconds" in '' | *[!0-9]*) die "DILLA_BOT_START_SECONDS and DILLA_BOT_HEALTH_SECONDS must be whole numbers" ;; esac
	[ "${#seconds}" -le 4 ] || die "DILLA_BOT_START_SECONDS and DILLA_BOT_HEALTH_SECONDS must be below 10000"
done
if [ -n "$VERSION" ]; then
	case "$VERSION" in v[0-9]*) ;; *) die "--version must be a release tag such as v0.1.0" ;; esac
	case "$VERSION" in *[!A-Za-z0-9.+_-]*) die "--version contains unsupported characters" ;; esac
fi
DOWNLOAD=0
[ -n "$BINARY" ] || DOWNLOAD=1
[ -n "$HELPER" ] || DOWNLOAD=1
if [ "$DOWNLOAD" -eq 1 ] && [ -z "$RELEASE_URL" ]; then
	[ -n "$VERSION" ] || die "this install-bot.sh is not from a release; pass --version <tag> or --release-url <url>"
	RELEASE_URL="https://github.com/dilla-chat/dilla/releases/download/$VERSION"
fi
if [ -n "$BINARY$HELPER" ] && [ -z "$SUMS" ] && [ "$INSECURE" -eq 0 ]; then
	[ "$DOWNLOAD" -eq 1 ] || die "local assets require --sums FILE, or explicitly use --insecure-skip-verify"
fi
[ "$CHECK_ARGS" -eq 0 ] || exit 0
[ "$(id -u)" = 0 ] || die "run this as root"

ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in x86_64 | amd64) ARCH=amd64 ;; aarch64 | arm64) ARCH=arm64 ;; *) die "unsupported architecture $ARCH_RAW: dillabot is built for amd64 and arm64" ;; esac
BIN_NAME="dillabot-linux-$ARCH"
HELPER_NAME="dilla-bot-core-linux-$ARCH"
BIN_DIR="$ROOT/usr/local/bin"
LIB_DIR="$ROOT/usr/local/lib/dilla"
DILLAD_BIN="$BIN_DIR/dillad"
UNIT_DIR="$ROOT/etc/systemd/system"
DILLA_UNIT="$UNIT_DIR/dilla.service"
STATE_DIR="$ROOT/var/lib/dillabot"
CREDENTIAL="$STATE_DIR/runtime.token"
BACKUP_PARENT="$ROOT/var/backups/dilla"
# The v1 Bot's files.
LEGACY_UNIT="$UNIT_DIR/dillabot.service"
LEGACY_CONF_DIR="$ROOT/etc/dillabot"
LEGACY_CONF="$LEGACY_CONF_DIR/bot.toml"
LEGACY_CREDENTIAL="$LEGACY_CONF_DIR/runtime.token"
LEGACY_HELPER="$ROOT/usr/local/libexec/dilla-bot-core"
LEGACY_MODULE="$LIB_DIR/dilla_core_wasi.wasm"
# The v1 installer's pre-upgrade copies, which hold the database key (Task 8: none outlives a confirmed upgrade).
OLD_COPIES="$ROOT/var/backups/dillabot"

[ -x "$DILLAD_BIN" ] || die "dillad is not installed at $DILLAD_BIN; install it first with install.sh"
[ -f "$DILLAD_CONFIG" ] || die "dillad config not found at $DILLAD_CONFIG"
[ -s "$BIN_DIR/dilla_core_wasi.wasm" ] || die "the core beside dillad ($BIN_DIR/dilla_core_wasi.wasm) is missing; upgrade dillad with install.sh --upgrade first"
id dillad >/dev/null 2>&1 || die "the dillad user does not exist; install dillad with install.sh first"
[ ! -L "$STATE_DIR" ] || die "$STATE_DIR is a symlink; the Bot's state directory must be a real directory"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT HUP INT TERM
have() { command -v "$1" >/dev/null 2>&1; }
fetch() {
	if have curl; then curl -fsSL --proto '=https,file' --proto-redir '=https' -o "$2" "$1" >&2 || die "download failed: $1"
	elif have wget; then case "$1" in https://*) ;; *) die "wget only fetches HTTPS URLs" ;; esac; wget -q --https-only -O "$2" "$1" || die "download failed: $1"
	else die "curl or wget is required to download release assets"; fi
}
get_asset() { # SOURCE RELEASE_NAME
	if [ -n "$1" ]; then [ -s "$1" ] || die "asset missing or empty: $1"; cp "$1" "$WORK/$2"; else fetch "$RELEASE_URL/$2" "$WORK/$2"; fi
}
get_asset "$BINARY" "$BIN_NAME"
get_asset "$HELPER" "$HELPER_NAME"
if [ -n "$SUMS" ]; then
	cp "$SUMS" "$WORK/SHA256SUMS"
elif [ "$DOWNLOAD" -eq 1 ]; then
	fetch "$RELEASE_URL/SHA256SUMS" "$WORK/SHA256SUMS"
elif [ "$INSECURE" -eq 1 ]; then
	log "warning: installing supplied local Bot assets without checksum verification"
fi
if [ -f "$WORK/SHA256SUMS" ]; then
	: >"$WORK/check.sums"
	for name in "$BIN_NAME" "$HELPER_NAME"; do
		awk -v n="$name" '($2 == n || $2 == "*" n) && $1 ~ /^[0-9a-f]+$/ && length($1) == 64' "$WORK/SHA256SUMS" >"$WORK/line"
		[ "$(wc -l <"$WORK/line")" -eq 1 ] || die "SHA256SUMS does not hold exactly one SHA-256 line for $name"
		cat "$WORK/line" >>"$WORK/check.sums"
	done
	(cd "$WORK" && sha256sum -c --strict --quiet check.sums) || die "checksum mismatch; nothing was changed"
	log "verified Bot assets against SHA256SUMS"
fi

# deploy/dilla.service, verbatim (deploy_test.go and install_bot_test.sh compare the two). The supervised Bot needs it:
# the keeper's capabilities, /run/dillad, ReadWritePaths=-/var/lib/dillabot and the sandbox rules bubblewrap needs.
cat >"$WORK/dilla.service" <<'UNIT'
[Unit]
Description=dilla server (dillad)
Documentation=https://github.com/dilla-chat/dilla/blob/main/docs/deploy/README.md
After=network-online.target dilla-media.service
Wants=network-online.target dilla-media.service

[Service]
Type=notify
NotifyAccess=main
ExecStart=/usr/local/bin/dillad serve --config=%E/dilla/dilla.toml
Restart=on-failure
RestartSec=5s
# exit.Config (78) means the operator must fix dilla.toml; restarting would loop.
RestartPreventExitStatus=78
# WatchdogSec is deliberately NOT set: dillad sends READY=1 and nothing else over
# NOTIFY_SOCKET, so a watchdog would have systemd abort a healthy service after
# the first interval.
# ACME issuance plus goose migrations plus the pre-migration VACUUM INTO can
# exceed the 90 s default on a large SQLite file.
TimeoutStartSec=300s
TimeoutStopSec=45s
# KillMode=mixed: a stop sends SIGTERM to serve alone, which stops the Bot it supervises through the bot-keeper
# (SIGTERM, a 10 s grace, then SIGKILL) and only then exits; the keeper ends when serve's control socket closes. The
# default, control-group, would SIGTERM the keeper and the Bot at once and skip the Bot's orderly shutdown. Anything
# still running when serve has exited, or at TimeoutStopSec, is SIGKILLed as before.
KillMode=mixed

User=dillad
Group=dillad
StateDirectory=dillad
# The default is 0755; this directory holds instances.key_history, the franking
# key and the database.
StateDirectoryMode=0700
ConfigurationDirectory=dilla
UMask=0077
# /run/dillad: dillad creates /run/dillad/bot (mode 0750, group dillabot) there and listens on
# /run/dillad/bot/gateway.sock for the Bot it supervises ([bot] in dilla.toml). systemd removes it at stop.
RuntimeDirectory=dillad
# The Bot runs inside this unit's sandbox as the dillabot user, and keeps its state and keys in
# /var/lib/dillabot (owned by dillabot, mode 0700, so dillad itself cannot read them). ProtectSystem=strict
# would otherwise leave that directory read-only to the Bot. The "-" keeps an instance without the Bot
# (no such directory) starting: systemd otherwise refuses to set up the namespace.
ReadWritePaths=-/var/lib/dillabot
# SQLCipher's enhanced memory security in the Bot locks its SQLite allocations in memory, as the
# dillabot unit allowed (LimitMEMLOCK=256M there). The limit is inherited by the Bot from this unit.
LimitMEMLOCK=256M

# CAP_NET_BIND_SERVICE: how a non-root process binds 443/tcp.
# CAP_SETUID CAP_SETGID: only for the bot-keeper child, which dillad starts before it opens any listener and
# which uses them for one thing, to start the configured Bot executable as the configured Bot user. The
# serving dillad drops both from every set (effective, permitted, inheritable, ambient and bounding) right
# after starting the keeper; `dillad bot-launch` drops every capability before it execs the Bot.
# CAP_SETPCAP: what lets the serving process and bot-launch remove those from the bounding set; both drop it
# as well, so only the keeper keeps it.
AmbientCapabilities=CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID CAP_SETPCAP
CapabilityBoundingSet=CAP_NET_BIND_SERVICE CAP_SETUID CAP_SETGID CAP_SETPCAP
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
# DevicePolicy=closed instead of PrivateDevices=yes: the Bot's bubblewrap sandbox mounts /proc and /dev
# inside its own user namespace, and PrivateDevices blocks that nested mount in the supported Debian LXC
# deployment (the dillabot unit made the same change). DevicePolicy=closed still denies every device node
# but the pseudo devices (/dev/null, /dev/zero, /dev/random, ...).
DevicePolicy=closed
ProtectProc=invisible
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
# AF_NETLINK is required, not optional: Go's net.Interfaces reads the interface
# addresses over netlink, and LiveKit's SFU setup and pion/turn's network both
# call it. Without it serve fails at start with "netlinkrib: address family not
# supported by protocol".
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
# RestrictSUIDSGID is deliberately NOT set: it blocks bubblewrap's bind mounts in this LXC, as the dillabot
# unit found. NoNewPrivileges=yes still means no set-id binary can gain privileges, for dillad or the Bot.
LockPersonality=yes
# MemoryDenyWriteExecute is deliberately NOT set: wazero's compiler back end
# writes and then executes generated machine code, so setting it would make the
# MLS path fail at runtime rather than at start. A blind copy of a hardening
# template is exactly how that regression arrives.

[Install]
WantedBy=multi-user.target
UNIT

# bot_toml FILE prints FILE with [bot] enabled = true and state_dir = '/var/lib/dillabot'. Keys already in [bot] are
# replaced in place, missing ones go right under its header, and a file without [bot] gains the table at its end.
# Nothing else changes, so a file that already says both prints unchanged.
bot_toml() {
	awk -v sd="'/var/lib/dillabot'" '
		function key(l, k) { return l ~ ("^" k "[ \t]*=") }
		{ line = $0; sub(/^[ \t]+/, "", line); indent = substr($0, 1, length($0) - length(line)) }
		NR == FNR {
			if (line ~ /^\[/) { inside = (line ~ /^\[bot\][ \t]*(#.*)?$/); if (inside) table = 1 }
			else if (inside && key(line, "enabled")) e = 1
			else if (inside && key(line, "state_dir")) s = 1
			next
		}
		FNR == 1 { inside = 0 }
		line ~ /^\[/ {
			inside = (line ~ /^\[bot\][ \t]*(#.*)?$/)
			print
			if (inside && !e) print "  enabled = true"
			if (inside && !s) print "  state_dir = " sd
			next
		}
		inside && key(line, "enabled") { print indent "enabled = true"; next }
		inside && key(line, "state_dir") { print indent "state_dir = " sd; next }
		{ print }
		END { if (!table) { print ""; print "[bot]"; print "  enabled = true"; print "  state_dir = " sd } }
	' "$1" "$1"
}

# --- bot health: begin (install.sh and install-bot.sh carry this block verbatim; install_bot_test.sh compares them) ---
# bot_pids PROCS UID prints the pids listed in the control-group file PROCS that are the Bot: every uid (real,
# effective, saved, filesystem) is UID, and argv[0] is /usr/local/bin/dillabot, which bot-launch passes when it execs
# the Bot. The Bot's helper and its sandbox run as the same user under other names.
bot_pids() {
	while IFS= read -r bp_pid; do
		case "$bp_pid" in '' | *[!0-9]*) continue ;; esac
		[ -r "$ROOT/proc/$bp_pid/status" ] || continue
		[ "$(awk '$1 == "Uid:" { print $2, $3, $4, $5 }' "$ROOT/proc/$bp_pid/status")" = "$2 $2 $2 $2" ] || continue
		[ "$(tr '\0' '\n' <"$ROOT/proc/$bp_pid/cmdline" 2>/dev/null | head -n 1)" = /usr/local/bin/dillabot ] || continue
		printf '%s\n' "$bp_pid"
	done <"$1"
}
# bot_health checks the supervised Bot from outside, as root: dilla.service is active; exactly one process in its
# control group is the Bot, running as dillabot; it keeps that pid for BOT_HEALTH_SECONDS (the supervisor did not
# restart it); and its /proc status shows every capability set (CapInh, CapPrm, CapEff, CapBnd, CapAmb) empty and
# NoNewPrivs set. That last part is the check of the keeper's real non-root chain: dillad starts the keeper with the
# unit's ambient capabilities, the keeper starts bot-launch as dillabot, and bot-launch empties every set before it
# execs the Bot. It logs one line when the Bot passes, and "RED bot:" with the reason when it does not.
bot_health() {
	bh_uid="$(id -u dillabot 2>/dev/null)" || bh_uid=""
	[ -n "$bh_uid" ] || { log "RED bot: the dillabot user does not resolve"; return 1; }
	bh_cg="$(systemctl show -p ControlGroup --value dilla 2>/dev/null)" || bh_cg=""
	case "$bh_cg" in /*) ;; *) log "RED bot: systemd names no control group for dilla.service"; return 1 ;; esac
	bh_procs=""
	for bh_f in "$ROOT/sys/fs/cgroup$bh_cg/cgroup.procs" "$ROOT/sys/fs/cgroup/unified$bh_cg/cgroup.procs" "$ROOT/sys/fs/cgroup/systemd$bh_cg/cgroup.procs"; do
		if [ -r "$bh_f" ]; then bh_procs="$bh_f"; break; fi
	done
	bh_pid=""
	bh_waited=0
	while :; do
		systemctl is-active --quiet dilla || { log "RED bot: dilla.service is not active; read journalctl -o cat -u dilla"; return 1; }
		bh_now=""
		[ -z "$bh_procs" ] || bh_now="$(bot_pids "$bh_procs" "$bh_uid")"
		if [ "$(printf '%s\n' "$bh_now" | grep -c .)" -gt 1 ]; then
			log "RED bot: more than one Bot process runs under dilla.service: $(printf '%s' "$bh_now" | tr '\n' ' ')"
			return 1
		fi
		if [ -z "$bh_pid" ]; then
			if [ -n "$bh_now" ]; then
				bh_pid="$bh_now"
				bh_waited=0
				continue
			fi
			[ "$bh_waited" -lt "$BOT_START_SECONDS" ] || {
				log "RED bot: no Bot process runs under dilla.service after ${BOT_START_SECONDS}s; read journalctl -o cat -u dilla"
				return 1
			}
		else
			[ "$bh_now" = "$bh_pid" ] || {
				log "RED bot: the Bot (pid $bh_pid) did not stay up for ${BOT_HEALTH_SECONDS}s; read journalctl -o cat -u dilla"
				return 1
			}
			[ "$bh_waited" -lt "$BOT_HEALTH_SECONDS" ] || break
		fi
		sleep 1
		bh_waited=$((bh_waited + 1))
	done
	bh_status="$ROOT/proc/$bh_pid/status"
	bh_sets="$(awk '$1 ~ /^Cap(Inh|Prm|Eff|Bnd|Amb):$/ { n++ } END { print n + 0 }' "$bh_status" 2>/dev/null)" || bh_sets=0
	[ "$bh_sets" = 5 ] || { log "RED bot: could not read the Bot's (pid $bh_pid) five capability sets from its /proc status"; return 1; }
	bh_caps="$(awk '$1 ~ /^Cap(Inh|Prm|Eff|Bnd|Amb):$/ && $2 !~ /^0+$/ { printf "%s%s=%s", sep, substr($1, 1, length($1) - 1), $2; sep = " " }' "$bh_status")"
	[ -z "$bh_caps" ] || { log "RED bot: the Bot (pid $bh_pid) holds capabilities: $bh_caps; it must hold none"; return 1; }
	[ "$(awk '$1 == "NoNewPrivs:" { print $2 }' "$bh_status")" = 1 ] || { log "RED bot: the Bot (pid $bh_pid) runs without no_new_privs"; return 1; }
	log "the Bot runs under dilla.service as dillabot (pid $bh_pid), holding no capabilities"
}
# --- bot health: end ---

# doctor_bot runs `dillad doctor` as dillad and requires its config and bot legs OK, with the Bot enabled. It logs
# the bot leg's line, or the reason and doctor's own report.
doctor_bot() {
	dr_out="$(runuser -u dillad -- "$DILLAD_BIN" doctor --config="$DILLAD_CONFIG" 2>&1)" || :
	dr_config="$(printf '%s\n' "$dr_out" | awk '$2 == "config" { print $1; exit }')"
	dr_bot="$(printf '%s\n' "$dr_out" | awk '$2 == "bot" { print; exit }')"
	dr_detail="$(printf '%s\n' "$dr_bot" | awk '{ $1 = ""; $2 = ""; sub(/^ +/, ""); print }')"
	if [ "$dr_config" != OK ]; then
		printf '%s\n' "$dr_out" | sed 's/^/  /' >&2
		log "dillad doctor: config is not OK"
		return 1
	fi
	if [ -z "$dr_bot" ]; then
		log "dillad doctor has no bot leg: this dillad predates the supervised Bot; upgrade it with install.sh --upgrade from this release first"
		return 1
	fi
	case "$dr_bot" in
	"OK "*)
		case "$dr_detail" in disabled*) log "dillad doctor: bot is $dr_detail"; return 1 ;; esac
		log "dillad doctor: bot OK, $dr_detail"
		;;
	*)
		log "dillad doctor: bot is not OK: $dr_bot"
		return 1
		;;
	esac
}

# Deletes every pre-upgrade copy the v1 installer left (they hold the database key and no rollback uses them). Only
# plain directories whose names match its own pattern are touched; symlinks and other entries are left alone.
remove_old_copies() {
	for dir in "$OLD_COPIES"/[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z-????????; do
		case "${dir##*/}" in *[!0-9A-Za-z-]*) continue ;; esac
		if [ -L "$dir" ] || [ ! -d "$dir" ]; then continue; fi
		if rm -rf "$dir"; then log "removed pre-upgrade copy $dir"; else log "warning: could not remove pre-upgrade copy $dir (it holds the database key)"; fi
	done
}
old_copies_exist() {
	for dir in "$OLD_COPIES"/[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z-????????; do
		if [ -d "$dir" ] && [ ! -L "$dir" ]; then return 0; fi
	done
	return 1
}

# ------------------------------------------------------------------ plan ----
# What this run has to change. Nothing is changed while the plan is made.
LEGACY=0
if [ -e "$LEGACY_UNIT" ] || [ -e "$LEGACY_CONF" ] || [ -e "$LEGACY_CREDENTIAL" ]; then LEGACY=1; fi
if [ -f "$LEGACY_CONF" ]; then
	legacy_state="$(awk -F= '$1 ~ /^[ \t]*state_dir[ \t]*$/ { v = $2; gsub(/^[ \t]*["\047]|["\047][ \t]*(#.*)?$/, "", v); print v }' "$LEGACY_CONF" | tail -n 1)"
	case "$legacy_state" in
	'' | /var/lib/dillabot | /var/lib/dillabot/) ;;
	*) die "the v1 Bot's state_dir is $legacy_state; dillad runs the Bot with /var/lib/dillabot (dilla.service makes only that writable). Move the state there and set state_dir in $LEGACY_CONF, then run this again. Nothing was changed" ;;
	esac
fi
if [ -f "$LEGACY_CREDENTIAL" ] && [ -f "$CREDENTIAL" ] && ! cmp -s "$LEGACY_CREDENTIAL" "$CREDENTIAL"; then
	die "two different runtime credentials exist, $LEGACY_CREDENTIAL and $CREDENTIAL; keep the one dillad knows and remove the other, then run this again. Nothing was changed"
fi
NEED_ACCOUNT=0
id dillabot >/dev/null 2>&1 || NEED_ACCOUNT=1
NEED_GROUP=0
case " $(id -nG dillad 2>/dev/null || :) " in *" dillabot "*) ;; *) NEED_GROUP=1 ;; esac
BOT_UID=""
[ "$NEED_ACCOUNT" -eq 1 ] || BOT_UID="$(id -u dillabot)"
NEED_STATE_DIR=0
if [ ! -d "$STATE_DIR" ] || [ "$NEED_ACCOUNT" -eq 1 ] || [ "$(stat -c '%u %a' "$STATE_DIR")" != "$BOT_UID 700" ]; then NEED_STATE_DIR=1; fi
NEED_OWNER=0
if [ -d "$STATE_DIR" ] && [ -n "$BOT_UID" ] && [ -n "$(find "$STATE_DIR" -mindepth 1 ! -uid "$BOT_UID" -print -quit)" ]; then NEED_OWNER=1; fi
NEED_CREDENTIAL=0
[ -f "$CREDENTIAL" ] || NEED_CREDENTIAL=1
NEED_BINARIES=0
cmp -s "$WORK/$BIN_NAME" "$BIN_DIR/dillabot" || NEED_BINARIES=1
cmp -s "$WORK/$HELPER_NAME" "$LIB_DIR/dilla-bot-core" || NEED_BINARIES=1
NEED_UNIT=0
cmp -s "$WORK/dilla.service" "$DILLA_UNIT" || NEED_UNIT=1
bot_toml "$DILLAD_CONFIG" >"$WORK/dilla.toml"
NEED_TOML=0
cmp -s "$WORK/dilla.toml" "$DILLAD_CONFIG" || NEED_TOML=1
NEED_RESTART=$((LEGACY + NEED_ACCOUNT + NEED_GROUP + NEED_STATE_DIR + NEED_OWNER + NEED_CREDENTIAL + NEED_BINARIES + NEED_UNIT + NEED_TOML))

if [ "$NEED_RESTART" -eq 0 ]; then
	if old_copies_exist; then
		remove_old_copies
	else
		log "nothing to change: the Bot already runs under dillad from this release"
	fi
	doctor_bot || die "the Bot's files fail dillad doctor; nothing was changed"
	bot_health || die "the Bot is not healthy under dillad; nothing was changed"
	exit 0
fi

# ---------------------------------------------------------------- change ----
# What has changed so far, for the rollback.
BACKUP=""
LEGACY_STOPPED=0
WAS_ENABLED=0
WAS_ACTIVE=0
CREDENTIAL_ADDED=0
NEW_RUNTIME=""
BINARIES_CHANGED=0
UNIT_CHANGED=0
LEGACY_REMOVED=0
TOML_CHANGED=0
RESTARTED=0

# restore SRC DEST puts SRC back at DEST, through a rename.
restore() { cp -p "$1" "$2.restore" && mv -f "$2.restore" "$2"; }

# rollback REASON puts every changed file back, restarts dilla on the previous configuration if it was restarted, and
# starts the v1 Bot again if it ran before. It always exits.
rollback() {
	log "error: $1"
	rb_ok=0
	if [ "$TOML_CHANGED" -eq 1 ]; then restore "$WORK/dilla.toml.prev" "$DILLAD_CONFIG" || { log "could not restore $DILLAD_CONFIG"; rb_ok=1; }; fi
	if [ "$UNIT_CHANGED" -eq 1 ]; then restore "$BACKUP/dilla.service" "$DILLA_UNIT" || { log "could not restore $DILLA_UNIT"; rb_ok=1; }; fi
	if [ "$BINARIES_CHANGED" -eq 1 ]; then
		for pair in "dillabot:$BIN_DIR/dillabot" "dilla-bot-core:$LIB_DIR/dilla-bot-core"; do
			if [ -f "$WORK/prev/${pair%%:*}" ]; then
				restore "$WORK/prev/${pair%%:*}" "${pair#*:}" || { log "could not restore ${pair#*:}"; rb_ok=1; }
			else
				rm -f "${pair#*:}" || rb_ok=1
			fi
		done
	fi
	if [ "$LEGACY_REMOVED" -eq 1 ]; then
		for name in dillabot.service bot.toml; do
			[ -f "$BACKUP/$name" ] || continue
			case "$name" in dillabot.service) dest="$LEGACY_UNIT" ;; *) dest="$LEGACY_CONF" ;; esac
			restore "$BACKUP/$name" "$dest" || { log "could not restore $dest"; rb_ok=1; }
		done
	fi
	if [ "$CREDENTIAL_ADDED" -eq 1 ]; then
		rm -f "$CREDENTIAL" || rb_ok=1
		[ -z "$NEW_RUNTIME" ] || log "the runtime $NEW_RUNTIME this run registered is unused; revoke it with: dillad admin bot-runtime revoke --id=$NEW_RUNTIME --config=$DILLAD_CONFIG"
	fi
	if [ "$UNIT_CHANGED" -eq 1 ] || [ "$LEGACY_REMOVED" -eq 1 ]; then systemctl daemon-reload || log "systemd could not reload the restored units"; fi
	if [ "$RESTARTED" -eq 1 ]; then
		systemctl restart dilla || log "warning: dilla did not restart on the restored configuration; read journalctl -o cat -u dilla"
	fi
	if [ "$LEGACY_STOPPED" -eq 1 ]; then
		[ "$WAS_ENABLED" -eq 0 ] || systemctl enable dillabot || { log "could not enable dillabot.service again"; rb_ok=1; }
		[ "$WAS_ACTIVE" -eq 0 ] || systemctl start dillabot || log "the v1 Bot did not start again; read journalctl -o cat -u dillabot"
	fi
	if [ "$rb_ok" -ne 0 ]; then
		die "$1; the rollback was incomplete: the previous units and bot.toml are in ${BACKUP:-(no backup was needed)}, and every step above names what it could not restore"
	fi
	if [ "$LEGACY" -eq 1 ]; then
		die "$1; restored the v1 Bot (dillabot.service and /etc/dillabot) and the previous dilla.toml, dilla.service and Bot binaries"
	fi
	die "$1; restored the previous dilla.toml, dilla.service and Bot binaries"
}

# The backup: the v1 unit and bot.toml, and the dilla.service this run replaces. Never the database key or the
# runtime credential: the state stays where it is, and the credential stays in /etc/dillabot until the end.
if [ "$LEGACY" -eq 1 ] || [ "$NEED_UNIT" -eq 1 ]; then
	install -d -m 0700 -o root -g root "$BACKUP_PARENT" || die "cannot create $BACKUP_PARENT; nothing was changed"
	BACKUP="$BACKUP_PARENT/bot-v1-$(date -u +%Y%m%dT%H%M%SZ)"
	mkdir -m 0700 "$BACKUP" || die "cannot create $BACKUP; nothing was changed"
	saved=""
	for pair in "dillabot.service:$LEGACY_UNIT" "bot.toml:$LEGACY_CONF" "dilla.service:$DILLA_UNIT"; do
		[ -f "${pair#*:}" ] || continue
		[ "${pair%%:*}" != dilla.service ] || [ "$NEED_UNIT" -eq 1 ] || continue
		cp -p "${pair#*:}" "$BACKUP/${pair%%:*}" || die "could not back up ${pair#*:}; nothing was changed"
		saved="$saved${saved:+, }${pair%%:*}"
	done
	case "$saved" in *", "*) saved="$(printf '%s' "$saved" | sed 's/\(.*\), /\1 and /')" ;; esac
	log "backed up $saved to $BACKUP (no keys, no credential)"
fi

if [ "$LEGACY" -eq 1 ] && [ -e "$LEGACY_UNIT" ]; then
	if systemctl is-enabled --quiet dillabot 2>/dev/null; then WAS_ENABLED=1; fi
	if systemctl is-active --quiet dillabot; then WAS_ACTIVE=1; fi
	systemctl disable --now dillabot || die "could not stop and disable dillabot.service; only the backup in $BACKUP was made"
	LEGACY_STOPPED=1
	log "stopped and disabled dillabot.service"
fi

if [ "$NEED_ACCOUNT" -eq 1 ]; then
	useradd --system --user-group --no-create-home --home-dir /var/lib/dillabot --shell /usr/sbin/nologin dillabot || rollback "could not create the dillabot user"
	BOT_UID="$(id -u dillabot)" || rollback "the new dillabot user does not resolve"
	log "created the dillabot user"
fi
if [ "$NEED_GROUP" -eq 1 ]; then
	# dillad hands the Bot socket's directory to the dillabot group; serve refuses to start without the membership.
	usermod -aG dillabot dillad || rollback "could not add dillad to the dillabot group"
	log "added dillad to the dillabot group"
fi
if [ "$NEED_STATE_DIR" -eq 1 ]; then
	if [ -d "$STATE_DIR" ]; then
		{ chown dillabot:dillabot "$STATE_DIR" && chmod 0700 "$STATE_DIR"; } || rollback "could not set $STATE_DIR to 0700 dillabot"
		log "set /var/lib/dillabot to 0700 dillabot"
	else
		install -d -m 0700 -o dillabot -g dillabot "$STATE_DIR" || rollback "could not create $STATE_DIR"
		log "created /var/lib/dillabot (0700 dillabot)"
	fi
fi
if [ "$NEED_OWNER" -eq 1 ]; then
	# Ownership only: no byte under the state directory changes.
	chown -R dillabot:dillabot "$STATE_DIR" || rollback "could not give dillabot its state under $STATE_DIR"
	log "gave dillabot the files under /var/lib/dillabot it did not own"
fi
if [ "$NEED_CREDENTIAL" -eq 1 ]; then
	if [ -f "$LEGACY_CREDENTIAL" ]; then
		# Copied by path, unchanged; the v1 copy is removed once the Bot runs healthy under dillad.
		{ install -m 0600 -o dillabot -g dillabot "$LEGACY_CREDENTIAL" "$CREDENTIAL.new" && mv -f "$CREDENTIAL.new" "$CREDENTIAL"; } ||
			rollback "could not move the runtime credential into $STATE_DIR"
		CREDENTIAL_ADDED=1
		log "moved the runtime credential to /var/lib/dillabot/runtime.token, unchanged"
	else
		# The CLI requires an owner-private parent with no symlink ancestors; it never prints the credential.
		mkdir -m 0700 "$WORK/credential"
		admin_out="$("$DILLAD_BIN" admin bot-runtime create --config="$DILLAD_CONFIG" --out="$WORK/credential/runtime.token")" ||
			rollback "dillad could not create a Bot runtime credential"
		[ -s "$WORK/credential/runtime.token" ] || rollback "dillad did not write a runtime credential file"
		NEW_RUNTIME="$(printf '%s\n' "$admin_out" | sed -n 's/^created bot runtime \([0-9a-f]*\);.*/\1/p')"
		{ install -m 0600 -o dillabot -g dillabot "$WORK/credential/runtime.token" "$CREDENTIAL.new" && mv -f "$CREDENTIAL.new" "$CREDENTIAL"; } ||
			rollback "could not install the new runtime credential"
		CREDENTIAL_ADDED=1
		log "dillad registered a new runtime credential: $admin_out"
	fi
fi
if [ "$NEED_BINARIES" -eq 1 ]; then
	# Root-owned, never group- or world-writable, in root-owned directories: the keeper refuses anything else.
	mkdir -m 0700 "$WORK/prev"
	[ ! -f "$BIN_DIR/dillabot" ] || cp -p "$BIN_DIR/dillabot" "$WORK/prev/dillabot"
	[ ! -f "$LIB_DIR/dilla-bot-core" ] || cp -p "$LIB_DIR/dilla-bot-core" "$WORK/prev/dilla-bot-core"
	BINARIES_CHANGED=1
	{
		install -d -m 0755 -o root -g root "$LIB_DIR" &&
			install -m 0755 -o root -g root "$WORK/$BIN_NAME" "$BIN_DIR/dillabot.new" && mv -f "$BIN_DIR/dillabot.new" "$BIN_DIR/dillabot" &&
			install -m 0755 -o root -g root "$WORK/$HELPER_NAME" "$LIB_DIR/dilla-bot-core.new" && mv -f "$LIB_DIR/dilla-bot-core.new" "$LIB_DIR/dilla-bot-core"
	} || rollback "could not install the Bot binaries"
	log "installed dillabot and dilla-bot-core (root:root 0755)"
fi
if [ "$NEED_UNIT" -eq 1 ]; then
	UNIT_CHANGED=1
	{ install -m 0644 "$WORK/dilla.service" "$DILLA_UNIT.new" && mv -f "$DILLA_UNIT.new" "$DILLA_UNIT"; } || rollback "could not install dilla.service"
	log "installed dilla.service from this release"
fi
if [ "$LEGACY" -eq 1 ]; then
	LEGACY_REMOVED=1
	rm -f "$LEGACY_UNIT" "$LEGACY_CONF" || rollback "could not remove dillabot.service and $LEGACY_CONF"
	log "removed dillabot.service and /etc/dillabot/bot.toml"
fi
if [ "$UNIT_CHANGED" -eq 1 ] || [ "$LEGACY_REMOVED" -eq 1 ]; then
	systemctl daemon-reload || rollback "systemd could not reload its units"
	log "reloaded systemd's units"
fi
if [ "$NEED_TOML" -eq 1 ]; then
	cp -p "$DILLAD_CONFIG" "$WORK/dilla.toml.prev"
	TOML_CHANGED=1
	# cp -p keeps dilla.toml's owner and mode (root:dillad 0640); the rename makes the change atomic.
	{ cp -p "$DILLAD_CONFIG" "$DILLAD_CONFIG.new" && cat "$WORK/dilla.toml" >"$DILLAD_CONFIG.new" && mv -f "$DILLAD_CONFIG.new" "$DILLAD_CONFIG"; } ||
		rollback "could not write [bot] into $DILLAD_CONFIG"
	log "set [bot] enabled = true, state_dir = '/var/lib/dillabot' in dilla.toml"
fi
doctor_bot || rollback "dillad doctor refuses the Bot under dillad; dilla.service was not restarted"
RESTARTED=1
systemctl restart dilla || rollback "dilla.service did not restart with the Bot enabled; read journalctl -o cat -u dilla"
log "restarted dilla.service"
bot_health || rollback "the Bot did not come up healthy under dillad"

# --------------------------------------------------------------- confirm ----
# No rollback runs after this point: the v1 leftovers go, and with them every copy of the database key.
if [ -f "$LEGACY_CREDENTIAL" ]; then
	if rm -f "$LEGACY_CREDENTIAL"; then log "removed the v1 credential copy /etc/dillabot/runtime.token"; else log "warning: could not remove $LEGACY_CREDENTIAL"; fi
fi
if [ -d "$LEGACY_CONF_DIR" ]; then rmdir "$LEGACY_CONF_DIR" 2>/dev/null || log "warning: $LEGACY_CONF_DIR holds files this script did not write; it was left in place"; fi
if [ "$LEGACY" -eq 1 ] && { [ -e "$LEGACY_HELPER" ] || [ -e "$LEGACY_MODULE" ]; }; then
	if rm -f "$LEGACY_HELPER" "$LEGACY_MODULE"; then log "removed the v1 helper and module"; else log "warning: could not remove $LEGACY_HELPER or $LEGACY_MODULE"; fi
fi
remove_old_copies
if [ -n "$BACKUP" ]; then
	log "done: the Bot runs under dillad; the previous units and bot.toml are kept in $BACKUP"
else
	log "done: the Bot runs under dillad"
fi
