//! The message log (packages/ui MessageLog and MessageRow, web `screens/conversation`, Signal §5.5): the channel's
//! welcome, day dividers, and one row per message: the 44px time gutter, the 30px avatar, the author and the body. The
//! body is the message's markdown laid out as the web lays it out ([`crate::screens::messages::body`]). A row continues
//! the run above it (no gutter time, avatar or author) when the same author wrote it within seven minutes, on the same
//! day (log-model.ts `groupedWithAbove`). The log keeps to its end until the person scrolls up, as the web log does at
//! present; PageUp and PageDown scroll it.
//!
//! Every height is the web CSS's (MessageLog.css, MessageRow.css) in Chromium's layout units, so the log's content has
//! the web's height and, at its end, the web's (whole-pixel) scroll offset. The log is not virtualised: every row of the
//! open timeline (the client's page: 100 rows, and 100 more for each "load earlier") is laid out.
//!
//! While the history has more, a ghost "load earlier" button sits centred above the first row in the welcome's place; the
//! welcome shows only at the history's true start. Rows put in front keep the reader's place (MessageLog.tsx:103-114).
//!
//! Each row is a box of its own at its CSS position: it takes the hover fill and bar (`--bg-3`, 2px `--accent`), the
//! mention tint (`--warn-soft`, 2px `--mention`) and the flash of a jump, and holds the toolbar over its top edge.

use dilla_client::features::utilities::polls;
use dilla_client::state::bot::PollCheck;
use dilla_client::state::bot_admin::{MusicMarker, music_marker};
use dilla_client::state::{
    AppState, Channel, ChannelId, CommunityId, Member, Timeline, TimelineItem, TimelineItemState,
    UserId,
};
use gpui::{
    AnyElement, App, ElementId, InteractiveElement, IntoElement, ParentElement, ScrollHandle,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::button::{Button, ButtonVariant};
use dilla_desktop_ui::components::tag::{Tag, TagKind, look};
use dilla_desktop_ui::components::{Avatar, AvatarShape, AvatarSize};
use dilla_desktop_ui::metrics::Density;
use dilla_desktop_ui::motion;
use dilla_desktop_ui::text::{
    Face, Line, MONO_ADVANCE, chromium_baseline, line, line_height, paragraph,
};
use dilla_desktop_ui::tokens::{color, font, motion as token, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::format;
use crate::screens::LABEL_TRACKING_WIDE;
use crate::screens::messages::body::{self, Laid};
use crate::screens::messages::css::len;
use crate::screens::messages::markdown::{self, Body, Pill};
use crate::screens::messages::ui::{self, MessageUi};
use crate::screens::messages::{editor, toolbar};
use crate::screens::shell::Clock;
use crate::slots::{self, SlotCx};
use crate::strings::{t, tf};

/// log-model.ts `GROUP_SECONDS`.
pub const GROUP_SECONDS: u64 = 7 * 60;

/// MessageRow.css: the 44px gutter, the 30px avatar, 10px between the columns, 16px right of the content.
const GUTTER: f32 = 44.0;
const AVATAR: f32 = 30.0;
const COLUMN_GAP: f32 = 10.0;
const ROW_RIGHT: f32 = 16.0;
/// The content's left edge: gutter, gap, avatar, gap.
const CONTENT_X: f32 = GUTTER + COLUMN_GAP + AVATAR + COLUMN_GAP;
/// A run's first row: 7px above, 3px below; a follow-up: 1px above and below. The gutter's time sits 18px down.
const ROW_TOP: f32 = 7.0;
const ROW_BOTTOM: f32 = 3.0;
const FOLLOW_PAD: f32 = 1.0;
const GUTTER_TOP: f32 = 18.0;
/// MessageRow.css `data-reply`: a reply's avatar moves 20px down (`margin-top: 1.25rem`), and so does its gutter's time
/// (`padding-top: 2.375rem`, the 18px plus 20px).
const REPLY_AVATAR_SHIFT: f32 = 20.0;
/// The row grid's 1px gap, and the empty attachment slot's 4px margin (`0.25rem`) every row keeps but the log's last.
const MAIN_GAP: f32 = 1.0;
const ATTACHMENTS_MARGIN: f32 = 4.0;
/// The head's gap between its parts (`gap: 0.5rem`).
const HEAD_GAP: f32 = 8.0;
/// MessageLog.css: 12px above the content, 6px below; the welcome's 16px 20px 18px padding, its 40px tile in a 1px
/// border and the title's 10px margin; the day divider's 4px 20px padding and 10px gap.
const LOG_TOP: f32 = 12.0;
const LOG_BOTTOM: f32 = 6.0;
/// The channel log's scope for what its rows remember of their entrances ([`motion::scope`]).
pub const LOG_SCOPE: &str = "channel-log";

/// What the arrows keep between the row they move to and the log's edge when they scroll it into view.
const REVEAL_MARGIN: f32 = 8.0;
const WELCOME_TOP: f32 = 16.0;
const WELCOME_X: f32 = 20.0;
const WELCOME_BOTTOM: f32 = 18.0;
const WELCOME_TILE: f32 = 42.0;
const WELCOME_TITLE_GAP: f32 = 10.0;
const DIVIDER_PAD: f32 = 4.0;
const DIVIDER_GAP: f32 = 10.0;
/// MessageLog.css `.d-message-log__earlier`: the 32px button (Button.css `.d-btn`); its `margin-bottom:
/// var(--group-gap)` steps with the density ([`Density::group_gap`]).
const EARLIER_HEIGHT: f32 = 32.0;

fn body_line() -> f32 {
    body::body_line()
}

// The head, its marks, the day divider and the welcome set no line height of their own (MessageRow.css, MessageLog.css):
// they take the root's `--line-height`, which steps with the density (Task 19b). The gutter's time keeps its own 1.5 and
// the body its 1.55, at every density.

fn head_line(d: Density) -> f32 {
    line_height(text::SM, d.line_height())
}
fn meta_line(d: Density) -> f32 {
    line_height(text::META, d.line_height())
}
/// `.d-message-row__gutter { line-height: 1.5 }`.
fn gutter_line() -> f32 {
    line_height(text::META, 1.5)
}
fn divider_height(d: Density) -> f32 {
    2.0 * DIVIDER_PAD + line_height(text::MICRO, d.line_height())
}
fn welcome_title_line(d: Density) -> f32 {
    line_height(text::WELCOME, d.line_height())
}
fn welcome_body_line(d: Density) -> f32 {
    line_height(text::UI, d.line_height())
}

/// The scroll handler of the log: whether the wheel moved up.
pub type OnWheel = std::rc::Rc<dyn Fn(bool, &mut gpui::App)>;

/// The "load earlier" button's press (Shell.tsx `loadEarlier`).
pub type OnEarlier = std::rc::Rc<dyn Fn(&mut gpui::App)>;

/// What the log shows above its first row (Conversation.tsx:394-399).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum LogHead {
    /// The timeline is not read yet.
    Loading,
    /// The history has more: the "load earlier" button.
    Earlier,
    /// The history's true start: the welcome.
    Welcome,
}

/// Conversation.tsx:396-399: the button while `hasEarlier`, the welcome only once the history is complete.
pub fn log_head(t: Option<&Timeline>) -> LogHead {
    match t {
        None => LogHead::Loading,
        Some(t) if t.has_earlier => LogHead::Earlier,
        Some(_) => LogHead::Welcome,
    }
}

/// MessageLog.tsx:103-114: the scroll offset that keeps the reader's place. A log that is not pinned and whose first row
/// changed had rows put in front, and moves by the whole growth of its height (a row arriving at the bottom in the same
/// frame included, as the web's does); a pinned log is set by the pin, and a first draw has nothing to keep.
pub fn anchored_offset(
    prev_first: Option<&str>,
    first: Option<&str>,
    prev_height: f32,
    height: f32,
    offset: f32,
    pinned: bool,
) -> f32 {
    if pinned || prev_first.is_none() || prev_first == first {
        return offset;
    }
    (offset + height - prev_height).max(0.0)
}

/// Shell.tsx:1788-1793 `scrollIntoView({ block: 'center' })`: the scroll offset that puts a row of `height` at `top` in the
/// middle of a viewport of `viewport` over content of `inner`, kept within the log.
pub fn centred_offset(top: f32, height: f32, viewport: f32, inner: f32) -> f32 {
    (top + height / 2.0 - viewport / 2.0).clamp(0.0, (inner - viewport).max(0.0))
}

/// What the log draws for one item.
struct Shown<'a> {
    item: &'a TimelineItem,
    grouped: bool,
    /// A follow-up that keeps a head line of its marks ([`marks_head`]).
    marks: bool,
    divider: Option<String>,
    last: bool,
    /// The body: its height now (from the memo), its elements only when the row is drawn.
    body: Plan,
    /// What other areas draw under the body ([`slots::message_extras`]), and its height.
    extras: Option<AnyElement>,
    extras_height: f32,
    /// What another area draws above the head (the reply line, [`slots::message_before_head`]), and its height.
    before: Option<AnyElement>,
    before_height: f32,
    /// The row's hover group.
    group: String,
    /// The row's place in the log: its automation ids are numbered by it.
    index: usize,
}

impl Shown<'_> {
    fn shape(&self) -> RowShape {
        RowShape {
            grouped: self.grouped,
            marks: self.marks,
            last: self.last,
            before: before_shift(self.before_height),
            body: self.body.height(),
            extras: self.extras_height,
            divider: self.divider.is_some(),
        }
    }
}

/// A row's body before it is drawn: the editor (always drawn), or the text and what it needs to be laid out.
enum Plan {
    Editor(Laid),
    Text {
        parts: Vec<Body>,
        color: gpui::Hsla,
        height: f32,
    },
}

impl Plan {
    fn height(&self) -> f32 {
        match self {
            Self::Editor(l) => l.height,
            Self::Text { height, .. } => *height,
        }
    }

    /// The elements, laid out in `width`.
    fn laid(self, width: f32, y0: f32, code_age: Option<std::time::Duration>) -> Laid {
        match self {
            Self::Editor(l) => l,
            Self::Text {
                parts,
                color,
                height,
            } => {
                let mut laid = body::layout_entering(&parts, width, color, y0, code_age);
                laid.height = height;
                laid
            }
        }
    }
}

/// What a row's height depends on.
#[derive(Clone, Copy, Debug)]
struct RowShape {
    grouped: bool,
    /// A follow-up's head line of marks ([`marks_head`]).
    marks: bool,
    last: bool,
    /// How far the head and the body move down for what is drawn above the head ([`before_shift`]).
    before: f32,
    /// The height of the row's body (its text, code cards and quotes, or the editor).
    body: f32,
    /// The height of the row's extras under its body (0 when no area draws any).
    extras: f32,
    /// A day divider above the row.
    divider: bool,
}

/// Each row's top, from `start`, and the end of the last row: a divider, then the row, one after another.
fn row_tops(start: f32, rows: &[RowShape], d: Density) -> (Vec<f32>, f32) {
    let mut y = start;
    let mut tops = Vec::with_capacity(rows.len());
    for r in rows {
        if r.divider {
            y += divider_height(d);
        }
        tops.push(y);
        y += r.height(d);
    }
    (tops, y)
}

impl RowShape {
    /// The row's height at the density `d` ([`row_height`]).
    fn height(&self, d: Density) -> f32 {
        row_height(
            self.grouped,
            self.marks,
            self.last,
            self.before,
            self.body,
            self.extras,
            d,
        )
    }
}

fn readable(i: &TimelineItem) -> bool {
    matches!(
        i.state,
        TimelineItemState::Ok | TimelineItemState::Pending | TimelineItemState::Failed
    )
}

/// log-model.ts `startsDay`: the item starts a local day the row above it (if any) is not in.
pub fn starts_day(prev: Option<&TimelineItem>, item: &TimelineItem, clock: Clock) -> bool {
    prev.is_none_or(|p| clock.day(p.ts) != clock.day(item.ts))
}

/// log-model.ts `groupedWithAbove`.
pub fn grouped_with_above(prev: Option<&TimelineItem>, item: &TimelineItem, clock: Clock) -> bool {
    let Some(prev) = prev else { return false };
    if item.sender_user.is_none() || prev.sender_user != item.sender_user {
        return false;
    }
    if !readable(prev) || !readable(item) || item.reply.is_some() {
        return false;
    }
    if starts_day(Some(prev), item, clock) {
        return false;
    }
    item.ts >= prev.ts && item.ts - prev.ts <= GROUP_SECONDS
}

/// The offset of the clock's zone at `secs`, in seconds east of UTC.
fn zone_of(clock: Clock, secs: i64) -> jiff::tz::Offset {
    clock.offset(secs.max(0))
}

/// log-model.ts `dayLabel`: today, yesterday, else the date.
pub fn day_label(ts: u64, clock: Clock) -> String {
    let zone = |secs: i64| zone_of(clock, secs);
    format::day_divider_in(ts, clock.now(), &zone, &format::date_locale())
}

/// The people a conversation names its authors from: a channel's server members; a DM's (a channel of no server) every
/// server's members, a person's first row kept (Shell.tsx `allMembers`, which `nameBook` reads for a DM).
fn log_members<'a>(state: &'a AppState, channel: &Channel) -> std::borrow::Cow<'a, [Member]> {
    use std::borrow::Cow;
    if state.dms.get(channel.id).is_none() {
        return Cow::Borrowed(
            state
                .members
                .get(&channel.community_id)
                .map_or(&[][..], Vec::as_slice),
        );
    }
    let mut all: Vec<Member> = Vec::new();
    for community in &state.communities {
        for m in state.members.get(&community.id).into_iter().flatten() {
            if !all.iter().any(|a| a.user_id == m.user_id) {
                all.push(m.clone());
            }
        }
    }
    Cow::Owned(all)
}

/// webhook.ts and shell-model.ts `authorName`: the member's display name, else the username, else the viewer's own,
/// else the first eight digits of the id.
fn author(state: &AppState, members: &[Member], item: &TimelineItem) -> String {
    let Some(user) = item.sender_user else {
        let device = item.sender_device.map(|d| d.hex()).unwrap_or_default();
        return tf(
            "shell.message.unknownAuthor",
            &[("id", &device[..device.len().min(8)])],
        );
    };
    user_name(state, members, user)
}

/// [`author`] of a user with no message of their own to name: the requester a "started music" line names (the web's
/// `authorName` with `senderDevice: null`).
pub fn user_name(state: &AppState, members: &[Member], user: UserId) -> String {
    if let Some(m) = members.iter().find(|m| m.user_id == user) {
        if !m.display.is_empty() {
            return m.display.clone();
        }
        if !m.username.is_empty() {
            return m.username.clone();
        }
    }
    if state.account.user_id == Some(user) && !state.account.username.is_empty() {
        return state.account.username.clone();
    }
    user.hex()[..8].to_owned()
}

/// shell-model.ts `mentionPill`: what a mention token shows as.
fn mention_pill(state: &AppState, members: &[Member], target: &str, mentions_me: bool) -> Pill {
    if target == "everyone" || target == "here" {
        return Pill {
            label: format!("@{target}"),
            me: mentions_me,
            broadcast: true,
            user: None,
        };
    }
    if let Some(m) = members.iter().find(|m| m.user_id.hex() == target) {
        let name = if m.display.is_empty() {
            &m.username
        } else {
            &m.display
        };
        return Pill {
            label: format!("@{name}"),
            me: state.account.user_id.is_some_and(|u| u.hex() == target),
            broadcast: false,
            user: Some(target.to_owned()),
        };
    }
    if state.account.user_id.is_some_and(|u| u.hex() == target) {
        return Pill {
            label: format!("@{}", state.account.username),
            me: true,
            broadcast: false,
            user: Some(target.to_owned()),
        };
    }
    Pill {
        label: tf(
            "shell.message.mentionUnknown",
            &[("id", &target[..target.len().min(8)])],
        ),
        me: false,
        broadcast: false,
        user: None,
    }
}

/// The automation ids of the private results after the log's last message (Task 20's `private_results`).
const PRIVATE_RESULT_IDS: [&str; 8] = [
    "private-result-0",
    "private-result-1",
    "private-result-2",
    "private-result-3",
    "private-result-4",
    "private-result-5",
    "private-result-6",
    "private-result-7",
];

/// A webhook post (Task 21's `decode_webhook_label`, MessageItem.tsx): the unedited message of `community`'s pinned Bot
/// user ending in its label line. Only that user's posts are read for it: the Bot defangs `[dilla-` in every text it
/// posts, a sender's included, so anyone else's stays text.
fn webhook_of(
    state: &AppState,
    community: CommunityId,
    item: &TimelineItem,
) -> Option<dilla_client::state::bot_admin::WebhookLabel> {
    let pinned = state.bot.catalogs.get(&community)?.bot_user?;
    if !item.bot || item.edited || item.sender_user != Some(pinned) {
        return None;
    }
    // MessageItem.tsx: a poll post or a music announcement is read as that, never as a webhook (`marker === null && music
    // === null`).
    if polls::final_poll_marker(&item.body).is_some() || music_marker(&item.body).is_some() {
        return None;
    }
    dilla_client::state::bot_admin::decode_webhook_label(&item.body)
}

/// A Bot's "started music" message (MessageItem.tsx:114-120): the unedited, non-poll message of a sender whose member row
/// is of account kind 1 (`sender_kind`; `None` when they are no member) and whose credential claims a Bot, ending in the
/// player marker. The credential alone is a claim any modified client can make, so a forged marker stays text. The row's
/// text ([`body_parts`]) and its card ([`crate::screens::bot_admin::player::music_card`]) both read this gate, so they
/// can never disagree.
pub fn announcement_marker(item: &TimelineItem, sender_kind: Option<u8>) -> Option<MusicMarker> {
    if !item.bot
        || sender_kind != Some(1)
        || item.edited
        || polls::final_poll_marker(&item.body).is_some()
    {
        return None;
    }
    music_marker(&item.body)
}

/// [`announcement_marker`] of `item` in `community`, its sender's kind read from the community's member rows.
pub fn announcement_in(
    state: &AppState,
    community: CommunityId,
    item: &TimelineItem,
) -> Option<MusicMarker> {
    let members = state.members.get(&community).map_or(&[][..], Vec::as_slice);
    announcement_marker(item, sender_kind(members, item))
}

/// What a poll post's body is (MessageItem.tsx `displayBody`): a Bot's post ending in the poll marker never reads as plain
/// markdown until it is known whether its card verifies.
#[derive(Clone, PartialEq, Eq)]
pub enum PollBody {
    /// Not a poll post: the body as it is.
    NoMarker,
    /// A verified poll: `poll.asked` naming the poll's creator (not the Bot).
    Asked(String),
    /// A post whose card is being verified: no text, the card's loading line stands for it.
    Pending,
    /// The post's own text with the marker line gone and its `<t:…>` tags as local times, before markdown.
    Fallback(String),
}

/// The variant and the lengths only: `Asked` and `Fallback` hold decrypted text, which never goes in Debug.
impl std::fmt::Debug for PollBody {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::NoMarker => f.write_str("NoMarker"),
            Self::Asked(n) => write!(f, "Asked(<{} bytes>)", n.len()),
            Self::Pending => f.write_str("Pending"),
            Self::Fallback(t) => write!(f, "Fallback(<{} bytes>)", t.len()),
        }
    }
}

/// [`PollBody`] of `item`. `live` is whether a client is running (the web's `p.client !== undefined`): only then can a
/// card verify, so a fixture or a closed session shows the text at once. The sender must be a kind-1 member (a credential
/// alone is a claim any client can make), as [`announcement_marker`] gates the music line.
pub fn poll_body(
    item: &TimelineItem,
    state: &AppState,
    community: Option<CommunityId>,
    live: bool,
    zone: &jiff::tz::TimeZone,
) -> PollBody {
    poll_body_in(item, state, community, live, zone, None)
}

/// [`poll_body`] of a post shown in `thread` (the thread's root id; `None` in the channel's log). A poll held for another
/// place is no card here (PollCard.tsx rejects a record whose `threadId` differs), so the post reads as its text.
pub fn poll_body_in(
    item: &TimelineItem,
    state: &AppState,
    community: Option<CommunityId>,
    live: bool,
    zone: &jiff::tz::TimeZone,
    thread: Option<dilla_client::state::MessageId>,
) -> PollBody {
    let members = community
        .and_then(|c| state.members.get(&c))
        .map_or(&[][..], Vec::as_slice);
    if !item.bot
        || sender_kind(members, item) != Some(1)
        || polls::final_poll_marker(&item.body).is_none()
    {
        return PollBody::NoMarker;
    }
    let readable = !item.edited && item.state == TimelineItemState::Ok;
    // The thread's root row has a card state of its own (the web's is per card, with the thread in its scope): its record is
    // never the log's, and its checks are `root_poll_checks`, so it shows nothing while its read runs and the post's text once
    // that gave up.
    let root = thread.is_some() && thread == item.msg_id;
    if !root
        && readable
        && let Some(poll) = item.msg_id.and_then(|m| state.bot.polls.get(&m))
    {
        if poll.thread != thread {
            return PollBody::Fallback(poll_fallback(&item.body, zone));
        }
        return PollBody::Asked(user_name(state, members, poll.creator));
    }
    // `Retry` is a given-up post being read again: it still shows as given up until the read succeeds.
    let gave_up = item
        .msg_id
        .and_then(|m| state.bot.checks(root).get(&m))
        .is_some_and(|c| matches!(c, PollCheck::GaveUp(_) | PollCheck::Retry(_)));
    if readable
        && live
        && community.is_some()
        && state.account.user_id.is_some()
        && item.sender_user.is_some()
        && !gave_up
    {
        return PollBody::Pending;
    }
    PollBody::Fallback(poll_fallback(&item.body, zone))
}

/// MessageItem.tsx `fallback` of a poll post: the final marker line removed (with the newline before it), each
/// `<t:seconds:style>` tag as the local time, or left as it is when that gives none.
fn poll_fallback(body: &str, zone: &jiff::tz::TimeZone) -> String {
    const MARKER: usize = "[dilla-poll:v1:".len() + 32 + 1;
    let mut text = body;
    if let Some(at) = text.len().checked_sub(MARKER)
        && text.is_char_boundary(at)
        && polls::final_poll_marker(&text[at..]).is_some()
        && text[at..].starts_with('[')
    {
        text = text[..at].strip_suffix('\n').unwrap_or(&text[..at]);
    }
    let mut out = String::with_capacity(text.len());
    let mut rest = text;
    while let Some(open) = rest.find("<t:") {
        out.push_str(&rest[..open]);
        let tag = &rest[open..];
        let digits = tag[3..].bytes().take_while(u8::is_ascii_digit).count();
        let close = 3 + digits;
        let style = tag.as_bytes().get(close + 1);
        let shaped = digits > 0
            && tag.as_bytes().get(close) == Some(&b':')
            && style.is_some_and(|s| b"tTdDfFR".contains(s))
            && tag.as_bytes().get(close + 2) == Some(&b'>');
        if !shaped {
            out.push_str("<t:");
            rest = &tag[3..];
            continue;
        }
        let raw = &tag[..close + 3];
        let when = tag[3..close]
            .parse::<i64>()
            .ok()
            .and_then(|s| format::local_utility_time(s, zone));
        out.push_str(when.as_deref().unwrap_or(raw));
        rest = &tag[close + 3..];
    }
    out.push_str(rest);
    out
}

/// The zone times in the log are drawn in: UTC under a fixture's frozen clock, else the system's.
pub(crate) fn zone_for(clock: Clock) -> jiff::tz::TimeZone {
    match clock {
        Clock::Fixed { utc: true, .. } => jiff::tz::TimeZone::UTC,
        _ => jiff::tz::TimeZone::system(),
    }
}

/// Whether `item` is the community Bot's post: its credential claims it AND the member row of its sender is a Bot (kind 1),
/// as the web's `item.bot && isCommunityBot(...)`; the claim is signed by the sender alone, so it is never trusted alone.
pub(crate) fn is_community_bot(members: &[Member], item: &TimelineItem) -> bool {
    item.bot
        && members
            .iter()
            .any(|m| Some(m.user_id) == item.sender_user && m.kind == 1)
}

fn sender_kind(members: &[Member], item: &TimelineItem) -> Option<u8> {
    members
        .iter()
        .find(|m| Some(m.user_id) == item.sender_user)
        .map(|m| m.kind)
}

/// The head's "via webhook <name>" after the tag, for a webhook post ([`webhook_of`]).
fn via_mark(state: &AppState, community: CommunityId, item: &TimelineItem) -> Option<String> {
    webhook_of(state, community, item)
        .map(|w| tf("message.viaWebhook", &[("name", w.name.as_str())]))
}

/// The body a row shows: the message's markdown for a readable one (a webhook post's text before its label line), the
/// state's note for the rest.
fn body_parts(
    state: &AppState,
    members: &[Member],
    community: CommunityId,
    item: &TimelineItem,
    live: bool,
    zone: &jiff::tz::TimeZone,
) -> Vec<Body> {
    match item.state {
        TimelineItemState::CannotRead => vec![Body::Text(t("shell.message.cannotRead").to_owned())],
        TimelineItemState::Deleted => vec![Body::Text(t("shell.message.deleted").to_owned())],
        _ => {
            // A poll post is read before anything else: its marker excludes a music announcement and a webhook.
            let poll = poll_body(item, state, Some(community), live, zone);
            match &poll {
                PollBody::NoMarker => {}
                PollBody::Asked(name) => {
                    return vec![Body::Text(tf("poll.asked", &[("name", name.as_str())]))];
                }
                PollBody::Pending => return Vec::new(),
                PollBody::Fallback(text) => {
                    return markdown::parse_markdown(text, &|target| {
                        mention_pill(state, members, target, item.mention)
                    });
                }
            }
            // The announcement's text is the Bot's line, whoever the marker names (MessageItem.tsx `displayBody`).
            if let Some(marker) = announcement_marker(item, sender_kind(members, item)) {
                let room = state
                    .channels
                    .iter()
                    .find(|c| c.id == marker.voice_channel && c.community_id == community)
                    .map_or_else(|| t("music.someRoom").to_owned(), |c| c.name.clone());
                let name = user_name(state, members, marker.requester);
                return vec![Body::Text(tf(
                    "music.started",
                    &[("name", name.as_str()), ("room", room.as_str())],
                ))];
            }
            let webhook = webhook_of(state, community, item);
            let text = webhook
                .as_ref()
                .map_or(item.body.as_str(), |w| w.text.as_str());
            markdown::parse_markdown(text, &|target| {
                mention_pill(state, members, target, item.mention)
            })
        }
    }
}

/// The body as a screen reader reads it: the text with each mention as its pill's label (`@name`), code as written.
pub fn spoken_body(parts: &[Body]) -> String {
    fn walk(parts: &[Body], out: &mut String) {
        for p in parts {
            match p {
                Body::Text(s) | Body::Code(s) | Body::CodeBlock { text: s, .. } => out.push_str(s),
                Body::Link { href } => out.push_str(href),
                Body::Mention(pill) => out.push_str(&pill.label),
                Body::Strong(c) | Body::Em(c) | Body::Strike(c) | Body::Quote(c) => walk(c, out),
            }
        }
    }
    let mut out = String::new();
    walk(parts, &mut out);
    out
}

/// A message row's accessible name: MessageRow.tsx labels the `<article>` by its head (the author and the time), then
/// the body follows; here both are the name, as the row's text has no nodes of its own.
fn spoken_row(name: &str, time: &str, body: &str) -> String {
    format!("{name}, {time}: {body}")
}

/// [`spoken_row`] of `item`: its author as the log names them, its time in the log's zone, its body as read.
pub fn spoken_item(
    state: &AppState,
    members: &[Member],
    community: CommunityId,
    item: &TimelineItem,
    clock: Clock,
    live: bool,
) -> String {
    let name = author(state, members, item);
    let zone = |secs: i64| zone_of(clock, secs);
    let time = format::time_in(item.ts, &zone);
    let zone = zone_for(clock);
    // A post whose card is being verified has no text; the card's `poll.loading` status is what the web's log announces
    // with it (PollCard.tsx), so that is its body here.
    if poll_body(item, state, Some(community), live, &zone) == PollBody::Pending {
        return spoken_row(&name, &time, t("poll.loading"));
    }
    let parts = body_parts(state, members, community, item, live, &zone);
    spoken_row(&name, &time, &spoken_body(&parts))
}

/// What the log reads out as it arrives (MessageLog.tsx `aria-live="polite" aria-relevant="additions"`): the newest
/// readable message of the open log when it is someone else's and newer than `seen`, the key last spoken (or the tail
/// when the log opened). Returns the key to remember and, when there is one, the text to announce.
pub fn arrival(
    state: &AppState,
    channel: &Channel,
    clock: Clock,
    seen: Option<&(ChannelId, String)>,
) -> Option<((ChannelId, String), Option<String>)> {
    let items = &state.timelines.get(&channel.id)?.items;
    let last = items.last()?;
    // A poll post that has resolved (verified, or given up) is a new addition to the web's polite log (its text and card
    // appear), so it is announced again under a key of its own.
    let zone = zone_for(clock);
    let resolved = !matches!(
        poll_body(last, state, Some(channel.community_id), true, &zone),
        PollBody::NoMarker | PollBody::Pending
    );
    let key = (
        channel.id,
        if resolved {
            format!("{}#resolved", last.key)
        } else {
            last.key.clone()
        },
    );
    match seen {
        Some(s) if *s == key => None,
        // Another log (or the first look at this one): its tail is what was already there.
        Some((c, _)) if *c != channel.id => Some((key, None)),
        None => Some((key, None)),
        Some(_) if last.own || !readable(last) => Some((key, None)),
        Some(_) => {
            let members = log_members(state, channel);
            // Only the running shell announces arrivals, so a poll's card can verify.
            let text = spoken_item(state, &members, channel.community_id, last, clock, true);
            Some((key, Some(text)))
        }
    }
}

/// A follow-up whose head shows a mark (a webhook's "via", edited, pinned, the send state) keeps a head line of its marks
/// (MessageRow.css: a grouped head leaves the flow only when everything in it is for a screen reader).
fn marks_head(state: &AppState, community: CommunityId, item: &TimelineItem) -> bool {
    !head_marks(item).is_empty() || via_mark(state, community, item).is_some()
}

/// Where a row's body starts, below the row's top: a follow-up's at its padding, under its marks' line when it keeps one.
fn body_offset(grouped: bool, marks: bool, d: Density) -> f32 {
    match (grouped, marks) {
        (true, false) => FOLLOW_PAD,
        // The grouped head is a flex row whose only in-flow children are meta spans (the author is screen-reader only,
        // absolutely placed): a flex container has no strut, so it is one meta line high (MessageRow.css).
        (true, true) => FOLLOW_PAD + meta_line(d) + MAIN_GAP,
        (false, _) => ROW_TOP + head_line(d) + MAIN_GAP,
    }
}

/// The attachments slot's height (the grid's 1px gap and its 4px margin) between the body and the other areas' parts:
/// none in the log's last row when it is empty (MessageLog.css `:last-child .d-message-row__attachments:empty`).
pub(crate) fn attachments_slot(last: bool) -> f32 {
    if last {
        0.0
    } else {
        MAIN_GAP + ATTACHMENTS_MARGIN
    }
}

/// How far the head and the body move down for a part `height` px high above the head (the reply line, `order: -1` in
/// the row's grid): its height and the grid's 1px gap; nothing without one.
fn before_shift(height: f32) -> f32 {
    if height > 0.0 { height + MAIN_GAP } else { 0.0 }
}

/// The height Chromium gives a row with a body `body` px high, `extras` px of other areas' parts under the body and
/// `before` px (a [`before_shift`]) above its head, at the density `d` (its head line's).
fn row_height(
    grouped: bool,
    marks: bool,
    last: bool,
    before: f32,
    body: f32,
    extras: f32,
    d: Density,
) -> f32 {
    let slot = attachments_slot(last);
    let body = before + body + extras;
    if grouped && marks {
        FOLLOW_PAD + meta_line(d) + MAIN_GAP + body + slot + FOLLOW_PAD
    } else if grouped {
        FOLLOW_PAD + body + slot + FOLLOW_PAD
    } else {
        ROW_TOP + head_line(d) + MAIN_GAP + body + slot + ROW_BOTTOM
    }
}

/// The welcome's lines, wrapped to the log's width less its padding (Shell.tsx `welcome`): a channel's "Welcome to
/// #name" over "This is the start of #name.", a DM's name over "This is the start of your conversation.".
/// The welcome's title and body as text (MessageLog.tsx's `<h2>` and `<p>`), for the reader's nodes.
fn welcome_text(channel: &str, dm: bool) -> (String, String) {
    if dm {
        (channel.to_owned(), t("shell.log.welcomeDmBody").to_owned())
    } else {
        (
            tf("shell.log.welcome", &[("channel", channel)]),
            tf("shell.log.welcomeBody", &[("channel", channel)]),
        )
    }
}

fn welcome_lines(channel: &str, dm: bool, width: f32, d: Density) -> (Vec<Line>, Vec<Line>) {
    let inner = width - 2.0 * WELCOME_X;
    let (title, body) = if dm {
        (channel.to_owned(), t("shell.log.welcomeDmBody").to_owned())
    } else {
        (
            tf("shell.log.welcome", &[("channel", channel)]),
            tf("shell.log.welcomeBody", &[("channel", channel)]),
        )
    };
    let title = paragraph(
        &title,
        text::WELCOME,
        welcome_title_line(d),
        color::FG,
        inner,
    )
    .into_iter()
    .map(|l| l.weight(font::WEIGHT_DISPLAY))
    .collect();
    let body = paragraph(&body, text::UI, welcome_body_line(d), color::FG_2, inner);
    (title, body)
}

/// Conversation.tsx:394-395: the note under the welcome of a community channel whose loaded log is empty. `shell.log.emptyMade`
/// when this client made the channel in this session (`madeHere`), else `shell.log.empty`; a DM, a log with rows and one
/// still loading have none.
pub fn empty_note(
    state: &AppState,
    channel: ChannelId,
    dm: bool,
    items: usize,
    loaded: bool,
) -> Option<&'static str> {
    if dm || items > 0 || !loaded {
        return None;
    }
    Some(if state.made_channels.contains(&channel) {
        "shell.log.emptyMade"
    } else {
        "shell.log.empty"
    })
}

/// `.d-message-log__welcome + .d-message-log__empty { padding: 0 1.25rem 0.5rem }`: the note's bottom padding. Its sides are
/// the welcome's ([`WELCOME_X`]).
const EMPTY_NOTE_BOTTOM: f32 = 8.0;

/// The note's lines (`font-size: var(--text-sm); color: var(--fg-3)`, no line height of its own, so the root's) and its
/// height with the bottom padding.
fn empty_note_lines(key: &str, width: f32) -> (Vec<Line>, f32) {
    let lh = dilla_desktop_ui::text::root_line_height(text::SM);
    let lines = paragraph(t(key), text::SM, lh, color::FG_3, width - 2.0 * WELCOME_X);
    let height = lines.len() as f32 * lh + EMPTY_NOTE_BOTTOM;
    (lines, height)
}

fn welcome_height(title: usize, body: usize, d: Density) -> f32 {
    WELCOME_TOP
        + WELCOME_TILE
        + WELCOME_TITLE_GAP
        + title as f32 * welcome_title_line(d)
        + body as f32 * welcome_body_line(d)
        + WELCOME_BOTTOM
}

/// The log's content, every element at its CSS position (content coordinates, logical px). Lines keep their fractions
/// ([`Line::exact`]); boxes take the whole pixel Chromium paints them on (the position rounded).
struct Placer {
    out: Vec<AnyElement>,
    /// The window's density: the welcome's and the divider's line heights.
    density: Density,
}

impl Placer {
    fn line(&mut self, line: Line, x: f32, top: f32) {
        self.out.push(line.exact(x, top));
    }

    fn block(&mut self, x: f32, top: f32, element: impl IntoElement) {
        self.out.push(
            div()
                .absolute()
                .left(px(x.round()))
                .top(px(top.round()))
                .child(element)
                .into_any_element(),
        );
    }

    /// [`Self::block`] at the fractional position (an inline glyph inside a text run).
    fn block_exact(&mut self, x: f32, top: f32, element: impl IntoElement) {
        self.out.push(
            div()
                .absolute()
                .left(px(x))
                .top(px(top))
                .child(element)
                .into_any_element(),
        );
    }

    /// The welcome at `top`; answers its height.
    fn welcome(
        &mut self,
        top: f32,
        glyph: IconName,
        (title, body): (Vec<Line>, Vec<Line>),
        (title_text, body_text): (String, String),
        width: f32,
    ) -> f32 {
        self.block(
            WELCOME_X,
            top + WELCOME_TOP,
            div()
                .size(px(WELCOME_TILE))
                .rounded(radius::LG)
                .bg(color::SURFACE_TILE)
                .border_1()
                .border_color(color::HAIRLINE_2)
                .flex()
                .items_center()
                .justify_center()
                .child(Icon::new(glyph).size(len(20.)).color(color::ACCENT)),
        );
        let d = self.density;
        let height = welcome_height(title.len(), body.len(), d);
        let mut y = top + WELCOME_TOP + WELCOME_TILE + WELCOME_TITLE_GAP;
        // The heading and the paragraph a reader finds, over the lines they are painted as.
        let (title_h, body_h) = (
            title.len() as f32 * welcome_title_line(d),
            body.len() as f32 * welcome_body_line(d),
        );
        let inner = (width - 2.0 * WELCOME_X).max(1.0);
        self.block(
            WELCOME_X,
            y,
            dilla_desktop_ui::a11y::heading(2, title_text, div().w(px(inner)).h(px(title_h))),
        );
        self.block(
            WELCOME_X,
            y + title_h,
            dilla_desktop_ui::a11y::paragraph(body_text, div().w(px(inner)).h(px(body_h))),
        );
        for l in title {
            self.line(l, WELCOME_X, y);
            y += welcome_title_line(d);
        }
        for l in body {
            self.line(l, WELCOME_X, y);
            y += welcome_body_line(d);
        }
        height
    }

    /// A day divider at `top`, `width` wide: the tracked label, then a rule to the right padding.
    fn divider(&mut self, top: f32, width: f32, label: String) {
        let label = label.to_uppercase();
        let size = text::MICRO;
        let label_width =
            label.chars().count() as f32 * f32::from(size) * (MONO_ADVANCE + LABEL_TRACKING_WIDE);
        let lh = line_height(size, self.density.line_height());
        self.line(
            line(label, size, lh, color::FG_3)
                .weight(font::WEIGHT_LABEL)
                .tracking(LABEL_TRACKING_WIDE),
            WELCOME_X,
            top + DIVIDER_PAD,
        );
        let rule_x = WELCOME_X + label_width + DIVIDER_GAP;
        self.block(
            rule_x,
            top + divider_height(self.density) / 2.0 - 0.5,
            div()
                .w(px((width - WELCOME_X - rule_x).max(0.0)))
                .h(len(1.))
                .bg(color::HAIRLINE),
        );
    }
}

/// The head's text after the name (meta size, `--fg-3`; `--fg-2` on a mention row's tint).
fn head_marks(item: &TimelineItem) -> Vec<String> {
    let mut marks = Vec::new();
    if item.edited {
        marks.push(t("shell.message.edited").to_owned());
    }
    if item.pinned {
        marks.push(t("shell.message.pinned").to_owned());
    }
    match item.state {
        TimelineItemState::Pending => marks.push(t("shell.message.pending").to_owned()),
        TimelineItemState::Failed => marks.push(t("shell.message.failed").to_owned()),
        _ => {}
    }
    marks
}

/// What a row paints, in the row's own coordinates (its box's top is 0, and sits on a whole pixel).
struct RowCx<'a> {
    state: &'a AppState,
    members: &'a [Member],
    /// The timeline's community: its pinned Bot user is the only author of a webhook post.
    community: CommunityId,
    clock: Clock,
    /// Whether a client is running: only then can a poll's card verify (`poll_body`).
    live: bool,
    width: f32,
    /// Every row's key in the log's order: the arrow keys step through it (Conversation.tsx `rowKey`).
    order: std::rc::Rc<Vec<String>>,
    /// The active row's key: the log's one tab stop.
    active: Option<String>,
    /// The window's density.
    density: Density,
    /// The clock the rows' entrances run on (Task 36c).
    now: std::time::Instant,
}

/// The shaped width of meta text (a webhook's name is the admin's: wide glyphs and emoji are measured, not counted;
/// final review M5).
fn meta_width(window: &Window, text_: &str) -> f32 {
    let run = gpui::TextRun {
        len: text_.len(),
        font: gpui::font(font::MONO),
        color: color::FG_3,
        background_color: None,
        underline: None,
        strikethrough: None,
    };
    let shaped = window.text_system().shape_line(
        gpui::SharedString::from(text_.to_owned()),
        text::META,
        &[run],
        None,
    );
    f32::from(shaped.width)
}

/// The head's marks from `x`, 8px apart: a webhook post's "via webhook <name>" (MessageRow's `viaLabel`), then edited,
/// pinned and the send state, in the meta style.
fn draw_marks(
    placer: &mut Placer,
    cx_row: &RowCx<'_>,
    item: &TimelineItem,
    mut x: f32,
    meta_top: f32,
    meta_color: gpui::Hsla,
    window: &Window,
) {
    let meta = meta_line(cx_row.density);
    if let Some(via) = via_mark(cx_row.state, cx_row.community, item) {
        let w = meta_width(window, &via);
        placer.line(line(via, text::META, meta, meta_color), x, meta_top);
        x += w + HEAD_GAP;
    }
    let marks = head_marks(item);
    let last = marks.len().saturating_sub(1);
    for (n, mark) in marks.into_iter().enumerate() {
        // The send state is the last mark; a failed one is the danger tone after the x glyph (MessageRow.css `::before`:
        // 0.875em, 0.25em right of it, its bottom 0.125em under the baseline), so the state is never colour alone.
        let failed = n == last && item.state == TimelineItemState::Failed;
        let tone = if failed { color::DANGER } else { meta_color };
        if failed {
            let em = f32::from(text::META);
            let baseline = meta_top + chromium_baseline(Face::Mono, em, meta);
            let glyph = 0.875 * em;
            placer.block_exact(
                x,
                baseline + 0.125 * em - glyph,
                Icon::new(IconName::X).size(px(glyph)).color(tone),
            );
            x += glyph + 0.25 * em;
        }
        let w = mark.chars().count() as f32 * f32::from(text::META) * MONO_ADVANCE;
        placer.line(line(mark, text::META, meta, tone), x, meta_top);
        x += w + HEAD_GAP;
    }
}

/// Where Chromium paints a box edge laid out at `v` css px: the whole px, half up (the vendored GPUI snaps box edges the
/// same way at a whole scale factor; core/vendor/README.md, "gpui-pre 0.3.12"). The log places its rows and bodies on
/// these edges; text lines keep the CSS fraction.
fn snap_px(v: f32) -> f32 {
    (v + 0.5).floor()
}

/// One row's elements, in coordinates relative to the row box at `base` ([`snap_px`] of `top`); `top` is the row's CSS
/// top.
#[allow(clippy::too_many_arguments)]
fn draw_row(
    cx_row: &RowCx<'_>,
    shown: Shown<'_>,
    top: f32,
    base: f32,
    ui: &gpui::Entity<MessageUi>,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let item = shown.item;
    let d = cx_row.density;
    let local = top - base;
    // The body's box sits on a whole pixel ([`snap_px`]); its lines keep the CSS position's fraction.
    let shift = before_shift(shown.before_height);
    let frac = {
        let at = local + shift + body_offset(shown.grouped, shown.marks, d);
        at - snap_px(at)
    };
    // CodeBlock.css:2: a code card rises in over `--duration-slow` from the row's mount (the row's stagger does not delay it).
    let code_age = (!cx.reduce_motion())
        .then(|| ui.read(cx).log.of(&item.key, cx_row.now))
        .flatten()
        .map(|(elapsed, _)| elapsed);
    let laid = shown
        .body
        .laid(cx_row.width - CONTENT_X - ROW_RIGHT, frac, code_age);
    let height = row_height(
        shown.grouped,
        shown.marks,
        shown.last,
        shift,
        laid.height,
        shown.extras_height,
        d,
    );
    let mut placer = Placer {
        out: Vec::new(),
        density: d,
    };
    // The reply line (11) sits where the head would, and the head, the avatar and the gutter move down (`data-reply`).
    let avatar_shift = if shown.before.is_some() {
        REPLY_AVATAR_SHIFT
    } else {
        0.0
    };
    if let Some(before) = shown.before {
        let top = local + if shown.grouped { FOLLOW_PAD } else { ROW_TOP };
        placer.block(CONTENT_X, top, before);
    }
    let meta_color = if item.mention {
        color::FG_2
    } else {
        color::FG_3
    };
    if !shown.grouped {
        let head = local + ROW_TOP + shift;
        let row_top = local + ROW_TOP + avatar_shift;
        let name = author(cx_row.state, cx_row.members, item);
        let zone = |secs: i64| zone_of(cx_row.clock, secs);
        let time = format::time_in(item.ts, &zone);
        // The time is right-aligned in the gutter, by its advance.
        let time_width = time.chars().count() as f32 * f32::from(text::META) * MONO_ADVANCE;
        placer.line(
            line(time, text::META, gutter_line(), color::FG_QUIET),
            GUTTER - time_width,
            row_top + GUTTER_TOP,
        );
        // MessageRow.tsx `avatarSrc`: a webhook post shows its avatar, read only from the instance (Task 28 item 15),
        // once it is in; the placeholder meanwhile and on a miss.
        let bot = is_community_bot(cx_row.members, item);
        let webhook_avatar = webhook_of(cx_row.state, cx_row.community, item)
            .and_then(|w| w.avatar_id)
            .zip(ctx.client.as_ref())
            .and_then(|(avatar, client)| {
                crate::screens::webhook_avatar::image(client, cx_row.community, &avatar, cx)
            });
        match webhook_avatar {
            // `.d-message-row__avatar-img`: the 30px msg avatar's box, `object-fit: cover`, the Bot's 6px corners.
            Some(image) => placer.block(
                GUTTER + COLUMN_GAP,
                row_top,
                gpui::StyledImage::object_fit(
                    gpui::img(image)
                        .size(px(AVATAR))
                        .rounded(dilla_desktop_ui::tokens::radius::BOT),
                    gpui::ObjectFit::Cover,
                ),
            ),
            // A Bot member's avatar is the brand square (Avatar.tsx `bot`), as the members pane draws it.
            None => placer.block(
                GUTTER + COLUMN_GAP,
                row_top,
                Avatar::new(
                    gpui::SharedString::from(format!("avatar-{}", item.key)),
                    name.clone(),
                )
                .size(AvatarSize::Msg)
                .shape(if bot {
                    AvatarShape::Bot
                } else {
                    AvatarShape::Circle
                }),
            ),
        }
        let name_width = name.chars().count() as f32 * f32::from(text::SM) * MONO_ADVANCE;
        let name_color = if bot {
            color::ACCENT_2
        } else {
            name_colour_of(
                cx_row.state,
                cx_row.community,
                cx_row.members,
                item.sender_user,
            )
            .map_or(color::FG, |rgb| gpui::rgb(rgb).into())
        };
        placer.line(
            line(name, text::SM, head_line(d), name_color).weight(font::WEIGHT_DISPLAY),
            CONTENT_X,
            head,
        );
        // The head is a baseline-aligned row of the name, the tag and the marks, 8px apart.
        let baseline = head + chromium_baseline(Face::Mono, f32::from(text::SM), head_line(d));
        let mut x = CONTENT_X + name_width + HEAD_GAP;
        if bot {
            let l = look(TagKind::Bot);
            let tag_w = 8.0
                + "BOT".chars().count() as f32
                    * (f32::from(l.size) * MONO_ADVANCE + l.tracking * f32::from(l.size));
            let tag_top =
                baseline - chromium_baseline(Face::Mono, f32::from(l.size), l.line_height);
            placer.block(
                x,
                tag_top,
                Tag::new(
                    gpui::SharedString::from(format!("tag-{}", item.key)),
                    TagKind::Bot,
                ),
            );
            x += tag_w + HEAD_GAP;
        }
        let meta_top =
            baseline - chromium_baseline(Face::Mono, f32::from(text::META), meta_line(d));
        draw_marks(&mut placer, cx_row, item, x, meta_top, meta_color, window);
    } else if shown.marks {
        // A follow-up that shows a mark keeps a head line of only its marks (the name and the tag are for a screen
        // reader there): a webhook's "via", edited, pinned, the send state (final review I3, MessageRow.css).
        // Only meta spans, baseline-aligned at one size: the line's top is the head's top.
        let meta_top = local + FOLLOW_PAD + shift;
        draw_marks(
            &mut placer,
            cx_row,
            item,
            CONTENT_X,
            meta_top,
            meta_color,
            window,
        );
    }
    let body_top = local + shift + body_offset(shown.grouped, shown.marks, d);
    let content_w = cx_row.width - CONTENT_X - ROW_RIGHT;
    let body_end = body_top + laid.height;
    placer.out.push(
        div()
            .absolute()
            .left(px(CONTENT_X))
            .top(px(snap_px(body_top)))
            .w(px(content_w))
            .h(px(snap_px(body_top + laid.height) - snap_px(body_top)))
            .child(div().relative().size_full().children(laid.elements))
            .into_any_element(),
    );
    // The other areas' parts sit under the attachments slot (the web's grid: body, attachments, reactions, thread), so
    // no guest needs to know whether its row is the log's last (Task 10's coupling).
    if let Some(extras) = shown.extras {
        placer.block(CONTENT_X, body_end + attachments_slot(shown.last), extras);
    }
    // The row's box: the hover fill and bar, the mention tint, the flash; the toolbar over its top edge.
    let group = shown.group.clone();
    let flash = ui.update(cx, |u, cx| u.flash_of(&item.key, cx));
    // MessageRow.tsx's `<article>`: one node per message, named by its author, time and text. Its id is the message's
    // key, so a row keeps its node while earlier messages load above it.
    let spoken = spoken_item(
        cx_row.state,
        cx_row.members,
        cx_row.community,
        item,
        cx_row.clock,
        cx_row.live,
    );
    // The keyboard (Conversation.tsx `rowKey`, MessageRow.css): the active row is the log's one tab stop; Up and Down
    // step to the row above and below, Home and End to the first and the newest, Escape goes to the composer. A row
    // that holds the focus, or a control in it, shows its toolbar and the inset focus outline.
    let key = item.key.clone();
    let handle = ui.update(cx, |u, cx| {
        u.row_focus
            .entry(key.clone())
            .or_insert_with(|| cx.focus_handle())
            .clone()
    });
    let stop = cx_row.active.as_deref() == Some(key.as_str());
    let focused_in = handle.contains_focused(window, cx);
    if focused_in && !stop {
        // The pointer (or a jump) put the focus on another row: it becomes the active one.
        let k = key.clone();
        ui.update(cx, |u, _| u.active = Some(k));
    }
    let keys = {
        let (order, ui, own) = (cx_row.order.clone(), ui.clone(), handle.clone());
        move |event: &gpui::KeyDownEvent, window: &mut Window, cx: &mut App| {
            let k = &event.keystroke;
            let m = &k.modifiers;
            if m.alt || m.control || m.platform || m.shift || m.function {
                return;
            }
            if k.key == "escape" {
                cx.stop_propagation();
                if own.is_focused(window) {
                    window.dispatch_action(Box::new(FocusComposer), cx);
                } else {
                    own.focus(window, cx);
                }
                return;
            }
            if !own.is_focused(window) {
                return;
            }
            let Some(from) = order.iter().position(|o| *o == key) else {
                return;
            };
            let Some(to) = arrow_target(k.key.as_str(), from, order.len()) else {
                return;
            };
            cx.stop_propagation();
            let next = order[to].clone();
            let h = ui.update(cx, |u, cx| {
                u.active = Some(next.clone());
                u.reveal = Some(next.clone());
                cx.notify();
                u.row_focus
                    .entry(next.clone())
                    .or_insert_with(|| cx.focus_handle())
                    .clone()
            });
            window.focus(&h, cx);
        }
    };
    let mut row = div()
        .id(gpui::SharedString::from(format!("message-{}", item.key)))
        .a11y(Role::Article, spoken)
        .track_focus(&handle.tab_stop(stop))
        .on_key_down(keys)
        .absolute()
        .left(len(0.))
        .top(px(base))
        .w(px(cx_row.width))
        .h(px(snap_px(top + height) - base))
        .group(group.clone());
    // MessageRow.css:8: `d-up` over `--duration-enter` on `--ease-out`, delayed by the first paint's stagger.
    if !cx.reduce_motion()
        && let Some((elapsed, delay)) = ui.read(cx).log.of(&item.key, cx_row.now)
        && elapsed < delay + token::ENTER
    {
        let (opacity, offset) =
            motion::rise_after_at(elapsed, delay, token::ENTER, token::EASE_OUT);
        row = row.top(px(base + offset)).opacity(opacity);
    }
    if focused_in {
        row = row.shadow(inset_ring());
    }
    // MessageRow.css `data-replying` (after `data-mention` in the cascade): the row the composer's reply bar aims at.
    let replying = is_replying(
        item.msg_id,
        crate::screens::threads::replying_of(ctx, cx)
            .filter(|r| Some(r.channel) == ctx.channel)
            .map(|r| r.msg_id),
    );
    // The row's fill: the jump's flash first, which beats the row's own fill and `:hover` as a CSS animation does.
    row = match row_fill(flash.is_some(), replying, item.mention, focused_in) {
        RowFill::Flash => dilla_desktop_ui::motion::flash(
            row,
            ElementId::NamedInteger(
                format!("row-flash-{}", item.key).into(),
                flash.unwrap_or_default(),
            ),
            cx,
        ),
        RowFill::Replying => row.bg(color::ACCENT_FAINT),
        RowFill::Mention => row.bg(color::WARN_SOFT),
        RowFill::Focused => row.bg(color::BG_3),
        RowFill::Hover => row.hover(|s| s.bg(color::BG_3)),
    };
    let bar = if replying {
        Some(color::ACCENT)
    } else if item.mention {
        Some(color::MENTION)
    } else {
        None
    };
    row = match bar {
        Some(c) => row.child(
            div()
                .absolute()
                .left(len(0.))
                .top(len(0.))
                .w(len(2.))
                .h_full()
                .bg(c),
        ),
        None => row.child(
            div()
                .absolute()
                .left(len(0.))
                .top(len(0.))
                .w(len(2.))
                .h_full()
                .bg(color::ACCENT)
                .invisible()
                .group_hover(group.clone(), |s| s.visible()),
        ),
    };
    row = row.child(crate::screens::messages::probe(
        crate::screens::messages::id_of("message-row", shown.index),
    ));
    // MessageRow.tsx's keyboard map: the toolbar comes right after the row itself (before the body, the attachments and
    // the reactions), so Tab from a focused row reaches its actions first.
    if let Some(bar) = toolbar::toolbar(item, shown.index, &group, focused_in, ctx, ui, window, cx)
    {
        row = row.child(bar);
    }
    row.children(placer.out).into_any_element()
}

/// What fills a message row's box (MessageRow.css, in the cascade's order).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum RowFill {
    /// The jump's flash: its own layer is the background, and the row has no hover style, because a CSS animation beats
    /// `:hover` and the row's own background.
    Flash,
    Replying,
    Mention,
    Focused,
    /// Nothing until the pointer is over the row.
    Hover,
}

fn row_fill(flash: bool, replying: bool, mention: bool, focused: bool) -> RowFill {
    if flash {
        RowFill::Flash
    } else if replying {
        RowFill::Replying
    } else if mention {
        RowFill::Mention
    } else if focused {
        RowFill::Focused
    } else {
        RowFill::Hover
    }
}

/// MessageItem.tsx `nameColorOf`: the author's name colour (README §5.5), the first of their roles that has one
/// (0xRRGGBB), `None` for none: the text colour then. A Bot's name is the accent's second tone instead.
fn name_colour_of(
    state: &AppState,
    community: CommunityId,
    members: &[Member],
    user: Option<UserId>,
) -> Option<u32> {
    let member = members.iter().find(|m| Some(m.user_id) == user)?;
    let roles = state.roles.roles.get(&community)?;
    member.role_ids.iter().find_map(|id| {
        roles
            .iter()
            .find(|r| &r.id == id)
            .map(|r| r.color)
            .filter(|c| *c != 0)
    })
}

/// Whether the row of message `msg` is the one the reply bar aims at (`target`).
fn is_replying(
    msg: Option<dilla_client::state::MessageId>,
    target: Option<dilla_client::state::MessageId>,
) -> bool {
    msg.is_some() && msg == target
}

/// Where Up, Down, Home and End move from row `from` of `count` (Conversation.tsx `rowKey`: the arrows stop at the
/// ends), `None` for any other key.
pub fn arrow_target(key: &str, from: usize, count: usize) -> Option<usize> {
    if count == 0 {
        return None;
    }
    match key {
        "up" => Some(from.saturating_sub(1)),
        "down" => Some((from + 1).min(count - 1)),
        "home" => Some(0),
        "end" => Some(count - 1),
        _ => None,
    }
}

/// MessageRow.css `:focus-visible { outline-offset: -2px }`: the focus ring drawn inside the row.
fn inset_ring() -> Vec<gpui::BoxShadow> {
    let mut ring = dilla_desktop_ui::metrics::focus_ring();
    for s in &mut ring {
        s.inset = true;
    }
    ring
}

/// The log of `channel`, `width` wide, kept at its end through `scroll` while `pinned`. Each row's
/// [`slots::message_extras`] sits under its body. `on_earlier` is the "load earlier" button's press.
#[allow(clippy::too_many_arguments)]
pub fn render(
    state: &AppState,
    channel: &Channel,
    clock: Clock,
    width: f32,
    scroll: &ScrollHandle,
    pinned: bool,
    on_wheel: OnWheel,
    on_earlier: OnEarlier,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> impl IntoElement + use<> {
    let ui = ui::ui(window, cx);
    let density = Density::of(window, cx);
    ui.update(cx, |u, _| u.ctx = Some(ctx.clone()));
    // Release lets go of this window's message state's client at once (review M5), not when the window next draws.
    if let Some(client) = &ctx.client {
        crate::release::hold(
            client,
            &ui,
            |u, _| {
                u.ctx = None;
                u.editing = None;
                u.pins = None;
                u.deleting = None;
                u.menu = None;
            },
            cx,
        );
    }
    let log_members = log_members(state, channel);
    let members: &[Member] = &log_members;
    let items: Vec<&TimelineItem> = state
        .timelines
        .get(&channel.id)
        .map(|t| t.items.iter().collect())
        .unwrap_or_default();
    let body_width = width - CONTENT_X - ROW_RIGHT;
    let editing_key = ui.read(cx).editing.as_ref().map(|e| e.key.clone());
    // Task 20's private results: the rows only this device shows, after the conversation's last message. They are rows
    // of the log too (MessageLog.tsx `d-message-log__rows`), so with one shown the last message is no longer the log's
    // last child and keeps its attachments slot.
    // Task 36c: the log mounts for a channel and its read state (Conversation.tsx `logKey`). A new mount makes what lives in
    // the rows (file cards, reaction chips, poll cards and voters) enter again with them, so it is known before they draw.
    let ready = state.timelines.contains_key(&channel.id);
    // Each window's log is its own (final review I1): its keys never move another window's entrances.
    let window_id = window.window_handle().window_id();
    motion::set_scope(cx, window_id, LOG_SCOPE);
    if ui.read(cx).log.remounts((channel.id, ready)) {
        motion::remount_log(cx, window_id, LOG_SCOPE);
    }
    let privates = crate::screens::commands::private_results(ctx, width, window, cx);
    let mut shown = Vec::with_capacity(items.len());
    for (n, item) in items.iter().enumerate() {
        let prev = n.checked_sub(1).map(|p| items[p]);
        let body_color = match item.state {
            TimelineItemState::Ok => color::FG_BODY,
            TimelineItemState::Pending => color::FG_2,
            // MessageRow.css colours only a pending body; a failed one keeps `--fg-body` (its head mark is the danger).
            TimelineItemState::Failed => color::FG_BODY,
            _ => color::FG_3,
        };
        // Only an empty attachments slot is hidden in the last row; one with cards keeps its gap and margin.
        let last = n + 1 == items.len() && item.attachments.is_empty() && privates.is_empty();
        let (extras, extras_height) = slots::message_extras(item, ctx, last, window, cx);
        let (before, before_height) = slots::message_before_head(item, ctx, window, cx)
            .map_or((None, 0.0), |b| (Some(b.element), b.height));
        let editing = editing_key.as_deref() == Some(item.key.as_str());
        let laid = if editing {
            let current = ui
                .read(cx)
                .editing
                .as_ref()
                .map(|ed| (ed.input.clone(), ed.used(cx)));
            match current {
                Some((input, used)) => {
                    let lines = editor::lines_of(&input.read(cx).value(), body_width);
                    let el = editor::editor(&input, used, &ui, body_width, window, cx);
                    Plan::Editor(Laid {
                        height: editor::height(lines),
                        elements: vec![el],
                    })
                }
                None => Plan::Editor(Laid {
                    height: 0.0,
                    elements: Vec::new(),
                }),
            }
        } else {
            let parts = body_parts(
                state,
                members,
                channel.community_id,
                item,
                ctx.client.is_some(),
                &zone_for(clock),
            );
            // The height comes from the memo (by the parts and the width): a body is laid out once, not on every draw.
            let mut height = body::height_cached(&parts, body_width);
            // An empty body still keeps its line (the web's row holds the empty text node's line box).
            if parts.is_empty() {
                height = body_line();
            }
            Plan::Text {
                parts,
                color: body_color,
                height,
            }
        };
        let grouped = grouped_with_above(prev, item, clock);
        shown.push(Shown {
            item,
            grouped,
            marks: grouped && marks_head(state, channel.community_id, item),
            divider: starts_day(prev, item, clock).then(|| day_label(item.ts, clock)),
            last,
            body: laid,
            extras: Some(extras),
            extras_height,
            before,
            before_height,
            group: format!("message-row-{}", item.key),

            index: n,
        });
    }
    // The rows are built: what is drawn after them is in no log.
    motion::end_scope(cx);
    // A DM's log: its channel record is the DM's (Shell.tsx: the at-sign and the DM's name).
    let dm = state.dms.get(channel.id).is_some();
    let (title, body_lines) = welcome_lines(&channel.name, dm, width, density);
    // Conversation.tsx:396-399: the "load earlier" button in the welcome's place while the history has more.
    let head = log_head(state.timelines.get(&channel.id));
    let head_height = match head {
        LogHead::Earlier => EARLIER_HEIGHT + f32::from(density.group_gap()),
        LogHead::Welcome | LogHead::Loading => {
            welcome_height(title.len(), body_lines.len(), density)
        }
    };
    // Conversation.tsx:394-395: the note under the welcome when the loaded log is empty.
    let note = (head == LogHead::Welcome)
        .then(|| {
            empty_note(
                state,
                channel.id,
                dm,
                shown.len() + privates.len(),
                state.timelines.contains_key(&channel.id),
            )
        })
        .flatten()
        .map(|key| (key, empty_note_lines(key, width)));
    let head_height = head_height + note.as_ref().map_or(0.0, |(_, (_, h))| *h);
    let shapes: Vec<RowShape> = shown.iter().map(Shown::shape).collect();
    let (_, rows_end) = row_tops(0.0, &shapes, density);
    let privates_height: f32 = privates.iter().map(|(_, h)| *h).sum();
    let content = LOG_TOP + head_height + LOG_BOTTOM + rows_end + privates_height;
    // Chromium's scroll range is whole pixels: the content's height rounds, and at the end that rounding sits below the
    // last row. A log shorter than its viewport keeps to the viewport's bottom (`margin-top: auto`).
    let viewport = f32::from(scroll.bounds().size.height);
    let height = content.round();
    // `margin-top: auto` puts a shorter log's content at the viewport's bottom in layout units: its rows end on the
    // bottom padding exactly, not on the rounded scroll height.
    let lift = (viewport - content).max(0.0);
    // The scrolled box: the viewport exactly for a shorter log (no sub-pixel scroll range from `content`'s rounding),
    // else the rounded content (T28 parity review m1).
    let inner = if lift > 0.0 { viewport } else { height };
    let mut placer = Placer {
        out: Vec::new(),
        density,
    };
    let mut y = lift + LOG_TOP;
    if head == LogHead::Earlier {
        // The button keeps to the log's top (`align-self: center`); the rows below it take the free room
        // (`.d-message-log__rows { margin-top: auto }`).
        placer.block(
            0.0,
            LOG_TOP,
            div().w(px(width)).flex().justify_center().child(
                Button::new("log-earlier", t("shell.log.earlier"))
                    .variant(ButtonVariant::Ghost)
                    .on_click(move |_, _, cx| on_earlier(cx)),
            ),
        );
        y += head_height;
    } else {
        let glyph = if dm { IconName::AtSign } else { IconName::Hash };
        y += placer.welcome(
            y,
            glyph,
            (title, body_lines),
            welcome_text(&channel.name, dm),
            width,
        );
        if let Some((key, (lines, height))) = note {
            let lh = dilla_desktop_ui::text::root_line_height(text::SM);
            let text_height = height - EMPTY_NOTE_BOTTOM;
            let inner_width = (width - 2.0 * WELCOME_X).max(1.0);
            placer.block(
                WELCOME_X,
                y,
                dilla_desktop_ui::a11y::paragraph(
                    t(key).to_owned(),
                    div().w(px(inner_width)).h(px(text_height)),
                ),
            );
            for l in lines {
                placer.line(l, WELCOME_X, y);
                y += lh;
            }
            y += EMPTY_NOTE_BOTTOM;
        }
    }
    let (tops, rows_bottom) = row_tops(y, &shapes, density);
    // What the log was last drawn with, to keep the reader's place when earlier rows come in front.
    let first_key = items.first().map(|i| i.key.clone());
    let (prev_first, prev_height) = {
        let u = ui.read(cx);
        (u.prev_first.clone(), u.prev_height)
    };
    // A jump (from the pins or a reply) brings its row into view and leaves the end.
    // A pressed reply line (11) asks for its original: the log scrolls to it and flashes it, as the pins' jump does.
    // Task 28 item 11 (Conversation.tsx): a message this log has not loaded is not jumped to; the conversation says so.
    if let Some(msg) = crate::screens::threads::take_jump(window, cx) {
        match items.iter().find(|i| i.msg_id == Some(msg)) {
            Some(found) => {
                let key = found.key.clone();
                ui.update(cx, |u, cx| u.jump_to(key, cx));
            }
            None => {
                let id = channel.id;
                ui.update(cx, |u, cx| u.not_loaded(id, cx));
            }
        }
    }
    let jump = ui.update(cx, |u, _| u.jump.take());
    let jumped = jump
        .as_deref()
        .and_then(|key| shown.iter().position(|s| s.item.key == key));
    // The keyboard's row (Package 11): the log's order, its active row (the newest unless one was chosen), and a row
    // the arrows moved to brought into view with the least scroll.
    let order: std::rc::Rc<Vec<String>> =
        std::rc::Rc::new(shown.iter().map(|s| s.item.key.clone()).collect());
    let active = ui.update(cx, |u, _| {
        u.row_focus.retain(|k, _| order.contains(k));
        u.active
            .clone()
            .filter(|k| order.contains(k))
            .or_else(|| order.last().cloned())
    });
    // Its rows enter once, from the frame the log first holds them, and the log asks for frames while any of it is still
    // moving. The private results are `.d-message-row`s of the log too (BotResult.tsx:60): they enter and count in the stagger.
    let now = cx.background_executor().now();
    let reduced = cx.reduce_motion();
    let enter_keys: Vec<String> = order
        .iter()
        .cloned()
        .chain((0..privates.len()).map(|n| format!("private-{n}")))
        .collect();
    let entering = ui.update(cx, |u, _| {
        u.enter_log((channel.id, ready), &enter_keys, now, reduced)
    });
    if entering.running {
        window.request_animation_frame();
    }
    let reveal = ui.update(cx, |u, _| u.reveal.take());
    let revealed = reveal.as_deref().and_then(|key| {
        let i = shown.iter().position(|s| s.item.key == key)?;
        let r = &shapes[i];
        let (top, bottom) = (tops[i], tops[i] + r.height(density));
        let now = -f32::from(scroll.offset().y);
        let max = (inner - viewport).max(0.0);
        let to = if top < now {
            Some((top - REVEAL_MARGIN).max(0.0))
        } else if bottom > now + viewport {
            Some((bottom - viewport + REVEAL_MARGIN).min(max))
        } else {
            None
        };
        Some((to, i + 1 == shown.len()))
    });
    if let Some((to, last)) = revealed {
        if let Some(to) = to {
            scroll.set_offset(gpui::point(len(0.), px(-to)));
        }
        // Away from the newest message the log leaves the end; on it, it follows the end again (as the wheel does).
        let follow = on_wheel.clone();
        cx.defer(move |cx| follow(!last, cx));
    }
    if revealed.is_some() {
        // The keyboard's row set the offset above; the end is not re-pinned under it.
    } else if let Some(at) = jumped {
        let max = (inner - viewport).max(0.0);
        let r = &shapes[at];
        let centre = ui.update(cx, |u, _| std::mem::take(&mut u.centre));
        let target = if centre {
            centred_offset(tops[at], r.height(density), viewport, inner)
        } else {
            (tops[at] - 48.0).max(0.0)
        };
        scroll.set_offset(gpui::point(len(0.), px(-target.min(max))));
        if centre {
            // The search hit's row takes the focus (its handle is made here when the row has not been drawn yet).
            let key = shown[at].item.key.clone();
            let handle = ui.update(cx, |u, cx| {
                u.row_focus
                    .entry(key)
                    .or_insert_with(|| cx.focus_handle())
                    .clone()
            });
            window.focus(&handle, cx);
        }
        // The log leaves the end. Its host (the shell) is drawing this log right now, so it hears of it once the frame is
        // done: an update of it from here would be a re-entrant update (a panic).
        let leave_end = on_wheel.clone();
        cx.defer(move |cx| leave_end(true, cx));
    } else if pinned && viewport > 0.0 {
        scroll.set_offset(gpui::point(len(0.), px(-(height - viewport).max(0.0))));
    } else if pinned {
        scroll.scroll_to_bottom();
    } else {
        // Earlier rows put in front (MessageLog.tsx:103-114): the offset moves by the added height, so the rows in view
        // stay put.
        let now = -f32::from(scroll.offset().y);
        let to = anchored_offset(
            prev_first.as_deref(),
            first_key.as_deref(),
            prev_height,
            inner,
            now,
            pinned,
        );
        if to != now {
            let max = (inner - viewport).max(0.0);
            scroll.set_offset(gpui::point(len(0.), px(-to.min(max))));
        }
    }
    ui.update(cx, |u, _| {
        u.prev_first = first_key;
        u.prev_height = inner;
    });
    let row_cx = RowCx {
        state,
        members,
        community: channel.community_id,
        clock,
        live: ctx.client.is_some(),
        width,
        order: order.clone(),
        active: active.clone(),
        density,
        now,
    };
    // Only the rows near the viewport are drawn (a screen above and below it): the rest keep their height from the memo.
    let at = if jumped.is_some() || revealed.is_some() {
        -f32::from(scroll.offset().y)
    } else if pinned && viewport > 0.0 {
        (height - viewport).max(0.0)
    } else {
        -f32::from(scroll.offset().y)
    };
    let (from, to) = (at - viewport, at + 2.0 * viewport);
    // The voice channels of the "started music" cards that intersect the viewport (not the screen above and below it,
    // which is only drawn): the player is read for these while the Bot is in the room (Decision D10).
    let mut cards_on_screen = std::collections::BTreeSet::new();
    for (s, top) in shown.into_iter().zip(tops) {
        if let Some(label) = s.divider.clone() {
            placer.divider(top - divider_height(density), width, label);
        }
        let bottom = top + s.shape().height(density);
        let editing = matches!(s.body, Plan::Editor(_));
        // The active row is always drawn: it is the log's tab stop wherever the log is scrolled.
        let keyed = active.as_deref() == Some(s.item.key.as_str());
        if viewport > 0.0 && !editing && !keyed && (bottom < from || top > to) {
            continue;
        }
        if (viewport <= 0.0 || (bottom >= at && top <= at + viewport))
            && readable(s.item)
            && let Some(marker) = announcement_in(state, channel.community_id, s.item)
        {
            cards_on_screen.insert(marker.voice_channel);
        }
        let base = snap_px(top);
        let el = draw_row(&row_cx, s, top, base, &ui, ctx, window, cx);
        placer.out.push(el);
    }
    crate::screens::bot_admin::player::card_on_screen(
        window.window_handle().window_id(),
        cards_on_screen,
        state,
        ctx.client.as_ref(),
        crate::screens::bot_admin::player::clock_ms(ctx.clock),
        cx,
    );
    let mut private_top = rows_bottom;
    for (n, (el, h)) in privates.into_iter().enumerate() {
        let id: &'static str = PRIVATE_RESULT_IDS
            .get(n)
            .copied()
            .unwrap_or("private-result");
        let mut block = div()
            .absolute()
            .left(len(0.))
            .top(px(private_top.round()))
            .child(
                div()
                    .id(id)
                    .debug_selector(move || id.to_owned())
                    .w(px(width))
                    .h(px(h))
                    .child(el),
            );
        // MessageRow.css:8 through BotResult's own `d-message-row`: it rises in as a message row does.
        if !reduced
            && let Some((elapsed, delay)) = ui.read(cx).log.of(&format!("private-{n}"), now)
            && elapsed < delay + token::ENTER
        {
            let (opacity, offset) =
                motion::rise_after_at(elapsed, delay, token::ENTER, token::EASE_OUT);
            block = block.top(px(private_top.round() + offset)).opacity(opacity);
        }
        placer.out.push(block.into_any_element());
        private_top += h;
    }
    let pager = scroll.clone();
    let focus = ui.update(cx, |u, cx| {
        u.log_focus.get_or_insert_with(|| cx.focus_handle()).clone()
    });
    let to_focus = focus.clone();
    // MessageLog.tsx: `role="log"` named "messages in #general" (a DM's by its name). A new message from someone else
    // in the open log is read out through the window's announcer (`announce_new`), as the web's `aria-live`.
    let target = if state.dms.get(channel.id).is_some() {
        channel.name.clone()
    } else {
        format!("#{}", channel.name)
    };
    // Conversation.tsx:397: the log is busy while its timeline or a "load earlier" is being read.
    let busy = head == LogHead::Loading || ui.read(cx).loading_earlier == Some(channel.id);
    let log = div().id("message-log").a11y(
        Role::Log,
        t("shell.log.label").replace("#{channel}", &target),
    );
    let log = if busy {
        log.node_props(|n| n.set_busy())
    } else {
        log
    };
    let to_top = (scroll.clone(), on_wheel.clone(), focus.clone(), ui.clone());
    log.key_context("MessageLog")
        .track_focus(&focus)
        .on_mouse_down(gpui::MouseButton::Left, move |_, window, cx| {
            // A focusable control inside (a reaction chip, a button) took the focus on this press already (GPUI marks
            // that with prevent_default): it keeps it. A press on the log's own surface focuses the log.
            if !window.default_prevented() {
                window.focus(&to_focus, cx);
            }
        })
        .size_full()
        .bg(color::BG)
        .overflow_y_scroll()
        .track_scroll(scroll)
        .on_scroll(move |event, _window, cx| {
            on_wheel(event.delta.pixel_delta(len(20.)).y > len(0.), cx);
        })
        // The log itself focused (a press on its surface): Up and Down go to its active row, as the web's log does.
        .on_key_down({
            let (ui, active) = (ui.clone(), active.clone());
            let log = focus.clone();
            move |event, window, cx| {
                let k = &event.keystroke;
                if !log.is_focused(window)
                    || k.modifiers.modified()
                    || !matches!(k.key.as_str(), "up" | "down")
                {
                    return;
                }
                let Some(key) = active.clone() else {
                    return;
                };
                cx.stop_propagation();
                let h = ui.update(cx, |u, cx| {
                    u.reveal = Some(key.clone());
                    cx.notify();
                    u.row_focus
                        .entry(key)
                        .or_insert_with(|| cx.focus_handle())
                        .clone()
                });
                window.focus(&h, cx);
            }
        })
        .on_action(move |_: &PageUp, _, _| page(&pager, -1.0))
        .on_action({
            let pager = scroll.clone();
            move |_: &PageDown, _, _| page(&pager, 1.0)
        })
        // Ctrl+Home scrolls the log to its top, as the browser scrolls the web's focused log (Ruling R16), where "load
        // earlier" sits; the log leaves the end. Only the log or a row holding the focus takes it: in a field inside the
        // log (the inline editor) it is the field's, as a textarea keeps it on the web.
        .on_action(move |_: &LogTop, window, cx| {
            let (scroll, leave_end, log, ui) = &to_top;
            let ours = log.is_focused(window)
                || ui.read(cx).row_focus.values().any(|h| h.is_focused(window));
            if !ours {
                cx.propagate();
                return;
            }
            scroll.set_offset(gpui::point(len(0.), len(0.)));
            leave_end(true, cx);
        })
        .child({
            let content = div().relative().w_full().h(px(inner)).children(placer.out);
            // MessageLog.css:3-4: a channel switch fades the log in while it rises 6px.
            match entering.log {
                Some((opacity, offset)) => content.top(px(offset)).opacity(opacity),
                None => content,
            }
        })
}

gpui::actions!(message_log, [PageUp, PageDown, LogTop, FocusComposer]);

/// PageUp and PageDown scroll the log by a viewport less a few lines.
fn page(scroll: &ScrollHandle, direction: f32) {
    let view = f32::from(scroll.bounds().size.height);
    let step = (view - 3.0 * body_line()).max(view / 2.0) * direction;
    let max = f32::from(scroll.max_offset().y);
    let now = -f32::from(scroll.offset().y);
    let next = (now + step).clamp(0.0, max);
    scroll.set_offset(gpui::point(len(0.), px(-next)));
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::{DeviceId, TimelineItemState, UserId};

    /// A jump's flash beats everything else on the row, a pointer over the row included (a CSS animation beats `:hover`):
    /// only a row with nothing else to show gets a hover style, and a flashing row never does.
    #[test]
    fn the_flash_beats_the_hover_and_every_other_fill() {
        for replying in [false, true] {
            for mention in [false, true] {
                for focused in [false, true] {
                    assert_eq!(row_fill(true, replying, mention, focused), RowFill::Flash);
                }
            }
        }
        assert_eq!(row_fill(false, false, false, false), RowFill::Hover);
        assert_eq!(row_fill(false, true, true, true), RowFill::Replying);
        assert_eq!(row_fill(false, false, true, true), RowFill::Mention);
        assert_eq!(row_fill(false, false, false, true), RowFill::Focused);
    }

    fn item(sender: u8, ts: u64) -> TimelineItem {
        TimelineItem {
            key: format!("s{ts}"),
            state: TimelineItemState::Ok,
            reason: String::new(),
            sender_user: Some(UserId([sender; 16])),
            sender_device: Some(DeviceId([sender; 16])),
            own: false,
            bot: false,
            ts,
            body: "hi".into(),
            msg_id: None,
            seq: Some(ts),
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
            actions: Vec::new(),
        }
    }

    /// Shell.tsx:1788-1793: a search hit's row goes to the middle of the viewport, within the log's ends.
    #[test]
    fn a_search_hit_is_centred() {
        assert_eq!(centred_offset(1000.0, 40.0, 600.0, 3000.0), 720.0);
        assert_eq!(centred_offset(100.0, 40.0, 600.0, 3000.0), 0.0);
        assert_eq!(centred_offset(2950.0, 40.0, 600.0, 3000.0), 2400.0);
    }

    #[test]
    fn runs_fold_within_seven_minutes_of_the_row_above() {
        let clock = Clock::Fixed {
            now: 1_794_476_484,
            utc: true,
        };
        let a = item(1, 1_794_470_000);
        let b = item(1, 1_794_470_000 + GROUP_SECONDS);
        let c = item(1, 1_794_470_000 + 2 * GROUP_SECONDS + 1);
        let d = item(2, 1_794_470_000 + 2 * GROUP_SECONDS + 2);
        assert!(!grouped_with_above(None, &a, clock));
        assert!(grouped_with_above(Some(&a), &b, clock));
        assert!(!grouped_with_above(Some(&b), &c, clock));
        assert!(!grouped_with_above(Some(&c), &d, clock));
    }

    #[test]
    fn a_new_day_never_folds_into_the_run_above_and_starts_a_divider() {
        let clock = Clock::Fixed {
            now: 1_794_476_484,
            utc: true,
        };
        // 2026-11-11 23:59:00 UTC and 2026-11-12 00:01:00 UTC: two minutes apart, two days.
        let late = item(1, 1_794_441_540);
        let early = item(1, 1_794_441_660);
        assert!(starts_day(None, &late, clock));
        assert!(starts_day(Some(&late), &early, clock));
        assert!(!grouped_with_above(Some(&late), &early, clock));
        // The same day does not start one.
        let next = item(1, 1_794_441_600 + 3_000);
        assert!(!starts_day(Some(&early), &next, clock));
    }

    #[test]
    fn an_authors_name_takes_the_first_role_colour_they_have() {
        use dilla_client::state::roles::RoleSummary;
        let community = CommunityId([9; 16]);
        let role = |n: u8, color: u32| RoleSummary {
            id: [n; 16],
            name: format!("r{n}"),
            color,
            position: u32::from(n),
            hoist: false,
            mentionable: false,
            allow: 0,
            deny: 0,
            members: 0,
        };
        let mut state = AppState::default();
        state.roles.roles.insert(
            community,
            vec![role(1, 0), role(2, 0x5aa8e6), role(3, 0xff0000)],
        );
        let member = |id: u8, roles: Vec<[u8; 16]>| Member {
            user_id: UserId([id; 16]),
            username: "u".into(),
            display: "U".into(),
            kind: 0,
            role_ids: roles,
            status_msg: String::new(),
            bio: String::new(),
        };
        let members = [
            member(1, vec![[1; 16], [2; 16], [3; 16]]),
            member(2, vec![[1; 16]]),
        ];
        let of =
            |u: Option<u8>| name_colour_of(&state, community, &members, u.map(|n| UserId([n; 16])));
        assert_eq!(
            of(Some(1)),
            Some(0x005a_a8e6),
            "the first coloured role, not the last"
        );
        assert_eq!(of(Some(2)), None, "a role with no colour");
        assert_eq!(of(Some(3)), None, "not a member");
        assert_eq!(of(None), None, "no author");
    }

    #[test]
    fn only_the_row_the_reply_bar_aims_at_is_replying() {
        use dilla_client::state::MessageId;
        let (a, b) = (MessageId([1; 16]), MessageId([2; 16]));
        assert!(is_replying(Some(a), Some(a)));
        assert!(!is_replying(Some(a), Some(b)));
        assert!(!is_replying(Some(a), None));
        assert!(!is_replying(None, None), "a row with no id is never it");
    }

    #[test]
    fn a_reply_and_an_unreadable_row_keep_their_own_head() {
        let clock = Clock::Fixed {
            now: 1_794_476_484,
            utc: true,
        };
        let a = item(1, 1_794_470_000);
        let mut reply = item(1, 1_794_470_010);
        reply.reply = Some(dilla_client::state::messages::ReplyRef {
            msg_id: dilla_client::state::MessageId([3; 16]),
            state: 0,
            sender_user: None,
            excerpt: String::new(),
            seq: None,
        });
        assert!(!grouped_with_above(Some(&a), &reply, clock));
        let mut gone = item(1, 1_794_470_020);
        gone.state = TimelineItemState::Deleted;
        assert!(!grouped_with_above(Some(&a), &gone, clock));
        let mut unknown = item(1, 1_794_470_030);
        unknown.sender_user = None;
        assert!(!grouped_with_above(Some(&a), &unknown, clock));
    }

    #[test]
    fn heights_come_from_the_css() {
        // The reference's DOM: 54.89 for a row, 49.89 for the log's last row, 75.03 for two lines; 131 for the welcome.
        let lh = body_line();
        assert!(
            (row_height(false, false, false, 0.0, lh, 0.0, Density::Regular) - 54.890_625).abs()
                < 1e-4
        );
        assert!(
            (row_height(false, false, true, 0.0, lh, 0.0, Density::Regular) - 49.890_625).abs()
                < 1e-4
        );
        assert!(
            (row_height(false, false, false, 0.0, 2.0 * lh, 0.0, Density::Regular) - 75.031_25)
                .abs()
                < 1e-4
        );
        assert_eq!(welcome_height(1, 1, Density::Regular), 131.0);
        assert_eq!(divider_height(Density::Regular), 23.0);
    }

    /// Task 19b: the head, the marks' line, the divider and the welcome take the density's `--line-height`; the body
    /// keeps its 1.55 and the gutter's time its 1.5, so a row moves only by its head line.
    #[test]
    fn the_density_steps_the_lines_that_take_the_root_line_height() {
        let lh = body_line();
        let (c, r) = (Density::Compact, Density::Regular);
        assert_eq!(head_line(c), line_height(text::SM, 1.4));
        assert_eq!(meta_line(c), line_height(text::META, 1.4));
        assert_eq!(gutter_line(), line_height(text::META, 1.5));
        let compact = row_height(false, false, false, 0.0, lh, 0.0, c);
        let regular = row_height(false, false, false, 0.0, lh, 0.0, r);
        assert!(compact < regular);
        assert!((regular - compact - (head_line(r) - head_line(c))).abs() < 1e-4);
        // A plain follow-up has no head line: the same at every density.
        assert_eq!(
            row_height(true, false, false, 0.0, lh, 0.0, c),
            row_height(true, false, false, 0.0, lh, 0.0, r)
        );
        assert_eq!(
            divider_height(c),
            2.0 * DIVIDER_PAD + line_height(text::MICRO, 1.4)
        );
        assert!(welcome_height(1, 1, c) < welcome_height(1, 1, r));
        assert!(welcome_height(1, 1, Density::Cozy) > welcome_height(1, 1, r));
    }

    #[test]
    fn days_are_named_like_the_web_log() {
        let now = 1_794_476_484; // 2026-11-12 09:41 UTC
        let clock = Clock::Fixed { now, utc: true };
        assert_eq!(day_label(now - 60, clock), "today");
        assert_eq!(day_label(now - 86_400, clock), "yesterday");
        // An older day is the date, in the region's form.
        let older = day_label(now - 10 * 86_400, clock);
        assert!(older.contains("2026") && older.contains('2'), "{older}");
    }

    #[test]
    fn authors_are_named_like_the_web() {
        let mut s = AppState::default();
        s.account.user_id = Some(UserId([9; 16]));
        s.account.username = "me".into();
        let member = Member {
            user_id: UserId([1; 16]),
            username: "ada".into(),
            display: "Ada L".into(),
            kind: 0,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        };
        let members = [member];
        assert_eq!(author(&s, &members, &item(1, 5)), "Ada L");
        assert_eq!(author(&s, &members, &item(9, 5)), "me");
        assert_eq!(
            author(&s, &members, &item(7, 5)),
            UserId([7; 16]).hex()[..8]
        );
        let mut anon = item(1, 5);
        anon.sender_user = None;
        assert_eq!(
            author(&s, &members, &anon),
            tf("shell.message.unknownAuthor", &[("id", "01010101")])
        );
    }

    #[test]
    fn mentions_are_pills_by_who_they_name() {
        let mut s = AppState::default();
        s.account.user_id = Some(UserId([9; 16]));
        s.account.username = "me".into();
        let everyone = mention_pill(&s, &[], "everyone", true);
        assert!(everyone.broadcast && everyone.me && everyone.label == "@everyone");
        let me = mention_pill(&s, &[], &UserId([9; 16]).hex(), false);
        assert!(me.me && me.label == "@me");
        let unknown = mention_pill(&s, &[], &UserId([4; 16]).hex(), true);
        assert!(!unknown.me && unknown.user.is_none());
    }

    /// A community "harbour" voice channel, the Bot "Glenn" (kind 1) and Ada (kind 0), and a Bot's announcement for a track
    /// Ada asked for in it.
    fn music_world() -> (AppState, CommunityId, TimelineItem) {
        let community = CommunityId([7; 16]);
        let mut s = AppState::default();
        s.channels.push(Channel {
            id: ChannelId([8; 16]),
            community_id: community,
            kind: 1,
            mode: 0,
            name: "harbour".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: Default::default(),
        });
        let member = |user: u8, name: &str, kind: u8| Member {
            user_id: UserId([user; 16]),
            username: name.to_lowercase(),
            display: name.to_owned(),
            kind,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        };
        s.members
            .insert(community, vec![member(1, "Ada", 0), member(9, "Glenn", 1)]);
        let mut i = item(9, 5);
        i.bot = true;
        i.body = format!(
            "Now playing\n[dilla-music:v1:{}:{}:{}]",
            "08".repeat(16),
            "01".repeat(16),
            "0a".repeat(16)
        );
        (s, community, i)
    }

    /// MessageItem.tsx:180-181: the Bot's announcement reads "{name} started music in {room}", whatever text it carries; the
    /// same body edited, or from a member of kind 0, is text.
    #[test]
    fn a_bot_music_announcement_reads_started_music() {
        let (s, community, i) = music_world();
        let members = s.members[&community].clone();
        assert_eq!(
            body_parts(&s, &members, community, &i, false, &UTC),
            [Body::Text("Ada started music in harbour".into())]
        );
        // A room this viewer cannot see is "a voice channel".
        let mut unseen = s.clone();
        unseen.channels.clear();
        assert_eq!(
            body_parts(&unseen, &members, community, &i, false, &UTC),
            [Body::Text("Ada started music in a voice channel".into())]
        );
        let verbatim = |item: &TimelineItem| {
            body_parts(&s, &members, community, item, false, &UTC)
                == body_parts(
                    &s,
                    &members,
                    community,
                    &item_with_body(&item.body),
                    false,
                    &UTC,
                )
        };
        let mut edited = i.clone();
        edited.edited = true;
        assert!(verbatim(&edited), "an edited post stays text");
        let mut member = i.clone();
        member.sender_user = Some(UserId([1; 16]));
        member.bot = false;
        assert!(verbatim(&member), "a member's post stays text");
    }

    /// Review Focus 4: the announcement is the Bot's alone. A forged marker (a member's post, a credential that claims a Bot
    /// with no Bot member row, an edit, a poll post) is no announcement and keeps its body, marker line included.
    #[test]
    fn a_forged_music_marker_stays_text() {
        let (s, community, i) = music_world();
        let members = s.members[&community].clone();
        assert!(
            announcement_marker(&i, Some(1)).is_some(),
            "the Bot's own post"
        );
        let mut member_post = i.clone();
        member_post.bot = false;
        let mut edited = i.clone();
        edited.edited = true;
        let mut poll = i.clone();
        poll.body = format!("{}\n[dilla-poll:v1:{}]", i.body, "ab".repeat(16));
        let forged = [
            ("a member's post", member_post, Some(1)),
            ("the claim of a member of kind 0", i.clone(), Some(0)),
            ("the claim of no member", i.clone(), None),
            ("an edited post", edited, Some(1)),
            ("a poll post", poll, Some(1)),
        ];
        for (what, item, kind) in forged {
            assert!(announcement_marker(&item, kind).is_none(), "{what}");
        }
        // The row's text agrees: it shows the body as it is.
        let mut kind0 = i.clone();
        kind0.sender_user = Some(UserId([1; 16]));
        assert_eq!(
            body_parts(&s, &members, community, &kind0, false, &UTC),
            body_parts(
                &s,
                &members,
                community,
                &item_with_body(&kind0.body),
                false,
                &UTC
            ),
            "a kind-0 member's claim"
        );
        assert!(announcement_in(&s, community, &kind0).is_none());
        assert!(announcement_in(&s, community, &i).is_some());
    }

    /// Integration 3, step 5 (Task 21's `decode_webhook_label` in Task 9's row): a webhook post is the community's pinned
    /// Bot user's unedited message ending in its label line. The row shows "via webhook <name>" in its head and the text
    /// before the label as its body; the same body from anyone else, or edited, stays text.
    #[test]
    fn a_webhook_post_of_the_pinned_bot_reads_via_webhook() {
        use dilla_client::state::bot::{BotCatalogView, BotStatus, BotStatusState};
        let community = CommunityId([7; 16]);
        let bot = UserId([9; 16]);
        let mut s = AppState::default();
        s.bot.catalogs.insert(
            community,
            BotCatalogView {
                revision: 1,
                commands: Vec::new(),
                status: BotStatus {
                    state: BotStatusState::Ready,
                    since: None,
                },
                bot_channels: None,
                bot_user: Some(bot),
                fingerprint: None,
            },
        );
        let mut i = item(9, 5);
        i.bot = true;
        i.body = format!(
            "Deploy done\n[dilla-webhook:v1:{}:CI%20bot]",
            "ab".repeat(16)
        );
        let label = webhook_of(&s, community, &i).expect("the pinned Bot's webhook post");
        assert_eq!(
            (label.text.as_str(), label.name.as_str()),
            ("Deploy done", "CI bot")
        );
        assert_eq!(
            via_mark(&s, community, &i).as_deref(),
            Some("via webhook CI bot")
        );
        assert_eq!(
            body_parts(&s, &[], community, &i, false, &UTC),
            body_parts(
                &s,
                &[],
                community,
                &item_with_body("Deploy done"),
                false,
                &UTC
            ),
            "the body is the text before the label"
        );
        let mut edited = i.clone();
        edited.edited = true;
        assert!(
            webhook_of(&s, community, &edited).is_none(),
            "an edited post stays text"
        );
        let mut other = i.clone();
        other.sender_user = Some(UserId([4; 16]));
        assert!(
            webhook_of(&s, community, &other).is_none(),
            "anyone else's stays text"
        );
        assert!(
            webhook_of(&s, CommunityId([1; 16]), &i).is_none(),
            "another community's Bot is not this one's"
        );
    }

    /// Integration 3, final review I3 (log-model.ts `groupedWithAbove` and MessageRow.css): two webhook posts in a row
    /// group as on the web (one sender, the pinned Bot user), and the follow-up still shows its own "via webhook
    /// <name>": a grouped row whose head shows a mark keeps a head line of its marks, so it is taller than a plain
    /// follow-up and its body starts under that line.
    #[test]
    fn a_grouped_webhook_post_keeps_its_via_label() {
        use dilla_client::state::bot::{BotCatalogView, BotStatus, BotStatusState};
        let community = CommunityId([7; 16]);
        let mut s = AppState::default();
        s.bot.catalogs.insert(
            community,
            BotCatalogView {
                revision: 1,
                commands: Vec::new(),
                status: BotStatus {
                    state: BotStatusState::Ready,
                    since: None,
                },
                bot_channels: None,
                bot_user: Some(UserId([9; 16])),
                fingerprint: None,
            },
        );
        let post = |ts: u64, name: &str| {
            let mut i = item(9, ts);
            i.bot = true;
            i.body = format!("text\n[dilla-webhook:v1:{}:{name}]", "ab".repeat(16));
            i
        };
        let (ci, alerts) = (post(5, "CI"), post(65, "Alerts"));
        let clock = Clock::Fixed {
            now: 1_794_476_484,
            utc: true,
        };
        assert!(
            grouped_with_above(Some(&ci), &alerts, clock),
            "they group, as on the web"
        );
        assert!(
            marks_head(&s, community, &alerts),
            "the follow-up keeps a head of its marks"
        );
        assert_eq!(
            via_mark(&s, community, &alerts).as_deref(),
            Some("via webhook Alerts")
        );
        assert!(
            !marks_head(&s, community, &item(9, 70)),
            "a plain follow-up has none"
        );
        let lh = body_line();
        assert!(
            row_height(true, true, false, 0.0, lh, 0.0, Density::Regular)
                > row_height(true, false, false, 0.0, lh, 0.0, Density::Regular)
        );
        assert!(
            body_offset(true, true, Density::Regular) > body_offset(true, false, Density::Regular)
        );
        assert_eq!(
            body_offset(true, true, Density::Regular),
            FOLLOW_PAD + meta_line(Density::Regular) + MAIN_GAP
        );
    }

    const UTC: jiff::tz::TimeZone = jiff::tz::TimeZone::UTC;

    /// MessageItem.poll.test.tsx's world: the Bot (a kind-1 member) posts a poll of `bob`'s, the poll held or not.
    fn poll_world(held: bool) -> (AppState, CommunityId, TimelineItem) {
        let (mut s, community, mut i) = music_world();
        s.members.get_mut(&community).unwrap().push(Member {
            user_id: UserId([2; 16]),
            username: "bob".into(),
            display: String::new(),
            kind: 0,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        });
        s.account.user_id = Some(UserId([1; 16]));
        let id = dilla_client::state::MessageId([4; 16]);
        i.msg_id = Some(id);
        i.body = format!(
            "Raid?\nBridge\nHarbour\nVote with /poll vote.\nCloses: <t:1800003600:R>\n[dilla-poll:v1:{}]",
            "ab".repeat(16)
        );
        if held {
            s.bot.polls.insert(
                id,
                dilla_client::state::bot::PollView {
                    id: [0xab; 16],
                    channel: ChannelId([8; 16]),
                    thread: None,
                    creator: UserId([2; 16]),
                    question: "Raid?".into(),
                    answers: vec!["Bridge".into(), "Harbour".into()],
                    multi: false,
                    created_at: 0,
                    due_at: 0,
                    closed: false,
                    counts: vec![0, 0],
                    my_choices: None,
                    revision: 1,
                    previews: vec![Vec::new(), Vec::new()],
                    distinct: 0,
                    capped: false,
                },
            );
        }
        (s, community, i)
    }

    fn poll_text(s: &AppState, community: CommunityId, i: &TimelineItem, live: bool) -> String {
        let members = s.members[&community].clone();
        spoken_body(&body_parts(s, &members, community, i, live, &UTC))
    }

    /// MessageItem.tsx:163-182: a verified poll post reads who asked, the poll's creator and not the Bot.
    #[test]
    fn a_verified_poll_post_reads_who_asked() {
        let (s, community, i) = poll_world(true);
        assert_eq!(poll_text(&s, community, &i, true), "bob asked:");
    }

    /// A post whose card is being verified shows no text: the card's loading line stands for it. A read the web gave up
    /// (or is retrying after giving up) shows the text again.
    #[test]
    fn a_poll_post_being_verified_shows_no_text() {
        let (mut s, community, i) = poll_world(false);
        assert_eq!(poll_text(&s, community, &i, true), "");
        let id = i.msg_id.unwrap();
        s.bot.poll_checks.insert(id, PollCheck::Pending);
        assert_eq!(poll_text(&s, community, &i, true), "");
        for gave_up in [
            PollCheck::GaveUp(ChannelId([8; 16])),
            PollCheck::Retry(ChannelId([8; 16])),
        ] {
            s.bot.poll_checks.insert(id, gave_up);
            assert!(poll_text(&s, community, &i, true).contains("Vote with /poll vote."));
        }
    }

    #[test]
    fn an_unverified_poll_post_shows_its_text_with_local_time() {
        let (s, community, i) = poll_world(false);
        let text = poll_text(&s, community, &i, false);
        assert!(text.contains("Vote with /poll vote."), "{text}");
        assert!(text.contains("Closes: Fri 15 Jan, 09:00 UTC"), "{text}");
        assert!(
            !text.contains("<t:") && !text.contains("[dilla-poll"),
            "{text}"
        );
        // A poll post of a sender who is no Bot member is plain text, marker line and tag included.
        let mut forged = i.clone();
        forged.sender_user = Some(UserId([1; 16]));
        forged.bot = false;
        assert_eq!(
            poll_body(&forged, &s, Some(community), true, &UTC),
            PollBody::NoMarker
        );
        assert!(poll_text(&s, community, &forged, true).contains("[dilla-poll"));
    }

    /// Decrypted text never goes in Debug: only the variant and the lengths do.
    #[test]
    fn a_poll_body_debug_redacts_its_text() {
        for body in [
            PollBody::Asked("secret-name".into()),
            PollBody::Fallback("secret text".into()),
        ] {
            let shown = format!("{body:?}");
            assert!(!shown.contains("secret"), "{shown}");
        }
        assert_eq!(format!("{:?}", PollBody::Pending), "Pending");
    }

    /// MessageLog.tsx's polite log: a pending poll is announced with the card's loading line, and again, with "{name}
    /// asked:", once it verifies.
    #[test]
    fn a_pending_poll_is_announced_loading_and_again_when_verified() {
        let (mut s, _, i) = poll_world(false);
        let channel = s.channels[0].clone();
        let mut earlier = item(1, 1);
        earlier.key = "first".into();
        let id = i.msg_id.unwrap();
        let tl = Timeline {
            channel_id: channel.id,
            group: Default::default(),
            items: vec![earlier, i.clone()],
            has_earlier: false,
        };
        s.timelines.insert(channel.id, tl);
        let clock = Clock::Fixed { now: 0, utc: true };
        let seen = Some((channel.id, "first".to_owned()));
        let (key, text) = arrival(&s, &channel, clock, seen.as_ref()).expect("a new row");
        let text = text.expect("announced");
        assert!(text.ends_with(": Loading poll…"), "{text}");
        // The same state again says nothing.
        assert!(arrival(&s, &channel, clock, Some(&key)).is_none());
        // The poll verifies: the row's new text is announced.
        let held = poll_world(true).0.bot.polls[&id].clone();
        s.bot.polls.insert(id, held);
        let (_, text) = arrival(&s, &channel, clock, Some(&key)).expect("a change");
        assert!(text.expect("announced").ends_with(": bob asked:"));
    }

    #[test]
    fn an_edited_poll_post_shows_its_text() {
        let (s, community, mut i) = poll_world(true);
        i.edited = true;
        let text = poll_text(&s, community, &i, true);
        assert!(text.contains("Vote with /poll vote."), "{text}");
        assert!(!text.contains("bob asked:"), "{text}");
    }

    /// The fallback keeps a tag it cannot turn into a time, and only a final marker line is removed.
    #[test]
    fn the_poll_fallback_keeps_what_it_cannot_read() {
        let marker = format!("[dilla-poll:v1:{}]", "ab".repeat(16));
        let text = poll_fallback(
            &format!("a <t:x:R> <t:99999999999999999999:R> <t:1:Z> <t:\n{marker}"),
            &UTC,
        );
        assert_eq!(text, "a <t:x:R> <t:99999999999999999999:R> <t:1:Z> <t:");
        let mid = format!("{marker}\nmore");
        assert_eq!(poll_fallback(&mid, &UTC), mid);
        assert_eq!(poll_fallback(&marker, &UTC), "");
    }

    fn item_with_body(body: &str) -> TimelineItem {
        let mut i = item(9, 5);
        i.body = body.into();
        i
    }

    #[test]
    fn the_head_marks_are_edited_pinned_and_the_send_state() {
        let mut i = item(1, 5);
        assert!(head_marks(&i).is_empty());
        i.edited = true;
        i.pinned = true;
        i.state = TimelineItemState::Pending;
        assert_eq!(head_marks(&i), ["edited", "pinned", "sending…"]);
    }

    /// Another area's part under a message (reactions, cards, a poll, a reply chip) moves every row below it down by
    /// exactly its height, and the log's content grows by it.
    #[test]
    fn extras_height_moves_the_next_row_down() {
        let lh = body_line();
        let row = |extras: f32, last: bool| RowShape {
            grouped: false,
            marks: false,
            last,
            before: 0.0,
            body: lh,
            extras,
            divider: false,
        };
        let follow = RowShape {
            grouped: true,
            marks: false,
            last: true,
            before: 0.0,
            body: 2.0 * lh,
            extras: 0.0,
            divider: true,
        };
        let (plain, plain_end) = row_tops(
            12.0,
            &[row(0.0, false), row(0.0, false), follow],
            Density::Regular,
        );
        let (with, with_end) = row_tops(
            12.0,
            &[row(0.0, false), row(26.5, false), follow],
            Density::Regular,
        );
        assert_eq!(with[0], plain[0]);
        assert_eq!(with[1], plain[1], "the row with extras starts where it did");
        assert!(
            (with[2] - plain[2] - 26.5).abs() < 1e-4,
            "the next row moves down by the extras' height"
        );
        assert!((with_end - plain_end - 26.5).abs() < 1e-4);
    }

    /// A body of code cards and quotes makes its row as tall as its blocks: the log follows the body's height.
    #[test]
    fn a_taller_body_moves_the_next_row_down() {
        let lh = body_line();
        let one = RowShape {
            grouped: false,
            marks: false,
            last: false,
            before: 0.0,
            body: lh,
            extras: 0.0,
            divider: false,
        };
        let tall = RowShape {
            body: lh + 70.0,
            ..one
        };
        let (a, _) = row_tops(0.0, &[one, one], Density::Regular);
        let (b, _) = row_tops(0.0, &[tall, one], Density::Regular);
        assert!((b[1] - a[1] - 70.0).abs() < 1e-4);
    }

    /// Conversation.tsx:396-399: "load earlier" while the history has more, the welcome only at its true start, and
    /// neither before the timeline is read.
    #[test]
    fn the_welcome_waits_for_the_start_of_the_history() {
        let timeline = |has_earlier| dilla_client::state::Timeline {
            channel_id: ChannelId([1; 16]),
            group: Default::default(),
            items: Vec::new(),
            has_earlier,
        };
        assert_eq!(log_head(None), LogHead::Loading);
        assert_eq!(log_head(Some(&timeline(true))), LogHead::Earlier);
        assert_eq!(log_head(Some(&timeline(false))), LogHead::Welcome);
    }

    /// Conversation.tsx:394-395: the note under the welcome, `emptyMade` for a channel this client made, none in a DM, with
    /// rows or while the log loads.
    #[test]
    fn an_empty_channel_says_older_messages_are_not_shown() {
        let mut s = AppState::default();
        let ch = ChannelId([1; 16]);
        assert_eq!(empty_note(&s, ch, false, 0, true), Some("shell.log.empty"));
        s.made_channels.insert(ch);
        assert_eq!(
            empty_note(&s, ch, false, 0, true),
            Some("shell.log.emptyMade")
        );
        assert_eq!(empty_note(&s, ch, true, 0, true), None);
        assert_eq!(empty_note(&s, ch, false, 1, true), None);
        assert_eq!(empty_note(&s, ch, false, 0, false), None);
    }

    /// MessageLog.tsx:103-114: rows put in front move the offset by the added height, so the rows in view stay put.
    #[test]
    fn earlier_rows_put_in_front_keep_the_reader_in_place() {
        assert_eq!(
            anchored_offset(Some("k5"), Some("k0"), 1000.0, 1600.0, 40.0, false),
            640.0
        );
        // The same first row: nothing was put in front.
        assert_eq!(
            anchored_offset(Some("k5"), Some("k5"), 1000.0, 1600.0, 40.0, false),
            40.0
        );
        // Pinned: the pin path sets the offset.
        assert_eq!(
            anchored_offset(Some("k5"), Some("k0"), 1000.0, 1600.0, 40.0, true),
            40.0
        );
        // The first draw has nothing to keep.
        assert_eq!(
            anchored_offset(None, Some("k0"), 0.0, 1600.0, 0.0, false),
            0.0
        );
        // A message arriving at the bottom in the same frame as the earlier page: the whole growth is added, as the web
        // adds it.
        assert_eq!(
            anchored_offset(Some("k5"), Some("k0"), 1000.0, 1640.0, 40.0, false),
            680.0
        );
    }
}
