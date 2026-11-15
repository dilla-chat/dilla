//! The screen fixture: one parity state, in a JSON file both shooters read.
//!
//! ```text
//! clients/desktop/parity/fixtures/<area>/<state>[-b].json    this schema
//! clients/desktop/parity/references/<os>/<area>/<state>[-b]-2x.png    the web client's frame of it
//! clients/desktop/parity/references/<os>/<area>/<state>[-b].shot.json    what the web shooter resolved (optional)
//! ```
//!
//! The web shooter (`parity/shoot.mjs`) seeds a real dillad with the [`WorldSpec`], signs the viewer in on a Chromium
//! at the fixture's window and replays the steps' `web` selectors. The GPUI side (`dilla-desktop`'s automation build)
//! draws the same world without a network and the driver replays the `gpui` ids. Neither side may hold data the file
//! does not: UI code never carries fixture content.
//!
//! Everything is named by short local ids (`ada`, `midgard`, `midgard/general`). [`WorldSpec::ids`] derives a stable
//! 16-byte id for each, the same way `shoot.mjs` does, so both sides can log the same world ([`ScreenFixture::fixture_line`]).
//! A real dillad hands out ids of its own: the shooter writes them, and the host-clock shift the shot ran under, into the
//! reference's [`Shot`] sidecar.
//!
//! The file is strict (`deny_unknown_fields`): a typo is an error, not a silently ignored field.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;

use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

/// One parity state.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ScreenFixture {
    /// `<area>/<state>[-b]`: the fixture's path under `fixtures/` and its reference's under `references/<os>/`.
    pub name: String,
    /// The window, logical px; shot at scale 2.
    pub window: (u32, u32),
    /// Who and what exists.
    pub world: WorldSpec,
    /// The person whose device is shot.
    pub viewer: String,
    /// A `router.ts` path. `{server}`, `{server/channel}` and `{dm/<id>}` stand for the hex ids of those entities.
    pub route: String,
    /// The frozen "now", unix seconds.
    pub clock: u64,
    /// Where the viewer's account stands: `ready` draws the app's own screens, any other phase draws what a device in
    /// that phase sees (`/welcome` is the sign-in screen of a device with no account).
    #[serde(default)]
    pub phase: Phase,
    /// The same actions on both sides, in order.
    #[serde(default)]
    pub steps: Vec<Step>,
}

/// The account phase of the viewer's device (`AccountPhase`, narrowed to what a fixture draws).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Phase {
    /// No account on this device yet: the welcome and sign-in screens.
    SignedOut,
    #[default]
    Ready,
    FactorSetup,
    /// The instance no longer accepts this device (Boot.tsx `revoked`): the world removes the viewer's device after
    /// the history, and the device starts again.
    Revoked,
    VersionRefused,
    /// The key that opens this device's store is gone (Boot.tsx `store-lost`): the browser's key wrapper is deleted, and
    /// the device starts again.
    StoreLost,
}

/// One action of a fixture, replayed on both sides.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(tag = "do", rename_all = "snake_case", deny_unknown_fields)]
pub enum Step {
    /// `web`: a Playwright selector (`css=` / `text=` / `role=`; a leading `if:` clicks only when the element is there);
    /// `gpui`: an automation element id. `-` on either side is a step with no counterpart there (the web side closes a
    /// pane the GPUI screen does not have yet).
    Click {
        web: String,
        gpui: String,
    },
    Hover {
        web: String,
        gpui: String,
    },
    /// A named key or chord (`enter`, `tab`, `shift-tab`, `escape`, `up`, `ctrl-k`, `ctrl-shift-m`, ...).
    Key {
        chord: String,
    },
    /// Clicks the element, then types `text`.
    Type {
        web: String,
        gpui: String,
        text: String,
    },
    /// Waits until the screen is idle (no pending work, no animation, fonts loaded).
    Settle,
}

/// People, servers, channels, messages, DMs, roles and calls.
#[derive(Clone, Debug, Default, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct WorldSpec {
    #[serde(default)]
    pub people: Vec<PersonSpec>,
    #[serde(default)]
    pub servers: Vec<ServerSpec>,
    /// Every message of every channel and DM, oldest first within its channel.
    #[serde(default)]
    pub messages: Vec<MessageSpec>,
    #[serde(default)]
    pub dms: Vec<DmSpec>,
    /// Who is in which voice channel or DM call.
    #[serde(default)]
    pub calls: Vec<CallSpec>,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct PersonSpec {
    pub id: String,
    /// The account name; the id when absent.
    #[serde(default)]
    pub username: Option<String>,
    pub display: String,
    /// A Bot account (member kind 1).
    #[serde(default)]
    pub bot: bool,
    #[serde(default)]
    pub status: String,
    #[serde(default)]
    pub bio: String,
}

impl PersonSpec {
    pub fn username(&self) -> &str {
        self.username.as_deref().unwrap_or(&self.id)
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ServerSpec {
    pub id: String,
    pub name: String,
    pub owner: String,
    /// Every member, the owner included.
    pub members: Vec<String>,
    #[serde(default)]
    pub roles: Vec<RoleSpec>,
    /// In creation order; the world makes `general` itself, so a seeded server lists a text channel `general`.
    pub channels: Vec<ChannelSpec>,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct RoleSpec {
    pub id: String,
    pub name: String,
    #[serde(default)]
    pub colour: Option<String>,
    #[serde(default)]
    pub permissions: Vec<String>,
    #[serde(default)]
    pub members: Vec<String>,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelKind {
    Text,
    Voice,
    Category,
}

impl ChannelKind {
    /// The wire's channel kind: 0 text, 1 voice, 2 category.
    pub fn wire(self) -> u8 {
        match self {
            Self::Text => 0,
            Self::Voice => 1,
            Self::Category => 2,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelSpec {
    pub id: String,
    pub kind: ChannelKind,
    pub name: String,
    #[serde(default)]
    pub topic: String,
    /// The id of the category this channel sits in.
    #[serde(default)]
    pub parent: Option<String>,
    /// The channel's sort position, 0 when absent: what a channel made without a position has (the web client then
    /// sorts equal positions by id, which is why a shot records the real ids).
    #[serde(default)]
    pub position: Option<u32>,
    /// Made by the server itself (the AFK channel), so a seeded world does not make it.
    #[serde(default)]
    pub system: bool,
    #[serde(default)]
    pub slowmode: u32,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct MessageSpec {
    /// Unique in the fixture; `reply_to` and `thread_of` name it.
    pub key: String,
    /// `<server>/<channel>` or `dm/<dm id>`.
    pub channel: String,
    pub sender: String,
    /// Unix seconds.
    pub ts: u64,
    pub body: String,
    #[serde(default)]
    pub edited: bool,
    #[serde(default)]
    pub pinned: bool,
    #[serde(default)]
    pub reply_to: Option<String>,
    /// The key of the message whose thread this one belongs to.
    #[serde(default)]
    pub thread_of: Option<String>,
    #[serde(default)]
    pub reactions: Vec<ReactionSpec>,
    #[serde(default)]
    pub attachments: Vec<AttachmentSpec>,
    #[serde(default)]
    pub poll: Option<PollSpec>,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ReactionSpec {
    pub emoji: String,
    pub by: Vec<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct AttachmentSpec {
    pub name: String,
    pub mime: String,
    pub size: u64,
    #[serde(default)]
    pub width: Option<u32>,
    #[serde(default)]
    pub height: Option<u32>,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct PollSpec {
    pub question: String,
    pub options: Vec<String>,
    /// `(option index, voter)` pairs.
    #[serde(default)]
    pub votes: Vec<VoteSpec>,
    #[serde(default)]
    pub closed: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct VoteSpec {
    pub option: usize,
    pub by: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct DmSpec {
    pub id: String,
    /// Two people for a DM, more for a group.
    pub members: Vec<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct CallSpec {
    /// A voice channel (`<server>/<channel>`) or a DM (`dm/<id>`).
    pub channel: String,
    pub participants: Vec<ParticipantSpec>,
}

#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ParticipantSpec {
    pub person: String,
    #[serde(default)]
    pub muted: bool,
    #[serde(default)]
    pub deafened: bool,
    #[serde(default)]
    pub sharing: bool,
}

/// What seeding a fixture's world needs (`ScreenFixture::seed_plan`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SeedPlan {
    /// People who run a headless client, the viewer first.
    pub clients: Vec<String>,
    /// `<server>/<channel>` paths the browser opens, the route's own last.
    pub visit: Vec<String>,
}

/// What the web shooter resolved when it shot a reference, next to the PNG: `<state>[-b].shot.json`.
///
/// A real dillad stamps messages with its own clock, which only moves forward from the moment it starts. The shooter
/// therefore lifts the whole fixture by `shift` seconds (every message's `ts`, and the browser's "now") and records it
/// here, so the GPUI side draws the same instants. `digest` is the SHA-256 of the fixture file the shot was made from:
/// a reference shot from another version of the fixture is stale and the comparison refuses it.
#[derive(Clone, Debug, PartialEq, Eq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct Shot {
    pub digest: String,
    /// Seconds added to every `ts` and to `clock`.
    #[serde(default)]
    pub shift: u64,
    /// Written by hand from the data of an earlier shot, not by the shooter: true until `shoot.mjs` writes the pair.
    #[serde(default)]
    pub hand_migrated: bool,
    /// The ids the real dillad gave each local id (`kind:name`, 32 lower-case hex digits). They replace the derived ids on
    /// the GPUI side and in the fixture line, because the web client sorts equal positions by id. A key that is not in the
    /// fixture's world, or a value that is not an id, is refused ([`ScreenFixture::effective_ids`]).
    #[serde(default)]
    pub ids: BTreeMap<String, String>,
}

/// Why a fixture is refused.
#[derive(Debug, thiserror::Error, PartialEq, Eq)]
pub enum SpecError {
    #[error("not valid fixture JSON: {0}")]
    Json(String),
    #[error("{0}")]
    Invalid(String),
}

fn invalid<T>(why: impl Into<String>) -> Result<T, SpecError> {
    Err(SpecError::Invalid(why.into()))
}

/// Lower-case hex of `bytes`.
pub fn hex(bytes: &[u8]) -> String {
    let mut out = String::with_capacity(bytes.len() * 2);
    for b in bytes {
        let _ = write!(out, "{b:02x}");
    }
    out
}

/// The 16 bytes of 32 lower-case hex digits (a strict reading: no sign, no case folding), or `None`.
pub fn unhex16(text: &str) -> Option<[u8; 16]> {
    if text.len() != 32 || !text.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f')) {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, byte) in out.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&text[2 * i..2 * i + 2], 16).ok()?;
    }
    Some(out)
}

/// The SHA-256 of `bytes`, hex: the digest both shooters log for a fixture file.
pub fn digest(bytes: &[u8]) -> String {
    hex(&Sha256::digest(bytes))
}

/// The stable id of the entity `local` of `kind` (`person`, `server`, `channel`, `dm`, `message`, `role`): the first 16
/// bytes of SHA-256 over `dilla-parity/1\0<kind>\0<local>`. `shoot.mjs` derives the same bytes.
pub fn derive_id(kind: &str, local: &str) -> [u8; 16] {
    let mut hasher = Sha256::new();
    hasher.update(b"dilla-parity/1\0");
    hasher.update(kind.as_bytes());
    hasher.update([0]);
    hasher.update(local.as_bytes());
    let full = hasher.finalize();
    let mut out = [0u8; 16];
    out.copy_from_slice(&full[..16]);
    out
}

/// The grammar of a key chord: `-`-joined modifiers (`ctrl`, `alt`, `shift`, `fn`, `secondary`) and one key, which is a
/// name, a letter, a digit or one of `, . / ; ' [ ] \ - = \``. `-` itself is written `minus`.
pub fn validate_chord(chord: &str) -> Result<(), SpecError> {
    const NAMED: [&str; 24] = [
        "enter",
        "tab",
        "escape",
        "backspace",
        "delete",
        "space",
        "up",
        "down",
        "left",
        "right",
        "home",
        "end",
        "pageup",
        "pagedown",
        "minus",
        "comma",
        "period",
        "slash",
        "semicolon",
        "quote",
        "backslash",
        "equals",
        "backtick",
        "insert",
    ];
    let parts: Vec<&str> = chord.split('-').collect();
    let (key, modifiers) = match parts.split_last() {
        Some((key, modifiers)) if !key.is_empty() => (*key, modifiers),
        _ => return invalid(format!("`{chord}` is not a key chord")),
    };
    for m in modifiers {
        if !matches!(*m, "ctrl" | "alt" | "shift" | "fn" | "secondary") {
            return invalid(format!("`{chord}`: `{m}` is not a modifier"));
        }
    }
    let function_key = key
        .strip_prefix('f')
        .and_then(|n| n.parse::<u8>().ok())
        .is_some_and(|n| (1..=12).contains(&n));
    let single = key.chars().count() == 1
        && key
            .chars()
            .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit());
    if NAMED.contains(&key) || function_key || single {
        Ok(())
    } else {
        invalid(format!("`{chord}`: `{key}` is not a key name"))
    }
}

impl ScreenFixture {
    pub fn parse(text: &str) -> Result<Self, SpecError> {
        let fixture: Self =
            serde_json::from_str(text).map_err(|e| SpecError::Json(e.to_string()))?;
        fixture.validate()?;
        Ok(fixture)
    }

    /// Every id of the world, by `kind:local`.
    pub fn ids(&self) -> BTreeMap<String, [u8; 16]> {
        self.world.ids()
    }

    /// The line both shooters log for a fixture: `fixture <name> <digest of the file> <kind:local=hex;...>`. The web
    /// shooter's `--check` and the GPUI driver's `fixture` command print the same line for the same file.
    pub fn fixture_line(&self, file_digest: &str) -> String {
        self.fixture_line_with(file_digest, &self.ids())
    }

    /// [`Self::fixture_line`] with the ids both sides use (see [`Self::effective_ids`]).
    pub fn fixture_line_with(&self, file_digest: &str, ids: &BTreeMap<String, [u8; 16]>) -> String {
        format!("fixture {} {} {}", self.name, file_digest, ids_line(ids))
    }

    /// The derived ids, replaced by the ones a shot recorded. Refuses an id that is not 32 lower-case hex digits and a key
    /// that names nothing in the world.
    pub fn effective_ids(
        &self,
        shot: Option<&Shot>,
    ) -> Result<BTreeMap<String, [u8; 16]>, SpecError> {
        let mut ids = self.ids();
        for (key, value) in shot.map(|s| &s.ids).into_iter().flatten() {
            let Some(slot) = ids.get_mut(key) else {
                return invalid(format!("sidecar id `{key}` is not in the fixture's world"));
            };
            let Some(id) = unhex16(value) else {
                return invalid(format!(
                    "sidecar id `{key}`: `{value}` is not 32 lower-case hex digits"
                ));
            };
            *slot = id;
        }
        Ok(ids)
    }

    /// The route with its `{...}` placeholders replaced by hex ids.
    pub fn resolved_route(&self) -> Result<String, SpecError> {
        self.resolved_route_with(derive_id)
    }

    /// [`Self::resolved_route`] with the id of each `(kind, local)` supplied by `id_of` (the ids a shot recorded).
    pub fn resolved_route_with(
        &self,
        id_of: impl Fn(&str, &str) -> [u8; 16],
    ) -> Result<String, SpecError> {
        let known = self.ids();
        let mut out = String::new();
        let mut rest = self.route.as_str();
        while let Some(open) = rest.find('{') {
            out.push_str(&rest[..open]);
            let Some(close) = rest[open..].find('}') else {
                return invalid(format!("route `{}`: an unclosed `{{`", self.route));
            };
            let name = &rest[open + 1..open + close];
            let (kind, local) = if let Some(dm) = name.strip_prefix("dm/") {
                ("dm", dm)
            } else if name.contains('/') {
                ("channel", name)
            } else {
                ("server", name)
            };
            if !known.contains_key(&format!("{kind}:{local}")) {
                return invalid(format!(
                    "route `{}`: `{name}` is not in the world",
                    self.route
                ));
            }
            out.push_str(&hex(&id_of(kind, local)));
            rest = &rest[open + close + 1..];
        }
        out.push_str(rest);
        Ok(out)
    }

    /// Who runs a headless client while the world is seeded, and which channels the browser opens before the history is
    /// posted (a device added later cannot read what was sent before it joined).
    pub fn seed_plan(&self) -> SeedPlan {
        // The viewer first (its client reads the channel list), then everyone who posts, in order of first message.
        let mut clients = vec![self.viewer.clone()];
        let mut history: Vec<String> = Vec::new();
        for m in &self.world.messages {
            if !clients.contains(&m.sender) {
                clients.push(m.sender.clone());
            }
            if !m.channel.starts_with("dm/") && !history.contains(&m.channel) {
                history.push(m.channel.clone());
            }
        }
        // The route's own text channels come last, with or without history: an empty channel is still opened.
        let mut route_channels = Vec::new();
        let mut rest = self.route.as_str();
        while let Some(open) = rest.find('{') {
            let Some(close) = rest[open..].find('}') else {
                break;
            };
            let name = &rest[open + 1..open + close];
            if name.contains('/')
                && !name.starts_with("dm/")
                && self
                    .world
                    .channel(name)
                    .is_some_and(|(_, c)| c.kind == ChannelKind::Text)
            {
                route_channels.push(name.to_owned());
            }
            rest = &rest[open + close + 1..];
        }
        let mut visit: Vec<String> = history
            .into_iter()
            .filter(|c| !route_channels.contains(c))
            .collect();
        visit.extend(route_channels);
        SeedPlan { clients, visit }
    }

    /// Checks every reference and rule of the file.
    pub fn validate(&self) -> Result<(), SpecError> {
        let (area, state) = match self.name.split_once('/') {
            Some((a, s)) if !a.is_empty() && !s.is_empty() && !s.contains('/') => (a, s),
            _ => return invalid(format!("name `{}` is not `<area>/<state>[-b]`", self.name)),
        };
        let slug = |s: &str| {
            s.chars()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '-')
        };
        if !slug(area) || !slug(state) {
            return invalid(format!(
                "name `{}`: lower-case letters, digits and `-` only",
                self.name
            ));
        }
        let (w, h) = self.window;
        if !(320..=3840).contains(&w) || !(240..=2160).contains(&h) {
            return invalid(format!("window {w}x{h} is outside 320x240..3840x2160"));
        }
        self.world.validate()?;
        if !self.world.people.iter().any(|p| p.id == self.viewer) {
            return invalid(format!(
                "viewer `{}` is not a person of the world",
                self.viewer
            ));
        }
        if let Some(latest) = self.world.messages.iter().map(|m| m.ts).max()
            && self.clock < latest
        {
            return invalid(format!(
                "clock {} is before the latest message ({latest})",
                self.clock
            ));
        }
        self.resolved_route()?;
        for (n, step) in self.steps.iter().enumerate() {
            let empty = |s: &str| s.trim().is_empty();
            match step {
                Step::Click { web, gpui } | Step::Hover { web, gpui } => {
                    if empty(web) || empty(gpui) {
                        return invalid(format!("step {n}: `web` and `gpui` are both required"));
                    }
                }
                Step::Type { web, gpui, .. } => {
                    if empty(web) || empty(gpui) {
                        return invalid(format!("step {n}: `web` and `gpui` are both required"));
                    }
                }
                Step::Key { chord } => validate_chord(chord)
                    .map_err(|e| SpecError::Invalid(format!("step {n}: {e}")))?,
                Step::Settle => {}
            }
        }
        Ok(())
    }
}

/// `kind:local=hex` joined by `;`, in key order.
pub fn ids_line(ids: &BTreeMap<String, [u8; 16]>) -> String {
    ids.iter()
        .map(|(k, v)| format!("{k}={}", hex(v)))
        .collect::<Vec<_>>()
        .join(";")
}

fn unique<'a>(what: &str, names: impl Iterator<Item = &'a str>) -> Result<(), SpecError> {
    let mut seen = BTreeSet::new();
    for name in names {
        if name.is_empty() || name.contains(['/', '{', '}', ';', '=', ':']) {
            return invalid(format!(
                "{what} id `{name}` is empty or has a reserved character"
            ));
        }
        if !seen.insert(name) {
            return invalid(format!("{what} id `{name}` is used twice"));
        }
    }
    Ok(())
}

impl WorldSpec {
    /// Every entity's stable id, by `kind:local`.
    pub fn ids(&self) -> BTreeMap<String, [u8; 16]> {
        let mut ids = BTreeMap::new();
        let mut put = |kind: &str, local: &str| {
            ids.insert(format!("{kind}:{local}"), derive_id(kind, local));
        };
        for p in &self.people {
            put("person", &p.id);
        }
        for s in &self.servers {
            put("server", &s.id);
            for c in &s.channels {
                put("channel", &format!("{}/{}", s.id, c.id));
            }
            for r in &s.roles {
                put("role", &format!("{}/{}", s.id, r.id));
            }
        }
        for d in &self.dms {
            put("dm", &d.id);
        }
        for m in &self.messages {
            put("message", &m.key);
        }
        ids
    }

    /// The server and channel a `<server>/<channel>` path names.
    pub fn channel(&self, path: &str) -> Option<(&ServerSpec, &ChannelSpec)> {
        let (server, channel) = path.split_once('/')?;
        let server = self.servers.iter().find(|s| s.id == server)?;
        let channel = server.channels.iter().find(|c| c.id == channel)?;
        Some((server, channel))
    }

    fn person(&self, id: &str, what: &str) -> Result<(), SpecError> {
        if self.people.iter().any(|p| p.id == id) {
            Ok(())
        } else {
            invalid(format!("{what}: `{id}` is not a person of the world"))
        }
    }

    pub fn validate(&self) -> Result<(), SpecError> {
        unique("person", self.people.iter().map(|p| p.id.as_str()))?;
        unique("server", self.servers.iter().map(|s| s.id.as_str()))?;
        unique("dm", self.dms.iter().map(|d| d.id.as_str()))?;
        unique("message", self.messages.iter().map(|m| m.key.as_str()))?;
        for s in &self.servers {
            unique(
                &format!("channel of {}", s.id),
                s.channels.iter().map(|c| c.id.as_str()),
            )?;
            unique(
                &format!("role of {}", s.id),
                s.roles.iter().map(|r| r.id.as_str()),
            )?;
            for m in &s.members {
                self.person(m, &format!("server {} members", s.id))?;
            }
            if !s.members.contains(&s.owner) {
                return invalid(format!(
                    "server {}: the owner `{}` is not a member",
                    s.id, s.owner
                ));
            }
            for c in &s.channels {
                if let Some(parent) = &c.parent {
                    let ok = s
                        .channels
                        .iter()
                        .any(|p| p.id == *parent && p.kind == ChannelKind::Category);
                    if !ok {
                        return invalid(format!(
                            "channel {}/{}: parent `{parent}` is not a category of the server",
                            s.id, c.id
                        ));
                    }
                    if c.kind == ChannelKind::Category {
                        return invalid(format!(
                            "channel {}/{}: a category has no parent",
                            s.id, c.id
                        ));
                    }
                }
            }
            for r in &s.roles {
                for m in &r.members {
                    if !s.members.contains(m) {
                        return invalid(format!(
                            "role {}/{}: `{m}` is not a member of the server",
                            s.id, r.id
                        ));
                    }
                }
            }
        }
        for d in &self.dms {
            if d.members.len() < 2 {
                return invalid(format!("dm {}: needs at least two people", d.id));
            }
            for m in &d.members {
                self.person(m, &format!("dm {} members", d.id))?;
            }
        }
        let mut seen: BTreeMap<&str, (&str, u64)> = BTreeMap::new();
        let mut last_ts: BTreeMap<&str, u64> = BTreeMap::new();
        for m in &self.messages {
            self.person(&m.sender, &format!("message {} sender", m.key))?;
            let members: Vec<&str> = if let Some(dm) = m.channel.strip_prefix("dm/") {
                let Some(dm) = self.dms.iter().find(|d| d.id == dm) else {
                    return invalid(format!("message {}: no dm `{}`", m.key, m.channel));
                };
                dm.members.iter().map(String::as_str).collect()
            } else {
                let Some((server, channel)) = self.channel(&m.channel) else {
                    return invalid(format!("message {}: no channel `{}`", m.key, m.channel));
                };
                if channel.kind != ChannelKind::Text {
                    return invalid(format!(
                        "message {}: `{}` is not a text channel",
                        m.key, m.channel
                    ));
                }
                server.members.iter().map(String::as_str).collect()
            };
            if !members.contains(&m.sender.as_str()) {
                return invalid(format!(
                    "message {}: `{}` is not in `{}`",
                    m.key, m.sender, m.channel
                ));
            }
            for (what, target) in [("reply_to", &m.reply_to), ("thread_of", &m.thread_of)] {
                if let Some(target) = target {
                    // Messages are oldest first: a reply names an earlier one of the same channel.
                    match seen.get(target.as_str()) {
                        Some((channel, _)) if *channel == m.channel => {}
                        _ => {
                            return invalid(format!(
                                "message {}: {what} `{target}` is not an earlier message of `{}`",
                                m.key, m.channel
                            ));
                        }
                    }
                }
            }
            if let Some(last) = last_ts.get(m.channel.as_str())
                && m.ts < *last
            {
                return invalid(format!(
                    "message {}: older than the message before it in `{}`",
                    m.key, m.channel
                ));
            }
            for r in &m.reactions {
                if r.emoji.is_empty() || r.by.is_empty() {
                    return invalid(format!(
                        "message {}: a reaction needs an emoji and a person",
                        m.key
                    ));
                }
                for by in &r.by {
                    self.person(by, &format!("message {} reaction", m.key))?;
                }
            }
            if let Some(poll) = &m.poll {
                if poll.options.len() < 2 {
                    return invalid(format!("message {}: a poll needs two options", m.key));
                }
                for v in &poll.votes {
                    if v.option >= poll.options.len() {
                        return invalid(format!("message {}: vote for option {}", m.key, v.option));
                    }
                    self.person(&v.by, &format!("message {} vote", m.key))?;
                }
            }
            seen.insert(&m.key, (&m.channel, m.ts));
            last_ts.insert(&m.channel, m.ts);
        }
        for call in &self.calls {
            let ok = call.channel.strip_prefix("dm/").map_or_else(
                || {
                    self.channel(&call.channel)
                        .is_some_and(|(_, c)| c.kind == ChannelKind::Voice)
                },
                |dm| self.dms.iter().any(|d| d.id == dm),
            );
            if !ok {
                return invalid(format!(
                    "call `{}` is not a voice channel or dm",
                    call.channel
                ));
            }
            for p in &call.participants {
                self.person(&p.person, "call participant")?;
            }
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const MINIMAL: &str = r#"{
      "name": "shell/minimal", "window": [1280, 800], "viewer": "ada", "clock": 1000,
      "route": "/c/{midgard}/{midgard/general}",
      "world": {
        "people": [{"id": "ada", "display": "Ada"}, {"id": "mira", "display": "Mira"}],
        "servers": [{"id": "midgard", "name": "Midgard", "owner": "ada", "members": ["ada", "mira"],
          "channels": [{"id": "general", "kind": "text", "name": "general"}]}],
        "messages": [{"key": "m1", "channel": "midgard/general", "sender": "mira", "ts": 990, "body": "hi"}]
      },
      "steps": [{"do": "key", "chord": "ctrl-k"}, {"do": "settle"}]
    }"#;

    fn fixture() -> ScreenFixture {
        ScreenFixture::parse(MINIMAL).unwrap()
    }

    #[test]
    fn minimal_fixture_parses_and_resolves_its_route() {
        let f = fixture();
        let route = f.resolved_route().unwrap();
        assert!(route.starts_with("/c/"), "{route}");
        assert_eq!(route.len(), "/c/".len() + 32 + 1 + 32);
        assert_eq!(f.steps.len(), 2);
    }

    #[test]
    fn ids_are_stable_and_distinct() {
        let ids = fixture().ids();
        assert_eq!(ids["person:ada"], derive_id("person", "ada"));
        assert_ne!(ids["person:ada"], ids["person:mira"]);
        // The same local name of another kind is another id.
        assert_ne!(derive_id("person", "x"), derive_id("server", "x"));
    }

    #[test]
    fn digest_is_sha256() {
        assert_eq!(
            digest(b"abc"),
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }

    #[test]
    fn unknown_fields_are_refused() {
        let bad = MINIMAL.replace("\"clock\": 1000,", "\"clock\": 1000, \"colour\": 1,");
        assert!(matches!(
            ScreenFixture::parse(&bad),
            Err(SpecError::Json(_))
        ));
    }

    fn refused(edit: impl Fn(&str) -> String) -> String {
        match ScreenFixture::parse(&edit(MINIMAL)) {
            Err(SpecError::Invalid(why)) => why,
            other => panic!("expected an invalid fixture, got {other:?}"),
        }
    }

    #[test]
    fn dangling_references_are_refused() {
        assert!(
            refused(|s| s.replace("\"sender\": \"mira\"", "\"sender\": \"zed\"")).contains("zed")
        );
        assert!(
            refused(|s| s.replace("\"viewer\": \"ada\"", "\"viewer\": \"zed\"")).contains("viewer")
        );
        assert!(
            refused(|s| s.replace("midgard/general\", \"sender", "midgard/nope\", \"sender"))
                .contains("no channel")
        );
        assert!(refused(|s| s.replace("{midgard/general}", "{midgard/nope}")).contains("route"));
        assert!(refused(|s| s.replace("\"clock\": 1000", "\"clock\": 5")).contains("clock"));
        assert!(refused(|s| s.replace("shell/minimal", "minimal")).contains("name"));
    }

    #[test]
    fn replies_name_an_earlier_message_of_the_same_channel() {
        let with_reply = |target: &str| {
            MINIMAL.replace(
                "\"body\": \"hi\"}",
                &format!(
                    "\"body\": \"hi\"}}, {{\"key\": \"m2\", \"channel\": \"midgard/general\", \"sender\": \"ada\", \"ts\": 995, \"body\": \"yo\", \"reply_to\": \"{target}\"}}"
                ),
            )
        };
        ScreenFixture::parse(&with_reply("m1")).unwrap();
        assert!(matches!(
            ScreenFixture::parse(&with_reply("m9")),
            Err(SpecError::Invalid(_))
        ));
    }

    #[test]
    fn chords_follow_the_grammar() {
        for ok in [
            "enter",
            "tab",
            "shift-tab",
            "escape",
            "up",
            "ctrl-k",
            "ctrl-shift-m",
            "f5",
            "a",
        ] {
            validate_chord(ok).unwrap();
        }
        for bad in [
            "",
            "ctrl-",
            "ctrl-ctrl2",
            "meta-k",
            "Enter",
            "ctrl-kk",
            "f13",
            "super-k",
        ] {
            assert!(validate_chord(bad).is_err(), "{bad} should be refused");
        }
    }

    #[test]
    fn a_shot_replaces_derived_ids_and_is_checked() {
        let f = fixture();
        let real = "00112233445566778899aabbccddeeff";
        let shot = |key: &str, value: &str| Shot {
            digest: String::new(),
            shift: 0,
            hand_migrated: false,
            ids: [(key.to_owned(), value.to_owned())].into(),
        };
        let ids = f.effective_ids(Some(&shot("person:ada", real))).unwrap();
        assert_eq!(hex(&ids["person:ada"]), real);
        assert_eq!(ids["person:mira"], derive_id("person", "mira"));
        for bad in [
            shot("person:ada", "+0112233445566778899aabbccddeef"),
            shot("person:ada", "xyz"),
            shot("person:ada", &real.to_uppercase()),
            shot("person:zed", real),
        ] {
            assert!(f.effective_ids(Some(&bad)).is_err(), "{bad:?}");
        }
        assert!(f.fixture_line_with("d", &ids).contains(real));
    }

    #[test]
    fn an_empty_world_still_has_a_viewer_client_and_opens_the_routes_channel() {
        let empty = MINIMAL.replace(
            r#""messages": [{"key": "m1", "channel": "midgard/general", "sender": "mira", "ts": 990, "body": "hi"}]"#,
            r#""messages": []"#,
        );
        let plan = ScreenFixture::parse(&empty).unwrap().seed_plan();
        assert_eq!(plan.clients, ["ada"]);
        assert_eq!(plan.visit, ["midgard/general"]);
        // With history: posters join the viewer, and the route's channel is visited last.
        let plan = fixture().seed_plan();
        assert_eq!(plan.clients, ["ada", "mira"]);
        assert_eq!(plan.visit, ["midgard/general"]);
    }

    #[test]
    fn phase_defaults_to_ready() {
        assert_eq!(fixture().phase, Phase::Ready);
        let out = MINIMAL.replace(
            "\"clock\": 1000,",
            "\"clock\": 1000, \"phase\": \"signed_out\",",
        );
        assert_eq!(ScreenFixture::parse(&out).unwrap().phase, Phase::SignedOut);
    }

    #[test]
    fn fixture_line_carries_name_digest_and_ids() {
        let f = fixture();
        let line = f.fixture_line(&digest(MINIMAL.as_bytes()));
        assert!(line.starts_with("fixture shell/minimal "));
        assert!(line.contains("person:ada="));
        assert!(line.contains("channel:midgard/general="));
    }
}
