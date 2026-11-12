//! The call signalling of a running [`Client`]: [`Client::calls`] wires this crate's call steps
//! ([`super::join_call`], [`super::leave_call`], [`super::rotate_call`], [`super::decline_ring`],
//! [`super::media_epoch`]) to the client's own store core and to its gateway socket.
//!
//! Every call route is one the instance serves only as a gateway request frame (socket-routes.ts
//! `SOCKET_ROUTES`), so the routes go through [`SocketHttp`]: while the socket is up they are
//! written on it, while it reconnects they wait for it, and none of them ever falls back to HTTP.
//! The HTTP transport `H` is reached only by a route the table names `Http`, and no call route is
//! one ([`Client::calls`] passes [`NoHttp`], which refuses every request).
//!
//! The steps run on the client's runtime; their answers come on a oneshot, as the rest of the
//! client's commands do.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use tokio::sync::{oneshot, watch};

use super::controller::{Left, RotateError, decline_ring};
use super::{
    CallPhase, CoreRefusal, HttpRoutes, JoinHooks, JoinRefused, JoinTarget, Joined, MediaEpoch,
    RouteError, join_call, leave_call, media_epoch, rotate_call,
};
use crate::client::{Client, ClientError, Inner};
use crate::seams::{Http, HttpError, Route};
use crate::sync::Engine;
use crate::sync::requests::SocketHttp;

/// controller.ts `WAIT_LIST_MS`: the bound on the join's trust refresh of a group's leaves.
pub const WAIT_LIST: Duration = Duration::from_millis(10_000);

/// An [`Http`] that refuses every request: the call stack's HTTP side, which no call route reaches.
#[derive(Clone, Copy, Debug, Default)]
pub struct NoHttp;

#[async_trait::async_trait]
impl Http for NoHttp {
    async fn call<R: Route>(
        &self,
        _path_args: &[&str],
        _req: R::Request,
    ) -> Result<R::Response, HttpError> {
        Err(HttpError::Transport(format!(
            "{} {} has no HTTP form in the call stack",
            R::METHOD,
            R::PATH
        )))
    }
}

/// The routes a [`Calls`] sends: over the client's gateway socket, `H` for HTTP-only rows.
pub type SocketCallRoutes<H> = HttpRoutes<SocketHttp<H>>;

/// The call signalling of one [`Client`] (see the module docs).
pub struct Calls<H = NoHttp> {
    inner: Arc<Inner>,
    routes: Arc<SocketCallRoutes<H>>,
}

impl<H> Clone for Calls<H> {
    fn clone(&self) -> Self {
        Self {
            inner: self.inner.clone(),
            routes: self.routes.clone(),
        }
    }
}

impl Client {
    /// The client's call signalling, over its gateway socket.
    pub fn calls(&self) -> Calls<NoHttp> {
        self.calls_with(NoHttp)
    }

    /// [`Client::calls`] with `http` for any route the table serves over HTTP (none of the call
    /// routes is one: a test passes a counting double to show it).
    pub fn calls_with<H: Http + 'static>(&self, http: H) -> Calls<H> {
        Calls {
            routes: Arc::new(HttpRoutes(SocketHttp::new(
                self.inner().socket_requests(),
                http,
            ))),
            inner: self.inner().clone(),
        }
    }
}

/// The call to join.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CallTarget {
    pub channel: [u8; 16],
    /// `None` for a DM's call.
    pub community: Option<[u8; 16]>,
    /// The decoders this device names (`vdec`), or empty.
    pub vdec: String,
}

/// A join in progress: its phase as it moves ([`CallPhase`]), a cancel, and its outcome. Dropping it before
/// [`CallJoin::outcome`] cancels the join: nobody is left to take the call it would start.
pub struct CallJoin {
    phase: watch::Receiver<CallPhase>,
    cancel: Arc<AtomicBool>,
    outcome: Option<oneshot::Receiver<Result<Result<Joined, JoinRefused>, ClientError>>>,
}

impl Drop for CallJoin {
    fn drop(&mut self) {
        if self.outcome.is_some() {
            self.cancel.store(true, Ordering::Release);
        }
    }
}

impl CallJoin {
    /// The join's phase: [`CallPhase::WaitingForRemove`] while a rejoin waits for this device's
    /// earlier Remove.
    pub fn phase(&self) -> watch::Receiver<CallPhase> {
        self.phase.clone()
    }

    /// Cancels the join (a leave, sign-out or revocation): nothing more is posted, and a wait stops.
    pub fn cancel(&self) {
        self.cancel.store(true, Ordering::Release);
    }

    /// The join's outcome. A client that stopped meanwhile is the outer error.
    pub async fn outcome(mut self) -> Result<Result<Joined, JoinRefused>, ClientError> {
        let Some(outcome) = self.outcome.take() else {
            return Err(ClientError::Stopped);
        };
        outcome.await.unwrap_or(Err(ClientError::Stopped))
    }
}

struct Hooks {
    engine: Arc<Engine>,
    cancel: Arc<AtomicBool>,
    phase: watch::Sender<CallPhase>,
}

#[async_trait::async_trait]
impl JoinHooks for Hooks {
    fn random_id(&self) -> [u8; 16] {
        let mut id = [0u8; 16];
        // No OS randomness is no call group: never a predictable id.
        getrandom::fill(&mut id).expect("the OS CSPRNG is available");
        id
    }

    async fn refresh_leaves(&self, _group: &[u8; 16]) {
        let _ = tokio::time::timeout(WAIT_LIST, self.engine.refresh_trust()).await;
    }

    fn cancelled(&self) -> bool {
        self.cancel.load(Ordering::Acquire) || self.engine.stopped()
    }

    fn phase(&self, phase: CallPhase) {
        self.phase.send_replace(phase);
    }
}

impl<H: Http + 'static> Calls<H> {
    /// The routes these calls send (each over the socket).
    pub fn routes(&self) -> &SocketCallRoutes<H> {
        &self.routes
    }

    /// Joins the channel's call ([`join_call`]), with the instance's policy version and external
    /// sender from the client's start.
    pub fn join(&self, target: CallTarget) -> CallJoin {
        let (phase_tx, phase) = watch::channel(CallPhase::Idle);
        let cancel = Arc::new(AtomicBool::new(false));
        let engine = self.inner.engine().clone();
        let instance = engine.instance();
        let target = JoinTarget {
            channel: target.channel,
            community: target.community,
            vdec: target.vdec,
            policy_version: instance.policy_version,
            external_sender_pub: instance.external_sender_pub,
        };
        let hooks = Hooks {
            engine: engine.clone(),
            cancel: cancel.clone(),
            phase: phase_tx,
        };
        let routes = self.routes.clone();
        let outcome = self.inner.spawn(async move {
            let core = engine.core().clone();
            let joined = join_call(&*core, &*routes, &hooks, &target).await;
            // A cancelled join (a leave, or the CallJoin dropped) that already holds a leaf of a group: nobody will
            // take this refusal and leave it, so its self-Remove and the forget happen here.
            if let Err(r) = &joined
                && let Some(group) = r.leave
                && hooks.cancel.load(Ordering::Acquire)
            {
                leave_call(&*core, &*routes, &group, &[0; 16], false).await;
            }
            Ok(joined)
        });
        CallJoin {
            phase,
            cancel,
            outcome: Some(outcome),
        }
    }

    /// The call group's current epoch keys through the core's call gate ([`media_epoch`]).
    pub fn media_epoch(&self, group: &[u8; 16]) -> Result<MediaEpoch, CoreRefusal> {
        media_epoch(&**self.inner.engine().core(), group)
    }

    /// Leaves the call ([`leave_call`]).
    pub fn leave(
        &self,
        group: [u8; 16],
        call: [u8; 16],
        alone: bool,
    ) -> oneshot::Receiver<Result<Left, ClientError>> {
        let core = self.inner.engine().core().clone();
        let routes = self.routes.clone();
        self.inner
            .spawn(async move { Ok(leave_call(&*core, &*routes, &group, &call, alone).await) })
    }

    /// The hourly rotation ([`rotate_call`]).
    pub fn rotate(
        &self,
        group: [u8; 16],
    ) -> oneshot::Receiver<Result<Result<u64, RotateError>, ClientError>> {
        let core = self.inner.engine().core().clone();
        let routes = self.routes.clone();
        self.inner
            .spawn(async move { Ok(rotate_call(&*core, &*routes, &group).await) })
    }

    /// Declines a ring ([`decline_ring`]).
    pub fn decline(
        &self,
        call: [u8; 16],
        ring_ts: u64,
    ) -> oneshot::Receiver<Result<Result<(), RouteError>, ClientError>> {
        let routes = self.routes.clone();
        self.inner
            .spawn(async move { Ok(decline_ring(&*routes, &call, ring_ts).await) })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    type Outcome = oneshot::Sender<Result<Result<Joined, JoinRefused>, ClientError>>;

    fn join() -> (CallJoin, Arc<AtomicBool>, Outcome) {
        let (_phase_tx, phase) = watch::channel(CallPhase::Joining);
        let cancel = Arc::new(AtomicBool::new(false));
        let (tx, rx) = oneshot::channel();
        (
            CallJoin {
                phase,
                cancel: cancel.clone(),
                outcome: Some(rx),
            },
            cancel,
            tx,
        )
    }

    #[test]
    fn dropping_a_join_cancels_it() {
        let (j, cancel, _tx) = join();
        drop(j);
        assert!(cancel.load(Ordering::Acquire));
    }

    #[tokio::test]
    async fn a_join_whose_outcome_was_taken_is_not_cancelled_by_its_drop() {
        let (j, cancel, tx) = join();
        let _ = tx.send(Err(ClientError::Stopped));
        assert!(j.outcome().await.is_err());
        assert!(!cancel.load(Ordering::Acquire));
    }
}
