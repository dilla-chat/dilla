//! One element filling its window, as the web's element full screen did (a stage tile's full screen, the inline video
//! player's): the window goes full screen for it and draws only that element; Escape (or the button again) gives the
//! window back. Apart from it, the whole app window can go full screen with its layout unchanged (View → full screen,
//! `native::menus`), which these never touch: an element that found its window already full screen leaves it so.
//!
//! The way out is the same however the element goes (its button, Escape, the stream ending, its client released while
//! it is shown): [`leave`] puts the window back if [`enter`] made it full screen, even with no `Window` at hand.

use gpui::{App, Window, WindowId};

/// The window goes full screen for one element; true when it was not already (and so goes back on [`leave`]).
pub fn enter(window: &mut Window) -> bool {
    let made = !window.is_fullscreen();
    if made {
        window.toggle_fullscreen();
    }
    made
}

/// The element no longer fills `owner`: the window leaves full screen if [`enter`] made it so (`made`). Deferred, so it
/// is safe from inside that window's own update and from a release that has no window at hand.
pub fn leave(owner: WindowId, made: bool, cx: &mut App) {
    if !made {
        return;
    }
    let Some(handle) = cx.windows().into_iter().find(|w| w.window_id() == owner) else {
        return;
    };
    cx.defer(move |cx| {
        let _ = handle.update(cx, |_, window, _| {
            if window.is_fullscreen() {
                window.toggle_fullscreen();
            }
        });
    });
}

#[cfg(test)]
mod tests {
    /// The window goes back only if `enter` made it full screen, and `leave` needs no window at hand.
    #[gpui::test]
    fn leave_restores_only_what_enter_made(cx: &mut gpui::TestAppContext) {
        let vcx = cx.add_empty_window();
        let w = vcx.update(|window, _| window.window_handle().window_id());
        let made = vcx.update(|window, _| super::enter(window));
        assert!(made);
        vcx.update(|_, cx| super::leave(w, made, cx));
        vcx.run_until_parked();
        assert!(!vcx.update(|window, _| window.is_fullscreen()));
        // Already full screen (the app's own): left as it is.
        vcx.update(|window, _| window.toggle_fullscreen());
        let made = vcx.update(|window, _| super::enter(window));
        assert!(!made);
        vcx.update(|_, cx| super::leave(w, made, cx));
        vcx.run_until_parked();
        assert!(vcx.update(|window, _| window.is_fullscreen()));
    }
}
