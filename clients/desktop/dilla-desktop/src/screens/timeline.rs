//! The message log (packages/ui MessageLog and MessageRow, web `screens/conversation`, Signal §5.5): the channel's
//! welcome, day dividers, and one row per message: the 44px time gutter, the 30px avatar, the author and the body. The
//! body is the message's markdown laid out as the web lays it out ([`crate::screens::messages::body`]). A row continues
//! the run above it (no gutter time, avatar or author) when the same author wrote it within seven minutes, on the same
//! day (log-model.ts `groupedWithAbove`). The log keeps to its end until the person scrolls up, as the web log does at
//! present; PageUp and PageDown scroll it.
//!
//! Every height is the web CSS's (MessageLog.css, MessageRow.css) in Chromium's layout units, so the log's content has
//! the web's height and, at its end, the web's (whole-pixel) scroll offset. The log is not virtualised: every row of the
//! open timeline (at most the client's 100-row page) is laid out.
//!
//! Each row is a box of its own at its CSS position: it takes the hover fill and bar (`--bg-3`, 2px `--accent`), the
//! mention tint (`--warn-soft`, 2px `--mention`) and the flash of a jump, and holds the toolbar over its top edge.

use dilla_client::state::{
    AppState, Channel, ChannelId, CommunityId, Member, TimelineItem, TimelineItemState,
};
use gpui::{
    AnyElement, App, InteractiveElement, IntoElement, ParentElement, ScrollHandle,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::tag::{Tag, TagKind, look};
use dilla_desktop_ui::components::{Avatar, AvatarSize};
use dilla_desktop_ui::text::{
    Face, Line, MONO_ADVANCE, chromium_baseline, line, line_height, paragraph,
};
use dilla_desktop_ui::tokens::{color, font, radius, text};
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
const WELCOME_TOP: f32 = 16.0;
const WELCOME_X: f32 = 20.0;
const WELCOME_BOTTOM: f32 = 18.0;
const WELCOME_TILE: f32 = 42.0;
const WELCOME_TITLE_GAP: f32 = 10.0;
const DIVIDER_PAD: f32 = 4.0;
const DIVIDER_GAP: f32 = 10.0;

fn body_line() -> f32 {
    body::body_line()
}
fn head_line() -> f32 {
    line_height(text::SM, 1.5)
}
fn meta_line() -> f32 {
    line_height(text::META, 1.5)
}
fn divider_height() -> f32 {
    2.0 * DIVIDER_PAD + line_height(text::MICRO, 1.5)
}

/// The scroll handler of the log: whether the wheel moved up.
pub type OnWheel = std::rc::Rc<dyn Fn(bool, &mut gpui::App)>;

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
    fn laid(self, width: f32, y0: f32) -> Laid {
        match self {
            Self::Editor(l) => l,
            Self::Text {
                parts,
                color,
                height,
            } => {
                let mut laid = body::layout_from(&parts, width, color, y0);
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
fn row_tops(start: f32, rows: &[RowShape]) -> (Vec<f32>, f32) {
    let mut y = start;
    let mut tops = Vec::with_capacity(rows.len());
    for r in rows {
        if r.divider {
            y += divider_height();
        }
        tops.push(y);
        y += row_height(r.grouped, r.marks, r.last, r.before, r.body, r.extras);
    }
    (tops, y)
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
fn zone_of(clock: Clock, secs: i64) -> chrono::FixedOffset {
    let east = clock
        .format(u64::try_from(secs).unwrap_or(0), |d| {
            d.offset().local_minus_utc().to_string()
        })
        .parse::<i32>()
        .unwrap_or(0);
    chrono::FixedOffset::east_opt(east)
        .unwrap_or_else(|| chrono::FixedOffset::east_opt(0).expect("utc"))
}

/// log-model.ts `dayLabel`: today, yesterday, else the date.
pub fn day_label(ts: u64, clock: Clock) -> String {
    let zone = |secs: i64| zone_of(clock, secs);
    format::day_divider_in(ts, clock.now(), &zone, &format::date_locale())
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
    dilla_client::state::bot_admin::decode_webhook_label(&item.body)
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
) -> Vec<Body> {
    match item.state {
        TimelineItemState::CannotRead => vec![Body::Text(t("shell.message.cannotRead").to_owned())],
        TimelineItemState::Deleted => vec![Body::Text(t("shell.message.deleted").to_owned())],
        _ => {
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
) -> String {
    let name = author(state, members, item);
    let zone = |secs: i64| zone_of(clock, secs);
    let time = format::time_in(item.ts, &zone);
    let parts = body_parts(state, members, community, item);
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
    let key = (channel.id, last.key.clone());
    match seen {
        Some(s) if *s == key => None,
        // Another log (or the first look at this one): its tail is what was already there.
        Some((c, _)) if *c != channel.id => Some((key, None)),
        None => Some((key, None)),
        Some(_) if last.own || !readable(last) => Some((key, None)),
        Some(_) => {
            let members = state
                .members
                .get(&channel.community_id)
                .map(Vec::as_slice)
                .unwrap_or_default();
            let text = spoken_item(state, members, channel.community_id, last, clock);
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
fn body_offset(grouped: bool, marks: bool) -> f32 {
    match (grouped, marks) {
        (true, false) => FOLLOW_PAD,
        // The grouped head is a flex row whose only in-flow children are meta spans (the author is screen-reader only,
        // absolutely placed): a flex container has no strut, so it is one meta line high (MessageRow.css).
        (true, true) => FOLLOW_PAD + meta_line() + MAIN_GAP,
        (false, _) => ROW_TOP + head_line() + MAIN_GAP,
    }
}

/// The attachments slot's height (the grid's 1px gap and its 4px margin) between the body and the other areas' parts:
/// none in the log's last row when it is empty (MessageLog.css `:last-child .d-message-row__attachments:empty`).
fn attachments_slot(last: bool) -> f32 {
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
/// `before` px (a [`before_shift`]) above its head.
fn row_height(grouped: bool, marks: bool, last: bool, before: f32, body: f32, extras: f32) -> f32 {
    let slot = attachments_slot(last);
    let body = before + body + extras;
    if grouped && marks {
        FOLLOW_PAD + meta_line() + MAIN_GAP + body + slot + FOLLOW_PAD
    } else if grouped {
        FOLLOW_PAD + body + slot + FOLLOW_PAD
    } else {
        ROW_TOP + head_line() + MAIN_GAP + body + slot + ROW_BOTTOM
    }
}

/// The welcome's lines, wrapped to the log's width less its padding (Shell.tsx `welcome`): a channel's "Welcome to
/// #name" over "This is the start of #name.", a DM's name over "This is the start of your conversation.".
fn welcome_lines(channel: &str, dm: bool, width: f32) -> (Vec<Line>, Vec<Line>) {
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
        line_height(text::WELCOME, 1.5),
        color::FG,
        inner,
    )
    .into_iter()
    .map(|l| l.weight(font::WEIGHT_DISPLAY))
    .collect();
    let body = paragraph(
        &body,
        text::UI,
        line_height(text::UI, 1.5),
        color::FG_2,
        inner,
    );
    (title, body)
}

fn welcome_height(title: usize, body: usize) -> f32 {
    WELCOME_TOP
        + WELCOME_TILE
        + WELCOME_TITLE_GAP
        + title as f32 * line_height(text::WELCOME, 1.5)
        + body as f32 * line_height(text::UI, 1.5)
        + WELCOME_BOTTOM
}

/// The log's content, every element at its CSS position (content coordinates, logical px). Lines keep their fractions
/// ([`Line::exact`]); boxes take the whole pixel Chromium paints them on (the position rounded).
struct Placer {
    out: Vec<AnyElement>,
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
    fn welcome(&mut self, top: f32, glyph: IconName, title: Vec<Line>, body: Vec<Line>) -> f32 {
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
        let height = welcome_height(title.len(), body.len());
        let mut y = top + WELCOME_TOP + WELCOME_TILE + WELCOME_TITLE_GAP;
        for l in title {
            self.line(l, WELCOME_X, y);
            y += line_height(text::WELCOME, 1.5);
        }
        for l in body {
            self.line(l, WELCOME_X, y);
            y += line_height(text::UI, 1.5);
        }
        height
    }

    /// A day divider at `top`, `width` wide: the tracked label, then a rule to the right padding.
    fn divider(&mut self, top: f32, width: f32, label: String) {
        let label = label.to_uppercase();
        let size = text::MICRO;
        let label_width =
            label.chars().count() as f32 * f32::from(size) * (MONO_ADVANCE + LABEL_TRACKING_WIDE);
        let lh = line_height(size, 1.5);
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
            top + divider_height() / 2.0 - 0.5,
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
    width: f32,
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
    if let Some(via) = via_mark(cx_row.state, cx_row.community, item) {
        let w = meta_width(window, &via);
        placer.line(line(via, text::META, meta_line(), meta_color), x, meta_top);
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
            let baseline = meta_top + chromium_baseline(Face::Mono, em, meta_line());
            let glyph = 0.875 * em;
            placer.block_exact(
                x,
                baseline + 0.125 * em - glyph,
                Icon::new(IconName::X).size(px(glyph)).color(tone),
            );
            x += glyph + 0.25 * em;
        }
        let w = mark.chars().count() as f32 * f32::from(text::META) * MONO_ADVANCE;
        placer.line(line(mark, text::META, meta_line(), tone), x, meta_top);
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
    let local = top - base;
    // The body's box sits on a whole pixel ([`snap_px`]); its lines keep the CSS position's fraction.
    let shift = before_shift(shown.before_height);
    let frac = {
        let at = local + shift + body_offset(shown.grouped, shown.marks);
        at - snap_px(at)
    };
    let laid = shown.body.laid(cx_row.width - CONTENT_X - ROW_RIGHT, frac);
    let height = row_height(
        shown.grouped,
        shown.marks,
        shown.last,
        shift,
        laid.height,
        shown.extras_height,
    );
    let mut placer = Placer { out: Vec::new() };
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
            line(time, text::META, meta_line(), color::FG_QUIET),
            GUTTER - time_width,
            row_top + GUTTER_TOP,
        );
        // MessageRow.tsx `avatarSrc`: a webhook post shows its avatar, read only from the instance (Task 28 item 15),
        // once it is in; the placeholder meanwhile and on a miss.
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
            None => placer.block(
                GUTTER + COLUMN_GAP,
                row_top,
                Avatar::new(
                    gpui::SharedString::from(format!("avatar-{}", item.key)),
                    name.clone(),
                )
                .size(AvatarSize::Msg),
            ),
        }
        let name_width = name.chars().count() as f32 * f32::from(text::SM) * MONO_ADVANCE;
        let bot = item.bot
            && cx_row
                .members
                .iter()
                .any(|m| Some(m.user_id) == item.sender_user && m.kind == 1);
        let name_color = if bot { color::ACCENT_2 } else { color::FG };
        placer.line(
            line(name, text::SM, head_line(), name_color).weight(font::WEIGHT_DISPLAY),
            CONTENT_X,
            head,
        );
        // The head is a baseline-aligned row of the name, the tag and the marks, 8px apart.
        let baseline = head + chromium_baseline(Face::Mono, f32::from(text::SM), head_line());
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
        let meta_top = baseline - chromium_baseline(Face::Mono, f32::from(text::META), meta_line());
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
    let body_top = local + shift + body_offset(shown.grouped, shown.marks);
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
    let flash = ui.read(cx).flashing(&item.key);
    // MessageRow.tsx's `<article>`: one node per message, named by its author, time and text. Its id is the message's
    // key, so a row keeps its node while earlier messages load above it.
    let spoken = spoken_item(
        cx_row.state,
        cx_row.members,
        cx_row.community,
        item,
        cx_row.clock,
    );
    let mut row = div()
        .id(gpui::SharedString::from(format!("message-{}", item.key)))
        .a11y(Role::Article, spoken)
        .absolute()
        .left(len(0.))
        .top(px(base))
        .w(px(cx_row.width))
        .h(px(snap_px(top + height) - base))
        .group(group.clone());
    row = if flash {
        row.bg(color::ACCENT_FLASH)
    } else if item.mention {
        row.bg(color::WARN_SOFT)
    } else {
        row.hover(|s| s.bg(color::BG_3))
    };
    let bar = if item.mention {
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
    row = row
        .child(crate::screens::messages::probe(
            crate::screens::messages::id_of("message-row", shown.index),
        ))
        .children(placer.out);
    if let Some(bar) = toolbar::toolbar(item, shown.index, &group, ctx, ui, window, cx) {
        row = row.child(bar);
    }
    row.into_any_element()
}

/// The log of `channel`, `width` wide, kept at its end through `scroll` while `pinned`. Each row's
/// [`slots::message_extras`] sits under its body.
#[allow(clippy::too_many_arguments)]
pub fn render(
    state: &AppState,
    channel: &Channel,
    clock: Clock,
    width: f32,
    scroll: &ScrollHandle,
    pinned: bool,
    on_wheel: OnWheel,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> impl IntoElement + use<> {
    let ui = ui::ui(window, cx);
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
    let members: &[Member] = state
        .members
        .get(&channel.community_id)
        .map_or(&[], Vec::as_slice);
    let items: Vec<&TimelineItem> = state
        .timelines
        .get(&channel.id)
        .map(|t| t.items.iter().collect())
        .unwrap_or_default();
    let body_width = width - CONTENT_X - ROW_RIGHT;
    let editing_key = ui.read(cx).editing.as_ref().map(|e| e.key.clone());
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
        let (extras, extras_height) = slots::message_extras(item, ctx, window, cx);
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
            let parts = body_parts(state, members, channel.community_id, item);
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
            // Only an empty attachments slot is hidden in the last row; one with cards keeps its gap and margin.
            last: n + 1 == items.len() && item.attachments.is_empty(),
            body: laid,
            extras: Some(extras),
            extras_height,
            before,
            before_height,
            group: format!("message-row-{}", item.key),

            index: n,
        });
    }
    // A DM's log: its channel record is the DM's (Shell.tsx: the at-sign and the DM's name).
    let dm = state.dms.get(channel.id).is_some();
    let (title, body_lines) = welcome_lines(&channel.name, dm, width);
    let shapes: Vec<RowShape> = shown.iter().map(Shown::shape).collect();
    let (_, rows_end) = row_tops(0.0, &shapes);
    // Task 20's private results: the rows only this device shows, after the conversation's last message.
    let privates = crate::screens::commands::private_results(ctx, width, window, cx);
    let privates_height: f32 = privates.iter().map(|(_, h)| *h).sum();
    let content = LOG_TOP
        + welcome_height(title.len(), body_lines.len())
        + LOG_BOTTOM
        + rows_end
        + privates_height;
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
    let mut placer = Placer { out: Vec::new() };
    let mut y = lift + LOG_TOP;
    let glyph = if dm { IconName::AtSign } else { IconName::Hash };
    y += placer.welcome(y, glyph, title, body_lines);
    let (tops, rows_bottom) = row_tops(y, &shapes);
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
    if let Some(at) = jumped {
        let target = (tops[at] - 48.0).max(0.0);
        let max = (inner - viewport).max(0.0);
        scroll.set_offset(gpui::point(len(0.), px(-target.min(max))));
        // The log leaves the end. Its host (the shell) is drawing this log right now, so it hears of it once the frame is
        // done: an update of it from here would be a re-entrant update (a panic).
        let leave_end = on_wheel.clone();
        cx.defer(move |cx| leave_end(true, cx));
    } else if pinned && viewport > 0.0 {
        scroll.set_offset(gpui::point(len(0.), px(-(height - viewport).max(0.0))));
    } else if pinned {
        scroll.scroll_to_bottom();
    }
    let row_cx = RowCx {
        state,
        members,
        community: channel.community_id,
        clock,
        width,
    };
    // Only the rows near the viewport are drawn (a screen above and below it): the rest keep their height from the memo.
    let at = if jumped.is_some() {
        -f32::from(scroll.offset().y)
    } else if pinned && viewport > 0.0 {
        (height - viewport).max(0.0)
    } else {
        -f32::from(scroll.offset().y)
    };
    let (from, to) = (at - viewport, at + 2.0 * viewport);
    for (s, top) in shown.into_iter().zip(tops) {
        if let Some(label) = s.divider.clone() {
            placer.divider(top - divider_height(), width, label);
        }
        let bottom = top
            + row_height(
                s.grouped,
                s.marks,
                s.last,
                before_shift(s.before_height),
                s.body.height(),
                s.extras_height,
            );
        let editing = matches!(s.body, Plan::Editor(_));
        if viewport > 0.0 && !editing && (bottom < from || top > to) {
            continue;
        }
        let base = snap_px(top);
        let el = draw_row(&row_cx, s, top, base, &ui, ctx, window, cx);
        placer.out.push(el);
    }
    let mut private_top = rows_bottom;
    for (n, (el, h)) in privates.into_iter().enumerate() {
        let id: &'static str = PRIVATE_RESULT_IDS
            .get(n)
            .copied()
            .unwrap_or("private-result");
        placer.block(
            0.0,
            private_top,
            div()
                .id(id)
                .debug_selector(move || id.to_owned())
                .w(px(width))
                .h(px(h))
                .child(el),
        );
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
    div()
        .id("message-log")
        .a11y(
            Role::Log,
            t("shell.log.label").replace("#{channel}", &target),
        )
        .key_context("MessageLog")
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
        .on_action(move |_: &PageUp, _, _| page(&pager, -1.0))
        .on_action({
            let pager = scroll.clone();
            move |_: &PageDown, _, _| page(&pager, 1.0)
        })
        .child(div().relative().w_full().h(px(inner)).children(placer.out))
}

gpui::actions!(message_log, [PageUp, PageDown]);

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
        assert!((row_height(false, false, false, 0.0, lh, 0.0) - 54.890_625).abs() < 1e-4);
        assert!((row_height(false, false, true, 0.0, lh, 0.0) - 49.890_625).abs() < 1e-4);
        assert!((row_height(false, false, false, 0.0, 2.0 * lh, 0.0) - 75.031_25).abs() < 1e-4);
        assert_eq!(welcome_height(1, 1), 131.0);
        assert_eq!(divider_height(), 23.0);
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
                bot_name: "Glenn".into(),
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
            body_parts(&s, &[], community, &i),
            body_parts(&s, &[], community, &item_with_body("Deploy done")),
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
                bot_name: "Glenn".into(),
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
            row_height(true, true, false, 0.0, lh, 0.0)
                > row_height(true, false, false, 0.0, lh, 0.0)
        );
        assert!(body_offset(true, true) > body_offset(true, false));
        assert_eq!(body_offset(true, true), FOLLOW_PAD + meta_line() + MAIN_GAP);
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
        let (plain, plain_end) = row_tops(12.0, &[row(0.0, false), row(0.0, false), follow]);
        let (with, with_end) = row_tops(12.0, &[row(0.0, false), row(26.5, false), follow]);
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
        let (a, _) = row_tops(0.0, &[one, one]);
        let (b, _) = row_tops(0.0, &[tall, one]);
        assert!((b[1] - a[1] - 70.0).abs() < 1e-4);
    }
}
