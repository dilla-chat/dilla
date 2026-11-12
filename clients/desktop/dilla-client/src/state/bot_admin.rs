//! `bot_admin`: Task 21's slice of [`super::AppState`]: the Bot's health, channels, webhooks and the music provider and queue,
//! and the pure decoders of the Bot's answers (ports of `packages/client-core/src/bot/{admin,moderation-log,provider-status}.ts`
//! and `voice/music.ts`). Nothing here holds key material: a webhook's address carries its secret, so it is answered once
//! by [`crate::features::bot_admin::BotAdmin::create_webhook`] and never lands in this slice.

use std::collections::{BTreeSet, HashMap};

use super::{ChannelId, CommunityId, UserId, hex};
use crate::gateway::frames::Value;
use crate::sync::value::{DecodeError, arr, at_least, bin16, opt, text, u53, uint};

/// What the slice holds. [`Default`] is the empty slice.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct BotAdminState {
    /// The Bot's health as the instance's catalog reports it (Task 20 feeds it through `BotAdmin::set_catalog`).
    pub catalog: HashMap<CommunityId, BotCatalog>,
    /// Bot channels (of a community) the Bot cannot join on its own: an admin's client adds it again (status read 94).
    pub needs_re_add: HashMap<CommunityId, BTreeSet<ChannelId>>,
    /// The last music starts, oldest first (status read 94).
    pub music_starts: HashMap<CommunityId, Vec<MusicStart>>,
    /// The webhook list, with no secret (a list never carries one).
    pub webhooks: HashMap<CommunityId, Vec<BotWebhook>>,
    /// The managed installation as a manager reads it.
    pub installation: HashMap<CommunityId, Installation>,
    /// Operation 24's last answer.
    pub provider: HashMap<CommunityId, ProviderStatusState>,
    /// One player per voice channel the Bot plays in.
    pub music: HashMap<ChannelId, MusicState>,
}

// ---- the catalog's view of the Bot (Task 20's `botCatalog:<community>`) ----

/// `BotCatalogView['status']['state']`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum CatalogState {
    Ready,
    Degraded,
    Offline,
    Unverified,
    Changed,
    Other,
}

/// What the Bot page reads of the catalog: the state, the server-stamped time it began, the command key's fingerprint and
/// the Bot channels this member can view.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BotCatalog {
    pub state: CatalogState,
    /// Unix seconds.
    pub since: Option<u64>,
    pub fingerprint: Option<String>,
    /// `None` while the catalog does not say (an older instance).
    pub bot_channels: Option<Vec<ChannelId>>,
}

/// The health line's string key and, for the two timed states, its `{time}` seconds. `None` for any other state
/// (BotHealth.tsx `healthText`).
pub fn health_line(status: &BotCatalog) -> Option<(&'static str, Option<u64>)> {
    match (status.state, status.since) {
        (CatalogState::Ready, _) => Some(("bot.health.ok", None)),
        (CatalogState::Degraded, None) => Some(("bot.health.failingNow", None)),
        (CatalogState::Degraded, Some(t)) => Some(("bot.health.failing", Some(t))),
        (CatalogState::Offline, None) => Some(("bot.health.offlineNow", None)),
        (CatalogState::Offline, Some(t)) => Some(("bot.health.offline", Some(t))),
        (CatalogState::Unverified, _) => Some(("commands.botUnverified", None)),
        (CatalogState::Changed, _) => Some(("commands.botChanged", None)),
        (CatalogState::Other, _) => None,
    }
}

// ---- sealed operations ----

/// The Bot page's status read, a sealed read with no v1 operation.
pub const BOT_STATUS_READ: u64 = 94;
/// The music starts the Bot keeps and the status read carries.
pub const MUSIC_STARTS_KEPT: usize = 20;
/// What an app operation adds to its v1 operation (the Bot's `protocol.OpApp*`).
pub const APP_OPERATION_BASE: u64 = 100;
/// Moderation log pages: the v1 operations (sealed: 100 + each).
pub const MODERATION_CASES: u64 = 60;
pub const MODERATION_EVIDENCE: u64 = 64;
/// Music provider status (v1 operation 24, sealed 124).
pub const PROVIDER_STATUS: u64 = 24;
const READS: [(u64, u64); 3] = [(71, 91), (10, 92), (84, 93)];

/// The sealed operation that runs v1 operation `v1` (a sealed-only read is itself) (operations.ts `sealedOperation`).
pub fn sealed_operation(v1: u64) -> u64 {
    if (90..APP_OPERATION_BASE).contains(&v1) {
        return v1;
    }
    READS
        .iter()
        .find(|(a, _)| *a == v1)
        .map_or(v1 + APP_OPERATION_BASE, |(_, sealed)| *sealed)
}

/// The v1 operations a surface sends that change state: their sealed arguments start with the expected revision.
pub const MUTATIONS: [u64; 24] = [
    2, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 25, 31, 51, 52, 61, 70, 73, 74, 80, 82, 83,
];

// ---- Bot status (read 94) ----

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct MusicStart {
    pub first_frame_ms: u64,
    pub cold: bool,
}

#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct BotPageStatus {
    pub needs_re_add: Vec<ChannelId>,
    pub music_starts: Vec<MusicStart>,
}

fn flag(v: &Value) -> Result<bool, DecodeError> {
    match uint(v)? {
        0 => Ok(false),
        1 => Ok(true),
        _ => Err(DecodeError("invalid flag")),
    }
}

fn decode_value(data: &[u8]) -> Result<Value, DecodeError> {
    Value::decode(data).map_err(|_| DecodeError("not CBOR"))
}

/// The status read's data: `[1, needs_re_add [bstr 16...], starts [[first_frame_ms, cold]...]]` (at most 20 starts).
pub fn decode_bot_status(data: &[u8]) -> Result<BotPageStatus, DecodeError> {
    if data.len() > 8192 {
        return Err(DecodeError("Bot status too large"));
    }
    let v = decode_value(data)?;
    let f = arr(&v, 3)?;
    if u53(&f[0])? != 1 {
        return Err(DecodeError("unsupported Bot status version"));
    }
    let rows = f[2].as_array().ok_or(DecodeError("not an array"))?;
    if rows.len() > MUSIC_STARTS_KEPT {
        return Err(DecodeError("too many music starts"));
    }
    Ok(BotPageStatus {
        needs_re_add: f[1]
            .as_array()
            .ok_or(DecodeError("not an array"))?
            .iter()
            .map(|c| bin16(c).map(ChannelId))
            .collect::<Result<_, _>>()?,
        music_starts: rows
            .iter()
            .map(|row| {
                let r = arr(row, 2)?;
                Ok(MusicStart {
                    first_frame_ms: u53(&r[0])?,
                    cold: flag(&r[1])?,
                })
            })
            .collect::<Result<_, _>>()?,
    })
}

/// The status read's data, for tests and the test Bot.
pub fn encode_bot_status(status: &BotPageStatus) -> Vec<u8> {
    Value::Array(vec![
        Value::Uint(1),
        Value::Array(
            status
                .needs_re_add
                .iter()
                .map(|c| Value::Bytes(c.0.to_vec()))
                .collect(),
        ),
        Value::Array(
            status
                .music_starts
                .iter()
                .map(|s| {
                    Value::Array(vec![
                        Value::Uint(s.first_frame_ms),
                        Value::Uint(u64::from(s.cold)),
                    ])
                })
                .collect(),
        ),
    ])
    .encode()
}

// ---- webhooks ----

/// One webhook, with no secret.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BotWebhook {
    pub webhook_id: [u8; 16],
    pub channel_id: ChannelId,
    pub name: String,
    /// The avatar's 64-hex digest.
    pub avatar: Option<String>,
    pub allow_everyone: bool,
    pub created_at: u64,
    pub rotated_at: Option<u64>,
}

impl BotWebhook {
    pub fn id_hex(&self) -> String {
        hex(&self.webhook_id)
    }
}

/// `GET .../bot/webhooks` -> `[[webhook_id, channel_id, name, avatar|null, allow_everyone, created_at, rotated_at|null]...]`.
pub fn decode_bot_webhooks(body: &[u8]) -> Result<Vec<BotWebhook>, DecodeError> {
    let v = decode_value(body)?;
    v.as_array()
        .ok_or(DecodeError("not an array"))?
        .iter()
        .map(|row| {
            let r = arr(row, 7)?;
            Ok(BotWebhook {
                webhook_id: bin16(&r[0])?,
                channel_id: ChannelId(bin16(&r[1])?),
                name: text(&r[2])?,
                avatar: opt(&r[3], |a| {
                    a.as_bytes()
                        .map(hex)
                        .ok_or(DecodeError("not a byte string"))
                })?,
                allow_everyone: flag(&r[4])?,
                created_at: u53(&r[5])?,
                rotated_at: opt(&r[6], u53)?,
            })
        })
        .collect()
}

/// The label line a webhook post ends in, and the text before it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct WebhookLabel {
    pub text: String,
    pub webhook_id: String,
    pub name: String,
    pub avatar_id: Option<String>,
}

fn lower_hex(s: &str, len: usize) -> bool {
    s.len() == len && s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f'))
}

/// JS `decodeURIComponent`: `None` where it throws.
fn uri_decode(s: &str) -> Option<String> {
    let b = s.as_bytes();
    let mut out = Vec::with_capacity(b.len());
    let mut i = 0;
    while i < b.len() {
        if b[i] == b'%' {
            let h = s.get(i + 1..i + 3)?;
            out.push(u8::from_str_radix(h, 16).ok()?);
            i += 3;
        } else {
            out.push(b[i]);
            i += 1;
        }
    }
    String::from_utf8(out).ok()
}

/// The label line a webhook post ends in (protocol/10 Webhooks), and the text before it; `None` for any other body. The
/// caller shows "via webhook <name>" only for a post the community's pinned Bot user authored: the Bot defangs `[dilla-`
/// in every free text it posts, so only its own label line can match.
pub fn decode_webhook_label(body: &str) -> Option<WebhookLabel> {
    const HEAD: &str = "[dilla-webhook:v1:";
    let nl = body.rfind('\n')?;
    let tail = body[nl + 1..].strip_prefix(HEAD)?.strip_suffix(']')?;
    let (id, rest) = tail.split_at_checked(32)?;
    let rest = rest.strip_prefix(':')?;
    let (name, avatar) = match rest.split_once(':') {
        Some((name, avatar)) => (name, Some(avatar)),
        None => (rest, None),
    };
    if !lower_hex(id, 32) || name.contains(['\n', ']']) || avatar.is_some_and(|a| !lower_hex(a, 64))
    {
        return None;
    }
    Some(WebhookLabel {
        text: body[..nl].to_owned(),
        webhook_id: id.to_owned(),
        name: uri_decode(name)?,
        avatar_id: avatar.map(str::to_owned),
    })
}

// ---- the installation ----

pub const MUSIC_VOLUMES: [u8; 4] = [25, 50, 75, 100];
pub const MUSIC_IDLE_MINUTES: [u8; 3] = [5, 15, 30];

/// The managed Bot installation (routes.ts `botInstallationRow`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Installation {
    pub id: [u8; 16],
    /// 0 pending .. 3 ready, 4 disabled, 5 failed.
    pub state: u8,
    pub revision: u64,
    pub user_id: Option<UserId>,
    pub capabilities: u64,
    pub updated_at: u64,
    pub restart_requested: u64,
    pub restart_handled: u64,
    pub version: Option<String>,
    pub music_volume: u8,
    pub music_idle_minutes: u8,
}

/// An installation row: eight fields, or thirteen on a manager's read.
pub fn decode_installation(
    v: &Value,
    community: &CommunityId,
) -> Result<Installation, DecodeError> {
    let a = v.as_array().ok_or(DecodeError("not an array"))?;
    if a.len() != 8 && a.len() != 13 {
        return Err(DecodeError(
            "expected eight or thirteen installation fields",
        ));
    }
    let state = uint(&a[2])?;
    let revision = uint(&a[3])?;
    let capabilities = u53(&a[6])?;
    if state > 5 || bin16(&a[1])? != community.0 || revision == 0 || capabilities >= (1 << 10) {
        return Err(DecodeError("installation scope, revision or capabilities"));
    }
    let mut row = Installation {
        id: bin16(&a[0])?,
        state: state as u8,
        revision,
        user_id: opt(&a[4], |u| bin16(u).map(UserId))?,
        capabilities,
        updated_at: uint(&a[7])?,
        restart_requested: 0,
        restart_handled: 0,
        version: None,
        music_volume: 50,
        music_idle_minutes: 15,
    };
    if a.len() == 13 {
        let (requested, handled) = (uint(&a[8])?, uint(&a[9])?);
        let version = opt(&a[10], text)?;
        let (volume, idle) = (u53(&a[11])?, u53(&a[12])?);
        if handled > requested
            || version
                .as_ref()
                .is_some_and(|s| s.is_empty() || s.len() > 32)
            || !MUSIC_VOLUMES.iter().any(|v| u64::from(*v) == volume)
            || !MUSIC_IDLE_MINUTES.iter().any(|v| u64::from(*v) == idle)
        {
            return Err(DecodeError(
                "installation restart, version or music settings",
            ));
        }
        row.restart_requested = requested;
        row.restart_handled = handled;
        row.version = version;
        row.music_volume = volume as u8;
        row.music_idle_minutes = idle as u8;
    }
    Ok(row)
}

// ---- music provider status (operation 24) ----

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ProviderSetup {
    Unknown,
    Configured,
    MissingRequirements,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ProviderReason {
    None,
    DependenciesMissing,
    UnsupportedBot,
    Unavailable,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MusicProviderStatus {
    pub installation_id: [u8; 16],
    /// Unix seconds.
    pub observed_at: u64,
    pub youtube_playback: ProviderSetup,
    pub youtube_reason: ProviderReason,
}

/// Where the Music page's provider row stands.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub enum ProviderStatusState {
    #[default]
    Loading,
    Known(MusicProviderStatus),
    /// The Bot offers provider status but did not answer in time.
    Partial,
    Unavailable,
}

/// The bounded overview capability tuple: whether the Bot reports operation 24.
pub fn decode_music_provider_capability(data: &[u8]) -> Result<bool, DecodeError> {
    if data.len() > 128 {
        return Err(DecodeError("invalid overview capability payload"));
    }
    let v = decode_value(data)?;
    let f = at_least(&v, 1)?;
    let version = uint(&f[0])?;
    if (version != 1 && version != 2) || f.len() != if version == 1 { 5 } else { 6 } {
        return Err(DecodeError("invalid overview capability shape"));
    }
    if bin16(&f[1])? == [0; 16] {
        return Err(DecodeError("invalid installation id"));
    }
    uint(&f[2])?;
    flag(&f[3])?;
    flag(&f[4])?;
    if version == 1 {
        return Ok(false);
    }
    let ops = f[5].as_array().ok_or(DecodeError("not an array"))?;
    if ops.len() > 32 {
        return Err(DecodeError("too many operations"));
    }
    let mut seen = BTreeSet::new();
    let mut offers = false;
    for op in ops {
        let n = uint(op)?;
        if n > 255 || !seen.insert(n) {
            return Err(DecodeError("invalid overview capability operation"));
        }
        offers |= n == PROVIDER_STATUS;
    }
    Ok(offers)
}

/// Operation 24's `[version, installation, observedAt, 4 retired slots, youtube setup, youtube reason]`.
pub fn decode_music_provider_status(data: &[u8]) -> Result<MusicProviderStatus, DecodeError> {
    if data.len() > 512 {
        return Err(DecodeError("invalid music provider status payload"));
    }
    let v = decode_value(data)?;
    let f = arr(&v, 9)?;
    if uint(&f[0])? != 1 {
        return Err(DecodeError("unsupported music provider status version"));
    }
    for retired in &f[3..7] {
        uint(retired)?;
    }
    let id = bin16(&f[1])?;
    if id == [0; 16] {
        return Err(DecodeError("invalid installation id"));
    }
    let youtube_playback = match uint(&f[7])? {
        0 => ProviderSetup::Unknown,
        1 => ProviderSetup::Configured,
        2 => ProviderSetup::MissingRequirements,
        _ => return Err(DecodeError("invalid music provider status enum")),
    };
    // Reason ids 1 to 7 are retired; the YouTube values keep their original ids.
    let youtube_reason = match uint(&f[8])? {
        0 => ProviderReason::None,
        8 => ProviderReason::DependenciesMissing,
        9 => ProviderReason::UnsupportedBot,
        10 => ProviderReason::Unavailable,
        _ => return Err(DecodeError("invalid music provider status enum")),
    };
    Ok(MusicProviderStatus {
        installation_id: id,
        observed_at: uint(&f[2])?,
        youtube_playback,
        youtube_reason,
    })
}

/// A snapshot older than this many seconds is stale (MusicProviderStatus.tsx).
pub const PROVIDER_STALE_S: u64 = 60;

// ---- the moderation log (operations 60 and 64) ----

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ModerationCase {
    pub key: String,
    pub channel_id: String,
    pub actor_id: String,
    pub message_id: String,
    pub revision: String,
    pub reason: String,
    pub action: String,
    pub excerpt: String,
    pub received_at: String,
}

/// One page: its rows, and the cursor of the next (hex) when there is one.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ModerationPage {
    pub status: u8,
    pub cases: Vec<ModerationCase>,
    pub next_cursor: Option<String>,
}

const MAX_ROWS: usize = 8;

fn is_id(v: &Value) -> bool {
    v.as_text().is_some_and(|s| lower_hex(s, 32))
}

/// Decodes a page's data, `[1, rows, cursor|null]`, strictly. Automatic case rows name this community.
pub fn decode_moderation_page(
    operation: u64,
    data: &[u8],
    community: &CommunityId,
) -> Result<(Vec<ModerationCase>, Option<String>), DecodeError> {
    const SHAPE: DecodeError = DecodeError("moderation response shape is invalid");
    let v = decode_value(data).map_err(|_| SHAPE)?;
    let f = v.as_array().ok_or(SHAPE)?;
    if f.len() != 3 || f[0] != Value::Uint(1) {
        return Err(SHAPE);
    }
    let rows = f[1].as_array().ok_or(SHAPE)?;
    if rows.len() > MAX_ROWS {
        return Err(SHAPE);
    }
    let cursor = match &f[2] {
        Value::Null => None,
        Value::Bytes(b) => Some(hex(b)),
        _ => return Err(SHAPE),
    };
    let mut cases = Vec::new();
    for value in rows {
        let row = value
            .as_array()
            .ok_or(DecodeError("moderation case shape is invalid"))?;
        if operation == MODERATION_EVIDENCE {
            const EV: DecodeError = DecodeError("manual evidence response shape is invalid");
            let [id, action, reason, excerpt, at] = row else {
                return Err(EV);
            };
            let (Some(id), Some(action), Some(reason), Some(excerpt), Some(at)) = (
                id.as_bytes().filter(|b| b.len() == 16),
                action.as_text().filter(|a| matches!(*a, "remove" | "warn")),
                reason.as_text().filter(|r| (1..=1024).contains(&r.len())),
                excerpt.as_text().filter(|e| e.len() <= 512),
                at.as_uint(),
            ) else {
                return Err(EV);
            };
            let evidence = hex(id);
            cases.push(ModerationCase {
                key: format!("manual:{evidence}"),
                channel_id: "0".repeat(32),
                actor_id: "0".repeat(32),
                message_id: evidence,
                revision: "1".to_owned(),
                reason: reason.to_owned(),
                action: action.to_owned(),
                excerpt: excerpt.to_owned(),
                received_at: at.to_string(),
            });
            continue;
        }
        const CASE: DecodeError = DecodeError("moderation case shape is invalid");
        if row.len() != 10
            || !row[..5].iter().all(is_id)
            || !matches!(row[5], Value::Uint(_))
            || !matches!(row[6], Value::Text(_))
            || !row[7]
                .as_text()
                .is_some_and(|a| matches!(a, "log" | "remove" | "warn" | "timeout"))
            || !row[8].as_text().is_some_and(|e| e.len() <= 700)
            || !matches!(row[9], Value::Uint(_))
        {
            return Err(CASE);
        }
        let s = |i: usize| row[i].as_text().unwrap_or_default().to_owned();
        if s(0) != community.hex() {
            return Err(DecodeError("moderation case community changed"));
        }
        let (revision, received) = (row[5].as_uint().unwrap_or(0), row[9].as_uint().unwrap_or(0));
        cases.push(ModerationCase {
            key: format!("{}:{revision}", s(4)),
            channel_id: s(2),
            actor_id: s(3),
            message_id: s(4),
            revision: revision.to_string(),
            reason: s(6),
            action: s(7),
            excerpt: s(8),
            received_at: received.to_string(),
        });
    }
    Ok((cases, cursor))
}

// ---- music (voice/music.ts) ----

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MusicTrack {
    pub id: String,
    pub title: String,
    pub artist: String,
    pub album: Option<String>,
    pub duration_ms: u64,
    /// The member who queued it, when the Bot recorded one.
    pub requester: Option<UserId>,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MusicSearchResult {
    pub track: MusicTrack,
    /// The address to enqueue.
    pub input: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Repeat {
    Off,
    One,
    All,
}

/// The Bot's clock stamp on a snapshot (Ruling AI): its elapsed ms since the position was the playhead, as of the
/// snapshot, and this device's clock (unix ms) when it arrived.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct PlayheadClock {
    pub elapsed_ms: u64,
    pub received_at_ms: u64,
}

#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MusicSnapshot {
    pub revision: u64,
    pub track: Option<MusicTrack>,
    pub queue: Vec<MusicTrack>,
    /// Total upcoming items when only a bounded prefix fits the response.
    pub queue_total: Option<u64>,
    pub position_ms: u64,
    pub volume: u8,
    pub paused: bool,
    pub shuffle: bool,
    pub repeat: Repeat,
    /// The viewer may control playback and manage the queue.
    pub can_control: bool,
    pub clock: Option<PlayheadClock>,
}

/// One player.
#[derive(Clone, Debug, PartialEq, Eq)]
#[allow(
    clippy::large_enum_variant,
    reason = "one snapshot per playing channel; boxing it only adds a hop for every reader"
)]
pub enum MusicState {
    Unavailable,
    Available(MusicSnapshot),
}

/// Ruling AI: the playhead to show at `now_ms`: the snapshot's position plus the Bot's elapsed time at the snapshot plus
/// the local time since it arrived, while playing, clamped to the track. Paused, idle or from an older Bot (no stamp): the
/// snapshot's position as it is.
pub fn playhead_ms(s: &MusicSnapshot, now_ms: u64) -> u64 {
    let duration = s.track.as_ref().map_or(0, |t| t.duration_ms);
    let (Some(_), false, Some(clock)) = (&s.track, s.paused, s.clock) else {
        return if duration > 0 {
            s.position_ms.min(duration)
        } else {
            s.position_ms
        };
    };
    let position = s.position_ms + clock.elapsed_ms + now_ms.saturating_sub(clock.received_at_ms);
    if duration > 0 {
        position.min(duration)
    } else {
        position
    }
}

/// `m:ss`, as the prototype prints a position and a duration (NowPlaying.tsx `playerTime`).
pub fn player_time(seconds: f64) -> String {
    let s = seconds.max(0.0).floor() as u64;
    format!("{}:{:02}", s / 60, s % 60)
}

const MUSIC_SHAPE: DecodeError = DecodeError("malformed music snapshot");

/// The 16 bytes of 32 lower-case hex digits.
fn unhex16(s: &str) -> Option<[u8; 16]> {
    if !lower_hex(s, 32) {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, byte) in out.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).ok()?;
    }
    Some(out)
}

fn is_video_id(s: &str) -> bool {
    s.len() == 11
        && s.bytes()
            .all(|b| b.is_ascii_alphanumeric() || b == b'_' || b == b'-')
}

fn music_track(v: &Value) -> Result<Option<MusicTrack>, DecodeError> {
    if matches!(v, Value::Null) {
        return Ok(None);
    }
    // A newer Bot may append slots; the eight this client reads must be there, the rest is ignored.
    let row = at_least(v, 8)?;
    let (item, provider, canonical) = (text(&row[0])?, text(&row[1])?, text(&row[2])?);
    if provider != "youtube" || !is_video_id(&canonical) || !lower_hex(&item, 32) {
        return Err(DecodeError("unsupported music track"));
    }
    let album = text(&row[5])?;
    // Slot 7 is the requester the Bot recorded (a user id; other markers, such as a search placeholder, are not a person).
    let requester = row[7]
        .as_text()
        .filter(|r| lower_hex(r, 32))
        .and_then(unhex16)
        .map(UserId);
    Ok(Some(MusicTrack {
        id: item,
        title: text(&row[3])?,
        artist: text(&row[4])?,
        album: (!album.is_empty()).then_some(album),
        duration_ms: u53(&row[6])?,
        requester,
    }))
}

/// A music snapshot (`[1, revision, track|null, queue, position, paused, volume, shuffle, repeat, can_control, ...]`).
/// `now_ms` is this device's clock (unix ms) when it arrived.
pub fn decode_music_snapshot(data: &[u8], now_ms: u64) -> Result<MusicSnapshot, DecodeError> {
    let v = decode_value(data)?;
    // v1 has 10 slots; Ruling AI adds slot 10 (unix ms when slot 4 was the playhead, 0 unknown) and 11 (its unix ms now),
    // and a bounded queue adds slot 12 (the total). Later additions are tolerated.
    let row = at_least(&v, 10)?;
    if uint(&row[0])? != 1 {
        return Err(DecodeError("unsupported music snapshot version"));
    }
    let revision = uint(&row[1])?;
    let track = music_track(&row[2])?;
    let queue = row[3]
        .as_array()
        .ok_or(MUSIC_SHAPE)?
        .iter()
        .map(|t| music_track(t)?.ok_or(DecodeError("malformed music queue")))
        .collect::<Result<Vec<_>, _>>()?;
    let queue_total = if row.len() > 12 {
        let n = uint(&row[12])?;
        if n > 500 || n < queue.len() as u64 {
            return Err(DecodeError("malformed music queue total"));
        }
        (n > queue.len() as u64).then_some(n)
    } else {
        None
    };
    let bounded = |i: usize, max: u64| -> Result<u64, DecodeError> {
        let n = uint(&row[i])?;
        if n > max { Err(MUSIC_SHAPE) } else { Ok(n) }
    };
    let clock = if row.len() >= 12 {
        let (at, now) = (uint(&row[10])?, uint(&row[11])?);
        (at != 0 && now >= at).then_some(PlayheadClock {
            elapsed_ms: now - at,
            received_at_ms: now_ms,
        })
    } else {
        None
    };
    Ok(MusicSnapshot {
        revision,
        track,
        queue,
        queue_total,
        position_ms: uint(&row[4])?,
        paused: bounded(5, 1)? == 1,
        volume: bounded(6, 100)? as u8,
        shuffle: bounded(7, 1)? == 1,
        repeat: match bounded(8, 2)? {
            0 => Repeat::Off,
            1 => Repeat::One,
            _ => Repeat::All,
        },
        can_control: bounded(9, 1)? == 1,
        clock,
    })
}

/// Search results: `[1, [[id, provider, canonical, title, artist, album, duration, ""]...]]`.
pub fn decode_music_search(data: &[u8]) -> Result<Vec<MusicSearchResult>, DecodeError> {
    let v = decode_value(data)?;
    let f = arr(&v, 2)?;
    if uint(&f[0])? != 1 {
        return Err(DecodeError("unsupported music search response"));
    }
    f[1].as_array()
        .ok_or(MUSIC_SHAPE)?
        .iter()
        .map(|value| {
            let r = arr(value, 8)?;
            let (id, provider, canonical) = (text(&r[0])?, text(&r[1])?, text(&r[2])?);
            if provider != "youtube" || !is_video_id(&canonical) || id != format!("yt:{canonical}")
            {
                return Err(DecodeError("unsupported music search result"));
            }
            if !text(&r[7])?.is_empty() {
                return Err(DecodeError("malformed music search result"));
            }
            let album = text(&r[5])?;
            Ok(MusicSearchResult {
                track: MusicTrack {
                    id,
                    title: text(&r[3])?,
                    artist: text(&r[4])?,
                    album: (!album.is_empty()).then_some(album),
                    duration_ms: u53(&r[6])?,
                    requester: None,
                },
                input: format!("https://www.youtube.com/watch?v={canonical}"),
            })
        })
        .collect()
}

#[cfg(test)]
mod tests;
