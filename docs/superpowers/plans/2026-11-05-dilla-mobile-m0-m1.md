# dilla mobile M0 + M1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove on real devices that the web client can run inside iOS and Android WebViews (M0), then ship Capacitor shells that sign in to any dilla instance and run the full client in the foreground (M1).

**Architecture:**
- `packages/mobile` is a Capacitor 7 app (`chat.dilla.app`). It bundles `packages/web/dist`, as desktop does, and injects a small `window.dillaMobile` bridge before the web entry runs.
- The web client already accepts an explicit instance URL for its core worker (`createCoreWorker({ instanceURL })`, `packages/web/src/main.tsx:21`). Mobile passes the instance the user picked, so the app runs at `capacitor://localhost` / `https://localhost` and talks to the instance cross-origin.
- The server gets a separate `mobile_origins` list for CORS and the gateway origin check.

**Tech Stack:**
- Capacitor 7 (`@capacitor/core`, `ios`, `android`, `app`, `browser`, `preferences`, `local-notifications`, `status-bar`, `haptics`)
- TypeScript / React (existing web)
- Go (dillad)
- Xcode on the Mac, Android SDK with an emulator on Linux

**Spec:** `docs/superpowers/specs/2026-11-05-dilla-mobile-design.md`

**Later plans (written after the M0 gate):** M2 push, M3 background calls, M4 release + docs.

## Global Constraints
- **App ID:** `chat.dilla.app` on both platforms. The display name is `Dilla`.
- **Custody:** browser tier. No change to `Tier::Browser` anywhere.
- **Platform floors:** iOS 17+ and Android API 29+, with an Android System WebView that supports OPFS `createSyncAccessHandle` (Chromium 108+).
- **Code loading:** no remote code. All JS/WASM is bundled and checked against `dilla-manifest.json` (`scripts/write-manifest.mjs`). `server.url` is never set in `capacitor.config.ts`.
- **Mobile CSP:** `connect-src 'self' https: wss:`, otherwise identical to `packages/desktop/src/bundle.ts:56`.
- **Multi-instance:** one install can hold many instances, matching desktop. Per-instance state relies on the existing `dilla/<instance hex>` OPFS layout and the instance-keyed KEK.
- **Mac work:** every Xcode / iPhone step is sent to the `dilla-MacBook` session via SendMessage. Each brief names the branch `feat/mobile-apps`, the commit, the commands and the report expected back.
- **Before pushing:** `npm run ci:local` must pass.

## Review Focus
1. **Restarting the app keeps you signed in.** If OPFS or IndexedDB is evicted between launches, the user silently loses their device. T1 checks persistence after a force-quit, and T6's emulator e2e relaunches the app and asserts the same account appears.
2. **A second instance can't read the first one's data.** T6's test adds two instances and asserts the second's worker is configured with its own URL and never the first's.
3. **An instance URL typed with a path, http:, or trailing junk is rejected before anything is stored** (desktop rule: `canonicalInstance` in `packages/desktop/src/policy.ts`). T6's picker test covers `http://x`, `https://x/path`, `https://x?q` and `x` with no scheme.
4. **A page served by some other app on `https://localhost` is refused by the server for anything beyond bearer-authed CORS.** T4 asserts `Access-Control-Allow-Credentials` is never sent and that WebAuthn RP origins don't include mobile origins.
5. **The phone layout always applies in the app, even on a tablet-width screen.** T5 asserts `phoneNow()` is true whenever the mobile bridge is present, whatever the width.

---

## M0: device spike (gate)

### Task 1: WebView capability probe app

**Files:**
- Create: `experiments/mobile-spike/` (a throwaway Capacitor 7 app: `package.json`, `capacitor.config.ts`, `index.html`, `probe.ts`, `vite.config.ts`, `ios/`, `android/`)
- Create: `experiments/mobile-spike/README.md` (how to run it on the emulator and on the iPhone)

**Interfaces:**
- Produces: a probe page that renders and `console.log`s one JSON line `DILLA_PROBE {"opfsSyncHandle":bool,"opfsPersisted":bool,"webLocks":bool,"encodedTransform":"script-transform"|"encoded-streams"|null,"audioWorklet":bool,"rnnoise":bool,"storagePersist":bool,"ua":string}`

- [ ] **Step 1: Install the Android SDK in user space (Linux).** Install cmdline-tools into `~/Android/Sdk`, then `sdkmanager "platform-tools" "emulator" "platforms;android-35" "system-images;android-35;google_apis;x86_64"`, then `avdmanager create avd -n dilla35 -k "system-images;android-35;google_apis;x86_64"`. Expected: `emulator -avd dilla35 -no-window` boots and `adb devices` lists it. `/dev/kvm` is present.
- [ ] **Step 2: Write `probe.ts`.**
  - It reuses the real checks instead of re-implementing them:
    - the core-wasm OPFS probe through `@dilla/client-core` runtime readiness (`packages/client-core/src/worker/runtime-readiness.ts`) in a module Worker
    - `navigator.locks.request`
    - `packages/media/src/support.ts` for the encoded-transform kind
    - loading the RNNoise worklet from `packages/media/src/audio/rnnoise.ts`
    - `navigator.storage.persist()`
  - `opfsPersisted` writes a marker file on the first launch and reads it on the next.
- [ ] **Step 3: Run on the Android emulator.** Run `npx cap run android --target dilla35`, read the `DILLA_PROBE` line from `adb logcat`, force-stop, relaunch and read it again. Expected: both lines are recorded and the second has `opfsPersisted: true`.
- [ ] **Step 4: Send the iOS run to `dilla-MacBook`.** It builds `experiments/mobile-spike` in Xcode, runs it on Jonas's iPhone, force-quits, relaunches, and returns both `DILLA_PROBE` lines from the Xcode console.
- [ ] **Step 5: Commit** `experiments/mobile-spike` with the message `spike(mobile): WebView capability probe`.

### Task 2: Background call-audio probe

**Files:**
- Modify: `experiments/mobile-spike/` (add `call-probe.ts`, plus the iOS `Info.plist` `UIBackgroundModes` entries `audio` and `voip`, and a tiny CallKit start-call in `ios/App/App/CallProbe.swift`)

- [ ] **Step 1: Write `call-probe.ts`.**
  - It sets up a loopback of two `RTCPeerConnection`s with the mic track.
  - The sender has an encoded transform (`RTCRtpScriptTransform` on WebKit, `createEncodedStreams` on Chromium) that XORs the payload, and the receiver reverses it.
  - It logs `DILLA_CALL {"t":seconds,"framesOut":n,"framesIn":n}` every 10 s.
- [ ] **Step 2: Android emulator.** Start the probe, press Home, wait 5 minutes, and check that the logcat frame counters kept rising.
- [ ] **Step 3: Send the iPhone run to `dilla-MacBook`.**
  - It starts a CallKit outgoing call (`CXStartCallAction`), activates `AVAudioSession` `.playAndRecord`/`.voiceChat` in `provider(_:didActivate:)`, and then starts the probe.
  - Then it locks the phone for 5 minutes.
  - It reports whether `framesIn` kept rising while locked, and whether audio came back after unlock.
- [ ] **Step 4: Commit** with the message `spike(mobile): background call-audio probe`.

### Task 3: Spike report and go/no-go

**Files:**
- Create: `docs/spikes/2026-11-mobile-webview.md`

- [ ] **Step 1: Write the report.** It has a table with one row per probe field and the T2 result, with columns iOS (device + version) | Android emulator (API + WebView version), and a go/no-go per row.
- [ ] **Step 2: Apply the gate.**
  - **No-go:** `opfsSyncHandle`, `opfsPersisted` or `encodedTransform` fails on either platform, or iOS loses call frames while locked. Stop and put the options to Jonas as AskUserQuestion prompts.
  - **Go:** continue with M1.
- [ ] **Step 3: Commit** with the message `docs(spikes): mobile WebView go/no-go`.

---

## M1: shells

### Task 4: Server accepts the mobile app origins (opus)

**Files:**
- Modify: `internal/config/config.go` (add `MobileOrigins []string \`toml:"mobile_origins"\`` to `HTTP`)
- Modify: `internal/config/defaults.go:23`. The default is `MobileOrigins: []string{"capacitor://localhost", "https://localhost"}`. **Do not** append these to `Auth.WebAuthn.RPOrigins` (line 144).
- Create: `internal/api/cors.go` (`func CORS(origins []string) func(http.Handler) http.Handler`)
- Modify: `internal/dillad/server.go:358` (pass `TrustedOrigins ∪ MobileOrigins` to the gateway, and wrap the `/v1/`, `/i/` and `/desktop-auth/` routes with `CORS`)
- Test: `internal/api/cors_test.go`, `internal/config/defaults_test.go`

**Interfaces:**
- Produces: instances accept requests from the mobile origins. The client needs nothing new.

- [ ] **Step 1: Write the failing tests.** Each is a table test against a handler wrapped with `CORS([]string{"capacitor://localhost","https://localhost"})`:
  - `TestCORSPreflightAllowedOrigin`: `OPTIONS` from `capacitor://localhost` with `Access-Control-Request-Method: POST` and `Access-Control-Request-Headers: authorization, content-type` returns 204 with `Access-Control-Allow-Origin: capacitor://localhost`, `Access-Control-Allow-Headers` containing `authorization`, `Access-Control-Max-Age: 600` and `Vary: Origin`. The header `Access-Control-Allow-Credentials` must be absent.
  - `TestCORSUnknownOrigin`: `Origin: https://evil.example` gets no `Access-Control-Allow-*` headers, and the request still reaches the handler (CORS is advisory and auth stays bearer).
  - `TestCORSNoOrigin`: same-origin requests are unchanged.
  - `TestMobileOriginsNotWebAuthn`: after `ApplyDefaults`, `Auth.WebAuthn.RPOrigins` contains neither mobile origin.
- [ ] **Step 2: Run them.** `go test ./internal/api/ -run TestCORS -v` and `go test ./internal/config/ -run TestMobileOrigins -v`. Expected: FAIL (undefined `CORS` / `MobileOrigins`).
- [ ] **Step 3: Implement `CORS`, the config field and the wiring.** Expose `Authorization`, `Content-Type` and the request headers the client already sends. Allowed methods are `GET, POST, PUT, PATCH, DELETE`.
- [ ] **Step 4: Gateway test.** In `internal/dillad/dilladtest`, a WebSocket dial to `/gateway` with `Origin: capacitor://localhost` succeeds and one with `Origin: https://evil.example` fails. Run `go test ./internal/dillad/... -run Mobile -v`. Expected: PASS.
- [ ] **Step 5: Commit** with the message `feat(server): accept the mobile app origins for CORS and the gateway`.

### Task 5: Web client knows the mobile bridge

**Files:**
- Create: `packages/mobile/src/contract.ts` (types only)
- Create: `packages/web/src/mobile.ts`
- Modify: `packages/web/src/main.tsx:17-21` (instance URL and `data-mobile`), `packages/web/src/layout.ts:12-19` (phone always on mobile), `packages/web/src/notify.ts` (foreground notifications through the bridge), `packages/web/src/webauthn.ts` + `screens/SignIn.tsx` (use `mobileBridge()?.browserAuth` where `desktopBridge()?.browserAuth` is used today)
- Test: `packages/web/src/mobile.test.ts`, `packages/web/src/layout.test.ts`

**Interfaces:**
- Produces, in `packages/mobile/src/contract.ts`:

```ts
export interface MobileInfo { platform: 'ios' | 'android'; version: string; instance: string }
export type MobileCommand =
  | { type: 'badge'; count: number }
  | { type: 'notify'; id: string; title: string; body: string }
  | { type: 'dismiss-notification'; id: string }
  | { type: 'haptic'; kind: 'light' | 'medium' }
  | { type: 'switch-instance' };
export type MobileEvent = { type: 'notification-click'; id: string } | { type: 'resume' } | { type: 'pause' };
export interface MobileBridge {
  info(): Promise<MobileInfo>;
  send(command: MobileCommand): Promise<void>;
  onEvent(listener: (event: MobileEvent) => void): () => void;
  browserAuth(request: import('../../desktop/src/browser-auth.ts').BrowserAuthRequest): Promise<import('../../desktop/src/browser-auth.ts').BrowserAuthResult>;
}
```

- Produces, in `packages/web/src/mobile.ts`: `export function mobileBridge(): MobileBridge | undefined`, which reads `globalThis.dillaMobile`.
- **Builder's call:** this is a separate `dillaMobile` global, not a shared `DesktopBridge`. `layout.ts` and about 20 call sites treat `desktopBridge()` as "desktop window". Reusing the global would turn off the phone shell.

- [ ] **Step 1: Write the failing tests.**
  - `mobile.test.ts`: `mobileBridge()` is `undefined` without the global and returns the object when `globalThis.dillaMobile` is set.
  - `layout.test.ts › phone layout is forced in the mobile app`: with `dillaMobile` set and `matchMedia` reporting a 1024px width, `phoneNow()` is `true`.
  - `notify.test.ts › mobile notifications go through the bridge`: a `notify` command is sent with the same title/body truncation as desktop (256/4096).
- [ ] **Step 2: Run them.** `npm test -w @dilla/web -- mobile layout notify`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - `main.tsx`: `const mobile = await mobileBridge()?.info();` sets `data-mobile` / `data-mobile-platform`, and `createCoreWorker` takes `{ instanceURL: (desktop ?? mobile).instance }` when either is present. The update watcher is a no-op on mobile, as on desktop.
  - `layout.ts`: when `mobileBridge()` is present, `snapshot` returns `true` and `subscribe` is a no-op. The AFK `exempt` path in `Shell.tsx` follows from `phone`.
- [ ] **Step 4: Run the tests again.** Expected: PASS. Then run `npm run typecheck -w @dilla/web`.
- [ ] **Step 5: Commit** with the message `feat(web): run inside the mobile shell`.

### Task 6: `packages/mobile` Capacitor shell

**Files:**
- Create: `packages/mobile/package.json` (`@dilla/mobile`; scripts `build`, `sync`, `test`, `typecheck`, `android:run`, `ios:open`)
- Create: `packages/mobile/capacitor.config.ts` (`appId: 'chat.dilla.app'`, `appName: 'Dilla'`, `webDir: 'dist'`, `android.allowMixedContent: false`; no `server.url`)
- Create: `packages/mobile/scripts/build.mjs`. It builds `@dilla/web` and copies `packages/web/dist` to `dist/`, the same way `packages/desktop/scripts/build.mjs` does. It then injects `<meta http-equiv="Content-Security-Policy">` (the mobile CSP), prepends `<script src="mobile-bridge.js"></script>` (classic, synchronous) before the module entry, and builds `src/bridge.ts` → `dist/mobile-bridge.js` and `src/picker.ts` → `dist/picker.js`. Last, it runs `scripts/write-manifest.mjs dist`.
- Create: `packages/mobile/src/bridge.ts`. It installs a frozen `window.dillaMobile` that implements `MobileBridge`:
  - It reads the current instance from `@capacitor/preferences` (`instances`, `current`). If there is none, it redirects to `picker.html`.
  - It maps `send` to the `local-notifications`, `haptics` and `app` plugins, and `switch-instance` to `picker.html`.
  - It maps `App` `pause`/`resume` to events.
- Create: `packages/mobile/src/browser-auth.ts`. This ports `packages/desktop/src/browser-auth.ts` to WebCrypto (`crypto.subtle.digest`, `crypto.getRandomValues`) and opens the system browser with `@capacitor/browser`. The flow, limits, timeouts and error strings are unchanged.
- Create: `packages/mobile/src/picker.html`, `src/picker.ts`. This ports `packages/desktop/src/manager.ts` (list, add and connect), validating with a copy of `canonicalInstance` from `packages/desktop/src/policy.ts`.
- Create: `packages/mobile/ios/`, `packages/mobile/android/` (from `npx cap add`, then committed). On iOS the bundle ID is `chat.dilla.app`, the deployment target 17.0, and `NSMicrophoneUsageDescription` / `NSCameraUsageDescription` are set. On Android `minSdk` is 29 and the app has `RECORD_AUDIO`, `MODIFY_AUDIO_SETTINGS`, `INTERNET` and `POST_NOTIFICATIONS`.
- Test: `packages/mobile/test/browser-auth.test.ts` (port the desktop cases), `packages/mobile/test/picker.test.ts`, `packages/mobile/test/build.test.mjs`, `packages/mobile/e2e/android-smoke.mjs`
- Modify: `package.json` workspaces, and `scripts/ci-local` (or whatever `npm run ci:local` calls) so it runs `npm test -w @dilla/mobile` and `npm run typecheck -w @dilla/mobile`

**Interfaces:**
- Consumes: `MobileBridge` (T5), and T4's `mobile_origins`
- Produces: `npm run build -w @dilla/mobile && npx cap sync`, which gives runnable iOS and Android projects

- [ ] **Step 1: Write the failing tests.**
  - `browser-auth.test.ts`: the desktop cases ported 1:1 (invalid request shape, 404 → `desktop-auth-unsupported`, 429 → `desktop-auth-rate-limited`, oversize reply, 2-minute expiry, verifier never in a URL).
  - `picker.test.ts`: `https://chat.example` is accepted and stored once (a duplicate add is a no-op). `http://x`, `https://x/path`, `https://x?q` and `x` are each rejected with the desktop copy "Could not connect. Enter a complete HTTPS instance address, then try again." Selecting the second instance sets `current` to it.
  - `build.test.mjs`: after the build, `dist/index.html` has the mobile CSP meta and loads `mobile-bridge.js` before the first `type="module"` script, every file under `dist/` is in `dilla-manifest.json` with a matching sha256, and `capacitor.config.ts` has no `server.url`.
- [ ] **Step 2: Run them.** `npm test -w @dilla/mobile`. Expected: FAIL.
- [ ] **Step 3: Implement** the files above.
- [ ] **Step 4: Run them again.** `npm test -w @dilla/mobile && npm run typecheck -w @dilla/mobile`. Expected: PASS.
- [ ] **Step 5: Android emulator e2e (`e2e/android-smoke.mjs`, driven through `adb` and the WebView devtools socket with Playwright `chromium.connectOverCDP`).** It runs against a local `dillad` dev instance (`dilladtest` host or `npm run dev:server`) reachable from the emulator at `https://10.0.2.2:<port>`, with the dev CA installed on the AVD. The run:
  1. adds the instance
  2. signs in two seeded accounts on two instances
  3. sends a message from the second account and sees it arrive on the device
  4. sends the app to the background for 120 s, resumes, and checks that a message sent meanwhile appears
  5. force-stops and relaunches, then asserts the same account is still signed in and both instances are listed

  Expected: the script exits 0.
- [ ] **Step 6: Send the iOS check to `dilla-MacBook`.** It runs `npm run build -w @dilla/mobile && npx cap sync ios`, then builds and runs on the iPhone against `https://dilla.chat` (CT 144 must be on a build that includes T4). It goes through the same manual steps as Step 5 and returns screenshots of the picker, the signed-in phone shell and a received message.
- [ ] **Step 7: Commit** with the message `feat(mobile): Capacitor shell for iOS and Android`.

### Task 7: Mobile CI lane

**Files:**
- Create: `.github/workflows/mobile.yml`. It has path triggers on `packages/mobile/**`, `packages/web/**`, `packages/client-core/**`, `packages/ui/**`, `packages/media/**` and `core/**`, and these jobs:
  - `mobile-unit`: test + typecheck on ubuntu
  - `mobile-android`: build the debug APK with `./gradlew assembleDebug` on ubuntu, then run the emulator smoke from T6 Step 5 with `reactivecircus`-free plain `sdkmanager`/`emulator` steps, matching the no-third-party-actions policy
  - `mobile-ios`: `xcodebuild -scheme App -sdk iphonesimulator -destination 'generic/platform=iOS Simulator' build` on macOS
  - `mobile-ok`: an aggregator
- Modify: `scripts/check-ci*` if it lints workflow shape (it requires `if-no-files-found: error` on evidence uploads)

- [ ] **Step 1:** Write the workflow following the structure of `.github/workflows/desktop-ci.yml`, including the electron-style cache and retry patterns for the Gradle and SDK downloads.
- [ ] **Step 2:** Run `npm run ci:local`. Expected: green, including the new mobile-unit steps.
- [ ] **Step 3: Commit, push, and open a draft PR.** Fix the first failing job immediately (fail-fast). Expected: `mobile-ok` is green. Ask Jonas before adding `mobile-ok` to the ruleset's required checks.

## Deviations from the spec (builder's calls, flagged for Jonas)
- **Separate `dillaMobile` global** instead of a shared `platform-contract` package (reason in T5).
- **No native KEK-wrapping plugin in M1.** The KEK's wrap key is a non-extractable WebCrypto key in IndexedDB, which sits next to the OPFS store. Clearing one clears the other, so a Keychain wrap would not prevent stranding. iOS app-container data protection already gives the "after first unlock" property Jonas chose. A Keychain/Keystore item is still needed in M2 for the push `wrap_key` that the notification extension reads, and it is built there.
