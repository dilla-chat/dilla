//! Area **bot admin** (Task 21): the Bot and Music server settings and the now-playing card.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/bot_admin/` is Task 21's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 21 fills them and keeps their signatures.

pub mod keys;

use crate::slots::{SectionId, SlotCx};
use gpui::{AnyElement, App, Empty, IntoElement, Window};

/// The now-playing card in the dock (`slots::dock`, after the voice dock).
pub fn now_playing(_window: &mut Window, _cx: &mut App) -> Option<AnyElement> {
    None
}

/// Draws one of this area's settings sections (`slots::settings_sections` points here).
pub fn settings_section(
    _section: SectionId,
    _ctx: &SlotCx,
    _window: &mut Window,
    _cx: &mut App,
) -> AnyElement {
    Empty.into_any_element()
}
