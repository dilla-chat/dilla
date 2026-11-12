//! Area **members** (Task 14): the members aside, the profile card, the person menu, moderation dialogs, the Roles, Self roles, Members, Bans and Moderation log sections.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/members/` is Task 14's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 14 fills them and keeps their signatures.

pub mod keys;

use crate::router::Route;
use crate::slots::{SectionId, SlotCx};
use gpui::{AnyElement, App, Empty, IntoElement, Window};

/// This area's aside (`slots::aside`): `None` while it is closed.
pub fn pane(_route: &Route, _window: &mut Window, _cx: &mut App) -> Option<AnyElement> {
    None
}

/// Opens or closes this area's aside. The channel header's button (Task 8) calls it. Fixed signature.
pub fn toggle_pane(_cx: &mut App) {}

/// Draws one of this area's settings sections (`slots::settings_sections` points here).
pub fn settings_section(
    _section: SectionId,
    _ctx: &SlotCx,
    _window: &mut Window,
    _cx: &mut App,
) -> AnyElement {
    Empty.into_any_element()
}
