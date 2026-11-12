//! The live call's group operations (controller.ts steps 4 and 5): each epoch's media keys, the
//! leave, the hourly rotation, and declining a ring (ring.ts).

use std::sync::Mutex;
use std::time::Duration;

use tokio::time::Instant;

use super::wire::decode_media_epoch;
use super::{CallCore, CallRoutes, CoreRefusal, MediaEpoch, RouteError, lock};

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
    /// The self-Remove was posted (false: refused, e.g. `E_REMOVE_PENDING`, or nothing to post).
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
        Ok(body) => routes.post_proposal(group, body).await.is_ok(),
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
/// pending commit is aborted. Well inside the instance's 5-minute replay record.
pub const ROTATE_RECONCILE: Duration = Duration::from_secs(60);
/// The first wait before a re-write; it doubles.
const ROTATE_BACKOFF: Duration = Duration::from_secs(1);

fn is_outcome_unknown(e: &RouteError) -> bool {
    e.code.as_deref() == Some("E_OUTCOME_UNKNOWN")
}

/// The hourly rotation (controller.ts `rotate`): an empty commit with a fresh leaf secret. A
/// refused post aborts the pending commit; an accepted one is confirmed, and the caller then reads
/// the new epoch's keys ([`media_epoch`]). Must not run once a leave has started.
///
/// A write whose answer was lost (`E_OUTCOME_UNKNOWN`: the instance may or may not have applied
/// it) is reconciled, never left pending (a pending commit blocks every later one of the group): the
/// same commit is written again with the same idempotency key, so the instance's replay record
/// answers with the first write's outcome (applied: its seq) or applies it now; either way it is
/// confirmed. At most [`ROTATE_WRITES`] writes, backed off, within [`ROTATE_RECONCILE`] of the first
/// loss; past that, or on any refusal, the pending commit is aborted. controller.ts aborts at once
/// on any failure.
pub async fn rotate_call<C: CallCore, R: CallRoutes + ?Sized>(
    core: &Mutex<C>,
    routes: &R,
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
            Err(e) if is_outcome_unknown(&e) && write < ROTATE_WRITES => {
                let at = *lost_at.get_or_insert_with(Instant::now);
                if at.elapsed() + backoff >= ROTATE_RECONCILE {
                    return abort(e);
                }
                tokio::time::sleep(backoff).await;
                backoff *= 2;
            }
            Err(e) => return abort(e),
        }
    }
    unreachable!("the last write returns")
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
