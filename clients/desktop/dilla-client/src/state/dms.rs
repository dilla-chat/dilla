//! `dms`: Task 13's slice of [`super::AppState`]: the DM list and the open DM.
//!
//! types.ts `DmSummary`, read from `GET /v1/dms` (routes.ts `listDms`) and named from the member lists the way
//! controller.ts `publishDms` names them. A DM's timeline is `AppState::timelines` under its channel id, as a channel's;
//! its badge is `AppState::unread` under the same id. Nothing here holds key material.

use crate::gateway::frames::Value;
use crate::sync::value::{DecodeError, Rows, at_least, bin16, u53};

use super::{ChannelGroupState, ChannelId, UserId};

/// 3: a DM of two people; 4: a group DM.
pub const DM_KIND_PAIR: u8 = 3;
pub const DM_KIND_GROUP: u8 = 4;

/// types.ts `DmSummary`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct DmSummary {
    pub id: ChannelId,
    /// [`DM_KIND_PAIR`] or [`DM_KIND_GROUP`].
    pub kind: u8,
    /// Every member, this account included.
    pub members: Vec<UserId>,
    /// The other members' names joined by ", " (this account's own name in a DM with itself).
    pub name: String,
    pub group: ChannelGroupState,
}

impl DmSummary {
    /// The other member of a two-person DM.
    pub fn peer(&self, own: Option<UserId>) -> Option<UserId> {
        (self.kind == DM_KIND_PAIR)
            .then(|| self.members.iter().copied().find(|m| Some(*m) != own))
            .flatten()
    }
}

/// The slice.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct DmState {
    /// `None` until the first list read answered.
    pub list: Option<Vec<DmSummary>>,
}

impl DmState {
    pub fn get(&self, id: ChannelId) -> Option<&DmSummary> {
        self.list.as_ref()?.iter().find(|d| d.id == id)
    }

    pub fn all(&self) -> &[DmSummary] {
        self.list.as_deref().unwrap_or_default()
    }

    /// The two-person DM with `user`, if any.
    pub fn with(&self, user: UserId, own: Option<UserId>) -> Option<&DmSummary> {
        self.all().iter().find(|d| d.peer(own) == Some(user))
    }
}

/// routes.ts `DmRow`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct DmRow {
    pub channel_id: [u8; 16],
    pub kind: u8,
    pub members: Vec<[u8; 16]>,
}

/// routes.ts `listDms`: `[[channel_id, kind 3|4, members|null, ...]]`.
pub(crate) fn decode_dms(raw: &[u8]) -> Result<Vec<DmRow>, DecodeError> {
    Rows::of(raw)?
        .iter()
        .map(|row| {
            let a = at_least(row, 3)?;
            let kind = u53(&a[1])?;
            if kind != 3 && kind != 4 {
                return Err(DecodeError("dm kind"));
            }
            let members = match &a[2] {
                Value::Null => Vec::new(),
                Value::Array(list) => list.iter().map(bin16).collect::<Result<_, _>>()?,
                _ => return Err(DecodeError("dm members")),
            };
            Ok(DmRow {
                channel_id: bin16(&a[0])?,
                kind: kind as u8,
                members,
            })
        })
        .collect()
}

/// routes.ts `postDm`'s answer: `[channel_id, ...]`.
pub(crate) fn decode_dm_opened(raw: &[u8]) -> Result<[u8; 16], DecodeError> {
    let v = crate::sync::value::decode(raw)?;
    bin16(&at_least(&v, 1)?[0])
}

/// The DM's name (controller.ts `publishDms`): the other members' names joined by ", ", else `own_name` (a DM with
/// oneself).
pub fn dm_name(
    members: &[UserId],
    own: Option<UserId>,
    own_name: &str,
    name_of: impl Fn(UserId) -> String,
) -> String {
    let others: Vec<String> = members
        .iter()
        .filter(|m| Some(**m) != own)
        .map(|m| name_of(*m))
        .collect();
    if others.is_empty() {
        own_name.to_owned()
    } else {
        others.join(", ")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn row(id: u8, kind: u64, members: Option<Vec<u8>>) -> Value {
        Value::Array(vec![
            Value::Bytes(vec![id; 16]),
            Value::Uint(kind),
            members.map_or(Value::Null, |m| {
                Value::Array(m.into_iter().map(|b| Value::Bytes(vec![b; 16])).collect())
            }),
        ])
    }

    #[test]
    fn decodes_the_list_and_refuses_other_kinds() {
        let raw = Value::Array(vec![row(1, 3, Some(vec![7, 8])), row(2, 4, None)]).encode();
        let rows = decode_dms(&raw).unwrap();
        assert_eq!(rows[0].members, vec![[7; 16], [8; 16]]);
        assert_eq!(rows[1].kind, 4);
        assert!(rows[1].members.is_empty());
        let bad = Value::Array(vec![row(1, 0, None)]).encode();
        assert!(decode_dms(&bad).is_err());
    }

    #[test]
    fn names_are_the_others_or_oneself() {
        let me = UserId([1; 16]);
        let name = |u: UserId| format!("u{}", u.0[0]);
        assert_eq!(
            dm_name(
                &[me, UserId([2; 16]), UserId([3; 16])],
                Some(me),
                "me",
                name
            ),
            "u2, u3"
        );
        assert_eq!(dm_name(&[me], Some(me), "me", name), "me");
        let dm = DmSummary {
            id: ChannelId([9; 16]),
            kind: DM_KIND_PAIR,
            members: vec![me, UserId([2; 16])],
            name: "u2".into(),
            group: ChannelGroupState::None,
        };
        assert_eq!(dm.peer(Some(me)), Some(UserId([2; 16])));
    }
}
