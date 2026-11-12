# Parity tolerances

Source: `experiments/gpui-pixel-proof` (Task 3), Linux, scale 2 (2560x1600), GPUI render against the Chromium "nohint" reference. Fixtures copied to `tests/fixtures/`.

## Linux

| gate | delta | percent | covers |
|---|---|---|---|
| whole frame | 64 | 0.5 | every pixel; catches wrong text colour, layout shift, fallback font |
| chrome | 16 | 0.2 | only pixels outside the text mask (percent of unmasked pixels); catches border and fill shifts |

Whole-frame margins (Task 3): good 0.296 %, nearest wrong 0.699 %. A 1px border-only shift scores the same as good (0.296 %): below the whole-frame gate's resolution, hence the chrome gate.

Chrome gate, text mask = pixels of the reference with a channel >= 70, grown by 3 px (`TextMask::from_bright_pixels(reference, 70, 3)`; the mask covers about 5 % of the frame). Share of unmasked pixels over each delta (python prototype, same mask, grow 3):

| render | >8 | >16 | >24 | >32 | >48 |
|---|---|---|---|---|---|
| good | 0.045 | 0.034 | 0.026 | 0.019 | 0.000 |
| (b1) border-only shift | 1.055 | 1.041 | 1.029 | 0.278 | 0.000 |
| (b2) layout shift | 1.229 | 1.170 | 1.146 | 0.589 | 0.000 |
| (a) wrong colour, (c) fallback font | ~good | ~good | ~good | ~good | ~0 |

(a) and (c) change only text, so the chrome gate does not see them; the whole-frame gate does.

Chosen: `chrome_max_delta = 16`, `chrome_max_pct = 0.2`. Good passes with 0.034 % (5.9x headroom), the border-only shift fails with 1.041 % (5.2x over the limit). The test asserts at least 4x over the limit. Compared with the whole-frame scan (good 1.268 %, b1 2.224 % at delta 8, a 1.75x ratio), masking text lifts the ratio to about 30x.

Delta 16 sits above the residual non-text noise (good drops from 0.045 % at 8 to 0.034 % at 16) and far below the border contrast (~40). Delta 24 would also work (0.026 % vs 1.029 %); 16 keeps more sensitivity for fills.

Limits: one fixture, one Linux GPU, one Chromium. The mask here is derived from the reference; text that appears only in the capture (an extra label) is not masked and counts as chrome, which is the safe direction. Anti-aliased glyph fringes wider than the 3 px grow would leak into chrome.

## macOS, Windows

All four values are exact placeholders (0 / 0.0): PENDING-CI. The chrome pair must be measured per OS the same way once CI runs the proof there.

## How screens supply the mask

The automation interface reports text element bounds (`ElementInfo`); a screen test converts them to device pixels (logical x scale) and builds `TextMask::from_rects`. Until then, `TextMask::from_bright_pixels` derives it from the reference.
