# Parity tolerances

Source: `experiments/gpui-pixel-proof` (Task 3), Linux, scale 2 (2560x1600), GPUI render against the Chromium "nohint" reference. Fixtures copied to `tests/fixtures/`.

## Linux

| gate | delta | percent | covers |
|---|---|---|---|
| whole frame | 64 | 0.5 | every pixel; catches wrong text colour, layout shift, fallback font |
| chrome | 16 | 0.2 | only pixels outside the text mask (percent of unmasked pixels); catches border and fill shifts |

Whole-frame margins (Task 3): good 0.296 %, nearest wrong 0.699 %. A 1px border-only shift scores the same as good (0.296 %): below the whole-frame gate's resolution, hence the chrome gate.

Chrome gate, fixture text mask = pixels of the reference with a channel >= 70, grown by 3 px (`TextMask::from_bright_pixels_fixture_only(reference, 70, 3)`; about 5 % of the frame). Share of unmasked pixels over delta 16, measured by the Rust path (`chrome_margins_measured_in_rust`, printed as `CHROME ...`):

| render | % over 16 |
|---|---|
| good | 0.0344 |
| (b1) border-only shift | 1.0414 |
| (b2) layout shift | 1.1698 |

(a) wrong colour and (c) fallback font change only text, so the chrome gate does not see them; the whole-frame gate does.

Chosen: `chrome_max_delta = 16`, `chrome_max_pct = 0.2`. Good passes with 0.0344 % (5.8x headroom), the border-only shift fails with 1.0414 % (5.2x over the limit). The tests assert good < limit/2 and b1 > 4x limit. Compared with the whole-frame scan (good 1.268 %, b1 2.224 % at delta 8, a 1.75x ratio), masking text lifts the ratio to about 30x.

Delta 16 sits above the residual non-text noise (good drops from 0.045 % at 8 to 0.034 % at 16) and far below the border contrast (~40). Delta 24 would also work (0.026 % vs 1.029 %); 16 keeps more sensitivity for fills.

Limits: one fixture, one Linux GPU, one Chromium. Anti-aliased glyph fringes wider than the 3 px grow would leak into chrome. The chrome gate fails with `ParityFailure::Mask` if the mask covers more than 50 % of the frame.

**The bright-pixel mask is fixture-only.** It treats every pixel with a channel >= 70 as text. That holds for the pixel-proof fixture (borders and fills peak at ~58) but not for real screens: Signal accent (#5AA8E6), brand, ok/warn/danger fills, focus rings, avatar fills, selected rows and icons are all brighter, so they and a 3 px fringe are masked and NOT checked by the chrome gate. A wrong border or fill on an accent button passes under that mask (`bright_pixel_mask_hides_accent_chrome_but_rect_mask_catches_it` demonstrates it). Screens must use `TextMask::from_rects` with real text bounds.

## macOS

Measured on macos-15 (Metal) against the **Linux web references** (macOS CI plan, decision D-A: no macOS goldens), over
119 honest screens (run 43838350768), with the deliberate defects on `shell/channel-view` and `-b` (run 43847238361):

| gate (`[macos]`) | honest median | honest worst | limit | defect it must catch |
|---|---|---|---|---|
| whole frame, delta 96 | 0.12 % | 0.36 % (`commands/reminders`) | 0.8 % | fallback font 1.78 / 2.14 %; 1 px shift 1.59 / 1.76 % |
| chrome, delta 16 | 0.056 % | 0.185 % (`sign-in/factor`) | 0.4 % | 1 px shift 0.73 / 0.61 % |
| colour, 2 px radius, delta 2 | 0.087 % | 0.68 % (avatar and picture screens) | 0.3 % | hairline recoloured 0.58 / 0.60 %; surface recoloured 27.6 / 24.8 % |

The eight avatar and picture screens get their own colour limit in `[macos_screens."<fixture>"]`, 2x their honest score.

**Margins.** The plan asks every limit to sit at least 2x above the worst honest screen and 2x below the defect. Whole
frame clears both (2.2x / 2.2x). Chrome is 2.16x above honest but 1.5–1.8x below the shift; colour is 1.9x below the
recoloured hairline, and the screens that score above 0.3 % honestly carry their own per-screen limits. **Jonas, 2026-11-16: accepted as measured; tighten later
with more data.** Every screen passes under these limits (run 43847238361).

CI never sets `DILLA_PARITY_MEASURE` (it turns every check into a measurement): `check-ci-workflow.mjs` refuses it.

### Glyph presence (macOS CI plan, Task 1)

`text_draws_glyphs` (tests/harness.rs, `glyph_coverage` in src/glyphs.rs): every line box `text-bounds` reports on
`shell/channel-view` must hold ink, a pixel whose largest channel is more than 48 off the box's most common colour, on
at least 2 % of the box. A line is inked at that share; the test asks for at least 30 lines, 90 % of them inked.
`noop_text_system_draws_no_glyphs` draws the same screen with GPUI's `NoopTextSystem`
(`DILLA_AUTOMATION_TEXT_SYSTEM=noop`), which is what a macOS build without gpui-pre-platform's `font-kit` gets: the
"no text at all" bug of 2026-11-16.

| frame | lines | inked | ink % |
|---|---|---|---|
| Linux (lavapipe), honest | 66 | 66 | 15.2 |
| macOS (Metal, macos-15), honest | 66 | 66 | 17.2 |
| either OS, `NoopTextSystem` | 66 | 0 | 0.0 |

The Noop text system still lays the lines out (its metrics are fixed), so the boxes come back and stay empty.

### Traffic-light clearance (macOS CI plan, Task 2)

`macos_header_clears_the_traffic_lights` (macOS only) draws `shell/channel-view` and `-b` with the shipping title bar
(`DILLA_AUTOMATION_WINDOW_CONTROLS`) and asks that no text line and no tracked element but the window's root, the bar
and the drag strip intersect `dilla_desktop::MAC_TRAFFIC_LIGHTS` (12, 16, 54 x 16: the three AppKit button frames at
`MAC_WINDOW_BUTTONS`). On macos-15 nothing does. Its control, `a_bar_without_the_inset_is_under_the_traffic_lights`
(every OS), draws the web bar 14 px in and finds the brand mark (`topbar-brand` 14,13 18x18) and two text lines there.

## Windows

All four values are exact placeholders (0 / 0.0): PENDING-CI. GPUI's headless mode has no renderer on Windows, so
Windows parity is measured with a real window later.

## How screens supply the mask

The automation interface reports text element bounds (`ElementInfo`); a screen test converts them to device pixels (logical x scale) and builds `TextMask::from_rects`. `TextMask::from_bright_pixels_fixture_only` must not be used by screen tests.

## Colour gate (Task 5)

The chrome gate's delta of 16 cannot see a fill or border one design token off: the dark ladder steps by 3 to 5 (BG 8,9,11;
BG_2 11,13,16; BG_3 13,16,20; HAIRLINE 26,29,35; HAIRLINE_2 35,40,49). The colour gate (`colour_*` in `tolerance.toml`, only
outside the text mask) asks for each pixel: is its colour within `colour_max_delta` (2) of some reference pixel within
`colour_radius` (2 device px) of it? A line or edge a pixel out of place still finds its colour; a wrong colour does not.
Measured on the two shell fixtures, share of unmasked pixels with no such neighbour:

| frame | % |
|---|---|
| channel-view, honest | 0.118 |
| channel-view-b, honest | 0.092 |
| channel-view, BG_2 surface recoloured to BG_3 | 27.6 |
| channel-view-b, the same | 24.8 |
| channel-view, hairlines recoloured to HAIRLINE_2 | 0.586 |
| channel-view-b, the same | 0.614 |

Chosen: `colour_max_pct = 0.3` (honest 2.5x under, a recoloured border 2x over, a recoloured surface 80x over). The
honest numbers are the layout's real 1 px differences beyond the radius and anti-aliased curves; a border recoloured on
only part of a screen moves the share less, so the gate sees whole-class errors (every hairline, every panel), not one
divider. macOS: see "macOS" above; Windows is a placeholder (PENDING-CI).

The Task 3 evidence images in `tests/fixtures/` are cropped to the bottom 1000 rows (y 600..1600) of the 2560x1600 renders:
the margins above were measured on the full frames, and the tests still hold on the crop.
