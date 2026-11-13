//! The onboarding area's key bindings (Task 25): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! Enter advances each step, as the web's form submit does: in a field the field's own Enter does it (its
//! `InputEvent::PressEnter`), on a focused button or link GPUI clicks it, and anywhere else in the frame [`Submit`] does.

use gpui::{App, KeyBinding, actions};

/// The key context of the onboarding frame (sign-up and done).
pub const CONTEXT: &str = "Onboarding";

actions!(onboarding, [Submit, Back]);

/// Registers this area's bindings.
pub fn bind(cx: &mut App) {
    cx.bind_keys([KeyBinding::new("enter", Submit, Some(CONTEXT))]);
}
