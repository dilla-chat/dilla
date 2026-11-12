//! The live call's group operations (controller.ts steps 4 and 5): each epoch's media keys, the
//! leave, the hourly rotation, and declining a ring (ring.ts).

use std::sync::Mutex;

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

/// The hourly rotation (controller.ts `rotate`): an empty commit with a fresh leaf secret. A
/// refused post aborts the pending commit; an accepted one is confirmed, and the caller then reads
/// the new epoch's keys ([`media_epoch`]). Must not run once a leave has started.
pub async fn rotate_call<C: CallCore, R: CallRoutes + ?Sized>(
    core: &Mutex<C>,
    routes: &R,
    group: &[u8; 16],
) -> Result<u64, RotateError> {
    let body = lock(core)?.call_rotate(group)?;
    match routes.post_commit(group, body).await {
        Ok(seq) => {
            // The instance accepted it: the pending commit is kept even if the confirm fails (the
            // engine adopts it when it arrives), so only the confirm's result is reported.
            lock(core)?.commit_confirm(group)?;
            Ok(seq)
        }
        Err(e) => {
            let _ = lock(core).map(|mut c| c.commit_abort(group));
            Err(e.into())
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
