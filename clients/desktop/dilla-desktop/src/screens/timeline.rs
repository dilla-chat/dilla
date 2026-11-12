//! The message log (packages/ui MessageLog and MessageRow, web `screens/conversation`, Signal §5.5): the channel's
//! welcome, day dividers, and one row per message: the 44px time gutter, the 30px avatar, the author and the body.
//! A row continues the run above it (no gutter time, avatar or author) when the same author wrote it within seven
//! minutes, on the same day (log-model.ts `groupedWithAbove`). The log keeps to its end, as the web log does at present.
//!
//! Heights are Chromium's, in its 1/64 px layout units: a body line of 13px at 1.55 is 20.140625px high.

use chrono::Datelike;
use dilla_client::state::{AppState, Channel, Member, TimelineItem, TimelineItemState, UserId};
use gpui::{
    AnyElement, InteractiveElement, IntoElement, ParentElement, ScrollHandle,
    StatefulInteractiveElement, Styled, div, px,
};

use dilla_desktop_ui::tokens::{color, font, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::screens::avatar_colour;
use crate::screens::shell::Clock;
use crate::text::{Line, line, lines};
use dilla_desktop_ui::components::avatar::initials_of;

/// log-model.ts `GROUP_SECONDS`.
pub const GROUP_SECONDS: u64 = 7 * 60;
/// One body line: 13px at line-height 1.55, in layout units.
const BODY_LINE: f32 = 20.140_625;
/// The author line: 12.5px at 1.5.
const HEAD_LINE: f32 = 18.75;
/// A row's padding: 7px above a run's first row and 3px below; 1px above and below a follow-up.
const ROW_TOP: f32 = 7.0;
const ROW_BOTTOM: f32 = 3.0;
/// The row grid's gap between the head and the body, and the space every row but the last keeps below its body (the
/// grid's gap and its empty attachment slot).
const MAIN_GAP: f32 = 1.0;
const SLOT: f32 = 5.0;
/// The log's padding: 12px above, 6px below.
const LOG_TOP: f32 = 12.0;
const LOG_BOTTOM: f32 = 6.0;
/// The welcome block (tile, title, body; 16px 20px 18px padding) and a day divider.
const WELCOME: f32 = 131.0;
const DIVIDER: f32 = 23.0;

/// What the log draws for one item.
struct Shown<'a> {
    item: &'a TimelineItem,
    grouped: bool,
    divider: Option<String>,
    last: bool,
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

/// The height Chromium gives a row.
fn row_height(shown: &Shown<'_>) -> f32 {
    let lines = body_text(shown.item).split('\n').count() as f32;
    let slot = if shown.last { 0.0 } else { SLOT };
    let main = if shown.grouped {
        lines * BODY_LINE + slot
    } else {
        HEAD_LINE + MAIN_GAP + lines * BODY_LINE + slot
    };
    let pad = if shown.grouped {
        2.0
    } else {
        ROW_TOP + ROW_BOTTOM
    };
    main + pad
}

/// The scroll handler of the log: whether the wheel moved up.
pub type OnWheel = std::rc::Rc<dyn Fn(bool, &mut gpui::App)>;

/// Where the log's lines go: every line painted from its exact web position ([`Line::exact`]); the content's top is a
/// whole pixel (the viewport's top less a whole offset), so those positions are the web's.
struct Placer {
    out: Vec<AnyElement>,
}

impl Placer {
    /// `line` with its pen at `x`, its web line box at `top` (content coordinates, logical px).
    fn text(&mut self, line: Line, x: f32, top: f32) {
        self.out.push(line.exact(x, top));
    }

    /// The time, right-aligned in the 44px gutter (by its advance: a laid-out box would round its width).
    fn time(&mut self, time: String, top: f32) {
        let width = time.chars().count() as f32 * 0.6 * 10.5;
        self.text(
            line(time, text::META, 15.75, color::FG_QUIET),
            44.0 - width,
            top,
        );
    }

    fn block(&mut self, top: f32, element: impl IntoElement) {
        self.out.push(
            div()
                .absolute()
                .left_0()
                .top(px(top))
                .w_full()
                .child(element)
                .into_any_element(),
        );
    }
}

fn welcome(channel: &str) -> impl IntoElement {
    div()
        .h(px(WELCOME))
        .pt(px(16.))
        .px(px(20.))
        .flex()
        .flex_col()
        .child(
            div()
                .size(px(42.))
                .rounded(radius::LG)
                .bg(color::SURFACE_TILE)
                .border_1()
                .border_color(color::HAIRLINE_2)
                .flex()
                .items_center()
                .justify_center()
                .child(Icon::new(IconName::Hash).size(px(20.)).color(color::ACCENT)),
        )
        .child(
            div().pt(px(10.)).child(
                line(
                    format!("Welcome to #{channel}"),
                    text::WELCOME,
                    27.0,
                    color::FG,
                )
                .weight(font::WEIGHT_DISPLAY),
            ),
        )
        .child(line(
            format!("This is the start of #{channel}."),
            text::UI,
            18.0,
            color::FG_2,
        ))
}

fn divider(label: String) -> impl IntoElement {
    div()
        .h(px(DIVIDER))
        .px(px(20.))
        .flex()
        .items_center()
        .gap(px(10.))
        .child(
            line(label.to_uppercase(), text::MICRO, 15.0, color::FG_3)
                .weight(font::WEIGHT_LABEL)
                .spacing(1.2),
        )
        .child(div().flex_1().h(px(1.)).bg(color::HAIRLINE))
}

/// One row at `top`: the avatar as a block, every line of text placed on its own.
fn row(placer: &mut Placer, top: f32, shown: &Shown<'_>, members: &[Member], clock: Clock) {
    let item = shown.item;
    let name = author(members, item.sender_user);
    let body_color = match item.state {
        TimelineItemState::Ok | TimelineItemState::Pending => color::FG_BODY,
        TimelineItemState::Failed => color::DANGER,
        _ => color::FG_3,
    };
    let body = body_text(item);
    let body_top = if shown.grouped {
        top + 1.0
    } else {
        let head = top + ROW_TOP;
        placer.time(
            clock.format(item.ts, |d| d.format("%H:%M").to_string()),
            head + 18.0,
        );
        placer.block(
            head,
            div()
                .pl(px(54.))
                .child(div().size(px(30.)).rounded_full().bg(avatar_colour(name))),
        );
        let initials = initials_of(name);
        let width = initials.chars().count() as f32 * 0.6 * 10.5;
        placer.text(
            line(initials, text::META, 15.75, color::BRAND_INK).weight(font::WEIGHT_DISPLAY),
            54.0 + (30.0 - width) / 2.0,
            head + (30.0 - 15.75) / 2.0,
        );
        placer.text(
            line(name.to_owned(), text::SM, HEAD_LINE, color::FG).weight(font::WEIGHT_DISPLAY),
            94.0,
            head,
        );
        head + HEAD_LINE + MAIN_GAP
    };
    for (n, text_line) in lines(&body, text::INPUT, BODY_LINE, body_color).enumerate() {
        placer.text(text_line, 94.0, body_top + n as f32 * BODY_LINE);
    }
}

/// The log of `channel`, kept at its end through `scroll` while `pinned`.
pub fn render(
    state: &AppState,
    channel: &Channel,
    clock: Clock,
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
    let mut shown = Vec::with_capacity(items.len());
    for (n, item) in items.iter().enumerate() {
        let prev = n.checked_sub(1).map(|p| items[p]);
        let starts_day = prev.is_none_or(|p| clock.day(p.ts) != clock.day(item.ts));
        shown.push(Shown {
            item,
            grouped: grouped_with_above(prev, item, clock),
            divider: starts_day.then(|| day_label(item.ts, clock)),
            last: n + 1 == items.len(),
        });
    }
    let mut content = LOG_TOP + WELCOME + LOG_BOTTOM;
    for s in &shown {
        content += row_height(s) + if s.divider.is_some() { DIVIDER } else { 0.0 };
    }
    // Chromium's scroll range is whole pixels: the content rounds, and at the end that rounding sits below the last row.
    let height = content.round();
    let viewport = f32::from(scroll.bounds().size.height);
    // A log shorter than its viewport keeps to the viewport's bottom (`margin-top: auto`).
    let lift = (viewport - height).max(0.0);
    let mut placer = Placer { out: Vec::new() };
    let mut y = lift + LOG_TOP;
    placer.block(y, welcome(&channel.name));
    y += WELCOME;
    for s in &shown {
        if let Some(label) = s.divider.clone() {
            placer.block(y, divider(label));
            y += DIVIDER;
        }
        row(&mut placer, y, s, members, clock);
        y += row_height(s);
    }
    // At the end, the offset is the web's exactly: the rounded content less the viewport (GPUI's own end would sit up to
    // a pixel off, its layout being snapped to device pixels).
    if pinned && viewport > 0.0 {
        scroll.set_offset(gpui::point(px(0.), px(-(height - viewport).max(0.0))));
    } else if pinned {
        scroll.scroll_to_bottom();
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
    fn rows_are_as_high_as_chromium_makes_them() {
        let a = item(1, 0);
        let one = |last| Shown {
            item: &a,
            grouped: false,
            divider: None,
            last,
        };
        // Measured in the reference: 54.89 for a row, 49.89 for the log's last row.
        assert!((row_height(&one(false)) - 54.890_625).abs() < 1e-4);
        assert!((row_height(&one(true)) - 49.890_625).abs() < 1e-4);
    }

    #[test]
    fn days_are_named_like_the_web_log() {
        let now = 1_794_476_484; // 2026-11-12 09:41 UTC
        let clock = Clock::Fixed { now, utc: true };
        assert_eq!(day_label(now - 60, clock), "today");
        assert_eq!(day_label(now - 86_400, clock), "yesterday");
    }
}
