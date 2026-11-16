# GPUI desktop — macOS checks in CI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** CI catches the macOS bugs found by hand on 2026-11-16 (dilla-MacBook, macOS 27, M4) before a person does:
- the app drew no text (`gpui-pre-platform` without `font-kit`, so `NoopTextSystem`);
- the traffic lights overlapped the header;
- ⌘Q did nothing, and the app menu had no standard items;
- the voice join failed;
- the Bot channel tag and the status picker were missing.

Today the workflow builds and unit-tests on macOS (`gpui` matrix leg `gpui-mac-arm64`, plus `gpui-mac-package`), but it
never compares a macOS frame, never runs a real call on macOS, and never drives the packaged app.

**Architecture:** three layers, with **no new macOS job** on a PR. The org is on the free plan (about 5 concurrent
macOS jobs), and every desktop-gpui run already holds two of them.
1. **In-process rendering (Metal, headless):** the parity tests (`dilla-parity`'s `Driver`, the `automation` build)
   run on macOS inside the existing `gpui-mac-arm64` leg's plain `cargo test`. That leg already renders headless
   frames on Metal. New in this plan:
   - a glyph-presence test, with a `NoopTextSystem` negative control;
   - a traffic-light clearance test;
   - the `parity_*` screens, compared against references with tolerances measured on macOS.
2. **Real-server calls:** a macOS-guarded step in the same leg runs the `--ignored` call and voice suites of
   `dilla-client`, `dilla-media` and `dilla-desktop` against `dilla-testhost`. It reuses that leg's debug build and
   its test host and WASI core.
3. **Packaged app:** `gpui-mac-package` already builds the release binary and the ad-hoc `Dilla.app`. It gains one
   script that does three things:
   - launches the app with `open`;
   - drives it through System Events (osascript) and checks its menus, ⌘Q, the single-instance handover, the
     autostart LaunchAgent and the tray;
   - takes a `screencapture`, OCRs it with Vision (a small Swift script), and uploads the shots.

Cadence: pull requests matching the existing path filter, pushes to main (as today), and a new nightly `schedule` on
main.

**Tech Stack:** GitHub `macos-15` arm64 (image `macos-15-arm64`, 20261104.0514.1 as of today). GPUI `gpui-pre
=0.3.12`: `HeadlessAppContext`, `current_headless_renderer` (Metal) and `gpui::NoopTextSystem`, which is public.
Also: osascript and System Events, `screencapture`, Swift with the Vision framework (`VNRecognizeTextRequest`), and
`plutil`. BlackHole 2ch, pinned by SHA-256, is used only if Task 0 finds no audio device.

**Spec / prior art:**
- `.github/workflows/desktop-gpui.yml`;
- `scripts/check-ci-workflow.mjs`: `GPUI_PINNED_STEPS`, `GPUI_PIN_JOBS`, `GPUI_MAC_PACKAGE_RUNS`,
  `GPUI_OFF_LINUX_IGNORE`, `checkDesktopGpuiWorkflow`;
- `clients/desktop/parity/TOLERANCES.md`, which marks macOS as "PENDING-CI: must be measured per OS the same way
  once CI runs the proof there";
- `clients/desktop/parity/references/linux/README.md`;
- `clients/desktop/MANUAL.md` and `MANUAL-a11y.md`;
- `.github/workflows/gpui-proofs.yml`: `workflow_dispatch` only, not CI, and the home of the spike.

---

## Research verdicts (checked 2026-11-16)

| Question | Verdict | Confidence | Evidence |
|---|---|---|---|
| Does Metal work on `macos-15` arm64, and does GPUI render? | **Yes, headless.** In run 43815267494, job `gpui-mac-arm64` (image macos-15-arm64 20261104.0514.1), `click_and_read_element` takes an 800x1240 headless screenshot and `hover_changes_state` compares pixels. Both pass in the plain `cargo test` today. GitHub's larger-runner docs say only Metal Performance Shaders and nested virtualization are unsupported (Apple Virtualization Framework); MPS is irrelevant here. A real on-screen window (Layer 3) has not been seen yet. | High headless; medium for an on-screen window (Task 0 proves it) | repo CI log; [GitHub larger-runner docs](https://docs.github.com/en/enterprise-cloud@latest/actions/reference/runners/larger-runners) |
| Zed's CI as a reference | Zed tests its macOS GPUI code on **self-hosted** Macs (`self-hosted`/`test` labels, later `self-mini-macos`), not hosted runners. Its docs say gpui tests need Screen Recording permission. No hosted-runner precedent, so our own run is the evidence. | Medium | [zed ci.yml v0.181](https://git.secluded.site/zed/blob/v0.181.0-pre/.github/workflows/ci.yml), [zed #41420](https://git.secluded.site/zed/commit/2751512ae8c9152c9ca378cc91083d885025fdcb), [zed macOS docs](https://zed.dev/docs/development/macos.md) |
| Which TCC permissions are pre-granted? | The image's `configure-tccdb-macos.sh` grants these in both the system and the user `TCC.db`. **Accessibility:** `/bin/bash`, `/usr/bin/osascript`, the agent `/opt/hca/hosted-compute-agent`. **AppleEvents:** `bash`/`osascript` → `com.apple.systemevents` and `com.apple.finder`. **ScreenCapture:** `bash`, `osascript`, the agent. **Microphone:** the agent and the provisioner. It also writes replayd's `ScreenCaptureApprovals.plist`, so the macOS 15 "bypass the private window picker" alert does not "break headed UI tests". **Not granted:** AppleEvents from osascript to `dev.dilla.desktop`. So `tell application "Dilla"` would block on a prompt, and the scripts talk only to System Events. | High | [configure-tccdb-macos.sh](https://github.com/actions/runner-images/blob/main/images/macos/scripts/build/configure-tccdb-macos.sh) |
| Can `TCC.db` be edited? | **Yes.** The image itself runs `sudo sqlite3 "/Library/Application Support/com.apple.TCC/TCC.db" "INSERT OR IGNORE …"` (`utils.sh` `configure_system_tccdb`), and `configure-system.sh` unloads daemons with a note that this "requires SIP to be disabled". Electron's and Element's workflows insert microphone rows the same way. We need no edit unless Task 0 finds a prompt. | High | [utils.sh](https://github.com/actions/runner-images/blob/main/images/macos/scripts/helpers/utils.sh), [configure-system.sh](https://github.com/actions/runner-images/blob/main/images/macos/scripts/build/configure-system.sh), [runner-images #9330](https://github.com/actions/runner-images/issues/9330) |
| Is there a logged-in GUI session (`open`, windows)? | **Yes.** `configure-autologin.sh` (in the `macOS-15.arm64` template) enables automatic GUI login for the runner user, and the TCC comment above refers to headed UI tests on hosted runners. **But** `configure-system.sh` unloads `com.apple.notificationcenterui`, so notification banners never show. | High session; banners: none | [configure-autologin.sh](https://github.com/actions/runner-images/blob/main/images/macos/scripts/build/configure-autologin.sh), [macOS-15.arm64 template](https://github.com/actions/runner-images/blob/main/images/macos/templates/macOS-15.arm64.anka.pkr.hcl) |
| CoreAudio devices on the runner | **Uncertain.** `install-audiodevice.sh` (switchaudio-osx, sox) exists, but none of the macOS 14, 15 or 26 templates runs it. Issue #13668 (open) reports that the "Null Audio Device" is intermittently missing at start-up on macos-15 (Intel larger runner). The known fix is BlackHole 2ch followed by `sudo killall coreaudiod`; no reboot is needed (#11746). Our app joins without devices (Package 8a: listen-only, `Speaker::open_waiting`), but then the voice-join path never opens a real CoreAudio unit, and that is the path that failed on the Mac. | Low (Task 0 decides) | [#13668](https://github.com/actions/runner-images/issues/13668), [#11746](https://github.com/actions/runner-images/issues/11746), [#10902](https://github.com/actions/runner-images/issues/10902) |

---

## Global Constraints

- **Job budget:**
  - no new macOS job on PR or push events;
  - Layers 1 and 2 extend the `gpui` matrix's macOS leg, and Layer 3 extends `gpui-mac-package`;
  - the release binary is built once, in `gpui-mac-package`, and the debug and automation build once, in the
    `gpui-mac-arm64` leg;
  - nothing is rebuilt across jobs, and no artifacts are passed between jobs (none are needed);
  - the spike in Task 0 is a `workflow_dispatch` job in `gpui-proofs.yml`: it is not CI, runs once and costs no
    concurrency on PRs.
- **Fail-closed, like the rest of CI:** no `continue-on-error`, no `|| true`, and no `if: always()` on a check step.
  The R22 `FORBIDDEN` list is enforced only for `WORKFLOWS` files, but desktop-gpui.yml follows it anyway. Every
  check that "cannot run here" fails with an `::error::` that names the missing prerequisite. It never skips silently.
- **Every new run line is pinned** in `scripts/check-ci-workflow.mjs`, in the style of `GPUI_PINNED_STEPS` and
  `GPUI_MAC_PACKAGE_RUNS`, with a negative test in `scripts/check-ci-workflow.test.mjs`. `npm run check:ci` and
  `npm run test:ci-check` pass.
- **No secrets and no environment** in the macOS PR jobs. This is already pinned for `gpui-mac-package`.
- **The automation interface stays test-only.** A new hook, such as the `NoopTextSystem` switch, lives in
  `src/automation.rs` behind the `automation` feature. `release_build_has_no_automation` still passes.
- Third-party downloads (BlackHole, if used) are pinned by URL and SHA-256 and checked with `shasum -a 256 -c`, as
  beta.5's Windows installer is.
- **Run `npm run ci:local` before every push.** The macOS steps cannot run on Linux: `ci-local.mjs` reports them as
  skipped with the reason "macOS only (desktop-gpui.yml)", the way it already reports its Linux-only steps.
- **One branch and one PR for the whole plan**, plus the spike dispatches from that branch.
- Model tiers: sonnet for Tasks 4–6, opus for Tasks 1–3 (renderer and parity judgement) and for the final review.

## Review Focus

1. **The negative controls really fail.**
   - `NoopTextSystem` fails the glyph test.
   - A traffic-light overlap fails the clearance test.
   - A fallback font or a recoloured surface fails macOS parity, with at least 2x margin between the honest score
     and the limit.
   - A menu without "Quit Dilla ⌘Q" fails Layer 3.

   Ask for each control's measured numbers in the PR.
2. **The tolerances are honest.** macOS numbers must be measured, never copied from Linux. The honest/limit and
   limit/negative ratios go in `TOLERANCES.md`.
3. **The CI-pin coverage holds.**
   - `checkDesktopGpuiWorkflow` still finds every ignored test file.
   - The new macOS lines cannot be dropped or commented out without `check:ci` saying so.
   - The new ignore spelling (`not(any(linux, macos))`) is recognised by `liveIgnoreSource`.
4. **The nightly does not break release gating.**
   - `server-release-gate.mjs` reads only `event=push` runs (line 83), so a scheduled run never counts.
   - Concurrency must not let a nightly run and a push to main cancel each other.
5. **Layer 3 cannot hang.**
   - Every osascript call goes to System Events only, never to the app (no AppleEvents grant).
   - Each wait has a deadline.
   - The app is killed in a cleanup step guarded by `if: ${{ !cancelled() }}`.

---

## MANUAL.md / MANUAL-a11y.md mapping (macOS items)

| Manual item | After this plan |
|---|---|
| `capture-probe screens` / `windows` (macOS: n/a, the system picker) | stays n/a |
| `capture-probe portal`: ScreenCaptureKit's picker; the first run asks for Screen Recording | **stays manual.** The picker is interactive system UI, and the first-run TCC prompt is the thing to see. |
| `capture-probe camera` (no camera source on macOS yet) | stays n/a (runners have no camera either) |
| In a call: Share a screen; the other side sees it; Stop ends it | **Layer 2** covers the media path (`dilla-media --test capture`: the window source is video-only and encrypted, and a closed source ends the track), if Task 0 shows the synthetic source runs on macOS. Picking a screen stays manual. |
| Camera in a call (n/a on macOS) | stays n/a |
| `capture-probe apps`, `app_audio` (not built on macOS; PipeWire) | stays n/a |
| VoiceOver walkthrough, §1–5 (speech, rotor) | **Speech stays manual.** **Layer 3** adds the adapter's existence: the AX window title is "Dilla", and the sign-in field is reachable by its name through System Events. That proves NSAccessibility is wired, not what VoiceOver says. |
| Bugs of 2026-11-16: no text | **Layer 1** (glyph test + Noop control), **Layer 3** (OCR finds the sign-in strings) |
| traffic lights over the header | **Layer 1** (no text or tracked element in the traffic-light rect), **Layer 3** (no OCR box intersects the AX buttons' frames) |
| ⌘Q / app menu items | **Layer 3** |
| voice join failed | **Layer 2** (`dilla-desktop --test voice` and `dilla-client --test voice` on macOS, with a real CoreAudio device per Task 0) |
| Bot channel tag, status picker | **Layer 1** if a parity fixture shows them (Task 3, step 6). Otherwise they are a parity-coverage gap on every OS, handed to the parity plan; they are not specific to macOS. |
| Notification banners | **stays manual:** the runner image unloads `notificationcenterui` |
| Print sheet (`src/print.rs`, AppKit's print operation) | **stays manual:** the modal sheet needs a printer setup, and the output is checked by eye |
| TCC prompts for microphone, camera and screen (first run of the signed app) | **stays manual:** CI's grants are pre-seeded, so no prompt ever shows |

---

## Task 0: Spike — prove the runner (gpui-proofs.yml, dispatch)

**Files:**
- Modify: `.github/workflows/gpui-proofs.yml` (new job `mac-ci-spike`, macos-15, `timeout-minutes: 90`)
- Create: `scripts/mac-ocr.swift`, also used by Task 5
- Create: `scripts/mac-audio-devices.swift`: lists CoreAudio devices via `AudioObjectGetPropertyData` (default
  input/output, names) and exits 0

**Interfaces:**
- `swift scripts/mac-ocr.swift <png>`: prints `x y w h<TAB>text`, one line per recognised line, in pixel coordinates
  with the origin at the top-left. It uses `VNRecognizeTextRequest` with `.accurate`, `en-US`, and no language
  correction.
- `swift scripts/mac-audio-devices.swift`: prints `input <name>`, `output <name>`, `default-input <name>`,
  `default-output <name>`, or `none`.

**Steps:**
- [ ] Session and graphics:
  - `launchctl print gui/$(id -u) | head`;
  - `system_profiler SPDisplaysDataType` (the Metal line);
  - `open -a TextEdit`, then poll for the window count and print it:
    `osascript -e 'tell application "System Events" to count windows of process "TextEdit"'`.
- [ ] TCC:
  - `sqlite3 "$HOME/Library/Application Support/com.apple.TCC/TCC.db" "select service,client from access"` and
    `csrutil status`;
  - read TextEdit's menu names through System Events;
  - `screencapture -x "$RUNNER_TEMP/s.png"`, then OCR it (TextEdit's menu bar words must appear).
- [ ] Audio: run `mac-audio-devices.swift` and `system_profiler SPAudioDataType` first. Then install BlackHole 2ch:
  - download the cask's pinned `.pkg` URL, check it with `shasum -a 256 -c`, run `sudo installer -pkg … -target /`
    and `sudo killall coreaudiod`;
  - wait up to 20 s for the device list to change, then list the devices again;
  - open the default input for 1 s with `cargo run -p dilla-media --example capture-probe -- audio` if it exists;
    otherwise use a 10-line `cpal` example under `experiments/` and report whether it got a TCC prompt or samples.
- [ ] Packaged app (release build, `desktop-gpui-package-mac.sh`), opened with `open "$RUNNER_TEMP/Dilla.app"`:
  - the window appears;
  - the Dilla menu's items are readable through System Events;
  - `menu bar 2` (the status items) holds our tray item;
  - `entire contents of window 1` shows AccessKit elements;
  - the keychain shows no prompt at start (watch for a `SecurityAgent` process).
- [ ] Dispatch: `gh workflow run gpui-proofs.yml --ref <branch>`, then
  `gh run watch` (read the first failure at once).

**Verify / outcome:** record each answer in the PR description as a table: question, observed output line, run URL.
Then decide:
- **Audio:** if a default output and input exist without BlackHole in 3 of 3 runs, Task 4 uses the runner's
  devices. Otherwise Task 4 installs BlackHole (the default, see the open decision).
- **On-screen window or System Events fails:** Layer 3 falls back to checking the files and process only (pid, the
  LaunchAgent written by an automation-free `--set-autostart` CLI flag), and the gap is reported to Jonas.
- **AccessKit elements absent:** drop the AX-title check from Task 5, and keep OCR.

## Task 1: Glyph presence and the `NoopTextSystem` negative control (Layer 1)

**Files:**
- Modify: `clients/desktop/dilla-desktop/src/automation.rs`
  - `Session::open`: under `DILLA_AUTOMATION_TEXT_SYSTEM=noop`, pass `Arc::new(gpui::NoopTextSystem::new())`
    instead of `platform.text_system()`;
  - add a doc line to the protocol header.
- Create: `clients/desktop/parity/src/glyphs.rs` (`pub fn glyph_coverage(shot: &RgbaImage, rects: &[Rect], scale: f32) -> GlyphCoverage`)
- Modify: `clients/desktop/parity/src/lib.rs` (export), `clients/desktop/parity/src/driver.rs` (`launch_fixture_with_env` if
  `launch_fixture`'s env slice is not enough)
- Modify: `clients/desktop/parity/tests/harness.rs` (two tests)

**Interfaces:**
```rust
pub struct GlyphCoverage { pub lines: usize, pub inked_lines: usize, pub ink_pct: f32 }
/// For each text-bounds rect (logical px, scaled to device px), the share of pixels whose largest channel differs from the
/// rect's most common colour by more than 48. A line is inked when that share is >= 2 %.
pub fn glyph_coverage(shot: &RgbaImage, rects: &[Rect], scale: f32) -> GlyphCoverage;
```

**Steps:**
- [ ] Test `text_draws_glyphs` (`#[cfg_attr(windows, ignore = "GPUI headless has no renderer on Windows")]`, the
  same spelling as `click_and_read_element`):
  - launch `fixtures/shell/channel-view.json`, then `wait-idle`, `text-bounds` and `screenshot`;
  - assert `lines >= 30` and `inked_lines * 10 >= lines * 9`.
- [ ] Test `noop_text_system_draws_no_glyphs`: the same fixture with `DILLA_AUTOMATION_TEXT_SYSTEM=noop`; assert
  `inked_lines == 0`. If `text-bounds` returns no lines under Noop, assert that instead, and document which case
  happens.
- [ ] Measure both on Linux locally and on macOS (push; the `gpui-mac-arm64` leg runs them in its plain
  `cargo test`). Put the numbers in a comment above the thresholds.
- [ ] Reproduce the original bug once on a scratch commit: remove `features = ["font-kit"]` from
  `dilla-desktop/Cargo.toml`. The mac leg must fail `text_draws_glyphs`. Link that run in the PR, then drop the
  commit.

**Verify:**
- `cargo test -p dilla-parity --test harness text_draws_glyphs noop_text_system` is green on Linux.
- The mac leg is green.
- The scratch run without `font-kit` was red.

## Task 2: Traffic-light clearance (Layer 1)

**Files:**
- Modify: `clients/desktop/parity/tests/harness.rs` (or `tests/screen.rs`, next to the shell fixtures)
- Read first: `dilla-desktop/src/screens/shell/window.rs` (titlebar options, `traffic_light_position`) and the
  header's leading padding on macOS

**Steps:**
- [ ] Add `pub const MAC_TRAFFIC_LIGHTS: Bounds` to `dilla-desktop` (one source of truth with the window options):
  the logical rect the system draws the three buttons in, derived from `traffic_light_position` plus 3 × 12 pt
  buttons and 8 pt gaps.
- [ ] Test `macos_header_clears_the_traffic_lights`
  (`#[cfg_attr(not(target_os = "macos"), ignore = "macOS titlebar only")]`) on `shell/channel-view` and
  `shell/channel-view-b`: no `text-bounds` rect, and no tracked element other than the root and the window-drag
  region, intersects `MAC_TRAFFIC_LIGHTS`.
- [ ] Negative control: in a test-only build, setting `DILLA_AUTOMATION_NO_TITLEBAR_INSET=1` drops the inset. Skip
  this hook if the inset is a constant; in that case, prove the control on a scratch commit as in Task 1.
- [ ] `check-ci-workflow.mjs`: the new macOS-only ignore must not require a Linux `--ignored` line. Extend
  `GPUI_OFF_LINUX_IGNORE` to accept `cfg_attr(not(target_os = "macos"), ignore…)`, and add a test case in
  `check-ci-workflow.test.mjs`.

**Verify:**
- Green on the mac leg.
- The scratch commit that drops the inset is red, with the run linked in the PR.
- `npm run check:ci` passes.

## Task 3: macOS parity tolerances and references (Layer 1)

**Files:**
- Modify: `clients/desktop/parity/src/fixture.rs` (`os_dir` / the reference lookup)
- Modify: `clients/desktop/parity/src/screen.rs` (measurement output)
- Modify: `clients/desktop/parity/tolerance.toml` `[macos]`
- Modify: `clients/desktop/parity/TOLERANCES.md` (the macOS section)
- Modify: `clients/desktop/parity/tests/screen.rs` and `chrome_gate.rs`, and the `parity_*` tests in
  `clients/desktop/dilla-desktop/tests/*.rs` (52 `fn parity_` today; every one not parked by R22): change their ignore attribute
- Modify: `scripts/check-ci-workflow.mjs` (`GPUI_OFF_LINUX_IGNORE`) and its test
- Create (only if decision D-A picks it): `clients/desktop/parity/references/macos/native/*`

**Decision D-A (default; Jonas may override):** macOS compares against the **Linux web references** with
**macOS-measured tolerances**. That is the comparison `TOLERANCES.md` already plans ("measured per OS the same way
once CI runs the proof there"), and it costs no new goldens.
- `fixture::load(path, Os::MacOs)` looks in `references/macos/<name>` first and falls back to `references/linux/<name>`.
- The GPUI-only `native/` references (the quit dialogs) are blessed on macOS:
  `DILLA_PARITY_BLESS=1 cargo test -p dilla-desktop --test quit parity_`, run on the runner, downloaded as an
  artifact and looked at by eye. They go to `references/macos/native/`, because they are GPUI's own frames and
  CoreText's.

**Steps:**
- [ ] Measurement mode: `DILLA_PARITY_MEASURE=1` makes `assert_screen_with` print a `MEASURE <fixture> whole=<pct>
  chrome=<pct> colour=<pct>` line and skip the assertion. It is a test-support env var; the CI pin (Task 6) forbids
  it in desktop-gpui.yml.
- [ ] Dispatch a measurement job on `macos-15` from `gpui-proofs.yml` (job `mac-parity-measure`):
  - `DILLA_PARITY_MEASURE=1 cargo test -p dilla-parity -p dilla-desktop parity_ -- --include-ignored --nocapture`,
    skipping the R22-parked tests by name (the list comes from `grep 'parity after T6c'`);
  - the honest runs;
  - the negative controls on the two shell fixtures: the fallback font (the existing env hook), BG_2→BG_3, and the
    hairlines recoloured, as on Linux;
  - upload `target/screen-parity/*.png` as an artifact.
- [ ] Choose the `[macos]` values. Each gate's limit must sit ≥ 2x above the worst honest score and ≥ 2x below the
  weakest negative control, the rule Linux used. Write the table into `TOLERANCES.md` with the run URL.
- [ ] **If no limit separates them for a gate** (CoreText against Chromium-on-Linux antialiasing is the likely
  culprit for whole-frame), stop and ask Jonas (decision D-A fallback, below). Do not loosen past the controls.
- [ ] Flip the ignore on every non-parked parity test from `not(target_os = "linux")` to
  `not(any(target_os = "linux", target_os = "macos"))`, with the reason "references are Linux and macOS only".
  Extend `GPUI_OFF_LINUX_IGNORE` to recognise the new spelling, with a test.
- [ ] Bot tag and status picker: check whether any parity fixture contains a Bot channel row or the user panel's
  status control (`grep -rl '"bot"\|status' clients/desktop/parity/fixtures`).
  - If one does, add an element-existence assertion (`driver.element("<id>").is_some()`) to that fixture's test, so
    the missing element fails on every OS.
  - If none does, record that in the PR and hand it to the parity plan.

**Verify:**
- The mac leg runs and passes the parity screens: in its log, `Running … tests/shell.rs`, and `parity_boot_revoked
  ... ok`, not `ignored`.
- `TOLERANCES.md` has the measured macOS table.
- `npm run check:ci` passes.

**D-A fallback (Jonas):**
- (B) shoot `references/macos/` from the web client on a macOS runner with `shoot.mjs`: Chromium on macOS, CoreText.
  This adds about 26 MB, the size of the Linux set (139 PNGs).
- (C) bless GPUI's own macOS frames as goldens. These catch regressions, not parity.

## Task 4: Real-server call tests on macOS (Layer 2)

**Files:**
- Modify: `.github/workflows/desktop-gpui.yml` (the `gpui` job; steps after `cargo test $GPUI_CRATES`)
- Modify: `scripts/check-ci-workflow.mjs` (`GPUI_PINNED_STEPS.macAudio`, `.macCalls`; `GPUI_PIN_JOBS` → `['gpui']`) and its test

**The subset, and why:**

| Runs on macOS | Stays Linux-only |
|---|---|
| `dilla-client`: `gateway`, `http_sign_in`, `calls`, `voice` (includes the two-client `two_clients_join_see_and_hear_no_key_in_state`), `ring`, `dms` (after `cargo build -p dilla-testkit --bin dilla-testkit`) | `dilla-media --test call` `desktop_and_web_in_one_call` (needs Playwright Chromium, the packages/media wasm and clang 23) |
| `dilla-media`: `live` (desktop to desktop through the SFU, with an epoch change), `e2e_call` (`desktop_call_through_dillad`), `capture` (if Task 0 shows the window source runs) | `dilla-media --test call` Bot timing tests (G3) and the Bot restart tests (need dillabot, dilla-bot-core and ffmpeg; they are latency budgets, measured on Linux only) |
| `dilla-desktop`: `voice` (the app's own join through the voice UI, including `no_output_device_joins_with_a_notice` and the live-call slot test) and `ring` | `dilla-media --test app_audio` (PipeWire), and the rest of the Bot shard (see the nightly decision) |

**Steps:**
- [ ] Audio step (`if: runner.os == 'macOS'`, name "A sound device for the call tests (macOS)"). It follows Task 0:
  - **BlackHole:** pinned URL and SHA-256, then `sudo installer`, `sudo killall coreaudiod`, a poll of up to 20 s
    with `swift scripts/mac-audio-devices.swift` until `default-output` and `default-input` exist, and `::error::`
    after that;
  - **runner devices:** the same poll, with no install.
- [ ] Calls step, "Run the call tests (macOS)", `if: runner.os == 'macOS' && !cancelled()`, `shell: bash`:
  ```bash
  cargo test -p dilla-client --test gateway --test http_sign_in --test calls --locked -- --ignored
  cargo test -p dilla-client --test voice --test ring --locked -- --ignored
  cargo build -p dilla-testkit --bin dilla-testkit --locked
  cargo test -p dilla-client --test dms --locked -- --ignored
  cargo test -p dilla-media --test live --test e2e_call --test capture --locked -- --ignored
  cargo test -p dilla-desktop --test voice --test ring --locked -- --ignored --skip parity_
  ```
  Each line is a separate `cargo test` so a failure names its suite. These lines run the media tests one suite at a
  time, as the Linux shard does with the timing tests: `voice.rs` serialises its call tests through `media_slot`, and
  two suites at once would contend for the one CoreAudio device.
- [ ] Microphone TCC: the test binaries are children of bash, under the agent that holds `kTCCServiceMicrophone`.
  If Task 0 saw a prompt anyway, add a `sudo sqlite3` insert for `/bin/bash`, the way runner-images and Electron
  do, as its own pinned step with a comment and source link.
- [ ] Timeout: raise the `gpui` job's `timeout-minutes` only if the leg's measured time plus the new steps exceeds
  100. Today the mac leg takes 32 minutes, and the longest job (the bot shard) takes 50.
- [ ] Pins: `macCalls` and `macAudio` are exact step texts in the `gpui` job. Add a test that dropping the
  `dilla-desktop --test voice` line or the `runner.os == 'macOS'` guard fails `check:ci`.

**Verify:**
- The mac leg log shows each suite's `test result: ok` with a non-zero passed count, and
  `two_clients_join_see_and_hear_no_key_in_state ... ok`.
- One scratch commit that makes `dilla_media::Speaker::open` fail on macOS (cfg-gated) turns the voice suite red.
  Link the run, then drop the commit.

## Task 5: Packaged-app scripting (Layer 3)

**Files:**
- Create: `scripts/desktop-gpui-mac-ui.sh` (bash; every check a function; a deadline helper `wait_for <secs> <cmd…>`)
- Use: `scripts/mac-ocr.swift` (Task 0)
- Modify: `.github/workflows/desktop-gpui.yml` (`gpui-mac-package`: two steps after "The updater's zip holds the same app")
- Modify: `scripts/check-ci-workflow.mjs` (`GPUI_MAC_PACKAGE_RUNS` += the script call and the cleanup) and its test

**Interfaces:**
- `bash scripts/desktop-gpui-mac-ui.sh <path/to/Dilla.app> <out-dir>`
  - exits 0, or 1 with `::error::<check>: <what>` lines;
  - leaves `<out-dir>/*.png` and `<out-dir>/ocr-*.txt` behind;
  - all System Events access goes through `first application process whose bundle identifier is "dev.dilla.desktop"`
    (call it `P`);
  - the script never targets the app with AppleEvents.

**Steps (checks in this order; each launch is `open "$APP"`, followed by a wait of up to 30 s for `count windows of P ≥ 1`):**
- [ ] **Copy:** the app is copied from the DMG to `$RUNNER_TEMP/apps/Dilla.app`, so it is writable, and
  `lsregister -f` registers it.
- [ ] **Text and title:**
  - `screencapture -x -o -l <CGWindowID>` of the main window (the window id comes from a CGWindowList call in the
    Swift helper), and a full-screen capture as well;
  - OCR finds the start window's strings (the sign-in title and the field label, read from `strings.rs`, not
    hard-coded twice);
  - the AX window title is "Dilla".
- [ ] **Traffic lights:**
  - the frames of `button 1..3 of window 1 of P` (System Events position and size, times the backing scale from
    `NSScreen`);
  - no OCR box intersects their union.
- [ ] **App menu:**
  - `name of every menu item of menu 1 of menu bar item 2 of menu bar 1 of P` contains at least
    "About Dilla", "Settings…", "Hide Dilla", "Hide Others", "Show All" and "Quit Dilla";
  - the shortcuts are read through `value of attribute "AXMenuItemCmdChar"` and `"AXMenuItemCmdModifiers"`:
    "Quit Dilla" is `Q` with modifiers 0 (⌘ only), "Settings…" is `,`, and "Hide Dilla" is `H`.
- [ ] **⌘Q when idle:**
  - `set frontmost of P to true`, then `keystroke "q" using command down`;
  - the pid is gone within 10 s;
  - no "Quit Dilla?" window appeared (the quit guards are idle: no call, no draft).
- [ ] **Single instance:**
  - relaunch, and record the pid with `pgrep -f "$APP/Contents/MacOS/dilla-desktop"`;
  - `open -n "$APP"`, then wait 5 s: exactly one pid, the same one;
  - `open "dilla://"` with no argument also leaves one pid.
- [ ] **Autostart:**
  - click the menu item "Start Dilla at login" in the tray (`menu bar 2`; the app menu mirrors the tray rows, so
    try the tray first and the app menu as a second check);
  - `~/Library/LaunchAgents/dev.dilla.desktop.plist` exists and passes `plutil -lint`;
  - `Label` = `dev.dilla.desktop`, `ProgramArguments.0` = `$APP/Contents/MacOS/dilla-desktop`,
    `ProgramArguments.1` = `--hidden`, `RunAtLoad` = true;
  - the item now shows a check (`AXMenuItemMarkChar`);
  - click again: the file is gone.
- [ ] **Tray items:** the status item's menu names are, in order, "Your instances…", a separator, …,
  "Start Dilla at login", …, "Quit Dilla" (the order `menus.rs`'s test pins). "Quit Dilla" there quits through the
  same guard: the pid is gone.
- [ ] **Workflow steps:**
  - (a) `bash scripts/desktop-gpui-mac-ui.sh "$RUNNER_TEMP/zip-check/Dilla.app" "$RUNNER_TEMP/mac-ui"`;
  - (b) `pkill -f Dilla.app/Contents/MacOS/dilla-desktop` under `if: ${{ !cancelled() }}`, plus
    `actions/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02 # v4.6.2` (the pin gpui-proofs.yml uses) of
    `$RUNNER_TEMP/mac-ui`, with `if-no-files-found: error`.

  Use the zip's app (the updater's bundle), not the DMG mount, which is read-only and was detached.

**Verify:**
- Green on the PR **only after the menu and ⌘Q fix has landed** (decision D-C). Until then the job is red, and its
  `::error::App menu` line names the missing items, which proves the check.
- The artifact holds a screenshot with readable text.
- `npm run check:ci` fails when the script line or the cleanup is removed.

## Task 6: Nightly schedule, concurrency, pins, local mirror

**Files:**
- Modify: `.github/workflows/desktop-gpui.yml` (`on.schedule`, `concurrency`)
- Modify: `scripts/check-ci-workflow.mjs`, `scripts/check-ci-workflow.test.mjs`
- Modify: `scripts/ci-local-plan.mjs` / `scripts/ci-local.mjs` (skip reasons only)

**Steps:**
- [ ] `on.schedule: [{ cron: '17 2 * * *' }]`: reuse `NIGHTLY_CRON` from `check-ci-workflow.mjs`, and pin it. A
  schedule runs on the default branch and ignores `paths`.
- [ ] Concurrency: change it to `desktop-gpui-${{ github.event_name }}-${{ github.ref }}`, so a nightly run and a
  push to main never cancel each other. Pin it, with a test.
- [ ] Assert that `server-release-gate.mjs` still filters `event=push`; it already does (line 83). Add one test that
  a successful `schedule` run on the same SHA is not taken as the gate.
- [ ] Pins collected:
  - `macAudio`, `macCalls`, the Layer 3 script line and the cleanup;
  - the absence of `DILLA_PARITY_MEASURE` in desktop-gpui.yml;
  - the `gpui` matrix still containing `{ os: macos-15, platform: mac, arch: arm64 }`.
- [ ] `ci-local`: the macOS steps are listed as skipped with the reason "macOS only (desktop-gpui.yml)". The
  Linux-runnable parts are already mirrored: the glyph test and the clearance test (`ignored` on Linux) need no
  change.

**Verify:** `npm run check:ci`, `npm run test:ci-check` and `npm run ci:local` all pass.

## Task 7: Docs

**Files:**
- Modify: `clients/desktop/MANUAL.md`: the intro ("only Linux runs a real call" becomes "Linux and macOS"), plus a
  "What CI covers on macOS" table, which is the mapping above.
- Modify: `clients/desktop/MANUAL-a11y.md`: one line saying that CI checks the macOS adapter exists (Layer 3) and
  that speech stays manual.
- Modify: `clients/desktop/parity/TOLERANCES.md` (Task 3) and `references/linux/README.md` (the macOS lookup and
  fallback).
- Modify: `clients/desktop/RELEASING.md`: before tagging, check the last nightly macOS run.

**Verify:**
- A haiku pass for wording.
- Every test name cited in a doc exists (`grep`) [dilla-cite-shipped-refs].

---

## Decisions (Jonas, 2026-11-16)

- **D-A references:** the Linux web references with measured per-screen macOS tolerances. No macOS goldens.
- **D-B placement:** layers 1 and 2 fold into the existing `gpui-mac-arm64` leg, and layer 3 into `gpui-mac-package`.
  No new macOS job on PRs.
- **D-C order:** settled. The app-menu and ⌘Q fix is already on feat/gpui-release-m2 (gpui-m2/mac-chrome).
- **D-D audio:** use the runner's own audio device when there is one; install BlackHole (pinned by SHA-256) only
  when the runner has none.
- **D-E nightly:** the whole workflow nightly on main, plus a nightly `macos-26` leg and the Bot suites on macOS.
- **Cadence:** pull requests matching desktop-gpui.yml's path filter, plus nightly on main. The repo is public, so
  macOS minutes are free. The free plan allows about 5 concurrent macOS jobs.

## Addendum (Jonas, 2026-11-16): no manual Mac pass; everything below is automated

The manual pass stopped. Its remaining items, and every bug it found, become CI assertions. Each lane owns its rows.
**Stays manual:** one smoke run per release candidate on the dilla-MacBook (build, launch, voice join, log file),
plus anything a runner cannot do. Each such item names why in RELEASING.md.

### Remaining manual items → lane

| Item | Lane | How |
|---|---|---|
| Quit dialog during an upload | 3 (app) | Start an upload in the packaged app via the automation hook or a scripted drag/file pick, press ⌘Q, assert the dialog's AX elements, cancel, assert still running |
| Inline video: controls, Download, speed, PiP | 1 (render) | In-process GPUI tests on macOS through Metal, using the existing video tests run on the mac leg; PiP window opens (window count) |
| H.264/H.265 handoff on macOS | 1 (render) | Assert macOS hands H.264/H.265 to the system player path (as documented) |
| dilla-video-toolbox crash check | 1 (render) | Nightly: build with `--features webrtc-sys/dilla-video-toolbox` and run the decode test; it must not SIGSEGV (known crash: red until fixed, tracked) |
| Dock badge | 3 (app) | Two profiles (`DILLA_DATA_DIR`): profile B sends a message, assert profile A's Dock tile badge label via AX (`AXStatusLabel` of the Dock item) |
| Notifications, ring Answer/Decline | 2 (calls) + 3 (app) | Lane 2: in-process ring tests on macOS. Lane 3: two profiles, B rings A, assert A's notification request reached the notification center (delivered notifications via `osascript`/log line); banners are off on runners, so assert delivery, not pixels |
| Screen share to a second client | 2 (calls) | Two clients on macOS, one shares a test pattern (ScreenCaptureKit needs Screen Recording; grant via TCC.db as the spike found), the other receives frames |
| Print sheet | 3 (app) | Trigger Print on backup codes via the automation hook, assert an AXSheet with the print dialog appears, cancel |
| AX tree | 3 (app) | The picker exposes AXTextField "Instance address", AXButton "Connect", AXTextField "Invite link", AXButton "Join"; the workspace exposes the channel list and composer |

### Every bug found on 2026-11-16 → the check that catches it

| Bug | Check (lane) |
|---|---|
| No text (font-kit missing) | glyph presence + NoopTextSystem control (1); `cargo tree` font-kit guard (mac-package) |
| Empty AX tree | AX walker assertions (3); `activation_hands_a_lazy_adapter_the_whole_tree` (unit) |
| Voice join fails (no TLS for wss) | two-client voice join on macOS against a **wss** room (2); `wss_rooms_reach_a_tls_handshake` (unit) |
| No logger | the log file exists after launch and has the startup line (3) |
| ⌘Q dead, the app menu copied the tray | menu items + shortcuts + ⌘Q quits (3); cmd-q keymap test (unit) |
| Popovers dead (status picker, server menu) | click the chevron and the avatar in the packaged app, assert the menu's AX elements (3) |
| Segment buttons and Copy dead | click a segment, assert its selected state; Copy, then `pbpaste` (3) |
| The picker rejects an https invite link | unit test (polish lane) + paste the https link in the packaged picker (3) |
| Expiry rounding, stale revoked card | unit tests (polish lane) |
| Web wording ×2 | unit test: no desktop string contains "browser", "page" or "tab" in a web sense (polish lane) |
| Idle vs online mismatch | unit test (polish lane) |
| Traffic lights over the brand mark | traffic-light clearance (1) |
| The picker stays open | `connect_closes_the_manager` (unit) + window count after connect (3) |
| 400×280 instance window | window size after connect ≥ the workspace minimum (3); unit (polish lane) |
| Bot tag missing | row badge unit test; parity row (1) when fixtures can express a Bot channel |
| E_STORE with a fake HOME | two-profile run uses `DILLA_DATA_DIR` (3) |
| README Metal Toolchain step, video-toolbox feature name | docs (done) |
