//! `RemindersPopover` (packages/ui RemindersPopover): the 342px panel under the bell. Its head is the Bot's tile,
//! "Reminders" and the Due / Upcoming tabs with their counts; then the rows of the picked tab (a 26px alarm-clock tile,
//! `--warn` on a due one, the text, "when · where", and Dismiss or Cancel) or the tab's empty line; then the footer note.

use std::rc::Rc;

use gpui::{
    App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use super::shadow_popover;
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::text::{line, line_height, paragraph};
use crate::tokens::{color, font, radius, text};

/// The panel's width: the prototype's 340 content and its 1px border.
pub const WIDTH: f32 = 342.0;

/// One reminder (`ReminderItem`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Item {
    pub id: SharedString,
    pub text: SharedString,
    pub when: SharedString,
    pub place: SharedString,
    pub due: bool,
    pub busy: bool,
}

/// Which tab is shown.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Tab {
    Due,
    Upcoming,
}

/// The popover's copy.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Labels {
    pub title: SharedString,
    pub due: SharedString,
    pub upcoming: SharedString,
    pub dismiss: SharedString,
    pub cancel: SharedString,
    pub empty_due: SharedString,
    pub empty_upcoming: SharedString,
    pub footer: SharedString,
}

type OnTab = Rc<dyn Fn(Tab, &mut Window, &mut App)>;
type OnAction = Rc<dyn Fn(usize, &mut Window, &mut App)>;

#[derive(IntoElement)]
pub struct RemindersPopover {
    id: ElementId,
    items: Vec<Item>,
    tab: Tab,
    bot_initials: Option<SharedString>,
    status: Option<SharedString>,
    labels: Labels,
    on_tab: Option<OnTab>,
    on_action: Option<OnAction>,
}

impl RemindersPopover {
    pub fn new(id: impl Into<ElementId>, items: Vec<Item>, tab: Tab, labels: Labels) -> Self {
        Self {
            id: id.into(),
            items,
            tab,
            bot_initials: None,
            status: None,
            labels,
            on_tab: None,
            on_action: None,
        }
    }

    pub fn bot_initials(mut self, initials: impl Into<SharedString>) -> Self {
        self.bot_initials = Some(initials.into());
        self
    }

    pub fn status(mut self, status: Option<SharedString>) -> Self {
        self.status = status;
        self
    }

    pub fn on_tab(mut self, f: impl Fn(Tab, &mut Window, &mut App) + 'static) -> Self {
        self.on_tab = Some(Rc::new(f));
        self
    }

    /// The row's one action (Dismiss a due one, Cancel an upcoming one), by its index in `items`.
    pub fn on_action(mut self, f: impl Fn(usize, &mut Window, &mut App) + 'static) -> Self {
        self.on_action = Some(Rc::new(f));
        self
    }
}

impl RenderOnce for RemindersPopover {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let xs = line_height(text::XS, 1.5);
        let due_count = self.items.iter().filter(|i| i.due).count();
        let upcoming_count = self.items.len() - due_count;
        let tab_button = |which: Tab, label: SharedString, count: usize, on: Option<OnTab>| {
            let selected = which == self.tab;
            div()
                .id(match which {
                    Tab::Due => "reminders-tab-due",
                    Tab::Upcoming => "reminders-tab-upcoming",
                })
                .flex()
                .items_center()
                .gap(px(5.0))
                .h(px(22.0))
                .px(px(8.0))
                .rounded(radius::SM)
                .cursor_pointer()
                .when_bg(selected, color::HOVER_HI)
                .on_click(move |_, window, cx| {
                    if let Some(f) = &on {
                        f(which, window, cx);
                    }
                })
                .child(line(
                    label,
                    text::XS,
                    xs,
                    if selected { color::FG } else { color::FG_2 },
                ))
                .child(div().opacity(0.7).child(line(
                    count.to_string(),
                    text::XS,
                    xs,
                    if selected { color::FG } else { color::FG_2 },
                )))
        };
        let head = div()
            .flex()
            .items_center()
            .gap(px(8.0))
            .py(px(10.0))
            .px(px(12.0))
            .border_b_1()
            .border_color(color::HAIRLINE)
            .children(self.bot_initials.clone().map(|initials| {
                div()
                    .flex_none()
                    .size(px(22.0))
                    .rounded(radius::ROW)
                    .bg(color::BRAND)
                    .flex()
                    .items_center()
                    .justify_center()
                    .child(
                        line(
                            initials,
                            text::NANO,
                            line_height(text::NANO, 1.5),
                            color::BRAND_INK,
                        )
                        .weight(font::WEIGHT_DISPLAY),
                    )
            }))
            .child(
                div().flex_1().child(
                    line(
                        self.labels.title.clone(),
                        text::SM,
                        line_height(text::SM, 1.5),
                        color::FG,
                    )
                    .weight(font::WEIGHT_DISPLAY),
                ),
            )
            .child(
                div()
                    .flex()
                    .p(px(2.0))
                    .bg(color::BG_2)
                    .border_1()
                    .border_color(color::HAIRLINE_2)
                    .rounded(radius::MD)
                    .child(tab_button(
                        Tab::Due,
                        self.labels.due.clone(),
                        due_count,
                        self.on_tab.clone(),
                    ))
                    .child(tab_button(
                        Tab::Upcoming,
                        self.labels.upcoming.clone(),
                        upcoming_count,
                        self.on_tab.clone(),
                    )),
            );
        let shown: Vec<(usize, &Item)> = self
            .items
            .iter()
            .enumerate()
            .filter(|(_, i)| i.due == (self.tab == Tab::Due))
            .collect();
        let empty = if self.tab == Tab::Due {
            self.labels.empty_due.clone()
        } else {
            self.labels.empty_upcoming.clone()
        };
        let sm = line_height(text::SM, 1.35);
        let meta = line_height(text::META, 1.35);
        let rows = shown.iter().map(|&(index, item)| {
            let act = self.on_action.clone();
            let label = if item.due {
                self.labels.dismiss.clone()
            } else {
                self.labels.cancel.clone()
            };
            let where_ = if item.place.is_empty() {
                item.when.to_string()
            } else {
                format!("{} · {}", item.when, item.place)
            };
            div()
                .flex()
                .items_start()
                .gap(px(10.0))
                .py(px(10.0))
                .px(px(8.0))
                .rounded(radius::SEG)
                .child(
                    div()
                        .flex_none()
                        .size(px(26.0))
                        .rounded(radius::MD)
                        .bg(if item.due {
                            color::WARN_TINT
                        } else {
                            color::HOVER
                        })
                        .flex()
                        .items_center()
                        .justify_center()
                        .child(
                            Icon::new(IconName::AlarmClock)
                                .size(px(13.0))
                                .color(if item.due { color::WARN } else { color::FG_2 }),
                        ),
                )
                .child(
                    div()
                        .flex_1()
                        .min_w_0()
                        .flex()
                        .flex_col()
                        .gap(px(2.0))
                        .children(paragraph(
                            &item.text,
                            text::SM,
                            sm,
                            color::FG,
                            WIDTH - 2.0 - 8.0 - 16.0 - 26.0 - 20.0 - 70.0,
                        ))
                        .child(line(where_, text::META, meta, color::FG_3)),
                )
                .child(
                    div()
                        .id(("reminder-action", index))
                        .flex_none()
                        .flex()
                        .items_center()
                        .h(px(26.0))
                        .px(px(8.0))
                        .border_1()
                        .border_color(color::LINE_STRONG)
                        .rounded(radius::ROW)
                        .cursor_pointer()
                        .opacity(if item.busy { 0.5 } else { 1.0 })
                        .on_click(move |_, window, cx| {
                            if let Some(f) = &act {
                                f(index, window, cx);
                            }
                        })
                        .child(line(
                            label,
                            text::META,
                            line_height(text::META, 1.5),
                            color::FG_CHIP,
                        )),
                )
        });
        let ui = line_height(text::UI, 1.5);
        let body = div()
            .id("reminders-body")
            .max_h(px(300.0))
            .overflow_y_scroll()
            .p(px(4.0))
            .children(self.status.clone().map(|s| {
                div()
                    .py(px(8.0))
                    .px(px(12.0))
                    .flex()
                    .justify_center()
                    .child(line(s, text::UI, ui, color::FG_3))
            }))
            .children(shown.is_empty().then(|| {
                div()
                    .py(px(24.0))
                    .px(px(12.0))
                    .flex()
                    .justify_center()
                    .child(line(empty, text::UI, ui, color::FG_3))
            }))
            .children(rows);
        div()
            .id(self.id)
            .w(px(WIDTH))
            .overflow_hidden()
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::LINE_STRONG)
            .rounded(radius::XL)
            .shadow(shadow_popover())
            .child(head)
            .child(body)
            .child(
                div()
                    .py(px(8.0))
                    .px(px(12.0))
                    .border_t_1()
                    .border_color(color::HAIRLINE)
                    .child(line(
                        self.labels.footer.clone(),
                        text::META,
                        line_height(text::META, 1.5),
                        color::FG_3,
                    )),
            )
    }
}

trait When: Sized {
    fn when_bg(self, on: bool, bg: gpui::Hsla) -> Self;
}

impl When for gpui::Stateful<gpui::Div> {
    fn when_bg(self, on: bool, bg: gpui::Hsla) -> Self {
        if on { self.bg(bg) } else { self }
    }
}
