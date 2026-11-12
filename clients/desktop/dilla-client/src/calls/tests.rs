//! The call steps over a scripted gateway socket ([`crate::sync::requests::script`]): what they write, and what they
//! do with the instance's answers.

use std::sync::{Arc, Mutex};

use dilla_core::cbor::Encoder;
use zeroize::Zeroizing;

use super::session::{NoHttp, leave_if_cancelled};
use super::*;
use crate::sync::api::ApiError;
use crate::sync::requests::script::{Script, refusal, response, up};

const GROUP: [u8; 16] = [0x44; 16];
const CALL: [u8; 16] = [0x33; 16];

/// A core that logs the steps a rotation and a leave take; nothing else is called.
#[derive(Default)]
struct Core {
    log: Vec<String>,
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
        self.log.push("rotate".into());
        Ok(b"commit".to_vec())
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
        Ok(())
    }
    fn commit_abort(&mut self, _: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.log.push("abort".into());
        Ok(())
    }
    fn groups(&self) -> Result<Vec<GroupSummary>, CoreRefusal> {
        unreachable!()
    }
}

fn seq_answer(seq: u64) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(2).uint(seq).uint(5);
    e.into_vec()
}

type Routes = SocketCallRoutes<NoHttp>;

type World = (
    Arc<Script>,
    Arc<crate::sync::requests::SocketRequests>,
    Arc<Routes>,
    Arc<Mutex<Core>>,
);

fn world() -> World {
    let script = Arc::new(Script::default());
    let socket = up(&script);
    let routes = Arc::new(HttpRoutes(SocketHttp::new(socket.clone(), NoHttp)));
    (
        script,
        socket,
        routes,
        Arc::new(Mutex::new(Core::default())),
    )
}

/// The socket drops after the commit was written: its answer is lost (E_OUTCOME_UNKNOWN). The socket comes back.
fn lose_the_answer(socket: &crate::sync::requests::SocketRequests) {
    socket.on_stopped(ApiError::local(0, "E_NETWORK", "the socket dropped"));
    socket.on_starting();
}

fn spawn_rotate(
    routes: &Arc<Routes>,
    core: &Arc<Mutex<Core>>,
) -> tokio::task::JoinHandle<Result<u64, RotateError>> {
    let (routes, core) = (routes.clone(), core.clone());
    tokio::spawn(async move { rotate_call(&*core, &*routes, &GROUP).await })
}

/// The first rotation's answer is lost. Whether the instance had applied it (its replay record answers the re-write
/// with the first outcome) or not (it applies the re-write now), the re-write carries the same commit under the same
/// key, and its answer confirms the pending commit: it is never left pending. The next rotation succeeds.
#[tokio::test(start_paused = true)]
async fn a_rotation_whose_answer_is_lost_is_rewritten_under_its_key_and_confirmed() {
    for applied_before in [true, false] {
        let (script, socket, routes, core) = world();
        let rotation = spawn_rotate(&routes, &core);
        let (_, key1) = script.request(0).await;
        lose_the_answer(&socket);
        socket.on_ready("incarnation".into());
        let (id2, key2) = script.request(1).await;
        assert_eq!(key1, key2, "the re-write carries the first write's key");
        assert!(key1.is_some());
        assert_eq!(script.body(0), script.body(1), "the same commit");
        // The replay record's answer (applied before) and a fresh apply look the same to the client: the commit's seq.
        let seq = if applied_before { 7 } else { 8 };
        socket.on_response(&response(id2, 200, &seq_answer(seq)));
        assert_eq!(rotation.await.unwrap().unwrap(), seq);
        assert_eq!(core.lock().unwrap().log, ["rotate", "confirm"]);

        // The next rotation writes under a new key and succeeds.
        let rotation = spawn_rotate(&routes, &core);
        let (id3, key3) = script.request(2).await;
        assert_ne!(key3, key1);
        socket.on_response(&response(id3, 200, &seq_answer(seq + 1)));
        assert_eq!(rotation.await.unwrap().unwrap(), seq + 1);
        assert_eq!(
            core.lock().unwrap().log,
            ["rotate", "confirm", "rotate", "confirm"]
        );
    }
}

/// The re-write is refused (the instance had not applied the commit and the group has moved on): the pending commit is
/// aborted, and the next rotation succeeds.
#[tokio::test(start_paused = true)]
async fn a_rewritten_rotation_that_is_refused_is_aborted() {
    let (script, socket, routes, core) = world();
    let rotation = spawn_rotate(&routes, &core);
    script.request(0).await;
    lose_the_answer(&socket);
    socket.on_ready("incarnation".into());
    let (id2, _) = script.request(1).await;
    socket.on_response(&response(id2, 409, &refusal("E_COMMIT_CONFLICT", None)));
    assert!(rotation.await.unwrap().is_err());
    assert_eq!(core.lock().unwrap().log, ["rotate", "abort"]);

    let rotation = spawn_rotate(&routes, &core);
    let (id3, _) = script.request(2).await;
    socket.on_response(&response(id3, 200, &seq_answer(9)));
    assert_eq!(rotation.await.unwrap().unwrap(), 9);
}

/// Every write loses its answer: after ROTATE_WRITES writes, within ROTATE_RECONCILE, the pending commit is aborted.
#[tokio::test(start_paused = true)]
async fn a_rotation_never_reconciled_is_aborted_within_its_bound() {
    let (script, socket, routes, core) = world();
    let began = tokio::time::Instant::now();
    let rotation = spawn_rotate(&routes, &core);
    for n in 0..controller::ROTATE_WRITES as usize {
        script.request(n).await;
        lose_the_answer(&socket);
        socket.on_ready("incarnation".into());
    }
    let e = rotation.await.unwrap().unwrap_err();
    assert!(e.to_string().contains("E_OUTCOME_UNKNOWN"), "{e}");
    assert!(began.elapsed() < controller::ROTATE_RECONCILE);
    assert_eq!(script.written(), controller::ROTATE_WRITES as usize);
    assert_eq!(core.lock().unwrap().log, ["rotate", "abort"]);
}

/// A join that was cancelled after its last check but still joined (the window before its outcome is sent): the call
/// is left (self-Remove, the call ended when alone, the forget) and the outcome is the cancellation.
#[tokio::test]
async fn a_join_that_lands_after_its_cancel_is_left() {
    let (script, socket, routes, core) = world();
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
    }
}
