//! The message log (packages/ui MessageLog and MessageRow, web `screens/conversation`, Signal §5.5): the channel's
//! welcome, day dividers, and one row per message: the 44px time gutter, the 30px avatar, the author and the body,
//! wrapped like the web's `white-space: pre-wrap; overflow-wrap: anywhere`. A row continues the run above it (no gutter
//! time, avatar or author) when the same author wrote it within seven minutes, on the same day (log-model.ts
//! `groupedWithAbove`). The log keeps to its end until the person scrolls up, as the web log does at present.
//!
//! Every height is the web CSS's (MessageLog.css, MessageRow.css) in Chromium's layout units, so the log's content has
//! the web's height and, at its end, the web's (whole-pixel) scroll offset. The log is not virtualised: every row of the
//! open timeline (at most the client's 100-row page) is laid out.

use chrono::Datelike;
use dilla_client::state::{AppState, Channel, Member, TimelineItem, TimelineItemState, UserId};
use gpui::{
    AnyElement, InteractiveElement, IntoElement, ParentElement, ScrollHandle,
    StatefulInteractiveElement, Styled, div, px,
};

use dilla_desktop_ui::components::{Avatar, AvatarSize};
use dilla_desktop_ui::text::{Line, MONO_ADVANCE, line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, font, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::screens::LABEL_TRACKING_WIDE;
use crate::screens::shell::Clock;

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
/// The row grid's 1px gap, and the empty attachment slot's 4px margin (`0.25rem`) every row keeps but the log's last.
const MAIN_GAP: f32 = 1.0;
const ATTACHMENTS_MARGIN: f32 = 4.0;
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
    line_height(text::INPUT, 1.55)
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
    divider: Option<String>,
    last: bool,
    /// The body's lines, wrapped to the content width.
    lines: Vec<Line>,
}

fn readable(i: &TimelineItem) -> bool {
    matches!(
        i.state,
        TimelineItemState::Ok | TimelineItemState::Pending | TimelineItemState::Failed
    )
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
    if clock.day(prev.ts) != clock.day(item.ts) {
        return false;
    }
    item.ts >= prev.ts && item.ts - prev.ts <= GROUP_SECONDS
}

/// log-model.ts `dayLabel`: today, yesterday, else the date.
pub fn day_label(ts: u64, clock: Clock) -> String {
    let days = clock.day(clock.now()) - clock.day(ts);
    match days {
        0 => "today".to_owned(),
        1 => "yesterday".to_owned(),
        _ => clock.format(ts, |d| {
            format!("{} {} {}", d.format("%A"), d.day(), d.format("%B"))
        }),
    }
}

fn author(members: &[Member], user: Option<UserId>) -> &str {
    user.and_then(|u| members.iter().find(|m| m.user_id == u))
        .map_or("unknown", |m| {
            if m.display.is_empty() {
                m.username.as_str()
            } else {
                m.display.as_str()
            }
        })
}

fn body_text(item: &TimelineItem) -> String {
    match item.state {
        TimelineItemState::CannotRead => "This message cannot be read on this device.".to_owned(),
        TimelineItemState::Deleted => "This message was deleted.".to_owned(),
        _ => item.body.clone(),
    }
}

/// The height Chromium gives a row with `lines` body lines.
fn row_height(grouped: bool, last: bool, lines: usize) -> f32 {
    let slot = if last {
        0.0
    } else {
        MAIN_GAP + ATTACHMENTS_MARGIN
    };
    let body = lines as f32 * body_line();
    if grouped {
        FOLLOW_PAD + body + slot + FOLLOW_PAD
    } else {
        ROW_TOP + head_line() + MAIN_GAP + body + slot + ROW_BOTTOM
    }
}

/// The welcome's lines, wrapped to the log's width less its padding.
fn welcome_lines(channel: &str, width: f32) -> (Vec<Line>, Vec<Line>) {
    let inner = width - 2.0 * WELCOME_X;
    let title = paragraph(
        &format!("Welcome to #{channel}"),
        text::WELCOME,
        line_height(text::WELCOME, 1.5),
        color::FG,
        inner,
    )
    .into_iter()
    .map(|l| l.weight(font::WEIGHT_DISPLAY))
    .collect();
    let body = paragraph(
        &format!("This is the start of #{channel}."),
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

    /// The welcome at `top`; answers its height.
    fn welcome(&mut self, top: f32, title: Vec<Line>, body: Vec<Line>) -> f32 {
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
                .child(Icon::new(IconName::Hash).size(px(20.)).color(color::ACCENT)),
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
                .h(px(1.))
                .bg(color::HAIRLINE),
        );
    }

    /// One row at `top`.
    fn row(&mut self, top: f32, shown: Shown<'_>, members: &[Member], clock: Clock) {
        let item = shown.item;
        let body_top = if shown.grouped {
            top + FOLLOW_PAD
        } else {
            let head = top + ROW_TOP;
            let name = author(members, item.sender_user);
            let time = clock.format(item.ts, |d| d.format("%H:%M").to_string());
            // The time is right-aligned in the gutter, by its advance.
            let time_width = time.chars().count() as f32 * f32::from(text::META) * MONO_ADVANCE;
            self.line(
                line(time, text::META, meta_line(), color::FG_QUIET),
                GUTTER - time_width,
                head + GUTTER_TOP,
            );
            self.block(
                GUTTER + COLUMN_GAP,
                head,
                Avatar::new(
                    gpui::SharedString::from(format!("avatar-{}", item.key)),
                    name.to_owned(),
                )
                .size(AvatarSize::Msg),
            );
            self.line(
                line(name.to_owned(), text::SM, head_line(), color::FG)
                    .weight(font::WEIGHT_DISPLAY),
                CONTENT_X,
                head,
            );
            head + head_line() + MAIN_GAP
        };
        for (n, l) in shown.lines.into_iter().enumerate() {
            self.line(l, CONTENT_X, body_top + n as f32 * body_line());
        }
    }
}

/// The log of `channel`, `width` wide, kept at its end through `scroll` while `pinned`.
pub fn render(
    state: &AppState,
    channel: &Channel,
    clock: Clock,
    width: f32,
    scroll: &ScrollHandle,
    pinned: bool,
    on_wheel: OnWheel,
) -> impl IntoElement + use<> {
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
    let mut shown = Vec::with_capacity(items.len());
    for (n, item) in items.iter().enumerate() {
        let prev = n.checked_sub(1).map(|p| items[p]);
        let starts_day = prev.is_none_or(|p| clock.day(p.ts) != clock.day(item.ts));
        let body_color = match item.state {
            TimelineItemState::Ok => color::FG_BODY,
            TimelineItemState::Pending => color::FG_2,
            TimelineItemState::Failed => color::DANGER,
            _ => color::FG_3,
        };
        shown.push(Shown {
            item,
            grouped: grouped_with_above(prev, item, clock),
            divider: starts_day.then(|| day_label(item.ts, clock)),
            last: n + 1 == items.len(),
            lines: paragraph(
                &body_text(item),
                text::INPUT,
                body_line(),
                body_color,
                body_width,
            ),
        });
    }
    let (title, body) = welcome_lines(&channel.name, width);
    let mut content = LOG_TOP + welcome_height(title.len(), body.len()) + LOG_BOTTOM;
    for s in &shown {
        content += row_height(s.grouped, s.last, s.lines.len())
            + if s.divider.is_some() {
                divider_height()
            } else {
                0.0
            };
    }
    // Chromium's scroll range is whole pixels: the content's height rounds, and at the end that rounding sits below the
    // last row. A log shorter than its viewport keeps to the viewport's bottom (`margin-top: auto`).
    let viewport = f32::from(scroll.bounds().size.height);
    let height = content.round();
    let lift = (viewport - height).max(0.0).round();
    if pinned && viewport > 0.0 {
        scroll.set_offset(gpui::point(px(0.), px(-(height - viewport).max(0.0))));
    } else if pinned {
        scroll.scroll_to_bottom();
    }
    let mut placer = Placer { out: Vec::new() };
    let mut y = lift + LOG_TOP;
    y += placer.welcome(y, title, body);
    for s in shown {
        if let Some(label) = s.divider.clone() {
            placer.divider(y, width, label);
            y += divider_height();
        }
        let h = row_height(s.grouped, s.last, s.lines.len());
        placer.row(y, s, members, clock);
        y += h;
    }
    div()
        .id("message-log")
        .size_full()
        .bg(color::BG)
        .overflow_y_scroll()
        .track_scroll(scroll)
        .on_scroll(move |event, _window, cx| {
            on_wheel(event.delta.pixel_delta(px(20.)).y > px(0.), cx);
        })
        .child(
            div()
                .relative()
                .w_full()
                .h(px(height + lift))
                .children(placer.out),
        )
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::{DeviceId, TimelineItemState};

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
    fn heights_come_from_the_css() {
        // The reference's DOM: 54.89 for a row, 49.89 for the log's last row, 75.03 for two lines; 131 for the welcome.
        assert!((row_height(false, false, 1) - 54.890_625).abs() < 1e-4);
        assert!((row_height(false, true, 1) - 49.890_625).abs() < 1e-4);
        assert!((row_height(false, false, 2) - 75.031_25).abs() < 1e-4);
        assert_eq!(welcome_height(1, 1), 131.0);
        assert_eq!(divider_height(), 23.0);
    }

    #[test]
    fn days_are_named_like_the_web_log() {
        let now = 1_794_476_484; // 2026-11-12 09:41 UTC
        let clock = Clock::Fixed { now, utc: true };
        assert_eq!(day_label(now - 60, clock), "today");
        assert_eq!(day_label(now - 86_400, clock), "yesterday");
    }
}
