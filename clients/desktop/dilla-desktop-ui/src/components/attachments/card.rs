//! `AttachmentCard` (packages/ui/src/AttachmentCard): an image tile, a file card, or the frame of a file too large to
//! open, and the state line under a card.

use std::sync::Arc;

use gpui::{
    AnyElement, Image, InteractiveElement, IntoElement, ObjectFit, ParentElement, SharedString,
    StatefulInteractiveElement, Styled, StyledImage, div, img, px,
};

use super::{LEADING, OnClick};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING, focus_ring};
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

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
    thumb: Option<Arc<Image>>,
    badge: Option<SharedString>,
    on_open: Option<OnClick>,
) -> AnyElement {
    let h = tile_height(width);
    let inner: AnyElement = match thumb {
        Some(image) => img(image)
            .size_full()
            .object_fit(ObjectFit::Cover)
            .rounded(radius::LG)
            .into_any_element(),
        None => div().size_full().rounded(radius::LG).bg(color::SURFACE_2).into_any_element(),
    };
    let mut tile = div()
        .id(gpui::ElementId::Name(id.into()))
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
    if let Some(on_open) = on_open {
        tile = tile
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
    on_save: Option<OnClick>,
    height: f32,
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
                .child(Icon::new(IconName::File).size(px(15.0)).color(color::ACCENT_2)),
        )
        .child(
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .flex_col()
                .child(line(name, text::SM, line_height(text::SM, FILE_LEADING), color::FG).ellipsis())
                .child(line(
                    meta,
                    text::META,
                    line_height(text::META, FILE_LEADING),
                    color::FG_3,
                )),
        );
    if let Some(on_save) = on_save {
        card = card.child(
            div()
                .id(gpui::ElementId::Name(format!("{id}-save").into()))
                .flex_none()
                .size(px(DOWNLOAD))
                .flex()
                .items_center()
                .justify_center()
                .rounded(radius::MD)
                .tab_index(0)
                .cursor_pointer()
                .hover(|s| s.bg(color::SURFACE_HI))
                .focus_visible(|s| s.shadow(focus_ring()))
                .on_click(move |e, w, cx| on_save(e, w, cx))
                .child(Icon::new(IconName::Download).size(px(14.0)).color(color::FG_STRONG)),
        );
    }
    card.into_any_element()
}

/// The frame of an image too large to open here: the neutral box, then the name and the size (no button).
pub fn too_large_image(name: impl Into<SharedString>, size: impl Into<SharedString>) -> (AnyElement, f32) {
    let h = tile_height(IMAGE_W);
    let meta_h = line_height(text::SM, LEADING);
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
    line_height(text::XS, LEADING)
}

/// The line under a card: opening, could not open (the danger tone and a mark, never colour alone), too large.
pub fn state_line(text_: impl Into<SharedString>, failed: bool) -> AnyElement {
    let h = state_line_height();
    let tone = if failed { color::DANGER } else { color::FG_3 };
    let mut row = div().flex().items_center().gap(px(3.0)).h(px(h));
    if failed {
        row = row.child(Icon::new(IconName::X).size(px(f32::from(text::XS) * 0.875)).color(tone));
    }
    row.child(line(text_, text::XS, h, tone)).into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

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
}
