//! Area **threads** (Task 11): the reply chip, the reply bar, the thread pane and the thread count.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/threads/` is Task 11's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 11 fills them and keeps their signatures.

pub mod keys;

use crate::router::Route;
use crate::slots::{Extra, MessageAction, SlotCx};
use dilla_client::state::TimelineItem;
use gpui::{AnyElement, App, Window};

/// The reply chip above a reply's body (`slots::message_extras`, first).
pub fn reply_chip(
    _item: &TimelineItem,
    _ctx: &SlotCx,
    _window: &mut Window,
    _cx: &mut App,
) -> Option<Extra> {
    None
}

/// A thread's reply count under its first message (`slots::message_extras`, after the reactions).
pub fn thread_count(
    _item: &TimelineItem,
    _ctx: &SlotCx,
    _window: &mut Window,
    _cx: &mut App,
) -> Option<Extra> {
    None
}

/// This area's entries in the message toolbar and context menu (`slots::message_actions`).
pub fn message_actions(_item: &TimelineItem) -> Vec<MessageAction> {
    Vec::new()
}

/// The "replying to" bar over the composer (`slots::composer_leading`, first).
pub fn reply_bar(_ctx: &SlotCx, _window: &mut Window, _cx: &mut App) -> Option<AnyElement> {
    None
}

/// This area's aside (`slots::aside`): `None` while it is closed.
pub fn pane(_route: &Route, _window: &mut Window, _cx: &mut App) -> Option<AnyElement> {
    None
}

/// Opens or closes this area's aside. The channel header's button (Task 8) calls it. Fixed signature.
pub fn toggle_pane(_cx: &mut App) {}
