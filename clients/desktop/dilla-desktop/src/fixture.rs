//! A screen fixture as what one signed-in client holds: the state the screens draw, built from the same JSON the web
//! shooter seeds a real dillad with (`dilla_client::testing::spec`, Task 5). The automation build draws it without a
//! network, at the fixture's window size and frozen clock, in UTC (the references are shot with `timezoneId: 'UTC'`).
//!
//! Nothing here is a screen's business: UI code never carries fixture content. A screen reads [`dilla_client::state`]
//! like it does live.
//!
//! The world is built in one place. The per-area functions at the end are where each Wave 1 area maps its own facet of
//! the spec into its own state slice; each is an empty stub until its area's slice exists, and each task edits only its
//! own function (the controller resolves the trivial merge).

use std::collections::HashMap;
use std::path::Path;

use dilla_client::state::MessageId;
use dilla_client::state::{
    AccountPhase, AccountState, AppState, Channel, ChannelGroupState, ChannelId, Community,
    CommunityId, ConnectionState, ConnectionStatus, Member, Timeline, TimelineItem,
    TimelineItemState, UserId,
    messages::{Attachment, Reaction, ReplyRef},
};
use dilla_client::testing::spec::{self, ChannelKind, ScreenFixture, Shot, WorldSpec};

use crate::router::Route;

/// A loaded fixture: the state to draw, the route it opens and the clock it is drawn at.
#[derive(Clone, Debug)]
pub struct Fixture {
    pub state: AppState,
    /// The route the fixture opens (`router.ts` path, resolved).
    pub route: Route,
    /// The text channel the route shows, when it shows one.
    pub open: Option<ChannelId>,
    /// The frozen "now" (the fixture's clock, plus the shot's shift).
    pub now: u64,
    /// The window, logical px.
    pub window: (f32, f32),
    /// The fixture file as the shooters read it.
    pub spec: ScreenFixture,
    /// SHA-256 of the file's bytes.
    pub digest: String,
}

/// The ids of the world: derived from the local names, or, for a world a real dillad made, the ids the shooter recorded
/// in the reference's sidecar (the web client sorts equal positions by id, so a screen only matches when it holds them).
#[derive(Clone, Copy, Default)]
pub struct Ids<'a> {
    shot: Option<&'a Shot>,
}

impl Ids<'_> {
    fn of(&self, kind: &str, local: &str) -> [u8; 16] {
        let recorded = self
            .shot
            .and_then(|s| s.ids.get(&format!("{kind}:{local}")))
            .and_then(|hex| spec::unhex16(hex));
        recorded.unwrap_or_else(|| spec::derive_id(kind, local))
    }

    fn user(&self, id: &str) -> UserId {
        UserId(self.of("person", id))
    }

    fn channel(&self, path: &str) -> ChannelId {
        ChannelId(self.of("channel", path))
    }

    fn community(&self, id: &str) -> CommunityId {
        CommunityId(self.of("server", id))
    }

    fn message(&self, key: &str) -> MessageId {
        MessageId(self.of("message", key))
    }
}

impl Fixture {
    /// Loads `path`; `shot` is the reference's sidecar (`<state>.shot.json`), whose shift lifts every time.
    pub fn load(path: &Path, shot: Option<&Path>) -> Result<Self, String> {
        let text = std::fs::read_to_string(path).map_err(|e| format!("{}: {e}", path.display()))?;
        let shot = match shot {
            Some(p) => {
                let raw =
                    std::fs::read_to_string(p).map_err(|e| format!("{}: {e}", p.display()))?;
                Some(
                    serde_json::from_str::<Shot>(&raw)
                        .map_err(|e| format!("{}: {e}", p.display()))?,
                )
            }
            None => None,
        };
        Self::parse(&text, shot.as_ref())
    }

    pub fn parse(text: &str, shot: Option<&Shot>) -> Result<Self, String> {
        let fixture = ScreenFixture::parse(text).map_err(|e| e.to_string())?;
        let shift = shot.map_or(0, |s| s.shift);
        let ids = Ids { shot };
        let route = Route::parse(
            &fixture
                .resolved_route_with(|kind, local| ids.of(kind, local))
                .map_err(|e| e.to_string())?,
        );
        let state = build_state(&fixture.world, &fixture.viewer, shift, ids);
        let open = match &route {
            Route::Channel {
                channel: Some(channel),
                ..
            } => Some(*channel),
            _ => None,
        };
        Ok(Self {
            state,
            route,
            open,
            now: fixture.clock + shift,
            window: (fixture.window.0 as f32, fixture.window.1 as f32),
            digest: spec::digest(text.as_bytes()),
            spec: fixture,
        })
    }

    /// The line the driver's `fixture` command answers and the web shooter's `--check` prints.
    pub fn line(&self) -> String {
        self.spec.fixture_line(&self.digest)
    }
}

/// The state one device of `viewer` holds in `world`, every time lifted by `shift` seconds.
pub fn build_state(world: &WorldSpec, viewer: &str, shift: u64, ids: Ids<'_>) -> AppState {
    let username = world
        .people
        .iter()
        .find(|p| p.id == viewer)
        .map_or(viewer, |p| p.username())
        .to_owned();
    let mut communities = Vec::new();
    let mut channels = Vec::new();
    let mut members = HashMap::new();
    let mut timelines = HashMap::new();
    for server in &world.servers {
        // The viewer sees the servers it belongs to.
        if !server.members.iter().any(|m| m == viewer) {
            continue;
        }
        let id = ids.community(&server.id);
        communities.push(Community {
            id,
            name: server.name.clone(),
            owner: ids.user(&server.owner),
            policy_version: 0,
        });
        members.insert(
            id,
            server
                .members
                .iter()
                .filter_map(|m| world.people.iter().find(|p| p.id == *m))
                .map(|p| Member {
                    user_id: ids.user(&p.id),
                    username: p.username().to_owned(),
                    display: p.display.clone(),
                    kind: u8::from(p.bot),
                    role_ids: server
                        .roles
                        .iter()
                        .filter(|r| r.members.contains(&p.id))
                        .map(|r| ids.of("role", &format!("{}/{}", server.id, r.id)))
                        .collect(),
                    status_msg: p.status.clone(),
                    bio: p.bio.clone(),
                })
                .collect(),
        );
        for (index, c) in server.channels.iter().filter(|c| !c.system).enumerate() {
            channels.push(channel_of(server, c, index as u32, id, ids));
        }
        for c in server.channels.iter().filter(|c| c.system) {
            channels.push(channel_of(server, c, 0, id, ids));
        }
        for c in server
            .channels
            .iter()
            .filter(|c| c.kind == ChannelKind::Text)
        {
            let path = format!("{}/{}", server.id, c.id);
            timelines.insert(
                ids.channel(&path),
                Timeline {
                    channel_id: ids.channel(&path),
                    group: ChannelGroupState::Active,
                    items: items_of(world, &path, viewer, shift, ids),
                    has_earlier: false,
                },
            );
        }
    }
    let mut state = AppState {
        account: AccountState {
            phase: AccountPhase::Ready,
            user_id: Some(ids.user(viewer)),
            device_id: None,
            username,
        },
        connection: ConnectionState {
            status: ConnectionStatus::Online,
            ..ConnectionState::default()
        },
        communities,
        channels,
        timelines,
        members,
        unread: HashMap::new(),
        ..AppState::default()
    };
    apply_reactions(world, viewer, &mut state);
    apply_threads(world, viewer, &mut state);
    apply_attachments(world, viewer, &mut state);
    apply_dms(world, viewer, &mut state);
    apply_roles(world, viewer, &mut state);
    apply_calls(world, viewer, &mut state);
    apply_polls(world, viewer, &mut state);
    state
}

fn channel_of(
    server: &spec::ServerSpec,
    c: &spec::ChannelSpec,
    _index: u32,
    community: CommunityId,
    ids: Ids<'_>,
) -> Channel {
    let path = format!("{}/{}", server.id, c.id);
    Channel {
        id: ids.channel(&path),
        community_id: community,
        kind: c.kind.wire(),
        mode: 0,
        name: c.name.clone(),
        topic: c.topic.clone(),
        parent_id: c
            .parent
            .as_ref()
            .map(|p| ids.channel(&format!("{}/{p}", server.id))),
        position: c.position.unwrap_or(0),
        slowmode: c.slowmode,
        text_group_id: None,
        group: if c.kind == ChannelKind::Text {
            ChannelGroupState::Active
        } else {
            ChannelGroupState::Unsupported
        },
    }
}

/// The timeline of the channel at `path` (`<server>/<channel>`), or of the dm `dm/<id>`.
fn items_of(
    world: &WorldSpec,
    path: &str,
    viewer: &str,
    shift: u64,
    ids: Ids<'_>,
) -> Vec<TimelineItem> {
    let messages: Vec<&spec::MessageSpec> = world
        .messages
        .iter()
        .filter(|m| m.channel == path)
        .collect();
    messages
        .iter()
        .map(|m| {
            let reply = m.reply_to.as_ref().and_then(|target| {
                let original = messages.iter().find(|o| &o.key == target)?;
                Some(ReplyRef {
                    msg_id: ids.message(&original.key),
                    state: 0,
                    sender_user: Some(ids.user(&original.sender)),
                    excerpt: original.body.clone(),
                    seq: None,
                })
            });
            TimelineItem {
                key: m.key.clone(),
                state: TimelineItemState::Ok,
                reason: String::new(),
                sender_user: Some(ids.user(&m.sender)),
                sender_device: None,
                own: m.sender == viewer,
                bot: world.people.iter().any(|p| p.id == m.sender && p.bot),
                ts: m.ts + shift,
                body: m.body.clone(),
                msg_id: Some(ids.message(&m.key)),
                seq: None,
                edited: m.edited,
                reply,
                reactions: m
                    .reactions
                    .iter()
                    .map(|r| Reaction {
                        emoji: r.emoji.clone(),
                        count: r.by.len() as u32,
                        mine: r.by.iter().any(|b| b == viewer),
                    })
                    .collect(),
                pinned: m.pinned,
                attachments: m
                    .attachments
                    .iter()
                    .enumerate()
                    .map(|(index, a)| Attachment {
                        index: index as u32,
                        size: a.size,
                        mime: a.mime.clone(),
                        name: a.name.clone(),
                    })
                    .collect(),
                mention: false,
            }
        })
        .collect()
}

// ---- Per-area facets -------------------------------------------------------------------------------------------
// One function per Wave 1 area, called by `build_state`. Each stays empty until its area's state slice has fields for
// the facet; its task fills only its own function.

/// Task 10: `AppState::reactions` (who reacted, picker state). The counts on timeline items are already set.
fn apply_reactions(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}

/// Task 11: `AppState::threads`, from `MessageSpec::thread_of`.
fn apply_threads(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}

/// Task 12: `AppState::attachments` (downloads, thumbnails). The items' attachment rows are already set.
fn apply_attachments(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}

/// Task 13: `AppState::dms` and `AppState::trust`, from `WorldSpec::dms` and the messages in `dm/<id>`.
fn apply_dms(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}

/// Task 14: `AppState::roles`, from `ServerSpec::roles`.
fn apply_roles(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}

/// Tasks 17 and 18: `AppState::voice` and `AppState::ring`, from `WorldSpec::calls`.
fn apply_calls(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}

/// Task 20: `AppState::bot` (polls), from `MessageSpec::poll`.
fn apply_polls(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}

#[cfg(test)]
mod tests {
    use super::*;

    fn committed(name: &str) -> Fixture {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../parity/fixtures")
            .join(name);
        Fixture::load(&path, None).unwrap()
    }

    #[test]
    fn the_committed_channel_view_loads() {
        let f = committed("shell/channel-view.json");
        assert_eq!(f.state.communities[0].name, "Midgard Crew");
        assert_eq!(f.window, (1280.0, 800.0));
        let open = f.open.expect("the route opens a channel");
        assert_eq!(f.state.timelines[&open].items.len(), 12);
        assert!(f.state.channel(open).is_some_and(|c| c.name == "general"));
        assert!(f.line().starts_with("fixture shell/channel-view "));
    }

    #[test]
    fn a_shot_lifts_every_time() {
        let path = Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../parity/fixtures/shell/channel-view.json");
        let text = std::fs::read_to_string(path).unwrap();
        let plain = Fixture::parse(&text, None).unwrap();
        let shot = Shot {
            digest: plain.digest.clone(),
            shift: 86_400,
            ids: Default::default(),
        };
        let lifted = Fixture::parse(&text, Some(&shot)).unwrap();
        assert_eq!(lifted.now, plain.now + 86_400);
        let open = plain.open.unwrap();
        assert_eq!(
            lifted.state.timelines[&open].items[0].ts,
            plain.state.timelines[&open].items[0].ts + 86_400
        );
    }
}
