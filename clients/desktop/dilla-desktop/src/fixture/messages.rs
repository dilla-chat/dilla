//! The messages facet of a fixture: `AppState::messages`, from the messages' `pinned` flag, the stored positions a
//! live client would hold, and who is typing (Task 34). Owned by Task 9; no other file maps it.

use dilla_client::state::messages::{PinnedItem, Typer};
use dilla_client::state::{AppState, ChannelId, UserId};
use dilla_client::testing::spec::WorldSpec;

use super::Ids;

/// How long a typing row lives past the frozen clock: a signal's own relay window (10 s, L-HTTP-96), so the row is on
/// screen for the whole frame, as the web's is for the frame it is shot in.
const TYPING_HOLD_MS: u64 = 10_000;

/// Gives every readable message of every timeline the seq a stored row has (its place, from 1: what the toolbar, the edit
/// shortcut and slow mode need), lists each channel's pinned messages for the pins dialog, and holds the people the world
/// has typing (`now` is the frame's clock, unix seconds).
pub(super) fn apply(world: &WorldSpec, viewer: &str, ids: Ids<'_>, now: u64, state: &mut AppState) {
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
    for t in &world.typing {
        let channel = match t.channel.strip_prefix("dm/") {
            Some(dm) => ChannelId(ids.of("dm", dm)),
            None => ids.channel(&t.channel),
        };
        let Some(person) = world.people.iter().find(|p| p.id == t.person) else {
            continue;
        };
        // The name the client's roster gives (the worker's `nameOf`): the display name, else the username.
        let name = if person.display.is_empty() {
            person.username().to_owned()
        } else {
            person.display.clone()
        };
        state
            .messages
            .typing
            .entry(channel)
            .or_default()
            .push(Typer {
                user: ids.user(&t.person),
                name,
                expires_at_ms: now * 1000 + TYPING_HOLD_MS,
            });
    }
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use crate::fixture::Fixture;

    fn load(name: &str) -> Fixture {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../parity/fixtures")
            .join(name);
        Fixture::load(&path, None).unwrap()
    }

    /// Task 34: the world's typing people are rows of the open channel that outlast the frozen clock, named as the roster
    /// names them.
    #[test]
    fn the_worlds_typing_people_are_typing_rows_past_the_clock() {
        let f = load("messages/typing-several.json");
        let rows: Vec<_> = f.state.messages.typing.values().collect();
        assert_eq!(rows.len(), 1);
        let names: Vec<&str> = rows[0].iter().map(|r| r.name.as_str()).collect();
        assert_eq!(names, ["Mira", "Björn"]);
        assert!(rows[0].iter().all(|r| r.expires_at_ms > f.now * 1000));
        assert!(
            load("messages/mention.json")
                .state
                .messages
                .typing
                .is_empty()
        );
    }
}
