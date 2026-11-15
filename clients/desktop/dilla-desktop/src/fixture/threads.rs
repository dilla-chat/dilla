//! The threads facet of a fixture: `AppState::threads`, from `MessageSpec::thread_of`. Owned by Task 11; no other file maps it.
//!
//! A message with `thread_of` is a reply in that message's thread: the channel's timeline does not list it (the core keeps
//! thread replies out of it), the thread holds it, and its root carries the count and the time of the last reply, which the
//! chip under the root shows. `reply_to` needs nothing here: the timeline's items carry their [`ReplyRef`].
//!
//! [`ReplyRef`]: dilla_client::state::messages::ReplyRef

use std::collections::{HashMap, HashSet};

use dilla_client::state::AppState;
use dilla_client::state::threads::{ThreadData, ThreadSummary};
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`: every channel timeline loses its thread replies, and `threads` gains one thread per root
/// (every one of the world, read as an open pane would) with the root's summary.
pub(super) fn apply(world: &WorldSpec, _viewer: &str, state: &mut AppState) {
    let replies: HashSet<&str> = world
        .messages
        .iter()
        .filter(|m| m.thread_of.is_some())
        .map(|m| m.key.as_str())
        .collect();
    if replies.is_empty() {
        return;
    }
    let mut summaries = HashMap::new();
    let mut threads = HashMap::new();
    for (channel, timeline) in &mut state.timelines {
        let (held, kept): (Vec<_>, Vec<_>) = std::mem::take(&mut timeline.items)
            .into_iter()
            .partition(|i| replies.contains(i.key.as_str()));
        timeline.items = kept;
        let mut by_root: HashMap<&str, Vec<_>> = HashMap::new();
        for spec in world.messages.iter().filter(|m| m.thread_of.is_some()) {
            if let Some(item) = held.iter().find(|i| i.key == spec.key) {
                by_root
                    .entry(spec.thread_of.as_deref().unwrap_or_default())
                    .or_default()
                    .push(item.clone());
            }
        }
        for (root_key, mut items) in by_root {
            let Some(root) = timeline.items.iter().find(|i| i.key == root_key).cloned() else {
                continue;
            };
            let Some(root_id) = root.msg_id else { continue };
            items.sort_by_key(|i| i.ts);
            let count = u32::try_from(items.len()).unwrap_or(u32::MAX);
            let last_ts = items.last().map_or(0, |i| i.ts);
            summaries
                .entry(*channel)
                .or_insert_with(HashMap::new)
                .insert(root_id, ThreadSummary { count, last_ts });
            threads.insert(
                root_id,
                ThreadData {
                    channel: *channel,
                    root: Some(root),
                    replies: items,
                    has_earlier: false,
                    count,
                },
            );
        }
    }
    state.threads.summaries = summaries;
    state.threads.threads = threads;
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use crate::fixture::{Fixture, FixtureScreen};

    /// A world with a thread: the root keeps its place in the channel, the replies are the thread's, and the root's chip
    /// reads the count and the last reply's time.
    #[test]
    fn replies_leave_the_channel_and_the_root_carries_their_count() {
        let text = std::fs::read_to_string(
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("../parity/fixtures/shell/channel-view.json"),
        )
        .unwrap();
        // Make the last two messages replies in the thread of the first.
        let mut spec: serde_json::Value = serde_json::from_str(&text).unwrap();
        let messages = spec["world"]["messages"].as_array_mut().unwrap();
        let root = messages[0]["key"].as_str().unwrap().to_owned();
        let n = messages.len();
        for m in &mut messages[n - 2..] {
            m["thread_of"] = serde_json::Value::String(root.clone());
        }
        let last_ts = messages[n - 1]["ts"].as_u64().unwrap();
        let f = Fixture::parse(&spec.to_string(), None).unwrap();
        let FixtureScreen::Shell { channel } = f.screen else {
            panic!("shell")
        };
        let timeline = &f.state.timelines[&channel];
        assert_eq!(
            timeline.items.len(),
            n - 2,
            "the replies are not rows of the channel"
        );
        let root_id = timeline.items[0].msg_id.unwrap();
        let summary = f.state.threads.summary(channel, root_id).unwrap();
        assert_eq!((summary.count, summary.last_ts), (2, last_ts));
        let data = &f.state.threads.threads[&root_id];
        assert_eq!(data.replies.len(), 2);
        assert_eq!(data.root.as_ref().unwrap().key, timeline.items[0].key);
        assert!(data.replies[0].ts <= data.replies[1].ts);
    }
}
