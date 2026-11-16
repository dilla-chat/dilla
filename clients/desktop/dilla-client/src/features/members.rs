//! Area **members** (Task 14): the member list's extras: presence, the grouping and the person menu, and the profile
//! card.
//!
//! Ports: packages/web/src/screens/admin/{members-model,member-card,role-palette}.ts, and the worker's presence
//! (worker/controller.ts `incomingPresence`, `loadPresence`).
//!
//! Receives op 48 `presence` from the gateway (`crate::sync::route`), through [`AreaFrames`].

use std::cmp::Ordering;
use std::collections::HashMap;
use std::sync::Arc;

use crate::client::{Client, Inner};
use crate::features::roles::{call, rank};
use crate::gateway::frames::{Frame, Value};
use crate::http::routes as r;
use crate::state::members::{Member, Presence, decode_members};
use crate::state::roles::{CommunityAccess, RoleSummary};
use crate::state::{CommunityId, UserId};
use crate::sync::api::ApiError;
use crate::sync::route::AreaFrames;
use crate::sync::value::{Rows, at_least, bin16, u53};

/// The members area's handle for members: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Members {
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Members`] handle.
    pub fn members(&self) -> Members {
        Members {
            inner: self.inner().clone(),
        }
    }
}

impl Members {
    /// Runs `job` (built from this handle) on the client's runtime; see [`crate::features::moderation::Moderation::run`].
    pub fn run<T: Send + 'static, F, J: FnOnce(Members) -> F>(
        &self,
        job: J,
    ) -> impl std::future::Future<Output = Result<T, ApiError>> + use<T, F, J>
    where
        F: std::future::Future<Output = Result<T, ApiError>> + Send + 'static,
    {
        crate::features::roles::on_runtime(&self.inner, job(self.clone()))
    }

    /// Reads the presence snapshot of `community` (the pane says loading until it lands).
    pub async fn load_presence(&self, community: CommunityId) -> Result<(), ApiError> {
        load_presence(&self.inner, community).await
    }

    /// Reads the member list of `community` again.
    pub async fn reload(&self, community: CommunityId) -> Result<(), ApiError> {
        reload_members(&self.inner, community).await
    }

    /// Everything the members pane and the settings sections of `community` read: the presence, and the caller's access
    /// with the roles and the overwrites.
    pub async fn open(&self, community: CommunityId) -> Result<(), ApiError> {
        let (presence, access) = tokio::join!(
            load_presence(&self.inner, community),
            crate::features::roles::load_access(&self.inner, community)
        );
        presence.and(access.map(|_| ()))
    }
}

impl Members {
    /// Tells the instance the person is here (op 57 `presence.activity`, gateway.ts `sendPresenceActivity`): the
    /// instance shows an account idle once none of its devices reported input for 10 minutes (gateway presence.go), so
    /// a client that never reports shows its own person idle while they use it. Sent only while connected; the caller
    /// throttles ([`ActivityThrottle`]). Whether the frame went out.
    pub fn presence_activity(&self) -> bool {
        if self.inner.snapshot().connection.status != crate::state::ConnectionStatus::Online {
            return false;
        }
        let frame = Frame {
            op: crate::gateway::frames::Op::PRESENCE_ACTIVITY,
            n: 0,
            group_id: None,
            payload: Vec::new(),
        };
        let Ok(bytes) = crate::gateway::frames::encode(&frame) else {
            return false;
        };
        self.inner.engine().send_gateway_frame(bytes)
    }
}

/// App.tsx's activity sampling: one report per 5 s of input at most (gateway.ts keeps the same 5 s).
#[derive(Clone, Copy, Debug, Default)]
pub struct ActivityThrottle {
    last: Option<std::time::Instant>,
}

impl ActivityThrottle {
    pub const EVERY: std::time::Duration = std::time::Duration::from_secs(5);

    /// Whether input at `now` is reported (and, if so, `now` is the last report).
    pub fn due(&mut self, now: std::time::Instant) -> bool {
        if self
            .last
            .is_some_and(|last| now.saturating_duration_since(last) < Self::EVERY)
        {
            return false;
        }
        self.last = Some(now);
        true
    }
}

#[cfg(test)]
mod activity_tests {
    use super::ActivityThrottle;
    use std::time::{Duration, Instant};

    #[test]
    fn input_is_reported_at_most_every_five_seconds() {
        let t0 = Instant::now();
        let mut t = ActivityThrottle::default();
        assert!(t.due(t0), "the first input is reported");
        assert!(!t.due(t0 + Duration::from_secs(1)));
        assert!(!t.due(t0 + Duration::from_millis(4_999)));
        assert!(t.due(t0 + Duration::from_secs(5)));
        assert!(!t.due(t0 + Duration::from_secs(6)));
    }
}

impl AreaFrames for Members {
    /// controller.ts `incomingPresence`: `[user_id, status]`, status 0..=3; anything else is ignored. A community whose
    /// presence is not read yet reads it whole.
    fn on_frame(inner: &Arc<Inner>, f: &Frame) {
        let (Some(user), Some(status)) = (
            f.payload.first().and_then(Value::as_bytes),
            f.payload.get(1).and_then(Value::as_uint),
        ) else {
            return;
        };
        let (Ok(user), Some(status)) = (<[u8; 16]>::try_from(user), Presence::of_wire(status))
        else {
            return;
        };
        let user = UserId(user);
        let s = inner.snapshot();
        let mut unread = Vec::new();
        for c in &s.communities {
            if !s
                .members
                .get(&c.id)
                .is_some_and(|m| m.iter().any(|m| m.user_id == user))
            {
                continue;
            }
            if s.people.presence.contains_key(&c.id) {
                let community = c.id;
                inner.publish(move |s| {
                    if let Some(map) = s.people.presence.get_mut(&community) {
                        map.insert(user, status);
                    }
                });
            } else {
                unread.push(c.id);
            }
        }
        for community in unread {
            let inner2 = Arc::clone(inner);
            drop(inner.spawn(async move {
                let _ = load_presence(&inner2, community).await;
                Ok(())
            }));
        }
    }
}

/// routes.ts `getPresenceSnapshot`: `[[user_id, status]]`, published whole.
pub(crate) async fn load_presence(inner: &Inner, community: CommunityId) -> Result<(), ApiError> {
    let c = community.hex();
    let raw = call::<r::GetPresenceSnapshot>(inner, &[&c], None, None).await?;
    let bad = |e: crate::sync::value::DecodeError| ApiError::local(0, "E_DECODE", e.0);
    let mut map = HashMap::new();
    for row in Rows::of(&raw).map_err(bad)?.iter() {
        let a = at_least(row, 2).map_err(bad)?;
        let status = Presence::of_wire(u53(&a[1]).map_err(bad)?)
            .ok_or_else(|| ApiError::local(0, "E_DECODE", "presence status"))?;
        map.insert(UserId(bin16(&a[0]).map_err(bad)?), status);
    }
    inner.publish(move |s| {
        if s.communities.iter().any(|c| c.id == community) {
            s.people.presence.insert(community, map);
        }
    });
    Ok(())
}

/// The worker's `reloadMembers`: the member list of `community` read again, every page, and the role counts with it.
pub(crate) async fn reload_members(inner: &Inner, community: CommunityId) -> Result<(), ApiError> {
    let mut all = Vec::new();
    for page in inner.api().list_members(&community.0).await? {
        all.extend(decode_members(&page).map_err(|e| ApiError::local(0, "E_DECODE", e.0))?);
    }
    inner.publish(move |s| {
        if !s.communities.iter().any(|c| c.id == community) {
            return;
        }
        if let Some(rows) = s.roles.roles.remove(&community) {
            s.roles
                .roles
                .insert(community, crate::features::roles::with_counts(rows, &all));
        }
        s.members.insert(community, all);
    });
    Ok(())
}

// ---- members-model.ts ----

/// members-model.ts `memberName`: the display name, else the username.
pub fn member_name(m: &Member) -> &str {
    if m.display.is_empty() {
        &m.username
    } else {
        &m.display
    }
}

/// `localeCompare` as the web sorts names: case and accents only break ties.
pub fn by_name(a: &str, b: &str) -> Ordering {
    let fold = |s: &str| -> String {
        s.chars()
            .flat_map(char::to_lowercase)
            .map(|c| match c {
                'à' | 'á' | 'â' | 'ã' | 'ä' | 'å' => 'a',
                'ç' => 'c',
                'è' | 'é' | 'ê' | 'ë' => 'e',
                'ì' | 'í' | 'î' | 'ï' => 'i',
                'ñ' => 'n',
                'ò' | 'ó' | 'ô' | 'õ' | 'ö' => 'o',
                'ù' | 'ú' | 'û' | 'ü' => 'u',
                'ý' | 'ÿ' => 'y',
                c => c,
            })
            .collect()
    };
    // Lower case before upper case at an otherwise equal place, as ICU's default tertiary order.
    let upper = |s: &str| {
        s.chars()
            .map(|c| u8::from(c.is_uppercase()))
            .collect::<Vec<_>>()
    };
    fold(a)
        .cmp(&fold(b))
        .then_with(|| upper(a).cmp(&upper(b)))
        .then_with(|| a.cmp(b))
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum MemberTag {
    Owner,
    You,
    Bot,
}

/// members-model.ts `MemberRow`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MemberRow {
    pub user_id: UserId,
    pub name: String,
    pub tags: Vec<MemberTag>,
    /// The colour of the member's highest coloured role (any role, hoisted or not).
    pub color: Option<u32>,
}

/// What a group of the list is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum GroupKind {
    /// A hoisted role.
    Role,
    /// Everyone who holds no hoisted role (and is online, when presence is known).
    Online,
    Bots,
    Offline,
}

/// members-model.ts `MemberGroup`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MemberGroup {
    pub kind: GroupKind,
    /// The hoisted role of a [`GroupKind::Role`] group.
    pub role: Option<RoleSummary>,
    pub members: Vec<MemberRow>,
}

fn colour_of(member: &Member, roles: &[RoleSummary]) -> Option<u32> {
    roles
        .iter()
        .filter(|r| r.color != 0 && member.role_ids.contains(&r.id))
        .max_by_key(|r| r.position)
        .map(|r| r.color)
}

/// members-model.ts `memberGroups`: by each member's highest hoisted role (highest first), then everyone else, then the
/// Bots, then (with presence) the offline people. Names sort as `localeCompare` does; each row is tagged owner, you or
/// bot. Empty groups stay in the answer (the view draws only non-empty ones).
pub fn member_groups(
    members: &[Member],
    roles: &[RoleSummary],
    owner: UserId,
    me: Option<UserId>,
    presence: Option<&HashMap<UserId, Presence>>,
) -> Vec<MemberGroup> {
    let mut hoisted: Vec<&RoleSummary> = roles.iter().filter(|r| r.hoist).collect();
    hoisted.sort_by_key(|r| std::cmp::Reverse(r.position));
    let mut buckets: HashMap<[u8; 16], Vec<MemberRow>> = HashMap::new();
    let (mut plain, mut bots, mut offline) = (Vec::new(), Vec::new(), Vec::new());
    for m in members {
        let best = m
            .role_ids
            .iter()
            .filter_map(|id| hoisted.iter().find(|r| r.id == *id))
            .max_by_key(|r| r.position);
        let mut tags = Vec::new();
        if m.user_id == owner {
            tags.push(MemberTag::Owner);
        }
        if Some(m.user_id) == me {
            tags.push(MemberTag::You);
        }
        if m.kind == 1 {
            tags.push(MemberTag::Bot);
        }
        let row = MemberRow {
            user_id: m.user_id,
            name: member_name(m).to_owned(),
            tags,
            color: colour_of(m, roles),
        };
        let is_offline = presence
            .is_some_and(|p| p.get(&m.user_id).copied().unwrap_or_default() == Presence::Offline);
        if m.kind == 1 {
            bots.push(row);
        } else if is_offline {
            offline.push(row);
        } else if let Some(best) = best {
            buckets.entry(best.id).or_default().push(row);
        } else {
            plain.push(row);
        }
    }
    let sort = |mut rows: Vec<MemberRow>| {
        rows.sort_by(|a, b| by_name(&a.name, &b.name));
        rows
    };
    let mut groups: Vec<MemberGroup> = hoisted
        .iter()
        .filter_map(|r| {
            buckets.remove(&r.id).map(|rows| MemberGroup {
                kind: GroupKind::Role,
                role: Some((*r).clone()),
                members: sort(rows),
            })
        })
        .collect();
    groups.push(MemberGroup {
        kind: GroupKind::Online,
        role: None,
        members: sort(plain),
    });
    groups.push(MemberGroup {
        kind: GroupKind::Bots,
        role: None,
        members: sort(bots),
    });
    if presence.is_some() {
        groups.push(MemberGroup {
            kind: GroupKind::Offline,
            role: None,
            members: sort(offline),
        });
    }
    groups
}

/// members-model.ts `MenuAction`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum MenuAction {
    Message,
    Compare,
    Roles,
    Warn,
    Timeout,
    Kick,
    Ban,
}

/// members-model.ts `menuFor`: message and compare (never on oneself), roles with manage_roles, and warn, timeout, kick
/// and ban only when the loaded access allows it and the target ranks below the caller (never the owner or oneself).
/// With no access loaded nothing gated is offered. Advisory only: the instance decides again.
pub fn menu_for(
    member: &Member,
    access: Option<&CommunityAccess>,
    me: UserId,
    owner: UserId,
    roles: &[RoleSummary],
) -> Vec<MenuAction> {
    let mut out = Vec::new();
    if member.user_id != me {
        out.extend([MenuAction::Message, MenuAction::Compare]);
    }
    let Some(access) = access.filter(|a| a.loaded) else {
        return out;
    };
    if access.manage_roles {
        out.push(MenuAction::Roles);
    }
    let may = rank::can_moderate(
        me.0,
        member.user_id.0,
        owner.0,
        access.top,
        rank::top_of(&member.role_ids, roles),
    );
    if may && access.moderate_members {
        out.extend([MenuAction::Warn, MenuAction::Timeout]);
    }
    if may && access.kick_members {
        out.push(MenuAction::Kick);
    }
    if may && access.ban_members {
        out.push(MenuAction::Ban);
    }
    out
}

/// member-card.ts `MemberCardData`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct MemberCard {
    pub user_id: UserId,
    pub name: String,
    pub username: String,
    pub status: String,
    pub bio: String,
    pub is_owner: bool,
    pub is_you: bool,
    pub is_bot: bool,
    /// The member's roles (never @everyone), highest first: `(name, colour)`, colour 0 for none.
    pub roles: Vec<(String, u32)>,
    pub actions: Vec<MenuAction>,
}

/// member-card.ts `memberCardData`.
pub fn member_card(
    member: &Member,
    access: Option<&CommunityAccess>,
    me: UserId,
    owner: UserId,
    roles: &[RoleSummary],
) -> MemberCard {
    let mut held: Vec<&RoleSummary> = roles
        .iter()
        .filter(|r| r.position != 0 && member.role_ids.contains(&r.id))
        .collect();
    held.sort_by_key(|r| std::cmp::Reverse(r.position));
    MemberCard {
        user_id: member.user_id,
        name: member_name(member).to_owned(),
        username: member.username.clone(),
        status: member.status_msg.clone(),
        bio: member.bio.clone(),
        is_owner: member.user_id == owner,
        is_you: member.user_id == me,
        is_bot: member.kind == 1,
        roles: held.iter().map(|r| (r.name.clone(), r.color)).collect(),
        actions: menu_for(member, access, me, owner, roles),
    }
}

/// role-palette.ts `SIGNAL`: the role editor's six swatches and the name key of each (`roles.color.<key>`).
pub const SIGNAL_SWATCHES: [(u32, &str); 6] = [
    (0xE9_B6EC, "pink"),
    (0x5A_A8E6, "blue"),
    (0xFF_D16A, "amber"),
    (0x7C_FF8E, "green"),
    (0xFF_8A65, "orange"),
    (0xB0_BEC5, "grey"),
];

/// role-palette.ts `signalPalette`: the six swatches, plus the role's own colour as a seventh (`custom`) when it is set
/// and not among them.
pub fn signal_palette(current: u32) -> Vec<(u32, &'static str)> {
    let mut out = SIGNAL_SWATCHES.to_vec();
    if current != 0 && !SIGNAL_SWATCHES.iter().any(|(v, _)| *v == current) {
        out.push((current, "custom"));
    }
    out
}
