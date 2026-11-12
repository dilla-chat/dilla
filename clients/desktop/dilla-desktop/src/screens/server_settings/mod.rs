//! Area **server settings** (Task 15): the Overview, Channels, GIFs, Invites, Welcome, Join roles and Danger zone sections.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/server_settings/` is Task 15's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 15 fills them and keeps their signatures.

pub mod keys;

use crate::slots::{SectionId, SlotCx};
use gpui::{AnyElement, App, Empty, IntoElement, Window};

/// Draws one of this area's settings sections (`slots::settings_sections` points here).
pub fn settings_section(
    _section: SectionId,
    _ctx: &SlotCx,
    _window: &mut Window,
    _cx: &mut App,
) -> AnyElement {
    Empty.into_any_element()
}
