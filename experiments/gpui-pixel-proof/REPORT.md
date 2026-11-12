# GPUI pixel proof: fidelity and tolerance

Question: can GPUI draw dilla's message feed within a small, stated pixel tolerance of Chromium, which is what beta.5 (Electron, Signal design) draws with?
The reference here is a **rasteriser-level floor**: a token-level HTML rendering in Chromium, not a beta.5 component reference. A comparison against
the real components comes with the screens. Only Linux has been run (locally). macOS and Windows are `PENDING-CI`: run `.github/workflows/gpui-proofs.yml` (workflow_dispatch) and paste
the `RESULT` lines it prints into the tables below.

## What was compared

| | beta.5 reference | GPUI |
|---|---|---|
| Renderer | Playwright 1.64.1 Chromium, `prefers-reduced-motion`, `document.fonts` awaited (the parity tool's recipe) | `gpui-pre =0.3.12`, offscreen via `HeadlessAppContext::capture_screenshot` |
| Capture | `page.screenshot`, viewport 1280x800, deviceScaleFactor 1 and 2 | GPUI test platform plus the platform's headless renderer: wgpu (Vulkan) on Linux, Metal on macOS. No window, no display |
| Font | JetBrains Mono variable (the fontsource latin file, converted to TTF once; same file both sides), base 13.5px, weight 400/600 | same file, loaded with `TextSystem::add_fonts` |
| Tokens | `packages/design-tokens/src/tokens.ts` signal values, read by hand into `fixture.json` (bg, surface, surface2, surfaceHi, hairline2, controlEdge, fg, fgBody, fg2, fg4) | same JSON |
| Geometry | 1px borders, radius 8 (pills 999), 32px avatars, 20px text lines | same JSON |

Fixture (`fixture.json`, one source for both sides): a bordered panel with 12 messages from 3 authors, one 5-line code block and one reaction row of
4 pills, rendered at 1280x800 at scale 1.0 and 2.0. Lines are hard-broken so wrapping cannot differ; line heights are fixed px.

Deviation from the brief, on purpose: the reference is a rasteriser-level floor (a token-level HTML rendering) of the layout in real Chromium, not the `MessageRow` component
from Storybook. Building Storybook and adding a 12-message story would have touched `packages/ui`, which this task may not. What is measured is
Chromium's rasteriser against GPUI's for identical geometry and font, which is the tolerance question. Pixel parity of the real feed is a later
task's job and will use these numbers.

"Scale 2.0" in GPUI: headless windows are fixed at scale factor 2.0, so one fixture px maps to `scale/2.0` logical px and the result is
1280x800 or 2560x1600 device pixels. That is DPR-correct for 2.0 and a logical-size trick for 1.0 (glyphs are rasterised at 13.5 device px either way).

## Metrics

- `max_delta`: the largest absolute difference of any R, G or B channel over the whole image (0-255).
- `differing`: a pixel whose largest channel delta is > 8 (about 3 %, above rounding in alpha blending). Also printed: > 0 and > 32.
- Gate from the brief: at scale 2.0, `differing` must be <= 2 %, else the OS is `NEEDS-JONAS`.

## Linux (Chromium from Playwright 1.64.1, wgpu on NVIDIA Vulkan)

Reference "default" is Chromium as launched by Playwright (hinting on, integer glyph advances). "nohint" is the same Chromium with
`--font-render-hinting=none --enable-font-subpixel-positioning`, run only to find out what the residual is.

| reference | scale | max delta | % > 0 | % > 8 (differing) | % > 32 |
|---|---|---|---|---|---|
| default | 1.0 | 214 | 3.675 | 3.117 | 2.409 |
| default | 2.0 | 214 | 2.740 | **2.428** | 2.063 |
| nohint | 1.0 | 136 | 2.931 | 2.119 | 0.956 |
| nohint | 2.0 | 214 | 1.674 | **1.268** | 0.695 |

Runs are deterministic: two full runs gave identical numbers.

Linux verdict: **NEEDS-JONAS** against the default reference (2.43 % > 2 % at 2.0); it passes (1.27 %) against the unhinted reference.
**Decision (Jonas, 2026-11-12): Linux follows GPUI's exact fractional glyph spacing; the Linux parity reference is the "nohint" Chromium.**
The tolerance below is therefore derived against "nohint" only.

### Where the difference comes from

1. The measurements are consistent with Chromium on Linux rounding every glyph advance to a whole device pixel under its default settings, at DPR 2 too.
   (The "nohint" variant flips two flags at once, `--font-render-hinting=none` and `--enable-font-subpixel-positioning`, so the data does not say which
   one removes the rounding.) A 13.5px JetBrains Mono glyph is
   8.1 px wide in GPUI (0.6 em, fractional positions) and 8 px in Chromium; the 12.5px code is 7.5 px in GPUI and 8 px in Chromium
   (a 48-character code line ends at x=448 in GPUI and x=472 in Chromium). Everything after the first few characters of a line is therefore
   shifted, which is most of the differing pixels and all of the worst crops (`evidence/linux/linux-2x-worst*.png`). Nothing in GPUI 0.3.12
   exposes a hinting or advance-rounding switch.
2. With Chromium's hinting off the layout agrees (boxes, borders, avatars, pills and radii line up outside text) and the residual is glyph
   anti-aliasing: GPUI's swash rasteriser versus Skia, sometimes one device pixel apart vertically (`evidence/linux/linux-nohint-2x-worst*.png`).
3. Chrome (borders, fills, radius 8 and pill shapes) matches apart from one-pixel anti-aliasing on rounded corners.

Which Chromium behaviour beta.5's Linux users see depends on their fontconfig. Jonas should decide which one is the reference for Linux before
Task 10 fixes a tolerance. On macOS Chromium does not round advances, so the "nohint" row is the likelier analogue there.

## macOS and Windows

| OS | status |
|---|---|
| macOS (macos-15, Metal headless) | PENDING-CI |
| Windows (windows-latest) | PENDING-CI, **expected red / unverified**: the `cfg(windows)` path has never been compiled, and `windows-latest` has no GPU for the wgpu renderer (it would have to fall back to a software adapter such as WARP). A red Windows leg is not evidence against GPUI. Caveat: GPUI's headless platform on Windows has no text system or renderer, so `main.rs` uses the wgpu renderer with cosmic-text. That is not the DirectWrite path the shipping Windows client uses, so the Windows number is a lower bound on realism. A DirectWrite measurement needs a real window on a 2560x1600 display, which GitHub's Windows runners do not have. The Windows code path has not been compiled yet. |

## Tolerance (read by Task 10)

Semantics, as implemented by `gpui-pixel-proof check <ref> <got> <max_delta> <max_pct>` (exit 1 on FAIL): a pixel differs if its largest R/G/B channel delta
is **greater than** `max_delta`; the check fails if more than `max_pct` percent of the pixels differ. Linux, scale 2.0, against the "nohint" reference only.

The first proposal (`220 3.9`) was not a gate: no pixel exceeds 214, so `max_delta` 220 never rejects anything, and 3.9 % is twice the 2 % budget. It is withdrawn.

Choosing the pair: share of pixels over each delta, for the correct render and four deliberately wrong ones (`./validate-tolerance.sh linux 0 0 scan`):

| variant | over 8 | over 16 | over 32 | over 48 | over 64 | over 96 | over 128 |
|---|---|---|---|---|---|---|---|
| good | 1.268 | 1.049 | 0.695 | 0.459 | 0.296 | 0.125 | 0.080 |
| (a) wrong-colour: message and code-block text in fg2 instead of fgBody | 1.836 | 1.646 | 1.381 | 1.125 | 0.870 | 0.114 | 0.053 |
| (b1) border-shift: only the panel and code-block border lines drawn 1px lower, all sizes and positions unchanged | 2.224 | 2.003 | 0.941 | 0.459 | 0.296 | 0.125 | 0.080 |
| (b2) layout-shift: panel and code block moved or resized by 1px (text and pills displaced too) | 2.726 | 2.493 | 1.649 | 0.881 | 0.699 | 0.474 | 0.364 |
| (c) fallback-font: DejaVu Sans Mono instead of JetBrains Mono | 2.062 | 1.931 | 1.714 | 1.479 | 1.283 | 0.998 | 0.782 |

Validation, `./validate-tolerance.sh linux 64 0.5` (delta 64, limit 0.5 %):

| render | expected | result | % over 64 (limit 0.5) |
|---|---|---|---|
| good | PASS | PASS | 0.296 |
| (a) wrong-colour | FAIL | FAIL | 0.870 |
| (b1) border-shift (border lines only) | FAIL | **PASS, known gap** | 0.296 |
| (b2) layout-shift | FAIL | FAIL | 0.699 |
| (c) fallback-font | FAIL | FAIL | 1.283 |

**A pure 1px border shift is below this gate's resolution; it needs a region-aware gate.** The border colours (hairline2 on bg, delta about 40) never exceed 64, so
at delta 64 the border-only fixture scores exactly the same as the correct render. It is visible at lower deltas (2.2 % over 8 against 1.3 %), but no
global (delta, percent) pair separates it from the correct render without also failing the correct one's own text anti-aliasing noise, and the pair
was deliberately not retuned to force it. An earlier version of this table called the layout shift a "border shift": it moved text and pills as well, which is what
made it fail; it is now (b2). `validate-tolerance.sh` reports (b1) as `KNOWN-GAP` without failing the Linux workflow step.

Earlier choice of pair: delta 64 separates the correct render (0.296 %) from (a), (b2) and (c) (best wrong one 0.699 %), so a limit of 0.5 % passes the first and fails those three
(1.7x headroom above the correct render, 1.4x below the nearest wrong one). The withdrawn `220 3.9` passes all of them.

Limits of this gate: it is validated against faults (a), (b2) and (c) only; (b1) is a known miss. Margins are 1.4x to 1.7x, not large, and it is calibrated on one fixture, one Linux GPU
and one Chromium; a different GPU driver or Chromium may move the "good" figure. A colour error smaller than about 64 levels on text, or text in a
fixture with little text, would be caught less reliably. The `DejaVu Sans Mono` fallback only exists on Linux. At scale 1.0 the pair is not validated.

```
TOLERANCE linux 64 0.5
TOLERANCE mac PENDING-CI PENDING-CI
TOLERANCE win PENDING-CI PENDING-CI
```

(Here `max_pct` is a percentage of pixels over `max_delta`, as above.) The workflow runs the scan on every OS so mac and win can get their own validated pair.

## How to reproduce

```
cd experiments/gpui-pixel-proof
cargo build --release
npm install && npx playwright install chromium
./run-proof.sh linux            # writes out/linux/*.png and out/linux/results.txt
```

Linux needs the usual GPUI dev packages and a Vulkan driver (a GPU, or Mesa lavapipe). `evidence/linux/` holds the images from the run behind
the tables: both renders, the diffs, and the three worst 64x64 (128x128 at 2x) crops, each as reference | GPUI | diff at 4x.

## Other findings

- `gpui_platform::current_platform(true)` on Linux has a no-op text system unless `gpui-pre-platform`'s `wayland` or `x11` feature is on: a first run
  produced all quads and no text. The proof enables both.
- JetBrains Mono's family name inside the TTF is "JetBrains Mono". The fontsource CSS name "JetBrains Mono Variable" does not resolve in GPUI.
- GPUI headless rendering needs the `test-support` features of `gpui-pre` and `gpui-pre-platform`; they must not be enabled in the shipping client.
- Programming ligatures (`->`, `::`) render in both and look alike.
