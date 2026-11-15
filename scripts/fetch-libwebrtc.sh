#!/usr/bin/env bash
# LiveKit's prebuilt libwebrtc for a release build, pinned by SHA-256 (GPUI release plan D14).
#
# webrtc-sys's build.rs downloads this archive itself (webrtc_sys_build::download_webrtc) with no digest check. A
# release build instead fetches it here, checks the checked-in digest, unpacks it and points LK_CUSTOM_WEBRTC at it, so
# webrtc-sys links exactly the reviewed bytes and downloads nothing. CI's ordinary builds keep downloading.
#
# The archives are the ones webrtc-sys-build 0.3.21 (webrtc-sys 0.3.53, Cargo.lock) names: tag WEBRTC_TAG =
# webrtc-a545de4 of livekit/rust-sdks, asset webrtc-<os>-<arch>-release.zip, root directory <os>-<arch>-release/.
# The digests were taken from the downloaded archives on 2026-11-15 and match GitHub's own asset digests. When
# Cargo.lock moves webrtc-sys-build, scripts/fetch-libwebrtc.test.mjs fails until the tag and digests here move too.
#
# Usage: fetch-libwebrtc.sh <target-triple> <out-dir> [--archive FILE]
# Prints LK_CUSTOM_WEBRTC=<dir> on success (append it to $GITHUB_ENV). Exit 1 on a mismatch or bad usage.
set -euo pipefail

WEBRTC_SYS_BUILD_VERSION=0.3.21
WEBRTC_TAG=webrtc-a545de4

usage() { echo 'usage: fetch-libwebrtc.sh <target-triple> <out-dir> [--archive FILE]' >&2; exit 1; }
[ "$#" -eq 2 ] || { [ "$#" -eq 4 ] && [ "$3" = --archive ]; } || usage
target=$1
out=$2
case "$target" in
  x86_64-unknown-linux-gnu)
    triple=linux-x64-release
    sha256=2ece73b9a72a2cacd73d0c62ac3a414e202f37885c3c8f49dddb43c1443ac0bd
    ;;
  aarch64-apple-darwin)
    triple=mac-arm64-release
    sha256=153a1f1a268b812b4a501deda6505037f29481691efec52be817f560ff09e206
    ;;
  x86_64-pc-windows-msvc)
    triple=win-x64-release
    sha256=862403fcb248c7d8803487e4c22b8821c6130e89100cb9c69be95ef45ff46146
    ;;
  *) echo "fetch-libwebrtc: no pinned libwebrtc for target $target" >&2; exit 1 ;;
esac
archive_name=webrtc-$triple.zip
url=https://github.com/livekit/rust-sdks/releases/download/$WEBRTC_TAG/$archive_name

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
archive=$tmp/$archive_name
if [ "$#" -eq 4 ]; then
  cp -- "$4" "$archive"
else
  curl --proto '=https' --tlsv1.2 --fail --silent --show-error --location --retry 3 "$url" -o "$archive"
fi
printf '%s  %s\n' "$sha256" "$archive" | sha256sum --check --status || {
  echo "fetch-libwebrtc: SHA-256 mismatch for $archive_name ($WEBRTC_TAG, webrtc-sys-build $WEBRTC_SYS_BUILD_VERSION)" >&2
  exit 1
}

mkdir -p "$out"
dir=$(cd "$out" && pwd -P)/$triple
[ ! -e "$dir" ] || { echo "fetch-libwebrtc: $dir already exists" >&2; exit 1; }
unzip -q "$archive" -d "$tmp/x"
[ "$(ls -A "$tmp/x")" = "$triple" ] || { echo "fetch-libwebrtc: $archive_name does not hold exactly $triple/" >&2; exit 1; }
for need in include lib webrtc.ninja; do
  [ -e "$tmp/x/$triple/$need" ] || { echo "fetch-libwebrtc: $archive_name has no $triple/$need" >&2; exit 1; }
done
mv "$tmp/x/$triple" "$dir"
echo "LK_CUSTOM_WEBRTC=$dir"
