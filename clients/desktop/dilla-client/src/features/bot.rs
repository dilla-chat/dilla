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
    Incomplete,
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
        }
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
static HINTS: Mutex<Vec<(Weak<Inner>, Arc<Mutex<hints::HintGate>>)>> = Mutex::new(Vec::new());

fn hints_for(inner: &Arc<Inner>) -> Arc<Mutex<hints::HintGate>> {
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
