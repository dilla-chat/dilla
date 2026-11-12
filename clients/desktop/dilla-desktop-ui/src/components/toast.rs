//! `Toast` (packages/ui/src/Toast): the prototype's single toast. One toast shows at a time for 2.4 s ([`TOAST_MS`], the UI
//! timeout); a new one replaces it and restarts the timer. Any code with an `&mut App` shows one with [`show_toast`]; one
//! [`ToastHost`] at the window root draws it, 24px above the bottom edge and centred (38px tall with its edge).

use std::time::Duration;

use gpui::{
    App, ElementId, Global, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, Styled, Window, div, px,
};

use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::shadow_toast;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

/// Toast.tsx: how long a toast shows.
pub const TOAST_MS: u64 = 2400;

#[derive(Default)]
struct Toasts {
    /// The text showing, and its sequence number (a timer only ends the toast it was started for).
    current: Option<(SharedString, u64)>,
    n: u64,
}

impl Global for Toasts {}

/// The text of the toast showing now, if any.
pub fn current(cx: &App) -> Option<SharedString> {
    cx.try_global::<Toasts>()
        .and_then(|t| t.current.as_ref().map(|(text, _)| text.clone()))
}

/// Shows `text` as the toast, replacing one already showing, and ends it after [`TOAST_MS`].
pub fn show_toast(text: impl Into<SharedString>, cx: &mut App) {
    let text = text.into();
    if !cx.has_global::<Toasts>() {
        cx.set_global(Toasts::default());
    }
    let n = {
        let toasts = cx.global_mut::<Toasts>();
        toasts.n += 1;
        toasts.current = Some((text, toasts.n));
        toasts.n
    };
    cx.refresh_windows();
    cx.spawn(async move |cx| {
        cx.background_executor()
            .timer(Duration::from_millis(TOAST_MS))
            .await;
        cx.update(|cx| {
            let toasts = cx.global_mut::<Toasts>();
            if toasts.current.as_ref().is_some_and(|(_, k)| *k == n) {
                toasts.current = None;
                cx.refresh_windows();
            }
        });
    })
    .detach();
}

/// The toast region: render it last in the window's root (it is positioned against that root).
#[derive(IntoElement)]
pub struct ToastHost {
    id: ElementId,
}

impl ToastHost {
    pub fn new(id: impl Into<ElementId>) -> Self {
        Self { id: id.into() }
    }
}

impl RenderOnce for ToastHost {
    fn render(self, _window: &mut Window, cx: &mut App) -> impl IntoElement {
        let region = div()
            .id(self.id.clone())
            .absolute()
            .left(px(8.0))
            .right(px(8.0))
            .bottom(px(24.0))
            .flex()
            .flex_col()
            .items_center()
            .gap(px(6.0));
        match current(cx) {
            None => region,
            Some(message) => region.child(
                div()
                    .id((self.id, "toast"))
                    .flex()
                    .items_center()
                    .gap(px(8.0))
                    .min_h(px(38.0))
                    .px(px(14.0))
                    .py(px(6.0))
                    .bg(color::TOAST)
                    .border_1()
                    .border_color(color::LINE_STRONG)
                    .rounded(radius::LG)
                    .shadow(shadow_toast())
                    .child(Icon::new(IconName::Check).size(px(14.0)).color(color::OK))
                    .child(line(message, text::UI, line_height(text::UI, 1.5), color::FG)),
            ),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_toast_shows_for_2_4_seconds() {
        assert_eq!(TOAST_MS, 2400);
    }
}
