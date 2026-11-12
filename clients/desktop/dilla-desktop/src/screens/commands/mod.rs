//! Area **bot** (Task 20): the command picker, private results, poll cards and dialogs, reminders.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/commands/` is Task 20's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 20 fills them and keeps their signatures.

pub mod keys;

use crate::slots::SlotCx;
use dilla_client::state::TimelineItem;
use gpui::{AnyElement, App};

/// A poll's card under its message (`slots::message_extras`, after the attachments).
pub fn poll_card(_item: &TimelineItem, _ctx: &SlotCx, _cx: &mut App) -> Option<AnyElement> {
    None
}

/// The command picker over the composer (`slots::composer_popup`).
pub fn picker(_ctx: &SlotCx, _cx: &mut App) -> Option<AnyElement> {
    None
}
