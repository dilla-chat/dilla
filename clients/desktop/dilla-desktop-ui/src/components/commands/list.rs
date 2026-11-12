//! `CommandList` (packages/ui CommandPicker, `.d-cmd-list`): the commands that match the `/` draft, over the composer.
//! No row takes focus: the draft field keeps it and ↑↓ move the selection. A greyed command stays listed at half
//! opacity and says why in its description. With no match the list says so and offers "Send as text".
//!
//! CommandPicker.css is content-box: a 40px row with 4px padding is 48px tall, the 260px list 268px.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use super::{accent_bar, shadow_2};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING_WIDE, TRACKING_TAG};
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// The list's scroll box: 260px of rows (`max-height: 16.25rem`) inside 4px of padding.
pub const LIST_MAX: f32 = 260.0;
/// One row: `min-height: 2.5rem` and 4px above and below (content-box).
pub const ROW_HEIGHT: f32 = 48.0;
const ROW_PAD_Y: f32 = 4.0;
const ROW_PAD_X: f32 = 10.0;
const SCROLL_PAD: f32 = 4.0;
const ICON_BOX: f32 = 26.0;

/// One command of the list (`CommandListItem`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ListItem {
    pub id: SharedString,
    pub icon: IconName,
    pub name: SharedString,
    pub args: SharedString,
    pub description: SharedString,
    pub source: SharedString,
    pub disabled: bool,
}

/// The list's copy (`labels`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ListLabels {
    pub title: SharedString,
    pub hint: SharedString,
    pub no_match: SharedString,
    pub send_as_text: SharedString,
}

type OnIndex = Rc<dyn Fn(usize, &mut Window, &mut App)>;
type OnClick = Rc<dyn Fn(&mut Window, &mut App)>;
type RowIds = Rc<dyn Fn(usize) -> ElementId>;

#[derive(IntoElement)]
pub struct CommandList {
    id: ElementId,
    items: Vec<ListItem>,
    active: Option<usize>,
    show_hint: bool,
    labels: ListLabels,
    on_pick: Option<OnIndex>,
    on_active: Option<OnIndex>,
    on_send_as_text: Option<OnClick>,
    row_ids: Option<RowIds>,
}

impl CommandList {
    pub fn new(id: impl Into<ElementId>, items: Vec<ListItem>, labels: ListLabels) -> Self {
        Self {
            id: id.into(),
            items,
            active: None,
            show_hint: true,
            labels,
            on_pick: None,
            on_active: None,
            on_send_as_text: None,
            row_ids: None,
        }
    }

    pub fn active(mut self, active: Option<usize>) -> Self {
        self.active = active;
        self
    }

    pub fn show_hint(mut self, show: bool) -> Self {
        self.show_hint = show;
        self
    }

    pub fn on_pick(mut self, f: impl Fn(usize, &mut Window, &mut App) + 'static) -> Self {
        self.on_pick = Some(Rc::new(f));
        self
    }

    pub fn on_active(mut self, f: impl Fn(usize, &mut Window, &mut App) + 'static) -> Self {
        self.on_active = Some(Rc::new(f));
        self
    }

    pub fn on_send_as_text(mut self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_send_as_text = Some(Rc::new(f));
        self
    }

    /// The element id of row `i` (automation); `<list id>-row-<i>` otherwise.
    pub fn row_ids(mut self, f: impl Fn(usize) -> ElementId + 'static) -> Self {
        self.row_ids = Some(Rc::new(f));
        self
    }

    /// The list's height for `rows` matching rows (0: the no-match line), header and borders included.
    pub fn height(rows: usize) -> f32 {
        let head = 2.0 * 8.0 + line_height(text::MICRO, 1.5) + 1.0;
        let body = if rows == 0 {
            2.0 * 10.0 + 28.0
        } else {
            (rows as f32 * ROW_HEIGHT).min(LIST_MAX) + 2.0 * SCROLL_PAD
        };
        2.0 + head + body
    }
}

fn row(item: &ListItem, active: bool) -> impl IntoElement {
    let name_line = line(
        format!("/{}", item.name),
        text::SM,
        line_height(text::SM, 1.35),
        color::FG,
    )
    .weight(font::WEIGHT_DISPLAY);
    let args = (!item.args.is_empty()).then(|| {
        line(
            format!(" {}", item.args),
            text::SM,
            line_height(text::SM, 1.35),
            color::FG_3,
        )
        .ellipsis()
    });
    let mut r = div()
        .relative()
        .flex()
        .items_center()
        .gap(px(10.0))
        .min_h(px(ROW_HEIGHT))
        .py(px(ROW_PAD_Y))
        .px(px(ROW_PAD_X))
        .rounded(radius::SEG)
        .overflow_hidden()
        .child(
            div()
                .flex_none()
                .size(px(ICON_BOX))
                .rounded(radius::MD)
                .bg(color::HOVER)
                .flex()
                .items_center()
                .justify_center()
                .child(Icon::new(item.icon).size(px(14.0)).color(if active {
                    color::ACCENT
                } else {
                    color::FG_2
                })),
        )
        .child(
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .flex_col()
                .child(div().flex().min_w_0().child(name_line).children(args))
                .child(
                    line(
                        item.description.clone(),
                        text::META,
                        line_height(text::META, 1.35),
                        color::FG_2,
                    )
                    .ellipsis(),
                ),
        )
        .child(
            div()
                .flex_none()
                .px(px(5.0))
                .border_1()
                .border_color(color::LINE_STRONG)
                .rounded(radius::TAG)
                .child(
                    line(item.source.clone(), text::CHIP, 15.0, color::FG_2).tracking(TRACKING_TAG),
                ),
        );
    if active {
        r = r.bg(color::SURFACE_HI).child(accent_bar());
    }
    if item.disabled {
        r = r.opacity(0.5);
    }
    r
}

impl RenderOnce for CommandList {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let head = div()
            .flex()
            .items_center()
            .gap(px(8.0))
            .py(px(8.0))
            .px(px(12.0))
            .border_b_1()
            .border_color(color::HAIRLINE)
            .child(
                line(
                    self.labels.title.to_uppercase(),
                    text::MICRO,
                    line_height(text::MICRO, 1.5),
                    color::FG_3,
                )
                .weight(font::WEIGHT_LABEL)
                .tracking(LABEL_TRACKING_WIDE),
            )
            .child(div().flex_1())
            .children(self.show_hint.then(|| {
                line(
                    self.labels.hint.clone(),
                    text::MICRO,
                    line_height(text::MICRO, 1.5),
                    color::FG_3,
                )
                .weight(font::WEIGHT_BTN)
            }));
        let list_id = self.id.clone();
        let rows: Vec<AnyElement> = self
            .items
            .iter()
            .enumerate()
            .map(|(i, item)| {
                let rid = self.row_ids.as_ref().map_or_else(
                    || ElementId::Name(format!("{list_id}-row-{i}").into()),
                    |f| f(i),
                );
                let pick = self.on_pick.clone();
                let hover = self.on_active.clone();
                div()
                    .id(rid)
                    .cursor_pointer()
                    .on_mouse_down(gpui::MouseButton::Left, move |_, window, cx| {
                        if let Some(f) = &pick {
                            f(i, window, cx);
                        }
                    })
                    .on_hover(move |&over, window, cx| {
                        if over && let Some(f) = &hover {
                            f(i, window, cx);
                        }
                    })
                    .child(row(item, self.active == Some(i)))
                    .into_any_element()
            })
            .collect();
        let none = self.items.is_empty().then(|| {
            let send = self.on_send_as_text.clone();
            div()
                .flex()
                .items_center()
                .gap(px(10.0))
                .p(px(10.0))
                .child(div().flex_1().min_w_0().child(line(
                    self.labels.no_match.clone(),
                    text::UI,
                    line_height(text::UI, 1.5),
                    color::FG_2,
                )))
                .child(
                    div()
                        .id("command-send-as-text")
                        .flex_none()
                        .flex()
                        .items_center()
                        .h(px(28.0))
                        .px(px(10.0))
                        .border_1()
                        .border_color(color::CONTROL_EDGE)
                        .rounded(radius::MD)
                        .cursor_pointer()
                        .on_click(move |_, window, cx| {
                            if let Some(f) = &send {
                                f(window, cx);
                            }
                        })
                        .child(line(
                            self.labels.send_as_text.clone(),
                            text::UI,
                            line_height(text::UI, 1.5),
                            color::FG,
                        )),
                )
        });
        div()
            .id(self.id)
            .overflow_hidden()
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::LINE_STRONG)
            .rounded(radius::XL)
            .shadow(shadow_2())
            .child(head)
            .child(
                div()
                    .id("command-list-scroll")
                    .max_h(px(LIST_MAX + 2.0 * SCROLL_PAD))
                    .overflow_y_scroll()
                    .when_some_rows(!rows.is_empty(), SCROLL_PAD)
                    .children(rows)
                    .children(none),
            )
    }
}

trait PadWhen {
    fn when_some_rows(self, rows: bool, pad: f32) -> Self;
}

impl PadWhen for gpui::Stateful<gpui::Div> {
    fn when_some_rows(self, rows: bool, pad: f32) -> Self {
        if rows { self.p(px(pad)) } else { self }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn heights_follow_the_content_box_rules() {
        // Header: 8 + 15 + 8 + 1 border; rows 48 each to 260, plus 4+4 padding; plus the 1px borders.
        assert_eq!(CommandList::height(1), 2.0 + 32.0 + 48.0 + 8.0);
        assert_eq!(CommandList::height(10), 2.0 + 32.0 + 260.0 + 8.0);
        assert_eq!(CommandList::height(0), 2.0 + 32.0 + 48.0);
    }
}
