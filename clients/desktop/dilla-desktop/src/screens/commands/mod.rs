//! Area **bot** (Task 20): the command picker, private results, poll cards and dialogs, reminders.
//!
//! Every file under `screens/commands/` is Task 20's. The hooks below are where `crate::slots` draws this area inside
//! another area's view; their signatures are Task 1's.
//!
//! - [`picker`] (`slots::composer_popup`): the `/` list over the composer, from the draft the composer reports through
//!   [`draft_changed`]; ↑↓, Tab, Enter and Escape reach it through [`keys`].
//! - [`poll_card`] (`slots::message_extras`): a verified poll under the Bot's post.
//! - [`poll_form`] (`slots::overlays`): `/poll create` opens the form, and Post sends the poll through the Bot.
//! - [`announcement_form`] (`slots::overlays`): `/announcement list|create|edit|delete` read, save and delete the Bot's
//!   scheduled announcements; `create` and `edit` open the editor.
//! - [`role_panel_form`] (`slots::overlays`): `/rolepanel create` opens the role-panel editor, and Publish has the Bot post
//!   the panel and registers its roles.
//! - [`private_results`]: the rows only this device shows, for the log of their conversation; [`on_send`] decides what
//!   a sent draft does (post it, or answer privately).

pub mod announcement_form;
pub mod builtins;
pub mod chips;
pub mod confirm;
pub mod copy;
pub mod keys;
pub mod modlog_dialog;
pub mod picker;
pub mod poll_form;
pub mod reminders;
pub mod role_panel_form;

use dilla_client::features::bot::{self, Plan, run};
use dilla_client::features::utilities::{poll_claim, polls};
use dilla_client::state::bot::{
    Conversation, PollCheck, PollView, PrivateResult, ResultProvider, ResultStatus,
};
use dilla_client::state::{TimelineItem, TimelineItemState};
use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, Styled, Window, div,
};

use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::commands::bot_result::{self, Author, BotResult, Status};
use dilla_desktop_ui::components::commands::list::{CommandList, ListItem, ListLabels};
use dilla_desktop_ui::components::commands::poll_card::{self, PollCard};
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, text};

use crate::screens::shell::Clock;
use crate::slots::{Extra, SlotCx};
use crate::strings::{t, tf};

/// The automation ids of the list's rows (at most the 25 the registry ranks).
pub const ROW_IDS: [&str; 25] = [
    "command-row-0",
    "command-row-1",
    "command-row-2",
    "command-row-3",
    "command-row-4",
    "command-row-5",
    "command-row-6",
    "command-row-7",
    "command-row-8",
    "command-row-9",
    "command-row-10",
    "command-row-11",
    "command-row-12",
    "command-row-13",
    "command-row-14",
    "command-row-15",
    "command-row-16",
    "command-row-17",
    "command-row-18",
    "command-row-19",
    "command-row-20",
    "command-row-21",
    "command-row-22",
    "command-row-23",
    "command-row-24",
];

/// The picker's state for one window's composer (final review M4: per window, so a draft in one window never opens or
/// closes another's list): the draft it reported, the highlighted row, the draft the list was closed on (Escape keeps
/// the draft and hides the list until the draft changes), and what the last frame showed. Its Debug gives the drafts'
/// lengths only: a draft is a message body, or a moderation command with its member and reason.
#[derive(Clone, Default, PartialEq, Eq)]
pub struct PickerState {
    pub draft: String,
    pub selected: usize,
    pub closed_for: Option<String>,
    /// The rows the last frame drew (`None`: the list was closed): what [`keys`] moves over.
    pub rows: Option<Vec<String>>,
    /// The command Tab or Enter picked on the list, for the chips.
    pub picked: Option<String>,
    /// The chips of the picked command, in place of the composer's field (Task 28 follow-up 14).
    pub chips: Option<chips::Chips>,
    /// The field of the composer that reported the draft (t28-bot-review I1): the list's and the chips' keys are
    /// taken only while it holds the focus.
    pub composer: Option<gpui::WeakFocusHandle>,
    /// A command draft to show again in the composer of its conversation (Task 30: its run's answer was uncertain,
    /// CommandHost.tsx:293-296); the composer takes it as it draws ([`take_reopen`]), and only for the client that ran it.
    pub reopen: Option<Reopen>,
}

/// An uncertain run's draft, for the composer of `at` in a window of the client numbered `client` (`Client::serial`,
/// `None` in a fixture). A composer of another client (another session, or none after a sign-out) never takes it.
#[derive(Clone, PartialEq, Eq)]
pub struct Reopen {
    pub client: Option<u64>,
    pub at: Conversation,
    pub text: String,
}

impl std::fmt::Debug for PickerState {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("PickerState")
            .field("draft_len", &self.draft.len())
            .field("selected", &self.selected)
            .field("closed_for_len", &self.closed_for.as_ref().map(String::len))
            .field("rows", &self.rows.as_ref().map(Vec::len))
            .field("picked", &self.picked)
            .field("chips", &self.chips)
            .field("reopen_len", &self.reopen.as_ref().map(|r| r.text.len()))
            .finish_non_exhaustive()
    }
}

/// The composer whose field is `focus` reports a draft in `window`: the list and chips belong to it.
pub fn set_composer(window: gpui::WindowId, focus: &gpui::FocusHandle, cx: &mut App) {
    activate(window, focus, true, cx);
}

/// `window`'s picker state (empty until its composer reports a draft).
pub fn picker_state(window: gpui::WindowId, cx: &App) -> PickerState {
    crate::per_window::get::<PickerState>(window, cx)
        .cloned()
        .unwrap_or_default()
}

/// The picker states of every composer of `window` (the active one and the parked ones) whose field is still alive.
pub fn picker_states(window: gpui::WindowId, cx: &App) -> Vec<PickerState> {
    crate::per_window::get::<PickerState>(window, cx)
        .into_iter()
        .chain(
            crate::per_window::get::<Parked>(window, cx)
                .into_iter()
                .flat_map(|p| p.0.iter()),
        )
        .filter(|s| s.composer.as_ref().is_some_and(|c| c.upgrade().is_some()))
        .cloned()
        .collect()
}

/// `window`'s picker state, to change.
pub fn picker_state_mut(window: gpui::WindowId, cx: &mut App) -> &mut PickerState {
    crate::per_window::get_mut::<PickerState>(window, cx)
}

/// The client of `windows` was released (Forget, Sign out, an instance's Remove; final review M7): their picker drafts
/// and chips (a typed ban reason) and their reminders popovers go, so nothing typed stays and the next sign-in reads its
/// reminders afresh.
pub fn release_windows(windows: &[gpui::WindowId], cx: &mut App) {
    for &window in windows {
        crate::per_window::set::<PickerState>(window, None, cx);
        crate::per_window::set::<Parked>(window, None, cx);
        reminders::forget_window(window, cx);
        poll_form::forget_window(window, cx);
        announcement_form::forget_window(window, cx);
        role_panel_form::forget_window(window, cx);
    }
    modlog_dialog::release_windows(windows, cx);
}

/// The composer of `window` reports its draft on every change. A new draft moves the highlight back to the first row
/// and opens a list that Escape closed.
pub fn draft_changed(window: gpui::WindowId, draft: &str, cx: &mut App) {
    let state = picker_state_mut(window, cx);
    if state.draft != draft {
        state.draft = draft.to_owned();
        state.selected = 0;
        state.picked = None;
        state.chips = None;
        if state.closed_for.as_deref() != Some(draft) {
            state.closed_for = None;
        }
    }
}

/// An uncertain run's draft goes back into `window`'s composer (CommandHost.tsx:293-296), while the window still shows
/// `at` (`isCurrent()`): the composer takes it as it next draws ([`take_reopen`]), and the picker opens on it. A window
/// that moved on leaves it to the command memory, which the composer reads when it comes back to `at`. `client` is the
/// serial of the client that ran it (`None` in a fixture): only its composer takes the draft.
pub fn reopen(
    window: gpui::WindowId,
    client: Option<u64>,
    at: Conversation,
    text: String,
    cx: &mut App,
) {
    if crate::screens::threads::channel_of(window, cx).is_some_and(|c| c != at.channel) {
        return;
    }
    picker_state_mut(window, cx).reopen = Some(Reopen { client, at, text });
    cx.refresh_windows();
}

/// The draft [`reopen`] left for `at` in `window`, once, for the composer of the client numbered `client`. A draft left
/// for another client is dropped unread (a late answer of a released session never reaches the next one).
pub fn take_reopen(
    window: gpui::WindowId,
    client: Option<u64>,
    at: Conversation,
    cx: &mut App,
) -> Option<String> {
    let state = picker_state_mut(window, cx);
    match &state.reopen {
        Some(r) if r.client != client => {
            state.reopen = None;
            None
        }
        Some(r) if r.at == at => state.reopen.take().map(|r| r.text),
        _ => None,
    }
}

/// The run of `draft` in `at` (CommandHost.tsx:243-246): the request id the client's command memory remembers for the
/// same invocation, else a new one. A fixture has no memory: every run is a new request.
pub fn attempt_of(ctx: &SlotCx, at: Conversation, draft: &str) -> bot::Attempt {
    let invocation =
        bot::invocation_key(&ctx.state, ctx.community, draft).unwrap_or_else(|| draft.to_owned());
    let request = match &ctx.client {
        Some(client) => with_memory(client, |m| m.attempt(at, &invocation, draft)),
        None => dilla_client::features::roles::request_id(),
    };
    bot::Attempt {
        request,
        invocation,
        text: draft.to_owned(),
    }
}

/// Runs `f` over `client`'s command memory ([`bot::Bot::memory`]).
pub fn with_memory<T>(
    client: &dilla_client::Client,
    f: impl FnOnce(&mut bot::memory::CommandMemory) -> T,
) -> T {
    let memory = client.bot().memory();
    let mut memory = memory
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    f(&mut memory)
}

/// [`bot::Bot::settle`] from the window's own thread: an uncertain run's draft is offered back to `window`'s composer at
/// once ([`reopen`]) instead of waiting for the composer to find it in the memory.
///
/// Only while `window` still runs `client` (`create::windows_of`: a sign-out or a Forget releases it first, and a late
/// answer of a released session must never put its decrypted draft back for whoever uses the window next), and shows
/// `at`; else the memory keeps it for when that client's composer comes back there, and goes with it at sign-out.
pub fn settle(
    client: &std::rc::Rc<dilla_client::Client>,
    window: gpui::WindowId,
    (at, row): (Conversation, u64),
    attempt: &bot::Attempt,
    outcome: bot::Outcome,
    cx: &mut App,
) {
    let uncertain = matches!(outcome, bot::Outcome::Uncertain(_));
    client.bot().settle(at, row, attempt, outcome);
    if uncertain
        && session_shows(client, window, at, cx)
        && let Some(text) = with_memory(client, |m| m.take_reopen(at))
    {
        reopen(window, Some(client.serial()), at, text, cx);
    }
    cx.refresh_windows();
}

/// Whether `window` still runs `client` (it was not released: sign-out, Forget) and does not show a conversation other
/// than `at`.
fn session_shows(
    client: &std::rc::Rc<dilla_client::Client>,
    window: gpui::WindowId,
    at: Conversation,
    cx: &mut App,
) -> bool {
    crate::screens::create::windows_of(client, cx).contains(&window)
        && crate::screens::threads::channel_of(window, cx).is_none_or(|c| c == at.channel)
}

/// The zone and locale times are drawn in: UTC under a fixture's frozen clock (the references are shot in UTC), else
/// the local zone and the OS region.
fn zone_of(clock: Clock) -> (fn(i64) -> jiff::tz::Offset, String) {
    match clock {
        Clock::Fixed { utc: true, .. } => (|_| jiff::tz::Offset::UTC, "en-US".to_owned()),
        _ => (crate::format::local_zone, crate::format::date_locale()),
    }
}

/// A poll's card under its message (`slots::message_extras`, after the attachments): [`poll_card_in`] for a post in the
/// channel's own log.
pub fn poll_card(
    item: &TimelineItem,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    poll_card_in(item, ctx, None, window, cx)
}

/// The loading line of a poll that is being read from the Bot (PollCard.tsx `poll.loading`): one line of the row's body
/// text, announced as a status.
fn poll_loading() -> Extra {
    let height = line_height(text::INPUT, 1.55);
    let label = t("poll.loading");
    let text_line = line(label, text::INPUT, height, color::FG_BODY);
    Extra {
        element: crate::tracked_box(
            "poll-loading",
            div()
                .id("poll-loading-text")
                .a11y(Role::Status, label)
                .child(text_line),
        )
        .into_any_element(),
        height,
    }
}

/// [`poll_card`] for a post in `thread` (the thread's root; `None` in the channel). The card is for a verified record this
/// device holds. While none is held a post that claims a poll (the pinned Bot's, unedited, with the marker) draws the
/// loading line, and the first such draw starts the read (`Utilities::verify_poll`); a read that was given up draws
/// nothing, so the post's own text is all there is. A post that is not the Bot's claim never reads, nor draws, anything:
/// a forged or unverifiable post is never drawn as a verified poll.
pub fn poll_card_in(
    item: &TimelineItem,
    ctx: &SlotCx,
    thread: Option<dilla_client::state::MessageId>,
    window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    poll_card_led(item, ctx, thread, 0.0, window, cx)
}

/// The card of a post in the channel's log, whose `last` says whether it is the log's last row. The web draws it in the
/// attachments slot's card container (`.d-message-row__card`: the grid's 1px gap and 6px above it), and the card has its
/// own 6px margin above it too; the timeline puts the part under the empty slot's 1px gap and 4px margin (none in the
/// last row), so the card is led by the rest of the container's 6px.
pub fn poll_card_below_body(
    item: &TimelineItem,
    ctx: &SlotCx,
    last: bool,
    window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    let lead = CONTAINER_MARGIN - crate::screens::timeline::attachments_slot(last);
    poll_card_led(item, ctx, None, lead, window, cx)
}

/// MessageRow.css `.d-message-row__attachments.d-message-row__card`: `margin-top: 6px`.
const CONTAINER_MARGIN: f32 = 6.0;

/// [`poll_card_in`] with `lead` px of the box above the card (the loading line has none). A post that ends in the poll
/// marker has the card's container under it whatever is drawn in it (MessageItem.tsx mounts `PollCard` for any marker, and a
/// card that gave up or was never one leaves the container, with its margin, empty), so the row keeps its room: with a
/// `lead`, a post that draws no card is an empty box that high.
fn poll_card_led(
    item: &TimelineItem,
    ctx: &SlotCx,
    thread: Option<dilla_client::state::MessageId>,
    lead: f32,
    window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    let drawn = poll_card_drawn(item, ctx, thread, lead, window, cx);
    if drawn.is_some() || lead <= 0.0 || polls::final_poll_marker(&item.body).is_none() {
        return drawn;
    }
    Some(Extra {
        element: div().into_any_element(),
        height: lead,
    })
}

fn poll_card_drawn(
    item: &TimelineItem,
    ctx: &SlotCx,
    thread: Option<dilla_client::state::MessageId>,
    lead: f32,
    _window: &mut Window,
    _cx: &mut App,
) -> Option<Extra> {
    // PollCard.tsx: only a Bot's unedited post whose verified record this device holds.
    // A post deleted while its read was under way draws nothing either (`poll_claim`'s gate).
    if !item.bot || item.edited || item.state != TimelineItemState::Ok {
        return None;
    }
    let message = item.msg_id?;
    let Some(poll) = ctx.state.bot.polls.get(&message) else {
        return match ctx.state.bot.poll_checks.get(&message) {
            Some(PollCheck::Pending) => Some(poll_loading()),
            // Given up: nothing shows. After a hint of the post's channel (`Retry`) the read runs again quietly, and the
            // card stays given up until it succeeds.
            Some(PollCheck::GaveUp(_)) => None,
            Some(PollCheck::Retry(_)) => {
                if let (Some(client), Some(community), Some(channel)) =
                    (&ctx.client, ctx.community, ctx.channel)
                {
                    client
                        .utilities()
                        .verify_poll(community, channel, thread, item.clone());
                }
                None
            }
            None => {
                let (client, community, channel) =
                    (ctx.client.as_ref()?, ctx.community?, ctx.channel?);
                let utilities = client.utilities();
                let pinned = utilities.bot_identity(community)?.user;
                poll_claim(item, Some(pinned))?;
                utilities.verify_poll(community, channel, thread, item.clone());
                Some(poll_loading())
            }
        };
    };
    // The card is at most 440px of content; at the desktop sizes the log is always wider than the card's box.
    let width = f32::MAX;
    let mine = ctx.state.account.user_id == Some(poll.creator);
    let options = polls::card_options(poll)
        .into_iter()
        .map(|o| poll_card::Option_ {
            text: o.text.into(),
            pct: o.pct,
            mine: o.mine,
            winner: o.winner,
            voters: o.voters.into_iter().map(Into::into).collect(),
        })
        .collect::<Vec<_>>();
    let closed_tag = t("poll.closedTag");
    let height = PollCard::height(
        &poll.question,
        &options,
        width,
        poll.closed.then_some(closed_tag),
        !poll.closed && mine,
    );
    let card = PollCard::new(
        ElementId::Name(format!("poll-{}", item.key).into()),
        poll.question.clone(),
        poll.closed,
        poll.multi,
        poll_meta(poll),
        options,
        poll_card::Labels {
            closed: t("poll.closedTag").into(),
            close: t("poll.close").into(),
        },
        width,
    )
    .can_vote(!poll.closed && ctx.client.is_some())
    .can_close(mine);
    Some(Extra {
        element: div()
            .pt(gpui::px(lead))
            .child(crate::tracked_box("poll-card", card))
            .into_any_element(),
        height: height + lead,
    })
}

/// PollCard.tsx's meta line: `Open for 24 hours · pick one · 0 voters` (or `Final results · …`).
pub fn poll_meta(poll: &PollView) -> String {
    let seconds = poll.due_at.saturating_sub(poll.created_at);
    let duration = if seconds >= 86_400 * 3 {
        tf("poll.days", &[("count", &(seconds / 86_400).to_string())])
    } else if seconds == 3600 {
        t("poll.hour").to_owned()
    } else {
        tf("poll.hours", &[("count", &(seconds / 3600).to_string())])
    };
    let head = if poll.closed {
        t("poll.final").to_owned()
    } else {
        tf("poll.openFor", &[("duration", &duration)])
    };
    let pick = t(if poll.multi {
        "poll.pickAny"
    } else {
        "poll.pickOne"
    })
    .to_owned();
    let count = if poll.multi {
        poll.distinct
    } else {
        poll.counts.iter().sum()
    };
    let voters = if poll.multi && poll.capped {
        tf("poll.voterLowerBound", &[("count", &count.to_string())])
    } else if count == 1 {
        tf("poll.voter", &[("count", "1")])
    } else {
        tf("poll.voterCount", &[("count", &count.to_string())])
    };
    [head, pick, voters].join(" · ")
}

/// The list's rows as the component draws them.
fn list_items(rows: &[picker::Row]) -> Vec<ListItem> {
    rows.iter()
        .map(|r| ListItem {
            id: r.id.clone().into(),
            icon: r.entry.icon,
            name: r.id.clone().into(),
            args: r.entry.args.clone().into(),
            description: r
                .reason
                .clone()
                .unwrap_or_else(|| r.entry.description.clone())
                .into(),
            source: r.entry.source.clone().into(),
            disabled: r.reason.is_some(),
        })
        .collect()
}

/// Whether `ctx`'s community has GIF search on: `/giphy` is listed only then (CommandHost.tsx:186). The first ask reads the
/// instance, and the picker draws again when it answers ([`crate::screens::attachments::ui::gif_on`]).
pub fn gifs_on(ctx: &SlotCx, cx: &mut App) -> bool {
    match (&ctx.client, ctx.community) {
        (Some(client), Some(community)) => {
            crate::screens::attachments::ui::gif_on(client, community, cx)
        }
        _ => false,
    }
}

/// The picker states of the window's other composers, parked while another one is [`activate`]d (the web keeps one
/// `CommandPicker` state per composer, CommandPicker.tsx:153-169: typing in the channel's never clears the thread's).
#[derive(Default)]
struct Parked(Vec<PickerState>);

/// Makes the picker state of the composer whose field is `owner` the window's [`picker_state`], parking the one that was.
/// With `create`, a composer with no state yet gets an empty one (it reports a draft); without, `false` says it has none,
/// and nothing changes. The command to reopen ([`reopen`]) is the window's and stays with whichever state is active.
pub fn activate(
    window: gpui::WindowId,
    owner: &gpui::FocusHandle,
    create: bool,
    cx: &mut App,
) -> bool {
    let is = |s: &PickerState| {
        s.composer
            .as_ref()
            .and_then(gpui::WeakFocusHandle::upgrade)
            .is_some_and(|f| &f == owner)
    };
    if is(picker_state_mut(window, cx)) {
        return true;
    }
    let parked = &mut crate::per_window::get_mut::<Parked>(window, cx).0;
    parked.retain(|p| p.composer.as_ref().is_some_and(|c| c.upgrade().is_some()));
    let incoming = match parked.iter().position(is) {
        Some(i) => parked.remove(i),
        None if create => PickerState {
            composer: Some(owner.downgrade()),
            ..PickerState::default()
        },
        None => return false,
    };
    let state = picker_state_mut(window, cx);
    let reopen = state.reopen.take();
    let mut old = std::mem::replace(state, incoming);
    state.reopen = reopen;
    old.reopen = None;
    if old.composer.is_some() {
        crate::per_window::get_mut::<Parked>(window, cx).0.push(old);
    }
    true
}

/// The composer `owner` starts over with an empty picker (a thread composer shown for another thread: the web remounts it
/// per root, Shell.tsx:2257). The command to reopen is the window's and is kept.
pub fn reset_composer(window: gpui::WindowId, owner: &gpui::FocusHandle, cx: &mut App) {
    if activate(window, owner, false, cx) {
        let state = picker_state_mut(window, cx);
        *state = PickerState {
            composer: state.composer.take(),
            reopen: state.reopen.take(),
            ..PickerState::default()
        };
    }
}

/// Whether the focused field of `window` belongs to a composer with a picker state: it is made the active one, so the
/// keys act on its list and chips. `false` for any other field.
pub fn activate_focused(window: &gpui::Window, cx: &mut App) -> bool {
    let wid = window.window_handle().window_id();
    let mut found = None;
    let mut candidates: Vec<gpui::FocusHandle> = Vec::new();
    candidates.extend(
        crate::per_window::get::<PickerState>(wid, cx).and_then(|s| s.composer.as_ref()?.upgrade()),
    );
    candidates.extend(
        crate::per_window::get::<Parked>(wid, cx)
            .into_iter()
            .flat_map(|p| p.0.iter())
            .filter_map(|p| p.composer.as_ref()?.upgrade()),
    );
    for focus in candidates {
        if focus.is_focused(window) {
            found = Some(focus);
        }
    }
    found.is_some_and(|focus| activate(wid, &focus, false, cx))
}

/// A row of the list of the composer `owner` was hovered: it moves that composer's highlight. Each composer's list acts on
/// its own picker state, not on whichever drew last (a composer with none moves nothing). `true` when the highlight moved.
pub(crate) fn highlight_in(
    window: gpui::WindowId,
    owner: &gpui::FocusHandle,
    i: usize,
    cx: &mut App,
) -> bool {
    if !activate(window, owner, false, cx) {
        return false;
    }
    let s = picker_state_mut(window, cx);
    let moved = s.selected != i;
    s.selected = i;
    moved
}

/// A row of the list of the composer `owner` was clicked: that composer's state picks it (a composer with none picks
/// nothing).
pub(crate) fn pick_in(window: gpui::WindowId, owner: &gpui::FocusHandle, i: usize, cx: &mut App) {
    if activate(window, owner, false, cx) {
        keys::pick(window, i, cx);
    }
}

/// The command picker over the composer whose field is `owner` (`slots::composer_popup`).
pub fn picker(
    ctx: &SlotCx,
    owner: &gpui::FocusHandle,
    window: &mut Window,
    cx: &mut App,
) -> Option<AnyElement> {
    let wid = window.window_handle().window_id();
    if !activate(wid, owner, false, cx) {
        return None;
    }
    let state = crate::per_window::get::<PickerState>(wid, cx)?.clone();
    // A picked command's chips: the popup is the active chip's suggestions instead of the list.
    if let Some(chips) = &state.chips {
        return chips::popup(ctx, owner, chips, gifs_on(ctx, cx), wid);
    }
    let (zone, _) = zone_of(ctx.clock);
    let rows = picker::list(
        &ctx.state,
        ctx.community,
        &state.draft,
        &zone,
        gifs_on(ctx, cx),
    )
    .filter(|_| state.closed_for.as_deref() != Some(state.draft.as_str()));
    let shown = rows
        .as_ref()
        .map(|r| r.iter().map(|r| r.id.clone()).collect::<Vec<_>>());
    if picker_state(wid, cx).rows != shown {
        picker_state_mut(wid, cx).rows = shown;
    }
    let rows = rows?;
    if let Some(community) = ctx.community
        && !ctx.state.roles.access_of(community).loaded
    {
        crate::screens::members::ensure_access(wid, community, ctx.client.as_ref(), cx);
    }
    let active = (!rows.is_empty()).then(|| state.selected.min(rows.len() - 1));
    // For automation (t28-bot-review I5): the highlighted row can be picked and run (a Bot command once its key is
    // verified). Nothing is drawn.
    let ready = active
        .and_then(|i| rows.get(i))
        .is_some_and(|r| r.reason.is_none())
        .then(|| crate::tracked_box("command-active-ready", div()).into_any_element());
    let list = CommandList::new(
        "command-list",
        list_items(&rows),
        ListLabels {
            title: copy::COMMANDS.into(),
            hint: copy::LIST_HINT.into(),
            no_match: copy::no_match(&state.draft).into(),
            send_as_text: copy::SEND_AS_TEXT.into(),
        },
    )
    .active(active)
    .row_ids(|i| ElementId::Name(ROW_IDS.get(i).copied().unwrap_or("command-row").into()))
    .on_active({
        let owner = owner.clone();
        move |i, window, cx| {
            if highlight_in(wid, &owner, i, cx) {
                window.refresh();
            }
        }
    })
    .on_pick({
        let owner = owner.clone();
        move |i, window, cx| {
            pick_in(wid, &owner, i, cx);
            window.refresh();
        }
    });
    // CommandPicker.css: 16px in from the composer's sides, 6px above it.
    Some(
        div()
            .px(dilla_desktop_ui::components::commands::list::PICKER_INSET_X)
            .pb(dilla_desktop_ui::components::commands::list::PICKER_GAP)
            .child(crate::tracked_box(
                "command-picker",
                div()
                    .w_full()
                    .child(list)
                    .children(ready.map(|r| div().absolute().top_0().left_0().child(r))),
            ))
            .into_any_element(),
    )
}

/// What a sent draft does (the composer asks before it posts). Its Debug gives the post's length only (R21).
#[derive(Clone, PartialEq, Eq)]
pub enum SendDecision {
    /// Post this text (a literal draft, `//x` as `/x`, or a built-in that posts).
    Post(String),
    /// Answered privately on this device; clear the draft.
    Handled,
    /// Not complete: keep the draft (the picker says why).
    Keep,
}

impl std::fmt::Debug for SendDecision {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Post(t) => write!(f, "Post({:?})", dilla_client::state::Redacted(t)),
            Self::Handled => write!(f, "Handled"),
            Self::Keep => write!(f, "Keep"),
        }
    }
}

/// Decides what sending `draft` in the conversation of `ctx` does, and runs a private answer. A private answer is
/// added to this device's in-memory results only (`dilla_client::features::bot`): nothing is posted or stored.
///
/// `encode` is the composer's encoder for text (`Composer::encoded`): a built-in that posts or edits text carries it
/// encoded ([`builtins::prepared`]), as builtin.ts sends `context.encodeMessage(text)`.
pub fn on_send(
    ctx: &SlotCx,
    draft: &str,
    encode: &dyn Fn(&str) -> String,
    window: gpui::WindowId,
    cx: &mut App,
) -> SendDecision {
    on_send_in(ctx, None, None, draft, encode, window, cx)
}

/// The picker's draft and chips go once a send of the composer `sender` is taken: its own ([`activate`] brings its state
/// in), never the other composer's. A composer with no picker state clears nothing. `None` (no composer named) is the
/// active one.
fn clear_picker(window: gpui::WindowId, sender: Option<&gpui::FocusHandle>, cx: &mut App) {
    if sender.is_none_or(|focus| activate(window, focus, false, cx)) {
        draft_changed(window, "", cx);
    }
}

/// [`on_send`] for the composer of `thread` in the channel of `ctx` (`None`: the channel's own). The thread binding is the
/// conversation's: its message lookups, its private rows, its command memory and what a post or a reply is sent into
/// (ComposerArea.tsx:292, CommandHost.tsx:84-86).
pub fn on_send_in(
    ctx: &SlotCx,
    thread: Option<dilla_client::state::MessageId>,
    sender: Option<&gpui::FocusHandle>,
    draft: &str,
    encode: &dyn Fn(&str) -> String,
    window: gpui::WindowId,
    cx: &mut App,
) -> SendDecision {
    let Some(channel) = ctx.channel else {
        return SendDecision::Post(draft.to_owned());
    };
    let at = Conversation { channel, thread };
    let plan = bot::plan_at(&ctx.state, ctx.community, Some(at), draft);
    // The web runs `poll create` through its own handler, not as a typed Bot command: the form opens over the
    // conversation (UtilityCommandProvider.tsx `handlers['poll create']`), with or without a client. Its run takes the
    // remembered request id (Task 30), so a create whose answer was uncertain is reconciled, not made twice.
    if let Plan::Bot { definition, args } = &plan
        && definition.id == "poll create"
    {
        poll_form::open(ctx, at, args, attempt_of(ctx, at, draft), window, cx);
        clear_picker(window, sender, cx);
        return SendDecision::Handled;
    }
    // The four announcement commands run through their own handler as well (bot-handlers.ts `announcementHandler`): the
    // Bot reads a schedule, not a typed command.
    if let Plan::Bot { definition, args } = &plan
        && let Some(command) = announcement_form::Command::of(&definition.id)
    {
        announcement_form::run(
            ctx,
            at,
            command,
            args,
            attempt_of(ctx, at, draft),
            window,
            cx,
        );
        clear_picker(window, sender, cx);
        return SendDecision::Handled;
    }
    // `/rolepanel create` runs through its own handler too (bot-handlers.ts `rolePanelHandler`): the editor opens over the
    // conversation, with or without a client. Its run takes the remembered request id (Task 30).
    if let Plan::Bot { definition, .. } = &plan
        && definition.id == role_panel_form::COMMAND
    {
        role_panel_form::run(ctx, at, attempt_of(ctx, at, draft), window, cx);
        clear_picker(window, sender, cx);
        return SendDecision::Handled;
    }
    let decision = match (&ctx.client, plan) {
        (_, Plan::Post(text)) => SendDecision::Post(text),
        (_, Plan::Incomplete) => SendDecision::Keep,
        (Some(client), Plan::Private(status, text, provider)) => {
            client.bot().add_result(at, status, text, provider);
            SendDecision::Handled
        }
        (None, Plan::Private(..)) => SendDecision::Handled,
        (Some(client), Plan::Bot { definition, args }) => {
            let Some(community) = ctx.community else {
                return SendDecision::Keep;
            };
            // bot-run.ts: music has no text channel, so a music command goes with the voice room this device is in (when
            // it is in one); every other Bot command is sent from this channel. Its answer still shows here.
            let send = bot::room::send_channel(&ctx.state, &definition.id, channel);
            // The command itself carries no thread (bot-run.ts has none); only its result row is the thread's.
            client.bot().run_command(
                at,
                community,
                Some(send),
                definition,
                args,
                &local_zone(),
                texts(ctx.clock),
            );
            SendDecision::Handled
        }
        (None, Plan::Bot { .. }) => SendDecision::Handled,
        // `/kick` and `/ban` ask first (CommandHost.tsx's moderation dialog).
        (Some(client), Plan::Moderate(moderation)) => {
            confirm::ask(
                window,
                confirm::Confirming {
                    moderation,
                    conversation: at,
                    client: client.clone(),
                },
                cx,
            );
            SendDecision::Handled
        }
        (None, Plan::Moderate(_)) => SendDecision::Handled,
        // Another area's built-in: its row is pending from the start (CommandHost.tsx:236), then it asks, or runs.
        (client, Plan::Builtin { run, confirm }) => {
            let conversation = at;
            let attempt = attempt_of(ctx, at, draft);
            // builtin.ts:225-230: a `/timeout` whose request id was already sent asks nothing again; it reads the
            // timeout (or refuses another member or length) as it runs.
            let confirm = confirm.filter(|_| {
                !client
                    .as_ref()
                    .is_some_and(|c| builtins::timeout_was_sent(c, &run, attempt.request))
            });
            let run = builtins::prepared_in(run, &ctx.state, at, encode);
            let text = copy::pending(run.id());
            let row = match client {
                Some(c) => c.bot().add_result(
                    conversation,
                    ResultStatus::Pending,
                    text,
                    ResultProvider::Builtin,
                ),
                None => add_fixture_row(window, conversation, ctx.clock, text, cx),
            };
            match confirm {
                Some(ask) => confirm::ask_builtin(
                    window,
                    confirm::AskingBuiltin {
                        ask,
                        run,
                        row,
                        attempt,
                        conversation,
                        client: client.clone(),
                    },
                    cx,
                ),
                // A fixture has no client to run it with.
                None if client.is_some() => {
                    builtins::run(run, row, attempt, conversation, ctx, window, cx);
                }
                None => builtins::run_in_fixture(run, row, conversation, window, cx),
            }
            SendDecision::Handled
        }
    };
    if decision != SendDecision::Keep {
        clear_picker(window, sender, cx);
    }
    decision
}

/// The local zone a reminder's `when` is read in: the OS zone's name and its offset at each instant.
fn local_zone() -> run::Zone {
    let name = std::env::var("TZ")
        .ok()
        .filter(|z| run_zone_name_ok(z))
        .unwrap_or_else(|| "UTC".to_owned());
    run::Zone {
        name,
        offset_at: std::sync::Arc::new(|t| crate::format::local_zone(t).seconds()),
    }
}

fn run_zone_name_ok(z: &str) -> bool {
    dilla_client::features::utilities::time::valid_zone_name(z)
}

/// The private results' copy (bot-run.ts, copy.ts), in the strings and the times of this app.
fn texts(clock: Clock) -> run::Texts {
    std::sync::Arc::new(move |text: &run::Text| {
        let (zone, _) = zone_of(clock);
        let since = |s: &Option<u64>| {
            s.map_or_else(|| "—".to_owned(), |s| crate::format::time_in(s, &zone))
        };
        match text {
            run::Text::Pending(id) => copy::pending(id),
            run::Text::NoAnswer => t("commands.botNoAnswer").to_owned(),
            run::Text::Failed => t("privateResult.failed").to_owned(),
            run::Text::Forbidden => t("commands.botForbidden").to_owned(),
            run::Text::Disabled => t("commands.botDisabled").to_owned(),
            run::Text::Offline(s) => tf("commands.botOffline", &[("time", &since(s))]),
            run::Text::Degraded(s) => tf("commands.botDegraded", &[("time", &since(s))]),
            run::Text::Absent => t("commands.botNotOnInstance").to_owned(),
            run::Text::NeedsBotChannel => t("commands.needsBotChannel").to_owned(),
            run::Text::CantCheckDevice => t("commands.botCantCheckDevice").to_owned(),
            run::Text::Unverified => t("commands.botUnverified").to_owned(),
            run::Text::Changed => t("commands.botChanged").to_owned(),
            run::Text::PostedIn { what, channel } => {
                tf("commands.postedIn", &[("what", what), ("channel", channel)])
            }
            run::Text::WholeNumber(name) => format!("{name} must be a whole number"),
            run::Text::ChooseFrom(name) => format!("Choose {name} from the suggestions."),
            run::Text::Duration(name) => format!("{name} must be a duration such as 10m or 2h"),
            run::Text::WhenHint => copy::WHEN_HINT.to_owned(),
        }
    })
}

/// A fixture window's built-in rows. A fixture has no client to keep private results, yet the screen it stands for
/// shows the command's pending row under the log (CommandHost.tsx:236), so the window keeps it here.
#[derive(Default)]
struct FixtureRows(Vec<(Conversation, PrivateResult)>);

/// Adds a fixture window's pending row for a built-in (`text` its pending copy); answers its id.
fn add_fixture_row(
    window: gpui::WindowId,
    conversation: Conversation,
    clock: Clock,
    text: String,
    cx: &mut App,
) -> u64 {
    let at = match clock {
        Clock::Fixed { now, .. } => now * 1000,
        Clock::Local => std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| u64::try_from(d.as_millis()).unwrap_or(u64::MAX)),
    };
    let rows = &mut crate::per_window::get_mut::<FixtureRows>(window, cx).0;
    let id = rows.iter().map(|(_, r)| r.id).max().unwrap_or(0) + 1;
    rows.push((
        conversation,
        PrivateResult {
            id,
            at,
            status: ResultStatus::Pending,
            text,
            provider: ResultProvider::Builtin,
            hold: false,
            settled_at: None,
        },
    ));
    id
}

/// A fixture window's row `row` ends as `status` with `text` (a built-in that needs no client ran).
pub(super) fn finish_fixture_row(
    window: gpui::WindowId,
    row: u64,
    status: ResultStatus,
    text: String,
    cx: &mut App,
) {
    for (_, r) in &mut crate::per_window::get_mut::<FixtureRows>(window, cx).0 {
        if r.id == row {
            r.status = status;
            r.text.clone_from(&text);
        }
    }
}

/// A fixture window's row `row` goes (its question was cancelled or abandoned).
pub(super) fn dismiss_fixture_row(window: gpui::WindowId, row: u64, cx: &mut App) {
    crate::per_window::get_mut::<FixtureRows>(window, cx)
        .0
        .retain(|(_, r)| r.id != row);
}

/// The private results of the open conversation, oldest first, each with its height in a log `width` wide: the log
/// draws them after its last message. A fixture window draws its own built-in rows ([`FixtureRows`]).
pub fn private_results(
    ctx: &SlotCx,
    width: f32,
    window: &mut Window,
    cx: &mut App,
) -> Vec<(AnyElement, f32)> {
    private_results_in(ctx, None, width, window, cx)
}

/// [`private_results`] of `thread`'s pane (`None`: the channel's own log): the rows are kept per thread
/// (CommandHost.tsx:74) and drawn after its replies (ThreadView.tsx:243-244).
pub fn private_results_in(
    ctx: &SlotCx,
    thread: Option<dilla_client::state::MessageId>,
    width: f32,
    window: &mut Window,
    cx: &mut App,
) -> Vec<(AnyElement, f32)> {
    let Some(channel) = ctx.channel else {
        return Vec::new();
    };
    let conversation = Conversation { channel, thread };
    let (zone, _) = zone_of(ctx.clock);
    let fixture_rows: Vec<PrivateResult> = if ctx.client.is_none() {
        crate::per_window::get::<FixtureRows>(window.window_handle().window_id(), cx)
            .map(|rows| {
                rows.0
                    .iter()
                    .filter(|(c, _)| *c == conversation)
                    .map(|(_, r)| r.clone())
                    .collect()
            })
            .unwrap_or_default()
    } else {
        Vec::new()
    };
    ctx.state
        .bot
        .results(&conversation)
        .iter()
        .chain(&fixture_rows)
        .map(|row| {
            let height = BotResult::height(&row.text, width);
            (
                result_row(row, conversation, ctx, width, &zone).into_any_element(),
                height,
            )
        })
        .collect()
}

fn result_row(
    row: &PrivateResult,
    conversation: Conversation,
    ctx: &SlotCx,
    width: f32,
    zone: crate::format::Zone,
) -> impl IntoElement + use<> {
    let (status, label) = match row.status {
        ResultStatus::Pending => (Status::Pending, copy::STATUS_PENDING),
        ResultStatus::Success => (Status::Success, copy::STATUS_SUCCESS),
        ResultStatus::Failed => (Status::Failed, copy::STATUS_FAILED),
        ResultStatus::Cancelled => (Status::Cancelled, copy::STATUS_CANCELLED),
    };
    let author = match row.provider {
        ResultProvider::Builtin => Author::Builtin {
            name: copy::AUTHOR_BUILTIN.into(),
        },
        ResultProvider::Bot => Author::Bot {
            name: picker::bot_name(&ctx.state, ctx.community).into(),
        },
    };
    let client = ctx.client.clone();
    let dismiss_client = client.clone();
    let id = row.id;
    let result = BotResult::new(
        ElementId::Name(format!("bot-result-{id}").into()),
        author,
        crate::format::time_in(row.at / 1000, zone),
        status,
        row.text.clone(),
        bot_result::Labels {
            status: label.into(),
            only_you: copy::ONLY_YOU.into(),
            dismiss: copy::DISMISS.into(),
        },
        width,
    )
    .on_dismiss(move |window, _cx| {
        if let Some(client) = &dismiss_client {
            client.bot().dismiss(conversation, id);
        }
        window.refresh();
    });
    let result = if row.hold
        && let Some(client) = client
    {
        result.on_retry(t("commands.retry"), move |window, _cx| {
            client.bot().retry(id);
            window.refresh();
        })
    } else {
        result
    };
    // For automation: the row's status (the last row of a status wins). Nothing is drawn.
    let marker = match row.status {
        ResultStatus::Pending => "bot-result-pending",
        ResultStatus::Success => "bot-result-success",
        ResultStatus::Failed => "bot-result-failed",
        ResultStatus::Cancelled => "bot-result-cancelled",
    };
    div().relative().w_full().child(result).child(
        div()
            .absolute()
            .top_0()
            .left_0()
            .child(crate::tracked_box(marker, div())),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::{ChannelId, UserId};

    /// R20: a moderation command being typed (its draft and its chips) prints neither the member nor the reason.
    #[test]
    fn a_moderation_draft_keeps_the_member_and_reason_out_of_debug() {
        let mut chips = chips::Chips::new("warn".to_owned());
        chips.values = vec!["Bjornsson".to_owned(), "secret-reason".to_owned()];
        chips.shown = vec!["Bjornsson".to_owned()];
        chips.labels = vec!["Bjornsson".to_owned()];
        chips.picked = vec![Some("Bjornsson".to_owned()), None];
        chips.sug_for = "Bjornsson".to_owned();
        let picker = PickerState {
            draft: "/warn Bjornsson secret-reason".to_owned(),
            closed_for: Some("/warn Bjornsson secret-reason".to_owned()),
            rows: Some(vec!["Bjornsson".to_owned()]),
            picked: Some("warn".to_owned()),
            chips: Some(chips.clone()),
            ..PickerState::default()
        };
        let offered = chips::Offered {
            title: "member",
            items: vec![chips::Suggestion {
                label: "Bjornsson".to_owned(),
                value: "Bjornsson".to_owned(),
                sub: "@bjornsson".to_owned(),
                icon: dilla_desktop_ui::IconName::User,
                avatar: Some("Bjornsson".to_owned()),
            }],
            can_accept: true,
            enter_accepts: true,
        };
        for shown in [
            format!("{chips:?}"),
            format!("{picker:?}"),
            format!("{picker:#?}"),
            format!("{offered:?}").to_lowercase(),
            // R21 round 1: the draft a send posts.
            format!(
                "{:?}",
                SendDecision::Post("Bjornsson secret-reason".to_owned())
            ),
        ] {
            assert!(!shown.contains("Bjornsson"), "a name");
            assert!(!shown.contains("secret"), "a reason");
        }
    }

    fn poll(counts: Vec<u64>, closed: bool, multi: bool, seconds: u64) -> PollView {
        PollView {
            id: [1; 16],
            channel: ChannelId([2; 16]),
            thread: None,
            creator: UserId([3; 16]),
            question: "What should we raid on Friday?".into(),
            answers: vec!["the bridge".into(), "the harbour".into()],
            multi,
            created_at: 1000,
            due_at: 1000 + seconds,
            closed,
            counts,
            my_choices: None,
            revision: 1,
            previews: vec![],
            distinct: 4,
            capped: true,
        }
    }

    #[test]
    fn the_meta_line_follows_the_web_card() {
        assert_eq!(
            poll_meta(&poll(vec![0, 0], false, false, 86_400)),
            "Open for 24 hours · pick one · 0 voters"
        );
        assert_eq!(
            poll_meta(&poll(vec![1, 0], false, false, 3600)),
            "Open for 1 hour · pick one · 1 voter"
        );
        assert_eq!(
            poll_meta(&poll(vec![2, 1], true, false, 259_200)),
            "Final results · pick one · 3 voters"
        );
        assert_eq!(
            poll_meta(&poll(vec![2, 1], false, true, 604_800)),
            "Open for 7 days · pick any · 4+ voters"
        );
    }

    fn bot_post(message: u8) -> TimelineItem {
        TimelineItem {
            key: format!("m{message}"),
            state: dilla_client::state::TimelineItemState::Ok,
            reason: String::new(),
            sender_user: Some(UserId([0xb0; 16])),
            sender_device: None,
            own: false,
            bot: true,
            ts: 1,
            body: "Mira asked:\n[dilla-poll:v1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa]".into(),
            msg_id: Some(dilla_client::state::MessageId([message; 16])),
            seq: Some(1),
            edited: false,
            reply: None,
            reactions: vec![],
            pinned: false,
            attachments: vec![],
            mention: false,
            actions: Vec::new(),
        }
    }

    /// PollCard.tsx: a poll being read says so (`poll.loading`), a held record is the card, a read that was given up
    /// draws nothing, and a post with no check and no client to read it draws nothing: never a card that is not verified.
    #[gpui::test]
    fn a_claimed_poll_shows_loading_until_verified(cx: &mut gpui::TestAppContext) {
        let (_, vcx) = cx.add_window_view(|_, _| gpui::Empty);
        let item = bot_post(7);
        let message = item.msg_id.unwrap();
        let draw = |bot: dilla_client::state::bot::BotState, vcx: &mut gpui::VisualTestContext| {
            let ctx = SlotCx {
                state: std::sync::Arc::new(dilla_client::state::AppState {
                    bot,
                    ..Default::default()
                }),
                ..SlotCx::default()
            };
            vcx.update(|window, cx| poll_card(&item, &ctx, window, cx))
        };
        let mut bot = dilla_client::state::bot::BotState::default();
        assert!(draw(bot.clone(), vcx).is_none(), "nothing reads it here");
        bot.poll_checks.insert(message, PollCheck::Pending);
        assert!(draw(bot.clone(), vcx).is_some_and(|e| e.height > 0.0));
        bot.poll_checks
            .insert(message, PollCheck::GaveUp(ChannelId([4; 16])));
        assert!(draw(bot.clone(), vcx).is_none());
        // A hint of its channel reads it again quietly: it still shows as given up (no loading line) until it succeeds.
        bot.poll_checks
            .insert(message, PollCheck::Retry(ChannelId([4; 16])));
        assert!(draw(bot.clone(), vcx).is_none());
        bot.polls
            .insert(message, poll(vec![0, 0], false, false, 86_400));
        assert!(draw(bot, vcx).is_some_and(|e| e.height > 0.0));
        // A post deleted while its read was under way stops showing "Loading poll…".
        let mut deleted = bot_post(7);
        deleted.state = dilla_client::state::TimelineItemState::Deleted;
        let mut pending = dilla_client::state::bot::BotState::default();
        pending.poll_checks.insert(message, PollCheck::Pending);
        let ctx = SlotCx {
            state: std::sync::Arc::new(dilla_client::state::AppState {
                bot: pending,
                ..Default::default()
            }),
            ..SlotCx::default()
        };
        assert!(vcx.update(|window, cx| poll_card(&deleted, &ctx, window, cx).is_none()));
    }

    /// MessageItem.tsx mounts the card's container for any post that ends in the poll marker, so a post whose card gave up (or
    /// that no card is drawn for) keeps the container's room under it: the 6px margin less what the row's empty slot already
    /// holds (none in the log's last row, 1px gap and 4px margin elsewhere), and a post with no marker keeps nothing.
    #[gpui::test]
    fn a_marker_post_with_no_card_keeps_the_containers_room(cx: &mut gpui::TestAppContext) {
        let (_, vcx) = cx.add_window_view(|_, _| gpui::Empty);
        let item = bot_post(7);
        let mut bot = dilla_client::state::bot::BotState::default();
        bot.poll_checks
            .insert(item.msg_id.unwrap(), PollCheck::GaveUp(ChannelId([4; 16])));
        let ctx = SlotCx {
            state: std::sync::Arc::new(dilla_client::state::AppState {
                bot,
                ..Default::default()
            }),
            ..SlotCx::default()
        };
        let room = |item: &TimelineItem, last: bool, vcx: &mut gpui::VisualTestContext| {
            vcx.update(|window, cx| {
                poll_card_below_body(item, &ctx, last, window, cx).map(|e| e.height)
            })
        };
        let slot = crate::screens::timeline::attachments_slot(false);
        assert_eq!(room(&item, true, vcx), Some(CONTAINER_MARGIN));
        assert_eq!(room(&item, false, vcx), Some(CONTAINER_MARGIN - slot));
        // The thread pane's card has no container lead: nothing is added.
        assert!(vcx.update(|window, cx| poll_card_in(&item, &ctx, None, window, cx).is_none()));
        let mut plain = bot_post(7);
        plain.body = "Mira asked:".into();
        assert_eq!(room(&plain, true, vcx), None);
    }

    /// Integration 3, final review M4: each window's composer has its own `/` list: a draft typed in window A opens
    /// nothing in window B, a send in B leaves A's list as it was, and B's keys never move A's highlight.
    #[gpui::test]
    fn each_window_has_its_own_command_list(cx: &mut gpui::TestAppContext) {
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let b = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        cx.update(|cx| {
            draft_changed(a, "/r", cx);
            picker_state_mut(a, cx).rows = Some(vec!["remind".into(), "roll".into()]);
            assert_eq!(
                picker_state(b, cx).draft,
                "",
                "B's composer reported nothing"
            );
            draft_changed(b, "", cx);
            assert_eq!(picker_state(a, cx).draft, "/r", "B's send leaves A's draft");
            assert!(!keys::apply(b, picker::Key::Down, cx), "B has no list open");
            assert_eq!(picker_state(a, cx).selected, 0);
            assert!(keys::apply(a, picker::Key::Down, cx));
            assert_eq!(picker_state(a, cx).selected, 1);
        });
    }

    /// Task 8's `Plan::Builtin` arm as the composer reaches it: `/delete` of an own message asks first (a fixture, without
    /// a client, still asks so the dialog can be shot), `/pin` runs at once and asks nothing, and a refusal is answered
    /// without a question. Each clears the draft. With a client, the pending row comes first: tests/bot.rs
    /// `a_builtin_sent_from_the_composer_shows_its_row_then_asks_or_runs`.
    #[gpui::test]
    fn a_builtin_send_asks_or_runs(cx: &mut gpui::TestAppContext) {
        let (state, dm, msg) = builtins::tests::dm_with("hello");
        let ctx = SlotCx {
            state: std::sync::Arc::new(state),
            channel: Some(dm),
            ..SlotCx::default()
        };
        let w = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let encode = |t: &str| t.to_owned();
        let id = msg.hex();
        // A fixture window's own pending rows (it has no client to keep them).
        let rows = |cx: &App| {
            crate::per_window::get::<FixtureRows>(w, cx).map_or(Vec::new(), |r| {
                r.0.iter()
                    .map(|(_, r)| (r.status, r.text.clone()))
                    .collect()
            })
        };
        cx.update(|cx| {
            for (draft, asks, pending) in [
                (format!("/delete message:{id}"), true, Some("delete")),
                (format!("/pin message:{id}"), false, Some("pin")),
                (
                    format!("/edit message:{} text:x", ChannelId([9; 16]).hex()),
                    false,
                    None,
                ),
            ] {
                draft_changed(w, &draft, cx);
                assert_eq!(on_send(&ctx, &draft, &encode, w, cx), SendDecision::Handled);
                assert_eq!(confirm::is_open(w, cx), asks, "{draft}");
                assert_eq!(picker_state(w, cx).draft, "", "the draft is cleared");
                assert_eq!(
                    rows(cx).last().cloned(),
                    pending.map(|id| (ResultStatus::Pending, copy::pending(id))),
                    "{draft}"
                );
                if asks {
                    // Abandoned: the question and its row go.
                    confirm::answer_builtin(w, None, cx);
                    assert!(rows(cx).is_empty());
                } else {
                    crate::per_window::get_mut::<FixtureRows>(w, cx).0.clear();
                }
            }
        });
    }

    /// CommandHost.tsx:233, :291: `/warn` is not CommandHost's `moderation` (only `/kick` and `/ban` are), so both a
    /// Cancel and an abandon (a click outside, leaving the conversation) take its pending row away.
    #[gpui::test]
    fn a_cancelled_or_abandoned_warn_leaves_no_row(cx: &mut gpui::TestAppContext) {
        use dilla_client::features::bot::{Ask, Builtin};
        use dilla_client::state::{CommunityId, UserId};
        let w = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let conversation = Conversation {
            channel: ChannelId([5; 16]),
            thread: None,
        };
        let rows = |cx: &App| {
            crate::per_window::get::<FixtureRows>(w, cx).map_or(Vec::new(), |r| {
                r.0.iter()
                    .map(|(_, r)| (r.status, r.text.clone()))
                    .collect()
            })
        };
        cx.update(|cx| {
            for choice in [Some(false), None] {
                crate::per_window::get_mut::<FixtureRows>(w, cx).0.clear();
                let row = add_fixture_row(w, conversation, Clock::Local, copy::pending("warn"), cx);
                confirm::ask_builtin(
                    w,
                    confirm::AskingBuiltin {
                        ask: Ask {
                            title: "Warn bea?".into(),
                            body: "spam".into(),
                        },
                        run: Builtin::Warn {
                            community: CommunityId([7; 16]),
                            user: UserId([2; 16]),
                            name: "bea".into(),
                            reason: "spam".into(),
                        },
                        row,
                        attempt: bot::Attempt {
                            request: [1; 16],
                            invocation: String::new(),
                            text: String::new(),
                        },
                        conversation,
                        client: None,
                    },
                    cx,
                );
                assert_eq!(rows(cx).len(), 1, "the row is pending while asked");
                confirm::answer_builtin(w, choice, cx);
                assert!(!confirm::is_open(w, cx));
                assert!(rows(cx).is_empty(), "{choice:?}");
            }
        });
    }

    #[test]
    fn a_new_draft_reopens_the_list_and_resets_the_highlight() {
        let mut app = PickerState {
            draft: "/".into(),
            selected: 3,
            closed_for: Some("/".into()),
            rows: None,
            picked: None,
            chips: None,
            composer: None,
            reopen: None,
        };
        // draft_changed's rule, applied without an App.
        let next = "/r";
        if app.draft != next {
            app.draft = next.into();
            app.selected = 0;
            if app.closed_for.as_deref() != Some(next) {
                app.closed_for = None;
            }
        }
        assert_eq!((app.selected, app.closed_for), (0, None));
    }
}
