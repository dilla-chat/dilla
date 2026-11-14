//! Sealed Bot commands: a port of packages/client-core/src/bot/catalog.ts (`BotCatalogStore`: the key check and the Bot
//! user pin) and packages/client-core/src/bot/commands.ts (`BotCommandRelay` and `createBotCommands`: seal, send, open the
//! answers, the 5 s first-answer timeout, Retry with the same request id, the one stale-key resend).
//!
//! The rules this module holds:
//! - Nothing is sealed to a key this device has not verified in the core against the community's **pinned** Bot user. The
//!   first Bot user seen is pinned in the core store (only for an account the instance attested as a Bot); another one is
//!   `changed`, greyed, and never used. A held key change of the Bot's device list (or `E_UMK_CHANGED`) greys it `changed`.
//! - Each send makes a one-time reply secret in the core; only the newest send of a request id keeps one. An op 62 answer
//!   is opened with it (HPKE, the request id and the sequence number bound as AAD): an answer sealed to another key, for
//!   another request, replayed, or for a request this device did not send is dropped unseen.
//! - Only the sealed `final` ends a stream; op 62's own flag is the server's and is not trusted.
//!
//! The I/O (the core, the routes) is behind [`BotIo`], so the rules are tested with a fake Bot.

use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Mutex, Weak};
use std::time::Duration;

use async_trait::async_trait;
use dilla_core::bot::command::{ReplySecret, open_reply};
use tokio::sync::mpsc;

use crate::features::bot_admin::{BotArg, SealedAnswer, SealedBot, SealedCommand, SendError};
use crate::gateway::frames::{BotCatalogFrame, BotReplyFrame, Value};
use crate::state::bot::{BotCatalogView, BotCommandSpec, BotStatus, BotStatusState};
use crate::state::{ChannelId, CommunityId, UserId};

use super::catalog::{decode_catalog_response, decode_commands};

/// commands.ts `BOT_FIRST_ANSWER_MS`.
pub const FIRST_ANSWER: Duration = Duration::from_secs(5);
/// commands.ts `BOT_REPLY_HOLD_MS`: a reply secret is forgotten this long after the last answer (or the send).
pub const REPLY_HOLD: Duration = Duration::from_secs(120);
/// commands.ts `BOT_KEY_STALE_STATUS`.
pub const KEY_STALE_STATUS: u16 = 410;
/// commands.ts `BOT_TRANSIENT_RESEND_MS`.
pub const TRANSIENT_RESEND: Duration = Duration::from_secs(1);

/// The refusals that may mean another Bot account signed the key: the Bot user is looked up again.
const SIGNER_REFUSALS: [&str; 3] = ["E_BOT_SIGNATURE", "E_DEVICE_UNLISTED", "E_CREDENTIAL"];
/// The seal refusals of a key the core no longer holds as verified.
const STALE_SEAL: [&str; 2] = ["E_BOT_KEY_STALE", "E_BOT_STALE"];

/// A refusal from the core or the instance: its code and HTTP status (0 when local).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Refusal {
    pub code: String,
    pub status: u16,
}

impl Refusal {
    pub fn new(code: &str, status: u16) -> Self {
        Self {
            code: code.to_owned(),
            status,
        }
    }
}

/// What the server names as a community's Bot user (`BotUserCandidate`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Candidate {
    One([u8; 16]),
    None,
    Ambiguous,
}

/// What the sealed path needs of the core and the instance.
#[async_trait]
pub trait BotIo: Send + Sync {
    /// The server's Bot user for the community (the key's signer).
    async fn bot_user_of(&self, community: CommunityId) -> Candidate;
    /// Reads the candidate's device-list head and ingests its account-kind attestation.
    async fn ingest_attestation(&self, user: [u8; 16]) -> Result<(), Refusal>;
    /// `bot_user_pin`: `(state, pinned)`, state 0 pinned now, 1 the same user, 2 another user (the pin stays).
    async fn pin_bot_user(
        &self,
        community: CommunityId,
        user: [u8; 16],
    ) -> Result<(u8, [u8; 16]), Refusal>;
    /// `bot_user_pinned`.
    async fn pinned_bot_user(&self, community: CommunityId) -> Option<[u8; 16]>;
    /// `bot_command_key_verify`: the key id and fingerprint.
    async fn verify(
        &self,
        publication: Vec<u8>,
        user: [u8; 16],
    ) -> Result<([u8; 16], String), Refusal>;
    /// Reads the Bot user's device list and pins it (`bot_device_list_pin`): 0 current, 1 a key change held, 2 behind.
    async fn pin_device_list(&self, user: [u8; 16]) -> Result<u8, Refusal>;
    /// `bot_command_seal`: the sealed bytes and the one-time reply secret.
    async fn seal(
        &self,
        publication: Vec<u8>,
        user: [u8; 16],
        request: Vec<u8>,
    ) -> Result<(Vec<u8>, ReplySecret), Refusal>;
    /// POST /v1/communities/{community}/bot/commands (202).
    async fn post(
        &self,
        community: CommunityId,
        body: Vec<u8>,
        idem: Vec<u8>,
    ) -> Result<(), Refusal>;
    /// GET /v1/communities/{community}/bot/catalog.
    async fn fetch_catalog(&self, community: CommunityId) -> Result<Vec<u8>, Refusal>;
    /// GET /v1/communities/{community}/bot/channels (the ids this member can view).
    async fn fetch_bot_channels(&self, community: CommunityId) -> Result<Vec<ChannelId>, Refusal>;
    /// Unix seconds (the command's `sent_at`).
    fn now_s(&self) -> u64;
    /// 16 random bytes.
    fn random16(&self) -> [u8; 16];
    /// The community's catalog view changed.
    fn publish(&self, community: CommunityId, view: Option<BotCatalogView>);
    /// Runs `task` on the client's runtime (the UI thread has none).
    fn spawn(&self, task: Task);
}

/// A task for the client's runtime.
pub type Task = std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send>>;

#[derive(Clone, Debug)]
struct Verified {
    publication: Vec<u8>,
    /// The pinned Bot user the publication was checked against (t28-bot-review M1): a verdict is never carried, or
    /// sealed to, under another pin.
    pinned: [u8; 16],
    #[allow(
        dead_code,
        reason = "kept with the verdict, as catalog.ts does; the view shows the fingerprint"
    )]
    key_id: [u8; 16],
    fingerprint: String,
}

#[derive(Clone, Debug)]
struct Entry {
    revision: u64,
    commands: Vec<BotCommandSpec>,
    publication: Option<Vec<u8>>,
    verified: Option<Verified>,
    refusal: Option<String>,
    server_status: BotStatus,
    bot_channels: Option<Vec<ChannelId>>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Problem {
    Changed,
    Ambiguous,
}

#[derive(Default)]
struct Store {
    entries: HashMap<CommunityId, Entry>,
    pins: HashMap<CommunityId, [u8; 16]>,
    problems: HashMap<CommunityId, Problem>,
    /// Bot users whose pinned device list holds a key change.
    list_held: HashSet<[u8; 16]>,
    /// Bot users whose device list this connection pinned (current or held): not read again.
    list_settled: HashSet<[u8; 16]>,
    bot_names: HashMap<CommunityId, String>,
    /// Each community's newest key check (t28-bot-review M2): an answer of an older one changes nothing, so an older
    /// failure never clears a newer verdict and an older `Ok` never lands after a newer refusal.
    checks: HashMap<CommunityId, u64>,
}

impl Store {
    /// `statusOf`: the greyed reasons this device adds to the instance's status.
    fn status_of(&self, community: CommunityId, e: &Entry) -> BotStatus {
        let none = |state| BotStatus { state, since: None };
        match self.problems.get(&community) {
            Some(Problem::Changed) => return none(BotStatusState::Changed),
            Some(Problem::Ambiguous) => return none(BotStatusState::Unverified),
            None => {}
        }
        let pinned = self.pins.get(&community);
        if e.verified.is_none()
            && e.publication.is_some()
            && (e.refusal.as_deref() == Some("E_UMK_CHANGED")
                || pinned.is_some_and(|p| self.list_held.contains(p)))
        {
            return none(BotStatusState::Changed);
        }
        if e.verified.is_some() || e.server_status.state != BotStatusState::Ready {
            return e.server_status;
        }
        if e.publication.is_none() {
            BotStatus {
                state: BotStatusState::Offline,
                since: e.server_status.since,
            }
        } else {
            none(BotStatusState::Unverified)
        }
    }

    fn view(&self, community: CommunityId) -> Option<BotCatalogView> {
        let e = self.entries.get(&community)?;
        Some(BotCatalogView {
            revision: e.revision,
            commands: e.commands.clone(),
            status: self.status_of(community, e),
            bot_channels: e.bot_channels.clone(),
            bot_user: self.pins.get(&community).copied().map(UserId),
            bot_name: self.bot_names.get(&community).cloned().unwrap_or_default(),
            fingerprint: e.verified.as_ref().map(|v| v.fingerprint.clone()),
        })
    }

    /// The check held for `community` when it was of `publication`: a re-read or a push of the same key keeps it while it is
    /// checked again (the web's check is synchronous, so it never shows a key unchecked in between). Never across a
    /// problem with the Bot user.
    fn carried(&self, community: CommunityId, publication: Option<&Vec<u8>>) -> Option<Verified> {
        if self.problems.contains_key(&community) {
            return None;
        }
        let v = self.entries.get(&community)?.verified.as_ref()?;
        (publication == Some(&v.publication) && self.pins.get(&community) == Some(&v.pinned))
            .then(|| v.clone())
    }

    /// The publication the core accepted, which is the only one anything is sealed to.
    fn publication(&self, community: CommunityId) -> Option<(Vec<u8>, [u8; 16])> {
        if self.problems.contains_key(&community) {
            return None;
        }
        let v = self.entries.get(&community)?.verified.as_ref()?;
        let pinned = *self.pins.get(&community)?;
        (pinned == v.pinned).then(|| (v.publication.clone(), pinned))
    }
}

/// The community a publication `[1, community_id, key_id, …]` names.
fn publication_community(publication: &[u8]) -> Option<[u8; 16]> {
    let v = Value::decode(publication).ok()?;
    let a = v.as_array()?;
    if a.len() < 2 || a[0].as_uint() != Some(1) {
        return None;
    }
    a[1].as_bytes()?.try_into().ok()
}

/// One answer of the Bot (`BotAnswer`), opened.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Answer {
    pub seq: u32,
    pub status: u16,
    pub body: Vec<u8>,
    pub final_: bool,
}

/// `decodeBotAnswer`: the sealed plaintext `[status ≤ 65535, body, final 0|1]`.
pub fn decode_answer(plain: &[u8]) -> Option<(u16, Vec<u8>, bool)> {
    let v = Value::decode(plain).ok()?;
    match v.as_array()? {
        [Value::Uint(s), Value::Bytes(b), Value::Uint(f)] if *s <= 0xffff && *f <= 1 => {
            Some((*s as u16, b.clone(), *f == 1))
        }
        _ => None,
    }
}

/// `encodeBotAnswer` (the fake Bot's side).
pub fn encode_answer(status: u16, body: &[u8], final_: bool) -> Vec<u8> {
    Value::Array(vec![
        Value::Uint(u64::from(status)),
        Value::Bytes(body.to_vec()),
        Value::Uint(u64::from(final_)),
    ])
    .encode()
}

struct Pending {
    secret: ReplySecret,
    seen: HashSet<u32>,
    tx: mpsc::UnboundedSender<Answer>,
    /// Bumped on every answer: the hold timer of an older generation does nothing.
    generation: u64,
}

/// One sealed command (`BotCommandSubmit`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Submit {
    pub community: CommunityId,
    pub channel: Option<ChannelId>,
    pub operation: u64,
    pub args: Vec<BotArg>,
    pub request_id: [u8; 16],
}

/// How a run ended (`BotRunItem` for the last item).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum RunEnd {
    /// The final answer.
    Final(Answer),
    /// No first answer within 5 s: the run may still have happened. Retry sends the same request id again.
    Timeout,
    /// Refused before the Bot saw it, or a second stale key.
    Refused(Refusal),
}

/// The sealed path of one client: the key store and the reply secrets.
pub struct Sealed {
    io: Arc<dyn BotIo>,
    store: Mutex<Store>,
    pending: Mutex<HashMap<[u8; 16], Pending>>,
    generation: std::sync::atomic::AtomicU64,
    /// The runs of held rows (no answer yet), by row id: what their Retry sends again.
    held: Mutex<HashMap<u64, super::run::HeldRun>>,
    /// One catalog read at a time per community (catalog.ts `inFlight`): a later read lands after an earlier one, so an
    /// older status never overwrites a newer one.
    reads: Mutex<HashMap<CommunityId, Arc<tokio::sync::Mutex<()>>>>,
    connection: std::sync::atomic::AtomicU64,
}

fn lock<T>(m: &Mutex<T>) -> std::sync::MutexGuard<'_, T> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

impl Sealed {
    pub fn new(io: Arc<dyn BotIo>) -> Arc<Self> {
        Arc::new(Self {
            io,
            store: Mutex::default(),
            pending: Mutex::default(),
            generation: std::sync::atomic::AtomicU64::new(1),
            held: Mutex::default(),
            reads: Mutex::default(),
            connection: std::sync::atomic::AtomicU64::new(0),
        })
    }

    fn notify(&self, community: CommunityId) {
        let view = lock(&self.store).view(community);
        self.io.publish(community, view);
    }

    /// The community's catalog view.
    pub fn view(&self, community: CommunityId) -> Option<BotCatalogView> {
        lock(&self.store).view(community)
    }

    /// The Bot's display name for the source tag.
    pub fn set_bot_name(&self, community: CommunityId, name: String) {
        lock(&self.store).bot_names.insert(community, name);
    }

    fn pin_of(&self, community: CommunityId) -> Option<[u8; 16]> {
        lock(&self.store).pins.get(&community).copied()
    }

    async fn load_pin(&self, community: CommunityId) -> Option<[u8; 16]> {
        if let Some(p) = self.pin_of(community) {
            return Some(p);
        }
        let stored = self.io.pinned_bot_user(community).await?;
        lock(&self.store).pins.insert(community, stored);
        Some(stored)
    }

    /// op 61: a newer revision replaces the held catalog; an older one is ignored.
    pub async fn apply_frame(&self, frame: &BotCatalogFrame) {
        let community = CommunityId(frame.community_id);
        let Ok(commands) = decode_commands(&frame.commands) else {
            return;
        };
        let publication = match &frame.publication {
            Some(p) if p.is_empty() || p.len() > 256 => return,
            other => other.clone(),
        };
        {
            let mut s = lock(&self.store);
            let held = s.entries.get(&community);
            if held.is_some_and(|h| frame.revision <= h.revision) {
                return;
            }
            // op 61 carries no status, but only a running Bot publishes: ready, unless a degraded one stays degraded.
            let server_status = match held {
                Some(h) if h.server_status.state == BotStatusState::Degraded => h.server_status,
                _ => BotStatus {
                    state: BotStatusState::Ready,
                    since: None,
                },
            };
            let bot_channels = held.and_then(|h| h.bot_channels.clone());
            let verified = s.carried(community, publication.as_ref());
            s.entries.insert(
                community,
                Entry {
                    revision: frame.revision,
                    commands,
                    publication: publication.clone(),
                    verified,
                    refusal: None,
                    server_status,
                    bot_channels,
                },
            );
        }
        if self.load_pin(community).await.is_none() && publication.is_some() {
            self.resolve_bot(community).await;
        }
        self.verify_entry(community).await;
        self.notify(community);
    }

    /// Reads the catalog and the Bot channels now (a sync, a state change, a stale key). A failure keeps what is held.
    pub async fn refresh(&self, community: CommunityId) -> Result<(), Refusal> {
        let gate = lock(&self.reads).entry(community).or_default().clone();
        let _one_at_a_time = gate.lock().await;
        let body = self.io.fetch_catalog(community).await?;
        let decoded =
            decode_catalog_response(&body).map_err(|_| Refusal::new("E_BOT_CATALOG", 0))?;
        let channels = self.io.fetch_bot_channels(community).await.ok();
        if decoded.publication.is_some() {
            self.load_pin(community).await;
            // Every read looks the Bot user up again: a reinstall under another account is noticed, never adopted.
            self.resolve_bot(community).await;
        }
        {
            let mut s = lock(&self.store);
            let current = s.entries.get(&community).cloned();
            let keep = current
                .as_ref()
                .is_some_and(|c| c.revision > decoded.revision);
            let verified = s.carried(community, decoded.publication.as_ref());
            let mut entry = match (&current, keep) {
                (Some(c), true) => c.clone(),
                _ => Entry {
                    revision: decoded.revision,
                    commands: decoded.commands,
                    publication: decoded.publication,
                    verified,
                    refusal: None,
                    server_status: decoded.status,
                    bot_channels: None,
                },
            };
            entry.server_status = decoded.status;
            entry.bot_channels = channels.or_else(|| current.and_then(|c| c.bot_channels));
            s.entries.insert(community, entry);
        }
        self.verify_entry(community).await;
        self.notify(community);
        Ok(())
    }

    /// `resolveBot`: the first Bot user seen is pinned; the same again is fine; another is `changed` and never used; more
    /// than one Bot account is refused.
    async fn resolve_bot(&self, community: CommunityId) {
        let candidate = match self.io.bot_user_of(community).await {
            Candidate::Ambiguous => {
                lock(&self.store)
                    .problems
                    .insert(community, Problem::Ambiguous);
                return;
            }
            Candidate::None => return,
            Candidate::One(c) => c,
        };
        let mut ingested = false;
        if self.pin_of(community).is_none() {
            let _ = self.io.ingest_attestation(candidate).await;
            ingested = true;
        }
        let answer = loop {
            match self.io.pin_bot_user(community, candidate).await {
                Ok(a) => break a,
                Err(r) if !ingested && r.code == "E_DEVICE_LIST_STALE" => {
                    let _ = self.io.ingest_attestation(candidate).await;
                    ingested = true;
                }
                // Refused (no attestation yet, or a human): greyed unverified, tried again on the next read.
                Err(_) => return,
            }
        };
        let mut s = lock(&self.store);
        s.pins.insert(community, answer.1);
        match answer.0 {
            0 => {
                s.problems.remove(&community);
            }
            1 => {
                if s.problems.get(&community) == Some(&Problem::Ambiguous) {
                    s.problems.remove(&community);
                }
            }
            _ => {
                s.problems.insert(community, Problem::Changed);
            }
        }
    }

    /// `verifyEntry`: the held publication is checked in the core against the pinned Bot user.
    async fn verify_entry(&self, community: CommunityId) {
        for _ in 0..2 {
            let (publication, pinned, check) = {
                let mut s = lock(&self.store);
                let check = {
                    let n = s.checks.entry(community).or_default();
                    *n += 1;
                    *n
                };
                let problem = s.problems.contains_key(&community);
                let pinned = s.pins.get(&community).copied();
                let Some(e) = s.entries.get_mut(&community) else {
                    return;
                };
                // A check carried over from the same publication stays until this one answers.
                let (Some(publication), Some(pinned), false) =
                    (e.publication.clone(), pinned, problem)
                else {
                    e.verified = None;
                    e.refusal = None;
                    return;
                };
                (publication, pinned, check)
            };
            // A publication for another community is refused before the core sees it.
            if publication_community(&publication) != Some(community.0) {
                if let Some(e) = lock(&self.store).entries.get_mut(&community) {
                    e.verified = None;
                    e.refusal = None;
                }
                return;
            }
            match self.io.verify(publication.clone(), pinned).await {
                Ok((key_id, fingerprint)) => {
                    let mut s = lock(&self.store);
                    s.list_held.remove(&pinned);
                    let current = s.checks.get(&community) == Some(&check)
                        && s.pins.get(&community) == Some(&pinned)
                        && !s.problems.contains_key(&community);
                    if let Some(e) = s.entries.get_mut(&community)
                        && current
                        && e.publication.as_ref() == Some(&publication)
                    {
                        e.verified = Some(Verified {
                            publication,
                            pinned,
                            key_id,
                            fingerprint,
                        });
                        e.refusal = None;
                    }
                    return;
                }
                Err(r) => {
                    {
                        let mut s = lock(&self.store);
                        if s.checks.get(&community) != Some(&check) {
                            // A newer check answers for the key.
                            return;
                        }
                        if let Some(e) = s.entries.get_mut(&community) {
                            e.verified = None;
                            e.refusal = Some(r.code.clone());
                        }
                    }
                    if SIGNER_REFUSALS.contains(&r.code.as_str()) {
                        self.resolve_bot(community).await;
                        return;
                    }
                    if r.code != "E_DEVICE_LIST_STALE"
                        || lock(&self.store).list_settled.contains(&pinned)
                    {
                        return;
                    }
                    // No device list for the Bot (no shared group), or one behind its head: read and pin it once on this
                    // connection, then check again.
                    match self.io.pin_device_list(pinned).await {
                        Ok(state) => {
                            let mut s = lock(&self.store);
                            if state != 2 {
                                s.list_settled.insert(pinned);
                            }
                            if state == 1 {
                                s.list_held.insert(pinned);
                            } else {
                                s.list_held.remove(&pinned);
                            }
                        }
                        Err(_) => return,
                    }
                }
            }
        }
    }

    /// A new gateway connection: device lists are read again when needed.
    pub fn new_connection(&self) {
        lock(&self.store).list_settled.clear();
    }

    /// The core no longer holds the key as verified: drop this device's verdict too.
    fn invalidate(&self, community: CommunityId) {
        if let Some(e) = lock(&self.store).entries.get_mut(&community) {
            e.verified = None;
        }
    }

    /// A key that did not check yet is checked again (local); `true` when it is usable now.
    async fn reverify(&self, community: CommunityId) -> bool {
        self.verify_entry(community).await;
        let usable = lock(&self.store).publication(community).is_some();
        self.notify(community);
        usable
    }

    async fn usable(&self, community: CommunityId) -> Result<(Vec<u8>, [u8; 16]), Refusal> {
        if let Some(p) = lock(&self.store).publication(community) {
            return Ok(p);
        }
        if self.reverify(community).await
            && let Some(p) = lock(&self.store).publication(community)
        {
            return Ok(p);
        }
        // No key this device verified: the commands are greyed and nothing is sealed.
        Err(Refusal::new("E_BOT_OFFLINE", 503))
    }

    /// The request the core seals: `[request_id, community_id, channel_id | null, operation, args, sent_at]`.
    fn request_bytes(&self, req: &Submit) -> Vec<u8> {
        Value::Array(vec![
            Value::Bytes(req.request_id.to_vec()),
            Value::Bytes(req.community.0.to_vec()),
            req.channel
                .map_or(Value::Null, |c| Value::Bytes(c.0.to_vec())),
            Value::Uint(req.operation),
            SealedCommand {
                operation: req.operation,
                channel: req.channel,
                args: req.args.clone(),
                request_id: req.request_id,
            }
            .args_value(),
            Value::Uint(self.io.now_s()),
        ])
        .encode()
    }

    /// `BotCommandRelay.send`: seals and sends one command; its answers arrive on the receiver. A resend of the same
    /// request id replaces the earlier reply secret (answers sealed to it are dropped).
    pub async fn send(
        self: &Arc<Self>,
        req: &Submit,
    ) -> Result<mpsc::UnboundedReceiver<Answer>, Refusal> {
        let (mut publication, mut user) = self.usable(req.community).await?;
        let request = self.request_bytes(req);
        let sealed = match self
            .io
            .seal(publication.clone(), user, request.clone())
            .await
        {
            Ok(s) => s,
            Err(r) if STALE_SEAL.contains(&r.code.as_str()) => {
                // The core no longer holds the key as verified: drop the verdict, check again, else read the catalog once.
                self.invalidate(req.community);
                if !self.reverify(req.community).await {
                    let _ = self.refresh(req.community).await;
                }
                (publication, user) = self.usable(req.community).await?;
                self.io.seal(publication, user, request).await?
            }
            Err(r) => return Err(r),
        };
        let (sealed, secret) = sealed;
        let (tx, rx) = mpsc::unbounded_channel();
        let generation = self
            .generation
            .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
        lock(&self.pending).insert(
            req.request_id,
            Pending {
                secret,
                seen: HashSet::new(),
                tx,
                generation,
            },
        );
        self.arm_hold(req.request_id, generation);
        let body = Value::Array(vec![
            Value::Bytes(req.request_id.to_vec()),
            Value::Bytes(sealed),
        ])
        .encode();
        if let Err(r) = self
            .io
            .post(req.community, body, self.io.random16().to_vec())
            .await
        {
            // Refused: nothing reached the Bot under this seal, so its reply secret goes now.
            let mut p = lock(&self.pending);
            if p.get(&req.request_id)
                .is_some_and(|e| e.generation == generation)
            {
                p.remove(&req.request_id);
            }
            return Err(r);
        }
        Ok(rx)
    }

    fn arm_hold(self: &Arc<Self>, request_id: [u8; 16], generation: u64) {
        let weak: Weak<Self> = Arc::downgrade(self);
        self.io.spawn(Box::pin(async move {
            tokio::time::sleep(REPLY_HOLD).await;
            if let Some(this) = weak.upgrade() {
                let mut p = lock(&this.pending);
                if p.get(&request_id)
                    .is_some_and(|e| e.generation == generation)
                {
                    p.remove(&request_id);
                }
            }
        }));
    }

    /// Runs `task` on the client's runtime.
    pub fn spawn(&self, task: Task) {
        self.io.spawn(task);
    }

    /// op 62: opens the answer with the newest send's reply secret. A replayed, unknown or unopenable answer is dropped.
    pub fn on_reply(self: &Arc<Self>, frame: &BotReplyFrame) {
        let mut pending = lock(&self.pending);
        let Some(entry) = pending.get_mut(&frame.request_id) else {
            return;
        };
        if entry.seen.contains(&frame.seq) {
            return;
        }
        let Ok(plain) = open_reply(&entry.secret, frame.request_id, frame.seq, &frame.sealed)
        else {
            return;
        };
        let Some((status, body, final_)) = decode_answer(&plain) else {
            return;
        };
        entry.seen.insert(frame.seq);
        let _ = entry.tx.send(Answer {
            seq: frame.seq,
            status,
            body,
            final_,
        });
        if final_ {
            pending.remove(&frame.request_id);
        } else {
            let generation = self
                .generation
                .fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            entry.generation = generation;
            drop(pending);
            self.arm_hold(frame.request_id, generation);
        }
    }

    /// After a timeout, the answers to `request_id` that still arrive (TS `onLate`): this device keeps the reply secret
    /// for [`REPLY_HOLD`] after the send, and they go here from now on. `None` when it holds none.
    pub fn late(&self, request_id: [u8; 16]) -> Option<mpsc::UnboundedReceiver<Answer>> {
        let mut pending = lock(&self.pending);
        let entry = pending.get_mut(&request_id)?;
        let (tx, rx) = mpsc::unbounded_channel();
        entry.tx = tx;
        Some(rx)
    }

    /// The run is over for its caller: its reply secret goes now.
    pub fn forget(&self, request_id: [u8; 16]) {
        lock(&self.pending).remove(&request_id);
    }

    /// Sign-out, revocation: every reply secret goes, and every held run with it.
    pub fn forget_all(&self) {
        lock(&self.held).clear();
        lock(&self.pending).clear();
    }

    /// Every gateway ready, loss or refusal starts a new generation: a ready's wait for the structure stops when it ends.
    pub(crate) fn next_connection_generation(&self) -> u64 {
        self.connection
            .fetch_add(1, std::sync::atomic::Ordering::Relaxed)
            + 1
    }

    /// The current connection generation.
    pub(crate) fn connection_generation(&self) -> u64 {
        self.connection.load(std::sync::atomic::Ordering::Relaxed)
    }

    /// Unix seconds by the instance's clock where known (the command's `sent_at`).
    pub fn now_s(&self) -> u64 {
        self.io.now_s()
    }

    /// Keeps a held row's run for its Retry.
    pub(crate) fn hold(&self, row: u64, run: super::run::HeldRun) {
        lock(&self.held).insert(row, run);
    }

    /// Takes a held row's run (its Retry); `None` when there is none.
    pub(crate) fn take_held(&self, row: u64) -> Option<super::run::HeldRun> {
        lock(&self.held).remove(&row)
    }

    /// Drops a held row's run and forgets its reply secret (the row was dismissed or evicted).
    pub(crate) fn drop_held(&self, row: u64) {
        let run = lock(&self.held).remove(&row);
        if let Some(run) = run {
            self.forget(run.submit.request_id);
        }
    }

    /// Whether row `row` holds a run.
    pub fn holds_row(&self, row: u64) -> bool {
        lock(&self.held).contains_key(&row)
    }

    /// Whether this device holds a reply secret for `request_id`.
    pub fn holds(&self, request_id: [u8; 16]) -> bool {
        lock(&self.pending).contains_key(&request_id)
    }

    fn catalog_ready(&self, community: CommunityId) -> bool {
        self.view(community)
            .is_some_and(|v| v.status.state == BotStatusState::Ready)
    }

    /// `createBotCommands().sendBotCommand`: one run. Each answer before the last goes to `progress`; the run ends with
    /// the final answer, a timeout (no first answer in 5 s) or a refusal. A 410 stale answer (or an `E_BOT_KEY_STALE`
    /// refusal) reads the catalog once and resends the same request id; the Bot's start-up refusals (401
    /// `E_BOT_SIGNATURE`, 503 `E_BOT_OFFLINE`) are resent once, a second later, while the catalog says ready.
    pub async fn run(
        self: &Arc<Self>,
        req: &Submit,
        mut progress: impl FnMut(&Answer) + Send,
    ) -> RunEnd {
        let (mut stale_resent, mut transient_resent) = (false, false);
        'send: loop {
            let mut rx = match self.send(req).await {
                Ok(rx) => rx,
                Err(r) if r.code == "E_BOT_KEY_STALE" && !stale_resent => {
                    stale_resent = true;
                    let _ = self.refresh(req.community).await;
                    continue 'send;
                }
                Err(r) => return RunEnd::Refused(r),
            };
            let mut last_seq: Option<u32> = None;
            let mut wait = FIRST_ANSWER;
            loop {
                // No first answer in 5 s, or answers stopped for the hold time after one: the run is over, unconfirmed.
                let Ok(Some(answer)) = tokio::time::timeout(wait, rx.recv()).await else {
                    return RunEnd::Timeout;
                };
                if answer.status == KEY_STALE_STATUS {
                    if stale_resent {
                        return RunEnd::Refused(Refusal::new("E_BOT_KEY_STALE", answer.status));
                    }
                    stale_resent = true;
                    let _ = self.refresh(req.community).await;
                    continue 'send;
                }
                if answer.final_
                    && answer.seq == 0
                    && !transient_resent
                    && is_transient(answer.status, &answer.body)
                    && self.catalog_ready(req.community)
                {
                    transient_resent = true;
                    tokio::time::sleep(TRANSIENT_RESEND).await;
                    continue 'send;
                }
                if last_seq.is_some_and(|l| answer.seq <= l) {
                    continue;
                }
                last_seq = Some(answer.seq);
                if answer.final_ {
                    return RunEnd::Final(answer);
                }
                progress(&answer);
                wait = REPLY_HOLD;
            }
        }
    }
}

/// A Bot answer that is one of its start-up refusals: the status and, as the body, exactly that status's code.
fn is_transient(status: u16, body: &[u8]) -> bool {
    matches!(
        (status, body),
        (401, b"E_BOT_SIGNATURE") | (503, b"E_BOT_OFFLINE")
    )
}

/// The transport Task 21's Bot operations use: one run, its final answer; a timeout is resent once with the same request
/// id (the Bot answers what it answered, or runs it if it never arrived) before it is reported.
pub struct Transport(pub Arc<Sealed>);

#[async_trait]
impl SealedBot for Transport {
    async fn send(
        &self,
        community: CommunityId,
        command: SealedCommand,
    ) -> Result<SealedAnswer, SendError> {
        let submit = Submit {
            community,
            channel: command.channel,
            operation: command.operation,
            args: command.args,
            request_id: command.request_id,
        };
        for attempt in 0..2 {
            match self.0.run(&submit, |_| {}).await {
                RunEnd::Final(a) => {
                    return Ok(SealedAnswer {
                        status: a.status,
                        body: a.body,
                    });
                }
                RunEnd::Refused(r) => {
                    return Err(SendError::Refused {
                        code: r.code,
                        status: r.status,
                    });
                }
                RunEnd::Timeout if attempt == 0 => continue,
                RunEnd::Timeout => break,
            }
        }
        self.0.forget(submit.request_id);
        Err(SendError::Timeout)
    }
}

#[cfg(test)]
pub(crate) mod tests;
