//! `server_admin`: Task 15's slice of [`super::AppState`]: the server settings' data, per community: what this account
//! may manage (`permissions:<id>`), the AFK timeout (`afk:<id>` settings), the invites that can still be used
//! (`invites:<id>`), the GIF settings and the welcome configuration. Nothing here holds key material: the Giphy key only
//! ever goes up and is never kept (`gifStatus` says only that a key is set).

use std::collections::HashMap;

use super::{ChannelId, CommunityId, UserId};

/// What this account may do community-wide (types.ts `CommunityAccess`, narrowed to what the settings read). The client's
/// answer is advisory: the instance decides every request again.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Access {
    /// The read landed; before it every section is hidden.
    pub loaded: bool,
    pub owner: bool,
    /// The highest role position this account holds (0 with none).
    pub top: u64,
    pub manage_community: bool,
    pub manage_channels: bool,
    pub manage_roles: bool,
    pub create_invite: bool,
    pub ban_members: bool,
    pub view_audit_log: bool,
    /// Holds the administrator bit (every permission, as the owner does).
    pub administrator: bool,
    /// The `@everyone` role (the one at position 0), once the roles are read.
    pub everyone: Option<[u8; 16]>,
    /// The overwrites were read too (without them a channel is never called private or public).
    pub overwrites_loaded: bool,
}

impl From<&super::roles::CommunityAccess> for Access {
    /// The settings' view of the one access (final review I2: one resolver, one slice).
    fn from(a: &super::roles::CommunityAccess) -> Self {
        Self {
            loaded: a.loaded,
            owner: a.owner,
            top: u64::from(a.top),
            manage_community: a.manage_community,
            manage_channels: a.manage_channels,
            manage_roles: a.manage_roles,
            create_invite: a.create_invite,
            ban_members: a.ban_members,
            view_audit_log: a.view_audit_log,
            administrator: a.administrator,
            everyone: a.everyone_id,
            overwrites_loaded: a.overwrites_loaded,
        }
    }
}

/// What this account may do in `community`, as the server settings read it: the one access slice
/// (`state.roles.access`), not loaded until a read landed.
pub fn access_in(state: &super::AppState, community: CommunityId) -> Access {
    Access::from(&state.roles.access_of(community))
}

/// One role as the channel permissions list it (`RoleRow`, narrowed to what the settings read).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AdminRole {
    pub id: [u8; 16],
    pub name: String,
    pub position: u64,
    /// The role's community-wide grant and refusal (26 bits).
    pub allow: u32,
    pub deny: u32,
}

/// The id of an invite.
#[derive(Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord)]
pub struct InviteId(pub [u8; 16]);

impl InviteId {
    pub fn hex(&self) -> String {
        super::hex(&self.0)
    }
}

impl std::fmt::Debug for InviteId {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "InviteId({})", self.hex())
    }
}

/// `InviteSummary`: an invite that can still be used.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Invite {
    pub id: InviteId,
    pub created_by: Option<UserId>,
    /// 0 is any number of uses.
    pub max_uses: u64,
    pub uses: u64,
    /// Unix seconds.
    pub created: u64,
    /// Unix seconds; 0 never expires.
    pub expires: u64,
}

/// The link an invite was made with. It is answered once, when the invite is made, and is not kept in the state.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct InviteCreated {
    pub id: InviteId,
    pub code: String,
    pub url: String,
    pub expires: u64,
}

/// `AFKSettings`: the AFK channel (the built-in one) and the minutes of inactivity before a move, in seconds.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct AfkSettings {
    pub channel: Option<ChannelId>,
    pub timeout_seconds: u64,
}

/// `GifRating`: the highest rating the server's GIF picker shows.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub enum GifRating {
    G,
    #[default]
    Pg,
    Pg13,
    R,
}

impl GifRating {
    pub const ALL: [GifRating; 4] = [Self::G, Self::Pg, Self::Pg13, Self::R];

    /// The wire's text (`g`, `pg`, `pg-13`, `r`).
    pub fn wire(self) -> &'static str {
        match self {
            Self::G => "g",
            Self::Pg => "pg",
            Self::Pg13 => "pg-13",
            Self::R => "r",
        }
    }

    pub fn from_wire(text: &str) -> Option<Self> {
        Self::ALL.into_iter().find(|r| r.wire() == text)
    }
}

/// `GifStatus`: whether this server has a Giphy key, and its rating.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct GifSettings {
    pub on: bool,
    pub rating: GifRating,
}

/// One channel overwrite (`OverwriteRow`): `kind` 0 is a role, 1 a member.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Overwrite {
    pub channel: ChannelId,
    pub kind: u8,
    pub target: [u8; 16],
    pub allow: u32,
    pub deny: u32,
}

/// The welcome configuration the Bot posts for a join (welcome-settings.ts `WelcomeSettingsValue`); `None` in the slice
/// is a server that never configured one.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct WelcomeSettings {
    pub channel: ChannelId,
    pub template: String,
    pub server_name: String,
}

/// What each read has landed for each community; a community with no entry has not been read yet.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct ServerAdminState {
    pub roles: HashMap<CommunityId, Vec<AdminRole>>,
    pub afk: HashMap<CommunityId, AfkSettings>,
    /// The AFK read failed (the section shows its load error).
    pub afk_load_failed: HashMap<CommunityId, bool>,
    pub invites: HashMap<CommunityId, Vec<Invite>>,
    pub gifs: HashMap<CommunityId, GifSettings>,
    pub overwrites: HashMap<CommunityId, Vec<Overwrite>>,
    pub welcome: HashMap<CommunityId, Option<WelcomeSettings>>,
    /// The instance's host as invite links show it (`host:port`), once known: from a link made, or the fixture.
    pub host: String,
}
