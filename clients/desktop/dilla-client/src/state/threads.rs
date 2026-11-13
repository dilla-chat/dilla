//! `threads`: Task 11's slice of [`super::AppState`]: the thread summaries of the channels it tracks, the threads it has
//! read, the one that is open and the message the composer is replying to.
//!
//! Ports types.ts `ThreadState` (`thread:<channel>`), `ThreadSummary` (a `TimelineItem`'s `thread`) and the reply the
//! composer carries. Nothing here holds key material: bodies are plaintext the core already decrypted and stored.

use std::collections::{BTreeSet, HashMap};

use super::{ChannelId, MessageId, TimelineItem, UserId};

/// `PERM.sendMessages` (packages/client-core/src/admin/perm.ts): the bit a reply and a thread need.
pub const SEND_MESSAGES: u64 = 1 << 1;

/// types.ts `ThreadSummary`: what the chip under a thread's root shows.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ThreadSummary {
    /// The replies the core shows for the root.
    pub count: u32,
    /// Unix seconds of the latest reply.
    pub last_ts: u64,
}

/// A thread as the core reads it: its root (when this device holds it) and a page of its replies.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ThreadData {
    pub channel: ChannelId,
    /// `None` when the root is not held here (the pane then says so).
    pub root: Option<TimelineItem>,
    /// The replies, oldest first, with this device's own pending ones last.
    pub replies: Vec<TimelineItem>,
    /// The core holds replies before the oldest one read ([`crate::features::threads::Threads::load_earlier`]).
    pub has_earlier: bool,
    /// The root's reply count (`thread.count` of its row), which the pane's rule shows even before every reply is read.
    pub count: u32,
}

/// The thread whose pane is open.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct OpenThread {
    pub channel: ChannelId,
    pub root: MessageId,
}

/// The message the composer replies to (the reply bar's "Replying to **Ada** · excerpt").
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ReplyTarget {
    pub channel: ChannelId,
    pub msg_id: MessageId,
    pub sender: Option<UserId>,
    /// The original's body as the bar shows it (one line, cut by the bar).
    pub excerpt: String,
}

/// The threads slice. [`Default`] is the empty slice.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct ThreadState {
    /// Channels whose roots' summaries are kept fresh ([`crate::features::threads::Threads::track`]).
    pub tracked: BTreeSet<ChannelId>,
    /// `timeline:<channel>`'s `thread` of every root it shows, by channel.
    pub summaries: HashMap<ChannelId, HashMap<MessageId, ThreadSummary>>,
    /// The threads read so far, by root: the open one, and (in a fixture) every one of the world.
    pub threads: HashMap<MessageId, ThreadData>,
    /// The open thread, or `None` while the aside is closed.
    pub open: Option<OpenThread>,
    /// The thread the aside showed last: the header's "Threads" button reopens it directly (e2e Q9).
    pub last_open: Option<OpenThread>,
    /// The reply bar's target.
    pub replying: Option<ReplyTarget>,
    /// What the viewer may do in a channel (the permission bits the roles area resolved, `useChannelBits`). A channel with
    /// no entry is unknown, which allows, as the web does (`bits == null`): the instance decides each request again.
    pub channel_bits: HashMap<ChannelId, u64>,
}

impl ThreadState {
    /// Whether the viewer may send in `channel`: MessageItem.tsx `canSend` (`bits == null || bits & sendMessages`).
    pub fn can_send(&self, channel: ChannelId) -> bool {
        self.channel_bits
            .get(&channel)
            .is_none_or(|bits| bits & SEND_MESSAGES != 0)
    }

    /// The summary of `root` in `channel`.
    pub fn summary(&self, channel: ChannelId, root: MessageId) -> Option<ThreadSummary> {
        self.summaries.get(&channel)?.get(&root).copied()
    }

    /// The open thread's data, if it has been read.
    pub fn open_data(&self) -> Option<&ThreadData> {
        self.threads.get(&self.open?.root)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_channel_without_bits_allows_and_one_without_the_send_bit_does_not() {
        let mut s = ThreadState::default();
        let ch = ChannelId([1; 16]);
        assert!(s.can_send(ch), "unknown allows, as the web does");
        s.channel_bits.insert(ch, 1);
        assert!(!s.can_send(ch));
        s.channel_bits.insert(ch, 1 | SEND_MESSAGES);
        assert!(s.can_send(ch));
    }
}
