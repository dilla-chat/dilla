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

The iOS project is committed (Swift Package Manager, no CocoaPods), with deployment target iOS 17 and the UIScene
lifecycle (`SceneDelegate.swift`): Xcode 27's iOS 27 SDK rejects targets below 15 and refuses to launch apps without a
scene manifest. The Mac needs Xcode 16+ (App Store, then open it
once to install its components), Node 24+ and, on the default route (Path B), the wasm toolchain.

Mac prerequisites:

- Xcode 16 or newer, opened once; `xcode-select -p` should print `/Applications/Xcode.app/Contents/Developer`
  (else `sudo xcode-select -s /Applications/Xcode.app`).
- Node 24 or newer: `nvm install 24 && nvm use 24` (the repo's `engines` field requires it).

**Path B (default): build on the Mac.** Verified with Node 24.22.2 (nvm), Homebrew rustup + llvm + wasm-pack and
wasm-bindgen-cli 0.2.128.

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

Then, from the repo root: `npm install`; the `wasm-pack build` command under Prerequisites;
`cd experiments/mobile-spike && npm install && npm run build`. No-install mode skips `wasm-opt`, so the core wasm is
unoptimized (about 3.7 MB); that is fine for the probe.

**Path A (only when a copy channel to the Mac exists): copy a Linux-built `dist/`.** On the Linux host, which has the
wasm toolchain:

```sh
cd experiments/mobile-spike && npm install && npm run build
tar czf /tmp/mobile-spike-dist.tgz dist      # then copy it to the Mac (scp, AirDrop)
```

On the Mac, from a checkout of this branch:

1. `cd experiments/mobile-spike && npm install` (only the Capacitor CLI and Vite; no root `npm install` needed).
2. `tar xzf <path>/mobile-spike-dist.tgz` inside `experiments/mobile-spike`, so `dist/index.html` exists. Do not run
   `npm run build` (it needs the wasm toolchain).

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

## Call probe (`call.html`, Task 2)

A second page, reached by the "call probe" link under the capability probe's button (the `DILLA_PROBE` page and its
contract are unchanged). A start button (one of three modes, below) sets up a loopback of two `RTCPeerConnection`s carrying one audio track. The
sender's encoded transform prepends a 2-byte magic (`d1 1a`) and XORs the frame; the receiver XORs it back, checks and
strips the magic, and counts `framesOk` / `framesBad` (a frame without the magic passes through unchanged; the first one
is recorded as `firstBadFrame` in `DILLA_CALL_DETAIL` with its size and first bytes, on both paths). Paths:
`createEncodedStreams` on Chromium, `RTCRtpScriptTransform` (`call-transform-worker.ts`) on WebKit, picked by the
repo's `isVoiceSupported()`. The received audio plays through an `<audio>` element, as in a real call. Every 10 s:

```
DILLA_CALL {"mode":m,"run":n,"t":seconds,"framesOut":n,"framesIn":n,"framesOk":n,"framesBad":n,"packetsSent":n,"packetsReceived":n}
DILLA_CALL_DETAIL {...}     # not part of the contract
```

`t` is wall-clock seconds since the call started; `packetsSent` / `packetsReceived` are getStats outbound-rtp and
inbound-rtp. `DILLA_CALL_DETAIL` carries `source` (`mic` when the mic shows any signal in its first 1.5 s; otherwise
`oscillator (...)`, a quiet 440 Hz tone, with the mic still open), `micLevel`, `micTrack` (`readyState`, `muted`,
`enabled`), `visibility`, `audioContext`, `audioPaused`, `pcState`, `inboundRtp` (`audioLevel`, `concealedSamples`,
`totalSamplesReceived`, `packetsLost`) and `at`. Opus DTX is stripped, so 20 ms frames flow on silence too (about 50
per second each way, 500 per line). The page also lists every `DILLA_CALL` line it logged (newest first), so the
history can be read on screen. "End call" logs a final line and `DILLA_CALL_END`.

**Start modes** (buttons; use one per app launch, force-quit between them; the mode is in every line):

| mode | order |
| --- | --- |
| `no-callkit` | AudioContext created in the tap, `getUserMedia`, `resume()`, loopback. No CallKit: does the WebView pipeline work on the device at all? |
| `gum-first` | AudioContext created in the tap, `getUserMedia`, `resume()`, then CallKit `startCall`, then loopback |
| `callkit-first` | CallKit `startCall` in the tap; the AudioContext is created only after `didActivate` (no user gesture), then `getUserMedia`, `resume()`, loopback |
| `no-callkit+session` | Native `CallProbe.configureSession` in the tap (`.playAndRecord` / `.voiceChat`, `setActive(true)`, no CallKit); the AudioContext is created after that (no gesture), then `getUserMedia`, `resume()`, loopback. Does an app-configured session fix capture? |

**Force mic** (checkbox, on by default, applies to every mode): never fall back to the oscillator; the raw
`getUserMedia` track goes through the loopback. See "Force-mic run" below for the extra fields.

**Step tracing** (added after the 2026-11-05 iPhone hang: `startCall` resolved, then nothing). Every await in the
start path is a step with an 8 s timeout:

```
DILLA_CALL_STEP {"mode":m,"run":n,"step":name,"phase":"start|ok|err|timeout|late-ok|late-err","ms":n,"at":n,"detail":{ctx,mic,vis,focus,activation,info}}
DILLA_CALL_EVENT {"mode":m,"run":n,"event":name,"at":n,"info":...,ctx,mic,...}
```

Steps: `callkit.startCall`, `ctx.create`, `getUserMedia`, `ctx.resume`, `permissions.microphone`, `enumerateDevices`,
`native.status`, `analyser.setup`, `mic.sample`, `oscillator.setup`, `source`, `pc.create`, `worker.ready`, `addTrack`,
`transform.attach.send`, `createOffer`, `setLocalDescription(A offer)`, `setRemoteDescription(B offer)`,
`createAnswer`, `setLocalDescription(B answer)`, `setRemoteDescription(A answer)`, `ontrack`,
`transform.attach.receive`, `audio.play`, `ice`, `firstFrame.out` / `.in` / `.ok`, and on the way out `ctx.close`,
`callkit.endCall`. `ms` is the step's own duration, `at` the time since the tap; `detail` always carries the
AudioContext state, the mic track (`readyState`, `muted`, `enabled`), visibility and user activation. `timeout` names
the step that hung; `late-ok` / `late-err` mean it settled after the timeout (with how long it took). A timeout in
`getUserMedia`, `ctx.resume`, the mic sample, `ice` or the first-frame waits does not abort the run (the mic track is
used even if the AudioContext is not running; the level is then not measurable); any other step timeout aborts it.
Events: `ctx.statechange`, `mic.mute` / `unmute` / `ended`, PC A/B ICE, connection, signaling and gathering states,
`visibility`, `pagehide` / `pageshow`, `window.error`, `unhandledrejection`, and a `heartbeat` every 5 s until the first
`DILLA_CALL` line listing the steps still pending (if heartbeats stop, the page's JS thread itself is stuck).

Extra buttons: "End call" (any time; ends the run and every active CallKit call), "ctx.resume (tap)" (does a tap
unstick a resume that hung without one?), "status" (permissions, devices, native session state), "native mic
permission" (`AVAudioApplication.requestRecordPermission`). Every aborted run, "End call" and `pagehide` call the native
`endCall`, which ends every active call; `startCall` and plugin load also end a leftover call first (logged as
`DILLA_CALL_NATIVE ending stale call <uuid>`), because a never-ended call makes the next start fail with CallKit error 7
(maximumCallGroupsReached).

All page lines go through `console.log`; on iOS Capacitor's console bridge forwards them to native stdout as
`⚡️  [log] - DILLA_CALL_STEP {...}` (needs `loggingBehavior: 'production'` for Release builds, which the config has).
Native lines (`DILLA_CALL_NATIVE +<ms since plugin load>ms ...`) are NSLog'ed and also shown in the on-page step list.
They cover provider ready / `providerDidBegin` / `providerDidReset`, `perform CXStartCallAction`, `didActivate`,
`didDeactivate` (with our call, pending start, app state, CallKit's call list and the session), `callChanged`,
`AVAudioSession` interruption (began/ended, raw reason, options), route change (reason, previous and current route),
media services lost/reset, silence-secondary-audio hint, and app background/foreground.

A small `framesBad` is expected: Chromium (Android WebView 124, measured) sends about 0.6 RTP audio packets per second
with a 0-byte payload that never pass through the send transform, so they arrive without the magic
(`packetsSent` = `framesIn` = `framesOut` + `framesBad`). They carry no audio and so cannot leak anything. A
`firstBadFrame` with `"bytes":0` is this bypass; a non-empty one, or `framesOk` falling behind `framesOut`, is a real
transform failure.

On iOS, the `gum-first` and `callkit-first` modes call the native `CallProbe` plugin (`ios/App/App/CallProbe.swift`, registered by
`SpikeBridgeViewController`, which `Main.storyboard` uses): a CallKit `CXStartCallAction`, whose handler sets the
`AVAudioSession` to `.playAndRecord` / `.voiceChat` and fulfills; CallKit activates the session and the plugin resolves
in `provider(_:didActivate:)`; the page starts the loopback only after that. `Info.plist` has `UIBackgroundModes`
`audio` + `voip` and `NSMicrophoneUsageDescription`. Native events are logged as `DILLA_CALL_NATIVE ...`. On Android
there is no foreground service on purpose: the run measures the baseline that wave M3's ConnectionService + foreground
service is meant to fix.

### Android emulator

With the app installed as above:

```sh
adb shell pm grant chat.dilla.spike android.permission.RECORD_AUDIO
# open "call probe" (link under the button), tap "no-callkit" (Android has no CallKit; all modes skip it)
adb logcat -v time > call.log &                  # keep the whole run
# ~30 s in the foreground, then:
adb shell input keyevent KEYCODE_HOME             # wait 5 minutes
adb shell dumpsys audio | grep 'pack:chat.dilla.spike'   # silenced:true = the OS feeds the app silence
adb shell monkey -p chat.dilla.spike -c android.intent.category.LAUNCHER 1
grep 'DILLA_CALL ' call.log
```

### iPhone (Mac)

The CallKit Swift was written on Linux and has never been compiled; fix any build error in `CallProbe.swift` and
report what changed.

**No debugger during the lock test.** An app running under Xcode's debugger is never suspended by iOS, which would make
the lock test meaningless. Xcode is used only to install; the run itself is launched from the home screen.

One-time: on the iPhone, Settings → Apps → Safari → Advanced → Web Inspector on. On the Mac, Safari → Settings →
Advanced → "Show features for web developers". (A Debug build of the spike has an inspectable WebView.)

1. Build and install exactly as in the iPhone section above (Path B, then steps 3-6). `npx cap sync ios` is needed
   after every `npm run build`. After Xcode has launched the app once, press Stop (⌘.) and swipe the app away in the
   app switcher, so no debugger is attached.
2. Open Console.app on the Mac, select the iPhone, press Start streaming, and filter on `DILLA_CALL_NATIVE`.
3. Launch "dilla spike" from the home screen. Tap "call probe". On the Mac, open Safari → Develop → <iPhone> →
   dilla spike → `call.html` and its Console tab (filter `DILLA_CALL`).
4. Tap the mode that reached `DILLA_CALL` lines in the diagnostic run (below) and allow the microphone prompt. Expect in Console.app `DILLA_CALL_NATIVE start fulfilled` and
   `DILLA_CALL_NATIVE didActivate session AVAudioSessionCategoryPlayAndRecord / AVAudioSessionModeVoiceChat`; on the
   phone the green call indicator; in the Web Inspector console the first `DILLA_CALL` line with `t: 0`. You should
   hear yourself (or a quiet tone) in the earpiece.
5. Wait for three lines (about 30 s). Close the Web Inspector window (so it does not keep the WebView busy), then
   press the side button to lock. Note the clock time.
6. Leave it locked for 5 minutes. Do not touch the phone. Console.app keeps streaming the native lines.
7. Unlock and return to the app (via the green call pill if needed). Reopen the Web Inspector (step 3) and wait for
   two more lines. Listen whether the loopback audio is back. Photograph or copy the on-page history, then tap
   "End call".

Log lines may stop while the phone is locked (a suspended WebView logs nothing), so the counters, not the line
cadence, are the evidence: all counters are cumulative. If frames flowed the whole time, the first line after the
unlock has `framesIn` ≈ `framesOk` ≈ 50 × `t` (e.g. `t: 360` → about 18 000). If the WebView was suspended while
locked, `framesIn` falls short of 50 × `t` by about 50 × the locked seconds, and the on-page history has a gap.

### Diagnostic run first (iPhone, devicectl --console)

`CallProbe.swift` was changed on Linux again (stale-call ending, diagnostics) and is **uncompiled**; fix any build error
and report it. Build and install as above (`npm run build && npx cap sync ios`, then Xcode Run once, then Stop). Then
launch from the Mac without a debugger, so the page and native lines arrive on one stream:

```sh
xcrun devicectl list devices                      # note the iPhone's identifier
xcrun devicectl device process launch --console --terminate-existing \
  --device <id> chat.dilla.spike 2>&1 | tee call-<mode>.log      # use your bundle id if you changed it
```

In the app: "call probe" → tap the mode → allow the microphone if asked → wait about 30 s in the foreground (keep the
phone unlocked and the app in front) → "End call" → Ctrl-C the devicectl command. Then do the next mode in a fresh
launch (the `--terminate-existing` relaunch is the force-quit). Order:

1. `no-callkit`
2. `gum-first`
3. `callkit-first`

For each, send the whole `call-<mode>.log` (or at least every `DILLA_CALL_STEP`, `DILLA_CALL_EVENT`,
`DILLA_CALL_NATIVE`, `DILLA_CALL` and `DILLA_CALL_DETAIL` line). If a run hangs, the last `DILLA_CALL_STEP ... "start"`
without an `ok`, and its `timeout` line, name the step; wait 20 s more for a possible `late-ok`, then try "ctx.resume
(tap)" and "status" once each before "End call". Also say whether the green call indicator / CallKit UI appeared.

A devicectl-launched process may be treated differently from a home-screen launch for suspension, so the lock test
below still launches from the home screen.

### Force-mic run (is the WebView mic really silent?)

On 2026-11-05 (iPhone 17 Pro, iOS 27.0.1, commit `5d1706e2`) every mode fell back to the oscillator with
`micPeakAtStart: 0`. With "force mic" ticked the mic track is sent as is, and the page reports, besides the usual lines:

- `DILLA_CALL_EVENT ... "mic.settings"`: the track's `getSettings()`, `getCapabilities()` and `getConstraints()`;
  the `enumerateDevices` step lists every device kind and label.
- `DILLA_CALL_STEP ... "native.audioSessionInfo (after gUM)"` and `(call up)` (also on "status"), plus the native
  line `DILLA_CALL_NATIVE ... audioSessionInfo(...)`: the `AVAudioSession` category, mode, options, `inputAvailable`,
  available inputs, current route, input gain and channels. If WebKit capture left the session at `SoloAmbient`,
  that explains a silent mic.
- In `DILLA_CALL_DETAIL`: `micPeak0_2` and `micPeak2_5` (analyser peak over 0-2 s and 2-5 s after it was attached;
  sampled every 50 ms; the analyser now feeds the destination through a zero gain so the graph is pulled),
  `micLevel` (peak since the previous line), `micLevelNow`, `micSamples`; `inboundRtp.audioLevel`,
  `inboundRtp.totalAudioEnergy` and `inboundRtp.energyDelta` (the receiver decodes what the mic sent, so this does not
  depend on the AudioContext at all); `mediaSource.*` (the sender's own measurement of the mic track, same fields).
  `energyDelta` is per 10 s line; with speech it is clearly above 0, digital silence gives 0.

Procedure (one mode per fresh launch with the devicectl command from the diagnostic run; "force mic" ticked):

1. `no-callkit`
2. `no-callkit+session`
3. `callkit-first`
4. `gum-first` (expected to hang in `interrupted`/`suspended`; inbound energy still tells whether RTP carries audio)

For each: tap the mode, then **talk continuously ("one, two, three, ...") for 20 s** starting right at the tap, so the
0-2 s and 2-5 s windows contain speech. Listen whether you hear your own voice in the loopback (earpiece or
speaker; note which). Then "status" once, then "End call".

Report per mode: `micPeak0_2`, `micPeak2_5` and `micLevel` from the lines at `t: 0`, 10 and 20; `inboundRtp.audioLevel`
and `inboundRtp.energyDelta` (and `mediaSource.energyDelta`) from the `t: 10` and `t: 20` lines; the `mic.settings`
event; the `audioSessionInfo` lines (after gUM, call up); whether you heard yourself; and the whole log.

Reading it: `energyDelta > 0` while `micPeak*` are 0 means the mic delivers audio and only the Web Audio measurement is
blind (an artifact). Both 0 with the session at `SoloAmbient` and the `no-callkit+session` run non-zero means the
WebView needs an app-configured `playAndRecord` session. Both 0 everywhere means the WKWebView mic is truly silent.

### Lock run (5 minutes)

With whichever mode reached `DILLA_CALL` frames (`framesOk` growing) in the diagnostic run, preferring `callkit-first`,
then `gum-first`, follow steps 1-7 above.

Report:

- iOS version and iPhone model; any change needed to make `CallProbe.swift` compile.
- The three diagnostic logs (above), one per mode.
- All `DILLA_CALL_NATIVE` lines from Console.app (with their timestamps), and whether `startCall` resolved (or its
  error).
- Every `DILLA_CALL` line from start to end (Web Inspector console or the on-page history), with one
  `DILLA_CALL_DETAIL` from before the lock and one from after the unlock (plus one from during the lock if any was
  logged). Point out `source`, `visibility`, `micTrack.muted`, `audioContext` and `inboundRtp`.
- The lock and unlock clock times, and for the first line after the unlock: `t`, `framesIn`, `framesOk`, `framesBad`
  and `framesIn / (50 × t)`. Whether lines kept coming while locked; whether `framesOk` ≈ `framesOut` and `framesBad`
  stayed small, with `firstBadFrame` (a 0-byte one is the harmless bypass above; a non-empty one is a failure); and
  whether the audio came back after the unlock.
