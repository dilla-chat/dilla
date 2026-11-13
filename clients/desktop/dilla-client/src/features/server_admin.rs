//! Area **server settings** (Task 15): overview, AFK channel and timeout, channels and their permissions, GIF settings, invites, welcome and join roles, the danger zone.
//!
//! Ports: packages/web/src/screens/admin/{welcome-settings,shared}.ts, and the admin half of
//! `packages/client-core/src/admin/{controller,rank,afk,perm}.ts` (the commands the sections send) with the routes they
//! use (`http/routes.ts`). Every request is a socket request (op 59), as the TS client sends them.
//!
//! The rules of the sections (what a name may be, which invites are still usable, how a channel moves, which sections a
//! permission shows) are plain functions below with their tests; the commands on [`ServerAdmin`] are thin around them
//! and publish into [`crate::state::server_admin::ServerAdminState`].

use std::sync::Arc;

use unicode_normalization::UnicodeNormalization;
use zeroize::Zeroizing;

use crate::client::{Client, ClientError, Inner};
use crate::gateway::frames::Value;
use crate::http::fill_path;
use crate::http::routes as r;
use crate::seams::Route;
use crate::state::server_admin::{
    Access, AdminRole, AfkSettings, GifRating, GifSettings, Invite, InviteCreated, InviteId,
    Overwrite, WelcomeSettings,
};
use crate::state::{Channel, ChannelId, CommunityId, UserId};
use crate::sync::api::ApiError;
use crate::sync::value::{DecodeError, Rows, at_least, bin16, decode, opt, text, u53, uint};

// ---- the bounds the instance and the TS client hold ----

/// internal/api maxCommunityNameBytes.
pub const COMMUNITY_NAME_MAX_BYTES: usize = 255;
/// internal/api maxChannelNameBytes.
pub const CHANNEL_NAME_MAX_BYTES: usize = 100;
/// internal/api topic bound.
pub const TOPIC_MAX_BYTES: usize = 1024;
/// routes.ts `INVITE_USES_MAX`.
pub const INVITE_USES_MAX: u64 = 1000;
/// routes.ts `INVITE_TTL_MAX_S`.
pub const INVITE_TTL_MAX_S: u64 = 30 * 24 * 3600;
/// routes.ts `INVITES_MAX`.
pub const INVITES_MAX: usize = 10_000;
/// routes.ts `OWNER_TOP`.
pub const OWNER_TOP: u64 = 1 << 31;
/// routes.ts `CHANNEL_POSITION_MAX`.
pub const CHANNEL_POSITION_MAX: u64 = (1 << 31) - 1;
/// afk.ts `AFK_BUILTIN_POSITION`: the position the built-in AFK channel keeps, last.
pub const AFK_BUILTIN_POSITION: u32 = 1 << 30;
/// routes.ts `SLOW_STEPS`, in seconds.
pub const SLOW_STEPS: [u32; 9] = [0, 5, 10, 30, 60, 300, 900, 3600, 21600];
/// The inactive timeouts the Overview offers, in seconds (OverviewSection.tsx `TIMEOUTS`).
pub const AFK_TIMEOUTS: [u64; 5] = [60, 300, 900, 1800, 3600];
/// The default timeout the Overview shows until the read lands.
pub const AFK_DEFAULT_TIMEOUT: u64 = 300;
/// OVERWRITES_MAX of routes.ts.
pub const OVERWRITES_MAX: usize = 10_000;

/// The 26 permission bits of internal/api/perm.go (admin/perm.ts `PERM`).
pub mod perm {
    pub const VIEW_CHANNEL: u32 = 1 << 0;
    pub const SEND_MESSAGES: u32 = 1 << 1;
    pub const MANAGE_MESSAGES: u32 = 1 << 2;
    pub const PIN_MESSAGES: u32 = 1 << 3;
    pub const ATTACH_FILES: u32 = 1 << 4;
    pub const ADD_REACTIONS: u32 = 1 << 5;
    pub const READ_HISTORY: u32 = 1 << 6;
    pub const CONNECT: u32 = 1 << 7;
    pub const SPEAK: u32 = 1 << 8;
    pub const VIDEO: u32 = 1 << 9;
    pub const SCREEN_SHARE: u32 = 1 << 10;
    pub const CREATE_INVITE: u32 = 1 << 11;
    pub const KICK_MEMBERS: u32 = 1 << 12;
    pub const BAN_MEMBERS: u32 = 1 << 13;
    pub const MANAGE_CHANNELS: u32 = 1 << 14;
    pub const MANAGE_ROLES: u32 = 1 << 15;
    pub const MANAGE_COMMUNITY: u32 = 1 << 16;
    pub const VIEW_AUDIT_LOG: u32 = 1 << 17;
    pub const MENTION_EVERYONE: u32 = 1 << 18;
    pub const BYPASS_SLOWMODE: u32 = 1 << 19;
    pub const ADMINISTRATOR: u32 = 1 << 20;
    pub const MUTE_MEMBERS: u32 = 1 << 21;
    pub const MOVE_MEMBERS: u32 = 1 << 22;
    pub const MANAGE_NICKNAMES: u32 = 1 << 23;
    pub const MODERATE_MEMBERS: u32 = 1 << 24;
    pub const CONTROL_MUSIC: u32 = 1 << 25;
    /// routes.ts `PERM_ALL`.
    pub const ALL: u32 = 0x3ff_ffff;

    /// The bits a channel overwrite can neither grant nor remove (perm.go communityBits).
    pub const COMMUNITY_BITS: u32 = KICK_MEMBERS
        | BAN_MEMBERS
        | MANAGE_COMMUNITY
        | MANAGE_ROLES
        | VIEW_AUDIT_LOG
        | ADMINISTRATOR
        | MANAGE_NICKNAMES
        | MODERATE_MEMBERS;
}

// ---- rules ----

/// rank.ts `accessOf`, as the settings view it: the one resolver (`features::roles::rank::access_of`, final review I2),
/// converted. There is no second formula here.
pub fn access_of(bits: u64, top: u64, owner: bool) -> Access {
    let top = u32::try_from(top).unwrap_or(u32::MAX);
    Access::from(&crate::features::roles::rank::access_of(bits, top, owner))
}

/// perm.ts `isPrivate`: a channel is private when `@everyone`'s overwrite denies viewing it.
pub fn is_private(overwrites: &[Overwrite], channel: ChannelId, everyone: [u8; 16]) -> bool {
    overwrites.iter().any(|o| {
        o.channel == channel
            && o.kind == 0
            && o.target == everyone
            && o.deny & perm::VIEW_CHANNEL != 0
    })
}

/// The `@everyone` overwrite to write to make a channel private or open, or `None` for no change (roles.ts
/// `setChannelPrivate`). Privacy denies viewing and takes the bit off the allow side too (a deny does not beat an allow
/// of the same bit); opening takes only the deny back. `(allow, deny)`; both 0 means delete the overwrite.
pub fn privacy_plan(current: Option<(u32, u32)>, make_private: bool) -> Option<(u32, u32)> {
    let (allow, deny) = current.unwrap_or((0, 0));
    if make_private {
        Some((allow & !perm::VIEW_CHANNEL, deny | perm::VIEW_CHANNEL))
    } else if deny & perm::VIEW_CHANNEL == 0 {
        None
    } else {
        Some((allow, deny & !perm::VIEW_CHANNEL))
    }
}

/// roles.ts `allowsMeToView`: some overwrite of the channel other than the `@everyone` one being denied lets this account
/// view it (an allow for itself, or for a role it holds).
pub fn allows_me_to_view(
    overwrites: &[Overwrite],
    channel: ChannelId,
    everyone: [u8; 16],
    me: UserId,
    held: &[[u8; 16]],
) -> bool {
    overwrites.iter().any(|o| {
        o.channel == channel
            && o.allow & perm::VIEW_CHANNEL != 0
            && ((o.kind == 1 && o.target == me.0)
                || (o.kind == 0 && o.target != everyone && held.contains(&o.target)))
    })
}

/// routes.ts `listRoles`: `[[id, name, color, position, allow, deny, hoist, mentionable]]`.
pub fn decode_roles(raw: &[u8]) -> Result<Vec<AdminRole>, DecodeError> {
    let rows = Rows::of(raw)?;
    rows.iter()
        .map(|row| {
            let a = at_least(row, 4)?;
            Ok(AdminRole {
                id: bin16(&a[0])?,
                name: text(&a[1])?,
                position: u53(&a[3])?,
                allow: a.get(4).map_or(Ok(0), bits)?,
                deny: a.get(5).map_or(Ok(0), bits)?,
            })
        })
        .collect()
}

/// Why the instance would refuse a name (Create.tsx `nameProblem`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NameProblem {
    Empty,
    Long,
    Control,
}

/// Why the instance would refuse `raw` (trimmed) as a name of at most `max_bytes` UTF-8 bytes; `None` when it would not.
pub fn name_problem(raw: &str, max_bytes: usize) -> Option<NameProblem> {
    let name = raw.trim();
    if name.is_empty() {
        Some(NameProblem::Empty)
    } else if name.len() > max_bytes {
        Some(NameProblem::Long)
    } else if name.chars().any(|c| c.is_control()) {
        Some(NameProblem::Control)
    } else {
        None
    }
}

/// shared.ts `nfc`: a typed name and a server name compare under NFC, trimmed ("Björn" typed as NFD matches).
pub fn nfc(s: &str) -> String {
    s.nfc().collect::<String>().trim().to_owned()
}

/// The text of a refusal the sections read by status or code (shared.ts `statusIs`, `forbidden`, `refused`).
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
#[error("{code} ({status})")]
pub struct AdminError {
    /// The HTTP status the instance answered with; 0 for a failure before any answer.
    pub status: u16,
    pub code: String,
}

impl AdminError {
    pub fn status_is(&self, status: u16) -> bool {
        self.status == status
    }

    /// `E_FORBIDDEN` or a 403.
    pub fn forbidden(&self) -> bool {
        self.code == "E_FORBIDDEN" || self.status == 403
    }

    /// A rename or edit the instance refused as invalid.
    pub fn refused(&self) -> bool {
        self.code == "E_INVALID_REQUEST" || self.code == "E_BAD_INPUT"
    }

    fn local(code: &str) -> Self {
        Self {
            status: 0,
            code: code.to_owned(),
        }
    }
}

impl From<ApiError> for AdminError {
    fn from(e: ApiError) -> Self {
        Self {
            status: e.status,
            code: e.code,
        }
    }
}

impl From<DecodeError> for AdminError {
    fn from(_: DecodeError) -> Self {
        Self::local("E_DECODE")
    }
}

/// The request body of `PUT /gifs/key`, `[key]`, written straight into a buffer that wipes itself: a one-element CBOR array
/// holding the key as a text string (the same bytes as `Value::encode`, which would copy the key through a plain `String`).
pub fn gif_key_body(key: &str) -> Zeroizing<Vec<u8>> {
    let mut out = Zeroizing::new(Vec::with_capacity(key.len() + 4));
    out.push(0x81);
    match key.len() {
        n @ 0..=23 => out.push(0x60 | n as u8),
        n @ 24..=255 => out.extend_from_slice(&[0x78, n as u8]),
        n => {
            out.push(0x79);
            out.extend_from_slice(&(n as u16).to_be_bytes());
        }
    }
    out.extend_from_slice(key.as_bytes());
    out
}

/// routes.ts `GIF_KEY`: a Giphy key is 8 to 64 letters and digits.
pub fn gif_key_valid(key: &str) -> bool {
    (8..=64).contains(&key.len()) && key.bytes().all(|b| b.is_ascii_alphanumeric())
}

/// controller.ts `loadInvites`: only the invites that can still be used (not revoked, not expired, not used up; 0 means
/// no limit for max uses and for expiry).
pub fn usable(rows: &[InviteRow], now: u64) -> Vec<Invite> {
    rows.iter()
        .filter(|i| {
            i.revoked_at.is_none()
                && (i.expires == 0 || i.expires > now)
                && (i.max_uses == 0 || i.uses < i.max_uses)
        })
        .map(|i| Invite {
            id: i.id,
            created_by: i.created_by,
            max_uses: i.max_uses,
            uses: i.uses,
            created: i.created,
            expires: i.expires,
        })
        .collect()
}

/// routes.ts `InviteRow`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct InviteRow {
    pub id: InviteId,
    pub created_by: Option<UserId>,
    pub max_uses: u64,
    pub uses: u64,
    pub created: u64,
    pub expires: u64,
    pub revoked_at: Option<u64>,
}

/// routes.ts `listInvites`'s rows: `[id, community, created_by, _, max_uses, uses, created, expires, revoked_at, ..]`.
pub fn decode_invites(raw: &[u8]) -> Result<Vec<InviteRow>, DecodeError> {
    let rows = Rows::of(raw)?;
    let out: Vec<InviteRow> = rows
        .iter()
        .map(|row| {
            let a = at_least(row, 9)?;
            Ok(InviteRow {
                id: InviteId(bin16(&a[0])?),
                created_by: opt(&a[2], bin16)?.map(UserId),
                max_uses: u53(&a[4])?,
                uses: u53(&a[5])?,
                created: uint(&a[6])?,
                expires: uint(&a[7])?,
                revoked_at: opt(&a[8], uint)?,
            })
        })
        .collect::<Result<_, DecodeError>>()?;
    if out.len() > INVITES_MAX {
        return Err(DecodeError("too many invites"));
    }
    Ok(out)
}

/// routes.ts `inviteUrl`: an absolute http(s) URL whose path is `/i/<code>` for the answered code, no query or fragment.
pub fn invite_url(raw: &str, code: &str) -> Option<String> {
    let url = url::Url::parse(raw).ok()?;
    if !matches!(url.scheme(), "http" | "https") {
        return None;
    }
    let want: String = url::form_urlencoded::byte_serialize(code.as_bytes()).collect();
    let want = want.replace('+', "%20");
    if url.path() != format!("/i/{want}") || url.query().is_some() || raw.contains('#') {
        return None;
    }
    Some(raw.to_owned())
}

/// routes.ts `listOverwrites`: `[[channel, kind, target, allow, deny]]`.
pub fn decode_overwrites(raw: &[u8]) -> Result<Vec<Overwrite>, DecodeError> {
    let rows = Rows::of(raw)?;
    let out: Vec<Overwrite> = rows
        .iter()
        .map(|row| {
            let a = at_least(row, 5)?;
            let kind = u8::try_from(u53(&a[1])?).map_err(|_| DecodeError("a kind out of range"))?;
            if kind > 1 {
                return Err(DecodeError("a kind out of range"));
            }
            Ok(Overwrite {
                channel: ChannelId(bin16(&a[0])?),
                kind,
                target: bin16(&a[2])?,
                allow: bits(&a[3])?,
                deny: bits(&a[4])?,
            })
        })
        .collect::<Result<_, DecodeError>>()?;
    if out.len() > OVERWRITES_MAX {
        return Err(DecodeError("too many overwrites"));
    }
    Ok(out)
}

fn bits(v: &Value) -> Result<u32, DecodeError> {
    let n = u53(v)?;
    u32::try_from(n)
        .ok()
        .filter(|n| n & !perm::ALL == 0)
        .ok_or(DecodeError("permission bits out of range"))
}

/// What a channel's edit does to its parent (controller.ts `updateChannel`'s `parent`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Parent {
    /// Leave it where it is.
    Keep,
    /// Out of its category.
    Top,
    Category(ChannelId),
}

/// The direction of a one-step move.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Direction {
    Up,
    Down,
}

/// afk.ts `isBuiltinAFK`: by id when the AFK settings name the channel, else the channel at exactly the reservation.
pub fn is_builtin_afk(channel: ChannelId, position: u32, afk: Option<ChannelId>) -> bool {
    match afk {
        Some(afk) => channel == afk,
        None => position == AFK_BUILTIN_POSITION,
    }
}

/// controller.ts `nextPosition`: after the last ordinary channel (the AFK reservation does not count).
pub fn next_position(positions: impl IntoIterator<Item = u32>) -> u32 {
    positions
        .into_iter()
        .filter(|&p| p < AFK_BUILTIN_POSITION)
        .map(|p| p + 1)
        .max()
        .unwrap_or(0)
}

/// controller.ts `moveChannel`: moves a channel one step inside its parent, among the ordinary channels of its kind
/// only (R-AFK8): the built-in AFK channel stays last at 2^30, never moves and is never passed, so a move of AFK, or down
/// from the last ordinary channel, is no change. Positions that are strictly increasing and below 2^30 trade between the
/// mover and its neighbour (two patches). With a tie anywhere among the ordinary channels, or one of them at or above
/// 2^30, those are renumbered densely 0..n-1 in the new display order, and only the channels whose position changes are
/// patched. Answers `(channel, new position)` pairs.
pub fn plan_move(
    channels: &[Channel],
    channel: ChannelId,
    direction: Direction,
    afk: Option<ChannelId>,
) -> Vec<(ChannelId, u32)> {
    let Some(row) = channels.iter().find(|c| c.id == channel) else {
        return Vec::new();
    };
    if is_builtin_afk(row.id, row.position, afk) {
        return Vec::new();
    }
    let mut siblings: Vec<&Channel> = channels
        .iter()
        .filter(|c| {
            c.community_id == row.community_id
                && c.parent_id == row.parent_id
                && (c.kind == 2) == (row.kind == 2)
                && !is_builtin_afk(c.id, c.position, afk)
        })
        .collect();
    siblings.sort_by(|a, b| a.position.cmp(&b.position).then(a.id.cmp(&b.id)));
    let Some(at) = siblings.iter().position(|c| c.id == channel) else {
        return Vec::new();
    };
    let to = match direction {
        Direction::Up => at.checked_sub(1),
        Direction::Down => Some(at + 1),
    };
    let Some(to) = to.filter(|&t| t < siblings.len()) else {
        return Vec::new();
    };
    let strict = siblings.windows(2).all(|w| w[1].position > w[0].position)
        && siblings.iter().all(|c| c.position < AFK_BUILTIN_POSITION);
    if strict {
        vec![
            (row.id, siblings[to].position),
            (siblings[to].id, row.position),
        ]
    } else {
        let mut order = siblings.clone();
        order.swap(at, to);
        order
            .iter()
            .enumerate()
            .map(|(i, c)| (c, u32::try_from(i).unwrap_or(u32::MAX)))
            .filter(|(c, p)| c.position != *p)
            .map(|(c, p)| (c.id, p))
            .collect()
    }
}

// ---- the welcome configuration (welcome-settings.ts) ----

/// Why a welcome document was refused.
#[derive(Clone, Copy, Debug, PartialEq, Eq, thiserror::Error)]
pub enum WelcomeError {
    #[error("invalid channel")]
    Channel,
    #[error("invalid welcome settings text")]
    Text,
    #[error("invalid welcome settings document")]
    Document,
}

/// welcome-settings.ts `encodeWelcomeSettings`: `[1, channel, template, server_name]`.
pub fn encode_welcome(value: &WelcomeSettings) -> Result<Vec<u8>, WelcomeError> {
    if value.channel.0 == [0u8; 16] {
        return Err(WelcomeError::Channel);
    }
    let template = value.template.len();
    let server = value.server_name.len();
    if !(1..=2000).contains(&template)
        || !(1..=256).contains(&server)
        || value.server_name.trim().is_empty()
        || value
            .server_name
            .chars()
            .any(|c| matches!(c, '\u{0}'..='\u{1f}' | '\u{7f}'))
    {
        return Err(WelcomeError::Text);
    }
    Ok(Value::Array(vec![
        Value::Uint(1),
        Value::Bytes(value.channel.0.to_vec()),
        Value::Text(value.template.clone()),
        Value::Text(value.server_name.clone()),
    ])
    .encode())
}

/// welcome-settings.ts `decodeWelcomeSettings`: `None` for the encrypted empty-array document that means unconfigured.
pub fn decode_welcome(data: &[u8]) -> Result<Option<WelcomeSettings>, WelcomeError> {
    let Value::Array(items) = Value::decode(data).map_err(|_| WelcomeError::Document)? else {
        return Err(WelcomeError::Document);
    };
    if items.is_empty() {
        return Ok(None);
    }
    let [
        Value::Uint(1),
        Value::Bytes(channel),
        Value::Text(template),
        Value::Text(server_name),
    ] = items.as_slice()
    else {
        return Err(WelcomeError::Document);
    };
    let channel: [u8; 16] = channel
        .as_slice()
        .try_into()
        .map_err(|_| WelcomeError::Document)?;
    if channel == [0u8; 16] {
        return Err(WelcomeError::Document);
    }
    let config = WelcomeSettings {
        channel: ChannelId(channel),
        template: template.clone(),
        server_name: server_name.clone(),
    };
    // Keep rendering inputs within the same bounds accepted by the service.
    encode_welcome(&config)?;
    Ok(Some(config))
}

/// perm.ts `PERM_GROUPS`: the grid's groups (`perm.group.<id>`) and, in order, each right's key (`perm.<key>`) and bit.
pub const PERM_GROUPS: [(&str, &[(&str, u32)]); 5] = [
    (
        "general",
        &[
            ("view", perm::VIEW_CHANNEL),
            ("manageChannels", perm::MANAGE_CHANNELS),
            ("manageRoles", perm::MANAGE_ROLES),
            ("manageServer", perm::MANAGE_COMMUNITY),
            ("invite", perm::CREATE_INVITE),
            ("auditLog", perm::VIEW_AUDIT_LOG),
            ("admin", perm::ADMINISTRATOR),
        ],
    ),
    (
        "members",
        &[
            ("kick", perm::KICK_MEMBERS),
            ("ban", perm::BAN_MEMBERS),
            ("nicknames", perm::MANAGE_NICKNAMES),
            ("moderate", perm::MODERATE_MEMBERS),
        ],
    ),
    (
        "text",
        &[
            ("send", perm::SEND_MESSAGES),
            ("attach", perm::ATTACH_FILES),
            ("react", perm::ADD_REACTIONS),
            ("history", perm::READ_HISTORY),
            ("pin", perm::PIN_MESSAGES),
            ("everyone", perm::MENTION_EVERYONE),
            ("manageMessages", perm::MANAGE_MESSAGES),
            ("slowmode", perm::BYPASS_SLOWMODE),
        ],
    ),
    (
        "voice",
        &[
            ("connect", perm::CONNECT),
            ("speak", perm::SPEAK),
            ("video", perm::VIDEO),
            ("screen", perm::SCREEN_SHARE),
            ("mute", perm::MUTE_MEMBERS),
            ("move", perm::MOVE_MEMBERS),
        ],
    ),
    ("music", &[("music", perm::CONTROL_MUSIC)]),
];

/// routes.ts `putOverwrite`'s validator: a role or a member, 26-bit masks, and no bit on both sides.
pub fn overwrite_valid(kind: u8, allow: u32, deny: u32) -> bool {
    kind <= 1 && allow & !perm::ALL == 0 && deny & !perm::ALL == 0 && allow & deny == 0
}

// ---- permission resolution and a channel's own permissions (admin/perm.ts, ChannelPermissions.tsx) ----

/// internal/api/perm.go as `admin/perm.ts` ports it, checked against `protocol/vectors/permissions.json`. Integration 3
/// (Task 15's merge item): one resolver; these delegate to Task 14's `roles::perm`, taking this area's shapes.
pub mod resolve {
    use super::Overwrite;
    use crate::features::roles::perm::{
        PermInput, PermOverwrite, PermRole, channel_bits_from, resolve_community, resolve_user,
    };

    /// `PermRole`.
    #[derive(Clone, Copy, Debug, PartialEq, Eq)]
    pub struct Role {
        pub id: [u8; 16],
        pub position: u64,
        pub allow: u32,
        pub deny: u32,
    }

    /// `PermInput`: who owns the server, which role is `@everyone`, every role, and the roles the user holds.
    pub struct Input<'a> {
        pub owner: [u8; 16],
        pub everyone: [u8; 16],
        pub roles: &'a [Role],
        pub held: &'a [[u8; 16]],
    }

    fn roles(input: &Input<'_>) -> Vec<PermRole> {
        input
            .roles
            .iter()
            .map(|r| PermRole {
                id: r.id,
                position: r.position,
                allow: r.allow,
                deny: r.deny,
            })
            .collect()
    }

    /// perm.go Resolve: the owner short-circuit, `@everyone`, then each held role in ascending position.
    pub fn community(input: &Input<'_>, user: [u8; 16]) -> u32 {
        let roles = roles(input);
        resolve_community(
            &PermInput {
                owner: input.owner,
                everyone_id: input.everyone,
                roles: &roles,
                held: input.held,
            },
            user,
        )
    }

    /// perm.go ResolveChannel: the overwrites of the held roles in ascending role position, then the user's own, then the
    /// community-scoped bits restored from the base. `overwrites` are the channel's own.
    pub fn channel(input: &Input<'_>, overwrites: &[Overwrite], user: [u8; 16]) -> u32 {
        let roles = roles(input);
        let resolved = resolve_user(
            &PermInput {
                owner: input.owner,
                everyone_id: input.everyone,
                roles: &roles,
                held: input.held,
            },
            user,
        );
        let overwrites: Vec<PermOverwrite> = overwrites
            .iter()
            .map(|o| PermOverwrite {
                kind: o.kind,
                target_id: o.target,
                allow: o.allow,
                deny: o.deny,
            })
            .collect();
        channel_bits_from(&resolved, &overwrites)
    }
}

/// The value of one permission in an entry's grid.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Tri {
    Allow,
    Inherit,
    Deny,
}

impl Tri {
    pub const ALL: [Tri; 3] = [Tri::Allow, Tri::Inherit, Tri::Deny];

    pub fn wire(self) -> &'static str {
        match self {
            Tri::Allow => "allow",
            Tri::Inherit => "inherit",
            Tri::Deny => "deny",
        }
    }

    pub fn from_wire(s: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|t| t.wire() == s)
    }
}

/// ChannelPermissions.tsx: what `bit` is in an entry (allow wins, then deny, else inherit).
pub fn tri_of(allow: u32, deny: u32, bit: u32) -> Tri {
    if allow & bit != 0 {
        Tri::Allow
    } else if deny & bit != 0 {
        Tri::Deny
    } else {
        Tri::Inherit
    }
}

/// ChannelPermissions.tsx `save`: the entry's `(allow, deny)` after `bit` is set to `value`, from the entry as it is now.
pub fn after_change(now: Option<(u32, u32)>, bit: u32, value: Tri) -> (u32, u32) {
    let (allow, deny) = now.unwrap_or((0, 0));
    (
        if value == Tri::Allow {
            allow | bit
        } else {
            allow & !bit
        },
        if value == Tri::Deny {
            deny | bit
        } else {
            deny & !bit
        },
    )
}

/// A read of `community`'s overwrites into the one overwrites slice (`state.roles.overwrites`, re-review N4): every
/// channel's entries, replacing what was held.
fn put_overwrites(s: &mut crate::state::AppState, community: CommunityId, rows: &[Overwrite]) {
    let mut by: std::collections::HashMap<ChannelId, Vec<crate::state::roles::OverwriteSummary>> =
        std::collections::HashMap::new();
    for o in rows {
        by.entry(o.channel)
            .or_default()
            .push(crate::state::roles::OverwriteSummary {
                kind: o.kind,
                target_id: o.target,
                allow: o.allow,
                deny: o.deny,
            });
    }
    s.roles.overwrites.insert(community, by);
}

/// ChannelPermissions.tsx `save`'s masks for one entry: the entry as the one overwrites slice holds it now, with `bit`
/// set to `value`. Refused (`chperm.error.unread`) while the overwrites are not read: an unknown entry is never
/// replaced (re-review N4), so a deny it holds can never be dropped by an edit of another bit.
pub fn change_of(
    state: &crate::state::AppState,
    community: CommunityId,
    channel: ChannelId,
    entry: (u8, [u8; 16]),
    bit: u32,
    value: Tri,
) -> Result<(u32, u32), &'static str> {
    let rows =
        crate::state::server_admin::overwrites_in(state, community).ok_or("chperm.error.unread")?;
    let now = rows
        .iter()
        .find(|o| o.channel == channel && o.kind == entry.0 && o.target == entry.1)
        .map(|o| (o.allow, o.deny));
    Ok(after_change(now, bit, value))
}

/// 32 lower-case hex digits of an id.
pub fn hex_id(id: &[u8; 16]) -> String {
    crate::state::hex(id)
}

/// The 16 bytes 32 lower-case hex digits spell (a `kind:id` choice of the Add select carries one).
pub fn unhex_id(s: &str) -> Option<[u8; 16]> {
    if s.len() != 32 || !s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f')) {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, byte) in out.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).ok()?;
    }
    Some(out)
}

/// `targetId.slice(0, 8)`: what an unknown role or member is called.
pub fn short_id(id: &[u8; 16]) -> String {
    crate::state::hex(id)[..8].to_owned()
}

/// One role or person with settings of its own in a channel (ChannelPermissions.tsx `Entry`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PermEntry {
    /// 0 a role, 1 a member.
    pub kind: u8,
    pub target: [u8; 16],
    /// Empty for `@everyone` (the screen says it in its own words).
    pub name: String,
    pub is_everyone: bool,
    pub allow: u32,
    pub deny: u32,
    /// Roles by position, members -1: the sort key.
    pub rank: i64,
    /// This account may change it (the owner may; others only below their own rank, never the server's owner).
    pub editable: bool,
}

/// What the entries and the add list read of the server.
pub struct PermContext<'a> {
    pub everyone: [u8; 16],
    /// This account owns the server.
    pub owner: bool,
    /// Its highest role position.
    pub top: u64,
    pub community_owner: UserId,
    pub me: UserId,
    pub roles: &'a [AdminRole],
    pub members: &'a [crate::state::Member],
}

fn shown_name(m: &crate::state::Member) -> String {
    if m.display.is_empty() {
        m.username.clone()
    } else {
        m.display.clone()
    }
}

fn member_top(m: &crate::state::Member, roles: &[AdminRole]) -> u64 {
    m.role_ids
        .iter()
        .map(|id| roles.iter().find(|r| r.id == *id).map_or(0, |r| r.position))
        .max()
        .unwrap_or(0)
}

/// ChannelPermissions.tsx `rows`: the channel's overwrites and the entries just added (empty), `@everyone` first, then roles
/// by position (highest first), then members.
pub fn perm_entries(
    channel_overwrites: &[Overwrite],
    added: &[(u8, [u8; 16])],
    ctx: &PermContext<'_>,
) -> Vec<PermEntry> {
    let mut all: Vec<(u8, [u8; 16], u32, u32)> = channel_overwrites
        .iter()
        .map(|o| (o.kind, o.target, o.allow, o.deny))
        .collect();
    for (kind, target) in added {
        if !all.iter().any(|(k, t, ..)| k == kind && t == target) {
            all.push((*kind, *target, 0, 0));
        }
    }
    let mut out: Vec<PermEntry> = all
        .into_iter()
        .map(|(kind, target, allow, deny)| {
            if kind == 0 {
                let role = ctx.roles.iter().find(|r| r.id == target);
                let is_everyone = target == ctx.everyone;
                PermEntry {
                    kind,
                    target,
                    name: if is_everyone {
                        String::new()
                    } else {
                        role.map_or_else(|| short_id(&target), |r| r.name.clone())
                    },
                    is_everyone,
                    allow,
                    deny,
                    rank: role.map_or(0, |r| i64::try_from(r.position).unwrap_or(i64::MAX)),
                    editable: ctx.owner || role.is_some_and(|r| r.position < ctx.top),
                }
            } else {
                let member = ctx.members.iter().find(|m| m.user_id.0 == target);
                let top = member.map_or(0, |m| member_top(m, ctx.roles));
                PermEntry {
                    kind,
                    target,
                    name: member.map_or_else(|| short_id(&target), shown_name),
                    is_everyone: false,
                    allow,
                    deny,
                    rank: -1,
                    editable: target != ctx.community_owner.0 && (ctx.owner || top < ctx.top),
                }
            }
        })
        .collect();
    // Stable: `@everyone` first, then highest rank first.
    out.sort_by(|a, b| {
        (b.is_everyone)
            .cmp(&a.is_everyone)
            .then(b.rank.cmp(&a.rank))
    });
    out
}

/// One choice of the Add select: a role or a person that has no entry yet.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AddOption {
    pub kind: u8,
    pub target: [u8; 16],
    /// Empty for `@everyone`.
    pub label: String,
    pub is_everyone: bool,
}

/// ChannelPermissions.tsx `options`: roles (ascending position) this account may change, then people it may.
pub fn add_options(rows: &[PermEntry], ctx: &PermContext<'_>) -> Vec<AddOption> {
    let taken =
        |kind: u8, target: &[u8; 16]| rows.iter().any(|r| r.kind == kind && r.target == *target);
    let mut roles: Vec<&AdminRole> = ctx
        .roles
        .iter()
        .filter(|r| !taken(0, &r.id) && (ctx.owner || r.position < ctx.top))
        .collect();
    roles.sort_by_key(|r| r.position);
    let mut out: Vec<AddOption> = roles
        .into_iter()
        .map(|r| AddOption {
            kind: 0,
            target: r.id,
            label: if r.id == ctx.everyone {
                String::new()
            } else {
                r.name.clone()
            },
            is_everyone: r.id == ctx.everyone,
        })
        .collect();
    out.extend(
        ctx.members
            .iter()
            .filter(|m| {
                !taken(1, &m.user_id.0)
                    && m.user_id != ctx.community_owner
                    && m.user_id != ctx.me
                    && (ctx.owner || member_top(m, ctx.roles) < ctx.top)
            })
            .map(|m| AddOption {
                kind: 1,
                target: m.user_id.0,
                label: shown_name(m),
                is_everyone: false,
            }),
    );
    out
}

/// What `me` holds in the channel (`channelBitsOf`): `None` while it cannot be told (no `@everyone`, not a member).
pub fn my_channel_bits(channel_overwrites: &[Overwrite], ctx: &PermContext<'_>) -> Option<u32> {
    if ctx.me == ctx.community_owner {
        return Some(perm::ALL);
    }
    let member = ctx.members.iter().find(|m| m.user_id == ctx.me)?;
    let roles: Vec<resolve::Role> = ctx
        .roles
        .iter()
        .map(|r| resolve::Role {
            id: r.id,
            position: r.position,
            allow: r.allow,
            deny: r.deny,
        })
        .collect();
    let input = resolve::Input {
        owner: ctx.community_owner.0,
        everyone: ctx.everyone,
        roles: &roles,
        held: &member.role_ids,
    };
    Some(resolve::channel(&input, channel_overwrites, ctx.me.0))
}

/// The bits a channel's grid offers: every right a channel overwrite can neither grant nor remove is left out.
pub fn channel_grid_bits(bit: u32) -> bool {
    bit & perm::COMMUNITY_BITS == 0
}

// ---- the commands ----

/// What a command's answer is: the refusal as the sections read it.
pub struct Reply<T>(tokio::sync::oneshot::Receiver<Result<Result<T, AdminError>, ClientError>>);

impl<T> Reply<T> {
    /// Waits for the command (a stopped client is a failure before any answer).
    pub async fn wait(self) -> Result<T, AdminError> {
        match self.0.await {
            Ok(Ok(answer)) => answer,
            Ok(Err(_)) | Err(_) => Err(AdminError::local("E_NETWORK")),
        }
    }
}

/// The server settings area's handle for server admin: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct ServerAdmin {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`ServerAdmin`] handle.
    pub fn server_admin(&self) -> ServerAdmin {
        ServerAdmin {
            inner: self.inner().clone(),
        }
    }
}

fn random_idem() -> Vec<u8> {
    let mut b = vec![0u8; 16];
    let _ = getrandom::fill(&mut b);
    b
}

/// One socket request of route `R`, its answer accepted at `ok` (any 2xx when empty).
async fn request<R: Route>(
    inner: &Inner,
    args: &[&str],
    body: Option<Vec<u8>>,
    ok: &[u16],
) -> Result<Vec<u8>, AdminError> {
    let method: &'static str = match R::METHOD {
        http::Method::GET => "GET",
        http::Method::POST => "POST",
        http::Method::PUT => "PUT",
        http::Method::PATCH => "PATCH",
        http::Method::DELETE => "DELETE",
        _ => return Err(AdminError::local("E_ROUTE")),
    };
    let route = fill_path(R::PATH, args).map_err(|_| AdminError::local("E_ROUTE"))?;
    let idem = (method != "GET").then(random_idem);
    let answer = inner
        .socket_requests()
        .request(
            method,
            route,
            zeroize::Zeroizing::new(body.unwrap_or_default()),
            idem,
        )
        .await?;
    let accepted = if ok.is_empty() {
        (200..300).contains(&answer.status)
    } else {
        ok.contains(&answer.status)
    };
    if accepted {
        Ok(answer.body)
    } else {
        Err(ApiError::from_body(answer.status, &answer.body).into())
    }
}

fn id_hex(community: CommunityId) -> String {
    community.hex()
}

impl ServerAdmin {
    fn run<T: Send + 'static>(
        &self,
        work: impl std::future::Future<Output = Result<T, AdminError>> + Send + 'static,
    ) -> Reply<T> {
        Reply(self.inner.spawn(async move { Ok(work.await) }))
    }

    /// GET /permissions, /roles and /overwrites: what this account may manage here (the sections show by it), the
    /// `@everyone` role and the channels' overwrites. The overwrites may fail alone: they are then "not loaded".
    pub fn load_access(&self, community: CommunityId) -> Reply<Access> {
        let inner = self.inner.clone();
        self.run(async move { read_access(&inner, community).await })
    }

    /// roles.ts `setChannelPrivate`: reads the access again, refuses when the overwrites cannot be read or the change
    /// would hide the channel from this account, then writes (or deletes) the `@everyone` overwrite.
    pub fn set_channel_private(&self, channel: ChannelId, make_private: bool) -> Reply<()> {
        let admin = self.clone();
        self.run(async move {
            let inner = &admin.inner;
            let Some(community) = inner.snapshot().channel(channel).map(|c| c.community_id) else {
                return Err(AdminError::local("E_BAD_INPUT"));
            };
            let (access, rows) = read_access_with(inner, community).await?;
            let (Some(rows), Some(everyone)) = (rows, access.everyone) else {
                return Err(AdminError::local("E_BAD_INPUT"));
            };
            let snapshot = inner.snapshot();
            let current = rows
                .iter()
                .find(|o| o.channel == channel && o.kind == 0 && o.target == everyone)
                .map(|o| (o.allow, o.deny));
            if make_private && !access.owner && !access.administrator {
                let me = snapshot.account.user_id.unwrap_or(UserId([0; 16]));
                let held: Vec<[u8; 16]> = snapshot
                    .members
                    .get(&community)
                    .and_then(|ms| ms.iter().find(|m| m.user_id == me))
                    .map(|m| m.role_ids.clone())
                    .unwrap_or_default();
                if !allows_me_to_view(&rows, channel, everyone, me, &held) {
                    return Err(AdminError::local("E_WOULD_HIDE"));
                }
            }
            let Some((allow, deny)) = privacy_plan(current, make_private) else {
                return Ok(());
            };
            if allow == 0 && deny == 0 {
                let target = crate::state::hex(&everyone);
                match request::<r::DeleteOverwrite>(
                    inner,
                    &[&channel.hex(), "0", &target],
                    None,
                    &[],
                )
                .await
                {
                    Ok(_) => {}
                    Err(e) if e.status_is(404) => {}
                    Err(e) => return Err(e),
                }
            } else {
                admin
                    .set_overwrite(community, channel, 0, everyone, allow, deny)
                    .wait()
                    .await?;
            }
            read_access(inner, community).await?;
            Ok(())
        })
    }

    /// GET /afk/settings: the AFK channel and the timeout. A failed read is published as such.
    pub fn load_afk(&self, community: CommunityId) -> Reply<AfkSettings> {
        let inner = self.inner.clone();
        self.run(async move {
            let read = async {
                let raw = request::<r::GetAfkSettings>(&inner, &[&id_hex(community)], None, &[200])
                    .await?;
                let value = decode(&raw)?;
                let a = at_least(&value, 2)?;
                Ok::<_, AdminError>(AfkSettings {
                    channel: opt(&a[0], bin16)?.map(ChannelId),
                    timeout_seconds: u53(&a[1])?,
                })
            }
            .await;
            let published = read.clone();
            inner.publish(move |s| match published {
                Ok(settings) => {
                    s.server_admin.afk.insert(community, settings);
                    s.server_admin.afk_load_failed.insert(community, false);
                }
                Err(_) => {
                    s.server_admin.afk_load_failed.insert(community, true);
                }
            });
            read
        })
    }

    /// PUT /afk/settings: `[timeout_seconds]`; the channel is the built-in one.
    pub fn set_afk(&self, community: CommunityId, timeout_seconds: u64) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            if !AFK_TIMEOUTS.contains(&timeout_seconds) {
                return Err(AdminError::local("E_ROUTE_INPUT"));
            }
            let body = Value::Array(vec![Value::Uint(timeout_seconds)]).encode();
            request::<r::SetAfkSettings>(&inner, &[&id_hex(community)], Some(body), &[204]).await?;
            inner.publish(move |s| {
                let channel = s.server_admin.afk.get(&community).and_then(|a| a.channel);
                s.server_admin.afk.insert(
                    community,
                    AfkSettings {
                        channel,
                        timeout_seconds,
                    },
                );
            });
            Ok(())
        })
    }

    /// PATCH /communities/{id}: `[name, null, null, null]`; the list shows the new name at once.
    pub fn rename(&self, community: CommunityId, name: &str) -> Reply<()> {
        let inner = self.inner.clone();
        let name = name.trim().to_owned();
        self.run(async move {
            if name_problem(&name, COMMUNITY_NAME_MAX_BYTES).is_some() {
                return Err(AdminError::local("E_BAD_INPUT"));
            }
            let body = Value::Array(vec![
                Value::Text(name.clone()),
                Value::Null,
                Value::Null,
                Value::Null,
            ])
            .encode();
            request::<r::PatchCommunity>(&inner, &[&id_hex(community)], Some(body), &[]).await?;
            inner.publish(move |s| {
                if let Some(c) = s.communities.iter_mut().find(|c| c.id == community) {
                    c.name = name;
                }
            });
            Ok(())
        })
    }

    /// POST /communities/{id}/leave.
    pub fn leave(&self, community: CommunityId) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            let body = Value::Array(Vec::new()).encode();
            request::<r::LeaveCommunity>(&inner, &[&id_hex(community)], Some(body), &[]).await?;
            forget(&inner, community);
            Ok(())
        })
    }

    /// DELETE /communities/{id}.
    pub fn delete_community(&self, community: CommunityId) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            request::<r::DeleteCommunity>(&inner, &[&id_hex(community)], None, &[]).await?;
            forget(&inner, community);
            Ok(())
        })
    }

    /// POST /communities/{id}/owner: `[user]`; the owner hands the server to another member.
    pub fn transfer_ownership(&self, community: CommunityId, user: UserId) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            let body = Value::Array(vec![Value::Bytes(user.0.to_vec())]).encode();
            request::<r::TransferOwnership>(&inner, &[&id_hex(community)], Some(body), &[]).await?;
            inner.publish(move |s| {
                if let Some(c) = s.communities.iter_mut().find(|c| c.id == community) {
                    c.owner = user;
                }
                s.roles.access.remove(&community);
            });
            Ok(())
        })
    }

    /// POST /channels with kind 2: a category, after the last ordinary one.
    pub fn create_category(&self, community: CommunityId, name: &str) -> Reply<ChannelId> {
        let inner = self.inner.clone();
        let name = name.trim().to_owned();
        self.run(async move {
            if name_problem(&name, CHANNEL_NAME_MAX_BYTES).is_some() {
                return Err(AdminError::local("E_BAD_INPUT"));
            }
            let snapshot = inner.snapshot();
            let afk = snapshot
                .server_admin
                .afk
                .get(&community)
                .and_then(|a| a.channel);
            let position = next_position(
                snapshot
                    .channels
                    .iter()
                    .filter(|c| c.community_id == community && Some(c.id) != afk)
                    .map(|c| c.position),
            );
            let body = Value::Array(vec![
                Value::Uint(2),
                Value::Uint(0),
                Value::Uint(0),
                Value::Null,
                Value::Text(name.clone()),
                Value::Text(String::new()),
                Value::Uint(u64::from(position)),
                Value::Uint(0),
            ])
            .encode();
            let raw =
                request::<r::CreateChannel>(&inner, &[&id_hex(community)], Some(body), &[]).await?;
            let value = decode(&raw)?;
            let a = at_least(&value, 3)?;
            let id = ChannelId(bin16(&a[0])?);
            let mode = u8::try_from(u53(&a[1])?).unwrap_or(0);
            inner.publish(move |s| {
                s.channels.push(Channel {
                    id,
                    community_id: community,
                    kind: 2,
                    mode,
                    name,
                    topic: String::new(),
                    parent_id: None,
                    position,
                    slowmode: 0,
                    text_group_id: None,
                    group: crate::state::ChannelGroupState::Unsupported,
                });
            });
            Ok(id)
        })
    }

    /// PATCH /channels/{id}: `[name, topic, null, null, parent, null, slowmode]`; `None` leaves a field alone.
    pub fn update_channel(
        &self,
        channel: ChannelId,
        name: Option<&str>,
        topic: Option<&str>,
        parent: Parent,
        slowmode: Option<u32>,
    ) -> Reply<()> {
        let inner = self.inner.clone();
        let name = name.map(|n| n.trim().to_owned());
        let topic = topic.map(str::to_owned);
        self.run(async move {
            if name
                .as_deref()
                .is_some_and(|n| name_problem(n, CHANNEL_NAME_MAX_BYTES).is_some())
                || topic.as_deref().is_some_and(|t| t.len() > TOPIC_MAX_BYTES)
                || slowmode.is_some_and(|s| !SLOW_STEPS.contains(&s))
            {
                return Err(AdminError::local("E_BAD_INPUT"));
            }
            let patch = ChannelPatch {
                name: name.clone(),
                topic: topic.clone(),
                parent: match parent {
                    Parent::Keep => None,
                    Parent::Top => Some(None),
                    Parent::Category(c) => Some(Some(c)),
                },
                position: None,
                slowmode,
            };
            patch_channel(&inner, channel, &patch).await?;
            inner.publish(move |s| {
                if let Some(c) = s.channels.iter_mut().find(|c| c.id == channel) {
                    if let Some(n) = name {
                        c.name = n;
                    }
                    if let Some(t) = topic {
                        c.topic = t;
                    }
                    if let Some(sm) = slowmode {
                        c.slowmode = sm;
                    }
                    match parent {
                        Parent::Keep => {}
                        Parent::Top => c.parent_id = None,
                        Parent::Category(p) => c.parent_id = Some(p),
                    }
                }
            });
            Ok(())
        })
    }

    /// Moves a channel one step ([`plan_move`]); whatever happens, the lists are read again by the sync engine.
    pub fn move_channel(&self, channel: ChannelId, direction: Direction) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            let snapshot = inner.snapshot();
            let Some(community) = snapshot.channel(channel).map(|c| c.community_id) else {
                return Err(AdminError::local("E_BAD_INPUT"));
            };
            let afk = snapshot
                .server_admin
                .afk
                .get(&community)
                .and_then(|a| a.channel);
            let changes = plan_move(&snapshot.channels, channel, direction, afk);
            let mut sent = Vec::new();
            let mut failure = None;
            for (id, position) in &changes {
                let patch = ChannelPatch {
                    position: Some(*position),
                    ..ChannelPatch::default()
                };
                match patch_channel(&inner, *id, &patch).await {
                    Ok(()) => sent.push((*id, *position)),
                    Err(e) => {
                        failure = Some(e);
                        break;
                    }
                }
            }
            inner.publish(move |s| {
                for (id, position) in sent {
                    if let Some(c) = s.channels.iter_mut().find(|c| c.id == id) {
                        c.position = position;
                    }
                }
            });
            failure.map_or(Ok(()), Err)
        })
    }

    /// DELETE /channels/{id}.
    pub fn delete_channel(&self, channel: ChannelId) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            request::<r::DeleteChannel>(&inner, &[&channel.hex()], None, &[]).await?;
            inner.publish(move |s| {
                s.channels.retain(|c| c.id != channel);
                for c in &mut s.channels {
                    if c.parent_id == Some(channel) {
                        c.parent_id = None;
                    }
                }
            });
            Ok(())
        })
    }

    /// GET /overwrites: the overwrites of every channel this account may view.
    pub fn load_overwrites(&self, community: CommunityId) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            let raw =
                request::<r::ListOverwrites>(&inner, &[&id_hex(community)], None, &[200]).await?;
            let rows = decode_overwrites(&raw)?;
            inner.publish(move |s| put_overwrites(s, community, &rows));
            Ok(())
        })
    }

    /// PUT /channels/{id}/overwrites/{kind}/{target}: `[allow, deny]`; kind 0 is a role, 1 a member. Both 0 deletes it.
    pub fn set_overwrite(
        &self,
        community: CommunityId,
        channel: ChannelId,
        kind: u8,
        target: [u8; 16],
        allow: u32,
        deny: u32,
    ) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            if !overwrite_valid(kind, allow, deny) {
                return Err(AdminError::local("E_ROUTE_INPUT"));
            }
            let kind_text = kind.to_string();
            let target_text = crate::state::hex(&target);
            let args = [channel.hex(), kind_text, target_text];
            let args: Vec<&str> = args.iter().map(String::as_str).collect();
            if allow == 0 && deny == 0 {
                // Nothing left to say: the overwrite goes (a 404 is a retried delete that already went through).
                match request::<r::DeleteOverwrite>(&inner, &args, None, &[]).await {
                    Ok(_) => {}
                    Err(e) if e.status_is(404) => {}
                    Err(e) => return Err(e),
                }
            } else {
                let body = Value::Array(vec![Value::Uint(allow.into()), Value::Uint(deny.into())])
                    .encode();
                request::<r::PutOverwrite>(&inner, &args, Some(body), &[]).await?;
            }
            inner.publish(move |s| {
                let rows = s
                    .roles
                    .overwrites
                    .entry(community)
                    .or_default()
                    .entry(channel)
                    .or_default();
                rows.retain(|o| !(o.kind == kind && o.target_id == target));
                if allow != 0 || deny != 0 {
                    rows.push(crate::state::roles::OverwriteSummary {
                        kind,
                        target_id: target,
                        allow,
                        deny,
                    });
                }
            });
            Ok(())
        })
    }

    /// DELETE /channels/{id}/overwrites/{kind}/{target}.
    pub fn clear_overwrite(
        &self,
        community: CommunityId,
        channel: ChannelId,
        kind: u8,
        target: [u8; 16],
    ) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            let kind_text = kind.to_string();
            let target_text = crate::state::hex(&target);
            request::<r::DeleteOverwrite>(
                &inner,
                &[&channel.hex(), &kind_text, &target_text],
                None,
                &[],
            )
            .await?;
            inner.publish(move |s| {
                if let Some(rows) = s
                    .roles
                    .overwrites
                    .get_mut(&community)
                    .and_then(|by| by.get_mut(&channel))
                {
                    rows.retain(|o| !(o.kind == kind && o.target_id == target));
                }
            });
            Ok(())
        })
    }

    /// GET /invites, then only the ones that can still be used.
    pub fn load_invites(&self, community: CommunityId, now: u64) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            let raw =
                request::<r::ListInvites>(&inner, &[&id_hex(community)], None, &[200]).await?;
            let rows = usable(&decode_invites(&raw)?, now);
            inner.publish(move |s| {
                s.server_admin.invites.insert(community, rows);
            });
            Ok(())
        })
    }

    /// POST /invites: `[max_uses, ttl_seconds, 0]`; 0 is any number of uses and never expires.
    pub fn create_invite(
        &self,
        community: CommunityId,
        max_uses: u64,
        ttl_seconds: u64,
    ) -> Reply<InviteCreated> {
        let inner = self.inner.clone();
        self.run(async move {
            if max_uses > INVITE_USES_MAX || ttl_seconds > INVITE_TTL_MAX_S {
                return Err(AdminError::local("E_ROUTE_INPUT"));
            }
            let body = Value::Array(vec![
                Value::Uint(max_uses),
                Value::Uint(ttl_seconds),
                Value::Uint(0),
            ])
            .encode();
            let raw =
                request::<r::CreateInvite>(&inner, &[&id_hex(community)], Some(body), &[]).await?;
            let value = decode(&raw)?;
            let a = at_least(&value, 4)?;
            let code = text(&a[1])?;
            let url = invite_url(&text(&a[2])?, &code)
                .ok_or_else(|| AdminError::local("E_CBOR_RANGE"))?;
            Ok(InviteCreated {
                id: InviteId(bin16(&a[0])?),
                code,
                url,
                expires: uint(&a[3])?,
            })
        })
    }

    /// DELETE /invites/{id}. A 404 is success (a retried request whose first attempt went through).
    pub fn revoke_invite(&self, community: CommunityId, invite: InviteId) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            match request::<r::RevokeInvite>(&inner, &[&invite.hex()], None, &[]).await {
                Ok(_) => {}
                Err(e) if e.status_is(404) => {}
                Err(e) => return Err(e),
            }
            inner.publish(move |s| {
                if let Some(rows) = s.server_admin.invites.get_mut(&community) {
                    rows.retain(|i| i.id != invite);
                }
            });
            Ok(())
        })
    }

    /// GET /gifs: whether this server has a key, and its rating.
    pub fn load_gifs(&self, community: CommunityId) -> Reply<GifSettings> {
        let inner = self.inner.clone();
        self.run(async move {
            let raw = request::<r::GifStatus>(&inner, &[&id_hex(community)], None, &[200]).await?;
            let value = decode(&raw)?;
            let a = at_least(&value, 2)?;
            let rating = GifRating::from_wire(&text(&a[1])?)
                .ok_or_else(|| AdminError::local("E_CBOR_RANGE"))?;
            let settings = GifSettings {
                on: uint(&a[0])? == 1,
                rating,
            };
            inner.publish(move |s| {
                s.server_admin.gifs.insert(community, settings);
            });
            Ok(settings)
        })
    }

    /// PUT /gifs/key: sets or replaces the server's key. The key only ever goes up and is in no error and no state.
    ///
    /// The key is a [`Zeroizing`] from the caller on; its request body is built straight into a `Zeroizing<Vec<u8>>`
    /// ([`gif_key_body`]) and wiped when the request settles, whatever its answer.
    pub fn set_gif_key(&self, community: CommunityId, key: Zeroizing<String>) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            if !gif_key_valid(&key) {
                return Err(AdminError::local("E_ROUTE_INPUT"));
            }
            let body = gif_key_body(&key);
            let sent =
                request::<r::GifSetKey>(&inner, &[&id_hex(community)], Some(body.to_vec()), &[204])
                    .await;
            drop(body);
            sent?;
            inner.publish(move |s| {
                s.server_admin.gifs.entry(community).or_default().on = true;
            });
            Ok(())
        })
    }

    /// DELETE /gifs/key.
    pub fn clear_gif_key(&self, community: CommunityId) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            request::<r::GifClearKey>(&inner, &[&id_hex(community)], None, &[204]).await?;
            inner.publish(move |s| {
                s.server_admin.gifs.entry(community).or_default().on = false;
            });
            Ok(())
        })
    }

    /// PUT /gifs/rating.
    pub fn set_gif_rating(&self, community: CommunityId, rating: GifRating) -> Reply<()> {
        let inner = self.inner.clone();
        self.run(async move {
            let body = Value::Array(vec![Value::Text(rating.wire().to_owned())]).encode();
            request::<r::GifSetRating>(&inner, &[&id_hex(community)], Some(body), &[204]).await?;
            inner.publish(move |s| {
                s.server_admin.gifs.entry(community).or_default().rating = rating;
            });
            Ok(())
        })
    }
}

/// routes.ts `ChannelPatch`: a parent of `Some(None)` is the top level (sixteen zero bytes).
#[derive(Clone, Debug, Default)]
struct ChannelPatch {
    name: Option<String>,
    topic: Option<String>,
    parent: Option<Option<ChannelId>>,
    position: Option<u32>,
    slowmode: Option<u32>,
}

/// PATCH /v1/channels/{id}: `[name, topic, mode, visibility, parent_id, position, slowmode]`, null leaves a field alone.
async fn patch_channel(
    inner: &Inner,
    channel: ChannelId,
    patch: &ChannelPatch,
) -> Result<(), AdminError> {
    let position = patch.position.map(u64::from);
    if position.is_some_and(|p| p > CHANNEL_POSITION_MAX) {
        return Err(AdminError::local("E_ROUTE_INPUT"));
    }
    let body = Value::Array(vec![
        patch.name.clone().map_or(Value::Null, Value::Text),
        patch.topic.clone().map_or(Value::Null, Value::Text),
        Value::Null,
        Value::Null,
        match patch.parent {
            None => Value::Null,
            Some(None) => Value::Bytes(vec![0; 16]),
            Some(Some(c)) => Value::Bytes(c.0.to_vec()),
        },
        position.map_or(Value::Null, Value::Uint),
        patch
            .slowmode
            .map_or(Value::Null, |s| Value::Uint(s.into())),
    ])
    .encode();
    request::<r::PatchChannel>(inner, &[&channel.hex()], Some(body), &[]).await?;
    Ok(())
}

/// The access read of [`ServerAdmin::load_access`].
async fn read_access(inner: &Arc<Inner>, community: CommunityId) -> Result<Access, AdminError> {
    read_access_with(inner, community).await.map(|(a, _)| a)
}

/// [`read_access`], with the overwrites it read (what the published state will hold: a publish lands on the publisher's
/// loop, so a caller that acts on what it just read must not read the snapshot back).
async fn read_access_with(
    inner: &Arc<Inner>,
    community: CommunityId,
) -> Result<(Access, Option<Vec<Overwrite>>), AdminError> {
    let hex = id_hex(community);
    let args = [hex.as_str()];
    let (perms, roles, overwrites) = tokio::join!(
        request::<r::GetPermissions>(inner, &args, None, &[200]),
        request::<r::ListRoles>(inner, &args, None, &[200]),
        request::<r::ListOverwrites>(inner, &args, None, &[200]),
    );
    let value = decode(&perms?)?;
    let a = at_least(&value, 2)?;
    let top = u53(&a[1])?;
    if top > OWNER_TOP {
        return Err(AdminError::local("E_CBOR_RANGE"));
    }
    let roles = decode_roles(&roles?)?;
    let overwrites = overwrites.ok().and_then(|raw| decode_overwrites(&raw).ok());
    let snapshot = inner.snapshot();
    let owner = snapshot
        .communities
        .iter()
        .find(|c| c.id == community)
        .is_some_and(|c| Some(c.owner) == snapshot.account.user_id);
    // The one resolver (rank.ts `accessOf`) into the one slice: the members area and the Bot pages read this read too.
    let mut one = crate::features::roles::rank::access_of(uint(&a[0])?, top as u32, owner);
    one.everyone_id = roles.iter().find(|r| r.position == 0).map(|r| r.id);
    let access = Access {
        overwrites_loaded: overwrites.is_some(),
        ..Access::from(&one)
    };
    let published = overwrites.clone();
    inner.publish(move |s| {
        s.roles.access.insert(community, one);
        s.server_admin.roles.insert(community, roles);
        if let Some(rows) = published {
            put_overwrites(s, community, &rows);
        }
    });
    Ok((access, overwrites))
}

/// A server this account left or deleted: gone from the lists and from this slice.
fn forget(inner: &Inner, community: CommunityId) {
    inner.publish(move |s| {
        s.communities.retain(|c| c.id != community);
        s.channels.retain(|c| c.community_id != community);
        s.members.remove(&community);
        s.roles.access.remove(&community);
        s.roles.overwrites.remove(&community);
        let a = &mut s.server_admin;
        a.roles.remove(&community);
        a.afk.remove(&community);
        a.afk_load_failed.remove(&community);
        a.invites.remove(&community);
        a.gifs.remove(&community);
        a.welcome.remove(&community);
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::state::ChannelGroupState;

    /// Integration 3, re-review N4: one overwrites slice. The member list's access read can land before any overwrites
    /// are read: an edit then refuses (`chperm.error.unread`, as ChannelPermissions.tsx does) instead of replacing the
    /// entry's unknown masks. Once the overwrites are read (by either read: one slice), an edit keeps what it does not
    /// change, so @everyone's View deny stays when Send is changed.
    #[test]
    fn a_permission_edit_waits_for_the_overwrites_and_keeps_what_it_does_not_change() {
        let community = CommunityId([7; 16]);
        let channel = ChannelId([8; 16]);
        let everyone = [9; 16];
        let mut s = crate::state::AppState::default();
        // The member list's read landed: the access, with @everyone's id, but no overwrites.
        let mut access =
            crate::features::roles::rank::access_of(u64::from(perm::MANAGE_ROLES), 5, false);
        access.everyone_id = Some(everyone);
        s.roles.access.insert(community, access);
        assert!(!crate::state::server_admin::access_in(&s, community).overwrites_loaded);
        assert_eq!(
            change_of(
                &s,
                community,
                channel,
                (0, everyone),
                perm::SEND_MESSAGES,
                Tri::Deny
            ),
            Err("chperm.error.unread"),
            "unread: the edit refuses"
        );
        // The overwrites are read: @everyone may not view the channel.
        s.roles.overwrites.entry(community).or_default().insert(
            channel,
            vec![crate::state::roles::OverwriteSummary {
                kind: 0,
                target_id: everyone,
                allow: 0,
                deny: perm::VIEW_CHANNEL,
            }],
        );
        assert!(crate::state::server_admin::access_in(&s, community).overwrites_loaded);
        let rows = crate::state::server_admin::overwrites_in(&s, community).expect("read");
        assert!(is_private(&rows, channel, everyone));
        assert_eq!(
            change_of(
                &s,
                community,
                channel,
                (0, everyone),
                perm::SEND_MESSAGES,
                Tri::Deny
            ),
            Ok((0, perm::VIEW_CHANNEL | perm::SEND_MESSAGES)),
            "the View deny stays"
        );
    }

    /// Review M9: a role position above u32 (the wire allows u53) keeps its rank: the higher of two such roles applies
    /// last, so its allow wins over the lower one's deny (no fall back to ordering by id).
    #[test]
    fn positions_above_u32_keep_their_order() {
        let (everyone, low, high) = ([1; 16], [0xf0; 16], [0x0f; 16]);
        let roles = [
            resolve::Role {
                id: everyone,
                position: 0,
                allow: perm::VIEW_CHANNEL,
                deny: 0,
            },
            resolve::Role {
                id: low,
                position: (1 << 33) + 1,
                allow: 0,
                deny: perm::SEND_MESSAGES,
            },
            resolve::Role {
                id: high,
                position: (1 << 33) + 2,
                allow: perm::SEND_MESSAGES,
                deny: 0,
            },
        ];
        let held = [low, high];
        let input = resolve::Input {
            owner: [9; 16],
            everyone,
            roles: &roles,
            held: &held,
        };
        assert_ne!(resolve::community(&input, [7; 16]) & perm::SEND_MESSAGES, 0);
    }

    fn ch(id: u8, kind: u8, parent: Option<u8>, position: u32) -> Channel {
        Channel {
            id: ChannelId([id; 16]),
            community_id: CommunityId([9; 16]),
            kind,
            mode: 0,
            name: format!("c{id}"),
            topic: String::new(),
            parent_id: parent.map(|p| ChannelId([p; 16])),
            position,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::None,
        }
    }

    fn id(n: u8) -> ChannelId {
        ChannelId([n; 16])
    }

    // ---- admin/rank.ts, admin/perm.ts ----

    #[test]
    fn the_owner_and_an_administrator_hold_every_permission() {
        let owner = access_of(0, OWNER_TOP, true);
        assert!(
            owner.loaded && owner.manage_community && owner.manage_channels && owner.view_audit_log
        );
        let admin = access_of(u64::from(perm::ADMINISTRATOR), 5, false);
        assert!(admin.manage_roles && admin.create_invite && admin.ban_members);
        assert!(!admin.owner);
    }

    #[test]
    fn a_member_holds_only_the_bits_it_was_given() {
        let a = access_of(
            u64::from(perm::CREATE_INVITE | perm::MANAGE_CHANNELS),
            1,
            false,
        );
        assert!(a.create_invite && a.manage_channels);
        assert!(!a.manage_community && !a.manage_roles && !a.ban_members && !a.view_audit_log);
    }

    // ---- Create.tsx nameProblem, shared.ts nfc ----

    #[test]
    fn a_name_is_empty_long_or_has_a_control_character() {
        assert_eq!(name_problem("  ", 10), Some(NameProblem::Empty));
        assert_eq!(name_problem("abcdefghijk", 10), Some(NameProblem::Long));
        // The bound is in UTF-8 bytes, not characters.
        assert_eq!(name_problem("ååååå", 9), Some(NameProblem::Long));
        assert_eq!(name_problem("a\u{7}b", 10), Some(NameProblem::Control));
        assert_eq!(name_problem(" Midgard ", 10), None);
    }

    #[test]
    fn a_typed_name_compares_under_nfc() {
        let nfd = "Bjo\u{308}rn";
        assert_ne!(nfd, "Björn");
        assert_eq!(nfc(nfd), nfc("Björn"));
        assert_eq!(nfc("  Björn "), "Björn");
    }

    #[test]
    fn a_refusal_is_read_by_status_or_code() {
        let e = AdminError {
            status: 403,
            code: "E_X".into(),
        };
        assert!(e.forbidden() && e.status_is(403) && !e.refused());
        let e = AdminError {
            status: 400,
            code: "E_BAD_INPUT".into(),
        };
        assert!(e.refused() && !e.forbidden());
    }

    #[test]
    fn a_giphy_key_is_8_to_64_letters_and_digits() {
        assert!(gif_key_valid("abcdEFGH12"));
        assert!(!gif_key_valid("short"));
        assert!(!gif_key_valid("has space 1234"));
        assert!(!gif_key_valid(&"a".repeat(65)));
        assert!(gif_key_valid(&"a".repeat(64)));
    }

    // ---- controller.ts loadInvites ----

    fn invite(n: u8, max: u64, uses: u64, expires: u64, revoked: Option<u64>) -> InviteRow {
        InviteRow {
            id: InviteId([n; 16]),
            created_by: None,
            max_uses: max,
            uses,
            created: 100,
            expires,
            revoked_at: revoked,
        }
    }

    #[test]
    fn only_invites_that_can_still_be_used_are_listed() {
        let rows = [
            invite(1, 0, 50, 0, None),      // no limit, never expires
            invite(2, 10, 10, 0, None),     // used up
            invite(3, 10, 3, 500, None),    // not yet expired
            invite(4, 10, 3, 400, None),    // expired exactly now
            invite(5, 10, 3, 0, Some(200)), // revoked
            invite(6, 1, 0, 1_000, None),   // one use left
        ];
        let listed: Vec<u8> = usable(&rows, 400).iter().map(|i| i.id.0[0]).collect();
        assert_eq!(listed, [1, 3, 6]);
    }

    #[test]
    fn an_invite_row_is_decoded_from_its_columns() {
        let row = |n: u8| {
            Value::Array(vec![
                Value::Bytes(vec![n; 16]),
                Value::Bytes(vec![9; 16]),
                Value::Bytes(vec![7; 16]),
                Value::Text("x".into()),
                Value::Uint(10),
                Value::Uint(2),
                Value::Uint(100),
                Value::Uint(900),
                Value::Null,
            ])
        };
        let raw = Value::Array(vec![row(1), row(2)]).encode();
        let rows = decode_invites(&raw).unwrap();
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[0].max_uses, 10);
        assert_eq!(rows[0].uses, 2);
        assert_eq!(rows[0].expires, 900);
        assert_eq!(rows[0].created_by, Some(UserId([7; 16])));
        assert_eq!(rows[0].revoked_at, None);
        assert!(decode_invites(&Value::Array(vec![Value::Array(vec![])]).encode()).is_err());
    }

    #[test]
    fn an_invite_url_must_be_the_landing_of_its_code() {
        assert!(invite_url("https://d.example/i/abc123", "abc123").is_some());
        assert!(invite_url("http://d.example:8080/i/abc123", "abc123").is_some());
        assert!(invite_url("https://d.example/i/other", "abc123").is_none());
        assert!(invite_url("https://d.example/i/abc123?x=1", "abc123").is_none());
        assert!(invite_url("https://d.example/i/abc123#frag", "abc123").is_none());
        assert!(invite_url("ftp://d.example/i/abc123", "abc123").is_none());
        assert!(invite_url("not a url", "abc123").is_none());
    }

    // ---- controller.ts nextPosition, moveChannel ----

    #[test]
    fn a_new_category_goes_after_the_last_ordinary_channel() {
        assert_eq!(next_position([]), 0);
        assert_eq!(next_position([0, 1, 4]), 5);
        // The AFK reservation does not count.
        assert_eq!(next_position([0, 2, AFK_BUILTIN_POSITION]), 3);
    }

    #[test]
    fn a_strictly_ordered_channel_trades_places_with_its_neighbour() {
        let list = [ch(1, 0, None, 0), ch(2, 0, None, 1), ch(3, 0, None, 5)];
        let up = plan_move(&list, id(3), Direction::Up, None);
        assert_eq!(up, [(id(3), 1), (id(2), 5)]);
        let down = plan_move(&list, id(1), Direction::Down, None);
        assert_eq!(down, [(id(1), 1), (id(2), 0)]);
    }

    #[test]
    fn the_first_cannot_move_up_and_the_last_cannot_move_down() {
        let list = [ch(1, 0, None, 0), ch(2, 0, None, 1)];
        assert!(plan_move(&list, id(1), Direction::Up, None).is_empty());
        assert!(plan_move(&list, id(2), Direction::Down, None).is_empty());
    }

    #[test]
    fn the_afk_channel_never_moves_and_is_never_passed() {
        let list = [
            ch(1, 0, None, 0),
            ch(2, 0, None, 1),
            ch(3, 1, None, AFK_BUILTIN_POSITION),
        ];
        // Not by id (no AFK settings) and by id.
        assert!(plan_move(&list, id(3), Direction::Up, None).is_empty());
        assert!(plan_move(&list, id(3), Direction::Up, Some(id(3))).is_empty());
        // Down from the last ordinary channel passes nothing.
        assert!(plan_move(&list, id(2), Direction::Down, Some(id(3))).is_empty());
    }

    #[test]
    fn ties_are_renumbered_densely_and_only_changes_are_sent() {
        // Three channels at the same position (a client that made them without one): id order is the display order.
        let list = [ch(1, 0, None, 0), ch(2, 0, None, 0), ch(3, 0, None, 0)];
        let plan = plan_move(&list, id(3), Direction::Up, None);
        // New order 1, 3, 2 -> positions 0, 1, 2; channel 1 stays at 0 and is not patched.
        assert_eq!(plan, [(id(3), 1), (id(2), 2)]);
    }

    #[test]
    fn a_channel_at_or_above_the_reservation_forces_the_dense_renumbering() {
        let list = [ch(1, 0, None, 0), ch(2, 0, None, AFK_BUILTIN_POSITION + 3)];
        let plan = plan_move(&list, id(2), Direction::Up, None);
        assert_eq!(plan, [(id(2), 0), (id(1), 1)]);
    }

    #[test]
    fn a_move_stays_inside_its_parent_and_kind() {
        let list = [
            ch(10, 2, None, 0),
            ch(1, 0, Some(10), 0),
            ch(2, 0, Some(10), 1),
            ch(3, 0, None, 7),
            ch(11, 2, None, 3),
        ];
        // Channel 1 is the first inside its category: nothing above it, even though other channels are at the top level.
        assert!(plan_move(&list, id(1), Direction::Up, None).is_empty());
        assert_eq!(
            plan_move(&list, id(1), Direction::Down, None),
            [(id(1), 1), (id(2), 0)]
        );
        // A category moves among categories only.
        assert_eq!(
            plan_move(&list, id(10), Direction::Down, None),
            [(id(10), 3), (id(11), 0)]
        );
    }

    // ---- welcome-settings.test.ts ----

    #[test]
    fn welcome_settings_encode_and_decode_the_versioned_configuration() {
        let config = WelcomeSettings {
            channel: ChannelId([0xab; 16]),
            template: "Hej {member} 👋".into(),
            server_name: "Midgård".into(),
        };
        let bytes = encode_welcome(&config).unwrap();
        assert_eq!(decode_welcome(&bytes).unwrap(), Some(config));
    }

    #[test]
    fn the_empty_array_is_an_unconfigured_welcome_setting() {
        assert_eq!(decode_welcome(&[0x80]).unwrap(), None);
    }

    #[test]
    fn malformed_welcome_configuration_is_refused() {
        // Trailing bytes after the array.
        assert!(decode_welcome(&[0x80, 0x00]).is_err());
        let zero = WelcomeSettings {
            channel: ChannelId([0; 16]),
            template: "Hi {member}".into(),
            server_name: "Dilla".into(),
        };
        assert_eq!(encode_welcome(&zero), Err(WelcomeError::Channel));
        let long = WelcomeSettings {
            channel: ChannelId([1; 16]),
            template: "x".repeat(2001),
            server_name: "Dilla".into(),
        };
        assert_eq!(encode_welcome(&long), Err(WelcomeError::Text));
        let control = WelcomeSettings {
            channel: ChannelId([1; 16]),
            template: "x".into(),
            server_name: "Di\nlla".into(),
        };
        assert_eq!(encode_welcome(&control), Err(WelcomeError::Text));
        // A document with another version is refused.
        let v2 = Value::Array(vec![
            Value::Uint(2),
            Value::Bytes(vec![1; 16]),
            Value::Text("x".into()),
            Value::Text("y".into()),
        ])
        .encode();
        assert_eq!(decode_welcome(&v2), Err(WelcomeError::Document));
    }

    #[test]
    fn the_gif_key_body_is_the_cbor_array_of_the_key() {
        for key in ["abcd1234", "abcdEFGH12345678abcdEFGH", &"k".repeat(64)] {
            assert_eq!(
                *gif_key_body(key),
                Value::Array(vec![Value::Text(key.to_owned())]).encode(),
                "{key}"
            );
        }
    }

    #[test]
    fn the_gif_rating_round_trips_its_wire_text() {
        for r in GifRating::ALL {
            assert_eq!(GifRating::from_wire(r.wire()), Some(r));
        }
        assert_eq!(GifRating::from_wire("x"), None);
    }

    // ---- admin/perm.ts isPrivate, admin/roles.ts setChannelPrivate ----

    fn ow(channel: u8, kind: u8, target: u8, allow: u32, deny: u32) -> Overwrite {
        Overwrite {
            channel: id(channel),
            kind,
            target: [target; 16],
            allow,
            deny,
        }
    }

    #[test]
    fn a_channel_is_private_when_everyone_is_denied_viewing() {
        let everyone = [7; 16];
        let rows = [
            ow(1, 0, 7, 0, perm::VIEW_CHANNEL),
            ow(2, 0, 7, 0, perm::SEND_MESSAGES),
        ];
        assert!(is_private(&rows, id(1), everyone));
        assert!(!is_private(&rows, id(2), everyone));
        assert!(!is_private(&rows, id(3), everyone));
        // Another role's deny is not @everyone's.
        assert!(!is_private(
            &[ow(1, 0, 8, 0, perm::VIEW_CHANNEL)],
            id(1),
            everyone
        ));
    }

    #[test]
    fn privacy_takes_the_bit_off_the_allow_side_too() {
        let both = perm::VIEW_CHANNEL | perm::SEND_MESSAGES;
        assert_eq!(
            privacy_plan(Some((both, 0)), true),
            Some((perm::SEND_MESSAGES, perm::VIEW_CHANNEL))
        );
        assert_eq!(privacy_plan(None, true), Some((0, perm::VIEW_CHANNEL)));
    }

    #[test]
    fn opening_a_channel_takes_only_the_deny_back() {
        assert_eq!(privacy_plan(None, false), None);
        assert_eq!(privacy_plan(Some((0, perm::SEND_MESSAGES)), false), None);
        // Nothing left over means the overwrite is deleted by the caller.
        assert_eq!(
            privacy_plan(Some((0, perm::VIEW_CHANNEL)), false),
            Some((0, 0))
        );
        assert_eq!(
            privacy_plan(Some((1 << 5, perm::VIEW_CHANNEL | perm::SPEAK)), false),
            Some((1 << 5, perm::SPEAK))
        );
    }

    #[test]
    fn making_private_must_leave_this_account_a_way_in() {
        let everyone = [7; 16];
        let me = UserId([5; 16]);
        let rows = [
            ow(1, 1, 5, perm::VIEW_CHANNEL, 0),
            ow(1, 0, 8, perm::VIEW_CHANNEL, 0),
        ];
        assert!(allows_me_to_view(&rows, id(1), everyone, me, &[]));
        // A role allow counts only for a role this account holds, and never @everyone's.
        let by_role = [ow(1, 0, 8, perm::VIEW_CHANNEL, 0)];
        assert!(!allows_me_to_view(&by_role, id(1), everyone, me, &[]));
        assert!(allows_me_to_view(&by_role, id(1), everyone, me, &[[8; 16]]));
        let by_everyone = [ow(1, 0, 7, perm::VIEW_CHANNEL, 0)];
        assert!(!allows_me_to_view(
            &by_everyone,
            id(1),
            everyone,
            me,
            &[[7; 16]]
        ));
        // Another channel's allow is not this one's.
        assert!(!allows_me_to_view(&rows, id(2), everyone, me, &[]));
    }

    #[test]
    fn roles_are_decoded_and_everyone_is_the_one_at_position_zero() {
        let row = |n: u8, name: &str, position: u64| {
            Value::Array(vec![
                Value::Bytes(vec![n; 16]),
                Value::Text(name.into()),
                Value::Uint(0),
                Value::Uint(position),
                Value::Uint(0),
                Value::Uint(0),
                Value::Uint(0),
                Value::Uint(0),
            ])
        };
        let raw = Value::Array(vec![row(1, "@everyone", 0), row(2, "mods", 3)]).encode();
        let roles = decode_roles(&raw).unwrap();
        assert_eq!(roles.iter().find(|r| r.position == 0).unwrap().id, [1; 16]);
        assert_eq!(roles[1].name, "mods");
    }

    // ---- permissions.json and ChannelPermissions.tsx ----

    fn unhex(s: &str) -> [u8; 16] {
        let mut out = [0u8; 16];
        for (i, b) in out.iter_mut().enumerate() {
            *b = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).unwrap();
        }
        out
    }

    /// Every case of protocol/vectors/permissions.json: the community bits and the channel bits, as perm.go resolves them.
    #[test]
    fn the_resolver_matches_the_permission_vectors() {
        let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../../../protocol/vectors/permissions.json");
        let file: serde_json::Value =
            serde_json::from_str(&std::fs::read_to_string(path).unwrap()).unwrap();
        let cases = file["cases"].as_array().unwrap();
        assert!(cases.len() >= 20);
        for case in cases {
            let name = case["name"].as_str().unwrap();
            let roles: Vec<resolve::Role> = case["roles"]
                .as_array()
                .unwrap()
                .iter()
                .map(|r| resolve::Role {
                    id: unhex(r["id"].as_str().unwrap()),
                    position: r["position"].as_u64().unwrap(),
                    allow: u32::try_from(r["allow"].as_u64().unwrap()).unwrap(),
                    deny: u32::try_from(r["deny"].as_u64().unwrap()).unwrap(),
                })
                .collect();
            let held: Vec<[u8; 16]> = case["held"]
                .as_array()
                .unwrap()
                .iter()
                .map(|h| unhex(h.as_str().unwrap()))
                .collect();
            let input = resolve::Input {
                owner: unhex(case["owner"].as_str().unwrap()),
                everyone: unhex(case["everyone"].as_str().unwrap()),
                roles: &roles,
                held: &held,
            };
            let overwrites: Vec<Overwrite> = case["overwrites"]
                .as_array()
                .unwrap()
                .iter()
                .map(|o| Overwrite {
                    channel: id(1),
                    kind: u8::try_from(o["kind"].as_u64().unwrap()).unwrap(),
                    target: unhex(o["target"].as_str().unwrap()),
                    allow: u32::try_from(o["allow"].as_u64().unwrap()).unwrap(),
                    deny: u32::try_from(o["deny"].as_u64().unwrap()).unwrap(),
                })
                .collect();
            let user = unhex(case["user"].as_str().unwrap());
            assert_eq!(
                u64::from(resolve::community(&input, user)),
                case["community"].as_u64().unwrap(),
                "{name}: community"
            );
            assert_eq!(
                u64::from(resolve::channel(&input, &overwrites, user)),
                case["channel"].as_u64().unwrap(),
                "{name}: channel"
            );
        }
    }

    #[test]
    fn the_grid_groups_cover_the_26_bits_once() {
        let bits: Vec<u32> = PERM_GROUPS
            .iter()
            .flat_map(|(_, k)| k.iter().map(|(_, b)| *b))
            .collect();
        assert_eq!(bits.len(), 26);
        assert_eq!(bits.iter().fold(0, |a, b| a | b), perm::ALL);
        let mut sorted = bits.clone();
        sorted.sort_unstable();
        sorted.dedup();
        assert_eq!(sorted.len(), 26);
    }

    #[test]
    fn a_grid_change_moves_one_bit_between_allow_deny_and_inherit() {
        let b = perm::SEND_MESSAGES;
        let other = perm::SPEAK;
        // From nothing.
        assert_eq!(after_change(None, b, Tri::Allow), (b, 0));
        assert_eq!(after_change(None, b, Tri::Deny), (0, b));
        assert_eq!(after_change(None, b, Tri::Inherit), (0, 0));
        // Allow to deny moves the bit across and leaves the others alone.
        assert_eq!(after_change(Some((b | other, 0)), b, Tri::Deny), (other, b));
        // Deny to allow, and either to inherit.
        assert_eq!(
            after_change(Some((0, b | other)), b, Tri::Allow),
            (b, other)
        );
        assert_eq!(after_change(Some((b, other)), b, Tri::Inherit), (0, other));
        // A result never holds one bit on both sides.
        for now in [None, Some((b, 0)), Some((0, b)), Some((b, other))] {
            for v in Tri::ALL {
                let (a, d) = after_change(now, b, v);
                assert_eq!(a & d, 0);
            }
        }
        assert_eq!(tri_of(b, 0, b), Tri::Allow);
        assert_eq!(tri_of(0, b, b), Tri::Deny);
        assert_eq!(tri_of(other, other, b), Tri::Inherit);
    }

    #[test]
    fn an_overlapping_overwrite_is_refused_before_any_request() {
        assert!(overwrite_valid(0, perm::SEND_MESSAGES, perm::SPEAK));
        assert!(!overwrite_valid(
            0,
            perm::SEND_MESSAGES | perm::SPEAK,
            perm::SPEAK
        ));
        assert!(!overwrite_valid(2, 0, 0));
        assert!(!overwrite_valid(1, 1 << 26, 0));
    }

    fn role(n: u8, name: &str, position: u64, allow: u32) -> AdminRole {
        AdminRole {
            id: [n; 16],
            name: name.into(),
            position,
            allow,
            deny: 0,
        }
    }

    fn member(n: u8, display: &str, roles: &[u8]) -> crate::state::Member {
        crate::state::Member {
            user_id: UserId([n; 16]),
            username: format!("user{n}"),
            display: display.into(),
            kind: 0,
            role_ids: roles.iter().map(|r| [*r; 16]).collect(),
            status_msg: String::new(),
            bio: String::new(),
        }
    }

    #[test]
    fn entries_sort_everyone_first_then_roles_high_to_low_then_people_and_gate_by_rank() {
        let roles = [
            role(7, "@everyone", 0, 0),
            role(8, "mods", 3, 0),
            role(9, "admins", 5, 0),
        ];
        let members = [
            member(1, "Ada", &[]),
            member(2, "", &[8]),
            member(3, "Cy", &[9]),
        ];
        let ctx = PermContext {
            everyone: [7; 16],
            owner: false,
            top: 4,
            community_owner: UserId([1; 16]),
            me: UserId([2; 16]),
            roles: &roles,
            members: &members,
        };
        let rows = [
            Overwrite {
                channel: id(1),
                kind: 1,
                target: [3; 16],
                allow: 0,
                deny: 1,
            },
            Overwrite {
                channel: id(1),
                kind: 0,
                target: [9; 16],
                allow: 1,
                deny: 0,
            },
            Overwrite {
                channel: id(1),
                kind: 0,
                target: [8; 16],
                allow: 2,
                deny: 0,
            },
            Overwrite {
                channel: id(1),
                kind: 0,
                target: [7; 16],
                allow: 0,
                deny: 1,
            },
            Overwrite {
                channel: id(1),
                kind: 0,
                target: [0x42; 16],
                allow: 4,
                deny: 0,
            },
        ];
        let entries = perm_entries(&rows, &[(1, [2; 16])], &ctx);
        let order: Vec<(u8, u8)> = entries.iter().map(|e| (e.kind, e.target[0])).collect();
        // @everyone, admins(5), mods(3), the unknown role (rank 0), then people in list order, then the added one.
        assert_eq!(order, [(0, 7), (0, 9), (0, 8), (0, 0x42), (1, 3), (1, 2)]);
        // Rank gates editing: a role at or above `top` (4) is not editable, a lower one is; the server's owner never.
        let by = |kind: u8, t: u8| {
            entries
                .iter()
                .find(|e| e.kind == kind && e.target[0] == t)
                .unwrap()
        };
        assert!(!by(0, 9).editable);
        assert!(by(0, 8).editable);
        assert!(
            !by(1, 3).editable,
            "Cy holds a role above this account's rank"
        );
        // An unknown role is named by the first eight hex digits of its id.
        assert_eq!(by(0, 0x42).name, "42424242");
        assert!(by(0, 7).is_everyone && by(0, 7).name.is_empty());
        // A person is shown by display name, else the account name.
        assert_eq!(by(1, 2).name, "user2");
        // The owner edits everyone but the server's owner.
        let owner_ctx = PermContext {
            owner: true,
            me: UserId([1; 16]),
            ..ctx
        };
        let rows = perm_entries(&rows, &[(1, [1; 16])], &owner_ctx);
        assert!(
            rows.iter()
                .find(|e| e.kind == 0 && e.target[0] == 9)
                .unwrap()
                .editable
        );
        assert!(
            !rows
                .iter()
                .find(|e| e.kind == 1 && e.target[0] == 1)
                .unwrap()
                .editable
        );
    }

    #[test]
    fn the_add_list_offers_untaken_roles_and_people_this_account_may_change() {
        let roles = [
            role(7, "@everyone", 0, 0),
            role(8, "mods", 3, 0),
            role(9, "admins", 5, 0),
        ];
        let members = [
            member(1, "Ada", &[]),
            member(2, "Bo", &[8]),
            member(3, "Cy", &[9]),
            member(4, "Di", &[]),
        ];
        let ctx = PermContext {
            everyone: [7; 16],
            owner: false,
            top: 4,
            community_owner: UserId([1; 16]),
            me: UserId([2; 16]),
            roles: &roles,
            members: &members,
        };
        let taken = [Overwrite {
            channel: id(1),
            kind: 0,
            target: [7; 16],
            allow: 0,
            deny: 1,
        }];
        let entries = perm_entries(&taken, &[], &ctx);
        let options = add_options(&entries, &ctx);
        let picked: Vec<(u8, u8)> = options.iter().map(|o| (o.kind, o.target[0])).collect();
        // mods (below rank 4), not admins (above), not @everyone (taken); people: Di only (not the owner Ada, not me Bo, not Cy).
        assert_eq!(picked, [(0, 8), (1, 4)]);
        let owner_ctx = PermContext {
            owner: true,
            me: UserId([1; 16]),
            ..ctx
        };
        let options = add_options(&entries, &owner_ctx);
        let picked: Vec<(u8, u8)> = options.iter().map(|o| (o.kind, o.target[0])).collect();
        assert_eq!(picked, [(0, 8), (0, 9), (1, 2), (1, 3), (1, 4)]);
    }

    #[test]
    fn what_this_account_holds_in_a_channel_gates_its_grid() {
        let roles = [role(
            7,
            "@everyone",
            0,
            perm::VIEW_CHANNEL | perm::SEND_MESSAGES,
        )];
        let members = [member(1, "Ada", &[]), member(2, "Bo", &[])];
        let ctx = PermContext {
            everyone: [7; 16],
            owner: false,
            top: 0,
            community_owner: UserId([1; 16]),
            me: UserId([2; 16]),
            roles: &roles,
            members: &members,
        };
        let mine = my_channel_bits(&[], &ctx).unwrap();
        assert_eq!(mine, perm::VIEW_CHANNEL | perm::SEND_MESSAGES);
        // A channel overwrite that denies sending takes it from the grid.
        let deny = [Overwrite {
            channel: id(1),
            kind: 0,
            target: [7; 16],
            allow: 0,
            deny: perm::SEND_MESSAGES,
        }];
        assert_eq!(my_channel_bits(&deny, &ctx).unwrap(), perm::VIEW_CHANNEL);
        // The owner holds everything; an unknown account is unknown.
        let owner = PermContext {
            me: UserId([1; 16]),
            ..ctx
        };
        assert_eq!(my_channel_bits(&[], &owner), Some(perm::ALL));
        let stranger = PermContext {
            me: UserId([9; 16]),
            ..owner
        };
        assert_eq!(my_channel_bits(&[], &stranger), None);
        // Community-scoped bits are not on offer in a channel's grid.
        assert!(!channel_grid_bits(perm::KICK_MEMBERS));
        assert!(channel_grid_bits(perm::SEND_MESSAGES));
    }

    #[test]
    fn overwrites_are_decoded_and_bounded() {
        let row = |kind: u64, allow: u64| {
            Value::Array(vec![
                Value::Bytes(vec![1; 16]),
                Value::Uint(kind),
                Value::Bytes(vec![2; 16]),
                Value::Uint(allow),
                Value::Uint(0),
            ])
        };
        let ok = Value::Array(vec![row(0, u64::from(perm::VIEW_CHANNEL)), row(1, 0)]).encode();
        let rows = decode_overwrites(&ok).unwrap();
        assert_eq!(rows.len(), 2);
        assert_eq!(rows[0].allow, perm::VIEW_CHANNEL);
        assert!(decode_overwrites(&Value::Array(vec![row(2, 0)]).encode()).is_err());
        assert!(decode_overwrites(&Value::Array(vec![row(0, 1 << 30)]).encode()).is_err());
    }
}
