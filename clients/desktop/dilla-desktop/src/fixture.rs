//! A screen fixture as what one signed-in client holds: the state the screens draw, built from the same JSON the web
//! shooter seeds a real dillad with (`dilla_client::testing::spec`, Task 5). The automation build draws it without a
//! network, at the fixture's window size and frozen clock, in UTC (the references are shot with `timezoneId: 'UTC'`).
//!
//! Nothing here is a screen's business: UI code never carries fixture content. A screen reads [`dilla_client::state`]
//! like it does live.
//!
//! The world is built in one place. Each Wave 1 area maps its own facet of the spec into its own state slice in its own
//! file, `fixture/<area>.rs` (`pub(super) fn apply`, an empty stub until the area's slice exists). This file only lists
//! the modules and calls them, one per line in sorted order, so no area edits it (ruling R11).
//!
//! The route decides what is drawn ([`Fixture::screen`]): a route or phase the app cannot draw yet is an error, never the
//! shell standing in for it.

use std::collections::{BTreeMap, HashMap};
use std::path::Path;

use dilla_client::state::MessageId;
use dilla_client::state::{
    AccountPhase, AccountState, AppState, Channel, ChannelGroupState, ChannelId, Community,
    CommunityId, ConnectionState, ConnectionStatus, Member, Timeline, TimelineItem,
    TimelineItemState, UserId,
    messages::{Attachment, Reaction, ReplyRef},
};
use dilla_client::testing::spec::{self, ChannelKind, Phase, ScreenFixture, Shot, WorldSpec};

use crate::router::Route;

mod attachments;
mod calls;
mod diagnostics;
mod dms;
mod notify;
mod polls;
mod reactions;
mod roles;
mod threads;

/// What a fixture's route and phase draw.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum FixtureScreen {
    /// The shell, on this channel.
    Shell { channel: ChannelId },
    /// The shell on a server's default channel (`/c/{server}`: the route names no channel, the shell opens the server's
    /// first text channel, as the web client does).
    ShellHome { community: CommunityId },
    /// The welcome and sign-in screen of a device with no account.
    SignIn,
}

/// A loaded fixture: the state to draw, the route it opens and the clock it is drawn at.
#[derive(Clone, Debug)]
pub struct Fixture {
    pub state: AppState,
    /// The route the fixture opens (`router.ts` path, resolved).
    pub route: Route,
    /// What the route draws.
    pub screen: FixtureScreen,
    /// The ids the world uses (derived, or the sidecar's), for the fixture line.
    ids: BTreeMap<String, [u8; 16]>,
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
#[derive(Clone, Copy)]
pub struct Ids<'a> {
    map: &'a BTreeMap<String, [u8; 16]>,
}

impl Ids<'_> {
    fn of(&self, kind: &str, local: &str) -> [u8; 16] {
        *self
            .map
            .get(&format!("{kind}:{local}"))
            .unwrap_or_else(|| panic!("`{kind}:{local}` is not in the validated world"))
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
        let map = fixture.effective_ids(shot).map_err(|e| e.to_string())?;
        let ids = Ids { map: &map };
        let route = Route::parse(
            &fixture
                .resolved_route_with(|kind, local| ids.of(kind, local))
                .map_err(|e| e.to_string())?,
        );
        let screen = screen_of(&route, fixture.phase)?;
        let state = build_state(&fixture, shift, ids, screen);
        Ok(Self {
            state,
            route,
            screen,
            now: fixture.clock + shift,
            window: (fixture.window.0 as f32, fixture.window.1 as f32),
            digest: spec::digest(text.as_bytes()),
            ids: map,
            spec: fixture,
        })
    }

    /// The line the driver's `fixture` command answers and the web shooter's `--check` prints.
    pub fn line(&self) -> String {
        self.spec.fixture_line_with(&self.digest, &self.ids)
    }
}

/// What `route` draws for a device in `phase`, or why the app cannot draw it yet (every other route is a screen of a
/// Wave 1 area that has not landed: the shell is never drawn in its place).
pub fn screen_of(route: &Route, phase: Phase) -> Result<FixtureScreen, String> {
    match (route, phase) {
        (
            Route::Channel {
                channel: Some(channel),
                ..
            },
            Phase::Ready,
        ) => Ok(FixtureScreen::Shell { channel: *channel }),
        (
            Route::Channel {
                community,
                channel: None,
            },
            Phase::Ready,
        ) => Ok(FixtureScreen::ShellHome {
            community: *community,
        }),
        (Route::Welcome { .. }, Phase::SignedOut) => Ok(FixtureScreen::SignIn),
        (route, phase) => Err(format!(
            "the app cannot draw route `{}` in phase {phase:?} yet",
            route.path()
        )),
    }
}

/// The state one device of the fixture's viewer holds in its world, every time lifted by `shift` seconds. A device with
/// no account holds none: the default state, its phase `Loading`.
pub fn build_state(
    fixture: &ScreenFixture,
    shift: u64,
    ids: Ids<'_>,
    screen: FixtureScreen,
) -> AppState {
    if screen == FixtureScreen::SignIn {
        return AppState::default();
    }
    let (world, viewer) = (&fixture.world, fixture.viewer.as_str());
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
        for c in server.channels.iter().filter(|c| !c.system) {
            channels.push(channel_of(server, c, id, ids));
        }
        for c in server.channels.iter().filter(|c| c.system) {
            channels.push(channel_of(server, c, id, ids));
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
    // The servers come in the order the web client lists them: by id.
    communities.sort_by_key(|c| c.id);
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
    attachments::apply(world, viewer, &mut state);
    calls::apply(world, viewer, &mut state);
    diagnostics::apply(world, viewer, &mut state);
    dms::apply(world, viewer, &mut state);
    notify::apply(fixture, ids, &mut state);
    polls::apply(world, viewer, &mut state);
    reactions::apply(world, viewer, &mut state);
    roles::apply(world, viewer, &mut state);
    threads::apply(world, viewer, &mut state);
    state
}

fn channel_of(
    server: &spec::ServerSpec,
    c: &spec::ChannelSpec,
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
        let FixtureScreen::Shell { channel: open } = f.screen else {
            panic!("the route draws the shell");
        };
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
            hand_migrated: false,
            ids: Default::default(),
        };
        let lifted = Fixture::parse(&text, Some(&shot)).unwrap();
        assert_eq!(lifted.now, plain.now + 86_400);
        let FixtureScreen::Shell { channel: open } = plain.screen else {
            panic!("shell");
        };
        assert_eq!(
            lifted.state.timelines[&open].items[0].ts,
            plain.state.timelines[&open].items[0].ts + 86_400
        );
    }

    fn with_route(route: &str, phase: &str) -> String {
        let text = std::fs::read_to_string(
            Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("../parity/fixtures/shell/channel-view.json"),
        )
        .unwrap();
        text.replace(
            "\"route\": \"/c/{midgard}/{midgard/general}\"",
            &format!("\"route\": \"{route}\", \"phase\": \"{phase}\""),
        )
    }

    #[test]
    fn the_welcome_route_draws_the_sign_in_screen_with_no_account() {
        let f = Fixture::parse(&with_route("/welcome", "signed_out"), None).unwrap();
        assert_eq!(f.screen, FixtureScreen::SignIn);
        assert!(f.state.communities.is_empty());
        assert_ne!(f.state.account.phase, AccountPhase::Ready);
    }

    #[test]
    fn a_server_route_without_a_channel_draws_the_shell_on_that_server() {
        let f = Fixture::parse(&with_route("/c/{midgard}", "ready"), None).unwrap();
        assert!(matches!(f.screen, FixtureScreen::ShellHome { .. }));
    }

    #[test]
    fn a_route_the_app_cannot_draw_is_refused() {
        for (route, phase) in [
            ("/settings", "ready"),
            ("/activity", "ready"),
            ("/welcome", "ready"),
            ("/c/{midgard}/{midgard/general}", "signed_out"),
            ("/c/{midgard}/{midgard/general}", "revoked"),
        ] {
            let err = Fixture::parse(&with_route(route, phase), None).unwrap_err();
            assert!(err.contains("cannot draw"), "{route} {phase}: {err}");
        }
    }
}
