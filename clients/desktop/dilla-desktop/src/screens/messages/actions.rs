//! This area's entries of the message toolbar and context menu (`slots::message_actions`): edit, pin or unpin, delete.
//! Edit and delete are the own message's; pin is offered to a reader whose channel bits (the one resolver,
//! `roles::viewer_channel_bits`) hold `pin_messages`, or while the bits are not known yet (MessageItem.tsx: `p.bits ==
//! null || bits & PERM.pinMessages`).

use std::rc::Rc;

use dilla_client::Area;
use dilla_client::state::{TimelineItem, TimelineItemState};
use gpui::{AppContext as _, SharedString};

use dilla_desktop_ui::IconName;

use super::ui;
use crate::slots::MessageAction;

/// The `pin_messages` bit of a channel's permission bits (internal/server/permissions.go, `roles::perm::PIN_MESSAGES`).
const PIN_MESSAGES: u64 = dilla_client::features::roles::perm::PIN_MESSAGES as u64;

/// The actions of `item` for a reader with the channel's permission `bits` (`None`: not resolved yet).
pub fn message_actions(item: &TimelineItem, bits: Option<u64>) -> Vec<MessageAction> {
    let (Some(msg), Some(_seq)) = (item.msg_id, item.seq) else {
        return Vec::new();
    };
    if item.state != TimelineItemState::Ok {
        return Vec::new();
    }
    let mut out = Vec::new();
    if item.own {
        let item = item.clone();
        out.push(MessageAction {
            id: SharedString::from("edit"),
            label: "shell.message.edit",
            icon: IconName::Pencil,
            area: Area::Messages,
            run: Rc::new(move |ctx, window, cx| {
                if let Some(h) = ui::handle(window, cx) {
                    h.update(cx, |ui, cx| ui.begin_edit(ctx, &item, window, cx));
                }
            }),
        });
    }
    let pinned = item.pinned;
    // Pin and unpin need the channel's pin_messages bit (Task 28: hidden from a reader without it, as the web does). A
    // delete of another person's message is the web's moderated removal (manage_messages, a reason and a proof request);
    // the desktop client has no such route yet, so it is not offered.
    let may_pin = bits.is_none_or(|b| b & PIN_MESSAGES != 0);
    if may_pin {
        out.push(MessageAction {
            id: SharedString::from("pin"),
            label: if pinned {
                "shell.message.unpin"
            } else {
                "shell.message.pin"
            },
            icon: IconName::Pin,
            area: Area::Messages,
            run: Rc::new(move |ctx, _window, cx| {
                if let (Some(client), Some(channel)) = (&ctx.client, ctx.channel) {
                    let done = client.messages().pin(channel, msg, !pinned);
                    cx.background_spawn(async move {
                        let _ = done.await;
                    })
                    .detach();
                }
            }),
        });
    }
    if item.own {
        out.push(MessageAction {
            id: SharedString::from("delete"),
            label: "shell.message.delete",
            icon: IconName::Trash2,
            area: Area::Messages,
            run: Rc::new(move |ctx, window, cx| {
                if let (Some(channel), Some(h)) = (ctx.channel, ui::handle(window, cx)) {
                    h.update(cx, |ui, cx| ui.ask_delete(channel, msg, cx));
                }
            }),
        });
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::{DeviceId, MessageId, UserId};

    fn item(own: bool, pinned: bool, state: TimelineItemState) -> TimelineItem {
        TimelineItem {
            key: "k".into(),
            state,
            reason: String::new(),
            sender_user: Some(UserId([1; 16])),
            sender_device: Some(DeviceId([1; 16])),
            own,
            bot: false,
            ts: 1,
            body: "hi".into(),
            msg_id: Some(MessageId([2; 16])),
            seq: Some(1),
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned,
            attachments: Vec::new(),
            mention: false,
        }
    }

    fn ids(item: &TimelineItem) -> Vec<String> {
        message_actions(item, None)
            .iter()
            .map(|a| a.id.to_string())
            .collect()
    }

    #[test]
    fn an_own_message_can_be_edited_pinned_and_deleted() {
        assert_eq!(
            ids(&item(true, false, TimelineItemState::Ok)),
            ["edit", "pin", "delete"]
        );
    }

    #[test]
    fn someone_elses_message_can_only_be_pinned() {
        assert_eq!(ids(&item(false, false, TimelineItemState::Ok)), ["pin"]);
    }

    #[test]
    fn a_pinned_message_offers_unpin() {
        let a = message_actions(&item(false, true, TimelineItemState::Ok), None);
        assert_eq!(a[0].label, "shell.message.unpin");
    }

    #[test]
    fn a_message_that_is_not_readable_or_not_stored_has_no_actions() {
        assert!(ids(&item(true, false, TimelineItemState::Pending)).is_empty());
        assert!(ids(&item(true, false, TimelineItemState::Deleted)).is_empty());
        let mut unstored = item(true, false, TimelineItemState::Ok);
        unstored.seq = None;
        assert!(message_actions(&unstored, None).is_empty());
    }

    /// Task 28 (docs lane, T14's deferral): pin and unpin follow the reader's pin_messages bit; unknown bits (a slice still
    /// loading) offer it, as the web does.
    #[test]
    fn pin_is_offered_only_with_the_pin_messages_bit() {
        let pick = |own: bool, bits: Option<u64>| -> Vec<String> {
            message_actions(&item(own, false, TimelineItemState::Ok), bits)
                .iter()
                .map(|a| a.id.to_string())
                .collect()
        };
        let read_send = 1 | (1 << 1);
        assert_eq!(pick(false, Some(read_send)), Vec::<String>::new());
        assert_eq!(pick(true, Some(read_send)), ["edit", "delete"]);
        assert_eq!(pick(false, Some(read_send | PIN_MESSAGES)), ["pin"]);
        assert_eq!(pick(false, None), ["pin"]);
    }
}
