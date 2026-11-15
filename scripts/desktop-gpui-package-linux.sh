#!/usr/bin/env bash
# The Linux release assets of the native Dilla desktop app (GPUI release plan, Task 5), from one release binary:
#   dilla-<v>-linux-x64.AppImage   appimagetool (pinned) with the pinned type2 runtime; nothing bundled but the binary
#   dilla-<v>-linux-x64.tar.gz     dilla-<v>/{dilla-desktop, dev.dilla.desktop.desktop, dev.dilla.desktop.metainfo.xml,
#                                  icons/hicolor/..., licenses/...}, reproducible (sorted, root-owned, fixed mtime)
#   dilla-<v>-linux-x64.deb        nfpm (pinned) from clients/desktop/packaging/linux/nfpm.yaml: package `dilla`
#   dilla-<v>-linux-x64.rpm        the same, rpm
#   dilla-<v>-linux-x64.aur.tar.gz the AUR recipe (dilla-bin) pinning the tar.gz (scripts/desktop-gpui-aur.mjs)
# and nothing else: the out-dir must be empty, and the script fails if anything is missing or extra at the end.
#
# Order: strip a copy of the binary, check the glibc floor (2.39) and the absence of test-only code, build the AppDir,
# the AppImage, the tarball, the deb and rpm, then the AUR recipe from the tarball.
#
# Usage: desktop-gpui-package-linux.sh <version> <binary> <out-dir>
# Environment:
#   DILLA_APPIMAGE_RUNTIME   the pinned type2 runtime (install-ci-tool.sh appimage-runtime); required
#   DILLA_WEBRTC_LICENSE     libwebrtc's LICENSE.md (default: $LK_CUSTOM_WEBRTC/LICENSE.md, from fetch-libwebrtc.sh)
#   DILLA_APPIMAGETOOL, DILLA_NFPM   the tools (default: appimagetool, nfpm on PATH)
#   DILLA_CARGO_METADATA     a saved `cargo metadata` JSON for the licence bundle (default: run cargo metadata)
#   SOURCE_DATE_EPOCH        the tarball's file times (default: 0)
set -euo pipefail

[ "$#" -eq 3 ] || { echo 'usage: desktop-gpui-package-linux.sh <version> <binary> <out-dir>' >&2; exit 1; }
version=$1
binary=$2
out=$3
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
linux=$root/clients/desktop/packaging/linux
appimagetool=${DILLA_APPIMAGETOOL:-appimagetool}
nfpm=${DILLA_NFPM:-nfpm}
runtime=${DILLA_APPIMAGE_RUNTIME:?set DILLA_APPIMAGE_RUNTIME to the pinned AppImage runtime (install-ci-tool.sh appimage-runtime)}
webrtc_license=${DILLA_WEBRTC_LICENSE:-${LK_CUSTOM_WEBRTC:+$LK_CUSTOM_WEBRTC/LICENSE.md}}
[ -n "$webrtc_license" ] && [ -f "$webrtc_license" ] || { echo "desktop-gpui-package-linux: libwebrtc's LICENSE.md not found (set DILLA_WEBRTC_LICENSE or LK_CUSTOM_WEBRTC)" >&2; exit 1; }
[ -f "$runtime" ] || { echo "desktop-gpui-package-linux: $runtime does not exist" >&2; exit 1; }
[ -f "$binary" ] || { echo "desktop-gpui-package-linux: $binary does not exist" >&2; exit 1; }

# The version check and the package versions (deb 0.2.0~alpha.N-1, rpm 0.2.0-0.alpha.N.1).
versions=$(node "$root/scripts/desktop-gpui-aur.mjs" versions "$version")
deb_version=$(sed -n 's/^DEB_VERSION=//p' <<<"$versions")
deb_release=$(sed -n 's/^DEB_RELEASE=//p' <<<"$versions")
rpm_version=$(sed -n 's/^RPM_VERSION=//p' <<<"$versions")
rpm_release=$(sed -n 's/^RPM_RELEASE=//p' <<<"$versions")

mkdir -p "$out"
out=$(cd "$out" && pwd -P)
[ -z "$(ls -A "$out")" ] || { echo "desktop-gpui-package-linux: $out is not empty" >&2; exit 1; }
base=dilla-$version-linux-x64
assets=("$base.AppImage" "$base.tar.gz" "$base.deb" "$base.rpm" "$base.aur.tar.gz")

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# 1-3: no test-only code (checked before strip too: the symbol names go with it), a stripped copy, the glibc floor
# and no test-only code again.
bash "$root/scripts/check-desktop-release-binary.sh" "$binary"
bin=$work/dilla-desktop
cp -- "$binary" "$bin"
strip --strip-all "$bin"
chmod 0755 "$bin"
bash "$root/scripts/check-glibc-max.sh" 2.39 "$bin"
bash "$root/scripts/check-desktop-release-binary.sh" "$bin"

# The tarball's directory, which the deb, the rpm and the AUR recipe all install from.
stage=$work/dilla-$version
mkdir -p "$stage/licenses" "$stage/icons"
install -m 0755 "$bin" "$stage/dilla-desktop"
install -m 0644 "$linux/dev.dilla.desktop.desktop" "$linux/dev.dilla.desktop.metainfo.xml" "$stage/"
# The deb's /usr/share/doc/dilla/copyright and the deb and rpm's /usr/bin/dilla-desktop (nfpm.yaml's `../` sources),
# outside the tarball's directory.
install -m 0644 "$linux/copyright" "$work/copyright"
install -m 0755 "$linux/usr-bin-dilla-desktop" "$work/usr-bin-dilla-desktop"
cp -R "$linux/icons/hicolor" "$stage/icons/"
metadata_args=()
[ -z "${DILLA_CARGO_METADATA:-}" ] || metadata_args=(--metadata "$DILLA_CARGO_METADATA")
node "$root/scripts/desktop-third-party.mjs" "$stage/licenses" --webrtc-license "$webrtc_license" "${metadata_args[@]}"
find "$stage" -type d -exec chmod 0755 {} +
find "$stage" -type f ! -name dilla-desktop -exec chmod 0644 {} +

# 4-5: the AppDir and the AppImage.
appdir=$work/Dilla.AppDir
mkdir -p "$appdir/usr/bin" "$appdir/usr/share/applications" "$appdir/usr/share/metainfo" "$appdir/usr/share/icons" "$appdir/usr/share/licenses"
install -m 0755 "$bin" "$appdir/usr/bin/dilla-desktop"
install -m 0755 "$linux/AppRun" "$appdir/AppRun"
install -m 0644 "$linux/dev.dilla.desktop.desktop" "$appdir/usr/share/applications/"
install -m 0644 "$linux/dev.dilla.desktop.desktop" "$appdir/"
install -m 0644 "$linux/dev.dilla.desktop.metainfo.xml" "$appdir/usr/share/metainfo/"
cp -R "$linux/icons/hicolor" "$appdir/usr/share/icons/"
install -m 0644 "$linux/icons/hicolor/256x256/apps/dev.dilla.desktop.png" "$appdir/dev.dilla.desktop.png"
ln -s dev.dilla.desktop.png "$appdir/.DirIcon"
cp -R "$stage/licenses" "$appdir/usr/share/licenses/dilla"
ARCH=x86_64 APPIMAGE_EXTRACT_AND_RUN=1 "$appimagetool" --runtime-file "$runtime" "$appdir" "$out/$base.AppImage"
chmod 0755 "$out/$base.AppImage"

# 6: the tarball, reproducible.
tar --sort=name --mtime="@${SOURCE_DATE_EPOCH:-0}" --owner=0 --group=0 --numeric-owner --format=gnu \
  -C "$work" -cf - "dilla-$version" | gzip -n -9 > "$out/$base.tar.gz"

# 7: deb and rpm, both named `dilla` (they upgrade an installed beta.5).
# nfpm reads its sources relative to the tarball's directory (nfpm.yaml's `./` paths).
(cd "$stage" && DILLA_PKG_VERSION=$deb_version DILLA_PKG_RELEASE=$deb_release \
  "$nfpm" package --config "$linux/nfpm.yaml" --packager deb --target "$out/$base.deb")
(cd "$stage" && DILLA_PKG_VERSION=$rpm_version DILLA_PKG_RELEASE=$rpm_release \
  "$nfpm" package --config "$linux/nfpm.yaml" --packager rpm --target "$out/$base.rpm")

# 8: the AUR recipe, pinning the tarball (a release asset only: nothing publishes it).
node "$root/scripts/desktop-gpui-aur.mjs" recipe "$version" "$out/$base.tar.gz" "$out" >/dev/null

# Exactly the five assets, each non-empty.
for asset in "${assets[@]}"; do
  [ -s "$out/$asset" ] || { echo "desktop-gpui-package-linux: $asset is missing or empty" >&2; exit 1; }
done
actual=$(cd "$out" && ls -A | LC_ALL=C sort)
expected=$(printf '%s\n' "${assets[@]}" | LC_ALL=C sort)
[ "$actual" = "$expected" ] || { printf 'desktop-gpui-package-linux: %s holds other files than the five assets:\n%s\n' "$out" "$actual" >&2; exit 1; }
printf '%s\n' "${assets[@]}"
