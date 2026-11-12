//! Area **voice** (Task 17): the voice channel row's people, the dock, the stage and the call controls.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/voice/` is Task 17's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 17 fills them and keeps their signatures.

pub mod keys;

use dilla_client::state::Channel;
use gpui::{AnyElement, App};

/// The people in a voice channel, nested under its row (`slots::channel_row_extras`).
pub fn channel_people(_channel: &Channel, _cx: &mut App) -> Option<AnyElement> {
    None
}

/// The voice dock over the user panel (`slots::dock`, first).
pub fn dock(_cx: &mut App) -> Option<AnyElement> {
    None
}
