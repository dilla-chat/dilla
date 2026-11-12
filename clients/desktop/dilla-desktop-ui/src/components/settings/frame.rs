//! The settings `frame` (packages/ui/src/SettingsFrame): Settings covers the window on `--bg`. A 240px side column (the
//! identity, the search, the nav) and the content pane sit in a grid of at most 1280px, centred; the close button is the
//! window's, at its top right, with ESC under it.
//!
//! The content pane is a column: an optional call-activity bar, the scrolling body (36px 40px 60px; the section inside at
//! most 720px, `wide` 1024px) and the sticky footer. **Focus** goes to the frame on opening and Tab stays inside; Escape
//! and the close button call `on_close` (the host's dirty-draft guard, `edit_state`, decides); closing hands focus back to
//! what had it. The frame fills its parent: render it last in the window's root.
//!
//! [`section_header`] and [`group_heading`] draw the section rhythm the CSS gives a section's children; rows that touch
//! (one group) go in their own column with no gap, as [`super::row::GroupPosition`] describes.

use std::rc::Rc;

use gpui::{
    AnyElement, App, ElementId, FocusHandle, InteractiveElement, IntoElement, ParentElement,
    RenderOnce, SharedString, StatefulInteractiveElement, Styled, Window, deferred, div, px,
};

use super::super::dialog::{
    HasOpenFocus, OVERLAY_CONTEXT, OpenFocus, TrapNext, TrapPrevious, bind_keys, sync_open_focus,
    trap_tab,
};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::{LABEL_TRACKING_WIDE, focus_ring};
use crate::text::{line, line_height};
use crate::tokens::{color, font, radius, text};

type CloseHandler = Rc<dyn Fn(&mut Window, &mut App) + 'static>;

struct Memory {
    panel: FocusHandle,
    focus: OpenFocus,
}

impl HasOpenFocus for Memory {
    fn open_focus(&mut self) -> &mut OpenFocus {
        &mut self.focus
    }
}

/// The content column's width: 720px, `wide` 1024px, never more than the pane.
pub fn section_width(wide: bool) -> f32 {
    if wide { 1024.0 } else { 720.0 }
}

#[derive(IntoElement)]
pub struct SettingsFrame {
    id: ElementId,
    open: bool,
    close_label: SharedString,
    nav: Option<AnyElement>,
    identity: Option<AnyElement>,
    search: Option<AnyElement>,
    activity: Option<AnyElement>,
    body: Vec<AnyElement>,
    footer: Option<AnyElement>,
    wide: bool,
    on_close: Option<CloseHandler>,
}

impl SettingsFrame {
    pub fn new(id: impl Into<ElementId>, close_label: impl Into<SharedString>) -> Self {
        Self {
            id: id.into(),
            open: true,
            close_label: close_label.into(),
            nav: None,
            identity: None,
            search: None,
            activity: None,
            body: Vec::new(),
            footer: None,
            wide: false,
            on_close: None,
        }
    }

    pub fn open(mut self, open: bool) -> Self {
        self.open = open;
        self
    }

    pub fn nav(mut self, nav: impl IntoElement) -> Self {
        self.nav = Some(nav.into_any_element());
        self
    }

    pub fn identity(mut self, identity: impl IntoElement) -> Self {
        self.identity = Some(identity.into_any_element());
        self
    }

    pub fn search(mut self, search: impl IntoElement) -> Self {
        self.search = Some(search.into_any_element());
        self
    }

    /// Call controls while a call runs: a quiet row above the content.
    pub fn activity(mut self, activity: impl IntoElement) -> Self {
        self.activity = Some(activity.into_any_element());
        self
    }

    /// The footer under the body (the dirty-draft guard's Save and Reset).
    pub fn footer(mut self, footer: impl IntoElement) -> Self {
        self.footer = Some(footer.into_any_element());
        self
    }

    pub fn wide(mut self, wide: bool) -> Self {
        self.wide = wide;
        self
    }

    pub fn on_close(mut self, handler: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_close = Some(Rc::new(handler));
        self
    }
}

impl ParentElement for SettingsFrame {
    fn extend(&mut self, elements: impl IntoIterator<Item = AnyElement>) {
        self.body.extend(elements);
    }
}

/// A section's title (20/700) and its sub-line (12px `--fg-2`, 4px under the title), then the 22px the content below keeps.
pub fn section_header(title: impl Into<SharedString>, sub: Option<&str>) -> gpui::Div {
    let mut head = div().flex().flex_col().pb(px(22.0)).child(
        line(
            title,
            text::HEADING,
            line_height(text::HEADING, 1.5),
            color::FG,
        )
        .weight(font::WEIGHT_DISPLAY),
    );
    if let Some(sub) = sub {
        head = head.child(div().pt(px(4.0)).child(line(
            sub.to_owned(),
            text::UI,
            line_height(text::UI, 1.5),
            color::FG_2,
        )));
    }
    head
}

/// A group's micro heading (10px 600, 0.12em, uppercase, `--fg-3`): 22px under the block above, 8px over its group.
pub fn group_heading(text_: impl Into<SharedString>) -> gpui::Div {
    div().pt(px(22.0)).pb(px(8.0)).child(
        line(
            text_.into().to_uppercase(),
            text::MICRO,
            line_height(text::MICRO, 1.5),
            color::FG_3,
        )
        .weight(font::WEIGHT_LABEL)
        .tracking(LABEL_TRACKING_WIDE),
    )
}

impl RenderOnce for SettingsFrame {
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
        bind_keys(cx);
        let memory = window.use_keyed_state((self.id.clone(), "frame"), cx, |_, cx| Memory {
            panel: cx.focus_handle().tab_stop(true),
            focus: OpenFocus::default(),
        });
        let panel = memory.read(cx).panel.clone();
        sync_open_focus(&memory, self.open, &panel, panel.clone(), window, cx);
        let root = div().id(self.id.clone());
        if !self.open {
            return root.size_0().into_any_element();
        }

        let on_close = self.on_close;
        let close_with = move |window: &mut Window, cx: &mut App| {
            if let Some(h) = &on_close {
                h(window, cx);
            }
        };

        // The side column: identity, search, nav.
        let mut side = div()
            .id((self.id.clone(), "side"))
            .flex()
            .flex_col()
            .flex_none()
            .gap(px(2.0))
            .w(px(240.0))
            .h_full()
            .px(px(14.0))
            .py(px(28.0))
            .border_r_1()
            .border_color(color::HAIRLINE)
            .overflow_y_scroll();
        for part in [self.identity, self.search, self.nav].into_iter().flatten() {
            side = side.child(div().flex_none().min_w_0().child(part));
        }

        // The pane: the activity bar, the scrolling body, the sticky footer.
        let mut pane = div()
            .relative()
            .flex()
            .flex_col()
            .flex_1()
            .min_w_0()
            .h_full();
        if let Some(activity) = self.activity {
            pane = pane.child(
                div()
                    .flex()
                    .items_center()
                    .gap(px(12.0))
                    .flex_none()
                    .pl(px(40.0))
                    .pr(px(72.0))
                    .py(px(12.0))
                    .border_b_1()
                    .border_color(color::HAIRLINE)
                    .child(activity),
            );
        }
        pane = pane.child(
            div()
                .id((self.id.clone(), "body"))
                .flex_1()
                .min_h_0()
                .overflow_y_scroll()
                .px(px(40.0))
                .pt(px(36.0))
                .pb(px(60.0))
                .child(
                    div()
                        .flex()
                        .flex_col()
                        .gap(px(14.0))
                        .w_full()
                        .max_w(px(section_width(self.wide)))
                        .children(self.body),
                ),
        );
        if let Some(footer) = self.footer {
            pane = pane.child(
                div()
                    .flex_none()
                    .px(px(40.0))
                    .py(px(10.0))
                    .bg(color::BG)
                    .border_t_1()
                    .border_color(color::HAIRLINE)
                    .child(footer),
            );
        }

        // The close button: a 36px ring in `--control-edge` with the x, ESC under it.
        let close_click = close_with.clone();
        let close = div()
            .id((self.id.clone(), "close"))
            .absolute()
            .top(px(22.0))
            .right(px(16.0))
            .flex()
            .flex_col()
            .items_center()
            .gap(px(4.0))
            .rounded(radius::MD)
            .cursor_pointer()
            .tab_index(0)
            .text_color(color::FG_2)
            .hover(|s| s.text_color(color::FG))
            .focus_visible(|s| s.shadow(focus_ring()))
            .on_click(move |_, window, cx| close_click(window, cx))
            .tooltip(super::super::tooltip::tooltip(
                self.close_label.clone(),
                &["Esc"],
            ))
            .tooltip_show_delay(super::super::tooltip::SHOW_DELAY)
            .child(
                div()
                    .flex()
                    .items_center()
                    .justify_center()
                    .size(px(36.0))
                    .rounded(radius::PILL)
                    .border_1()
                    .border_color(color::CONTROL_EDGE)
                    .child(Icon::new(IconName::X).size(px(16.0)).color(color::FG_2)),
            )
            .child(
                line("ESC", text::CHIP, line_height(text::CHIP, 1.5), color::FG_2).tracking(0.1),
            );

        let (next_panel, prev_panel) = (panel.clone(), panel.clone());
        let frame = root
            .absolute()
            .inset_0()
            .flex()
            .justify_center()
            .bg(color::BG)
            .occlude()
            .child(
                div()
                    .id((self.id.clone(), "panel"))
                    .track_focus(&panel)
                    .key_context(OVERLAY_CONTEXT)
                    .relative()
                    .flex()
                    .w_full()
                    .max_w(px(1280.0))
                    .h_full()
                    .on_action(move |_: &TrapNext, window, cx| {
                        trap_tab(&next_panel, true, window, cx)
                    })
                    .on_action(move |_: &TrapPrevious, window, cx| {
                        trap_tab(&prev_panel, false, window, cx)
                    })
                    .on_key_down(move |event, window, cx| {
                        if event.keystroke.key == "escape" {
                            cx.stop_propagation();
                            close_with(window, cx);
                        }
                    })
                    .child(side)
                    .child(pane)
                    .child(close),
            );
        deferred(frame).with_priority(8).into_any_element()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_content_column_is_720_or_1024_wide() {
        assert_eq!(section_width(false), 720.0);
        assert_eq!(section_width(true), 1024.0);
    }
}
