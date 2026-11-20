//! `/announcement list|create|edit|delete` (Task 28; bot-handlers.ts `announcementHandler`, BotCommandDialogs.tsx
//! `AnnouncementEditor`). The web runs the four through its own handler: the Bot keeps the schedules and the handler
//! reads, saves and deletes them with sealed operations 50, 51 and 52. They are not typed catalog commands, which the
//! Bot cannot read as a schedule.
//!
//! - Every command ([`run`]): the command's pending row is added first and says how the command ended. Only in a
//!   community channel where this member holds Manage Community and whose Bot offers announcements ([`check`]), else the
//!   row says why (Failed). The schedules are then read from the Bot.
//! - `list`: the row is the schedules, `{id}: {summary}` a line each, or "No scheduled announcements.".
//! - `delete`: the schedule the `announcement` argument names (else the row says it is no longer available) is asked
//!   about ("Delete this scheduled announcement?"); Cancel or a click outside leaves no row; Continue checks the
//!   permission again and deletes.
//! - `create` and `edit`: the editor opens ([`AnnouncementForm`]), the `text` argument prefilling the message. Save checks the fields in the
//!   web's order ([`validate_schedule`]), then the permission again, and sends the one save of the editor's `parent`
//!   request id; the editor closes as it sends, as the web's dialog does, and the row says what the Bot answered. Cancel,
//!   Escape, a click outside and leaving the conversation end the command with "Announcement editing cancelled.".
//!
//! The form's Debug gives no text: the message is decrypted content (R20, R21).

use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::bot::announcements::{self, Schedule, ScheduleKind, ScheduleRow};
use dilla_client::features::bot::registry::Args;
use dilla_client::features::roles::perm::MANAGE_COMMUNITY;
use dilla_client::features::server_admin::hex_id;
use dilla_client::state::bot::{Conversation, ResultProvider, ResultStatus};
use dilla_client::state::bot_admin::sealed_operation;
use dilla_client::state::{AppState, ChannelId, CommunityId};
use gpui::{
    AnyElement, App, AppContext, Entity, Focusable, InteractiveElement, IntoElement, ParentElement,
    Styled, Subscription, Window, WindowId, div, px,
};
use gpui_component::input::{InputEvent, InputState, TextareaState};

use dilla_desktop_ui::a11y::{Accessible, Live, Role};
use dilla_desktop_ui::components::button::{Button, ButtonSize};
use dilla_desktop_ui::components::confirm_dialog::ConfirmDialog;
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::components::select::Select;
use dilla_desktop_ui::components::settings::frame::SECTION_GAP;
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::text::{line, root_line_height};
use dilla_desktop_ui::tokens::{color, text};

use super::{FixtureRows, add_fixture_row, copy};
use crate::screens::shell::Clock;
use crate::slots::SlotCx;
use crate::strings::t;

/// The v1 operation the Bot lists schedules with: its sealed operation in the catalog means the Bot offers
/// announcements (bot-handlers.ts:388).
const LIST: u64 = announcements::LIST;

/// The four commands.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Command {
    List,
    Create,
    Edit,
    Delete,
}

impl Command {
    /// The command a registry id names.
    pub fn of(id: &str) -> Option<Self> {
        Some(match id {
            "announcement list" => Self::List,
            "announcement create" => Self::Create,
            "announcement edit" => Self::Edit,
            "announcement delete" => Self::Delete,
            _ => return None,
        })
    }

    /// The registry id.
    pub fn id(self) -> &'static str {
        match self {
            Self::List => "announcement list",
            Self::Create => "announcement create",
            Self::Edit => "announcement edit",
            Self::Delete => "announcement delete",
        }
    }
}

// ---- the editor's rules (BotCommandDialogs.tsx:26-70, :103-119) ----

/// What the editor's fields hold, as `AnnouncementEditor.submit` reads them. Its Debug gives no text.
#[derive(Clone, PartialEq, Eq)]
pub struct FormValues {
    pub channel: Option<ChannelId>,
    pub text: String,
    pub kind: ScheduleKind,
    pub date: String,
    pub hour: String,
    pub minute: String,
    pub weekday: String,
    pub timezone: String,
}

impl std::fmt::Debug for FormValues {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("FormValues")
            .field("channel", &self.channel)
            .field("text", &dilla_client::state::Redacted(&self.text))
            .field("kind", &self.kind)
            .finish_non_exhaustive()
    }
}

/// The zone `name` names, spelled as the zone database spells it (`canonicalTimezone`): `None` for a name it does not
/// know.
pub fn canonical_zone(name: &str) -> Option<String> {
    jiff::tz::db()
        .get(name)
        .ok()?
        .iana_name()
        .map(str::to_owned)
}

/// JavaScript's `Number(text)` for the fields that hold a whole number: blank is 0, and a fraction is no integer.
fn whole(text: &str) -> Option<i64> {
    let text = text.trim();
    if text.is_empty() {
        return Some(0);
    }
    let n: f64 = text.parse().ok()?;
    (n.is_finite() && n.fract() == 0.0 && n.abs() < 9e15).then_some(n as i64)
}

/// When `date` (`YYYY-MM-DD`) at `hour`:`minute` happens in `zone` (`occurrence`): `None` when that local minute does not
/// exist there (a clock set forward over it), as the web returns 0. A minute that happens twice is its first.
pub fn occurrence(date: &str, hour: u8, minute: u8, zone: &str) -> Option<jiff::Timestamp> {
    let b = date.as_bytes();
    let digits =
        |r: std::ops::Range<usize>| b.get(r).is_some_and(|d| d.iter().all(u8::is_ascii_digit));
    if b.len() != 10
        || b[4] != b'-'
        || b[7] != b'-'
        || !digits(0..4)
        || !digits(5..7)
        || !digits(8..10)
    {
        return None;
    }
    let part = |r: std::ops::Range<usize>| date[r].parse::<i16>().ok();
    let day = jiff::civil::Date::new(
        part(0..4)?,
        i8::try_from(part(5..7)?).ok()?,
        i8::try_from(part(8..10)?).ok()?,
    )
    .ok()?;
    let local = day.at(i8::try_from(hour).ok()?, i8::try_from(minute).ok()?, 0, 0);
    let zoned = jiff::tz::db()
        .get(zone)
        .ok()?
        .to_ambiguous_zoned(local)
        .compatible()
        .ok()?;
    // A minute skipped over is moved to the other side of the gap: it did not happen.
    (zoned.datetime() == local).then(|| zoned.timestamp())
}

/// `AnnouncementEditor.submit`'s checks, in its order. `Err` is a `strings` key: `botCommand.dialog.invalid` for a field
/// out of its limits, `botCommand.dialog.futureMinute` for a one-off that is not a complete future minute in its zone.
pub fn validate_schedule(
    form: &FormValues,
    now: jiff::Timestamp,
) -> Result<Schedule, &'static str> {
    const INVALID: &str = "botCommand.dialog.invalid";
    const FUTURE: &str = "botCommand.dialog.futureMinute";
    let text = form.text.trim();
    let zone = canonical_zone(&form.timezone);
    let (hour, minute, weekday) = (whole(&form.hour), whole(&form.minute), whole(&form.weekday));
    let in_range = |n: Option<i64>, last: i64| n.filter(|n| (0..=last).contains(n));
    let (Some(channel), Some(hour), Some(minute), Some(weekday), Some(zone)) = (
        form.channel,
        in_range(hour, 23),
        in_range(minute, 59),
        in_range(weekday, 6),
        zone,
    ) else {
        return Err(INVALID);
    };
    if text.is_empty() || text.len() > announcements::TEXT_MAX_BYTES {
        return Err(INVALID);
    }
    let (hour, minute) = (hour as u8, minute as u8);
    if form.kind == ScheduleKind::OneOff {
        let when = occurrence(&form.date, hour, minute, &zone).ok_or(FUTURE)?;
        if when.as_second().div_euclid(60) <= now.as_second().div_euclid(60) {
            return Err(FUTURE);
        }
    }
    Ok(Schedule {
        channel,
        text: text.to_owned(),
        kind: form.kind,
        timezone: zone,
        date: if form.kind == ScheduleKind::OneOff {
            form.date.clone()
        } else {
            String::new()
        },
        hour,
        minute,
        // The Bot refuses a daily schedule that carries a weekday (recurrence.go `validate`), which the web's editor sends as
        // today's: only a weekly schedule has one.
        weekday: if form.kind == ScheduleKind::Weekly {
            weekday as u8
        } else {
            0
        },
    })
}

/// Whether `state` lets this member run announcements in `channel` of `community`, or why not: the words of the handler
/// (:380-388). The channel's bits are the viewer's resolved ones; none known is none held (`bits !== undefined`).
pub fn check(
    state: &AppState,
    community: Option<CommunityId>,
    channel: ChannelId,
) -> Result<CommunityId, &'static str> {
    let community = community.ok_or("Announcements are available only in this community.")?;
    if !holds_manage(state, channel) {
        return Err("Manage Community permission is required.");
    }
    let offered = state.bot.catalogs.get(&community).is_some_and(|c| {
        c.commands
            .iter()
            .any(|command| u64::from(command.operation) == sealed_operation(LIST))
    });
    if !offered {
        return Err("Announcements are unavailable.");
    }
    Ok(community)
}

/// `validateCurrent(…, 'manageCommunity')`.
fn holds_manage(state: &AppState, channel: ChannelId) -> bool {
    state
        .threads
        .channel_bits
        .get(&channel)
        .is_some_and(|bits| bits & u64::from(MANAGE_COMMUNITY) != 0)
}

/// What the editor starts with, from the clock, the zone and the schedule being edited (AnnouncementEditor's state).
#[derive(Clone, PartialEq, Eq)]
pub struct Opening {
    pub channel: Option<ChannelId>,
    pub text: String,
    pub kind: ScheduleKind,
    pub timezone: String,
    pub date: String,
    pub hour: String,
    pub minute: String,
    pub weekday: u8,
}

impl std::fmt::Debug for Opening {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Opening")
            .field("channel", &self.channel)
            .field("text", &dilla_client::state::Redacted(&self.text))
            .field("kind", &self.kind)
            .finish_non_exhaustive()
    }
}

/// The zone this device's times are in: UTC under a fixture's frozen UTC clock, else the system zone (`UTC` when it has
/// no name).
fn device_zone(clock: Clock) -> (String, jiff::tz::TimeZone) {
    if matches!(clock, Clock::Fixed { utc: true, .. }) {
        return ("UTC".to_owned(), jiff::tz::TimeZone::UTC);
    }
    let zone = jiff::tz::TimeZone::system();
    (zone.iana_name().unwrap_or("UTC").to_owned(), zone)
}

/// The editor's first values: the schedule's own for an edit, else the first text channel, once, now plus a minute in the
/// device's zone and today's weekday; `prefill` (the `text` argument) replaces the message when it is not blank.
pub fn opening(
    clock: Clock,
    channels: &[(ChannelId, String)],
    existing: Option<&ScheduleRow>,
    prefill: &str,
) -> Opening {
    let (zone_name, zone) = device_zone(clock);
    let now = jiff::Timestamp::from_second(i64::try_from(clock.now()).unwrap_or(0))
        .unwrap_or(jiff::Timestamp::UNIX_EPOCH);
    let soon = (now + jiff::SignedDuration::from_secs(60)).to_zoned(zone.clone());
    let today = now.to_zoned(zone);
    let default_date = format!("{:04}-{:02}-{:02}", soon.year(), soon.month(), soon.day());
    Opening {
        channel: existing
            .map(|r| r.channel)
            .or_else(|| channels.first().map(|(id, _)| *id)),
        text: if prefill.trim().is_empty() {
            existing.map(|r| r.text.clone()).unwrap_or_default()
        } else {
            prefill.to_owned()
        },
        kind: existing.map_or(ScheduleKind::OneOff, |r| r.kind),
        timezone: existing.map_or(zone_name, |r| r.timezone.clone()),
        date: existing.map_or(default_date, |r| r.date.clone()),
        hour: format!("{:02}", existing.map_or(soon.hour() as u8, |r| r.hour)),
        minute: format!("{:02}", existing.map_or(soon.minute() as u8, |r| r.minute)),
        weekday: existing.map_or(today.weekday().to_sunday_zero_offset() as u8, |r| r.weekday),
    }
}

// ---- the editor ----

/// The editor of one window: the fields' inputs, the choices and the save in flight. Its Debug gives no text.
pub struct AnnouncementForm {
    /// The text channels a schedule may go to (`kind == 0`), in the community's order.
    pub channels: Vec<(ChannelId, String)>,
    pub channel: Option<ChannelId>,
    pub message: Entity<TextareaState>,
    pub date: Entity<InputState>,
    pub hour: Entity<InputState>,
    pub minute: Entity<InputState>,
    pub timezone: Entity<InputState>,
    pub kind: ScheduleKind,
    pub weekday: u8,
    /// `true` while editing a schedule (the title and the saved answer say so).
    pub editing: bool,
    /// Why the last Save did not go (a field's refusal).
    pub error: Option<String>,
    /// A save is out: Save is disabled, and a second sends nothing.
    pub busy: bool,
    /// The editor's one request id: every save of this editor sends it (Task 30 replaces it with the remembered
    /// attempt's).
    pub parent: [u8; 16],
    /// The command's row.
    pub row: u64,
    pub conversation: Conversation,
    pub community: CommunityId,
    /// The schedule being edited, and the list's revision the save quotes.
    pub existing: Option<ScheduleRow>,
    pub list_revision: u64,
    pub clock: Clock,
    /// `None` in a fixture: its Save does nothing.
    pub client: Option<Rc<Client>>,
    /// Each input's subscription.
    _subs: Vec<Subscription>,
    /// The save is out and the editor is to go.
    done: bool,
}

impl std::fmt::Debug for AnnouncementForm {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("AnnouncementForm")
            .field("channels", &self.channels.len())
            .field("kind", &self.kind)
            .field("editing", &self.editing)
            .field("error", &self.error.is_some())
            .field("busy", &self.busy)
            .field("parent", &self.parent)
            .field("row", &self.row)
            .field("conversation", &self.conversation)
            .field("community", &self.community)
            .field("client", &self.client.is_some())
            .finish_non_exhaustive()
    }
}

impl AnnouncementForm {
    /// A Save asks to go: the editor's `parent` request id when none is out, else `None` (a second press while one is
    /// out).
    pub fn begin_save(&mut self) -> Option<[u8; 16]> {
        if self.busy {
            return None;
        }
        self.busy = true;
        Some(self.parent)
    }

    /// The save is answered: Save is enabled again. `keep_open` is false for an answer that may have taken effect, which
    /// keeps its id (a resend settles that request); after any definitive answer the next save is a new request (R29: the
    /// Bot would answer a stored refusal again).
    pub fn end_save(&mut self, keep_open: bool) {
        self.busy = false;
        if keep_open {
            self.parent = dilla_client::features::roles::request_id();
        } else {
            self.done = true;
        }
    }
}

/// What the command's answer asks the window to show, until its window draws it (an input is made with a window).
struct Wanted {
    opening: Opening,
    channels: Vec<(ChannelId, String)>,
    existing: Option<ScheduleRow>,
    list_revision: u64,
    parent: [u8; 16],
    row: u64,
    conversation: Conversation,
    community: CommunityId,
    clock: Clock,
    client: Option<Rc<Client>>,
}

/// A schedule waiting to be deleted: the question is asked.
struct Deleting {
    target: ScheduleRow,
    list_revision: u64,
    parent: [u8; 16],
    row: u64,
    conversation: Conversation,
    community: CommunityId,
    client: Rc<Client>,
}

#[derive(Default)]
struct Slot {
    wanted: Option<Wanted>,
    form: Option<AnnouncementForm>,
    deleting: Option<Deleting>,
}

fn slot(window: WindowId, cx: &mut App) -> &mut Slot {
    crate::per_window::get_mut::<Slot>(window, cx)
}

/// `window` forgets what it holds (its client was released): nothing typed stays, nothing is sent.
pub fn forget_window(window: WindowId, cx: &mut App) {
    crate::per_window::set::<Slot>(window, None, cx);
}

/// Whether `window` holds an editor or a question.
pub fn is_open(window: WindowId, cx: &App) -> bool {
    crate::per_window::get::<Slot>(window, cx)
        .is_some_and(|s| s.wanted.is_some() || s.form.is_some() || s.deleting.is_some())
}

/// The editor of `window`, once it was drawn.
pub fn form_of(window: WindowId, cx: &App) -> Option<&AnnouncementForm> {
    crate::per_window::get::<Slot>(window, cx)?.form.as_ref()
}

/// The command's row says how it ended. A fixture keeps its row in the window.
fn finish(
    client: Option<&Rc<Client>>,
    window: WindowId,
    conversation: Conversation,
    row: u64,
    (status, text): (ResultStatus, String),
    cx: &mut App,
) {
    match client {
        Some(client) => client.bot().finish(conversation, row, status, text),
        None => {
            for (_, r) in &mut crate::per_window::get_mut::<FixtureRows>(window, cx).0 {
                if r.id == row {
                    r.status = status;
                    r.text.clone_from(&text);
                }
            }
        }
    }
    cx.refresh_windows();
}

/// The command's row goes (its question was cancelled, or abandoned).
fn dismiss(
    client: Option<&Rc<Client>>,
    window: WindowId,
    conversation: Conversation,
    row: u64,
    cx: &mut App,
) {
    match client {
        Some(client) => client.bot().dismiss(conversation, row),
        None => super::dismiss_fixture_row(window, row, cx),
    }
}

fn failed(text: &str) -> (ResultStatus, String) {
    (ResultStatus::Failed, text.to_owned())
}

/// One of the four commands was sent from the composer. Its row is pending from here ([`finish`] ends it); a community
/// without the rights or the Bot's offer ends it Failed ([`check`]); otherwise the schedules are read from the Bot and
/// the command goes on in [`after_list`]. A fixture has no Bot to read: its `create` opens the editor at once.
pub fn run(ctx: &SlotCx, command: Command, args: &Args, window: WindowId, cx: &mut App) {
    let Some(channel) = ctx.channel else {
        return;
    };
    let conversation = Conversation {
        channel,
        thread: None,
    };
    let pending = copy::pending(command.id());
    let row = match &ctx.client {
        Some(client) => client.bot().add_result(
            conversation,
            ResultStatus::Pending,
            pending,
            ResultProvider::Bot,
        ),
        None => {
            let row = add_fixture_row(window, conversation, ctx.clock, pending, cx);
            for (_, r) in &mut crate::per_window::get_mut::<FixtureRows>(window, cx).0 {
                if r.id == row {
                    r.provider = ResultProvider::Bot;
                }
            }
            row
        }
    };
    let community = match check(&ctx.state, ctx.community, channel) {
        Ok(community) => community,
        Err(why) => {
            return finish(
                ctx.client.as_ref(),
                window,
                conversation,
                row,
                failed(why),
                cx,
            );
        }
    };
    let started = Started {
        command,
        args: args.clone(),
        conversation,
        community,
        row,
        clock: ctx.clock,
    };
    let Some(client) = ctx.client.clone() else {
        if command == Command::Create {
            let parent = dilla_client::features::roles::request_id();
            open_editor(&ctx.state, started, (None, 0, parent), None, window, cx);
        }
        return;
    };
    let parent = dilla_client::features::roles::request_id();
    cx.spawn(async move |cx| {
        let listed = client.bot().list_announcements(community, parent).await;
        cx.update(|cx| after_list(window, &client, started, parent, listed, cx));
    })
    .detach();
}

/// A command that is reading the schedules.
struct Started {
    command: Command,
    args: Args,
    conversation: Conversation,
    community: CommunityId,
    row: u64,
    clock: Clock,
}

/// The Bot answered the read of the schedules: the command goes on (`announcementHandler` after `listAnnouncements`).
fn after_list(
    w: WindowId,
    client: &Rc<Client>,
    started: Started,
    parent: [u8; 16],
    listed: Result<(Vec<ScheduleRow>, u64), String>,
    cx: &mut App,
) {
    let Started {
        command,
        args,
        conversation,
        community,
        row,
        ..
    } = &started;
    let (command, conversation, community, row) = (*command, *conversation, *community, *row);
    let end = |outcome: (ResultStatus, String), cx: &mut App| {
        finish(Some(client), w, conversation, row, outcome, cx);
    };
    let (rows, list_revision) = match listed {
        Ok(read) => read,
        Err(why) => return end((ResultStatus::Failed, why), cx),
    };
    let named = args
        .get("announcement")
        .and_then(|id| rows.iter().find(|r| &hex_id(&r.id) == id))
        .cloned();
    let gone = "That announcement is no longer available. Refresh the list and choose it again.";
    match command {
        Command::List => end(
            (
                ResultStatus::Success,
                if rows.is_empty() {
                    "No scheduled announcements.".to_owned()
                } else {
                    rows.iter()
                        .map(|r| format!("{}: {}", hex_id(&r.id), announcements::summary(r)))
                        .collect::<Vec<_>>()
                        .join("\n")
                },
            ),
            cx,
        ),
        Command::Delete => match named {
            None => end(failed(gone), cx),
            Some(target) => {
                slot(w, cx).deleting = Some(Deleting {
                    target,
                    list_revision,
                    parent,
                    row,
                    conversation,
                    community,
                    client: client.clone(),
                });
                cx.refresh_windows();
            }
        },
        Command::Create | Command::Edit => {
            if command == Command::Edit && named.is_none() {
                return end(failed(gone), cx);
            }
            let state = client.state().borrow().clone();
            open_editor(
                &state,
                started,
                (named, list_revision, parent),
                Some(client.clone()),
                w,
                cx,
            );
        }
    }
}

/// The editor is asked for: it is drawn from the next frame ([`overlay`]).
fn open_editor(
    state: &AppState,
    started: Started,
    (existing, list_revision, parent): (Option<ScheduleRow>, u64, [u8; 16]),
    client: Option<Rc<Client>>,
    w: WindowId,
    cx: &mut App,
) {
    let channels: Vec<(ChannelId, String)> = state
        .channels
        .iter()
        .filter(|c| c.community_id == started.community && c.kind == 0)
        .map(|c| (c.id, c.name.clone()))
        .collect();
    let prefill = started.args.get("text").map_or("", String::as_str);
    slot(w, cx).wanted = Some(Wanted {
        opening: opening(started.clock, &channels, existing.as_ref(), prefill),
        channels,
        existing,
        list_revision,
        parent,
        row: started.row,
        conversation: started.conversation,
        community: started.community,
        clock: started.clock,
        client,
    });
    cx.refresh_windows();
}

/// One of the editor's inputs: a change clears the editor's refusal.
fn clear_on_change<T: gpui::EventEmitter<InputEvent> + 'static>(
    w: WindowId,
    input: &Entity<T>,
    cx: &mut App,
) -> Subscription {
    cx.subscribe(input, move |_, event: &InputEvent, cx| {
        if matches!(event, InputEvent::Change) {
            if let Some(form) = slot(w, cx).form.as_mut() {
                form.error = None;
            }
            cx.refresh_windows();
        }
    })
}

fn make(w: WindowId, wanted: Wanted, window: &mut Window, cx: &mut App) -> AnnouncementForm {
    let o = &wanted.opening;
    let single = |value: &str, window: &mut Window, cx: &mut App| {
        let input = cx.new(|cx| InputState::new(window, cx));
        if !value.is_empty() {
            input.update(cx, |s, cx| s.set_value(value.to_owned(), window, cx));
        }
        input
    };
    let message = cx.new(|cx| TextareaState::new(window, cx));
    if !o.text.is_empty() {
        message.update(cx, |s, cx| s.set_value(o.text.clone(), window, cx));
    }
    let (date, hour, minute, timezone) = (
        single(&o.date, window, cx),
        single(&o.hour, window, cx),
        single(&o.minute, window, cx),
        single(&o.timezone, window, cx),
    );
    let mut subs = vec![clear_on_change(w, &message, cx)];
    for input in [&date, &hour, &minute, &timezone] {
        subs.push(clear_on_change(w, input, cx));
    }
    AnnouncementForm {
        channels: wanted.channels,
        channel: o.channel,
        message,
        date,
        hour,
        minute,
        timezone,
        kind: o.kind,
        weekday: o.weekday,
        editing: wanted.existing.is_some(),
        error: None,
        busy: false,
        parent: wanted.parent,
        row: wanted.row,
        conversation: wanted.conversation,
        community: wanted.community,
        existing: wanted.existing,
        list_revision: wanted.list_revision,
        clock: wanted.clock,
        client: wanted.client,
        _subs: subs,
        done: false,
    }
}

/// The editor's fields as typed.
fn typed(w: WindowId, cx: &mut App) -> Option<FormValues> {
    let form = slot(w, cx).form.as_ref()?;
    let (message, date, hour, minute, timezone) = (
        form.message.clone(),
        form.date.clone(),
        form.hour.clone(),
        form.minute.clone(),
        form.timezone.clone(),
    );
    let (channel, kind, weekday) = (form.channel, form.kind, form.weekday);
    let value = |input: &Entity<InputState>| input.read(cx).value().to_string();
    Some(FormValues {
        channel,
        text: message.read(cx).value().to_string(),
        kind,
        date: value(&date),
        hour: value(&hour),
        minute: value(&minute),
        weekday: weekday.to_string(),
        timezone: value(&timezone),
    })
}

/// Save: checks the fields, then the permission, then sends the editor's one request. A fixture has no client to send
/// with, so its Save does nothing after the checks.
pub fn submit(w: WindowId, cx: &mut App) {
    let Some(values) = typed(w, cx) else {
        return;
    };
    let Some(form) = slot(w, cx).form.as_ref() else {
        return;
    };
    if form.busy {
        return;
    }
    let now = jiff::Timestamp::from_second(i64::try_from(form.clock.now()).unwrap_or(0))
        .unwrap_or(jiff::Timestamp::UNIX_EPOCH);
    let schedule = match validate_schedule(&values, now) {
        Ok(schedule) => schedule,
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
    let Some(parent) = form.begin_save() else {
        return;
    };
    let (conversation, community, row) = (form.conversation, form.community, form.row);
    let (existing, list_revision) = (form.existing.clone(), form.list_revision);
    // The web closes the dialog as it submits (the row carries the rest): the editor goes on the next frame.
    form.done = true;
    // The permission is read again as the save goes (`validateCurrent` after the dialog).
    let held = holds_manage(&client.state().borrow(), conversation.channel);
    if !held {
        finish(
            Some(&client),
            w,
            conversation,
            row,
            failed("Manage Community permission changed before saving."),
            cx,
        );
        return;
    }
    cx.spawn(async move |cx| {
        let answer = client
            .bot()
            .save_announcement(
                community,
                parent,
                existing.as_ref(),
                &schedule,
                list_revision,
            )
            .await;
        cx.update(|cx| settled(w, &client, (conversation, row), parent, answer, cx));
    })
    .detach();
    cx.refresh_windows();
}

/// What a save or a deletion answered: the row's end, and the editor's. The row is finished even if the editor went, and
/// the editor is only touched while it is still the one that sent `parent`.
fn settled(
    w: WindowId,
    client: &Rc<Client>,
    (conversation, row): (Conversation, u64),
    parent: [u8; 16],
    answer: Result<String, announcements::Failure>,
    cx: &mut App,
) {
    let (outcome, uncertain) = match answer {
        Ok(text) => ((ResultStatus::Success, text), false),
        Err(f) => {
            let uncertain = f.uncertain();
            ((f.status, f.text), uncertain)
        }
    };
    // The Bot's list may have changed (C drops what it held): the chips read it again when next offered.
    if uncertain || outcome.0 == ResultStatus::Success {
        super::chips::announcements_changed(client);
    }
    finish(Some(client), w, conversation, row, outcome, cx);
    if let Some(form) = slot(w, cx).form.as_mut()
        && form.parent == parent
    {
        form.end_save(!uncertain);
    }
}

/// Cancel, Escape, a click outside and leaving the conversation: the command ends Failed, "Announcement editing
/// cancelled." (:432). A save already out finishes its own row.
pub fn cancel(w: WindowId, cx: &mut App) {
    let s = slot(w, cx);
    s.wanted = None;
    let Some(form) = s.form.take() else {
        return;
    };
    if !form.busy {
        finish(
            form.client.as_ref(),
            w,
            form.conversation,
            form.row,
            failed("Announcement editing cancelled."),
            cx,
        );
    }
}

/// The person answered the deletion's question: `Some(true)` deletes, `Some(false)` (Cancel) and `None` (a click outside)
/// leave no row.
pub fn answer_delete(w: WindowId, choice: Option<bool>, cx: &mut App) {
    let Some(d) = slot(w, cx).deleting.take() else {
        return;
    };
    if choice != Some(true) {
        dismiss(Some(&d.client), w, d.conversation, d.row, cx);
        cx.refresh_windows();
        return;
    }
    let held = holds_manage(&d.client.state().borrow(), d.conversation.channel);
    if !held {
        finish(
            Some(&d.client),
            w,
            d.conversation,
            d.row,
            failed("Manage Community permission changed before deletion."),
            cx,
        );
        return;
    }
    let Deleting {
        target,
        list_revision,
        parent,
        row,
        conversation,
        community,
        client,
    } = d;
    cx.spawn(async move |cx| {
        let answer = client
            .bot()
            .delete_announcement(community, parent, &target, list_revision)
            .await;
        cx.update(|cx| settled(w, &client, (conversation, row), parent, answer, cx));
    })
    .detach();
    cx.refresh_windows();
}

/// The destination select chose `channel` (`None`: "Choose a channel").
pub fn choose_channel(w: WindowId, channel: Option<ChannelId>, cx: &mut App) {
    if let Some(form) = slot(w, cx).form.as_mut() {
        form.channel = channel;
        form.error = None;
    }
    cx.refresh_windows();
}

/// The repeat select chose `kind`: a one-off shows its date, a weekly one its weekday.
pub fn choose_kind(w: WindowId, kind: ScheduleKind, cx: &mut App) {
    if let Some(form) = slot(w, cx).form.as_mut() {
        form.kind = kind;
        form.error = None;
    }
    cx.refresh_windows();
}

/// The weekday select chose `weekday` (0 is Sunday).
pub fn choose_weekday(w: WindowId, weekday: u8, cx: &mut App) {
    if let Some(form) = slot(w, cx).form.as_mut() {
        form.weekday = weekday;
        form.error = None;
    }
    cx.refresh_windows();
}

/// The editor and the deletion's question over the window (`slots::overlays`), while one is open.
pub fn overlay(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let w = window.window_handle().window_id();
    if let Some(d) = slot(w, cx).deleting.as_ref() {
        // Leaving the conversation the command was typed in: the question goes, no row stays.
        if ctx.channel != Some(d.conversation.channel) {
            answer_delete(w, None, cx);
            return None;
        }
        let (title, body) = (
            "Delete this scheduled announcement?",
            announcements::summary(&d.target),
        );
        return Some(
            ConfirmDialog::new(
                "announcement-delete",
                title,
                body,
                copy::CONTINUE,
                copy::WORKING,
                copy::CANCEL,
            )
            .on_confirm(move |window, cx| {
                answer_delete(w, Some(true), cx);
                window.refresh();
            })
            .on_close(move |window, cx| {
                answer_delete(w, Some(false), cx);
                window.refresh();
            })
            .on_backdrop(move |window, cx| {
                answer_delete(w, None, cx);
                window.refresh();
            })
            .confirm_wrap(|button| {
                crate::tracked_box("announcement-delete-confirm", button).into_any_element()
            })
            .into_any_element(),
        );
    }
    if let Some(wanted) = slot(w, cx).wanted.take() {
        let form = make(w, wanted, window, cx);
        slot(w, cx).form = Some(form);
    }
    let form = slot(w, cx).form.as_ref()?;
    // Closed by its save.
    if form.done {
        slot(w, cx).form = None;
        return None;
    }
    // Leaving the conversation closes the editor and ends the command (CommandHost's `isCurrent`).
    if ctx.channel != Some(form.conversation.channel) {
        cancel(w, cx);
        return None;
    }
    Some(editor(w, cx)?.into_any_element())
}

/// The destination select's value for no channel chosen.
const NO_CHANNEL: &str = "";

/// The editor's dialog (BotCommandDialogs.tsx `AnnouncementEditor`): its fields in the web's order, with its labels.
fn editor(w: WindowId, cx: &mut App) -> Option<impl IntoElement> {
    let form = slot(w, cx).form.as_ref()?;
    let (channels, channel, kind, weekday) =
        (form.channels.clone(), form.channel, form.kind, form.weekday);
    let (message, date, hour, minute, timezone) = (
        form.message.clone(),
        form.date.clone(),
        form.hour.clone(),
        form.minute.clone(),
        form.timezone.clone(),
    );
    let (editing, busy, error) = (form.editing, form.busy, form.error.clone());

    let ids: Vec<(String, String)> = std::iter::once((
        NO_CHANNEL.to_owned(),
        t("botCommand.announcement.chooseDestination").to_owned(),
    ))
    .chain(channels.iter().map(|(id, name)| (id.hex(), name.clone())))
    .collect();
    let pairs: Vec<(&str, &str)> = ids.iter().map(|(v, l)| (v.as_str(), l.as_str())).collect();
    let destination = Select::new(
        "announcement-destination",
        t("botCommand.announcement.destination"),
        &pairs,
        channel.map_or_else(|| NO_CHANNEL.to_owned(), |c| c.hex()),
    )
    .on_change(move |value, _, cx| {
        choose_channel(
            w,
            channels
                .iter()
                .map(|(id, _)| *id)
                .find(|id| id.hex() == value),
            cx,
        );
    });
    let recurrence = Select::new(
        "announcement-recurrence",
        t("botCommand.announcement.recurrence"),
        &[
            ("one_off", t("botCommand.announcement.once")),
            ("daily", t("botCommand.announcement.daily")),
            ("weekly", t("botCommand.announcement.weekly")),
        ],
        kind.wire(),
    )
    .on_change(move |value, _, cx| {
        choose_kind(
            w,
            match value {
                "daily" => ScheduleKind::Daily,
                "weekly" => ScheduleKind::Weekly,
                _ => ScheduleKind::OneOff,
            },
            cx,
        );
    });
    let mut body = div()
        .flex()
        .flex_col()
        .gap(px(SECTION_GAP))
        .child(destination)
        .child(crate::tracked_focus(
            "announcement-message",
            &message.focus_handle(cx),
            TextField::multiline(
                "announcement-message-field",
                t("botCommand.announcement.message"),
                &message,
            ),
        ))
        .child(crate::tracked_box("announcement-recurrence", recurrence));
    if kind == ScheduleKind::OneOff {
        body = body.child(crate::tracked_focus(
            "announcement-date",
            &date.focus_handle(cx),
            TextField::new(
                "announcement-date-field",
                t("botCommand.announcement.date"),
                &date,
            ),
        ));
    }
    if kind == ScheduleKind::Weekly {
        let days: Vec<(String, String)> = (0..7)
            .map(|d| {
                (
                    d.to_string(),
                    t(&format!("botCommand.weekday.{d}")).to_owned(),
                )
            })
            .collect();
        let pairs: Vec<(&str, &str)> = days.iter().map(|(v, l)| (v.as_str(), l.as_str())).collect();
        body = body.child(
            Select::new(
                "announcement-weekday",
                t("botCommand.announcement.weekday"),
                &pairs,
                weekday.to_string(),
            )
            .on_change(move |value, _, cx| choose_weekday(w, value.parse().unwrap_or(0), cx)),
        );
    }
    let field = |id: &'static str, key: &str, input: &Entity<InputState>, cx: &App| {
        crate::tracked_focus(
            id,
            &input.focus_handle(cx),
            TextField::new(format!("{id}-field"), t(key), input),
        )
    };
    body = body
        .child(field(
            "announcement-hour",
            "botCommand.announcement.hour",
            &hour,
            cx,
        ))
        .child(field(
            "announcement-minute",
            "botCommand.announcement.minute",
            &minute,
            cx,
        ))
        .child(field(
            "announcement-timezone",
            "botCommand.announcement.timezone",
            &timezone,
            cx,
        ));
    if let Some(error) = error {
        body = body.child(crate::tracked_box(
            "announcement-error",
            div()
                .id("announcement-error-text")
                .a11y(Role::Alert, error.clone())
                .live(Live::Assertive)
                .child(line(
                    error,
                    text::SM,
                    root_line_height(text::SM),
                    color::DANGER,
                )),
        ));
    }
    let cancel_button = Button::new("announcement-cancel-button", t("chperm.cancel"))
        .size(ButtonSize::Dialog)
        .on_click(move |_, window, cx| {
            cancel(w, cx);
            window.refresh();
        });
    let save = Button::new("announcement-save-button", t("botCommand.save"))
        .size(ButtonSize::Dialog)
        .disabled(busy)
        .on_click(move |_, window, cx| {
            submit(w, cx);
            window.refresh();
        });
    Some(
        Dialog::new(
            "announcement-dialog",
            t(if editing {
                "botCommand.announcement.edit"
            } else {
                "botCommand.announcement.create"
            }),
            t("dialog.close"),
        )
        .body(body)
        .footer(crate::tracked_box("announcement-cancel", cancel_button))
        .footer(crate::tracked_box("announcement-save", save))
        .on_close(move |window, cx| {
            cancel(w, cx);
            window.refresh();
        }),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::channels::{Channel, ChannelGroupState};
    use dilla_client::state::{ChannelId, UserId};

    const COMMUNITY: CommunityId = CommunityId([1; 16]);
    const CHANNEL: [u8; 16] = [7; 16];
    /// 2026-11-12 11:08:58 UTC, a Thursday.
    const NOW: u64 = 1_794_481_738;

    fn now() -> jiff::Timestamp {
        jiff::Timestamp::from_second(NOW as i64).unwrap()
    }

    fn values() -> FormValues {
        FormValues {
            channel: Some(ChannelId(CHANNEL)),
            text: "stand-up".into(),
            kind: ScheduleKind::OneOff,
            date: "2026-12-01".into(),
            hour: "09".into(),
            minute: "30".into(),
            weekday: "3".into(),
            timezone: "UTC".into(),
        }
    }

    /// BotCommandDialogs.tsx:40-52: the fields' limits show `botCommand.dialog.invalid`, a one-off that is not a complete
    /// future minute in its zone `botCommand.dialog.futureMinute`.
    #[test]
    fn validate_schedule_follows_the_dialog() {
        let check = |edit: &dyn Fn(&mut FormValues)| {
            let mut v = values();
            edit(&mut v);
            validate_schedule(&v, now())
        };
        let good = check(&|_| {}).expect("a good one-off");
        assert_eq!(
            (
                good.hour,
                good.minute,
                good.date.as_str(),
                good.timezone.as_str()
            ),
            (9, 30, "2026-12-01", "UTC")
        );
        assert_eq!(good.channel, ChannelId(CHANNEL));
        let invalid = Err("botCommand.dialog.invalid");
        assert_eq!(check(&|v| v.hour = "24".into()), invalid);
        assert_eq!(check(&|v| v.hour = "-1".into()), invalid);
        assert_eq!(check(&|v| v.hour = "1.5".into()), invalid);
        assert_eq!(check(&|v| v.hour = "x".into()), invalid);
        assert_eq!(check(&|v| v.minute = "60".into()), invalid);
        assert_eq!(check(&|v| v.weekday = "7".into()), invalid);
        assert_eq!(check(&|v| v.timezone = "Mars/Base".into()), invalid);
        assert_eq!(check(&|v| v.channel = None), invalid);
        assert_eq!(check(&|v| v.text = "  \n ".into()), invalid);
        assert_eq!(check(&|v| v.text = "é".repeat(901)), invalid, "1802 bytes");
        assert!(check(&|v| v.text = "é".repeat(900)).is_ok(), "1800 bytes");
        // A blank number is 0, as `Number('')` is.
        assert_eq!(check(&|v| v.hour = String::new()).unwrap().hour, 0);
        // The message is saved trimmed, and a daily one carries no date.
        let daily = check(&|v| {
            v.kind = ScheduleKind::Daily;
            v.text = "  hi  ".into();
            v.date = "not a date".into();
        })
        .expect("a daily one has no date to check");
        assert_eq!((daily.text.as_str(), daily.date.as_str()), ("hi", ""));
        // Only a weekly schedule carries its weekday: the Bot refuses a daily one that does.
        assert_eq!(daily.weekday, 0);
        let weekly = check(&|v| v.kind = ScheduleKind::Weekly).expect("a weekly one");
        assert_eq!(weekly.weekday, 3);
        // A one-off: its date's shape, a minute the zone skipped over (the clocks of Stockholm go from 02:00 to 03:00 on
        // 2026-03-29), the past and the minute that is already running.
        let future = Err("botCommand.dialog.futureMinute");
        assert_eq!(check(&|v| v.date = "2026-1-2".into()), future);
        assert_eq!(check(&|v| v.date = "2026-02-30".into()), future);
        assert_eq!(
            check(&|v| {
                v.date = "2026-03-29".into();
                v.hour = "02".into();
                v.timezone = "Europe/Stockholm".into();
            }),
            future,
            "02:30 did not happen in Stockholm that day"
        );
        assert_eq!(check(&|v| v.date = "2026-11-10".into()), future);
        // 11:08 is running: 11:08 is not a future minute, 11:09 is.
        let running = |minute: &str| {
            check(&|v| {
                v.date = "2026-11-12".into();
                v.hour = "11".into();
                v.minute = minute.into();
            })
        };
        assert_eq!(running("08"), future);
        assert!(running("09").is_ok());
    }

    /// `occurrence` finds the instant, and `None` for a minute that does not exist; a minute that happens twice is its
    /// first.
    #[test]
    fn occurrence_finds_the_instant_or_none() {
        let at = |d: &str, h, m, z: &str| occurrence(d, h, m, z).map(|t| t.as_second());
        assert_eq!(at("2026-11-12", 11, 9, "UTC"), Some(1_794_481_740));
        assert_eq!(
            at("2026-11-12", 12, 9, "Europe/Stockholm"),
            Some(1_794_481_740),
            "Stockholm is an hour ahead in November"
        );
        assert_eq!(at("2026-03-29", 2, 30, "Europe/Stockholm"), None, "a gap");
        // The clocks go back from 03:00 to 02:00 on 2026-10-25: 02:30 happens at 00:30 UTC, then at 01:30 UTC.
        let fold = at("2026-10-25", 2, 30, "Europe/Stockholm").expect("a fold has an instant");
        assert_eq!(fold, 1_792_888_200, "the first 02:30, which is 00:30 UTC");
        assert_eq!(at("2026-11-12", 11, 9, "Mars/Base"), None);
        assert_eq!(at("12/11/2026", 11, 9, "UTC"), None);
    }

    /// `canonicalTimezone`: a name as the zone database spells it (`Intl` answers the canonical spelling of any case). jiff
    /// 0.2.38 on Linux looks a name up case-insensitively and answers the database's spelling, so `europe/stockholm` is
    /// `Europe/Stockholm`; a platform whose zone database is case-sensitive answers `None`, which Save then refuses as an
    /// unknown zone, so the test accepts either.
    #[test]
    fn canonical_zone_spells_a_zone_as_its_database_does() {
        assert_eq!(
            canonical_zone("Europe/Stockholm").as_deref(),
            Some("Europe/Stockholm")
        );
        assert_eq!(canonical_zone("UTC").as_deref(), Some("UTC"));
        assert_eq!(canonical_zone("Mars/Base"), None);
        assert_eq!(canonical_zone(""), None);
        let lower = canonical_zone("europe/stockholm");
        assert!(
            matches!(lower.as_deref(), None | Some("Europe/Stockholm")),
            "{lower:?}"
        );
    }

    fn rows(items: &[(u8, &str)]) -> Vec<ScheduleRow> {
        items
            .iter()
            .map(|(id, text)| ScheduleRow {
                id: [*id; 16],
                channel: ChannelId(CHANNEL),
                text: (*text).into(),
                kind: ScheduleKind::Weekly,
                timezone: "Europe/Stockholm".into(),
                date: String::new(),
                hour: 9,
                minute: 5,
                weekday: 1,
                enabled: true,
                revision: 3,
            })
            .collect()
    }

    /// AnnouncementEditor's first values: the first text channel, once, a minute from now in the device's zone and today's
    /// weekday; an edit starts with the schedule's own; a typed `text` argument replaces the message when it is not blank.
    #[test]
    fn the_editor_starts_with_the_webs_values() {
        let channels = [
            (ChannelId([5; 16]), "general".to_owned()),
            (ChannelId([6; 16]), "news".to_owned()),
        ];
        let clock = Clock::Fixed {
            now: NOW,
            utc: true,
        };
        let fresh = opening(clock, &channels, None, "");
        assert_eq!(fresh.channel, Some(ChannelId([5; 16])));
        assert_eq!(
            (
                fresh.kind,
                fresh.timezone.as_str(),
                fresh.date.as_str(),
                fresh.hour.as_str(),
                fresh.minute.as_str(),
                fresh.weekday
            ),
            (ScheduleKind::OneOff, "UTC", "2026-11-12", "11", "09", 4)
        );
        assert_eq!(opening(clock, &[], None, "").channel, None);
        let existing = rows(&[(1, "from the row")]).remove(0);
        let edit = opening(clock, &channels, Some(&existing), "  ");
        assert_eq!(
            (
                edit.text.as_str(),
                edit.kind,
                edit.hour.as_str(),
                edit.minute.as_str(),
                edit.weekday
            ),
            ("from the row", ScheduleKind::Weekly, "09", "05", 1)
        );
        assert_eq!(edit.timezone, "Europe/Stockholm");
        assert_eq!(
            opening(clock, &channels, Some(&existing), "typed").text,
            "typed"
        );
    }

    fn state(offers: bool, manage: bool) -> AppState {
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
        let mut commands = vec![
            spec("announcement list", 50, &[]),
            spec("announcement create", 51, &[("text", ArgType::Text)]),
            spec("announcement edit", 51, &[("announcement", ArgType::Text)]),
            spec(
                "announcement delete",
                52,
                &[("announcement", ArgType::Text)],
            ),
        ];
        if offers {
            commands.push(spec("announcement app", 150, &[]));
        }
        let mut s = AppState::default();
        s.account.user_id = Some(UserId([9; 16]));
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
            if manage {
                1 | u64::from(MANAGE_COMMUNITY)
            } else {
                1
            },
        );
        let channel = |id: [u8; 16], kind: u8, name: &str| Channel {
            id: ChannelId(id),
            community_id: COMMUNITY,
            kind,
            mode: 0,
            name: name.into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Active,
        };
        s.channels = vec![
            channel(CHANNEL, 0, "general"),
            channel([8; 16], 1, "Lounge"),
            channel([6; 16], 0, "news"),
        ];
        s
    }

    fn ctx(state: AppState) -> SlotCx {
        SlotCx {
            state: std::sync::Arc::new(state),
            community: Some(COMMUNITY),
            channel: Some(ChannelId(CHANNEL)),
            clock: Clock::Fixed {
                now: NOW,
                utc: true,
            },
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

    /// bot-handlers.ts:380-388: a command in a DM, without Manage Community, or from a Bot that does not offer
    /// announcements ends its row with the handler's words.
    #[test]
    fn a_command_checks_where_it_may_run() {
        let ch = ChannelId(CHANNEL);
        assert_eq!(
            check(&state(true, true), Some(COMMUNITY), ch),
            Ok(COMMUNITY)
        );
        assert_eq!(
            check(&state(true, true), None, ch),
            Err("Announcements are available only in this community.")
        );
        assert_eq!(
            check(&state(true, false), Some(COMMUNITY), ch),
            Err("Manage Community permission is required.")
        );
        let mut unknown = state(true, true);
        unknown.threads.channel_bits.clear();
        assert_eq!(
            check(&unknown, Some(COMMUNITY), ch),
            Err("Manage Community permission is required."),
            "bits not read are bits not held (`bits !== undefined`)"
        );
        assert_eq!(
            check(&state(false, true), Some(COMMUNITY), ch),
            Err("Announcements are unavailable.")
        );
    }

    /// The editor opens for `/announcement create` where the web's handler lets it, draws its fields in the web's order with
    /// the typed text, refuses a Save with a blank message, and Cancel ends the command's row Failed.
    #[gpui::test]
    fn announcement_create_opens_the_editor(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        let encode = |t: &str| t.to_owned();
        for (offers, manage, why) in [
            (
                true,
                false,
                Some("Manage Community permission is required."),
            ),
            (false, true, Some("Announcements are unavailable.")),
        ] {
            let ctx = ctx(state(offers, manage));
            window.update(|_, cx| {
                assert_eq!(
                    super::super::on_send(&ctx, "/announcement create", &encode, w, cx),
                    super::super::SendDecision::Handled
                );
                assert!(!is_open(w, cx));
                assert_eq!(
                    held_rows(w, cx),
                    [(ResultStatus::Failed, why.unwrap().to_owned())]
                );
                crate::per_window::get_mut::<FixtureRows>(w, cx).0.clear();
            });
        }
        let ctx = ctx(state(true, true));
        window.update(|window, cx| {
            super::super::on_send(
                &ctx,
                "/announcement create text:\"Stand-up at nine\"",
                &encode,
                w,
                cx,
            );
            assert!(is_open(w, cx));
            assert_eq!(
                held_rows(w, cx),
                [(ResultStatus::Pending, "/announcement create…".to_owned())],
                "the command's row is pending while the editor is open"
            );
            assert!(overlay(&ctx, window, cx).is_some(), "the editor is drawn");
            let form = form_of(w, cx).expect("the editor was made");
            assert_eq!(
                form.channel,
                Some(ChannelId(CHANNEL)),
                "the first text channel"
            );
            assert_eq!(form.channels.len(), 2, "text channels only");
            assert_eq!(
                form.message.read(cx).value().to_string(),
                "Stand-up at nine"
            );
            assert_eq!(form.hour.read(cx).value().to_string(), "11");
            assert_eq!(form.minute.read(cx).value().to_string(), "09");
            assert_eq!(form.timezone.read(cx).value().to_string(), "UTC");
            assert_eq!(form.date.read(cx).value().to_string(), "2026-11-12");
            // A blank message is refused where it is typed, and nothing is sent.
            let message = form.message.clone();
            message.update(cx, |s, cx| s.set_value("  ", window, cx));
            submit(w, cx);
            let form = form_of(w, cx).unwrap();
            assert_eq!(
                form.error.as_deref(),
                Some("Check the required fields and limits, then try again.")
            );
            assert!(!form.busy);
            cancel(w, cx);
            assert!(!is_open(w, cx));
            assert!(overlay(&ctx, window, cx).is_none());
            assert_eq!(
                held_rows(w, cx),
                [(
                    ResultStatus::Failed,
                    "Announcement editing cancelled.".to_owned()
                )]
            );
        });
    }

    /// The other three commands need the Bot's list: a fixture has none, so they only add their pending row. Leaving the
    /// conversation closes the editor and ends the command like Cancel.
    #[gpui::test]
    fn leaving_the_conversation_ends_the_command(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        window.update(|window, cx| {
            let here = ctx(state(true, true));
            super::super::on_send(&here, "/announcement create", &|t| t.to_owned(), w, cx);
            assert!(overlay(&here, window, cx).is_some());
            let mut elsewhere = ctx(state(true, true));
            elsewhere.channel = Some(ChannelId([9; 16]));
            assert!(overlay(&elsewhere, window, cx).is_none());
            assert!(!is_open(w, cx));
            assert_eq!(
                held_rows(w, cx),
                [(
                    ResultStatus::Failed,
                    "Announcement editing cancelled.".to_owned()
                )]
            );
        });
    }

    fn form(cx: &mut gpui::TestAppContext) -> AnnouncementForm {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        window.update(|window, cx| {
            make(
                w,
                Wanted {
                    opening: Opening {
                        channel: Some(ChannelId(CHANNEL)),
                        text: "the secret plan".into(),
                        kind: ScheduleKind::Daily,
                        timezone: "UTC".into(),
                        date: String::new(),
                        hour: "09".into(),
                        minute: "00".into(),
                        weekday: 1,
                    },
                    channels: vec![(ChannelId(CHANNEL), "general".into())],
                    existing: None,
                    list_revision: 7,
                    parent: [9; 16],
                    row: 1,
                    conversation: Conversation {
                        channel: ChannelId(CHANNEL),
                        thread: None,
                    },
                    community: COMMUNITY,
                    clock: Clock::Local,
                    client: None,
                },
                window,
                cx,
            )
        })
    }

    /// While one save is out a second sends nothing; after a definitive answer the next save is a new request (R29: the Bot
    /// would answer a stored refusal again), and only an answer that may have taken effect keeps its id and closes the
    /// editor.
    #[gpui::test]
    fn a_second_save_while_busy_sends_nothing(cx: &mut gpui::TestAppContext) {
        let mut form = form(cx);
        assert_eq!(form.begin_save(), Some([9; 16]));
        assert!(form.busy);
        assert_eq!(form.begin_save(), None, "a second press while it is out");
        form.end_save(true);
        assert!(!form.busy && !form.done);
        let fresh = form.begin_save().expect("idle again");
        assert_ne!(fresh, [9; 16], "a new request after a definitive answer");
        form.end_save(false);
        assert!(form.done, "an uncertain answer closes the editor");
        assert_eq!(form.parent, fresh, "and keeps its id");
    }

    /// R20/R21: the editor and the opening hold the message; their Debug says how much, never what.
    #[gpui::test]
    fn the_editor_keeps_its_text_out_of_debug(cx: &mut gpui::TestAppContext) {
        let form = form(cx);
        let opening = Opening {
            channel: None,
            text: "the secret plan".into(),
            kind: ScheduleKind::Daily,
            timezone: "UTC".into(),
            date: String::new(),
            hour: "09".into(),
            minute: "00".into(),
            weekday: 1,
        };
        let mut values = values();
        values.text = "the secret plan".into();
        for shown in [
            format!("{form:?}"),
            format!("{form:#?}"),
            format!("{opening:?}"),
            format!("{opening:#?}"),
            format!("{values:?}"),
            format!("{values:#?}"),
        ] {
            assert!(!shown.contains("secret"), "{shown}");
        }
    }

    /// bot-handlers.ts:571-575: an `announcement` chip offers the schedules the Bot last listed whose id is the query or
    /// whose summary holds it, labelled with the text's first 100 characters, at most 25.
    #[test]
    fn an_announcement_chip_offers_the_listed_schedules() {
        use super::super::chips::provided_suggestions;
        use dilla_client::features::bot::registry::{ArgKind, CommandDefinition};
        let mut s = state(true, true);
        let long = "x".repeat(120);
        s.bot.announcements.insert(
            COMMUNITY,
            rows(&[(1, "stand-up"), (2, "retro"), (3, &long)]),
        );
        let definition: CommandDefinition = super::super::picker::definitions(&s, Some(COMMUNITY))
            .into_iter()
            .find(|c| c.id == "announcement delete")
            .expect("the Bot's command, as the picker reads it");
        let option = definition
            .options
            .iter()
            .find(|o| o.kind == ArgKind::Announcement)
            .expect("an announcement option");
        let offer = |s: &AppState, query: &str| {
            provided_suggestions(
                s,
                Some(COMMUNITY),
                Some(ChannelId(CHANNEL)),
                &definition,
                option,
                query,
                &Args::new(),
            )
        };
        let all = offer(&s, "");
        assert_eq!(all.len(), 3);
        assert_eq!(all[0].label, "stand-up");
        assert_eq!(all[0].sub, "stand-up — weekly, Europe/Stockholm 09:05");
        assert_eq!(all[0].value, hex_id(&[1; 16]));
        assert_eq!(
            all[2].label.chars().count(),
            100,
            "the first 100 characters"
        );
        assert_eq!(offer(&s, "RETRO").len(), 1, "the summary, case folded");
        assert_eq!(offer(&s, "weekly").len(), 3);
        assert_eq!(offer(&s, &hex_id(&[2; 16])).len(), 1, "an id");
        assert!(offer(&s, "nothing like it").is_empty());
        // Not read yet: nothing to offer.
        s.bot.announcements.clear();
        assert!(offer(&s, "").is_empty());
        // At most 25.
        let many: Vec<(u8, &str)> = (1..=30).map(|i| (i, "x")).collect();
        s.bot.announcements.insert(COMMUNITY, rows(&many));
        assert_eq!(offer(&s, "").len(), 25);
    }
}
