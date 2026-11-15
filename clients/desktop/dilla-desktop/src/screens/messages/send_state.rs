//! A row's send and read state under its body (MessageItem.tsx, MessageRow.tsx `pendingAction`, `detail` and `actions`).
//!
//! An edit, delete, reaction or pin still in the outbox shows on its message (`pendingAction`): "saving…" while it waits,
//! "not saved" with **retry** and **discard** when it failed (the first failed one wins). A post whose file is gone from
//! the instance (`E_ATTACHMENT_MISSING`) says so and offers only discard. Then: a failed send
//! shows its reason (`send.tooLarge` for `E_TOO_LARGE`, the code otherwise) and offers **retry** and **discard**; a row
//! that cannot be read says why when the reason is a privacy verdict (`privacy.changed`, `privacy.unlistedMessage`,
//! `privacy.revokedMessage` for `E_DEVICE_REVOKED`).
//!
//! MessageRow.css: `.d-message-row__detail` is a chip (`--text-xs`, `--fg-3`, `0 0.25rem` padding, a 1px `--hairline-2`
//! border, `--r-md`) at the start of the column; `.d-message-row__actions` is a row of ghost buttons 0.5rem apart, 0.25rem
//! under the item above it. Each is a row of the message's grid, 1px under the item above it.

use dilla_client::state::{MessageId, TimelineItem, TimelineItemState};
use gpui::{
    App, AppContext as _, InteractiveElement as _, IntoElement, ParentElement, SharedString,
    Styled, Window, div, px,
};

use dilla_desktop_ui::components::button::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::text::{line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, font, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::slots::{EXTRA_GAP, Extra, SlotCx};
use crate::strings::t;

/// `.d-message-row__detail`'s horizontal padding (0.25rem) and border (1px).
const DETAIL_PAD_X: f32 = 4.0;
const DETAIL_BORDER: f32 = 1.0;
/// `.d-message-row__actions`: 0.25rem above, 0.5rem between the buttons; a `Button` is 2rem tall.
const ACTIONS_TOP: f32 = 4.0;
const ACTIONS_GAP: f32 = 8.0;
const BUTTON_HEIGHT: f32 = 32.0;

/// MessageItem.tsx `gone`: a failed post whose file is no longer on the instance.
fn gone(item: &TimelineItem) -> bool {
    item.state == TimelineItemState::Failed
        && item.reason == "E_ATTACHMENT_MISSING"
        && item.msg_id.is_some()
}

/// What a pending-action line offers.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum LineAction {
    Retry(MessageId),
    Discard(MessageId),
}

/// MessageRow's `pendingAction` line: its label, whether it is the failed tone, and its buttons.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PendingLine {
    pub label: &'static str,
    pub failed: bool,
    pub actions: Vec<LineAction>,
}

/// MessageItem.tsx `pendingAction`: the gone file's notice, else the first failed fold ("not saved", retry and discard),
/// else the first pending one ("saving…").
pub fn pending_line(item: &TimelineItem) -> Option<PendingLine> {
    if gone(item) {
        return Some(PendingLine {
            label: t("shell.message.attachmentGone"),
            failed: true,
            actions: vec![LineAction::Discard(item.msg_id?)],
        });
    }
    let action = item
        .actions
        .iter()
        .find(|a| a.failed)
        .or_else(|| item.actions.first())?;
    Some(if action.failed {
        PendingLine {
            label: t("shell.message.notSaved"),
            failed: true,
            actions: vec![
                LineAction::Retry(action.msg_id),
                LineAction::Discard(action.msg_id),
            ],
        }
    } else {
        PendingLine {
            label: t("shell.message.saving"),
            failed: false,
            actions: Vec::new(),
        }
    })
}

/// The detail line of `item` (MessageItem.tsx `detail`): a failed send's reason, or a cannot-read row's privacy verdict.
pub fn detail(item: &TimelineItem) -> Option<String> {
    if gone(item) {
        return None;
    }
    match item.state {
        TimelineItemState::Failed if item.reason.is_empty() => None,
        TimelineItemState::Failed if item.reason == "E_TOO_LARGE" => {
            Some(t("send.tooLarge").to_owned())
        }
        TimelineItemState::Failed => Some(item.reason.clone()),
        TimelineItemState::CannotRead => match item.reason.as_str() {
            "E_UMK_CHANGED" => Some(t("privacy.changed").to_owned()),
            "E_DEVICE_UNLISTED" => Some(t("privacy.unlistedMessage").to_owned()),
            "E_DEVICE_REVOKED" => Some(t("privacy.revokedMessage").to_owned()),
            _ => None,
        },
        _ => None,
    }
}

/// Whether `item` offers retry and discard: a failed send the outbox still holds (it has a message id).
pub fn offers_retry(item: &TimelineItem) -> bool {
    item.state == TimelineItemState::Failed && item.msg_id.is_some() && !gone(item)
}

/// The pending line's height: a row of 2rem buttons, or one `--text-xs` line.
pub fn pending_height(line: &PendingLine) -> f32 {
    if line.actions.is_empty() {
        line_height(text::XS, 1.5)
    } else {
        BUTTON_HEIGHT
    }
}

/// A ghost button that retries or discards an outbox row.
fn outbox_button(id: String, action: LineAction, ctx: &SlotCx) -> Button {
    let (label, msg, retry) = match action {
        LineAction::Retry(m) => (t("shell.message.retry"), m, true),
        LineAction::Discard(m) => (t("shell.message.discard"), m, false),
    };
    let client = ctx.client.clone();
    Button::new(SharedString::from(id), label)
        .variant(ButtonVariant::Ghost)
        .size(ButtonSize::Md)
        .on_click(move |_, _, cx| {
            if let Some(c) = &client {
                let done = if retry {
                    c.messages().retry(msg)
                } else {
                    c.messages().discard(msg)
                };
                cx.background_spawn(async move {
                    let _ = done.await;
                })
                .detach();
            }
        })
}

/// The detail chip's lines in a column `column` wide, and the chip's height.
pub fn detail_lines(detail: &str, column: f32) -> (Vec<dilla_desktop_ui::text::Line>, f32) {
    let lh = line_height(text::XS, 1.5);
    let lines = paragraph(
        detail,
        text::XS,
        lh,
        color::FG_3,
        column - 2.0 * (DETAIL_PAD_X + DETAIL_BORDER),
    );
    let height = lines.len() as f32 * lh + 2.0 * DETAIL_BORDER;
    (lines, height)
}

/// The height of the actions row.
pub fn actions_height() -> f32 {
    ACTIONS_TOP + BUTTON_HEIGHT
}

/// The state under `item`'s body: its detail chip, then (a failed send) retry and discard; `None` when it has neither.
pub fn send_state(
    item: &TimelineItem,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    let pending = pending_line(item);
    let detail = detail(item);
    let retry = offers_retry(item);
    if pending.is_none() && detail.is_none() && !retry {
        return None;
    }
    let column = crate::screens::reactions::column_width(window, cx);
    let mut height = 0.0;
    let mut col = div().flex().flex_col().items_start();
    if let Some(p) = pending {
        // `.d-message-row__pending`: `--text-xs`, `--fg-3` (`--fg-2` on a mention row), the danger tone and the x
        // glyph when failed (0.875em, 0.25em right of it), the buttons 0.5rem apart, all centred on the line.
        let tone = if p.failed {
            color::DANGER
        } else if item.mention {
            color::FG_2
        } else {
            color::FG_3
        };
        let em = f32::from(text::XS);
        let h = pending_height(&p);
        height += h;
        let mut label = div().flex().items_center();
        if p.failed {
            label = label.child(
                div()
                    .mr(px(0.25 * em))
                    .child(Icon::new(IconName::X).size(px(0.875 * em)).color(tone)),
            );
        }
        label = label.child(line(p.label, text::XS, line_height(text::XS, 1.5), tone));
        col = col.child(
            div()
                .id(SharedString::from(format!("msg-pending-{}", item.key)))
                .flex()
                .items_center()
                .gap(px(ACTIONS_GAP))
                .h(px(h))
                .child(label)
                .children(p.actions.iter().map(|a| {
                    let name = match a {
                        LineAction::Retry(_) => "retry",
                        LineAction::Discard(_) => "discard",
                    };
                    outbox_button(format!("msg-pending-{name}-{}", item.key), *a, ctx)
                })),
        );
    }
    if let Some(detail) = detail {
        let (lines, h) = detail_lines(&detail, column);
        if height > 0.0 {
            height += EXTRA_GAP;
            col = col.child(div().h(px(EXTRA_GAP)));
        }
        height += h;
        col = col.child(
            div()
                .id(SharedString::from(format!("msg-detail-{}", item.key)))
                .flex_none()
                .flex()
                .flex_col()
                .h(px(h))
                .px(px(DETAIL_PAD_X))
                .border_1()
                .border_color(color::HAIRLINE_2)
                .rounded(radius::MD)
                .font_family(font::MONO)
                .children(lines.into_iter().map(IntoElement::into_any_element)),
        );
    }
    if retry && let Some(msg) = item.msg_id {
        if height > 0.0 {
            height += EXTRA_GAP;
            col = col.child(div().h(px(EXTRA_GAP)));
        }
        height += actions_height();
        col = col.child(
            div()
                .flex()
                .gap(px(ACTIONS_GAP))
                .pt(px(ACTIONS_TOP))
                .child(outbox_button(
                    format!("msg-retry-{}", item.key),
                    LineAction::Retry(msg),
                    ctx,
                ))
                .child(outbox_button(
                    format!("msg-discard-{}", item.key),
                    LineAction::Discard(msg),
                    ctx,
                )),
        );
    }
    Some(Extra {
        element: col.into_any_element(),
        height,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn item(state: TimelineItemState, reason: &str) -> TimelineItem {
        TimelineItem {
            key: "o1".into(),
            state,
            reason: reason.into(),
            sender_user: None,
            sender_device: None,
            own: true,
            bot: false,
            ts: 1,
            body: "hi".into(),
            msg_id: Some(MessageId([1; 16])),
            seq: None,
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
            actions: Vec::new(),
        }
    }

    /// MessageItem.tsx: `E_TOO_LARGE` reads as `send.tooLarge`, any other failure as its code; a cannot-read row names
    /// a privacy verdict and nothing else.
    #[test]
    fn the_detail_is_the_webs() {
        assert_eq!(
            detail(&item(TimelineItemState::Failed, "E_TOO_LARGE")).as_deref(),
            Some(t("send.tooLarge"))
        );
        assert_eq!(
            detail(&item(TimelineItemState::Failed, "E_FORBIDDEN")).as_deref(),
            Some("E_FORBIDDEN")
        );
        assert_eq!(
            detail(&item(TimelineItemState::CannotRead, "E_DEVICE_REVOKED")).as_deref(),
            Some(t("privacy.revokedMessage"))
        );
        assert_eq!(
            detail(&item(TimelineItemState::CannotRead, "E_DEVICE_UNLISTED")).as_deref(),
            Some(t("privacy.unlistedMessage"))
        );
        assert_eq!(
            detail(&item(TimelineItemState::CannotRead, "E_UMK_CHANGED")).as_deref(),
            Some(t("privacy.changed"))
        );
        assert_eq!(
            detail(&item(TimelineItemState::CannotRead, "E_DECRYPT")),
            None
        );
        assert_eq!(detail(&item(TimelineItemState::Ok, "")), None);
        assert!(offers_retry(&item(TimelineItemState::Failed, "E_X")));
        assert!(!offers_retry(&item(TimelineItemState::Pending, "")));
        assert!(!offers_retry(&item(
            TimelineItemState::CannotRead,
            "E_DEVICE_REVOKED"
        )));
    }

    /// MessageItem.tsx `pendingAction`: a waiting fold says "saving…"; a failed one wins and offers retry and discard of
    /// its own outbox row; a post whose file is gone offers only discard, and no detail or retry row.
    #[test]
    fn a_pending_fold_shows_on_its_message() {
        use dilla_client::state::PendingAction;
        let fold = |n: u8, failed: bool| PendingAction {
            msg_id: MessageId([n; 16]),
            kind: 1,
            failed,
            reason: if failed {
                "E_FORBIDDEN".into()
            } else {
                String::new()
            },
        };
        let mut ok = item(TimelineItemState::Ok, "");
        assert_eq!(pending_line(&ok), None);
        ok.actions = vec![fold(7, false)];
        let saving = pending_line(&ok).unwrap();
        assert_eq!(
            (saving.label, saving.failed),
            (t("shell.message.saving"), false)
        );
        assert!(saving.actions.is_empty());
        assert!((pending_height(&saving) - line_height(text::XS, 1.5)).abs() < 1e-4);
        ok.actions.push(fold(8, true));
        let failed = pending_line(&ok).unwrap();
        assert_eq!(failed.label, t("shell.message.notSaved"));
        assert_eq!(
            failed.actions,
            [
                LineAction::Retry(MessageId([8; 16])),
                LineAction::Discard(MessageId([8; 16]))
            ]
        );
        assert_eq!(pending_height(&failed), BUTTON_HEIGHT);
        let gone_row = item(TimelineItemState::Failed, "E_ATTACHMENT_MISSING");
        let line = pending_line(&gone_row).unwrap();
        assert_eq!(line.label, t("shell.message.attachmentGone"));
        assert_eq!(line.actions, [LineAction::Discard(MessageId([1; 16]))]);
        assert_eq!(detail(&gone_row), None);
        assert!(!offers_retry(&gone_row));
    }

    /// A one-line chip is the xs line in its 1px border; a long verdict wraps in the column.
    #[test]
    fn the_chip_wraps_in_the_column() {
        let (one, h1) = detail_lines("E_FORBIDDEN", 600.0);
        assert_eq!(one.len(), 1);
        assert!((h1 - (line_height(text::XS, 1.5) + 2.0)).abs() < 1e-4);
        let (many, _) = detail_lines(t("privacy.unlistedMessage"), 300.0);
        assert!(many.len() > 1);
    }
}
