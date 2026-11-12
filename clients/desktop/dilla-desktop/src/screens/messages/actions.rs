//! This area's entries of the message toolbar and context menu (`slots::message_actions`): edit, pin or unpin, delete.
//! Edit and delete are the own message's; pin is offered on every readable message (the instance decides who may pin;
//! the roles slice that would hide it from people without the right is Task 14's).

use std::rc::Rc;

use dilla_client::Area;
use dilla_client::state::{TimelineItem, TimelineItemState};
use gpui::{AppContext as _, SharedString};

use dilla_desktop_ui::IconName;

use super::ui;
use crate::slots::MessageAction;

pub fn message_actions(item: &TimelineItem) -> Vec<MessageAction> {
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
                if let Some(h) = ui::handle(cx) {
                    h.update(cx, |ui, cx| ui.begin_edit(ctx, &item, window, cx));
                }
            }),
        });
    }
    let pinned = item.pinned;
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
    if item.own {
        out.push(MessageAction {
            id: SharedString::from("delete"),
            label: "shell.message.delete",
            icon: IconName::Trash2,
            area: Area::Messages,
            run: Rc::new(move |ctx, _window, cx| {
                if let (Some(channel), Some(h)) = (ctx.channel, ui::handle(cx)) {
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
        message_actions(item)
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
        let a = message_actions(&item(false, true, TimelineItemState::Ok));
        assert_eq!(a[0].label, "shell.message.unpin");
    }

    #[test]
    fn a_message_that_is_not_readable_or_not_stored_has_no_actions() {
        assert!(ids(&item(true, false, TimelineItemState::Pending)).is_empty());
        assert!(ids(&item(true, false, TimelineItemState::Deleted)).is_empty());
        let mut unstored = item(true, false, TimelineItemState::Ok);
        unstored.seq = None;
        assert!(message_actions(&unstored).is_empty());
    }
}
