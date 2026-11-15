//! Area **bot admin** (Task 21): the music provider and the now-playing queue.
//!
//! Ports: packages/client-core/src/voice/music.ts (the per-community adapter). Every read and control is a sealed Bot
//! command (music state 92, app operations 100 + the v1 operation) over [`BotOperations`]; the Bot answers a mutation with
//! a receipt and the state it makes only arrives in a snapshot, so a control settles when the confirmed snapshot has.
//!
//! One [`Music`] holds what the adapter keeps per channel (the revision of the newest snapshot, the channels a conflict
//! marked stale, the attempts that may be resent), so hold one per page. The player states land in
//! `AppState::bot_admin.music`.

use std::collections::{BTreeSet, HashMap};
use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use tokio::sync::oneshot;

use crate::client::{Client, ClientError, Inner};
use crate::features::bot_admin::{BotAdmin, BotArg, BotOperations, BotRequest, BotResult, OpError};
use crate::state::bot_admin::{
    MusicSearchResult, MusicSnapshot, MusicState, Repeat, decode_music_search,
    decode_music_snapshot,
};
use crate::state::{ChannelId, CommunityId, UserId};

/// `VoiceMusicErrorCode` and the adapter's other failures.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum MusicError {
    #[error("You do not have permission to control this Bot.")]
    Forbidden,
    #[error("Music state changed. Refresh and try again.")]
    Conflict,
    /// A track the Bot could not load (a definite refusal of an enqueue).
    #[error("The Bot could not load that track.")]
    Unavailable,
    #[error("The Bot could not do that right now. Try again.")]
    OperationUnavailable,
    /// The Bot accepted the operation, but the resulting state could not be confirmed. A retry with the same attempt id
    /// settles it.
    #[error(
        "The Bot accepted the music operation, but the resulting state could not be confirmed."
    )]
    AcceptedUnconfirmed { request_id: [u8; 16] },
    /// The panel's view is out of date (a queue item that is gone, music state not read yet).
    #[error("{0}")]
    Stale(String),
    #[error("Invalid {0}.")]
    Invalid(&'static str),
    #[error("the Bot is unreachable ({0})")]
    Transport(String),
}

impl MusicError {
    /// The `strings.rs` key of the toast a refused control shows (MusicPlayer's catch).
    pub fn toast_key(&self) -> &'static str {
        match self {
            Self::OperationUnavailable => "music.error.operation-unavailable",
            Self::Unavailable => "music.error.unavailable",
            _ => "music.blocked.dj",
        }
    }
}

fn response_error(status: u8) -> MusicError {
    match status {
        2 => MusicError::Forbidden,
        4 => MusicError::Conflict,
        _ => MusicError::OperationUnavailable,
    }
}

fn definite_failure(operation: u64) -> MusicError {
    if operation == 12 {
        MusicError::Unavailable
    } else {
        MusicError::OperationUnavailable
    }
}

fn now_ms() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map_or(0, |d| d.as_millis() as u64)
}

#[derive(Clone)]
struct Attempt {
    channel: ChannelId,
    operation: u64,
    args: Vec<BotArg>,
    revision: u64,
    sent: bool,
    accepted_revision: Option<u64>,
}

#[derive(Default)]
struct Local {
    revisions: HashMap<ChannelId, u64>,
    stale: BTreeSet<ChannelId>,
    attempts: HashMap<[u8; 16], Attempt>,
    attempt_order: Vec<[u8; 16]>,
}

/// The music adapter's handle: cheap to clone.
#[derive(Clone)]
pub struct Music {
    admin: BotAdmin,
    inner: Arc<Inner>,
    local: Arc<Mutex<Local>>,
}

impl Client {
    /// The [`Music`] handle.
    pub fn music(&self) -> Music {
        Music::new(self.bot_admin())
    }
}

type Done<T> = oneshot::Receiver<Result<T, MusicError>>;

fn spawn<T: Send + 'static>(
    inner: &Arc<Inner>,
    f: impl std::future::Future<Output = Result<T, MusicError>> + Send + 'static,
) -> Done<T> {
    let (tx, rx) = oneshot::channel();
    let rx_client = inner.spawn(async move {
        let _ = tx.send(f.await);
        Ok::<(), ClientError>(())
    });
    drop(rx_client);
    rx
}

const ATTEMPT_LIMIT: usize = 128;

impl Music {
    pub fn new(admin: BotAdmin) -> Self {
        Self {
            inner: admin.inner().clone(),
            admin,
            local: Arc::default(),
        }
    }

    fn community_of(&self, channel: ChannelId) -> Result<CommunityId, MusicError> {
        self.inner
            .snapshot()
            .channel(channel)
            .map(|c| c.community_id)
            .ok_or_else(|| MusicError::Stale("Invalid voice channel id.".to_owned()))
    }

    fn ops(&self, channel: ChannelId) -> Result<Arc<BotOperations>, MusicError> {
        Ok(self.admin.operations(self.community_of(channel)?))
    }

    fn local(&self) -> std::sync::MutexGuard<'_, Local> {
        self.local.lock().unwrap_or_else(|e| e.into_inner())
    }

    fn state_of(&self, channel: ChannelId) -> Option<MusicSnapshot> {
        match self.inner.snapshot().bot_admin.music.get(&channel) {
            Some(MusicState::Available(s)) => Some(s.clone()),
            _ => None,
        }
    }

    fn publish(&self, channel: ChannelId, state: MusicState) {
        self.inner.publish(move |s| {
            s.bot_admin.music.insert(channel, state);
        });
    }

    /// A snapshot older than the newest one held is dropped.
    fn apply(&self, channel: ChannelId, snapshot: MusicSnapshot) {
        {
            let mut local = self.local();
            if local
                .revisions
                .get(&channel)
                .is_some_and(|r| *r > snapshot.revision)
            {
                return;
            }
            local.revisions.insert(channel, snapshot.revision);
        }
        self.publish(channel, MusicState::Available(snapshot));
    }

    fn revision(&self, channel: ChannelId) -> u64 {
        self.local().revisions.get(&channel).copied().unwrap_or(0)
    }

    async fn request(
        &self,
        channel: ChannelId,
        operation: u64,
        args: Vec<BotArg>,
        revision: Option<u64>,
        attempt: Option<[u8; 16]>,
    ) -> Result<BotResult, OpError> {
        let ops = self.ops(channel).map_err(|_| OpError::Invalid)?;
        let result = ops
            .request(BotRequest {
                operation,
                channel: Some(channel),
                args,
                revision,
                request_id: attempt,
            })
            .await?;
        if operation == 10 {
            if result.status == 0 {
                let snapshot = decode_music_snapshot(&result.data, now_ms());
                match snapshot {
                    Ok(s) if result.revision == Some(s.revision) => self.apply(channel, s),
                    _ => {
                        self.publish(channel, MusicState::Unavailable);
                        return Err(OpError::Refused {
                            code: "E_MUSIC_SNAPSHOT".to_owned(),
                            request_id: result.request_id,
                        });
                    }
                }
            } else {
                self.publish(channel, MusicState::Unavailable);
            }
        }
        Ok(result)
    }

    /// Reads the channel's snapshot (92 as the Bot's catalog names it; v1 operation 10).
    pub async fn refresh_now(&self, channel: ChannelId) -> Result<(), MusicError> {
        let response = self
            .request(channel, 10, Vec::new(), None, None)
            .await
            .map_err(transport)?;
        if response.status != 0 {
            return Err(response_error(response.status));
        }
        if response.revision.is_none_or(|r| r > self.revision(channel)) {
            return Err(MusicError::Stale(
                "The Bot music snapshot revision does not match its authenticated operation response.".to_owned(),
            ));
        }
        if self.state_of(channel).is_none() {
            return Err(MusicError::Stale(
                "The Bot did not return a music snapshot.".to_owned(),
            ));
        }
        self.local().stale.remove(&channel);
        Ok(())
    }

    /// Re-reads the snapshot (a Bot hint, a page that became visible, a second after the track should end). An answer that
    /// cannot be read marks the player unavailable.
    pub fn refresh(&self, channel: ChannelId) -> Done<()> {
        let this = self.clone();
        spawn(&self.inner, async move {
            let out = this.refresh_now(channel).await;
            if out.is_err() {
                this.publish(channel, MusicState::Unavailable);
            }
            out
        })
    }

    async fn mutate(
        &self,
        channel: ChannelId,
        operation: u64,
        args: Vec<BotArg>,
        attempt: Option<[u8; 16]>,
    ) -> Result<(), MusicError> {
        // Pause and skip answer the snapshot they make.
        let immediate = operation == 13 || operation == 15;
        let existing = attempt.and_then(|id| self.local().attempts.get(&id).cloned());
        if let Some(rec) = &existing
            && (rec.channel != channel || rec.operation != operation)
        {
            return Err(MusicError::Stale(
                "The music attempt id is already bound to another operation.".to_owned(),
            ));
        }
        let ops = self.ops(channel)?;
        if existing.is_none()
            && let Some(id) = attempt
            && !immediate
        {
            // A retry of an attempt this page made before: the Bot answers what it did.
            let prior = ops
                .read(id, operation)
                .await
                .map_err(|_| MusicError::AcceptedUnconfirmed { request_id: id })?;
            if let Some(prior) = prior {
                return self.settle_prior(channel, operation, id, prior).await;
            }
        }
        let mut record = match existing {
            Some(r) => r,
            None => {
                if !immediate
                    && (self.state_of(channel).is_none() || self.local().stale.contains(&channel))
                {
                    self.refresh_now(channel).await?;
                }
                let before = self.state_of(channel);
                if !immediate && before.is_none() {
                    return Err(MusicError::Stale("Music state is unavailable.".to_owned()));
                }
                let queue_has = |id: &BotArg| match (id, &before) {
                    (BotArg::Text(id), Some(s)) => s.queue.iter().any(|t| &t.id == id),
                    _ => false,
                };
                if (operation == 21 || operation == 22) && !queue_has(&args[0]) {
                    return Err(MusicError::Stale(
                        "That queue item is no longer available. Refresh the queue and choose it again.".to_owned(),
                    ));
                }
                if operation == 22
                    && let Some(dest @ BotArg::Text(dest_id)) = args.get(1)
                    && !(queue_has(dest) && args[0] != *dest && !dest_id.is_empty())
                {
                    return Err(MusicError::Stale(
                        "The destination queue item is no longer available. Refresh the queue and choose it again.".to_owned(),
                    ));
                }
                // Bind every press to the item the person saw, including an empty queue.
                let bound = if operation == 15
                    && let Some(b) = &before
                {
                    vec![BotArg::Text(
                        b.track.as_ref().map_or_else(String::new, |t| t.id.clone()),
                    )]
                } else {
                    args
                };
                let rec = Attempt {
                    channel,
                    operation,
                    args: bound,
                    revision: if immediate { 0 } else { self.revision(channel) },
                    sent: false,
                    accepted_revision: None,
                };
                if let Some(id) = attempt {
                    let mut local = self.local();
                    local.attempts.insert(id, rec.clone());
                    local.attempt_order.push(id);
                    while local.attempt_order.len() > ATTEMPT_LIMIT {
                        let oldest = local.attempt_order.remove(0);
                        local.attempts.remove(&oldest);
                    }
                }
                rec
            }
        };
        let request_id = attempt.unwrap_or([0; 16]);
        let result: BotResult = if attempt.is_some() && record.sent {
            let reconciled = ops
                .read(request_id, operation)
                .await
                .map_err(|_| MusicError::AcceptedUnconfirmed { request_id })?;
            reconciled.ok_or(MusicError::AcceptedUnconfirmed { request_id })?
        } else {
            if attempt.is_some() {
                record.sent = true;
                self.remember(attempt, &record);
            }
            match ops
                .request(BotRequest {
                    operation,
                    channel: Some(channel),
                    args: record.args.clone(),
                    revision: Some(record.revision),
                    request_id: attempt,
                })
                .await
            {
                Ok(r) => r,
                Err(OpError::Refused { code, .. }) => {
                    // A refused send never ran: the attempt is over.
                    if let Some(id) = attempt {
                        self.local().attempts.remove(&id);
                    }
                    return Err(MusicError::Transport(code));
                }
                Err(OpError::Uncertain { request_id: rid }) => {
                    return Err(MusicError::AcceptedUnconfirmed { request_id: rid });
                }
                Err(OpError::Invalid) => return Err(MusicError::Invalid("music request")),
            }
        };
        let forget = |this: &Self| {
            if let Some(id) = attempt {
                this.local().attempts.remove(&id);
            }
        };
        if result.status == 5 && result.revision.is_none() {
            forget(self);
            return Err(definite_failure(operation));
        }
        if result.status == 5 {
            record.sent = true;
            return Err(MusicError::AcceptedUnconfirmed {
                request_id: attempt.unwrap_or(result.request_id),
            });
        }
        if result.status == 4 {
            forget(self);
            // A conflict means our cached revision is stale. Refresh before rejecting so the next action can proceed.
            self.local().stale.insert(channel);
            self.refresh_now(channel).await?;
            return Err(response_error(4));
        }
        if result.status != 0 {
            forget(self);
            return Err(response_error(result.status));
        }
        let unconfirmed = |_why: &str| MusicError::AcceptedUnconfirmed {
            request_id: attempt.unwrap_or(result.request_id),
        };
        if immediate && result.data.len() > 1 {
            return match decode_music_snapshot(&result.data, now_ms()) {
                Ok(s) if result.revision == Some(s.revision) => {
                    self.apply(channel, s);
                    self.local().stale.remove(&channel);
                    forget(self);
                    Ok(())
                }
                _ => Err(unconfirmed("control snapshot revision mismatch")),
            };
        }
        // The receipt is an empty array at a revision past the one the request carried.
        let receipt_ok = crate::gateway::frames::Value::decode(&result.data)
            .is_ok_and(|v| v.as_array().is_some_and(<[_]>::is_empty))
            && result.revision.is_some_and(|r| r > record.revision);
        if !receipt_ok {
            record.sent = true;
            record.accepted_revision = result.revision;
            self.remember(attempt, &record);
            let _ = self.refresh_now(channel).await;
            return Err(unconfirmed("malformed receipt"));
        }
        let accepted = result.revision.unwrap_or(0);
        self.remember(
            attempt,
            &Attempt {
                accepted_revision: Some(accepted),
                ..record
            },
        );
        // A successful operation response is not the accepted state: the panel settles when the confirmed snapshot arrives.
        self.refresh_now(channel)
            .await
            .map_err(|_| unconfirmed("refresh failed"))?;
        if self.state_of(channel).is_none() || self.revision(channel) < accepted {
            return Err(unconfirmed("resulting music state is not yet visible"));
        }
        forget(self);
        Ok(())
    }

    fn remember(&self, attempt: Option<[u8; 16]>, record: &Attempt) {
        if let Some(id) = attempt {
            self.local().attempts.insert(id, record.clone());
        }
    }

    /// A retry whose request id the Bot already answered.
    async fn settle_prior(
        &self,
        channel: ChannelId,
        operation: u64,
        id: [u8; 16],
        prior: BotResult,
    ) -> Result<(), MusicError> {
        let unconfirmed = MusicError::AcceptedUnconfirmed { request_id: id };
        if prior.status == 5 && prior.revision.is_none() {
            return Err(definite_failure(operation));
        }
        if prior.status == 5 {
            return Err(unconfirmed);
        }
        if prior.status != 0 {
            return Err(response_error(prior.status));
        }
        let receipt_ok = crate::gateway::frames::Value::decode(&prior.data)
            .is_ok_and(|v| v.as_array().is_some_and(<[_]>::is_empty))
            && prior.revision.is_some();
        if !receipt_ok {
            let _ = self.refresh_now(channel).await;
            return Err(unconfirmed);
        }
        self.refresh_now(channel)
            .await
            .map_err(|_| unconfirmed.clone())?;
        if self.revision(channel) < prior.revision.unwrap_or(0) {
            return Err(unconfirmed);
        }
        Ok(())
    }

    fn control(
        &self,
        channel: ChannelId,
        operation: u64,
        args: Vec<BotArg>,
        attempt: Option<[u8; 16]>,
    ) -> Done<()> {
        let this = self.clone();
        spawn(&self.inner, async move {
            this.mutate(channel, operation, args, attempt).await
        })
    }

    pub fn pause(&self, channel: ChannelId, paused: bool, attempt: Option<[u8; 16]>) -> Done<()> {
        self.control(channel, if paused { 13 } else { 14 }, Vec::new(), attempt)
    }
    pub fn skip(&self, channel: ChannelId, attempt: Option<[u8; 16]>) -> Done<()> {
        self.control(channel, 15, Vec::new(), attempt)
    }
    pub fn stop(&self, channel: ChannelId, attempt: Option<[u8; 16]>) -> Done<()> {
        self.control(channel, 16, Vec::new(), attempt)
    }
    pub fn seek(
        &self,
        channel: ChannelId,
        position_ms: u64,
        attempt: Option<[u8; 16]>,
    ) -> Done<()> {
        self.control(channel, 17, vec![BotArg::Uint(position_ms)], attempt)
    }
    pub fn set_volume(
        &self,
        channel: ChannelId,
        volume: u64,
        attempt: Option<[u8; 16]>,
    ) -> Done<()> {
        if volume > 100 {
            return failed(&self.inner, MusicError::Invalid("volume"));
        }
        self.control(channel, 18, vec![BotArg::Uint(volume)], attempt)
    }
    /// Shuffle toggles; a request for the state it is already in does nothing.
    pub fn set_shuffle(
        &self,
        channel: ChannelId,
        enabled: bool,
        attempt: Option<[u8; 16]>,
    ) -> Done<()> {
        if self.state_of(channel).is_some_and(|s| s.shuffle == enabled)
            && attempt.is_none_or(|id| !self.local().attempts.contains_key(&id))
        {
            return failed_ok(&self.inner);
        }
        self.control(channel, 19, Vec::new(), attempt)
    }
    pub fn set_repeat(
        &self,
        channel: ChannelId,
        repeat: Repeat,
        attempt: Option<[u8; 16]>,
    ) -> Done<()> {
        let n = match repeat {
            Repeat::Off => 0,
            Repeat::One => 1,
            Repeat::All => 2,
        };
        self.control(channel, 20, vec![BotArg::Uint(n)], attempt)
    }
    pub fn remove(
        &self,
        channel: ChannelId,
        queue_item: String,
        attempt: Option<[u8; 16]>,
    ) -> Done<()> {
        self.control(channel, 21, vec![BotArg::Text(queue_item)], attempt)
    }
    pub fn clear_queue(&self, channel: ChannelId, attempt: Option<[u8; 16]>) -> Done<()> {
        self.control(channel, 23, Vec::new(), attempt)
    }
    pub fn enqueue(&self, channel: ChannelId, input: &str, attempt: Option<[u8; 16]>) -> Done<()> {
        match validated_input(input, 2048, "queue input") {
            Ok(input) => self.control(channel, 12, vec![BotArg::Text(input)], attempt),
            Err(e) => failed(&self.inner, e),
        }
    }
    /// Moves `queue_item` to `destination` (an index within the current queue).
    pub fn move_item(
        &self,
        channel: ChannelId,
        queue_item: String,
        destination: usize,
        attempt: Option<[u8; 16]>,
    ) -> Done<()> {
        let existing = attempt.and_then(|id| self.local().attempts.get(&id).cloned());
        if let Some(rec) = existing {
            return self.control(channel, 22, rec.args, attempt);
        }
        let Some(state) = self.state_of(channel) else {
            return failed(
                &self.inner,
                MusicError::Stale("Music state is unavailable.".to_owned()),
            );
        };
        if destination >= state.queue.len() {
            return failed(
                &self.inner,
                MusicError::Stale("Choose a position within the current queue.".to_owned()),
            );
        }
        let rest: Vec<&str> = state
            .queue
            .iter()
            .map(|t| t.id.as_str())
            .filter(|id| *id != queue_item)
            .collect();
        let before = rest
            .get(destination.min(rest.len()))
            .map(|s| (*s).to_owned());
        self.control(
            channel,
            22,
            vec![
                BotArg::Text(queue_item),
                before.map_or(BotArg::Null, BotArg::Text),
            ],
            attempt,
        )
    }
    /// Searches YouTube (operation 11): results have stable ids and a watch address to enqueue.
    pub fn search(
        &self,
        channel: ChannelId,
        query: &str,
        attempt: Option<[u8; 16]>,
    ) -> Done<Vec<MusicSearchResult>> {
        let this = self.clone();
        let query = validated_input(query, 200, "search query");
        spawn(&self.inner, async move {
            let query = query?;
            let r = this
                .request(channel, 11, vec![BotArg::Text(query)], None, attempt)
                .await
                .map_err(transport)?;
            if r.status != 0 {
                return Err(response_error(r.status));
            }
            decode_music_search(&r.data).map_err(|_| MusicError::OperationUnavailable)
        })
    }
    /// "{you} started music in {room}": the Bot posts it in the text channel where `/play` ran, once per track, for the
    /// requester of the playing track (operation 25 at revision 0).
    pub fn announce(&self, channel: ChannelId, text_channel: ChannelId) -> Done<()> {
        let this = self.clone();
        spawn(&self.inner, async move {
            let r = this
                .request(
                    channel,
                    25,
                    vec![BotArg::Text(text_channel.hex())],
                    Some(0),
                    None,
                )
                .await
                .map_err(transport)?;
            if r.status != 0 {
                return Err(response_error(r.status));
            }
            Ok(())
        })
    }

    /// Whether `user` queued the track (the requester the Bot recorded).
    pub fn requested_by(track: &crate::state::bot_admin::MusicTrack, user: UserId) -> bool {
        track.requester == Some(user)
    }
}

fn transport(e: OpError) -> MusicError {
    match e {
        OpError::Refused { code, .. } => MusicError::Transport(code),
        OpError::Uncertain { request_id } => MusicError::AcceptedUnconfirmed { request_id },
        OpError::Invalid => MusicError::Invalid("music request"),
    }
}

fn failed<T: Send + 'static>(inner: &Arc<Inner>, e: MusicError) -> Done<T> {
    spawn(inner, async move { Err(e) })
}

fn failed_ok(inner: &Arc<Inner>) -> Done<()> {
    spawn(inner, async { Ok(()) })
}

/// A search query or queue input: trimmed, 1 to `limit` bytes, no control characters.
fn validated_input(value: &str, limit: usize, label: &'static str) -> Result<String, MusicError> {
    let trimmed = value.trim();
    if trimmed.is_empty() || trimmed.len() > limit || trimmed.chars().any(|c| c.is_control()) {
        return Err(MusicError::Invalid(label));
    }
    Ok(trimmed.to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn inputs_are_trimmed_bounded_and_free_of_control_characters() {
        assert_eq!(
            validated_input("  lofi  ", 200, "search query").unwrap(),
            "lofi"
        );
        assert!(validated_input("   ", 200, "search query").is_err());
        assert!(validated_input("a\u{0007}b", 200, "search query").is_err());
        assert!(validated_input(&"x".repeat(201), 200, "search query").is_err());
        assert!(validated_input("a\u{007f}", 200, "queue input").is_err());
    }

    #[test]
    fn status_two_is_forbidden_four_a_conflict_and_the_rest_unavailable() {
        assert_eq!(response_error(2), MusicError::Forbidden);
        assert_eq!(response_error(4), MusicError::Conflict);
        assert_eq!(response_error(9), MusicError::OperationUnavailable);
        assert_eq!(definite_failure(12), MusicError::Unavailable);
        assert_eq!(definite_failure(15), MusicError::OperationUnavailable);
    }

    #[test]
    fn a_refusal_names_the_toast_the_player_shows() {
        assert_eq!(
            MusicError::OperationUnavailable.toast_key(),
            "music.error.operation-unavailable"
        );
        assert_eq!(
            MusicError::Unavailable.toast_key(),
            "music.error.unavailable"
        );
        assert_eq!(MusicError::Forbidden.toast_key(), "music.blocked.dj");
    }
}
