//! The call steps over a scripted gateway socket ([`crate::sync::requests::script`]): what they write, and what they
//! do with the instance's answers.

use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};

use dilla_core::cbor::Encoder;
use zeroize::Zeroizing;

use super::session::{NoHttp, leave_if_cancelled};
use super::wire::decode_commit_body;
use super::*;
use crate::sync::api::ApiError;
use crate::sync::requests::script::{Script, refusal, response, up};

const GROUP: [u8; 16] = [0x44; 16];
const CALL: [u8; 16] = [0x33; 16];

/// A core that logs the steps a rotation and a leave take, and models the call group's state as dilla-core's
/// `ClientCore` keeps it through a rotation: its epoch, cursor, pending commit and the epoch's media key (here: the
/// commit that took the epoch, which every member derives alike). How a catch-up treats each commit ([`Core::apply`])
/// is what dilla-core's `a_kept_rotation_is_settled_by_catch_up_whichever_commit_won` and
/// `an_aborted_rotation_the_instance_applied_needs_a_resync` establish against the real core.
struct Core {
    log: Vec<String>,
    state: u64,
    epoch: u64,
    next_seq: u64,
    pending: Option<Vec<u8>>,
    /// Every commit this core built: an own commit that arrives with nothing pending cannot be processed.
    built: Vec<Vec<u8>>,
    key: Vec<u8>,
}

impl Default for Core {
    fn default() -> Self {
        Self {
            log: Vec::new(),
            state: 2,
            epoch: 1,
            next_seq: 7,
            pending: None,
            built: Vec::new(),
            key: b"join".to_vec(),
        }
    }
}

impl Core {
    /// One commit row of a catch-up: `(seq, the epoch it was committed from, blob)`.
    fn apply(&mut self, (seq, from_epoch, blob): &(u64, u64, Vec<u8>)) {
        if *seq < self.next_seq || self.state != 2 {
            return;
        }
        if *from_epoch < self.epoch {
            // An epoch this core already holds (its own commit confirmed from the answer): skipped.
        } else if self.pending.as_ref() == Some(blob) {
            self.log.push("adopt".into());
            self.pending = None;
            self.epoch += 1;
            self.key = blob.clone();
        } else if self.built.contains(blob) {
            // Its own commit, aborted: the core cannot process it; the group needs a resync.
            self.log.push("needs resync".into());
            self.state = 3;
            return;
        } else {
            if self.pending.take().is_some() {
                self.log.push("clear".into());
            }
            self.log.push("apply".into());
            self.epoch += 1;
            self.key = blob.clone();
        }
        self.next_seq = seq + 1;
    }
}

impl CallCore for Core {
    fn call_group_create(
        &mut self,
        _: &[u8; 16],
        _: Option<&[u8; 16]>,
        _: &[u8; 16],
        _: u64,
        _: &[u8; 32],
    ) -> Result<Vec<u8>, CoreRefusal> {
        unreachable!()
    }
    fn call_group_join_external(
        &mut self,
        _: &[u8; 16],
        _: Option<&[u8; 16]>,
        _: &[u8; 16],
        _: u64,
        _: &[u8],
        _: &[u8],
    ) -> Result<Vec<u8>, CoreRefusal> {
        unreachable!()
    }
    fn call_join_check(&self, _: &[u8; 16]) -> Result<(), CoreRefusal> {
        unreachable!()
    }
    fn call_media_epoch(&self, _: &[u8; 16]) -> Result<Zeroizing<Vec<u8>>, CoreRefusal> {
        unreachable!()
    }
    fn call_leave_proposal(&mut self, _: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        self.log.push("leave_proposal".into());
        Ok(vec![0x80])
    }
    fn call_rotate(&mut self, _: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        if self.pending.is_some() || self.state != 2 {
            return Err(CoreRefusal {
                code: "E_CORE_STATE".into(),
                detail: "a commit is pending".into(),
            });
        }
        self.log.push("rotate".into());
        let commit =
            format!("rotation {} of epoch {}", self.built.len() + 1, self.epoch).into_bytes();
        self.built.push(commit.clone());
        self.pending = Some(commit.clone());
        let mut e = Encoder::new();
        e.array(5).uint(self.epoch).bytes(&commit).bytes(b"info");
        e.array(0);
        e.null();
        Ok(e.into_vec())
    }
    fn call_group_forget(&mut self, _: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.log.push("forget".into());
        Ok(())
    }
    fn group_registered(&mut self, _: &[u8; 16], _: u64) -> Result<(), CoreRefusal> {
        unreachable!()
    }
    fn group_joined(&mut self, _: &[u8; 16], _: u64) -> Result<(), CoreRefusal> {
        unreachable!()
    }
    fn group_discard(&mut self, _: &[u8; 16]) -> Result<(), CoreRefusal> {
        unreachable!()
    }
    fn commit_confirm(&mut self, _: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.log.push("confirm".into());
        if let Some(commit) = self.pending.take() {
            self.epoch += 1;
            self.key = commit;
        }
        Ok(())
    }
    fn commit_abort(&mut self, _: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.log.push("abort".into());
        self.pending = None;
        Ok(())
    }
    fn groups(&self) -> Result<Vec<GroupSummary>, CoreRefusal> {
        Ok(vec![GroupSummary {
            group_id: GROUP,
            kind: 1,
            target_id: CALL,
            state: self.state,
            epoch: self.epoch,
            next_seq: self.next_seq,
            pending_commit: self.pending.is_some(),
        }])
    }
}

/// The commits the instance holds for the group: `(seq, the epoch it was committed from, blob)`.
type Held = Arc<Mutex<Vec<(u64, u64, Vec<u8>)>>>;

/// The sync engine's catch-up over the model: the core applies every commit the instance holds from its cursor on.
/// While `reachable` is false the catch-up's read fails and nothing is applied.
struct CatchUp {
    core: Arc<Mutex<Core>>,
    held: Held,
    reachable: AtomicBool,
    runs: AtomicUsize,
}

#[async_trait::async_trait]
impl CallCatchUp for CatchUp {
    async fn catch_up(&self, _group: &[u8; 16]) {
        self.runs.fetch_add(1, Ordering::SeqCst);
        if !self.reachable.load(Ordering::SeqCst) {
            return;
        }
        let mut core = self.core.lock().unwrap();
        for row in self.held.lock().unwrap().iter() {
            core.apply(row);
        }
    }
}

fn seq_answer(seq: u64) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(2).uint(seq).uint(5);
    e.into_vec()
}

/// A `GET /v1/groups/{id}/handshakes` page of commits `[seq, epoch, 1, null, blob]` (the row's epoch is the one the
/// commit opened).
fn handshake_page(rows: &[(u64, u64, Vec<u8>)]) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(rows.len());
    for (seq, from, blob) in rows {
        e.array(5)
            .uint(*seq)
            .uint(from + 1)
            .uint(1)
            .null()
            .bytes(blob);
    }
    e.into_vec()
}

type Routes = SocketCallRoutes<NoHttp>;

struct World {
    script: Arc<Script>,
    socket: Arc<crate::sync::requests::SocketRequests>,
    routes: Arc<Routes>,
    core: Arc<Mutex<Core>>,
    held: Held,
    sync: Arc<CatchUp>,
}

fn world() -> World {
    let script = Arc::new(Script::default());
    let socket = up(&script);
    let routes = Arc::new(HttpRoutes(SocketHttp::new(socket.clone(), NoHttp)));
    let core = Arc::new(Mutex::new(Core::default()));
    let held = Held::default();
    let sync = Arc::new(CatchUp {
        core: core.clone(),
        held: held.clone(),
        reachable: AtomicBool::new(true),
        runs: AtomicUsize::new(0),
    });
    World {
        script,
        socket,
        routes,
        core,
        held,
        sync,
    }
}

/// The socket drops after the commit was written: its answer is lost (E_OUTCOME_UNKNOWN). The socket comes back.
fn lose_the_answer(socket: &crate::sync::requests::SocketRequests) {
    socket.on_stopped(ApiError::local(0, "E_NETWORK", "the socket dropped"));
    socket.on_starting();
}

impl World {
    fn spawn_rotate(&self) -> tokio::task::JoinHandle<Result<u64, RotateError>> {
        let (routes, core, sync) = (self.routes.clone(), self.core.clone(), self.sync.clone());
        tokio::spawn(async move { rotate_call(&*core, &*routes, &*sync, &GROUP).await })
    }

    /// The MLS commit of the `n`th request frame (a commit body).
    fn commit_of(&self, n: usize) -> Vec<u8> {
        decode_commit_body(&self.script.body(n))
            .expect("a commit body")
            .1
    }

    /// The core's `(epoch, pending, key)`.
    fn state(&self) -> (u64, bool, Vec<u8>) {
        let c = self.core.lock().unwrap();
        (c.epoch, c.pending.is_some(), c.key.clone())
    }

    fn log(&self) -> Vec<String> {
        self.core.lock().unwrap().log.clone()
    }

    /// The next rotation writes once, under a key of its own, and is confirmed at `seq`.
    async fn rotates_at(&self, n: usize, seq: u64) {
        let rotation = self.spawn_rotate();
        let (id, key) = self.script.request(n).await;
        if n > 0 {
            assert_ne!(key, self.script.request(0).await.1);
        }
        self.socket
            .on_response(&response(id, 200, &seq_answer(seq)));
        assert_eq!(rotation.await.unwrap().unwrap(), seq);
    }
}

/// The first rotation's answer is lost. Whether the instance had applied it (its replay record answers the re-write
/// with the first outcome) or not (it applies the re-write now), the re-write carries the same commit under the same
/// key, and its answer confirms the pending commit: it is never left pending. The next rotation succeeds.
#[tokio::test(start_paused = true)]
async fn a_rotation_whose_answer_is_lost_is_rewritten_under_its_key_and_confirmed() {
    for applied_before in [true, false] {
        let w = world();
        let rotation = w.spawn_rotate();
        let (_, key1) = w.script.request(0).await;
        lose_the_answer(&w.socket);
        w.socket.on_ready("incarnation".into());
        let (id2, key2) = w.script.request(1).await;
        assert_eq!(key1, key2, "the re-write carries the first write's key");
        assert!(key1.is_some());
        assert_eq!(w.script.body(0), w.script.body(1), "the same commit");
        // The replay record's answer (applied before) and a fresh apply look the same to the client: the commit's seq.
        let seq = if applied_before { 7 } else { 8 };
        w.socket.on_response(&response(id2, 200, &seq_answer(seq)));
        assert_eq!(rotation.await.unwrap().unwrap(), seq);
        assert_eq!(w.log(), ["rotate", "confirm"]);

        w.rotates_at(2, seq + 1).await;
        assert_eq!(w.log(), ["rotate", "confirm", "rotate", "confirm"]);
        assert_eq!(w.sync.runs.load(Ordering::SeqCst), 0);
    }
}

/// Task 3b: the first write was applied and its answer lost; the instance restarted, so its replay record is gone and
/// the re-write runs the handler again, which refuses the epoch the first write took (409 E_COMMIT_CONFLICT). The
/// pending commit is NOT aborted: the read from the cursor finds this commit as the one that took the epoch, and the
/// group's catch-up adopts it as the pending commit. The rotation is done at the instance's seq, at the new epoch,
/// with the key every member derives from that commit; the next rotation succeeds.
#[tokio::test(start_paused = true)]
async fn a_lost_rotation_the_restarted_instance_applied_is_confirmed_by_catch_up() {
    let w = world();
    let rotation = w.spawn_rotate();
    let (_, key1) = w.script.request(0).await;
    let commit = w.commit_of(0);
    w.held.lock().unwrap().push((7, 1, commit.clone()));
    lose_the_answer(&w.socket);
    w.socket.on_ready("restarted".into());
    let (id2, key2) = w.script.request(1).await;
    assert_eq!(key1, key2);
    w.socket
        .on_response(&response(id2, 409, &refusal("E_COMMIT_CONFLICT", None)));
    let (id3, _) = w.script.request(2).await;
    let route = w.script.route(2);
    assert!(
        route.ends_with(&format!(
            "/handshakes?from=7&limit={}",
            controller::RECONCILE_PAGE
        )),
        "{route}"
    );
    w.socket.on_response(&response(
        id3,
        200,
        &handshake_page(&w.held.lock().unwrap()),
    ));
    assert_eq!(rotation.await.unwrap().unwrap(), 7);
    assert_eq!(w.log(), ["rotate", "adopt"], "never aborted");
    assert_eq!(
        w.state(),
        (2, false, commit),
        "the new epoch, the instance's commit's key"
    );
    assert_eq!(w.sync.runs.load(Ordering::SeqCst), 1);

    w.rotates_at(3, 8).await;
    assert_eq!(w.state().0, 3);
}

/// Task 3b: the first write was NOT applied (its answer lost with the socket); meanwhile another member's commit took
/// the epoch, and the restarted instance refuses the re-write for it. The read finds that commit; the catch-up applies
/// it and clears the pending commit. The rotation is refused cleanly (nothing pending, the peer's epoch and key), and
/// the next rotation succeeds.
#[tokio::test(start_paused = true)]
async fn a_lost_rotation_the_restarted_instance_never_applied_is_cleared_by_catch_up() {
    let w = world();
    let rotation = w.spawn_rotate();
    w.script.request(0).await;
    lose_the_answer(&w.socket);
    w.held
        .lock()
        .unwrap()
        .push((7, 1, b"the peer's commit".to_vec()));
    w.socket.on_ready("restarted".into());
    let (id2, _) = w.script.request(1).await;
    w.socket
        .on_response(&response(id2, 409, &refusal("E_COMMIT_CONFLICT", None)));
    let (id3, _) = w.script.request(2).await;
    w.socket.on_response(&response(
        id3,
        200,
        &handshake_page(&w.held.lock().unwrap()),
    ));
    let e = rotation.await.unwrap().unwrap_err();
    assert!(e.to_string().contains("E_COMMIT_CONFLICT"), "{e}");
    assert_eq!(w.log(), ["rotate", "clear", "apply"]);
    assert_eq!(w.state(), (2, false, b"the peer's commit".to_vec()));

    w.rotates_at(3, 8).await;
    assert_eq!(w.state().0, 3);
    assert_eq!(w.log()[3..], ["rotate", "confirm"]);
}

/// The catch-up cannot read the group, but the read of the handshakes found this commit: the instance holds it, so it
/// is confirmed. A later catch-up skips it (its epoch is held) and the group stays active: no resync.
#[tokio::test(start_paused = true)]
async fn an_applied_rotation_is_confirmed_when_the_catch_up_fails() {
    let w = world();
    w.sync.reachable.store(false, Ordering::SeqCst);
    let rotation = w.spawn_rotate();
    w.script.request(0).await;
    let commit = w.commit_of(0);
    w.held.lock().unwrap().push((7, 1, commit.clone()));
    lose_the_answer(&w.socket);
    w.socket.on_ready("restarted".into());
    let (id2, _) = w.script.request(1).await;
    w.socket
        .on_response(&response(id2, 409, &refusal("E_COMMIT_CONFLICT", None)));
    let (id3, _) = w.script.request(2).await;
    w.socket.on_response(&response(
        id3,
        200,
        &handshake_page(&w.held.lock().unwrap()),
    ));
    assert_eq!(rotation.await.unwrap().unwrap(), 7);
    assert_eq!(w.log(), ["rotate", "confirm"]);
    assert_eq!(w.state(), (2, false, commit.clone()));

    w.sync.reachable.store(true, Ordering::SeqCst);
    w.sync.catch_up(&GROUP).await;
    assert_eq!(w.state(), (2, false, commit), "skipped, still active");
}

/// Neither the read nor the catch-up reaches the instance: whose commit took the epoch is not known, and the pending
/// commit is kept (never aborted: the refusal proved some commit took its epoch). The engine's next catch-up settles
/// it, adopting it when it is this device's own.
#[tokio::test(start_paused = true)]
async fn an_unsettled_rotation_stays_pending_for_the_next_catch_up() {
    let w = world();
    w.sync.reachable.store(false, Ordering::SeqCst);
    let rotation = w.spawn_rotate();
    w.script.request(0).await;
    let commit = w.commit_of(0);
    w.held.lock().unwrap().push((7, 1, commit.clone()));
    lose_the_answer(&w.socket);
    w.socket.on_ready("restarted".into());
    let (id2, _) = w.script.request(1).await;
    w.socket
        .on_response(&response(id2, 409, &refusal("E_COMMIT_CONFLICT", None)));
    let (id3, _) = w.script.request(2).await;
    w.socket
        .on_response(&response(id3, 403, &refusal("E_FORBIDDEN", None)));
    // The read is tried again after the catch-up, and fails again.
    let (id4, _) = w.script.request(3).await;
    w.socket
        .on_response(&response(id4, 403, &refusal("E_FORBIDDEN", None)));
    let e = rotation.await.unwrap().unwrap_err();
    assert!(e.to_string().contains("E_OUTCOME_UNKNOWN"), "{e}");
    assert_eq!(w.log(), ["rotate"]);
    assert!(w.state().1, "still pending");

    w.sync.reachable.store(true, Ordering::SeqCst);
    w.sync.catch_up(&GROUP).await;
    assert_eq!(w.log(), ["rotate", "adopt"]);
    assert_eq!(w.state(), (2, false, commit));
}

/// The first read fails, the catch-up adopts the commit, and the read tried again after it names this commit as the
/// one that took the epoch: the rotation is done at its seq.
#[tokio::test(start_paused = true)]
async fn a_read_that_fails_before_the_catch_up_is_tried_again_after_it() {
    let w = world();
    let rotation = w.spawn_rotate();
    w.script.request(0).await;
    let commit = w.commit_of(0);
    w.held.lock().unwrap().push((7, 1, commit.clone()));
    lose_the_answer(&w.socket);
    w.socket.on_ready("restarted".into());
    let (id2, _) = w.script.request(1).await;
    w.socket
        .on_response(&response(id2, 409, &refusal("E_COMMIT_CONFLICT", None)));
    let (id3, _) = w.script.request(2).await;
    w.socket
        .on_response(&response(id3, 403, &refusal("E_FORBIDDEN", None)));
    let (id4, _) = w.script.request(3).await;
    assert!(w.script.route(3).contains("/handshakes?from=7&"));
    w.socket.on_response(&response(
        id4,
        200,
        &handshake_page(&w.held.lock().unwrap()),
    ));
    assert_eq!(rotation.await.unwrap().unwrap(), 7);
    assert_eq!(w.log(), ["rotate", "adopt"]);
    assert_eq!(w.state(), (2, false, commit));
}

/// The cursor does not move past a rotation confirmed from its answer, so the read from it serves that earlier own
/// commit first. The commit that took THIS rotation's epoch is the row of the next epoch: here it is this rotation's,
/// so with the catch-up unreachable it is confirmed (never misread as a peer's win and aborted).
#[tokio::test(start_paused = true)]
async fn an_earlier_confirmed_rotation_before_the_cursor_is_not_taken_for_the_winner() {
    let w = world();
    w.rotates_at(0, 7).await;
    let first = w.commit_of(0);
    w.held.lock().unwrap().push((7, 1, first.clone()));
    assert_eq!(
        w.core.lock().unwrap().next_seq,
        7,
        "confirm leaves the cursor"
    );

    w.sync.reachable.store(false, Ordering::SeqCst);
    let rotation = w.spawn_rotate();
    w.script.request(1).await;
    let second = w.commit_of(1);
    w.held.lock().unwrap().push((8, 2, second.clone()));
    lose_the_answer(&w.socket);
    w.socket.on_ready("restarted".into());
    let (id2, _) = w.script.request(2).await;
    w.socket
        .on_response(&response(id2, 409, &refusal("E_COMMIT_CONFLICT", None)));
    let (id3, _) = w.script.request(3).await;
    assert!(w.script.route(3).contains("/handshakes?from=7&"));
    w.socket.on_response(&response(
        id3,
        200,
        &handshake_page(&w.held.lock().unwrap()),
    ));
    assert_eq!(rotation.await.unwrap().unwrap(), 8);
    assert_eq!(w.log(), ["rotate", "confirm", "rotate", "confirm"]);
    assert_eq!(w.state(), (3, false, second));
}

/// After a lost answer the re-write meets a restarting instance: 503 E_UNAVAILABLE (the replay record's answer while
/// the first write still runs, or a store failure: neither proves anything) until the socket's own retries are spent.
/// That is no refusal: the rotation stays in its write loop, and the third write is answered.
#[tokio::test(start_paused = true)]
async fn an_unavailable_rewrite_after_a_lost_answer_is_written_again() {
    let w = world();
    let began = tokio::time::Instant::now();
    let rotation = w.spawn_rotate();
    let (_, key) = w.script.request(0).await;
    lose_the_answer(&w.socket);
    w.socket.on_ready("restarted".into());
    // The re-write and the socket's own retries of it: the last 503 reaches the rotation.
    let attempts = crate::sync::requests::UNAVAILABLE_ATTEMPTS as usize;
    for n in 1..=attempts {
        let (id, k) = w.script.request(n).await;
        assert_eq!(k, key);
        w.socket
            .on_response(&response(id, 503, &refusal("E_UNAVAILABLE", None)));
    }
    let (id, k) = w.script.request(attempts + 1).await;
    assert_eq!(k, key, "the third write, under the same key");
    w.socket.on_response(&response(id, 200, &seq_answer(7)));
    assert_eq!(rotation.await.unwrap().unwrap(), 7);
    assert_eq!(w.log(), ["rotate", "confirm"]);
    assert!(began.elapsed() < controller::ROTATE_RECONCILE);
}

/// Without the reconciliation (the old rotate_call aborted on the refused re-write), the instance holds a commit the
/// core dropped: the next catch-up cannot process it and the group needs a resync. Kept as the model's check against
/// dilla-core's `an_aborted_rotation_the_instance_applied_needs_a_resync`.
#[test]
fn an_abort_of_an_applied_rotation_needs_a_resync() {
    let mut core = Core::default();
    let body = core.call_rotate(&GROUP).unwrap();
    let commit = decode_commit_body(&body).unwrap().1;
    core.commit_abort(&GROUP).unwrap();
    core.apply(&(7, 1, commit));
    assert_eq!((core.state, core.epoch, core.next_seq), (3, 1, 7));
}

/// Every write loses its answer: after ROTATE_WRITES writes, within ROTATE_RECONCILE, the pending commit is aborted.
#[tokio::test(start_paused = true)]
async fn a_rotation_never_reconciled_is_aborted_within_its_bound() {
    let w = world();
    let began = tokio::time::Instant::now();
    let rotation = w.spawn_rotate();
    for n in 0..controller::ROTATE_WRITES as usize {
        w.script.request(n).await;
        lose_the_answer(&w.socket);
        w.socket.on_ready("incarnation".into());
    }
    let e = rotation.await.unwrap().unwrap_err();
    assert!(e.to_string().contains("E_OUTCOME_UNKNOWN"), "{e}");
    assert!(began.elapsed() < controller::ROTATE_RECONCILE);
    assert_eq!(w.script.written(), controller::ROTATE_WRITES as usize);
    assert_eq!(w.log(), ["rotate", "abort"]);
    assert_eq!(w.sync.runs.load(Ordering::SeqCst), 0);
}

/// A join that was cancelled after its last check but still joined (the window before its outcome is sent): the call
/// is left (self-Remove, the call ended when alone, the forget) and the outcome is the cancellation.
#[tokio::test]
async fn a_join_that_lands_after_its_cancel_is_left() {
    let World {
        script,
        socket,
        routes,
        core,
        ..
    } = world();
    let joined = Joined {
        group_id: GROUP,
        media: MediaEpoch {
            epoch: 1,
            base_key: Zeroizing::new([7; 16]),
            own_leaf: 0,
            roster: vec![(0, [1; 16], [2; 16])],
            code: String::new(),
        },
        started: CallStarted {
            created: true,
            call_id: CALL,
            group_id: GROUP,
            livekit_url: String::new(),
            token: Zeroizing::new(String::new()),
            ice_servers: vec![],
        },
    };
    let leaving = {
        let (routes, core) = (routes.clone(), core.clone());
        tokio::spawn(async move { leave_if_cancelled(&*core, &*routes, Ok(joined), true).await })
    };
    // Alone (it registered the group): the call is ended conditionally, then the self-Remove is posted.
    let (id, _) = script.request(0).await;
    socket.on_response(&response(id, 204, b""));
    let (id, _) = script.request(1).await;
    socket.on_response(&response(id, 200, &seq_answer(3)));
    let refused = leaving.await.unwrap().unwrap_err();
    assert_eq!(refused.code, "E_NO_CALL");
    assert_eq!(refused.leave, None);
    assert_eq!(core.lock().unwrap().log, ["leave_proposal", "forget"]);

    // Not cancelled: answered as it is, nothing posted.
    let r = leave_if_cancelled(&*core, &*routes, Err(join_refused()), false).await;
    assert_eq!(r.unwrap_err().code, "E_X");
    assert_eq!(script.written(), 2);
}

fn join_refused() -> JoinRefused {
    JoinRefused {
        code: "E_X".into(),
        phase: join::Phase::Failed,
        leave: Some(GROUP),
        alone: false,
        users: Vec::new(),
        reported: None,
    }
}
