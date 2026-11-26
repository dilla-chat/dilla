//! `AttachmentCard` (packages/ui/src/AttachmentCard): an image tile, a file card, or the frame of a file too large to
//! open, and the state line under a card.

use std::sync::Arc;

use gpui::{
    AnyElement, Image, ImageFormat, InteractiveElement, IntoElement, ObjectFit, ParentElement,
    SharedString, StatefulInteractiveElement, Styled, StyledImage, div, img, px,
};

use super::OnClick;
use crate::a11y::{Accessible, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING, focus_ring};
use crate::picture::{inner_radius, rounded_picture};
use crate::text::{line, line_height};
use crate::tokens::{color, font, motion, radius, text};

/// AttachmentCard.tsx `CARD_MAX_PX`: a sent image's tile is 280 wide, 4:3.
pub const IMAGE_W: f32 = 280.0;
/// AttachmentCard.tsx `GIF_MAX_PX`.
pub const GIF_W: f32 = 240.0;
/// `.d-attachment-card__file`: 300 wide (18.75rem).
pub const FILE_W: f32 = 300.0;
/// `.d-attachment-card { gap: 0.25rem }`: between a card and its state line.
pub const STATE_GAP: f32 = 4.0;
/// `.d-attachment-card__file { padding: 0.625rem 0.75rem }`.
const FILE_PAD_Y: f32 = 10.0;
const FILE_PAD_X: f32 = 12.0;
/// `.d-attachment-card__glyph`: 2rem.
const GLYPH: f32 = 32.0;
/// `.d-attachment-card__download`: 1.75rem.
const DOWNLOAD: f32 = 28.0;
/// `.d-attachment-card__text { line-height: 1.35 }`.
const FILE_LEADING: f32 = 1.35;

/// The tile's height for a width (`aspect-ratio: 4 / 3`).
pub fn tile_height(w: f32) -> f32 {
    w * 3.0 / 4.0
}

/// Where `object-fit: cover` puts a picture of `iw` x `ih` in a `cw` x `ch` box: the picture's left, top, width and
/// height relative to the box, centred, scaled until it covers the box on both sides.
pub fn cover_box(cw: f32, ch: f32, iw: f32, ih: f32) -> (f32, f32, f32, f32) {
    let scale = (cw / iw).max(ch / ih);
    let (w, h) = (iw * scale, ih * scale);
    ((cw - w) / 2.0, (ch - h) / 2.0, w, h)
}

/// [`cover_box`] when it may be drawn by hand: the size is a real one (a thumbnail's header comes from another person,
/// so zero is possible) and the box is finite; otherwise `None` and GPUI's own `ObjectFit::Cover` draws the tile.
fn exact_cover_box(cw: f32, ch: f32, iw: u32, ih: u32) -> Option<(f32, f32, f32, f32)> {
    if iw == 0 || ih == 0 {
        return None;
    }
    let b = cover_box(cw, ch, iw as f32, ih as f32);
    [b.0, b.1, b.2, b.3]
        .iter()
        .all(|v| v.is_finite())
        .then_some(b)
}

/// The tile's corner radius (`radius::LG`) in logical px, for the corner squares of [`exact_cover`].
const CORNER: f32 = 8.0;

/// A thumbnail cover-cropped in a `cw` x `ch` box with exact (sub-texel) UVs and the box's rounded corners.
/// `overflow_hidden` clips to a rectangle only (GPUI has no rounded content mask), so a whole picture in an oversized
/// cover box would show square corners wherever it overflows. Two layers instead: GPUI's own rounded `Cover` under the
/// whole tile, then the exact picture over it in two overlapping bands (full width between the corner rows, full height
/// between the corner columns) that leave the four `CORNER` squares to the layer below. Only those squares take
/// the whole-texel crop.
fn exact_cover(image: Arc<Image>, cw: f32, ch: f32, cover: (f32, f32, f32, f32)) -> AnyElement {
    let (x, y, w, h) = cover;
    // A band at (bx, by) of bw x bh in the box, showing the picture at its place in the box.
    let band = |bx: f32, by: f32, bw: f32, bh: f32| {
        div()
            .absolute()
            .left(px(bx))
            .top(px(by))
            .w(px(bw))
            .h(px(bh))
            .overflow_hidden()
            .child(
                img(image.clone())
                    .absolute()
                    .left(px(x - bx))
                    .top(px(y - by))
                    .w(px(w))
                    .h(px(h))
                    .object_fit(ObjectFit::Fill),
            )
    };
    div()
        .relative()
        .size_full()
        .overflow_hidden()
        .rounded(inner_radius(radius::LG, px(1.0)))
        .child(rounded_picture(
            image.clone(),
            radius::LG,
            px(1.0),
            ObjectFit::Cover,
        ))
        .child(band(0.0, CORNER, cw, ch - 2.0 * CORNER))
        .child(band(CORNER, 0.0, cw - 2.0 * CORNER, ch))
        .into_any_element()
}

/// AttachmentCard.tsx `fileMeta`: "48 KB · PNG", the extension upper-cased; a name without one shows the size alone.
pub fn file_meta(name: &str, size: &str) -> String {
    match name.rfind('.') {
        Some(dot) if dot > 0 && dot < name.len() - 1 => {
            format!("{size} · {}", name[dot + 1..].to_uppercase())
        }
        _ => size.to_owned(),
    }
}

/// An image card: one button holding the thumbnail (cover-cropped in the 4:3 tile, the `hairline-2` line, radius
/// `lg`), or a neutral box of the same size while there is none. A GIF still carries its mark at the top left.
pub fn image_tile(
    id: impl Into<SharedString>,
    width: f32,
    // The thumbnail's pixel size (width, height), when known: the cover crop is then exact (see `cover_box`).
    thumb_size: Option<(u32, u32)>,
    thumb: Option<Arc<Image>>,
    badge: Option<SharedString>,
    // A GIF that plays: its picture gets an id, which keeps GPUI's frame clock (`Img` animates only with one).
    moving: bool,
    // What it opens, named for a screen reader (`shell.attachment.open`), and the open itself.
    on_open: Option<(SharedString, OnClick)>,
) -> AnyElement {
    let h = tile_height(width);
    let id: SharedString = id.into();
    let inner: AnyElement = match thumb {
        // A GIF is one rounded Cover picture: `exact_cover`'s three layers would each keep their own frame clock (and
        // could drift a frame apart at the band seams), and their square bands snap apart from the rounded layer at 1x.
        // It loses the sub-texel crop ("design, not pixels", 2026-11-20). A GIF that plays carries an id, which keeps
        // its frame clock (GPUI's `Img` animates only with one); a still one holds its first frame.
        Some(image) if image.format == ImageFormat::Gif => {
            let picture = rounded_picture(image, radius::LG, px(1.0), ObjectFit::Cover);
            if moving {
                picture
                    .id(gpui::ElementId::Name(format!("{id}-img").into()))
                    .into_any_element()
            } else {
                picture.into_any_element()
            }
        }
        Some(image) => {
            match thumb_size.and_then(|(iw, ih)| exact_cover_box(width - 2.0, h - 2.0, iw, ih)) {
                // `object-fit: cover` by hand: GPUI's own Cover crops the picture in whole texels (`Window::paint_image`
                // rounds a sub-tile's offset and size), a little off Chromium's. The whole picture is scaled into the
                // cover box instead and the tile clips what overflows, so the crop lands on sub-texel UVs.
                Some(cover) => exact_cover(image, width - 2.0, h - 2.0, cover),
                None => {
                    rounded_picture(image, radius::LG, px(1.0), ObjectFit::Cover).into_any_element()
                }
            }
        }
        None => div()
            .size_full()
            .rounded(radius::LG)
            .bg(color::SURFACE_2)
            .into_any_element(),
    };
    let mut tile = div()
        .id(gpui::ElementId::Name(id))
        .relative()
        .flex_none()
        .w(px(width))
        .h(px(h))
        .overflow_hidden()
        .rounded(radius::LG)
        .border_1()
        .border_color(color::HAIRLINE_2)
        .child(inner);
    if let Some(badge) = badge {
        tile = tile.child(
            div()
                .absolute()
                .left(px(6.0))
                .top(px(6.0))
                .px(px(4.0))
                .rounded(radius::TAG)
                .bg(color::SCRIM_TAG)
                .child(
                    line(badge, text::BADGE, 15.0, color::FG)
                        .weight(font::WEIGHT_DISPLAY)
                        .tracking(LABEL_TRACKING),
                ),
        );
    }
    if let Some((label, on_open)) = on_open {
        tile = tile
            .a11y(Role::Button, label)
            .tab_index(0)
            .cursor_pointer()
            .focus_visible(|s| s.shadow(focus_ring()))
            .on_click(move |e, w, cx| on_open(e, w, cx));
    }
    tile.into_any_element()
}

/// The file card's own height: its padding, the glyph (taller than the two text lines) and its line.
pub fn file_card_height() -> f32 {
    let text = line_height(text::SM, FILE_LEADING) + line_height(text::META, FILE_LEADING);
    2.0 * FILE_PAD_Y + GLYPH.max(text) + 2.0
}

/// A file card: the glyph, the name and its meta line, and a download button when `on_save` is given. `height`
/// stretches it to the row's line (an image beside it), else it is [`file_card_height`].
pub fn file_card(
    id: impl Into<SharedString>,
    name: impl Into<SharedString>,
    meta: impl Into<SharedString>,
    // The download, named for a screen reader (`shell.attachment.save`), and the save itself.
    on_save: Option<(SharedString, OnClick)>,
    height: f32,
    // How long the card has been entering (`d-up` over `--duration-slow`, AttachmentCard.css:14); `None` draws it as it is.
    entered: Option<std::time::Duration>,
) -> AnyElement {
    let id: SharedString = id.into();
    let mut card = div()
        .flex()
        .flex_none()
        .items_center()
        .gap(px(10.0))
        .w(px(FILE_W))
        .h(px(height))
        .px(px(FILE_PAD_X))
        .py(px(FILE_PAD_Y))
        .bg(color::BG_3)
        .border_1()
        .border_color(color::HAIRLINE_2)
        .rounded(radius::LG)
        .child(
            div()
                .flex_none()
                .size(px(GLYPH))
                .flex()
                .items_center()
                .justify_center()
                .rounded(radius::SEG)
                .bg(color::HOVER)
                .child(
                    Icon::new(IconName::File)
                        .size(px(15.0))
                        .color(color::ACCENT_2),
                ),
        )
        .child(
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .flex_col()
                .child(
                    line(
                        name,
                        text::SM,
                        line_height(text::SM, FILE_LEADING),
                        color::FG,
                    )
                    .ellipsis(),
                )
                .child(line(
                    meta,
                    text::META,
                    line_height(text::META, FILE_LEADING),
                    color::FG_3,
                )),
        );
    if let Some((label, on_save)) = on_save {
        card = card.child(
            div()
                .id(gpui::ElementId::Name(format!("{id}-save").into()))
                .flex_none()
                .size(px(DOWNLOAD))
                .flex()
                .items_center()
                .justify_center()
                .rounded(radius::MD)
                .a11y(Role::Button, label)
                .tab_index(0)
                .cursor_pointer()
                .hover(|s| s.bg(color::SURFACE_HI))
                .focus_visible(|s| s.shadow(focus_ring()))
                .on_click(move |e, w, cx| on_save(e, w, cx))
                .child(
                    Icon::new(IconName::Download)
                        .size(px(14.0))
                        .color(color::FG_STRONG),
                ),
        );
    }
    if let Some(elapsed) = entered.filter(|e| *e < motion::SLOW) {
        let (opacity, offset) = crate::motion::rise_at(elapsed, motion::SLOW, crate::motion::EASE);
        card = card.relative().top(px(offset)).opacity(opacity);
    }
    card.into_any_element()
}

/// The frame of an image too large to open here: the neutral box, then the name and the size (no button).
pub fn too_large_image(
    name: impl Into<SharedString>,
    size: impl Into<SharedString>,
) -> (AnyElement, f32) {
    let h = tile_height(IMAGE_W);
    let meta_h = crate::text::root_line_height(text::SM);
    let el = div()
        .flex()
        .flex_col()
        .gap(px(STATE_GAP))
        .w(px(IMAGE_W))
        .child(
            div()
                .w(px(IMAGE_W))
                .h(px(h))
                .rounded(radius::LG)
                .bg(color::SURFACE_2)
                .border_1()
                .border_color(color::HAIRLINE_2),
        )
        .child(
            div()
                .flex()
                .gap(px(8.0))
                .child(line(name, text::SM, meta_h, color::FG).ellipsis())
                .child(line(size, text::XS, meta_h, color::FG_3)),
        )
        .into_any_element();
    (el, h + STATE_GAP + meta_h)
}

/// The height of a state line (`.d-attachment-card__state`, 11px).
pub fn state_line_height() -> f32 {
    crate::text::root_line_height(text::XS)
}

/// The line under a card: opening, could not open (the danger tone and a mark, never colour alone), too large.
pub fn state_line(text_: impl Into<SharedString>, failed: bool) -> AnyElement {
    let h = state_line_height();
    let tone = if failed { color::DANGER } else { color::FG_3 };
    let mut row = div().flex().items_center().gap(px(3.0)).h(px(h));
    if failed {
        row = row.child(
            Icon::new(IconName::X)
                .size(px(f32::from(text::XS) * 0.875))
                .color(tone),
        );
    }
    row.child(line(text_, text::XS, h, tone)).into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The box covers the `cw` x `ch` tile, keeps the picture's aspect and is centred.
    fn assert_covers(cw: f32, ch: f32, iw: f32, ih: f32) {
        let (x, y, w, h) = cover_box(cw, ch, iw, ih);
        let eps = 1e-3;
        assert!(w >= cw - eps && h >= ch - eps, "covers {cw}x{ch}: {w}x{h}");
        assert!((w / h - iw / ih).abs() < 1e-4, "aspect of {iw}x{ih}");
        assert!((x - (cw - w) / 2.0).abs() < eps && (y - (ch - h) / 2.0).abs() < eps);
        assert!(((x + w / 2.0) - cw / 2.0).abs() < eps && ((y + h / 2.0) - ch / 2.0).abs() < eps);
    }

    #[test]
    fn cover_box_covers_keeps_aspect_and_centres() {
        assert_covers(278.0, 208.0, 1920.0, 1080.0); // landscape: crops the sides
        assert_covers(278.0, 208.0, 1080.0, 1920.0); // portrait: crops top and bottom
        assert_covers(278.0, 208.0, 320.0, 240.0); // nearly the tile's aspect
        assert_covers(278.0, 208.0, 500.0, 500.0); // square
        let (x, y, w, h) = cover_box(280.0, 210.0, 320.0, 240.0); // exactly the tile's aspect
        assert_eq!((x, y, w, h), (0.0, 0.0, 280.0, 210.0));
    }

    #[test]
    fn exact_cover_takes_wide_tall_and_square_pictures() {
        // Wide, tall and square pictures overflow the tile by many pixels; their corners come from the rounded layer
        // under the bands (`exact_cover`), so all of them take the exact path.
        for (iw, ih) in [
            (320, 240),
            (384, 240),
            (1920, 1080),
            (1080, 1920),
            (500, 500),
        ] {
            let (x, y, w, h) = exact_cover_box(278.0, 208.0, iw, ih).expect("a real picture");
            assert!(
                x <= 0.0 && y <= 0.0 && w >= 278.0 && h >= 208.0,
                "{iw}x{ih}"
            );
        }
        assert_eq!(
            px(CORNER),
            radius::LG,
            "the corner squares are the tile's radius"
        );
    }

    #[test]
    fn exact_cover_refuses_a_zero_size_picture() {
        assert!(exact_cover_box(278.0, 208.0, 0, 0).is_none());
        assert!(exact_cover_box(278.0, 208.0, 0, 240).is_none());
        assert!(exact_cover_box(278.0, 208.0, 320, 0).is_none());
    }

    #[test]
    fn file_meta_upper_cases_the_extension() {
        assert_eq!(file_meta("loot-list.txt", "39 B"), "39 B · TXT");
        assert_eq!(file_meta("README", "1 KB"), "1 KB");
        assert_eq!(file_meta(".bashrc", "1 KB"), "1 KB");
        assert_eq!(file_meta("x.", "1 KB"), "1 KB");
    }

    #[test]
    fn a_tile_is_four_by_three_and_a_file_card_is_its_glyph_and_padding_high() {
        assert_eq!(tile_height(IMAGE_W), 210.0);
        assert_eq!(tile_height(GIF_W), 180.0);
        assert_eq!(file_card_height(), 54.0);
    }

    fn gif_tile(moving: bool) -> AnyElement {
        image_tile(
            "attachment-1-0",
            GIF_W,
            Some((1, 1)),
            Some(crate::components::attachments::testing::gif()),
            None,
            moving,
            None,
        )
    }

    /// A posted GIF that plays asks for its next frame (its picture has an id, GPUI's frame clock); one shown still, or
    /// any under reduced motion, asks for none.
    #[gpui::test]
    fn a_posted_gif_asks_for_its_next_frame(cx: &mut gpui::TestAppContext) {
        use crate::components::attachments::testing::frames_asked;
        assert!(
            frames_asked(cx, false, |_, _| gif_tile(true)) > 0,
            "a playing GIF"
        );
        assert_eq!(
            frames_asked(cx, false, |_, _| gif_tile(false)),
            0,
            "a still GIF"
        );
        assert_eq!(
            frames_asked(cx, true, |_, _| gif_tile(true)),
            0,
            "reduced motion"
        );
    }

    /// A GIF is one rounded Cover picture, not `exact_cover`'s three layers (three frame clocks could drift apart at the
    /// band seams, and the bands' square masks snap apart from the rounded layer at 1x).
    #[gpui::test]
    fn a_gif_tile_paints_one_picture(cx: &mut gpui::TestAppContext) {
        use crate::components::attachments::testing::sprites;
        assert_eq!(sprites(cx, |_, _| gif_tile(true)).0.len(), 1, "playing");
        assert_eq!(sprites(cx, |_, _| gif_tile(false)).0.len(), 1, "still");
    }

    /// A posted GIF that plays paints its next frame a frame's delay later.
    #[gpui::test]
    fn a_posted_gif_paints_its_next_frame(cx: &mut gpui::TestAppContext) {
        use crate::components::attachments::testing::frames_painted;
        assert_eq!(
            frames_painted(cx, false, |_, _| gif_tile(true)),
            2,
            "it plays"
        );
        assert_eq!(
            frames_painted(cx, false, |_, _| gif_tile(false)),
            1,
            "a still"
        );
        assert_eq!(
            frames_painted(cx, true, |_, _| gif_tile(true)),
            1,
            "reduced motion"
        );
    }

    /// Every picture of a tile follows the tile's rounded corners at the border's inside (radius `lg` less its 1px
    /// border: 7, not 8): a still drawn exactly (its rounded layer under the bands), one drawn by GPUI's Cover (no
    /// size known), and a GIF.
    #[gpui::test]
    fn a_tile_s_pictures_follow_its_rounded_corners(cx: &mut gpui::TestAppContext) {
        use crate::picture::{
            inner_radius,
            testing::{picture_corners, png},
        };
        let inner = inner_radius(radius::LG, px(1.0));
        let tile = |size: Option<(u32, u32)>, image: Arc<Image>| {
            move |_: &mut gpui::Window, _: &mut gpui::App| {
                image_tile("t", IMAGE_W, size, Some(image.clone()), None, false, None)
            }
        };
        let exact = picture_corners(cx, tile(Some((64, 48)), png()));
        // The rounded layer is painted first, the square bands over it.
        assert_eq!(exact.len(), 3);
        assert_eq!(exact[0].top_left, inner, "the exact path's rounded layer");
        let cover = picture_corners(cx, tile(None, png()));
        assert_eq!(cover[0].top_left, inner, "GPUI's Cover");
        let gif = picture_corners(
            cx,
            tile(
                Some((64, 48)),
                crate::components::attachments::testing::gif_sized(64, 48),
            ),
        );
        assert_eq!(gif[0].top_left, inner, "a GIF");
    }
}
