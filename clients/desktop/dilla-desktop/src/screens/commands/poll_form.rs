//! `/poll create` opens the poll form (Task 26; UtilityCommandProvider.tsx `handlers['poll create']`, `PollEditor`,
//! packages/ui PollDialog). The web runs it through its own handler, not as a typed Bot command: the form opens over the
//! conversation, and Post sends one `create` (operation 70) to the community's Bot.
//!
//! - Opening ([`open`]): only in a community channel where this member may send and whose Bot offers poll creation, else
//!   the command's row says why (Failed). The `question`, `answers` (split at the commas), `multiple` and `duration`
//!   arguments prefill the fields ([`opening`]).
//! - Post ([`submit`]): the fields are checked in the web's order ([`validate`]); a good form takes its one request id
//!   ([`PollForm::begin_post`]), shows the command's pending row and sends; the form closes as it sends, as the web's
//!   dialog does. Only Post sends (the web's fields have no Enter handler).
//! - The answer ([`settled`]): posted, the toast and the row say so and the form closes; an answer that may or may not
//!   have taken effect closes the form too (the row says to check the conversation); any definitive refusal keeps the
//!   form open with the reason, and a second Post is a new request (R29).
//! - Task 30: the form's request id is the one the command memory remembers for the draft, and the poll is kept there
//!   (`CommandMemory::polls`) as it goes. An uncertain answer puts the draft back in the composer; sent again, the
//!   command reconciles that poll without the form ([`reconcile`]).
//! - Cancel, Escape, a click outside and leaving the conversation close the form and leave no row (CommandHost.tsx:297).
//!   The Bot no longer offering polls closes it with a Failed row.
//!
//! The form's Debug gives no text: the question and the answers are decrypted content (R20, R21).

use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::bot::memory::PollAttempt;
use dilla_client::features::bot::registry::Args;
use dilla_client::features::bot::{Attempt, Outcome};
use dilla_client::features::utilities::{UtilityError, polls};
use dilla_client::state::bot::{Conversation, ResultProvider, ResultStatus};
use dilla_client::state::bot_admin::sealed_operation;
use dilla_client::state::threads::SEND_MESSAGES;
use dilla_client::state::{AppState, ChannelId, CommunityId, UserId};
use gpui::{
    AnyElement, App, AppContext, Entity, Focusable, IntoElement, ParentElement, Styled,
    Subscription, Window, WindowId, div, px,
};
use gpui_component::input::{Input, InputEvent, InputState};

use dilla_desktop_ui::components::commands::poll_dialog::{self, DURATIONS, PollDialog};
use dilla_desktop_ui::components::dialog::InitialFocus;
use dilla_desktop_ui::components::text_field;
use dilla_desktop_ui::metrics::spread_ring;
use dilla_desktop_ui::tokens::{color, font, radius, text};

use super::{FixtureRows, add_fixture_row, copy, picker};
use crate::slots::SlotCx;
use crate::strings::{t, tf};

/// The v1 operation a poll is created with (utilities.ts `pollCreate`).
const POLL_CREATE: u64 = 70;
/// `DURATIONS`' default: 24h (UtilityCommandProvider.tsx:126).
const DEFAULT_DURATION: usize = 3;
/// How many answers the form holds (`answers.length < 10`) and how few (`<= 2` cannot be removed).
const MAX_ANSWERS: usize = 10;
const MIN_ANSWERS: usize = 2;
/// The question's and an answer's longest text, in characters (PollDialog's counters; `poll.questionLong`, `answerLong`).
const QUESTION_MAX: usize = 300;
const ANSWER_MAX: usize = 100;

/// The automation ids of the answers' fields.
const ANSWER_IDS: [&str; MAX_ANSWERS] = [
    "poll-answer-0",
    "poll-answer-1",
    "poll-answer-2",
    "poll-answer-3",
    "poll-answer-4",
    "poll-answer-5",
    "poll-answer-6",
    "poll-answer-7",
    "poll-answer-8",
    "poll-answer-9",
];

/// PollDialog.css: `.d-poll-dialog input` is 34px high (content box) with `0 10px` padding and a 1px edge; the question's
/// own `input` is 38px with `0 12px`. A frame's height here includes the two edge pixels.
const ANSWER_H: f32 = 34.0 + 2.0;
const ANSWER_PAD_X: f32 = 10.0;
const QUESTION_H: f32 = 38.0 + 2.0;
const QUESTION_PAD_X: f32 = 12.0;
/// `.d-poll-dialog__question input:focus`: a 3px `--accent-ring`.
const QUESTION_RING: f32 = 3.0;

/// What the form starts with (UtilityCommandProvider.tsx:124-126, :374).
#[derive(Clone, PartialEq, Eq)]
pub struct Opening {
    pub question: String,
    /// The answer rows, at least two (blank ones included).
    pub answers: Vec<String>,
    /// The chosen duration's index in [`DURATIONS`].
    pub duration: usize,
    pub multi: bool,
}

impl std::fmt::Debug for Opening {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Opening")
            .field("question", &dilla_client::state::Redacted(&self.question))
            .field("answers", &dilla_client::state::RedactedList(&self.answers))
            .field("duration", &self.duration)
            .field("multi", &self.multi)
            .finish()
    }
}

/// The answer rows of the `answers` argument: split where a comma stands with its surrounding white space
/// (`answers.replace(/\s*,\s*/gu, '\n')`), never fewer than two rows.
fn answer_rows(arg: &str) -> Vec<String> {
    let parts: Vec<&str> = arg.split(',').collect();
    let last = parts.len() - 1;
    let mut rows: Vec<String> = parts
        .iter()
        .enumerate()
        .map(|(i, p)| {
            let p = if i > 0 { p.trim_start() } else { p };
            let p = if i < last { p.trim_end() } else { p };
            p.to_owned()
        })
        .collect();
    if arg.is_empty() {
        rows.clear();
    }
    while rows.len() < MIN_ANSWERS {
        rows.push(String::new());
    }
    rows
}

/// The form's fields from the typed command's arguments.
pub fn opening(args: &Args) -> Opening {
    let arg = |name: &str| args.get(name).map_or("", String::as_str);
    Opening {
        question: arg("question").to_owned(),
        answers: answer_rows(arg("answers")),
        duration: DURATIONS
            .iter()
            .position(|d| *d == arg("duration"))
            .unwrap_or(DEFAULT_DURATION),
        multi: arg("multiple") == "on",
    }
}

/// The text of a refusal that a field can explain, as `PollEditor.submit` checks it: the question first, then the answers,
/// then the whole payload's size. Answers are the non-blank ones, trimmed; `Err` is a `strings` key.
pub fn validate(
    question: &str,
    answers: &[String],
    multi: bool,
    duration: usize,
    channel: [u8; 16],
) -> Result<polls::PollCreate, &'static str> {
    let question = question.trim();
    let answers: Vec<String> = answers
        .iter()
        .map(|a| a.trim().to_owned())
        .filter(|a| !a.is_empty())
        .collect();
    if question.is_empty() {
        return Err("poll.questionRequired");
    }
    if question.chars().count() > QUESTION_MAX {
        return Err("poll.questionLong");
    }
    if answers.len() < MIN_ANSWERS {
        return Err("poll.answersRequired");
    }
    if answers.iter().any(|a| a.chars().count() > ANSWER_MAX) {
        return Err("poll.answerLong");
    }
    let mut unique = answers.clone();
    unique.sort();
    unique.dedup();
    if answers.len() > MAX_ANSWERS || unique.len() != answers.len() {
        return Err("poll.duplicates");
    }
    let input = polls::PollCreate {
        channel,
        thread: None,
        question: question.to_owned(),
        answers,
        multi,
        duration: *polls::POLL_DURATIONS
            .get(duration)
            .unwrap_or(&polls::POLL_DURATIONS[DEFAULT_DURATION]),
    };
    // The canonical payload is at most 1800 bytes (`encodePollCreatePayload`).
    polls::encode_poll_create(&input).map_err(|_| "poll.aggregateValidation")?;
    Ok(input)
}

/// Whether the Bot of `community` offers poll creation here (`supportedOperations.includes(70)`: its catalog holds the
/// sealed operation of v1 operation 70, operations.ts:48).
fn offers_polls(state: &AppState, community: CommunityId) -> bool {
    state.bot.catalogs.get(&community).is_some_and(|c| {
        c.commands
            .iter()
            .any(|command| u64::from(command.operation) == sealed_operation(POLL_CREATE))
    })
}

/// `canCreateFromContext` and the Bot's capabilities (UtilityCommandProvider.tsx:354-362): the community that may hold the
/// poll, or why it may not. The channel's bits are the viewer's resolved ones; none known (`bits ?? 0`) is no send.
pub fn can_open(
    state: &AppState,
    community: Option<CommunityId>,
    channel: ChannelId,
) -> Result<CommunityId, &'static str> {
    let community = community.ok_or(copy::POLL_NOT_HERE)?;
    let may_send = state
        .threads
        .channel_bits
        .get(&channel)
        .is_some_and(|bits| bits & SEND_MESSAGES != 0);
    if !may_send {
        return Err(copy::POLL_NOT_HERE);
    }
    if !offers_polls(state, community) {
        return Err(copy::POLL_NOT_OFFERED);
    }
    Ok(community)
}

/// The form of one window: the fields' inputs, the choices and the post in flight. Its Debug gives no text.
pub struct PollForm {
    pub question: Entity<InputState>,
    pub answers: Vec<Entity<InputState>>,
    /// The chosen duration's index in [`DURATIONS`].
    pub duration: usize,
    pub multi: bool,
    /// Why the last Post did not go (a field's refusal, or the Bot's).
    pub error: Option<String>,
    /// A Post is out: Post is disabled, and Enter sends nothing.
    pub busy: bool,
    /// The form's one request id: every Post of this form sends it. It is the command's remembered id (Task 30) until a
    /// definitive refusal, after which the next Post is a new request.
    pub request: [u8; 16],
    /// The command's invocation key and draft, which an uncertain answer keeps in the command memory with the request.
    invocation: String,
    draft: String,
    /// The command's row, once a Post made it pending.
    pub row: Option<u64>,
    pub conversation: Conversation,
    pub community: CommunityId,
    /// `None` in a fixture: its Post does nothing.
    pub client: Option<Rc<Client>>,
    /// The Bot user the community's catalog named when the form opened (adopted when it first resolves): another one
    /// closes the form (UtilityCommandProvider.tsx:322-329).
    pub bot_user: Option<UserId>,
    /// The question's subscription first, then each answer's.
    subs: Vec<Subscription>,
    /// The post is over and the form is to go (the answer closed it).
    done: bool,
}

impl std::fmt::Debug for PollForm {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PollForm")
            .field("answers", &self.answers.len())
            .field("duration", &self.duration)
            .field("multi", &self.multi)
            .field("error", &self.error.is_some())
            .field("busy", &self.busy)
            .field("request", &self.request)
            .field("row", &self.row)
            .field("conversation", &self.conversation)
            .field("community", &self.community)
            .field("client", &self.client.is_some())
            .finish_non_exhaustive()
    }
}

impl PollForm {
    /// A Post asks to go: the form's request id when none is out, else `None` (a second press while one is out).
    pub fn begin_post(&mut self) -> Option<[u8; 16]> {
        if self.busy {
            return None;
        }
        self.busy = true;
        Some(self.request)
    }

    /// The post is answered: Post is enabled again. `keep_open` is false when the answer closed the form. The id is kept
    /// only by an answer that may have taken effect (the form then closes, and a resend settles that request): after any
    /// definitive answer the next Post is a new request, as CommandHost.tsx:301-303 mints a fresh id (the id is bound to
    /// the body, and a stored refusal would be answered again).
    pub fn end_post(&mut self, keep_open: bool) {
        self.busy = false;
        if keep_open {
            self.request = dilla_client::features::roles::request_id();
        } else {
            self.done = true;
        }
    }
}

/// The form a command asked for, until its window draws it (an input is made with a window).
struct Wanted {
    opening: Opening,
    conversation: Conversation,
    community: CommunityId,
    client: Option<Rc<Client>>,
    attempt: Attempt,
    bot_user: Option<UserId>,
}

#[derive(Default)]
struct Slot {
    wanted: Option<Wanted>,
    form: Option<PollForm>,
}

fn slot(window: WindowId, cx: &mut App) -> &mut Slot {
    crate::per_window::get_mut::<Slot>(window, cx)
}

/// `window` forgets its form (its client was released): nothing typed stays, nothing is sent.
pub fn forget_window(window: WindowId, cx: &mut App) {
    crate::per_window::set::<Slot>(window, None, cx);
}

/// Whether `window` holds a poll form.
pub fn is_open(window: WindowId, cx: &App) -> bool {
    crate::per_window::get::<Slot>(window, cx)
        .is_some_and(|s| s.wanted.is_some() || s.form.is_some())
}

/// The form of `window`, once it was drawn.
pub fn form_of(window: WindowId, cx: &App) -> Option<&PollForm> {
    crate::per_window::get::<Slot>(window, cx)?.form.as_ref()
}

/// The row of a command that did not run: `text` as Failed. A fixture keeps it in the window.
fn fail(ctx: &SlotCx, window: WindowId, conversation: Conversation, text: &str, cx: &mut App) {
    match &ctx.client {
        Some(client) => {
            client.bot().add_result(
                conversation,
                ResultStatus::Failed,
                text.to_owned(),
                ResultProvider::Builtin,
            );
        }
        None => {
            let row = add_fixture_row(window, conversation, ctx.clock, text.to_owned(), cx);
            for (_, r) in &mut crate::per_window::get_mut::<FixtureRows>(window, cx).0 {
                if r.id == row {
                    r.status = ResultStatus::Failed;
                }
            }
        }
    }
}

/// `/poll create` was sent from the composer: the form opens with the typed arguments, or the command's row says why it
/// cannot (Failed). The form is drawn from the next frame ([`overlay`]). A run whose request already sent a poll that may
/// have been made reconciles it instead, without the form ([`reconcile`]).
pub fn open(ctx: &SlotCx, args: &Args, attempt: Attempt, window: WindowId, cx: &mut App) {
    let Some(channel) = ctx.channel else {
        return;
    };
    let conversation = Conversation {
        channel,
        thread: None,
    };
    if let Some(client) = &ctx.client {
        let prior = super::with_memory(client, |m| m.polls.get(&attempt.request).cloned());
        if let Some(prior) = prior {
            return reconcile(ctx, client, prior, attempt, window, cx);
        }
    }
    match can_open(&ctx.state, ctx.community, channel) {
        Err(why) => fail(ctx, window, conversation, why, cx),
        Ok(community) => {
            slot(window, cx).wanted = Some(Wanted {
                opening: opening(args),
                conversation,
                community,
                client: ctx.client.clone(),
                attempt,
                bot_user: ctx
                    .state
                    .bot
                    .catalogs
                    .get(&community)
                    .and_then(|c| c.bot_user),
            });
        }
    }
    cx.refresh_windows();
}

/// UtilityCommandProvider.tsx:366, :368 (Task 30): a poll create that may have been made settles that request. The
/// mutation it went out with is immutable, so the form does not open: the saved poll is sent again under the same id (the
/// web's `read`, which resends what its page sent), and the Bot answers what it did. A record that matches it and whose
/// post is in the conversation ends it made; a refusal of the change ends it rejected; anything else stays uncertain.
/// Another conversation, community or Bot, or a channel where polls cannot be made now, is uncertain at once.
fn reconcile(
    ctx: &SlotCx,
    client: &Rc<Client>,
    prior: PollAttempt,
    attempt: Attempt,
    window: WindowId,
    cx: &mut App,
) {
    let conversation = prior.at;
    let row = client.bot().add_result(
        conversation,
        ResultStatus::Pending,
        copy::pending("poll create"),
        ResultProvider::Builtin,
    );
    let uncertain = || Outcome::Uncertain(t("poll.createdUncertain").to_owned());
    let pinned = client
        .utilities()
        .bot_identity(prior.community)
        .map(|b| b.user);
    if ctx.channel != Some(conversation.channel)
        || ctx.community != Some(prior.community)
        || can_open(&ctx.state, ctx.community, conversation.channel).is_err()
        || pinned != Some(prior.bot_user)
    {
        return super::settle(
            client,
            window,
            (conversation, row),
            &attempt,
            uncertain(),
            cx,
        );
    }
    let (community, request, value) = (prior.community, attempt.request, prior.value.clone());
    let thread = prior.value.thread.map(dilla_client::state::MessageId);
    let job = client.utilities().run(move |u| async move {
        let resent = u.create_poll(community, value, request).await;
        if matches!(resent, Err(UtilityError::Status(4))) {
            return Ok(Err(4));
        }
        let held = u
            .poll_by_id(community, request, conversation.channel, thread)
            .await;
        Ok(Ok(held))
    });
    let client = client.clone();
    cx.spawn(async move |cx| {
        let answer = job.await;
        cx.update(|cx| {
            let state = client.state().borrow().clone();
            let outcome = match answer {
                Ok(Err(_)) => {
                    super::with_memory(&client, |m| m.drop_poll(request));
                    Outcome::Failure(
                        "Poll creation was rejected. Start a new poll command.".to_owned(),
                    )
                }
                Ok(Ok(Ok(poll)))
                    if poll.question == prior.value.question
                        && poll.answers == prior.value.answers
                        && poll.multi == prior.value.multi
                        && published(&state, conversation.channel, prior.bot_user, request) =>
                {
                    super::with_memory(&client, |m| m.drop_poll(request));
                    dilla_desktop_ui::components::toast::show_toast_in(
                        Some(window),
                        t("poll.created"),
                        cx,
                    );
                    Outcome::Success(t("poll.created").to_owned())
                }
                _ => uncertain(),
            };
            super::settle(&client, window, (conversation, row), &attempt, outcome, cx);
        });
    })
    .detach();
}

/// `pollPublished` (UtilityCommandProvider.tsx:59-66): the conversation holds the Bot's post of the poll `id`: an
/// unedited post of `bot_user`, as a Bot, whose last line is `[dilla-poll:v1:{id}]`.
fn published(state: &AppState, channel: ChannelId, bot_user: UserId, id: [u8; 16]) -> bool {
    state.timelines.get(&channel).is_some_and(|t| {
        t.items.iter().any(|item| {
            item.state == dilla_client::state::TimelineItemState::Ok
                && !item.edited
                && item.bot
                && item.sender_user == Some(bot_user)
                && item.msg_id.is_some()
                && polls::final_poll_marker(&item.body) == Some(id)
        })
    })
}

/// One of the form's inputs: a change clears the form's refusal.
fn input_of(
    w: WindowId,
    placeholder: &str,
    value: &str,
    window: &mut Window,
    cx: &mut App,
) -> (Entity<InputState>, Subscription) {
    let placeholder = placeholder.to_owned();
    let input = cx.new(|cx| InputState::new(window, cx).placeholder(placeholder));
    if !value.is_empty() {
        input.update(cx, |s, cx| s.set_value(value.to_owned(), window, cx));
    }
    let sub = cx.subscribe(&input, move |_, event: &InputEvent, cx| match event {
        InputEvent::Change => {
            if let Some(form) = slot(w, cx).form.as_mut() {
                form.error = None;
            }
            cx.refresh_windows();
        }
        // The web's fields have no Enter handler (PollDialog.tsx): only Post sends.
        InputEvent::PressEnter { .. } | InputEvent::Focus | InputEvent::Blur => {}
    });
    (input, sub)
}

/// `answerPlaceholders[Math.min(index, 2)]`.
fn answer_placeholder(index: usize) -> &'static str {
    t([
        "poll.answerFirst",
        "poll.answerSecond",
        "poll.answerAnother",
    ][index.min(2)])
}

fn make(w: WindowId, wanted: Wanted, window: &mut Window, cx: &mut App) -> PollForm {
    let (question, question_sub) = input_of(
        w,
        t("poll.questionPlaceholder"),
        &wanted.opening.question,
        window,
        cx,
    );
    let mut subs = vec![question_sub];
    let mut answers = Vec::new();
    for (i, value) in wanted.opening.answers.iter().enumerate() {
        let (input, sub) = input_of(w, answer_placeholder(i), value, window, cx);
        answers.push(input);
        subs.push(sub);
    }
    PollForm {
        question,
        answers,
        duration: wanted.opening.duration,
        multi: wanted.opening.multi,
        error: None,
        busy: false,
        request: wanted.attempt.request,
        invocation: wanted.attempt.invocation,
        draft: wanted.attempt.text,
        row: None,
        conversation: wanted.conversation,
        community: wanted.community,
        client: wanted.client,
        bot_user: wanted.bot_user,
        subs,
        done: false,
    }
}

/// The form's fields as typed: the question, the answers (blank ones too), the choices and the channel.
struct Typed {
    question: String,
    answers: Vec<String>,
    multi: bool,
    duration: usize,
    channel: [u8; 16],
}

fn typed(w: WindowId, cx: &mut App) -> Option<Typed> {
    let form = slot(w, cx).form.as_ref()?;
    let (question, answers) = (form.question.clone(), form.answers.clone());
    let (multi, duration, channel) = (form.multi, form.duration, form.conversation.channel.0);
    let value = |input: &Entity<InputState>| input.read(cx).value().to_string();
    Some(Typed {
        question: value(&question),
        answers: answers.iter().map(value).collect(),
        multi,
        duration,
        channel,
    })
}

/// Post, or Enter in a field: checks the fields, then sends the form's one request (`PollEditor.submit` and
/// `handlers['poll create']`). A fixture has no client to send with, so its Post does nothing.
pub fn submit(w: WindowId, cx: &mut App) {
    let Some(typed) = typed(w, cx) else {
        return;
    };
    if slot(w, cx).form.as_ref().is_some_and(|f| f.busy) {
        return;
    }
    let input = match validate(
        &typed.question,
        &typed.answers,
        typed.multi,
        typed.duration,
        typed.channel,
    ) {
        Ok(input) => input,
        Err(key) => {
            if let Some(form) = slot(w, cx).form.as_mut() {
                form.error = Some(t(key).to_owned());
            }
            cx.refresh_windows();
            return;
        }
    };
    let Some(form) = slot(w, cx).form.as_mut() else {
        return;
    };
    form.error = None;
    let Some(client) = form.client.clone() else {
        return;
    };
    let community = form.community;
    // :384-386: the attempt is kept just before the create, so a run of the same request reconciles it. It is bound to
    // the Bot user the community pins (the web's `installationId`); with none pinned nothing could be sealed to the Bot,
    // and no poll goes out unguarded.
    let Some(bot_user) = form
        .bot_user
        .or_else(|| client.utilities().bot_identity(community).map(|b| b.user))
    else {
        form.error = Some(copy::POLL_BOT_UNAVAILABLE.to_owned());
        cx.refresh_windows();
        return;
    };
    let Some(request) = form.begin_post() else {
        return;
    };
    let conversation = form.conversation;
    let attempt = Attempt {
        request,
        invocation: form.invocation.clone(),
        text: form.draft.clone(),
    };
    let kept = PollAttempt {
        at: conversation,
        community,
        bot_user,
        value: input.clone(),
    };
    super::with_memory(&client, |m| m.keep_poll(request, kept));
    // The row is pending from the post (`startResult`, :379); a second Post after a refusal makes a second row.
    let row = client.bot().add_result(
        conversation,
        ResultStatus::Pending,
        copy::pending("poll create"),
        ResultProvider::Builtin,
    );
    form.row = Some(row);
    // The web closes the dialog as it submits (the row carries the rest): the form goes on the next frame.
    form.done = true;
    let job = client
        .utilities()
        .run(move |u| async move { u.create_poll(community, input, request).await });
    cx.spawn(async move |cx| {
        let answer = job.await.map(|_| ());
        cx.update(|cx| settled(w, (&client, conversation, row), &attempt, answer, cx));
    })
    .detach();
    cx.refresh_windows();
}

/// How a refused create reads in its row (`caught`, UtilityCommandProvider.tsx:76-85).
fn refusal(e: &UtilityError) -> &'static str {
    match e {
        UtilityError::Unavailable(_) => copy::POLL_BOT_UNAVAILABLE,
        UtilityError::Status(4) => copy::POLL_RECORD_CHANGED,
        UtilityError::Status(2 | 3) => copy::POLL_NO_LONGER_AVAILABLE,
        UtilityError::Payload => copy::POLL_TOO_LARGE,
        _ => copy::POLL_CHECK_FIELDS,
    }
}

/// What the Bot answered a Post: the row's end, and the form's. The row is finished even if the form was cancelled while
/// the post was out, and the form is only touched while it is still the one that sent `request`.
fn settled(
    w: WindowId,
    sent: (&Rc<Client>, Conversation, u64),
    attempt: &Attempt,
    answer: Result<(), UtilityError>,
    cx: &mut App,
) {
    let (client, conversation, row) = sent;
    let request = attempt.request;
    let (outcome, text, keep_open) = match &answer {
        Ok(()) => (
            Outcome::Success(t("poll.created").to_owned()),
            t("poll.created"),
            false,
        ),
        // A mutation that may have happened: the form goes, and the attempt stays so a resend of the same request
        // reconciles it.
        Err(UtilityError::Uncertain { .. }) => (
            Outcome::Uncertain(t("poll.createdUncertain").to_owned()),
            t("poll.createdUncertain"),
            false,
        ),
        Err(e) => (Outcome::Failure(refusal(e).to_owned()), refusal(e), true),
    };
    // :392-394: an accepted create, and a refusal of a first attempt, release it.
    if !matches!(answer, Err(UtilityError::Uncertain { .. })) {
        super::with_memory(client, |m| m.drop_poll(request));
    }
    super::settle(client, w, (conversation, row), attempt, outcome, cx);
    if answer.is_ok() {
        dilla_desktop_ui::components::toast::show_toast_in(Some(w), t("poll.created"), cx);
    }
    if let Some(form) = slot(w, cx).form.as_mut()
        && form.request == request
    {
        form.end_post(keep_open);
        form.error = keep_open.then(|| text.to_owned());
    }
    cx.refresh_windows();
}

/// Cancel, Escape, a click outside: the form goes and leaves no row. A post already out finishes its own row.
pub fn cancel(w: WindowId, cx: &mut App) {
    let s = slot(w, cx);
    s.wanted = None;
    s.form = None;
    cx.refresh_windows();
}

/// The labels of PollDialog (`pollDialogLabels`), filled in for this form.
fn labels(channel: &str, count: usize) -> poll_dialog::Labels {
    poll_dialog::Labels {
        title: if channel.is_empty() {
            t("poll.titleWithoutChannel").to_owned()
        } else {
            tf("poll.title", &[("channel", channel)])
        }
        .into(),
        close: t("poll.dismiss").into(),
        cancel: t("poll.cancel").into(),
        post: t("poll.post").into(),
        question: t("poll.question").into(),
        counter: tf("poll.counter", &[("n", &count.to_string())]).into(),
        answers: t("poll.answers").into(),
        answer_range: t("poll.answerRange").into(),
        add_answer: t("poll.addAnswer").into(),
        duration: t("poll.duration").into(),
        multi: t("poll.multiple").into(),
        footer: t("poll.footer").into(),
        durations: DURATIONS.map(|d| t(&format!("poll.duration.{d}")).into()),
    }
}

/// An input in PollDialog's frame (`.d-poll-dialog input`): `--surface-2`, a field edge, 7px corners; over its limit its
/// edge is `--danger` (the text stays `--fg`), and a focused field's edge is `--accent` (PollDialog.css `input:focus`);
/// the question's focus also has a 3px ring.
fn framed(
    input: &Entity<InputState>,
    label: String,
    (height, pad_x): (f32, f32),
    over: bool,
    ring: bool,
    window: &Window,
    cx: &App,
) -> impl IntoElement {
    let focused = input.focus_handle(cx).contains_focused(window, cx);
    let frame = div()
        .flex()
        .items_center()
        .w_full()
        .h(px(height))
        .px(px(pad_x))
        .bg(color::SURFACE_2)
        .border_1()
        .border_color(if over {
            color::DANGER
        } else {
            text_field::edge(false, focused, false)
        })
        .rounded(radius::SEG)
        .text_size(text::INPUT)
        .font_family(font::MONO)
        .text_color(color::FG)
        .child(
            Input::new(input)
                .aria_label(label)
                .appearance(false)
                .text_size(text::INPUT)
                .px_0()
                .py_0(),
        );
    if ring && focused {
        frame.shadow(spread_ring(color::ACCENT_RING, QUESTION_RING))
    } else {
        frame
    }
}

/// The form over the window (`slots::overlays`), while one is open.
pub fn overlay(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let w = window.window_handle().window_id();
    if let Some(wanted) = slot(w, cx).wanted.take() {
        let form = make(w, wanted, window, cx);
        slot(w, cx).form = Some(form);
    }
    let form = slot(w, cx).form.as_ref()?;
    // Closed by its answer.
    if form.done {
        slot(w, cx).form = None;
        return None;
    }
    // Leaving the conversation the command was typed in closes the form, nothing recorded (CommandHost's `isCurrent`).
    if ctx.channel != Some(form.conversation.channel) {
        slot(w, cx).form = None;
        return None;
    }
    // The Bot stopped offering polls, or is another Bot, while the form was open (:322-329, :376): the form goes, and the
    // row says so. A post already out is answered on its own.
    let held_user = ctx
        .state
        .bot
        .catalogs
        .get(&form.community)
        .and_then(|c| c.bot_user);
    // A pin unknown at open is adopted when it first resolves; only a change from a known user closes the form.
    let changed = form.bot_user.is_some() && held_user.is_some() && held_user != form.bot_user;
    if !form.busy && (changed || !offers_polls(&ctx.state, form.community)) {
        let conversation = form.conversation;
        slot(w, cx).form = None;
        fail(ctx, w, conversation, copy::POLL_STOPPED_OFFERING, cx);
        return None;
    }
    if form.bot_user.is_none() && held_user.is_some() {
        let adopted = held_user;
        if let Some(f) = slot(w, cx).form.as_mut() {
            f.bot_user = adopted;
        }
    }
    let form = slot(w, cx).form.as_ref()?;
    let (question, answers) = (form.question.clone(), form.answers.clone());
    let (duration, multi, busy, error) = (form.duration, form.multi, form.busy, form.error.clone());
    let question_text = question.read(cx).value().to_string();
    let answer_texts: Vec<String> = answers
        .iter()
        .map(|a| a.read(cx).value().to_string())
        .collect();
    let count = question_text.chars().count();
    let channel = ctx
        .state
        .channel(ctx.channel?)
        .map(|c| c.name.clone())
        .unwrap_or_default();
    let can_post = !busy
        && !question_text.trim().is_empty()
        && answer_texts.iter().filter(|a| !a.trim().is_empty()).count() >= MIN_ANSWERS;
    let bot = picker::bot_name(&ctx.state, ctx.community);
    let mut dialog = PollDialog::new(
        "poll-dialog",
        bot,
        labels(&channel, count),
        crate::tracked_focus(
            "poll-question",
            &question.focus_handle(cx),
            framed(
                &question,
                t("poll.question").to_owned(),
                (QUESTION_H, QUESTION_PAD_X),
                false,
                true,
                window,
                cx,
            ),
        ),
    )
    .question_over(count > QUESTION_MAX)
    .duration(duration)
    .multi(multi)
    .can_post(can_post)
    .error(error.map(Into::into))
    .initial_focus(InitialFocus::Handle(question.focus_handle(cx)))
    .post_wrap(|post| crate::tracked_box("poll-post", post).into_any_element())
    .on_cancel(move |_, cx| cancel(w, cx))
    .on_post(move |_, cx| submit(w, cx))
    .on_multi(move |_, cx| {
        if let Some(form) = slot(w, cx).form.as_mut() {
            form.multi = !form.multi;
            form.error = None;
        }
        cx.refresh_windows();
    })
    .on_duration(move |i, _, cx| {
        if let Some(form) = slot(w, cx).form.as_mut() {
            form.duration = i;
            form.error = None;
        }
        cx.refresh_windows();
    })
    .on_add(move |window, cx| add_answer(w, window, cx))
    .on_remove(move |i, window, cx| remove_answer(w, i, window, cx));
    for (i, (input, value)) in answers.iter().zip(&answer_texts).enumerate() {
        let over = value.chars().count() > ANSWER_MAX;
        let field = framed(
            input,
            tf("poll.answerLabel", &[("number", &(i + 1).to_string())]),
            (ANSWER_H, ANSWER_PAD_X),
            over,
            false,
            window,
            cx,
        );
        // For automation: each answer's field, by its row (the rows past ten do not exist).
        let tracked = crate::tracked_focus(
            ANSWER_IDS.get(i).copied().unwrap_or("poll-answer"),
            &input.focus_handle(cx),
            field,
        );
        dialog = dialog.answer(tracked, over);
    }
    Some(dialog.into_any_element())
}

/// Add answer: one more row, up to ten.
fn add_answer(w: WindowId, window: &mut Window, cx: &mut App) {
    let Some(rows) = slot(w, cx).form.as_ref().map(|f| f.answers.len()) else {
        return;
    };
    if rows >= MAX_ANSWERS {
        return;
    }
    let (input, sub) = input_of(w, answer_placeholder(rows), "", window, cx);
    if let Some(form) = slot(w, cx).form.as_mut() {
        form.answers.push(input);
        form.subs.push(sub);
        form.error = None;
    }
    cx.refresh_windows();
}

/// Remove answer `index`: never below two rows; the focus goes to the row before it (`removedFocus`).
fn remove_answer(w: WindowId, index: usize, window: &mut Window, cx: &mut App) {
    let Some(form) = slot(w, cx).form.as_mut() else {
        return;
    };
    if form.answers.len() <= MIN_ANSWERS || index >= form.answers.len() {
        return;
    }
    form.answers.remove(index);
    drop(form.subs.remove(index + 1));
    form.error = None;
    let next = form.answers[index.saturating_sub(1)].clone();
    window.focus(&next.focus_handle(cx), cx);
    cx.refresh_windows();
}

#[cfg(test)]
mod tests {
    use super::*;

    const CHANNEL: [u8; 16] = [7; 16];

    fn rows(items: &[&str]) -> Vec<String> {
        items.iter().map(|s| (*s).to_owned()).collect()
    }

    /// PollEditor.submit's order and limits (UtilityCommandProvider.tsx:131-139), by the strings key each shows.
    #[test]
    fn poll_form_validates_as_the_web() {
        let ok = rows(&["x", "y"]);
        let check = |q: &str, a: &[String]| validate(q, a, false, 3, CHANNEL).map(|_| ());
        assert_eq!(check("", &ok), Err("poll.questionRequired"));
        assert_eq!(check("  ", &ok), Err("poll.questionRequired"));
        assert_eq!(check(&"a".repeat(301), &ok), Err("poll.questionLong"));
        assert_eq!(check("q", &rows(&["x", " "])), Err("poll.answersRequired"));
        assert_eq!(
            check("q", &rows(&["x", &"a".repeat(101)])),
            Err("poll.answerLong")
        );
        assert_eq!(check("q", &rows(&["a", "a"])), Err("poll.duplicates"));
        assert_eq!(
            check("q", &rows(&["a", " a "])),
            Err("poll.duplicates"),
            "compared trimmed"
        );
        let eleven: Vec<String> = (0..11).map(|i| format!("answer {i}")).collect();
        assert_eq!(check("q", &eleven), Err("poll.duplicates"));
        // Ten answers of 100 characters and a 300-character question are each within their limits (in ASCII they would also
        // fit the payload's 1800 bytes: 1300), but two-byte letters take the payload over it.
        let long: Vec<String> = (0..10).map(|i| format!("{i}{}", "é".repeat(99))).collect();
        assert_eq!(
            check(&"é".repeat(300), &long),
            Err("poll.aggregateValidation")
        );
        // The answers' order is the person's, the blank ones go, and the default duration is 24 hours.
        let good = validate(" Raid? ", &rows(&[" b ", "", "a"]), true, 3, CHANNEL).unwrap();
        assert_eq!(good.question, "Raid?");
        assert_eq!(good.answers, ["b", "a"]);
        assert_eq!(
            (good.duration, good.multi, good.thread),
            (86_400, true, None)
        );
        assert_eq!(good.channel, CHANNEL);
        assert_eq!(
            validate("q", &ok, false, 0, CHANNEL).unwrap().duration,
            3600
        );
    }

    #[test]
    fn the_answers_argument_prefills_rows() {
        let args = |pairs: &[(&str, &str)]| -> Args {
            pairs
                .iter()
                .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
                .collect()
        };
        assert_eq!(
            opening(&args(&[("answers", "red, green ,blue")])).answers,
            ["red", "green", "blue"]
        );
        // Never fewer than two rows (:374), and an empty segment stays a blank row.
        assert_eq!(opening(&args(&[])).answers, ["", ""]);
        assert_eq!(opening(&args(&[("answers", "solo")])).answers, ["solo", ""]);
        assert_eq!(
            opening(&args(&[("answers", "a,,b")])).answers,
            ["a", "", "b"]
        );
        let all = opening(&args(&[
            ("question", "Raid?"),
            ("multiple", "on"),
            ("duration", "7d"),
        ]));
        assert_eq!(
            (all.question.as_str(), all.multi, all.duration),
            ("Raid?", true, 5)
        );
        let defaults = opening(&args(&[("duration", "5y"), ("multiple", "off")]));
        assert_eq!(
            (defaults.multi, defaults.duration),
            (false, DEFAULT_DURATION)
        );
    }

    fn form(cx: &mut gpui::TestAppContext) -> PollForm {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        window.update(|window, cx| {
            make(
                w,
                Wanted {
                    opening: Opening {
                        question: "the secret question".into(),
                        answers: rows(&["the secret answer", ""]),
                        duration: DEFAULT_DURATION,
                        multi: false,
                    },
                    conversation: Conversation {
                        channel: ChannelId(CHANNEL),
                        thread: None,
                    },
                    community: CommunityId([1; 16]),
                    client: None,
                    attempt: Attempt {
                        request: [9; 16],
                        invocation: String::new(),
                        text: String::new(),
                    },
                    bot_user: None,
                },
                window,
                cx,
            )
        })
    }

    /// While one post is out a second sends nothing; after a definitive refusal the next Post is a new request (the web
    /// mints a fresh id), and only an answer that closes the form (uncertain) keeps its id.
    #[gpui::test]
    fn a_second_post_while_busy_sends_nothing(cx: &mut gpui::TestAppContext) {
        let mut form = form(cx);
        assert_eq!(form.begin_post(), Some([9; 16]));
        assert!(form.busy);
        assert_eq!(form.begin_post(), None, "a second press while it is out");
        form.end_post(true);
        assert!(!form.busy && !form.done);
        let fresh = form.begin_post().expect("idle again");
        assert_ne!(fresh, [9; 16], "a new request after a refusal");
        form.end_post(false);
        assert!(form.done, "an answer that closes the form");
        assert_eq!(form.request, fresh, "an uncertain post keeps its id");
    }

    /// R20/R21: the form holds decrypted content; its Debug and the opening's say how much, never what.
    #[gpui::test]
    fn the_form_keeps_its_text_out_of_debug(cx: &mut gpui::TestAppContext) {
        let form = form(cx);
        let opening = Opening {
            question: "the secret question".into(),
            answers: rows(&["the secret answer", ""]),
            duration: 3,
            multi: false,
        };
        for shown in [
            format!("{form:?}"),
            format!("{form:#?}"),
            format!("{opening:?}"),
            format!("{opening:#?}"),
        ] {
            assert!(!shown.contains("secret"), "{shown}");
        }
        assert!(format!("{form:?}").contains("answers: 2"));
    }

    const COMMUNITY: CommunityId = CommunityId([1; 16]);

    /// A community whose Bot offers the `poll create` command, and poll creation to the app (sealed operation 170) when
    /// `offers`; the viewer may send in the channel when `may_send`.
    fn world(offers: bool, may_send: bool) -> AppState {
        use dilla_client::state::bot::{
            ArgShape, ArgType, BotCatalogView, BotCommandSpec, BotStatus, BotStatusState,
        };
        let spec = |name: &str, operation: u32, args: &[(&str, ArgType)]| BotCommandSpec {
            name: name.into(),
            description: String::new(),
            operation,
            args: args
                .iter()
                .map(|&(n, ty)| ArgShape {
                    name: n.into(),
                    ty,
                    required: false,
                })
                .collect(),
            permission_bit: 0,
            needs_bot_channel: false,
            handler_kind: 0,
        };
        let mut s = AppState::default();
        let mut commands = vec![spec(
            "poll create",
            70,
            &[
                ("question", ArgType::Text),
                ("answers", ArgType::Text),
                ("multiple", ArgType::Bool),
                ("duration", ArgType::Duration),
            ],
        )];
        if offers {
            commands.push(spec("poll app", 170, &[]));
        }
        s.bot.catalogs.insert(
            COMMUNITY,
            BotCatalogView {
                revision: 1,
                commands,
                status: BotStatus {
                    state: BotStatusState::Ready,
                    since: None,
                },
                bot_channels: Some(vec![ChannelId(CHANNEL)]),
                bot_user: None,
                fingerprint: None,
            },
        );
        s.threads.channel_bits.insert(
            ChannelId(CHANNEL),
            if may_send { 1 | SEND_MESSAGES } else { 1 },
        );
        s
    }

    fn ctx(state: AppState, channel: [u8; 16]) -> SlotCx {
        SlotCx {
            state: std::sync::Arc::new(state),
            community: Some(COMMUNITY),
            channel: Some(ChannelId(channel)),
            ..SlotCx::default()
        }
    }

    /// The window's own rows (a fixture has no client to keep them): status and text.
    fn held_rows(w: WindowId, cx: &App) -> Vec<(ResultStatus, String)> {
        crate::per_window::get::<FixtureRows>(w, cx).map_or(Vec::new(), |r| {
            r.0.iter()
                .map(|(_, r)| (r.status, r.text.clone()))
                .collect()
        })
    }

    /// UtilityCommandProvider.tsx:354-362: the form opens only in a channel where this member may send and whose Bot offers
    /// polls; elsewhere the command's row says why. Opened, it draws with the arguments typed (the answers split at their
    /// commas) and Cancel leaves no row (CommandHost.tsx:297).
    #[gpui::test]
    fn poll_create_opens_the_form_only_where_the_web_does(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        let encode = |t: &str| t.to_owned();
        let draft =
            r#"/poll create question:Raid? answers:"red, green ,blue" multiple:on duration:7d"#;
        for (offers, may_send, why) in [
            (true, false, Some("Create a poll in a community channel.")),
            (
                false,
                true,
                Some("The Bot cannot create polls in this channel."),
            ),
            (true, true, None),
        ] {
            let ctx = ctx(world(offers, may_send), CHANNEL);
            window.update(|_, cx| {
                assert_eq!(
                    super::super::on_send(&ctx, draft, &encode, w, cx),
                    super::super::SendDecision::Handled,
                    "the draft is taken either way"
                );
                match why {
                    Some(why) => {
                        assert!(!is_open(w, cx));
                        assert_eq!(held_rows(w, cx), [(ResultStatus::Failed, why.to_owned())]);
                    }
                    None => {
                        assert!(is_open(w, cx));
                        assert!(
                            held_rows(w, cx).is_empty(),
                            "the form has no row before Post"
                        );
                    }
                }
            });
            if why.is_some() {
                window.update(|_, cx| crate::per_window::get_mut::<FixtureRows>(w, cx).0.clear());
            }
        }
        let ctx = ctx(world(true, true), CHANNEL);
        window.update(|window, cx| {
            assert!(overlay(&ctx, window, cx).is_some(), "the form is drawn");
            let form = form_of(w, cx).expect("the form was made");
            let typed = |i: &Entity<InputState>| i.read(cx).value().to_string();
            assert_eq!(typed(&form.question), "Raid?");
            let answers: Vec<String> = form.answers.iter().map(typed).collect();
            assert_eq!(answers, ["red", "green", "blue"]);
            assert_eq!((form.duration, form.multi), (5, true));
            cancel(w, cx);
            assert!(!is_open(w, cx));
            assert!(overlay(&ctx, window, cx).is_none());
        });
        window.update(|_, cx| {
            assert_eq!(held_rows(w, cx).len(), 0, "Cancel leaves nothing");
        });
    }

    /// UtilityCommandProvider.tsx:322-329: a catalog naming another Bot user closes the form like a withdrawn one (a pin
    /// unknown at open is adopted, not a change).
    #[gpui::test]
    fn the_form_closes_when_the_catalog_names_another_bot(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        let with_bot = |user: Option<u8>| {
            let mut state = world(true, true);
            state.bot.catalogs.get_mut(&COMMUNITY).unwrap().bot_user =
                user.map(|b| UserId([b; 16]));
            ctx(state, CHANNEL)
        };
        window.update(|window, cx| {
            // Unknown at open, then resolved: adopted, the form stays.
            super::super::on_send(&with_bot(None), "/poll create", &|t| t.to_owned(), w, cx);
            assert!(overlay(&with_bot(None), window, cx).is_some());
            assert!(overlay(&with_bot(Some(1)), window, cx).is_some());
            assert!(overlay(&with_bot(Some(1)), window, cx).is_some());
            // Another Bot: the form goes, and the row says the Bot stopped offering polls.
            assert!(overlay(&with_bot(Some(2)), window, cx).is_none());
            assert!(!is_open(w, cx));
            assert_eq!(
                held_rows(w, cx),
                [(
                    ResultStatus::Failed,
                    "The Bot stopped offering polls. Try again.".to_owned()
                )]
            );
        });
    }

    /// A refused Post names why in the form and sends nothing; with no client (a fixture) a good Post does nothing at all.
    #[gpui::test]
    fn post_checks_the_fields_and_a_fixture_sends_nothing(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        let ctx = ctx(world(true, true), CHANNEL);
        window.update(|_, cx| {
            super::super::on_send(&ctx, "/poll create", &|t| t.to_owned(), w, cx);
        });
        window.update(|window, cx| {
            overlay(&ctx, window, cx);
            submit(w, cx);
            let form = form_of(w, cx).unwrap();
            assert_eq!(form.error.as_deref(), Some("Write a question"));
            assert!(!form.busy);
            let (question, first, second) = (
                form.question.clone(),
                form.answers[0].clone(),
                form.answers[1].clone(),
            );
            question.update(cx, |s, cx| s.set_value("Raid?", window, cx));
            first.update(cx, |s, cx| s.set_value("the bridge", window, cx));
            second.update(cx, |s, cx| s.set_value("the harbour", window, cx));
            submit(w, cx);
            let form = form_of(w, cx).unwrap();
            assert!(!form.busy, "no client: nothing is out");
            assert!(form.row.is_none());
            assert_eq!(held_rows(w, cx).len(), 0);
        });
    }

    /// CommandHost's `isCurrent` and :376: leaving the conversation closes the form with no row; the Bot withdrawing polls
    /// closes it with a Failed row.
    #[gpui::test]
    fn the_form_closes_when_its_conversation_or_its_polls_go(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        let open = |window: &mut gpui::VisualTestContext| {
            window.update(|window, cx| {
                let ctx = ctx(world(true, true), CHANNEL);
                super::super::on_send(&ctx, "/poll create", &|t| t.to_owned(), w, cx);
                assert!(overlay(&ctx, window, cx).is_some());
            });
        };
        open(window);
        window.update(|window, cx| {
            assert!(overlay(&ctx(world(true, true), [8; 16]), window, cx).is_none());
            assert!(!is_open(w, cx), "another conversation");
            assert!(held_rows(w, cx).is_empty());
        });
        open(window);
        window.update(|window, cx| {
            let withdrawn = ctx(world(false, true), CHANNEL);
            assert!(overlay(&withdrawn, window, cx).is_none());
            assert!(!is_open(w, cx), "the Bot stopped offering polls");
            assert_eq!(
                held_rows(w, cx),
                [(
                    ResultStatus::Failed,
                    "The Bot stopped offering polls. Try again.".to_owned()
                )]
            );
        });
    }

    /// The refusals' copy is `caught`'s, by what the Bot answered.
    #[test]
    fn a_refusal_reads_as_the_web_says() {
        assert_eq!(
            refusal(&UtilityError::Unavailable("E_BOT_OFFLINE".into())),
            "The Bot is unavailable in this community."
        );
        assert_eq!(
            refusal(&UtilityError::Status(4)),
            "The record changed. Refresh it and try again."
        );
        for s in [2, 3] {
            assert_eq!(
                refusal(&UtilityError::Status(s)),
                "This action is no longer available."
            );
        }
        assert_eq!(
            refusal(&UtilityError::Payload),
            "The answers use too much space. Shorten them and try again."
        );
        assert_eq!(
            refusal(&UtilityError::Status(1)),
            "Check the fields and try again."
        );
        assert_eq!(
            refusal(&UtilityError::Invalid),
            "Check the fields and try again."
        );
    }
}
