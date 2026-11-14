//! Area **bot** (Task 20): Bot commands over `dilla_core` (sealed), private results, polls, reminders.
//!
//! Ports: packages/client-core/src/bot/{catalog,commands,hints,operations,protocol}.ts, packages/client-core/src/commands/{classic,registry}.ts.
//!
//! Receives op 61 `bot_catalog`, op 62 `bot_reply` and op 63 `bot_state_changed` from the gateway (`crate::sync::route`), through [`AreaFrames`].
//!
//! What runs where:
//! - [`Bot::run`] parses a composer draft against the built-ins and the community Bot's catalog ([`registry`]). A literal
//!   draft, or a built-in that posts (`/me`, `/shrug`, `/code`, ...), comes back as [`RunOutcome::Post`] for the caller to
//!   send like any message. Everything else answers with a private result.
//! - Private results ([`private`]) live only in [`BotState::private_results`]: no store, no transport, no timeline. A
//!   finished row goes away [`crate::state::bot::PRIVATE_RESULT_TTL_MS`] after it finished; the area sweeps itself on a timer.

pub mod catalog;
pub mod hints;
mod io;
pub mod private;
pub mod registry;
pub mod room;
pub mod run;
pub mod sealed;

use std::sync::atomic::AtomicU64;
use std::sync::{Arc, Mutex, Weak};

use crate::client::{Client, Inner};
use crate::gateway::frames::{self, Frame, Op};
use crate::state::bot::{Conversation, ResultProvider, ResultStatus};
use crate::state::{AppState, CommunityId};
use crate::sync::route::AreaFrames;

use registry::{CommandDefinition, Parsed, Provider};
use sealed::{Refusal, Sealed, Transport};

/// The bot area's handle for bot: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Bot {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Bot`] handle.
    pub fn bot(&self) -> Bot {
        Bot {
            inner: self.inner().clone(),
        }
    }
}

/// A webhook avatar id as the instance names one: 64 lower-case hex digits (routes.ts `getWebhookAvatar`).
pub fn is_webhook_avatar_id(id: &str) -> bool {
    id.len() == 64 && id.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

impl Bot {
    /// routes.ts `getWebhookAvatar` (Task 28 item 15): `GET …/bot/webhook-avatars/{avatar}`, the instance's re-encoded
    /// PNG of a webhook post's avatar (at most 1 MiB), the only place one is ever loaded from. `None` on 404 (the
    /// instance keeps avatars in memory: gone after its restart). An id that is not one is refused before any request.
    pub fn webhook_avatar(
        &self,
        community: CommunityId,
        avatar: String,
    ) -> tokio::sync::oneshot::Receiver<Result<Option<Vec<u8>>, crate::ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            if !is_webhook_avatar_id(&avatar) {
                return Err(crate::ClientError::Sync("E_CBOR_SHAPE".into()));
            }
            let reply = inner
                .api()
                .http_call::<crate::http::routes::GetWebhookAvatar>(
                    &[&community.hex(), &avatar],
                    crate::http::routes::NoBody,
                    true,
                )
                .await
                .map_err(|e| {
                    if e.status == 0 && e.code == "E_NETWORK" {
                        crate::ClientError::Network(e.detail)
                    } else {
                        crate::ClientError::Sync(e.code)
                    }
                })?;
            Ok((reply.status == 200 && !reply.body.is_empty()).then_some(reply.body))
        })
    }
}

/// What running a draft did.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum RunOutcome {
    /// Post this text in the conversation (a literal draft, or a built-in that posts); the draft is cleared.
    Post(String),
    /// A private result row was added (its id); the draft is cleared.
    Private(u64),
    /// The command is not complete: the draft stays and the picker shows why.
    Incomplete,
    /// A Bot command: the caller runs it with [`Bot::run_command`] (it supplies the copy of the private result).
    Bot {
        definition: CommandDefinition,
        args: registry::Args,
    },
    /// A moderation command (`/kick`, `/ban`): the caller asks for confirmation, then runs it with [`Bot::moderate`].
    Moderate(Moderation),
}

/// `privateResultCopy.author`: who a private result names as its author.
pub const BUILTIN_AUTHOR: &str = "dilla";

/// commandPickerCopy `notACommand`.
pub fn not_a_command(name: &str) -> String {
    format!("/{name} isn’t a command here. To post it as text, start with //.")
}

/// privateResultCopy `unavailable`.
pub const UNAVAILABLE: &str = "This command is temporarily unavailable.";

static NEXT_RESULT: AtomicU64 = AtomicU64::new(1);

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| u64::try_from(d.as_millis()).unwrap_or(u64::MAX))
}

/// The commands a draft is parsed against in `community`: the built-ins, then its Bot's catalog commands.
pub fn definitions(state: &AppState, community: Option<CommunityId>) -> Vec<CommandDefinition> {
    let mut all = registry::commands().to_vec();
    if let Some(view) = community.and_then(|c| state.bot.catalogs.get(&c)) {
        all.extend(registry::bot_command_definitions(&view.commands));
    }
    all
}

/// What `draft` does in `community` without running it (the picker's and the composer's reading of a draft). Pure:
/// [`Bot::run`] applies it.
pub fn plan(state: &AppState, community: Option<CommunityId>, draft: &str) -> Plan {
    let defs = definitions(state, community);
    let refs: Vec<&CommandDefinition> = defs.iter().collect();
    match registry::parse_command(draft, &refs) {
        Parsed::Literal(text) => Plan::Post(text),
        Parsed::Unknown { .. } => {
            // The prototype's send(): the first word names the command that is not there.
            let name = draft[1..]
                .split_whitespace()
                .next()
                .unwrap_or("")
                .to_lowercase();
            if name.is_empty() {
                Plan::Incomplete
            } else {
                Plan::Private(
                    ResultStatus::Failed,
                    not_a_command(&name),
                    ResultProvider::Builtin,
                )
            }
        }
        Parsed::Incomplete { .. } | Parsed::Invalid { .. } => Plan::Incomplete,
        Parsed::Invocation { command_id, args } => {
            let Some(def) = defs.iter().find(|d| d.id == command_id) else {
                return Plan::Incomplete;
            };
            if def.provider == Provider::Bot {
                // A catalog command: sealed to the community's Bot, answered privately ([`Bot::run_command`]).
                return Plan::Bot {
                    definition: def.clone(),
                    args,
                };
            }
            let arg = |n: &str| args.get(n).map(String::as_str);
            match command_id.as_str() {
                "help" => match arg("command") {
                    Some(c) => match registry::help_text(c) {
                        Some(t) => Plan::Private(ResultStatus::Success, t, ResultProvider::Builtin),
                        None => Plan::Private(
                            ResultStatus::Failed,
                            format!("Unknown command: {c}."),
                            ResultProvider::Builtin,
                        ),
                    },
                    None => Plan::Private(
                        ResultStatus::Success,
                        registry::all_help(),
                        ResultProvider::Builtin,
                    ),
                },
                "me" => {
                    // selfDisplayName: the member's display name here, else the account's name.
                    let me = state.account.user_id;
                    let name = community
                        .and_then(|c| state.members.get(&c))
                        .and_then(|m| m.iter().find(|m| Some(m.user_id) == me))
                        .map(|m| m.display.clone())
                        .filter(|d| !d.is_empty())
                        .unwrap_or_else(|| state.account.username.clone());
                    let text = format!("_{name} {}_", arg("text").unwrap_or_default());
                    Plan::Post(
                        text.strip_prefix("_ ")
                            .map_or(text.clone(), |t| format!("_{t}")),
                    )
                }
                "shrug" | "tableflip" | "unflip" => Plan::Post(
                    registry::classic_message(&command_id, arg("text")).unwrap_or_default(),
                ),
                "kick" | "ban" => moderation_plan(state, community, &command_id, &args),
                "code" => {
                    let language = arg("language").unwrap_or_default().trim().to_owned();
                    let text = arg("text").unwrap_or_default();
                    let valid = (1..=32).contains(&language.len())
                        && language
                            .chars()
                            .all(|c| c.is_alphanumeric() || c == '_' || c == '+' || c == '-');
                    if !valid {
                        Plan::Private(
                            ResultStatus::Failed,
                            "Language must be 1 to 32 letters, digits, underscores, plus signs, or hyphens.".to_owned(),
                            ResultProvider::Builtin,
                        )
                    } else if text.contains("```") {
                        Plan::Private(
                            ResultStatus::Failed,
                            "Code text cannot contain a closing code fence.".to_owned(),
                            ResultProvider::Builtin,
                        )
                    } else {
                        Plan::Post(format!("```{language}\n{text}\n```"))
                    }
                }
                // The other built-ins act on another area (voice, members, search, ...): those areas run them.
                _ => Plan::Private(
                    ResultStatus::Failed,
                    UNAVAILABLE.to_owned(),
                    ResultProvider::Builtin,
                ),
            }
        }
    }
}

/// The copy of a moderation command's private row (copy.ts `privateResultCopy`, `mod.*.done`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ModerationTexts {
    pub cancelled: String,
    pub pending: String,
    pub done: String,
}

/// builtin.ts `mutationError`: a definitive refusal names its code; anything else is an uncertain outcome.
pub fn mutation_error(e: &crate::sync::api::ApiError) -> String {
    const DEFINITIVE: [&str; 8] = [
        "E_BAD_INPUT",
        "E_FORBIDDEN",
        "E_NOT_FOUND",
        "E_UNAUTHORIZED",
        "E_CONFLICT",
        "E_RATE_LIMITED",
        "E_BOT_PERMISSION",
        "E_BOT_BAD_INPUT",
    ];
    let code = (!e.code.is_empty() && e.code != "E_HTTP").then_some(e.code.as_str());
    if code.is_some_and(|c| DEFINITIVE.contains(&c)) || (400..500).contains(&e.status) {
        return code.map_or("Command was refused.".to_owned(), |c| {
            format!("Command was refused ({c}).")
        });
    }
    code.map_or(
        "The command result is uncertain. Check the current server state before retrying.".to_owned(),
        |c| {
            format!(
                "The command result is uncertain ({c}). Check the current server state before retrying."
            )
        },
    )
}

/// What a draft does ([`plan`]).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Plan {
    Post(String),
    Private(ResultStatus, String, ResultProvider),
    /// A Bot command to seal and send.
    Bot {
        definition: CommandDefinition,
        args: registry::Args,
    },
    /// A moderation command, to confirm first (CommandHost.tsx: the moderation dialog), then [`Bot::moderate`].
    Moderate(Moderation),
    Incomplete,
}

/// A checked moderation command (builtin.ts `kick`, `ban`): its member, the name the dialog shows, and what it does.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Moderation {
    pub community: CommunityId,
    pub user: crate::state::UserId,
    pub name: String,
    pub action: ModAction,
}

/// What a [`Moderation`] does.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum ModAction {
    Kick,
    /// `seconds` is `None` for a permanent ban; `duration` is the length as typed (`1d`, `permanent`).
    Ban {
        seconds: Option<u64>,
        duration: String,
        reason: String,
    },
}

impl ModAction {
    /// `kick` or `ban`: the strings' keys.
    pub fn id(&self) -> &'static str {
        match self {
            Self::Kick => "kick",
            Self::Ban { .. } => "ban",
        }
    }
}

/// builtin.ts `eligibleModerationTarget`: the member `who` names (a user id, else a username or display name) when this
/// account holds the action's permission and outranks them; their id and the name to show.
pub fn moderation_target(
    state: &AppState,
    community: CommunityId,
    who: &str,
    kick: bool,
) -> Option<(crate::state::UserId, String)> {
    use crate::features::roles::rank;
    let access = state.roles.access_of(community);
    let owner = state.communities.iter().find(|c| c.id == community)?.owner;
    let roles = state.roles.roles_of(community);
    let members = state.members.get(&community)?;
    let me = state.account.user_id?;
    let who = who.trim();
    let lower = who.trim_start_matches('@').to_lowercase();
    let target = members
        .iter()
        .find(|m| m.user_id.hex() == who)
        .or_else(|| members.iter().find(|m| m.username.to_lowercase() == lower))
        .or_else(|| {
            members
                .iter()
                .find(|m| !m.display.is_empty() && m.display.to_lowercase() == lower)
        })?;
    let mine = members.iter().find(|m| m.user_id == me)?;
    let permitted = if kick {
        access.kick_members
    } else {
        access.ban_members
    };
    if !access.loaded || !permitted {
        return None;
    }
    rank::can_moderate(
        me.0,
        target.user_id.0,
        owner.0,
        rank::top_of(&mine.role_ids, roles),
        rank::top_of(&target.role_ids, roles),
    )
    .then(|| {
        let name = if target.display.is_empty() {
            target.username.clone()
        } else {
            target.display.clone()
        };
        (target.user_id, name)
    })
}

/// builtin.ts `reasonProblem`: more than 512 bytes, or a control character.
fn reason_problem(reason: &str) -> bool {
    reason.len() > 512
        || reason
            .chars()
            .any(|c| matches!(c as u32, 0..=8 | 0x0b..=0x1f | 0x7f..=0x9f))
}

fn moderation_plan(
    state: &AppState,
    community: Option<CommunityId>,
    command_id: &str,
    args: &registry::Args,
) -> Plan {
    let failed = |text: &str| {
        Plan::Private(
            ResultStatus::Failed,
            text.to_owned(),
            ResultProvider::Builtin,
        )
    };
    let Some(community) = community else {
        return failed("This command needs a community channel.");
    };
    let kick = command_id == "kick";
    let who = args.get("user").map_or("", String::as_str);
    let Some((user, name)) = moderation_target(state, community, who, kick) else {
        return failed(if kick {
            "This member cannot be kicked with your current permissions and rank."
        } else {
            "This member cannot be banned with your current permissions and rank."
        });
    };
    let action = if kick {
        ModAction::Kick
    } else {
        let duration = args
            .get("duration")
            .cloned()
            .unwrap_or_else(|| "1d".to_owned());
        let seconds = if duration == "permanent" {
            None
        } else {
            match run::duration_seconds(&duration) {
                Some(s) if (3600..=3_153_600_000).contains(&s) => Some(s),
                _ => return failed("Ban duration must be from 1h to 100y."),
            }
        };
        let reason = args
            .get("reason")
            .map_or(String::new(), |r| r.trim().to_owned());
        if reason_problem(&reason) {
            return failed(
                "Reason must be at most 512 UTF-8 bytes and contain no control characters.",
            );
        }
        ModAction::Ban {
            seconds,
            duration,
            reason,
        }
    };
    Plan::Moderate(Moderation {
        community,
        user,
        name,
        action,
    })
}

impl Bot {
    /// Runs `draft` in `conversation` (of `community`, `None` in a DM). A private answer is added to this device's
    /// in-memory results only: nothing about it is stored or sent.
    pub fn run(
        &self,
        conversation: Conversation,
        community: Option<CommunityId>,
        draft: &str,
    ) -> RunOutcome {
        match plan(&self.inner.snapshot(), community, draft) {
            Plan::Post(text) => RunOutcome::Post(text),
            Plan::Incomplete => RunOutcome::Incomplete,
            Plan::Private(status, text, provider) => {
                RunOutcome::Private(self.add_result(conversation, status, text, provider))
            }
            Plan::Bot { definition, args } => RunOutcome::Bot { definition, args },
            Plan::Moderate(m) => RunOutcome::Moderate(m),
        }
    }

    /// Runs a moderation command the person answered (CommandHost.tsx): cancelled, a "cancelled" row and nothing sent;
    /// confirmed, a pending row, then the kick or ban, and the row says how it ended (`done` is its success copy). The
    /// target is checked again first: permissions or rank may have changed while the dialog was open. Answers the row.
    pub fn moderate(
        &self,
        conversation: Conversation,
        m: Moderation,
        confirmed: bool,
        texts: ModerationTexts,
    ) -> u64 {
        if !confirmed {
            return self.add_result(
                conversation,
                ResultStatus::Cancelled,
                texts.cancelled,
                ResultProvider::Builtin,
            );
        }
        let row = self.add_result(
            conversation,
            ResultStatus::Pending,
            texts.pending,
            ResultProvider::Builtin,
        );
        let inner = self.inner.clone();
        let runs = self.runs();
        drop(self.inner.spawn(async move {
            let still = moderation_target(
                &inner.snapshot(),
                m.community,
                &m.user.hex(),
                m.action == ModAction::Kick,
            )
            .is_some_and(|(u, _)| u == m.user);
            let outcome = if still {
                let moderation = crate::features::moderation::Moderation::of(&inner);
                let done = match &m.action {
                    ModAction::Kick => moderation.kick(m.community, m.user).await,
                    ModAction::Ban {
                        seconds, reason, ..
                    } => moderation.ban(m.community, m.user, reason, *seconds).await,
                };
                done.map_err(|e| mutation_error(&e))
            } else {
                Err("Your permissions or the member’s rank changed.".to_owned())
            };
            let (status, text) = match outcome {
                Ok(()) => (ResultStatus::Success, texts.done),
                Err(text) => (ResultStatus::Failed, text),
            };
            runs.patch(
                conversation,
                row,
                private::Patch {
                    status: Some(status),
                    text: Some(text),
                    hold: None,
                },
            );
            Ok(())
        }));
        row
    }

    /// The private rows of this client and the runs behind them.
    pub(crate) fn runs(&self) -> run::Runs {
        run::Runs::new(
            self.sealed(),
            Arc::new(run::InnerRows {
                inner: Arc::downgrade(&self.inner),
            }),
        )
    }

    /// Adds a private result row; a finished one goes away by itself after the TTL.
    pub fn add_result(
        &self,
        conversation: Conversation,
        status: ResultStatus,
        text: String,
        provider: ResultProvider,
    ) -> u64 {
        self.runs().add_result(conversation, status, text, provider)
    }

    /// The Dismiss button: the row goes, and a held run behind it is forgotten (its reply secret too).
    pub fn dismiss(&self, conversation: Conversation, id: u64) {
        self.runs().dismiss(conversation, id);
    }

    /// Removes every private result whose time is up at `now_ms` (unix ms). The area calls it on its own timer.
    pub fn sweep_at(&self, now_ms: u64) {
        self.inner.publish(move |s| {
            private::sweep(&mut s.bot, now_ms);
        });
    }

    /// The handle of the client `inner` (the utilities reach the transport through it).
    pub(crate) fn of(inner: &Arc<Inner>) -> Self {
        Self {
            inner: inner.clone(),
        }
    }

    /// The sealed path of this client (the key store and the reply secrets).
    pub fn sealed(&self) -> Arc<Sealed> {
        sealed_for(&self.inner)
    }

    /// Reads `community`'s catalog and Bot channels from the instance, then checks its key against the pinned Bot user.
    pub async fn sync_catalog(&self, community: CommunityId) -> Result<(), Refusal> {
        self.sealed().refresh(community).await
    }

    /// The transport Task 21's Bot operations and this area's polls and reminders use.
    pub fn transport(&self) -> Arc<dyn crate::features::bot_admin::SealedBot> {
        Arc::new(Transport(self.sealed()))
    }
}

/// The sealed path of each running client, by its `Inner` (the gateway's op 62 reaches the area through a static hook).
static REGISTRY: Mutex<Vec<(Weak<Inner>, Arc<Sealed>)>> = Mutex::new(Vec::new());

/// The sealed path of the client `inner`, made on first use.
pub(crate) fn sealed_for(inner: &Arc<Inner>) -> Arc<Sealed> {
    let mut r = REGISTRY
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    r.retain(|(w, _)| w.strong_count() > 0);
    if let Some((_, s)) = r
        .iter()
        .find(|(w, _)| std::ptr::eq(w.as_ptr(), Arc::as_ptr(inner)))
    {
        return s.clone();
    }
    let sealed = Sealed::new(Arc::new(io::InnerIo {
        inner: Arc::downgrade(inner),
    }));
    r.push((Arc::downgrade(inner), sealed.clone()));
    sealed
}

/// The gateway's lifecycle, as controller.ts drives the Bot catalog: on every ready the device lists are read again
/// when needed and each community's catalog is read (its Bot user looked up again, its key checked) once the structure
/// and the members are in. A loss or a refusal ends that wait. Nothing is forgotten here: a refused session is usually
/// renewed (4004 on expiry), and its runs and Retries go on; only a revocation ([`revoked`]) or a stop ([`stopped`])
/// forgets them, as TS `enterRevoked` and `forgetAccount`.
pub(crate) fn on_connection(inner: &Arc<Inner>, event: &crate::sync::ConnectionEvent) {
    on_connection_of(&sealed_for(inner), Arc::downgrade(inner), event);
}

/// [`on_connection`] for one client's sealed path.
pub(crate) fn on_connection_of(
    sealed: &Arc<Sealed>,
    inner: Weak<Inner>,
    event: &crate::sync::ConnectionEvent,
) {
    let generation = sealed.next_connection_generation();
    if !matches!(event, crate::sync::ConnectionEvent::Ready(_)) {
        return;
    }
    sealed.new_connection();
    // controller.ts (review M6): once the ready's trust refresh has settled, a community whose first pin was refused for
    // want of the Bot's attestation is resolved again (Task 28 item 21, `attestationsChanged`).
    let (after, refreshing) = (sealed.clone(), inner.clone());
    sealed.spawn(Box::pin(async move {
        let Some(engine) = refreshing.upgrade().map(|i| i.engine().clone()) else {
            return;
        };
        engine.refresh_trust().await;
        if after.connection_generation() == generation {
            after.attestations_changed().await;
        }
    }));
    let weak = inner;
    let waiting = sealed.clone();
    sealed.spawn(Box::pin(async move {
        // The structure is read after the ready: wait (bounded) for every community's members, while this connection
        // lasts.
        for _ in 0..120 {
            if waiting.connection_generation() != generation {
                return;
            }
            let Some(inner) = weak.upgrade() else {
                return;
            };
            let s = inner.snapshot();
            drop(inner);
            if !s.communities.is_empty()
                && s.communities.iter().all(|c| s.members.contains_key(&c.id))
            {
                for c in &s.communities {
                    let _ = waiting.refresh(c.id).await;
                }
                return;
            }
            tokio::time::sleep(std::time::Duration::from_millis(250)).await;
        }
    }));
}

/// The device was revoked (the client signs out): every reply secret and held run of that client goes.
pub(crate) fn revoked(engine: &Arc<crate::sync::Engine>) {
    let r = REGISTRY
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    for (w, sealed) in r.iter() {
        if w.upgrade().is_some_and(|i| Arc::ptr_eq(i.engine(), engine)) {
            sealed.forget_all();
        }
    }
}

/// The client stopped: its sealed path forgets everything and is dropped.
pub(crate) fn stopped(inner: &Arc<Inner>) {
    let mut r = REGISTRY
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    r.retain(|(w, sealed)| {
        let mine = std::ptr::eq(w.as_ptr(), Arc::as_ptr(inner));
        if mine || w.strong_count() == 0 {
            sealed.forget_all();
        }
        !mine && w.strong_count() > 0
    });
}

/// Each running client's op 63 bookkeeping ([`hints::HintGate`]), by its `Inner`.
static HINTS: Mutex<Vec<(Weak<Inner>, SharedGate)>> = Mutex::new(Vec::new());

/// One client's [`hints::HintGate`], shared with its trailing fetches.
type SharedGate = Arc<Mutex<hints::HintGate>>;

fn hints_for(inner: &Arc<Inner>) -> SharedGate {
    let mut r = HINTS
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    r.retain(|(w, _)| w.strong_count() > 0);
    if let Some((_, g)) = r
        .iter()
        .find(|(w, _)| std::ptr::eq(w.as_ptr(), Arc::as_ptr(inner)))
    {
        return g.clone();
    }
    let gate = Arc::new(Mutex::new(hints::HintGate::default()));
    r.push((Arc::downgrade(inner), gate.clone()));
    gate
}

/// Runs `fetch` for op 63's hint `frame` when the gate says (hints.ts `request`): now, after the coalescing window, or not
/// at all.
fn coalesced(
    inner: &Arc<Inner>,
    frame: &frames::BotStateChangedFrame,
    fetch: impl FnOnce() + Send + 'static,
) {
    let scope = (frame.scope_kind, frame.scope_id);
    let gate = hints_for(inner);
    let hint = gate
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .on_hint(scope, frame.revision, std::time::Instant::now());
    match hint {
        hints::Hint::Drop => {}
        hints::Hint::Now => fetch(),
        hints::Hint::After(wait) => drop(inner.spawn(async move {
            tokio::time::sleep(wait).await;
            gate.lock()
                .unwrap_or_else(std::sync::PoisonError::into_inner)
                .due(scope, std::time::Instant::now());
            fetch();
            Ok(())
        })),
    }
}

/// The voice channel whose player op 63 asks to read again: a call scope (kind 2, its id the voice channel) of the call
/// this device is in, where the dock shows the player (music.tsx reads only a shown player).
fn music_hint(
    state: &AppState,
    frame: &frames::BotStateChangedFrame,
) -> Option<crate::state::ChannelId> {
    let call = &state.voice.call;
    let channel = call.channel?;
    (frame.scope_kind == 2
        && call.phase == crate::calls::CallPhase::In
        && channel.0 == frame.scope_id)
        .then_some(channel)
}

impl AreaFrames for Bot {
    fn on_frame(inner: &Arc<Inner>, f: &Frame) {
        let sealed = sealed_for(inner);
        match f.op {
            Op::BOT_CATALOG => {
                if let Ok(frame) = frames::bot_catalog_of(f) {
                    drop(inner.spawn(async move {
                        sealed.apply_frame(&frame).await;
                        Ok(())
                    }));
                }
            }
            Op::BOT_REPLY => {
                if let Ok(frame) = frames::bot_reply_of(f) {
                    sealed.on_reply(&frame);
                }
            }
            Op::BOT_STATE_CHANGED => {
                let Ok(frame) = frames::bot_state_changed_of(f) else {
                    return;
                };
                // A community's Bot changed state (scope kind 0): read its catalog and status again, at most once per
                // 250 ms (Task 28 item 15, hints.ts).
                if frame.scope_kind == 0 {
                    let reading = inner.clone();
                    coalesced(inner, &frame, move || {
                        drop(reading.spawn(async move {
                            let _ = sealed.refresh(CommunityId(frame.scope_id)).await;
                            Ok(())
                        }));
                    });
                }
                // The player of this device's call changed (Task 21's `Music::refresh`, integration 3 step 5). Only a
                // shown player is read (hints.ts `visible`).
                if let Some(channel) = music_hint(&inner.snapshot(), &frame) {
                    let reading = inner.clone();
                    coalesced(inner, &frame, move || {
                        let transport = crate::features::bot_admin::default_transport(&reading);
                        let admin =
                            crate::features::bot_admin::BotAdmin::new(reading.clone(), transport);
                        drop(crate::features::music::Music::new(admin).refresh(channel));
                    });
                }
            }
            _ => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gateway::frames::Value;
    use crate::state::bot::{BotCatalogView, BotStatus, BotStatusState};

    /// Task 28 item 15 (routes.ts `getWebhookAvatar`): only a 64-digit lower-case hex id is ever put in the path.
    #[test]
    fn a_webhook_avatar_id_is_64_lower_hex() {
        assert!(is_webhook_avatar_id(&"ab".repeat(32)));
        assert!(!is_webhook_avatar_id(&"AB".repeat(32)));
        assert!(!is_webhook_avatar_id(&"ab".repeat(31)));
        assert!(!is_webhook_avatar_id(&format!("{}/x", "a".repeat(62))));
    }

    /// Integration 3, step 5: op 63's call scope (kind 2, its id the voice channel) reads the player again while this
    /// device is in that call (the dock shows it, music.tsx's `watchBotHint`); another call, another kind or no call
    /// reads nothing.
    #[test]
    fn a_call_scope_hint_reads_the_player_of_this_devices_call() {
        let lounge = crate::state::ChannelId([8; 16]);
        let hint = |kind: u8, id: [u8; 16]| frames::BotStateChangedFrame {
            scope_kind: kind,
            scope_id: id,
            revision: 4,
        };
        let mut s = AppState::default();
        assert_eq!(music_hint(&s, &hint(2, lounge.0)), None, "no call");
        s.voice.call.phase = crate::calls::CallPhase::In;
        s.voice.call.channel = Some(lounge);
        assert_eq!(music_hint(&s, &hint(2, lounge.0)), Some(lounge));
        assert_eq!(music_hint(&s, &hint(2, [9; 16])), None, "another call");
        assert_eq!(music_hint(&s, &hint(1, lounge.0)), None, "a channel scope");
    }

    fn state_with_catalog() -> (AppState, CommunityId) {
        let c = CommunityId([7; 16]);
        let mut s = AppState::default();
        s.account.username = "mira".into();
        let remind = Value::Array(vec![
            Value::Text("remind".into()),
            Value::Text("Set a reminder".into()),
            Value::Uint(80),
            Value::Array(vec![
                Value::Array(vec![
                    Value::Text("when".into()),
                    Value::Uint(4),
                    Value::Uint(1),
                ]),
                Value::Array(vec![
                    Value::Text("text".into()),
                    Value::Uint(0),
                    Value::Uint(1),
                ]),
            ]),
            Value::Uint(0),
            Value::Uint(0),
            Value::Uint(0),
        ]);
        s.bot.catalogs.insert(
            c,
            BotCatalogView {
                revision: 3,
                commands: catalog::decode_commands(&[remind]).unwrap(),
                status: BotStatus {
                    state: BotStatusState::Ready,
                    since: None,
                },
                bot_channels: None,
                bot_user: None,
                bot_name: String::new(),
                fingerprint: None,
            },
        );
        (s, c)
    }

    #[test]
    fn literal_and_classic_drafts_post() {
        let (s, c) = state_with_catalog();
        assert_eq!(plan(&s, Some(c), "hello"), Plan::Post("hello".into()));
        assert_eq!(plan(&s, Some(c), "//shrug"), Plan::Post("/shrug".into()));
        assert_eq!(
            plan(&s, Some(c), "/me packs the rope"),
            Plan::Post("_mira packs the rope_".into())
        );
        assert_eq!(
            plan(&s, Some(c), "/tableflip now"),
            Plan::Post("(╯°□°)╯︵ ┻━┻ now".into())
        );
        assert_eq!(
            plan(&s, Some(c), "/code rust fn x() {}"),
            Plan::Post("```rust\nfn x() {}\n```".into())
        );
    }

    #[test]
    fn help_and_unknown_answer_privately() {
        let (s, c) = state_with_catalog();
        match plan(&s, Some(c), "/help") {
            Plan::Private(ResultStatus::Success, text, ResultProvider::Builtin) => {
                assert!(text.starts_with("/help [command]"));
            }
            other => panic!("{other:?}"),
        }
        assert_eq!(
            plan(&s, Some(c), "/xyz"),
            Plan::Private(
                ResultStatus::Failed,
                not_a_command("xyz"),
                ResultProvider::Builtin
            )
        );
        assert_eq!(plan(&s, Some(c), "/code"), Plan::Incomplete);
    }

    /// builtin.ts `kick`/`ban`: a joined member this account outranks, named by username or id, is a moderation to
    /// confirm; without the permission, against the owner, oneself or an unknown name it fails privately; a ban checks
    /// its length and reason first.
    #[test]
    fn kick_and_ban_ask_first_only_for_a_member_this_account_may_moderate() {
        use crate::state::members::Member;
        use crate::state::{Community, UserId};
        let c = CommunityId([7; 16]);
        let (me, bea, owner) = (UserId([1; 16]), UserId([2; 16]), UserId([3; 16]));
        let member = |user_id, username: &str| Member {
            user_id,
            username: username.into(),
            display: String::new(),
            kind: 0,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        };
        let mut s = AppState::default();
        s.account.user_id = Some(me);
        s.communities.push(Community {
            id: c,
            name: "Midgard".into(),
            owner: me,
            policy_version: 1,
        });
        s.members.insert(
            c,
            vec![member(me, "ada"), member(bea, "bea"), member(owner, "olaf")],
        );
        let mut access = s.roles.access_of(c);
        access.loaded = true;
        access.owner = true;
        access.kick_members = true;
        access.ban_members = true;
        s.roles.access.insert(c, access.clone());
        let kick = Plan::Moderate(Moderation {
            community: c,
            user: bea,
            name: "bea".into(),
            action: ModAction::Kick,
        });
        assert_eq!(plan(&s, Some(c), "/kick user:bea"), kick);
        assert_eq!(
            plan(&s, Some(c), &format!("/kick user:{}", bea.hex())),
            kick
        );
        let refused =
            |text: &str| Plan::Private(ResultStatus::Failed, text.into(), ResultProvider::Builtin);
        let cannot_kick =
            refused("This member cannot be kicked with your current permissions and rank.");
        assert_eq!(plan(&s, Some(c), "/kick user:ada"), cannot_kick, "oneself");
        assert_eq!(plan(&s, Some(c), "/kick user:nobody"), cannot_kick);
        match plan(&s, Some(c), "/ban user:bea duration:2d reason:spam") {
            Plan::Moderate(Moderation {
                action: ModAction::Ban {
                    seconds, reason, ..
                },
                ..
            }) => assert_eq!((seconds, reason.as_str()), (Some(172_800), "spam")),
            other => panic!("{other:?}"),
        }
        assert!(matches!(
            plan(&s, Some(c), "/ban user:bea duration:permanent"),
            Plan::Moderate(Moderation {
                action: ModAction::Ban { seconds: None, .. },
                ..
            })
        ));
        assert_eq!(
            plan(&s, Some(c), "/ban user:bea duration:10m"),
            refused("Ban duration must be from 1h to 100y.")
        );
        // Not the owner and without Kick Members: nobody can be kicked.
        s.communities[0].owner = owner;
        access.owner = false;
        access.kick_members = false;
        s.roles.access.insert(c, access);
        assert_eq!(plan(&s, Some(c), "/kick user:bea"), cannot_kick);
    }

    #[test]
    fn catalog_commands_are_sealed_and_only_in_their_community() {
        let (s, c) = state_with_catalog();
        match plan(&s, Some(c), "/remind 1h stretch") {
            Plan::Bot { definition, args } => {
                assert_eq!(definition.id, "remind");
                assert_eq!(args.get("text").map(String::as_str), Some("stretch"));
            }
            other => panic!("{other:?}"),
        }
        assert_eq!(
            plan(&s, None, "/remind 1h stretch"),
            Plan::Private(
                ResultStatus::Failed,
                not_a_command("remind"),
                ResultProvider::Builtin
            )
        );
    }
}
