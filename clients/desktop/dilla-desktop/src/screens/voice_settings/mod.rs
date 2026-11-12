//! Area **voice settings** (Task 19): the Voice section, the microphone test and the synthesised sounds.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/voice_settings/` is Task 19's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 19 fills them and keeps their signatures.

pub mod keys;
pub mod sounds;

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
