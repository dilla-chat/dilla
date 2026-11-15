//! A row's send and read state under its body (MessageItem.tsx, MessageRow.tsx `detail` and `actions`): a failed send
//! shows its reason (`send.tooLarge` for `E_TOO_LARGE`, the code otherwise) and offers **retry** and **discard**; a row
//! that cannot be read says why when the reason is a privacy verdict (`privacy.changed`, `privacy.unlistedMessage`,
//! `privacy.revokedMessage` for `E_DEVICE_REVOKED`).
//!
//! MessageRow.css: `.d-message-row__detail` is a chip (`--text-xs`, `--fg-3`, `0 0.25rem` padding, a 1px `--hairline-2`
//! border, `--r-md`) at the start of the column; `.d-message-row__actions` is a row of ghost buttons 0.5rem apart, 0.25rem
//! under the item above it. Each is a row of the message's grid, 1px under the item above it.

use dilla_client::state::{TimelineItem, TimelineItemState};
use gpui::{
    App, AppContext as _, InteractiveElement as _, IntoElement, ParentElement, SharedString,
    Styled, Window, div, px,
};

use dilla_desktop_ui::components::button::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::text::{line_height, paragraph};
use dilla_desktop_ui::tokens::{color, font, radius, text};

use crate::slots::{EXTRA_GAP, Extra, SlotCx};
use crate::strings::t;

/// `.d-message-row__detail`'s horizontal padding (0.25rem) and border (1px).
const DETAIL_PAD_X: f32 = 4.0;
const DETAIL_BORDER: f32 = 1.0;
/// `.d-message-row__actions`: 0.25rem above, 0.5rem between the buttons; a `Button` is 2rem tall.
const ACTIONS_TOP: f32 = 4.0;
const ACTIONS_GAP: f32 = 8.0;
const BUTTON_HEIGHT: f32 = 32.0;

/// The detail line of `item` (MessageItem.tsx `detail`): a failed send's reason, or a cannot-read row's privacy verdict.
pub fn detail(item: &TimelineItem) -> Option<String> {
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
    item.state == TimelineItemState::Failed && item.msg_id.is_some()
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
    let detail = detail(item);
    let retry = offers_retry(item);
    if detail.is_none() && !retry {
        return None;
    }
    let column = crate::screens::reactions::column_width(window, cx);
    let mut height = 0.0;
    let mut col = div().flex().flex_col().items_start();
    if let Some(detail) = detail {
        let (lines, h) = detail_lines(&detail, column);
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
        let client = ctx.client.clone();
        let again = client.clone();
        col = col.child(
            div()
                .flex()
                .gap(px(ACTIONS_GAP))
                .pt(px(ACTIONS_TOP))
                .child(
                    Button::new(
                        SharedString::from(format!("msg-retry-{}", item.key)),
                        t("shell.message.retry"),
                    )
                    .variant(ButtonVariant::Ghost)
                    .size(ButtonSize::Md)
                    .on_click(move |_, _, cx| {
                        if let Some(c) = &client {
                            let done = c.messages().retry(msg);
                            cx.background_spawn(async move {
                                let _ = done.await;
                            })
                            .detach();
                        }
                    }),
                )
                .child(
                    Button::new(
                        SharedString::from(format!("msg-discard-{}", item.key)),
                        t("shell.message.discard"),
                    )
                    .variant(ButtonVariant::Ghost)
                    .size(ButtonSize::Md)
                    .on_click(move |_, _, cx| {
                        if let Some(c) = &again {
                            let done = c.messages().discard(msg);
                            cx.background_spawn(async move {
                                let _ = done.await;
                            })
                            .detach();
                        }
                    }),
                ),
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
    use dilla_client::state::MessageId;

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
