//! Area **reactions** (Task 10): the reaction bar, the emoji grid and the who-reacted tooltip.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/reactions/` is Task 10's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 10 fills them and keeps their signatures.

pub mod keys;

use crate::slots::{MessageAction, SlotCx};
use dilla_client::state::TimelineItem;
use gpui::{AnyElement, App};

/// The reaction bar under a message (`slots::message_extras`, last).
pub fn reaction_bar(_item: &TimelineItem, _ctx: &SlotCx, _cx: &mut App) -> Option<AnyElement> {
    None
}

/// This area's entries in the message toolbar and context menu (`slots::message_actions`).
pub fn message_actions(_item: &TimelineItem) -> Vec<MessageAction> {
    Vec::new()
}

/// The composer's emoji button (`slots::composer_buttons`, after the attachment buttons).
pub fn emoji_button(_ctx: &SlotCx, _cx: &mut App) -> Option<AnyElement> {
    None
}
