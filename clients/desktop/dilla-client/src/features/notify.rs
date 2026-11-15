//! Area **notify** (Task 22): mention, DM and ring decisions, mutes per channel and server, the device-wide default, the
//! channel-on-screen suppression and the total badge count.
//!
//! Ports: packages/web/src/{notify,notify-permission}.ts, packages/client-core/src/state/{badges,mentions,settings}.ts.
//!
//! What is here, bottom up:
//! - the settings, as the instance's preferences under `notify.*` ([`Settings`], [`SettingsSource`]) and the pure
//!   decisions over them ([`effective_notify_mode`], [`is_muted`], [`suppresses_everyone`], [`should_notify`],
//!   [`ring_notification`]), the same tables as the web's unit tests;
//! - the badge counts ([`row_counts`], [`rail_counts`], [`total_badge`]): a muted channel hides its count but keeps its
//!   mentions, and the total is what the later tray and dock icon show;
//! - the notices a state change raises ([`NoticeTracker`], the worker's `reportActivity`), turned into notifications by
//!   [`Engine`] and handed to a [`Notifier`]. This plan's notifier is the in-app [`RecordingNotifier`]; the OS delivers in
//!   the next plan.
//!
//! Attacker statement (the web's lesson e): the notifier refuses nothing. What it shows is the body of a message the core
//! already decrypted and the timeline renders, on this device only.

use std::collections::{BTreeMap, HashMap};
use std::sync::{Arc, Mutex};

use tokio::sync::watch;

use crate::client::{Client, ClientError, Inner};
use crate::prefs::{Prefs, PrefsError};
use crate::state::unread::Permission;
use crate::state::{AppState, ChannelId, CommunityId, TimelineItemState, UserId, hex};

// ---- the settings --------------------------------------------------------------------------------------------------

/// `SETTINGS.notifyDefault` (`dms-mentions` | `everything` | `nothing`), the device-wide default.
pub const KEY_DEFAULT: &str = "notify.default";
/// `SETTINGS.notifyDesktop` (`on` | `off`): notifications on this device. Absent is on.
pub const KEY_DESKTOP: &str = "notify.desktop";
/// The join, leave and ring sounds (`on` | `off`), which Task 19's `sounds` read through [`Settings::sounds_on`].
pub const KEY_SOUNDS: &str = "notify.sounds";

/// `SETTINGS.notifyChannel`: `all` | `mentions` | `nothing`.
pub fn key_channel_mode(channel: ChannelId) -> String {
    format!("notify.channel.{}", channel.hex())
}
/// `SETTINGS.muteChannel`: `1` when muted.
pub fn key_channel_mute(channel: ChannelId) -> String {
    format!("notify.mute.channel.{}", channel.hex())
}
/// `SETTINGS.notifyCommunity`: `all` | `mentions` | `nothing`.
pub fn key_community_mode(community: CommunityId) -> String {
    format!("notify.community.{}", community.hex())
}
/// `SETTINGS.muteCommunity`: `1` when the whole server is muted.
pub fn key_community_mute(community: CommunityId) -> String {
    format!("notify.mute.community.{}", community.hex())
}
/// `SETTINGS.everyoneCommunity`: `1` = ignore @everyone and @here.
pub fn key_community_everyone(community: CommunityId) -> String {
    format!("notify.everyone.community.{}", community.hex())
}

/// `NotifyMode`: what a channel (or server) notifies for.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NotifyMode {
    All,
    Mentions,
    Nothing,
}

impl NotifyMode {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::All => "all",
            Self::Mentions => "mentions",
            Self::Nothing => "nothing",
        }
    }

    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "all" => Some(Self::All),
            "mentions" => Some(Self::Mentions),
            "nothing" => Some(Self::Nothing),
            _ => None,
        }
    }
}

/// `DEFAULTS`: the value of the device-wide default, `notify.default`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DefaultChoice {
    /// Direct messages and mentions (the default when nothing is stored).
    DmsMentions,
    Everything,
    Nothing,
}

impl DefaultChoice {
    pub fn as_str(self) -> &'static str {
        match self {
            Self::DmsMentions => "dms-mentions",
            Self::Everything => "everything",
            Self::Nothing => "nothing",
        }
    }

    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "dms-mentions" => Some(Self::DmsMentions),
            "everything" => Some(Self::Everything),
            "nothing" => Some(Self::Nothing),
            _ => None,
        }
    }
}

/// Where the settings are read from: the instance's [`Prefs`] in the app, a map in the tests.
pub trait SettingsSource {
    fn get(&self, key: &str) -> Option<String>;
}

impl SettingsSource for Prefs {
    fn get(&self, key: &str) -> Option<String> {
        Prefs::get::<String>(self, key)
    }
}

impl SettingsSource for HashMap<String, String> {
    fn get(&self, key: &str) -> Option<String> {
        HashMap::get(self, key).cloned()
    }
}

impl SettingsSource for BTreeMap<String, String> {
    fn get(&self, key: &str) -> Option<String> {
        BTreeMap::get(self, key).cloned()
    }
}

/// `effectiveNotifyMode`: the channel's own value first, then its server's, then the device default. `kind` is the
/// channel's: 0 a community channel, 3 a DM, 4 a group DM; without a stored default a DM notifies for all and a channel
/// for mentions.
pub fn effective_notify_mode(
    src: &dyn SettingsSource,
    channel: ChannelId,
    kind: u8,
    community: Option<CommunityId>,
) -> NotifyMode {
    if let Some(mode) = src
        .get(&key_channel_mode(channel))
        .and_then(|v| NotifyMode::parse(&v))
    {
        return mode;
    }
    if let Some(mode) = community.and_then(|c| {
        src.get(&key_community_mode(c))
            .and_then(|v| NotifyMode::parse(&v))
    }) {
        return mode;
    }
    match src
        .get(KEY_DEFAULT)
        .and_then(|v| DefaultChoice::parse(&v))
        .unwrap_or(DefaultChoice::DmsMentions)
    {
        DefaultChoice::Everything => NotifyMode::All,
        DefaultChoice::Nothing => NotifyMode::Nothing,
        DefaultChoice::DmsMentions if kind == 3 || kind == 4 => NotifyMode::All,
        DefaultChoice::DmsMentions => NotifyMode::Mentions,
    }
}

/// `isMuted`: the channel, or (given its server) the whole server, is muted.
pub fn is_muted(
    src: &dyn SettingsSource,
    channel: ChannelId,
    community: Option<CommunityId>,
) -> bool {
    src.get(&key_channel_mute(channel)).as_deref() == Some("1")
        || community.is_some_and(|c| src.get(&key_community_mute(c)).as_deref() == Some("1"))
}

/// `suppressesEveryone`: this server ignores @everyone and @here.
pub fn suppresses_everyone(src: &dyn SettingsSource, community: CommunityId) -> bool {
    src.get(&key_community_everyone(community)).as_deref() == Some("1")
}

/// The device-wide switches, read from a [`SettingsSource`].
pub struct Settings<'a>(pub &'a dyn SettingsSource);

impl Settings<'_> {
    /// `notify.desktop` is not `off`.
    pub fn desktop_on(&self) -> bool {
        self.0.get(KEY_DESKTOP).as_deref() != Some("off")
    }

    /// `notify.sounds` is not `off` (the web keeps two keys, `sound.voice` and `sound.ring`, and the Notifications row
    /// writes both; one key here).
    pub fn sounds_on(&self) -> bool {
        self.0.get(KEY_SOUNDS).as_deref() != Some("off")
    }

    pub fn default_choice(&self) -> DefaultChoice {
        self.0
            .get(KEY_DEFAULT)
            .and_then(|v| DefaultChoice::parse(&v))
            .unwrap_or(DefaultChoice::DmsMentions)
    }
}

/// Settings → Notifications' "Notify me for" value while the server has none of its own: the device default as the
/// notifier reads it (`defaultMode` in Notifications.tsx).
pub fn server_mode_shown(src: &dyn SettingsSource, community: CommunityId) -> NotifyMode {
    src.get(&key_community_mode(community))
        .and_then(|v| NotifyMode::parse(&v))
        .unwrap_or(match Settings(src).default_choice() {
            DefaultChoice::Everything => NotifyMode::All,
            DefaultChoice::Nothing => NotifyMode::Nothing,
            DefaultChoice::DmsMentions => NotifyMode::Mentions,
        })
}

// ---- the badges ----------------------------------------------------------------------------------------------------

/// What a channel row shows: a muted channel hides its unread count and keeps its mentions (badges.spec.ts).
pub fn row_counts(state: &AppState, src: &dyn SettingsSource, channel: ChannelId) -> (u32, u32) {
    let b = state.unread.get(&channel).copied().unwrap_or_default();
    let community = state.channel(channel).map(|c| c.community_id);
    if is_muted(src, channel, community) {
        (0, b.mentions)
    } else {
        (b.unread, b.mentions)
    }
}

/// What a server's rail tile shows: the sum of its channels' row counts.
pub fn rail_counts(
    state: &AppState,
    src: &dyn SettingsSource,
    community: CommunityId,
) -> (u32, u32) {
    state
        .channels
        .iter()
        .filter(|c| c.community_id == community)
        .map(|c| row_counts(state, src, c.id))
        .fold((0, 0), |(u, m), (cu, cm)| {
            (u.saturating_add(cu), m.saturating_add(cm))
        })
}

/// What the DMs tile shows: the sum of the DMs' counts (a channel the community list does not know is a DM).
pub fn dm_counts(state: &AppState, src: &dyn SettingsSource) -> (u32, u32) {
    state
        .unread
        .keys()
        .filter(|id| state.channel(**id).is_none())
        .map(|id| row_counts(state, src, *id))
        .fold((0, 0), |(u, m), (cu, cm)| {
            (u.saturating_add(cu), m.saturating_add(cm))
        })
}

/// The total the tray and dock icon show: every channel's and DM's unread count, a muted one's mentions only.
pub fn total_badge(state: &AppState, src: &dyn SettingsSource) -> u32 {
    state
        .unread
        .keys()
        .map(|id| {
            let (unread, mentions) = row_counts(state, src, *id);
            if is_muted(src, *id, state.channel(*id).map(|c| c.community_id)) {
                mentions
            } else {
                unread
            }
        })
        .fold(0u32, u32::saturating_add)
}

// ---- notices and the decision --------------------------------------------------------------------------------------

/// types.ts `Notice.kind`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NoticeKind {
    Message,
    Mention,
    Dm,
}

/// types.ts `Notice`: a new message from another user that arrived live.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Notice {
    pub channel: ChannelId,
    /// `None` for a DM.
    pub community: Option<CommunityId>,
    pub kind: NoticeKind,
    pub sender_user: Option<UserId>,
    pub sender_name: String,
    pub body: String,
    pub ts: u64,
}

/// badges.ts `NOTICE_BODY_MAX`: code points.
pub const NOTICE_BODY_MAX: usize = 200;

/// The presence choice the decision reads (`ctx.presenceChoice`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Presence {
    /// Not loaded yet: nothing notifies.
    Unknown,
    Online,
    Idle,
    Dnd,
    Invisible,
}

/// `NotifyContext`.
#[derive(Clone, Debug)]
pub struct NotifyContext {
    pub permission: Permission,
    pub presence: Presence,
    /// The window is visible and focused.
    pub visible: bool,
    /// The channel the timeline shows.
    pub on_screen: Option<ChannelId>,
    /// The own user and role ids, for a server that ignores @everyone and @here; either unset keeps every mention.
    pub me: Option<UserId>,
    pub roles: Option<Vec<[u8; 16]>>,
}

/// `mentionsDirectly`: the user or one of their roles, named (not @everyone or @here).
pub fn mentions_directly(body: &str, me: UserId, roles: &[[u8; 16]]) -> bool {
    body.contains(&format!("<@{}>", me.hex()))
        || roles
            .iter()
            .any(|r| body.contains(&format!("<@{}>", hex(r))))
}

/// L-TS-25's decision for one notice. `kind` is the channel's for the mode: 0 a community channel, 3 or 4 a DM.
pub fn should_notify(
    notice: &Notice,
    kind: u8,
    ctx: &NotifyContext,
    src: &dyn SettingsSource,
) -> bool {
    if matches!(ctx.presence, Presence::Unknown | Presence::Dnd) {
        return false;
    }
    if ctx.permission != Permission::Granted {
        return false;
    }
    // Settings → Notifications' Desktop notifications switch: `off` silences them on this device.
    if !Settings(src).desktop_on() {
        return false;
    }
    if is_muted(src, notice.channel, notice.community) {
        return false;
    }
    // The person is looking at it: the timeline shows it, so nothing pops up over it.
    if ctx.visible && ctx.on_screen == Some(notice.channel) {
        return false;
    }
    let mode = effective_notify_mode(src, notice.channel, kind, notice.community);
    if mode == NotifyMode::All {
        return true;
    }
    // A server that ignores @everyone and @here turns a broadcast-only mention into a plain message. It fails open: with
    // the own user or roles unknown, a mention is never demoted.
    let broadcast_only = notice.kind == NoticeKind::Mention
        && notice
            .community
            .is_some_and(|c| suppresses_everyone(src, c))
        && matches!((ctx.me, &ctx.roles), (Some(me), Some(roles)) if !mentions_directly(&notice.body, me, roles));
    mode == NotifyMode::Mentions
        && !broadcast_only
        && matches!(notice.kind, NoticeKind::Mention | NoticeKind::Dm)
}

/// `(key, vars)` to text: the desktop passes `strings::tf`, so the client holds no copy of its own.
pub type Text = Arc<dyn Fn(&str, &[(&str, &str)]) -> String + Send + Sync>;

/// What an OS notification says. Task 22 owns this type and may add fields; Task 18 (ring) delivers through it.
/// Build one with [`Notification::new`]: the type grows.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Notification {
    pub kind: NotificationKind,
    pub title: String,
    pub body: String,
    /// One per channel (`dilla:<channel>`) or call (`dilla-ring-<call>-<ts>`): a later one with the same tag replaces it.
    pub tag: String,
    /// What a click opens.
    pub channel: Option<ChannelId>,
}

impl Notification {
    /// A notification with no tag and no target.
    pub fn new(kind: NotificationKind, title: impl Into<String>, body: impl Into<String>) -> Self {
        Self {
            kind,
            title: title.into(),
            body: body.into(),
            tag: String::new(),
            channel: None,
        }
    }
}

/// Why a notification is shown.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NotificationKind {
    Mention,
    Dm,
    Ring,
}

/// Where notifications go: an in-app recording implementation in this plan (Task 22), the OS in the next. Task 18's
/// ring notifications go through it. Fixed signature: change it only with the controller.
pub trait Notifier: Send + Sync {
    fn deliver(&self, notification: &Notification);
}

/// One notification the recording notifier holds, and whether it was closed (clicked).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Shown {
    pub notification: Notification,
    pub closed: bool,
}

/// The in-app notifier: records what would be shown, in order (the e2e's `notificationsShown`). A notification with a
/// tag already shown replaces it, as the browser does.
#[derive(Clone, Debug, Default)]
pub struct RecordingNotifier {
    shown: Arc<Mutex<Vec<Shown>>>,
}

impl RecordingNotifier {
    pub fn new() -> Self {
        Self::default()
    }

    /// Everything shown so far, oldest first.
    pub fn shown(&self) -> Vec<Shown> {
        self.lock().clone()
    }

    /// The tags shown so far.
    pub fn tags(&self) -> Vec<String> {
        self.lock()
            .iter()
            .map(|s| s.notification.tag.clone())
            .collect()
    }

    /// A click on the `index`th notification (`clickNotification`): closes it and answers the channel it opens.
    pub fn click(&self, index: usize) -> Option<ChannelId> {
        let mut shown = self.lock();
        let item = shown.get_mut(index)?;
        item.closed = true;
        item.notification.channel
    }

    /// Forgets everything shown (a release: titles and bodies name people and messages).
    pub fn clear(&self) {
        self.lock().clear();
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Vec<Shown>> {
        self.shown
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
    }
}

impl Notifier for RecordingNotifier {
    fn deliver(&self, notification: &Notification) {
        let mut shown = self.lock();
        let entry = Shown {
            notification: notification.clone(),
            closed: false,
        };
        match (!notification.tag.is_empty())
            .then(|| {
                shown
                    .iter()
                    .position(|s| s.notification.tag == notification.tag)
            })
            .flatten()
        {
            Some(at) => shown[at] = entry,
            None => {
                shown.push(entry);
                // A ring buffer: the newest `RECORDED_MAX`, the oldest dropped first.
                let over = shown.len().saturating_sub(RECORDED_MAX);
                shown.drain(..over);
            }
        }
    }
}

/// The most notifications [`RecordingNotifier`] keeps.
pub const RECORDED_MAX: usize = 64;

/// `noticeTitle`: `notify.title.dm` for a DM notice, else `notify.title.channel`; an unknown name reads as the first 8
/// hex digits of its id.
pub fn notice_title(
    notice: &Notice,
    channel_name: Option<&str>,
    server_name: Option<&str>,
    text: &Text,
) -> String {
    match notice.community {
        None => {
            let sender = if notice.sender_name.is_empty() {
                notice
                    .sender_user
                    .map_or_else(|| notice.channel.hex(), |u| u.hex())[..8]
                    .to_owned()
            } else {
                notice.sender_name.clone()
            };
            text("notify.title.dm", &[("sender", &sender)])
        }
        Some(community) => {
            let channel =
                channel_name.map_or_else(|| notice.channel.hex()[..8].to_owned(), str::to_owned);
            let server = server_name.map_or_else(|| community.hex()[..8].to_owned(), str::to_owned);
            text(
                "notify.title.channel",
                &[("channel", &channel), ("server", &server)],
            )
        }
    }
}

/// `plainBody`: the body with `<@id>` tokens as the names the timeline shows. A name the state does not know reads as
/// `shell.message.mentionUnknown`; a role as `shell.message.mentionRole`.
pub fn plain_body(
    body: &str,
    state: &AppState,
    community: Option<CommunityId>,
    text: &Text,
) -> String {
    plain_body_with(body, state, community, text, &|_| None)
}

/// [`plain_body`] with the names of the roles `role_name` knows (hex id to name, from Task 14's role slice): a known
/// role reads as `mention.role`, as the web shows it.
pub fn plain_body_with(
    body: &str,
    state: &AppState,
    community: Option<CommunityId>,
    text: &Text,
    role_name: &dyn Fn(&str) -> Option<String>,
) -> String {
    let known = |id: &str| {
        let members: Vec<&crate::state::Member> = match community {
            Some(c) => state.members.get(&c).into_iter().flatten().collect(),
            None => state.members.values().flatten().collect(),
        };
        let found = members.iter().find(|m| m.user_id.hex() == id);
        if let Some(m) = found {
            return format!(
                "@{}",
                if m.display.is_empty() {
                    &m.username
                } else {
                    &m.display
                }
            );
        }
        if state.account.user_id.is_some_and(|u| u.hex() == id) {
            return format!("@{}", state.account.username);
        }
        if let Some(name) = role_name(id) {
            return text("mention.role", &[("name", &name)]);
        }
        let is_role = members
            .iter()
            .any(|m| m.role_ids.iter().any(|r| hex(r) == id));
        if is_role {
            text("shell.message.mentionRole", &[])
        } else {
            text("shell.message.mentionUnknown", &[("id", &id[..8])])
        }
    };
    let mut out = String::with_capacity(body.len());
    let mut rest = body;
    while let Some(open) = rest.find("<@") {
        out.push_str(&rest[..open]);
        let after = &rest[open + 2..];
        let token = after.find('>').map(|close| &after[..close]).filter(|t| {
            *t == "everyone"
                || *t == "here"
                || (t.len() == 32 && t.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f')))
        });
        match token {
            Some("everyone") => out.push_str("@everyone"),
            Some("here") => out.push_str("@here"),
            Some(id) => out.push_str(&known(id)),
            None => {
                out.push_str("<@");
                rest = after;
                continue;
            }
        }
        rest = &after[token.map_or(0, str::len) + 1..];
    }
    out.push_str(rest);
    out
}

/// The notification a notice raises, or `None` when [`should_notify`] says no.
#[allow(
    clippy::too_many_arguments,
    reason = "the decision's inputs, as the web's"
)]
pub fn notification_for(
    notice: &Notice,
    kind: u8,
    ctx: &NotifyContext,
    src: &dyn SettingsSource,
    state: &AppState,
    text: &Text,
) -> Option<Notification> {
    if !should_notify(notice, kind, ctx, src) {
        return None;
    }
    let channel_name = state.channel(notice.channel).map(|c| c.name.as_str());
    let server_name = notice
        .community
        .and_then(|id| state.communities.iter().find(|c| c.id == id))
        .map(|c| c.name.as_str());
    let title = notice_title(notice, channel_name, server_name, text);
    let body = plain_body(&notice.body, state, notice.community, text);
    Some(Notification {
        kind: if notice.community.is_none() {
            NotificationKind::Dm
        } else {
            NotificationKind::Mention
        },
        title,
        body,
        tag: format!("dilla:{}", notice.channel.hex()),
        channel: Some(notice.channel),
    })
}

/// An incoming DM ring (`Ring` in the web's types), as `ringNotification` reads it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RingInput {
    pub channel: ChannelId,
    pub call_id: String,
    pub ring_ts: u64,
    pub caller_name: String,
    /// Already in a call.
    pub busy: bool,
    /// The DM is muted: the ring is shown in the app and makes no sound.
    pub quiet: bool,
    /// The DM's kind for its notify mode: 3 a DM, 4 a group DM.
    pub kind: u8,
    /// The server whose mute and mode apply, `None` for a DM.
    pub community: Option<CommunityId>,
}

/// The notification of an incoming DM ring, or `None` when none may show: only while the window is hidden (a visible one
/// has the banner), only with the permission granted (a ring never asks for it), never for a busy or quiet ring, and never
/// when the DM's notify mode is nothing. One per call (`tag`); the caller closes it when the ring ends.
pub fn ring_notification(
    ring: &RingInput,
    ctx: &NotifyContext,
    src: &dyn SettingsSource,
    text: &Text,
) -> Option<Notification> {
    if matches!(ctx.presence, Presence::Unknown | Presence::Dnd) {
        return None;
    }
    if ctx.permission != Permission::Granted || ctx.visible {
        return None;
    }
    if ring.busy || ring.quiet || is_muted(src, ring.channel, ring.community) {
        return None;
    }
    if effective_notify_mode(src, ring.channel, ring.kind, ring.community) == NotifyMode::Nothing {
        return None;
    }
    Some(Notification {
        kind: NotificationKind::Ring,
        title: text("call.ring.notifyTitle", &[("name", &ring.caller_name)]),
        body: text("call.ring.notifyBody", &[]),
        tag: format!("dilla-ring-{}-{}", ring.call_id, ring.ring_ts),
        channel: Some(ring.channel),
    })
}

// ---- the notices a state change raises ------------------------------------------------------------------------------

/// The worker's `reportActivity`: remembers the newest stored message of each channel it has seen and answers the notices
/// of the messages that are newer, from another user, readable, beyond the high-water mark of their channel. A channel's mark is seeded
/// from what its timeline holds the first time it appears, so history is never replayed, however late it loads; but a
/// message in it received at or after the tracker's start (`floor`, the web's `noticeFloor`, on the instance's clock) is live: the first message
/// of a DM opened by its peer reaches this device in the same snapshot as the DM itself (Task 28,
/// `tests/notify::dm_opened_by_peer_appears_badges_notifies_live`).
#[derive(Clone, Debug)]
pub struct NoticeTracker {
    marks: HashMap<ChannelId, u64>,
    /// Unix seconds on the instance's clock (the rows' `recv_ts`): a first-seen channel's messages received before it are
    /// history.
    floor: u64,
    /// The gateway readies the floor was last set at (`ConnectionState::readies`).
    readies: u64,
}

impl Default for NoticeTracker {
    fn default() -> Self {
        Self::new()
    }
}

impl NoticeTracker {
    /// A tracker starting now on this device's clock ([`Notify::start`] uses the instance's).
    pub fn new() -> Self {
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_secs());
        Self::with_floor(now)
    }

    /// A tracker whose start is `floor` (unix seconds).
    pub fn with_floor(floor: u64) -> Self {
        Self {
            marks: HashMap::new(),
            floor,
            readies: 0,
        }
    }

    /// controller.ts `noticeFloor = serverNowS()` at every ready (t28-bot-review I4, M6): once the gateway is ready
    /// again (`readies`, the state's count, grew), what this ready catches up was not delivered live, so a channel first
    /// seen from now on holds history up to `now` (unix seconds, the instance's clock).
    pub fn ready(&mut self, readies: u64, now: u64) {
        if readies != self.readies {
            self.readies = readies;
            self.floor = now;
        }
    }

    /// The notices `state` holds beyond what earlier calls saw, oldest first.
    pub fn observe(&mut self, state: &AppState) -> Vec<Notice> {
        let own = state.account.user_id;
        let mut out = Vec::new();
        for (channel, timeline) in &state.timelines {
            let seeded = self.marks.contains_key(channel);
            let mark = self.marks.get(channel).copied().unwrap_or(0);
            let community = state.channel(*channel).map(|c| c.community_id);
            let mut newest = mark;
            for item in &timeline.items {
                let Some(seq) = item.seq else { continue };
                newest = newest.max(seq);
                // The first time a channel appears, what it holds from before the start is history.
                if (!seeded && item.ts < self.floor)
                    || seq <= mark
                    || item.state != TimelineItemState::Ok
                    || item.own
                    || item.sender_user.is_none()
                    || item.sender_user == own
                {
                    continue;
                }
                let kind = if community.is_none() {
                    NoticeKind::Dm
                } else if item.mention {
                    NoticeKind::Mention
                } else {
                    NoticeKind::Message
                };
                out.push(Notice {
                    channel: *channel,
                    community,
                    kind,
                    sender_user: item.sender_user,
                    sender_name: sender_name(state, community, item.sender_user),
                    body: item.body.chars().take(NOTICE_BODY_MAX).collect(),
                    ts: item.ts,
                });
            }
            self.marks.insert(*channel, newest);
        }
        out.sort_by_key(|n| (n.ts, n.channel));
        out
    }
}

fn sender_name(state: &AppState, community: Option<CommunityId>, user: Option<UserId>) -> String {
    let Some(user) = user else {
        return String::new();
    };
    let lists: Vec<&Vec<crate::state::Member>> = match community {
        Some(c) => state.members.get(&c).into_iter().collect(),
        None => state.members.values().collect(),
    };
    lists
        .into_iter()
        .flatten()
        .find(|m| m.user_id == user)
        .map(|m| {
            if m.display.is_empty() {
                m.username.clone()
            } else {
                m.display.clone()
            }
        })
        .unwrap_or_default()
}

/// What the window tells the notifier.
pub trait NotifyEnv: Send + Sync {
    /// The window is visible and focused.
    fn visible(&self) -> bool;
    /// The channel the timeline shows.
    fn on_screen(&self) -> Option<ChannelId>;
    fn presence(&self) -> Presence;
}

/// The channel kind for a notice's mode (`kind` of `shouldNotify`): a community channel is 0, anything else a DM. The slice
/// does not tell a group DM from a DM yet (Task 13's `dms`), so a DM reads as 3.
fn kind_of(notice: &Notice) -> u8 {
    if notice.community.is_some() { 0 } else { 3 }
}

/// Follows the state and hands every notification [`should_notify`] allows to the notifier: `startNotifier`.
pub struct Engine {
    tracker: NoticeTracker,
    notifier: Arc<dyn Notifier>,
    env: Arc<dyn NotifyEnv>,
    text: Text,
}

impl Engine {
    pub fn new(notifier: Arc<dyn Notifier>, env: Arc<dyn NotifyEnv>, text: Text) -> Self {
        Self {
            tracker: NoticeTracker::new(),
            notifier,
            env,
            text,
        }
    }

    /// Handles one snapshot; answers what it delivered.
    pub fn on_state(&mut self, state: &AppState, src: &dyn SettingsSource) -> Vec<Notification> {
        let mut delivered = Vec::new();
        for notice in self.tracker.observe(state) {
            let me = state.account.user_id;
            let roles = notice.community.and_then(|c| {
                let own = state
                    .members
                    .get(&c)?
                    .iter()
                    .find(|m| Some(m.user_id) == me)?;
                Some(own.role_ids.clone())
            });
            let ctx = NotifyContext {
                permission: state.notify.permission,
                presence: self.env.presence(),
                visible: self.env.visible(),
                on_screen: self.env.on_screen(),
                me,
                roles,
            };
            if let Some(n) =
                notification_for(&notice, kind_of(&notice), &ctx, src, state, &self.text)
            {
                self.notifier.deliver(&n);
                delivered.push(n);
            }
        }
        delivered
    }
}

/// A running [`Engine`]: stops when dropped or on [`NotifyTask::stop`].
pub struct NotifyTask {
    stop: watch::Sender<bool>,
}

impl NotifyTask {
    pub fn stop(&self) {
        let _ = self.stop.send(true);
    }
}

impl Drop for NotifyTask {
    fn drop(&mut self) {
        self.stop();
    }
}

// ---- the handle -----------------------------------------------------------------------------------------------------

/// The notify area's handle for notify: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Notify {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Notify`] handle.
    pub fn notify(&self) -> Notify {
        Notify {
            inner: self.inner().clone(),
        }
    }
}

impl Notify {
    /// The instance's preferences as a settings source.
    pub fn settings(&self) -> Prefs {
        self.inner.prefs().clone()
    }

    /// What channel row `channel` shows now.
    pub fn row_counts(&self, channel: ChannelId) -> (u32, u32) {
        row_counts(&self.inner.snapshot(), self.inner.prefs(), channel)
    }

    /// The total the tray and dock icon show.
    pub fn total_badge(&self) -> u32 {
        total_badge(&self.inner.snapshot(), self.inner.prefs())
    }

    fn write(&self, key: &str, value: Option<&str>) -> Result<(), PrefsError> {
        match value {
            Some(v) => self.inner.prefs().set(key, &v.to_owned())?,
            None => self.inner.prefs().remove(key)?,
        }
        // The screens redraw on a state change.
        self.inner.publish(|s| s.notify.revision += 1);
        Ok(())
    }

    /// The device-wide default (the DM list head's menu, `signalSettings.menu.*`).
    pub fn set_default(&self, choice: DefaultChoice) -> Result<(), PrefsError> {
        self.write(KEY_DEFAULT, Some(choice.as_str()))
    }

    /// A channel's own mode; `None` clears it (follows its server and the default).
    pub fn set_channel_mode(
        &self,
        channel: ChannelId,
        mode: Option<NotifyMode>,
    ) -> Result<(), PrefsError> {
        self.write(&key_channel_mode(channel), mode.map(NotifyMode::as_str))
    }

    /// Mutes or unmutes a channel (the row menu's Mute).
    pub fn set_channel_muted(&self, channel: ChannelId, muted: bool) -> Result<(), PrefsError> {
        self.write(&key_channel_mute(channel), muted.then_some("1"))
    }

    /// A server's mode (Settings → Notifications' "Notify me for").
    pub fn set_community_mode(
        &self,
        community: CommunityId,
        mode: Option<NotifyMode>,
    ) -> Result<(), PrefsError> {
        self.write(&key_community_mode(community), mode.map(NotifyMode::as_str))
    }

    pub fn set_community_muted(
        &self,
        community: CommunityId,
        muted: bool,
    ) -> Result<(), PrefsError> {
        self.write(&key_community_mute(community), muted.then_some("1"))
    }

    /// Whether this server's @everyone and @here notify (the switch is on while they do).
    pub fn set_everyone_notifies(
        &self,
        community: CommunityId,
        notifies: bool,
    ) -> Result<(), PrefsError> {
        self.write(
            &key_community_everyone(community),
            (!notifies).then_some("1"),
        )
    }

    /// Notifications on this device. Switching on while the permission is not asked yet asks for it.
    pub fn set_desktop(&self, on: bool) -> Result<(), PrefsError> {
        self.write(KEY_DESKTOP, (!on).then_some("off"))?;
        if on && self.inner.snapshot().notify.permission == Permission::Default {
            self.request_permission();
        }
        Ok(())
    }

    /// The join, leave and ring sounds.
    pub fn set_sounds(&self, on: bool) -> Result<(), PrefsError> {
        self.write(KEY_SOUNDS, (!on).then_some("off"))
    }

    /// `askPermission`: the in-app notifier needs no OS permission, so it is granted. The OS delivery of the next plan asks
    /// the system here.
    pub fn request_permission(&self) {
        self.inner.publish(|s| {
            if s.notify.permission == Permission::Default {
                s.notify.permission = Permission::Granted;
            }
        });
    }

    /// Starts the notifier: from now on, a new message that [`should_notify`] allows goes to `notifier`. Nothing held
    /// before this call is replayed.
    pub fn start(
        &self,
        client: &Client,
        notifier: Arc<dyn Notifier>,
        env: Arc<dyn NotifyEnv>,
        text: Text,
    ) -> NotifyTask {
        let (stop, mut stopped) = watch::channel(false);
        let mut state = client.state();
        let prefs = self.inner.prefs().clone();
        let mut engine = Engine::new(notifier, env, text);
        // The floor is on the instance's clock, as the rows' receive times are (controller.ts `noticeFloor =
        // serverNowS()`).
        let api = self.inner.api().clone();
        engine.tracker = NoticeTracker::with_floor(api.server_now_s());
        // The first value only sets the marks; a ready it already counts set no floor later than this one.
        let first = state.borrow_and_update().clone();
        engine.tracker.readies = first.connection.readies;
        engine.tracker.observe(&first);
        drop(self.inner.spawn(async move {
            loop {
                tokio::select! {
                    _ = stopped.changed() => break,
                    changed = state.changed() => {
                        if changed.is_err() {
                            break;
                        }
                        let snapshot = state.borrow_and_update().clone();
                        engine.tracker.ready(snapshot.connection.readies, api.server_now_s());
                        engine.on_state(&snapshot, &prefs);
                    }
                }
            }
            Ok::<(), ClientError>(())
        }));
        NotifyTask { stop }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::{
        AccountState, BadgeState, Channel, ChannelGroupState, Community, Member, Timeline,
        TimelineItem,
    };

    /// Integration 3, re-review minor: the in-app notifier keeps the newest `RECORDED_MAX` notifications (oldest first,
    /// the oldest dropped), and a release forgets them all (their titles and bodies name people and messages).
    #[test]
    fn the_recorder_is_bounded_and_forgets_on_release() {
        let n = RecordingNotifier::new();
        for i in 0..RECORDED_MAX + 5 {
            let mut x = Notification::new(NotificationKind::Mention, format!("t{i}"), "b");
            x.tag = format!("tag-{i}");
            n.deliver(&x);
        }
        let shown = n.shown();
        assert_eq!(shown.len(), RECORDED_MAX);
        assert_eq!(shown[0].notification.title, "t5", "the oldest went first");
        n.clear();
        assert!(n.shown().is_empty());
    }

    const COMMUNITY: CommunityId = CommunityId([0xa1; 16]);
    const GENERAL: ChannelId = ChannelId([0xc3; 16]);
    const RANDOM: ChannelId = ChannelId([0xd4; 16]);
    const DM: ChannelId = ChannelId([0x9a; 16]);
    const ME: UserId = UserId([0x11; 16]);
    const BOB: UserId = UserId([0x28; 16]);
    const ROLE: [u8; 16] = [0xf7; 16];

    fn text() -> Text {
        Arc::new(|key, vars| {
            let src = match key {
                "notify.title.channel" => "#{channel} · {server}",
                "notify.title.dm" => "{sender}",
                "call.ring.notifyTitle" => "{name} is calling",
                "call.ring.notifyBody" => "Open dilla to answer.",
                "activity.dm" => "Direct message",
                "activity.newCount" => "{n} new",
                "shell.message.mentionRole" => "@role",
                "mention.role" => "@{name}",
                "shell.message.mentionUnknown" => "@{id}",
                other => panic!("no string {other}"),
            };
            vars.iter().fold(src.to_owned(), |s, (k, v)| {
                s.replace(&format!("{{{k}}}"), v)
            })
        })
    }

    fn settings(pairs: &[(&str, &str)]) -> HashMap<String, String> {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect()
    }

    fn notice(channel: ChannelId, kind: NoticeKind) -> Notice {
        Notice {
            channel,
            community: (kind != NoticeKind::Dm).then_some(COMMUNITY),
            kind,
            sender_user: Some(BOB),
            sender_name: "bob".into(),
            body: "hi".into(),
            ts: 1_790_000_000,
        }
    }

    fn ctx() -> NotifyContext {
        NotifyContext {
            permission: Permission::Granted,
            presence: Presence::Online,
            visible: true,
            on_screen: Some(GENERAL),
            me: None,
            roles: None,
        }
    }

    fn channel(id: ChannelId, name: &str) -> Channel {
        Channel {
            id,
            community_id: COMMUNITY,
            kind: 0,
            mode: 0,
            name: name.into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Active,
        }
    }

    fn member(user_id: UserId, username: &str, display: &str, role_ids: Vec<[u8; 16]>) -> Member {
        Member {
            user_id,
            username: username.into(),
            display: display.into(),
            kind: 0,
            role_ids,
            status_msg: String::new(),
            bio: String::new(),
        }
    }

    fn world() -> AppState {
        AppState {
            account: AccountState {
                user_id: Some(ME),
                username: "ada".into(),
                ..AccountState::default()
            },
            communities: vec![Community {
                id: COMMUNITY,
                name: "Midgard".into(),
                owner: ME,
                policy_version: 0,
            }],
            channels: vec![channel(GENERAL, "general"), channel(RANDOM, "random")],
            members: HashMap::from([(
                COMMUNITY,
                vec![
                    member(ME, "ada", "Ada L", vec![ROLE]),
                    member(BOB, "bob", "", vec![]),
                ],
            )]),
            ..AppState::default()
        }
    }

    type Row = (
        &'static str,
        Notice,
        u8,
        NotifyContext,
        Vec<(String, String)>,
        bool,
    );

    // L-TS-25 (notify.test.ts `shouldNotify`): the same rows, the same answers.
    #[test]
    fn should_notify_rows() {
        let m = |s: &str, v: &str| (s.to_owned(), v.to_owned());
        let hidden = |presence| NotifyContext { presence, ..ctx() };
        let rows: Vec<Row> = vec![
            (
                "a DM by default",
                notice(DM, NoticeKind::Dm),
                3,
                ctx(),
                vec![],
                true,
            ),
            (
                "a DM in Do Not Disturb",
                notice(DM, NoticeKind::Dm),
                3,
                hidden(Presence::Dnd),
                vec![],
                false,
            ),
            (
                "a DM before the saved choice loads",
                notice(DM, NoticeKind::Dm),
                3,
                hidden(Presence::Unknown),
                vec![],
                false,
            ),
            (
                "a mention in Do Not Disturb",
                notice(RANDOM, NoticeKind::Mention),
                0,
                hidden(Presence::Dnd),
                vec![],
                false,
            ),
            (
                "a group DM by default",
                notice(DM, NoticeKind::Dm),
                4,
                ctx(),
                vec![],
                true,
            ),
            (
                "a plain message by default",
                notice(RANDOM, NoticeKind::Message),
                0,
                ctx(),
                vec![],
                false,
            ),
            (
                "a mention by default",
                notice(RANDOM, NoticeKind::Mention),
                0,
                ctx(),
                vec![],
                true,
            ),
            (
                "a mention in the channel on screen",
                notice(GENERAL, NoticeKind::Mention),
                0,
                ctx(),
                vec![],
                false,
            ),
            (
                "a DM on screen",
                notice(DM, NoticeKind::Dm),
                3,
                NotifyContext {
                    on_screen: Some(DM),
                    ..ctx()
                },
                vec![],
                false,
            ),
            (
                "a mention on screen while the window is hidden",
                notice(GENERAL, NoticeKind::Mention),
                0,
                NotifyContext {
                    visible: false,
                    ..ctx()
                },
                vec![],
                true,
            ),
            (
                "without permission",
                notice(RANDOM, NoticeKind::Mention),
                0,
                NotifyContext {
                    permission: Permission::Default,
                    ..ctx()
                },
                vec![],
                false,
            ),
            (
                "when denied",
                notice(RANDOM, NoticeKind::Mention),
                0,
                NotifyContext {
                    permission: Permission::Denied,
                    ..ctx()
                },
                vec![],
                false,
            ),
            (
                "without the API",
                notice(RANDOM, NoticeKind::Mention),
                0,
                NotifyContext {
                    permission: Permission::Unsupported,
                    ..ctx()
                },
                vec![],
                false,
            ),
            (
                "in a muted channel",
                notice(RANDOM, NoticeKind::Mention),
                0,
                ctx(),
                vec![m(&key_channel_mute(RANDOM), "1")],
                false,
            ),
            (
                "in a muted DM",
                notice(DM, NoticeKind::Dm),
                3,
                ctx(),
                vec![m(&key_channel_mute(DM), "1")],
                false,
            ),
            (
                "a message when the default is everything",
                notice(RANDOM, NoticeKind::Message),
                0,
                ctx(),
                vec![m(KEY_DEFAULT, "everything")],
                true,
            ),
            (
                "a mention when the default is nothing",
                notice(RANDOM, NoticeKind::Mention),
                0,
                ctx(),
                vec![m(KEY_DEFAULT, "nothing")],
                false,
            ),
            (
                "a DM when the default is nothing",
                notice(DM, NoticeKind::Dm),
                3,
                ctx(),
                vec![m(KEY_DEFAULT, "nothing")],
                false,
            ),
            (
                "a channel set to all under nothing",
                notice(RANDOM, NoticeKind::Message),
                0,
                ctx(),
                vec![
                    m(KEY_DEFAULT, "nothing"),
                    m(&key_channel_mode(RANDOM), "all"),
                ],
                true,
            ),
            (
                "a DM set to nothing",
                notice(DM, NoticeKind::Dm),
                3,
                ctx(),
                vec![m(&key_channel_mode(DM), "nothing")],
                false,
            ),
            (
                "a DM set to mentions",
                notice(DM, NoticeKind::Dm),
                3,
                ctx(),
                vec![m(&key_channel_mode(DM), "mentions")],
                true,
            ),
            (
                "a plain message in a channel set to mentions",
                notice(RANDOM, NoticeKind::Message),
                0,
                ctx(),
                vec![
                    m(KEY_DEFAULT, "everything"),
                    m(&key_channel_mode(RANDOM), "mentions"),
                ],
                false,
            ),
        ];
        for (name, n, kind, c, pairs, want) in rows {
            let src: HashMap<String, String> = pairs.into_iter().collect();
            assert_eq!(should_notify(&n, kind, &c, &src), want, "{name}");
        }
    }

    #[test]
    fn the_desktop_switch_silences_this_device() {
        let n = notice(RANDOM, NoticeKind::Mention);
        let c = NotifyContext {
            visible: false,
            ..ctx()
        };
        assert!(should_notify(&n, 0, &c, &settings(&[])));
        assert!(!should_notify(
            &n,
            0,
            &c,
            &settings(&[(KEY_DESKTOP, "off")])
        ));
        assert!(should_notify(&n, 0, &c, &settings(&[(KEY_DESKTOP, "on")])));
    }

    #[test]
    fn per_server_modes_mutes_and_ignored_everyone() {
        let server = |v: &str| (key_community_mode(COMMUNITY), v.to_owned());
        let mention = notice(RANDOM, NoticeKind::Mention);
        // a server set to nothing silences a channel with no override; a channel override still wins
        let src: HashMap<_, _> = [server("nothing")].into();
        assert!(!should_notify(&mention, 0, &ctx(), &src));
        let src: HashMap<_, _> =
            [server("nothing"), (key_channel_mode(RANDOM), "all".into())].into();
        assert!(should_notify(&mention, 0, &ctx(), &src));
        // a server set to all notifies a plain message
        let src: HashMap<_, _> = [server("all")].into();
        assert!(should_notify(
            &notice(RANDOM, NoticeKind::Message),
            0,
            &ctx(),
            &src
        ));
        // a muted server notifies nothing
        let src: HashMap<_, _> = [(key_community_mute(COMMUNITY), "1".to_owned())].into();
        assert!(!should_notify(&mention, 0, &ctx(), &src));

        // @everyone ignored: a broadcast-only mention is a plain message, a direct one stays
        let ignore: HashMap<_, _> = [(key_community_everyone(COMMUNITY), "1".to_owned())].into();
        let with = |body: String| Notice {
            body,
            ..mention.clone()
        };
        let c = NotifyContext {
            me: Some(ME),
            roles: Some(vec![]),
            ..ctx()
        };
        assert!(!should_notify(
            &with("hey <@everyone>".into()),
            0,
            &c,
            &ignore
        ));
        assert!(!should_notify(&with("hey <@here>".into()), 0, &c, &ignore));
        let direct = format!("hey <@everyone> <@{}>", ME.hex());
        assert!(should_notify(&with(direct), 0, &c, &ignore));
        assert!(should_notify(
            &with("hey <@everyone>".into()),
            0,
            &c,
            &settings(&[])
        ));
        let role_body = format!("<@everyone> <@{}>", hex(&ROLE));
        let with_role = NotifyContext {
            roles: Some(vec![ROLE]),
            ..c.clone()
        };
        assert!(should_notify(
            &with(role_body.clone()),
            0,
            &with_role,
            &ignore
        ));
        assert!(!should_notify(&with(role_body), 0, &c, &ignore));
        // as a plain message it follows the message rules: all notifies it
        let both: HashMap<_, _> = [
            (key_community_everyone(COMMUNITY), "1".to_owned()),
            server("all"),
        ]
        .into();
        assert!(should_notify(&with("hey <@everyone>".into()), 0, &c, &both));
        // it fails open: the own user or roles unknown keeps the mention
        assert!(should_notify(
            &with("hey <@everyone>".into()),
            0,
            &ctx(),
            &ignore
        ));
    }

    #[test]
    fn titles_name_the_channel_and_server_or_the_sender() {
        let t = text();
        let channel_notice = notice(RANDOM, NoticeKind::Mention);
        assert_eq!(
            notice_title(&channel_notice, Some("random"), Some("Midgard"), &t),
            "#random · Midgard"
        );
        assert_eq!(
            notice_title(&notice(DM, NoticeKind::Dm), None, None, &t),
            "bob"
        );
        assert_eq!(
            notice_title(&channel_notice, None, None, &t),
            "#d4d4d4d4 · a1a1a1a1"
        );
        let unnamed = Notice {
            sender_name: String::new(),
            ..notice(DM, NoticeKind::Dm)
        };
        assert_eq!(notice_title(&unnamed, None, None, &t), "28282828");
    }

    #[test]
    fn a_mention_shows_as_the_name_it_stands_for_never_the_raw_token() {
        let state = world();
        let t = text();
        let body = format!("<@{}> look, <@{}> and <@everyone>", ME.hex(), BOB.hex());
        assert_eq!(
            plain_body(&body, &state, Some(COMMUNITY), &t),
            "@Ada L look, @bob and @everyone"
        );
        let dm = format!("ping <@{}> <@{}>", ME.hex(), "ee".repeat(16));
        assert_eq!(plain_body(&dm, &state, None, &t), "ping @Ada L @eeeeeeee");
        let role = format!("<@{}>", hex(&ROLE));
        assert_eq!(plain_body(&role, &state, Some(COMMUNITY), &t), "@role");
        assert_eq!(
            plain_body("a <@ b <@nothing> c", &state, None, &t),
            "a <@ b <@nothing> c"
        );
    }

    #[test]
    fn a_known_role_reads_as_mention_role() {
        let state = world();
        let t = text();
        let body = format!("<@{}>", hex(&ROLE));
        let names = |id: &str| (id == hex(&ROLE)).then(|| "mods".to_owned());
        assert_eq!(
            plain_body_with(&body, &state, Some(COMMUNITY), &t, &names),
            "@mods"
        );
    }

    #[test]
    fn the_notification_carries_the_title_and_the_plain_body_as_the_web_shows_them() {
        let state = world();
        let t = text();
        let n = Notice {
            body: format!("<@{}> look", ME.hex()),
            ..notice(DM, NoticeKind::Dm)
        };
        let c = NotifyContext {
            on_screen: None,
            ..ctx()
        };
        let shown = notification_for(&n, 3, &c, &settings(&[]), &state, &t).unwrap();
        assert_eq!(
            (shown.title.as_str(), shown.body.as_str()),
            ("bob", "@Ada L look")
        );
        assert_eq!(shown.tag, format!("dilla:{}", DM.hex()));
        assert_eq!(shown.channel, Some(DM));
    }

    fn ring() -> RingInput {
        RingInput {
            channel: DM,
            call_id: "c1".into(),
            ring_ts: 1_900_000_030,
            caller_name: "bob".into(),
            busy: false,
            quiet: false,
            kind: 3,
            community: None,
        }
    }

    // notify.test.ts `ringNotification`
    #[test]
    fn a_ring_raises_one_tagged_notification_unless_something_forbids_it() {
        let t = text();
        let hidden = NotifyContext {
            visible: false,
            ..ctx()
        };
        let n = ring_notification(&ring(), &hidden, &settings(&[]), &t).unwrap();
        assert_eq!(
            (n.title.as_str(), n.body.as_str()),
            ("bob is calling", "Open dilla to answer.")
        );
        assert_eq!(n.tag, "dilla-ring-c1-1900000030");
        assert_eq!(n.kind, NotificationKind::Ring);
        let none = |r: RingInput, c: &NotifyContext, s: &HashMap<String, String>| {
            ring_notification(&r, c, s, &t).is_none()
        };
        let with = |presence| NotifyContext {
            presence,
            ..hidden.clone()
        };
        assert!(none(ring(), &with(Presence::Unknown), &settings(&[])));
        assert!(none(ring(), &with(Presence::Dnd), &settings(&[])));
        assert!(none(
            RingInput {
                busy: true,
                ..ring()
            },
            &hidden,
            &settings(&[])
        ));
        assert!(none(
            RingInput {
                quiet: true,
                ..ring()
            },
            &hidden,
            &settings(&[])
        ));
        assert!(none(
            ring(),
            &hidden,
            &settings(&[(&key_channel_mode(DM), "nothing")])
        ));
        assert!(
            none(ring(), &ctx(), &settings(&[])),
            "a visible window has the banner"
        );
        for permission in [
            Permission::Default,
            Permission::Denied,
            Permission::Unsupported,
        ] {
            let c = NotifyContext {
                permission,
                ..hidden.clone()
            };
            assert!(none(ring(), &c, &settings(&[])));
        }
        for pair in [
            (key_community_mute(COMMUNITY), "1"),
            (key_community_mode(COMMUNITY), "nothing"),
        ] {
            let s = settings(&[(&pair.0, pair.1)]);
            let r = RingInput {
                community: Some(COMMUNITY),
                ..ring()
            };
            assert!(none(r, &hidden, &s));
        }
    }

    fn item(seq: u64, sender: UserId, ts: u64, body: &str, mention: bool) -> TimelineItem {
        TimelineItem {
            key: format!("s{seq}"),
            state: TimelineItemState::Ok,
            reason: String::new(),
            sender_user: Some(sender),
            sender_device: None,
            own: sender == ME,
            bot: false,
            ts,
            body: body.into(),
            msg_id: None,
            seq: Some(seq),
            edited: false,
            reply: None,
            reactions: vec![],
            pinned: false,
            attachments: vec![],
            mention,
        }
    }

    fn with_items(state: &AppState, channel: ChannelId, items: Vec<TimelineItem>) -> AppState {
        let mut s = state.clone();
        s.timelines.insert(
            channel,
            Timeline {
                channel_id: channel,
                group: ChannelGroupState::Active,
                items,
                has_earlier: false,
            },
        );
        s
    }

    #[test]
    fn the_tracker_never_replays_history_and_raises_each_new_message_once() {
        let mut tracker = NoticeTracker::new();
        let base = with_items(&world(), RANDOM, vec![item(1, BOB, 500, "old", true)]);
        assert!(
            tracker.observe(&base).is_empty(),
            "the first sight is history"
        );
        let next = with_items(
            &base,
            RANDOM,
            vec![
                item(1, BOB, 500, "old", true),
                item(2, ME, 1_001, "mine", false),
                item(3, BOB, 1_002, "look", true),
                item(4, BOB, 1_003, "plain", false),
            ],
        );
        let notices = tracker.observe(&next);
        let got: Vec<_> = notices.iter().map(|n| (n.body.as_str(), n.kind)).collect();
        assert_eq!(
            got,
            [
                ("look", NoticeKind::Mention),
                ("plain", NoticeKind::Message)
            ]
        );
        assert_eq!(notices[0].sender_name, "bob");
        assert!(tracker.observe(&next).is_empty(), "each only once");
        // a message that is not readable, a pending one and an own one raise nothing
        let mut unreadable = item(5, BOB, 1_020, "", false);
        unreadable.state = TimelineItemState::CannotRead;
        let mut pending = item(0, BOB, 1_021, "x", false);
        pending.seq = None;
        let quiet = with_items(
            &next,
            RANDOM,
            vec![unreadable, pending, item(6, ME, 1_022, "me", false)],
        );
        assert!(tracker.observe(&quiet).is_empty());
    }

    /// Task 28: a channel first seen holding a message received after the start (a DM its peer just opened) notifies it;
    /// one received before the start stays history.
    #[test]
    fn a_channel_first_seen_with_a_message_received_after_the_start_notifies_it() {
        let mut tracker = NoticeTracker::with_floor(1_000);
        assert!(tracker.observe(&world()).is_empty());
        let opened = with_items(
            &world(),
            RANDOM,
            vec![
                item(1, BOB, 999, "before", false),
                item(2, BOB, 1_000, "after", false),
            ],
        );
        let notices = tracker.observe(&opened);
        assert_eq!(
            notices.iter().map(|n| n.body.as_str()).collect::<Vec<_>>(),
            ["after"]
        );
        assert!(tracker.observe(&opened).is_empty(), "once");
    }

    /// t28-bot-review I4: after a reconnect, a channel first seen holding rows the ready caught up (a DM its peer opened
    /// while this device slept) notifies none of them; a message received after the ready does.
    #[test]
    fn a_reconnect_moves_the_floor_so_caught_up_rows_of_a_first_seen_channel_stay_quiet() {
        let mut tracker = NoticeTracker::with_floor(1_000);
        tracker.ready(1, 1_000);
        assert!(tracker.observe(&world()).is_empty());
        // Asleep from 1_000 to 30_000; the reconnect's ready.
        tracker.ready(2, 30_000);
        tracker.ready(2, 40_000);
        let caught_up: Vec<_> = (1..=30)
            .map(|i| item(i, BOB, 1_000 + i * 900, "while asleep", false))
            .collect();
        let opened = with_items(&world(), RANDOM, caught_up.clone());
        assert!(
            tracker.observe(&opened).is_empty(),
            "caught up, not live (one ready sets the floor once)"
        );
        let mut rows = caught_up;
        rows.push(item(31, BOB, 30_001, "live", false));
        let live = with_items(&world(), RANDOM, rows);
        assert_eq!(
            tracker
                .observe(&live)
                .iter()
                .map(|n| n.body.as_str())
                .collect::<Vec<_>>(),
            ["live"]
        );
    }

    #[test]
    fn history_that_loads_after_the_start_raises_no_notice_and_a_live_message_after_it_does() {
        let mut tracker = NoticeTracker::new();
        assert!(tracker.observe(&world()).is_empty());
        // a channel whose timeline first appears late, holding old messages
        let loaded = with_items(
            &world(),
            RANDOM,
            vec![
                item(1, BOB, 10, "old one", true),
                item(2, BOB, 11, "old two", true),
            ],
        );
        assert!(
            tracker.observe(&loaded).is_empty(),
            "loaded history is not news"
        );
        let live = with_items(
            &loaded,
            RANDOM,
            vec![
                item(1, BOB, 10, "old one", true),
                item(2, BOB, 11, "old two", true),
                item(3, BOB, 12, "live", true),
            ],
        );
        let notices = tracker.observe(&live);
        assert_eq!(
            notices.iter().map(|n| n.body.as_str()).collect::<Vec<_>>(),
            ["live"]
        );
    }

    struct Env {
        visible: bool,
        on_screen: Option<ChannelId>,
    }

    impl NotifyEnv for Env {
        fn visible(&self) -> bool {
            self.visible
        }
        fn on_screen(&self) -> Option<ChannelId> {
            self.on_screen
        }
        fn presence(&self) -> Presence {
            Presence::Online
        }
    }

    // badges.spec.ts `notifications_follow_default_and_channel_on_screen`, on the engine: DMs and mentions by default,
    // everything on request, the channel on screen quiet, nothing silences all, a click opens and closes.
    #[test]
    fn notifications_follow_the_default_and_the_channel_on_screen() {
        let recorder = RecordingNotifier::new();
        let env = Env {
            visible: true,
            on_screen: Some(GENERAL),
        };
        let mut engine = Engine::new(Arc::new(recorder.clone()), Arc::new(env), text());
        let mut state = with_items(&world(), GENERAL, vec![item(1, BOB, 500, "hello", false)]);
        state = with_items(&state, RANDOM, vec![]);
        // a DM is opened (its timeline held, empty) before anything arrives in it
        state = with_items(&state, DM, vec![]);
        let mut src = settings(&[]);
        assert!(engine.on_state(&state, &src).is_empty());
        let mention = format!("<@{}> look", ME.hex());

        // (a) DMs and mentions: a plain message stays a badge, a mention notifies
        state = with_items(&state, RANDOM, vec![item(1, BOB, 1_001, "plain", false)]);
        assert!(engine.on_state(&state, &src).is_empty());
        state = with_items(&state, DM, vec![item(1, BOB, 1_002, &mention, false)]);
        let shown = engine.on_state(&state, &src);
        assert_eq!(shown.len(), 1);
        assert_eq!(
            (shown[0].title.as_str(), shown[0].body.as_str()),
            ("bob", "@Ada L look")
        );
        assert_eq!(shown[0].tag, format!("dilla:{}", DM.hex()));

        // (b) everything: the channel on screen stays quiet, another notifies
        src.insert(KEY_DEFAULT.into(), "everything".into());
        let on_screen = vec![
            item(1, BOB, 500, "hello", false),
            item(2, BOB, 1_003, "on screen", false),
        ];
        state = with_items(&state, GENERAL, on_screen);
        assert!(
            engine.on_state(&state, &src).is_empty(),
            "the channel on screen"
        );
        let again = vec![
            item(1, BOB, 1_001, "plain", false),
            item(2, BOB, 1_004, "plain again", false),
        ];
        state = with_items(&state, RANDOM, again);
        let shown = engine.on_state(&state, &src);
        let titles: Vec<_> = shown.iter().map(|n| n.title.as_str()).collect();
        assert_eq!(titles, ["#random · Midgard"]);
        assert_eq!(
            recorder.tags(),
            [
                format!("dilla:{}", DM.hex()),
                format!("dilla:{}", RANDOM.hex())
            ]
        );
        assert_eq!(recorder.click(1), Some(RANDOM));
        assert!(recorder.shown()[1].closed);
        assert!(!recorder.shown()[0].closed);

        // (c) nothing: a mention only counts
        src.insert(KEY_DEFAULT.into(), "nothing".into());
        state = with_items(&state, RANDOM, vec![item(3, BOB, 1_005, &mention, true)]);
        assert!(engine.on_state(&state, &src).is_empty());
        assert_eq!(recorder.shown().len(), 2);
    }

    #[test]
    fn a_later_notification_with_the_same_tag_replaces_the_earlier() {
        let recorder = RecordingNotifier::new();
        let mut n = Notification::new(NotificationKind::Dm, "bob", "one");
        n.tag = "dilla:x".into();
        recorder.deliver(&n);
        n.body = "two".into();
        recorder.deliver(&n);
        let shown = recorder.shown();
        assert_eq!(shown.len(), 1);
        assert_eq!(shown[0].notification.body, "two");
    }

    fn with_badges(state: &AppState, pairs: &[(ChannelId, u32, u32)]) -> AppState {
        let mut s = state.clone();
        for (c, unread, mentions) in pairs {
            s.unread.insert(
                *c,
                BadgeState {
                    unread: *unread,
                    mentions: *mentions,
                },
            );
        }
        s
    }

    // badges.spec.ts `unopened_channels_badge_open_reads_mute_hides_count`, the counts' part.
    #[test]
    fn a_mute_hides_the_count_but_not_the_mention_and_the_totals_follow() {
        let state = with_badges(&world(), &[(GENERAL, 0, 0), (RANDOM, 1, 0), (DM, 2, 1)]);
        let none = settings(&[]);
        assert_eq!(row_counts(&state, &none, RANDOM), (1, 0));
        assert_eq!(rail_counts(&state, &none, COMMUNITY), (1, 0));
        assert_eq!(dm_counts(&state, &none), (2, 1));
        assert_eq!(total_badge(&state, &none), 3);
        let loot = ChannelId([0xe5; 16]);
        let mut state = state;
        state.channels.push(channel(loot, "loot"));
        state.unread.insert(
            loot,
            BadgeState {
                unread: 1,
                mentions: 1,
            },
        );
        assert_eq!(rail_counts(&state, &none, COMMUNITY), (2, 1));
        let muted = settings(&[(&key_channel_mute(loot), "1")]);
        assert_eq!(row_counts(&state, &muted, loot), (0, 1));
        assert_eq!(rail_counts(&state, &muted, COMMUNITY), (1, 1));
        assert_eq!(total_badge(&state, &muted), 1 + 2 + 1);
        let muted_server = settings(&[(&key_community_mute(COMMUNITY), "1")]);
        assert_eq!(row_counts(&state, &muted_server, RANDOM), (0, 0));
        assert_eq!(total_badge(&state, &muted_server), 2 + 1);
    }

    #[test]
    fn the_notifications_section_shows_the_default_as_the_servers_value() {
        assert_eq!(
            server_mode_shown(&settings(&[]), COMMUNITY),
            NotifyMode::Mentions
        );
        let everything = settings(&[(KEY_DEFAULT, "everything")]);
        assert_eq!(server_mode_shown(&everything, COMMUNITY), NotifyMode::All);
        let nothing = settings(&[(KEY_DEFAULT, "nothing")]);
        assert_eq!(server_mode_shown(&nothing, COMMUNITY), NotifyMode::Nothing);
        let own = settings(&[
            (KEY_DEFAULT, "nothing"),
            (&key_community_mode(COMMUNITY), "all"),
        ]);
        assert_eq!(server_mode_shown(&own, COMMUNITY), NotifyMode::All);
    }
}
