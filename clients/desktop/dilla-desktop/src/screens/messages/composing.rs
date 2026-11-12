//! The composer's rules, apart from its drawing (packages/web `conversation/ComposerArea.tsx` and packages/ui
//! `Composer`): what a send carries, the `</>` fence, the per-channel draft, who may post where, and slow mode's wait.

use dilla_client::state::{AppState, Channel, ChannelId, TimelineItem, TimelineItemState};

use super::slow;

/// The composer's budget, in UTF-8 bytes of the text as it is sent.
pub const MAX_LEN: usize = 4000;

/// `maxLength / 10`: the counter shows when this much is left.
pub fn counter_visible(remaining: i64) -> bool {
    remaining <= (MAX_LEN / 10) as i64
}

/// The `</>` button's insert: the draft (ending in one line break) and a yaml fence, the caret (in characters) on the
/// empty line inside (`insertCodeFence`).
pub fn insert_code_fence(draft: &str) -> (String, usize) {
    let fence = "```";
    let mut text = if draft.is_empty() {
        String::new()
    } else {
        let mut d = draft.to_owned();
        if !d.ends_with('\n') {
            d.push('\n');
        }
        d
    };
    text.push_str(fence);
    text.push_str("yaml\n\n");
    text.push_str(fence);
    let caret = text.chars().count() - 4;
    (text, caret)
}

/// What a send posts: `//` at the start posts the rest with one leading slash (the prototype's `send()`:
/// `//shrug` posts `/shrug`).
pub fn outgoing(raw: &str) -> String {
    raw.strip_prefix("//")
        .map_or_else(|| raw.to_owned(), |rest| format!("/{rest}"))
}

/// The preference key a channel's draft is kept under (`drafts.*` is this area's prefix).
pub fn draft_key(channel: ChannelId) -> String {
    format!("drafts.{}", channel.hex())
}

/// Why the composer cannot send, if it cannot.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Blocked {
    /// An announcement channel the viewer may not post in: `announce.readOnly`.
    ReadOnly,
}

/// Whether the viewer may post in `channel`. Announcement channels (mode 1) take posts only from the roles the server
/// names; until the roles slice (Task 14) says which, only the server's owner is known to post there, so every other
/// viewer gets the read-only line (the instance decides in the end). Every other channel is open.
pub fn may_send(state: &AppState, channel: &Channel) -> Result<(), Blocked> {
    if channel.mode != 1 {
        return Ok(());
    }
    let owner = state
        .communities
        .iter()
        .find(|c| c.id == channel.community_id)
        .map(|c| c.owner);
    if owner.is_some() && owner == state.account.user_id {
        Ok(())
    } else {
        Err(Blocked::ReadOnly)
    }
}

/// The newest own post that the instance holds (what Up edits): readable, stored, with its id.
pub fn edit_target(items: &[TimelineItem]) -> Option<&TimelineItem> {
    items.iter().rev().find(|i| {
        i.own && i.state == TimelineItemState::Ok && i.msg_id.is_some() && i.seq.is_some()
    })
}

/// Slow mode at `now_ms`: when the next post may go (0: now), and whether a post of this device waits for it.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Default)]
pub struct Slow {
    pub until_ms: u64,
    pub queued: bool,
}

/// The wait on `channel`, from the timeline: the channel's slow-mode seconds after the newest own stored post. A post
/// of this device still pending in the outbox is the queued one.
pub fn slow_state(channel: &Channel, items: &[TimelineItem], bypass: bool, now_ms: u64) -> Slow {
    let last = items
        .iter()
        .rev()
        .find(|i| i.own && i.state == TimelineItemState::Ok && i.seq.is_some())
        .map(|i| i.ts);
    let until_ms = slow::wait_until_ms(channel.slowmode, last, bypass, now_ms);
    Slow {
        until_ms,
        queued: until_ms > now_ms
            && items
                .iter()
                .any(|i| i.own && i.state == TimelineItemState::Pending),
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::{
        ChannelGroupState, Community, CommunityId, DeviceId, MessageId, UserId,
    };

    fn channel(mode: u8, slowmode: u32) -> Channel {
        Channel {
            id: ChannelId([1; 16]),
            community_id: CommunityId([2; 16]),
            kind: 0,
            mode,
            name: "news".into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode,
            text_group_id: None,
            group: ChannelGroupState::Active,
        }
    }

    fn item(own: bool, state: TimelineItemState, ts: u64) -> TimelineItem {
        TimelineItem {
            key: format!("s{ts}"),
            state,
            reason: String::new(),
            sender_user: Some(UserId([9; 16])),
            sender_device: Some(DeviceId([9; 16])),
            own,
            bot: false,
            ts,
            body: "x".into(),
            msg_id: Some(MessageId([ts as u8; 16])),
            seq: (state != TimelineItemState::Pending).then_some(ts),
            edited: false,
            reply: None,
            reactions: Vec::new(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
        }
    }

    #[test]
    fn the_code_button_appends_a_yaml_fence_with_the_caret_inside() {
        let (text, caret) = insert_code_fence("");
        assert_eq!(text, "```yaml\n\n```");
        assert_eq!(caret, 8, "on the empty line");
        let (text, _) = insert_code_fence("look");
        assert_eq!(text, "look\n```yaml\n\n```");
        let (text, _) = insert_code_fence("look\n");
        assert_eq!(text, "look\n```yaml\n\n```");
        let (text, caret) = insert_code_fence("a");
        assert_eq!(text.chars().nth(caret), Some('\n'));
    }

    #[test]
    fn a_double_slash_posts_one() {
        assert_eq!(outgoing("//shrug"), "/shrug");
        assert_eq!(outgoing("/shrug"), "/shrug");
        assert_eq!(outgoing("a//b"), "a//b");
        assert_eq!(outgoing("//"), "/");
    }

    #[test]
    fn a_draft_belongs_to_one_channel_under_the_drafts_prefix() {
        let k = draft_key(ChannelId([0xab; 16]));
        assert_eq!(k, format!("drafts.{}", "ab".repeat(16)));
        assert!(!dilla_client::prefs::names_a_secret(&k));
    }

    #[test]
    fn only_the_owner_posts_in_an_announcement_channel_for_now() {
        let mut s = AppState::default();
        s.communities.push(Community {
            id: CommunityId([2; 16]),
            name: "S".into(),
            owner: UserId([7; 16]),
            policy_version: 1,
        });
        s.account.user_id = Some(UserId([8; 16]));
        assert_eq!(may_send(&s, &channel(1, 0)), Err(Blocked::ReadOnly));
        assert_eq!(may_send(&s, &channel(0, 0)), Ok(()));
        s.account.user_id = Some(UserId([7; 16]));
        assert_eq!(may_send(&s, &channel(1, 0)), Ok(()));
    }

    #[test]
    fn up_edits_the_newest_readable_stored_own_post() {
        let items = [
            item(true, TimelineItemState::Ok, 1),
            item(false, TimelineItemState::Ok, 2),
            item(true, TimelineItemState::Pending, 3),
            item(true, TimelineItemState::Deleted, 4),
        ];
        assert_eq!(edit_target(&items).map(|i| i.ts), Some(1));
        assert!(edit_target(&items[1..]).is_none());
    }

    #[test]
    fn slow_mode_waits_after_the_own_post_and_queues_a_pending_one() {
        let now = 1_000 * 1000;
        let ch = channel(0, 30);
        let items = [item(true, TimelineItemState::Ok, 990)];
        let s = slow_state(&ch, &items, false, now);
        assert_eq!(s.until_ms, 1_020_000);
        assert!(!s.queued);
        let with_pending = [
            items[0].clone(),
            item(true, TimelineItemState::Pending, 995),
        ];
        assert!(slow_state(&ch, &with_pending, false, now).queued);
        assert_eq!(slow_state(&ch, &items, true, now), Slow::default());
        assert_eq!(
            slow_state(&channel(0, 0), &items, false, now),
            Slow::default()
        );
        // Over: no wait, so nothing is queued either.
        assert_eq!(slow_state(&ch, &items, false, 2_000_000), Slow::default());
    }

    #[test]
    fn the_counter_shows_within_a_tenth_of_the_budget() {
        assert!(!counter_visible(401));
        assert!(counter_visible(400));
        assert!(counter_visible(-1));
    }
}
