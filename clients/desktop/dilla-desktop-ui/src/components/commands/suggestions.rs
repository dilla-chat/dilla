//! `CommandSuggestions` (packages/ui CommandPicker, `.d-cmd-sugs`): the choices for the active chip, over the composer
//! (Task 28 wave 2, lane C). No row takes focus: the chip keeps it and ↑↓ move the selection. A row is its icon tile,
//! its label and an optional second line.
//!
//! CommandPicker.css is content-box: a 34px row with 2px padding is 38px tall, the 220px list 224px with its padding.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use super::{accent_bar, shadow_2};
use crate::components::avatar::{Avatar, AvatarSize};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::LABEL_TRACKING_WIDE;
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

/// `.d-cmd-sugs__list`: `max-height: 13.75rem`.
pub const LIST_MAX: f32 = 220.0;
/// One row: `min-height: 2.125rem` and 2px above and below (content-box).
pub const ROW_HEIGHT: f32 = 38.0;
/// `.d-composer__top > .d-cmd-sugs`: `width: min(21.25rem, 100% - 2rem)`.
pub const WIDTH: f32 = 340.0;
const ICON_BOX: f32 = 22.0;

/// One suggestion (`CommandSuggestion`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SuggestionItem {
    pub icon: IconName,
    pub label: SharedString,
    /// The second line (a reminder's preview); empty: none.
    pub sub: SharedString,
    /// A member's name: the row draws that member's avatar (initials, `AvatarSize::Sug`) in place of the icon tile.
    pub avatar: Option<SharedString>,
}

type OnIndex = Rc<dyn Fn(usize, &mut Window, &mut App)>;
type RowIds = Rc<dyn Fn(usize) -> ElementId>;

#[derive(IntoElement)]
pub struct CommandSuggestions {
    id: ElementId,
    title: SharedString,
    items: Vec<SuggestionItem>,
    active: Option<usize>,
    on_pick: Option<OnIndex>,
    on_active: Option<OnIndex>,
    row_ids: Option<RowIds>,
}

impl CommandSuggestions {
    pub fn new(
        id: impl Into<ElementId>,
        title: impl Into<SharedString>,
        items: Vec<SuggestionItem>,
    ) -> Self {
        Self {
            id: id.into(),
            title: title.into(),
            items,
            active: None,
            on_pick: None,
            on_active: None,
            row_ids: None,
        }
    }

    pub fn active(mut self, active: Option<usize>) -> Self {
        self.active = active;
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

    /// The element id of row `i` (automation); `<id>-row-<i>` otherwise.
    pub fn row_ids(mut self, f: impl Fn(usize) -> ElementId + 'static) -> Self {
        self.row_ids = Some(Rc::new(f));
        self
    }
}

/// The row's leading slot: a member's avatar, else the icon tile.
fn lead(item: &SuggestionItem, index: usize) -> AnyElement {
    match &item.avatar {
        Some(name) => div()
            .flex_none()
            // Recorded in test builds only (gpui `test-support`): the render test in dilla-desktop looks the avatar up.
            .debug_selector(move || format!("command-suggestion-avatar-{index}"))
            .child(
                Avatar::new(
                    ElementId::Name(format!("command-suggestion-avatar-{index}").into()),
                    name.clone(),
                )
                .size(AvatarSize::Sug),
            )
            .into_any_element(),
        None => div()
            .flex_none()
            .size(px(ICON_BOX))
            .rounded(radius::ROW)
            .bg(color::HOVER)
            .flex()
            .items_center()
            .justify_center()
            .child(Icon::new(item.icon).size(px(12.0)).color(color::FG_2))
            .into_any_element(),
    }
}

fn row(item: &SuggestionItem, index: usize, active: bool) -> impl IntoElement {
    let sub = (!item.sub.is_empty()).then(|| {
        line(
            item.sub.clone(),
            text::META,
            line_height(text::META, 1.3),
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
        .py(px(2.0))
        .px(px(8.0))
        .rounded(radius::MD)
        .overflow_hidden()
        .child(lead(item, index))
        .child(
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .flex_col()
                .child(
                    line(
                        item.label.clone(),
                        text::UI,
                        line_height(text::UI, 1.3),
                        color::FG,
                    )
                    .ellipsis(),
                )
                .children(sub),
        );
    if active {
        r = r.bg(color::SURFACE_HI).child(accent_bar());
    }
    r
}

impl RenderOnce for CommandSuggestions {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let title = line(
            self.title.to_uppercase(),
            text::MICRO,
            line_height(text::MICRO, 1.5),
            color::FG_3,
        )
        .weight(font::WEIGHT_LABEL)
        .tracking(LABEL_TRACKING_WIDE);
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
                    .child(row(item, i, self.active == Some(i)))
                    .into_any_element()
            })
            .collect();
        div()
            .id(self.id)
            .max_w(px(WIDTH))
            .w_full()
            .overflow_hidden()
            .bg(color::SURFACE_2)
            .border_1()
            .border_color(color::LINE_STRONG)
            .rounded(radius::XL)
            .shadow(shadow_2())
            .child(div().pt(px(8.0)).pb(px(6.0)).px(px(12.0)).child(title))
            .child(
                div()
                    .id("command-suggestions-scroll")
                    .max_h(px(LIST_MAX + 4.0))
                    .overflow_y_scroll()
                    .pb(px(4.0))
                    .px(px(4.0))
                    .children(rows),
            )
    }
}
