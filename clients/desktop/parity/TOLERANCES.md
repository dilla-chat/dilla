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

## macOS, Windows

All four values are exact placeholders (0 / 0.0): PENDING-CI. The chrome pair must be measured per OS the same way once CI runs the proof there.

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
divider. macOS and Windows are exact placeholders (PENDING-CI).

The Task 3 evidence images in `tests/fixtures/` are cropped to the bottom 1000 rows (y 600..1600) of the 2560x1600 renders:
the margins above were measured on the full frames, and the tests still hold on the crop.
