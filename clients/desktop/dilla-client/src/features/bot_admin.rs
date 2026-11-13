//! Area **bot admin** (Task 21): the Bot server settings: health, Bot channels, webhooks, the moderation log page.
//!
//! Ports: packages/client-core/src/bot/{admin,moderation-log,provider-status}.ts and the routes of
//! `packages/client-core/src/http/routes.ts` that the Bot page calls (all socket routes).
//!
//! A webhook's address carries its secret. It is answered only by [`BotAdmin::create_webhook`] and
//! [`BotAdmin::rotate_webhook`], as a [`Zeroizing`] string the caller shows once and drops; it is never kept in
//! [`BotAdminState`], never logged and never in an error. The response buffer it was read from is zeroized too.
//!
//! The sealed Bot commands (status read 94, provider status 24, moderation pages 60 and 64, and the music operations) go
//! through [`SealedBot`], the seam Task 20's `features::bot` implements (it owns the Bot's command key, catalog and
//! sealing). [`default_transport`] is that transport; tests supply a fake.

use std::collections::{BTreeSet, HashMap};
use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use tokio::sync::oneshot;
use zeroize::Zeroizing;

use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::Value;
use crate::http::fill_path;
use crate::http::routes::{self as r, lookup};
use crate::seams::Route;
use crate::state::bot_admin::{
    BOT_STATUS_READ, BotCatalog, BotPageStatus, BotWebhook, Installation, MODERATION_CASES,
    MODERATION_EVIDENCE, MUSIC_IDLE_MINUTES, MUSIC_VOLUMES, MUTATIONS, ModerationPage,
    PROVIDER_STALE_S, PROVIDER_STATUS, ProviderStatusState, decode_bot_status, decode_bot_webhooks,
    decode_installation, decode_moderation_page, decode_music_provider_status, sealed_operation,
};
use crate::state::{ChannelId, CommunityId, hex};
use crate::sync::api::ApiError;
use crate::sync::value::{self as v, bin16};

// ---- the sealed command seam (operations.ts) ----

/// One argument of a sealed command.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum BotArg {
    Uint(u64),
    Text(String),
    Bytes(Vec<u8>),
    Null,
}

impl BotArg {
    fn value(&self) -> Value {
        match self {
            Self::Uint(n) => Value::Uint(*n),
            Self::Text(t) => Value::Text(t.clone()),
            Self::Bytes(b) => Value::Bytes(b.clone()),
            Self::Null => Value::Null,
        }
    }
}

/// What [`BotOperations`] asks of the transport: a sealed operation, its arguments in the Bot's order (a mutation's
/// expected revision first), and the request id (16 bytes) that makes a resend run once.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SealedCommand {
    pub operation: u64,
    pub channel: Option<ChannelId>,
    pub args: Vec<BotArg>,
    pub request_id: [u8; 16],
}

impl SealedCommand {
    /// The arguments as the CBOR array the Bot opens.
    pub fn args_value(&self) -> Value {
        Value::Array(self.args.iter().map(BotArg::value).collect())
    }
}

/// The Bot's final answer: `200 [revision|null, data, extra?]`, or a bare refusal code as the body of another status.
#[derive(Clone, Debug)]
pub struct SealedAnswer {
    pub status: u16,
    pub body: Vec<u8>,
}

/// Why a command did not get an answer.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum SendError {
    /// The command never ran: the instance or the Bot refused it (offline, an operation the Bot does not offer, ...).
    #[error("the Bot did not take the request ({code})")]
    Refused { code: String, status: u16 },
    /// No answer in time (after the transport's own one resend): the Bot may still have acted. Sending the same request id
    /// again settles it.
    #[error("the Bot did not answer in time")]
    Timeout,
}

/// Sends one sealed command to the community's Bot and waits for its pushed answer (op 62). Task 20's `Bot` implements it.
#[async_trait]
pub trait SealedBot: Send + Sync {
    async fn send(
        &self,
        community: CommunityId,
        command: SealedCommand,
    ) -> Result<SealedAnswer, SendError>;
}

/// Task 20's transport: sealed to the community's verified, pinned Bot key (`features::bot::sealed`). With no such key a
/// command is refused with `E_BOT_OFFLINE`.
pub(crate) fn default_transport(inner: &Arc<Inner>) -> Arc<dyn SealedBot> {
    crate::features::bot::Bot::of(inner).transport()
}

/// An app operation's request, in v1 terms (operations.ts `BotOperationRequest`).
#[derive(Clone, Debug, Default)]
pub struct BotRequest {
    /// The v1 operation (a sealed-only read is itself).
    pub operation: u64,
    pub channel: Option<ChannelId>,
    /// Ids go as 32-hex text and are sent as 16 bytes where the operation takes one.
    pub args: Vec<BotArg>,
    /// A mutation's expected revision.
    pub revision: Option<u64>,
    pub request_id: Option<[u8; 16]>,
}

/// One answer of an app operation or sealed read.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BotResult {
    pub request_id: [u8; 16],
    /// 0 ok, or the v1 status a refusal stands for (1 invalid, 2 forbidden, 3 stale, 4 conflict, 5 unavailable, ...).
    pub status: u8,
    pub revision: Option<u64>,
    pub data: Vec<u8>,
}

/// Why an operation failed.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum OpError {
    #[error("the Bot did not take the request ({code})")]
    Refused { code: String, request_id: [u8; 16] },
    #[error("the Bot did not answer in time")]
    Uncertain { request_id: [u8; 16] },
    #[error("invalid Bot request")]
    Invalid,
}

/// The v1 argument positions that hold an id (sent as 16 bytes when given as hex).
fn id_args(operation: u64) -> &'static [usize] {
    match operation {
        21 | 25 | 31 | 51 | 52 | 61 | 71 | 73 | 74 | 75 | 82 | 83 => &[0],
        22 | 72 => &[0, 1],
        _ => &[],
    }
}

/// A sealed answer's bare refusal code, as the v1 status it stands for.
fn status_of_code(code: &str) -> Option<u8> {
    Some(match code {
        "E_INVALID" => 1,
        "E_FORBIDDEN" => 2,
        "E_BOT_STALE" => 3,
        "E_CONFLICT" => 4,
        "E_UNAVAILABLE" => 5,
        "E_BUSY" => 6,
        "E_NOT_FOUND" => 7,
        "E_UPGRADE_REQUIRED" => 8,
        "E_INTERNAL" => 9,
        _ => return None,
    })
}

/// `appOperationAnswer`: 200 `[revision, data, extra?]`, or a refusal's v1 status.
fn app_answer(request_id: [u8; 16], answer: &SealedAnswer) -> Result<BotResult, OpError> {
    let refused = |code: &str| OpError::Refused {
        code: code.to_owned(),
        request_id,
    };
    if answer.status == 200 {
        let value = Value::decode(&answer.body).map_err(|_| refused("E_BOT_ANSWER"))?;
        let f = value.as_array().ok_or_else(|| refused("E_BOT_ANSWER"))?;
        if !(2..=3).contains(&f.len()) {
            return Err(refused("E_BOT_ANSWER"));
        }
        let revision = match &f[0] {
            Value::Null => None,
            other => Some(v::u53(other).map_err(|_| refused("E_BOT_ANSWER"))?),
        };
        let data = f[1]
            .as_bytes()
            .ok_or_else(|| refused("E_BOT_ANSWER"))?
            .to_vec();
        return Ok(BotResult {
            request_id,
            status: 0,
            revision,
            data,
        });
    }
    let code = String::from_utf8_lossy(&answer.body).into_owned();
    match status_of_code(&code) {
        Some(status) => Ok(BotResult {
            request_id,
            status,
            revision: None,
            data: Vec::new(),
        }),
        None => Err(refused(&clean_code(&code))),
    }
}

/// 16 random bytes, never all zero: a request id.
pub fn new_request_id() -> [u8; 16] {
    let mut b = [0u8; 16];
    let _ = getrandom::fill(&mut b);
    if b == [0; 16] {
        b[15] = 1;
    }
    b
}

const SENT_LIMIT: usize = 128;

/// Bot operations for one community over a [`SealedBot`]: it prepares each request the way operations.ts does and remembers
/// what it sent, so [`BotOperations::read`] can resend a request id and the Bot answers what it answered then.
pub struct BotOperations {
    community: CommunityId,
    transport: Arc<dyn SealedBot>,
    sent: Mutex<Sent>,
}

#[derive(Default)]
struct Sent {
    order: Vec<[u8; 16]>,
    by_id: HashMap<[u8; 16], SealedCommand>,
}

impl BotOperations {
    pub fn new(community: CommunityId, transport: Arc<dyn SealedBot>) -> Self {
        Self {
            community,
            transport,
            sent: Mutex::new(Sent::default()),
        }
    }

    /// The sealed command a request becomes: ids as 16 bytes, a mutation's revision first, the sealed operation.
    pub fn prepare(&self, input: &BotRequest) -> Result<SealedCommand, OpError> {
        let ids = id_args(input.operation);
        let mut args = Vec::with_capacity(input.args.len() + 1);
        for (i, a) in input.args.iter().enumerate() {
            args.push(match a {
                BotArg::Text(t) if ids.contains(&i) && is_id(t) => {
                    BotArg::Bytes(unhex(t).ok_or(OpError::Invalid)?.to_vec())
                }
                other => other.clone(),
            });
        }
        if MUTATIONS.contains(&input.operation) {
            args.insert(0, BotArg::Uint(input.revision.unwrap_or(0)));
        }
        Ok(SealedCommand {
            operation: sealed_operation(input.operation),
            channel: input.channel,
            args,
            request_id: input.request_id.unwrap_or_else(new_request_id),
        })
    }

    async fn run(&self, command: SealedCommand) -> Result<BotResult, OpError> {
        let request_id = command.request_id;
        match self.transport.send(self.community, command).await {
            Ok(answer) => app_answer(request_id, &answer),
            Err(SendError::Timeout) => Err(OpError::Uncertain { request_id }),
            Err(SendError::Refused { code, .. }) => Err(OpError::Refused {
                code: clean_code(&code),
                request_id,
            }),
        }
    }

    /// Sends the request and waits for the Bot's answer.
    pub async fn request(&self, input: BotRequest) -> Result<BotResult, OpError> {
        let command = self.prepare(&input)?;
        {
            let mut sent = self.sent.lock().unwrap_or_else(|e| e.into_inner());
            if sent
                .by_id
                .insert(command.request_id, command.clone())
                .is_none()
            {
                sent.order.push(command.request_id);
            }
            while sent.order.len() > SENT_LIMIT {
                let oldest = sent.order.remove(0);
                sent.by_id.remove(&oldest);
            }
        }
        self.run(command).await
    }

    /// Sends a request this page sent before again, with its request id: the Bot answers what it answered then, or runs it
    /// if it never arrived. `None` when this page never sent it.
    pub async fn read(
        &self,
        request_id: [u8; 16],
        operation: u64,
    ) -> Result<Option<BotResult>, OpError> {
        let prior = {
            let sent = self.sent.lock().unwrap_or_else(|e| e.into_inner());
            sent.by_id.get(&request_id).cloned()
        };
        match prior {
            Some(c) if c.operation == sealed_operation(operation) => self.run(c).await.map(Some),
            _ => Ok(None),
        }
    }
}

fn is_id(s: &str) -> bool {
    s.len() == 32 && s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

fn unhex(s: &str) -> Option<[u8; 16]> {
    if !is_id(s) {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, byte) in out.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).ok()?;
    }
    Some(out)
}

// ---- the handle ----

/// The Bot admin area's handle: cheap to clone, no I/O on construction. Hold one per page: it remembers the requests it sent
/// (for a resend that settles an unanswered one).
#[derive(Clone)]
pub struct BotAdmin {
    inner: Arc<Inner>,
    ops: Arc<Mutex<HashMap<CommunityId, Arc<BotOperations>>>>,
    transport: Arc<dyn SealedBot>,
}

impl Client {
    /// The [`BotAdmin`] handle.
    pub fn bot_admin(&self) -> BotAdmin {
        let inner = self.inner().clone();
        let transport = default_transport(&inner);
        BotAdmin::new(inner, transport)
    }
}

/// A webhook just made or rotated: its address, which is shown once.
pub struct WebhookAddress {
    pub webhook_id: [u8; 16],
    /// The address, secret included. Held only while its dialog is open.
    pub url: Zeroizing<String>,
}

impl std::fmt::Debug for WebhookAddress {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("WebhookAddress")
            .field("webhook_id", &hex(&self.webhook_id))
            .field("url", &"<secret>")
            .finish()
    }
}

/// `reAddBot`'s answer.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ReAdd {
    /// The Bot's old leaf is being proposed out: ask again once it is committed.
    Pending,
    /// The Bot is proposed in.
    Added,
}

/// The restart counters (`[requested, handled]`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct RestartCounts {
    pub requested: u64,
    pub handled: u64,
}

const RETRY_MAX_ATTEMPTS: u32 = 5;

/// A refusal code as it may be shown: the part before any `:`, 1 to 32 of `A-Z`, `0-9` and `_`, else `E_UNKNOWN`. The Bot's answers and the
/// instance's bodies are not trusted to carry text.
pub fn clean_code(raw: &str) -> String {
    // A locally made refusal may carry detail after a colon ("E_ROUTE_INPUT: invalid ..."): only the code is kept.
    let raw = raw.split_once(':').map_or(raw, |(code, _)| code);
    let ok = (1..=32).contains(&raw.len())
        && raw
            .bytes()
            .all(|b| b.is_ascii_uppercase() || b.is_ascii_digit() || b == b'_');
    if ok {
        raw.to_owned()
    } else {
        "E_UNKNOWN".to_owned()
    }
}

/// An instance refusal as the error carries it: `CODE (status)` for an HTTP status, so a page can read both
/// ([`split_refusal`]); the code is cleaned ([`clean_code`]).
fn sync_err(e: ApiError) -> ClientError {
    if e.status == 0 && e.code == "E_NETWORK" {
        ClientError::Network(e.detail)
    } else if e.status == 0 {
        ClientError::Sync(clean_code(&e.code))
    } else {
        ClientError::Sync(format!("{} ({})", clean_code(&e.code), e.status))
    }
}

/// The code and the HTTP status of a [`ClientError::Sync`] text made by this module.
pub fn split_refusal(text: &str) -> (&str, Option<u16>) {
    match text.split_once(" (") {
        Some((code, rest)) => (code, rest.strip_suffix(')').and_then(|n| n.parse().ok())),
        None => (text, None),
    }
}

fn decode_err(e: v::DecodeError) -> ClientError {
    ClientError::Sync(format!("E_DECODE: {}", e.0))
}

impl BotAdmin {
    pub(crate) fn new(inner: Arc<Inner>, transport: Arc<dyn SealedBot>) -> Self {
        Self {
            inner,
            ops: Arc::default(),
            transport,
        }
    }

    pub(crate) fn inner(&self) -> &Arc<Inner> {
        &self.inner
    }

    /// A handle over another transport (tests, and Task 20's at merge).
    pub fn with_transport(mut self, transport: Arc<dyn SealedBot>) -> Self {
        self.transport = transport;
        self.ops = Arc::default();
        self
    }

    /// This community's operations over the transport.
    pub fn operations(&self, community: CommunityId) -> Arc<BotOperations> {
        let mut ops = self.ops.lock().unwrap_or_else(|e| e.into_inner());
        ops.entry(community)
            .or_insert_with(|| Arc::new(BotOperations::new(community, self.transport.clone())))
            .clone()
    }

    /// One socket route's call, with `socket_call`'s 429 wait.
    async fn call<R: Route>(
        inner: &Arc<Inner>,
        args: &[&str],
        body: Vec<u8>,
        _idem: bool,
    ) -> Result<Zeroizing<Vec<u8>>, ApiError> {
        Self::call_with::<R>(inner, args, Zeroizing::new(body), None).await
    }

    /// [`Self::call`] with a body that is zeroized on drop (a step-up token is in it) and the idempotency key of the
    /// request (a mutation without one gets a random key). Every copy of the body is a `Zeroizing` buffer (the caller's, each
    /// attempt's, the transport queue's), wiped when it drops. Not wiped: the encoded frame, a plain buffer sized for it
    /// (no growth) that lives in the gateway's writer channel until `write_loop` hands it to the websocket library, which
    /// takes ownership of its bytes.
    async fn call_with<R: Route>(
        inner: &Arc<Inner>,
        args: &[&str],
        body: Zeroizing<Vec<u8>>,
        idem: Option<Vec<u8>>,
    ) -> Result<Zeroizing<Vec<u8>>, ApiError> {
        let entry = lookup(&R::METHOD, R::PATH)
            .ok_or_else(|| ApiError::local(0, "E_ROUTE", "the route is not in the table"))?;
        let route = fill_path(entry.path, args)
            .map_err(|e| ApiError::local(0, "E_ROUTE", &e.to_string()))?;
        let method: &'static str = match R::METHOD {
            http::Method::GET => "GET",
            http::Method::POST => "POST",
            http::Method::PUT => "PUT",
            http::Method::PATCH => "PATCH",
            http::Method::DELETE => "DELETE",
            _ => return Err(ApiError::local(0, "E_ROUTE", "unsupported method")),
        };
        let idem = (method != "GET").then(|| idem.unwrap_or_else(|| new_request_id().to_vec()));
        for attempt in 1..=RETRY_MAX_ATTEMPTS {
            let answer = inner
                .socket_requests()
                .request(method, route.clone(), body.clone(), idem.clone())
                .await?;
            let answer_body = Zeroizing::new(answer.body);
            if entry.accepts(answer.status) {
                if entry
                    .max_body
                    .is_some_and(|cap| answer_body.len() as u64 > cap)
                {
                    return Err(ApiError::local(
                        answer.status,
                        "E_BODY_TOO_LARGE",
                        "the response body is too large",
                    ));
                }
                return Ok(answer_body);
            }
            let error = ApiError::from_body(answer.status, &answer_body);
            if error.status == 429 && attempt < RETRY_MAX_ATTEMPTS {
                tokio::time::sleep(std::time::Duration::from_millis(
                    error.retry_after_ms.unwrap_or(1000),
                ))
                .await;
                continue;
            }
            return Err(error);
        }
        unreachable!("the loop returns on its last attempt")
    }

    fn spawn<T: Send + 'static>(
        &self,
        f: impl std::future::Future<Output = Result<T, ClientError>> + Send + 'static,
    ) -> oneshot::Receiver<Result<T, ClientError>> {
        self.inner.spawn(f)
    }

    // ---- health and the catalog ----

    /// Records what the instance's catalog says of the Bot (Task 20's op 61 handler calls it at merge): the state, its
    /// server time, the command key's fingerprint and the Bot channels this member can view.
    pub fn set_catalog(&self, community: CommunityId, catalog: Option<BotCatalog>) {
        self.inner.publish(move |s| match catalog {
            Some(c) => {
                s.bot_admin.catalog.insert(community, c);
            }
            None => {
                s.bot_admin.catalog.remove(&community);
            }
        });
    }

    /// The Bot's own status read (94): the Bot channels it needs re-adding to and the last music starts. A manager reads it
    /// when the catalog offers it.
    pub fn refresh_status(
        &self,
        community: CommunityId,
    ) -> oneshot::Receiver<Result<BotPageStatus, ClientError>> {
        let (this, inner) = (self.clone(), self.inner.clone());
        self.spawn(async move {
            let result = this
                .operations(community)
                .request(BotRequest {
                    operation: BOT_STATUS_READ,
                    ..BotRequest::default()
                })
                .await
                .map_err(op_err)?;
            if result.status != 0 {
                return Err(ClientError::Sync(format!("E_BOT_STATUS_{}", result.status)));
            }
            let status = decode_bot_status(&result.data).map_err(decode_err)?;
            let kept = status.clone();
            inner.publish(move |s| {
                s.bot_admin.needs_re_add.insert(
                    community,
                    kept.needs_re_add.iter().copied().collect::<BTreeSet<_>>(),
                );
                s.bot_admin
                    .music_starts
                    .insert(community, kept.music_starts);
            });
            Ok(status)
        })
    }

    /// Adds the Bot to a Bot channel it could not join on its own.
    pub fn re_add(
        &self,
        community: CommunityId,
        channel: ChannelId,
    ) -> oneshot::Receiver<Result<ReAdd, ClientError>> {
        let this = self.clone();
        self.spawn(async move {
            let body = Self::call::<r::ReAddBot>(
                &this.inner,
                &[&community.hex(), &channel.hex()],
                Vec::new(),
                true,
            )
            .await
            .map_err(sync_err)?;
            let value = Value::decode(&body).map_err(|_| decode_err(v::DecodeError("not CBOR")))?;
            let state = v::uint(&v::arr(&value, 1).map_err(decode_err)?[0]).map_err(decode_err)?;
            let outcome = match state {
                0 => ReAdd::Added,
                1 => ReAdd::Pending,
                _ => return Err(decode_err(v::DecodeError("invalid re-add state"))),
            };
            drop(this.refresh_status(community));
            Ok(outcome)
        })
    }

    /// Marks a text channel as a Bot channel: the instance proposes the Bot into its group.
    pub fn mark_bot_channel(
        &self,
        community: CommunityId,
        channel: ChannelId,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            Self::call::<r::MarkBotChannel>(
                &inner,
                &[&community.hex(), &channel.hex()],
                Vec::new(),
                true,
            )
            .await
            .map(|_| ())
            .map_err(sync_err)
        })
    }

    pub fn unmark_bot_channel(
        &self,
        community: CommunityId,
        channel: ChannelId,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            Self::call::<r::UnmarkBotChannel>(
                &inner,
                &[&community.hex(), &channel.hex()],
                Vec::new(),
                true,
            )
            .await
            .map(|_| ())
            .map_err(sync_err)
        })
    }

    // ---- the installation ----

    /// Reads the managed installation (`null` when there is none) into the slice.
    pub fn refresh_installation(
        &self,
        community: CommunityId,
    ) -> oneshot::Receiver<Result<Option<Installation>, ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            let body =
                Self::call::<r::GetBotInstallation>(&inner, &[&community.hex()], Vec::new(), true)
                    .await
                    .map_err(sync_err)?;
            let value = Value::decode(&body).map_err(|_| decode_err(v::DecodeError("not CBOR")))?;
            let row = match value {
                Value::Null => None,
                other => Some(decode_installation(&other, &community).map_err(decode_err)?),
            };
            let kept = row.clone();
            inner.publish(move |s| {
                match kept {
                    Some(i) => s.bot_admin.installation.insert(community, i),
                    None => s.bot_admin.installation.remove(&community),
                };
            });
            Ok(row)
        })
    }

    /// Asks the runtime to restart this installation; a request while one is pending joins it.
    pub fn restart(
        &self,
        community: CommunityId,
        installation: [u8; 16],
    ) -> oneshot::Receiver<Result<RestartCounts, ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            let body = Value::Array(vec![Value::Bytes(installation.to_vec())]).encode();
            let answer = Self::call::<r::RestartBot>(&inner, &[&community.hex()], body, false)
                .await
                .map_err(sync_err)?;
            let value =
                Value::decode(&answer).map_err(|_| decode_err(v::DecodeError("not CBOR")))?;
            let f = v::arr(&value, 2).map_err(decode_err)?;
            let (requested, handled) = (
                v::uint(&f[0]).map_err(decode_err)?,
                v::uint(&f[1]).map_err(decode_err)?,
            );
            if handled > requested {
                return Err(decode_err(v::DecodeError("restart counters invalid")));
            }
            Ok(RestartCounts { requested, handled })
        })
    }

    /// The Music page's default volume and idle-leave minutes. The slice shows the pick at once and is put back if the
    /// instance refuses.
    pub fn set_music_settings(
        &self,
        community: CommunityId,
        installation: [u8; 16],
        volume: u8,
        idle_minutes: u8,
    ) -> oneshot::Receiver<Result<Installation, ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            if !MUSIC_VOLUMES.contains(&volume) || !MUSIC_IDLE_MINUTES.contains(&idle_minutes) {
                return Err(ClientError::Sync(
                    "E_ROUTE_INPUT: volume must be 25, 50, 75 or 100 and idle minutes 5, 15 or 30"
                        .to_owned(),
                ));
            }
            let before = inner
                .snapshot()
                .bot_admin
                .installation
                .get(&community)
                .cloned();
            inner.publish(move |s| {
                if let Some(i) = s.bot_admin.installation.get_mut(&community) {
                    i.music_volume = volume;
                    i.music_idle_minutes = idle_minutes;
                }
            });
            let body = Value::Array(vec![
                Value::Bytes(installation.to_vec()),
                Value::Uint(u64::from(volume)),
                Value::Uint(u64::from(idle_minutes)),
            ])
            .encode();
            let answer =
                Self::call::<r::SetBotMusicSettings>(&inner, &[&community.hex()], body, false)
                    .await;
            let saved = answer.map_err(sync_err).and_then(|b| {
                let value =
                    Value::decode(&b).map_err(|_| decode_err(v::DecodeError("not CBOR")))?;
                decode_installation(&value, &community).map_err(decode_err)
            });
            match saved {
                Ok(next) => {
                    let kept = next.clone();
                    inner.publish(move |s| {
                        // The answer is the eight-field row on a mutation: keep what a manager's read carried.
                        let merged = match s.bot_admin.installation.get(&community) {
                            Some(prev) if kept.version.is_none() && kept.restart_requested == 0 => {
                                Installation {
                                    restart_requested: prev.restart_requested,
                                    restart_handled: prev.restart_handled,
                                    version: prev.version.clone(),
                                    music_volume: volume,
                                    music_idle_minutes: idle_minutes,
                                    ..kept
                                }
                            }
                            _ => kept,
                        };
                        s.bot_admin.installation.insert(community, merged);
                    });
                    Ok(next)
                }
                Err(e) => {
                    inner.publish(move |s| match before {
                        Some(i) => {
                            s.bot_admin.installation.insert(community, i);
                        }
                        None => {
                            s.bot_admin.installation.remove(&community);
                        }
                    });
                    Err(e)
                }
            }
        })
    }

    /// Enables the Bot (no installation yet, or a disabled one) or changes the capabilities of a live one. The server wants
    /// a fresh second factor: `step_up` is its token (Task 26's step-up dialog answers it). The token travels in a
    /// zeroizing buffer, is never kept and never named by an error. `request_id` is kept across a retry: the server binds it
    /// to the actor, the payload and the result.
    pub fn configure_installation(
        &self,
        community: CommunityId,
        request_id: [u8; 16],
        expected_revision: Option<u64>,
        capabilities: u64,
        step_up: Zeroizing<String>,
    ) -> oneshot::Receiver<Result<Installation, ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            if expected_revision == Some(0) || capabilities >= (1 << 10) || step_up.is_empty() {
                return Err(ClientError::Sync(
                    "E_ROUTE_INPUT: invalid revision, capabilities or step-up token".to_owned(),
                ));
            }
            // Built straight into a buffer that is zeroized on drop, sized so it never reallocates (a realloc would leave
            // the token behind in the old block).
            let mut e = dilla_core::cbor::Encoder::with_capacity(64 + step_up.len());
            e.array(4).bytes(&request_id);
            match expected_revision {
                Some(r) => e.uint(r),
                None => e.null(),
            };
            e.uint(capabilities).text(&step_up);
            let body = Zeroizing::new(e.into_vec());
            let answer = Self::call_with::<r::ConfigureBotInstallation>(
                &inner,
                &[&community.hex()],
                body,
                Some(request_id.to_vec()),
            )
            .await
            .map_err(sync_err)?;
            let value =
                Value::decode(&answer).map_err(|_| decode_err(v::DecodeError("not CBOR")))?;
            let row = decode_installation(&value, &community).map_err(decode_err)?;
            let kept = row.clone();
            inner.publish(move |s| {
                s.bot_admin.installation.insert(community, kept);
            });
            Ok(row)
        })
    }

    /// Disables the installation (at the revision it was read at).
    pub fn disable_installation(
        &self,
        community: CommunityId,
        request_id: [u8; 16],
        expected_revision: u64,
    ) -> oneshot::Receiver<Result<Installation, ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            if expected_revision == 0 {
                return Err(ClientError::Sync(
                    "E_ROUTE_INPUT: revision must be positive".to_owned(),
                ));
            }
            let body = Value::Array(vec![
                Value::Bytes(request_id.to_vec()),
                Value::Uint(expected_revision),
            ])
            .encode();
            let answer = Self::call_with::<r::DisableBotInstallation>(
                &inner,
                &[&community.hex()],
                Zeroizing::new(body),
                Some(request_id.to_vec()),
            )
            .await
            .map_err(sync_err)?;
            let value =
                Value::decode(&answer).map_err(|_| decode_err(v::DecodeError("not CBOR")))?;
            let row = decode_installation(&value, &community).map_err(decode_err)?;
            let kept = row.clone();
            inner.publish(move |s| {
                s.bot_admin.installation.insert(community, kept);
            });
            Ok(row)
        })
    }

    // ---- webhooks ----

    /// Reads the webhook list (no secret) into the slice.
    pub fn refresh_webhooks(
        &self,
        community: CommunityId,
    ) -> oneshot::Receiver<Result<Vec<BotWebhook>, ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move { Self::load_webhooks(&inner, community).await })
    }

    async fn load_webhooks(
        inner: &Arc<Inner>,
        community: CommunityId,
    ) -> Result<Vec<BotWebhook>, ClientError> {
        let body = Self::call::<r::ListBotWebhooks>(inner, &[&community.hex()], Vec::new(), true)
            .await
            .map_err(sync_err)?;
        let rows = decode_bot_webhooks(&body).map_err(decode_err)?;
        let kept = rows.clone();
        inner.publish(move |s| {
            s.bot_admin.webhooks.insert(community, kept);
        });
        Ok(rows)
    }

    /// Makes a webhook for one Bot channel. Its address is answered once, here.
    pub fn create_webhook(
        &self,
        community: CommunityId,
        channel: ChannelId,
        name: String,
        allow_everyone: bool,
    ) -> oneshot::Receiver<Result<WebhookAddress, ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            let name = name.trim().to_owned();
            if name.is_empty() {
                return Err(ClientError::Sync(
                    "E_ROUTE_INPUT: name is required".to_owned(),
                ));
            }
            let body = Value::Array(vec![
                Value::Bytes(channel.0.to_vec()),
                Value::Text(name),
                Value::Null,
                Value::Uint(u64::from(allow_everyone)),
            ])
            .encode();
            let answer =
                Self::call::<r::CreateBotWebhook>(&inner, &[&community.hex()], body, false)
                    .await
                    .map_err(sync_err)?;
            let made = webhook_address(&answer, 2, 1, true)?;
            let _ = Self::load_webhooks(&inner, community).await;
            Ok(made)
        })
    }

    /// A new secret for a webhook: the old address stops working. The new address is answered once, here.
    pub fn rotate_webhook(
        &self,
        community: CommunityId,
        webhook: [u8; 16],
    ) -> oneshot::Receiver<Result<WebhookAddress, ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            let answer = Self::call::<r::RotateBotWebhook>(
                &inner,
                &[&community.hex(), &hex(&webhook)],
                Vec::new(),
                false,
            )
            .await
            .map_err(sync_err)?;
            let mut made = webhook_address(&answer, 1, 0, false)?;
            made.webhook_id = webhook;
            let _ = Self::load_webhooks(&inner, community).await;
            Ok(made)
        })
    }

    /// Lets a webhook mention everyone, or not. The list shows it at once; a refusal reads the list again.
    pub fn set_webhook_everyone(
        &self,
        community: CommunityId,
        webhook: [u8; 16],
        allow_everyone: bool,
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            let name = inner
                .snapshot()
                .bot_admin
                .webhooks
                .get(&community)
                .and_then(|rows| rows.iter().find(|w| w.webhook_id == webhook))
                .map(|w| w.name.clone())
                .ok_or_else(|| ClientError::Sync("E_NOT_FOUND".to_owned()))?;
            inner.publish(move |s| {
                if let Some(w) = s
                    .bot_admin
                    .webhooks
                    .get_mut(&community)
                    .and_then(|rows| rows.iter_mut().find(|w| w.webhook_id == webhook))
                {
                    w.allow_everyone = allow_everyone;
                }
            });
            let body = Value::Array(vec![
                Value::Text(name),
                Value::Null,
                Value::Uint(u64::from(allow_everyone)),
            ])
            .encode();
            let sent = Self::call::<r::PatchBotWebhook>(
                &inner,
                &[&community.hex(), &hex(&webhook)],
                body,
                true,
            )
            .await;
            match sent {
                Ok(_) => Ok(()),
                Err(e) => {
                    let _ = Self::load_webhooks(&inner, community).await;
                    Err(sync_err(e))
                }
            }
        })
    }

    pub fn delete_webhook(
        &self,
        community: CommunityId,
        webhook: [u8; 16],
    ) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.spawn(async move {
            Self::call::<r::DeleteBotWebhook>(
                &inner,
                &[&community.hex(), &hex(&webhook)],
                Vec::new(),
                true,
            )
            .await
            .map_err(sync_err)?;
            let _ = Self::load_webhooks(&inner, community).await;
            Ok(())
        })
    }

    // ---- the music provider (operation 24) ----

    /// Observes the runtime's playback requirements (sealed 124). The answer must name this installation and not be dated
    /// past now + 5 s. A refusal or a malformed answer is `Unavailable`; no answer in time is `Partial`.
    pub fn refresh_provider_status(
        &self,
        community: CommunityId,
        installation: [u8; 16],
        now_s: u64,
    ) -> oneshot::Receiver<Result<ProviderStatusState, ClientError>> {
        let (this, inner) = (self.clone(), self.inner.clone());
        self.spawn(async move {
            let set = |state: ProviderStatusState| {
                let kept = state.clone();
                inner.publish(move |s| {
                    s.bot_admin.provider.insert(community, kept);
                });
                state
            };
            set(ProviderStatusState::Loading);
            let outcome = this
                .operations(community)
                .request(BotRequest {
                    operation: PROVIDER_STATUS,
                    ..BotRequest::default()
                })
                .await;
            let state = match outcome {
                Ok(r) if r.status == 0 => match decode_music_provider_status(&r.data) {
                    Ok(st) if st.installation_id == installation && st.observed_at <= now_s + 5 => {
                        ProviderStatusState::Known(st)
                    }
                    _ => ProviderStatusState::Unavailable,
                },
                Ok(_) => ProviderStatusState::Unavailable,
                Err(OpError::Uncertain { .. }) => ProviderStatusState::Partial,
                Err(_) => ProviderStatusState::Unavailable,
            };
            Ok(set(state))
        })
    }

    // ---- the moderation log (operations 60 and 64) ----

    /// Reads one page of the moderation log: automatic cases (`MODERATION_CASES`) or manual evidence
    /// (`MODERATION_EVIDENCE`). A refusal comes back as its v1 status with no rows.
    pub fn moderation_page(
        &self,
        community: CommunityId,
        operation: u64,
        cursor: Option<String>,
    ) -> oneshot::Receiver<Result<ModerationPage, ClientError>> {
        let this = self.clone();
        self.spawn(async move {
            if operation != MODERATION_CASES && operation != MODERATION_EVIDENCE {
                return Err(ClientError::Sync(
                    "E_ROUTE_INPUT: moderation operation".to_owned(),
                ));
            }
            let after = match cursor {
                None => BotArg::Null,
                Some(c) => BotArg::Bytes(parse_cursor(&c).ok_or_else(|| {
                    ClientError::Sync("E_ROUTE_INPUT: moderation cursor is invalid".to_owned())
                })?),
            };
            let result = this
                .operations(community)
                .request(BotRequest {
                    operation,
                    args: vec![after],
                    ..BotRequest::default()
                })
                .await
                .map_err(op_err)?;
            if result.status != 0 {
                return Ok(ModerationPage {
                    status: result.status,
                    cases: Vec::new(),
                    next_cursor: None,
                });
            }
            let (cases, next_cursor) =
                decode_moderation_page(operation, &result.data, &community).map_err(decode_err)?;
            Ok(ModerationPage {
                status: 0,
                cases,
                next_cursor,
            })
        })
    }
}

/// A cursor is 1 to 256 bytes as lower-case hex pairs.
fn parse_cursor(hex_text: &str) -> Option<Vec<u8>> {
    let n = hex_text.len();
    if n == 0
        || !n.is_multiple_of(2)
        || n > 512
        || !hex_text
            .bytes()
            .all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
    {
        return None;
    }
    (0..n)
        .step_by(2)
        .map(|i| u8::from_str_radix(&hex_text[i..i + 2], 16).ok())
        .collect()
}

/// The address of a create (`[webhook_id, url]`) or a rotate (`[url]`) answer. What is wiped: the decoded text is
/// *moved* (never copied) into the returned `Zeroizing` string, and an error never names it. What is not: the decoder
/// reads the answer's bytes, which the caller holds in its own `Zeroizing` buffer (wiped when it drops), and the socket
/// transport keeps the frame it received in a plain buffer until it drops it.
fn webhook_address(
    body: &[u8],
    len: usize,
    url_at: usize,
    has_id: bool,
) -> Result<WebhookAddress, ClientError> {
    // Whatever text the decoded tree holds is wiped when it drops, on every path out of here, a value that is not the
    // expected array included.
    let top = Value::decode(body).map_err(|_| decode_err(v::DecodeError("not CBOR")))?;
    let inner = array_or_wipe(top).map_err(|_| decode_err(v::DecodeError("not an array")))?;
    let mut items = WipeValues(inner);
    if items.0.len() != len {
        return Err(decode_err(v::DecodeError("an array of the wrong length")));
    }
    let webhook_id = if has_id {
        bin16(&items.0[0]).map_err(decode_err)?
    } else {
        [0; 16]
    };
    let moved = std::mem::replace(&mut items.0[url_at], Value::Null);
    let Value::Text(url) = moved else {
        // Not the text we want: wipe what was moved out before it drops.
        let mut other = [moved];
        wipe_values(&mut other);
        return Err(decode_err(v::DecodeError("not a text string")));
    };
    Ok(WebhookAddress {
        webhook_id,
        url: Zeroizing::new(url),
    })
}

/// The items of a decoded top-level array. Any other value is wiped before it is handed back (and dropped), so a body that
/// is not the array it should be never frees its text unwiped.
fn array_or_wipe(value: Value) -> Result<Vec<Value>, Value> {
    match value {
        Value::Array(items) => Ok(items),
        mut other => {
            wipe_values(std::slice::from_mut(&mut other));
            Err(other)
        }
    }
}

/// A decoded array whose strings and byte strings are zeroized when it drops.
struct WipeValues(Vec<Value>);

impl Drop for WipeValues {
    fn drop(&mut self) {
        wipe_values(&mut self.0);
    }
}

/// Zeroizes (and empties) every string and byte string among `items`, one level deep and through nested arrays.
fn wipe_values(items: &mut [Value]) {
    use zeroize::Zeroize;
    for item in items {
        match item {
            Value::Text(t) => t.zeroize(),
            Value::Bytes(b) => b.zeroize(),
            Value::Array(inner) => wipe_values(inner),
            _ => {}
        }
    }
}

fn op_err(e: OpError) -> ClientError {
    match e {
        OpError::Refused { code, .. } => ClientError::Sync(clean_code(&code)),
        OpError::Uncertain { .. } => ClientError::Sync("E_BOT_UNCERTAIN".to_owned()),
        OpError::Invalid => ClientError::Sync("E_ROUTE_INPUT".to_owned()),
    }
}

/// Whether a provider snapshot observed at `observed_at` is stale at `now_s`.
pub fn provider_is_stale(observed_at: u64, now_s: u64) -> bool {
    now_s.saturating_sub(observed_at) > PROVIDER_STALE_S
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::bot_admin::{APP_OPERATION_BASE, MODERATION_CASES, encode_bot_status};
    use std::sync::Mutex as StdMutex;

    /// A transport that answers from a closure and records every command it was sent.
    type Answerer = Box<dyn Fn(&SealedCommand) -> Result<SealedAnswer, SendError> + Send + Sync>;

    struct Fake {
        sent: StdMutex<Vec<SealedCommand>>,
        answer: Answerer,
    }

    #[async_trait]
    impl SealedBot for Fake {
        async fn send(&self, _: CommunityId, c: SealedCommand) -> Result<SealedAnswer, SendError> {
            self.sent.lock().unwrap().push(c.clone());
            (self.answer)(&c)
        }
    }

    fn fake(
        answer: impl Fn(&SealedCommand) -> Result<SealedAnswer, SendError> + Send + Sync + 'static,
    ) -> Arc<Fake> {
        Arc::new(Fake {
            sent: StdMutex::default(),
            answer: Box::new(answer),
        })
    }

    fn ok(revision: Option<u64>, data: Vec<u8>) -> Result<SealedAnswer, SendError> {
        let rev = revision.map_or(Value::Null, Value::Uint);
        Ok(SealedAnswer {
            status: 200,
            body: Value::Array(vec![rev, Value::Bytes(data)]).encode(),
        })
    }

    const COMMUNITY: CommunityId = CommunityId([0xab; 16]);
    const RECEIPT: &str = "61616161616161616161616161616161";

    fn run<T>(f: impl std::future::Future<Output = T>) -> T {
        tokio::runtime::Builder::new_current_thread()
            .enable_all()
            .build()
            .unwrap()
            .block_on(f)
    }

    #[test]
    fn welcome_settings_are_app_operations_and_the_save_sends_its_revision_first() {
        let t = fake(|_| ok(Some(4), Value::Array(vec![]).encode()));
        let ops = BotOperations::new(COMMUNITY, t.clone());
        run(async {
            ops.request(BotRequest {
                operation: 1,
                args: vec![BotArg::Uint(5)],
                ..BotRequest::default()
            })
            .await
            .unwrap();
            let doc = Value::Array(vec![Value::Uint(1)]).encode();
            ops.request(BotRequest {
                operation: 2,
                args: vec![BotArg::Uint(5), BotArg::Bytes(doc.clone())],
                revision: Some(4),
                ..BotRequest::default()
            })
            .await
            .unwrap();
            let sent = t.sent.lock().unwrap();
            assert_eq!(
                sent.iter().map(|c| c.operation).collect::<Vec<_>>(),
                [101, 102]
            );
            assert_eq!(sent[0].args, [BotArg::Uint(5)]);
            assert_eq!(
                sent[1].args,
                [BotArg::Uint(4), BotArg::Uint(5), BotArg::Bytes(doc)]
            );
        });
    }

    #[test]
    fn moderation_evidence_is_161_with_revision_zero_first_and_the_receipt_as_16_bytes() {
        let t = fake(|_| ok(None, Vec::new()));
        let ops = BotOperations::new(COMMUNITY, t.clone());
        let result = run(ops.request(BotRequest {
            operation: 61,
            args: vec![
                BotArg::Text(RECEIPT.to_owned()),
                BotArg::Text("spam".to_owned()),
                BotArg::Text("buy now".to_owned()),
            ],
            revision: Some(0),
            ..BotRequest::default()
        }))
        .unwrap();
        let sent = t.sent.lock().unwrap();
        assert_eq!(sent[0].operation, 161);
        assert_eq!(
            sent[0].args,
            [
                BotArg::Uint(0),
                BotArg::Bytes(vec![0x61; 16]),
                BotArg::Text("spam".to_owned()),
                BotArg::Text("buy now".to_owned())
            ]
        );
        assert_eq!(result.status, 0);
    }

    #[test]
    fn the_status_read_is_sent_as_it_is_with_no_arguments() {
        let status = BotPageStatus {
            needs_re_add: vec![ChannelId([0x70; 16])],
            music_starts: vec![crate::state::bot_admin::MusicStart {
                first_frame_ms: 640,
                cold: true,
            }],
        };
        let data = encode_bot_status(&status);
        let t = fake(move |_| ok(None, data.clone()));
        let ops = BotOperations::new(COMMUNITY, t.clone());
        let result = run(ops.request(BotRequest {
            operation: BOT_STATUS_READ,
            ..BotRequest::default()
        }))
        .unwrap();
        let sent = t.sent.lock().unwrap();
        assert_eq!((sent[0].operation, sent[0].args.len()), (94, 0));
        assert_eq!(decode_bot_status(&result.data).unwrap(), status);
    }

    #[test]
    fn a_bare_refusal_code_is_the_v1_status_and_an_unknown_one_is_refused() {
        let t = fake(|c| match c.operation {
            160 => Ok(SealedAnswer {
                status: 403,
                body: b"E_FORBIDDEN".to_vec(),
            }),
            _ => Ok(SealedAnswer {
                status: 500,
                body: b"E_WHATEVER".to_vec(),
            }),
        });
        let ops = BotOperations::new(COMMUNITY, t);
        let denied = run(ops.request(BotRequest {
            operation: MODERATION_CASES,
            args: vec![BotArg::Null],
            ..BotRequest::default()
        }))
        .unwrap();
        assert_eq!((denied.status, denied.data.len()), (2, 0));
        let unknown = run(ops.request(BotRequest {
            operation: 9,
            ..BotRequest::default()
        }))
        .unwrap_err();
        assert!(matches!(unknown, OpError::Refused { ref code, .. } if code == "E_WHATEVER"));
    }

    #[test]
    fn a_timeout_is_uncertain_and_a_resend_of_the_request_id_settles_it() {
        let calls = Arc::new(StdMutex::new(0));
        let seen = calls.clone();
        let t = fake(move |_| {
            let mut n = seen.lock().unwrap();
            *n += 1;
            if *n == 1 {
                Err(SendError::Timeout)
            } else {
                ok(Some(8), Vec::new())
            }
        });
        let ops = BotOperations::new(COMMUNITY, t.clone());
        let id = [9u8; 16];
        let first = run(ops.request(BotRequest {
            operation: 16,
            revision: Some(7),
            request_id: Some(id),
            ..BotRequest::default()
        }));
        assert_eq!(first, Err(OpError::Uncertain { request_id: id }));
        let settled = run(ops.read(id, 16)).unwrap().unwrap();
        assert_eq!((settled.status, settled.revision), (0, Some(8)));
        // The resend is the same command, the same request id: the Bot runs it once.
        let sent = t.sent.lock().unwrap();
        assert_eq!(sent[0], sent[1]);
        // A request this page never sent is not read.
        assert_eq!(run(ops.read([1; 16], 16)).unwrap(), None);
    }

    #[test]
    fn a_webhook_address_is_never_in_debug_output() {
        let body = Value::Array(vec![
            Value::Bytes(vec![7; 16]),
            Value::Text("https://dilla.test/hooks/x/SECRET".to_owned()),
        ])
        .encode();
        let made = webhook_address(&body, 2, 1, true).unwrap();
        assert_eq!(&*made.url, "https://dilla.test/hooks/x/SECRET");
        let shown = format!("{made:?}");
        assert!(
            !shown.contains("SECRET") && shown.contains("<secret>"),
            "{shown}"
        );
    }

    #[test]
    fn a_refusal_code_is_capped_and_an_http_status_stays_in_the_error() {
        assert_eq!(clean_code("E_FORBIDDEN"), "E_FORBIDDEN");
        assert_eq!(clean_code(&"E".repeat(32)), "E".repeat(32));
        for bad in ["", "e_lower", "E X", "E_\u{202e}EVIL", &"E".repeat(33)] {
            assert_eq!(clean_code(bad), "E_UNKNOWN", "{bad:?}");
        }
        // A Bot answer's bare code is cleaned before it can be shown.
        let t = fake(|_| {
            Ok(SealedAnswer {
                status: 500,
                body: b"E_OK <b>hi</b>".to_vec(),
            })
        });
        let ops = BotOperations::new(COMMUNITY, t);
        let e = run(ops.request(BotRequest {
            operation: 9,
            ..BotRequest::default()
        }))
        .unwrap_err();
        assert!(
            matches!(e, OpError::Refused { ref code, .. } if code == "E_UNKNOWN"),
            "{e:?}"
        );
        // An instance refusal keeps its status: a 403 reads as forbidden, a 409 as a conflict.
        let forbidden = sync_err(ApiError::local(403, "E_WHATEVER", ""));
        assert!(
            matches!(&forbidden, ClientError::Sync(s) if split_refusal(s) == ("E_WHATEVER", Some(403)))
        );
        let odd = sync_err(ApiError::local(409, "not a code!", ""));
        assert!(
            matches!(&odd, ClientError::Sync(s) if split_refusal(s) == ("E_UNKNOWN", Some(409)))
        );
        assert_eq!(split_refusal("E_X"), ("E_X", None));
        // A locally made refusal keeps its code and drops the detail after the colon.
        assert_eq!(
            clean_code("E_ROUTE_INPUT: invalid revision"),
            "E_ROUTE_INPUT"
        );
        assert_eq!(clean_code("E_DECODE: not CBOR"), "E_DECODE");
        assert_eq!(clean_code("not a code: detail"), "E_UNKNOWN");
        assert_eq!(clean_code(": detail"), "E_UNKNOWN");
    }

    #[test]
    fn a_webhook_answer_that_is_refused_names_no_text_and_a_good_one_moves_it_out() {
        // Wrong length, wrong type, not CBOR: each is refused without the text in the error.
        let secret = "https://dilla.test/hooks/a/SECRETSECRET";
        let wrong_len =
            Value::Array(vec![Value::Text(secret.into()), Value::Text("x".into())]).encode();
        let not_text = Value::Array(vec![Value::Bytes(vec![7; 16]), Value::Uint(1)]).encode();
        let not_array = Value::Text(secret.into()).encode();
        for body in [wrong_len, not_text, not_array, vec![0xff]] {
            let e = webhook_address(&body, 1, 0, false).expect_err("refused");
            assert!(!format!("{e:?}").contains("SECRETSECRET"), "{e:?}");
        }
        let ok = Value::Array(vec![Value::Text(secret.into())]).encode();
        assert_eq!(&*webhook_address(&ok, 1, 0, false).unwrap().url, secret);
        // The wiping guard really zeroes what it holds.
        let mut tree = vec![
            Value::Text("abc".into()),
            Value::Bytes(vec![9; 3]),
            Value::Array(vec![Value::Text("def".into())]),
        ];
        wipe_values(&mut tree);
        assert_eq!(
            tree,
            vec![
                Value::Text(String::new()),
                Value::Bytes(Vec::new()),
                Value::Array(vec![Value::Text(String::new())])
            ]
        );
    }

    #[test]
    fn a_top_level_value_that_is_not_an_array_is_wiped_before_it_is_dropped() {
        // The value handed back by the refusal is the one that is dropped: it must already be empty.
        assert_eq!(
            array_or_wipe(Value::Text("SECRET".into())).unwrap_err(),
            Value::Text(String::new())
        );
        assert_eq!(
            array_or_wipe(Value::Bytes(vec![7; 8])).unwrap_err(),
            Value::Bytes(Vec::new())
        );
        assert_eq!(
            array_or_wipe(Value::Array(vec![Value::Uint(1)])).unwrap(),
            vec![Value::Uint(1)]
        );
    }

    #[test]
    fn a_cursor_is_hex_pairs_of_one_to_256_bytes() {
        assert_eq!(parse_cursor("0102"), Some(vec![1, 2]));
        assert_eq!(parse_cursor(""), None);
        assert_eq!(parse_cursor("012"), None);
        assert_eq!(parse_cursor("0G"), None);
        assert_eq!(parse_cursor(&"ab".repeat(257)), None);
    }

    #[test]
    fn the_provider_is_stale_after_a_minute() {
        assert!(!provider_is_stale(100, 160));
        assert!(provider_is_stale(100, 161));
        assert_eq!(APP_OPERATION_BASE, 100);
    }
}
