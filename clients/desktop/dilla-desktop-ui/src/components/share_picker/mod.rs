//! `ScreenSourcePicker` (packages/ui/src/ScreenSourcePicker, beta.5's desktop picker window): "Share your screen" in a
//! 680px Signal dialog, Screens and Windows tabs over a grid of 16:9 thumbnails with each source's name under it. A tile
//! is selected by a press or the arrows (selection follows focus); Share, Enter or a double press shares it; Cancel or
//! Escape shares nothing.
//!
//! ScreenSourcePicker.css: the tabs 16px apart over a `--hairline` (12px `--fg-3`, the open one `--fg` bold on a 2px
//! `--accent` underline, 8px under the label); the panel at most `min(52vh, 420px)`, 2px in; the grid
//! `repeat(auto-fill, minmax(180px, 1fr))` 12px apart; a tile 6px in a 1px edge, 10px corners, 6px between the thumbnail
//! and the name, `--hover` under the pointer, the selected one `--surface-2` in the `--accent` edge; the thumbnail
//! `--bg-2` in a `--hairline-2` edge, 6px corners, the image contained (no image: the monitor or app-window glyph in
//! `--fg-3`); the name 12.5px `--fg-2` (selected `--fg`) on one line with an ellipsis.

use std::rc::Rc;
use std::sync::Arc;

use gpui::{
    App, ElementId, ImageSource, InteractiveElement, IntoElement, ParentElement, RenderImage,
    RenderOnce, SharedString, StatefulInteractiveElement, Styled, StyledImage, Window, div, img,
    px,
};

use super::button::{Button, ButtonSize, ButtonVariant};
use super::dialog::{Dialog, InitialFocus};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::focus_ring;
use crate::roving::focus_handles;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// `.d-dialog:has(.d-screen-picker)`'s width.
pub const WIDTH: f32 = 680.0;
/// `minmax(180px, 1fr)` and the grid's gap.
pub const MIN_TILE: f32 = 180.0;
pub const GAP: f32 = 12.0;
/// The panel's most height: `min(52vh, 420px)`.
pub fn panel_max_height(viewport_height: f32) -> f32 {
    (viewport_height * 0.52).min(420.0)
}

/// A screen or a window.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum TileKind {
    Screen,
    Window,
}

/// One source offered for sharing. `key` is stable across refreshes (a refresh keeps the selection).
#[derive(Clone)]
pub struct PickerTile {
    pub key: SharedString,
    pub name: SharedString,
    pub kind: TileKind,
    pub thumbnail: Option<Arc<RenderImage>>,
}

/// The picker's copy.
#[derive(Clone)]
pub struct PickerLabels {
    pub title: SharedString,
    pub screens: SharedString,
    pub windows: SharedString,
    pub share: SharedString,
    pub cancel: SharedString,
    pub empty: SharedString,
}

type OnTab = Rc<dyn Fn(TileKind, &mut Window, &mut App)>;
type OnTile = Rc<dyn Fn(SharedString, &mut Window, &mut App)>;
type OnCancel = Rc<dyn Fn(&mut Window, &mut App)>;

/// How many tiles a row holds in a grid `width` wide (`auto-fill`), and each tile's width.
pub fn columns(width: f32) -> (usize, f32) {
    let n = ((width + GAP) / (MIN_TILE + GAP)).floor().max(1.0) as usize;
    (n, (width - GAP * (n as f32 - 1.0)) / n as f32)
}

/// The tile the key moves to from `from` in a grid of `len` tiles, `columns` a row (ScreenSourcePicker `onTileKey`):
/// the arrows step (Up and Down by a row) and stop at the ends; Home and End jump.
pub fn grid_next(key: &str, from: usize, len: usize, columns: usize) -> Option<usize> {
    if len == 0 {
        return None;
    }
    let last = len - 1;
    let c = columns.max(1);
    match key {
        "home" => Some(0),
        "end" => Some(last),
        "right" => Some((from + 1).min(last)),
        "left" => Some(from.saturating_sub(1)),
        "down" => Some((from + c).min(last)),
        "up" => Some(from.saturating_sub(c)),
        _ => None,
    }
}

#[derive(IntoElement)]
pub struct SharePicker {
    id: ElementId,
    tiles: Vec<PickerTile>,
    tab: TileKind,
    selected: Option<SharedString>,
    labels: PickerLabels,
    on_tab: Option<OnTab>,
    on_select: Option<OnTile>,
    on_share: Option<OnTile>,
    on_cancel: Option<OnCancel>,
}

impl SharePicker {
    pub fn new(id: impl Into<ElementId>, labels: PickerLabels) -> Self {
        Self {
            id: id.into(),
            tiles: Vec::new(),
            tab: TileKind::Screen,
            selected: None,
            labels,
            on_tab: None,
            on_select: None,
            on_share: None,
            on_cancel: None,
        }
    }

    /// Every source, both kinds.
    pub fn tiles(mut self, tiles: Vec<PickerTile>) -> Self {
        self.tiles = tiles;
        self
    }

    pub fn tab(mut self, tab: TileKind) -> Self {
        self.tab = tab;
        self
    }

    /// The chosen tile; one the current tab does not show selects nothing.
    pub fn selected(mut self, key: Option<SharedString>) -> Self {
        self.selected = key;
        self
    }

    pub fn on_tab(mut self, f: impl Fn(TileKind, &mut Window, &mut App) + 'static) -> Self {
        self.on_tab = Some(Rc::new(f));
        self
    }

    pub fn on_select(mut self, f: impl Fn(SharedString, &mut Window, &mut App) + 'static) -> Self {
        self.on_select = Some(Rc::new(f));
        self
    }

    pub fn on_share(mut self, f: impl Fn(SharedString, &mut Window, &mut App) + 'static) -> Self {
        self.on_share = Some(Rc::new(f));
        self
    }

    pub fn on_cancel(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_cancel = Some(Rc::new(f));
        self
    }
}

impl RenderOnce for SharePicker {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let viewport = window.viewport_size();
        let dialog_w = WIDTH.min(f32::from(viewport.width) - 24.0);
        // The body inside the dialog's 1px edge and 16px padding, the panel 2px in from that.
        let grid_w = dialog_w - 2.0 - 32.0 - 4.0;
        let (cols, tile_w) = columns(grid_w);
        let shown: Vec<PickerTile> = self
            .tiles
            .iter()
            .filter(|t| t.kind == self.tab)
            .cloned()
            .collect();
        let selected = self
            .selected
            .clone()
            .filter(|k| shown.iter().any(|t| &t.key == k));
        let id = self.id.clone();
        let kinds = [TileKind::Screen, TileKind::Window];
        let tab_handles = focus_handles((id.clone(), "tab-focus"), 2, window, cx);
        let tile_handles = focus_handles((id.clone(), "tile-focus"), shown.len(), window, cx);
        let on_tab = self.on_tab.clone();
        let on_select = self.on_select.clone();
        let on_share = self.on_share.clone();

        // The tabs: Screens, Windows; the arrows, Home and End move between them (and show the other kind).
        let mut tabs = div()
            .flex()
            .gap(px(16.0))
            .border_b_1()
            .border_color(color::HAIRLINE);
        for (i, kind) in kinds.into_iter().enumerate() {
            let open = kind == self.tab;
            let label = match kind {
                TileKind::Screen => self.labels.screens.clone(),
                TileKind::Window => self.labels.windows.clone(),
            };
            let (pick, key_pick) = (on_tab.clone(), on_tab.clone());
            let handles = tab_handles.clone();
            tabs = tabs.child(
                div()
                    .id((id.clone(), format!("tab-{i}")))
                    .track_focus(&tab_handles[i].clone().tab_stop(open))
                    .mb(px(-1.0))
                    .pb(px(8.0))
                    .border_b_2()
                    .border_color(if open {
                        color::ACCENT
                    } else {
                        gpui::transparent_black()
                    })
                    .cursor_pointer()
                    .focus_visible(|s| s.shadow(focus_ring()))
                    .child(
                        line(
                            label,
                            text::UI,
                            line_height(text::UI, 1.3),
                            if open { color::FG } else { color::FG_3 },
                        )
                        .weight(if open {
                            font::WEIGHT_DISPLAY
                        } else {
                            font::WEIGHT_BODY
                        }),
                    )
                    .on_click(move |_, window, cx| {
                        if let Some(f) = &pick {
                            f(kind, window, cx);
                        }
                    })
                    .on_key_down(move |event, window, cx| {
                        let to = match event.keystroke.key.as_str() {
                            "right" | "left" => Some(1 - i),
                            "home" => Some(0),
                            "end" => Some(1),
                            _ => None,
                        };
                        if let Some(to) = to {
                            cx.stop_propagation();
                            handles[to].focus(window, cx);
                            if let Some(f) = &key_pick {
                                f(kinds[to], window, cx);
                            }
                        }
                    }),
            );
        }

        let thumb_w = tile_w - 14.0;
        let thumb_h = thumb_w * 9.0 / 16.0;
        let panel = if shown.is_empty() {
            div().py(px(24.0)).flex().justify_center().child(line(
                self.labels.empty.clone(),
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_3,
            ))
        } else {
            let mut grid = div().flex().flex_wrap().gap(px(GAP));
            let tab_stop = selected
                .clone()
                .or_else(|| shown.first().map(|t| t.key.clone()));
            let len = shown.len();
            for (index, tile) in shown.iter().enumerate() {
                let chosen = selected.as_ref() == Some(&tile.key);
                let thumb = div()
                    .w(px(thumb_w))
                    .h(px(thumb_h))
                    .flex()
                    .items_center()
                    .justify_center()
                    .overflow_hidden()
                    .bg(color::BG_2)
                    .border_1()
                    .border_color(color::HAIRLINE_2)
                    .rounded(radius::MD)
                    .child(match &tile.thumbnail {
                        Some(image) => img(ImageSource::Render(image.clone()))
                            .size_full()
                            .object_fit(gpui::ObjectFit::Contain)
                            .into_any_element(),
                        None => Icon::new(match tile.kind {
                            TileKind::Window => IconName::AppWindow,
                            TileKind::Screen => IconName::Monitor,
                        })
                        .color(color::FG_3)
                        .into_any_element(),
                    });
                let name = div().min_w_0().overflow_hidden().child(
                    line(
                        tile.name.clone(),
                        text::SM,
                        line_height(text::SM, 1.3),
                        if chosen { color::FG } else { color::FG_2 },
                    )
                    .ellipsis(),
                );
                let (key_click, key_double, key_keys) =
                    (tile.key.clone(), tile.key.clone(), tile.key.clone());
                let (sel, share, sel_keys, share_keys) = (
                    on_select.clone(),
                    on_share.clone(),
                    on_select.clone(),
                    on_share.clone(),
                );
                let handles = tile_handles.clone();
                let keys: Vec<SharedString> = shown.iter().map(|t| t.key.clone()).collect();
                let stop = tab_stop.as_ref() == Some(&tile.key);
                grid = grid.child(
                    div()
                        .id((id.clone(), format!("tile-{index}")))
                        .track_focus(&tile_handles[index].clone().tab_stop(stop))
                        .w(px(tile_w))
                        .flex()
                        .flex_col()
                        .gap(px(6.0))
                        .min_w_0()
                        .p(px(6.0))
                        .border_1()
                        .border_color(if chosen {
                            color::ACCENT
                        } else {
                            gpui::transparent_black()
                        })
                        .rounded(radius::XL)
                        .when_chosen(chosen)
                        .cursor_pointer()
                        .focus_visible(|s| s.shadow(focus_ring()))
                        .child(thumb)
                        .child(name)
                        .on_click(move |event, window, cx| {
                            if event.click_count() >= 2 {
                                if let Some(f) = &share {
                                    f(key_double.clone(), window, cx);
                                }
                            } else if let Some(f) = &sel {
                                f(key_click.clone(), window, cx);
                            }
                        })
                        .on_key_down(move |event, window, cx| {
                            let key = event.keystroke.key.as_str();
                            if key == "enter" {
                                cx.stop_propagation();
                                if let Some(f) = &share_keys {
                                    f(key_keys.clone(), window, cx);
                                }
                                return;
                            }
                            if key == "space" {
                                cx.stop_propagation();
                                if let Some(f) = &sel_keys {
                                    f(key_keys.clone(), window, cx);
                                }
                                return;
                            }
                            let m = &event.keystroke.modifiers;
                            if m.alt || m.control || m.platform || m.shift {
                                return;
                            }
                            if let Some(to) = grid_next(key, index, len, cols) {
                                cx.stop_propagation();
                                handles[to].focus(window, cx);
                                if let Some(f) = &sel_keys {
                                    f(keys[to].clone(), window, cx);
                                }
                            }
                        }),
                );
            }
            div().child(grid)
        };
        let body = div().flex().flex_col().gap(px(12.0)).child(tabs).child(
            div()
                .id((id.clone(), "panel"))
                .max_h(px(panel_max_height(f32::from(viewport.height))))
                .overflow_y_scroll()
                .p(px(2.0))
                .child(panel),
        );
        let share_key = selected.clone();
        let share = self.on_share.clone();
        let cancel = self.on_cancel.clone();
        Dialog::new(
            id.clone(),
            self.labels.title.clone(),
            self.labels.cancel.clone(),
        )
        .width(WIDTH)
        .initial_focus(InitialFocus::Handle(
            tab_handles[if self.tab == TileKind::Screen { 0 } else { 1 }].clone(),
        ))
        .body(body)
        .footer(
            Button::new((id.clone(), "share"), self.labels.share.clone())
                .variant(ButtonVariant::Accent)
                .size(ButtonSize::Dialog)
                .disabled(share_key.is_none())
                .on_click(move |_, window, cx| {
                    if let (Some(k), Some(f)) = (&share_key, &share) {
                        f(k.clone(), window, cx);
                    }
                }),
        )
        .on_close(move |window, cx| {
            if let Some(f) = &cancel {
                f(window, cx);
            }
        })
    }
}

/// The selected tile's colours: `--surface-2` under it; the pointer's `--hover` otherwise.
trait ChosenTile {
    fn when_chosen(self, chosen: bool) -> Self;
}

impl ChosenTile for gpui::Stateful<gpui::Div> {
    fn when_chosen(self, chosen: bool) -> Self {
        if chosen {
            self.bg(color::SURFACE_2)
        } else {
            self.hover(|s| s.bg(color::HOVER))
        }
    }
}

/// The tile width the picker lays out at `viewport_width` (tests and parity fixtures).
pub fn tile_width(viewport_width: f32) -> f32 {
    columns(WIDTH.min(viewport_width - 24.0) - 38.0).1
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn three_tiles_fill_a_row_of_the_680px_dialog() {
        // 680 - 2 (edge) - 32 (body) - 4 (panel) = 642: three 206px tiles 12px apart.
        assert_eq!(columns(642.0), (3, 206.0));
        assert_eq!(tile_width(1280.0), 206.0);
        // A narrow window: one column.
        assert_eq!(columns(150.0).0, 1);
        assert_eq!(panel_max_height(600.0), 312.0);
        assert_eq!(panel_max_height(1200.0), 420.0);
    }

    #[test]
    fn the_arrows_move_through_the_grid_by_tile_and_by_row() {
        assert_eq!(grid_next("right", 0, 5, 3), Some(1));
        assert_eq!(grid_next("right", 4, 5, 3), Some(4), "stops at the end");
        assert_eq!(grid_next("left", 0, 5, 3), Some(0));
        assert_eq!(grid_next("down", 1, 5, 3), Some(4));
        assert_eq!(grid_next("down", 3, 5, 3), Some(4), "the last row's end");
        assert_eq!(grid_next("up", 4, 5, 3), Some(1));
        assert_eq!(grid_next("home", 3, 5, 3), Some(0));
        assert_eq!(grid_next("end", 0, 5, 3), Some(4));
        assert_eq!(grid_next("a", 0, 5, 3), None);
        assert_eq!(grid_next("down", 0, 0, 3), None);
    }
}
