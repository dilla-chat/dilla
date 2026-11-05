#!/usr/bin/env bash
# The Android emulator for mobile.yml's mobile-android job, on a GitHub-hosted Ubuntu runner (its preinstalled SDK at
# $ANDROID_HOME, JDK 21 at $JAVA_HOME_21_X64). The emulator smoke test (packages/mobile/e2e/android-smoke.mjs) starts
# the AVD itself; this script only provisions it.
#   kvm      let the runner user open /dev/kvm (the standard udev rule; the emulator refuses to run without KVM)
#   sdk      install the SDK packages the build and the AVD need, then check they are really there: sdkmanager can
#            print a download failure and still exit 0. The workflow runs this through scripts/retry-on.mjs and an
#            actions/cache of the system image, so only a miss downloads it.
#   avd      create the AVD the smoke test boots (dilla35, API 35 google_apis: adb root works there), software GPU
set -euo pipefail

API=35
IMAGE="system-images;android-${API};google_apis;x86_64"
AVD=dilla35
sdk="${ANDROID_HOME:?ANDROID_HOME is not set}"
bin="$sdk/cmdline-tools/latest/bin"

# It changes the system (udev), the SDK and replaces the dilla35 AVD (avdmanager --force reads the account's home
# directory, not $HOME): a GitHub runner only, never a developer's machine, where dilla35 is the README's own AVD.
if [ "${GITHUB_ACTIONS:-}" != true ]; then
  echo "ci-android: runs on a GitHub Actions runner only (GITHUB_ACTIONS=true); it would replace this machine's $AVD AVD" >&2
  exit 2
fi

case "${1:-}" in
kvm)
  echo 'KERNEL=="kvm", GROUP="kvm", MODE="0666", OPTIONS+="static_node=kvm"' | sudo tee /etc/udev/rules.d/99-kvm4all.rules >/dev/null
  sudo udevadm control --reload-rules
  sudo udevadm trigger --name-match=kvm
  test -r /dev/kvm -a -w /dev/kvm || { echo "ci-android: /dev/kvm is not usable on this runner" >&2; exit 1; }
  ;;
sdk)
  # `yes` ends on SIGPIPE once sdkmanager stops reading, which pipefail would report: only sdkmanager's status counts.
  { yes || :; } | "$bin/sdkmanager" --licenses >/dev/null
  "$bin/sdkmanager" --install platform-tools emulator "platforms;android-36" "$IMAGE"
  missing=()
  for path in platform-tools/adb emulator/emulator "platforms/android-36/android.jar" "system-images/android-${API}/google_apis/x86_64/system.img"; do
    [ -e "$sdk/$path" ] || missing+=("$path")
  done
  if [ "${#missing[@]}" -gt 0 ]; then
    echo "ci-android: Failed to download: ${missing[*]} missing under $sdk after sdkmanager" >&2
    exit 1
  fi
  ;;
avd)
  # Pin the AVD directory: avdmanager's default differs between cmdline-tools versions and runner images (on the
  # hosted runner the AVD did not land in ~/.android/avd), and the emulator in the next steps must find the same one.
  export ANDROID_AVD_HOME="${ANDROID_AVD_HOME:-$HOME/.android/avd}"
  mkdir -p "$ANDROID_AVD_HOME"
  echo "ANDROID_AVD_HOME=$ANDROID_AVD_HOME" >> "$GITHUB_ENV"
  echo no | "$bin/avdmanager" create avd --force --name "$AVD" --package "$IMAGE" --device pixel_6 --path "$ANDROID_AVD_HOME/$AVD.avd"
  config="$ANDROID_AVD_HOME/$AVD.avd/config.ini"
  if [ ! -f "$config" ]; then
    echo "ci-android: avdmanager did not write $config; AVDs it knows:" >&2
    "$bin/avdmanager" list avd >&2 || true
    exit 1
  fi
  # No GPU on the runner: SwiftShader. The smoke test starts the emulator with -no-window and no -gpu flag, so the
  # AVD's own config carries the mode.
  sed -i -E '/^(hw\.gpu\.enabled|hw\.gpu\.mode|hw\.ramSize|disk\.dataPartition\.size|hw\.keyboard) *=/d' "$config"
  printf '%s\n' hw.gpu.enabled=yes hw.gpu.mode=swiftshader_indirect hw.ramSize=4096 disk.dataPartition.size=6G hw.keyboard=yes >> "$config"
  echo "ci-android: AVD $AVD ($IMAGE)"
  ;;
*)
  echo "usage: $0 kvm|sdk|avd" >&2
  exit 2
  ;;
esac
