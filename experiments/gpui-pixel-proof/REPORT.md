# GPUI pixel proof: fidelity and tolerance

Question: can GPUI draw dilla's message feed within a small, stated pixel tolerance of beta.5 (Electron/Chromium, Signal design)?
Only Linux has been run (locally). macOS and Windows are `PENDING-CI`: run `.github/workflows/gpui-proofs.yml` (workflow_dispatch) and paste
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

Deviation from the brief, on purpose: the reference is a token-level HTML rendering of the layout in real Chromium, not the `MessageRow` component
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

Linux verdict: **NEEDS-JONAS** against the default reference (2.43 % > 2 % at 2.0); it would pass (1.27 %) against the unhinted reference.

### Where the difference comes from

1. Chromium on Linux with default hinting rounds every glyph advance to a whole device pixel, at DPR 2 too. A 13.5px JetBrains Mono glyph is
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
| Windows (windows-latest) | PENDING-CI. Caveat: GPUI's headless platform on Windows has no text system or renderer, so `main.rs` uses the wgpu renderer with cosmic-text. That is not the DirectWrite path the shipping Windows client uses, so the Windows number is a lower bound on realism. A DirectWrite measurement needs a real window on a 2560x1600 display, which GitHub's Windows runners do not have. The Windows code path has not been compiled yet. |

## Proposed tolerance (read by Task 10)

Derived from Linux against the default reference (the worse one): the worst of scale 1.0 and 2.0, with headroom (max delta rounded up to the next 10;
percent differing times 1.25, rounded up to 0.1). `max_pct` means percentage of pixels with channel delta > 8.

```
TOLERANCE linux 220 3.9
TOLERANCE mac PENDING-CI PENDING-CI
TOLERANCE win PENDING-CI PENDING-CI
```

A `max_delta` of 220/255 is honest rather than useful: single glyph edges differ that much. For regression tests `max_pct` is the meaningful bound;
text-free regions (chrome) can be tested with a stricter, separate bound.

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
