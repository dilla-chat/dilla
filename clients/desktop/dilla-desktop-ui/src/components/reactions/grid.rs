//! `EmojiGrid` (packages/ui/src/EmojiGrid): the fixed reactions as a non-modal popover dialog, 320px wide: buttons in 8
//! columns of 34px cells named by their names, one tab stop that follows focus, the first (or the search field, when
//! there is one) focused on open. Arrows move in two dimensions without wrapping, Home and End go to the ends; Enter,
//! Space or a click picks. Escape closes and goes no further; a press outside the grid closes it too (except on the
//! opener, which is its own toggle). Returning focus to the opener is the host's, on [`GridEvent::Close`].
//!
//! The composer's picker (README 5.5) adds a search field over the grid that filters by name, and a footer naming the
//! emoji under the pointer or the focus (the shield, until a real hover or focus replaces it).
//!
//! The grid is a view: it keeps the query, the focused cell and the cell shown in the footer. It emits [`GridEvent`]s.

use std::rc::Rc;

use gpui::{
    AnyElement, App, AppContext as _, Bounds, Context, Entity, EventEmitter, FocusHandle, Focusable,
    InteractiveElement, IntoElement, ParentElement, Pixels, Render, SharedString,
    StatefulInteractiveElement, Styled, Subscription, Window, div, px,
};
use gpui_component::input::{Input, InputEvent, InputState};

use crate::metrics::focus_ring;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// The grid's columns (`COLUMNS`).
pub const COLUMNS: usize = 8;
/// The panel's width (`20rem`), a cell's height (`2.125rem`) and its emoji's size (`1.1875rem`).
pub const WIDTH: f32 = 320.0;
pub const CELL_HEIGHT: f32 = 34.0;
pub const CELL_FONT: f32 = 19.0;
/// The gap between cells (`2px`) and the grid's padding (`0.375rem`).
pub const CELL_GAP: f32 = 2.0;
pub const GRID_PAD: f32 = 6.0;
/// The search head's padding (`0.5rem`), its field's height (`1.875rem`) and the footer's height (`1.875rem`).
pub const HEAD_PAD: f32 = 8.0;
pub const SEARCH_HEIGHT: f32 = 30.0;
pub const FOOT_HEIGHT: f32 = 30.0;
/// The grid's cap (`12.5rem`): more rows scroll.
pub const GRID_MAX_HEIGHT: f32 = 200.0;
/// The emoji the footer names before a real hover or focus replaces it (the prototype's shield).
pub const PREVIEW: &str = "🛡️";

/// One emoji of the grid with its name (`shell.emoji.NN`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct EmojiItem {
    pub emoji: SharedString,
    pub name: SharedString,
}

/// What the grid tells its host.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum GridEvent {
    Pick(SharedString),
    /// Escape, or a press outside: the host closes the grid and gives the focus back to the opener.
    Close,
}

/// The cell to move to from `i` among `n` for `key` (a GPUI key name), or `None` when the key is not the grid's. No
/// wrapping; Home and End go to the ends; any modifier makes a key not the grid's.
pub fn grid_next(key: &str, ctrl_alt_shift: bool, i: usize, n: usize) -> Option<usize> {
    if ctrl_alt_shift || n == 0 {
        return None;
    }
    Some(match key {
        "right" if i + 1 < n => i + 1,
        "left" if i >= 1 => i - 1,
        "down" if i + COLUMNS < n => i + COLUMNS,
        "up" if i >= COLUMNS => i - COLUMNS,
        "right" | "left" | "down" | "up" => i,
        "home" => 0,
        "end" => n - 1,
        _ => return None,
    })
}

/// The cells whose name contains `query` (trimmed, lower-cased), in order; all of them for an empty query.
pub fn filter(items: &[EmojiItem], query: &str) -> Vec<usize> {
    let q = query.trim().to_lowercase();
    items
        .iter()
        .enumerate()
        .filter(|(_, i)| q.is_empty() || i.name.to_lowercase().contains(&q))
        .map(|(n, _)| n)
        .collect()
}

/// The footer's name of an emoji: `:the_name:`.
pub fn footer_name(name: &str) -> String {
    format!(":{}:", name.replace(' ', "_"))
}

/// The panel's height for `cells` cells, with or without the search head and footer.
pub fn height(cells: usize, search: bool) -> f32 {
    let rows = cells.div_ceil(COLUMNS);
    let grid = if rows == 0 {
        2.0 * GRID_PAD
    } else {
        (2.0 * GRID_PAD + rows as f32 * CELL_HEIGHT + (rows - 1) as f32 * CELL_GAP).min(GRID_MAX_HEIGHT)
    };
    let head = if search {
        2.0 * HEAD_PAD + SEARCH_HEIGHT + 1.0
    } else {
        0.0
    };
    let foot = if search { FOOT_HEIGHT } else { 0.0 };
    head + grid + foot + 2.0
}

/// Which element of the grid a decorator wraps.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Part {
    /// The search field's frame.
    Search,
    /// The `n`th cell shown (after the filter).
    Cell(usize),
}

/// Wraps an element of the grid, given the focus handle it holds (the host's automation ids).
pub type Decorate = Rc<dyn Fn(Part, &FocusHandle, AnyElement) -> AnyElement>;

struct Search {
    input: Entity<InputState>,
}

pub struct EmojiGrid {
    id: SharedString,
    label: SharedString,
    items: Vec<EmojiItem>,
    search: Option<Search>,
    handles: Vec<FocusHandle>,
    /// What `Focusable` answers when there is neither a search field nor a cell.
    fallback: FocusHandle,
    /// The cell that is the tab stop (an index into the filtered cells).
    focus: usize,
    /// The cell the footer names (an index into `items`).
    shown: Option<usize>,
    /// The opener's bounds, so a press on it is its own toggle and not an outside press.
    opener: Option<Bounds<Pixels>>,
    decorate: Option<Decorate>,
    _subscriptions: Vec<Subscription>,
}

impl EventEmitter<GridEvent> for EmojiGrid {}

impl Focusable for EmojiGrid {
    fn focus_handle(&self, cx: &App) -> FocusHandle {
        match &self.search {
            Some(s) => s.input.read(cx).focus_handle(cx),
            None => self
                .handles
                .first()
                .unwrap_or(&self.fallback)
                .clone(),
        }
    }
}

impl EmojiGrid {
    /// A grid of `items`. With `search` (the field's placeholder) it is the composer's picker.
    pub fn new(
        id: impl Into<SharedString>,
        label: impl Into<SharedString>,
        items: Vec<EmojiItem>,
        search: Option<SharedString>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let handles = items.iter().map(|_| cx.focus_handle()).collect();
        let mut subscriptions = Vec::new();
        let search = search.map(|placeholder| {
            let input = cx.new(|cx| InputState::new(window, cx).placeholder(placeholder));
            subscriptions.push(cx.subscribe_in(&input, window, Self::on_input));
            Search { input }
        });
        let shown = search
            .as_ref()
            .and_then(|_| items.iter().position(|i| i.emoji == PREVIEW));
        Self {
            id: id.into(),
            label: label.into(),
            items,
            search,
            handles,
            fallback: cx.focus_handle(),
            focus: 0,
            shown,
            opener: None,
            decorate: None,
            _subscriptions: subscriptions,
        }
    }

    /// Where the opener is, so a press on it does not count as an outside press.
    pub fn set_opener(&mut self, bounds: Option<Bounds<Pixels>>) {
        self.opener = bounds;
    }

    /// Lets the host wrap the search field and the cells (its automation ids).
    pub fn set_decorate(&mut self, decorate: Decorate) {
        self.decorate = Some(decorate);
    }

    /// The panel's label (`aria-label`).
    pub fn label(&self) -> &SharedString {
        &self.label
    }

    /// Puts the focus where the grid starts: the search field, else the first cell.
    pub fn focus_first(&self, window: &mut Window, cx: &mut App) {
        match &self.search {
            Some(s) => s.input.read(cx).focus_handle(cx).focus(window, cx),
            None => {
                if let Some(h) = self.handles.first() {
                    h.focus(window, cx);
                }
            }
        }
    }

    fn query(&self, cx: &App) -> String {
        self.search
            .as_ref()
            .map(|s| s.input.read(cx).value().to_string())
            .unwrap_or_default()
    }

    fn on_input(
        &mut self,
        _input: &Entity<InputState>,
        event: &InputEvent,
        _window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            InputEvent::Change => {
                self.focus = 0;
                cx.notify();
            }
            InputEvent::PressEnter { .. } => {
                // Enter in the search picks the first match.
                let shown = filter(&self.items, &self.query(cx));
                if let Some(&first) = shown.first() {
                    cx.emit(GridEvent::Pick(self.items[first].emoji.clone()));
                }
            }
            _ => {}
        }
    }

    fn cell(
        &self,
        pos: usize,
        index: usize,
        cx: &mut Context<Self>,
    ) -> AnyElement {
        let item = self.items[index].clone();
        let decorate = self.decorate.clone();
        let emoji = item.emoji.clone();
        let handle = self.handles[pos].clone();
        let view = cx.entity();
        let hover_view = view.clone();
        let cell = div()
            .id((SharedString::from(format!("{}-cell", self.id)), pos))
            .track_focus(&handle)
            .tab_stop(pos == self.focus)
            .flex()
            .items_center()
            .justify_center()
            .h(px(CELL_HEIGHT))
            .rounded(radius::MD)
            .cursor_pointer()
            .hover(|s| s.bg(color::SURFACE_HI))
            .focus_visible(|s| s.bg(color::SURFACE_HI).shadow(focus_ring()))
            .on_hover(move |hovered, _window, cx| {
                if *hovered {
                    hover_view.update(cx, |this, cx| {
                        if this.search.is_some() && this.shown != Some(index) {
                            this.shown = Some(index);
                            cx.notify();
                        }
                    });
                }
            })
            .child(line(
                item.emoji.clone(),
                px(CELL_FONT),
                line_height(px(CELL_FONT), 1.0),
                color::FG,
            ))
            .on_click(move |_, _window, cx| {
                let emoji = emoji.clone();
                view.update(cx, |_, cx| cx.emit(GridEvent::Pick(emoji)));
            })
            .into_any_element();
        match decorate {
            Some(d) => d(Part::Cell(pos), &handle, cell),
            None => cell,
        }
    }
}

impl Render for EmojiGrid {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let query = self.query(cx);
        let shown_cells = filter(&self.items, &query);
        // The cell the focus is in follows the handles: moving focus by Tab or a click sets the roving stop.
        if let Some(pos) = self.handles.iter().position(|h| h.is_focused(window)) {
            if pos < shown_cells.len() && pos != self.focus {
                self.focus = pos;
                self.shown = shown_cells.get(pos).copied();
            }
        }
        let view = cx.entity();
        let outside = view.clone();
        let opener = self.opener;

        let mut grid = div()
            .id(SharedString::from(format!("{}-cells", self.id)))
            .grid()
            .grid_cols(COLUMNS as u16)
            .gap(px(CELL_GAP))
            .p(px(GRID_PAD))
            .max_h(px(GRID_MAX_HEIGHT))
            .overflow_y_scroll();
        for (pos, &index) in shown_cells.iter().enumerate() {
            grid = grid.child(self.cell(pos, index, cx));
        }
        let handles = self.handles.clone();
        let n = shown_cells.len();
        let key_view = view.clone();
        let grid = grid.on_key_down(move |event, window, cx| {
            let key = &event.keystroke;
            let modified = key.modifiers.alt
                || key.modifiers.control
                || key.modifiers.platform
                || key.modifiers.shift;
            let Some(from) = handles.iter().position(|h| h.is_focused(window)) else {
                return;
            };
            if let Some(next) = grid_next(key.key.as_str(), modified, from, n) {
                cx.stop_propagation();
                if let Some(h) = handles.get(next) {
                    h.focus(window, cx);
                }
                key_view.update(cx, |this, cx| {
                    this.focus = next;
                    cx.notify();
                });
            }
        });

        let mut root = div()
            .id(self.id.clone())
            .relative()
            .flex()
            .flex_col()
            .w(px(WIDTH))
            .max_w_full()
            .overflow_hidden()
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::LINE_STRONG)
            .rounded(radius::XL)
            .shadow(shadow_2())
            .occlude()
            .on_mouse_down_out(move |event, _window, cx| {
                if opener.is_some_and(|b| b.contains(&event.position)) {
                    return;
                }
                outside.update(cx, |_, cx| cx.emit(GridEvent::Close));
            })
            .on_key_down({
                let view = view.clone();
                move |event, _window, cx| {
                    if event.keystroke.key == "escape" {
                        cx.stop_propagation();
                        view.update(cx, |_, cx| cx.emit(GridEvent::Close));
                    }
                }
            });
        if let Some(search) = &self.search {
            let first_view = view.clone();
            let handles = self.handles.clone();
            let decorate = self.decorate.clone();
            let search_focus = search.input.read(cx).focus_handle(cx);
            let field = div()
                .id(SharedString::from(format!("{}-search", self.id)))
                .flex()
                .items_center()
                .w_full()
                .h(px(SEARCH_HEIGHT))
                .px(px(10.0))
                .bg(color::BG_2)
                .border_1()
                .border_color(if search.input.focus_handle(cx).contains_focused(window, cx) {
                    color::ACCENT
                } else {
                    color::HAIRLINE_2
                })
                .rounded(radius::MD)
                .text_size(text::INPUT)
                .font_family(font::MONO)
                .text_color(color::FG)
                .child(Input::new(&search.input).appearance(false))
                .on_key_down(move |event, window, cx| {
                    if event.keystroke.key == "down" {
                        cx.stop_propagation();
                        if let Some(h) = handles.first() {
                            h.focus(window, cx);
                        }
                        first_view.update(cx, |this, cx| {
                            this.focus = 0;
                            cx.notify();
                        });
                    }
                })
                .into_any_element();
            let field = match decorate {
                Some(d) => d(Part::Search, &search_focus, field),
                None => field,
            };
            root = root.child(
                div()
                    .p(px(HEAD_PAD))
                    .border_b_1()
                    .border_color(color::HAIRLINE)
                    .child(field),
            );
        }
        root = root.child(grid);
        if self.search.is_some() {
            let named: Option<AnyElement> = self
                .shown
                .and_then(|i| self.items.get(i))
                .map(|item| {
                    div()
                        .flex()
                        .items_center()
                        .gap(px(8.0))
                        .child(line(
                            item.emoji.clone(),
                            px(15.0),
                            line_height(px(15.0), 1.0),
                            color::FG,
                        ))
                        .child(line(
                            footer_name(&item.name),
                            text::XS,
                            line_height(text::XS, 1.5),
                            color::FG_2,
                        ))
                        .into_any_element()
                });
            root = root.child(
                div()
                    .flex()
                    .items_center()
                    .h(px(FOOT_HEIGHT))
                    .px(px(10.0))
                    .border_t_1()
                    .border_color(color::HAIRLINE)
                    .children(named),
            );
        }
        root
    }
}

/// `--shadow-2`: `0 20px 50px rgba(0,0,0,0.55)`.
fn shadow_2() -> Vec<gpui::BoxShadow> {
    vec![gpui::BoxShadow {
        color: gpui::hsla(0.0, 0.0, 0.0, 0.55),
        offset: gpui::point(px(0.0), px(20.0)),
        blur_radius: px(50.0),
        spread_radius: px(0.0),
        inset: false,
    }]
}

#[cfg(test)]
mod tests {
    use super::*;

    fn items(n: usize) -> Vec<EmojiItem> {
        (0..n)
            .map(|i| EmojiItem {
                emoji: format!("e{i}").into(),
                name: format!("name {i}").into(),
            })
            .collect()
    }

    #[test]
    fn arrows_move_in_two_dimensions_without_wrapping() {
        // 20 cells: rows of 8, 8 and 4.
        assert_eq!(grid_next("right", false, 0, 20), Some(1));
        assert_eq!(grid_next("right", false, 19, 20), Some(19));
        assert_eq!(grid_next("left", false, 0, 20), Some(0));
        assert_eq!(grid_next("left", false, 5, 20), Some(4));
        assert_eq!(grid_next("down", false, 3, 20), Some(11));
        // The last row has only 4 cells: Down from column 5 of row 2 has nowhere to go.
        assert_eq!(grid_next("down", false, 13, 20), Some(13));
        assert_eq!(grid_next("down", false, 11, 20), Some(19));
        assert_eq!(grid_next("up", false, 3, 20), Some(3));
        assert_eq!(grid_next("up", false, 11, 20), Some(3));
    }

    #[test]
    fn home_and_end_go_to_the_ends_and_a_modifier_is_not_the_grids() {
        assert_eq!(grid_next("home", false, 9, 20), Some(0));
        assert_eq!(grid_next("end", false, 0, 20), Some(19));
        assert_eq!(grid_next("right", true, 0, 20), None);
        assert_eq!(grid_next("enter", false, 0, 20), None);
        assert_eq!(grid_next("right", false, 0, 0), None);
    }

    #[test]
    fn the_search_filters_by_name_ignoring_case_and_padding() {
        let all = items(12);
        assert_eq!(filter(&all, "").len(), 12);
        assert_eq!(filter(&all, "  NAME 1 "), vec![1, 10, 11]);
        assert!(filter(&all, "zzz").is_empty());
    }

    #[test]
    fn the_footer_names_the_emoji_with_underscores() {
        assert_eq!(footer_name("crossed swords"), ":crossed_swords:");
    }

    #[test]
    fn the_panel_is_as_high_as_its_rows_up_to_the_cap() {
        // 32 emoji: 4 rows. Grid 12 + 4*34 + 3*2 = 154; plain panel adds the 2px of borders.
        assert_eq!(height(32, false), 156.0);
        // With the search: head 8+30+8+1, foot 30.
        assert_eq!(height(32, true), 156.0 + 47.0 + 30.0);
        // 100 cells would be 13 rows: capped at 12.5rem.
        assert_eq!(height(100, false), 202.0);
    }
}
