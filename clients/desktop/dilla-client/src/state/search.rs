//! `search`: Task 23's slice of [`super::AppState`]: the palette's newest message search and what it found
//! (`packages/client-core` slice `search`: `{ requestId, hits, cursor }`, hits as `SearchHitView`). The query and the
//! hits stay on this device: the search runs over the store's decrypted rows, and nothing here is sent anywhere.
//! Nothing here holds key material.

use super::{ChannelId, CommunityId, MessageId};

/// One message hit, as the palette lists it (`SearchHitView`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SearchHit {
    /// The channel or direct message the message is in.
    pub channel: ChannelId,
    /// `None` for a direct message.
    pub community: Option<CommunityId>,
    pub seq: u64,
    /// At most 160 characters of the message's shown text.
    pub snippet: String,
    /// The sender's name; `None` when the row cannot be read.
    pub author: Option<String>,
    /// Unix seconds the row was received at; `None` when the row cannot be read.
    pub ts: Option<u64>,
    /// The thread the message shows in, when it is a reply.
    pub thread_root: Option<MessageId>,
}

impl SearchHit {
    /// The hit's identity in a list (`<channel>:<seq>`, as the web's `SearchHitView.id`).
    pub fn id(&self) -> String {
        format!("{}:{}", self.channel.hex(), self.seq)
    }
}

/// The newest search: [`Default`] is none yet.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct SearchState {
    /// The newest request's number; an answer for an older one is dropped.
    pub request: u64,
    /// The trimmed query the newest request searches for.
    pub query: String,
    /// The hits of the newest request, the core's order (newest group, newest message first); empty while it runs.
    pub hits: Vec<SearchHit>,
    /// The newest request has not been answered yet.
    pub loading: bool,
}
