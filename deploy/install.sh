#!/bin/sh
# Installs dillad on a Debian 13 host, LXC container or VM, as the systemd
# service of docs/deploy/README.md §3, without Docker. Run it as root.
#
# Take this file from a release (releases/download/<tag>/install.sh): the
# release stamps its own tag into DILLA_RELEASE_TAG below, so it installs that
# release by default. A copy straight from git is unstamped and needs
# --version <tag> or --release-url <url>.
#
#   behind a TLS-terminating proxy (Pangolin, Traefik, Caddy, nginx):
#     sh install.sh --behind-proxy --domain chat.example.org \
#       --listen 0.0.0.0:8080 --trusted-proxy 10.0.0.5/32 --public-ip 203.0.113.10
#   reachable from the internet on 443/tcp itself:
#     sh install.sh --public --domain chat.example.org \
#       --public-ip 203.0.113.10 --agree-tos
#   a newer release over an installed instance:
#     sh install.sh --upgrade --version <tag>
#
# stdout carries exactly one thing: the bootstrap invite link, printed once at
# the end of a first install, so `link=$(sh install.sh ...)` captures it and
# nothing else. Every progress line, every command's output and every warning
# goes to stderr. The instance keys and the TURN and LiveKit secrets never
# leave the files dillad init writes them to.
#
# DILLA_INSTALL_ROOT, when set, prefixes every path the script writes or
# checks; deploy/install_test.sh uses it to run the install in a temporary
# directory with stubs for useradd, chown, systemctl and runuser.
set -eu

# The release this copy installs by default. release.yml rewrites this exact
# line to the tag it publishes; leave it empty in git.
DILLA_RELEASE_TAG=""

LINK=""
LINK_PRINTED=0
POST_INIT=0
WORK=""

log() { printf 'install: %s\n' "$*" >&2; }
die() {
	log "error: $*"
	if [ "$POST_INIT" -eq 1 ]; then
		log "the instance is half-installed: init ran, so its invite link is printed below, once."
		log "to start over: systemctl disable --now dilla; rm -rf /var/lib/dillad /etc/dilla/dilla.toml; then run install.sh again"
	fi
	exit 1
}

# On any exit after init, the link is printed once: init consumed it, and the
# database holds only its hash.
# shellcheck disable=SC2317,SC2329 # invoked by the EXIT trap below (SC2317 on older shellcheck)
on_exit() {
	[ -z "$WORK" ] || rm -rf "$WORK"
	if [ -n "$LINK" ] && [ "$LINK_PRINTED" -eq 0 ]; then
		LINK_PRINTED=1
		printf '%s\n' "$LINK"
	fi
}
trap on_exit EXIT

usage() {
	cat >&2 <<'USAGE'
usage: install.sh --behind-proxy --domain NAME --listen ADDR:PORT --trusted-proxy CIDR... (--public-ip IP | --node-ip IP | --no-media-ip) [options]
       install.sh --public --domain NAME --public-ip IP --agree-tos [options]
       install.sh --upgrade [--yes] [--version TAG] [--release-url URL] [--binary FILE --core FILE --sums FILE]

Installs dillad as the systemd service `dilla`: the user dillad, /usr/local/bin/dillad
with dilla_core_wasi.wasm beside it, /var/lib/dillad (0700) and /etc/dilla/dilla.toml
(0640 root:dillad). Prints the bootstrap invite link once, on stdout.

modes (exactly one):
  --behind-proxy         a reverse proxy terminates TLS (tls.mode = "behind_proxy");
                         TURN stays off
    --domain NAME        the public DNS name of the instance (instance.domain)
    --listen ADDR:PORT   the plain HTTP listener the proxy reaches (server.plain_listen)
    --trusted-proxy CIDR the proxy's own address, as narrow as you can (a /32);
                         repeatable (server.trusted_proxy_cidrs)
    --public-ip IP       the address clients send UDP --udp-port to (with Pangolin, the
                         Pangolin server's): instance.public_ip, and livekit.node_ip
                         unless --node-ip
    --no-media-ip        install without either; calls cannot reach the SFU from
                         outside this host until livekit.node_ip is set
  --public               dillad terminates TLS on 443/tcp (tls.mode = "acme_tls_alpn")
    --domain NAME        the public DNS name of the instance (instance.domain)
    --public-ip IP       the address clients reach the instance on (instance.public_ip)
    --agree-tos          accept the ACME CA's subscriber agreement (tls.agreed)
  --upgrade              replace the binary and core, restart API and media services,
                         and run doctor; migrates livekit.mode from in_process to sidecar
                         once, saving dilla.toml.prev; rolls back a failed API restart.
                         Restarting the SFU ends every live call, so it first runs
                         `dillad media --rooms` and refuses, changing nothing, if a room
                         has participants or the check cannot be made
    --yes                upgrade anyway, ending any live calls (with --upgrade only)

options:
  --version TAG          the release to install, e.g. v0.1.0 (default: the tag this copy
                         was released with; a copy from git has none)
  --release-url URL      where the release assets are downloaded from (default:
                         https://github.com/jonasthim/dilla/releases/download/TAG)
  --binary FILE          use this dillad binary instead of downloading one
  --core FILE            use this dilla_core_wasi.wasm instead of downloading one
  --sums FILE            the SHA256SUMS to check local files and downloaded assets against
  --insecure-skip-verify install local files without a --sums check
  --udp-port N           LiveKit's media port (livekit.udp_port, default 7882)
  --node-ip IP           the address LiveKit advertises for media (livekit.node_ip)
  --check-args           validate the arguments and exit without changing anything;
                         the release itself is not checked (download and checksums come later)
  -h, --help             show this help
USAGE
}

MODE=""
UPGRADE=0
ASSUME_YES=0
CHECK_ARGS=0
DOMAIN=""
PUBLIC_IP=""
AGREE_TOS=0
LISTEN=""
TRUSTED=""
VERSION=""
RELEASE_URL=""
BINARY=""
CORE=""
SUMS=""
INSECURE=0
NO_MEDIA_IP=0
UDP_PORT="7882"
NODE_IP=""
SET_MODE_FLAGS=""

set_mode() {
	if [ -n "$MODE" ] && [ "$MODE" != "$1" ]; then
		die "give only one of --behind-proxy and --public"
	fi
	MODE="$1"
}
# Flags that only a first install reads; --upgrade refuses them.
saw() { SET_MODE_FLAGS="$SET_MODE_FLAGS $1"; }

# Flags take their value as "--flag value" or "--flag=value".
while [ "$#" -gt 0 ]; do
	flag="$1"
	value=""
	case "$flag" in
	--*=*)
		value="${flag#*=}"
		flag="${flag%%=*}"
		shift
		;;
	--behind-proxy | --public | --agree-tos | --upgrade | --yes | --check-args | --insecure-skip-verify | --no-media-ip | -h | --help)
		shift
		;;
	--domain | --public-ip | --listen | --trusted-proxy | --udp-port | --node-ip | --version | --release-url | --binary | --core | --sums)
		[ "$#" -ge 2 ] || die "$flag needs a value"
		value="$2"
		shift 2
		;;
	-*)
		usage
		die "unknown option $flag"
		;;
	*)
		usage
		die "unexpected argument $flag"
		;;
	esac
	case "$flag" in
	--behind-proxy)
		set_mode behind-proxy
		saw "$flag"
		;;
	--public)
		set_mode public
		saw "$flag"
		;;
	--upgrade) UPGRADE=1 ;;
	--yes) ASSUME_YES=1 ;;
	--check-args) CHECK_ARGS=1 ;;
	--insecure-skip-verify) INSECURE=1 ;;
	--no-media-ip)
		NO_MEDIA_IP=1
		saw "$flag"
		;;
	--domain)
		DOMAIN="$value"
		saw "$flag"
		;;
	--public-ip)
		PUBLIC_IP="$value"
		saw "$flag"
		;;
	--agree-tos)
		AGREE_TOS=1
		saw "$flag"
		;;
	--listen)
		LISTEN="$value"
		saw "$flag"
		;;
	--trusted-proxy)
		TRUSTED="$TRUSTED $value"
		saw "$flag"
		;;
	--udp-port)
		UDP_PORT="$value"
		saw "$flag"
		;;
	--node-ip)
		NODE_IP="$value"
		saw "$flag"
		;;
	--version) VERSION="$value" ;;
	--release-url) RELEASE_URL="${value%/}" ;;
	--binary) BINARY="$value" ;;
	--core) CORE="$value" ;;
	--sums) SUMS="$value" ;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage
		die "unknown option $flag"
		;;
	esac
done

# ------------------------------------------------------------ validation ----
# Every value that reaches dilla.toml is checked against what dillad itself
# accepts, and against a strict character set: the file is edited as text, and
# a quote or a newline in a value would be TOML injection.

# A decimal number without leading zeros (TOML refuses 07882), at most $2.
valid_uint() {
	case "$1" in
	'' | *[!0-9]*) return 1 ;;
	0) ;;
	0*) return 1 ;;
	esac
	[ "${#1}" -le 6 ] && [ "$1" -le "$2" ]
}
valid_ipv4() {
	case "$1" in
	*[!0-9.]* | .* | *. | *..*) return 1 ;;
	esac
	old_ifs="$IFS"
	IFS=.
	# shellcheck disable=SC2086 # split on the dots
	set -- $1
	IFS="$old_ifs"
	[ "$#" -eq 4 ] || return 1
	for octet in "$@"; do
		valid_uint "$octet" 255 || return 1
	done
}
valid_ipv6() {
	case "$1" in
	'' | *[!0-9A-Fa-f:.]* | *:::*) return 1 ;;
	*:*:*) ;;
	*) return 1 ;;
	esac
	colons="$(printf '%s' "$1" | tr -cd ':')"
	[ "${#colons}" -le 7 ] || return 1
	case "$1" in
	*::*::*) return 1 ;;
	*.*) valid_ipv4 "${1##*:}" ;;
	esac
}
valid_ip() { valid_ipv4 "$1" || valid_ipv6 "$1"; }
valid_port() { valid_uint "$1" 65535 && [ "$1" -ge 1 ]; }
valid_cidr() {
	case "$1" in
	*/*/* | */) return 1 ;;
	*/*) ;;
	*) return 1 ;;
	esac
	if valid_ipv4 "${1%/*}"; then
		valid_uint "${1#*/}" 32
	else
		valid_ipv6 "${1%/*}" && valid_uint "${1#*/}" 128
	fi
}
valid_listen() {
	case "$1" in
	'' | *[!0-9A-Za-z.:\[\]-]*) return 1 ;;
	*:*) ;;
	*) return 1 ;;
	esac
	valid_port "${1##*:}"
}
valid_domain() {
	case "$1" in
	'' | *[!A-Za-z0-9.-]* | .* | *. | *..*) return 1 ;;
	*.*) return 0 ;;
	*) return 1 ;;
	esac
}

if [ "$UPGRADE" -eq 1 ]; then
	[ -z "$SET_MODE_FLAGS" ] || die "--upgrade keeps the installed network settings; drop$SET_MODE_FLAGS"
else
	[ "$ASSUME_YES" -eq 0 ] || die "--yes is for --upgrade"
	[ -n "$MODE" ] || {
		usage
		die "choose a mode: --behind-proxy or --public (or --upgrade for an installed instance)"
	}
	valid_domain "$DOMAIN" || die "--domain must be a DNS name such as chat.example.org, got \"$DOMAIN\""
	valid_port "$UDP_PORT" || die "--udp-port must be a port number 1-65535 without leading zeros, got \"$UDP_PORT\""
	[ -z "$NODE_IP" ] || valid_ip "$NODE_IP" || die "--node-ip must be an IPv4 or IPv6 address, got \"$NODE_IP\""
	[ -z "$PUBLIC_IP" ] || valid_ip "$PUBLIC_IP" || die "--public-ip must be an IPv4 or IPv6 address, got \"$PUBLIC_IP\""
	if [ "$MODE" = behind-proxy ]; then
		[ -n "$LISTEN" ] || die "--behind-proxy needs --listen ADDR:PORT, the plain HTTP listener the proxy reaches"
		valid_listen "$LISTEN" || die "--listen must be ADDR:PORT such as 0.0.0.0:8080, got \"$LISTEN\""
		[ -n "$TRUSTED" ] || die "--behind-proxy needs at least one --trusted-proxy CIDR: dillad trusts X-Forwarded-For only from it"
		for cidr in $TRUSTED; do
			valid_cidr "$cidr" || die "--trusted-proxy must be a CIDR such as 10.0.0.5/32, got \"$cidr\""
		done
		[ "$AGREE_TOS" -eq 0 ] || die "--agree-tos is for --public: behind a proxy dillad requests no certificate"
		if [ -z "$PUBLIC_IP" ] && [ -z "$NODE_IP" ] && [ "$NO_MEDIA_IP" -eq 0 ]; then
			die "--behind-proxy needs --public-ip or --node-ip, the address clients send UDP $UDP_PORT to (with Pangolin, the Pangolin server's); without one, calls cannot reach the SFU from outside this host. Pass --no-media-ip to install without it anyway"
		fi
	else
		[ -z "$LISTEN" ] || die "--listen is for --behind-proxy; --public serves on 443"
		[ -z "$TRUSTED" ] || die "--trusted-proxy is for --behind-proxy; a direct listener must not honour X-Forwarded-For"
		[ "$NO_MEDIA_IP" -eq 0 ] || die "--no-media-ip is for --behind-proxy; --public needs --public-ip"
		[ -n "$PUBLIC_IP" ] || die "--public needs --public-ip, the address clients reach the instance on"
		[ "$AGREE_TOS" -eq 1 ] || die "--public needs --agree-tos: tls.agreed records your acceptance of the ACME CA's subscriber agreement, and this script has no standing to accept it for you"
	fi
fi

# Where the binary and the core come from, and how they are checked. A file
# that is downloaded is always checked; a local file is checked against
# --sums, or installed unchecked only with --insecure-skip-verify.
DOWNLOAD=0
[ -n "$BINARY" ] || DOWNLOAD=1
[ -n "$CORE" ] || DOWNLOAD=1
if [ -n "$BINARY$CORE" ] && [ -z "$SUMS" ] && [ "$INSECURE" -eq 0 ]; then
	die "local binaries are checked against a SHA256SUMS: pass --sums FILE, or --insecure-skip-verify to install them unchecked"
fi

[ -n "$VERSION" ] || VERSION="$DILLA_RELEASE_TAG"
if [ -n "$VERSION" ]; then
	case "$VERSION" in
	v[0-9]*) ;;
	*) die "--version must be a release tag such as v0.1.0, got \"$VERSION\"" ;;
	esac
	case "$VERSION" in
	*[!A-Za-z0-9.+_-]*) die "--version must be a release tag such as v0.1.0, got \"$VERSION\"" ;;
	esac
fi
if [ -z "$RELEASE_URL" ] && [ "$DOWNLOAD" -eq 1 ]; then
	[ -n "$VERSION" ] || die "this install.sh is not from a release, so it has no default version: pass --version <tag> (see https://github.com/jonasthim/dilla/releases) or --release-url <url>"
	RELEASE_URL="https://github.com/jonasthim/dilla/releases/download/$VERSION"
fi

if [ "$CHECK_ARGS" -eq 1 ]; then
	# Says what it checked (on stderr: the Proxmox helper reads stdout as the invite link), so an rc of 0 is not read as "this release exists and is good to install".
	echo "arguments are valid; the release was not checked" >&2
	exit 0
fi

# ------------------------------------------------------------------ host ----
[ "$(id -u)" = 0 ] || die "run this as root (inside the container or VM that will host dilla)"

ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*) die "unsupported architecture $ARCH_RAW: dillad is built for amd64 and arm64" ;;
esac

ROOT="${DILLA_INSTALL_ROOT:-}"
ROOT="${ROOT%/}"
BIN_DIR="$ROOT/usr/local/bin"
UNIT_PATH="$ROOT/etc/systemd/system/dilla.service"
MEDIA_UNIT_PATH="$ROOT/etc/systemd/system/dilla-media.service"
CONF_DIR="$ROOT/etc/dilla"
CONF="$CONF_DIR/dilla.toml"
DATA_DIR="$ROOT/var/lib/dillad"
BIN_NAME="dillad-linux-$ARCH"
CORE_NAME="dilla_core_wasi.wasm"

INSTALLED=0
if [ -e "$CONF" ]; then
	INSTALLED=1
elif [ -d "$DATA_DIR" ] && [ -n "$(ls -A "$DATA_DIR")" ]; then
	INSTALLED=1
fi
if [ "$UPGRADE" -eq 1 ]; then
	[ "$INSTALLED" -eq 1 ] || die "--upgrade found no instance: neither $CONF nor a non-empty $DATA_DIR exists; install first"
	[ -e "$CONF" ] || die "--upgrade found $DATA_DIR but no $CONF; restore the config before upgrading"
elif [ "$INSTALLED" -eq 1 ]; then
	die "an instance is already installed here ($CONF or $DATA_DIR exists); run with --upgrade to replace the binary and the core, which never touches the config or the data"
fi

WORK="$(mktemp -d)"

# ------------------------------------------------------------- artefacts ----
have() { command -v "$1" >/dev/null 2>&1; }

# Only what is missing, and only when something has to be downloaded.
ensure_download_tools() {
	missing=""
	have curl || have wget || missing="curl"
	[ -s "$ROOT/etc/ssl/certs/ca-certificates.crt" ] || missing="$missing ca-certificates"
	[ -n "$missing" ] || return 0
	log "installing$missing with apt"
	DEBIAN_FRONTEND=noninteractive apt-get update >&2
	# shellcheck disable=SC2086 # one word per package
	DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends $missing >&2
}

fetch() { # URL OUT
	if have curl; then
		curl -fsSL --proto '=https,file' --proto-redir '=https' -o "$2" "$1" >&2 || die "download of $1 failed"
	else
		case "$1" in
		https://*) ;;
		*) die "download of $1 failed: wget fetches https:// only" ;;
		esac
		wget -q --https-only -O "$2" "$1" >&2 || die "download of $1 failed"
	fi
}

if [ "$DOWNLOAD" -eq 1 ]; then
	ensure_download_tools
fi

# CHECK holds the names that must match SHA256SUMS; UNCHECKED the local files
# that --insecure-skip-verify lets through.
CHECK=""
UNCHECKED=""
if [ -n "$BINARY" ]; then
	[ -s "$BINARY" ] || die "the dillad binary $BINARY is missing or empty"
	cp "$BINARY" "$WORK/$BIN_NAME"
	if [ -n "$SUMS" ]; then CHECK="$CHECK $BIN_NAME"; else UNCHECKED="$UNCHECKED $BIN_NAME"; fi
else
	log "downloading $BIN_NAME from $RELEASE_URL"
	fetch "$RELEASE_URL/$BIN_NAME" "$WORK/$BIN_NAME"
	CHECK="$CHECK $BIN_NAME"
fi
if [ -n "$CORE" ]; then
	[ -s "$CORE" ] || die "the wasi core $CORE is missing or empty"
	cp "$CORE" "$WORK/$CORE_NAME"
	if [ -n "$SUMS" ]; then CHECK="$CHECK $CORE_NAME"; else UNCHECKED="$UNCHECKED $CORE_NAME"; fi
else
	log "downloading $CORE_NAME from $RELEASE_URL"
	fetch "$RELEASE_URL/$CORE_NAME" "$WORK/$CORE_NAME"
	CHECK="$CHECK $CORE_NAME"
fi

if [ -n "$CHECK" ]; then
	if [ -n "$SUMS" ]; then
		[ -f "$SUMS" ] || die "--sums $SUMS does not exist"
		cp "$SUMS" "$WORK/SHA256SUMS"
	else
		log "downloading SHA256SUMS from $RELEASE_URL"
		fetch "$RELEASE_URL/SHA256SUMS" "$WORK/SHA256SUMS"
	fi
	# An empty or short SHA256SUMS refuses: verification is decided by where the
	# files came from, never by what the SHA256SUMS file happens to hold.
	[ -s "$WORK/SHA256SUMS" ] || die "SHA256SUMS is empty; refusing to install unverified files"
	# Exactly one line per asset, by exact name, so a SHA256SUMS that leaves one
	# out cannot pass for both.
	: >"$WORK/check.sums"
	for name in $CHECK; do
		awk -v n="$name" '($2 == n || $2 == "*" n) && $1 ~ /^[0-9a-f]+$/ && length($1) == 64' "$WORK/SHA256SUMS" >"$WORK/line"
		[ "$(wc -l <"$WORK/line")" -eq 1 ] || die "SHA256SUMS does not hold exactly one SHA-256 line for $name; refusing to install unverified files"
		cat "$WORK/line" >>"$WORK/check.sums"
	done
	if ! (cd "$WORK" && sha256sum -c --strict --quiet check.sums) >&2; then
		die "checksum mismatch: the files do not match SHA256SUMS; nothing was installed"
	fi
	log "verified$CHECK against SHA256SUMS"
fi
if [ -n "$UNCHECKED" ]; then
	log "warning: --insecure-skip-verify: installing$UNCHECKED unverified"
fi

# Installs a file next to its destination and renames it over, so a running
# dillad keeps its open inode and an interrupted copy never leaves half a file.
put() { # SRC DEST MODE
	install -m "$3" "$1" "$2.new"
	mv -f "$2.new" "$2"
}

write_media_unit() {
	cat >"$WORK/dilla-media.service" <<'UNIT'
[Unit]
Description=dilla media SFU
Documentation=https://github.com/jonasthim/dilla/blob/main/docs/deploy/README.md
After=network-online.target
Wants=network-online.target
PartOf=dilla.service

[Service]
Type=simple
ExecStart=/usr/local/bin/dillad media --config=%E/dilla/dilla.toml
Restart=on-failure
RestartSec=5s
RestartPreventExitStatus=78
TimeoutStartSec=30s
TimeoutStopSec=25s
User=dillad
Group=dillad
StateDirectory=dillad
StateDirectoryMode=0700
ConfigurationDirectory=dilla
UMask=0077
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectProc=invisible
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictSUIDSGID=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
UNIT
	put "$WORK/dilla-media.service" "$MEDIA_UNIT_PATH" 0644
}

livekit_mode() {
	awk '
		/^[[:space:]]*\[livekit\][[:space:]]*$/ { inside = 1; next }
		inside && /^[[:space:]]*\[/ { exit }
		inside && /^[[:space:]]*mode[[:space:]]*=/ {
			line = $0
			sub(/^[^=]*=[[:space:]]*/, "", line)
			sub(/[[:space:]]*#.*/, "", line)
			gsub(/["\047[:space:]]/, "", line)
			print line
			exit
		}
	' "$CONF"
}

set_livekit_mode() { # MODE
	awk -v mode="$1" '
		/^[[:space:]]*\[livekit\][[:space:]]*$/ { inside = 1 }
		inside && /^[[:space:]]*\[/ && $0 !~ /^[[:space:]]*\[livekit\][[:space:]]*$/ { inside = 0 }
		inside && /^[[:space:]]*mode[[:space:]]*=/ {
			sub(/mode[[:space:]]*=.*/, "mode = \047" mode "\047")
			found = 1
		}
		{ print }
		END { if (!found) exit 3 }
	' "$CONF" >"$WORK/dilla-sidecar.toml" || die "dilla.toml has no livekit.mode; cannot migrate it to a media sidecar"
	cp -p "$CONF" "$CONF.new"
	cat "$WORK/dilla-sidecar.toml" >"$CONF.new"
	mv -f "$CONF.new" "$CONF"
}

run_doctor() {
	DOCTOR_RC=0
	DOCTOR_OUT="$(runuser -u dillad -- "$BIN_DIR/dillad" doctor --config="$CONF" 2>&1)" || DOCTOR_RC=$?
}

doctor_and_report() {
	log "running dillad doctor"
	run_doctor
	printf '%s\n' "$DOCTOR_OUT" | sed 's/^/  /' >&2
	# Legs that need DNS for the domain, outbound HTTPS or the proxy: red until
	# those exist, which is expected on a fresh install. The udp leg is never red:
	# it reads WARN "not probed" by default and cannot prove the forward.
	PENDING="$(printf '%s\n' "$DOCTOR_OUT" | awk '$1 == "FAIL" && ($2 == "clock" || $2 == "certificate" || $2 == "turn") { printf "%s ", $2 }')"
	BROKEN="$(printf '%s\n' "$DOCTOR_OUT" | awk '$1 == "FAIL" && !($2 == "clock" || $2 == "certificate" || $2 == "turn") { printf "%s ", $2 }')"
	if [ -n "$PENDING" ]; then
		log "doctor: these checks need DNS, the network or the proxy set up first, and fail until DNS and the proxy are in place: $PENDING"
		log "        run the doctor again once they are: runuser -u dillad -- /usr/local/bin/dillad doctor --config=/etc/dilla/dilla.toml"
	fi
	if [ -n "$BROKEN" ]; then
		log "error: doctor found a problem with the install itself: $BROKEN"
		return 1
	fi
	if [ "$DOCTOR_RC" -ne 0 ] && [ -z "$PENDING" ]; then
		log "error: dillad doctor exited $DOCTOR_RC"
		return 1
	fi
	return 0
}

# schema_version BINARY: the database's schema version, as BINARY's `migrate status` reads it.
schema_version() {
	sv_out="$(runuser -u dillad -- "$1" migrate status --config="$CONF")" || return 1
	sv="$(printf '%s\n' "$sv_out" | awk '$1 == "current" { sub(/,$/, "", $2); print $2; exit }')"
	case "$sv" in '' | *[!0-9]*) return 1 ;; esac
	printf '%s\n' "$sv"
}

# rollback_stuck REASON: the rollback cannot prove the database is at a schema the previous binary
# reads, so the previous binary is NOT started (it refuses a newer schema, so the instance would
# stay down either way). The new binary stays in place, and the operator is told the ways out.
rollback_stuck() {
	log "error: dilla did not restart with the new binary, and the rollback stopped: $1"
	if [ "$STOPPED" -eq 1 ]; then
		log "dilla is stopped. The new binary is still $BIN_DIR/dillad; the previous one is $BIN_DIR/dillad.prev."
	else
		log "dilla could not be stopped, so systemd may still be restarting the new binary ($BIN_DIR/dillad); the previous one is $BIN_DIR/dillad.prev."
	fi
	if [ -z "${SCHEMA_AFTER:-}" ]; then
		log "First read the database's schema version with the previous binary:"
		log "    runuser -u dillad -- $BIN_DIR/dillad.prev migrate status --config=$CONF"
		log "If it prints \"current $SCHEMA_BEFORE\" (or lower), nothing was migrated: put the previous pair back and start it:"
		log "    systemctl stop dilla; cp -p $BIN_DIR/dillad.prev $BIN_DIR/dillad; cp -p $BIN_DIR/$CORE_NAME.prev $BIN_DIR/$CORE_NAME; systemctl start dilla"
		log "If it prints a higher current version, the new binary migrated it; then either:"
	else
		log "The database is at schema $SCHEMA_AFTER and the previous binary refuses anything above $SCHEMA_BEFORE, so do not put it back as it is. Either:"
	fi
	log "  1. fix what stopped the new release (journalctl -o cat -u dilla) and run: systemctl start dilla"
	if [ "${LIVEKIT_MODE_BEFORE:-}" = in_process ]; then
		log "     If returning to the previous binary, first stop dilla-media and restore $CONF.prev to $CONF."
	fi
	log "  2. or stop dilla, take the database back with the NEW binary, then put the previous pair back:"
	log "       runuser -u dillad -- $BIN_DIR/dillad migrate down --to=$SCHEMA_BEFORE --i-know-what-i-am-doing --config=$CONF"
	log "       cp -p $BIN_DIR/dillad.prev $BIN_DIR/dillad; cp -p $BIN_DIR/$CORE_NAME.prev $BIN_DIR/$CORE_NAME; systemctl start dilla"
	log "  3. or, on SQLite, restore the copy the new binary took before it migrated, <db.path>.pre-migration-<unix time>"
	log "     beside the database (it loses everything written since), then put the previous pair back as in 2"
	exit 1
}

# live_calls_preflight refuses (exit 1, nothing touched) while a room has participants or when the
# check cannot be made, unless --yes. It runs at the moment the upgrade is about to stop things:
# an answer from earlier does not count. `media --rooms` prints "no live rooms", or per room
# "<room><TAB><n> participants" followed by the participants' identities indented by two spaces.
live_calls_preflight() {
	_rooms=""
	if [ "$LIVEKIT_MODE_BEFORE" != sidecar ]; then
		_why="livekit.mode is $LIVEKIT_MODE_BEFORE, so the SFU runs inside dilla and its rooms cannot be listed; this upgrade restarts it and ends every live call"
	elif ! systemctl is-active --quiet dilla-media; then
		# A stopped SFU holds no rooms (a rollback stops dilla first, and dilla-media with it through PartOf).
		log "dilla-media is not running, so there are no live calls"
		return 0
	elif _rooms="$(runuser -u dillad -- "$BIN_DIR/dillad" media --rooms --config="$CONF")"; then
		_busy="$(printf '%s\n' "$_rooms" | awk -F'\t' '$2 ~ /^[1-9][0-9]* participants$/ { print "  " $1 ": " $2 }')"
		if [ -z "$_busy" ]; then
			case "$_rooms" in
			"no live rooms" | *participants*) return 0 ;;
			esac
			_why="dillad media --rooms printed something unexpected: $_rooms"
		else
			_why="there are live calls, and restarting the SFU disconnects everyone in them"
			log "live rooms:"
			printf '%s\n' "$_busy" | while IFS= read -r _line; do log "$_line"; done
		fi
	else
		_why="could not list the live rooms (runuser -u dillad -- $BIN_DIR/dillad media --rooms --config=$CONF failed)"
	fi
	if [ "$ASSUME_YES" -eq 1 ]; then
		log "warning: $_why; continuing because of --yes"
		return 0
	fi
	die "$_why. Nothing was changed. Try again when the rooms are empty, or pass --yes to upgrade anyway"
}

# ---------------------------------------------------------------- upgrade ----
if [ "$UPGRADE" -eq 1 ]; then
	log "upgrading $BIN_DIR/dillad and $BIN_DIR/$CORE_NAME"
	LIVEKIT_MODE_BEFORE="$(livekit_mode)"
	case "$LIVEKIT_MODE_BEFORE" in
	in_process | sidecar) ;;
	*) die "livekit.mode is $LIVEKIT_MODE_BEFORE; the upgrade supports in_process or sidecar" ;;
	esac
	# The schema the previous binary runs on. The new binary migrates the database when it
	# starts, and a dillad refuses a database newer than its own migrations, so a rollback must
	# first take the database back to this version, with the new binary (the only one that knows
	# the way down).
	SCHEMA_BEFORE="$(schema_version "$BIN_DIR/dillad")" ||
		die "could not read the database's schema version with the installed binary (runuser -u dillad -- $BIN_DIR/dillad migrate status --config=$CONF); nothing was changed"
	# Last check before anything is written or stopped.
	live_calls_preflight
	# The previous pair stays beside the new one, for the rollback below and by hand.
	cp -p "$BIN_DIR/dillad" "$BIN_DIR/dillad.prev"
	cp -p "$BIN_DIR/$CORE_NAME" "$BIN_DIR/$CORE_NAME.prev"
	cp -p "$CONF" "$CONF.prev"
	put "$WORK/$BIN_NAME" "$BIN_DIR/dillad" 0755
	put "$WORK/$CORE_NAME" "$BIN_DIR/$CORE_NAME" 0644
	write_media_unit
	systemctl daemon-reload >&2 || die "systemd could not load dilla-media.service"
	if [ "$LIVEKIT_MODE_BEFORE" = in_process ]; then
		log "moving the SFU into dilla-media.service"
		systemctl stop dilla >&2 || die "could not stop the previous in-process SFU"
		set_livekit_mode sidecar
	fi
	if ! systemctl enable --now dilla-media >&2 || ! runuser -u dillad -- "$BIN_DIR/dillad" media --probe --config="$CONF" >&2; then
		log "error: dilla-media did not start; restoring the previous binary and config"
		[ "$LIVEKIT_MODE_BEFORE" = sidecar ] || systemctl disable --now dilla-media >&2 || :
		[ "$LIVEKIT_MODE_BEFORE" = sidecar ] || cp -p "$CONF.prev" "$CONF"
		put "$BIN_DIR/dillad.prev" "$BIN_DIR/dillad" 0755
		put "$BIN_DIR/$CORE_NAME.prev" "$BIN_DIR/$CORE_NAME" 0644
		[ "$LIVEKIT_MODE_BEFORE" = sidecar ] || systemctl start dilla >&2 || :
		die "the media service did not start; read journalctl -o cat -u dilla-media"
	fi
	log "restarting dilla"
	if ! systemctl restart dilla >&2; then
		log "error: dilla did not restart with the new binary"
		# Stopped first: systemd restarts a failed dilla on its own (Restart=on-failure), and each
		# start of the new binary would migrate the database up again.
		STOPPED=1
		systemctl stop dilla >&2 || STOPPED=0
		# Read with the PREVIOUS binary: it loaded this dilla.toml a minute ago, and its `migrate
		# status` reports a newer schema without refusing it. The new binary may refuse the config
		# (a release that validates it more strictly), in which case it never migrated anything.
		SCHEMA_AFTER=""
		SCHEMA_AFTER="$(schema_version "$BIN_DIR/dillad.prev")" || {
			SCHEMA_AFTER=""
			rollback_stuck "could not read the database's schema version with the previous binary"
		}
		if [ "$SCHEMA_AFTER" -gt "$SCHEMA_BEFORE" ]; then
			# The new binary got past its config load and migrated, so it can take the database down.
			[ "$STOPPED" -eq 1 ] || rollback_stuck "dilla could not be stopped, so the database cannot be migrated down safely"
			log "the new binary migrated the database from schema $SCHEMA_BEFORE to $SCHEMA_AFTER; taking it back to $SCHEMA_BEFORE"
			runuser -u dillad -- "$BIN_DIR/dillad" migrate down --to="$SCHEMA_BEFORE" --i-know-what-i-am-doing --config="$CONF" >&2 ||
				rollback_stuck "migrate down --to=$SCHEMA_BEFORE failed"
		fi
		log "putting the previous binary and core back"
		if [ "$LIVEKIT_MODE_BEFORE" = in_process ]; then
			systemctl disable --now dilla-media >&2 || :
			cp -p "$CONF.prev" "$CONF"
		fi
		put "$BIN_DIR/dillad.prev" "$BIN_DIR/dillad" 0755
		put "$BIN_DIR/$CORE_NAME.prev" "$BIN_DIR/$CORE_NAME" 0644
		if systemctl restart dilla >&2; then
			die "the upgrade failed and was rolled back; the previous release is running. Read why with: journalctl -o cat -u dilla"
		fi
		die "the upgrade failed and was rolled back, but dilla did not start with the previous binary either; read its log with: journalctl -o cat -u dilla"
	fi
	doctor_and_report || exit 1
	log "restarting the sidecar SFU on the new binary; this disconnects everyone in a call, and not every client rejoins"
	if ! systemctl restart dilla-media >&2 || ! runuser -u dillad -- "$BIN_DIR/dillad" media --probe --config="$CONF" >&2; then
		die "the API upgraded, but dilla-media did not become ready; read journalctl -o cat -u dilla-media"
	fi
	log "upgrade done; the previous binary, core and config are kept as $BIN_DIR/dillad.prev, $CORE_NAME.prev and $CONF.prev"
	log "dilla.service was not rewritten; compare it with the release's deploy/dilla.service"
	exit 0
fi

# ---------------------------------------------------------------- install ----
log "installing dillad ($ARCH) per docs/deploy §3"
mkdir -p "$BIN_DIR" "$ROOT/etc/systemd/system"
put "$WORK/$BIN_NAME" "$BIN_DIR/dillad" 0755
# Beside the binary: dillad looks for the wasi core in its own executable's directory.
put "$WORK/$CORE_NAME" "$BIN_DIR/$CORE_NAME" 0644

# deploy/dilla.service, verbatim (deploy/install_test.sh compares the two).
cat >"$WORK/dilla.service" <<'UNIT'
[Unit]
Description=dilla server (dillad)
Documentation=https://github.com/jonasthim/dilla/blob/main/docs/deploy/README.md
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

User=dillad
Group=dillad
StateDirectory=dillad
# The default is 0755; this directory holds instances.key_history, the franking
# key and the database.
StateDirectoryMode=0700
ConfigurationDirectory=dilla
UMask=0077

# How a non-root process binds 443/tcp.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectProc=invisible
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
# AF_NETLINK is required, not optional: Go's net.Interfaces reads the interface
# addresses over netlink, and LiveKit's SFU setup and pion/turn's network both
# call it. Without it serve fails at start with "netlinkrib: address family not
# supported by protocol".
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictSUIDSGID=yes
LockPersonality=yes
# MemoryDenyWriteExecute is deliberately NOT set: wazero's compiler back end
# writes and then executes generated machine code, so setting it would make the
# MLS path fail at runtime rather than at start. A blind copy of a hardening
# template is exactly how that regression arrives.

[Install]
WantedBy=multi-user.target
UNIT
put "$WORK/dilla.service" "$UNIT_PATH" 0644

# deploy/dilla-media.service, verbatim.
write_media_unit

if getent passwd dillad >/dev/null 2>&1; then
	log "the dillad user exists"
else
	useradd --system --user-group --no-create-home --home-dir /var/lib/dillad --shell /usr/sbin/nologin dillad >&2
fi

# init needs --public-ip and --agree-tos whatever the mode. Behind a proxy
# without an address it gets loopback, and both values are corrected below.
INIT_IP="$PUBLIC_IP"
[ -n "$INIT_IP" ] || INIT_IP="127.0.0.1"
log "initialising the instance"
# init writes the config straight to $CONF, so its directory exists first.
install -d -m 0755 "$CONF_DIR"
INIT_OUT="$("$BIN_DIR/dillad" init --config="$CONF" --data-dir="$DATA_DIR" --domain="$DOMAIN" --public-ip="$INIT_IP" --agree-tos)" ||
	die "dillad init failed"
LINK="$(printf '%s\n' "$INIT_OUT" | sed -n 's|^bootstrap invite.*: \(https://[^ ]*\)$|\1|p' | head -n 1)"
# Everything init printed except the link itself, which has one destination.
printf '%s\n' "$INIT_OUT" | grep -v '^bootstrap invite' >&2 || :
[ -n "$LINK" ] || die "dillad init did not print a bootstrap invite link"
POST_INIT=1

# toml_set TABLE KEY VALUE: replaces the value of KEY in [TABLE] of the file
# init wrote. VALUE is TOML text; its characters were validated above.
toml_set() {
	awk -v t="[$1]" -v k="$2" -v v="$3" '
		{ line = $0; sub(/^[ \t]+/, "", line) }
		line ~ /^\[/ { inside = (line == t) }
		inside && index(line, k " = ") == 1 {
			indent = substr($0, 1, length($0) - length(line))
			print indent k " = " v
			found = 1
			next
		}
		{ print }
		END { if (!found) exit 3 }
	' "$CONF" >"$WORK/dilla.toml" || die "dilla.toml has no $2 in [$1]; this dillad's init writes a layout this install.sh does not know (use the install.sh of the same release)"
	cat "$WORK/dilla.toml" >"$CONF"
}

toml_set livekit udp_port "$UDP_PORT"
toml_set livekit mode "'sidecar'"
[ -z "$NODE_IP" ] || toml_set livekit node_ip "'$NODE_IP'"
if [ "$MODE" = behind-proxy ]; then
	log "configuring tls.mode = behind_proxy on $LISTEN"
	toml_set tls mode "'behind_proxy'"
	# The operator accepted no CA agreement, and none is needed behind a proxy.
	toml_set tls agreed false
	toml_set server plain_listen "'$LISTEN'"
	cidrs=""
	for cidr in $TRUSTED; do
		cidrs="${cidrs:+$cidrs, }'$cidr'"
	done
	toml_set server trusted_proxy_cidrs "[$cidrs]"
	# init writes turn.enabled = true; behind a proxy the relay would need a
	# turn.listen port of its own (docs/deploy §1), so it stays off.
	toml_set turn enabled false
	if [ -z "$PUBLIC_IP" ]; then
		toml_set instance public_ip "''"
		[ -n "$NODE_IP" ] || toml_set livekit node_ip "''"
	fi
fi

log "setting the owners and modes of $CONF and $DATA_DIR"
chown root:dillad "$CONF"
chmod 0640 "$CONF"
chown -R dillad:dillad "$DATA_DIR"
chmod 0700 "$DATA_DIR"

# The edited file passes dillad's own config check (strict decoding plus
# Validate) before the unit is enabled, so a bad value never reaches a start loop.
log "checking dilla.toml"
run_doctor
if ! printf '%s\n' "$DOCTOR_OUT" | grep -q '^OK  *config '; then
	printf '%s\n' "$DOCTOR_OUT" | grep '^FAIL  *config ' >&2 || printf '%s\n' "$DOCTOR_OUT" | sed 's/^/  /' >&2
	die "dillad refuses the edited /etc/dilla/dilla.toml; the service was not started"
fi

log "starting the service"
STATUS=0
if systemctl daemon-reload >&2 && systemctl enable --now dilla-media >&2 &&
	runuser -u dillad -- "$BIN_DIR/dillad" media --probe --config="$CONF" >&2 &&
	systemctl enable --now dilla >&2; then
	doctor_and_report || STATUS=1
else
	STATUS=1
	log "error: the service did not start; read its log with: journalctl -o cat -u dilla"
fi

# ------------------------------------------------------------- next steps ----
if [ "$MODE" = behind-proxy ]; then
	port="${LISTEN##*:}"
	log ""
	log "next steps in Pangolin (or any TLS-terminating proxy):"
	log "  1. an HTTPS resource for $DOMAIN whose target is http://<this host's address>:$port;"
	log "     Pangolin SSO (resource authentication) OFF for it, dilla signs its users in itself;"
	log "     WebSocket upgrades must pass through (Traefik passes them by default; add no"
	log "     middleware that strips Upgrade or Connection)."
	log "  2. the raw UDP forward for media, $UDP_PORT/udp to this host: a Traefik UDP entry point"
	log "     udp-$UDP_PORT on \":$UDP_PORT/udp\" with udp.timeout 30s, $UDP_PORT:$UDP_PORT/udp published on"
	log "     the gerbil service, and a raw UDP resource with exactly one target, this host's $UDP_PORT/udp."
	log "     The tunnel MTU stays 1280."
	log "     See docs/deploy/README.md, \"Behind a WireGuard tunnel (Pangolin)\"."
	if [ -z "$NODE_IP" ] && [ -z "$PUBLIC_IP" ]; then
		log "  3. livekit.node_ip is empty: set it in /etc/dilla/dilla.toml to the public address clients"
		log "     send $UDP_PORT/udp to (the Pangolin server's), then systemctl restart dilla. Until"
		log "     then calls cannot reach the SFU from outside this host."
	fi
	log "  Only the proxy's own address belongs in --trusted-proxy: any host in that range can set"
	log "  X-Forwarded-For and pick the address dillad rate-limits it by."
	log "  TURN is off (tls.mode = behind_proxy): a member behind a UDP-blocking network cannot join calls."
else
	log ""
	log "next steps:"
	log "  1. DNS: $DOMAIN resolving to $PUBLIC_IP."
	log "  2. forward 443/tcp and $UDP_PORT/udp to this host; nothing else needs to be open."
fi
log "  then: runuser -u dillad -- /usr/local/bin/dillad doctor --config=/etc/dilla/dilla.toml"
log ""
log "the bootstrap invite link is the line on stdout; it is valid for 24 hours, works once and is not stored anywhere."

# The link is printed once, even when the service failed to start: the invite
# stays valid for 24 hours and cannot be shown again.
LINK_PRINTED=1
printf '%s\n' "$LINK"
exit "$STATUS"
