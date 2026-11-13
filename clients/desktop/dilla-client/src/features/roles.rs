//! Area **members** (Task 14): roles, the permission grid, assigning roles and self roles.
//!
//! Ports: packages/client-core/src/admin/{authority,perm,rank,roles}.ts, and the access read of
//! packages/client-core/src/admin/controller.ts (`loadAccess`).
//!
//! Every client gate here is advisory: the instance decides each request again, and a refusal reaches the caller with
//! its own code ([`ApiError`]). A 403 (the access the view held is stale) or a 409 (a role moved meanwhile) reads the
//! access again before the refusal is handed on, so the view never keeps offering what the instance just refused.

use std::collections::{BTreeMap, HashMap};
use std::sync::Arc;

use crate::client::{Client, Inner};
use crate::gateway::frames::Value;
use crate::http::routes as r;
use crate::seams::Route;
use crate::state::roles::{CommunityAccess, OverwriteSummary, RoleId, RoleSummary, SelfRole};
use crate::state::{ChannelId, CommunityId, UserId, hex};
use crate::sync::api::ApiError;
use crate::sync::value::{DecodeError, Rows, at_least, bin16, text, u53, uint};

/// The permission bits and the resolver of internal/api/perm.go, ported line for line from perm.ts. Checked against
/// `protocol/vectors/permissions.json`.
pub mod perm {
    use super::RoleId;

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

    /// routes.ts `PERM_ALL`: the 26 bits.
    pub const PERM_ALL: u32 = 0x3ff_ffff;

    /// The bits a channel overwrite can neither grant nor remove (perm.go communityBits).
    pub const COMMUNITY_BITS: u32 = KICK_MEMBERS
        | BAN_MEMBERS
        | MANAGE_COMMUNITY
        | MANAGE_ROLES
        | VIEW_AUDIT_LOG
        | ADMINISTRATOR
        | MANAGE_NICKNAMES
        | MODERATE_MEMBERS;

    /// perm.ts `PermKey`: the copy key of each bit (`perm.<key>` in the strings).
    #[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
    pub enum PermKey {
        View,
        Send,
        ManageMessages,
        Pin,
        Attach,
        React,
        History,
        Connect,
        Speak,
        Video,
        Screen,
        Invite,
        Kick,
        Ban,
        ManageChannels,
        ManageRoles,
        ManageServer,
        AuditLog,
        Everyone,
        Slowmode,
        Admin,
        Mute,
        Move,
        Nicknames,
        Moderate,
        Music,
    }

    impl PermKey {
        /// Every key, in bit order.
        pub const ALL: [PermKey; 26] = [
            Self::View,
            Self::Send,
            Self::ManageMessages,
            Self::Pin,
            Self::Attach,
            Self::React,
            Self::History,
            Self::Connect,
            Self::Speak,
            Self::Video,
            Self::Screen,
            Self::Invite,
            Self::Kick,
            Self::Ban,
            Self::ManageChannels,
            Self::ManageRoles,
            Self::ManageServer,
            Self::AuditLog,
            Self::Everyone,
            Self::Slowmode,
            Self::Admin,
            Self::Mute,
            Self::Move,
            Self::Nicknames,
            Self::Moderate,
            Self::Music,
        ];

        /// perm.ts `PERM_BY_KEY`.
        pub fn bit(self) -> u32 {
            match self {
                Self::View => VIEW_CHANNEL,
                Self::Send => SEND_MESSAGES,
                Self::ManageMessages => MANAGE_MESSAGES,
                Self::Pin => PIN_MESSAGES,
                Self::Attach => ATTACH_FILES,
                Self::React => ADD_REACTIONS,
                Self::History => READ_HISTORY,
                Self::Connect => CONNECT,
                Self::Speak => SPEAK,
                Self::Video => VIDEO,
                Self::Screen => SCREEN_SHARE,
                Self::Invite => CREATE_INVITE,
                Self::Kick => KICK_MEMBERS,
                Self::Ban => BAN_MEMBERS,
                Self::ManageChannels => MANAGE_CHANNELS,
                Self::ManageRoles => MANAGE_ROLES,
                Self::ManageServer => MANAGE_COMMUNITY,
                Self::AuditLog => VIEW_AUDIT_LOG,
                Self::Everyone => MENTION_EVERYONE,
                Self::Slowmode => BYPASS_SLOWMODE,
                Self::Admin => ADMINISTRATOR,
                Self::Mute => MUTE_MEMBERS,
                Self::Move => MOVE_MEMBERS,
                Self::Nicknames => MANAGE_NICKNAMES,
                Self::Moderate => MODERATE_MEMBERS,
                Self::Music => CONTROL_MUSIC,
            }
        }

        /// The key's name as the strings spell it (`perm.<key>`).
        pub fn key(self) -> &'static str {
            match self {
                Self::View => "view",
                Self::Send => "send",
                Self::ManageMessages => "manageMessages",
                Self::Pin => "pin",
                Self::Attach => "attach",
                Self::React => "react",
                Self::History => "history",
                Self::Connect => "connect",
                Self::Speak => "speak",
                Self::Video => "video",
                Self::Screen => "screen",
                Self::Invite => "invite",
                Self::Kick => "kick",
                Self::Ban => "ban",
                Self::ManageChannels => "manageChannels",
                Self::ManageRoles => "manageRoles",
                Self::ManageServer => "manageServer",
                Self::AuditLog => "auditLog",
                Self::Everyone => "everyone",
                Self::Slowmode => "slowmode",
                Self::Admin => "admin",
                Self::Mute => "mute",
                Self::Move => "move",
                Self::Nicknames => "nicknames",
                Self::Moderate => "moderate",
                Self::Music => "music",
            }
        }
    }

    use PermKey as K;

    /// perm.ts `PERM_GROUPS` (channel permissions).
    pub const PERM_GROUPS: [(&str, &[PermKey]); 5] = [
        (
            "general",
            &[
                K::View,
                K::ManageChannels,
                K::ManageRoles,
                K::ManageServer,
                K::Invite,
                K::AuditLog,
                K::Admin,
            ],
        ),
        ("members", &[K::Kick, K::Ban, K::Nicknames, K::Moderate]),
        (
            "text",
            &[
                K::Send,
                K::Attach,
                K::React,
                K::History,
                K::Pin,
                K::Everyone,
                K::ManageMessages,
                K::Slowmode,
            ],
        ),
        (
            "voice",
            &[K::Connect, K::Speak, K::Video, K::Screen, K::Mute, K::Move],
        ),
        ("music", &[K::Music]),
    ];

    /// perm.ts `ROLE_PERM_GROUPS`: the role editor's General, Moderation, Voice and Music.
    pub const ROLE_PERM_GROUPS: [(&str, &[PermKey]); 4] = [
        (
            "general",
            &[
                K::View,
                K::Send,
                K::Pin,
                K::Everyone,
                K::Attach,
                K::React,
                K::History,
                K::Invite,
            ],
        ),
        (
            "moderation",
            &[
                K::ManageChannels,
                K::Kick,
                K::Ban,
                K::ManageMessages,
                K::Moderate,
                K::Nicknames,
                K::Slowmode,
                K::ManageRoles,
                K::ManageServer,
                K::AuditLog,
                K::Admin,
            ],
        ),
        (
            "voice",
            &[K::Connect, K::Speak, K::Screen, K::Video, K::Mute, K::Move],
        ),
        ("music", &[K::Music]),
    ];

    /// perm.ts `PermRole`.
    #[derive(Clone, Debug, PartialEq, Eq)]
    pub struct PermRole {
        pub id: RoleId,
        pub position: u32,
        pub allow: u32,
        pub deny: u32,
    }

    /// perm.ts `PermOverwrite`: kind 0 a role, 1 a member.
    #[derive(Clone, Debug, PartialEq, Eq)]
    pub struct PermOverwrite {
        pub kind: u8,
        pub target_id: [u8; 16],
        pub allow: u32,
        pub deny: u32,
    }

    /// perm.ts `PermInput`.
    #[derive(Clone, Debug)]
    pub struct PermInput<'a> {
        pub owner: [u8; 16],
        pub everyone_id: RoleId,
        pub roles: &'a [PermRole],
        pub held: &'a [RoleId],
    }

    /// Deny first, then allow: an allow on the same bit wins.
    fn apply(bits: u32, allow: u32, deny: u32) -> u32 {
        ((bits & !(deny & PERM_ALL)) | (allow & PERM_ALL)) & PERM_ALL
    }

    fn finish(bits: u32) -> u32 {
        if bits & ADMINISTRATOR != 0 {
            return PERM_ALL;
        }
        if bits & VIEW_CHANNEL == 0 {
            return 0;
        }
        bits
    }

    /// The held roles and @everyone, by position then id (byte order is the hex order).
    fn held_roles<'a>(input: &PermInput<'a>) -> Vec<&'a PermRole> {
        let mut held: Vec<&PermRole> = input
            .roles
            .iter()
            .filter(|r| r.id == input.everyone_id || input.held.contains(&r.id))
            .collect();
        held.sort_by(|a, b| a.position.cmp(&b.position).then(a.id.cmp(&b.id)));
        held
    }

    /// perm.go Resolve: the owner short-circuit, @everyone, then each held role in ascending position.
    pub fn resolve_community(input: &PermInput<'_>, user: [u8; 16]) -> u32 {
        if user == input.owner {
            return PERM_ALL;
        }
        let bits = held_roles(input)
            .iter()
            .fold(0, |bits, r| apply(bits, r.allow, r.deny));
        finish(bits)
    }

    /// perm.ts `ResolvedUser`: one user's community bits and the held role ids in ascending position.
    #[derive(Clone, Debug, PartialEq, Eq)]
    pub struct ResolvedUser {
        pub user: [u8; 16],
        pub base: u32,
        pub held: Vec<RoleId>,
    }

    pub fn resolve_user(input: &PermInput<'_>, user: [u8; 16]) -> ResolvedUser {
        let held = held_roles(input);
        let ids = held.iter().map(|r| r.id).collect();
        if user == input.owner {
            return ResolvedUser {
                user,
                base: PERM_ALL,
                held: ids,
            };
        }
        let bits = held.iter().fold(0, |bits, r| apply(bits, r.allow, r.deny));
        ResolvedUser {
            user,
            base: finish(bits),
            held: ids,
        }
    }

    /// perm.go ResolveChannel from a resolved user: the overwrites of the held roles in ascending role position, then the
    /// user's own, then the community-scoped bits restored from the base. An overwrite counts only for its own kind and
    /// target.
    pub fn channel_bits_from(u: &ResolvedUser, overwrites: &[PermOverwrite]) -> u32 {
        let base = u.base;
        if base == PERM_ALL || base == 0 {
            return base;
        }
        let mut bits = base;
        for role in &u.held {
            for o in overwrites
                .iter()
                .filter(|o| o.kind == 0 && o.target_id == *role)
            {
                bits = apply(bits, o.allow, o.deny);
            }
        }
        for o in overwrites
            .iter()
            .filter(|o| o.kind == 1 && o.target_id == u.user)
        {
            bits = apply(bits, o.allow, o.deny);
        }
        bits = (bits & !COMMUNITY_BITS) | (base & COMMUNITY_BITS);
        finish(bits)
    }

    /// perm.ts `resolveChannel`. `afk`: every member may view and connect to the built-in AFK channel (R35); the owner
    /// and administrators keep `PERM_ALL`.
    pub fn resolve_channel(
        input: &PermInput<'_>,
        overwrites: &[PermOverwrite],
        user: [u8; 16],
        afk: bool,
    ) -> u32 {
        let bits = channel_bits_from(&resolve_user(input, user), overwrites);
        if afk && bits != PERM_ALL {
            bits | VIEW_CHANNEL | CONNECT
        } else {
            bits
        }
    }

    /// A channel is private when @everyone's overwrite denies viewing it.
    pub fn is_private(overwrites: &[PermOverwrite], everyone_id: RoleId) -> bool {
        overwrites
            .iter()
            .any(|o| o.kind == 0 && o.target_id == everyone_id && o.deny & VIEW_CHANNEL != 0)
    }
}

/// Who may do what to whom in a server (rank.ts).
pub mod rank {
    use super::perm;
    use crate::state::roles::{CommunityAccess, RoleId, RoleSummary};

    /// routes.ts `OWNER_TOP`: the owner's rank.
    pub const OWNER_TOP: u32 = 1 << 31;

    /// rank.ts `accessOf`: what a member may do community-wide, from the resolved bits and rank. The owner and an
    /// administrator hold every permission.
    pub fn access_of(bits: u64, top: u32, owner: bool) -> CommunityAccess {
        let all = owner || bits & u64::from(perm::ADMINISTRATOR) != 0;
        let has = |bit: u32| all || bits & u64::from(bit) != 0;
        CommunityAccess {
            top,
            owner,
            manage_community: has(perm::MANAGE_COMMUNITY),
            view_audit_log: has(perm::VIEW_AUDIT_LOG),
            manage_channels: has(perm::MANAGE_CHANNELS),
            kick_members: has(perm::KICK_MEMBERS),
            ban_members: has(perm::BAN_MEMBERS),
            moderate_members: has(perm::MODERATE_MEMBERS),
            manage_messages: has(perm::MANAGE_MESSAGES),
            create_invite: has(perm::CREATE_INVITE),
            manage_roles: has(perm::MANAGE_ROLES),
            loaded: true,
            everyone_id: None,
            overwrites_loaded: false,
        }
    }

    /// rank.ts `topOf`: the highest role position among `role_ids`; 0 with none. A role the client has no row for counts
    /// as just under the owner, so a stale role list never lets anyone act on a member the client cannot rank.
    pub fn top_of(role_ids: &[RoleId], roles: &[RoleSummary]) -> u32 {
        role_ids
            .iter()
            .map(|id| {
                roles
                    .iter()
                    .find(|r| r.id == *id)
                    .map_or(OWNER_TOP - 1, |r| r.position)
            })
            .max()
            .unwrap_or(0)
    }

    /// rank.ts `canModerate`: never the owner or oneself; always for the owner; otherwise only from a strictly higher
    /// rank than the target's, and never with no rank at all.
    pub fn can_moderate(
        me: [u8; 16],
        target: [u8; 16],
        owner: [u8; 16],
        my_top: u32,
        target_top: u32,
    ) -> bool {
        if target == owner || target == me {
            return false;
        }
        if me == owner {
            return true;
        }
        if my_top == 0 {
            return false;
        }
        target_top < my_top
    }
}

/// authority.ts: who may pin and who may mention everyone in each text channel's group.
pub mod authority {
    use std::collections::{BTreeMap, HashMap};

    use super::RoleId;
    use super::perm::{self, PermInput, PermOverwrite, PermRole, ResolvedUser};

    /// The sorted user ids allowed `pin_messages` and `mention_everyone` in one group's channel.
    #[derive(Clone, Debug, Default, PartialEq, Eq)]
    pub struct AuthorityView {
        pub pinners: Vec<[u8; 16]>,
        pub broadcasters: Vec<[u8; 16]>,
    }

    /// One channel as authority.ts reads it.
    #[derive(Clone, Debug)]
    pub struct AuthorityChannel {
        pub id: [u8; 16],
        pub kind: u8,
        pub mode: u8,
        pub text_group: Option<[u8; 16]>,
    }

    /// authority.ts `authorityFor`: group → its view, for every text channel (kind 0, mode 0) with a group.
    pub fn authority_for(
        owner: [u8; 16],
        everyone_id: RoleId,
        roles: &[PermRole],
        members: &[([u8; 16], Vec<RoleId>)],
        overwrites: &HashMap<[u8; 16], Vec<PermOverwrite>>,
        channels: &[AuthorityChannel],
    ) -> BTreeMap<[u8; 16], AuthorityView> {
        // The first row counts, as channelBitsOf's find.
        let mut held: BTreeMap<[u8; 16], &[RoleId]> = BTreeMap::new();
        for (user, ids) in members {
            held.entry(*user).or_insert(ids.as_slice());
        }
        held.entry(owner).or_insert(&[]);
        let users: Vec<ResolvedUser> = held
            .iter()
            .map(|(user, ids)| {
                perm::resolve_user(
                    &PermInput {
                        owner,
                        everyone_id,
                        roles,
                        held: ids,
                    },
                    *user,
                )
            })
            .collect();
        let view_of = |bits_of: &dyn Fn(&ResolvedUser) -> u32| {
            let mut view = AuthorityView::default();
            for u in &users {
                let bits = bits_of(u);
                if bits & perm::PIN_MESSAGES != 0 {
                    view.pinners.push(u.user);
                }
                if bits & perm::MENTION_EVERYONE != 0 {
                    view.broadcasters.push(u.user);
                }
            }
            view
        };
        let mut base: Option<AuthorityView> = None;
        let mut out = BTreeMap::new();
        for channel in channels {
            let Some(group) = channel.text_group else {
                continue;
            };
            if channel.kind != 0 || channel.mode != 0 {
                continue;
            }
            let own = overwrites.get(&channel.id).map_or(&[][..], Vec::as_slice);
            if own.is_empty() {
                let view = base.get_or_insert_with(|| view_of(&|u| u.base)).clone();
                out.insert(group, view);
                continue;
            }
            out.insert(group, view_of(&|u| perm::channel_bits_from(u, own)));
        }
        out
    }
}

// ---- input checks (roles.ts ROLES_VALIDATORS) ----

/// routes.ts `ROLE_NAME_MAX_BYTES`.
pub const ROLE_NAME_MAX_BYTES: usize = 100;
/// routes.ts `ROLE_COLOR_MAX`.
pub const ROLE_COLOR_MAX: u32 = 0xff_ffff;
/// The highest position the instance stores for a role.
pub const ROLE_POSITION_MAX: u32 = (1 << 31) - 1;
/// routes.ts `ROLES_MAX`: roles per community, and self roles in one policy.
pub const ROLES_MAX: usize = 250;

/// roles.ts `roleNameProblem`.
pub fn role_name_problem(name: &str) -> Option<String> {
    if name.trim().is_empty() {
        return Some("role name is blank".into());
    }
    (name.len() > ROLE_NAME_MAX_BYTES)
        .then(|| format!("role name is longer than {ROLE_NAME_MAX_BYTES} bytes"))
}

/// roles.ts `colorProblem`.
pub fn color_problem(color: u32) -> Option<String> {
    (color > ROLE_COLOR_MAX).then(|| format!("color must be an integer 0..{ROLE_COLOR_MAX}"))
}

/// roles.ts `bitsProblem`.
pub fn bits_problem(field: &str, bits: u32) -> Option<String> {
    (bits > perm::PERM_ALL).then(|| format!("{field} must be an integer 0..{}", perm::PERM_ALL))
}

/// What a role update changes; `None` leaves a field alone.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct RolePatch {
    pub name: Option<String>,
    pub color: Option<u32>,
    pub hoist: Option<bool>,
    pub mentionable: Option<bool>,
    pub allow: Option<u32>,
}

/// roles.ts `ROLES_VALIDATORS.createRole`.
pub fn create_problem(name: &str, color: u32, allow: u32) -> Option<String> {
    role_name_problem(name)
        .or_else(|| color_problem(color))
        .or_else(|| bits_problem("allow", allow))
}

/// roles.ts `ROLES_VALIDATORS.updateRole`.
pub fn update_problem(patch: &RolePatch) -> Option<String> {
    if let Some(p) = patch.name.as_deref().and_then(role_name_problem) {
        return Some(p);
    }
    if let Some(p) = patch.color.and_then(color_problem) {
        return Some(p);
    }
    if let Some(p) = patch.allow.and_then(|a| bits_problem("allow", a)) {
        return Some(p);
    }
    (*patch == RolePatch::default()).then(|| "nothing to change".into())
}

// ---- the editors' gates (RolesSection.tsx, SelfRolesSection.tsx, MemberRolesFlow.tsx) ----

/// RolesSection.tsx `heldBits`: the bits `me` holds community-wide; everything for the owner and administrators, else
/// the resolver's answer.
pub fn held_bits(
    owner: bool,
    owner_id: [u8; 16],
    everyone: RoleId,
    roles: &[RoleSummary],
    held: &[RoleId],
    me: [u8; 16],
) -> u32 {
    if owner {
        return perm::PERM_ALL;
    }
    let perm_roles: Vec<perm::PermRole> = roles
        .iter()
        .map(|r| perm::PermRole {
            id: r.id,
            position: r.position,
            allow: r.allow,
            deny: r.deny,
        })
        .collect();
    let bits = perm::resolve_community(
        &perm::PermInput {
            owner: owner_id,
            everyone_id: everyone,
            roles: &perm_roles,
            held,
        },
        me,
    );
    if bits & perm::ADMINISTRATOR != 0 {
        perm::PERM_ALL
    } else {
        bits
    }
}

/// Why a permission toggle of the role editor is disabled.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PermLock {
    /// The role is at or above my rank, or I may not manage roles (`serverSettings.roles.locked`).
    Locked,
    /// I do not hold the bit myself (`roles.editor.notHeld`).
    NotHeld,
}

/// RolesSection.tsx: a permission row's lock, `None` when it may be toggled.
pub fn permission_lock(role_editable: bool, mine: u32, bit: u32) -> Option<PermLock> {
    if !role_editable {
        Some(PermLock::Locked)
    } else if mine & bit == 0 {
        Some(PermLock::NotHeld)
    } else {
        None
    }
}

/// SelfRolesSection.tsx `SELF_ROLE_BITS`: what a self-assignable role may carry.
pub const SELF_ROLE_BITS: u32 = perm::VIEW_CHANNEL
    | perm::SEND_MESSAGES
    | perm::ATTACH_FILES
    | perm::ADD_REACTIONS
    | perm::READ_HISTORY
    | perm::CONNECT
    | perm::SPEAK
    | perm::VIDEO
    | perm::SCREEN_SHARE;

/// SelfRolesSection.tsx `candidates`: never @everyone, nothing beyond [`SELF_ROLE_BITS`] allowed or denied, and below
/// my rank unless I own the server; in the given order.
pub fn self_role_candidates<'a>(
    roles: &'a [RoleSummary],
    access: &CommunityAccess,
) -> Vec<&'a RoleSummary> {
    roles
        .iter()
        .filter(|r| {
            r.position > 0
                && r.allow & !SELF_ROLE_BITS == 0
                && r.deny & !SELF_ROLE_BITS == 0
                && (access.owner || r.position < access.top)
        })
        .collect()
}

/// MemberRolesFlow.tsx `assignable`: the roles highest first (then by id), without @everyone.
pub fn assignable(roles: &[RoleSummary], everyone: Option<RoleId>) -> Vec<RoleSummary> {
    let mut list: Vec<RoleSummary> = roles
        .iter()
        .filter(|r| Some(r.id) != everyone && r.position != 0)
        .cloned()
        .collect();
    list.sort_by(|a, b| b.position.cmp(&a.position).then(a.id.cmp(&b.id)));
    list
}

/// Why a role of the assign dialog cannot be ticked.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum AssignLock {
    /// At or above my highest role (`roles.assign.locked`).
    Rank,
    /// It carries a bit I do not hold (`roles.editor.notHeld`).
    NotHeld,
}

/// MemberRolesFlow.tsx `rankLocked` then `lacking`.
pub fn assign_lock(role: &RoleSummary, access: &CommunityAccess, mine: u32) -> Option<AssignLock> {
    if !access.owner && role.position >= access.top {
        Some(AssignLock::Rank)
    } else if role.allow & !mine & perm::PERM_ALL != 0 {
        Some(AssignLock::NotHeld)
    } else {
        None
    }
}

/// How the assign dialog says a refused grant or revoke.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum AssignRefusal {
    /// `roles.assign.error.twoFactor`.
    TwoFactor,
    /// `roles.assign.error.forbidden`.
    Forbidden,
    /// `roles.error.gone`.
    RoleGone,
    /// `roles.assign.error.gone`.
    MemberGone,
    /// `roles.assign.error.other {code}`.
    Other(String),
}

/// MemberRolesFlow.tsx: the target's missing factor by its own code (before the generic 403), then a 403, then a 404
/// (the role gone when the list no longer has it, else the member), else the code.
pub fn assign_refusal(e: &ApiError, role_still_listed: bool) -> AssignRefusal {
    if e.code == "E_TARGET_NO_FACTOR" {
        AssignRefusal::TwoFactor
    } else if e.code == "E_FORBIDDEN" || e.status == 403 {
        AssignRefusal::Forbidden
    } else if e.status == 404 {
        if role_still_listed {
            AssignRefusal::MemberGone
        } else {
            AssignRefusal::RoleGone
        }
    } else {
        AssignRefusal::Other(e.code.clone())
    }
}

/// One step of a role up or down the list.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Direction {
    Up,
    Down,
}

fn by_position_then_id(a: &RoleSummary, b: &RoleSummary) -> std::cmp::Ordering {
    a.position.cmp(&b.position).then(a.id.cmp(&b.id))
}

/// roles.ts `createRole`: the lowest free position, which must be below my top unless I own the server; `None` is "no
/// room below your highest role".
pub fn create_position(rows: &[RoleSummary], owner: bool, top: u32) -> Option<u32> {
    let mut position = 1;
    while rows.iter().any(|r| r.position == position) {
        position += 1;
    }
    let bound = if owner {
        ROLE_POSITION_MAX
    } else {
        top.saturating_sub(1)
    };
    (position <= bound).then_some(position)
}

/// roles.ts `moveRole`: the patches that move `role` one step, in order; empty when nothing moves. The neighbour is the
/// next role up or down (never @everyone) and must be below my top unless I own the server. With different positions
/// the two trade; with equal positions only the mover steps past the neighbour. `None` is an unknown role.
pub fn move_plan(
    rows: &[RoleSummary],
    role: RoleId,
    direction: Direction,
    owner: bool,
    top: u32,
) -> Option<Vec<(RoleId, u32)>> {
    let mut sorted: Vec<&RoleSummary> = rows.iter().collect();
    sorted.sort_by(|a, b| by_position_then_id(a, b));
    let at = sorted.iter().position(|r| r.id == role)?;
    let mover = sorted[at];
    let other = match direction {
        Direction::Up => sorted.get(at + 1),
        Direction::Down => at.checked_sub(1).and_then(|i| sorted.get(i)),
    };
    let Some(other) = other else {
        return Some(Vec::new());
    };
    if mover.position == 0 || other.position == 0 || (!owner && other.position >= top) {
        return Some(Vec::new());
    }
    if mover.position != other.position {
        return Some(vec![(mover.id, other.position), (other.id, mover.position)]);
    }
    let to = match direction {
        Direction::Up => u64::from(other.position) + 1,
        Direction::Down => u64::from(other.position.saturating_sub(1).max(1)),
    };
    if to > u64::from(ROLE_POSITION_MAX) || (!owner && to >= u64::from(top)) {
        return Some(Vec::new());
    }
    Some(vec![(mover.id, to as u32)])
}

// ---- the wire ----

/// Runs `job` on the client's own runtime (the members commands need its tokio reactor), so a view can await the answer
/// on any executor. A client that stopped meanwhile answers E_STOPPED.
pub(crate) fn on_runtime<T: Send + 'static, J>(
    inner: &Inner,
    job: J,
) -> impl std::future::Future<Output = Result<T, ApiError>> + use<T, J>
where
    J: std::future::Future<Output = Result<T, ApiError>> + Send + 'static,
{
    let rx = inner.spawn(async move { Ok(job.await) });
    async move {
        match rx.await {
            Ok(Ok(answer)) => answer,
            _ => Err(refusal("E_STOPPED", "the client stopped")),
        }
    }
}

/// A local refusal (the worker's own `Refusal`): reaches the view as its code, like a server's.
pub(crate) fn refusal(code: &str, detail: &str) -> ApiError {
    ApiError::local(0, code, detail)
}

fn bad(e: DecodeError) -> ApiError {
    ApiError::local(0, "E_DECODE", e.0)
}

fn random16() -> [u8; 16] {
    let mut b = [0u8; 16];
    let _ = getrandom::fill(&mut b);
    b
}

/// A fresh request id (routes.ts `requestId`): random, stable across the retries of one request.
pub fn request_id() -> [u8; 16] {
    random16()
}

/// One socket route through the client's own [`crate::sync::api::Api::socket_call`], so its refusal hook sees every
/// refusal (E_FACTOR_REQUIRED sends the account to factor setup) and a 429 is waited out. The answer's body.
pub(crate) async fn call<R: Route>(
    inner: &Inner,
    args: &[&str],
    body: Option<Vec<u8>>,
    idem: Option<Vec<u8>>,
) -> Result<Vec<u8>, ApiError> {
    inner
        .api()
        .socket_call::<R>(args, body, idem, true)
        .await
        .map(|a| a.body)
}

/// [`call`] without waiting out a 429 (routes.ts `retry429: false`: the self-role writes).
pub(crate) async fn call_once<R: Route>(
    inner: &Inner,
    args: &[&str],
    body: Option<Vec<u8>>,
    idem: Option<Vec<u8>>,
) -> Result<Vec<u8>, ApiError> {
    inner
        .api()
        .socket_call::<R>(args, body, idem, false)
        .await
        .map(|a| a.body)
}

/// routes.ts `roleRow`: `[id, name, color, position, allow, deny, hoist, mentionable]`.
fn decode_roles(raw: &[u8]) -> Result<Vec<RoleSummary>, DecodeError> {
    let rows = Rows::of(raw)?;
    if rows.iter().len() > ROLES_MAX {
        return Err(DecodeError("too many roles"));
    }
    rows.iter()
        .map(|row| {
            let a = at_least(row, 8)?;
            let flag = |v: &Value| match u53(v)? {
                0 => Ok(false),
                1 => Ok(true),
                _ => Err(DecodeError("expected 0 or 1")),
            };
            let position = u53(&a[3])?;
            if position > u64::from(rank::OWNER_TOP) {
                return Err(DecodeError("role position out of range"));
            }
            Ok(RoleSummary {
                id: bin16(&a[0])?,
                name: text(&a[1])?,
                color: u32::try_from(u53(&a[2])?).map_err(|_| DecodeError("role colour"))?
                    & ROLE_COLOR_MAX,
                position: position as u32,
                allow: (uint(&a[4])? & u64::from(perm::PERM_ALL)) as u32,
                deny: (uint(&a[5])? & u64::from(perm::PERM_ALL)) as u32,
                hoist: flag(&a[6])?,
                mentionable: flag(&a[7])?,
                members: 0,
            })
        })
        .collect()
}

/// routes.ts `overwriteRow`: `[channel_id, kind, target_id, allow, deny]`.
fn decode_overwrites(raw: &[u8]) -> Result<Vec<(ChannelId, OverwriteSummary)>, DecodeError> {
    Rows::of(raw)?
        .iter()
        .map(|row| {
            let a = at_least(row, 5)?;
            let kind = match u53(&a[1])? {
                k @ (0 | 1) => k as u8,
                _ => return Err(DecodeError("overwrite kind")),
            };
            Ok((
                ChannelId(bin16(&a[0])?),
                OverwriteSummary {
                    kind,
                    target_id: bin16(&a[2])?,
                    allow: (uint(&a[3])? & u64::from(perm::PERM_ALL)) as u32,
                    deny: (uint(&a[4])? & u64::from(perm::PERM_ALL)) as u32,
                },
            ))
        })
        .collect()
}

/// routes.ts `listSelfRoles`: `[[id, name, position, self_selected]]`.
fn decode_self_roles(raw: &[u8]) -> Result<Vec<SelfRole>, DecodeError> {
    let rows = Rows::of(raw)?;
    if rows.iter().len() > ROLES_MAX {
        return Err(DecodeError("too many self roles"));
    }
    rows.iter()
        .map(|row| {
            let a = at_least(row, 4)?;
            if a.len() != 4 {
                return Err(DecodeError("expected four self-role fields"));
            }
            let position = u53(&a[2])?;
            if position > u64::from(rank::OWNER_TOP) {
                return Err(DecodeError("self-role position out of range"));
            }
            Ok(SelfRole {
                id: bin16(&a[0])?,
                name: text(&a[1])?,
                position: position as u32,
                self_selected: match u53(&a[3])? {
                    0 => false,
                    1 => true,
                    _ => return Err(DecodeError("self-role flag")),
                },
            })
        })
        .collect()
}

/// The role counts (roles.ts `publishRoles`): @everyone counts every member.
pub(crate) fn with_counts(
    mut rows: Vec<RoleSummary>,
    members: &[crate::state::Member],
) -> Vec<RoleSummary> {
    for r in &mut rows {
        r.members = if r.position == 0 {
            members.len() as u32
        } else {
            members
                .iter()
                .filter(|m| m.role_ids.contains(&r.id))
                .count() as u32
        };
    }
    rows
}

/// The members area's handle for roles: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Roles {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Roles`] handle.
    pub fn roles(&self) -> Roles {
        Roles {
            inner: self.inner().clone(),
        }
    }
}

impl Roles {
    /// Runs `job` (built from this handle) on the client's runtime; see [`Moderation::run`](super::moderation::Moderation::run).
    pub fn run<T: Send + 'static, F, J: FnOnce(Roles) -> F>(
        &self,
        job: J,
    ) -> impl std::future::Future<Output = Result<T, ApiError>> + use<T, F, J>
    where
        F: std::future::Future<Output = Result<T, ApiError>> + Send + 'static,
    {
        on_runtime(&self.inner, job(self.clone()))
    }

    fn listed(&self, community: CommunityId) -> Result<(), ApiError> {
        if self
            .inner
            .snapshot()
            .communities
            .iter()
            .any(|c| c.id == community)
        {
            Ok(())
        } else {
            Err(refusal("E_BAD_INPUT", "unknown community"))
        }
    }

    /// controller.ts `loadAccess`: reads the caller's permissions, the roles and the overwrites of `community`, and
    /// publishes them. A failed read publishes nothing and leaves what the view holds, except that a failed overwrites
    /// read alone clears `overwrites_loaded`.
    pub async fn load_access(&self, community: CommunityId) -> Result<(), ApiError> {
        load_access(&self.inner, community).await.map(|_| ())
    }

    /// The rows, rank and @everyone of a server; read first when the view has none yet.
    async fn access(
        &self,
        community: CommunityId,
    ) -> Result<(Vec<RoleSummary>, CommunityAccess, RoleId), ApiError> {
        let read = |inner: &Inner| {
            let s = inner.snapshot();
            let access = s.roles.access.get(&community)?.clone();
            let rows = s.roles.roles.get(&community)?.clone();
            let everyone = access.everyone_id?;
            access.loaded.then_some((rows, access, everyone))
        };
        if let Some(held) = read(&self.inner) {
            return Ok(held);
        }
        // The read's own answer: what it publishes lands in the snapshot a moment later.
        match load_access(&self.inner, community).await {
            Ok(Some((rows, access))) if access.everyone_id.is_some() => {
                let everyone = access.everyone_id.unwrap_or_default();
                Ok((rows, access, everyone))
            }
            _ => Err(refusal(
                "E_BAD_INPUT",
                "the roles of this server are not loaded",
            )),
        }
    }

    /// Runs a request; a 403 or a 409 reads the access again, and the refusal still reaches the caller.
    async fn guarded<T>(
        &self,
        community: CommunityId,
        run: impl std::future::Future<Output = Result<T, ApiError>>,
    ) -> Result<T, ApiError> {
        match run.await {
            Ok(v) => Ok(v),
            Err(e) => {
                if e.status == 403 || e.status == 409 {
                    let _ = load_access(&self.inner, community).await;
                }
                Err(e)
            }
        }
    }

    async fn after_members(&self, community: CommunityId) {
        let _ = crate::features::members::reload_members(&self.inner, community).await;
        let _ = load_access(&self.inner, community).await;
    }

    /// roles.ts `createRole`: at the lowest free position below my top. Answers the new role's id.
    pub async fn create_role(
        &self,
        community: CommunityId,
        name: &str,
        color: u32,
        hoist: bool,
        mentionable: bool,
        allow: u32,
    ) -> Result<RoleId, ApiError> {
        if let Some(p) = create_problem(name, color, allow) {
            return Err(refusal("E_BAD_INPUT", &p));
        }
        self.listed(community)?;
        let (rows, access, _) = self.access(community).await?;
        let position = create_position(&rows, access.owner, access.top)
            .ok_or_else(|| refusal("E_BAD_INPUT", "no room below your highest role"))?;
        let body = Value::Array(vec![
            Value::Text(name.to_owned()),
            Value::Uint(color.into()),
            Value::Uint(position.into()),
            Value::Uint(allow.into()),
            Value::Uint(0),
            Value::Uint(hoist.into()),
            Value::Uint(mentionable.into()),
        ])
        .encode();
        let c = community.hex();
        let raw = self
            .guarded(
                community,
                call::<r::CreateRole>(&self.inner, &[&c], Some(body), None),
            )
            .await?;
        let v = Value::decode(&raw).map_err(|_| refusal("E_DECODE", "not CBOR"))?;
        let id = at_least(&v, 1).and_then(|a| bin16(&a[0])).map_err(bad)?;
        let _ = load_access(&self.inner, community).await;
        Ok(id)
    }

    /// roles.ts `updateRole`: only the changed fields are sent.
    pub async fn update_role(
        &self,
        community: CommunityId,
        role: RoleId,
        patch: RolePatch,
    ) -> Result<(), ApiError> {
        if let Some(p) = update_problem(&patch) {
            return Err(refusal("E_BAD_INPUT", &p));
        }
        self.listed(community)?;
        let flag = |v: Option<bool>| v.map_or(Value::Null, |b| Value::Uint(b.into()));
        let body = Value::Array(vec![
            patch.name.map_or(Value::Null, Value::Text),
            patch.color.map_or(Value::Null, |c| Value::Uint(c.into())),
            Value::Null,
            patch.allow.map_or(Value::Null, |a| Value::Uint(a.into())),
            Value::Null,
            flag(patch.hoist),
            flag(patch.mentionable),
        ])
        .encode();
        let id = hex(&role);
        self.guarded(
            community,
            call::<r::PatchRole>(&self.inner, &[&id], Some(body), None),
        )
        .await?;
        let _ = load_access(&self.inner, community).await;
        Ok(())
    }

    /// roles.ts `moveRole`: one step up or down ([`move_plan`]). Any refusal reads the access once: what the instance
    /// holds decides, not the rows the view had.
    pub async fn move_role(
        &self,
        community: CommunityId,
        role: RoleId,
        direction: Direction,
    ) -> Result<(), ApiError> {
        self.listed(community)?;
        let (rows, access, _) = self.access(community).await?;
        let plan = move_plan(&rows, role, direction, access.owner, access.top)
            .ok_or_else(|| refusal("E_BAD_INPUT", "unknown role"))?;
        if plan.is_empty() {
            return Ok(());
        }
        for (id, position) in plan {
            let body = Value::Array(vec![
                Value::Null,
                Value::Null,
                Value::Uint(position.into()),
                Value::Null,
                Value::Null,
                Value::Null,
                Value::Null,
            ])
            .encode();
            let id = hex(&id);
            if let Err(e) = call::<r::PatchRole>(&self.inner, &[&id], Some(body), None).await {
                let _ = load_access(&self.inner, community).await;
                return Err(e);
            }
        }
        let _ = load_access(&self.inner, community).await;
        Ok(())
    }

    /// roles.ts `deleteRole`: a 404 is a retried request whose first attempt went through.
    pub async fn delete_role(&self, community: CommunityId, role: RoleId) -> Result<(), ApiError> {
        self.listed(community)?;
        let id = hex(&role);
        match self
            .guarded(
                community,
                call::<r::DeleteRole>(&self.inner, &[&id], None, None),
            )
            .await
        {
            // E_ code: E_NOT_FOUND (the role is already gone).
            Err(e) if e.status != 404 => return Err(e),
            _ => {}
        }
        self.after_members(community).await;
        Ok(())
    }

    /// roles.ts `grantRole`.
    pub async fn grant_role(
        &self,
        community: CommunityId,
        user: UserId,
        role: RoleId,
    ) -> Result<(), ApiError> {
        self.listed(community)?;
        let (c, u, id) = (community.hex(), user.hex(), hex(&role));
        let body = Value::Array(Vec::new()).encode();
        self.guarded(
            community,
            call::<r::GrantRole>(&self.inner, &[&c, &u, &id], Some(body), None),
        )
        .await?;
        self.after_members(community).await;
        Ok(())
    }

    /// roles.ts `revokeRole`: a 404 means the role was not held any more (a retried request).
    pub async fn revoke_role(
        &self,
        community: CommunityId,
        user: UserId,
        role: RoleId,
    ) -> Result<(), ApiError> {
        self.listed(community)?;
        let (c, u, id) = (community.hex(), user.hex(), hex(&role));
        match self
            .guarded(
                community,
                call::<r::RevokeRole>(&self.inner, &[&c, &u, &id], None, None),
            )
            .await
        {
            // E_ code: E_NOT_FOUND (the grant is already gone).
            Err(e) if e.status != 404 => return Err(e),
            _ => {}
        }
        self.after_members(community).await;
        Ok(())
    }

    /// controller.ts `listSelfRoles`: published into `RoleState::self_roles`.
    pub async fn load_self_roles(&self, community: CommunityId) -> Result<Vec<SelfRole>, ApiError> {
        self.listed(community)?;
        let c = community.hex();
        let raw = call::<r::ListSelfRoles>(&self.inner, &[&c], None, None).await?;
        let rows = decode_self_roles(&raw).map_err(bad)?;
        let published = rows.clone();
        self.inner.publish(move |s| {
            s.roles.self_roles.insert(community, published);
        });
        Ok(rows)
    }

    /// controller.ts `configureSelfRoles`: the roles members may pick for themselves (at most [`ROLES_MAX`], no
    /// duplicates). The request id is stable across retries.
    pub async fn configure_self_roles(
        &self,
        community: CommunityId,
        roles: &[RoleId],
    ) -> Result<(), ApiError> {
        self.listed(community)?;
        if roles.len() > ROLES_MAX {
            return Err(refusal("E_ROUTE_INPUT", "at most 250 roles"));
        }
        let mut seen = std::collections::HashSet::new();
        if !roles.iter().all(|r| seen.insert(*r)) {
            return Err(refusal("E_ROUTE_INPUT", "duplicate role"));
        }
        let request = request_id();
        let body = Value::Array(vec![
            Value::Bytes(request.to_vec()),
            Value::Array(roles.iter().map(|r| Value::Bytes(r.to_vec())).collect()),
        ])
        .encode();
        let c = community.hex();
        let raw = self
            .guarded(
                community,
                call_once::<r::ConfigureSelfRoles>(
                    &self.inner,
                    &[&c],
                    Some(body),
                    Some(request.to_vec()),
                ),
            )
            .await?;
        let v = Value::decode(&raw).map_err(|_| refusal("E_DECODE", "not CBOR"))?;
        let ok = v.as_array().is_some_and(|a| {
            a.len() == 2
                && a[0].as_bytes() == Some(&request[..])
                && a[1].as_uint() == Some(roles.len() as u64)
        });
        if !ok {
            return Err(refusal(
                "E_CBOR_SHAPE",
                "invalid self-role policy acknowledgement",
            ));
        }
        let _ = self.load_self_roles(community).await;
        Ok(())
    }

    /// controller.ts `selectSelfRole`: picks (or drops) a self-assignable role for oneself.
    pub async fn select_self_role(
        &self,
        community: CommunityId,
        role: RoleId,
        enabled: bool,
    ) -> Result<(), ApiError> {
        self.listed(community)?;
        let request = request_id();
        let body = Value::Array(vec![
            Value::Bytes(request.to_vec()),
            Value::Uint(enabled.into()),
        ])
        .encode();
        let (c, id) = (community.hex(), hex(&role));
        call_once::<r::SelectSelfRole>(&self.inner, &[&c, &id], Some(body), Some(request.to_vec()))
            .await?;
        let _ = self.load_self_roles(community).await;
        let _ = crate::features::members::reload_members(&self.inner, community).await;
        Ok(())
    }
}

/// controller.ts `readAccess`: the permissions, roles and overwrites, published together. Answers what it published
/// (the roles without their counts), or `None` for a server this account no longer lists.
pub(crate) async fn load_access(
    inner: &Inner,
    community: CommunityId,
) -> Result<Option<(Vec<RoleSummary>, CommunityAccess)>, ApiError> {
    let c = community.hex();
    let args = [c.as_str()];
    let perms = call::<r::GetPermissions>(inner, &args, None, None);
    let roles = call::<r::ListRoles>(inner, &args, None, None);
    let overwrites = call::<r::ListOverwrites>(inner, &args, None, None);
    let (perms, roles, overwrites) = tokio::join!(perms, roles, overwrites);
    let perms = Value::decode(&perms?).map_err(|_| refusal("E_DECODE", "not CBOR"))?;
    let a = at_least(&perms, 2).map_err(bad)?;
    let bits = uint(&a[0]).map_err(bad)?;
    let top = u53(&a[1]).map_err(bad)?;
    if top > u64::from(rank::OWNER_TOP) {
        return Err(refusal("E_CBOR_RANGE", "rank above the owner's"));
    }
    let rows = decode_roles(&roles?).map_err(bad)?;
    let overwrites = overwrites.ok().and_then(|raw| decode_overwrites(&raw).ok());
    let snapshot = inner.snapshot();
    let me = snapshot.account.user_id;
    let Some(entry) = snapshot.communities.iter().find(|x| x.id == community) else {
        return Ok(None);
    };
    let owner = me.is_some_and(|me| entry.owner == me);
    let mut access = rank::access_of(bits, top as u32, owner);
    access.everyone_id = rows.iter().find(|r| r.position == 0).map(|r| r.id);
    access.overwrites_loaded = overwrites.is_some();
    let answer = (rows.clone(), access.clone());
    inner.publish(move |s| {
        if !s.communities.iter().any(|c| c.id == community) {
            return;
        }
        let members = s.members.get(&community).cloned().unwrap_or_default();
        s.roles.roles.insert(community, with_counts(rows, &members));
        s.roles.access.insert(community, access);
        if let Some(list) = overwrites {
            let mut by: HashMap<ChannelId, Vec<OverwriteSummary>> = HashMap::new();
            for (ch, o) in list {
                by.entry(ch).or_default().push(o);
            }
            s.roles.overwrites.insert(community, by);
        }
    });
    Ok(Some(answer))
}

/// The members of `community` sorted by user id: the shape [`authority::authority_for`] takes.
pub fn members_of(members: &[crate::state::Member]) -> Vec<([u8; 16], Vec<RoleId>)> {
    let mut out: BTreeMap<[u8; 16], Vec<RoleId>> = BTreeMap::new();
    for m in members {
        out.entry(m.user_id.0).or_insert_with(|| m.role_ids.clone());
    }
    out.into_iter().collect()
}

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Mutex};

    use crate::gateway::frames::{self, Frame, Op, Value};
    use crate::http::routes as r;
    use crate::seams::GatewayError;
    use crate::sync::api::Api;
    use crate::sync::requests::{RequestSink, SocketRequests};

    /// A socket that refuses every request with `code` (403).
    struct Refusing {
        socket: Arc<SocketRequests>,
        code: &'static str,
    }

    impl RequestSink for Refusing {
        fn send_now(&self, frame: Vec<u8>) -> Result<(), GatewayError> {
            let f = frames::decode(&frame).expect("a request frame");
            let id = f.payload[0].as_uint().expect("an id");
            let body = Value::Array(vec![
                Value::Text(self.code.into()),
                Value::Text(String::new()),
                Value::Null,
            ])
            .encode();
            let reply = frames::encode(&Frame {
                op: Op::RESPONSE,
                n: 0,
                group_id: None,
                payload: vec![Value::Uint(id), Value::Uint(403), Value::Bytes(body)],
            })
            .expect("a response frame");
            let socket = self.socket.clone();
            tokio::spawn(async move { socket.on_response(&reply) });
            Ok(())
        }
    }

    /// Fix round 1 (5): the members commands go through `Api::socket_call`, so its refusal hook sees their refusals: a
    /// factor-required answer sends the account to factor setup (client.rs installs the hook).
    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_members_refusal_reaches_the_refusal_hook() {
        let conn = rusqlite::Connection::open_in_memory().unwrap();
        let core = Arc::new(Mutex::new(
            dilla_core::client::ClientCore::open(Arc::new(Mutex::new(conn))).unwrap(),
        ));
        let auth = crate::auth::AuthToken::new("token");
        let http = crate::http::ReqwestHttp::new(
            url::Url::parse("http://127.0.0.1:9").unwrap(),
            auth.clone(),
        )
        .unwrap();
        let socket = SocketRequests::new();
        socket.attach_sink(Some(Arc::new(Refusing {
            socket: socket.clone(),
            code: "E_FACTOR_REQUIRED",
        })));
        socket.on_ready("one".into());
        let seen = Arc::new(Mutex::new(Vec::<String>::new()));
        let hook = {
            let seen = seen.clone();
            Arc::new(move |code: &str| seen.lock().unwrap().push(code.to_owned()))
        };
        let api = Api::new(http, socket, auth, core, hook);
        let c = "11".repeat(16);
        let u = "22".repeat(16);
        let e = api
            .socket_call::<r::KickMember>(&[&c, &u], None, None, true)
            .await
            .unwrap_err();
        assert_eq!((e.status, e.code.as_str()), (403, "E_FACTOR_REQUIRED"));
        assert_eq!(*seen.lock().unwrap(), ["E_FACTOR_REQUIRED"]);
    }
}
