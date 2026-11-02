#!/usr/bin/env bash
# Disposable GitHub runners only. Normal distribution managers install the
# stock package; the app/test process always runs as a separate ordinary UID.
set -euo pipefail
[[ "${GITHUB_ACTIONS:-}" == true && "${GITHUB_REPOSITORY:-}" == jonasthim/dilla ]]
[[ "$(node -p 'process.versions.node.split(".")[0]')" == 24 ]]
format=${2:-${1:-}}
[[ "$format" == deb || "$format" == rpm || "$format" == aur ]]
observer=e2e/web/support/native-linux-package-acceptance.mjs

run_user() {
  # A private display, bus, password-protected disposable keyring and NSS CA
  # are created by the existing stock test. No login profile or sudo policy.
  install -d -m 700 -o dilla-ci -g dilla-ci /home/dilla-ci/.config /home/dilla-ci/.cache \
    /home/dilla-ci/.local/share /home/dilla-ci/.run /home/dilla-ci/tmp
  runuser -u dilla-ci -- env -i HOME=/home/dilla-ci USER=dilla-ci LOGNAME=dilla-ci PATH="$PATH" LANG=C.UTF-8 \
    XDG_CONFIG_HOME=/home/dilla-ci/.config XDG_CACHE_HOME=/home/dilla-ci/.cache \
    XDG_DATA_HOME=/home/dilla-ci/.local/share XDG_RUNTIME_DIR=/home/dilla-ci/.run TMPDIR=/home/dilla-ci/tmp \
    DILLA_E2E_HOST_DATA_ROOT=/home/dilla-ci/tmp/hosts \
    GITHUB_ACTIONS=true "GITHUB_REPOSITORY=$GITHUB_REPOSITORY" \
    "GITHUB_SHA=$GITHUB_SHA" "GITHUB_RUN_ID=$GITHUB_RUN_ID" "GITHUB_RUN_ATTEMPT=$GITHUB_RUN_ATTEMPT" \
    CI=true DILLA_E2E_TOOLING_DIR=/home/dilla-ci/workspace/artifacts/e2e-test-tools \
    "DILLA_NATIVE_BUILT_PACKAGE=${built_package:-}" \
    "DILLA_NATIVE_ACCEPTANCE_OUTPUT=$output/runtime.json" \
    bash -euc 'cd /home/dilla-ci/workspace
      node scripts/e2e-test-tools.mjs verify artifacts/e2e-test-tools
      node e2e/web/support/native-linux-package-acceptance.mjs installed "$1" "$2" "$3"
      xvfb-run -a bash e2e/web/support/installed-keyring.sh bash packages/desktop/scripts/with-window-manager.sh npm exec -w @dilla/e2e -- playwright test --config playwright.native-stock.config.ts
      node e2e/web/support/native-linux-package-acceptance.mjs finish "$1" "$2" "$3"' native-acceptance "$inputs" "$output" "$format"
}

if [[ "${1:-}" == --host-installed ]]; then
  [[ $EUID == 0 && "$format" == deb ]]
  inputs=/home/dilla-ci/native-linux-package-inputs
  output=/home/dilla-ci/native-linux-package-acceptance
  run_user
  exit
fi

if [[ "${1:-}" == --container ]]; then
  [[ $EUID == 0 && "$format" != deb ]]
  export PATH=/ci-node/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
  inputs=/packages
  output=/output
  if [[ "$format" == rpm ]]; then
    dnf install -y xorg-x11-server-Xvfb openbox xprop dbus-x11 nss-tools gnome-keyring python3 openssl libXtst util-linux tar gzip bsdtar git rpm desktop-file-utils shared-mime-info
    sha256sum /etc/dnf/dnf.conf /etc/yum.repos.d/*.repo > /output/signature-policy-before.txt
    dnf --dump-main-config > /output/dnf-effective-config.txt
    dnf install -y /packages/dilla-*-linux-x64.rpm
    sha256sum --check /output/signature-policy-before.txt
    rpm -qa | sort > /output/installed-manager-packages.txt
  else
    # Official Arch repository signatures remain required. LocalFileSigLevel
    # stays at the image default; no keys or signature policy are replaced.
    pacman -Syu --needed --noconfirm base-devel xorg-server-xvfb openbox xorg-xprop dbus nss gnome-keyring python openssl libxtst libarchive git desktop-file-utils shared-mime-info alsa-lib gtk3 libnotify libxss xdg-utils at-spi2-core libdrm libxkbcommon mesa libsecret libpipewire
    sha256sum /etc/pacman.conf > /output/signature-policy-before.txt
    pacman-conf SigLevel LocalFileSigLevel > /output/pacman-effective-policy.txt
  fi
  useradd --create-home --shell /bin/bash dilla-ci
  cp -a /input /home/dilla-ci/workspace
  chown -R dilla-ci:dilla-ci /home/dilla-ci/workspace
  chown dilla-ci:dilla-ci /output
  if [[ "$format" == aur ]]; then
    install -d -o dilla-ci -g dilla-ci /home/dilla-ci/recipe /home/dilla-ci/source-cache
    bsdtar -xf /packages/dilla-*-linux-x64.aur.tar.gz -C /home/dilla-ci/recipe
    cp /packages/dilla-*-linux-x64.deb /home/dilla-ci/source-cache/
    chown -R dilla-ci:dilla-ci /home/dilla-ci/recipe /home/dilla-ci/source-cache
    # The canonical source URL and SHA256 remain intact; makepkg consumes its
    # ordinary source cache and still validates integrity/dependencies.
    runuser -u dilla-ci -- env HOME=/home/dilla-ci PATH="$PATH" SRCDEST=/home/dilla-ci/source-cache \
      bash -euc 'cd /home/dilla-ci/recipe; makepkg --cleanbuild --noconfirm'
    built=(/home/dilla-ci/recipe/dilla-bin-*-x86_64.pkg.tar.zst)
    [[ ${#built[@]} == 1 && -f "${built[0]}" ]]
    built_package=${built[0]}
    pacman -U --noconfirm "$built_package"
    sha256sum --check /output/signature-policy-before.txt
    pacman -Q > /output/installed-manager-packages.txt
  fi
  run_user
  exit
fi

[[ $EUID != 0 && -n "${RUNNER_TEMP:-}" && -n "${GITHUB_WORKSPACE:-}" ]]
cd "$GITHUB_WORKSPACE"
inputs="$RUNNER_TEMP/native-linux-package-inputs"
output="$RUNNER_TEMP/native-linux-package-acceptance"
mkdir -m 755 "$output"
# The workflow downloads these packages from this run, not release URLs.
node "$observer" prepare "$inputs" "$output" "$format"
if [[ "$format" == deb ]]; then
  sudo apt-get update
  sudo apt-get install -y xvfb openbox x11-utils dbus-x11 libnss3-tools gnome-keyring python3 openssl libxtst6 desktop-file-utils shared-mime-info
  sha256sum /etc/apt/apt.conf.d/* > "$output/signature-policy-before.txt"
  sudo apt-get install -y "$inputs"/dilla-*-linux-x64.deb
  sha256sum --check "$output/signature-policy-before.txt"
  dpkg-query -W > "$output/installed-manager-packages.txt"
  sudo useradd --create-home --shell /bin/bash dilla-ci
  sudo cp -a "$GITHUB_WORKSPACE" /home/dilla-ci/workspace
  sudo chown -R dilla-ci:dilla-ci /home/dilla-ci/workspace
  # runner.temp may have private ancestors. Give the dedicated UID independent
  # copies rather than changing traversal rights on the runner's home/temp.
  sudo cp -a "$inputs" /home/dilla-ci/native-linux-package-inputs
  sudo install -d -m 700 -o dilla-ci -g dilla-ci /home/dilla-ci/native-linux-package-acceptance
  sudo cp "$output/inputs.json" "$output/signature-policy-before.txt" "$output/installed-manager-packages.txt" /home/dilla-ci/native-linux-package-acceptance/
  sudo chown -R dilla-ci:dilla-ci /home/dilla-ci/native-linux-package-inputs /home/dilla-ci/native-linux-package-acceptance
  # Sudo is the hosted runner's existing package-install boundary. It grants
  # no sudo access to dilla-ci and forwards only these explicit CI variables.
  sudo env "PATH=$PATH" GITHUB_ACTIONS=true "GITHUB_REPOSITORY=$GITHUB_REPOSITORY" \
    "GITHUB_SHA=$GITHUB_SHA" "GITHUB_RUN_ID=$GITHUB_RUN_ID" "GITHUB_RUN_ATTEMPT=$GITHUB_RUN_ATTEMPT" \
    bash e2e/web/support/native-linux-package-run.sh --host-installed deb
  # Read reports with only the test UID's access, then write them as the runner
  # for upload. No elevated reader follows test-owned paths.
  for report in installed.json runtime.json acceptance-deb.json; do
    sudo -H -u dilla-ci cat "/home/dilla-ci/native-linux-package-acceptance/$report" > "$output/$report"
  done
else
  case "$format" in
    rpm) image='registry.fedoraproject.org/fedora:44@sha256:9b53c238bb4e9f6221b4820bbdfa61c1bd8273d386dacaaed429a81dd7362085' ;;
    aur) image='docker.io/library/archlinux:base-devel-20261025.0.616225@sha256:624bdc7fc6615155d67bc34cf7146c0c9fd0e3c92943977da63a630e4ca49c30' ;;
  esac
  node_root=$(dirname "$(dirname "$(command -v node)")")
  docker run --rm --init --cap-add=SYS_ADMIN --shm-size=1g \
    --mount "type=bind,src=$GITHUB_WORKSPACE,dst=/input,readonly" \
    --mount "type=bind,src=$inputs,dst=/packages,readonly" \
    --mount "type=bind,src=$node_root,dst=/ci-node,readonly" \
    --mount "type=bind,src=$output,dst=/output" \
    --env PATH=/ci-node/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    --env GITHUB_ACTIONS=true --env "GITHUB_REPOSITORY=$GITHUB_REPOSITORY" \
    --env "GITHUB_SHA=$GITHUB_SHA" --env "GITHUB_RUN_ID=$GITHUB_RUN_ID" --env "GITHUB_RUN_ATTEMPT=$GITHUB_RUN_ATTEMPT" \
    "$image" bash /input/e2e/web/support/native-linux-package-run.sh --container "$format"
fi
test -s "$output/acceptance-$format.json"
