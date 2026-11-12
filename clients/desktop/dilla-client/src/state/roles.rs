//! `roles`: Task 14's slice of [`super::AppState`]: each community's roles, the caller's access, the channel overwrites
//! and the self-assignable roles (types.ts `RoleSummary`, `CommunityAccess`, `OverwriteSummary`, the `roles:`,
//! `permissions:` and `overwrites:` slices). Nothing here holds key material.
//!
//! Every gate the UI draws from here is advisory: the instance decides every request again, and a refusal reaches the
//! view with its own code.

use std::collections::HashMap;

use super::{ChannelId, CommunityId};

/// A role id: 16 bytes, as `Member::role_ids` holds them.
pub type RoleId = [u8; 16];

/// types.ts `RoleSummary`: one role row, with the count of members holding it (@everyone counts every member).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RoleSummary {
    pub id: RoleId,
    pub name: String,
    /// 0xRRGGBB; 0 is no colour.
    pub color: u32,
    /// 0 is @everyone; higher ranks higher.
    pub position: u32,
    pub hoist: bool,
    pub mentionable: bool,
    pub allow: u32,
    pub deny: u32,
    pub members: u32,
}

impl RoleSummary {
    /// The @everyone role (position 0).
    pub fn is_everyone(&self) -> bool {
        self.position == 0
    }
}

/// types.ts `CommunityAccess`: what the caller may do community-wide, from `GET /v1/communities/{id}/permissions`
/// (`features::roles::access_of`). The owner and an administrator hold every permission.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct CommunityAccess {
    /// The caller's rank: the highest position among the roles held, `OWNER_TOP` for the owner.
    pub top: u32,
    pub owner: bool,
    pub manage_community: bool,
    pub view_audit_log: bool,
    pub manage_channels: bool,
    pub kick_members: bool,
    pub ban_members: bool,
    pub moderate_members: bool,
    pub manage_messages: bool,
    pub create_invite: bool,
    pub manage_roles: bool,
    /// A read has landed. With `false` the view offers no gated action at all.
    pub loaded: bool,
    /// The @everyone role's id; `None` until the roles are read.
    pub everyone_id: Option<RoleId>,
    /// The overwrites read landed with the last access read.
    pub overwrites_loaded: bool,
}

/// types.ts `OverwriteSummary`: one channel overwrite (kind 0 a role, 1 a member).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct OverwriteSummary {
    pub kind: u8,
    pub target_id: [u8; 16],
    pub allow: u32,
    pub deny: u32,
}

/// One self-assignable role as `GET /v1/communities/{id}/self-roles` lists it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct SelfRole {
    pub id: RoleId,
    pub name: String,
    pub position: u32,
    /// The caller holds it by their own choice.
    pub self_selected: bool,
}

/// Task 14's slice: by community.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct RoleState {
    /// The roles, as last read (in the instance's order; views sort them).
    pub roles: HashMap<CommunityId, Vec<RoleSummary>>,
    /// The caller's access. Absent: not read yet, which the views treat as no access.
    pub access: HashMap<CommunityId, CommunityAccess>,
    /// Each channel's overwrites, by channel.
    pub overwrites: HashMap<CommunityId, HashMap<ChannelId, Vec<OverwriteSummary>>>,
    /// The self-assignable roles, once read.
    pub self_roles: HashMap<CommunityId, Vec<SelfRole>>,
}

impl RoleState {
    /// The caller's access in `community`, or the empty (not loaded) access.
    pub fn access_of(&self, community: CommunityId) -> CommunityAccess {
        self.access.get(&community).cloned().unwrap_or_default()
    }

    /// The roles of `community`, or none.
    pub fn roles_of(&self, community: CommunityId) -> &[RoleSummary] {
        self.roles.get(&community).map_or(&[], Vec::as_slice)
    }
}
