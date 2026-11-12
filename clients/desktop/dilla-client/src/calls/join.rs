//! The join of a call (join.ts, L-TS-56 step 1): find the channel's call group (L-HTTP-100),
//! register one or join it by external commit, settle a pending leaf before anything is posted
//! (RV-1), derive the join epoch's key through the core's gate, and start the call (L-HTTP-102).
//! Nothing is posted for a tree the gate refuses, and the join commit is posted only after
//! `call_join_check` passes.
//!
//! Not ported from join.ts: the rejoin's wait for this device's own earlier Remove (409
//! `E_REMOVE_PENDING`). The [`crate::seams::Http`] seam carries a status without the instance's
//! `E_*` code, so it cannot tell that refusal from a lost commit race; such a join fails closed
//! (a [`JoinRefused`]) and the caller may try again.

use std::sync::Mutex;

use zeroize::Zeroizing;

use super::controller::media_epoch;
use super::{
    CallCore, CallRoutes, CallStarted, ChannelCall, CoreRefusal, MediaEpoch, RouteError, lock,
};

/// join.ts `JOIN_READS_MAX`: the most GroupInfo/tree read pairs one external join makes when the
/// pair disagrees (a commit landed between the reads).
pub const JOIN_READS_MAX: usize = 3;

/// The call to join.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct JoinTarget {
    pub channel: [u8; 16],
    /// `None` for a DM's call.
    pub community: Option<[u8; 16]>,
    /// The decoders this device names (`vdec`), or empty.
    pub vdec: String,
    /// The instance's policy version and external-sender key (`GET /v1/instance`).
    pub policy_version: u64,
    pub external_sender_pub: [u8; 32],
}

/// A joined call: its group, the join epoch's keys, and the LiveKit room.
#[derive(Debug)]
pub struct Joined {
    pub group_id: [u8; 16],
    pub media: MediaEpoch,
    pub started: CallStarted,
}

/// What the join needs from its owner.
#[async_trait::async_trait]
pub trait JoinHooks: Send + Sync {
    /// A fresh random group id (16 bytes from the OS CSPRNG).
    fn random_id(&self) -> [u8; 16];
    /// The worker's bounded trust refresh of the group's leaves (device lists); never fails.
    async fn refresh_leaves(&self, group: &[u8; 16]);
    /// True once the join was cancelled (a leave, sign-out or revocation): nothing more is posted.
    fn cancelled(&self) -> bool;
}

/// A join that ended without a call. `leave` names an active call group this device must leave
/// (its self-Remove and a forget, [`super::leave_call`]); `alone` marks one whose tree holds only
/// this device (nobody can commit its Remove, so the call is ended conditionally instead).
#[derive(Debug, thiserror::Error)]
#[error("{code} ({phase:?})")]
pub struct JoinRefused {
    pub code: String,
    pub phase: Phase,
    pub leave: Option<[u8; 16]>,
    pub alone: bool,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    /// A trust block (the person must compare safety numbers): `E_UMK_CHANGED`, `E_BINDING`, ...
    Blocked,
    Failed,
}

/// The call gate's hard refusals (core-port.ts `TRUST_BLOCKS`).
fn is_trust_block(code: &str) -> bool {
    matches!(
        code,
        "E_UMK_CHANGED"
            | "E_CREDENTIAL"
            | "E_DEVICE_UNLISTED"
            | "E_DEVICE_REVOKED"
            | "E_TIER_MISMATCH"
            | "E_BINDING"
    )
}

fn refused(code: &str, leave: Option<[u8; 16]>) -> JoinRefused {
    JoinRefused {
        code: code.to_owned(),
        phase: if is_trust_block(code) {
            Phase::Blocked
        } else {
            Phase::Failed
        },
        leave,
        alone: false,
    }
}

fn route_code(e: &RouteError) -> String {
    e.code.clone().unwrap_or_else(|| {
        e.status
            .map_or("E_NETWORK".into(), |s| format!("E_HTTP_{s}"))
    })
}

impl From<CoreRefusal> for JoinRefused {
    fn from(e: CoreRefusal) -> Self {
        refused(&e.code, None)
    }
}

fn stopped(leave: Option<[u8; 16]>) -> JoinRefused {
    refused("E_NO_CALL", leave)
}

/// Joins the channel's call. On success the caller connects `dilla_media::Call` with
/// `started.livekit_url` / `started.token` and publishes `media` (and every later epoch's keys,
/// [`super::media_epoch`]) to it.
pub async fn join_call<C: CallCore, R: CallRoutes + ?Sized, H: JoinHooks + ?Sized>(
    core: &Mutex<C>,
    routes: &R,
    hooks: &H,
    target: &JoinTarget,
) -> Result<Joined, JoinRefused> {
    let mut reread_exists = true;
    let mut reread_closed = true;
    let mut read = routes
        .get_channel_call(&target.channel)
        .await
        .map_err(|e| refused(&route_code(&e), None))?;
    loop {
        forget_stale(core, &target.channel, read.group_id.as_ref());
        let group;
        let mut created = false;
        match read.group_id {
            None => {
                group = hooks.random_id();
                let body = lock(core)?
                    .call_group_create(
                        &group,
                        target.community.as_ref(),
                        &target.channel,
                        target.policy_version,
                        &target.external_sender_pub,
                    )
                    .map_err(|e| refused(&e.code, None))?;
                if hooks.cancelled() {
                    let _ = lock(core).map(|mut c| c.group_discard(&group));
                    return Err(stopped(None));
                }
                match routes.post_group(body).await {
                    Ok(next_seq) => {
                        lock(core)?
                            .group_registered(&group, next_seq)
                            .map_err(|e| refused(&e.code, None))?;
                        created = true;
                    }
                    Err(e) => {
                        let _ = lock(core).map(|mut c| c.group_discard(&group));
                        // Another device registered the channel's call group first: read the
                        // call once more and join theirs.
                        if e.is(409, "E_GROUP_EXISTS") && reread_exists {
                            reread_exists = false;
                            read = reread(routes, target).await?;
                            continue;
                        }
                        return Err(refused(&route_code(&e), None));
                    }
                }
            }
            Some(existing) => {
                group = existing;
                join_existing(core, routes, hooks, target, &group).await?;
            }
        }
        let media = join_epoch(core, hooks, &group).await?;
        if hooks.cancelled() {
            return Err(stopped(Some(group)));
        }
        match routes.start_call(&target.channel, &target.vdec).await {
            Ok(started) => {
                return Ok(Joined {
                    group_id: group,
                    media,
                    started,
                });
            }
            Err(e) => {
                // The held key is zeroized as `media` drops; nothing more is posted.
                let alone =
                    created || (media.roster.len() == 1 && media.roster[0].0 == media.own_leaf);
                drop(media);
                // L-HTTP-105: the start raced the idle close of the group: it is gone at the
                // instance, so it is forgotten here and the call read once more.
                if e.is(503, "E_UNAVAILABLE") && reread_closed {
                    reread_closed = false;
                    let _ = lock(core).map(|mut c| c.call_group_forget(&group));
                    read = reread(routes, target).await?;
                    continue;
                }
                let mut r = refused(&route_code(&e), Some(group));
                r.alone = alone;
                return Err(r);
            }
        }
    }
}

async fn reread<R: CallRoutes + ?Sized>(
    routes: &R,
    target: &JoinTarget,
) -> Result<ChannelCall, JoinRefused> {
    routes
        .get_channel_call(&target.channel)
        .await
        .map_err(|e| refused(&route_code(&e), None))
}

/// The external join: a hard verdict refuses at the core's join and nothing is posted; a pending
/// leaf is settled by one bounded refresh, and the commit is posted only after `call_join_check`
/// passes (RV-1). A GroupInfo and tree of different epochs (a commit between the two reads) are
/// read again, at most [`JOIN_READS_MAX`] times.
async fn join_existing<C: CallCore, R: CallRoutes + ?Sized, H: JoinHooks + ?Sized>(
    core: &Mutex<C>,
    routes: &R,
    hooks: &H,
    target: &JoinTarget,
    group: &[u8; 16],
) -> Result<(), JoinRefused> {
    for reads in 1..=JOIN_READS_MAX {
        let again = reads < JOIN_READS_MAX && !hooks.cancelled();
        let info = routes
            .get_group_info(group)
            .await
            .map_err(|e| refused(&route_code(&e), None))?;
        let tree = routes
            .get_group_tree(group)
            .await
            .map_err(|e| refused(&route_code(&e), None))?;
        let joined = lock(core)?.call_group_join_external(
            group,
            target.community.as_ref(),
            &target.channel,
            target.policy_version,
            &info,
            &tree,
        );
        let body = match joined {
            Ok(body) => Zeroizing::new(body),
            Err(e) if e.code == "E_CORE_INPUT" && again => continue,
            Err(e) => return Err(refused(&e.code, None)),
        };
        let checked = check(core, hooks, group).await;
        if let Err(e) = checked {
            discard(core, group);
            return Err(refused(&e.code, None));
        }
        if hooks.cancelled() {
            discard(core, group);
            return Err(stopped(None));
        }
        match routes.post_resync(group, body.to_vec()).await {
            Ok(seq) => {
                lock(core)?
                    .group_joined(group, seq)
                    .map_err(|e| refused(&e.code, None))?;
                return Ok(());
            }
            Err(e) => {
                discard(core, group);
                let stale = e.is(422, "E_COMMIT_INVALID") || e.is(409, "E_COMMIT_CONFLICT");
                if again && stale && moved_since(routes, group, &info).await {
                    continue;
                }
                return Err(refused(&route_code(&e), None));
            }
        }
    }
    Err(refused("E_CORE_INPUT", None))
}

async fn check<C: CallCore, H: JoinHooks + ?Sized>(
    core: &Mutex<C>,
    hooks: &H,
    group: &[u8; 16],
) -> Result<(), CoreRefusal> {
    match lock(core)?.call_join_check(group) {
        Err(e) if e.code == "E_DEVICE_LIST_STALE" => {}
        other => return other,
    }
    hooks.refresh_leaves(group).await;
    lock(core)?.call_join_check(group)
}

/// The join epoch's key through the gate; a pending leaf is read once more, any refusal leaves the
/// joined group.
async fn join_epoch<C: CallCore, H: JoinHooks + ?Sized>(
    core: &Mutex<C>,
    hooks: &H,
    group: &[u8; 16],
) -> Result<MediaEpoch, JoinRefused> {
    match media_epoch(core, group) {
        Ok(m) => return Ok(m),
        Err(e) if e.code == "E_DEVICE_LIST_STALE" => {}
        Err(e) => return Err(refused(&e.code, Some(*group))),
    }
    hooks.refresh_leaves(group).await;
    media_epoch(core, group).map_err(|e| refused(&e.code, Some(*group)))
}

async fn moved_since<R: CallRoutes + ?Sized>(routes: &R, group: &[u8; 16], info: &[u8]) -> bool {
    routes
        .get_group_info(group)
        .await
        .is_ok_and(|now| now != info)
}

/// A staged (state 1) group is discarded; anything else is left as it is.
fn discard<C: CallCore>(core: &Mutex<C>, group: &[u8; 16]) {
    let Ok(mut c) = lock(core) else { return };
    let staged = c
        .groups()
        .map(|g| g.iter().any(|g| &g.group_id == group && g.state == 1))
        .unwrap_or(false);
    if staged {
        let _ = c.group_discard(group);
    }
}

/// A call group of this channel this device still holds, other than the one the instance names
/// now, is gone at the instance: its state goes, so the core's one-group-per-channel rule lets the
/// next one in.
fn forget_stale<C: CallCore>(core: &Mutex<C>, channel: &[u8; 16], current: Option<&[u8; 16]>) {
    let Ok(mut c) = lock(core) else { return };
    let Ok(groups) = c.groups() else { return };
    for g in groups {
        if g.kind != 1 || g.state == 4 || &g.target_id != channel || Some(&g.group_id) == current {
            continue;
        }
        let _ = if g.state <= 1 {
            c.group_discard(&g.group_id)
        } else {
            c.call_group_forget(&g.group_id)
        };
    }
}
