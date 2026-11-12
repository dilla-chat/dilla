//! Area **ring** (Task 18): the ring banner, DM calls, blocked calls and the AFK notice.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/ring/` is Task 18's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 18 fills them and keeps their signatures.

pub mod keys;

use gpui::{AnyElement, App};

/// The ring banner (`slots::overlays`).
pub fn ring_banner(_cx: &mut App) -> Option<AnyElement> {
    None
}
