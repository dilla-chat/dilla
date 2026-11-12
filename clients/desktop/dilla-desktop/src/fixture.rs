//! A channel-view fixture: what one signed-in client holds (its community, channels, members and one timeline), read
//! from the JSON `examples/parity_world.rs` writes, so the automation build draws the same screen as the web reference
//! without a network. Times are drawn in UTC (the reference is shot with `timezoneId: 'UTC'`) against the fixture's own
//! `now`.

use std::collections::HashMap;
use std::path::Path;

use dilla_client::state::{
    AccountPhase, AccountState, AppState, Channel, ChannelGroupState, ChannelId, Community,
    CommunityId, ConnectionState, ConnectionStatus, Member, Timeline, TimelineItem,
    TimelineItemState, UserId,
};
use serde::Deserialize;

#[derive(Deserialize)]
struct Raw {
    now: u64,
    /// The window, logical px (the reference's viewport); 1280x800 when absent.
    #[serde(default)]
    window: Option<(f32, f32)>,
    user: RawUser,
    communities: Vec<RawCommunity>,
    channels: Vec<RawChannel>,
    /// Per community (hex id).
    members: HashMap<String, Vec<RawMember>>,
    open: String,
    messages: Vec<RawMessage>,
}

#[derive(Deserialize)]
struct RawUser {
    id: String,
    username: String,
}

#[derive(Deserialize)]
#[allow(dead_code)]
struct RawCommunity {
    id: String,
    name: String,
    owner: String,
}

#[derive(Deserialize)]
struct RawChannel {
    id: String,
    community: String,
    kind: u8,
    mode: u8,
    name: String,
    topic: String,
    parent: Option<String>,
    position: u32,
}

#[derive(Deserialize)]
struct RawMember {
    id: String,
    username: String,
    display: String,
    kind: u8,
}

#[derive(Deserialize)]
struct RawMessage {
    key: String,
    sender: String,
    ts: u64,
    own: bool,
    body: String,
}

/// A loaded fixture: the state to draw, the channel it shows and the clock it is drawn at.
#[derive(Clone, Debug)]
pub struct Fixture {
    pub state: AppState,
    pub open: ChannelId,
    pub now: u64,
    /// The window, logical px.
    pub window: (f32, f32),
}

fn id16(hex: &str) -> Result<[u8; 16], String> {
    if hex.len() != 32 {
        return Err(format!("not a 16-byte id: {hex}"));
    }
    let mut out = [0u8; 16];
    for (i, byte) in out.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&hex[i * 2..i * 2 + 2], 16)
            .map_err(|_| format!("not hex: {hex}"))?;
    }
    Ok(out)
}

impl Fixture {
    pub fn load(path: &Path) -> Result<Self, String> {
        let text = std::fs::read_to_string(path).map_err(|e| e.to_string())?;
        Self::parse(&text)
    }

    pub fn parse(text: &str) -> Result<Self, String> {
        let raw: Raw = serde_json::from_str(text).map_err(|e| e.to_string())?;
        let open = ChannelId(id16(&raw.open)?);
        let user = UserId(id16(&raw.user.id)?);
        let channels = raw
            .channels
            .iter()
            .map(|c| {
                Ok(Channel {
                    id: ChannelId(id16(&c.id)?),
                    community_id: CommunityId(id16(&c.community)?),
                    kind: c.kind,
                    mode: c.mode,
                    name: c.name.clone(),
                    topic: c.topic.clone(),
                    parent_id: c.parent.as_deref().map(id16).transpose()?.map(ChannelId),
                    position: c.position,
                    slowmode: 0,
                    text_group_id: None,
                    group: if c.kind == 0 {
                        ChannelGroupState::Active
                    } else {
                        ChannelGroupState::Unsupported
                    },
                })
            })
            .collect::<Result<Vec<_>, String>>()?;
        let mut members = HashMap::new();
        for (community, rows) in &raw.members {
            let rows = rows
                .iter()
                .map(|m| {
                    Ok(Member {
                        user_id: UserId(id16(&m.id)?),
                        username: m.username.clone(),
                        display: m.display.clone(),
                        kind: m.kind,
                        role_ids: Vec::new(),
                        status_msg: String::new(),
                        bio: String::new(),
                    })
                })
                .collect::<Result<Vec<_>, String>>()?;
            members.insert(CommunityId(id16(community)?), rows);
        }
        let communities = raw
            .communities
            .iter()
            .map(|c| {
                Ok(Community {
                    id: CommunityId(id16(&c.id)?),
                    name: c.name.clone(),
                    owner: UserId(id16(&c.owner)?),
                    policy_version: 0,
                })
            })
            .collect::<Result<Vec<_>, String>>()?;
        let items = raw
            .messages
            .iter()
            .map(|m| {
                Ok(TimelineItem {
                    key: m.key.clone(),
                    state: TimelineItemState::Ok,
                    reason: String::new(),
                    sender_user: Some(UserId(id16(&m.sender)?)),
                    sender_device: None,
                    own: m.own,
                    bot: false,
                    ts: m.ts,
                    body: m.body.clone(),
                    msg_id: None,
                    seq: None,
                    edited: false,
                    reply: None,
                    reactions: Vec::new(),
                    pinned: false,
                    attachments: Vec::new(),
                    mention: false,
                })
            })
            .collect::<Result<Vec<_>, String>>()?;
        let state = AppState {
            account: AccountState {
                phase: AccountPhase::Ready,
                user_id: Some(user),
                device_id: None,
                username: raw.user.username.clone(),
            },
            connection: ConnectionState {
                status: ConnectionStatus::Online,
                ..ConnectionState::default()
            },
            communities,
            channels,
            timelines: HashMap::from([(
                open,
                Timeline {
                    channel_id: open,
                    group: ChannelGroupState::Active,
                    items,
                    has_earlier: false,
                },
            )]),
            members,
            unread: HashMap::new(),
        };
        Ok(Self {
            state,
            open,
            now: raw.now,
            window: raw.window.unwrap_or((1280.0, 800.0)),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_committed_fixture_loads() {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../parity/references/linux/channel-view.fixture.json");
        let f = Fixture::load(&path).unwrap();
        assert_eq!(f.state.communities[0].name, "Midgard Crew");
        assert_eq!(f.window, (1280.0, 800.0));
        assert_eq!(f.state.timelines[&f.open].items.len(), 12);
        assert!(f.state.channel(f.open).is_some_and(|c| c.name == "general"));
    }
}
