//! Area **reactions** (Task 10): react and unreact through `ClientCore`, and who reacted.
//!
//! Ports: the reaction parts of packages/web/src/screens/conversation/MessageItem.tsx and
//! packages/client-core/src/worker/controller.ts (`react`, `fold`).
//!
//! Who reacted (`AppState::reactions`) is filled only by a parity fixture's world: the core's timeline rows carry a count and
//! a `mine` flag per emoji and no reactor list, and the web client has none either (its `ReactionTooltip` is not wired), so
//! the live client shows the faces it knows and none for the rest of the count.
//!
//! A reaction is a fold queued as an outbox row of type 3 (add) or 4 (remove) aimed at the message, its body the emoji
//! (controller.ts `react`: "the worker does not de-duplicate reactions; the core folds them per user"). The timeline
//! changes only when the core folds the row, so the chip's count and its `mine` flag move once the instance stored it
//! and this device read it back; a refusal of the core (an emoji of 0 or more than 32 bytes, a group that is not
//! active) is answered as it is.

use std::sync::Arc;

use tokio::sync::oneshot;

use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::Value;
use crate::state::{ChannelId, MessageId};

/// The fold type that adds the sender's reaction (`send.ts` types: 3 react, 4 unreact).
const FOLD_REACT: u64 = 3;
/// The fold type that removes it.
const FOLD_UNREACT: u64 = 4;
/// dilla-core's bound on a reaction's emoji (`emoji must be 1..=32 bytes`).
pub const MAX_EMOJI_BYTES: usize = 32;

/// The reactions area's handle for reactions: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Reactions {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Reactions`] handle.
    pub fn reactions(&self) -> Reactions {
        Reactions {
            inner: self.inner().clone(),
        }
    }
}

/// The send request of a reaction fold: `[type, reply_to, body, attachments, thread_id]` (the core's `send_prepare`).
pub fn fold_request(msg: MessageId, emoji: &str, on: bool) -> Vec<u8> {
    Value::Array(vec![
        Value::Uint(if on { FOLD_REACT } else { FOLD_UNREACT }),
        Value::Bytes(msg.0.to_vec()),
        Value::Text(emoji.to_owned()),
        Value::Array(Vec::new()),
        Value::Null,
    ])
    .encode()
}

impl Reactions {
    /// Adds (`on`) or removes this account's `emoji` reaction on `msg` of `channel`. Answers once the fold is in the
    /// outbox; the timeline shows it when the core folds it.
    pub fn react(
        &self,
        channel: ChannelId,
        msg: MessageId,
        emoji: &str,
        on: bool,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        let emoji = emoji.to_owned();
        self.inner.spawn(async move {
            if emoji.is_empty() || emoji.len() > MAX_EMOJI_BYTES {
                return Err(ClientError::Sync("E_BAD_INPUT".to_owned()));
            }
            let known = inner
                .snapshot()
                .channel(channel)
                .cloned()
                .ok_or(ClientError::UnknownChannel)?;
            if !known.is_text() {
                return Err(ClientError::UnknownChannel);
            }
            let engine = inner.engine().clone();
            // The channel's text group, held already (the person reads its timeline): opening it again is the same call
            // `Client::send_text` makes.
            let g = engine
                .open_channel(
                    Some(known.community_id.0),
                    channel.0,
                    known.text_group_id.map(|g| g.0),
                )
                .await?;
            let request = fold_request(msg, &emoji, on);
            // Behind the trust retry, as every send is (controller.ts `sendWithTrustRetry`).
            engine
                .send_with_trust_retry(g, zeroize::Zeroizing::new(request))
                .await
                .map_err(|e| ClientError::Sync(e.code()))?;
            Ok(())
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_reaction_is_a_type_3_fold_and_an_unreaction_a_type_4() {
        let msg = MessageId([7; 16]);
        let Value::Array(on) = Value::decode(&fold_request(msg, "👍", true)).unwrap() else {
            panic!("an array")
        };
        let Value::Array(off) = Value::decode(&fold_request(msg, "👍", false)).unwrap() else {
            panic!("an array")
        };
        assert!(matches!(on[0], Value::Uint(3)));
        assert!(matches!(off[0], Value::Uint(4)));
        assert!(matches!(&on[1], Value::Bytes(b) if b == &vec![7u8; 16]));
        assert!(matches!(&on[2], Value::Text(t) if t == "👍"));
        assert_eq!(on.len(), 5);
    }
}
