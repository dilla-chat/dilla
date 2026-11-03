#!/usr/bin/env bash
# The pinned clang 23 line for the wasm C build (ruling of 2026-10-13: scripts/check-ci-workflow.mjs holds why and
# what), installed from apt.llvm.org with the archive key checked by fingerprint on the very keyring apt is pointed
# at, every llvm-toolchain-23 package at one exact 23.1 build and read back with dpkg-query.
#
# The eight .deb files of that build are kept in an actions/cache directory: when the archive cannot be reached at
# all (run 639, 2026-11-03: apt.llvm.org answered nothing for over twenty minutes and every Linux job failed here),
# the cached build, which apt verified against the archive's signed index on the run that fetched it, is installed
# instead. A reachable archive whose candidate is not a 23.1 build still fails closed: that is a new upload, and
# the fix is to re-pin, not to build with whatever the cache holds.
#
# The archive's source list is removed once the packages are installed, so a later `apt-get update` (the Playwright
# dependency install) does not depend on apt.llvm.org either.
#
#   install  install clang 23 and write to $GITHUB_OUTPUT (when set): source=archive|cache, build=<the installed
#            build as a cache key suffix> and changed=true when the cache directory now holds a build it did not
set -euo pipefail

cache="${LLVM_DEB_CACHE_DIR:-$HOME/.cache/llvm-23-debs}"
archives="${LLVM_APT_ARCHIVES:-/var/cache/apt/archives}"
keyring="${LLVM_KEYRING:-/usr/share/keyrings/apt-llvm.gpg}"
source_list="${LLVM_SOURCE_LIST:-/etc/apt/sources.list.d/apt-llvm-23.list}"
apt_conf="${LLVM_APT_CONF:-/etc/apt/apt.conf.d}"
bin="${LLVM_BIN:-/usr/bin}"
temp="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
fingerprint=6084F3CF814B57C1CF12EFD515CF4D18AF4F7421
packages=(clang-23 llvm-23 libllvm23 libclang-cpp23 libclang1-23 libclang-common-23-dev llvm-23-linker-tools llvm-23-runtime)

# The one cached .deb of a package, or nothing.
cached_deb() {
  local found=("$cache/$1_"*.deb)
  if [ "${#found[@]}" -eq 1 ] && [ -f "${found[0]}" ]; then printf '%s\n' "${found[0]}"; fi
}

# Runs under errexit in its own subshell: any failed check ends the step, except that an archive which cannot be
# reached returns 3 (the one case the cached build answers). A reachable archive is pinned or fails the step.
install_from_archive() {
  if ! curl -fsSL --max-time 60 https://apt.llvm.org/llvm-snapshot.gpg.key -o "$temp/llvm-snapshot.asc"; then
    echo "apt.llvm.org could not be reached for its archive key" >&2
    return 3
  fi
  gpg --dearmor -o "$temp/apt-llvm.gpg" "$temp/llvm-snapshot.asc"
  primaries=$(gpg --show-keys --with-colons "$temp/apt-llvm.gpg" | awk -F: '$1 == "pub" { want = 1; next } $1 == "fpr" && want { print $10; want = 0 }')
  test "$primaries" = "$fingerprint"
  sudo install -m 0644 "$temp/apt-llvm.gpg" "$keyring"
  cmp "$temp/apt-llvm.gpg" "$keyring"
  echo "deb [signed-by=$keyring] https://apt.llvm.org/noble/ llvm-toolchain-noble-23 main" | sudo tee "$source_list"
  if ! sudo apt-get update; then
    sudo rm -f "$source_list"
    echo "apt.llvm.org could not be reached for its package index" >&2
    return 3
  fi
  apt-cache policy clang-23 llvm-23
  v=$(apt-cache policy clang-23 | awk '/Candidate:/ { print $2 }')
  case "$v" in 1:23.1.*~++*) ;; *) echo "expected clang 23.1.x candidate, got $v" >&2; exit 1 ;; esac
  # apt keeps what it downloads (the image may discard it, docker-clean style), so the build can be cached.
  echo 'APT::Keep-Downloaded-Packages "true";' | sudo tee "$apt_conf/99keep-debs" >/dev/null
  sudo rm -f "$apt_conf/docker-clean"
  sudo apt-get install -y --no-install-recommends "clang-23=$v" "llvm-23=$v" "libllvm23=$v" "libclang-cpp23=$v" "libclang1-23=$v" "libclang-common-23-dev=$v" "llvm-23-linker-tools=$v" "llvm-23-runtime=$v"
  sudo rm -f "$source_list"
  # The cache holds exactly this build's eight debs; a run that installed the same build changes nothing.
  local p downloaded=() before=()
  for p in "${packages[@]}"; do
    local files=("$archives/${p}_"*.deb)
    if [ "${#files[@]}" -ne 1 ] || [ ! -f "${files[0]}" ]; then echo "apt kept no single .deb of $p in $archives" >&2; exit 1; fi
    downloaded+=("${files[0]}")
  done
  mapfile -t before < <(cd "$cache" && ls -- *.deb 2>/dev/null | sort || true)
  rm -f "$cache"/*.deb
  cp -- "${downloaded[@]}" "$cache"/
  local changed=true
  if [ "$(printf '%s\n' "${before[@]}")" = "$(cd "$cache" && ls -- *.deb | sort)" ]; then changed=false; fi
  echo "$changed" > "$temp/ci-llvm.changed"
}

install_from_cache() {
  local p deb debs=()
  for p in "${packages[@]}"; do
    deb=$(cached_deb "$p")
    if [ -z "$deb" ]; then echo "no cached .deb of $p in $cache: apt.llvm.org is unreachable and no earlier run cached its build" >&2; exit 1; fi
    debs+=("$deb")
  done
  echo "installing the cached clang 23 build: apt.llvm.org is unreachable" >&2
  sudo apt-get install -y --no-install-recommends "${debs[@]}"
  changed=false
  source=cache
}

case "${1:-}" in
install)
  test "$(lsb_release -cs)" = noble
  mkdir -p "$cache"
  set +e
  ( set -e; install_from_archive )
  status=$?
  set -e
  case "$status" in
  0) source=archive; changed=$(cat "$temp/ci-llvm.changed") ;;
  3) install_from_cache ;;
  *) exit "$status" ;;
  esac
  v=$(dpkg-query -W -f='${Version}' clang-23)
  case "$v" in 1:23.1.*~++*) ;; *) echo "expected an installed clang 23.1.x build, got $v" >&2; exit 1 ;; esac
  for p in clang-23 llvm-23 libllvm23 libclang-cpp23 libclang1-23 libclang-common-23-dev llvm-23-linker-tools llvm-23-runtime; do test "$(dpkg-query -W -f='${Version}' "$p")" = "$v"; done
  "$bin/clang-23" --version | grep -F "clang version 23.1."
  "$bin/clang-23" --version
  "$bin/llvm-ar-23" --version
  build=$(printf '%s' "$v" | tr -c 'A-Za-z0-9._-' '-')
  echo "clang 23 build $v installed from the $source"
  if [ -n "${GITHUB_OUTPUT:-}" ]; then printf 'source=%s\nbuild=%s\nchanged=%s\n' "$source" "$build" "$changed" >> "$GITHUB_OUTPUT"; fi
  ;;
*)
  echo "usage: $0 install" >&2
  exit 2
  ;;
esac
