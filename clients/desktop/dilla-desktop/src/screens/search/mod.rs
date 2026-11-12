//! Area **search** (Task 23): the search palette.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/search/` is Task 23's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 23 fills them and keeps their signatures.

pub mod keys;

use gpui::{AnyElement, App};

/// The search palette (`slots::overlays`): `None` while it is closed.
pub fn palette(_cx: &mut App) -> Option<AnyElement> {
    None
}
