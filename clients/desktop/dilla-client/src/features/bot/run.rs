//! A Bot command from the picker, run as one sealed command (packages/web/src/screens/commands/bot-run.ts): its
//! arguments as the catalog's shapes say, its answers streamed into a private result row, an unanswered run held with a
//! Retry that reuses the request id. The copy is the caller's ([`Texts`]); nothing here holds a string the UI shows.

use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use super::private::Patch;
use super::registry::{ArgKind, Args, CommandDefinition};
use super::sealed::{Answer, RunEnd, Submit};
use super::{Bot, now_ms, private};
use crate::features::bot_admin::{BotArg, new_request_id};
use crate::features::utilities::time::{ReminderTime, ReminderZone, parse_reminder_time};
use crate::state::bot::{
    BotCatalogView, BotStatusState, Conversation, ResultProvider, ResultStatus,
};
use crate::state::{AppState, ChannelId, CommunityId};

/// What a private result row says, in the caller's copy (bot-run.ts and copy.ts).
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Text {
    /// `privateResultCopy.pending(id)`: "Saving your reminder…".
    Pending(String),
    /// `commands.botNoAnswer`.
    NoAnswer,
    /// `privateResult.failed`.
    Failed,
    /// `commands.botForbidden`.
    Forbidden,
    /// `commands.botDisabled`.
    Disabled,
    /// `commands.botOffline` / `commands.botDegraded`, with the state's since (unix seconds).
    Offline(Option<u64>),
    Degraded(Option<u64>),
    /// `commands.botNotOnInstance`.
    Absent,
    /// `commands.needsBotChannel`.
    NeedsBotChannel,
    /// `commands.botCantCheckDevice`.
    CantCheckDevice,
    /// `commands.botUnverified`.
    Unverified,
    /// `commands.botChanged`.
    Changed,
    /// `commands.postedIn`.
    PostedIn {
        what: String,
        channel: String,
    },
    /// An argument problem (botArgsOf's messages; `WhenHint` is copy.ts `whenHint`).
    WholeNumber(String),
    ChooseFrom(String),
    Duration(String),
    WhenHint,
}

/// The caller's copy.
pub type Texts = Arc<dyn Fn(&Text) -> String + Send + Sync>;

/// The local zone a `when` is read in.
pub struct Zone {
    pub name: String,
    pub offset_at: Arc<dyn Fn(i64) -> i32 + Send + Sync>,
}

/// `botCommandReason`: why a Bot command is greyed, or `None` when it can run.
pub fn reason(view: Option<&BotCatalogView>, definition: &CommandDefinition) -> Option<Text> {
    let (view, bot) = (view?, definition.bot.as_ref()?);
    Some(match view.status.state {
        BotStatusState::Degraded => Text::Degraded(view.status.since),
        BotStatusState::Offline => Text::Offline(view.status.since),
        BotStatusState::Disabled => Text::Disabled,
        BotStatusState::Absent => Text::Absent,
        BotStatusState::Unverified => Text::Unverified,
        BotStatusState::Changed => Text::Changed,
        BotStatusState::Ready => {
            if bot.needs_bot_channel && view.bot_channels.as_ref().is_none_or(Vec::is_empty) {
                Text::NeedsBotChannel
            } else {
                return None;
            }
        }
    })
}

/// `registryDuration` + `durationSeconds`: `10m`, `1h30m` or a spoken `10 minutes`, as whole seconds.
pub fn duration_seconds(value: &str) -> Option<u64> {
    let text = value.trim().to_lowercase();
    if let Some(ms) = super::registry::duration_millis(&text) {
        let s = (ms / 1000.0).round();
        return (0.0..9.0e15).contains(&s).then_some(s as u64);
    }
    let digits: String = text.chars().take_while(char::is_ascii_digit).collect();
    let unit = text[digits.len()..].trim();
    let n: u64 = digits.parse().ok()?;
    let scale = match unit {
        "s" | "sec" | "secs" | "second" | "seconds" => 1,
        "m" | "min" | "mins" | "minute" | "minutes" => 60,
        "h" | "hr" | "hrs" | "hour" | "hours" => 3600,
        "d" | "day" | "days" => 86_400,
        "w" | "wk" | "wks" | "week" | "weeks" => 604_800,
        _ => return None,
    };
    n.checked_mul(scale)
}

/// `botArgsOf`: the sealed arguments in the catalog's order, or the problem to show.
pub fn bot_args(
    definition: &CommandDefinition,
    args: &Args,
    zone: &Zone,
    now: i64,
) -> Result<Vec<BotArg>, Text> {
    let types = definition
        .bot
        .as_ref()
        .map(|b| b.arg_types.clone())
        .unwrap_or_default();
    let mut out = Vec::new();
    for (i, option) in definition.options.iter().enumerate() {
        let Some(value) = args.get(&option.name).filter(|v| !v.trim().is_empty()) else {
            out.push(BotArg::Null);
            continue;
        };
        use crate::state::bot::ArgType as T;
        out.push(match types.get(i) {
            Some(T::Uint) => {
                let v = value.trim();
                match v.parse::<u64>() {
                    Ok(n) if v.bytes().all(|b| b.is_ascii_digit()) && n < (1 << 53) => {
                        BotArg::Uint(n)
                    }
                    _ => return Err(Text::WholeNumber(option.name.clone())),
                }
            }
            Some(T::User | T::Channel) => match unhex16(value) {
                Some(id) => BotArg::Bytes(id.to_vec()),
                None => return Err(Text::ChooseFrom(option.name.clone())),
            },
            Some(T::Duration) if option.kind == ArgKind::Time => {
                let z = ReminderZone {
                    name: &zone.name,
                    offset_at: &*zone.offset_at,
                };
                let seconds = match parse_reminder_time(value, z, now) {
                    ReminderTime::Relative { seconds, .. } => seconds,
                    ReminderTime::Absolute { unix_seconds, .. } => unix_seconds - now,
                    _ => return Err(Text::WhenHint),
                };
                if seconds < 0 {
                    return Err(Text::WhenHint);
                }
                BotArg::Uint(seconds as u64)
            }
            Some(T::Duration) => match duration_seconds(value) {
                Some(s) => BotArg::Uint(s),
                None => return Err(Text::Duration(option.name.clone())),
            },
            Some(T::Bool) => BotArg::Uint(u64::from(value == "on")),
            _ => BotArg::Text(value.clone()),
        });
    }
    Ok(out)
}

/// The text of an answer body (UTF-8), or `None` when empty or not text.
fn answer_text(body: &[u8]) -> Option<String> {
    (!body.is_empty())
        .then(|| String::from_utf8(body.to_vec()).ok())
        .flatten()
}

fn is_code(text: &str) -> bool {
    text.len() > 2
        && text.starts_with("E_")
        && text[2..]
            .bytes()
            .all(|b| b.is_ascii_uppercase() || b == b'_')
}

/// `codeText`: a refusal code as the result row says it.
fn code_text(code: &str, view: Option<&BotCatalogView>) -> Option<Text> {
    Some(match code {
        "E_BOT_OFFLINE" => Text::Offline(view.and_then(|v| v.status.since)),
        "E_NOT_BOT_CHANNEL" => Text::NeedsBotChannel,
        "E_BOT_SIGNATURE" => Text::CantCheckDevice,
        "E_BOT_KEY_STALE" | "E_BOT_STALE" => Text::Unverified,
        _ => return None,
    })
}

/// `answerResult`: a final answer as the row's status and text.
pub fn answer_result(
    answer: &Answer,
    view: Option<&BotCatalogView>,
) -> (ResultStatus, Option<Text>, Option<String>) {
    let text = answer_text(&answer.body);
    if answer.status < 400 {
        return (ResultStatus::Success, None, text);
    }
    let coded = text.as_deref().filter(|t| is_code(t)).and_then(|t| {
        code_text(t, view).or_else(|| (t == "E_FORBIDDEN").then_some(Text::Forbidden))
    });
    match (coded, text) {
        (Some(c), _) => (ResultStatus::Failed, Some(c), None),
        (None, Some(t)) if !is_code(&t) => (ResultStatus::Failed, None, Some(t)),
        _ if answer.status == 403 => (ResultStatus::Failed, Some(Text::Forbidden), None),
        _ => (ResultStatus::Failed, Some(Text::Failed), None),
    }
}

/// What a held (unanswered) row needs for its Retry.
struct Held {
    bot: Bot,
    conversation: Conversation,
    submit: Submit,
    definition: CommandDefinition,
    args: Args,
    texts: Texts,
}

static HELD: Mutex<Option<HashMap<u64, Held>>> = Mutex::new(None);

fn held() -> std::sync::MutexGuard<'static, Option<HashMap<u64, Held>>> {
    HELD.lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

impl Bot {
    fn patch(&self, conversation: Conversation, id: u64, patch: Patch) {
        let now = now_ms();
        self.inner.publish(move |s| {
            private::update(&mut s.bot, &conversation, id, patch, now);
        });
    }

    /// Runs a picked Bot command in `conversation` (a channel of `community`): a pending private row at once, its
    /// answers as they come, and its end. Answers the row's id. A row with no answer is held with a Retry
    /// ([`Bot::retry`]), which sends the same request id again.
    #[allow(clippy::too_many_arguments)]
    pub fn run_command(
        &self,
        conversation: Conversation,
        community: CommunityId,
        channel: Option<ChannelId>,
        definition: CommandDefinition,
        args: Args,
        zone: &Zone,
        texts: Texts,
    ) -> u64 {
        let state = self.inner.snapshot();
        let view = state.bot.catalogs.get(&community);
        let fail = |text: Text| {
            self.add_result(
                conversation,
                ResultStatus::Failed,
                texts(&text),
                ResultProvider::Bot,
            )
        };
        if let Some(r) = reason(view, &definition) {
            return fail(r);
        }
        let Some(bot) = definition.bot.clone() else {
            return fail(Text::Failed);
        };
        let now = i64::try_from(now_ms() / 1000).unwrap_or(i64::MAX);
        let sealed_args = match bot_args(&definition, &args, zone, now) {
            Ok(a) => a,
            Err(t) => return fail(t),
        };
        let id = self.add_result(
            conversation,
            ResultStatus::Pending,
            texts(&Text::Pending(definition.id.clone())),
            ResultProvider::Bot,
        );
        let submit = Submit {
            community,
            channel,
            operation: u64::from(bot.operation),
            args: sealed_args,
            request_id: new_request_id(),
        };
        self.drive(id, conversation, submit, definition, args, texts);
        id
    }

    /// Sends a held row's command again with its request id (the Bot answers what it answered, or runs it once).
    pub fn retry(&self, row: u64) -> bool {
        let Some(h) = held().get_or_insert_with(HashMap::new).remove(&row) else {
            return false;
        };
        h.bot.patch(
            h.conversation,
            row,
            Patch {
                status: Some(ResultStatus::Pending),
                text: Some((h.texts)(&Text::Pending(h.definition.id.clone()))),
                hold: Some(false),
            },
        );
        h.bot
            .drive(row, h.conversation, h.submit, h.definition, h.args, h.texts);
        true
    }

    fn drive(
        &self,
        row: u64,
        conversation: Conversation,
        submit: Submit,
        definition: CommandDefinition,
        args: Args,
        texts: Texts,
    ) {
        let bot = self.clone();
        let sealed = self.sealed();
        drop(self.inner.spawn(async move {
            let progress_bot = bot.clone();
            let progress_texts = texts.clone();
            let end = sealed
                .run(&submit, move |a| {
                    if let Some(t) = answer_text(&a.body) {
                        let _ = &progress_texts;
                        progress_bot.patch(
                            conversation,
                            row,
                            Patch {
                                text: Some(t),
                                ..Patch::default()
                            },
                        );
                    }
                })
                .await;
            let snapshot = bot.inner.snapshot();
            let view = snapshot.bot.catalogs.get(&submit.community);
            let (status, text, hold) = match end {
                RunEnd::Final(answer) => {
                    let (status, coded, plain) = answer_result(&answer, view);
                    let text = match (coded, plain) {
                        (Some(c), _) => texts(&c),
                        (None, Some(t)) => t,
                        (None, None) => posted_in(&snapshot, &definition, &args)
                            .map(|t| texts(&t))
                            .unwrap_or_default(),
                    };
                    (status, text, false)
                }
                RunEnd::Timeout => (ResultStatus::Cancelled, texts(&Text::NoAnswer), true),
                RunEnd::Refused(r) => {
                    if r.code == "E_BOT_OFFLINE" {
                        let _ = sealed.refresh(submit.community).await;
                    }
                    let t = code_text(&r.code, view).unwrap_or(if r.status == 403 {
                        Text::Disabled
                    } else {
                        Text::Failed
                    });
                    (ResultStatus::Failed, texts(&t), false)
                }
            };
            if hold {
                held().get_or_insert_with(HashMap::new).insert(
                    row,
                    Held {
                        bot: bot.clone(),
                        conversation,
                        submit,
                        definition,
                        args,
                        texts,
                    },
                );
            }
            bot.patch(
                conversation,
                row,
                Patch {
                    status: Some(status),
                    text: Some(text),
                    hold: Some(hold),
                },
            );
            Ok(())
        }));
    }
}

/// A command whose result is a channel post, answered with no text, says where it went.
fn posted_in(state: &AppState, definition: &CommandDefinition, args: &Args) -> Option<Text> {
    let bot = definition.bot.as_ref()?;
    if !bot.needs_bot_channel {
        return None;
    }
    let index = bot
        .arg_types
        .iter()
        .position(|t| *t == crate::state::bot::ArgType::Channel)?;
    let target = args.get(&definition.options.get(index)?.name)?;
    let channel = state.channels.iter().find(|c| c.id.hex() == *target)?;
    Some(Text::PostedIn {
        what: format!("/{}", definition.id),
        channel: channel.name.clone(),
    })
}

/// 32 lower-case hex digits as 16 bytes, not all zero.
fn unhex16(text: &str) -> Option<[u8; 16]> {
    if text.len() != 32 || !text.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f')) {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, byte) in out.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&text[2 * i..2 * i + 2], 16).ok()?;
    }
    (out != [0; 16]).then_some(out)
}

#[cfg(test)]
mod tests {
    //! Ported from bot-handlers.test.tsx and bot-run's argument and answer rules.
    use super::*;
    use crate::features::bot::registry::{bot_command_definitions, commands};
    use crate::state::bot::{ArgShape, ArgType, BotCommandSpec, BotStatus};

    fn def(name: &str, args: &[(&str, ArgType)], needs: bool) -> CommandDefinition {
        bot_command_definitions(&[BotCommandSpec {
            name: name.into(),
            description: String::new(),
            operation: 80,
            args: args
                .iter()
                .map(|&(n, ty)| ArgShape {
                    name: n.into(),
                    ty,
                    required: false,
                })
                .collect(),
            permission_bit: 0,
            needs_bot_channel: needs,
            handler_kind: 0,
        }])
        .remove(0)
    }

    fn utc() -> Zone {
        Zone {
            name: "UTC".into(),
            offset_at: Arc::new(|_| 0),
        }
    }

    fn args(pairs: &[(&str, &str)]) -> Args {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect()
    }

    #[test]
    fn arguments_follow_the_catalog_shapes() {
        let d = def(
            "remind",
            &[("when", ArgType::Duration), ("text", ArgType::Text)],
            false,
        );
        let got = bot_args(
            &d,
            &args(&[("when", "1h"), ("text", "stretch")]),
            &utc(),
            1_767_225_600,
        )
        .unwrap();
        assert_eq!(got, [BotArg::Uint(3600), BotArg::Text("stretch".into())]);
        assert_eq!(
            bot_args(&d, &args(&[("when", "soon")]), &utc(), 0),
            Err(Text::WhenHint)
        );
        let d = def(
            "x",
            &[
                ("n", ArgType::Uint),
                ("who", ArgType::User),
                ("for", ArgType::Duration),
                ("loud", ArgType::Bool),
            ],
            false,
        );
        let id = "0123456789abcdef0123456789abcdef";
        let got = bot_args(
            &d,
            &args(&[
                ("n", "7"),
                ("who", id),
                ("for", "10 minutes"),
                ("loud", "on"),
            ]),
            &utc(),
            0,
        )
        .unwrap();
        assert_eq!(got[0], BotArg::Uint(7));
        assert!(matches!(&got[1], BotArg::Bytes(b) if b.len() == 16));
        assert_eq!(got[2], BotArg::Uint(600));
        assert_eq!(got[3], BotArg::Uint(1));
        assert_eq!(
            bot_args(&d, &args(&[("n", "-1")]), &utc(), 0),
            Err(Text::WholeNumber("n".into()))
        );
        assert_eq!(
            bot_args(&d, &args(&[("who", "ada")]), &utc(), 0),
            Err(Text::ChooseFrom("who".into()))
        );
        assert_eq!(
            bot_args(&d, &args(&[]), &utc(), 0).unwrap(),
            vec![BotArg::Null; 4]
        );
    }

    #[test]
    fn answers_become_row_texts() {
        let a = |status, body: &str| Answer {
            seq: 0,
            status,
            body: body.as_bytes().to_vec(),
            final_: true,
        };
        assert_eq!(
            answer_result(&a(200, "Reminder set."), None),
            (ResultStatus::Success, None, Some("Reminder set.".into()))
        );
        assert_eq!(
            answer_result(&a(403, "E_FORBIDDEN"), None),
            (ResultStatus::Failed, Some(Text::Forbidden), None)
        );
        assert_eq!(
            answer_result(&a(503, "E_BOT_OFFLINE"), None),
            (ResultStatus::Failed, Some(Text::Offline(None)), None)
        );
        assert_eq!(
            answer_result(&a(400, "Pick a time"), None),
            (ResultStatus::Failed, None, Some("Pick a time".into()))
        );
        assert_eq!(
            answer_result(&a(500, "E_WHATEVER"), None),
            (ResultStatus::Failed, Some(Text::Failed), None)
        );
        assert_eq!(
            answer_result(&a(403, ""), None),
            (ResultStatus::Failed, Some(Text::Forbidden), None)
        );
    }

    #[test]
    fn a_greyed_command_says_why() {
        let d = def("poll create", &[("question", ArgType::Text)], true);
        let mut view = BotCatalogView {
            revision: 1,
            commands: vec![],
            status: BotStatus {
                state: BotStatusState::Ready,
                since: None,
            },
            bot_channels: Some(vec![]),
            bot_user: None,
            bot_name: String::new(),
            fingerprint: None,
        };
        assert_eq!(reason(Some(&view), &d), Some(Text::NeedsBotChannel));
        view.bot_channels = Some(vec![ChannelId([1; 16])]);
        assert_eq!(reason(Some(&view), &d), None);
        view.status.state = BotStatusState::Changed;
        assert_eq!(reason(Some(&view), &d), Some(Text::Changed));
        assert_eq!(reason(Some(&view), commands().first().unwrap()), None);
    }

    #[test]
    fn spoken_and_registry_durations() {
        assert_eq!(duration_seconds("10m"), Some(600));
        assert_eq!(duration_seconds("1h30m"), Some(5400));
        assert_eq!(duration_seconds("7 days"), Some(604_800));
        assert_eq!(duration_seconds("soon"), None);
    }
}
