# Giphy GIFs on the GPUI desktop: diagnosis (read only, nothing edited)

Worktree: `/home/thim/Repositories/dilla/.claude/worktrees/signal-design`, HEAD 1512afac1. Paths below are relative to it.
No message text, member names or E2EE content was printed. Nothing was fetched from Giphy.

---

## Bug 1: GIFs never animate (picker grid and posted GIFs)

### Root cause: every `img()` that draws a GIF has no element id

The vendored GPUI does animate GIFs. `Image::to_image_data` decodes every frame (`core/vendor/gpui-pre/src/platform.rs:3418-3420` `decode_gif_image`, `:3497`). Frame advance happens in `Img::request_layout` (`core/vendor/gpui-pre/src/elements/img.rs:286-385`), but only when the element has a global id:

- `window.with_optional_element_state(global_id, …)` (`img.rs:286`) hands back `state = None` when the element has no id (`window.rs:4782-4806`). The frame clock lives in that state (`ImgState { frame_index, last_frame_time }`, `img.rs:249-253`). With no state, `frame_index` stays 0 (`img.rs:294`, `:312-342`).
- The next frame is requested only under `if global_id.is_some() && data.frame_count() > 1 && window.is_window_active() && !cx.reduce_motion()` (`img.rs:378-384`).

The upstream test for this path uses `img(...).id(TEST_IMG_ID)` (`img.rs:909-916`).

Every GIF draw site in the desktop uses a bare `img(image)`, so GPUI shows frame 0 and never schedules another frame:

| Where | file:line | What it draws |
|---|---|---|
| GIF picker tile | `clients/desktop/dilla-desktop-ui/src/components/attachments/gif_picker.rs:99-102` | `img(image).size_full().object_fit(Contain)`, no `.id()` |
| Posted GIF, exact cover (the usual path) | `clients/desktop/dilla-desktop-ui/src/components/attachments/card.rs:84-90` (two bands) and `:99-102` (rounded layer) | three bare `img(image.clone())` |
| Posted GIF, fallback (no header size) | `card.rs:139-143` | bare `img(image)` |
| Lightbox (GIF opened full size) | `clients/desktop/dilla-desktop-ui/src/components/attachments/lightbox.rs:46-50` | bare `img(image)` |

The bytes are fine:
- The server's preview is Giphy's animated `200w.gif` and the full file is `giphy.gif` (`internal/api/gifs.go:776-778`). No `_still` rendition is used.
- The client refuses anything that is not `GIF8[79]a` (`clients/desktop/dilla-client/src/features/gifs.rs` `is_gif_bytes`, `media()`).
- `ui::picture` tags the bytes `ImageFormat::Gif` (`clients/desktop/dilla-desktop/src/screens/attachments/ui.rs:266-296`).
- A posted GIF already loads its full file up to 8 MiB "because GPUI animates it" (`clients/desktop/dilla-desktop/src/screens/attachments/mod.rs:218-262`). That comment is the intent; the missing id defeats it.

Reduced motion is not the cause for most users:
- `cx.reduce_motion()` follows Appearance → Reduce motion, or the OS when that is set to "system" (`clients/desktop/dilla-desktop/src/motion.rs:139-153`).
- Only the parity fixtures force it on (`clients/desktop/dilla-desktop/src/app.rs:266`).
- Even with motion allowed, nothing animates without an id. GPUI also pauses animation while the window is inactive (`img.rs:316`, `:380`).

### Reference behaviour (TS is a behaviour reference only)

- **Picker grid**: plain `<img src=blob:…gif>` (`packages/ui/src/GifPicker/GifPicker.tsx:222`), so the browser animates every preview. Reduced motion only removes the cell's enter and hover transitions (`packages/ui/src/GifPicker/GifPicker.css:20`); the GIFs themselves still played.
- **Posted GIFs**: autoplay rules in `packages/web/src/screens/conversation/Attachments.tsx:65-71` and `:121-150`. The spec is `docs/design/flows/16-gif-picker.md:208-209`.
  - A GIF plays only if it is 8 MiB or less, not too large, and already sent.
  - It plays for **5 minutes from first sighting** (`GIF_PLAY_MS`).
  - It starts only when **at least 50 % of it is visible** (IntersectionObserver).
  - When it leaves the view, its blob is dropped.
  - It **never plays under `prefers-reduced-motion: reduce`**.
  - A still GIF shows the `GIF` badge.
  - The lightbox always shows the full, animated file.
- **Desktop today**: loads the full GIF for every live card at once, keeps it for the client's lifetime, and has no 5-minute window or visibility rule (`mod.rs:219-262`).

### Fix (minimal)

1. **Picker**: in `gif_picker.rs:99`, change to `img(image).id(ElementId::Name(format!("gif-img-{}", tile.id).into()))`. The id must be unique per tile; the cell already uses `gif-{id}`.
2. **Posted GIF**: in `card.rs` `image_tile`, give the image an id derived from the tile id, e.g. `format!("{id}-img")`. For an animated picture, do not use `exact_cover`'s three layers: three separate `Img` states each keep their own frame clock and can drift a frame apart at the band seams. When the image is a GIF (`image.format() == ImageFormat::Gif`), or the caller passes `animated: true`, draw one `img(image).id(..).size_full().object_fit(Cover).rounded(LG - 1px)`. This loses the sub-texel crop on GIFs, which Jonas accepts ("design, not pixels", 2026-11-20). Stills keep `exact_cover`.
3. **Lightbox**: in `lightbox.rs:46`, add `.id("lightbox-img")` so an opened GIF plays.
4. **Reduced motion**: nothing to add. GPUI's `Img` already freezes on frame 0 when `cx.reduce_motion()` is set (`img.rs:316`, `:381`).
   - This matches the reference for posted GIFs.
   - For the picker it is stricter than the browser was (the browser animated picker previews regardless). That is a defensible WCAG 2.2.2 choice, but it is a deviation, so it needs one line from Jonas.
5. **Optional, to match the reference fully**: the 5-minute and visible-only rules for posted GIFs. The 5-minute deadline and the per-`(channel, seq, index)` first sighting are easy (a map in `ui.rs` plus a timer that swaps `moving` back to the thumbnail and releases the full image). "Visible" can be approximated by "drawn this frame". The timeline only draws on-screen rows, so a card that stops being requested for N frames counts as off-screen.

### Cost of animating many GIFs, and how the reference limited it

GPUI decodes **all frames to RGBA up front** (`platform.rs:3418`) and uploads each frame to the sprite atlas the first time it is painted. The atlas is keyed per frame (`window.rs` `paint_image` → `tile` per `frame_index`).

- **Picker**: a `200w.gif` preview is about 200×(110-200) px with 20-60 frames, so about 120 KB per frame and about 2.5-7 MB of RAM per tile, decoded. A page of 24 is about 60-170 MB, and each "more" adds another 24. The CPU cost is a relayout of the picker on every frame of the fastest GIF (about 10-25 fps); repaint is cheap because the frames are already textures.
- **Posted GIFs**: up to 8 MiB compressed each. A long-lived chat can hold dozens of them.
- **Release today**: the desktop never releases decoded previews. `GifState.previews` (`ui.rs:155`, filled at `:1470-1476`) is dropped when the picker closes (`ui.rs:361`, `:399`, `:1348`, `:1520`, `keys.rs:94`). GPUI's asset cache still holds every `RenderImage` keyed by the `Image`, and the atlas still holds any frame that was drawn. There is no `remove_asset` or `drop_image` for GIFs anywhere. `drop_image` is used only for video and share frames (`ui.rs:949/959`, `share_picker/mod.rs:216-224`).
- **What the reference did to bound it**:
  - Picker: at most 2 preview fetches at a time (`GIF_PREVIEWS_AT_ONCE`, web `GifPicker.tsx:15`), retried with backoff, and blob URLs revoked on unmount (`GifPicker.tsx:68-79`).
  - Posted GIFs: the 8 MiB cap, the 5-minute window, playing only when at least half visible, and dropping the blob when it leaves the view (`Attachments.tsx:121-150`).
- **Recommended for the desktop**:
  - On picker close, and on a new search that clears items, call `image.clone().get_render_image(window, cx)` → `cx.drop_image(r, None)`, then `image.remove_asset(cx)` (`img.rs:573-581`) for each preview.
  - For posted GIFs, release the full image (same two calls, then remove from `ui.images`) at the 5-minute deadline and when the card leaves view.
  - Optionally fetch previews 2 at a time; today `search_gifs` fetches them one after another (`ui.rs:1468-1478`), which is already a natural bound.

### Tests that prove it (crate style: `#[gpui::test]` with `TestAppContext`, `add_empty_window().draw(...)`)

Fixture: a hand-written 2-frame GIF89a as a `const` byte array, or one built with `image::codecs::gif::GifEncoder` in `dilla-desktop`, which already depends on `image`. `dilla-desktop-ui` has no direct `image` dependency, so it would use the const.

- **`dilla-desktop-ui/src/components/attachments/gif_picker.rs` tests**: `a_gif_preview_asks_for_its_next_frame`.
  1. `let w = cx.add_empty_window();` and activate it (`w.update(|w, _| w.activate_window())`; `is_window_active` gates animation).
  2. Draw `gif_picker(...)` with one `GifTile { image: Some(Arc::new(Image::from_bytes(ImageFormat::Gif, TWO_FRAMES))) }`.
  3. `cx.run_until_parked()`, then draw again.
  4. Assert `w.update(|w, cx| w.simulate_next_frame(cx)) > 0` (`window.rs:2963`).
  5. Also assert it is `== 0` after `cx.update(|cx| cx.set_reduce_motion(true))` and a redraw.
  6. This fails today: no id means no `request_animation_frame`.
- **`card.rs` tests**: the same shape for `image_tile(.., Some(gif), ..)` as `a_posted_gif_asks_for_its_next_frame`, plus a still-PNG control that asks for no frame. For the single-layer change, assert that a GIF tile paints exactly one polychrome sprite (`w.rendered_frame.scene.polychrome_sprites`, as `img.rs` `image_object_fit_cover_crops_to_element_bounds` does).
- **Lightbox**: the same pattern in `lightbox.rs`.
- **Release**: in `dilla-desktop/src/screens/attachments/ui.rs` tests, `closing_the_picker_releases_its_previews`. Insert a preview, decode it (draw once), close the picker, then assert `!image.is_asset_cached(cx)` (`img.rs:586`, test-support).

---

## Bug 2: "shouldn't need to be clicked and then press Enter to send"

### What the desktop does

- **Click**: `gif_picker.rs:118` `.on_click(move |e, w, cx| pick(e, w, cx))`, built at `clients/desktop/dilla-desktop/src/screens/attachments/mod.rs:1041` as `on_pick: click(move |_, cx| ui::pick_gif(i, cx))`.
- **Keyboard**: Enter while the picker is open maps to `Key::GifPick` (`keys.rs:58`), which calls `ui::pick_gif(active)` (`keys.rs:98-101`).
- **`pick_gif`** (`ui.rs:1486-1538`):
  1. Fetches the `Full` GIF through the instance.
  2. Closes the picker (`ui(cx).gif = None`, `:1520`).
  3. Calls `attach(to, [NewFile::from_bytes(name, "image/gif", bytes)], [TrayPreview::Gif])` (`:1521-1526`), which only adds the file to the **tray** (`ui.rs:541-576` → `client.attachments().attach(&tray, files)`).
- **Sending**: the person must then press Enter in the composer. `Shell::send` → `take_for_send` (`ui.rs:675-690`) → `client.attachments().send(channel, text, files, reply)` (`clients/desktop/dilla-desktop/src/screens/shell.rs:873-900`).

### What the old app and the spec did

The desktop is a faithful port. The old app also attached rather than sent:

- Web `packages/web/src/screens/conversation/GifPicker.tsx:147-168`: `pick` fetches the full file, `attachFiles(...)` puts it in the tray, `announce(t('gif.added'))`, then `onClose('picked')`.
- `ComposerArea.tsx:286`: `picked` focuses the textarea.
- The announced string is `'gif added. press enter to send.'` (`packages/web/src/strings/gif.ts:16`).
- Jonas's own spec, `docs/design/flows/16-gif-picker.md`:
  - `:28`: "added to the file tray like a chosen file; the picker closes and focus goes to the message box".
  - `:30`: "Enter in the message box sends it".
  - `:58`, `:66`: the same rules for focus and the announcement.

So this is a **requested behaviour change**, not a regression. Click-to-send (Discord/Slack-like) contradicts flow 16 rows 28 and 30 and needs Jonas's decision as an AskUserQuestion. Two options:

- **(a)** A click or Enter on a result sends the GIF at once as its own message, with no draft text. The draft and other tray files are left alone.
- **(b)** Keep the tray, but send it at once when the draft and tray are otherwise empty.

The recommendation is (a), because it is predictable.

Smaller gaps against the existing spec, worth fixing either way:
- No `gif.added` announcement: the string exists at `strings.rs:818` but is unused.
- No `gif.picking` status line: `strings.rs:832`, also unused.
- No explicit focus return to the composer.
- The file name is title-based (`ui.rs:1514-1519`) where the spec says `giphy-{id}.gif` (flow 16 `:30`).

### Fix for (a), minimal

In `ui.rs` `pick_gif`, on `Ok(Ok(bytes))`, replace the `attach(...)` call with a send path:

1. Spawn an async block that runs `let tray = tray_of(&client, channel, cx);` (or a private tray) and `let ids = tray.add(vec![file]).await?`. `Tray::add` seals, uploads and answers once the entries are processed (`dilla-client/src/features/attachments.rs:808-815`).
2. Call `let d = tray.take(&ids)?`. It takes only those ids and leaves other tray files and the draft alone (`:1031-1055`).
3. Call `client.attachments().send(channel, String::new(), d, None).await` (`:1185`).
4. On error, show the `gif.pickFailed` toast and leave the entry in the tray so nothing is lost.
5. Keep `picking` and the dimmed tile until the upload finishes, then close the picker and announce.

A DM needs no special case: `attachments().send` resolves the channel's group (`group_of`). A pending reply could be taken with `threads::take_reply` if wanted; the reference had none on a GIF pick.

Tests:
- **`dilla-client/tests/attachments.rs`** (tokio plus fake instance, existing style): `a_tray_add_then_take_sends_only_that_file`. Add a file while another entry is in the tray, `take(&ids)`, then `send`. Assert the post carries one descriptor and the other entry remains.
- **`dilla-desktop/tests/attachments.rs`** (`#[gpui::test]`, as `the_dm_gif_notice_is_per_window_and_goes_with_its_client` at `:566`): `picking_a_gif_sends_it_without_enter`. Fake client, open the picker with one item, `ui::pick_gif(0, cx)`, `run_until_parked`. Assert the picker is closed, the tray holds no GIF entry, and the fake client recorded one send with one `image/gif` attachment and an empty body.
- **`keys.rs:171`**: `the_gif_picker_moves_by_cells_and_rows_and_picks_on_enter` stays as it is. Enter still calls `GifPick`, which now sends.

---

## 3. "The image sticks out the rounded edges" on posted GIFs

### Where message image, GIF and video cards draw

- **Image and GIF cards**: `clients/desktop/dilla-desktop/src/screens/attachments/mod.rs:216-303` calls `image_tile(...)` (`clients/desktop/dilla-desktop-ui/src/components/attachments/card.rs:122-186`).
  - The tile is `.rounded(LG=8).border_1().overflow_hidden()` (`card.rs:153-162`).
  - When the picture's header size is known, it uses `exact_cover` (`card.rs:72-107`): a rounded GPUI-`Cover` layer (`:99-102`, `.rounded(LG)`) under two un-rounded bands that skip the four 8 px corner squares (`:104-105`).
  - Otherwise it uses a single `img(...).object_fit(Cover).rounded(LG)` (`:139-143`).
- **Video cards**: `video.rs:348-367`. The frame is `.overflow_hidden().rounded(XL)` and the picture carries its own `.when_rounded` (`:361-367`, comment "GPUI clips no child to its parent's corners").

### The suspected cause is confirmed in GPUI but already handled on these cards

The cause is real in GPUI: `ContentMask` is rectangle-only (`core/vendor/gpui-pre/src/window.rs:2466-2473`). An image's own radii do work:
- `paint_image` rounds the **visible** bounds (`bounds ∩ image_bounds`, `window.rs:5391`, `:5459-5461`).
- The wgpu shader applies them (`gpui-pre-wgpu/src/shaders.wgsl:1291-1303` `quad_sdf(… corner_radii)`).

But `image_tile` and `video` already give the picture its own radius. In the latest GPUI parity shot (`target/screen-parity/attachments-cards-gpui-2x.png`, 2x, 14:21 today), the image card's corners are cleanly rounded: the crop of the top-left and bottom-right corners shows the border arc with no square pixels.

**So the screenshot is not reproduced from code or the 2x shot.** Candidates, in order:

1. **What Jonas runs** may predate `exact_cover`. It arrived in #190 (`093f129e1`, 0.2.0-alpha.2), so an older build would show it.
2. **A scale factor other than 2.**
   - At a fractional scale, the DILLA layout patch does not snap (`core/vendor/README.md` "gpui-pre 0.3.12"). The bands' rectangular content masks and the rounded layer then snap separately (`window.rs:5417`, `snapped_content_mask`), and a band can round out by a device pixel into the corner square.
   - At 1x the corners are only 8 px, so a 1 px sliver is visible.
3. **The inner radius equals the outer one.** The picture is rounded at 8 px inside a 1 px border; CSS gives the inner edge 7 px. Geometry puts the picture's arc *inside* the border's inner arc (diagonal 3.34 vs 3.05 px from the corner), so this is not a leak. It is still worth aligning: use `LG - 1px` for the picture.

To settle it: ask for the build version and display scale, or shoot `parity/fixtures/attachments/cards.json` at 1x and 1.25x. If (2) holds, the fix is to drop `exact_cover`'s bands for GIFs (Bug 1 fix 2 already does this) and/or snap the band rects to device pixels.

## 4. Sweep: rounded containers with `overflow_hidden` that hold a picture or video

| # | Place | file:line | Container | Picture | Corners leak? |
|---|---|---|---|---|---|
| 1 | Message image/GIF card | `dilla-desktop-ui/src/components/attachments/card.rs:72-107, 139-143, 153-162` | rounded LG 8 + 1 px border | rounded LG itself (exact_cover layers) | **No** at 2x (shot checked); possible 1 device px at fractional scale; inner radius should be 7 |
| 2 | Video card / inline player | `dilla-desktop-ui/src/components/attachments/video.rs:348-367` | rounded XL 10, no border | `.when_rounded` XL itself | No |
| 3 | **GIF picker tile** | `dilla-desktop-ui/src/components/attachments/gif_picker.rs:99-119` | rounded MD 6 + 1 px border + overflow_hidden | bare `img` Contain; the tile is sized to the GIF's aspect, so the picture fills it | **Yes**, all four corners |
| 4 | **Composer tray image preview** | `dilla-desktop-ui/src/components/attachments/tray.rs:92-96` in `:192-208` | rounded LG 8 + 1 px border + overflow_hidden | bare `img` Cover at the top of the card | **Yes**, top two corners (the progress bar is at the bottom) |
| 5 | **Voice stage tile video** | `dilla-desktop/src/screens/voice/mod.rs:3575-3590` in `:3727-3748` | rounded XL 10 + border + overflow_hidden | bare `img` Contain | **Yes** when the stream's aspect matches the tile (a 16:9 camera or screen in a 16:9 tile); letterboxed streams are safe |
| 6 | **DM call tile video** | `dilla-desktop/src/screens/voice/mod.rs:4394-4407` | rounded MD 6 + overflow_hidden, 16:9 | bare `img` Contain | **Yes** for 16:9 video (the usual case) |
| 7 | **Share picker thumbnail** | `dilla-desktop-ui/src/components/share_picker/mod.rs:283-299` | rounded MD 6 + 1 px border + overflow_hidden | bare `img` Contain | **Yes** when the screen or window aspect matches the thumb box (16:9 screens) |
| 8 | Lightbox | `dilla-desktop-ui/src/components/attachments/lightbox.rs:46-50` | no rounded clip (the loading box is rounded, empty) | bare `img` | No (nothing to leak past) |
| 9 | Webhook avatar in the timeline | `dilla-desktop/src/screens/timeline.rs:1214-1224` | none | `img(...).rounded(BOT)` itself | No |
| 10 | Profile card banner/avatar | Task 9 (being fixed elsewhere) | | | Noted, skipped |

There are no link-preview or embed images in the desktop yet: no `img()` outside the sites above (`grep 'img(' clients/desktop/*/src`).

### One shared approach

Add a helper in `dilla-desktop-ui` (e.g. `src/picture.rs`, or `components/attachments/mod.rs`):

```rust
/// A picture that fills a rounded box: GPUI clips children to rectangles only (window.rs ContentMask), so the
/// picture carries the box's inner radius itself (outer radius minus the border), as CSS's overflow clip would.
pub fn rounded_picture(src: impl Into<ImageSource>, outer: Pixels, border: Pixels, fit: ObjectFit) -> Img {
    img(src).size_full().object_fit(fit).rounded((outer - border).max(px(0.)))
}
```

Use it at #3 (MD, 1 px), #4 (LG, 1 px; round only the top corners with `rounded_t`), #5 (XL minus the edge width), #6 (MD, 0), #7 (MD, 1 px), and #1's fallback and GIF path (LG, 1 px).

Because `paint_image` rounds the visible rect, Contain pictures that are letterboxed get rounded corners on the picture itself. That differs slightly from CSS, which leaves square picture corners inside the rounded box, but it never leaks and reads as intended.

For animated or GIF content, pass an `.id(...)` as well (Bug 1).

### Test pattern

A unit test per site in the crate's `#[gpui::test]` style:

1. Draw the component in `add_empty_window()` with a solid-colour picture: a 1-frame `RenderImage`, as `img.rs` tests build with `ImageBuffer::from_pixel`.
2. Assert on the scene: `w.rendered_frame.scene.polychrome_sprites.last().corner_radii` is at least the inner radius, scaled. GPUI's own `img.rs` tests assert `corner_radius` the same way (`img.rs:~890`).

Pixel sampling (pixel (1,1) is not the picture's colour) needs a real renderer. Keep it for the parity screen shots: `parity/shoot.mjs` at 1x and 2x for `attachments/cards`, the GIF picker and the voice stage.
