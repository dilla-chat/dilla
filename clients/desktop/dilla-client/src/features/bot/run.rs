//! A Bot command from the picker, run as one sealed command (packages/web/src/screens/commands/bot-run.ts): its
//! arguments as the catalog's shapes say, its answers streamed into a private result row, an unanswered run held with a
//! Retry that reuses the request id. The copy is the caller's ([`Texts`]); nothing here holds a string the UI shows.

use std::sync::atomic::Ordering;
use std::sync::{Arc, Weak};
use std::time::Duration;

use super::private::Patch;
use super::registry::{ArgKind, Args, CommandDefinition};
use super::sealed::{Answer, RunEnd, Sealed, Submit};
use super::{Bot, NEXT_RESULT, now_ms, private};
use crate::client::Inner;
use crate::features::bot_admin::{BotArg, new_request_id};
use crate::features::utilities::time::{ReminderTime, ReminderZone, parse_reminder_time};
use crate::state::bot::{
    BotCatalogView, BotState, BotStatusState, Conversation, PRIVATE_RESULT_TTL_MS, ResultProvider,
    ResultStatus,
};
use crate::state::{ChannelId, CommunityId};

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

/// What a held (unanswered) row keeps for its Retry, in its client's [`Sealed`]: the request id is sent again.
pub(crate) struct HeldRun {
    pub(crate) conversation: Conversation,
    pub(crate) submit: Submit,
    pub(crate) definition: CommandDefinition,
    pub(crate) args: Args,
    pub(crate) texts: Texts,
}

/// Where a client's private rows live, and the clock they are stamped with.
pub trait Rows: Send + Sync {
    /// Unix milliseconds.
    fn now_ms(&self) -> u64;
    /// Applies `f` to the bot slice.
    fn apply(&self, f: Box<dyn FnOnce(&mut BotState) + Send>);
    /// The earliest moment a row dismisses itself, as the slice stands.
    fn next_expiry(&self) -> Option<u64>;
    /// A channel's name (a channel post's "posted in #…").
    fn channel_name(&self, channel: ChannelId) -> Option<String>;
}

/// The rows of a running client: its published state.
pub(crate) struct InnerRows {
    pub(crate) inner: Weak<Inner>,
}

impl Rows for InnerRows {
    fn now_ms(&self) -> u64 {
        now_ms()
    }
    fn apply(&self, f: Box<dyn FnOnce(&mut BotState) + Send>) {
        if let Some(inner) = self.inner.upgrade() {
            inner.publish(move |s| f(&mut s.bot));
        }
    }
    fn next_expiry(&self) -> Option<u64> {
        private::next_expiry(&self.inner.upgrade()?.snapshot().bot)
    }
    fn channel_name(&self, channel: ChannelId) -> Option<String> {
        self.inner
            .upgrade()?
            .snapshot()
            .channel(channel)
            .map(|c| c.name.clone())
    }
}

/// The private rows and the Bot runs behind them, for one client.
#[derive(Clone)]
pub struct Runs {
    sealed: Arc<Sealed>,
    rows: Arc<dyn Rows>,
}

/// Once a published sweep lands, the next is looked for at least this long later (the slice is published on the client's
/// loop, so a sweep is seen a moment after it ran).
const SWEEP_SETTLE: Duration = Duration::from_millis(250);

impl Runs {
    pub fn new(sealed: Arc<Sealed>, rows: Arc<dyn Rows>) -> Self {
        Self { sealed, rows }
    }

    /// Adds a row; rows the conversation cap evicts take their held runs with them.
    pub fn add_result(
        &self,
        conversation: Conversation,
        status: ResultStatus,
        text: String,
        provider: ResultProvider,
    ) -> u64 {
        let id = NEXT_RESULT.fetch_add(1, Ordering::Relaxed);
        let now = self.rows.now_ms();
        let sealed = self.sealed.clone();
        self.rows.apply(Box::new(move |b| {
            for evicted in private::add(b, conversation, status, text, provider, id, now) {
                sealed.drop_held(evicted);
            }
        }));
        if status != ResultStatus::Pending {
            self.schedule_sweep();
        }
        id
    }

    /// Changes a row; one that settles (finished and not held) goes away by itself after the TTL.
    pub fn patch(&self, conversation: Conversation, id: u64, patch: Patch) {
        let settles =
            patch.status.is_some_and(|s| s != ResultStatus::Pending) && patch.hold != Some(true);
        let now = self.rows.now_ms();
        self.rows.apply(Box::new(move |b| {
            private::update(b, &conversation, id, patch, now);
        }));
        if settles {
            self.schedule_sweep();
        }
    }

    /// The Dismiss button: the row goes, and its held run with it (its reply secret is forgotten, as TS `cancel()`).
    pub fn dismiss(&self, conversation: Conversation, id: u64) {
        self.rows.apply(Box::new(move |b| {
            private::dismiss(b, &conversation, id);
        }));
        self.sealed.drop_held(id);
    }

    /// Sweeps the rows whose time is up, at the TTL and then at each next expiry, until none is left to expire.
    fn schedule_sweep(&self) {
        let rows = self.rows.clone();
        self.sealed.spawn(Box::pin(async move {
            tokio::time::sleep(Duration::from_millis(PRIVATE_RESULT_TTL_MS)).await;
            loop {
                let now = rows.now_ms();
                rows.apply(Box::new(move |b| {
                    private::sweep(b, now);
                }));
                tokio::time::sleep(SWEEP_SETTLE).await;
                let Some(next) = rows.next_expiry() else {
                    return;
                };
                let wait = next.saturating_sub(rows.now_ms());
                tokio::time::sleep(Duration::from_millis(wait)).await;
            }
        }));
    }

    /// Runs a picked Bot command in `conversation` (a channel of `community`): a pending private row at once, its answers
    /// as they come, and its end. Answers the row's id. A row with no answer is held with a Retry ([`Runs::retry`]).
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
        let view = self.sealed.view(community);
        let fail = |text: Text| {
            self.add_result(
                conversation,
                ResultStatus::Failed,
                texts(&text),
                ResultProvider::Bot,
            )
        };
        if let Some(r) = reason(view.as_ref(), &definition) {
            return fail(r);
        }
        let Some(bot) = definition.bot.clone() else {
            return fail(Text::Failed);
        };
        let now = i64::try_from(self.sealed.now_s()).unwrap_or(i64::MAX);
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
        self.drive(
            id,
            HeldRun {
                conversation,
                submit,
                definition,
                args,
                texts,
            },
        );
        id
    }

    /// Sends a held row's command again with its request id; `false` when the row holds no run (dismissed, evicted,
    /// signed out).
    pub fn retry(&self, row: u64) -> bool {
        let Some(h) = self.sealed.take_held(row) else {
            return false;
        };
        self.patch(
            h.conversation,
            row,
            Patch {
                status: Some(ResultStatus::Pending),
                text: Some((h.texts)(&Text::Pending(h.definition.id.clone()))),
                hold: Some(false),
            },
        );
        self.drive(row, h);
        true
    }

    /// After a timeout: the answers that still arrive (until the reply secret's hold ends) update the held row, and the
    /// final one settles it and drops its run. A stale-key answer is not shown (the Retry reads the catalog again).
    fn listen_late(
        &self,
        row: u64,
        conversation: Conversation,
        mut late: tokio::sync::mpsc::UnboundedReceiver<Answer>,
        texts: Texts,
        community: CommunityId,
    ) {
        let this = self.clone();
        self.sealed.spawn(Box::pin(async move {
            let deadline = tokio::time::Instant::now() + super::sealed::REPLY_HOLD;
            while let Ok(Some(a)) = tokio::time::timeout_at(deadline, late.recv()).await {
                if a.status == super::sealed::KEY_STALE_STATUS {
                    continue;
                }
                if !a.final_ {
                    if let Some(t) = answer_text(&a.body) {
                        this.patch(
                            conversation,
                            row,
                            Patch {
                                text: Some(t),
                                ..Patch::default()
                            },
                        );
                    }
                    continue;
                }
                // Only while the row still holds its run: a Retry or a dismiss took it over.
                if this.sealed.take_held(row).is_none() {
                    return;
                }
                let view = this.sealed.view(community);
                let (status, coded, plain) = answer_result(&a, view.as_ref());
                let text = coded.map(|c| texts(&c)).or(plain).unwrap_or_default();
                this.patch(
                    conversation,
                    row,
                    Patch {
                        status: Some(status),
                        text: Some(text),
                        hold: Some(false),
                    },
                );
                return;
            }
        }));
    }

    fn drive(&self, row: u64, run: HeldRun) {
        let this = self.clone();
        self.sealed.spawn(Box::pin(async move {
            let (conversation, progress_runs) = (run.conversation, this.clone());
            let end = this
                .sealed
                .run(&run.submit, move |a| {
                    if let Some(t) = answer_text(&a.body) {
                        progress_runs.patch(
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
            let view = this.sealed.view(run.submit.community);
            let texts = run.texts.clone();
            let (status, text, hold) = match end {
                RunEnd::Final(answer) => {
                    let (status, coded, plain) = answer_result(&answer, view.as_ref());
                    let text = match (coded, plain) {
                        (Some(c), _) => texts(&c),
                        (None, Some(t)) => t,
                        (None, None) => posted_in(&*this.rows, &run.definition, &run.args)
                            .map(|t| texts(&t))
                            .unwrap_or_default(),
                    };
                    (status, text, false)
                }
                RunEnd::Timeout => (ResultStatus::Cancelled, texts(&Text::NoAnswer), true),
                RunEnd::Refused(r) => {
                    if r.code == "E_BOT_OFFLINE" {
                        let _ = this.sealed.refresh(run.submit.community).await;
                    }
                    let t = code_text(&r.code, view.as_ref()).unwrap_or(if r.status == 403 {
                        Text::Disabled
                    } else {
                        Text::Failed
                    });
                    (ResultStatus::Failed, texts(&t), false)
                }
            };
            if hold {
                // onLate (M4): an answer that still arrives within the hold time settles the row.
                let (request_id, community, late_texts) = (
                    run.submit.request_id,
                    run.submit.community,
                    run.texts.clone(),
                );
                this.sealed.hold(row, run);
                if let Some(late) = this.sealed.late(request_id) {
                    this.listen_late(row, conversation, late, late_texts, community);
                }
            } else {
                this.sealed.forget(run.submit.request_id);
            }
            this.patch(
                conversation,
                row,
                Patch {
                    status: Some(status),
                    text: Some(text),
                    hold: Some(hold),
                },
            );
        }));
    }
}

impl Bot {
    /// Runs a picked Bot command ([`Runs::run_command`]).
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
        self.runs().run_command(
            conversation,
            community,
            channel,
            definition,
            args,
            zone,
            texts,
        )
    }

    /// Sends a held row's command again ([`Runs::retry`]).
    pub fn retry(&self, row: u64) -> bool {
        self.runs().retry(row)
    }
}

/// A command whose result is a channel post, answered with no text, says where it went.
fn posted_in(rows: &dyn Rows, definition: &CommandDefinition, args: &Args) -> Option<Text> {
    let bot = definition.bot.as_ref()?;
    if !bot.needs_bot_channel {
        return None;
    }
    let index = bot
        .arg_types
        .iter()
        .position(|t| *t == crate::state::bot::ArgType::Channel)?;
    let target = unhex16(args.get(&definition.options.get(index)?.name)?)?;
    Some(Text::PostedIn {
        what: format!("/{}", definition.id),
        channel: rows.channel_name(ChannelId(target))?,
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

    /// The private rows and their runs against the fake Bot, on a paused clock.
    mod runs {
        use std::sync::Mutex;

        use super::*;
        use crate::features::bot::registry::bot_command_definitions;
        use crate::features::bot::sealed::tests::{
            COMMUNITY, Fake, posted, ready, ready_fake, reply,
        };

        /// The rows in memory, stamped by the client runtime's (paused) clock.
        struct MemRows {
            state: Mutex<BotState>,
            start: tokio::time::Instant,
            runtime: tokio::runtime::Handle,
        }

        impl MemRows {
            fn new() -> Arc<Self> {
                Self::on(tokio::runtime::Handle::current())
            }
            fn on(runtime: tokio::runtime::Handle) -> Arc<Self> {
                let _in = runtime.enter();
                Arc::new(Self {
                    state: Mutex::default(),
                    start: tokio::time::Instant::now(),
                    runtime: runtime.clone(),
                })
            }
            fn rows(&self, c: &Conversation) -> Vec<crate::state::bot::PrivateResult> {
                self.state.lock().unwrap().results(c).to_vec()
            }
        }

        impl Rows for MemRows {
            fn now_ms(&self) -> u64 {
                let _in = self.runtime.enter();
                1_000_000 + self.start.elapsed().as_millis() as u64
            }
            fn apply(&self, f: Box<dyn FnOnce(&mut BotState) + Send>) {
                f(&mut self.state.lock().unwrap());
            }
            fn next_expiry(&self) -> Option<u64> {
                private::next_expiry(&self.state.lock().unwrap())
            }
            fn channel_name(&self, _: ChannelId) -> Option<String> {
                None
            }
        }

        const HERE: Conversation = Conversation {
            channel: ChannelId([2; 16]),
            thread: None,
        };

        fn texts() -> Texts {
            Arc::new(|t: &Text| format!("{t:?}"))
        }

        fn utc() -> Zone {
            Zone {
                name: "UTC".into(),
                offset_at: Arc::new(|_| 0),
            }
        }

        async fn setup() -> (Arc<Fake>, Arc<Sealed>, Arc<MemRows>, Runs) {
            let fake = ready_fake();
            let sealed = ready(&fake).await;
            let rows = MemRows::new();
            let runs = Runs::new(sealed.clone(), rows.clone());
            (fake, sealed, rows, runs)
        }

        fn remind(sealed: &Sealed) -> CommandDefinition {
            bot_command_definitions(&sealed.view(COMMUNITY).unwrap().commands).remove(0)
        }

        /// The request id of the `n`th post.
        fn posted_id(fake: &Fake, n: usize) -> [u8; 16] {
            let body = fake.posted.lock().unwrap()[n].clone();
            let v = crate::gateway::frames::Value::decode(&body).unwrap();
            v.as_array().unwrap()[0]
                .as_bytes()
                .unwrap()
                .try_into()
                .unwrap()
        }

        async fn settle() {
            for _ in 0..50 {
                tokio::task::yield_now().await;
            }
        }

        #[tokio::test(start_paused = true)]
        async fn a_bot_result_settles_and_goes_away_at_the_ttl() {
            let (fake, sealed, rows, runs) = setup().await;
            let row = runs.run_command(
                HERE,
                COMMUNITY,
                Some(HERE.channel),
                remind(&sealed),
                Args::new(),
                &utc(),
                texts(),
            );
            posted(&fake, 1).await;
            let key = fake.reply_keys.lock().unwrap()[0];
            sealed.on_reply(&reply(
                key,
                posted_id(&fake, 0),
                0,
                200,
                "Reminder set.",
                true,
            ));
            settle().await;
            let r = rows.rows(&HERE);
            assert_eq!(
                (r[0].id, r[0].status, r[0].text.as_str()),
                (row, ResultStatus::Success, "Reminder set.")
            );
            assert!(r[0].settled_at.is_some());
            tokio::time::sleep(Duration::from_millis(PRIVATE_RESULT_TTL_MS - 1000)).await;
            settle().await;
            assert_eq!(rows.rows(&HERE).len(), 1, "still there before the TTL");
            tokio::time::sleep(Duration::from_millis(2000)).await;
            settle().await;
            assert!(rows.rows(&HERE).is_empty(), "gone at the TTL");
        }

        #[tokio::test(start_paused = true)]
        async fn an_unanswered_run_is_held_and_dismiss_forgets_it() {
            let (fake, sealed, rows, runs) = setup().await;
            let row = runs.run_command(
                HERE,
                COMMUNITY,
                Some(HERE.channel),
                remind(&sealed),
                Args::new(),
                &utc(),
                texts(),
            );
            posted(&fake, 1).await;
            tokio::time::sleep(Duration::from_secs(6)).await;
            settle().await;
            let rid = posted_id(&fake, 0);
            assert!(rows.rows(&HERE)[0].hold);
            assert!(sealed.holds_row(row) && sealed.holds(rid));
            runs.dismiss(HERE, row);
            assert!(
                !sealed.holds_row(row) && !sealed.holds(rid),
                "dismiss drops the run and its reply secret"
            );
            assert!(!runs.retry(row));
            // A held row never goes by the TTL: it stays until it settles or is dismissed.
            assert!(rows.rows(&HERE).is_empty());
        }

        #[tokio::test(start_paused = true)]
        async fn a_late_answer_settles_a_held_row() {
            let (fake, sealed, rows, runs) = setup().await;
            let row = runs.run_command(
                HERE,
                COMMUNITY,
                Some(HERE.channel),
                remind(&sealed),
                Args::new(),
                &utc(),
                texts(),
            );
            posted(&fake, 1).await;
            tokio::time::sleep(Duration::from_secs(6)).await;
            settle().await;
            assert!(rows.rows(&HERE)[0].hold);
            let key = fake.reply_keys.lock().unwrap()[0];
            sealed.on_reply(&reply(
                key,
                posted_id(&fake, 0),
                0,
                200,
                "Reminder set.",
                true,
            ));
            settle().await;
            let r = rows.rows(&HERE);
            assert_eq!(
                (r[0].status, r[0].hold, r[0].text.as_str()),
                (ResultStatus::Success, false, "Reminder set.")
            );
            assert!(!sealed.holds_row(row));
        }

        #[tokio::test(start_paused = true)]
        async fn retry_resends_the_held_request_id_until_sign_out() {
            let (fake, sealed, _rows, runs) = setup().await;
            let row = runs.run_command(
                HERE,
                COMMUNITY,
                Some(HERE.channel),
                remind(&sealed),
                Args::new(),
                &utc(),
                texts(),
            );
            posted(&fake, 1).await;
            tokio::time::sleep(Duration::from_secs(6)).await;
            settle().await;
            assert!(runs.retry(row));
            posted(&fake, 2).await;
            assert_eq!(posted_id(&fake, 0), posted_id(&fake, 1));
            tokio::time::sleep(Duration::from_secs(6)).await;
            settle().await;
            assert!(sealed.holds_row(row));
            sealed.forget_all();
            assert!(!sealed.holds_row(row) && !sealed.holds(posted_id(&fake, 0)));
            assert!(!runs.retry(row), "nothing is sent again after sign-out");
            settle().await;
            assert_eq!(fake.posted.lock().unwrap().len(), 2);
        }

        /// The UI thread has no runtime: a command, its Retry and a settling row all run on the client's runtime, and the
        /// row dismisses itself at the TTL.
        #[test]
        fn runs_work_from_a_thread_without_a_runtime() {
            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .start_paused(true)
                .build()
                .unwrap();
            let fake = ready_fake();
            *fake.runtime.lock().unwrap() = Some(rt.handle().clone());
            let sealed = rt.block_on(ready(&fake));
            let rows = MemRows::on(rt.handle().clone());
            let runs = Runs::new(sealed.clone(), rows.clone());
            assert!(
                tokio::runtime::Handle::try_current().is_err(),
                "this thread has no runtime"
            );

            // A command that gets no answer is held; its Retry, from this thread, sends the same request id.
            let row = runs.run_command(
                HERE,
                COMMUNITY,
                Some(HERE.channel),
                remind(&sealed),
                Args::new(),
                &utc(),
                texts(),
            );
            rt.block_on(async {
                posted(&fake, 1).await;
                tokio::time::sleep(Duration::from_secs(6)).await;
                settle().await;
            });
            assert!(rows.rows(&HERE)[0].hold && sealed.holds_row(row));
            assert!(runs.retry(row));
            rt.block_on(async {
                posted(&fake, 2).await;
                let key = fake.reply_keys.lock().unwrap()[1];
                sealed.on_reply(&reply(
                    key,
                    posted_id(&fake, 1),
                    0,
                    200,
                    "Reminder set.",
                    true,
                ));
                settle().await;
            });
            assert_eq!(posted_id(&fake, 0), posted_id(&fake, 1));
            let r = rows.rows(&HERE);
            assert_eq!(
                (r[0].status, r[0].text.as_str()),
                (ResultStatus::Success, "Reminder set.")
            );

            // A settling patch from this thread schedules the sweep there too.
            let other = runs.add_result(
                HERE,
                ResultStatus::Pending,
                "x".into(),
                ResultProvider::Builtin,
            );
            runs.patch(
                HERE,
                other,
                Patch {
                    status: Some(ResultStatus::Failed),
                    ..Patch::default()
                },
            );
            rt.block_on(async {
                tokio::time::sleep(Duration::from_millis(PRIVATE_RESULT_TTL_MS + 2000)).await;
                settle().await;
            });
            assert!(
                rows.rows(&HERE).is_empty(),
                "both rows dismissed at the TTL"
            );
        }

        /// A refused session that is renewed (4004 on expiry) keeps in-flight runs and Retry; a revocation forgets them.
        #[tokio::test(start_paused = true)]
        async fn a_renewed_session_keeps_held_runs_and_a_revocation_forgets_them() {
            let (fake, sealed, _rows, runs) = setup().await;
            let row = runs.run_command(
                HERE,
                COMMUNITY,
                Some(HERE.channel),
                remind(&sealed),
                Args::new(),
                &utc(),
                texts(),
            );
            posted(&fake, 1).await;
            tokio::time::sleep(Duration::from_secs(6)).await;
            settle().await;
            let refused =
                crate::sync::ConnectionEvent::Refused(crate::gateway::Refusal::Revoked, 4004);
            crate::features::bot::on_connection_of(&sealed, Weak::new(), &refused);
            crate::features::bot::on_connection_of(
                &sealed,
                Weak::new(),
                &crate::sync::ConnectionEvent::Down,
            );
            assert!(
                sealed.holds_row(row) && sealed.holds(posted_id(&fake, 0)),
                "a renewal keeps the run"
            );
            assert!(runs.retry(row));
            posted(&fake, 2).await;
            tokio::time::sleep(Duration::from_secs(6)).await;
            settle().await;
            assert!(sealed.holds_row(row));
            sealed.forget_all();
            assert!(
                !sealed.holds_row(row) && !sealed.holds(posted_id(&fake, 0)),
                "a revocation forgets it"
            );
        }

        #[tokio::test(start_paused = true)]
        async fn a_held_run_evicted_by_the_conversation_cap_is_forgotten() {
            let (fake, sealed, _rows, runs) = setup().await;
            let row = runs.run_command(
                HERE,
                COMMUNITY,
                Some(HERE.channel),
                remind(&sealed),
                Args::new(),
                &utc(),
                texts(),
            );
            posted(&fake, 1).await;
            tokio::time::sleep(Duration::from_secs(6)).await;
            settle().await;
            assert!(sealed.holds_row(row));
            for n in 0..60u8 {
                tokio::time::sleep(Duration::from_millis(1)).await;
                let c = Conversation {
                    channel: ChannelId([100u8.wrapping_add(n); 16]),
                    thread: None,
                };
                runs.add_result(
                    c,
                    ResultStatus::Success,
                    "x".into(),
                    ResultProvider::Builtin,
                );
            }
            assert!(!sealed.holds_row(row) && !sealed.holds(posted_id(&fake, 0)));
        }
    }
}
