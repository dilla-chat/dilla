# @dilla/mobile: Dilla for iOS and Android

A Capacitor 8.6 shell around the bundled `@dilla/web` client. The app ships the client itself (`webDir: dist`, no
`server.url`) and talks to whichever Dilla instance the person picks, over HTTPS:

- Origin: `capacitor://localhost` on iOS, `https://localhost` on Android. Instances accept those two through
  `http.mobile_origins` (CORS on `/v1/` and `/i/`, and the gateway).
- `window.dillaMobile` (`src/bridge.ts`) implements the `MobileBridge` contract (`src/contract.ts`) that
  `packages/web/src/mobile.ts` reads: instance info, local notifications and their permission, haptics, pause/resume,
  switching instance, and browser sign-in.
- Browser sign-in (`src/browser-auth.ts`) is the desktop poll flow: the system browser (`@capacitor/browser`: SFSafariViewController /
  Custom Tabs) opens `/desktop-auth/<id>`, and the app polls over **native HTTP** (`CapacitorHttp`), which sends no
  `Origin` header. `/desktop-auth/` refuses any foreign Origin, so the WebView's own `fetch` cannot be used there.
- The instance picker (`src/picker.html`, `src/picker.ts`) keeps `instances` and `current` in `@capacitor/preferences`.
  Instances are HTTPS origins with a DNS name (no base path, no IP literal), validated with a copy of desktop's
  `canonicalInstance`, and probed with `GET <instance>/v1/instance` through the WebView's `fetch` (so through the
  server's CORS) before they are stored. The client's **Switch instance** (You, and the boot error screen) sends
  `switch-instance`, which returns to the picker.
- App data is not backed up: Android sets `allowBackup="false"`, and iOS marks `Library/WebKit`, `Library/Cookies`
  and `Library/Application Support` as excluded from backup at launch (`AppDelegate.swift`).
- All instances share the app's one origin, so the core worker keys each instance's OPFS store, KEK record and leader
  lock on `SHA-256(instance URL, 0x00, instanceId)` (client-core `worker/storage-key.ts`), never on the instanceId alone.
- CSP (a `<meta>` in every page): desktop's policy with `'self'` for the local assets. Capacitor's native bridge
  is injected as a document-start script, which page CSP does not govern.

## Commands

```sh
npm run build -w @dilla/mobile        # builds @dilla/web with __DILLA_MOBILE__ into .web-build/, then dist/ (CSP, bridge, picker, manifest)
npx cap sync                          # from packages/mobile: copies dist/ into ios/ and android/
npm test -w @dilla/mobile             # unit tests (browser-auth, picker, build, native project settings)
npm run typecheck -w @dilla/mobile
npm run e2e:android -w @dilla/mobile  # the emulator smoke test (below)
```

`npm run build` needs `packages/core-wasm/pkg` (the wasm-pack build of `core/dilla-core-wasm`), like any web build:

```sh
wasm-pack build core/dilla-core-wasm --target web --profile wasm-release --mode no-install --out-dir ../../packages/core-wasm/pkg
```

## iPhone (Mac, Xcode 27)

The iOS project is committed: Swift Package Manager (no CocoaPods), bundle id `chat.dilla.app`, deployment target
iOS 17, the UIScene lifecycle (`SceneDelegate.swift`, `SceneDelegateProxy`). The root view controller comes from
`Main.storyboard` only (`CAPBridgeViewController`); the scene delegate does not build a window in code.

Mac prerequisites:

- Xcode 27, opened once; `xcode-select -p` should print `/Applications/Xcode.app/Contents/Developer`
  (else `sudo xcode-select -s /Applications/Xcode.app`).
- Node 24 or newer: `nvm install 24 && nvm use 24`.
- The wasm toolchain (Path B, as verified in the M0 spike with Node 24.22.2, Homebrew rustup + llvm + wasm-pack and
  wasm-bindgen-cli 0.2.128):

```sh
brew install rustup llvm wasm-pack
# Homebrew's rustup has no rustup-init and its proxies are keg-only: put them on PATH.
export PATH="$(brew --prefix rustup)/bin:$HOME/.cargo/bin:$PATH"
rustup default 1.98.1          # rust-toolchain.toml pins 1.98.1 + wasm32-unknown-unknown; cargo adds the target
# --mode no-install needs a wasm-bindgen CLI whose version matches wasm-bindgen in Cargo.lock (currently 0.2.128):
#   grep -A1 '^name = "wasm-bindgen"$' Cargo.lock
cargo install wasm-bindgen-cli --version 0.2.128 --locked
# Apple's clang has no wasm32 target; the bundled SQLite C build needs Homebrew's llvm.
export CC_wasm32_unknown_unknown=$(brew --prefix llvm)/bin/clang AR_wasm32_unknown_unknown=$(brew --prefix llvm)/bin/llvm-ar
```

Build and run, from the repo root:

1. `npm install` (npm 11 may reformat unrelated lines of `package-lock.json`; do not commit those).
2. The `wasm-pack build` command above.
3. `npm run build -w @dilla/mobile`
4. `cd packages/mobile && npx cap sync ios` (copies `dist/` into `ios/App/App/public`, writes `CapApp-SPM/Package.swift`).
5. `npx cap open ios` (or open `ios/App/App.xcodeproj`). Xcode resolves `capacitor-swift-pm` 8.6.0 on first open; wait
   until "Resolving package graph" finishes. Commit any `Package.resolved` Xcode writes: Capacitor 8.6's template
   `.gitignore` does not ignore it.
6. Select the **App** target → Signing & Capabilities → tick "Automatically manage signing" and pick your Team. If
   `chat.dilla.app` is not available to your team, use a suffix there only (do not commit it).
7. Plug in the iPhone (Developer Mode on: Settings → Privacy & Security → Developer Mode). Pick it as the run
   destination and press Run (⌘R). Xcode 27 has no Simulator.app: simulators and devices are listed in
   **Device Hub** (Xcode → Open Developer Tool → Device Hub, `/Applications/Xcode.app/Contents/Applications/DeviceHub.app`),
   which replaces "Simulator" and the "Devices and Simulators" window. To run on a simulator, boot one in Device Hub
   and pick it as the destination.
8. In the app: the picker asks for an instance (`https://dilla.chat`); sign in or sign up; the phone shell opens.
   Web Inspector: Safari → Develop → <iPhone> → Dilla → `localhost`.

After each `npm run build`, repeat step 4 before running again.

## Android emulator

Host setup (as in the M0 spike): Android SDK at `~/Android/Sdk` (platform-tools, emulator, platform 36, a
`google_apis` x86_64 system image), JDK 21 (`~/.local/jdk/jdk-21*`; Gradle rejects newer JDKs), KVM, and an AVD, here
`dilla35` (API 35).

```sh
export ANDROID_HOME=~/Android/Sdk JAVA_HOME=$(ls -d ~/.local/jdk/jdk-21*)
npm run build -w @dilla/mobile
cd packages/mobile && npx cap sync android
npx cap run android --target dilla35          # or: cd android && ./gradlew assembleDebug
```

Network trust: the **release** build trusts system CAs only and refuses cleartext
(`android/app/src/main/res/xml/network_security_config.xml`). The **debug** build also trusts user-installed CAs
(`android/app/src/debug/res/xml/network_security_config.xml`), so a local instance with a development CA works on the
emulator. `test/native-projects.test.mjs` keeps the sources that way, and
`npm run android:check-release -w @dilla/mobile` checks a built release APK (after `./gradlew assembleRelease`).

### The smoke test

`npm run e2e:android -w @dilla/mobile` (add `-- --skip-build` to reuse the last APK, `-- --keep-emulator` to leave an
emulator it started running). It needs `target/dilla-testhost` (`go build -o target/dilla-testhost ./cmd/dilla-testhost`),
`target/release/dilla-testkit` (`cargo build -p dilla-testkit --release --locked`), openssl, and an AVD on a
debuggable `google_apis` image with Chrome (it uses `adb root` to install a throwaway CA as a user CA, and Chrome's
debug command-line file to skip Chrome's first run).

It starts two test hosts behind HTTPS proxies at `https://a.dilla.test:18443/` and `https://b.dilla.test:18444/`.
DNS is hermetic: the emulator's global HTTP proxy (`settings put global http_proxy 10.0.2.2:18480`, reset afterwards)
is a local CONNECT proxy that maps exactly those two names and ports to this machine, so the WebView, the Custom Tab
and CapacitorHttp all reach them with no outside DNS (a WebView `--host-resolver-rules` flag would miss CapacitorHttp;
dnsmasq behind `-dns-server` would need the privileged port 53). It installs a fresh debug APK, and through Playwright's Android driver (the WebView DevTools
socket): adds instance A in the picker, signs up, joins a native peer's server and sees the peer's message (and the peer
sees the reply); adds instance B and signs up there; checks that browser sign-in goes out over native HTTP with no
Origin and opens Custom Tabs; sends the app to the background for 120 s while the peer posts, resumes and sees it;
force-stops and relaunches into the same account on A with both instances in the picker; and fails on any CSP
violation. Screenshots, logcat on failure and the emulator's own log (`emulator.log`, when the test started it) land in
`e2e/.out/` (`DILLA_ANDROID_E2E_OUT` moves them; CI uses `target/mobile-android-e2e`).

## Not yet

- App-icon badge counts (no core Capacitor API; push brings them in wave M2). `send({ type: 'badge' })` is accepted
  and ignored.
- Voice calls (untested in the app), push, and background calls: waves M2 and M3. Android uses ConnectionService; iOS uses
  no CallKit in v1 (see the spec's "iOS calls after the M0 device spike").
