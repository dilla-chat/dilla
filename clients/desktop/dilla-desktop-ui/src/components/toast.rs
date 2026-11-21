//! `Toast` (packages/ui/src/Toast): the prototype's single toast. One toast shows at a time for 2.4 s ([`TOAST_MS`], the UI
//! timeout); a new one replaces it and restarts the timer. Any code with an `&mut App` shows one with [`show_toast`]; one
//! [`ToastHost`] at the window root draws it, 24px above the bottom edge and centred (38px tall with its edge).

use std::time::Duration;

use gpui::{
    App, ElementId, Global, InteractiveElement, IntoElement, ParentElement, RenderOnce,
    SharedString, StatefulInteractiveElement, Styled, Window, WindowId, div, px,
};

use crate::a11y::{Accessible, Live, Role};
use crate::icon::Icon;
use crate::icon_names::IconName;
use crate::metrics::shadow_toast;
use crate::text::{line, line_height};
use crate::tokens::{color, radius, text};

/// Toast.tsx: how long a toast shows.
pub const TOAST_MS: u64 = 2400;

#[derive(Default)]
struct Toasts {
    /// The text showing, its sequence number (a timer only ends the toast it was started for) and the window it was
    /// shown for (`None`: every window).
    current: Option<(SharedString, u64, Option<WindowId>)>,
    n: u64,
    /// A frozen clock (a drawn fixture's, as the web shooter's browser has): a toast never ends, so the frame holds it.
    frozen: bool,
}

impl Global for Toasts {}

/// The toasts stop ending: the clock of a drawn fixture is frozen, as the web shooter's browser clock is, so a toast stays on
/// screen for the frame (the parity harness's fixture windows).
pub fn freeze(cx: &mut App) {
    if !cx.has_global::<Toasts>() {
        cx.set_global(Toasts::default());
    }
    cx.global_mut::<Toasts>().frozen = true;
}

/// The text of the toast showing now, if any.
pub fn current(cx: &App) -> Option<SharedString> {
    cx.try_global::<Toasts>()
        .and_then(|t| t.current.as_ref().map(|(text, _, _)| text.clone()))
}

/// The text of the toast `window` shows now: one shown for it, or for every window.
pub fn current_in(window: WindowId, cx: &App) -> Option<SharedString> {
    cx.try_global::<Toasts>().and_then(|t| {
        t.current
            .as_ref()
            .filter(|(_, _, w)| w.is_none_or(|w| w == window))
            .map(|(text, _, _)| text.clone())
    })
}

/// Shows `text` as the toast of the window that has the keyboard (every window when none has it), replacing one
/// already showing, and ends it after [`TOAST_MS`].
pub fn show_toast(text: impl Into<SharedString>, cx: &mut App) {
    let window = cx.active_window().map(|w| w.window_id());
    show_toast_in(window, text, cx);
}

/// Shows `text` as the toast of `window` (`None`: every window).
pub fn show_toast_in(window: Option<WindowId>, text: impl Into<SharedString>, cx: &mut App) {
    let text = text.into();
    if !cx.has_global::<Toasts>() {
        cx.set_global(Toasts::default());
    }
    let (n, frozen) = {
        let toasts = cx.global_mut::<Toasts>();
        toasts.n += 1;
        toasts.current = Some((text, toasts.n, window));
        (toasts.n, toasts.frozen)
    };
    cx.refresh_windows();
    if frozen {
        return;
    }
    cx.spawn(async move |cx| {
        cx.background_executor()
            .timer(Duration::from_millis(TOAST_MS))
            .await;
        cx.update(|cx| {
            let toasts = cx.global_mut::<Toasts>();
            if toasts.current.as_ref().is_some_and(|(_, k, _)| *k == n) {
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
    fn render(self, window: &mut Window, cx: &mut App) -> impl IntoElement {
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
        let window_id = window.window_handle().window_id();
        let n = cx
            .try_global::<Toasts>()
            .and_then(|t| t.current.as_ref().map(|(_, n, _)| *n))
            .unwrap_or_default();
        match current_in(window_id, cx) {
            None => region,
            // `role="status"`: read out once as it appears (each toast is a new node, so a repeated "Copied" is too).
            Some(message) => {
                let enter = (self.id.clone(), format!("toast-enter-{n}"));
                let toast = div()
                    .id((self.id, format!("toast-{n}")))
                    .a11y(Role::Status, message.clone())
                    .aria_value(message.clone())
                    .live(Live::Polite)
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
                    .child(line(
                        message,
                        text::UI,
                        line_height(text::UI, 1.5),
                        color::FG,
                    ));
                // Toast.css:7: `d-toast` over `--duration-toast`, each toast on its own.
                region.child(crate::motion::toast_in(toast, enter, cx))
            }
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

    #[gpui::test]
    fn a_toast_ends_on_its_timer_unless_the_clock_is_frozen(cx: &mut gpui::TestAppContext) {
        let later = Duration::from_millis(TOAST_MS + 100);
        cx.update(|cx| show_toast_in(None, "gone", cx));
        assert!(cx.update(|cx| current(cx).is_some()));
        cx.executor().advance_clock(later);
        cx.run_until_parked();
        assert!(cx.update(|cx| current(cx).is_none()), "the timer ended it");
        cx.update(|cx| {
            freeze(cx);
            show_toast_in(None, "stays", cx);
        });
        cx.executor().advance_clock(later);
        cx.run_until_parked();
        assert_eq!(
            cx.update(|cx| current(cx)).as_deref(),
            Some("stays"),
            "a frozen clock holds it"
        );
    }
}
