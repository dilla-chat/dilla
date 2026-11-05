# SP-M0 Mobile WebView: GO for Capacitor on both platforms; iOS calls run without CallKit in v1

Date: 2026-11-05. Spec: `docs/superpowers/specs/2026-11-05-dilla-mobile-design.md` (see "iOS calls after the M0 device
spike"). Plan: `docs/superpowers/plans/2026-11-05-dilla-mobile-m0-m1.md`, tasks 1-3. Spike code:
`experiments/mobile-spike` on `feat/mobile-apps`. Every number below comes from the spike's own `DILLA_PROBE`,
`DILLA_CALL` and `DILLA_CALL_DETAIL` lines, `dumpsys audio`, or the device logs reported by the Mac session.

## Question

Can a Capacitor shell (system WebView, no browser chrome) run dilla's web client on Android and iOS? The gate:
`opfsSyncHandle`, `opfsPersisted` and `encodedTransform` must pass on both platforms, and iOS must not lose call
frames while the screen is locked. A fail on any of these stops M1 and goes to Jonas.

## Setup

| Target | Detail |
|---|---|
| Android emulator | AVD `dilla35`, `google_apis;x86_64`, API 35 (Android 15, build AE3A.240806.043), KVM, WebView Chrome 124.0.6367.219 (bundled; the image has no Play Store, so it cannot update). Origin `https://localhost`. |
| iOS simulator | iOS 27.2, Xcode 27.2. Origin `capacitor://localhost`. |
| iPhone | iPhone 17 Pro, iOS 27.0.1, Xcode 27.2, dev-signed (team 456U2Z5LYK). |
| Spike app | Capacitor 7.6.10, Vite 7.3.7, appId `chat.dilla.spike`. Real repo code imported by path: `localRuntimeReadiness` over `@dilla/core-wasm` `probe_persistence` (module Worker), `isVoiceSupported()`, `DillaRnnoiseProcessor`. |

Spike commits on `feat/mobile-apps` (`experiments/mobile-spike`), oldest first:

| SHA | What |
|---|---|
| `855f228e` | capability probe (`DILLA_PROBE`) |
| `23e26c9f` | a tap run is no persistence evidence; Mac prerequisites |
| `dad234ae` | background call-audio probe (loopback with an XOR "encoded transform") |
| `6fb53e7d`, `f44726f6` | frame-integrity counters, RTP stats, iPhone run without debugger |
| `1dfe3c88` | iOS 27 SDK build: iOS 17 target, UIScene lifecycle |
| `5d1706e2` | per-step call tracing, three start orders (no-callkit, gum-first, callkit-first) |
| `a625e8ce` | force-mic option, no-callkit+session mode, mic/RTP energy evidence |
| `8b86d962` | WKWebView capture-state control and KVO, re-gUM option |

## Capability probe

`DILLA_PROBE` runs on launch. `opfsPersisted` is measured by a marker file written with `createSyncAccessHandle`,
then read after the app process was killed and relaunched (Android `am force-stop`, iOS SIGTERM). A "Run again" tap
in the same process reports `false` by design and is not evidence.

| Field | Android emulator (API 35, WebView 124) | iOS simulator (27.2) | iPhone (17 Pro, 27.0.1) | Verdict |
|---|---|---|---|---|
| `isSecureContext` | true (`https://localhost`) | true (`capacitor://localhost`) | true | go |
| `opfsSyncHandle` | true | true | true | go (gate) |
| `opfsPersisted` (after kill + relaunch) | true (false on first launch, as expected) | true | true | go (gate) |
| `webLocks` | true | true | true | go |
| `encodedTransform` | `encoded-streams` (`createEncodedStreams`) | script transform (`RTCRtpScriptTransform`) | script transform | go (gate) |
| `audioWorklet` | true | true | true | go |
| `rnnoise` (worklet loaded, wasm compiled, no gesture, no mic permission) | true | true | true | go |
| `storagePersist` | false | false | false | not a gate |

`storagePersist` is `navigator.storage.persist()`. It is false on all three, and that does not matter here. It is a
browser-profile eviction guard, and an app's WebView data lives in the app's private storage, which the OS does not
evict like a browser profile. The persistence that matters is `opfsPersisted`, which passes after a real process
kill on all three.

Notes from the probe runs:

- The platforms ran different spike builds: Android at the `855f228e`/`23e26c9f` era, iOS at `1dfe3c88` and later. `voiceSupport` picks `insertable-streams` on Chromium and the
  script-transform path on WebKit, the same split `packages/media/src/support.ts` already makes.
- The simulator run was labelled SIMULATOR; the device run repeated every field.
- The iPhone's first build needs `-allowProvisioningDeviceRegistration`.
- A stale CallKit `didDeactivate` was seen at launch on the device (cause unconfirmed; the probe now ends stale
  calls).

## Calls

The call probe is a two-`RTCPeerConnection` loopback with one Opus track (DTX stripped, so frames flow on silence).
The sender's encoded transform XORs each frame after prepending a magic `d1 1a`; the receiver XORs back, checks and
strips the magic, and counts `framesOk`/`framesBad`. `framesOut` is the sender's frame count; 50 per second is the
expected rate.

### Android emulator, app backgrounded for 5 minutes, no foreground service

Two runs (Home at t of about 45 s, foreground at t of about 365 s in run 1; run 1 ended at t=387 s; run 2 has no t value for these points).

- Frames kept flowing at the full rate the whole time: +500 `framesOut` per 10 s, in the background too.
- The process stayed alive (same PID throughout), JS timers were not throttled, the peer connection stayed
  `connected`, `packetsLost` was 0, `concealedSamples` stayed at the startup value of 11964.
- **The mic was silenced.** `dumpsys audio` showed the app's recording session as `silenced:true` at every
  one-minute sample while backgrounded, and `silenced:false` after the app returned to the foreground. The receiver's
  `audioLevel` dropped from about 0.00046 to 0.0000305 (1/32768, digital zero) while hidden and came back after.
- Conclusion: the pipeline survives, but a backgrounded call sends dead air. M3 needs a foreground service of type
  microphone plus ConnectionService.
- Limits: an emulator has no Doze, no App Standby buckets in 5 minutes, no OEM battery killers and no memory
  pressure. "Process alive, timers not throttled" is weak evidence for real phones. The mic silencing is platform
  policy and is firm. The virtual mic's level was near 0 even in the foreground, so the evidence is `dumpsys`, not
  the page.

### iPhone with CallKit: WKWebView capture gets no samples

Tried on the device with the force-mic option and four start orders. Under an active CallKit call WebKit mutes or
starves page capture, in every variant:

| Variant | Result |
|---|---|
| `callkit-first` (CallKit start and `didActivate`, then getUserMedia) | track `muted:true` from the start; tone fallback frames flowed, mic carried nothing; Jonas heard nothing |
| `gum-first` (getUserMedia, then CallKit) | `mic.mute` at `didActivate`; AudioContext `interrupted` then `suspended`; 0 frames |
| `callkit-first` + `WKWebView.setMicrophoneCaptureState(.active)` | KVO and the track report `active`/unmuted, but `totalSamplesDuration` stays 0 |
| `callkit-first` + a fresh getUserMedia after `didActivate` | the new track is born muted |

Other CallKit observations: the session category was `SoloAmbient` until the plugin set `playAndRecord`/`voiceChat`;
the output route flipped from Receiver to Speaker about 3 s after `didActivate` and recurred; WebKit unmutes the mic
track when the app goes to the background (it is muted in the foreground under CallKit); in a roughly 76 s
lock run (callkit-first, tone source) `framesOut` reached 4497 at t=92 (97.8% of 50/s) and the CallKit pill showed on
the lock screen, so frames survive the lock but carry no mic. Conclusion: CallKit plus a WKWebView mic is not
workable on iOS 27.0.1. The only way to keep CallKit is a native audio path.

### iPhone without CallKit: works, including 5 minutes locked

The app's own native plugin sets `AVAudioSession` to `playAndRecord`/`voiceChat` and activates it before
getUserMedia. `Info.plist` had `UIBackgroundModes` `audio` **and** `voip` (no PushKit), and the 388 s lock run below was measured with both set. `audio` alone, the planned v1 configuration, is **untested** and must be verified on a device in wave M3.

- Foreground, no CallKit: the mic works (Jonas heard himself; `micPeak` 1). An earlier 0 was a warm-up artifact in
  the first 1.5 s, not a capture failure. WebKit captures out of process, and the app session itself stays
  `SoloAmbient` unless the plugin sets it.
- A roughly 100 s lock run showed a live mic with the orange mic indicator on the lock screen, and the loopback was
  heard.
- **Strict 5-minute-plus lock run: about 388 s continuous.** Sent and received sample durations were within 0.7 s of
  each other, concealed samples were under 0.72 s, packets lost were 0, and the AudioContext recovered after the
  unlock.
- The AudioContext goes `interrupted` while the app is hidden, so the analyser cannot measure, and sometimes recovers
  later. `<audio>` playback keeps going. dilla's AudioWorklet chain (`input-stage.ts`, RNNoise, DeepFilter) must be
  bypassed while hidden: swap the sender to the raw mic track on hide and back on show, and play remote audio
  through `<audio>`, not the AudioContext.
- Cost of no CallKit: no full-screen lock-screen ringing and no green call pill. Rings are time-sensitive
  notifications. No PushKit.

## Other findings

- **0-byte RTP payloads bypass the send transform** on Chromium (WebView 124): about 0.6 packets/s (3 in the first
  10 s, then 6 per 10 s). `packetsSent == packetsReceived == framesIn == framesOut + framesBad`, `framesOk ==
  framesOut` exactly, and `firstBadFrame` was `{"bytes":0}`. They carry no audio, so nothing leaks. The real receive
  transform must pass empty unmarked frames and keep rejecting non-empty ones that lack the header. (Matches the
  empty-frame handling already in SP-09.)
- **iOS 27 SDK requires the UIScene lifecycle.** The Capacitor 7.6.10 template is window-based and the spike crashed
  at launch. Capacitor 8.5.0 (2026-07-31) is the first with `SceneDelegateProxy` and the scene template; 8.0.0-8.4.3
  have none. 8.6.0 is the latest (2026-10-28). M1 uses Capacitor 8.6.x. 9.0.0-beta.0 moves to SwiftUI and an iOS 16
  floor and is not used. Keep one root-controller source (storyboard or code), and drop the `armv7` capability.
- **Deployment target 17** (plan floor iOS 17; Xcode 27 needs 15 or higher).
- **"JS Eval error" at every launch** (simulator and device) was Capacitor 7.6.10 evaluating JavaScript before the
  WebView loaded (resume-before-load; this cause is reasoning, not a measurement). It is gone in 8.6 per the M1 shell work; still to be confirmed on a device build.
- **The user agent is frozen at iOS 18_7** on iOS 27. Never detect the OS from the UA.
- **WebKit's 7-day ITP script-storage cap** is a Safari/ITP policy and does not apply to an app's WKWebView.
- **First device build** needs `xcodebuild -allowProvisioningDeviceRegistration`.
- **Xcode 27 has Device Hub instead of Simulator.app**; README and docs say "Device Hub", not "Simulator".
- Spike hygiene: Xcode's swiftpm `Package.resolved` is not ignored by the Capacitor template; the root `npm install`
  under npm 11 rewrites `package-lock.json`.

## Decision

**GO.** Every gate item passes on all three targets: `opfsSyncHandle`, `opfsPersisted` and `encodedTransform` pass
on the Android emulator, the iOS simulator and the iPhone, and iOS did not lose call frames while locked in the mode
we will ship.

- M1: Capacitor 8.6.x shells on both platforms, deployment target iOS 17.
- iOS calls: no CallKit in v1 (Jonas, 2026-11-05, via AskUserQuestion, re-confirmed "no call screen in v1"). App-owned
  `playAndRecord`/`voiceChat` session with `UIBackgroundModes: audio` (the spike measured `audio` + `voip`; `audio` alone is untested, verify in M3), rings as time-sensitive pushes, no PushKit.
  CallKit returns only with a native audio path (native WebRTC with dilla's frame encryption), a later project.
- M3: Android foreground service of type microphone plus ConnectionService; iOS swaps the sender to the raw mic while
  hidden; the receive transform passes empty frames.
- Not shown by this spike: a physical Android phone (Doze, OEM killers, a current WebView), and the iPhone
  cellular-interruption path.
