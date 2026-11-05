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
contract are unchanged). "Start call" sets up a loopback of two `RTCPeerConnection`s carrying one audio track. The
sender's encoded transform prepends a 2-byte magic (`d1 1a`) and XORs the frame; the receiver XORs it back, checks and
strips the magic, and counts `framesOk` / `framesBad` (a frame without the magic passes through unchanged). Paths:
`createEncodedStreams` on Chromium, `RTCRtpScriptTransform` (`call-transform-worker.ts`) on WebKit, picked by the
repo's `isVoiceSupported()`. The received audio plays through an `<audio>` element, as in a real call. Every 10 s:

```
DILLA_CALL {"t":seconds,"framesOut":n,"framesIn":n,"framesOk":n,"framesBad":n,"packetsSent":n,"packetsReceived":n}
DILLA_CALL_DETAIL {...}     # not part of the contract
```

`t` is wall-clock seconds since the call started; `packetsSent` / `packetsReceived` are getStats outbound-rtp and
inbound-rtp. `DILLA_CALL_DETAIL` carries `source` (`mic` when the mic shows any signal in its first 1.5 s; otherwise
`oscillator (...)`, a quiet 440 Hz tone, with the mic still open), `micLevel`, `micTrack` (`readyState`, `muted`,
`enabled`), `visibility`, `audioContext`, `audioPaused`, `pcState`, `inboundRtp` (`audioLevel`, `concealedSamples`,
`totalSamplesReceived`, `packetsLost`) and `at`. Opus DTX is stripped, so 20 ms frames flow on silence too (about 50
per second each way, 500 per line). The page also lists every `DILLA_CALL` line it logged (newest first), so the
history can be read on screen. "End call" logs a final line and `DILLA_CALL_END`.

On iOS, "Start call" first calls the native `CallProbe` plugin (`ios/App/App/CallProbe.swift`, registered by
`SpikeBridgeViewController`, which `Main.storyboard` uses): a CallKit `CXStartCallAction`, whose handler sets the
`AVAudioSession` to `.playAndRecord` / `.voiceChat` and fulfills; CallKit activates the session and the plugin resolves
in `provider(_:didActivate:)`, and only then does the page start the loopback. `Info.plist` has `UIBackgroundModes`
`audio` + `voip` and `NSMicrophoneUsageDescription`. Native events are logged as `DILLA_CALL_NATIVE ...`. On Android
there is no foreground service on purpose: the run measures the baseline that wave M3's ConnectionService + foreground
service is meant to fix.

### Android emulator

With the app installed as above:

```sh
adb shell pm grant chat.dilla.spike android.permission.RECORD_AUDIO
# open "call probe" (link under the button), tap "Start call"
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
4. Tap "Start call" and allow the microphone prompt. Expect in Console.app `DILLA_CALL_NATIVE start fulfilled` and
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

Report:

- iOS version and iPhone model; any change needed to make `CallProbe.swift` compile.
- All `DILLA_CALL_NATIVE` lines from Console.app (with their timestamps), and whether `startCall` resolved (or its
  error).
- Every `DILLA_CALL` line from start to end (Web Inspector console or the on-page history), with one
  `DILLA_CALL_DETAIL` from before the lock and one from after the unlock (plus one from during the lock if any was
  logged). Point out `source`, `visibility`, `micTrack.muted`, `audioContext` and `inboundRtp`.
- The lock and unlock clock times, and for the first line after the unlock: `t`, `framesIn`, `framesOk`, `framesBad`
  and `framesIn / (50 × t)`. Whether lines kept coming while locked, whether `framesBad` stayed 0, and whether the
  audio came back after the unlock.
