//! The join of a call (join.ts, L-TS-56 step 1): find the channel's call group (L-HTTP-100),
//! register one or join it by external commit, settle a pending leaf before anything is posted
//! (RV-1), derive the join epoch's key through the core's gate, and start the call (L-HTTP-102).
//! Nothing is posted for a tree the gate refuses, and the join commit is posted only after
//! `call_join_check` passes.
//!
//! A rejoin while this device's own earlier Remove is still outstanding (join.ts, web-3 task 10,
//! concern 2): the instance refuses the resync of a Remove's target with 409 `E_REMOVE_PENDING`
//! (and only that refusal waits; every other one, a 403 included, fails at once). The join reports
//! [`CallPhase::WaitingForRemove`] and waits until another member has committed the Remove (the
//! device then joins afresh, in the same group) or the call has ended and its group closed (the
//! device registers a fresh one). Each step, [`REJOIN_STEP`] apart, reads the call and the group's
//! GroupInfo and tries the join again only when either has changed; the wait ends [`REJOIN_WAIT`]
//! after the first refusal, failing closed with that refusal.
//!
//! A join whose external commit reaches the call group after the last member ended the call (its
//! `DELETE ?if_alone=1` closed the group) is refused 409 `E_GROUP_CLOSED`, never as a stale
//! epoch: the join reads the call once more and registers a fresh call group, as a first joiner
//! does (join.ts).
//!
//! A join whose external commit reaches the call group while it is frozen (a member left and the
//! instance's Remove of its leaf is still outstanding: 425 `E_COMMIT_REQUIRED`, the instance's
//! freeze, invariant 5) waits for an online member to commit that Remove and tries again on
//! [`FROZEN_RETRY`], each step at least the instance's `retry_after_ms` hint up to the schedule's
//! longest step (the Bot's `retryMusicCallGroupJoin`, the text channel's own 425 retry). An external
//! commit cannot carry the Remove itself (RFC 9420 §12.4.3.2: no proposals by reference), so the
//! joiner can only wait. A freeze that outlasts the schedule fails the join with that refusal.

use std::sync::Mutex;
use std::time::Duration;

use tokio::time::Instant;
use zeroize::Zeroizing;

use super::controller::media_epoch;
use super::{
    CallCore, CallPhase, CallRoutes, CallStarted, ChannelCall, CoreRefusal, MediaEpoch, RouteError,
    lock,
};
use crate::state::UserId;

/// join.ts `JOIN_READS_MAX`: the most GroupInfo/tree read pairs one external join makes when the
/// pair disagrees (a commit landed between the reads).
pub const JOIN_READS_MAX: usize = 3;
/// join.ts `REJOIN_STEP_MS`: how far apart the rejoin's wait reads the call and its GroupInfo (two
/// requests: at most one a second).
pub const REJOIN_STEP: Duration = Duration::from_millis(2_000);
/// join.ts `REJOIN_WAIT_MS`: how long after the first `E_REMOVE_PENDING` the rejoin waits. It
/// outlasts the room's departure timeout (20 s) and its webhook.
pub const REJOIN_WAIT: Duration = Duration::from_millis(30_000);
/// How long the join waits before each new try of an external commit refused 425
/// `E_COMMIT_REQUIRED` (the call group frozen behind an outstanding Remove); a step is lengthened to
/// the instance's `retry_after_ms` hint, never past the schedule's longest step. The schedule and
/// the bound are the Bot's (`cmd/dillabot/music_call_join.go` `retryMusicCallGroupJoin`).
pub const FROZEN_RETRY: [Duration; 3] = [
    Duration::from_millis(250),
    Duration::from_millis(1_000),
    Duration::from_millis(2_000),
];

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
    /// True once the join was cancelled (a leave, sign-out or revocation): nothing more is posted,
    /// and a rejoin's wait stops there.
    fn cancelled(&self) -> bool;
    /// The join's phase moved: [`CallPhase::Joining`] as it starts,
    /// [`CallPhase::WaitingForRemove`] once it waits for this device's earlier Remove (join.ts
    /// `rejoining`: the page says so in its status region), [`CallPhase::In`] when it has joined
    /// and [`CallPhase::Idle`] when it ended without a call.
    fn phase(&self, _phase: CallPhase) {}
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
    /// join.ts `e.users`: the users of the call's room as the last read of the call listed them (deduplicated), the
    /// candidates a refusal may name as its changed contact (ruling I2, review M4).
    pub users: Vec<UserId>,
    /// core-port.ts `CoreError.changedUser`: the leaf of the call tree the core's gate named in an `E_UMK_CHANGED`
    /// refusal (its detail, 32 hex digits), when it named one.
    pub reported: Option<UserId>,
}

/// The changed contact a core refusal names: `E_UMK_CHANGED` with exactly 32 lower-case hex digits as its detail
/// (core-port.ts `CHANGED_USER`).
pub fn reported_user(e: &CoreRefusal) -> Option<UserId> {
    if e.code != "E_UMK_CHANGED" || e.detail.len() != 32 {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        let pair = &e.detail[2 * i..2 * i + 2];
        if !pair
            .bytes()
            .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
        {
            return None;
        }
        *b = u8::from_str_radix(pair, 16).ok()?;
    }
    Some(UserId(out))
}

/// A refusal of the core, with the changed contact it names.
fn core_refused(e: &CoreRefusal, leave: Option<[u8; 16]>) -> JoinRefused {
    JoinRefused {
        reported: reported_user(e),
        ..refused(&e.code, leave)
    }
}

/// The participants of the last call read (join.ts `seen.read`).
type Seen = Mutex<Vec<UserId>>;

fn record(seen: &Seen, read: &ChannelCall) {
    if let Ok(mut s) = seen.lock() {
        s.clear();
        for p in &read.participants {
            if !s.contains(&UserId(p.user_id)) {
                s.push(UserId(p.user_id));
            }
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Phase {
    /// A trust block (the person must compare safety numbers): `E_UMK_CHANGED`, `E_BINDING`, ...
    Blocked,
    Failed,
}

/// The call gate's hard refusals (core-port.ts `TRUST_BLOCKS`).
pub(crate) fn is_trust_block(code: &str) -> bool {
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
        users: Vec::new(),
        reported: None,
    }
}

/// Logs a route of the join that failed at `step`: its HTTP status, `E_*` code, retry hint and transport detail (a
/// status line or a transport error; never a body, a key or a token), before it is mapped to a refusal or retried.
fn route_failed(step: &str, e: &RouteError) {
    log::warn!(
        "calls: join: {step} failed: status {} code {} retry_after_ms {:?}: {}",
        e.status
            .map_or_else(|| "none".to_owned(), |s| s.to_string()),
        e.code.as_deref().unwrap_or("none"),
        e.retry_after_ms,
        e.detail
    );
}

/// Logs a refusal of the core at `step`, by its code only (a core detail is not meant for a log).
fn core_failed(step: &str, e: &CoreRefusal) {
    log::warn!("calls: join: {step} refused by the core: {}", e.code);
}

/// [`route_failed`], then the refusal's code.
fn logged_route(step: &str, e: &RouteError) -> String {
    route_failed(step, e);
    route_code(e)
}

/// [`core_failed`], then the refusal.
fn logged_core(step: &str, e: &CoreRefusal, leave: Option<[u8; 16]>) -> JoinRefused {
    core_failed(step, e);
    core_refused(e, leave)
}

fn route_code(e: &RouteError) -> String {
    e.code.clone().unwrap_or_else(|| {
        e.status
            .map_or("E_NETWORK".into(), |s| format!("E_HTTP_{s}"))
    })
}

impl From<CoreRefusal> for JoinRefused {
    fn from(e: CoreRefusal) -> Self {
        logged_core("core", &e, None)
    }
}

fn stopped(leave: Option<[u8; 16]>) -> JoinRefused {
    refused("E_NO_CALL", leave)
}

/// A refusal of the external join, and whether its cause was the instance's 409
/// `E_REMOVE_PENDING` (join.ts `isHttp(e.cause, 409, 'E_REMOVE_PENDING')`) or 409
/// `E_GROUP_CLOSED` (the call ended as the join was built: its group is closed), or 425
/// `E_COMMIT_REQUIRED` (the group is frozen behind an outstanding proposal: `frozen` carries the
/// instance's retry hint).
struct ExistingRefused {
    refused: JoinRefused,
    remove_pending: bool,
    group_closed: bool,
    frozen: Option<Duration>,
}

impl From<JoinRefused> for ExistingRefused {
    fn from(refused: JoinRefused) -> Self {
        Self {
            refused,
            remove_pending: false,
            group_closed: false,
            frozen: None,
        }
    }
}

impl From<CoreRefusal> for ExistingRefused {
    fn from(e: CoreRefusal) -> Self {
        JoinRefused::from(e).into()
    }
}

/// A refused route of the external join (logged as `step`).
fn route_refused(step: &str, e: &RouteError) -> ExistingRefused {
    route_failed(step, e);
    ExistingRefused {
        refused: refused(&route_code(e), None),
        remove_pending: e.is(409, "E_REMOVE_PENDING"),
        group_closed: e.is(409, "E_GROUP_CLOSED"),
        frozen: e
            .is(425, "E_COMMIT_REQUIRED")
            .then(|| Duration::from_millis(e.retry_after_ms.unwrap_or(0))),
    }
}

/// Joins the channel's call. On success the caller connects `dilla_media::Call` with
/// `started.livekit_url` / `started.token` and publishes `media` (and every later epoch's keys,
/// [`super::media_epoch`]) to it. The phases go to [`JoinHooks::phase`].
pub async fn join_call<C: CallCore, R: CallRoutes + ?Sized, H: JoinHooks + ?Sized>(
    core: &Mutex<C>,
    routes: &R,
    hooks: &H,
    target: &JoinTarget,
) -> Result<Joined, JoinRefused> {
    hooks.phase(CallPhase::Joining);
    let seen = Seen::default();
    let joined = join_loop(core, routes, hooks, target, &seen)
        .await
        .map_err(|mut e| {
            e.users = seen.lock().map(|s| s.clone()).unwrap_or_default();
            log::warn!(
                "calls: join failed: {} ({:?}; leaves a group: {}, alone: {}, cancelled: {})",
                e.code,
                e.phase,
                e.leave.is_some(),
                e.alone,
                hooks.cancelled()
            );
            e
        });
    hooks.phase(if joined.is_ok() {
        CallPhase::In
    } else {
        CallPhase::Idle
    });
    joined
}

async fn join_loop<C: CallCore, R: CallRoutes + ?Sized, H: JoinHooks + ?Sized>(
    core: &Mutex<C>,
    routes: &R,
    hooks: &H,
    target: &JoinTarget,
    seen: &Seen,
) -> Result<Joined, JoinRefused> {
    let mut reread_exists = true;
    let mut reread_closed = true;
    let mut reread_ended = true;
    let mut frozen_tries = 0;
    let mut wait_since: Option<Instant> = None;
    let mut read = reread(routes, target, seen).await?;
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
                    .map_err(|e| {
                        core_failed("call group create", &e);
                        refused(&e.code, None)
                    })?;
                if hooks.cancelled() {
                    let _ = lock(core).map(|mut c| c.group_discard(&group));
                    return Err(stopped(None));
                }
                match routes.post_group(body).await {
                    Ok(next_seq) => {
                        lock(core)?
                            .group_registered(&group, next_seq)
                            .map_err(|e| {
                                core_failed("group registered", &e);
                                refused(&e.code, None)
                            })?;
                        created = true;
                    }
                    Err(e) => {
                        route_failed("register the call group", &e);
                        let _ = lock(core).map(|mut c| c.group_discard(&group));
                        // Another device registered the channel's call group first: read the
                        // call once more and join theirs.
                        if e.is(409, "E_GROUP_EXISTS") && reread_exists {
                            reread_exists = false;
                            read = reread(routes, target, seen).await?;
                            continue;
                        }
                        return Err(refused(&route_code(&e), None));
                    }
                }
            }
            Some(existing) => {
                group = existing;
                let mut tried: Option<Vec<u8>> = None;
                if let Err(e) = join_existing(core, routes, hooks, target, &group, &mut tried).await
                {
                    // The call ended as this join was built (the last member's `DELETE
                    // ?if_alone=1` closed its group): the instance refuses the external commit for
                    // the closed group (409 `E_GROUP_CLOSED`). That group is final, so the call is
                    // read once more and, when it names no group, a fresh one is registered
                    // through the same steps (and the same core gate) as any first joiner's.
                    // Once only: a second refusal fails the join.
                    if e.group_closed && reread_ended && !hooks.cancelled() {
                        log::info!("calls: join: the call group closed; registering a fresh one");
                        reread_ended = false;
                        read = reread(routes, target, seen).await?;
                        continue;
                    }
                    // The group is frozen behind an outstanding proposal (a member left: its
                    // Remove): an online member commits it within the instance's commit deadline,
                    // so the join waits a step of FROZEN_RETRY and tries again on the call as it is
                    // then. The staged join was discarded; the next try reads the moved group.
                    if let Some(hint) = e.frozen.filter(|_| frozen_tries < FROZEN_RETRY.len()) {
                        if hooks.cancelled() {
                            return Err(e.refused);
                        }
                        let longest = FROZEN_RETRY[FROZEN_RETRY.len() - 1];
                        let step = FROZEN_RETRY[frozen_tries].max(hint.min(longest));
                        log::info!(
                            "calls: join: the call group is frozen (425 E_COMMIT_REQUIRED); try {} of {} in {} ms",
                            frozen_tries + 1,
                            FROZEN_RETRY.len(),
                            step.as_millis()
                        );
                        tokio::time::sleep(step).await;
                        frozen_tries += 1;
                        if hooks.cancelled() {
                            return Err(e.refused);
                        }
                        read = reread(routes, target, seen).await?;
                        continue;
                    }
                    let Some(info) = tried.filter(|_| e.remove_pending && !hooks.cancelled())
                    else {
                        return Err(e.refused);
                    };
                    let since = *wait_since.get_or_insert_with(|| {
                        hooks.phase(CallPhase::WaitingForRemove);
                        Instant::now()
                    });
                    read = wait_for_remove(
                        routes, hooks, target, seen, e.refused, since, &group, &info,
                    )
                    .await?;
                    continue;
                }
            }
        }
        let media = join_epoch(core, hooks, &group).await?;
        if hooks.cancelled() {
            return Err(stopped(Some(group)));
        }
        match routes.start_call(&target.channel, &target.vdec).await {
            // A leave (or a dropped join) that arrived while the start was in flight: the call has
            // started with this device admitted, so it leaves at once (its self-Remove, ending the
            // call when alone, and the forget) and the join ends as cancelled. join.ts returns the
            // join here and leaves it to the controller's later leave; this never hands out a call
            // nobody wants.
            Ok(started) if hooks.cancelled() => {
                let alone =
                    created || (media.roster.len() == 1 && media.roster[0].0 == media.own_leaf);
                drop(media);
                super::leave_call(core, routes, &group, &started.call_id, alone).await;
                return Err(stopped(None));
            }
            Ok(started) => {
                return Ok(Joined {
                    group_id: group,
                    media,
                    started,
                });
            }
            Err(e) => {
                route_failed("start the call", &e);
                // The held key is zeroized as `media` drops; nothing more is posted.
                let alone =
                    created || (media.roster.len() == 1 && media.roster[0].0 == media.own_leaf);
                drop(media);
                // L-HTTP-105: the start raced the idle close of the group: it is gone at the
                // instance, so it is forgotten here and the call read once more.
                if e.is(503, "E_UNAVAILABLE") && reread_closed {
                    reread_closed = false;
                    let _ = lock(core).map(|mut c| c.call_group_forget(&group));
                    read = reread(routes, target, seen).await?;
                    continue;
                }
                let mut r = refused(&route_code(&e), Some(group));
                r.alone = alone;
                return Err(r);
            }
        }
    }
}

/// The rejoin's wait ([`REJOIN_WAIT`] from `since`; join.ts `waitForRemove`): each step reads the
/// call, and while it still names the refused group, its GroupInfo; it answers the call read once
/// either has changed (the call ended or moved on, or a commit landed: perhaps the Remove), and
/// fails with `refusal` once the bound has passed or the join is cancelled.
#[allow(
    clippy::too_many_arguments,
    reason = "the wait reads the call as the join does, recording its users (join.ts seen.read)"
)]
async fn wait_for_remove<R: CallRoutes + ?Sized, H: JoinHooks + ?Sized>(
    routes: &R,
    hooks: &H,
    target: &JoinTarget,
    seen: &Seen,
    refusal: JoinRefused,
    since: Instant,
    group: &[u8; 16],
    info: &[u8],
) -> Result<ChannelCall, JoinRefused> {
    loop {
        let left = (since + REJOIN_WAIT).saturating_duration_since(Instant::now());
        if left.is_zero() || hooks.cancelled() {
            return Err(refusal);
        }
        tokio::time::sleep(left.min(REJOIN_STEP)).await;
        if hooks.cancelled() {
            return Err(refusal);
        }
        let read = reread(routes, target, seen).await?;
        // A leave during a read stops the wait there; the join goes no further.
        if hooks.cancelled() {
            return Err(refusal);
        }
        if read.group_id.as_ref() != Some(group) {
            return Ok(read);
        }
        let now = routes
            .get_group_info(group)
            .await
            .map_err(|e| refused(&logged_route("read the GroupInfo (rejoin wait)", &e), None))?;
        if hooks.cancelled() {
            return Err(refusal);
        }
        if now != info {
            return Ok(read);
        }
    }
}

async fn reread<R: CallRoutes + ?Sized>(
    routes: &R,
    target: &JoinTarget,
    seen: &Seen,
) -> Result<ChannelCall, JoinRefused> {
    let read = routes
        .get_channel_call(&target.channel)
        .await
        .map_err(|e| refused(&logged_route("read the channel's call", &e), None))?;
    record(seen, &read);
    Ok(read)
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
    tried: &mut Option<Vec<u8>>,
) -> Result<(), ExistingRefused> {
    for reads in 1..=JOIN_READS_MAX {
        let again = reads < JOIN_READS_MAX && !hooks.cancelled();
        let info = routes
            .get_group_info(group)
            .await
            .map_err(|e| route_refused("read the GroupInfo", &e))?;
        // The GroupInfo the commit is built on: the rejoin's wait compares it.
        *tried = Some(info.clone());
        let tree = routes
            .get_group_tree(group)
            .await
            .map_err(|e| route_refused("read the group tree", &e))?;
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
            Err(e) if e.code == "E_CORE_INPUT" && again => {
                log::info!("calls: join: the GroupInfo and tree disagree; reading them again");
                continue;
            }
            Err(e) => return Err(logged_core("external join", &e, None).into()),
        };
        let checked = check(core, hooks, group).await;
        if let Err(e) = checked {
            discard(core, group);
            return Err(logged_core("join check", &e, None).into());
        }
        if hooks.cancelled() {
            discard(core, group);
            return Err(stopped(None).into());
        }
        match routes.post_resync(group, body.to_vec()).await {
            Ok(seq) => {
                // The instance accepted the commit: this device is a member now, so a failure
                // from here on names the group to leave (its self-Remove and a forget).
                lock(core)
                    .and_then(|mut c| c.group_joined(group, seq))
                    .map_err(|e| {
                        core_failed("group joined", &e);
                        ExistingRefused::from(refused(&e.code, Some(*group)))
                    })?;
                return Ok(());
            }
            Err(e) => {
                discard(core, group);
                // 409 `E_GROUP_CLOSED` is not stale: the group is final, and `join_loop`
                // registers a fresh one.
                let stale = e.is(422, "E_COMMIT_INVALID") || e.is(409, "E_COMMIT_CONFLICT");
                if again && stale && moved_since(routes, group, &info).await {
                    route_failed("post the join commit (stale, trying again)", &e);
                    continue;
                }
                return Err(route_refused("post the join commit", &e));
            }
        }
    }
    Err(refused("E_CORE_INPUT", None).into())
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
        Err(e) => return Err(logged_core("join epoch keys", &e, Some(*group))),
    }
    hooks.refresh_leaves(group).await;
    media_epoch(core, group).map_err(|e| logged_core("join epoch keys", &e, Some(*group)))
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
