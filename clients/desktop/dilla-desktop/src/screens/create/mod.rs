//! Area **onboarding** (Task 25): the create server and create channel flows, join by invite, the add-server dialog.
//!
//! Stub from Task 1 (area scaffold): every file under `screens/create/` is Task 25's. The hooks below are where
//! `crate::slots` draws this area inside another area's view; Task 25 fills them and keeps their signatures.

use dilla_client::state::CommunityId;
use gpui::App;

/// Opens the create-channel flow for `community`. The channel list's "+" (Task 8) calls it. Fixed signature.
pub fn open_create_channel(_community: CommunityId, _cx: &mut App) {}
