//! `members:<community>` (types.ts `MemberSummary`), read as routes.ts `listMembers` reads `GET
//! /v1/communities/{id}/members?after=` in pages of [`MEMBERS_PAGE`].

use crate::sync::value::{DecodeError, Rows, at_least, bin16, text, u53};

use super::UserId;

/// routes.ts `MEMBERS_PAGE`.
pub const MEMBERS_PAGE: usize = 200;

/// `MemberSummary`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Member {
    pub user_id: UserId,
    pub username: String,
    /// The server nickname when set, else the account's display name.
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
            let nick = text(&a[2])?;
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
                display: if nick.is_empty() { display } else { nick },
                kind,
                role_ids,
                status_msg: a.get(7).map(text).transpose()?.unwrap_or_default(),
                bio: a.get(8).map(text).transpose()?.unwrap_or_default(),
            })
        })
        .collect()
}
