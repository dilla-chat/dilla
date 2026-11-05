# mobile-spike: WebView capability probe (M0, throwaway)

A Capacitor 7 app (`chat.dilla.spike`, never `chat.dilla.app`) whose page runs the repo's real capability checks once
per launch and logs one line:

```
DILLA_PROBE {"opfsSyncHandle":bool,"opfsPersisted":bool,"webLocks":bool,"encodedTransform":"script-transform"|"encoded-streams"|null,"audioWorklet":bool,"rnnoise":bool,"storagePersist":bool,"ua":string}
```

followed by `DILLA_PROBE_DETAIL {...}` with the reasons behind any `false` (isSecureContext, origin, the raw
`probe_persistence` report, the RNNoise worklet report, the voice-support verdict). The page shows both, and a
"Run again" button re-runs the probe under a user tap (in case audio needs activation).

| field | check (real repo code) |
| --- | --- |
| `opfsSyncHandle` | `localRuntimeReadiness` (`packages/client-core/src/worker/runtime-readiness.ts`) over `@dilla/core-wasm`'s `probe_persistence`, in a module Worker |
| `opfsPersisted` | a marker file in OPFS: absent on the first launch, found after a force-quit and relaunch |
| `webLocks` | `navigator.locks.request` |
| `encodedTransform` | `isVoiceSupported()` (`packages/media/src/support.ts`); `insertable-streams` is reported as `encoded-streams` |
| `audioWorklet`, `rnnoise` | `DillaRnnoiseProcessor.init` (`packages/media/src/audio/rnnoise.ts`) on a test-tone track, no microphone |
| `storagePersist` | `navigator.storage.persist()` |

Not a workspace, not in `ci:local`. `node_modules/`, `dist/`, Gradle and Xcode build outputs are gitignored.

## Prerequisites (both platforms)

The probe bundles two gitignored wasm files from the repo; `npm run build` (its `prebuild`) checks them:

- `packages/core-wasm/pkg/` — from the repo root:
  `wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg`
- `packages/media/src/audio/generated/rnnoise.wasm` — extracted by `prebuild` itself; it needs the repo root's
  `npm install` (for `@jitsi/rnnoise-wasm`).

If the core wasm cannot be built on a machine, build `dist/` on one that can and copy it over (`dist/` is all
`cap sync` needs).

## Android emulator (Linux)

One-time: JDK 21 (Gradle 8.11 does not run on newer JDKs), Android SDK in `~/Android/Sdk`:

```sh
# cmdline-tools unpacked to ~/Android/Sdk/cmdline-tools/latest
sdkmanager "platform-tools" "emulator" "platforms;android-35" "system-images;android-35;google_apis;x86_64"
avdmanager create avd -n dilla35 -k "system-images;android-35;google_apis;x86_64"
```

Run:

```sh
export JAVA_HOME=<jdk-21> ANDROID_HOME=~/Android/Sdk PATH=$ANDROID_HOME/platform-tools:$ANDROID_HOME/emulator:$PATH
emulator -avd dilla35 -no-window -no-audio &      # wait for: adb shell getprop sys.boot_completed -> 1
cd experiments/mobile-spike
npm install && npm run build
adb logcat -c
npx cap run android --target dilla35
adb logcat -d | grep DILLA_PROBE                 # first launch: opfsPersisted false
adb shell am force-stop chat.dilla.spike
adb logcat -c
adb shell monkey -p chat.dilla.spike -c android.intent.category.LAUNCHER 1
adb logcat -d | grep DILLA_PROBE                 # relaunch: opfsPersisted true
```

The lines arrive under the `Capacitor/Console` tag.

## iPhone (Mac, Xcode 16+)

The iOS project is committed (Swift Package Manager, no CocoaPods). The Mac needs Xcode 16+ (App Store, then open it
once to install its components) and Node 24+. It does **not** need Rust if it uses a `dist/` built on Linux, which is
the recommended path.

Mac prerequisites:

- Xcode 16 or newer, opened once; `xcode-select -p` should print `/Applications/Xcode.app/Contents/Developer`
  (else `sudo xcode-select -s /Applications/Xcode.app`).
- Node 24 or newer: `nvm install 24 && nvm use 24` (the repo's `engines` field requires it).

**Path A (recommended): copy a Linux-built `dist/`.** On the Linux host, which has the wasm toolchain:

```sh
cd experiments/mobile-spike && npm install && npm run build
tar czf /tmp/mobile-spike-dist.tgz dist      # then copy it to the Mac (scp, AirDrop)
```

On the Mac, from a checkout of this branch:

1. `cd experiments/mobile-spike && npm install` (only the Capacitor CLI and Vite; no root `npm install` needed).
2. `tar xzf <path>/mobile-spike-dist.tgz` inside `experiments/mobile-spike`, so `dist/index.html` exists. Do not run
   `npm run build` (it needs the wasm toolchain).

**Path B: build on the Mac.** Needs the whole wasm toolchain:

- Rust via rustup (`brew install rustup && rustup-init -y`); the repo's `rust-toolchain.toml` pins 1.98.1 with the
  `wasm32-unknown-unknown` target, which rustup installs on the first `cargo` call in the repo.
- wasm-pack: `cargo install wasm-pack` (or `brew install wasm-pack`).
- A clang that targets wasm32 for the bundled SQLite C build. Apple's clang does not: `brew install llvm`, then
  `export CC_wasm32_unknown_unknown=$(brew --prefix llvm)/bin/clang AR_wasm32_unknown_unknown=$(brew --prefix llvm)/bin/llvm-ar`.

Then: repo root `npm install`; the `wasm-pack build` command under Prerequisites;
`cd experiments/mobile-spike && npm install && npm run build`.

Then, either path:

3. `npx cap sync ios` (copies `dist/` into `ios/App/App/public` and writes `Package.swift`).
4. `npx cap open ios` (or open `ios/App/App.xcodeproj` in Xcode). Xcode resolves the `capacitor-swift-pm` package on
   first open; wait until "Resolving package graph" finishes.
5. Select the **App** target → Signing & Capabilities → tick "Automatically manage signing" and pick your Team. If
   `chat.dilla.spike` is taken for your team, use a suffix (e.g. `chat.dilla.spike.jonas`) there only.
6. Plug in the iPhone (Developer Mode on: Settings → Privacy & Security → Developer Mode), pick it as the run
   destination, press Run (⌘R). On the first install, trust the developer profile on the phone (Settings → General →
   VPN & Device Management) and run again.
7. Read the line in Xcode's debug console (View → Debug Area → Activate Console, ⇧⌘C); filter for `DILLA_PROBE`.
   Capacitor prints it as `⚡️  [log] - DILLA_PROBE {...}`. Copy both `DILLA_PROBE` and `DILLA_PROBE_DETAIL`.
   First launch: `opfsPersisted` is `false`.
8. Force-quit: swipe the app away in the app switcher (or Stop ⌘. in Xcode). Relaunch it from Xcode (⌘R, which does
   not reinstall over the data container) or from the home screen. If launched from the home screen, read the lines
   in Console.app (select the iPhone, filter `DILLA_PROBE`) or Safari → Develop → <iPhone> → dilla spike → Console.
   Second launch: `opfsPersisted` should be `true`.

Report both `DILLA_PROBE` lines verbatim, plus the `DILLA_PROBE_DETAIL` lines and the iOS version.
