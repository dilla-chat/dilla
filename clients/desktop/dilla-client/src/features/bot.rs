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
//!   finished row goes away [`PRIVATE_RESULT_TTL_MS`] after it finished; the area sweeps itself on a timer.

pub mod catalog;
pub mod private;
pub mod registry;

use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use crate::client::{Client, Inner};
use crate::gateway::frames::{self, Frame, Op};
use crate::state::bot::{
    BotCatalogView, BotState, BotStatus, BotStatusState, Conversation, PRIVATE_RESULT_TTL_MS,
    ResultProvider, ResultStatus,
};
use crate::state::{AppState, CommunityId};
use crate::sync::route::AreaFrames;

use registry::{CommandDefinition, Parsed, Provider};

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

/// What running a draft did.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum RunOutcome {
    /// Post this text in the conversation (a literal draft, or a built-in that posts); the draft is cleared.
    Post(String),
    /// A private result row was added (its id); the draft is cleared.
    Private(u64),
    /// The command is not complete: the draft stays and the picker shows why.
    Incomplete,
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
                // Sealed Bot commands answer through the Bot (op 62); until this device runs them, it says so privately.
                return Plan::Private(
                    ResultStatus::Failed,
                    UNAVAILABLE.to_owned(),
                    ResultProvider::Bot,
                );
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
        }
    }

    /// Adds a private result row and schedules its dismissal once it is finished.
    pub fn add_result(
        &self,
        conversation: Conversation,
        status: ResultStatus,
        text: String,
        provider: ResultProvider,
    ) -> u64 {
        let id = NEXT_RESULT.fetch_add(1, Ordering::Relaxed);
        let now = now_ms();
        self.inner.publish(move |s| {
            private::add(&mut s.bot, conversation, status, text, provider, id, now);
        });
        if status != ResultStatus::Pending {
            self.schedule_sweep();
        }
        id
    }

    /// The Dismiss button.
    pub fn dismiss(&self, conversation: Conversation, id: u64) {
        self.inner.publish(move |s| {
            private::dismiss(&mut s.bot, &conversation, id);
        });
    }

    /// Removes every private result whose time is up at `now_ms` (unix ms). The area calls it on its own timer.
    pub fn sweep_at(&self, now_ms: u64) {
        self.inner.publish(move |s| {
            private::sweep(&mut s.bot, now_ms);
        });
    }

    fn schedule_sweep(&self) {
        let bot = self.clone();
        // A spawned task that outlives a stopped client only publishes into a closed channel.
        // The answer is not awaited: the sweep publishes what it removes.
        drop(self.inner.spawn(async move {
            tokio::time::sleep(Duration::from_millis(PRIVATE_RESULT_TTL_MS)).await;
            bot.sweep_at(now_ms());
            Ok(())
        }));
    }

    /// Reads `community`'s catalog from the instance (`GET /v1/communities/{community}/bot/catalog`). A Bot with a
    /// published command key stays `Unverified` on this device until the key is checked against the Bot's device list.
    pub async fn sync_catalog(
        &self,
        community: CommunityId,
    ) -> Result<(), crate::sync::api::ApiError> {
        let route = format!("/v1/communities/{}/bot/catalog", community.hex());
        let answer = self
            .inner
            .socket_requests()
            .request("GET", route, Vec::new(), None)
            .await?;
        if answer.status == 404 {
            self.inner.publish(move |s| {
                s.bot.catalogs.remove(&community);
            });
            return Ok(());
        }
        let decoded = catalog::decode_catalog_response(&answer.body).map_err(|e| {
            crate::sync::api::ApiError::local(answer.status, "E_BOT_CATALOG", &e.to_string())
        })?;
        self.inner.publish(move |s| {
            let previous = s.bot.catalogs.get(&community);
            let status =
                if decoded.publication.is_some() && decoded.status.state == BotStatusState::Ready {
                    BotStatus {
                        state: BotStatusState::Unverified,
                        since: decoded.status.since,
                    }
                } else {
                    decoded.status
                };
            let view = BotCatalogView {
                revision: decoded.revision,
                commands: decoded.commands,
                status,
                bot_channels: previous.and_then(|p| p.bot_channels.clone()),
                bot_user: previous.and_then(|p| p.bot_user),
                bot_name: previous.map(|p| p.bot_name.clone()).unwrap_or_default(),
            };
            s.bot.catalogs.insert(community, view);
        });
        Ok(())
    }
}

/// Applies an op 61 catalog to `state`: the newer revision replaces the held commands; the status stays.
pub fn apply_catalog_frame(state: &mut BotState, f: &frames::BotCatalogFrame) {
    let Ok(commands) = catalog::decode_commands(&f.commands) else {
        return;
    };
    let community = CommunityId(f.community_id);
    let entry = state
        .catalogs
        .entry(community)
        .or_insert_with(|| BotCatalogView {
            revision: 0,
            commands: Vec::new(),
            status: BotStatus {
                state: BotStatusState::Unverified,
                since: None,
            },
            bot_channels: None,
            bot_user: None,
            bot_name: String::new(),
        });
    if f.revision < entry.revision {
        return;
    }
    entry.revision = f.revision;
    entry.commands = commands;
}

impl AreaFrames for Bot {
    fn on_frame(inner: &Arc<Inner>, f: &Frame) {
        match f.op {
            Op::BOT_CATALOG => {
                if let Ok(frame) = frames::bot_catalog_of(f) {
                    inner.publish(move |s| apply_catalog_frame(&mut s.bot, &frame));
                }
            }
            Op::BOT_STATE_CHANGED => {
                // A community's Bot changed state (scope kind 0): read its catalog and status again.
                if let Ok(frame) = frames::bot_state_changed_of(f)
                    && frame.scope_kind == 0
                {
                    let bot = Bot {
                        inner: inner.clone(),
                    };
                    drop(inner.spawn(async move {
                        let _ = bot.sync_catalog(CommunityId(frame.scope_id)).await;
                        Ok(())
                    }));
                }
            }
            // op 62 answers a sealed command this device sent; it holds no reply secret until it seals one.
            _ => {}
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gateway::frames::Value;
    use crate::state::bot::BotCommandSpec;

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
        apply_catalog_frame(
            &mut s.bot,
            &frames::BotCatalogFrame {
                community_id: c.0,
                revision: 3,
                commands: vec![remind],
                publication: None,
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
    fn catalog_commands_parse_only_in_their_community() {
        let (s, c) = state_with_catalog();
        assert!(matches!(
            plan(&s, Some(c), "/remind 1h stretch"),
            Plan::Private(_, _, ResultProvider::Bot)
        ));
        assert_eq!(
            plan(&s, None, "/remind 1h stretch"),
            Plan::Private(
                ResultStatus::Failed,
                not_a_command("remind"),
                ResultProvider::Builtin
            )
        );
    }

    #[test]
    fn an_older_catalog_frame_is_ignored() {
        let (mut s, c) = state_with_catalog();
        apply_catalog_frame(
            &mut s.bot,
            &frames::BotCatalogFrame {
                community_id: c.0,
                revision: 2,
                commands: vec![],
                publication: None,
            },
        );
        let view = &s.bot.catalogs[&c];
        assert_eq!(view.revision, 3);
        assert_eq!(
            view.commands
                .iter()
                .map(|c: &BotCommandSpec| c.name.as_str())
                .collect::<Vec<_>>(),
            ["remind"]
        );
    }
}
