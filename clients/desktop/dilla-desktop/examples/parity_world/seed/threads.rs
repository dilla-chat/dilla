//! Seeds the replies and threads of a fixture into the real dillad. Owned by Task 11; no other file does.
//!
//! A reply (`reply_to`) and a thread reply (`thread_of`) are posted through the area's own API
//! ([`dilla_client::features::threads::Threads`]), at the message's own time. The history loop of `main.rs` posts every
//! other message with `send_text`; it must hand these to [`post`] instead, which answers `None` for a plain message:
//!
//! ```text
//! let sent = match seed::post(m, client, channel, &posted).await {
//!     Some(sent) => sent,
//!     None => client.send_text(channel, m.body.clone()).await.unwrap(),
//! };
//! posted.insert(m.key.clone(), sent.expect("queued"));
//! ```
//!
//! (`posted` maps a message's key to the id the client queued it under.) Task 11's report names the lines.

use std::collections::HashMap;

use dilla_client::Client;
use dilla_client::state::{ChannelId, MessageId};
use dilla_client::testing::host::Person;
use dilla_client::testing::spec::{MessageSpec, ScreenFixture};

/// Names what this world cannot seed. Nothing: replies and threads are posted by [`post`].
pub(super) fn unseedable(_f: &ScreenFixture, _out: &mut Vec<String>) {}

#[allow(
    dead_code,
    reason = "the history loop of main.rs calls it once the controller wires it (Task 11 report)"
)]
/// Posts `m` when it is a reply or a thread reply: the client queues it with its `reply_to` or its `thread_id`, naming the
/// target by the id `posted` holds for the key. `None` for a plain message.
pub async fn post(
    m: &MessageSpec,
    client: &Client,
    channel: ChannelId,
    posted: &HashMap<String, MessageId>,
) -> Option<Result<MessageId, dilla_client::ClientError>> {
    let target = |key: &Option<String>| {
        key.as_ref().map(|k| {
            *posted
                .get(k)
                .unwrap_or_else(|| panic!("`{k}` is posted before its replies"))
        })
    };
    let threads = client.threads();
    let queued = match (target(&m.reply_to), target(&m.thread_of)) {
        (None, None) => return None,
        (Some(to), None) => threads.reply(channel, to, m.body.clone()),
        (_, Some(root)) => threads.send_in_thread(channel, root, m.body.clone()),
    };
    Some(
        queued
            .await
            .unwrap_or(Err(dilla_client::ClientError::Stopped)),
    )
}

/// Seeds the facet once the history is posted: nothing more (the replies were posted with it).
#[allow(clippy::unused_async)]
pub(super) async fn seed(_f: &ScreenFixture, _people: &[(String, Person)]) {}
