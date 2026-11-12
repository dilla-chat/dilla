//! Area **attachments** (Task 12): the attachment tray, cards, the image grid, the lightbox, the media player and the GIF picker.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/attachments/` is Task 12's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 12 fills them and keeps their signatures.

pub mod keys;

use crate::slots::SlotCx;
use dilla_client::state::TimelineItem;
use gpui::{AnyElement, App, Window};

/// A message's attachment cards and image grid (`slots::message_extras`, after the reply chip).
pub fn attachment_cards(_item: &TimelineItem, _ctx: &SlotCx, _cx: &mut App) -> Option<AnyElement> {
    None
}

/// The attachment tray over the composer (`slots::composer_leading`, after the reply bar).
pub fn tray(_ctx: &SlotCx, _cx: &mut App) -> Option<AnyElement> {
    None
}

/// The composer's attach and GIF buttons (`slots::composer_buttons`, first).
pub fn composer_buttons(_ctx: &SlotCx, _cx: &mut App) -> Option<AnyElement> {
    None
}

/// The DM GIF privacy setting (settings/DmGifPrivacySetting.tsx), drawn inside the Privacy section. Task 13's Privacy
/// section calls it; Task 12 implements it. Fixed signature: change it only with the controller.
pub fn dm_gif_privacy(_ctx: &SlotCx, _window: &mut Window, _cx: &mut App) -> Option<AnyElement> {
    None
}
