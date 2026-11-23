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
`polls/card` gets its own chrome limit too, 1.17 %: 2x its honest macOS score, 0.586 % (run 44883563855). That is the
same half-pixel row-top snap Jonas accepted on Linux (Task 40, see "Linux per-screen").

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

## Linux per-screen

Nine screens carry their own Linux limits in `[linux_screens."<fixture>"]` (Jonas 2026-11-19: "fix crop, tolerate emoji"),
each 2x its honest score, the way the macOS ones are, except `user-settings/appearance-compact` (Jonas 2026-11-20:
"design, not pixels"), whose limit is just over its score. Scores are the `DILLA_PARITY_MEASURE` lines of the Linux run.

| screen | gate | before the crop fix | after | limit | reason |
|---|---|---|---|---|---|
| `attachments/cards` | chrome (shared 0.2) | 0.7293 | 0.0903 | shared | GPUI cropped the cover image in whole texels (`Window::paint_image`'s sub-tile rounds its offset and size); the tile now draws the whole picture into a clipped box offset by the cover offset (`card::cover_box`) |
| `attachments/cards` | colour (shared 0.3) | 0.6533 | 0.3988 | 0.8 | raster resampling: GPUI scales the thumbnail bilinearly, Skia bicubically; the remaining row is the picture's blended top and bottom edge row (sub-pixel start) |
| `attachments/cards-b` | chrome | 1.4499 | 0.4162 | 0.84 | as cards, over nine tiles |
| `attachments/cards-b` | colour | 2.6966 | 1.0786 | 2.16 | as cards, over nine tiles |
| `reactions/many-wrapping` | chrome / colour | 0.2392 / 0.2719 | unchanged | 0.48 / 0.54 | colour emoji bitmap scaled to 15 px otherwise than Skia's (raster) |
| `reactions/many-wrapping-b` | chrome / colour | 0.2331 / 0.2545 | unchanged | 0.47 / 0.51 | as many-wrapping |
| `reactions/emoji-grid` | chrome / colour | 0.2812 / 0.4397 | unchanged | 0.57 / 0.88 | the picker grid's colour emoji, as many-wrapping |
| `user-settings/appearance-compact` | chrome (shared 0.2) | — | 0.2562 | 0.26 | Jonas 2026-11-20, "design, not pixels" (Task 19b): a browser-rendering artefact, accepted. Chromium places `.d-segmented`'s inline-flex group from its line's rounded baseline: laid out at y 213.5, the group's baseline is 213.5 + 19.95 = 233.45 at compact, rounded to 233, so the group paints at 213 (at regular 233.875 rounds to 234 and it paints at 214). GPUI's layout rounds the row's 1.5 px centring to whole px first (214 at both densities), so the three Segmented groups sit 1 px low at compact only. The label takes the root line height as the web's does |
| `polls/card` | chrome (shared 0.2) | 0.5945 | unchanged | 1.19 | Jonas 2026-11-20, "design, not pixels" (Task 40): a browser-rendering artefact, accepted. Chromium lays the rows of the log at the fractions of its 20.14 px body lines (the third row starts at 475.61, its card at 535.5), where GPUI's log keeps whole-pixel row tops (476, card 536), so the card and everything in it sits half a pixel low and its borders, answer rows and the Close poll button snap one device pixel apart. Every difference is that offset: the card's height, its 6 + 6 px margins, the answers and the meta line were measured against the web's layout (`shoot.mjs --out`) and agree. `polls/card-b` (0.1789) and `polls/dialog` (0.1146) pass the shared limit |
| `dms/picker` | chrome (shared 0.2) | 0.2366 | unchanged | 0.48 | Jonas 2026-11-21, "design, not pixels" (Task 42): a browser-rendering artefact, accepted. The labels of the dialog's small `message` buttons are centred at a fractional x in Chromium and one device pixel further right in GPUI (the buttons' edges, the rows, the DM list behind (rows step 44 px, fixed in this task), the sizes and the colours agree). The world holds no DM message another person sent, so neither side draws an unread dot (`dms/home` and `-b` are GPUI references for that, `native/dms-home{,-b}`) |
| `dms/picker-b` | chrome (shared 0.2) | 0.2859 | unchanged | 0.58 | as `dms/picker`, over the labels of three rows |

All of it is raster resampling, invisible to the eye at the screen's size: the trip crops (reference, GPUI, difference > 24)
show the same pictures, the same rounded 8 px corners and the same chip and cell positions.

**The emoji advance is the web's.** Chromium's `layout.json` (shot with `shoot.mjs --out`) gives every reaction chip a width of
42.67 px (a pitch of 46.67 with the 4 px gap) and its count at x 422.36, 469.03, 515.70, 562.38, ...; GPUI's `text-bounds` put
the same counts at x 422, 469, 516, 562, 609, 655, 702, 749, 795, 842, 889, 935 (whole pixels), the same pitch to rounding.
The 12 picker emoji sit in fixed 38.5 px cells (the web's emoji span is 23.72 px wide at 19 px, every one alike), and both
frames put every glyph in the same cell. So the advances match and the residue is the bitmap face's raster alone.

macOS does not use these entries; the five tests stay `#[ignore]`d there until its own limits are measured.

**`voice-settings/mic-test-running{,-b}`: the automatic threshold (Task 33, no limit raised).** The web's frame shows the line at
-70 dB and "threshold -70 dB", GPUI's at -48 dB. The rule is the same on both sides (`packages/media/src/audio/sensitivity.ts`:
start at -48 dB, then the room's noise floor plus 15 dB, held between -70 and -30 dB; the native gate's test is
`automatic_starts_at_minus_48_then_follows_the_floor_plus_15`). The web fixture's microphone is silence (`silence.wav`), so by the
shot its floor has fallen to the dead-silent end and the line sits on the -70 clamp; GPUI's headless test has measured no frame yet
when it captures and shows the resting -48. It is the fixture's noise floor at capture time, not a drawing difference: the line is
`left: ${threshold - MIN}%` of the meter's width on both sides (`SensitivityMeter::line_fraction`, `threshold_x_is_proportional`).
The screens pass the shared gate (chrome 0.1638 / 0.0877).

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

## Motion (Task 36b)

The references are shot with reduced motion, so the parity fixtures run the app with GPUI's `reduce_motion` set: no entrance
(`d-up`) and the equaliser bars at full height, as the web's zeroed `--duration-*` tokens leave them. Accepted differences
from the web's animation, none visible in a reference:

- `d-bar`'s first beat: CSS keeps the second and third bar at rest height until their delay (beat/4, beat/2) has passed;
  the desktop draws them at the phase they would have, once, at the start.
- "Reduced motion: Use system setting" follows the OS (Windows animation setting, macOS Reduce motion, GNOME `enable-animations`
  through the portal on Linux; other Linux desktops animate), read again at most every 10 s on a state change, not on the
  OS's own notification.
- The dock's compact row draws no equaliser, as the web's does not (the task brief named it by mistake).

### Live indicators (Task 36e)

The speaking meter, typing sweep, pending spinner, brand caret, ring wave and jump-to flash all draw their reduced-motion form
in the references (the same `reduce_motion` as above): full meter bars, a still sweep gradient, an upright spinner, a caret
that stays on, the plain 2px `--ok` ring, and MessageRow.css's own reduced form of the flash (the tint plus an inset 2px
`--accent` edge). Accepted differences from the web's animation, none visible in a reference:

- The caret is on until its 49% stop and off from there, on a timer at each stop; the keyframe's 10 ms `ease` fall between 49%
  and 50% (shorter than a frame) is a step.
- The speaking meter and the typing sweep fade as the web's `opacity` transitions do (`--duration-fast`, `--duration-turn`,
  `ease`); one difference: the fade-out of the sweep is drawn at rest, centred, as the web's (its animation is removed when
  typing stops), and the meter's keeps bouncing while it fades.
- Typing again while the sweep fades out starts its fade-in from 0 (a new transition, a one-frame dip); the web's `opacity`
  transition reverses from where it is. Brief, and not in a reference (Task 36e).
- `d-row-flash`'s tint is a layer in the row, under its content, standing in for the row's background (the row has no fill of
  its own and no hover style while it flashes, as a CSS animation beats both). A focused flashing row under reduced motion
  loses its focus ring to the flash's edge, as the CSS's `box-shadow` replaces the other shadows.

### Overlays, toasts and badges (Task 36d)

Menus, dialogs, pickers, the toast, the tooltip, the scrims and the badges all draw their reduced-motion form in the
references (the same `reduce_motion`): in place, opaque, full size. Accepted differences from the web's animation, none
visible in a reference:

- `d-modal` and `d-toast` also scale (`0.985` and `0.96` from the first frame). GPUI cannot scale an element and what is in
  it, so the desktop draws their fade and their 8px (modal) or 10px (toast) settling and not the scale: a 300px menu is 2px
  narrower for the first frames on the web. `d-pop` (the channel-row pill, the rail badge, the bell's count) is only a scale,
  so it is drawn: the badge is rebuilt at each scale about its centre (every length and its text multiplied), over a hidden
  full-size copy that keeps the layout, so its text is laid out at fractional sizes for the pop (the web scales the
  rasterised glyphs).
- The dialog's and the search palette's backdrops are a layer of their own over the window (`d-fade` on that layer), the panel
  above it entering by itself, as the web's `::backdrop` and `<dialog>` do.
- The share picker is a `Dialog` here (the web's is a popover above the Share button), so its panel enters with the
  popover's `--duration-popover` but a scrim fades in behind it.
- The video player's ⋮ menu is the app's `ActionMenu`, so it enters with `d-modal` over 160 ms; the web reference's is
  Chromium's native overflow menu, which has none. The shot is taken still (every shot through the automation driver and the
  video-player test sets reduced motion), so it compares the end state.
- Overlays repaint at the display's frame rate while they enter (at most 450 ms).
- Entrances of Task 36c (Jonas 2026-11-21, "Fade + slide is enough"): `d-up`, `d-invite-up`, `d-poll-card-entry`,
  `d-poll-answer-entry`, `d-log-fade`/`d-log-rise` and `d-fade` are drawn as their opacity and offset, which GPUI can put on
  a subtree. The log's 6px rise moves the log's content, not its scroll box. `d-pop` (the reaction chip, the poll voters) is
  only a scale, so it is drawn with the rebuild-at-scale helper, as the badges are. No entrance of 36c has a `scale(...)`
  part on a subtree; `d-sheet-up` and `d-in` have no desktop site (phone sheets; `d-in` is used nowhere on the web).
- The message log remembers each row's entrance by the message's key (the web's row is a DOM node that mounts once), so
  scrolling a row out of the drawn range and back, or any redraw, never plays it again. A file card, a poll card, a reaction
  chip and a poll voter, which live in a row the log may not draw, start at their first draw instead of at the row's mount (a
  row that arrives off screen plays them when it is first scrolled to).
- Entrances ask for a frame at the display's rate while they run (a message row: at most 380 ms plus its stagger, the first
  paint's last row 1.14 s); nothing asks once they are over.
