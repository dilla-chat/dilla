#!/usr/bin/env bash
# Install-and-launch smoke tests of the Linux release assets (GPUI release plan, Task 6), run as root inside a clean
# distribution container by desktop-gpui-release.yml's smoke job (and by hand with podman or docker):
#
#   docker run --rm -v "$PWD:/w" -w /w ubuntu:24.04 bash scripts/desktop-gpui-smoke-linux.sh ubuntu-deb 0.2.0-alpha.1 dist
#
# Checks:
#   ubuntu-deb      apt installs the deb; dilla-desktop --version; the AppImage runs with --appimage-extract-and-run
#   debian-deb      apt installs the deb; dilla-desktop --version
#   fedora-rpm      dnf installs the rpm; dilla-desktop --version
#   arch-aur        the AUR recipe (verified against the tar.gz) builds with makepkg from the local tar.gz, namcap
#                   reviews the recipe and the package, pacman installs it; dilla-desktop --version
#   ubuntu-upgrade  beta.5's deb, then this deb over it: the version moves up, /opt/Dilla holds only the GPUI files,
#                   the old app data is kept, dilla-desktop --version works through /usr/bin
#   fedora-upgrade  the same with beta.5's rpm (its %postun must not break the upgrade)
#   launch          the AppImage starts under Xvfb on lavapipe with a GNOME Keyring in a D-Bus session, has its
#                   window (class dev.dilla.desktop) up and is still alive after 10 s, and exits on SIGTERM
# The upgrade checks read beta.5's packages from <beta5-dir>, checked against their pinned SHA-256 here.
#
# Usage: desktop-gpui-smoke-linux.sh <check> <version> <assets-dir> [<beta5-dir>]
set -euo pipefail

BETA5_DEB=dilla-0.1.0-beta.5-linux-x64.deb
BETA5_DEB_SHA256=1bfe5deba7d2d2e6b5fc01ecde0c2e0b0784ad383e4a638be91537d1ee6832e9
BETA5_RPM=dilla-0.1.0-beta.5-linux-x64.rpm
BETA5_RPM_SHA256=bef3fa2e385a38b95665a3011b7bc00ad7bae5c1cb1114a39a38f186c063b715

[ "$#" -ge 3 ] && [ "$#" -le 4 ] || { echo 'usage: desktop-gpui-smoke-linux.sh <check> <version> <assets-dir> [<beta5-dir>]' >&2; exit 1; }
check=$1
version=$2
assets=$(cd "$3" && pwd -P)
beta5=${4:-}
base=dilla-$version-linux-x64
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+-alpha\.[0-9]+$ ]] || { echo "not a release version: $version" >&2; exit 1; }
alpha=${version##*.}
core=${version%-alpha.*}

fail() { echo "smoke ($check): $*" >&2; exit 1; }
# `dilla-desktop --version` prints `Dilla <version> (<commit>)` and opens no display.
says_version() {
  local got
  got=$("$@" --version)
  echo "$got"
  [[ $got == "Dilla $version ("*")" ]] || fail "$* --version printed '$got', not 'Dilla $version (...)'"
}
apt_install() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -q
  apt-get install -y -q --no-install-recommends "$@"
}
pinned() {  # <dir> <file> <sha256>
  [ -n "$1" ] && [ -f "$1/$2" ] || fail "beta.5's $2 is not in '$1'"
  printf '%s  %s\n' "$3" "$1/$2" | sha256sum --check --strict || fail "$2 does not match its pinned SHA-256"
}
# Only the GPUI files are left in /opt/Dilla after an upgrade (Electron's are removed with the old package). beta.5's
# rpm did not own its directories, so rpm leaves Electron's emptied directories (locales/, resources/) behind: they
# may stay, but must hold nothing.
opt_is_gpui_only() {
  local got
  got=$(cd /opt/Dilla && find . ! -type d | LC_ALL=C sort | tr '\n' ' ')
  [ "$got" = "./dilla-desktop ./licenses/LICENSE ./licenses/OFL-DMSerifDisplay.txt ./licenses/OFL-JetBrainsMono.txt ./licenses/THIRD-PARTY.md ./licenses/libwebrtc-LICENSE.md " ] \
    || fail "/opt/Dilla holds other files than the GPUI app's: $got"
  got=$(cd /opt/Dilla && find . -mindepth 1 -type d ! -path ./licenses | LC_ALL=C sort | tr '\n' ' ')
  [ -z "$got" ] || echo "note: empty directories left by the old package: $got"
}
# Electron's app data (~/.config/Dilla) stays on disk, unused: no package script removes it.
plant_user_data() { mkdir -p /root/.config/Dilla; echo beta5 > /root/.config/Dilla/marker; }
user_data_kept() { [ "$(cat /root/.config/Dilla/marker 2>/dev/null)" = beta5 ] || fail "beta.5's app data was removed"; }

case "$check" in
  ubuntu-deb|debian-deb)
    apt_install "$assets/$base.deb"
    [ "$(dpkg-query -W -f '${Version}' dilla)" = "$core~alpha.$alpha-1" ] || fail "dpkg reports $(dpkg-query -W -f '${Version}' dilla)"
    says_version dilla-desktop
    [ "$(command -v dilla-desktop)" = /usr/bin/dilla-desktop ] || fail "dilla-desktop is not /usr/bin/dilla-desktop"
    test -f /usr/share/applications/dev.dilla.desktop.desktop
    test -f /usr/share/icons/hicolor/512x512/apps/dev.dilla.desktop.png
    test -f /opt/Dilla/licenses/THIRD-PARTY.md
    if [ "$check" = ubuntu-deb ]; then
      cp "$assets/$base.AppImage" /tmp/dilla.AppImage
      chmod +x /tmp/dilla.AppImage
      (cd /tmp && says_version ./dilla.AppImage --appimage-extract-and-run)
    fi
    apt-get remove -y -q dilla
    [ ! -e /opt/Dilla ] && [ ! -e /usr/bin/dilla-desktop ] || fail "removing the package left files behind"
    ;;
  fedora-rpm)
    dnf install -y -q --setopt=install_weak_deps=False "$assets/$base.rpm"
    [ "$(rpm -q --qf '%{VERSION}-%{RELEASE}' dilla)" = "$core-0.alpha.$alpha.1" ] || fail "rpm reports $(rpm -q dilla)"
    says_version dilla-desktop
    rpm -V dilla || fail "rpm -V found changed files"
    dnf remove -y -q dilla
    [ ! -e /opt/Dilla ] && [ ! -e /usr/bin/dilla-desktop ] || fail "removing the package left files behind"
    ;;
  arch-aur)
    pacman -Syu --noconfirm --needed base-devel namcap nodejs
    node "$root/scripts/desktop-gpui-aur.mjs" verify "$version" "$assets/$base.tar.gz" "$assets/$base.aur.tar.gz"
    build=$(mktemp -d)
    tar -xzf "$assets/$base.aur.tar.gz" -C "$build"
    # makepkg takes a source file already beside the PKGBUILD instead of downloading it; the recipe's sha256sums
    # still checks it.
    cp "$assets/$base.tar.gz" "$build/"
    useradd -m builder
    chown -R builder: "$build"
    # The recipe's dependencies, installed as root (the builder has no sudo), so namcap sees them installed.
    # shellcheck disable=SC1091
    mapfile -t deps < <(cd "$build" && bash -c 'source ./PKGBUILD && printf "%s\n" "${depends[@]}"')
    pacman -S --noconfirm --needed "${deps[@]}"
    namcap "$build/PKGBUILD"
    su builder -c "cd '$build' && makepkg --noconfirm"
    pkg=$(ls "$build"/dilla-bin-*.pkg.tar.zst)
    namcap "$pkg" | tee "$build/namcap.txt"
    # The one accepted error: the app lives in /opt/Dilla (beta.5's layout, release plan D3).
    if grep -E ' E: ' "$build/namcap.txt" | grep -vF "E: ELF files outside of a valid path ('opt/')."; then fail "namcap reports errors"; fi
    pacman -U --noconfirm "$pkg"
    [ "$(pacman -Q dilla-bin)" = "dilla-bin ${core}alpha$alpha-1" ] || fail "pacman reports $(pacman -Q dilla-bin)"
    [ "$(vercmp "${core}alpha$alpha-1" 0.1.0beta5-1)" = 1 ] || fail "vercmp orders the release below beta.5"
    says_version dilla-desktop
    ;;
  ubuntu-upgrade)
    pinned "$beta5" "$BETA5_DEB" "$BETA5_DEB_SHA256"
    apt_install "$beta5/$BETA5_DEB"
    [ "$(dpkg-query -W -f '${Version}' dilla)" = 0.1.0~beta.5-1 ] || fail "beta.5 did not install"
    test -e /opt/Dilla/chrome-sandbox
    plant_user_data
    dpkg --compare-versions "$core~alpha.$alpha-1" gt 0.1.0~beta.5-1 || fail "dpkg orders the release below beta.5"
    apt_install "$assets/$base.deb"
    [ "$(dpkg-query -W -f '${Version}' dilla)" = "$core~alpha.$alpha-1" ] || fail "dpkg reports $(dpkg-query -W -f '${Version}' dilla) after the upgrade"
    opt_is_gpui_only
    user_data_kept
    says_version /usr/bin/dilla-desktop
    says_version dilla-desktop
    # beta.5's postrm removed its update-alternatives group during the upgrade; nothing of it is left.
    if update-alternatives --query dilla-desktop >/dev/null 2>&1; then fail "beta.5's dilla-desktop alternative is still registered"; fi
    ;;
  fedora-upgrade)
    pinned "$beta5" "$BETA5_RPM" "$BETA5_RPM_SHA256"
    dnf install -y -q --setopt=install_weak_deps=False "$beta5/$BETA5_RPM"
    [ "$(rpm -q --qf '%{VERSION}-%{RELEASE}' dilla)" = 0.1.0-0.beta.5.1 ] || fail "beta.5 did not install"
    test -e /opt/Dilla/chrome-sandbox
    plant_user_data
    dnf install -y -q rpmdevtools
    order=0
    rpmdev-vercmp "0:$core-0.alpha.$alpha.1" 0:0.1.0-0.beta.5.1 >/dev/null || order=$?
    [ "$order" -eq 11 ] || fail "rpm does not order the release above beta.5 (rpmdev-vercmp exit $order)"
    dnf install -y -q --setopt=install_weak_deps=False "$assets/$base.rpm"
    [ "$(rpm -q --qf '%{VERSION}-%{RELEASE}' dilla)" = "$core-0.alpha.$alpha.1" ] || fail "rpm reports $(rpm -q dilla) after the upgrade"
    opt_is_gpui_only
    user_data_kept
    says_version /usr/bin/dilla-desktop
    ;;
  launch)
    apt_install xvfb xauth dbus dbus-x11 gnome-keyring mesa-vulkan-drivers libvulkan1 libegl1 libgl1-mesa-dri \
      libxkbcommon-x11-0 libxkbcommon0 libxcb1 libasound2t64 libwayland-client0 libx11-6 libx11-xcb1 libdrm2 libgbm1 \
      fonts-dejavu-core xdg-utils procps x11-utils
    cp "$assets/$base.AppImage" /tmp/dilla.AppImage
    chmod +x /tmp/dilla.AppImage
    cat > /tmp/launch.sh <<'EOS'
set -u
echo -n smoke | gnome-keyring-daemon --unlock --components=secrets >/tmp/keyring.env
. /tmp/keyring.env
export GNOME_KEYRING_CONTROL
APPIMAGE_EXTRACT_AND_RUN=1 /tmp/dilla.AppImage >/tmp/dilla.log 2>&1 &
pid=$!
sleep 10
if ! kill -0 "$pid" 2>/dev/null; then echo "the app exited within 10 s:"; cat /tmp/dilla.log; exit 1; fi
# Its window is mapped, with the app id as its class (the desktop entry's StartupWMClass).
if ! xwininfo -root -tree | grep -F '"dev.dilla.desktop"'; then echo "no window of class dev.dilla.desktop:"; xwininfo -root -tree; cat /tmp/dilla.log; kill -KILL "$pid"; exit 1; fi
kill -TERM "$pid"
for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
if kill -0 "$pid" 2>/dev/null; then echo "the app ignored SIGTERM:"; cat /tmp/dilla.log; kill -KILL "$pid"; exit 1; fi
echo "the app ran 10 s and exited on SIGTERM; its output:"
cat /tmp/dilla.log
EOS
    LIBGL_ALWAYS_SOFTWARE=1 xvfb-run -a -s '-screen 0 1280x800x24' dbus-run-session -- bash /tmp/launch.sh
    ;;
  *) fail "unknown check" ;;
esac
echo "smoke ($check): ok"
