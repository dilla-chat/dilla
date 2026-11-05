# dilla mobile design: Android and iPhone

Status: draft for review, 2026-11-05. Branch `feat/mobile-apps`.

This supersedes the mobile line of `2026-09-23-dilla-design.md` (line 679, which planned Android/iOS through UniFFI). Mobile v1 is a Capacitor shell instead. The native UniFFI core is deferred to the later native-custody project.

## Context
Jonas wants dilla on Android and iPhone. Today there is the React web client (`packages/web`), the WASM core (`packages/core-wasm`) and an Electron desktop shell (`packages/desktop`, beta.2) that wraps them. The design spec of 2026-09-23 planned mobile as phase 2, using UniFFI (line 679). Jonas has now chosen a **Capacitor shell** instead, because it reuses the web UI and the existing 390px phone shell, and because the WebView's `RTCRtpScriptTransform` keeps SFrame call encryption working. That avoids the blocker the spec recorded: LiveKit's native SDKs offer no hook for a custom frame encryptor.

### Decisions Jonas made (2026-11-05)
- **Approach:** a Capacitor shell for both iOS and Android, wrapping `@dilla/web` and the WASM core.
- **Audience:** the friend group first. iPhone users get TestFlight on Jonas's personal paid Apple account; Android users get a sideloaded, signed APK. Public stores wait for the legal entity and trademark gates.
- **Push:** included in v1. A project-run, content-blind relay sends to APNs (iOS) and **FCM only** (Android).
- **Notification text:** shows who and where, such as "Alice in #general" or "Alice is calling". The instance encrypts the payload per device, and a native extension decrypts it. Message bodies are never in a push.
- **Calls:** full support. iOS uses CallKit + PushKit, Android uses ConnectionService and a foreground service, and calls keep running in the background and with the screen locked.
- **Custody:** browser tier, the same as desktop today. Native tier (UniFFI plus Keychain/Keystore) stays a later project that covers desktop and mobile together.
- **Tooling:** Jonas has a Mac, a paid Apple Developer account and CI macOS runners. **There is no Android phone**, so Android is tested on emulators locally and in CI, and a friend's phone does the final smoke test.

### Decisions after the builder check (2026-11-05)
- **Mute filtering happens on the phone.** The push payload carries ids only. Native code (the iOS NSE and the Android `FirebaseMessagingService`) decrypts it and drops it when the channel or community is muted or the mode excludes it, using an on-device copy of the notify settings that the app writes to the App Group / app storage. The server never learns mutes, which keeps the web3.8 mute privacy. iOS needs Apple's `com.apple.developer.usernotifications.filtering` entitlement, which must be requested **now** (task M2.0). Until Apple grants it, iOS shows muted pushes too. Mentions-only mode can't be decided from metadata, so it is treated as "all" in push and refined in the foreground.
- **Calls can be answered while locked.** The Keychain/Keystore key that wraps the KEK uses `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly` on iOS, and on Android a Keystore key with no user-auth binding. That lets a killed or rebooted-then-unlocked-once phone boot the WebView and answer from CallKit.
- **The relay accepts a static allowlist.** `cmd/dilla-push` config lists the Ed25519 instance public keys it accepts (CT 144 plus whatever Jonas adds). Requests are signed by the instance and rate-limited per key. Open enrolment is out of scope.
- **Android updates through an in-app prompt.** The app reads the GitHub release feed and checks the pinned signing key and the APK sha256 (trust pinned like `packages/desktop/release-trust.json`). It then prompts the user and installs through `PackageInstaller` with `REQUEST_INSTALL_PACKAGES`. iOS updates come through TestFlight.
- **Releases are lockstep.** Every server release tag also cuts the mobile builds (TestFlight upload and signed APK) from the same commit. There is no minimum-version gate in v1.
- **App ID is `chat.dilla.app`** on both platforms, from Jonas's domain dilla.chat. The ID only names the app. Users can sign in to any dilla server on any domain. It fixes the APNs topic (`chat.dilla.app`, `chat.dilla.app.voip`), the App Group `group.chat.dilla.app`, the Keychain access group and the Firebase project.
- **Multiple instances, matching desktop.** One install can sign in to several instances. The bundled CSP uses `connect-src https: wss:`, and the push registration, name cache and mute copy are kept per instance. Deep links and push payloads carry an instance id for routing.
- **Builder's calls:**
  - Login reuses the desktop poll flow (`/desktop-auth/.../consume`), so no `dilla://` callback is needed. `dilla://` stays for invites only.
  - DM pushes omit `community_id`.
  - "Is calling" pushes exist only for DM rings (`internal/api/ring.go`). Voice-channel activity sends no push.
  - The `wrap_key` is 32 random bytes made on the device and sent once over the authenticated device session at push registration. It is rotated on each re-registration.

## Process
1. Write `docs/superpowers/specs/2026-11-05-dilla-mobile-design.md` from this file. It records the decisions above, notes that it supersedes spec line 679, and covers the architecture below.
2. A builder-check subagent (opus) reviews it. Any questions only Jonas can answer go to him as AskUserQuestion prompts.
3. Write `docs/superpowers/plans/2026-11-05-dilla-mobile.md` with superpowers:writing-plans. It is built wave by wave by SDD subagents, with opus for push crypto, authz and key handling and sonnet for recipe tasks. Every agent runs `npm run ci:local` before pushing.

## Architecture

### Wave M0: device spike (gates everything else, a throwaway build)
This wave checks the WebView risks on a real iPhone (iOS 17+) and an Android emulator (API 34+ WebView):
- the OPFS `opfs-sahpool` store with `createSyncAccessHandle` in a module Worker, and whether it **persists across app restarts**
- Web Locks
- `RTCRtpScriptTransform` (WKWebView) and `createEncodedStreams` (Android) with the existing `packages/media/src/support.ts` gate
- AudioWorklet with RNNoise
- WKWebView keeping WebRTC audio alive in the background with `UIBackgroundModes: audio, voip` and a CallKit audio session

**Exit:** a written go/no-go per item. If OPFS persistence or encoded transforms fail, stop and bring Jonas the options before any more work.

### Wave M1: the shells (`packages/mobile`)
- Capacitor 7 project with `ios/` and `android/`. The build reuses the desktop pattern from `packages/desktop/scripts/build.mjs`: build `@dilla/web`, copy `dist`, and verify `dilla-manifest.json` with `scripts/write-manifest.mjs`.
- **Origin:** Capacitor serves from `capacitor://localhost` on iOS and `https://localhost` on Android, unlike desktop, which serves at each instance's own origin.
  - The server needs a strict CORS allowlist for those two origins on `/v1`, `/i`, `/gateway` and `/rtc`.
  - The web CSP `connect-src` must allow the chosen instance origin.
  - The per-instance OPFS layout `dilla/<instance hex>` already keeps instances apart.
- **Shared bridge contract:** move `packages/desktop/src/contract.ts` into a new `packages/platform-contract`. The web side moves from `desktopBridge()` in `packages/web/src/desktop.ts` to `platformBridge()`, which returns `{kind: 'desktop'|'ios'|'android', ...}`. The mobile bridge implements the subset it can: badge, notifications, theme, external-browser auth, deep links, haptics and safe areas.
- **Login:** reuse the `browser-auth.ts` PKCE-style flow through `@capacitor/browser` (ASWebAuthenticationSession / Custom Tabs) with a `dilla://` callback. Passkeys work in the system browser.
- **KEK:** `client-core/src/account/kek.ts` keeps the WebCrypto KEK in IndexedDB. On mobile, a native plugin wraps it with a Keychain key (iOS) or Keystore key (Android) so wiping WebView storage can't strand it. The device stays browser tier.
- **Phone layout:** `PHONE_QUERY` (`packages/web/src/layout.ts:10`) also matches on mobile. The native bridge forces phone mode and the AFK `exempt: true` path (`Shell.tsx:620`). The live app has three tabs (home/activity/you), which stays as is.
- **Lifecycle:** the iOS app is suspended in the background, so the gateway socket drops. On resume the app reconnects and syncs, and push covers the gap. The `pagehide` leave and the PTT `visibilitychange` release must not end a call while CallKit owns it.

### Wave M2: push (server, relay, clients)
- **Server (opus):**
  - New table `device_push(device_id PK → provider, token, wrap_key, created)` with `ON DELETE CASCADE`, in both the sqlite and postgres schemas plus sqlc queries in `internal/store/{sqlite,postgres}/queries/devices.sql`. The rows are cleared in `RevokeDevice` and `internal/ops/revoke_devices.go`.
  - Endpoint `PUT/DELETE /v1/devices/{id}/push`, authenticated by the device session.
  - Fan-out happens on op 19 `MessageCT` (to devices with no open gateway, mute filtering happens on the phone, see above), on op 53 `CallRing`, and on ring end (`internal/api/ring.go` today delivers only to live gateway sessions).
  - The payload is `{kind, community_id, channel_id, sender_id, call_id?}`, sealed with AES-GCM under the device's `wrap_key`. The relay sees only ciphertext plus the provider token.
- **Relay (`cmd/dilla-push`, Go):**
  - It is stateless and forwards `{provider, token, ciphertext, priority, collapse_id}` to APNs (token-based .p8, normal and VoIP topics) and FCM HTTP v1.
  - Instances authenticate with a per-instance signed request, and the relay rate-limits per instance.
  - It runs on Jonas's homelab with his APNs key. The relay URL is set in instance config.
- **iOS:**
  - A Notification Service Extension written in Swift with CryptoKit decrypts the payload and turns ids into names from an App Group cache that the app writes, then shows "Alice in #general". If decryption fails it shows "New activity".
  - PushKit VoIP pushes go straight to `CXProvider.reportNewIncomingCall`, which is required.
- **Android:** an FCM data message reaches a native `FirebaseMessagingService`, which decrypts the payload and posts a notification on a per-kind channel. Rings use a full-screen intent.
- **Tap:** the deep link opens the channel or call, the app syncs, and the existing `notify.ts` takes over in the foreground.

### Wave M3: background calls
- **iOS:**
  - The CallKit plugin maps answer, decline, end and mute to the existing call session (`packages/web/src/call/session.ts`).
  - `AVAudioSession` is set to `playAndRecord`/`voiceChat` and activated in CallKit's `didActivate` before WebRTC starts.
  - Outgoing calls are reported to CallKit as well.
- **Android:**
  - A self-managed `ConnectionService`, plus a foreground service of type `microphone|phoneCall` while in a call.
  - Audio focus and Bluetooth routing are handled.
- **Server:** check that the AFK sweep and the "away after 30 s without gateway" rule (`internal/api/afk.go`) don't cut a backgrounded phone that is in a call. iOS keeps a WKWebView with active WebRTC audio running in the background, and the M0 spike confirms that.

### Wave M4: release pipeline and docs
- **`mobile-ci.yml`:** typecheck, `cap sync` and an Android emulator e2e smoke (sign in, send a message, join a call) on Linux. An iOS simulator build runs on a macOS runner.
- **`mobile-release.yml`** runs on tag `mobile-v*`:
  - **iOS:** Fastlane, App Store Connect API key, TestFlight upload.
  - **Android:** sign the APK with a release keystore held in a GitHub environment, and publish it to the GitHub release with a sha256.
  - Keys and digests are pinned the same way as `packages/desktop/release-trust.json`.
- **Docs:** README "what works" and `docs/user/mobile.md` (install via TestFlight or APK, enabling notifications), plus phone screenshots taken on real devices at 390px. Jonas sees the shots before shipping.
- **Deploy:** after the release is verified, tell the Homelab session to upgrade CT 144 and deploy the push relay.

## Out of scope (v1)
- Native custody tier and UniFFI
- Public App Store / Play listing
- UnifiedPush
- Screen share from the phone
- Message previews in push notifications

## Verification
- **M0:** the spike report with a result for each item on a physical iPhone and an Android emulator.
- **Server:** Go tests for `device_push` CRUD, the revoke cascade, and the fan-out going only to devices without an open gateway. A test proves the relay never receives plaintext: it asserts the request body is an AES-GCM blob. There are authz tests on the push endpoint, and an opus security review of the push boundary.
- **Clients:** an emulator e2e on Android in CI. On a real iPhone, run the manual checklist:
  - sign in through the system browser
  - send and receive a message
  - lock the phone and get "Alice in #general"
  - get a call while the app is killed, see the CallKit UI, answer, and keep audio with the screen locked for 5 minutes
  - get no AFK cut during that call
- `npm run ci:local` is green before each push. Then fail-fast CI, PR, review and merge, following the existing per-wave flow.
