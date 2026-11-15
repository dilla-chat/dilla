//! The messages facet of a fixture: `AppState::messages`, from the messages' `pinned` flag, and the stored positions a
//! live client would hold. Owned by Task 9; no other file maps it.

use dilla_client::state::messages::PinnedItem;
use dilla_client::state::{AppState, UserId};
use dilla_client::testing::spec::WorldSpec;

/// Gives every readable message of every timeline the seq a stored row has (its place, from 1: what the toolbar, the edit
/// shortcut and slow mode need), and lists each channel's pinned messages for the pins dialog.
pub(super) fn apply(_world: &WorldSpec, viewer: &str, state: &mut AppState) {
    let _ = viewer;
    let mut pins = Vec::new();
    for (channel, timeline) in &mut state.timelines {
        let mut list = Vec::new();
        for (n, item) in timeline.items.iter_mut().enumerate() {
            if item.seq.is_none() {
                item.seq = Some(n as u64 + 1);
            }
            if item.pinned
                && let Some(msg_id) = item.msg_id
            {
                list.push(PinnedItem {
                    msg_id,
                    seq: item.seq.unwrap_or(0),
                    sender_user: item.sender_user,
                    excerpt: item.body.clone(),
                    pinned_by: item.sender_user.unwrap_or(UserId([0; 16])),
                    pinned_seq: item.seq.unwrap_or(0),
                    ts: item.ts,
                    thread_root: None,
                });
            }
        }
        pins.push((*channel, list));
    }
    state.messages.pins.extend(pins);
}
