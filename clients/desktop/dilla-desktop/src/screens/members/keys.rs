//! The members area's key bindings (Task 14): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! Escape closes the profile card wherever the focus is (Task 9: ProfileCard.tsx moves the focus into the card and closes
//! on its Escape; here an interceptor takes the key while the window has a card open), and the person menu while the
//! focus is in the members pane. Tab and Shift+Tab stay inside an open card (its `CARD_CONTEXT`). Shift+F10 and the Menu
//! key open the person menu through `ActionMenu`'s own context-menu routing (`opens_context_menu`).

use gpui::{App, Global, KeyBinding, actions};

use dilla_desktop_ui::components::dialog::{TrapNext, TrapPrevious};
use dilla_desktop_ui::components::members::CARD_CONTEXT;

actions!(dilla_members, [CloseCard]);

/// App-wide by design: the Escape interceptor is installed once per app; what it closes is the card of the window that
/// took the key.
struct Bound;
impl Global for Bound {}

/// Registers this area's bindings.
pub fn bind(cx: &mut App) {
    cx.bind_keys([
        KeyBinding::new("escape", CloseCard, Some("MembersPane")),
        KeyBinding::new("tab", TrapNext, Some(CARD_CONTEXT)),
        KeyBinding::new("shift-tab", TrapPrevious, Some(CARD_CONTEXT)),
    ]);
    if cx.has_global::<Bound>() {
        return;
    }
    cx.set_global(Bound);
    cx.intercept_keystrokes(|event, window, cx| {
        let key = &event.keystroke;
        if key.key != "escape" || key.modifiers.modified() {
            return;
        }
        let w = window.window_handle().window_id();
        if super::close_card(w, window, cx) {
            cx.stop_propagation();
        }
    })
    .detach();
}
