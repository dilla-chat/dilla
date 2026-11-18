#!/usr/bin/env bash
# The macOS release assets of the native Dilla desktop app (GPUI release plan, Task 12), from one release binary:
#   dilla-<v>-mac-arm64.dmg   Dilla.app and an /Applications link (hdiutil ULFO)
#   dilla-<v>-mac-arm64.zip   Dilla.app (ditto, signature kept): what the in-app updater downloads
# and nothing else: the out-dir must be empty.
#
# Without CSC_LINK the app and the DMG are signed ad hoc (a PR build, a local build): they run, but Gatekeeper
# rejects them. With CSC_LINK they are signed with the Developer ID Application certificate and notarized:
#   1. the certificate goes into a temporary keychain (removed on exit);
#   2. the binary, then Dilla.app, are signed (hardened runtime, secure timestamp, entitlements.plist);
#   3. Dilla.app is notarized (zipped for notarytool) and stapled, so the copy dragged out of the DMG has its ticket;
#   4. the DMG is built from the stapled app, signed, notarized and stapled;
#   5. the zip is made from the stapled app, and `spctl` must accept the app and the DMG.
# (The plan names one submission, for the DMG; the app is submitted first as well so it can carry its own ticket.)
#
# Usage: desktop-gpui-package-mac.sh <version> <binary> <out-dir>      version: X.Y.Z or X.Y.Z-<prerelease>
# Environment (all or none of the keyed ones):
#   CSC_LINK, CSC_KEY_PASSWORD                          the base64 Developer ID .p12 and its password (as beta.5)
#   APPLE_API_KEY, APPLE_API_KEY_ID, APPLE_API_ISSUER   App Store Connect API key (the .p8: a path, its text or its
#                                                       base64), its key id and the issuer id (as beta.5)
#   DILLA_WEBRTC_LICENSE                                libwebrtc's LICENSE.md for the full licence bundle (required
#                                                       with CSC_LINK; without it only Dilla's licence and the font
#                                                       licences ship)
#   DILLA_CARGO_METADATA                                a saved `cargo metadata` JSON for the licence bundle
set -euo pipefail

die() { echo "desktop-gpui-package-mac: $*" >&2; exit 1; }
[ "$#" -eq 3 ] || { echo 'usage: desktop-gpui-package-mac.sh <version> <binary> <out-dir>' >&2; exit 1; }
version=$1
binary=$2
out=$3
[[ "$version" =~ ^([0-9]+\.[0-9]+\.[0-9]+)(-[0-9A-Za-z.]+)?$ ]] || die "version '$version' is not X.Y.Z or X.Y.Z-<prerelease>"
short_version=${BASH_REMATCH[1]}
build_version=${version//-alpha./a}
build_version=${build_version//-beta./b}
[ "$(uname -s)" = Darwin ] || die 'runs on macOS only (codesign, hdiutil, ditto)'
[ -f "$binary" ] || die "$binary does not exist"
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
mac=$root/clients/desktop/packaging/macos
brand=$root/clients/desktop/assets/brand

keyed=0
if [ -n "${CSC_LINK:-}" ]; then
  keyed=1
  : "${CSC_KEY_PASSWORD:?set CSC_KEY_PASSWORD with CSC_LINK}"
  : "${APPLE_API_KEY:?set APPLE_API_KEY with CSC_LINK (a signed build is always notarized)}"
  : "${APPLE_API_KEY_ID:?set APPLE_API_KEY_ID with CSC_LINK}"
  : "${APPLE_API_ISSUER:?set APPLE_API_ISSUER with CSC_LINK}"
  [ -n "${DILLA_WEBRTC_LICENSE:-}" ] || die 'set DILLA_WEBRTC_LICENSE with CSC_LINK (a signed build ships the full licence bundle)'
fi
if [ -n "${DILLA_WEBRTC_LICENSE:-}" ]; then [ -f "$DILLA_WEBRTC_LICENSE" ] || die "$DILLA_WEBRTC_LICENSE does not exist"; fi

mkdir -p "$out"
out=$(cd "$out" && pwd -P)
[ -z "$(ls -A "$out")" ] || die "$out is not empty"
base=dilla-$version-mac-arm64

work=$(mktemp -d)
keychain=
# macOS's bash 3.2 enters an EXIT trap with $? = 0 after a fatal error such as an unbound variable, so the script
# would exit 0 having built nothing (alpha.1's sign run 44241908836 went on to Gatekeeper without a DMG). The last
# line sets `finished`; any other way out fails.
finished=0
cleanup() {
  local status=$?
  if [ -n "$keychain" ]; then security delete-keychain "$keychain" >/dev/null 2>&1 || true; fi
  rm -rf "$work"
  if [ "$finished" != 1 ] && [ "$status" = 0 ]; then status=1; fi
  exit "$status"
}
trap cleanup EXIT

# No test-only code (the check reads raw bytes), then a stripped copy: -x keeps the global symbols the dynamic
# loader and crash reports need.
bash "$root/scripts/check-desktop-release-binary.sh" "$binary"
stage=$work/stage
app=$stage/Dilla.app
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources/licenses"
install -m 0755 "$binary" "$app/Contents/MacOS/dilla-desktop"
strip -x "$app/Contents/MacOS/dilla-desktop"
bash "$root/scripts/check-desktop-release-binary.sh" "$app/Contents/MacOS/dilla-desktop"

sed -e "s/@SHORT_VERSION@/$short_version/" -e "s/@BUILD_VERSION@/$build_version/" "$mac/Info.plist.in" > "$app/Contents/Info.plist"
install -m 0644 "$brand/Dilla.icns" "$app/Contents/Resources/Dilla.icns"
if [ -n "${DILLA_WEBRTC_LICENSE:-}" ]; then
  metadata_args=()
  [ -z "${DILLA_CARGO_METADATA:-}" ] || metadata_args=(--metadata "$DILLA_CARGO_METADATA")
  node "$root/scripts/desktop-third-party.mjs" "$app/Contents/Resources/licenses" --target aarch64-apple-darwin \
    --webrtc-license "$DILLA_WEBRTC_LICENSE" ${metadata_args[@]+"${metadata_args[@]}"}
else
  install -m 0644 "$root/LICENSE" "$root/clients/desktop/dilla-desktop-ui/assets/fonts/OFL-JetBrainsMono.txt" \
    "$root/clients/desktop/dilla-desktop-ui/assets/fonts/OFL-DMSerifDisplay.txt" "$app/Contents/Resources/licenses/"
fi
plutil -lint "$app/Contents/Info.plist" "$mac/entitlements.plist"

# 1: the certificate and the notarization key.
identity=-
sign_flags=(--force --entitlements "$mac/entitlements.plist")
notary=()
if [ "$keyed" = 1 ]; then
  keychain=$work/dilla-signing.keychain-db
  keychain_password=$(uuidgen)
  p12=$work/certificate.p12
  printf '%s' "$CSC_LINK" | base64 --decode > "$p12"
  security create-keychain -p "$keychain_password" "$keychain"
  security set-keychain-settings -lut 21600 "$keychain"
  security unlock-keychain -p "$keychain_password" "$keychain"
  security import "$p12" -k "$keychain" -P "$CSC_KEY_PASSWORD" -T /usr/bin/codesign -T /usr/bin/security
  rm -f "$p12"
  security set-key-partition-list -S apple-tool:,apple: -s -k "$keychain_password" "$keychain" >/dev/null
  # codesign must be able to search the keychain; the user's own search list stays.
  existing=()
  while IFS= read -r line; do existing+=("$(echo "$line" | tr -d '"' | sed 's/^ *//')"); done < <(security list-keychains -d user)
  security list-keychains -d user -s "$keychain" ${existing[@]+"${existing[@]}"}
  identity=$(security find-identity -v -p codesigning "$keychain" | sed -n 's/^ *[0-9]*) \([0-9A-F]\{40\}\) "Developer ID Application:.*/\1/p' | head -n 1)
  [ -n "$identity" ] || die 'the certificate is not a Developer ID Application identity'
  sign_flags+=(--options runtime --timestamp --keychain "$keychain")
  if [ -f "$APPLE_API_KEY" ]; then
    key=$APPLE_API_KEY
  else
    key=$work/AuthKey.p8
    if grep -q 'BEGIN PRIVATE KEY' <<<"$APPLE_API_KEY"; then printf '%s\n' "$APPLE_API_KEY" > "$key"; else printf '%s' "$APPLE_API_KEY" | base64 --decode > "$key"; fi
  fi
  notary=(xcrun notarytool submit --key "$key" --key-id "$APPLE_API_KEY_ID" --issuer "$APPLE_API_ISSUER" --wait)
else
  # Ad hoc keeps the hardened runtime flag and the entitlements, so what CI verifies is the shape that ships.
  sign_flags+=(--options runtime)
fi

# 2: the binary, then the bundle.
codesign "${sign_flags[@]}" --sign "$identity" "$app/Contents/MacOS/dilla-desktop"
codesign "${sign_flags[@]}" --sign "$identity" "$app"
codesign --verify --strict --verbose=2 "$app"

notarize() { # <file>: submit, wait, and fail unless accepted
  local log
  log=$("${notary[@]}" "$1" 2>&1) || { echo "$log" >&2; die "notarytool failed for $1"; }
  echo "$log"
  grep -q 'status: Accepted' <<<"$log" || die "$1 was not accepted by the notary service"
}

# 3: the app's own ticket.
if [ "$keyed" = 1 ]; then
  ditto -c -k --keepParent "$app" "$work/notarize-app.zip"
  notarize "$work/notarize-app.zip"
  xcrun stapler staple "$app"
fi

# 4: the DMG.
dmgroot=$work/dmg
mkdir -p "$dmgroot"
ditto "$app" "$dmgroot/Dilla.app"
ln -s /Applications "$dmgroot/Applications"
dmg=$out/$base.dmg
# hdiutil sizes a -srcfolder image by its own estimate, which can fall short of the app: "create failed - No space left
# on device" and "could not access /Volumes/Dilla/Dilla.app/…" while the runner's disk had 100 GB free (PR #179,
# 2026-11-17). Sized explicitly: the folder plus a quarter and 32 MB (the compressed ULFO is what ships).
dmg_kb=$(du -sk "$dmgroot" | cut -f 1)
dmg_mb=$(( dmg_kb * 5 / 4 / 1024 + 32 ))
df -h "$work" "$out"
echo "desktop-gpui-package-mac: DMG folder ${dmg_kb} KB, image ${dmg_mb} MB"
hdiutil create -volname Dilla -srcfolder "$dmgroot" -size "${dmg_mb}m" -ov -format ULFO "$dmg"
if [ "$keyed" = 1 ]; then
  codesign --force --timestamp --keychain "$keychain" --sign "$identity" "$dmg"
  notarize "$dmg"
  xcrun stapler staple "$dmg"
else
  codesign --force --sign - "$dmg"
fi

# 5: the updater's zip, from the stapled app, and the Gatekeeper verdicts.
ditto -c -k --keepParent "$app" "$out/$base.zip"
codesign --verify --strict --verbose=2 "$dmg"
if [ "$keyed" = 1 ]; then
  xcrun stapler validate "$app"
  xcrun stapler validate "$dmg"
  spctl -a -t exec -vv "$app"
  spctl -a -t open --context context:primary-signature -vv "$dmg"
fi

[ "$(ls "$out" | sort | tr '\n' ' ')" = "$base.dmg $base.zip " ] || die "expected exactly $base.dmg and $base.zip in $out, found: $(ls "$out" | tr '\n' ' ')"
ls -l "$out"
finished=1
