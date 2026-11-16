//! `members:<community>` (types.ts `MemberSummary`), read as routes.ts `listMembers` reads `GET
//! /v1/communities/{id}/members?after=` in pages of [`MEMBERS_PAGE`].

use crate::sync::value::{DecodeError, Rows, at_least, bin16, text, u53};

use std::collections::HashMap;

use super::{CommunityId, UserId};

/// routes.ts `MEMBERS_PAGE`.
pub const MEMBERS_PAGE: usize = 200;

/// `MemberSummary`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Member {
    pub user_id: UserId,
    pub username: String,
    /// The account's display name (controller.ts `members:` rows: `display: m.display`). The member row's server
    /// nickname is not shown: the web never reads it, and the managed Bot's nick stays the "Dilla Bot" it was enrolled
    /// with while its name (`PATCH /bot/profile`) is the account's display.
    pub display: String,
    /// 0 a person, 1 a Bot.
    pub kind: u8,
    pub role_ids: Vec<[u8; 16]>,
    pub status_msg: String,
    pub bio: String,
}

/// routes.ts `memberRow`: `[user_id, joined, nick, role_ids|null, username, display, kind, status_msg?, bio?]`.
pub(crate) fn decode_members(raw: &[u8]) -> Result<Vec<Member>, DecodeError> {
    Rows::of(raw)?
        .iter()
        .map(|row| {
            let a = at_least(row, 7)?;
            text(&a[2])?; // the nickname: checked, not shown (the web's `members:` rows carry `display` only)
            let role_ids = match &a[3] {
                crate::gateway::frames::Value::Null => Vec::new(),
                v => v
                    .as_array()
                    .ok_or(DecodeError("role ids"))?
                    .iter()
                    .map(bin16)
                    .collect::<Result<_, _>>()?,
            };
            let display = text(&a[5])?;
            let kind = match u53(&a[6])? {
                0 => 0,
                1 => 1,
                _ => return Err(DecodeError("member kind")),
            };
            Ok(Member {
                user_id: UserId(bin16(&a[0])?),
                username: text(&a[4])?,
                display,
                kind,
                role_ids,
                status_msg: a.get(7).map(text).transpose()?.unwrap_or_default(),
                bio: a.get(8).map(text).transpose()?.unwrap_or_default(),
            })
        })
        .collect()
}

/// types.ts `PresenceStatus`: the wire's 0 offline, 1 online, 2 idle, 3 dnd.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash)]
pub enum Presence {
    #[default]
    Offline,
    Online,
    Idle,
    Dnd,
}

impl Presence {
    /// The wire's status number, or `None` outside 0..=3.
    pub fn of_wire(n: u64) -> Option<Self> {
        Some(match n {
            0 => Self::Offline,
            1 => Self::Online,
            2 => Self::Idle,
            3 => Self::Dnd,
            _ => return None,
        })
    }
}

/// types.ts `BanSummary`: one ban of a community, as `GET .../bans` lists it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BanSummary {
    pub user_id: UserId,
    /// The banned account's display name, else its username ('' from an older instance).
    pub name: String,
    pub reason: String,
    pub by_user: UserId,
    /// Unix seconds.
    pub created: u64,
    /// Unix seconds; `None` is for ever.
    pub expires: Option<u64>,
}

/// Task 14's slice of [`super::AppState`]: presence, bans and timeouts (the member lists stay in `AppState::members`, the
/// sync engine's). [`Default`] is the empty slice.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct PeopleState {
    /// Each community's presence snapshot, kept current by op 48. Absent: not read yet (the pane says loading).
    pub presence: HashMap<CommunityId, HashMap<UserId, Presence>>,
    /// Each community's bans, once read (the Bans section reads them).
    pub bans: HashMap<CommunityId, Vec<BanSummary>>,
    /// The timeouts this device has read or set, by community and member: the expiry in Unix seconds.
    pub timeouts: HashMap<CommunityId, HashMap<UserId, u64>>,
}

impl PeopleState {
    /// The presence of `user` in `community`: offline when unknown.
    pub fn presence_of(&self, community: CommunityId, user: UserId) -> Presence {
        self.presence
            .get(&community)
            .and_then(|m| m.get(&user))
            .copied()
            .unwrap_or_default()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::gateway::frames::Value;

    fn row(nick: &str, username: &str, display: &str, kind: u64) -> Value {
        Value::Array(vec![
            Value::Bytes(vec![7; 16]),
            Value::Uint(1),
            Value::Text(nick.into()),
            Value::Null,
            Value::Text(username.into()),
            Value::Text(display.into()),
            Value::Uint(kind),
        ])
    }

    /// The community renamed its Bot to "Mr. Kim" (`PATCH /bot/profile` sets the account's display); the member row's
    /// nick is still the enrolled "Dilla Bot". The web shows "Mr. Kim", so the desktop does too.
    #[test]
    fn a_renamed_bot_shows_its_profile_name_not_the_enrolled_nick() {
        let raw = Value::Array(vec![row("Dilla Bot", "bot-abc", "Mr. Kim", 1)]).encode();
        let members = decode_members(&raw).unwrap();
        assert_eq!(members[0].display, "Mr. Kim");
        assert_eq!(members[0].kind, 1);
    }

    /// A person's row: the account's display, whatever the nick holds.
    #[test]
    fn a_member_shows_the_account_display_name() {
        let raw = Value::Array(vec![row("nick", "ada", "Ada", 0), row("", "bo", "", 0)]).encode();
        let members = decode_members(&raw).unwrap();
        assert_eq!(members[0].display, "Ada");
        assert_eq!(members[1].display, "");
        assert_eq!(members[1].username, "bo");
    }
}
