#!/bin/sh
# Guided Linux installation for dillabot on a host that already runs dillad.
# Run as root. The release workflow stamps DILLA_BOT_RELEASE_TAG.
set -eu
DILLA_BOT_RELEASE_TAG=""

ROOT="${DILLA_INSTALL_ROOT:-}"
ROOT="${ROOT%/}"
log() { printf 'install-bot: %s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }
usage() {
	cat >&2 <<'USAGE'
usage: install-bot.sh --base-url HTTPS_URL --instance-id ID [options]

Installs dillabot beside an existing dillad server. It creates a Bot runtime
credential with `dillad admin bot-runtime create`, then stores it in an
owner-only file readable only by the dillabot service account.

required:
  --base-url URL          public Dilla server origin, e.g. https://chat.example.org
  --instance-id ID        32 lowercase hexadecimal instance ID
options:
  --dillad-config FILE    dillad config (default: /etc/dilla/dilla.toml)
  --version TAG           release tag (default: this release's stamped tag)
  --release-url URL       release asset URL (default: GitHub release URL for TAG)
  --binary FILE           local dillabot binary
  --helper FILE           local dilla-bot-core native helper
  --module FILE           local dilla_core_wasi.wasm module
  --sums FILE             checksums for supplied local assets
  --upgrade               replace Bot binaries and unit; preserve config, credential and state
  --keep-backups N        after a successful upgrade keep the N newest pre-upgrade copies
                          under /var/backups/dillabot, the new one included (N >= 1, default 3)
  --insecure-skip-verify  allow supplied local assets without --sums
  --check-args            validate options without changing anything
  -h, --help              show this help

The server and instance administrator must already exist. After installation,
use Dilla Server Settings > Bot to enable Bot features for a community.
USAGE
}

BASE_URL=""
INSTANCE_ID=""
DILLAD_CONFIG="/etc/dilla/dilla.toml"
VERSION="$DILLA_BOT_RELEASE_TAG"
RELEASE_URL=""
BINARY=""
HELPER=""
MODULE=""
SUMS=""
INSECURE=0
UPGRADE=0
KEEP_BACKUPS=3
CHECK_ARGS=0
while [ "$#" -gt 0 ]; do
	flag="$1"; value=""
	case "$flag" in
	--*=*) value="${flag#*=}"; flag="${flag%%=*}"; shift ;;
	--check-args | --insecure-skip-verify | --upgrade | -h | --help) shift ;;
	--base-url | --instance-id | --dillad-config | --version | --release-url | --binary | --helper | --module | --sums | --keep-backups)
		[ "$#" -ge 2 ] || die "$flag needs a value"
		value="$2"; shift 2 ;;
	-*) usage; die "unknown option $flag" ;;
	*) usage; die "unexpected argument $flag" ;;
	esac
	case "$flag" in
	--base-url) BASE_URL="$value" ;;
	--instance-id) INSTANCE_ID="$value" ;;
	--dillad-config) DILLAD_CONFIG="$value" ;;
	--version) VERSION="$value" ;;
	--release-url) RELEASE_URL="${value%/}" ;;
	--binary) BINARY="$value" ;;
	--helper) HELPER="$value" ;;
	--module) MODULE="$value" ;;
	--sums) SUMS="$value" ;;
	--keep-backups) KEEP_BACKUPS="$value" ;;
	--insecure-skip-verify) INSECURE=1 ;;
	--check-args) CHECK_ARGS=1 ;;
	--upgrade) UPGRADE=1 ;;
	-h | --help) usage; exit 0 ;;
	*) usage; die "unknown option $flag" ;;
	esac
done

case "$KEEP_BACKUPS" in
'' | *[!0-9]*) die "--keep-backups must be a whole number of at least 1" ;;
esac
[ "${#KEEP_BACKUPS}" -le 6 ] && [ "$KEEP_BACKUPS" -ge 1 ] || die "--keep-backups must be a whole number of at least 1"

if [ "$UPGRADE" -eq 0 ]; then
	[ -n "$BASE_URL" ] || die "--base-url is required"
	[ -n "$INSTANCE_ID" ] || die "--instance-id is required"
	case "$BASE_URL" in
	https://*) ORIGIN="${BASE_URL#https://}" ;;
	*) die "--base-url must use https://" ;;
	esac
	case "$ORIGIN" in
	'' | */* | *\?* | *\#* | *[!A-Za-z0-9:._-]*) die "--base-url must be a plain HTTPS origin without path, query, fragment or credentials" ;;
	esac
	HOST="$ORIGIN"
	case "$ORIGIN" in
	*:*)
		HOST="${ORIGIN%:*}"
		PORT="${ORIGIN##*:}"
		case "$PORT" in '' | *[!0-9]*) die "--base-url port must be a decimal number" ;; esac
		[ "${#PORT}" -le 5 ] && [ "$PORT" -gt 0 ] && [ "$PORT" -le 65535 ] || die "--base-url port must be between 1 and 65535"
		case "$HOST" in *:*) die "--base-url IPv6 literals are not supported by this installer" ;; esac
		;;
	esac
	case "$HOST" in
	localhost | 127.0.0.1 | *.*) ;;
	*) die "--base-url must include a server hostname" ;;
	esac
	case "$HOST" in
	.* | *. | *..* | *[!A-Za-z0-9.-]*) die "--base-url contains an invalid hostname" ;;
	esac
	[ "${#INSTANCE_ID}" -eq 32 ] || die "--instance-id must be 32 lowercase hexadecimal characters"
	case "$INSTANCE_ID" in *[!0-9a-f]*) die "--instance-id must be 32 lowercase hexadecimal characters" ;; esac
	[ "$INSTANCE_ID" != 00000000000000000000000000000000 ] || die "--instance-id must not be zero"
	[ -f "$DILLAD_CONFIG" ] || die "dillad config not found at $DILLAD_CONFIG"
else
	[ -z "$BASE_URL$INSTANCE_ID" ] || die "--base-url and --instance-id are first-install options; --upgrade preserves bot.toml"
fi
if [ -n "$VERSION" ]; then
	case "$VERSION" in v[0-9]*) ;; *) die "--version must be a release tag such as v0.1.0" ;; esac
	case "$VERSION" in *[!A-Za-z0-9.+_-]*) die "--version contains unsupported characters" ;; esac
fi
DOWNLOAD=0
[ -n "$BINARY" ] || DOWNLOAD=1
[ -n "$HELPER" ] || DOWNLOAD=1
[ -n "$MODULE" ] || DOWNLOAD=1
if [ "$DOWNLOAD" -eq 1 ] && [ -z "$RELEASE_URL" ]; then
	[ -n "$VERSION" ] || die "this install-bot.sh is not from a release; pass --version <tag> or --release-url <url>"
	RELEASE_URL="https://github.com/jonasthim/dilla/releases/download/$VERSION"
fi
if [ -n "$BINARY$HELPER$MODULE" ] && [ -z "$SUMS" ] && [ "$INSECURE" -eq 0 ]; then
	[ "$DOWNLOAD" -eq 1 ] || die "local assets require --sums FILE, or explicitly use --insecure-skip-verify"
fi
[ "$CHECK_ARGS" -eq 0 ] || exit 0
[ "$(id -u)" = 0 ] || die "run this as root"

ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in x86_64 | amd64) ARCH=amd64 ;; aarch64 | arm64) ARCH=arm64 ;; *) die "unsupported architecture $ARCH_RAW: dillabot is built for amd64 and arm64" ;; esac
BIN_NAME="dillabot-linux-$ARCH"
HELPER_NAME="dilla-bot-core-linux-$ARCH"
MODULE_NAME="dilla_core_wasi.wasm"
BIN_DIR="$ROOT/usr/local/bin"
LIB_DIR="$ROOT/usr/local/lib/dilla"
LIBEXEC_DIR="$ROOT/usr/local/libexec"
CONF_DIR="$ROOT/etc/dillabot"
CONF="$CONF_DIR/bot.toml"
CREDENTIAL="$CONF_DIR/runtime.token"
DATA_DIR="$ROOT/var/lib/dillabot"
UNIT="$ROOT/etc/systemd/system/dillabot.service"
DILLAD_BIN="$ROOT/usr/local/bin/dillad"
if [ "$UPGRADE" -eq 1 ]; then
	for required in "$CONF" "$CREDENTIAL" "$DATA_DIR" "$UNIT" "$BIN_DIR/dillabot" "$LIBEXEC_DIR/dilla-bot-core" "$LIB_DIR/dilla_core_wasi.wasm"; do
		[ -e "$required" ] || die "--upgrade requires the existing Bot installation; missing $required"
	done
	id dillabot >/dev/null 2>&1 || die "--upgrade requires the existing dillabot account"
else
	[ -x "$DILLAD_BIN" ] || die "dillad is not installed at $DILLAD_BIN; install and start dillad first"
	[ -f "$DILLAD_CONFIG" ] || die "dillad config not found at $DILLAD_CONFIG"
	[ ! -e "$CONF_DIR" ] && [ ! -e "$CONF" ] && [ ! -e "$CREDENTIAL" ] && [ ! -e "$DATA_DIR" ] && [ ! -e "$UNIT" ] && [ ! -e "$BIN_DIR/dillabot" ] && [ ! -e "$LIBEXEC_DIR/dilla-bot-core" ] && [ ! -e "$LIB_DIR/dilla_core_wasi.wasm" ] || die "Bot files or state already exist; this guided installer only creates a new installation and will not replace Bot state or credentials"
fi

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT HUP INT TERM
have() { command -v "$1" >/dev/null 2>&1; }
fetch() {
	if have curl; then curl -fsSL --proto '=https,file' --proto-redir '=https' -o "$2" "$1" >&2 || die "download failed: $1"
	elif have wget; then case "$1" in https://*) ;; *) die "wget only fetches HTTPS URLs" ;; esac; wget -q --https-only -O "$2" "$1" || die "download failed: $1"
	else die "curl or wget is required to download release assets"; fi
}
get_asset() { # SOURCE RELEASE_NAME LOCAL_NAME
	if [ -n "$1" ]; then [ -s "$1" ] || die "asset missing or empty: $1"; cp "$1" "$WORK/$3"; else fetch "$RELEASE_URL/$2" "$WORK/$3"; fi
}
get_asset "$BINARY" "$BIN_NAME" "$BIN_NAME"
get_asset "$HELPER" "$HELPER_NAME" "$HELPER_NAME"
get_asset "$MODULE" "$MODULE_NAME" "$MODULE_NAME"
if [ -n "$SUMS" ]; then
	cp "$SUMS" "$WORK/SHA256SUMS"
elif [ "$DOWNLOAD" -eq 1 ]; then
	fetch "$RELEASE_URL/SHA256SUMS" "$WORK/SHA256SUMS"
elif [ "$INSECURE" -eq 1 ]; then
	log "warning: installing supplied local Bot assets without checksum verification"
fi
if [ -f "$WORK/SHA256SUMS" ]; then
	: >"$WORK/check.sums"
	for name in "$BIN_NAME" "$HELPER_NAME" "$MODULE_NAME"; do
		awk -v n="$name" '($2 == n || $2 == "*" n) && $1 ~ /^[0-9a-f]+$/ && length($1) == 64' "$WORK/SHA256SUMS" >"$WORK/line"
		[ "$(wc -l <"$WORK/line")" -eq 1 ] || die "SHA256SUMS does not hold exactly one SHA-256 line for $name"
		cat "$WORK/line" >>"$WORK/check.sums"
	done
	(cd "$WORK" && sha256sum -c --strict --quiet check.sums) || die "checksum mismatch; no Bot files were installed"
	log "verified Bot assets against SHA256SUMS"
fi

cat >"$WORK/dillabot.service" <<'UNIT'
[Unit]
Description=Dilla community bot
Documentation=https://github.com/jonasthim/dilla/blob/main/docs/deploy/README.md
After=network-online.target dilla.service
Wants=network-online.target
Requires=dilla.service
PartOf=dilla.service

[Service]
Type=simple
ExecStart=/usr/local/bin/dillabot --config=/etc/dillabot/bot.toml
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
User=dillabot
Group=dillabot
StateDirectory=dillabot
StateDirectoryMode=0700
UMask=0077
# SQLCipher's enhanced memory security locks SQLite allocations in memory.
LimitMEMLOCK=256M
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
# Bubblewrap mounts /proc and /dev inside its own user namespace; PrivateDevices
# blocks that nested mount in the supported Debian LXC deployment.
DevicePolicy=closed
ProtectProc=invisible
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
# RestrictSUIDSGID blocks bubblewrap's bind mounts in this LXC. The service
# remains unprivileged, with NoNewPrivileges=yes and a private state directory.
LockPersonality=yes
# The bot's WASI MLS engine uses wazero's compiler backend, which needs
# executable generated memory. MemoryDenyWriteExecute would break startup.

[Install]
WantedBy=multi-user.target
UNIT
install_assets() {
	install -m 0755 "$WORK/$BIN_NAME" "$BIN_DIR/dillabot.new" || return 1
	mv -f "$BIN_DIR/dillabot.new" "$BIN_DIR/dillabot" || return 1
	install -m 0755 "$WORK/$HELPER_NAME" "$LIBEXEC_DIR/dilla-bot-core.new" || return 1
	mv -f "$LIBEXEC_DIR/dilla-bot-core.new" "$LIBEXEC_DIR/dilla-bot-core" || return 1
	install -m 0644 "$WORK/$MODULE_NAME" "$LIB_DIR/dilla_core_wasi.wasm.new" || return 1
	mv -f "$LIB_DIR/dilla_core_wasi.wasm.new" "$LIB_DIR/dilla_core_wasi.wasm" || return 1
	install -m 0644 "$WORK/dillabot.service" "$UNIT.new" || return 1
	mv -f "$UNIT.new" "$UNIT" || return 1
}
# service_path prints the PATH the Bot service runs with, so the doctor's
# dependency check sees what the service sees rather than the caller's PATH
# (pct exec into the LXC has no /usr/local/bin). Order: an Environment=PATH in
# the installed unit, then systemd's manager default, then its documented value.
service_path() {
	sp=""
	if [ -r "$UNIT" ]; then
		sp="$(sed -n 's/^Environment=["]*PATH=\([^" ]*\)["]*[[:space:]]*$/\1/p' "$UNIT" | tail -n 1)"
	fi
	if [ -z "$sp" ]; then
		sp="$(systemctl show-environment 2>/dev/null | sed -n 's/^PATH=//p' | head -n 1)"
	fi
	[ -n "$sp" ] || sp="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin"
	printf '%s\n' "$sp"
}
run_bot_doctor() {
	doctor_runuser="$(command -v runuser)" || return 1
	doctor_env="$(command -v env)" || return 1
	doctor_path="$(service_path)"
	log "running dillabot doctor with the service PATH $doctor_path"
	"$doctor_runuser" -u dillabot -- "$doctor_env" "PATH=$doctor_path" "$BIN_DIR/dillabot" doctor --config="$CONF"
}

rollback_upgrade() { # BACKUP WAS_ACTIVE
	backup="$1"; was_active="$2"
	log "rolling back Bot binary and local-state changes"
	systemctl stop dillabot >/dev/null 2>&1 || :
	for name in dillabot dilla-bot-core dilla_core_wasi.wasm dillabot.service; do
		case "$name" in
		dillabot) dest="$BIN_DIR/dillabot" ;;
		dilla-bot-core) dest="$LIBEXEC_DIR/dilla-bot-core" ;;
		dilla_core_wasi.wasm) dest="$LIB_DIR/dilla_core_wasi.wasm" ;;
		dillabot.service) dest="$UNIT" ;;
		esac
		cp -a "$backup/files/$name" "$dest.restore" && mv -f "$dest.restore" "$dest" || log "rollback failed for $dest"
	done
	rm -rf "$DATA_DIR"
	cp -a "$backup/state" "$DATA_DIR" || log "rollback failed for Bot state; use $backup/state"
	systemctl daemon-reload || log "rollback daemon-reload failed"
	if [ "$was_active" -eq 1 ]; then systemctl start dillabot || log "previous Bot service did not restart; use systemctl status dillabot"; fi
}

# Keep the newest KEEP pre-upgrade copies. Only plain directories whose names
# match the installer's own pattern are touched, ordered by that name (the UTC
# timestamp prefix sorts chronologically); symlinks and other files are skipped.
# NEW, the copy this run made, is never a candidate: it counts as one of the KEEP
# whatever its random suffix sorts to within its second.
prune_backups() { # PARENT KEEP NEW
	parent="$1"; keep=$(($2 - 1)); new="$3"
	set --
	for dir in "$parent"/[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]T[0-9][0-9][0-9][0-9][0-9][0-9]Z-????????; do
		case "${dir##*/}" in
		*[!0-9A-Za-z-]*) continue ;;
		esac
		if [ -L "$dir" ] || [ ! -d "$dir" ] || [ "$dir" = "$new" ]; then continue; fi
		set -- "$@" "$dir"
	done
	[ "$#" -gt "$keep" ] || return 0
	remove=$(($# - keep))
	for dir in "$@"; do
		[ "$remove" -gt 0 ] || break
		remove=$((remove - 1))
		if rm -rf "$dir"; then log "removed old pre-upgrade copy $dir"; else log "warning: could not remove old pre-upgrade copy $dir"; fi
	done
}

if [ "$UPGRADE" -eq 1 ]; then
	log "checking current Bot files before upgrade"
	run_bot_doctor >&2 || die "the existing Bot files fail doctor; repair them before upgrading"
	WAS_ACTIVE=0
	if systemctl is-active --quiet dillabot; then WAS_ACTIVE=1; fi
	if [ "$WAS_ACTIVE" -eq 1 ]; then systemctl stop dillabot || die "could not stop dillabot; no files were changed"; fi
	BACKUP_PARENT="$ROOT/var/backups/dillabot"
	mkdir -p "$ROOT/var/backups" || { [ "$WAS_ACTIVE" -eq 0 ] || systemctl start dillabot; die "cannot create backup parent; no Bot files were changed"; }
	install -d -m 0700 -o root -g root "$BACKUP_PARENT" || { [ "$WAS_ACTIVE" -eq 0 ] || systemctl start dillabot; die "cannot secure backup parent; no Bot files were changed"; }
	BACKUP="$(mktemp -d "$BACKUP_PARENT/$(date -u +%Y%m%dT%H%M%SZ)-XXXXXXXX")" || { [ "$WAS_ACTIVE" -eq 0 ] || systemctl start dillabot; die "cannot create pre-upgrade backup"; }
	mkdir -m 0700 "$BACKUP/files" || { rm -rf "$BACKUP"; [ "$WAS_ACTIVE" -eq 0 ] || systemctl start dillabot; die "cannot create backup files directory"; }
	if ! cp -a "$DATA_DIR" "$BACKUP/state" || ! cp -a "$BIN_DIR/dillabot" "$BACKUP/files/dillabot" || ! cp -a "$LIBEXEC_DIR/dilla-bot-core" "$BACKUP/files/dilla-bot-core" || ! cp -a "$LIB_DIR/dilla_core_wasi.wasm" "$BACKUP/files/dilla_core_wasi.wasm" || ! cp -a "$UNIT" "$BACKUP/files/dillabot.service"; then
		rm -rf "$BACKUP"
		[ "$WAS_ACTIVE" -eq 0 ] || systemctl start dillabot
		die "could not make a complete pre-upgrade backup; no Bot files were changed"
	fi
	if ! install_assets; then
		rollback_upgrade "$BACKUP" "$WAS_ACTIVE"
		die "could not install the Bot assets; restored the previous files (backup: $BACKUP)"
	fi
	if ! run_bot_doctor >&2; then
		rollback_upgrade "$BACKUP" "$WAS_ACTIVE"
		die "upgraded Bot files failed doctor; restored the previous files and state (backup: $BACKUP)"
	fi
	systemctl daemon-reload || { rollback_upgrade "$BACKUP" "$WAS_ACTIVE"; die "systemd reload failed; restored the previous Bot files (backup: $BACKUP)"; }
	if [ "$WAS_ACTIVE" -eq 1 ]; then
		if ! systemctl start dillabot || ! systemctl is-active --quiet dillabot; then
			rollback_upgrade "$BACKUP" "$WAS_ACTIVE"
			die "upgraded Bot service did not remain active; restored the previous files and state (backup: $BACKUP)"
		fi
	fi
	log "Bot binaries upgraded; preserved config, runtime credential and state"
	log "cold pre-upgrade copy retained at $BACKUP (root-only; includes the database key)"
	prune_backups "$BACKUP_PARENT" "$KEEP_BACKUPS" "$BACKUP"
else
	if id dillabot >/dev/null 2>&1; then die "the dillabot account already exists; inspect it and any Bot files before installing"; fi
	useradd --system --user-group --no-create-home --home-dir /var/lib/dillabot --shell /usr/sbin/nologin dillabot
	mkdir -p "$BIN_DIR" "$LIB_DIR" "$LIBEXEC_DIR" "$ROOT/etc/systemd/system"
	install -d -m 0750 -o root -g dillabot "$CONF_DIR"
	install -d -m 0700 -o dillabot -g dillabot "$DATA_DIR"
	install_assets || die "could not install Bot assets"

	# The CLI requires its temporary secret parent to be owner-private with no
	# symlink ancestors. It never returns the token in its output.
	SECRET_DIR="$WORK/credential"
	mkdir -m 0700 "$SECRET_DIR"
	ADMIN_OUT="$("$DILLAD_BIN" admin bot-runtime create --config="$DILLAD_CONFIG" --out="$SECRET_DIR/runtime.token")" || die "dillad could not create a Bot runtime credential; no service was enabled"
	[ -s "$SECRET_DIR/runtime.token" ] || die "dillad did not write a runtime credential file"
	log "dillad registered the runtime credential: $ADMIN_OUT"
	install -m 0600 -o dillabot -g dillabot "$SECRET_DIR/runtime.token" "$CREDENTIAL.new"
	mv -f "$CREDENTIAL.new" "$CREDENTIAL"
	cat >"$WORK/bot.toml" <<CONFIG
base_url = "$BASE_URL"
instance_id = "$INSTANCE_ID"
runtime_credential_file = "$CREDENTIAL"
helper_executable = "/usr/local/libexec/dilla-bot-core"
wasi_module = "/usr/local/lib/dilla/dilla_core_wasi.wasm"
state_dir = "/var/lib/dillabot"
reconcile_interval = "30s"
CONFIG
	install -m 0640 -o root -g dillabot "$WORK/bot.toml" "$CONF.new"
	mv -f "$CONF.new" "$CONF"
	chown -R dillabot:dillabot "$DATA_DIR"
	chmod 0700 "$DATA_DIR"
	log "checking local Bot files with dillabot doctor"
	run_bot_doctor >&2 || die "dillabot doctor failed; inspect local files before starting the service"
	systemctl daemon-reload
	systemctl enable --now dillabot || die "dillabot service failed to start; inspect systemctl status dillabot and journalctl -u dillabot"
	log "installation complete; use Server Settings > Bot to enable it for a community"
fi
