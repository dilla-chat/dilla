//! The settings `row` (packages/ui/src/SettingsRow): at least 56px, 10px 16px of padding, wrapping on narrow widths: an
//! optional 32px icon tile, the label (12.5/600) over its hint (11px `--fg-2`), and the control at the right. Consecutive
//! rows draw one group with a 10px radius and hairlines between them: the host says where a row sits ([`GroupPosition`]), the
//! first adds the top edge, the last the bottom edge, and a group of n is `56n + 2` tall.
//!
//! A row with a `menu` is a tab stop that opens its context menu with a right click, Shift+F10 or the Menu key; a button
//! label adds a visible `⋯` trigger as well.

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, MouseButton, ParentElement,
    Pixels, Point, RenderOnce, SharedString, Styled, Window, div, px,
};

use super::super::action_menu::{ActionMenu, MenuItem, opens_context_menu};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING, focus_ring};
use crate::text::{line, line_height, paragraph};
use crate::tokens::{color, font, radius, text};

#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum Layout {
    /// The control at the right of the text.
    #[default]
    Inline,
    /// The control under the text, full width (a field, a list).
    Stacked,
}

/// Where a row sits in a group of consecutive rows.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum GroupPosition {
    #[default]
    Only,
    First,
    Middle,
    Last,
}

impl GroupPosition {
    /// The row's minimum height: 58 alone, 57 at either end of a group, 56 between.
    pub fn min_height(self) -> f32 {
        match self {
            Self::Only => 58.0,
            Self::First | Self::Last => 57.0,
            Self::Middle => 56.0,
        }
    }

    /// Whether the row draws its bottom edge (a row with another below it leaves it to that row's top edge).
    pub fn bottom_edge(self) -> bool {
        matches!(self, Self::Only | Self::Last)
    }

    /// Positions of `n` consecutive rows.
    pub fn of(index: usize, n: usize) -> Self {
        match (index, n) {
            (_, 0 | 1) => Self::Only,
            (0, _) => Self::First,
            (i, n) if i + 1 == n => Self::Last,
            _ => Self::Middle,
        }
    }
}

type SelectHandler = std::rc::Rc<dyn Fn(&str, &mut Window, &mut App) + 'static>;

struct RowMenu {
    label: SharedString,
    items: Vec<MenuItem>,
    button_label: Option<SharedString>,
    on_select: SelectHandler,
}

struct Memory {
    open: bool,
    at: Option<Point<Pixels>>,
    focus: gpui::FocusHandle,
}

#[derive(IntoElement)]
pub struct SettingsRow {
    id: ElementId,
    label: SharedString,
    hint: Option<SharedString>,
    icon: Option<IconName>,
    tag: Option<SharedString>,
    layout: Layout,
    position: GroupPosition,
    menu: Option<RowMenu>,
    control: Vec<AnyElement>,
}

impl SettingsRow {
    pub fn new(id: impl Into<ElementId>, label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            label: label.into(),
            hint: None,
            icon: None,
            tag: None,
            layout: Layout::Inline,
            position: GroupPosition::Only,
            menu: None,
            control: Vec::new(),
        }
    }

    pub fn hint(mut self, hint: impl Into<SharedString>) -> Self {
        self.hint = Some(hint.into());
        self
    }

    pub fn icon(mut self, icon: IconName) -> Self {
        self.icon = Some(icon);
        self
    }

    /// A small accent chip after the label.
    pub fn tag(mut self, tag: impl Into<SharedString>) -> Self {
        self.tag = Some(tag.into());
        self
    }

    pub fn layout(mut self, layout: Layout) -> Self {
        self.layout = layout;
        self
    }

    pub fn position(mut self, position: GroupPosition) -> Self {
        self.position = position;
        self
    }

    /// The row's context menu; `button_label` adds a visible `⋯` trigger named by it.
    pub fn menu(
        mut self,
        label: impl Into<SharedString>,
        items: Vec<MenuItem>,
        button_label: Option<&str>,
        on_select: impl Fn(&str, &mut Window, &mut App) + 'static,
    ) -> Self {
        self.menu = Some(RowMenu {
            label: label.into(),
            items,
            button_label: button_label.map(|l| SharedString::from(l.to_owned())),
            on_select: std::rc::Rc::new(on_select),
        });
        self
    }
}

impl ParentElement for SettingsRow {
    fn extend(&mut self, elements: impl IntoIterator<Item = AnyElement>) {
        self.control.extend(elements);
    }
}

/// `max-width: 60ch` at the hint's 11px mono advance.
fn hint_width() -> f32 {
    60.0 * f32::from(text::XS) * crate::text::MONO_ADVANCE
}

impl RenderOnce for SettingsRow {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        let stacked = self.layout == Layout::Stacked;
        let memory = window.use_keyed_state((self.id.clone(), "row"), cx, |_, cx| Memory {
            open: false,
            at: None,
            focus: cx.focus_handle().tab_stop(true),
        });

        let mut text_col = div().flex().flex_col().min_w_0();
        if !stacked {
            text_col = text_col.flex_1().min_w(px(220.0));
        }
        let mut label = div().flex().items_center().gap(px(8.0)).child(
            line(
                self.label.clone(),
                text::SM,
                line_height(text::SM, 1.4),
                color::FG,
            )
            .weight(font::WEIGHT_LABEL),
        );
        if let Some(tag) = &self.tag {
            label = label.child(
                div()
                    .px(px(5.0))
                    .rounded(radius::TAG)
                    .bg(color::ACCENT_TAG)
                    .child(
                        line(
                            tag.to_uppercase(),
                            text::CHIP,
                            line_height(text::CHIP, 1.4),
                            color::ACCENT_2,
                        )
                        .weight(font::WEIGHT_LABEL)
                        .tracking(0.06),
                    ),
            );
        }
        text_col = text_col.child(label);
        if let Some(hint) = &self.hint {
            text_col = text_col.children(paragraph(
                hint,
                text::XS,
                line_height(text::XS, 1.4),
                color::FG_2,
                hint_width(),
            ));
        }

        let mut control = div()
            .flex()
            .flex_wrap()
            .items_center()
            .gap(px(10.0))
            .min_w_0();
        control = if stacked {
            control.flex_col().items_start().w_full().gap(px(16.0))
        } else {
            control.justify_end().ml_auto()
        };
        control = control.children(self.control);

        let (open, at, focus) = memory.read_with(cx, |m, _| (m.open, m.at, m.focus.clone()));
        let mut menus: Vec<AnyElement> = Vec::new();
        let set = {
            let memory = memory.clone();
            move |open: bool, at: Option<Point<Pixels>>, cx: &mut App| {
                memory.update(cx, |m, cx| {
                    m.open = open;
                    m.at = at;
                    cx.notify();
                });
            }
        };
        if let Some(menu) = &self.menu {
            let on_select = menu.on_select.clone();
            let close = set.clone();
            // The context menu (right click, Shift+F10, the Menu key): no trigger, anchored under the row or at the pointer.
            let mut context = ActionMenu::new(
                (self.id.clone(), "context"),
                menu.label.clone(),
                menu.items.clone(),
            )
            .no_trigger(Some(&focus))
            .open(open)
            .on_select({
                let on_select = on_select.clone();
                move |id, window, cx| on_select(id, window, cx)
            })
            .on_open_change(move |open, _, cx| close(open, None, cx));
            if let Some(at) = at {
                context = context.at(at);
            }
            menus.push(context.into_any_element());
            if let Some(button) = &menu.button_label {
                control = control.child(
                    ActionMenu::new(
                        (self.id.clone(), "more"),
                        button.clone(),
                        menu.items.clone(),
                    )
                    .on_select(move |id, window, cx| on_select(id, window, cx)),
                );
            }
        }

        let mut row = div()
            .id(self.id.clone())
            .relative()
            .flex()
            .flex_wrap()
            .gap_x(px(14.0))
            .gap_y(px(10.0))
            .w_full()
            .min_h(px(self.position.min_height()))
            .min_w_0()
            .px(px(16.0))
            .py(px(10.0))
            .border_1()
            .border_color(color::HAIRLINE);
        row = match self.position {
            GroupPosition::Only => row.rounded(radius::XL),
            GroupPosition::First => row.rounded_t(radius::XL).border_b_0(),
            GroupPosition::Middle => row.border_b_0(),
            GroupPosition::Last => row.rounded_b(radius::XL),
        };
        row = if stacked {
            row.flex_col().gap(px(14.0)).p(px(16.0))
        } else {
            row.items_center()
        };
        if self.menu.is_some() {
            let key_set = set.clone();
            let mouse_set = set.clone();
            row = row
                .track_focus(&focus)
                .tab_stop(true)
                .focus_visible(|s| s.shadow(focus_ring()))
                .on_key_down(move |event, _, cx| {
                    if opens_context_menu(&event.keystroke) {
                        cx.stop_propagation();
                        key_set(true, None, cx);
                    }
                })
                .on_mouse_down(MouseButton::Right, move |event, _, cx| {
                    mouse_set(true, Some(event.position), cx);
                });
        }
        if let Some(icon) = self.icon {
            row = row.child(
                div()
                    .flex()
                    .flex_none()
                    .items_center()
                    .justify_center()
                    .size(px(32.0))
                    .rounded(radius::LG)
                    .bg(color::SURFACE_TILE)
                    .child(Icon::new(icon).size(px(15.0)).color(color::ACCENT)),
            );
        }
        row.child(text_col).child(control).children(menus)
    }
}

#[allow(dead_code)]
const _: f32 = LABEL_TRACKING;

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_group_of_n_rows_is_56n_plus_2_tall() {
        for n in 1..6 {
            let total: f32 = (0..n).map(|i| GroupPosition::of(i, n).min_height()).sum();
            assert_eq!(total, 56.0 * n as f32 + 2.0, "n = {n}");
        }
    }

    #[test]
    fn only_the_last_row_closes_a_group() {
        assert!(GroupPosition::Only.bottom_edge() && GroupPosition::Last.bottom_edge());
        assert!(!GroupPosition::First.bottom_edge() && !GroupPosition::Middle.bottom_edge());
    }

    #[test]
    fn the_hint_measure_is_sixty_characters() {
        assert!((hint_width() - 396.0).abs() < 0.01);
    }
}
