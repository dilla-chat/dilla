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
use crate::features::moderation::{Evidence, Removal, RemovalOutcome};
use crate::gateway::frames::{self, Frame, Op};
use crate::state::bot::{Conversation, ResultProvider, ResultStatus};
use crate::state::{AppState, ChannelId, CommunityId, MessageId, TimelineItem, TimelineItemState};
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

/// What running a draft did. Its Debug is redacted like [`Plan`]'s.
#[derive(Clone, PartialEq, Eq)]
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
    /// A built-in that acts on another area ([`Plan::Builtin`]): the caller asks first when `confirm` is set, then runs it.
    Builtin { run: Builtin, confirm: Option<Ask> },
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
    plan_in(state, community, None, draft)
}

/// [`plan`] for a draft typed in `channel`: the built-ins that read the channel's timeline (`/delete`, `/edit`, ...) need
/// it. `None` where no conversation is open (the picker's reading of a draft).
pub fn plan_in(
    state: &AppState,
    community: Option<CommunityId>,
    channel: Option<ChannelId>,
    draft: &str,
) -> Plan {
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
                "timeout" | "untimeout" | "warn" | "unban" => {
                    member_builtin_plan(state, community, &command_id, &args)
                }
                "reply" | "edit" | "delete" | "pin" | "unpin" | "topic" => {
                    message_plan(state, community, channel, &command_id, &args)
                }
                "role add" | "role remove" | "selfrole list" | "selfrole add"
                | "selfrole remove" => role_plan(state, community, &command_id, &args),
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
                // Left here: the navigation, UI-opening and voice built-ins (`/giphy /dm /invite /search /thread
                // /join /leave /mute /unmute /deafen /undeafen /modlog`), which answer "unavailable" until their areas
                // move onto [`Plan::Builtin`]; `every_builtin_has_a_handler` lists them exactly, and this arm goes
                // with the last of them (`registry::commands()` is closed, so no other id reaches it).
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
///
/// Its Debug gives only lengths: the copy names the member acted on ("{name} was banned.").
#[derive(Clone, PartialEq, Eq)]
pub struct ModerationTexts {
    pub cancelled: String,
    pub pending: String,
    pub done: String,
}

impl std::fmt::Debug for ModerationTexts {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "ModerationTexts({}, {}, {} bytes)",
            self.cancelled.len(),
            self.pending.len(),
            self.done.len()
        )
    }
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
#[derive(Clone, PartialEq, Eq)]
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
    /// A built-in that acts on another area: asks first when `confirm` is set (CommandHost.tsx's generic `Dialog`), then
    /// the caller runs `run`. Its row is pending from the start (CommandHost.tsx:236).
    Builtin {
        run: Builtin,
        confirm: Option<Ask>,
    },
    Incomplete,
}

/// R21: a post is a message body, a private row's text may quote a sealed answer, and a Bot command's argument values
/// are sealed: Debug gives their lengths and the argument names only.
impl std::fmt::Debug for Plan {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Post(text) => write!(f, "Post({:?})", crate::state::Redacted(text)),
            Self::Private(status, text, provider) => write!(
                f,
                "Private({status:?}, {:?}, {provider:?})",
                crate::state::Redacted(text)
            ),
            Self::Bot { definition, args } => f
                .debug_struct("Bot")
                .field("command", &definition.id)
                .field("args", &registry::ArgNames(args))
                .finish(),
            Self::Moderate(m) => f.debug_tuple("Moderate").field(m).finish(),
            Self::Builtin { run, confirm } => f
                .debug_struct("Builtin")
                .field("run", run)
                .field("confirm", confirm)
                .finish(),
            Self::Incomplete => write!(f, "Incomplete"),
        }
    }
}

impl std::fmt::Debug for RunOutcome {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Post(text) => write!(f, "Post({:?})", crate::state::Redacted(text)),
            Self::Private(id) => write!(f, "Private({id})"),
            Self::Incomplete => write!(f, "Incomplete"),
            Self::Bot { definition, args } => f
                .debug_struct("Bot")
                .field("command", &definition.id)
                .field("args", &registry::ArgNames(args))
                .finish(),
            Self::Moderate(m) => f.debug_tuple("Moderate").field(m).finish(),
            Self::Builtin { run, confirm } => f
                .debug_struct("Builtin")
                .field("run", run)
                .field("confirm", confirm)
                .finish(),
        }
    }
}

/// The question a built-in asks before it runs: the dialog's title and body (`ConfirmDialog`). Its `Debug` gives only
/// their lengths: the body may carry a moderation reason ("…Reason: {reason}") and the title the member it acts on
/// ("Unban {name}?").
#[derive(Clone, PartialEq, Eq)]
pub struct Ask {
    pub title: String,
    pub body: String,
}

impl std::fmt::Debug for Ask {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "Ask({} bytes, {} bytes)",
            self.title.len(),
            self.body.len()
        )
    }
}

/// A built-in command that acts on another area, with what it needs from the draft. The commands move here from the
/// "unavailable" catch-all of [`plan_in`] as their areas land (Tasks 10, 11, 24, 25). The text a command carries is the
/// composer's encoded text once `on_send` has prepared it (`Reply`, `Edit`), and `RemoveOther`'s excerpt is filled there
/// too; its `Debug` never prints any of it (E2EE content stays out of logs).
#[derive(Clone, PartialEq, Eq)]
pub enum Builtin {
    /// `/reply`: posts `text` in the conversation as a reply to `to` (builtin.ts `publish`).
    Reply { to: MessageId, text: String },
    /// `/edit`: replaces the text of this account's message `msg`.
    Edit { msg: MessageId, text: String },
    /// `/delete` of this account's own message.
    DeleteOwn { msg: MessageId },
    /// `/delete` of another person's message (builtin.ts `removeOtherMessage`): removed for everyone with `reason`, the
    /// message's `excerpt` kept as moderation evidence ([`removal_step`]).
    RemoveOther {
        msg: MessageId,
        seq: u64,
        reason: String,
        excerpt: String,
    },
    /// `/pin` (`on`) and `/unpin`.
    Pin { msg: MessageId, on: bool },
    /// `/topic`: sets `channel`'s topic to `text`.
    Topic { channel: ChannelId, text: String },
    /// `/timeout`: restricts `user` (`name` is shown) for `seconds` from when it runs.
    Timeout {
        community: CommunityId,
        user: crate::state::UserId,
        name: String,
        seconds: u64,
    },
    /// `/untimeout`: reads the member's timeout first, then asks ([`Self::UntimeoutConfirmed`]) only when there is one.
    Untimeout {
        community: CommunityId,
        user: crate::state::UserId,
        name: String,
    },
    /// `/untimeout` once its question was answered: lifts the timeout.
    UntimeoutConfirmed {
        community: CommunityId,
        user: crate::state::UserId,
        name: String,
    },
    /// `/warn`: warns `user` with `reason` (moderation evidence: never printed).
    Warn {
        community: CommunityId,
        user: crate::state::UserId,
        name: String,
        reason: String,
    },
    /// `/unban`: lifts the ban of `user`, `name` as the ban row names them.
    Unban {
        community: CommunityId,
        user: crate::state::UserId,
        name: String,
    },
    /// `/role add` (`add`) and `/role remove`: grants or revokes `role` of `user`, checked again as it runs
    /// ([`assignable_role`]). `role_name` and `member_name` are what its question shows.
    Role {
        community: CommunityId,
        user: crate::state::UserId,
        role: crate::state::roles::RoleId,
        add: bool,
        role_name: String,
        member_name: String,
    },
    /// `/selfrole list`: reads the self-assignable roles and lists them.
    SelfRoleList { community: CommunityId },
    /// `/selfrole add` (`add`) and `/selfrole remove` of the self-assignable role whose id is `role` (hex, as typed):
    /// the roles are read first, and only one that is offered is picked or dropped.
    SelfRole {
        community: CommunityId,
        role: String,
        add: bool,
    },
}

impl std::fmt::Debug for Builtin {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        // Only the ids and the lengths: the texts are message bodies, reasons and excerpts. A moderation command names no
        // member (neither id nor name): who it acts on is moderation content.
        match self {
            Self::Reply { to, text } => write!(f, "Reply({to:?}, {} bytes)", text.len()),
            Self::Edit { msg, text } => write!(f, "Edit({msg:?}, {} bytes)", text.len()),
            Self::DeleteOwn { msg } => write!(f, "DeleteOwn({msg:?})"),
            Self::RemoveOther { msg, seq, .. } => write!(f, "RemoveOther({msg:?}, {seq})"),
            Self::Pin { msg, on } => write!(f, "Pin({msg:?}, {on})"),
            Self::Topic { channel, text } => {
                write!(f, "Topic({channel:?}, {} bytes)", text.len())
            }
            Self::Timeout {
                community, seconds, ..
            } => write!(f, "Timeout({community:?}, {seconds})"),
            Self::Untimeout { community, .. } => write!(f, "Untimeout({community:?})"),
            Self::UntimeoutConfirmed { community, .. } => {
                write!(f, "UntimeoutConfirmed({community:?})")
            }
            Self::Warn {
                community, reason, ..
            } => write!(f, "Warn({community:?}, {} bytes)", reason.len()),
            Self::Unban { community, .. } => write!(f, "Unban({community:?})"),
            // Who gains or loses which role is a role action's content: neither the member nor the role shows.
            Self::Role { community, add, .. } => write!(f, "Role({community:?}, {add})"),
            Self::SelfRoleList { community } => write!(f, "SelfRoleList({community:?})"),
            Self::SelfRole { community, add, .. } => write!(f, "SelfRole({community:?}, {add})"),
        }
    }
}

impl Builtin {
    /// The command's id, for its pending row's copy (`privateResultCopy.pending(id)`).
    pub fn id(&self) -> &'static str {
        match self {
            Self::Reply { .. } => "reply",
            Self::Edit { .. } => "edit",
            Self::DeleteOwn { .. } | Self::RemoveOther { .. } => "delete",
            Self::Pin { on: true, .. } => "pin",
            Self::Pin { on: false, .. } => "unpin",
            Self::Topic { .. } => "topic",
            Self::Timeout { .. } => "timeout",
            Self::Untimeout { .. } | Self::UntimeoutConfirmed { .. } => "untimeout",
            Self::Warn { .. } => "warn",
            Self::Unban { .. } => "unban",
            Self::Role { add: true, .. } => "role add",
            Self::Role { add: false, .. } => "role remove",
            Self::SelfRoleList { .. } => "selfrole list",
            Self::SelfRole { add: true, .. } => "selfrole add",
            Self::SelfRole { add: false, .. } => "selfrole remove",
        }
    }

    /// Whether cancelling its question removes its pending row (CommandHost.tsx:291, :322: `cancel && !moderation`). A
    /// command that wants its cancelled row kept answers `false` in its own arm; the match has no wildcard so a new
    /// command must decide.
    pub fn dismissed_on_cancel(&self) -> bool {
        match self {
            // CommandHost's `moderation` is `/kick` and `/ban` only (:233): a cancelled `/delete`, even of another
            // person's message, and a cancelled `/timeout`, `/untimeout`, `/warn`, `/unban` or `/role remove` leave no
            // row.
            Self::Reply { .. }
            | Self::Edit { .. }
            | Self::DeleteOwn { .. }
            | Self::RemoveOther { .. }
            | Self::Pin { .. }
            | Self::Topic { .. }
            | Self::Timeout { .. }
            | Self::Untimeout { .. }
            | Self::UntimeoutConfirmed { .. }
            | Self::Warn { .. }
            | Self::Unban { .. }
            | Self::Role { .. }
            | Self::SelfRoleList { .. }
            | Self::SelfRole { .. } => true,
        }
    }
}

/// What the `/delete` of another person's message does next, after its one `remove_message` (builtin.ts:355-379). The
/// removal is never sent again, so no step would. Its Debug gives the row's text by length, as [`PrivateResult`]'s does.
///
/// [`PrivateResult`]: crate::state::bot::PrivateResult
#[derive(Clone, PartialEq, Eq)]
pub enum RemovalStep {
    /// The removal may have landed: settle the same proof (`Moderation::resolve_removal`).
    Resolve([u8; 16]),
    /// Accepted, its evidence not stored yet: send it once more (`Moderation::send_removal_evidence`).
    SendEvidence([u8; 16]),
    /// The row's answer. `uncertain` marks the web's `result('uncertain', …)` lines.
    Done {
        status: ResultStatus,
        text: String,
        uncertain: bool,
    },
}

impl std::fmt::Debug for RemovalStep {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Resolve(p) => f.debug_tuple("Resolve").field(p).finish(),
            Self::SendEvidence(p) => f.debug_tuple("SendEvidence").field(p).finish(),
            Self::Done {
                status,
                text,
                uncertain,
            } => f
                .debug_struct("Done")
                .field("status", status)
                .field("text", &crate::state::Redacted(text))
                .field("uncertain", uncertain)
                .finish(),
        }
    }
}

/// The next [`RemovalStep`] for `o`, the outcome so far: `resolved` once it was resolved (`o` is then the resolved
/// outcome), `evidence_sent` once its evidence was sent again (`o.evidence` is then that answer).
pub fn removal_step(o: &RemovalOutcome, resolved: bool, evidence_sent: bool) -> RemovalStep {
    let proof = o.proof_request_id;
    let done = |status, text: String, uncertain| RemovalStep::Done {
        status,
        text,
        uncertain,
    };
    match o.removal {
        Removal::Rejected => done(
            ResultStatus::Failed,
            "The message could not be removed.".to_owned(),
            false,
        ),
        Removal::Unknown if !resolved => RemovalStep::Resolve(proof),
        Removal::Unknown => done(
            ResultStatus::Failed,
            format!(
                "Message removal is unresolved. Recovery proof: {}.",
                crate::state::hex(&proof)
            ),
            true,
        ),
        Removal::Accepted if o.evidence == Evidence::Accepted => done(
            ResultStatus::Success,
            "Message removed and moderation evidence recorded.".to_owned(),
            false,
        ),
        // builtin.ts sends the evidence again only with the removal's evidence request id.
        Removal::Accepted if !evidence_sent && o.evidence_request_id.is_some() => {
            RemovalStep::SendEvidence(proof)
        }
        Removal::Accepted => {
            let evidence = match o.evidence {
                Evidence::Pending => "pending",
                Evidence::Rejected => "rejected",
                Evidence::Accepted | Evidence::Unknown => "unknown",
            };
            let hex = crate::state::hex(&proof);
            let text = if resolved {
                format!(
                    "Message removal was accepted; moderation evidence is {evidence}. Recovery proof: {hex}."
                )
            } else {
                format!(
                    "Message removed; moderation evidence is {evidence}. Recovery proof: {hex}."
                )
            };
            done(ResultStatus::Failed, text, true)
        }
    }
}

/// builtin.ts:344-349: why the one `remove_message` failed. A refusal (E_BAD_INPUT, E_FORBIDDEN, E_NOT_FOUND, or HTTP
/// 400, 403 or 404) removed nothing; anything else may have.
pub fn removal_error(e: &crate::sync::api::ApiError) -> String {
    if ["E_BAD_INPUT", "E_FORBIDDEN", "E_NOT_FOUND"].contains(&e.code.as_str())
        || [400, 403, 404].contains(&e.status)
    {
        "The message could not be removed with the current permissions or message state.".to_owned()
    } else {
        "Message removal outcome is uncertain. Check the conversation before retrying.".to_owned()
    }
}

/// builtin.ts:62-64: the stored message of `channel` whose id is `id` (hex): `Ok`, with a seq. An outbox row is not
/// addressable yet.
pub fn exact_message<'a>(
    state: &'a AppState,
    channel: ChannelId,
    id: &str,
) -> Option<&'a TimelineItem> {
    state.timelines.get(&channel)?.items.iter().find(|i| {
        i.state == TimelineItemState::Ok
            && i.seq.is_some()
            && i.msg_id.is_some_and(|m| m.hex() == id)
    })
}

/// [`mutation_error`] for a sync step's failure: a refusal names its code (read as an API error of that code); any other
/// failure is an uncertain outcome without a code.
pub fn mutation_error_of(e: &crate::ClientError) -> String {
    let code = match e {
        crate::ClientError::Sync(code) => code.clone(),
        _ => String::new(),
    };
    mutation_error(&crate::sync::api::ApiError {
        status: 0,
        code,
        detail: String::new(),
        retry_after_ms: None,
        extra: Vec::new(),
    })
}

/// A checked moderation command (builtin.ts `kick`, `ban`): its member, the name and `@username` the dialog shows, and
/// what it does. Its Debug names no member and no reason (moderation content): only the community and the action.
#[derive(Clone, PartialEq, Eq)]
pub struct Moderation {
    pub community: CommunityId,
    pub user: crate::state::UserId,
    pub name: String,
    pub username: String,
    pub action: ModAction,
}

impl std::fmt::Debug for Moderation {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "Moderation({:?}, {:?})", self.community, self.action)
    }
}

/// What a [`Moderation`] does. Its Debug gives a ban's length, never its reason.
#[derive(Clone, PartialEq, Eq)]
pub enum ModAction {
    Kick,
    /// `seconds` is `None` for a permanent ban; `duration` is the length as typed (`1d`, `permanent`).
    Ban {
        seconds: Option<u64>,
        duration: String,
        reason: String,
    },
}

impl std::fmt::Debug for ModAction {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Kick => write!(f, "Kick"),
            Self::Ban {
                seconds, reason, ..
            } => write!(f, "Ban({seconds:?}, {} bytes)", reason.len()),
        }
    }
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

/// A member [`moderation_target`] names: their id, the name to show and their username. Its Debug names none of them:
/// the member a moderation command acts on is moderation content.
#[derive(Clone, PartialEq, Eq)]
pub struct Target {
    pub user: crate::state::UserId,
    pub name: String,
    pub username: String,
}

impl std::fmt::Debug for Target {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Target").finish_non_exhaustive()
    }
}

/// builtin.ts `eligibleModerationTarget` read once for a community: this account's permission for the action and its
/// rank, so a whole member list is judged in one pass ([`ModerationGate::allows`]), as [`moderation_target`] judges one.
pub struct ModerationGate<'a> {
    me: crate::state::UserId,
    owner: crate::state::UserId,
    my_top: u32,
    roles: &'a [crate::state::roles::RoleSummary],
}

impl ModerationGate<'_> {
    /// Whether this account may moderate `target` (rank.ts `canModerate`).
    pub fn allows(&self, target: &crate::state::members::Member) -> bool {
        use crate::features::roles::rank;
        rank::can_moderate(
            self.me.0,
            target.user_id.0,
            self.owner.0,
            self.my_top,
            rank::top_of(&target.role_ids, self.roles),
        )
    }
}

/// What a [`ModerationGate`] is read for (builtin.ts `ModerationAction`): the permission it needs.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum GateAction {
    Kick,
    Ban,
    /// `/timeout`, `/untimeout` and `/warn`: Moderate Members.
    Timeout,
}

/// The [`ModerationGate`] of `community` for a kick (`kick`) or a ban: `None` when the access is not loaded, the
/// permission is not held, or this account is not a member there.
pub fn moderation_gate(
    state: &AppState,
    community: CommunityId,
    kick: bool,
) -> Option<ModerationGate<'_>> {
    moderation_gate_for(
        state,
        community,
        if kick {
            GateAction::Kick
        } else {
            GateAction::Ban
        },
    )
}

/// [`moderation_gate`] by action: the permission is Kick Members, Ban Members or Moderate Members (builtin.ts:329).
pub fn moderation_gate_for(
    state: &AppState,
    community: CommunityId,
    action: GateAction,
) -> Option<ModerationGate<'_>> {
    use crate::features::roles::rank;
    let access = state.roles.access_of(community);
    let permitted = match action {
        GateAction::Kick => access.kick_members,
        GateAction::Ban => access.ban_members,
        GateAction::Timeout => access.moderate_members,
    };
    if !access.loaded || !permitted {
        return None;
    }
    let owner = state.communities.iter().find(|c| c.id == community)?.owner;
    let roles = state.roles.roles_of(community);
    let me = state.account.user_id?;
    let mine = state
        .members
        .get(&community)?
        .iter()
        .find(|m| m.user_id == me)?;
    Some(ModerationGate {
        me,
        owner,
        my_top: rank::top_of(&mine.role_ids, roles),
        roles,
    })
}

/// builtin.ts `eligibleModerationTarget`: the member whose user id is `who` when this account holds the action's
/// permission and outranks them. A name is never resolved here, as names are not unique (w2-bot-review I-B1): the
/// picker's member chip puts the id of the member picked.
pub fn moderation_target(
    state: &AppState,
    community: CommunityId,
    who: &str,
    kick: bool,
) -> Option<Target> {
    moderation_target_for(
        state,
        community,
        who,
        if kick {
            GateAction::Kick
        } else {
            GateAction::Ban
        },
    )
}

/// [`moderation_target`] by action.
pub fn moderation_target_for(
    state: &AppState,
    community: CommunityId,
    who: &str,
    action: GateAction,
) -> Option<Target> {
    let who = crate::state::UserId(crate::features::server_admin::unhex_id(who.trim())?);
    let gate = moderation_gate_for(state, community, action)?;
    let target = state
        .members
        .get(&community)?
        .iter()
        .find(|m| m.user_id == who)?;
    gate.allows(target).then(|| member_target(target))
}

fn member_target(member: &crate::state::members::Member) -> Target {
    Target {
        user: member.user_id,
        name: if member.display.is_empty() {
            member.username.clone()
        } else {
            member.display.clone()
        },
        username: member.username.clone(),
    }
}

/// builtin.ts `eligibleAssignableRole`: the role named `role` (hex) and the member `user` (hex) when this account may
/// grant (`add`) or revoke it: roles are managed and loaded, both members are known, the member is neither this account
/// nor the owner, the role is neither @everyone nor at or above this account's rank, it is not held (`add`) or held
/// (revoke), and it carries no bit this account lacks ([`roles::assign_lock`]).
pub fn assignable_role(
    state: &AppState,
    community: CommunityId,
    user: &str,
    role: &str,
    add: bool,
) -> Option<(crate::state::roles::RoleSummary, Target)> {
    use crate::features::roles;
    let user = crate::state::UserId(crate::features::server_admin::unhex_id(user.trim())?);
    let role_id = crate::features::server_admin::unhex_id(role.trim())?;
    let access = state.roles.access_of(community);
    let everyone = access.everyone_id?;
    if !access.loaded || !access.manage_roles {
        return None;
    }
    let owner = state.communities.iter().find(|c| c.id == community)?.owner;
    let all = state.roles.roles_of(community);
    let members = state.members.get(&community)?;
    let me = state.account.user_id?;
    let target = members.iter().find(|m| m.user_id == user)?;
    let mine = members.iter().find(|m| m.user_id == me)?;
    let role = all.iter().find(|r| r.id == role_id)?;
    if target.user_id == me
        || target.user_id == owner
        || role.id == everyone
        || role.position == 0
        || target.role_ids.contains(&role.id) == add
    {
        return None;
    }
    let held = roles::held_bits(access.owner, owner.0, everyone, all, &mine.role_ids, me.0);
    roles::assign_lock(role, &access, held)
        .is_none()
        .then(|| (role.clone(), member_target(target)))
}

/// builtin.ts `reasonProblem`: more than 512 bytes, or a control character.
fn reason_problem(reason: &str) -> bool {
    reason.len() > 512
        || reason
            .chars()
            .any(|c| matches!(c as u32, 0..=8 | 0x0b..=0x1f | 0x7f..=0x9f))
}

/// builtin.ts `reply`, `edit`, `delete`, `pin`/`unpin` (`pinMessage`) and `topic`: the checks made before anything is
/// sent, against the message of `channel` the draft names ([`exact_message`]) and the reader's bits in that channel. In a
/// DM (`community` is `None`) no bit is needed to pin, and no topic is set.
fn message_plan(
    state: &AppState,
    community: Option<CommunityId>,
    channel: Option<ChannelId>,
    command_id: &str,
    args: &registry::Args,
) -> Plan {
    use crate::features::roles::{perm, viewer_channel_bits};
    let failed = |text: &str| {
        Plan::Private(
            ResultStatus::Failed,
            text.to_owned(),
            ResultProvider::Builtin,
        )
    };
    let arg = |n: &str| args.get(n).map_or("", String::as_str);
    let has = |bit: u32| {
        channel
            .and_then(|c| viewer_channel_bits(state, c))
            .is_some_and(|b| b & u64::from(bit) != 0)
    };
    let run = |run: Builtin, confirm: Option<Ask>| Plan::Builtin { run, confirm };
    if command_id == "topic" {
        return match channel {
            Some(channel) if community.is_some() && has(perm::MANAGE_CHANNELS) => run(
                Builtin::Topic {
                    channel,
                    text: arg("text").to_owned(),
                },
                None,
            ),
            _ => failed("You cannot manage this channel."),
        };
    }
    let item = channel.and_then(|c| exact_message(state, c, arg("message")));
    // `exact_message` finds only a stored message: it has its id and its seq.
    let found = item.and_then(|i| Some((i, i.msg_id?, i.seq?)));
    match command_id {
        "reply" => match found {
            Some((_, to, _)) => run(
                Builtin::Reply {
                    to,
                    text: arg("text").to_owned(),
                },
                None,
            ),
            None => failed("That message is not available in this conversation."),
        },
        "edit" => match found.filter(|(i, ..)| i.own) {
            Some((_, msg, _)) => run(
                Builtin::Edit {
                    msg,
                    text: arg("text").to_owned(),
                },
                None,
            ),
            None => failed("You can edit only your own loaded message."),
        },
        "delete" => {
            let Some((item, msg, seq)) = found else {
                return failed("That message is not available in this conversation.");
            };
            let title = "Delete this message?".to_owned();
            if item.own {
                return run(
                    Builtin::DeleteOwn { msg },
                    Some(Ask {
                        title,
                        body: "This permanently deletes your message.".to_owned(),
                    }),
                );
            }
            let reason = arg("reason").trim();
            if !has(perm::MANAGE_MESSAGES) || reason.is_empty() {
                return failed(DELETE_OTHER_REFUSED);
            }
            run(
                Builtin::RemoveOther {
                    msg,
                    seq,
                    reason: reason.to_owned(),
                    excerpt: String::new(),
                },
                Some(Ask {
                    title,
                    body: format!("Remove this message from everyone? Reason: {reason}"),
                }),
            )
        }
        _ => {
            let on = command_id == "pin";
            let Some((item, msg, _)) = found else {
                return failed("That message is not available in this channel.");
            };
            if community.is_some() && !has(perm::PIN_MESSAGES) {
                return failed("You cannot pin messages here.");
            }
            if item.pinned == on {
                return Plan::Private(
                    ResultStatus::Success,
                    if on {
                        "Message is already pinned."
                    } else {
                        "Message is already unpinned."
                    }
                    .to_owned(),
                    ResultProvider::Builtin,
                );
            }
            run(Builtin::Pin { msg, on }, None)
        }
    }
}

/// builtin.ts `delete`: another person's message needs Manage Messages and a reason (checked again as it runs).
pub const DELETE_OTHER_REFUSED: &str =
    "Deleting another person’s message requires Manage Messages and a reason.";

/// builtin.ts `timeout`, `untimeout`, `warn` (:204-264) and `unban` (:168-182): the checks made before anything is
/// asked. `/unban` reads the bans this device has (the member chip read them, Task 9); its run reads them again.
fn member_builtin_plan(
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
    let arg = |n: &str| args.get(n).map_or("", String::as_str);
    let run = |run: Builtin, confirm: Option<Ask>| Plan::Builtin { run, confirm };
    if command_id == "unban" {
        if !may_unban(state, community) {
            return failed("You cannot unban members here.");
        }
        let ban = crate::features::server_admin::unhex_id(arg("user").trim()).and_then(|who| {
            state
                .people
                .bans
                .get(&community)?
                .iter()
                .find(|b| b.user_id.0 == who)
        });
        let Some(ban) = ban else {
            return failed("That member is not currently banned.");
        };
        return run(
            Builtin::Unban {
                community,
                user: ban.user_id,
                name: ban.name.clone(),
            },
            Some(Ask {
                title: format!("Unban {}?", ban.name),
                body: "This restores the member’s ability to join the community.".to_owned(),
            }),
        );
    }
    let Some(Target { user, name, .. }) =
        moderation_target_for(state, community, arg("user"), GateAction::Timeout)
    else {
        return failed(if command_id == "warn" {
            "This member cannot be warned with your current permissions and rank."
        } else {
            "This member cannot be timed out with your current permissions and rank."
        });
    };
    match command_id {
        "timeout" => {
            let duration = arg("duration");
            let Some(seconds) =
                run::duration_seconds(duration).filter(|s| (60..=2_419_200).contains(s))
            else {
                return failed("Timeout duration must be from 1m to 28d.");
            };
            let ask = Ask {
                title: format!("Timeout {name}?"),
                body: format!("Restrict this member for {duration}."),
            };
            run(
                Builtin::Timeout {
                    community,
                    user,
                    name,
                    seconds,
                },
                Some(ask),
            )
        }
        "untimeout" => run(
            Builtin::Untimeout {
                community,
                user,
                name,
            },
            None,
        ),
        _ => {
            let reason = arg("reason").trim();
            if reason.is_empty() || reason_problem(reason) {
                return failed(
                    "Enter a reason of at most 512 UTF-8 bytes without control characters.",
                );
            }
            let ask = Ask {
                title: format!("Warn {name}?"),
                body: reason.to_owned(),
            };
            run(
                Builtin::Warn {
                    community,
                    user,
                    name,
                    reason: reason.to_owned(),
                },
                Some(ask),
            )
        }
    }
}

/// builtin.ts `role add`, `role remove` (`updateMemberRole`, :395-404) and the `selfrole` commands (:267-271, :405-413):
/// a role is granted or revoked only where [`assignable_role`] allows it, and removing one asks first. The self roles
/// are read as the command runs, so planning one checks only that it is typed in a community.
fn role_plan(
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
    let arg = |n: &str| args.get(n).map_or("", String::as_str);
    let run = |run: Builtin, confirm: Option<Ask>| Plan::Builtin { run, confirm };
    match command_id {
        "selfrole list" => run(Builtin::SelfRoleList { community }, None),
        "selfrole add" | "selfrole remove" => run(
            Builtin::SelfRole {
                community,
                role: arg("role").trim().to_owned(),
                add: command_id == "selfrole add",
            },
            None,
        ),
        _ => {
            let add = command_id == "role add";
            let Some((role, member)) =
                assignable_role(state, community, arg("user"), arg("role"), add)
            else {
                return failed(
                    "This member or role is not assignable with your current permissions and rank.",
                );
            };
            let confirm = (!add).then(|| Ask {
                title: format!("Remove {} from {}?", role.name, member.name),
                body: "This changes the member’s roles.".to_owned(),
            });
            run(
                Builtin::Role {
                    community,
                    user: member.user,
                    role: role.id,
                    add,
                    role_name: role.name,
                    member_name: member.name,
                },
                confirm,
            )
        }
    }
}

/// builtin.ts `unban`'s `canBan`: this device knows its access in `community` and it holds Ban Members.
pub fn may_unban(state: &AppState, community: CommunityId) -> bool {
    let access = state.roles.access_of(community);
    access.loaded && access.ban_members
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
    let Some(Target {
        user,
        name,
        username,
    }) = moderation_target(state, community, who, kick)
    else {
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
        username,
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
        match plan_in(
            &self.inner.snapshot(),
            community,
            Some(conversation.channel),
            draft,
        ) {
            Plan::Post(text) => RunOutcome::Post(text),
            Plan::Incomplete => RunOutcome::Incomplete,
            Plan::Private(status, text, provider) => {
                RunOutcome::Private(self.add_result(conversation, status, text, provider))
            }
            Plan::Bot { definition, args } => RunOutcome::Bot { definition, args },
            Plan::Moderate(m) => RunOutcome::Moderate(m),
            Plan::Builtin { run, confirm } => RunOutcome::Builtin { run, confirm },
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
            .is_some_and(|t| t.user == m.user);
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

    /// A built-in's row says how it ended (`status` and `text`): the pending row of [`Plan::Builtin`] becomes its result.
    pub fn finish(&self, conversation: Conversation, row: u64, status: ResultStatus, text: String) {
        self.runs().patch(
            conversation,
            row,
            private::Patch {
                status: Some(status),
                text: Some(text),
                hold: None,
            },
        );
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

/// Starts a trailing fetch's wait on the client's runtime.
type Spawn<'a> = &'a dyn Fn(std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send>>);

/// Runs `fetch` for op 63's hint `frame` when the gate says (hints.ts `request`): now, after the coalescing window, or not
/// at all.
fn coalesced(
    gate: &SharedGate,
    frame: &frames::BotStateChangedFrame,
    spawn: Spawn<'_>,
    fetch: impl FnOnce() + Send + 'static,
) {
    let scope = (frame.scope_kind, frame.scope_id);
    let hint = gate
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
        .on_hint(scope, frame.revision, std::time::Instant::now());
    match hint {
        hints::Hint::Drop => {}
        hints::Hint::Now => fetch(),
        hints::Hint::After(wait) => {
            let gate = gate.clone();
            spawn(Box::pin(async move {
                tokio::time::sleep(wait).await;
                gate.lock()
                    .unwrap_or_else(std::sync::PoisonError::into_inner)
                    .due(scope, std::time::Instant::now());
                fetch();
            }));
        }
    }
}

/// Op 63 (Task 28 item 15, hints.ts): a community's Bot changed state (scope kind 0), so its catalog and status are read
/// again (`catalog`); the player of this device's call changed (`player`, Task 21's `Music::refresh`; only a shown player
/// is read, hints.ts `visible`). Each through the client's [`hints::HintGate`]: at most one read per scope per 250 ms.
fn on_state_changed(
    gate: &SharedGate,
    frame: &frames::BotStateChangedFrame,
    state: &AppState,
    spawn: Spawn<'_>,
    catalog: impl FnOnce(CommunityId) + Send + 'static,
    player: impl FnOnce(crate::state::ChannelId) + Send + 'static,
) {
    if frame.scope_kind == 0 {
        let community = CommunityId(frame.scope_id);
        coalesced(gate, frame, spawn, move || catalog(community));
    }
    if let Some(channel) = music_hint(state, frame) {
        coalesced(gate, frame, spawn, move || player(channel));
    }
}

/// The voice channel whose player op 63 asks to read again: a call scope (kind 2, its id the voice channel) of the call
/// this device is in, where the dock shows the player, or of a channel whose music card is on screen
/// (`bot_admin.music_watched`; music.tsx reads only a shown player).
fn music_hint(
    state: &AppState,
    frame: &frames::BotStateChangedFrame,
) -> Option<crate::state::ChannelId> {
    if frame.scope_kind != 2 {
        return None;
    }
    let channel = crate::state::ChannelId(frame.scope_id);
    let call = &state.voice.call;
    let in_call = call.phase == crate::calls::CallPhase::In && call.channel == Some(channel);
    (in_call || state.bot_admin.music_watched.contains(&channel)).then_some(channel)
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
                // PollCard.tsx: a pushed hint of a channel reads its given-up polls again.
                if frame.scope_kind == 1 {
                    crate::features::utilities::retry_gave_up_in(
                        inner,
                        crate::state::ChannelId(frame.scope_id),
                    );
                }
                let runtime = inner.clone();
                let spawn =
                    move |wait: std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send>>| {
                        drop(runtime.spawn(async move {
                            wait.await;
                            Ok(())
                        }));
                    };
                let (catalog_reader, player_reader) = (inner.clone(), inner.clone());
                on_state_changed(
                    &hints_for(inner),
                    &frame,
                    &inner.snapshot(),
                    &spawn,
                    move |community| {
                        drop(catalog_reader.spawn(async move {
                            let _ = sealed.refresh(community).await;
                            Ok(())
                        }));
                    },
                    move |channel| {
                        let transport =
                            crate::features::bot_admin::default_transport(&player_reader);
                        let admin = crate::features::bot_admin::BotAdmin::new(
                            player_reader.clone(),
                            transport,
                        );
                        drop(crate::features::music::Music::new(admin).refresh(channel));
                    },
                );
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

    /// builtin.ts:62-64: a message is found by its id only when it is stored (`Ok` with a seq); an outbox row, a bad id
    /// and another channel's message are not.
    #[test]
    fn plan_in_reads_the_channels_timeline() {
        use crate::state::{
            ChannelGroupState, ChannelId, MessageId, Timeline, TimelineItem, TimelineItemState,
        };
        let ch = ChannelId([1; 16]);
        let item = |msg: u8, state, seq| TimelineItem {
            key: format!("o{msg}"),
            state,
            reason: String::new(),
            sender_user: None,
            sender_device: None,
            own: true,
            bot: false,
            ts: 0,
            body: String::new(),
            msg_id: Some(MessageId([msg; 16])),
            seq,
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
            actions: Vec::new(),
        };
        let (m, p) = (MessageId([7; 16]), MessageId([8; 16]));
        let mut s = AppState::default();
        s.timelines.insert(
            ch,
            Timeline {
                channel_id: ch,
                group: ChannelGroupState::Active,
                items: vec![
                    item(7, TimelineItemState::Ok, Some(5)),
                    item(8, TimelineItemState::Pending, None),
                ],
                has_earlier: false,
            },
        );
        assert_eq!(
            exact_message(&s, ch, &m.hex()).map(|i| i.seq),
            Some(Some(5))
        );
        assert!(exact_message(&s, ch, &p.hex()).is_none());
        assert!(exact_message(&s, ch, "nothex").is_none());
        assert!(exact_message(&s, ChannelId([2; 16]), &m.hex()).is_none());
    }

    /// A community channel `ch` read by Ada (not the owner) whose `@everyone` grants `allow`, beside a DM channel `dm`.
    /// `ch` holds Ada's pinned message A, Bea's message B and Ada's outbox row P; `dm` holds A again, not pinned there.
    fn message_world(allow: u32) -> (AppState, CommunityId, crate::state::ChannelId) {
        use crate::features::roles::perm;
        use crate::state::channels::{Channel, ChannelGroupState, Community};
        use crate::state::members::Member;
        use crate::state::roles::{CommunityAccess, RoleSummary};
        use crate::state::{ChannelId, Timeline, TimelineItem, UserId};
        let (c, ch, dm, everyone) = (
            CommunityId([1; 16]),
            ChannelId([2; 16]),
            ChannelId([5; 16]),
            [3; 16],
        );
        let (ada, bea) = (UserId([4; 16]), UserId([6; 16]));
        let mut s = AppState::default();
        s.account.user_id = Some(ada);
        s.communities.push(Community {
            id: c,
            name: "Midgard".into(),
            owner: UserId([9; 16]),
            policy_version: 0,
        });
        s.channels.push(Channel {
            id: ch,
            community_id: c,
            kind: 0,
            mode: 0,
            name: "general".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Active,
        });
        let member = |user_id, username: &str| Member {
            user_id,
            username: username.into(),
            display: String::new(),
            kind: 0,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        };
        s.members
            .insert(c, vec![member(ada, "ada"), member(bea, "bea")]);
        s.roles.roles.insert(
            c,
            vec![RoleSummary {
                id: everyone,
                name: "@everyone".into(),
                color: 0,
                position: 0,
                hoist: false,
                mentionable: false,
                allow: perm::VIEW_CHANNEL | perm::SEND_MESSAGES | allow,
                deny: 0,
                members: 2,
            }],
        );
        s.roles.access.insert(
            c,
            CommunityAccess {
                loaded: true,
                everyone_id: Some(everyone),
                ..CommunityAccess::default()
            },
        );
        s.roles.overwrites.entry(c).or_default();
        let item = |msg: u8, own: bool, state, seq, pinned| TimelineItem {
            key: format!("o{msg}"),
            state,
            reason: String::new(),
            sender_user: Some(if own { ada } else { bea }),
            sender_device: None,
            own,
            bot: false,
            ts: 0,
            body: format!("body {msg}"),
            msg_id: Some(MessageId([msg; 16])),
            seq,
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned,
            attachments: Vec::new(),
            mention: false,
            actions: Vec::new(),
        };
        let timeline = |channel_id, items| Timeline {
            channel_id,
            group: ChannelGroupState::Active,
            items,
            has_earlier: false,
        };
        s.timelines.insert(
            ch,
            timeline(
                ch,
                vec![
                    item(0xa, true, TimelineItemState::Ok, Some(1), true),
                    item(0xb, false, TimelineItemState::Ok, Some(2), false),
                    item(0xc, true, TimelineItemState::Pending, None, false),
                ],
            ),
        );
        s.timelines.insert(
            dm,
            timeline(
                dm,
                vec![item(0xa, true, TimelineItemState::Ok, Some(1), false)],
            ),
        );
        (s, c, ch)
    }

    /// builtin.ts `reply`, `edit`, `delete`, `pin` and `topic` (:116-152, :414-423): each checks the message and the
    /// channel's bits before it may run. Refused here, nothing is sent; an own message asks before it is deleted; a
    /// message already pinned answers without a send; a DM needs no bit to pin.
    #[test]
    fn message_builtins_check_before_running() {
        use crate::features::roles::perm;
        use crate::state::ChannelId;
        let (s, c, ch) = message_world(0);
        let (a, b, p) = (
            MessageId([0xa; 16]).hex(),
            MessageId([0xb; 16]).hex(),
            MessageId([0xc; 16]).hex(),
        );
        let failed =
            |text: &str| Plan::Private(ResultStatus::Failed, text.into(), ResultProvider::Builtin);
        let run = |s: &AppState, draft: &str| plan_in(s, Some(c), Some(ch), draft);
        assert_eq!(
            run(&s, &format!("/edit message:{b} text:x")),
            failed("You can edit only your own loaded message.")
        );
        assert_eq!(
            run(&s, &format!("/delete message:{b} reason:spam")),
            failed("Deleting another person’s message requires Manage Messages and a reason.")
        );
        assert_eq!(
            run(&s, &format!("/delete message:{a}")),
            Plan::Builtin {
                run: Builtin::DeleteOwn {
                    msg: MessageId([0xa; 16])
                },
                confirm: Some(Ask {
                    title: "Delete this message?".into(),
                    body: "This permanently deletes your message.".into(),
                }),
            }
        );
        assert_eq!(
            run(&s, &format!("/pin message:{a}")),
            failed("You cannot pin messages here.")
        );
        assert_eq!(
            run(&s, &format!("/reply message:{p} text:x")),
            failed("That message is not available in this conversation.")
        );
        assert_eq!(
            run(&s, "/topic text:x"),
            failed("You cannot manage this channel.")
        );
        assert_eq!(
            run(&s, &format!("/reply message:{a} text:hello")),
            Plan::Builtin {
                run: Builtin::Reply {
                    to: MessageId([0xa; 16]),
                    text: "hello".into()
                },
                confirm: None,
            }
        );

        let (s, c, ch) =
            message_world(perm::PIN_MESSAGES | perm::MANAGE_MESSAGES | perm::MANAGE_CHANNELS);
        let run = |draft: &str| plan_in(&s, Some(c), Some(ch), draft);
        assert_eq!(
            run(&format!("/pin message:{a}")),
            Plan::Private(
                ResultStatus::Success,
                "Message is already pinned.".into(),
                ResultProvider::Builtin
            )
        );
        assert_eq!(
            run("/topic text:x"),
            Plan::Builtin {
                run: Builtin::Topic {
                    channel: ch,
                    text: "x".into()
                },
                confirm: None,
            }
        );
        // Another person's message: a moderator with a reason removes it for everyone, after the question.
        assert_eq!(
            run(&format!("/delete message:{b}")),
            failed("Deleting another person’s message requires Manage Messages and a reason."),
            "no reason"
        );
        assert_eq!(
            run(&format!("/delete message:{b} reason:spam")),
            Plan::Builtin {
                run: Builtin::RemoveOther {
                    msg: MessageId([0xb; 16]),
                    seq: 2,
                    reason: "spam".into(),
                    excerpt: String::new(),
                },
                confirm: Some(Ask {
                    title: "Delete this message?".into(),
                    body: "Remove this message from everyone? Reason: spam".into(),
                }),
            }
        );
        // In a DM no bit is needed to pin (builtin.ts:417), and a topic is never set there.
        let dm = ChannelId([5; 16]);
        assert_eq!(
            plan_in(&s, None, Some(dm), &format!("/pin message:{a}")),
            Plan::Builtin {
                run: Builtin::Pin {
                    msg: MessageId([0xa; 16]),
                    on: true
                },
                confirm: None,
            }
        );
        assert_eq!(
            plan_in(&s, None, Some(dm), "/topic text:x"),
            failed("You cannot manage this channel.")
        );
        assert_eq!(
            plan_in(&s, None, Some(dm), &format!("/unpin message:{a}")),
            Plan::Private(
                ResultStatus::Success,
                "Message is already unpinned.".into(),
                ResultProvider::Builtin
            )
        );
        assert_eq!(
            plan_in(&s, None, Some(dm), &format!("/pin message:{b}")),
            failed("That message is not available in this channel.")
        );
    }

    /// builtin.ts:355-379 as data (Review Focus 1): after the one `remove_message`, the steps only resolve the same proof
    /// or send its evidence, each at most once; no step removes again (`RemovalStep` has no such variant).
    #[test]
    fn removal_steps_never_send_a_second_removal() {
        use crate::features::moderation::{Evidence, Removal, RemovalOutcome};
        const P: [u8; 16] = [7; 16];
        let hex = crate::state::hex(&P);
        let outcome = |removal, proof, evidence| RemovalOutcome {
            removal,
            proof_request_id: proof,
            evidence_request_id: Some([8; 16]),
            evidence,
        };
        let done = |status, text: String, uncertain| RemovalStep::Done {
            status,
            text,
            uncertain,
        };
        assert_eq!(
            removal_step(
                &outcome(Removal::Unknown, P, Evidence::Pending),
                false,
                false
            ),
            RemovalStep::Resolve(P)
        );
        assert_eq!(
            removal_step(
                &outcome(Removal::Unknown, P, Evidence::Pending),
                true,
                false
            ),
            done(
                ResultStatus::Failed,
                format!("Message removal is unresolved. Recovery proof: {hex}."),
                true
            )
        );
        for resolved in [false, true] {
            assert_eq!(
                removal_step(
                    &outcome(Removal::Rejected, P, Evidence::Pending),
                    resolved,
                    false
                ),
                done(
                    ResultStatus::Failed,
                    "The message could not be removed.".into(),
                    false
                )
            );
        }
        assert_eq!(
            removal_step(
                &outcome(Removal::Accepted, P, Evidence::Pending),
                false,
                false
            ),
            RemovalStep::SendEvidence(P)
        );
        assert_eq!(
            removal_step(
                &outcome(Removal::Accepted, P, Evidence::Pending),
                false,
                true
            ),
            done(
                ResultStatus::Failed,
                format!("Message removed; moderation evidence is pending. Recovery proof: {hex}."),
                true
            )
        );
        // builtin.ts:368: without the removal's evidence request id nothing is sent again; the evidence is reported.
        let no_request = RemovalOutcome {
            evidence_request_id: None,
            ..outcome(Removal::Accepted, P, Evidence::Unknown)
        };
        assert_eq!(
            removal_step(&no_request, false, false),
            done(
                ResultStatus::Failed,
                format!("Message removed; moderation evidence is unknown. Recovery proof: {hex}."),
                true
            )
        );
        match removal_step(
            &outcome(Removal::Accepted, P, Evidence::Rejected),
            true,
            true,
        ) {
            RemovalStep::Done {
                status: ResultStatus::Failed,
                text,
                uncertain: true,
            } => assert!(
                text.starts_with("Message removal was accepted; moderation evidence is rejected."),
                "{text}"
            ),
            other => panic!("{other:?}"),
        }
        for (resolved, sent) in [(false, false), (false, true), (true, false), (true, true)] {
            assert_eq!(
                removal_step(
                    &outcome(Removal::Accepted, P, Evidence::Accepted),
                    resolved,
                    sent
                ),
                done(
                    ResultStatus::Success,
                    "Message removed and moderation evidence recorded.".into(),
                    false
                )
            );
        }
        // The one removal's own failure (builtin.ts:344-349): a refusal is definitive, anything else uncertain.
        let api = |status, code: &str| crate::sync::api::ApiError {
            status,
            code: code.into(),
            detail: String::new(),
            retry_after_ms: None,
            extra: Vec::new(),
        };
        let refused =
            "The message could not be removed with the current permissions or message state.";
        let unsure =
            "Message removal outcome is uncertain. Check the conversation before retrying.";
        assert_eq!(removal_error(&api(0, "E_FORBIDDEN")), refused);
        assert_eq!(removal_error(&api(404, "E_HTTP")), refused);
        assert_eq!(removal_error(&api(0, "E_NETWORK")), unsure);
        assert_eq!(removal_error(&api(409, "E_CONFLICT")), unsure);
    }

    /// builtin.ts:337-376 carries the message's text as its evidence: a [`Builtin`] never prints a body, a reason or an
    /// excerpt (E2EE content stays out of logs and test output).
    #[test]
    fn a_builtin_never_prints_its_text() {
        let remove = Builtin::RemoveOther {
            msg: MessageId([1; 16]),
            seq: 3,
            reason: "secret reason".into(),
            excerpt: "secret excerpt".into(),
        };
        let reply = Builtin::Reply {
            to: MessageId([1; 16]),
            text: "secret body".into(),
        };
        // The question of a removal names its reason in its body; a plan prints neither.
        let plan = Plan::Builtin {
            run: remove.clone(),
            confirm: Some(Ask {
                title: "Delete this message?".into(),
                body: "Remove this message from everyone? Reason: secret reason".into(),
            }),
        };
        // A warning's reason is its question's body.
        let warn = Plan::Builtin {
            run: Builtin::Warn {
                community: CommunityId([7; 16]),
                user: crate::state::UserId([2; 16]),
                name: "bea".into(),
                reason: "secret reason".into(),
            },
            confirm: Some(Ask {
                title: "Warn bea?".into(),
                body: "secret reason".into(),
            }),
        };
        for shown in [
            format!("{remove:?}"),
            format!("{reply:?}"),
            format!("{warn:?}"),
            format!("{warn:#?}"),
            format!("{plan:?}"),
            format!(
                "{:#?}",
                RunOutcome::Builtin {
                    run: remove,
                    confirm: None
                }
            ),
        ] {
            assert!(!shown.contains("secret"), "{}", shown.len());
            // R20: nor the member a moderation command acts on.
            assert!(!shown.contains("bea"), "{}", shown.len());
        }
        // The question is still listed, by its lengths ("Delete this message?" is 20 bytes).
        assert!(format!("{plan:?}").contains("Ask(20 bytes"));
    }

    /// R20 round 1: every moderation built-in, and a moderation target, print neither the member's id nor their name
    /// (one case per variant, so re-adding either to any arm fails here).
    #[test]
    fn each_moderation_builtin_keeps_its_member_out_of_debug() {
        let (community, user) = (CommunityId([7; 16]), crate::state::UserId([2; 16]));
        let name = || "Bjornsson".to_owned();
        let cases = [
            (
                "Timeout",
                Builtin::Timeout {
                    community,
                    user,
                    name: name(),
                    seconds: 600,
                },
            ),
            (
                "Untimeout",
                Builtin::Untimeout {
                    community,
                    user,
                    name: name(),
                },
            ),
            (
                "UntimeoutConfirmed",
                Builtin::UntimeoutConfirmed {
                    community,
                    user,
                    name: name(),
                },
            ),
            (
                "Warn",
                Builtin::Warn {
                    community,
                    user,
                    name: name(),
                    reason: "secret-reason".into(),
                },
            ),
            (
                "Unban",
                Builtin::Unban {
                    community,
                    user,
                    name: name(),
                },
            ),
        ];
        let id = format!("{user:?}");
        for (variant, b) in cases {
            for shown in [format!("{b:?}"), format!("{b:#?}")] {
                assert!(shown.starts_with(variant), "{variant}: the variant shows");
                assert!(!shown.contains(&id), "{variant}: the member's id");
                assert!(!shown.contains("Bjornsson"), "{variant}: the member's name");
                assert!(!shown.contains("secret"), "{variant}: the reason");
            }
        }
        let target = Target {
            user,
            name: name(),
            username: "bjornsson".into(),
        };
        let shown = format!("{target:?}");
        assert!(!shown.contains(&id) && !shown.to_lowercase().contains("bjornsson"));
    }

    /// R21: posts, private rows, sealed Bot arguments and answers, polls, search hits and notifications print no decrypted
    /// text in Debug output (one value per redacted type).
    #[test]
    fn decrypted_texts_of_commands_and_notices_stay_out_of_debug() {
        const SECRET: &str = "zebra-body-7";
        let mut args = registry::Args::new();
        args.insert("text".into(), SECRET.into());
        let definition = registry::commands()[0].clone();
        let (channel, user) = (ChannelId([9; 16]), crate::state::UserId([2; 16]));
        let shown = [
            format!("{:?}", Plan::Post(SECRET.into())),
            format!(
                "{:?}",
                Plan::Private(ResultStatus::Success, SECRET.into(), ResultProvider::Bot)
            ),
            format!(
                "{:#?}",
                Plan::Bot {
                    definition: definition.clone(),
                    args: args.clone(),
                }
            ),
            format!("{:?}", RunOutcome::Post(SECRET.into())),
            format!("{:#?}", RunOutcome::Bot { definition, args }),
            format!(
                "{:?}",
                private::Patch {
                    status: None,
                    text: Some(SECRET.into()),
                    hold: None,
                }
            ),
            format!(
                "{:?}",
                sealed::Answer {
                    seq: 1,
                    status: 200,
                    body: SECRET.as_bytes().to_vec(),
                    final_: true,
                }
            ),
            format!(
                "{:?}",
                crate::state::bot_admin::WebhookLabel {
                    text: SECRET.into(),
                    webhook_id: "w".repeat(32),
                    name: "CI".into(),
                    avatar_id: None,
                }
            ),
            format!(
                "{:?}",
                RemovalStep::Done {
                    status: ResultStatus::Failed,
                    text: SECRET.into(),
                    uncertain: true,
                }
            ),
            format!(
                "{:?}",
                vec![
                    crate::gateway::frames::Value::Text(SECRET.into()),
                    crate::gateway::frames::Value::Bytes(SECRET.as_bytes().to_vec()),
                ]
            ),
            format!(
                "{:?}",
                vec![
                    crate::features::bot_admin::BotArg::Text(SECRET.into()),
                    crate::features::bot_admin::BotArg::Bytes(SECRET.as_bytes().to_vec()),
                ]
            ),
            format!(
                "{:?}",
                crate::features::bot_admin::SealedAnswer {
                    status: 200,
                    body: SECRET.as_bytes().to_vec(),
                }
            ),
            format!(
                "{:?}",
                crate::features::utilities::polls::PollCreate {
                    channel: [9; 16],
                    thread: None,
                    question: SECRET.into(),
                    answers: vec![SECRET.into()],
                    multi: false,
                    duration: 60,
                }
            ),
            format!(
                "{:?}",
                crate::features::utilities::polls::CardOption {
                    text: SECRET.into(),
                    pct: 0,
                    mine: false,
                    winner: false,
                    voters: Vec::new(),
                }
            ),
            format!(
                "{:?}",
                crate::features::search::RawHit {
                    group: [9; 16],
                    seq: 1,
                    snippet: SECRET.into(),
                    thread_root: None,
                }
            ),
            format!(
                "{:?}",
                crate::features::notify::Notice {
                    channel,
                    community: None,
                    kind: crate::features::notify::NoticeKind::Dm,
                    sender_user: Some(user),
                    sender_name: "Ada".into(),
                    body: SECRET.into(),
                    ts: 1,
                }
            ),
            format!(
                "{:?}",
                crate::features::notify::Notification {
                    kind: crate::features::notify::NotificationKind::Dm,
                    title: "Ada".into(),
                    body: SECRET.into(),
                    tag: "dilla:x".into(),
                    channel: Some(channel),
                }
            ),
        ];
        for s in shown {
            assert!(!s.contains("zebra"), "a decrypted text: {} bytes", s.len());
        }
    }

    /// R20: a checked kick or ban, its private row's copy and the row itself name neither the member nor the reason.
    #[test]
    fn moderation_rows_keep_the_member_and_reason_out_of_debug() {
        let ban = Moderation {
            community: CommunityId([7; 16]),
            user: crate::state::UserId([2; 16]),
            name: "Bjornsson".into(),
            username: "bjornsson".into(),
            action: ModAction::Ban {
                seconds: Some(3600),
                duration: "1h".into(),
                reason: "secret-reason".into(),
            },
        };
        let texts = ModerationTexts {
            cancelled: "Ban of Bjornsson cancelled.".into(),
            pending: "Banning Bjornsson…".into(),
            done: "Bjornsson was banned.".into(),
        };
        let row = crate::state::bot::PrivateResult {
            id: 1,
            at: 0,
            status: ResultStatus::Success,
            text: "Bjornsson was banned. secret-reason".into(),
            provider: ResultProvider::Builtin,
            hold: false,
            settled_at: None,
        };
        let user = format!("{:?}", crate::state::UserId([2; 16]));
        for shown in [
            format!("{ban:?}"),
            format!("{ban:#?}"),
            format!("{texts:?}"),
            format!("{row:?}"),
        ] {
            assert!(!shown.to_lowercase().contains("bjornsson"), "a name");
            assert!(!shown.contains("secret"), "a reason");
            assert!(!shown.contains(&user), "the member's id");
        }
    }

    /// builtin.ts `eligibleAssignableRole` (:384-393): a role is assignable when this account manages roles and ranks
    /// above it, it carries nothing this account lacks, and the member is neither this account nor the owner nor already
    /// holding it (revoke: holding it); @everyone never.
    #[test]
    fn assignable_role_follows_the_web_rules() {
        use crate::state::members::Member;
        use crate::state::roles::{CommunityAccess, RoleSummary};
        use crate::state::{Community, UserId};
        let c = CommunityId([7; 16]);
        let (me, bea, owner) = (UserId([1; 16]), UserId([2; 16]), UserId([3; 16]));
        let role = |n: u8, position: u32, allow: u32| RoleSummary {
            id: [n; 16],
            name: format!("r{n}"),
            color: 0,
            position,
            hoist: false,
            mentionable: false,
            allow,
            deny: 0,
            members: 0,
        };
        let member = |user_id, username: &str, role_ids: Vec<[u8; 16]>| Member {
            user_id,
            username: username.into(),
            display: String::new(),
            kind: 0,
            role_ids,
            status_msg: String::new(),
            bio: String::new(),
        };
        let mut s = AppState::default();
        s.account.user_id = Some(me);
        s.communities.push(Community {
            id: c,
            name: "Midgard".into(),
            owner,
            policy_version: 1,
        });
        // I hold role 10 (position 5, no bits); Bea holds role 13.
        s.members.insert(
            c,
            vec![
                member(me, "ada", vec![[10; 16]]),
                member(bea, "bea", vec![[13; 16]]),
                member(owner, "olaf", Vec::new()),
            ],
        );
        s.roles.roles.insert(
            c,
            vec![
                role(9, 0, 0),
                role(10, 5, 0),
                role(11, 5, 0),
                role(12, 2, 0x1 << 25),
                role(13, 2, 0),
                role(14, 3, 0),
            ],
        );
        s.roles.access.insert(
            c,
            CommunityAccess {
                top: 5,
                manage_roles: true,
                loaded: true,
                everyone_id: Some([9; 16]),
                ..CommunityAccess::default()
            },
        );
        let id = |n: u8| {
            [n; 16]
                .iter()
                .map(|b| format!("{b:02x}"))
                .collect::<String>()
        };
        let pick = |user: UserId, role: u8, add: bool| {
            assignable_role(&s, c, &user.hex(), &id(role), add).map(|(r, t)| (r.id, t.user))
        };
        assert_eq!(
            pick(bea, 14, true),
            Some(([14; 16], bea)),
            "a lower role not held"
        );
        assert_eq!(pick(bea, 14, false), None, "not held: nothing to remove");
        assert_eq!(pick(bea, 13, true), None, "already held");
        assert_eq!(
            pick(bea, 13, false),
            Some(([13; 16], bea)),
            "held: removable"
        );
        assert_eq!(pick(bea, 9, true), None, "@everyone");
        assert_eq!(pick(bea, 11, true), None, "at the caller's top");
        assert_eq!(pick(bea, 12, true), None, "carries a bit the caller lacks");
        assert_eq!(pick(me, 14, true), None, "oneself");
        assert_eq!(pick(owner, 14, true), None, "the owner");
        let mut no_manage = s.clone();
        no_manage.roles.access.get_mut(&c).unwrap().manage_roles = false;
        assert!(assignable_role(&no_manage, c, &bea.hex(), &id(14), true).is_none());
    }

    /// builtin.ts `mutationError` for a sync step: a definitive code is named, anything else is uncertain.
    #[test]
    fn mutation_error_of_names_definitive_codes() {
        use crate::ClientError;
        assert_eq!(
            mutation_error_of(&ClientError::Sync("E_FORBIDDEN".into())),
            "Command was refused (E_FORBIDDEN)."
        );
        assert_eq!(
            mutation_error_of(&ClientError::Network("down".into())),
            "The command result is uncertain. Check the current server state before retrying."
        );
    }

    /// Review HL item 15 (weak test), at the op 63 handler's level: three hints of one community's Bot inside 250 ms read
    /// its catalog twice (the first at once, one trailing read for the other two), as hints.test.ts expects of
    /// `BotHints`; an older revision reads nothing; another scope is its own.
    #[tokio::test(start_paused = true)]
    async fn three_op_63_hints_in_250_ms_read_the_bot_twice() {
        use std::sync::atomic::{AtomicUsize, Ordering};
        let gate: SharedGate = Arc::default();
        let reads = Arc::new(AtomicUsize::new(0));
        let spawn = |wait: std::pin::Pin<Box<dyn std::future::Future<Output = ()> + Send>>| {
            drop(tokio::spawn(wait));
        };
        let state = AppState::default();
        let hint = |id: u8, revision: u64| frames::BotStateChangedFrame {
            scope_kind: 0,
            scope_id: [id; 16],
            revision,
        };
        let deliver = |frame: frames::BotStateChangedFrame| {
            let r = reads.clone();
            on_state_changed(
                &gate,
                &frame,
                &state,
                &spawn,
                move |c| {
                    assert_eq!(c, CommunityId([1; 16]));
                    r.fetch_add(1, Ordering::SeqCst);
                },
                |_| panic!("no call: no player read"),
            );
        };
        deliver(hint(1, 1));
        deliver(hint(1, 2));
        deliver(hint(1, 3));
        deliver(hint(1, 2));
        assert_eq!(reads.load(Ordering::SeqCst), 1, "the first at once");
        tokio::time::sleep(hints::HINT_COALESCE * 2).await;
        tokio::task::yield_now().await;
        assert_eq!(
            reads.load(Ordering::SeqCst),
            2,
            "one trailing read for the rest"
        );
    }

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
        // A watched channel follows its hint without a call.
        let mut s = AppState::default();
        s.bot_admin.music_watched.insert(lounge);
        assert_eq!(music_hint(&s, &hint(2, lounge.0)), Some(lounge));
        assert_eq!(music_hint(&s, &hint(1, lounge.0)), None);
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

    /// builtin.ts `kick`/`ban`: a joined member this account outranks, named by user id, is a moderation to confirm (a
    /// name is never resolved, w2-bot-review I-B1: two members may share one); without the permission, against the
    /// owner, oneself or an unknown id it fails privately; a ban checks its length and reason first.
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
            username: "bea".into(),
            action: ModAction::Kick,
        });
        let (bea_id, me_id) = (bea.hex(), me.hex());
        assert_eq!(plan(&s, Some(c), &format!("/kick user:{bea_id}")), kick);
        let refused =
            |text: &str| Plan::Private(ResultStatus::Failed, text.into(), ResultProvider::Builtin);
        let cannot_kick =
            refused("This member cannot be kicked with your current permissions and rank.");
        assert_eq!(
            plan(&s, Some(c), &format!("/kick user:{me_id}")),
            cannot_kick,
            "oneself"
        );
        assert_eq!(plan(&s, Some(c), "/kick user:nobody"), cannot_kick);
        // w2-bot-review I-B1: a name is never resolved, not even a unique one; the picker's member chip puts the id.
        assert_eq!(plan(&s, Some(c), "/kick user:bea"), cannot_kick);
        assert_eq!(plan(&s, Some(c), "/kick user:@bea"), cannot_kick);
        let bea_id = bea_id.as_str();
        match plan(
            &s,
            Some(c),
            &format!("/ban user:{bea_id} duration:2d reason:spam"),
        ) {
            Plan::Moderate(Moderation {
                action: ModAction::Ban {
                    seconds, reason, ..
                },
                ..
            }) => assert_eq!((seconds, reason.as_str()), (Some(172_800), "spam")),
            other => panic!("{other:?}"),
        }
        assert!(matches!(
            plan(
                &s,
                Some(c),
                &format!("/ban user:{bea_id} duration:permanent")
            ),
            Plan::Moderate(Moderation {
                action: ModAction::Ban { seconds: None, .. },
                ..
            })
        ));
        assert_eq!(
            plan(&s, Some(c), &format!("/ban user:{bea_id} duration:10m")),
            refused("Ban duration must be from 1h to 100y.")
        );
        // Not the owner and without Kick Members: nobody can be kicked.
        s.communities[0].owner = owner;
        access.owner = false;
        access.kick_members = false;
        s.roles.access.insert(c, access);
        assert_eq!(
            plan(&s, Some(c), &format!("/kick user:{bea_id}")),
            cannot_kick
        );
    }

    /// builtin.ts `timeout`, `warn` and `unban` (:168-182, :204-264): the member, the length, the reason and the ban are
    /// checked before anything is asked; `/timeout` and `/warn` need Moderate Members, `/unban` Ban Members and a ban this
    /// device has read.
    #[test]
    fn moderation_builtins_check_before_running() {
        use crate::state::members::{BanSummary, Member};
        use crate::state::{Community, UserId};
        let c = CommunityId([7; 16]);
        let (me, bea) = (UserId([1; 16]), UserId([2; 16]));
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
        s.members
            .insert(c, vec![member(me, "ada"), member(bea, "bea")]);
        let mut access = s.roles.access_of(c);
        access.loaded = true;
        access.owner = true;
        s.roles.access.insert(c, access.clone());
        let refused =
            |text: &str| Plan::Private(ResultStatus::Failed, text.into(), ResultProvider::Builtin);
        let bea_id = bea.hex();
        let timeout = |d: &str| format!("/timeout user:{bea_id} duration:{d}");
        assert_eq!(
            plan(&s, Some(c), &timeout("10m")),
            refused("This member cannot be timed out with your current permissions and rank."),
            "no Moderate Members"
        );
        assert_eq!(
            plan(&s, Some(c), &format!("/untimeout user:{bea_id}")),
            refused("This member cannot be timed out with your current permissions and rank.")
        );
        assert_eq!(
            plan(&s, Some(c), &format!("/warn user:{bea_id} reason:spam")),
            refused("This member cannot be warned with your current permissions and rank.")
        );
        access.moderate_members = true;
        s.roles.access.insert(c, access.clone());
        assert_eq!(
            plan(&s, Some(c), &timeout("30s")),
            refused("Timeout duration must be from 1m to 28d.")
        );
        assert_eq!(
            plan(&s, Some(c), &timeout("29d")),
            refused("Timeout duration must be from 1m to 28d.")
        );
        assert_eq!(
            plan(&s, Some(c), &timeout("10m")),
            Plan::Builtin {
                run: Builtin::Timeout {
                    community: c,
                    user: bea,
                    name: "bea".into(),
                    seconds: 600,
                },
                confirm: Some(Ask {
                    title: "Timeout bea?".into(),
                    body: "Restrict this member for 10m.".into(),
                }),
            }
        );
        // The question comes once the timeout is read (builtins::run): none here.
        assert_eq!(
            plan(&s, Some(c), &format!("/untimeout user:{bea_id}")),
            Plan::Builtin {
                run: Builtin::Untimeout {
                    community: c,
                    user: bea,
                    name: "bea".into(),
                },
                confirm: None,
            }
        );
        let bad_reason =
            refused("Enter a reason of at most 512 UTF-8 bytes without control characters.");
        assert_eq!(
            plan(
                &s,
                Some(c),
                &format!("/warn user:{bea_id} reason:\"a\u{1}b\"")
            ),
            bad_reason
        );
        assert_eq!(
            plan(
                &s,
                Some(c),
                &format!("/warn user:{bea_id} reason:{}", "x".repeat(513))
            ),
            bad_reason
        );
        assert_eq!(
            plan(
                &s,
                Some(c),
                &format!("/warn user:{bea_id} reason:\" spam \"")
            ),
            Plan::Builtin {
                run: Builtin::Warn {
                    community: c,
                    user: bea,
                    name: "bea".into(),
                    reason: "spam".into(),
                },
                confirm: Some(Ask {
                    title: "Warn bea?".into(),
                    body: "spam".into(),
                }),
            }
        );

        let unban = format!("/unban user:{bea_id}");
        assert_eq!(
            plan(&s, Some(c), &unban),
            refused("You cannot unban members here."),
            "no Ban Members"
        );
        access.ban_members = true;
        s.roles.access.insert(c, access);
        assert_eq!(
            plan(&s, Some(c), &unban),
            refused("That member is not currently banned."),
            "no bans read"
        );
        s.people.bans.insert(
            c,
            vec![BanSummary {
                user_id: bea,
                name: "Bea B".into(),
                reason: String::new(),
                by_user: me,
                created: 0,
                expires: None,
            }],
        );
        assert_eq!(
            plan(&s, Some(c), &unban),
            Plan::Builtin {
                run: Builtin::Unban {
                    community: c,
                    user: bea,
                    name: "Bea B".into(),
                },
                confirm: Some(Ask {
                    title: "Unban Bea B?".into(),
                    body: "This restores the member’s ability to join the community.".into(),
                }),
            }
        );
        assert_eq!(
            plan(&s, Some(c), &format!("/unban user:{}", me.hex())),
            refused("That member is not currently banned.")
        );
        // Not CommandHost's `moderation` (only `/kick` and `/ban`, CommandHost.tsx:233): a Cancel dismisses the row (:291).
        for run in [
            Builtin::Timeout {
                community: c,
                user: bea,
                name: String::new(),
                seconds: 60,
            },
            Builtin::Untimeout {
                community: c,
                user: bea,
                name: String::new(),
            },
            Builtin::UntimeoutConfirmed {
                community: c,
                user: bea,
                name: String::new(),
            },
            Builtin::Warn {
                community: c,
                user: bea,
                name: String::new(),
                reason: String::new(),
            },
            Builtin::Unban {
                community: c,
                user: bea,
                name: String::new(),
            },
        ] {
            assert!(run.dismissed_on_cancel(), "{run:?}");
        }
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

    /// builtin.test.ts:501 (`has a real handler registered for every built-in catalog entry`): in a state where this
    /// account may do anything (the community's owner, every permission loaded), each built-in of the registry, given
    /// valid arguments, parses and is answered by its own arm, never by the "unavailable" catch-all. `PENDING` lists the
    /// built-ins that still answer it (the navigation, UI-opening and voice commands, whose areas land next); the list is
    /// exact, so giving one of them its arm fails here until it leaves the list, and the catch-all goes with the last.
    #[test]
    fn every_builtin_has_a_handler() {
        use super::registry::ArgKind;
        use crate::state::MessageId;
        const PENDING: [&str; 12] = [
            "giphy", "dm", "invite", "search", "thread", "join", "leave", "mute", "unmute",
            "deafen", "undeafen", "modlog",
        ];
        let (mut s, c, ch) = message_world(u32::MAX);
        let ada = s.account.user_id.unwrap();
        s.communities[0].owner = ada;
        s.roles.access.insert(
            c,
            crate::state::roles::CommunityAccess {
                top: u32::MAX,
                owner: true,
                administrator: true,
                manage_community: true,
                view_audit_log: true,
                manage_channels: true,
                kick_members: true,
                ban_members: true,
                moderate_members: true,
                manage_messages: true,
                create_invite: true,
                manage_roles: true,
                loaded: true,
                everyone_id: Some([3; 16]),
            },
        );
        let bea = crate::state::UserId([6; 16]).hex();
        let mut unavailable = Vec::new();
        for command in registry::commands() {
            let mut draft = format!("/{}", command.id);
            for option in command.options.iter().filter(|o| o.required) {
                let value = match (option.kind, option.name.as_str()) {
                    (ArgKind::User, _) => bea.clone(),
                    (ArgKind::Role, _) => crate::features::server_admin::hex_id(&[3; 16]),
                    (ArgKind::Message, _) => MessageId([0xb; 16]).hex(),
                    (ArgKind::Channel, _) => ch.hex(),
                    (ArgKind::Duration, _) => "1h".to_owned(),
                    (_, "language") => "rust".to_owned(),
                    _ => "x".to_owned(),
                };
                draft.push_str(&format!(" {}:{value}", option.name));
            }
            match plan_in(&s, Some(c), Some(ch), &draft) {
                Plan::Incomplete => panic!("{draft} does not parse"),
                Plan::Private(_, text, _) if text == UNAVAILABLE => {
                    unavailable.push(command.id.as_str());
                }
                _ => {}
            }
        }
        assert_eq!(unavailable, PENDING);
    }

    /// builtin.ts `updateMemberRole` (:395-404): a role is granted or revoked only where [`assignable_role`] allows it;
    /// removing one asks first, adding one does not.
    #[test]
    fn role_builtins_check_before_running() {
        use crate::state::members::Member;
        use crate::state::roles::{CommunityAccess, RoleSummary};
        use crate::state::{Community, UserId};
        let c = CommunityId([7; 16]);
        let (me, bea) = (UserId([1; 16]), UserId([2; 16]));
        let (everyone, mine, crew, guest) = ([9; 16], [10; 16], [13; 16], [14; 16]);
        let role = |id: [u8; 16], name: &str, position: u32| RoleSummary {
            id,
            name: name.into(),
            color: 0,
            position,
            hoist: false,
            mentionable: false,
            allow: 0,
            deny: 0,
            members: 0,
        };
        let member = |user_id, username: &str, role_ids: Vec<[u8; 16]>| Member {
            user_id,
            username: username.into(),
            display: String::new(),
            kind: 0,
            role_ids,
            status_msg: String::new(),
            bio: String::new(),
        };
        let mut s = AppState::default();
        s.account.user_id = Some(me);
        s.communities.push(Community {
            id: c,
            name: "Midgard".into(),
            owner: UserId([3; 16]),
            policy_version: 1,
        });
        s.members.insert(
            c,
            vec![
                member(me, "ada", vec![mine]),
                member(bea, "bea", vec![crew]),
            ],
        );
        s.roles.roles.insert(
            c,
            vec![
                role(everyone, "@everyone", 0),
                role(mine, "Mods", 5),
                role(crew, "Crew", 2),
                role(guest, "Guest", 1),
            ],
        );
        s.roles.access.insert(
            c,
            CommunityAccess {
                top: 5,
                manage_roles: true,
                loaded: true,
                everyone_id: Some(everyone),
                ..CommunityAccess::default()
            },
        );
        let hex = crate::features::server_admin::hex_id;
        let draft =
            |verb: &str, r: &[u8; 16]| format!("/role {verb} user:{} role:{}", bea.hex(), hex(r));
        let refused = Plan::Private(
            ResultStatus::Failed,
            "This member or role is not assignable with your current permissions and rank.".into(),
            ResultProvider::Builtin,
        );
        assert_eq!(
            plan(&s, Some(c), &draft("add", &everyone)),
            refused,
            "@everyone"
        );
        assert_eq!(
            plan(&s, Some(c), &draft("add", &crew)),
            refused,
            "already held"
        );
        assert_eq!(
            plan(&s, Some(c), &draft("remove", &guest)),
            refused,
            "not held"
        );
        assert_eq!(
            plan(&s, Some(c), &draft("remove", &crew)),
            Plan::Builtin {
                run: Builtin::Role {
                    community: c,
                    user: bea,
                    role: crew,
                    add: false,
                    role_name: "Crew".into(),
                    member_name: "bea".into(),
                },
                confirm: Some(Ask {
                    title: "Remove Crew from bea?".into(),
                    body: "This changes the member’s roles.".into(),
                }),
            }
        );
        assert_eq!(
            plan(&s, Some(c), &draft("add", &guest)),
            Plan::Builtin {
                run: Builtin::Role {
                    community: c,
                    user: bea,
                    role: guest,
                    add: true,
                    role_name: "Guest".into(),
                    member_name: "bea".into(),
                },
                confirm: None,
            }
        );
        // The self roles are read as the command runs: planning checks nothing but the community.
        assert_eq!(
            plan(&s, Some(c), "/selfrole list"),
            Plan::Builtin {
                run: Builtin::SelfRoleList { community: c },
                confirm: None,
            }
        );
        assert_eq!(
            plan(
                &s,
                Some(c),
                &format!("/selfrole remove role:{}", hex(&crew))
            ),
            Plan::Builtin {
                run: Builtin::SelfRole {
                    community: c,
                    role: hex(&crew),
                    add: false,
                },
                confirm: None,
            }
        );
    }

    /// R20/R21: a role command names neither the member nor the role it acts on in its Debug, nor do the self-role
    /// commands name their role (one case per variant).
    #[test]
    fn role_builtins_keep_the_member_and_role_out_of_debug() {
        let (community, user) = (CommunityId([7; 16]), crate::state::UserId([2; 16]));
        let role = [0xab; 16];
        let cases = [
            (
                "Role",
                Builtin::Role {
                    community,
                    user,
                    role,
                    add: true,
                    role_name: "Crewmates".into(),
                    member_name: "Bjornsson".into(),
                },
            ),
            (
                "SelfRole",
                Builtin::SelfRole {
                    community,
                    role: crate::features::server_admin::hex_id(&role),
                    add: true,
                },
            ),
            ("SelfRoleList", Builtin::SelfRoleList { community }),
        ];
        let id = format!("{user:?}");
        for (variant, b) in cases {
            for shown in [format!("{b:?}"), format!("{b:#?}")] {
                assert!(shown.starts_with(variant), "{variant}: the variant shows");
                assert!(!shown.contains(&id), "{variant}: the member's id");
                assert!(!shown.contains("Bjornsson"), "{variant}: the member's name");
                assert!(!shown.contains("Crewmates"), "{variant}: the role's name");
                assert!(!shown.contains("ab"), "{variant}: the role's id");
                assert!(!shown.contains("171"), "{variant}: the role's id bytes");
            }
            assert!(b.dismissed_on_cancel(), "{variant}");
        }
    }
}
