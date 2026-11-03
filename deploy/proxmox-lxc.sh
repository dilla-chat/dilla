#!/usr/bin/env bash
# Creates a new unprivileged Debian 13 LXC container on a Proxmox VE host and
# installs dillad in it with install.sh (deploy/install.sh), which does the
# systemd install of docs/deploy/README.md §3 inside the container.
#
# stdout carries exactly one thing: the bootstrap invite link, printed once, so
# `link=$(./proxmox-lxc.sh ...)` captures it and nothing else. Every progress
# line, every command's output and every warning goes to stderr.
set -euo pipefail

log() { printf 'proxmox-lxc: %s\n' "$*" >&2; }
die() {
	log "error: $*"
	exit 1
}

usage() {
	cat >&2 <<'USAGE'
usage: proxmox-lxc.sh --domain NAME --public-ip IP --agree-tos [options]
       proxmox-lxc.sh --behind-proxy --domain NAME --listen ADDR:PORT --trusted-proxy CIDR... [options]

Run it as root on the Proxmox host, from a directory that also holds install.sh.
Without --behind-proxy the instance terminates TLS itself (install.sh --public).

required (default mode):
  --domain NAME        the public DNS name of the instance (instance.domain)
  --public-ip IP       the address clients reach the instance on, IPv4 or IPv6
  --agree-tos          accept the ACME CA's subscriber agreement (tls.agreed)

behind a TLS-terminating proxy:
  --behind-proxy       install.sh --behind-proxy: tls.mode = "behind_proxy", TURN off
  --listen ADDR:PORT   the plain HTTP listener the proxy reaches
  --trusted-proxy CIDR the proxy's own address (a /32); repeatable
  --public-ip IP       the address clients send UDP 7882 to (or --node-ip IP,
                       or --no-media-ip to install without one)

container options:
  --ctid N             container id (default: the next free id)
  --hostname NAME      container hostname (default: dilla)
  --bridge NAME        network bridge (default: vmbr0)
  --storage NAME       rootfs storage (default: local-lvm)
  --template-storage N storage that holds the Debian template (default: local)

passed to install.sh:
  --version TAG        the release to install (default: the tag stamped into the
                       install.sh beside this script; a copy from git has none)
  --release-url URL    where dillad-linux-<arch>, dilla_core_wasi.wasm and
                       SHA256SUMS are downloaded from (default: releases/download/TAG)
  --binary PATH        use this dillad binary instead of downloading one
  --core PATH          use this dilla_core_wasi.wasm instead of downloading one
  --sums PATH          check local files against this SHA256SUMS
  --insecure-skip-verify  install local files without a --sums check
  --udp-port N         LiveKit's media port (default 7882)
  --node-ip IP         the address LiveKit advertises for media
USAGE
}

CTID=""
CT_HOSTNAME="dilla"
BRIDGE="vmbr0"
STORAGE="local-lvm"
TEMPLATE_STORAGE="local"
BINARY=""
CORE=""
SUMS=""
RELEASE_URL=""
BEHIND_PROXY=0
# The arguments install.sh gets, apart from the mode and the local files.
INSTALL_ARGS=()

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
	--agree-tos | --behind-proxy | --insecure-skip-verify | --no-media-ip | -h | --help)
		shift
		;;
	--*)
		[ "$#" -ge 2 ] || die "$flag needs a value"
		value="$2"
		shift 2
		;;
	*)
		usage
		die "unexpected argument $flag"
		;;
	esac
	case "$flag" in
	--ctid) CTID="$value" ;;
	--hostname) CT_HOSTNAME="$value" ;;
	--bridge) BRIDGE="$value" ;;
	--storage) STORAGE="$value" ;;
	--template-storage) TEMPLATE_STORAGE="$value" ;;
	--binary) BINARY="$value" ;;
	--core) CORE="$value" ;;
	--sums) SUMS="$value" ;;
	--behind-proxy) BEHIND_PROXY=1 ;;
	--agree-tos | --insecure-skip-verify | --no-media-ip) INSTALL_ARGS+=("$flag") ;;
	--release-url)
		RELEASE_URL="${value%/}"
		INSTALL_ARGS+=("--release-url=$RELEASE_URL")
		;;
	--domain | --public-ip | --listen | --trusted-proxy | --udp-port | --node-ip | --version)
		INSTALL_ARGS+=("$flag=$value")
		;;
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
if [ "$BEHIND_PROXY" -eq 1 ]; then
	MODE_ARG="--behind-proxy"
else
	MODE_ARG="--public"
fi

command -v pct >/dev/null 2>&1 || die "run this on a Proxmox host as root"
[ "$(id -u)" -eq 0 ] || die "run this on a Proxmox host as root"

case "${BASH_SOURCE[0]}" in
*/*) SCRIPT_DIR="${BASH_SOURCE[0]%/*}" ;;
*) SCRIPT_DIR="." ;;
esac
SCRIPT_DIR="$(cd "$SCRIPT_DIR" && pwd)"
INSTALLER="$SCRIPT_DIR/install.sh"
[ -f "$INSTALLER" ] || die "install.sh is not next to this script ($INSTALLER)"

# Local files are pushed to these paths in the container.
FILE_ARGS=()
[ -z "$BINARY" ] || FILE_ARGS+=(--binary=/root/dilla-install/dillad)
[ -z "$CORE" ] || FILE_ARGS+=(--core=/root/dilla-install/dilla_core_wasi.wasm)
[ -z "$SUMS" ] || FILE_ARGS+=(--sums=/root/dilla-install/SHA256SUMS)

# install.sh validates the arguments here, before a container exists.
"$BASH" "$INSTALLER" "$MODE_ARG" "${INSTALL_ARGS[@]}" "${FILE_ARGS[@]}" --check-args ||
	die "install.sh refused the arguments above; nothing was created"

# The tag a release stamped into install.sh, unless --version overrides it.
VERSION=""
while IFS= read -r line; do
	case "$line" in
	DILLA_RELEASE_TAG=\"*\")
		VERSION="${line#DILLA_RELEASE_TAG=\"}"
		VERSION="${VERSION%\"}"
		;;
	esac
done <"$INSTALLER"
UDP_PORT=7882
for a in "${INSTALL_ARGS[@]}"; do
	case "$a" in
	--version=*) VERSION="${a#--version=}" ;;
	--udp-port=*) UDP_PORT="${a#--udp-port=}" ;;
	esac
done

if [ -z "$CTID" ]; then
	if command -v pvesh >/dev/null 2>&1; then
		CTID="$(pvesh get /cluster/nextid)"
	else
		die "--ctid is required: pvesh is not available to choose the next free id"
	fi
fi
case "$CTID" in
'' | *[!0-9]*) die "--ctid must be a number, got \"$CTID\"" ;;
esac
if pct status "$CTID" >/dev/null 2>&1; then
	die "container $CTID already exists; choose another --ctid"
fi

for f in "$BINARY" "$CORE" "$SUMS"; do
	[ -z "$f" ] || [ -s "$f" ] || die "$f is missing or empty"
done
# Whatever is not local is downloaded inside the container; a release URL that
# does not answer fails here, before a container exists.
if [ -z "$BINARY" ] || [ -z "$CORE" ]; then
	# install.sh --check-args has refused a download with neither a version nor a URL.
	probe="${RELEASE_URL:-https://github.com/dilla-chat/dilla/releases/download/$VERSION}"
	log "checking $probe/SHA256SUMS"
	curl -fsSL -o /dev/null "$probe/SHA256SUMS" >&2 || die "the release at $probe does not answer; nothing was created"
fi

log "looking for the Debian 13 template"
pveam update >&2 || log "warning: pveam update failed; using the template list already on this host"
TEMPLATE="$(pveam available --section system | awk '$2 ~ /^debian-13-standard/ { print $2 }' | sort -V | tail -n 1)"
[ -n "$TEMPLATE" ] || die "no debian-13-standard template is listed by pveam"
if ! pveam list "$TEMPLATE_STORAGE" | grep -q "$TEMPLATE"; then
	log "downloading $TEMPLATE to $TEMPLATE_STORAGE"
	pveam download "$TEMPLATE_STORAGE" "$TEMPLATE" >&2
fi

# Unprivileged, with nesting: the unit's sandboxing directives (PrivateTmp,
# ProtectSystem, ...) need it to start inside a container.
log "creating container $CTID ($CT_HOSTNAME)"
pct create "$CTID" "$TEMPLATE_STORAGE:vztmpl/$TEMPLATE" \
	--hostname "$CT_HOSTNAME" \
	--unprivileged 1 \
	--features nesting=1 \
	--memory 2048 \
	--cores 2 \
	--rootfs "$STORAGE:8" \
	--net0 "name=eth0,bridge=$BRIDGE,ip=dhcp" \
	--onboot 1 \
	--ostype debian >&2
# From here on a failure leaves a container behind; say how to remove it.
CREATED_OK=0
# shellcheck disable=SC2329 # invoked by the EXIT trap below
on_exit() {
	[ "$CREATED_OK" -eq 1 ] || log "container $CTID was created but the install did not finish; to remove it: pct stop $CTID; pct destroy $CTID"
}
trap on_exit EXIT
log "starting container $CTID"
pct start "$CTID" >&2
pct exec "$CTID" -- systemctl is-system-running --wait >/dev/null 2>&1 || true

log "pushing install.sh into the container"
pct exec "$CTID" -- install -d -m 0700 /root/dilla-install >&2
pct push "$CTID" "$INSTALLER" /root/dilla-install/install.sh --perms 0700 >&2
[ -z "$BINARY" ] || pct push "$CTID" "$BINARY" /root/dilla-install/dillad --perms 0755 >&2
[ -z "$CORE" ] || pct push "$CTID" "$CORE" /root/dilla-install/dilla_core_wasi.wasm --perms 0644 >&2
[ -z "$SUMS" ] || pct push "$CTID" "$SUMS" /root/dilla-install/SHA256SUMS --perms 0644 >&2

log "running install.sh in container $CTID"
STATUS=0
OUT="$(pct exec "$CTID" -- sh /root/dilla-install/install.sh "$MODE_ARG" "${INSTALL_ARGS[@]}" "${FILE_ARGS[@]}")" || STATUS=$?
# install.sh prints the link alone on its last stdout line; anything else on
# stdout (pct's own chatter) goes to stderr.
LINK="$(printf '%s\n' "$OUT" | grep '^https://' | tail -n 1 || true)"
printf '%s\n' "$OUT" | grep -v '^https://' >&2 || true
pct exec "$CTID" -- rm -rf /root/dilla-install >&2 || log "warning: could not remove /root/dilla-install from the container"

if [ "$STATUS" -ne 0 ]; then
	log "error: install.sh failed (exit $STATUS); its own error is in the lines above. If the service was installed, its log is: pct exec $CTID -- journalctl -o cat -u dilla"
else
	CREATED_OK=1
fi
if [ "$BEHIND_PROXY" -eq 1 ]; then
	log "route your proxy to container $CTID's --listen port, and forward $UDP_PORT/udp to it"
else
	log "forward 443/tcp and $UDP_PORT/udp from the host to container $CTID; a NATed container keeps turn.relay_ip = \"auto\""
fi
# The link is printed once and is stored nowhere: the database holds only its
# SHA-256. It is printed even when the install failed, because the invite stays
# valid for 24 hours and cannot be shown again.
[ -z "$LINK" ] || printf '%s\n' "$LINK"
[ "$STATUS" -eq 0 ] || exit 1
[ -n "$LINK" ] || die "install.sh did not print a bootstrap invite link"
