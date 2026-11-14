//! The live call's group operations (controller.ts steps 4 and 5): each epoch's media keys, the
//! leave, the hourly rotation, and declining a ring (ring.ts).

use std::sync::Mutex;
use std::time::Duration;

use tokio::time::Instant;

use super::wire::{GroupSummary, decode_commit_body, decode_media_epoch};
use super::{CallCatchUp, CallCore, CallRoutes, CoreRefusal, MediaEpoch, RouteError, lock};

/// The call group's current epoch keys through the core's call gate (`call_media_epoch`): a
/// refusal (`E_DEVICE_UNLISTED`, `E_UMK_CHANGED`, `E_DEVICE_LIST_STALE`, ...) derives no key. The
/// caller hands the result to the call's media (`dilla_media::EpochFeed::publish`) after every
/// commit the call group applies.
pub fn media_epoch<C: CallCore>(
    core: &Mutex<C>,
    group: &[u8; 16],
) -> Result<MediaEpoch, CoreRefusal> {
    let body = lock(core)?.call_media_epoch(group)?;
    decode_media_epoch(&body).map_err(|detail| CoreRefusal {
        code: "E_CBOR_SHAPE".into(),
        detail,
    })
}

/// How a leave went.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Left {
    /// This device was alone and the instance ended the call.
    pub ended: bool,
    /// The instance acknowledged the self-Remove: it took it, or answered 409 `E_REMOVE_PENDING` (a Remove of this
    /// leaf stands there already). False: refused otherwise, its answer lost, or nothing to post.
    pub proposed: bool,
}

/// Leaves the call (controller.ts `leaveLive`): when `alone` (the last keyed roster held only this
/// device and the room's snapshot shows no other) the call is ended conditionally first
/// (`DELETE /v1/calls/{id}?if_alone=1`: a device that has just joined keeps it); then the
/// self-Remove is proposed (another member commits it) and the group is forgotten. Never a rotation
/// after this: `call_rotate` would cancel the posted Remove. Failures of the routes are tolerated
/// as the TypeScript controller tolerates them; the group is forgotten either way.
pub async fn leave_call<C: CallCore, R: CallRoutes + ?Sized>(
    core: &Mutex<C>,
    routes: &R,
    group: &[u8; 16],
    call: &[u8; 16],
    alone: bool,
) -> Left {
    let mut ended = false;
    if alone {
        ended = routes.end_call_if_alone(call).await.unwrap_or(false);
    }
    let proposal = lock(core).and_then(|mut c| c.call_leave_proposal(group));
    let proposed = match proposal {
        Ok(body) => match routes.post_proposal(group, body).await {
            Ok(_) => true,
            Err(e) => e.is(409, "E_REMOVE_PENDING"),
        },
        Err(_) => false,
    };
    let _ = lock(core).map(|mut c| c.call_group_forget(group));
    Left { ended, proposed }
}

/// Why a rotation did not land.
#[derive(Debug, thiserror::Error)]
pub enum RotateError {
    #[error("the core refused the rotation: {0}")]
    Core(#[from] CoreRefusal),
    #[error("the instance refused the rotation: {0}")]
    Route(#[from] RouteError),
}

/// How many times one rotation commit is written in all: the first write and its re-writes after
/// lost answers.
pub const ROTATE_WRITES: u32 = 3;
/// The bound on reconciling a rotation whose answer was lost, from the first loss: past it the
/// pending commit is aborted. Well inside the instance's 5-minute replay record. The read and the
/// catch-up after a re-write refused for its epoch ([`rotate_call`]) have bounds of this length of
/// their own.
pub const ROTATE_RECONCILE: Duration = Duration::from_secs(60);
/// The first wait before a re-write; it doubles.
const ROTATE_BACKOFF: Duration = Duration::from_secs(1);
/// The handshake rows one read of the reconciliation asks for, and the most pages it reads.
pub const RECONCILE_PAGE: u64 = 100;
const RECONCILE_PAGES: u32 = 50;

fn is_outcome_unknown(e: &RouteError) -> bool {
    e.code.as_deref() == Some("E_OUTCOME_UNKNOWN")
}

/// After a lost answer, an answer to a re-write that says nothing about the first write: the
/// instance's replay record keeps neither a 5xx nor a 429 (`socket_dispatch.go`), answers 503
/// `E_UNAVAILABLE` while the first write still runs, and an upgraded instance can answer 401 or
/// 426 before any handler. The re-write is written again, as after a lost answer.
fn says_nothing(e: &RouteError) -> bool {
    matches!(e.status, Some(s) if s >= 500 || s == 429 || s == 401 || s == 426)
}

/// The DS's one-commit-per-epoch refusal (`commitPreflight` step 2): the instance's epoch has moved
/// past the one the commit was built for, so SOME commit took that epoch. After a lost answer that
/// commit may be this one: the instance applied the first write, and the re-write ran the handler
/// again because the replay record was gone (an instance restart, or a first write minutes old).
fn is_epoch_taken(e: &RouteError) -> bool {
    e.is(409, "E_COMMIT_CONFLICT")
}

/// This device's row of the group.
fn row_of<C: CallCore>(core: &Mutex<C>, group: &[u8; 16]) -> Option<GroupSummary> {
    let rows = lock(core).ok()?.groups().ok()?;
    rows.into_iter().find(|r| r.group_id == *group)
}

/// controller.ts `onGroupChanged`: whether this device's own leaf is gone from the call group (state 4: another member
/// committed its Remove, as after a voice takeover) or the group's state is lost (3: a call group is never resynced).
/// Either ends the call as removed, with no self-Remove. A group this core does not hold is not.
pub fn own_leaf_gone<C: CallCore>(core: &Mutex<C>, group: &[u8; 16]) -> bool {
    row_of(core, group).is_some_and(|r| r.state == 3 || r.state == 4)
}

/// The hourly rotation (controller.ts `rotate`): an empty commit with a fresh leaf secret. A
/// refused post aborts the pending commit; an accepted one is confirmed, and the caller then reads
/// the new epoch's keys ([`media_epoch`]). Must not run once a leave has started.
///
/// A write whose answer was lost (`E_OUTCOME_UNKNOWN`: the instance may or may not have applied
/// it) is reconciled, never left pending blindly (a pending commit blocks every later one of the
/// group): the same commit is written again with the same idempotency key, so the instance's replay
/// record answers with the first write's outcome (applied: its seq) or applies it now; either way it
/// is confirmed. At most [`ROTATE_WRITES`] writes, backed off, within [`ROTATE_RECONCILE`] of the
/// first loss; past that the pending commit is aborted.
///
/// A re-write refused 409 `E_COMMIT_CONFLICT` is NOT proof that the commit was not applied: when the
/// replay record is gone (an instance restart, or a first write minutes old: exactly when requests
/// raise `E_OUTCOME_UNKNOWN`) the handler runs again and refuses the epoch the first write took.
/// Aborting then would leave the instance holding a commit the core has dropped: the core cannot
/// process its own commit when it arrives and the group needs a resync (dilla-core's
/// `an_aborted_rotation_the_instance_applied_needs_a_resync`). So the pending commit is kept and the
/// group is caught up from its cursor ([`CallCatchUp`]), which settles it ([`reconcile`]). An
/// answer to a re-write that says nothing about the first write ([`says_nothing`]: 5xx, 429, 401,
/// 426) is treated as a lost answer, within the same bound. Any other refusal of a re-write aborts
/// at once, as controller.ts does on any failure: the DS's later checks run only once the epoch
/// check passed (the instance is still at the commit's epoch, so it never applied it), and its
/// earlier ones find the group gone or this device without a current leaf in it (nothing left to
/// rotate).
pub async fn rotate_call<C: CallCore, R: CallRoutes + ?Sized, S: CallCatchUp + ?Sized>(
    core: &Mutex<C>,
    routes: &R,
    sync: &S,
    group: &[u8; 16],
) -> Result<u64, RotateError> {
    let mut idem = [0u8; 16];
    if getrandom::fill(&mut idem).is_err() {
        return Err(RouteError {
            status: None,
            code: Some("E_RANDOM".into()),
            detail: "no OS randomness for an idempotency key".into(),
        }
        .into());
    }
    // The cursor before the commit is built: every commit of the group before it is applied, so the
    // first commit the instance holds at or after it is the one that takes this commit's epoch.
    let cursor = row_of(core, group).map(|r| r.next_seq);
    let body = lock(core)?.call_rotate(group)?;
    let abort = |e: RouteError| {
        let _ = lock(core).map(|mut c| c.commit_abort(group));
        Err(e.into())
    };
    let mut lost_at: Option<Instant> = None;
    let mut backoff = ROTATE_BACKOFF;
    for write in 1..=ROTATE_WRITES {
        let posted = match lost_at {
            None => routes.post_commit(group, body.clone(), idem).await,
            Some(at) => {
                let left = ROTATE_RECONCILE.saturating_sub(at.elapsed());
                tokio::time::timeout(left, routes.post_commit(group, body.clone(), idem))
                    .await
                    .unwrap_or_else(|_| {
                        Err(RouteError {
                            status: None,
                            code: Some("E_OUTCOME_UNKNOWN".into()),
                            detail: "the rotation was not reconciled in time".into(),
                        })
                    })
            }
        };
        match posted {
            Ok(seq) => {
                // The instance accepted it: the pending commit is kept even if the confirm fails
                // (the engine adopts it when it arrives), so only the confirm's result is reported.
                lock(core)?.commit_confirm(group)?;
                return Ok(seq);
            }
            Err(e)
                if (is_outcome_unknown(&e) || lost_at.is_some() && says_nothing(&e))
                    && write < ROTATE_WRITES =>
            {
                let at = *lost_at.get_or_insert_with(Instant::now);
                if at.elapsed() + backoff >= ROTATE_RECONCILE {
                    return abort(e);
                }
                tokio::time::sleep(backoff).await;
                backoff *= 2;
            }
            Err(e) if lost_at.is_some() && is_epoch_taken(&e) => {
                return reconcile(core, routes, sync, group, &body, cursor, e).await;
            }
            Err(e) => return abort(e),
        }
    }
    unreachable!("the last write returns")
}

/// The commit that took epoch `epoch` (its seq and blob): the commit row of epoch `epoch + 1` the
/// instance holds at or after `from`, read page by page; `None` when it holds none. Rows of earlier
/// epochs are skipped: the cursor does not move past a commit this device confirmed from its
/// answer, so its own earlier rotation can come first.
async fn commit_that_took<R: CallRoutes + ?Sized>(
    routes: &R,
    group: &[u8; 16],
    mut from: u64,
    epoch: u64,
) -> Result<Option<(u64, Vec<u8>)>, RouteError> {
    let opened = epoch + 1;
    for _ in 0..RECONCILE_PAGES {
        let page = routes.get_handshakes(group, from, RECONCILE_PAGE).await?;
        if let Some((seq, row_epoch, blob)) = page.commits.into_iter().find(|c| c.1 >= opened) {
            if row_epoch > opened {
                // Its row is behind the cursor: not this device's to judge from here.
                return Err(RouteError {
                    status: None,
                    code: Some("E_OUTCOME_UNKNOWN".into()),
                    detail: "the commit that took the epoch is not after the cursor".into(),
                });
            }
            return Ok(Some((seq, blob)));
        }
        match page.last {
            Some(last) if page.rows as u64 >= RECONCILE_PAGE => from = last + 1,
            _ => return Ok(None),
        }
    }
    Err(RouteError {
        status: None,
        code: Some("E_OUTCOME_UNKNOWN".into()),
        detail: "no commit within the pages read".into(),
    })
}

/// A rotation whose re-write after a lost answer was refused for its epoch ([`is_epoch_taken`]).
/// The pending commit is kept, the instance's handshakes from the cursor the commit was built at say
/// whose commit took the epoch, and the group's catch-up settles the core:
///
/// - this commit: the catch-up adopts it as the pending commit (or, when the catch-up did not reach
///   it, it is confirmed: the instance holds it), and the rotation is done at its seq;
/// - another member's commit: the catch-up applies it, which clears the pending one (or, when the
///   catch-up did not reach it, the pending commit is aborted: its epoch is taken), and the refusal
///   is the answer;
/// - no commit after the cursor: the instance never applied this one, and it is aborted.
///
/// A read that fails before the catch-up is tried once more after it. When both fail, whose commit
/// took the epoch is not known and the answer is `E_OUTCOME_UNKNOWN`: a pending commit the catch-up
/// left stays pending (the refusal proved that a commit took the epoch, so the engine's next
/// catch-up adopts or clears it), and a group the catch-up settled is at a new epoch either way, so
/// the caller re-reads [`media_epoch`] after any rotation outcome, as after every commit. A group
/// the catch-up left inactive (lost, or needing a rejoin) has nothing to rotate: a pending commit of
/// it is aborted.
async fn reconcile<C: CallCore, R: CallRoutes + ?Sized, S: CallCatchUp + ?Sized>(
    core: &Mutex<C>,
    routes: &R,
    sync: &S,
    group: &[u8; 16],
    body: &[u8],
    cursor: Option<u64>,
    refused: RouteError,
) -> Result<u64, RotateError> {
    let read = || async {
        let (epoch, commit) = decode_commit_body(body).ok()?;
        let from = cursor?;
        tokio::time::timeout(
            ROTATE_RECONCILE,
            commit_that_took(routes, group, from, epoch),
        )
        .await
        .ok()?
        .ok()
        .map(|took| took.map(|(seq, blob)| (seq, blob == commit)))
    };
    let first = read().await;
    let _ = tokio::time::timeout(ROTATE_RECONCILE, sync.catch_up(group)).await;
    let took = match first {
        Some(took) => Some(took),
        None => read().await,
    };
    let row = row_of(core, group);
    let active = row.as_ref().is_some_and(|r| r.state == 2);
    let pending = row.as_ref().is_some_and(|r| r.pending_commit);
    let abort = || {
        let _ = lock(core).map(|mut c| c.commit_abort(group));
    };
    match took {
        Some(Some((seq, true))) if active => {
            if pending {
                lock(core)?.commit_confirm(group)?;
            }
            Ok(seq)
        }
        None if active => Err(RouteError {
            status: None,
            code: Some("E_OUTCOME_UNKNOWN".into()),
            detail: if pending {
                "the rotation's epoch was taken; the next catch-up settles it".into()
            } else {
                "the rotation's epoch was taken and settled; whose commit took it is not known"
                    .into()
            },
        }
        .into()),
        _ => {
            if pending {
                abort();
            }
            Err(refused.into())
        }
    }
}

/// Declines a ring (ring.ts): the body echoes the ring's `expires_ts`, so a late decline never stops
/// a later ring of the same DM.
pub async fn decline_ring<R: CallRoutes + ?Sized>(
    routes: &R,
    call: &[u8; 16],
    ring_ts: u64,
) -> Result<(), RouteError> {
    routes.decline_call(call, ring_ts).await
}
