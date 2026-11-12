//! Area **dms** (Task 13): the E_UMK_CHANGED, E_CREDENTIAL and E_DEVICE_* blocks, compare by safety number and QR, verify, remove the changed device.
//!
//! Ports: packages/client-core/src/state/trust.ts and the trust half of controller.ts (`publishTrust`,
//! `publishSendBlocks`, `noteChangedLeaves`, `sharedUser`, `verifyContact`, `safetyNumber`).
//!
//! Every verdict comes from the core: [`AppState::trust`] is read again from `trust_state`, `send_block_code` and
//! `send_block_own` whenever anything may have moved them (a group changed, a member list or the DM list landed, a ready,
//! a trust refresh pass, a verification), and never kept from an earlier read when the core cannot answer (R3: every
//! contact then reads as history pending, not verified). The only things kept here are what the core stores nothing for
//! and the web keeps in its worker too: which groups held a contact's changed leaf (so a validated Remove of it can be
//! named, `removedIn`).
//!
//! "Remove the changed device" is the validated Remove of protocol/01: the changed device leaves (or is removed by a
//! member with the right), and this device commits the Remove as any member does (the sync engine's proposal and commit
//! path). The core then clears only that group's send block; the contact stays changed and unverified elsewhere, and the
//! conversation says so (`privacy.removed`).

use std::collections::{HashMap, HashSet};
use std::hash::{Hash, Hasher};
use std::sync::{Arc, Mutex, OnceLock, Weak};
use std::time::Duration;

use tokio::sync::{oneshot, watch};

use crate::client::{Client, ClientError, Inner};
use crate::state::channels::channel_group_state;
use crate::state::messages::{build_timeline, decode_outbox, decode_timeline};
use crate::state::trust::{OwnTrust, SendBlock, TrustBlock, TrustState, TrustSummary};
use crate::state::{AppState, Channel, ChannelGroupState, ChannelId, TIMELINE_PAGE, UserId, hex};
use crate::sync::GroupRow;
use crate::sync::api::try_core;
use crate::sync::trust::{Annotations, annotations_of, decode_pending_leaves, decode_trust_state};

/// The dms area's handle for trust: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Trust {
    inner: Arc<Inner>,
    state: watch::Receiver<Arc<AppState>>,
}

impl Client {
    /// The [`Trust`] handle.
    pub fn trust(&self) -> Trust {
        Trust {
            inner: self.inner().clone(),
            state: self.state(),
        }
    }
}

/// What the dms area keeps per client between reads (controller.ts `changedLeafGroups`, `removedGroups`, the DM rows
/// and the watcher), shared by [`Trust`] and [`super::dms::Dms`].
#[derive(Default)]
pub(crate) struct AreaMemory {
    /// user → the groups that held a changed leaf of theirs on the last read.
    changed_leaf_groups: HashMap<[u8; 16], HashSet<[u8; 16]>>,
    /// user → the groups whose validated Remove took their changed leaf, while the key stays changed.
    removed_groups: HashMap<[u8; 16], HashSet<[u8; 16]>>,
    /// The DMs the instance lists: channel → (kind, members, text group once known).
    pub(crate) dms: Vec<super::dms::KnownDm>,
    /// DMs whose group an open is in flight for (one at a time per DM).
    pub(crate) opening: HashSet<[u8; 16]>,
}

type Registry = Mutex<Vec<(Weak<Inner>, Arc<tokio::sync::Mutex<AreaMemory>>)>>;

/// The area's memory of `inner`'s client (made on first use; a dropped client's entry is swept).
pub(crate) fn memory(inner: &Arc<Inner>) -> Arc<tokio::sync::Mutex<AreaMemory>> {
    static REGISTRY: OnceLock<Registry> = OnceLock::new();
    let mut all = REGISTRY
        .get_or_init(Registry::default)
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    all.retain(|(w, _)| w.strong_count() > 0);
    if let Some((_, m)) = all
        .iter()
        .find(|(w, _)| std::ptr::eq(w.as_ptr(), Arc::as_ptr(inner)))
    {
        return m.clone();
    }
    let m = Arc::new(tokio::sync::Mutex::new(AreaMemory::default()));
    all.push((Arc::downgrade(inner), m.clone()));
    m
}

fn sync_err(e: impl std::fmt::Display) -> ClientError {
    ClientError::Sync(e.to_string())
}

fn core_code(e: &dilla_core::client::ClientError) -> ClientError {
    ClientError::Sync(e.code.to_owned())
}

/// 64 lower-case hex digits as the 32 bytes they spell.
fn key_of(hex64: &str) -> Option<[u8; 32]> {
    if !crate::state::trust::is_key_hex(hex64) {
        return None;
    }
    let mut out = [0u8; 32];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(&hex64[2 * i..2 * i + 2], 16).ok()?;
    }
    Some(out)
}

impl Trust {
    /// Starts the area's watcher once per client: the DM list and the trust slice are read again on every ready and
    /// whenever something they derive from changes. Idempotent; the UI calls it when it first draws the client.
    pub fn start(&self) {
        start(&self.inner, &self.state);
    }

    /// Reads the trust slice from the core now.
    pub fn refresh(&self) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move { publish_trust(&inner).await })
    }

    /// controller.ts `safetyNumber`: the twelve groups of five digits for exactly `umk` (the comparison key the dialog
    /// shows, 64 lower-case hex digits), for a contact a current group is shared with. The core refuses any other key
    /// (`E_DEVICE_LIST_STALE`); no shared group is `E_CORE_NOT_FOUND`.
    pub fn safety_number(
        &self,
        user: UserId,
        umk: String,
    ) -> oneshot::Receiver<Result<Vec<String>, ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let key = key_of(&umk).ok_or_else(|| ClientError::Sync("E_BAD_INPUT".into()))?;
            if !shared_user(&inner, user).await {
                return Err(ClientError::Sync("E_CORE_NOT_FOUND".into()));
            }
            let core = inner.engine().core().clone();
            let digits = try_core(&core, move |c| c.safety_number_for(&user.0, &key))
                .await
                .map_err(sync_err)?
                .map_err(|e| core_code(&e))?;
            let groups = crate::state::trust::safety_groups(&digits);
            if groups.len() != 12 {
                return Err(ClientError::Sync("E_CORE_INPUT".into()));
            }
            Ok(groups)
        })
    }

    /// controller.ts `verifyContact`: approves exactly `umk` for `user` in the core (which refuses any key but its
    /// comparison key, and a key whose signed list no longer names a current leaf). The approval re-judges the user's
    /// leaves and releases what they held; the trust slice and the open timelines are read again, and the user's leaves
    /// are fetched again on the next refresh.
    pub fn verify(&self, user: UserId, umk: String) -> oneshot::Receiver<Result<(), ClientError>> {
        let inner = self.inner.clone();
        self.inner.spawn(async move {
            let key = key_of(&umk).ok_or_else(|| ClientError::Sync("E_BAD_INPUT".into()))?;
            if !shared_user(&inner, user).await {
                return Err(ClientError::Sync("E_CORE_NOT_FOUND".into()));
            }
            let core = inner.engine().core().clone();
            try_core(&core, move |c| c.trust_verify(&user.0, &key))
                .await
                .map_err(sync_err)?
                .map_err(|e| core_code(&e))?;
            crate::sync::trust::forget_user(inner.engine(), &user.0).await;
            publish_trust(&inner).await?;
            republish_timelines(&inner).await;
            let engine = inner.engine().clone();
            let again = inner.clone();
            inner.spawn(async move {
                engine.refresh_trust().await;
                publish_trust(&again).await
            });
            Ok(())
        })
    }
}

/// Starts the watcher of `inner`'s client once (the UI calls this on every frame that draws the area).
pub(crate) fn start(inner: &Arc<Inner>, state: &watch::Receiver<Arc<AppState>>) {
    static STARTED: Mutex<Vec<Weak<Inner>>> = Mutex::new(Vec::new());
    {
        let mut started = STARTED
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner);
        started.retain(|w| w.strong_count() > 0);
        if started
            .iter()
            .any(|w| std::ptr::eq(w.as_ptr(), Arc::as_ptr(inner)))
        {
            return;
        }
        started.push(Arc::downgrade(inner));
    }
    let (inner, state) = (inner.clone(), state.clone());
    inner.clone().spawn(async move {
        watch_loop(inner, state).await;
        Ok(())
    });
}

/// While no ready came, the DM list is not read; afterwards it is read again on every ready and at this interval, so a DM
/// another person opened shows without a restart (the engine keeps a Welcome for a group it does not expect and says
/// nothing, see the task report).
const DM_POLL: Duration = Duration::from_secs(15);

/// The parts of the snapshot the trust slice and the DM list derive from (not the dms and trust slices themselves, so the
/// area's own publishes do not wake it).
fn fingerprint(s: &AppState) -> u64 {
    let mut h = std::collections::hash_map::DefaultHasher::new();
    s.account.user_id.hash(&mut h);
    s.connection.readies.hash(&mut h);
    (s.connection.status as u8).hash(&mut h);
    for c in &s.channels {
        (c.id, c.text_group_id, c.group as u8).hash(&mut h);
    }
    let mut communities: Vec<_> = s.members.iter().collect();
    communities.sort_by_key(|(c, _)| **c);
    for (c, members) in communities {
        c.hash(&mut h);
        for m in members {
            (m.user_id, &m.username, &m.display).hash(&mut h);
        }
    }
    let mut timelines: Vec<_> = s.timelines.iter().collect();
    timelines.sort_by_key(|(c, _)| **c);
    for (c, t) in timelines {
        c.hash(&mut h);
        t.items.len().hash(&mut h);
        for item in &t.items {
            (&item.key, item.state as u8).hash(&mut h);
        }
    }
    h.finish()
}

async fn watch_loop(inner: Arc<Inner>, mut state: watch::Receiver<Arc<AppState>>) {
    let mut revisions = crate::sync::trust::revisions();
    let mut last_print = None;
    let mut last_readies = 0u64;
    let mut poll = tokio::time::interval(DM_POLL);
    poll.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    loop {
        let mut polled = false;
        tokio::select! {
            changed = state.changed() => if changed.is_err() { return },
            _ = revisions.changed() => last_print = None,
            _ = poll.tick() => polled = true,
        }
        // Coalesce a burst of snapshots.
        tokio::time::sleep(Duration::from_millis(30)).await;
        let s = state.borrow_and_update().clone();
        let online = s.connection.status == crate::state::ConnectionStatus::Online;
        if online && (s.connection.readies != last_readies || polled) {
            last_readies = s.connection.readies;
            let _ = super::dms::reload(&inner).await;
            last_print = None;
        }
        let print = fingerprint(&s);
        if last_print != Some(print) {
            last_print = Some(print);
            // controller.ts refreshes trust on every group change (`onGroupChanged`), a join's activation included, so a
            // contact's key is pinned when this device joins their group, not only when they next write. The engine runs
            // its refresh only for applied rows or a new epoch; the refresh skips leaves it already fetched.
            let engine = inner.engine().clone();
            inner.spawn(async move {
                engine.refresh_trust().await;
                Ok(())
            });
            super::dms::publish_dms(&inner).await;
            let _ = publish_trust(&inner).await;
        }
    }
}

/// derive.ts `channelGroupState` for a DM, which is a text channel in mode 0 of no community.
pub(crate) fn dm_group_state(channel: ChannelId, groups: &[GroupRow]) -> ChannelGroupState {
    let pseudo = Channel {
        id: channel,
        community_id: crate::state::CommunityId([0; 16]),
        kind: 0,
        mode: 0,
        name: String::new(),
        topic: String::new(),
        parent_id: None,
        position: 0,
        slowmode: 0,
        text_group_id: None,
        group: ChannelGroupState::None,
    };
    channel_group_state(&pseudo, groups, &|_| false, &|_| false)
}

/// controller.ts `nameOf`: a member's display name (else username) from the first loaded member list that has them, else
/// the first 8 hex characters.
pub(crate) fn name_of(s: &AppState, user: UserId) -> String {
    let mut communities: Vec<_> = s.communities.iter().map(|c| c.id).collect();
    communities.dedup();
    for c in communities {
        if let Some(m) = s
            .members
            .get(&c)
            .and_then(|list| list.iter().find(|m| m.user_id == user))
        {
            return if m.display.is_empty() {
                m.username.clone()
            } else {
                m.display.clone()
            };
        }
    }
    user.hex()[..8].to_owned()
}

/// controller.ts `sharedUser`: a DM with `user` whose group is active here, or a community whose member list has them
/// and one of whose groups is active here.
fn shared_with(
    s: &AppState,
    dms: &[super::dms::KnownDm],
    groups: &[GroupRow],
    user: UserId,
) -> bool {
    let active = |g: &GroupRow| g.state == 2;
    if dms.iter().any(|dm| {
        dm.members.contains(&user.0)
            && groups
                .iter()
                .any(|g| active(g) && g.target_id == dm.channel)
    }) {
        return true;
    }
    s.communities.iter().any(|c| {
        s.members
            .get(&c.id)
            .is_some_and(|list| list.iter().any(|m| m.user_id == user))
            && groups
                .iter()
                .any(|g| active(g) && g.community_id == Some(c.id.0))
    })
}

async fn shared_user(inner: &Arc<Inner>, user: UserId) -> bool {
    let s = inner.snapshot();
    let groups = inner.engine().groups().await;
    let dms = memory(inner).lock().await.dms.clone();
    shared_with(&s, &dms, &groups, user)
}

/// controller.ts `noteChangedLeaves`: a group that had a changed leaf of a user, is still active here and has none now
/// lost it to a validated Remove.
fn note_changed_leaves(
    memory: &mut AreaMemory,
    leaves: &[crate::sync::trust::PendingLeaf],
    groups: &[GroupRow],
) {
    let mut now: HashMap<[u8; 16], HashSet<[u8; 16]>> = HashMap::new();
    for leaf in leaves.iter().filter(|l| l.reason == "changed") {
        now.entry(leaf.user_id).or_default().insert(leaf.group_id);
    }
    let active: HashSet<[u8; 16]> = groups
        .iter()
        .filter(|g| g.state == 2)
        .map(|g| g.group_id)
        .collect();
    for (user, before) in &memory.changed_leaf_groups {
        for group in before {
            if now.get(user).is_some_and(|g| g.contains(group)) || !active.contains(group) {
                continue;
            }
            memory
                .removed_groups
                .entry(*user)
                .or_default()
                .insert(*group);
        }
    }
    for (user, removed) in &mut memory.removed_groups {
        removed.retain(|g| active.contains(g) && !now.get(user).is_some_and(|n| n.contains(g)));
    }
    memory.removed_groups.retain(|_, r| !r.is_empty());
    memory.changed_leaf_groups = now;
}

/// What one read of the core answered.
struct CoreRead {
    records: Result<Vec<crate::sync::trust::TrustRecord>, ()>,
    leaves: Vec<crate::sync::trust::PendingLeaf>,
    blocks: Vec<([u8; 16], Option<&'static str>, bool)>,
}

/// controller.ts `publishTrust` and `publishSendBlocks`: the trust slice from the core, published.
pub(crate) async fn publish_trust(inner: &Arc<Inner>) -> Result<(), ClientError> {
    let s = inner.snapshot();
    let groups = inner.engine().groups().await;
    let memory = memory(inner);
    let dms = memory.lock().await.dms.clone();
    let known = |channel: &[u8; 16]| {
        s.channel(ChannelId(*channel)).is_some() || dms.iter().any(|d| d.channel == *channel)
    };
    let blocked_groups: Vec<[u8; 16]> = groups
        .iter()
        .filter(|g| g.kind == 0 && g.state == 2 && known(&g.target_id))
        .map(|g| g.group_id)
        .collect();
    let core = inner.engine().core().clone();
    let read = try_core(&core, move |c| {
        let records = c
            .trust_state()
            .map_err(|_| ())
            .and_then(|raw| decode_trust_state(&raw).map_err(|_| ()));
        let leaves = c
            .pending_leaves()
            .ok()
            .and_then(|raw| decode_pending_leaves(&raw).ok())
            .unwrap_or_default();
        // A group whose gate the core cannot read is blocked (fail closed).
        let blocks = blocked_groups
            .iter()
            .map(|g| match (c.send_block_code(g), c.send_block_own(g)) {
                (Ok(code), Ok(own)) => (*g, code, own),
                _ => (*g, Some("E_CREDENTIAL"), false),
            })
            .collect();
        CoreRead {
            records,
            leaves,
            blocks,
        }
    })
    .await
    .map_err(sync_err)?;
    let annotations: Annotations = annotations_of(inner.engine()).await;
    let removed = {
        let mut m = memory.lock().await;
        note_changed_leaves(&mut m, &read.leaves, &groups);
        m.removed_groups.clone()
    };
    let own = s.account.user_id;
    // The page learns the channel a Remove unblocked, never the group id.
    let channel_of: HashMap<[u8; 16], ChannelId> = groups
        .iter()
        .filter(|g| g.kind == 0 && known(&g.target_id))
        .map(|g| (g.group_id, ChannelId(g.target_id)))
        .collect();
    let mut send_blocks = HashMap::new();
    for (g, code, own_block) in &read.blocks {
        if let (Some(code), Some(channel)) = (code, channel_of.get(g)) {
            send_blocks.insert(
                *channel,
                SendBlock {
                    code: TrustBlock::of(code),
                    own: *own_block,
                },
            );
        }
    }
    let Ok(records) = read.records else {
        // R3: a trust state the core cannot read never leaves an earlier verified view standing: every contact reads as
        // history pending (not verified, no compare) until the core answers again; the gate is read as it is.
        inner.publish(move |state| {
            for c in &mut state.trust.contacts {
                c.verified = false;
                c.unlisted = true;
            }
            state.trust.failed = true;
            state.trust.loaded = true;
            state.trust.send_blocks = send_blocks;
        });
        return Ok(());
    };
    let own_row = records.iter().find(|r| Some(UserId(r.user_id)) == own);
    let own_blocked = own_row.and_then(|r| r.blocked.as_deref().map(TrustBlock::of));
    let own_trust = if own_blocked == Some(TrustBlock::UmkChanged) || annotations.own_refused {
        Some(OwnTrust::Changed)
    } else if own_blocked.is_some() || own.is_some_and(|o| annotations.unlisted.contains(&o.0)) {
        Some(OwnTrust::Unlisted)
    } else {
        None
    };
    let contacts: Vec<TrustSummary> = records
        .iter()
        .filter(|r| Some(UserId(r.user_id)) != own)
        .map(|r| {
            let user = UserId(r.user_id);
            let blocked = r.blocked.as_deref().map(TrustBlock::of);
            // An approved key ends the changed state everywhere, so no group is left to call removed.
            let mut removed_in: Vec<ChannelId> = if r.changed_umk.is_none() {
                Vec::new()
            } else {
                removed
                    .get(&r.user_id)
                    .map(|gs| {
                        gs.iter()
                            .filter_map(|g| channel_of.get(g).copied())
                            .collect()
                    })
                    .unwrap_or_default()
            };
            removed_in.sort();
            removed_in.dedup();
            let unlisted = blocked.is_some_and(|b| b != TrustBlock::UmkChanged)
                || annotations.unlisted.contains(&r.user_id)
                || annotations.pending.contains(&r.user_id);
            TrustSummary {
                user_id: user,
                name: name_of(&s, user),
                shared: shared_with(&s, &dms, &groups, user),
                umk: hex(&r.umk),
                first_seen: r.first_seen,
                // F4: never verified while the core holds a failed or changed verdict for one of the contact's leaves.
                verified: r.verified && blocked.is_none(),
                changed_umk: r.changed_umk.map(|k| hex(&k)),
                unlisted,
                removed_in,
                blocked,
            }
        })
        .collect();
    {
        let mut m = memory.lock().await;
        for r in &records {
            if r.changed_umk.is_none() {
                m.removed_groups.remove(&r.user_id);
            }
        }
    }
    let next = TrustState {
        loaded: true,
        contacts,
        send_blocks,
        own: own_trust,
        failed: false,
    };
    inner.publish(move |state| {
        if state.trust != next {
            state.trust = next;
        }
    });
    Ok(())
}

/// After a verification the core released what the user's leaves held: every timeline this device shows is read again
/// from the core (controller.ts reads every open timeline again when the trust generation moved).
async fn republish_timelines(inner: &Arc<Inner>) {
    let s = inner.snapshot();
    let (Some(own_user), Some(own_device)) = (s.account.user_id, s.account.device_id) else {
        return;
    };
    let groups = inner.engine().groups().await;
    let core = inner.engine().core().clone();
    let mut fresh = Vec::new();
    for (channel, timeline) in &s.timelines {
        let Some(g) = crate::state::channels::channel_group(*channel, &groups)
            .filter(|g| g.state != 4)
            .map(|g| g.group_id)
        else {
            continue;
        };
        let Ok((rows, outbox)) = try_core(&core, move |c| {
            (c.timeline(&g, 0, TIMELINE_PAGE), c.outbox(&g))
        })
        .await
        else {
            continue;
        };
        let Some(rows) = rows.ok().and_then(|raw| decode_timeline(&raw).ok()) else {
            continue;
        };
        let outbox = outbox
            .ok()
            .and_then(|raw| decode_outbox(&raw).ok())
            .unwrap_or_default();
        fresh.push(build_timeline(
            *channel,
            timeline.group,
            &rows,
            &outbox,
            own_user.0,
            own_device.0,
            TIMELINE_PAGE,
        ));
    }
    inner.publish(move |state| {
        for t in fresh {
            state.timelines.insert(t.channel_id, t);
        }
    });
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::sync::trust::PendingLeaf;

    fn group(id: u8, state: u8) -> GroupRow {
        GroupRow {
            group_id: [id; 16],
            kind: 0,
            community_id: None,
            target_id: [id + 100; 16],
            state,
            epoch: 1,
            next_seq: 1,
            proposals_pending: 0,
            pending_commit: false,
        }
    }

    fn changed(user: u8, g: u8) -> PendingLeaf {
        PendingLeaf {
            group_id: [g; 16],
            leaf_index: 1,
            user_id: [user; 16],
            cached_version: 1,
            reason: "changed".into(),
        }
    }

    // controller.ts noteChangedLeaves: a validated Remove names the group; it is forgotten once the group is gone or the
    // changed leaf is back.
    #[test]
    fn a_validated_remove_is_noted_per_group() {
        let mut m = AreaMemory::default();
        let groups = [group(1, 2), group(2, 2)];
        note_changed_leaves(&mut m, &[changed(9, 1), changed(9, 2)], &groups);
        assert!(m.removed_groups.is_empty());
        // The leaf left group 1 only.
        note_changed_leaves(&mut m, &[changed(9, 2)], &groups);
        assert_eq!(
            m.removed_groups.get(&[9; 16]).cloned(),
            Some(HashSet::from([[1; 16]]))
        );
        // Group 1 is gone here: nothing to name.
        note_changed_leaves(&mut m, &[changed(9, 2)], &[group(1, 4), group(2, 2)]);
        assert!(m.removed_groups.is_empty());
    }

    #[test]
    fn keys_parse_only_as_lower_hex() {
        assert_eq!(key_of(&"ab".repeat(32)), Some([0xab; 32]));
        assert_eq!(key_of(&"AB".repeat(32)), None);
        assert_eq!(key_of("ab"), None);
    }

    #[test]
    fn a_dm_group_state_follows_its_core_group() {
        let g = group(1, 2);
        assert_eq!(
            dm_group_state(ChannelId([101; 16]), std::slice::from_ref(&g)),
            ChannelGroupState::Active
        );
        assert_eq!(
            dm_group_state(ChannelId([102; 16]), &[g]),
            ChannelGroupState::None
        );
    }
}
