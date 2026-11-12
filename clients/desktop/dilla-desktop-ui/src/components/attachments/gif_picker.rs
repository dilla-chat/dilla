//! `GifPicker` (packages/ui/src/GifPicker): the 380px popover over the composer: the search field and the Giphy
//! attribution, a three-column masonry of previews, the status line and "more". The app owns the field (an input
//! entity) and the keyboard: arrows move the active result, Enter picks it, Escape closes.

use std::sync::Arc;

use gpui::{
    AnyElement, Image, InteractiveElement, IntoElement, ObjectFit, ParentElement, SharedString,
    StatefulInteractiveElement, Styled, StyledImage, div, img, px,
};

use super::{LEADING, OnClick};
use crate::metrics::LABEL_TRACKING;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

/// `.d-gif-picker`: 23.75rem wide, 0.5rem padding, 0.375rem gaps.
pub const WIDTH: f32 = 380.0;
const PAD: f32 = 8.0;
const GAP: f32 = 6.0;
/// GifPicker.tsx `COLUMNS` and `TILE_GAP`.
pub const COLUMNS: usize = 3;
const TILE_GAP: f32 = 4.0;
/// `max-height: min(24rem, 60vh)`.
pub const MAX_H: f32 = 384.0;

/// One result.
pub struct GifTile {
    pub id: SharedString,
    pub width: u32,
    pub height: u32,
    /// The preview, `None` while it loads.
    pub image: Option<Arc<Image>>,
    pub on_pick: OnClick,
}

/// GifPicker.tsx `tileSpan` without the 1px grid: a tile's height in a column `column` wide (1:1 when unknown).
pub fn tile_height(width: u32, height: u32, column: f32) -> f32 {
    let ratio = if width > 0 && height > 0 { height as f32 / width as f32 } else { 1.0 };
    column * ratio
}

/// The masonry's placement: each tile goes to the shortest column, left to right, so the reading order is the
/// result order. Answers each tile's column and top.
pub fn place(tiles: &[(u32, u32)], column: f32) -> Vec<(usize, f32)> {
    let mut tops = [0.0f32; COLUMNS];
    tiles
        .iter()
        .map(|&(w, h)| {
            let (c, top) = tops
                .iter()
                .copied()
                .enumerate()
                .fold((0, f32::MAX), |best, (i, t)| if t < best.1 { (i, t) } else { best });
            tops[c] = top + tile_height(w, h, column) + TILE_GAP;
            (c, top)
        })
        .collect()
}

/// What the picker shows.
pub struct GifPickerProps {
    pub search: AnyElement,
    pub powered: SharedString,
    pub tiles: Vec<GifTile>,
    /// The result the arrows are on.
    pub active: Option<usize>,
    /// The result being added; dimmed.
    pub picking: Option<SharedString>,
    /// The status line (loading, empty, error) and whether it is an error.
    pub status: Option<(SharedString, bool)>,
    pub more: Option<(SharedString, OnClick)>,
}

/// The picker.
pub fn gif_picker(p: GifPickerProps) -> AnyElement {
    let column = (WIDTH - 2.0 - 2.0 * PAD - (COLUMNS as f32 - 1.0) * GAP) / COLUMNS as f32;
    let dims: Vec<(u32, u32)> = p.tiles.iter().map(|t| (t.width, t.height)).collect();
    let places = place(&dims, column);
    let grid_h = places
        .iter()
        .zip(&dims)
        .map(|((_, top), &(w, h))| top + tile_height(w, h, column))
        .fold(0.0, f32::max);
    let mut grid = div().id("gif-grid").relative().h(px(grid_h)).w_full();
    for (i, (tile, (c, top))) in p.tiles.into_iter().zip(places).enumerate() {
        let h = tile_height(tile.width, tile.height, column);
        let busy = p.picking.as_ref() == Some(&tile.id);
        let active = p.active == Some(i);
        let pick = tile.on_pick.clone();
        let body: AnyElement = match tile.image {
            Some(image) => img(image).size_full().object_fit(ObjectFit::Contain).into_any_element(),
            None => div().size_full().bg(color::SURFACE_HI).into_any_element(),
        };
        let mut cell = div()
            .id(gpui::ElementId::Name(format!("gif-{}", tile.id).into()))
            .absolute()
            .left(px(c as f32 * (column + GAP)))
            .top(px(top))
            .w(px(column))
            .h(px(h))
            .overflow_hidden()
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(gpui::transparent_black())
            .rounded(radius::MD)
            .cursor_pointer()
            .on_click(move |e, w, cx| pick(e, w, cx))
            .child(body);
        if busy {
            cell = cell.opacity(0.5);
        }
        if active {
            cell = cell.border_color(color::ACCENT);
        }
        grid = grid.child(cell);
    }
    let mut foot = div().flex().items_center().justify_between().gap(px(8.0));
    if let Some((label, f)) = p.more {
        foot = foot.child(
            div()
                .id("gif-more")
                .h(px(28.0))
                .px(px(10.0))
                .flex()
                .items_center()
                .border_1()
                .border_color(color::LINE_STRONG)
                .rounded(radius::MD)
                .cursor_pointer()
                .hover(|s| s.bg(color::SURFACE_HI))
                .on_click(move |e, w, cx| f(e, w, cx))
                .child(line(label, text::UI, line_height(text::UI, LEADING), color::FG)),
        );
    }
    let status_h = line_height(text::SM, LEADING).max(20.0);
    let status = div().min_h(px(20.0)).child(match p.status {
        Some((s, error)) => line(s, text::SM, status_h, if error { color::DANGER } else { color::FG_2 }).into_any_element(),
        None => div().into_any_element(),
    });
    div()
        .id("gif-picker")
        .flex()
        .flex_col()
        .gap(px(GAP))
        .w(px(WIDTH))
        .max_h(px(MAX_H))
        .p(px(PAD))
        .bg(color::SURFACE_2)
        .border_1()
        .border_color(color::LINE_STRONG)
        .rounded(radius::XL)
        .occlude()
        .child(
            div()
                .flex()
                .items_center()
                .gap(px(8.0))
                .pb(px(8.0))
                .border_b_1()
                .border_color(color::HAIRLINE)
                .child(div().flex_1().min_w_0().child(p.search))
                .child(
                    line(p.powered, text::CHIP, line_height(text::CHIP, LEADING), color::FG_3)
                        .tracking(LABEL_TRACKING),
                ),
        )
        .child(div().id("gif-scroll").flex_1().min_h_0().overflow_y_scroll().child(grid))
        .child(status)
        .child(foot)
        .into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_masonry_fills_the_shortest_column_left_to_right() {
        let placed = place(&[(100, 100), (100, 50), (100, 100), (100, 100)], 100.0);
        assert_eq!(placed[0], (0, 0.0));
        assert_eq!(placed[1], (1, 0.0));
        assert_eq!(placed[2], (2, 0.0));
        assert_eq!(placed[3], (1, 54.0), "under the short tile");
        assert_eq!(tile_height(0, 0, 120.0), 120.0, "unknown dimensions are square");
    }
}
