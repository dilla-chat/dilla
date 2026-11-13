//! The AFK channel's page (packages/ui/src/CallStage `afk`, the prototype's AFK view): the moon tile, a title that says who
//! is away, the explainer with the server's timeout, and one button (Go AFK, or Come back once away). The stage that hosts
//! it (the voice area) draws the header around it.

use gpui::{
    App, ElementId, FocusHandle, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, div, px,
};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::focus_ring;
use crate::text::{line, line_height, paragraph};
use crate::tokens::{color, font, radius, text};

/// CSS `rem`.
const REM: f32 = 16.0;
/// `.d-call-stage__afk { gap: 0.75rem; padding: 1.5rem }`.
const GAP: f32 = 0.75 * REM;
const PAD: f32 = 1.5 * REM;
/// `.d-call-stage__afk-tile`: 3rem inside a 1px edge; the moon at 20px.
const TILE: f32 = 3.0 * REM;
const MOON: f32 = 20.0;
/// `.d-call-stage__afk-text { max-width: 340px }`.
const TEXT_MAX: f32 = 340.0;
/// `.d-call-stage__afk-action { height: 2.25rem; padding: 0 1rem }`.
const ACTION_H: f32 = 2.25 * REM;
const ACTION_PAD_X: f32 = REM;

pub type OnAfk = Box<dyn Fn(&gpui::ClickEvent, &mut Window, &mut App)>;

#[derive(IntoElement)]
pub struct AfkPage {
    id: ElementId,
    title: SharedString,
    text: SharedString,
    action: SharedString,
    on_action: OnAfk,
    focus: Option<FocusHandle>,
}

impl AfkPage {
    pub fn new(
        id: impl Into<ElementId>,
        title: impl Into<SharedString>,
        text: impl Into<SharedString>,
        action: impl Into<SharedString>,
        on_action: impl Fn(&gpui::ClickEvent, &mut Window, &mut App) + 'static,
    ) -> Self {
        Self {
            id: id.into(),
            title: title.into(),
            text: text.into(),
            action: action.into(),
            on_action: Box::new(on_action),
            focus: None,
        }
    }

    /// Tracks `handle` as the button's focus.
    pub fn focus_handle(mut self, handle: &FocusHandle) -> Self {
        self.focus = Some(handle.clone());
        self
    }
}

impl RenderOnce for AfkPage {
    fn render(self, _window: &mut Window, _cx: &mut App) -> impl IntoElement {
        let id = self.id.clone();
        let mut action = div()
            .id((id, "action"))
            .flex()
            .flex_none()
            .items_center()
            .h(px(ACTION_H))
            .px(px(ACTION_PAD_X))
            .font_family(font::MONO)
            .text_size(text::UI)
            .border_1()
            .border_color(color::CONTROL_EDGE)
            .rounded(radius::SEG)
            .cursor_pointer()
            .hover(|s| s.border_color(color::WARN))
            .focus_visible(|s| s.shadow(focus_ring()))
            .child(line(
                self.action,
                text::UI,
                line_height(text::UI, 1.5),
                color::FG,
            ));
        action = match &self.focus {
            Some(h) => action.track_focus(h).tab_stop(true),
            None => action.tab_index(0),
        };
        div()
            .id(self.id)
            .flex_1()
            .flex()
            .flex_col()
            .items_center()
            .justify_center()
            .gap(px(GAP))
            .p(px(PAD))
            .child(
                div()
                    .flex()
                    .items_center()
                    .justify_center()
                    .size(px(TILE + 2.0))
                    .rounded(radius::XXL)
                    .bg(color::SURFACE_TILE)
                    .border_1()
                    .border_color(color::HAIRLINE_2)
                    .child(Icon::new(IconName::Moon).size(px(MOON)).color(color::WARN)),
            )
            .child(
                line(
                    self.title,
                    text::CARD,
                    line_height(text::CARD, 1.5),
                    color::FG,
                )
                .weight(font::WEIGHT_DISPLAY),
            )
            // `text-align: center` inside 340px: each wrapped line centred.
            .child(
                div()
                    .flex()
                    .flex_col()
                    .items_center()
                    .max_w(px(TEXT_MAX))
                    .children(paragraph(
                        &self.text,
                        text::UI,
                        line_height(text::UI, 1.5),
                        color::FG_2,
                        TEXT_MAX,
                    )),
            )
            .child(action.on_click(self.on_action))
    }
}
