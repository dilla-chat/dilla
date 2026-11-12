//! Area **diagnostics** (Task 24): the Diagnostics section, the offline link banner and the version-refused banner.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/diagnostics/` is Task 24's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 24 fills them and keeps their signatures.

pub mod keys;

use crate::slots::{SectionId, SlotCx};
use gpui::{AnyElement, App, Empty, IntoElement, Window};

/// The banners over the main column (`slots::banners`): the offline link and the version refusal.
pub fn banners(_ctx: &SlotCx, _cx: &mut App) -> Vec<AnyElement> {
    Vec::new()
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
