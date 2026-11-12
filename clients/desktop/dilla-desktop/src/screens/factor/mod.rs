//! Area **factors** (Task 26): the factor setup, the forced-factor screen, backup codes and the Sign-in section.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/factor/` is Task 26's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 26 fills them and keeps their signatures.

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
