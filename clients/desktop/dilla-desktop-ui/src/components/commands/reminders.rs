//! `RemindersPopover` (packages/ui RemindersPopover): the 342px panel under the bell. Its head is the Bot's tile,
//! "Reminders" and the Due / Upcoming tabs with their counts; then the rows of the picked tab (a 26px alarm-clock tile,
//! `--warn` on a due one, the text, "when · where", and Dismiss or Cancel) or the tab's empty line; then the footer note.
//!
//! It draws only: the host owns the list, the picked tab, the tabs' focus handles (the picked tab takes the focus when
//! the panel opens, ←→ Home End move between the tabs) and what Escape and a press outside do.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, FocusHandle, InteractiveElement, IntoElement, KeyDownEvent,
    ParentElement, RenderOnce, SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use super::shadow_popover;
use crate::a11y::{Accessible, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{OPACITY_MUTED, focus_ring};
use crate::text::{flow_paragraph, line, line_height};
use crate::tokens::{color, font, radius, text};

/// The panel's width: the prototype's 340 content and its 1px border.
pub const WIDTH: f32 = 342.0;
/// `.d-reminders__row`: 10px between the tile, the text and the action, 8px each side.
const ROW_GAP: f32 = 10.0;
const ROW_PAD_X: f32 = 8.0;
/// `.d-reminders__icon`.
const TILE: f32 = 26.0;
/// `.d-reminders__body`'s padding.
const BODY_PAD: f32 = 4.0;
/// `.d-reminders__action`'s padding each side and its border.
const ACTION_PAD_X: f32 = 8.0;
/// `.d-reminders__text`'s line height.
const TEXT_LINE: f32 = 1.35;

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

impl Tab {
    /// The tab's automation id.
    pub fn id(self) -> &'static str {
        match self {
            Self::Due => "reminders-tab-due",
            Self::Upcoming => "reminders-tab-upcoming",
        }
    }

    /// `onTabKey`: ←→ flip, Home is Due, End is Upcoming; `None` for any other key.
    pub fn after_key(self, key: &str) -> Option<Self> {
        match key {
            "home" => Some(Self::Due),
            "end" => Some(Self::Upcoming),
            "left" | "right" => Some(match self {
                Self::Due => Self::Upcoming,
                Self::Upcoming => Self::Due,
            }),
            _ => None,
        }
    }
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
/// Draws an extra child into the element named by its id (the app's automation bounds probe).
pub type Probe = Rc<dyn Fn(&str) -> AnyElement>;

#[derive(IntoElement)]
pub struct RemindersPopover {
    id: ElementId,
    items: Vec<Item>,
    tab: Tab,
    bot_initials: Option<SharedString>,
    status: Option<SharedString>,
    loading: bool,
    labels: Labels,
    tab_focus: Option<(FocusHandle, FocusHandle)>,
    on_tab: Option<OnTab>,
    on_action: Option<OnAction>,
    probe: Option<Probe>,
}

impl RemindersPopover {
    pub fn new(id: impl Into<ElementId>, items: Vec<Item>, tab: Tab, labels: Labels) -> Self {
        Self {
            id: id.into(),
            items,
            tab,
            bot_initials: None,
            status: None,
            loading: false,
            labels,
            tab_focus: None,
            on_tab: None,
            on_action: None,
            probe: None,
        }
    }

    pub fn bot_initials(mut self, initials: impl Into<SharedString>) -> Self {
        self.bot_initials = Some(initials.into());
        self
    }

    /// A loading or error line over the list.
    pub fn status(mut self, status: Option<SharedString>) -> Self {
        self.status = status;
        self
    }

    /// The status is the loading line: the list is not known yet, so the tab's empty line is not drawn under it.
    pub fn loading(mut self, loading: bool) -> Self {
        self.loading = loading;
        self
    }

    /// The tabs' focus handles (Due, Upcoming): the picked one is the panel's one tab stop.
    pub fn tab_focus(mut self, due: FocusHandle, upcoming: FocusHandle) -> Self {
        self.tab_focus = Some((due, upcoming));
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

    /// Draws `probe(id)` into the tabs (`reminders-tab-due`, `reminders-tab-upcoming`), each row (`reminder-<id>`) and its
    /// action (`reminder-action-<id>`).
    pub fn probe(mut self, probe: Probe) -> Self {
        self.probe = Some(probe);
        self
    }
}

impl RenderOnce for RemindersPopover {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let xs = line_height(text::XS, 1.5);
        let due_count = self.items.iter().filter(|i| i.due).count();
        let upcoming_count = self.items.len() - due_count;
        let probe = self.probe.clone();
        let probed = move |id: &str| probe.as_ref().map(|p| p(id));
        let tab_button = |which: Tab, label: SharedString, count: usize| {
            let selected = which == self.tab;
            let fg = if selected { color::FG } else { color::FG_2 };
            let (on_click, on_key) = (self.on_tab.clone(), self.on_tab.clone());
            let mut tab = div()
                .id(which.id())
                .a11y(Role::Tab, format!("{label}, {count}"))
                .aria_selected(selected)
                .relative()
                .flex()
                .items_center()
                .gap(px(5.0))
                .h(px(22.0))
                .px(px(8.0))
                .rounded(radius::SM)
                .cursor_pointer()
                .focus_visible(|s| s.shadow(focus_ring()))
                .when_bg(selected, color::HOVER_HI)
                .on_click(move |_, window, cx| {
                    if let Some(f) = &on_click {
                        f(which, window, cx);
                    }
                })
                .on_key_down(move |e: &KeyDownEvent, window, cx| {
                    if let (Some(next), Some(f)) =
                        (self.tab.after_key(e.keystroke.key.as_str()), &on_key)
                    {
                        cx.stop_propagation();
                        f(next, window, cx);
                    }
                })
                .child(line(label, text::XS, xs, fg))
                .child(
                    div()
                        .opacity(0.7)
                        .child(line(count.to_string(), text::XS, xs, fg)),
                )
                .children(probed(which.id()));
            if let Some((due, upcoming)) = &self.tab_focus {
                let handle = if which == Tab::Due { due } else { upcoming };
                tab = tab.track_focus(&handle.clone().tab_stop(selected));
            }
            tab
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
                    .child(tab_button(Tab::Due, self.labels.due.clone(), due_count))
                    .child(tab_button(
                        Tab::Upcoming,
                        self.labels.upcoming.clone(),
                        upcoming_count,
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
        let sm = line_height(text::SM, TEXT_LINE);
        let meta = line_height(text::META, TEXT_LINE);
        let action_line = line_height(text::META, 1.5);
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
            let row_id = format!("reminder-{}", item.id);
            let action_id = format!("reminder-action-{}", item.id);
            let busy = item.busy;
            let mut action = div()
                .id(SharedString::from(action_id.clone()))
                .relative()
                .flex_none()
                .flex()
                .items_center()
                .h(px(TILE))
                .px(px(ACTION_PAD_X))
                .border_1()
                .border_color(color::LINE_STRONG)
                .rounded(radius::ROW)
                .child(line(label, text::META, action_line, color::FG_CHIP))
                .children(probed(&action_id));
            action = if busy {
                action.opacity(OPACITY_MUTED).cursor_not_allowed()
            } else {
                action
                    .cursor_pointer()
                    .hover(|s| s.border_color(color::ACCENT))
                    .on_click(move |_, window, cx| {
                        if let Some(f) = &act {
                            f(index, window, cx);
                        }
                    })
            };
            div()
                .id(SharedString::from(row_id.clone()))
                .relative()
                .flex()
                .items_start()
                .gap(px(ROW_GAP))
                .py(px(10.0))
                .px(px(ROW_PAD_X))
                .rounded(radius::SEG)
                .hover(|s| s.bg(color::HOVER))
                .child(
                    div()
                        .flex_none()
                        .size(px(TILE))
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
                        // `overflow-wrap: anywhere` in what the row leaves the text.
                        .child(flow_paragraph(item.text.clone(), text::SM, sm, color::FG))
                        .child(line(where_, text::META, meta, color::FG_3)),
                )
                .child(action)
                .children(probed(&row_id))
        });
        let ui = line_height(text::UI, 1.5);
        let body = div()
            .id("reminders-body")
            .max_h(px(300.0))
            .overflow_y_scroll()
            .p(px(BODY_PAD))
            .children(self.status.clone().map(|s| {
                div()
                    .py(px(8.0))
                    .px(px(12.0))
                    .flex()
                    .justify_center()
                    .child(line(s, text::UI, ui, color::FG_3))
            }))
            .children((shown.is_empty() && !self.loading).then(|| {
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
                    .child(flow_paragraph(
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

#[cfg(test)]
mod tests {
    use super::Tab;

    /// `onTabKey`: the arrows flip between the two tabs, Home and End go to the first and the last; other keys pass.
    #[test]
    fn the_arrows_home_and_end_move_between_the_tabs() {
        assert_eq!(Tab::Due.after_key("right"), Some(Tab::Upcoming));
        assert_eq!(Tab::Upcoming.after_key("left"), Some(Tab::Due));
        assert_eq!(Tab::Upcoming.after_key("home"), Some(Tab::Due));
        assert_eq!(Tab::Due.after_key("end"), Some(Tab::Upcoming));
        assert_eq!(Tab::Due.after_key("enter"), None);
    }
}
