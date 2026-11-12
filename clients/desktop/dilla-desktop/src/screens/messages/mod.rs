//! Area **messages** (Task 9): message row extras' host, markdown, code blocks, mentions, edit and delete, pins, the composer's own parts.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/messages/` is Task 9's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 9 fills them and keeps their signatures.

pub mod keys;

use crate::slots::MessageAction;
use dilla_client::state::TimelineItem;

/// This area's entries in the message toolbar and context menu (`slots::message_actions`).
pub fn message_actions(_item: &TimelineItem) -> Vec<MessageAction> {
    Vec::new()
}
