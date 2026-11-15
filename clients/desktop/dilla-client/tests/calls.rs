//! Task 11: the call signalling (`dilla_client::calls`) over a scripted core and instance, in the
//! order join.ts and controller.ts take, and the wire decoders against bodies built as dillad and
//! dilla-core build them.
//!
//! Screens wave 1, Task 3: the rejoin's wait for this device's own Remove (scripted, and against a
//! real dillad), and a running `Client`'s call signalling over its gateway socket. The real-dillad
//! tests are `#[ignore]`d (they need `target/dilla-testhost`, the WASI core, and they build the
//! testkit's peer and dilla-media's `call-peer`): `cargo test -p dilla-client --test calls --
//! --ignored`.

#[path = "support/call_world.rs"]
mod world;

use std::collections::VecDeque;
use std::sync::Mutex;
use std::time::{Duration, Instant};

use dilla_client::calls::wire::{decode_call_started, decode_channel_call, decode_media_epoch};
use dilla_client::calls::{
    CallCatchUp, CallCore, CallPhase, CallRoutes, CallStarted, ChannelCall, CoreRefusal,
    GroupSummary, HandshakePage, JoinHooks, JoinTarget, REJOIN_STEP, REJOIN_WAIT, RouteError,
    join_call, leave_call, media_epoch, rotate_call,
};
use dilla_core::cbor::Encoder;
use zeroize::Zeroizing;

const CHANNEL: [u8; 16] = [0x33; 16];
const COMMUNITY: [u8; 16] = [0x22; 16];
const GROUP: [u8; 16] = [0x44; 16];
const FRESH: [u8; 16] = [0x55; 16];
const ME: [u8; 16] = [0xaa; 16];
const ME_USER: [u8; 16] = [0xa1; 16];
const PEER: [u8; 16] = [0xbb; 16];

fn epoch_body(epoch: u64, roster: &[(u64, [u8; 16])]) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(5).uint(epoch).bytes(&[7; 16]).uint(0);
    e.array(roster.len());
    for (leaf, device) in roster {
        e.array(3).uint(*leaf).bytes(device).bytes(&ME_USER);
    }
    e.text("12345 12345 12345 12345 12345 12345");
    e.into_vec()
}

fn refusal(code: &str) -> CoreRefusal {
    CoreRefusal {
        code: code.into(),
        detail: String::new(),
    }
}

/// A core whose answers are scripted; every call is logged.
#[derive(Default)]
struct FakeCore {
    log: Vec<String>,
    join_external: VecDeque<Result<Vec<u8>, CoreRefusal>>,
    groups: Vec<GroupSummary>,
    joined_fails: bool,
}

impl CallCore for FakeCore {
    fn call_group_create(
        &mut self,
        id: &[u8; 16],
        _c: Option<&[u8; 16]>,
        _ch: &[u8; 16],
        _p: u64,
        _x: &[u8; 32],
    ) -> Result<Vec<u8>, CoreRefusal> {
        self.log.push(format!("create {:02x}", id[0]));
        Ok(b"group body".to_vec())
    }
    fn call_group_join_external(
        &mut self,
        id: &[u8; 16],
        _c: Option<&[u8; 16]>,
        _ch: &[u8; 16],
        _p: u64,
        info: &[u8],
        _t: &[u8],
    ) -> Result<Vec<u8>, CoreRefusal> {
        self.log.push(format!(
            "join_external {:02x} {}",
            id[0],
            String::from_utf8_lossy(info)
        ));
        self.join_external
            .pop_front()
            .unwrap_or(Ok(b"external commit".to_vec()))
    }
    fn call_join_check(&self, _id: &[u8; 16]) -> Result<(), CoreRefusal> {
        Ok(())
    }
    fn call_media_epoch(&self, _id: &[u8; 16]) -> Result<Zeroizing<Vec<u8>>, CoreRefusal> {
        Ok(Zeroizing::new(epoch_body(3, &[(0, ME), (1, PEER)])))
    }
    fn call_leave_proposal(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        self.log.push(format!("leave_proposal {:02x}", id[0]));
        Ok(b"remove".to_vec())
    }
    fn call_rotate(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        self.log.push(format!("rotate {:02x}", id[0]));
        Ok(b"rotation".to_vec())
    }
    fn call_group_forget(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.log.push(format!("forget {:02x}", id[0]));
        Ok(())
    }
    fn group_registered(&mut self, id: &[u8; 16], next_seq: u64) -> Result<(), CoreRefusal> {
        self.log
            .push(format!("registered {:02x} {next_seq}", id[0]));
        Ok(())
    }
    fn group_joined(&mut self, id: &[u8; 16], seq: u64) -> Result<(), CoreRefusal> {
        self.log.push(format!("joined {:02x} {seq}", id[0]));
        if self.joined_fails {
            return Err(refusal("E_CORE_STATE"));
        }
        Ok(())
    }
    fn group_discard(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.log.push(format!("discard {:02x}", id[0]));
        Ok(())
    }
    fn commit_confirm(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.log.push(format!("confirm {:02x}", id[0]));
        Ok(())
    }
    fn commit_abort(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.log.push(format!("abort {:02x}", id[0]));
        Ok(())
    }
    fn groups(&self) -> Result<Vec<GroupSummary>, CoreRefusal> {
        Ok(self.groups.clone())
    }
}

/// A core that wraps FakeCore but scripts the gate (join check and media epoch).
struct GatedCore {
    inner: FakeCore,
    check: Mutex<VecDeque<Result<(), CoreRefusal>>>,
    media: Mutex<VecDeque<Result<Vec<u8>, CoreRefusal>>>,
}

impl CallCore for GatedCore {
    fn call_group_create(
        &mut self,
        id: &[u8; 16],
        c: Option<&[u8; 16]>,
        ch: &[u8; 16],
        p: u64,
        x: &[u8; 32],
    ) -> Result<Vec<u8>, CoreRefusal> {
        self.inner.call_group_create(id, c, ch, p, x)
    }
    fn call_group_join_external(
        &mut self,
        id: &[u8; 16],
        c: Option<&[u8; 16]>,
        ch: &[u8; 16],
        p: u64,
        info: &[u8],
        t: &[u8],
    ) -> Result<Vec<u8>, CoreRefusal> {
        self.inner.call_group_join_external(id, c, ch, p, info, t)
    }
    fn call_join_check(&self, _id: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.check.lock().unwrap().pop_front().unwrap_or(Ok(()))
    }
    fn call_media_epoch(&self, id: &[u8; 16]) -> Result<Zeroizing<Vec<u8>>, CoreRefusal> {
        self.media
            .lock()
            .unwrap()
            .pop_front()
            .map(|r| r.map(Zeroizing::new))
            .unwrap_or_else(|| self.inner.call_media_epoch(id))
    }
    fn call_leave_proposal(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        self.inner.call_leave_proposal(id)
    }
    fn call_rotate(&mut self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        self.inner.call_rotate(id)
    }
    fn call_group_forget(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.inner.call_group_forget(id)
    }
    fn group_registered(&mut self, id: &[u8; 16], s: u64) -> Result<(), CoreRefusal> {
        self.inner.group_registered(id, s)
    }
    fn group_joined(&mut self, id: &[u8; 16], s: u64) -> Result<(), CoreRefusal> {
        self.inner.group_joined(id, s)
    }
    fn group_discard(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.inner.group_discard(id)
    }
    fn commit_confirm(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.inner.commit_confirm(id)
    }
    fn commit_abort(&mut self, id: &[u8; 16]) -> Result<(), CoreRefusal> {
        self.inner.commit_abort(id)
    }
    fn groups(&self) -> Result<Vec<GroupSummary>, CoreRefusal> {
        self.inner.groups()
    }
}

fn gated(
    check: Vec<Result<(), CoreRefusal>>,
    media: Vec<Result<Vec<u8>, CoreRefusal>>,
) -> GatedCore {
    GatedCore {
        inner: FakeCore::default(),
        check: Mutex::new(check.into()),
        media: Mutex::new(media.into()),
    }
}

fn err(status: u16, code: &str) -> RouteError {
    RouteError {
        status: Some(status),
        code: Some(code.into()),
        detail: String::new(),
    }
}

/// A catch-up that is never needed: these rotations are never refused for their epoch after a lost answer (the
/// scripted-socket tests in `src/calls/tests.rs` cover that reconciliation).
struct NoCatchUp;

#[async_trait::async_trait]
impl CallCatchUp for NoCatchUp {
    async fn catch_up(&self, _group: &[u8; 16]) {
        panic!("no catch-up expected")
    }
}

#[derive(Default)]
struct FakeRoutes {
    log: Mutex<Vec<String>>,
    calls: Mutex<VecDeque<ChannelCall>>,
    post_group: Mutex<VecDeque<Result<u64, RouteError>>>,
    resync: Mutex<VecDeque<Result<u64, RouteError>>>,
    start: Mutex<VecDeque<Result<(), RouteError>>>,
    commit: Mutex<VecDeque<Result<u64, RouteError>>>,
    /// GroupInfo answers in order; then `info`.
    infos: Mutex<VecDeque<Vec<u8>>>,
    /// Once `resync` is spent: every later resync is this refusal (none: accepted).
    resync_always: Mutex<Option<RouteError>>,
    proposal: Mutex<VecDeque<Result<u64, RouteError>>>,
    /// The idempotency key of every commit written.
    idems: Mutex<Vec<[u8; 16]>>,
    /// Set while the start is in flight: a leave that lands during it.
    cancel_on_start: Option<std::sync::Arc<std::sync::atomic::AtomicBool>>,
    alone: bool,
}

impl FakeRoutes {
    fn log(&self, s: String) {
        self.log.lock().unwrap().push(s);
    }
    fn posted(&self) -> Vec<String> {
        self.log.lock().unwrap().clone()
    }
}

fn started() -> CallStarted {
    CallStarted {
        created: false,
        call_id: CHANNEL,
        group_id: GROUP,
        livekit_url: "ws://127.0.0.1:7880".into(),
        token: Zeroizing::new("lk-token".into()),
        ice_servers: vec![],
    }
}

#[async_trait::async_trait]
impl CallRoutes for FakeRoutes {
    async fn get_channel_call(&self, _c: &[u8; 16]) -> Result<ChannelCall, RouteError> {
        self.log("get_call".into());
        Ok(self
            .calls
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(ChannelCall {
                group_id: Some(GROUP),
                call_id: Some(CHANNEL),
                participants: vec![],
            }))
    }
    async fn post_group(&self, _b: Vec<u8>) -> Result<u64, RouteError> {
        self.log("post_group".into());
        self.post_group.lock().unwrap().pop_front().unwrap_or(Ok(1))
    }
    async fn get_group_info(&self, _g: &[u8; 16]) -> Result<Vec<u8>, RouteError> {
        self.log("get_info".into());
        Ok(self
            .infos
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or_else(|| b"info".to_vec()))
    }
    async fn get_group_tree(&self, _g: &[u8; 16]) -> Result<Vec<u8>, RouteError> {
        self.log("get_tree".into());
        Ok(b"tree".to_vec())
    }
    async fn post_resync(&self, _g: &[u8; 16], _b: Vec<u8>) -> Result<u64, RouteError> {
        self.log("post_resync".into());
        let queued = self.resync.lock().unwrap().pop_front();
        queued.unwrap_or_else(|| {
            self.resync_always
                .lock()
                .unwrap()
                .clone()
                .map_or(Ok(9), Err)
        })
    }
    async fn post_commit(
        &self,
        _g: &[u8; 16],
        _b: Vec<u8>,
        idem: [u8; 16],
    ) -> Result<u64, RouteError> {
        self.log("post_commit".into());
        self.idems.lock().unwrap().push(idem);
        self.commit.lock().unwrap().pop_front().unwrap_or(Ok(10))
    }
    async fn get_handshakes(
        &self,
        _g: &[u8; 16],
        from: u64,
        _limit: u64,
    ) -> Result<HandshakePage, RouteError> {
        self.log(format!("get_handshakes {from}"));
        Ok(HandshakePage::default())
    }
    async fn post_proposal(&self, _g: &[u8; 16], b: Vec<u8>) -> Result<u64, RouteError> {
        self.log(format!("post_proposal {}", String::from_utf8_lossy(&b)));
        self.proposal.lock().unwrap().pop_front().unwrap_or(Ok(11))
    }
    async fn start_call(&self, _c: &[u8; 16], _v: &str) -> Result<CallStarted, RouteError> {
        self.log("start".into());
        if let Some(c) = &self.cancel_on_start {
            c.store(true, std::sync::atomic::Ordering::SeqCst);
        }
        self.start
            .lock()
            .unwrap()
            .pop_front()
            .unwrap_or(Ok(()))
            .map(|()| started())
    }
    async fn end_call_if_alone(&self, _c: &[u8; 16]) -> Result<bool, RouteError> {
        self.log("end_if_alone".into());
        Ok(self.alone)
    }
    async fn decline_call(&self, _c: &[u8; 16], _t: u64) -> Result<(), RouteError> {
        Ok(())
    }
}

struct Hooks {
    refreshed: Mutex<u32>,
    phases: Mutex<Vec<CallPhase>>,
    cancel: std::sync::Arc<std::sync::atomic::AtomicBool>,
}

#[async_trait::async_trait]
impl JoinHooks for Hooks {
    fn random_id(&self) -> [u8; 16] {
        FRESH
    }
    async fn refresh_leaves(&self, _g: &[u8; 16]) {
        *self.refreshed.lock().unwrap() += 1;
    }
    fn cancelled(&self) -> bool {
        self.cancel.load(std::sync::atomic::Ordering::SeqCst)
    }
    fn phase(&self, phase: CallPhase) {
        self.phases.lock().unwrap().push(phase);
    }
}

fn hooks() -> Hooks {
    Hooks {
        refreshed: Mutex::new(0),
        phases: Mutex::new(Vec::new()),
        cancel: std::sync::Arc::default(),
    }
}

fn target() -> JoinTarget {
    JoinTarget {
        channel: CHANNEL,
        community: Some(COMMUNITY),
        vdec: String::new(),
        policy_version: 1,
        external_sender_pub: [9; 32],
    }
}

#[tokio::test]
async fn the_first_join_registers_the_call_group_then_starts_the_call() {
    let core = Mutex::new(FakeCore::default());
    let routes = FakeRoutes::default();
    routes.calls.lock().unwrap().push_back(ChannelCall {
        group_id: None,
        call_id: None,
        participants: vec![],
    });
    let joined = join_call(&core, &routes, &hooks(), &target())
        .await
        .unwrap();
    assert_eq!(joined.group_id, FRESH);
    assert_eq!(joined.media.epoch, 3);
    assert_eq!(*joined.media.base_key, [7; 16]);
    assert_eq!(joined.started.token.as_str(), "lk-token");
    assert_eq!(routes.posted(), ["get_call", "post_group", "start"]);
    assert_eq!(core.lock().unwrap().log, ["create 55", "registered 55 1"]);
}

#[tokio::test]
async fn a_join_of_a_live_call_posts_its_external_commit_only_after_the_gate_passes() {
    let core = Mutex::new(gated(vec![], vec![]));
    let routes = FakeRoutes::default();
    let joined = join_call(&core, &routes, &hooks(), &target())
        .await
        .unwrap();
    assert_eq!(joined.group_id, GROUP);
    assert_eq!(
        routes.posted(),
        ["get_call", "get_info", "get_tree", "post_resync", "start"]
    );
    assert_eq!(
        core.lock().unwrap().inner.log,
        ["join_external 44 info", "joined 44 9"]
    );
}

#[tokio::test]
async fn a_gate_refusal_posts_nothing_and_is_a_block() {
    let core = Mutex::new(gated(vec![Err(refusal("E_UMK_CHANGED"))], vec![]));
    core.lock().unwrap().inner.groups = vec![GroupSummary {
        group_id: GROUP,
        kind: 1,
        target_id: CHANNEL,
        state: 1,
        epoch: 0,
        next_seq: 0,
        pending_commit: false,
    }];
    let routes = FakeRoutes::default();
    let refused = join_call(&core, &routes, &hooks(), &target())
        .await
        .unwrap_err();
    assert_eq!(refused.code, "E_UMK_CHANGED");
    assert_eq!(refused.phase, dilla_client::calls::join::Phase::Blocked);
    assert_eq!(refused.leave, None);
    assert_eq!(
        routes.posted(),
        ["get_call", "get_info", "get_tree"],
        "no commit posted"
    );
    assert_eq!(
        core.lock().unwrap().inner.log,
        ["join_external 44 info", "discard 44"],
        "the staged group is discarded"
    );
    assert_eq!(refused.users, vec![], "an empty room names nobody");
    assert_eq!(refused.reported, None, "the refusal named no leaf");
}

/// join.ts `e.users` and core-port.ts `changedUser` (Task 18 review I1): a gate refusal carries the room's users as the
/// last call read listed them, and the leaf the core named in its detail.
#[tokio::test]
async fn a_gate_refusal_carries_the_rooms_users_and_the_leaf_the_core_named() {
    use dilla_client::calls::CallParticipant;
    use dilla_client::state::UserId;
    let named = "b2".repeat(16);
    let core = Mutex::new(gated(
        vec![Err(CoreRefusal {
            code: "E_UMK_CHANGED".into(),
            detail: named.clone(),
        })],
        vec![],
    ));
    core.lock().unwrap().inner.groups = vec![GroupSummary {
        group_id: GROUP,
        kind: 1,
        target_id: CHANNEL,
        state: 1,
        epoch: 0,
        next_seq: 0,
        pending_commit: false,
    }];
    let routes = FakeRoutes::default();
    let p = |u: u8, d: u8| CallParticipant {
        user_id: [u; 16],
        device_id: [d; 16],
        flags: 0,
    };
    routes.calls.lock().unwrap().push_back(ChannelCall {
        group_id: Some(GROUP),
        call_id: Some(CHANNEL),
        participants: vec![p(0xc3, 1), p(0xc3, 2), p(0xd4, 3)],
    });
    let refused = join_call(&core, &routes, &hooks(), &target())
        .await
        .unwrap_err();
    assert_eq!(refused.users, vec![UserId([0xc3; 16]), UserId([0xd4; 16])]);
    assert_eq!(refused.reported, Some(UserId([0xb2; 16])));
    // A detail that is not exactly one user id names nobody.
    for detail in ["", "B2B2", &"zz".repeat(16)] {
        let e = CoreRefusal {
            code: "E_UMK_CHANGED".into(),
            detail: detail.into(),
        };
        assert_eq!(
            dilla_client::calls::join::reported_user(&e),
            None,
            "{detail:?}"
        );
    }
    let other = CoreRefusal {
        code: "E_CREDENTIAL".into(),
        detail: named,
    };
    assert_eq!(dilla_client::calls::join::reported_user(&other), None);
}

#[tokio::test]
async fn a_pending_leaf_is_refreshed_once_before_the_commit_is_posted() {
    let core = Mutex::new(gated(
        vec![Err(refusal("E_DEVICE_LIST_STALE")), Ok(())],
        vec![],
    ));
    let routes = FakeRoutes::default();
    let h = hooks();
    join_call(&core, &routes, &h, &target()).await.unwrap();
    assert_eq!(*h.refreshed.lock().unwrap(), 1);
    assert!(routes.posted().contains(&"post_resync".to_owned()));
}

#[tokio::test]
async fn a_media_key_refusal_after_the_join_leaves_the_group() {
    let core = Mutex::new(gated(vec![], vec![Err(refusal("E_DEVICE_UNLISTED"))]));
    let routes = FakeRoutes::default();
    let refused = join_call(&core, &routes, &hooks(), &target())
        .await
        .unwrap_err();
    assert_eq!(refused.code, "E_DEVICE_UNLISTED");
    assert_eq!(refused.leave, Some(GROUP), "the joined group must be left");
    assert!(
        !routes.posted().contains(&"start".to_owned()),
        "no call started"
    );
}

#[tokio::test]
async fn a_lost_registration_race_joins_the_winners_group() {
    let core = Mutex::new(gated(vec![], vec![]));
    let routes = FakeRoutes::default();
    routes.calls.lock().unwrap().push_back(ChannelCall {
        group_id: None,
        call_id: None,
        participants: vec![],
    });
    routes
        .post_group
        .lock()
        .unwrap()
        .push_back(Err(err(409, "E_GROUP_EXISTS")));
    let joined = join_call(&core, &routes, &hooks(), &target())
        .await
        .unwrap();
    assert_eq!(joined.group_id, GROUP);
    assert_eq!(
        routes.posted(),
        [
            "get_call",
            "post_group",
            "get_call",
            "get_info",
            "get_tree",
            "post_resync",
            "start"
        ]
    );
    assert_eq!(
        core.lock().unwrap().inner.log[..2],
        ["create 55".to_owned(), "discard 55".to_owned()]
    );
}

#[tokio::test]
async fn a_failed_start_of_a_call_this_device_registered_is_ended_alone() {
    let core = Mutex::new(FakeCore::default());
    let routes = FakeRoutes::default();
    routes.calls.lock().unwrap().push_back(ChannelCall {
        group_id: None,
        call_id: None,
        participants: vec![],
    });
    routes
        .start
        .lock()
        .unwrap()
        .push_back(Err(err(403, "E_FORBIDDEN")));
    let refused = join_call(&core, &routes, &hooks(), &target())
        .await
        .unwrap_err();
    assert_eq!(refused.code, "E_FORBIDDEN");
    assert_eq!(refused.leave, Some(FRESH));
    assert!(refused.alone);
}

#[tokio::test]
async fn a_leave_ends_the_call_when_alone_then_proposes_its_remove_and_forgets() {
    let core = Mutex::new(FakeCore::default());
    let routes = FakeRoutes {
        alone: true,
        ..Default::default()
    };
    let left = leave_call(&core, &routes, &GROUP, &CHANNEL, true).await;
    assert!(left.ended && left.proposed);
    assert_eq!(routes.posted(), ["end_if_alone", "post_proposal remove"]);
    assert_eq!(core.lock().unwrap().log, ["leave_proposal 44", "forget 44"]);
}

/// Review HL I2: a leave's Remove counts as acknowledged when the instance took it, or answers 409 `E_REMOVE_PENDING`
/// (a Remove of this leaf stands there already); any other refusal or a lost answer is not.
#[tokio::test]
async fn a_pending_remove_is_an_acknowledged_leave() {
    let core = Mutex::new(FakeCore::default());
    let routes = FakeRoutes::default();
    routes
        .proposal
        .lock()
        .unwrap()
        .push_back(Err(err(409, "E_REMOVE_PENDING")));
    let left = leave_call(&core, &routes, &GROUP, &CHANNEL, false).await;
    assert!(left.proposed && !left.ended);
    routes
        .proposal
        .lock()
        .unwrap()
        .push_back(Err(err(503, "E_UNAVAILABLE")));
    let left = leave_call(&core, &routes, &GROUP, &CHANNEL, false).await;
    assert!(!left.proposed && !left.ended);
}

#[tokio::test]
async fn a_rotation_is_confirmed_when_posted_and_aborted_when_refused() {
    let core = Mutex::new(FakeCore::default());
    let routes = FakeRoutes::default();
    assert_eq!(
        rotate_call(&core, &routes, &NoCatchUp, &GROUP)
            .await
            .unwrap(),
        10
    );
    routes
        .commit
        .lock()
        .unwrap()
        .push_back(Err(err(409, "E_COMMIT_CONFLICT")));
    assert!(
        rotate_call(&core, &routes, &NoCatchUp, &GROUP)
            .await
            .is_err()
    );
    assert_eq!(
        core.lock().unwrap().log,
        ["rotate 44", "confirm 44", "rotate 44", "abort 44"]
    );
}

fn outcome_unknown() -> RouteError {
    RouteError {
        status: None,
        code: Some("E_OUTCOME_UNKNOWN".into()),
        detail: "the request was written and its answer was lost".into(),
    }
}

/// A rotation whose answer was lost (E_OUTCOME_UNKNOWN) is reconciled, never left pending: the same commit is written
/// again with the same idempotency key (the instance's replay record answers the first write's outcome, or applies it
/// now) and confirmed; the next rotation writes under a new key. A proposal whose answer was lost aborts nothing.
#[tokio::test(start_paused = true)]
async fn a_lost_rotation_answer_is_rewritten_with_its_key_and_confirmed() {
    let core = Mutex::new(FakeCore::default());
    let routes = FakeRoutes::default();
    routes
        .commit
        .lock()
        .unwrap()
        .push_back(Err(outcome_unknown()));
    assert_eq!(
        rotate_call(&core, &routes, &NoCatchUp, &GROUP)
            .await
            .unwrap(),
        10
    );
    let idems = routes.idems.lock().unwrap().clone();
    assert_eq!(idems.len(), 2);
    assert_eq!(
        idems[0], idems[1],
        "the re-write carries the first write's key"
    );
    assert_eq!(core.lock().unwrap().log, ["rotate 44", "confirm 44"]);

    // The next rotation succeeds, under a key of its own.
    assert_eq!(
        rotate_call(&core, &routes, &NoCatchUp, &GROUP)
            .await
            .unwrap(),
        10
    );
    let idems = routes.idems.lock().unwrap().clone();
    assert_ne!(idems[2], idems[0]);
    assert_eq!(
        core.lock().unwrap().log,
        ["rotate 44", "confirm 44", "rotate 44", "confirm 44"]
    );

    routes
        .proposal
        .lock()
        .unwrap()
        .push_back(Err(outcome_unknown()));
    let left = leave_call(&core, &routes, &GROUP, &CHANNEL, false).await;
    assert!(!left.proposed && !left.ended);
    assert_eq!(
        core.lock().unwrap().log[4..],
        ["leave_proposal 44", "forget 44"]
    );
}

/// Bounded: a rotation whose every write loses its answer is aborted after ROTATE_WRITES writes, within
/// ROTATE_RECONCILE, and the group takes its next rotation; a refusal of a re-write aborts at once.
#[tokio::test(start_paused = true)]
async fn an_unreconciled_rotation_is_aborted_within_the_bound() {
    use dilla_client::calls::controller::{ROTATE_RECONCILE, ROTATE_WRITES};
    let core = Mutex::new(FakeCore::default());
    let routes = FakeRoutes::default();
    routes
        .commit
        .lock()
        .unwrap()
        .extend((0..ROTATE_WRITES).map(|_| Err(outcome_unknown())));
    let began = tokio::time::Instant::now();
    let e = rotate_call(&core, &routes, &NoCatchUp, &GROUP)
        .await
        .unwrap_err();
    assert!(e.to_string().contains("E_OUTCOME_UNKNOWN"), "{e}");
    assert!(began.elapsed() < ROTATE_RECONCILE, "{:?}", began.elapsed());
    assert_eq!(routes.idems.lock().unwrap().len(), ROTATE_WRITES as usize);
    assert_eq!(core.lock().unwrap().log, ["rotate 44", "abort 44"]);

    // A re-write refused by a check that proves it was never applied (past the epoch check: the instance is still at
    // the commit's epoch) aborts at once, with no catch-up and no read. The epoch's refusal (409 E_COMMIT_CONFLICT)
    // is reconciled instead: src/calls/tests.rs.
    routes
        .commit
        .lock()
        .unwrap()
        .extend([Err(outcome_unknown()), Err(err(422, "E_COMMIT_INVALID"))]);
    assert!(
        rotate_call(&core, &routes, &NoCatchUp, &GROUP)
            .await
            .is_err()
    );
    assert_eq!(core.lock().unwrap().log[2..], ["rotate 44", "abort 44"]);
    assert!(
        !routes
            .posted()
            .iter()
            .any(|p| p.starts_with("get_handshakes"))
    );

    // The next rotation succeeds.
    assert_eq!(
        rotate_call(&core, &routes, &NoCatchUp, &GROUP)
            .await
            .unwrap(),
        10
    );
    assert_eq!(core.lock().unwrap().log[4..], ["rotate 44", "confirm 44"]);
}

#[test]
fn the_media_epoch_decodes_into_a_zeroizing_key_and_refuses_trailing_bytes() {
    let core = Mutex::new(FakeCore::default());
    let m = media_epoch(&core, &GROUP).unwrap();
    assert_eq!((m.epoch, m.own_leaf, m.roster.len()), (3, 0, 2));
    assert_eq!(m.roster[1], (1, PEER, ME_USER));
    let mut body = epoch_body(3, &[(0, ME)]);
    body.push(0);
    assert!(decode_media_epoch(&body).is_err());
}

#[test]
fn the_call_answers_decode_as_routes_ts_decodes_them() {
    // GET /v1/channels/{id}/call: [group|null, call|null, [[user, device, flags]], ...more].
    let mut e = Encoder::new();
    e.array(4).bytes(&GROUP).null();
    e.array(1).array(3).bytes(&ME_USER).bytes(&ME).uint(5);
    e.uint(0);
    let call = decode_channel_call(&e.into_vec()).unwrap();
    assert_eq!(call.group_id, Some(GROUP));
    assert_eq!(call.call_id, None);
    assert_eq!(call.participants[0].flags, 5);
    let mut e = Encoder::new();
    e.array(3).null().null();
    e.array(1).array(3).bytes(&ME_USER).bytes(&ME).uint(32);
    assert!(
        decode_channel_call(&e.into_vec()).is_err(),
        "flags above 31"
    );

    // POST /v1/channels/{id}/calls: [call, group, url, token, [[urls], user, cred], caps].
    let mut e = Encoder::new();
    e.array(6)
        .bytes(&CHANNEL)
        .bytes(&GROUP)
        .text("wss://sfu.example")
        .text("jwt");
    e.array(1).array(3);
    e.array(1).text("turn:relay.example:3478");
    e.text("u").text("p");
    e.array(3).uint(1).uint(1).uint(1);
    let s = decode_call_started(201, &e.into_vec()).unwrap();
    assert!(s.created);
    assert_eq!(s.livekit_url, "wss://sfu.example");
    assert_eq!(s.token.as_str(), "jwt");
    assert_eq!(s.ice_servers[0].0, ["turn:relay.example:3478"]);
}

#[tokio::test]
async fn a_failure_after_the_instance_accepted_the_join_commit_names_the_group_to_leave() {
    let core = Mutex::new(gated(vec![], vec![]));
    core.lock().unwrap().inner.joined_fails = true;
    let routes = FakeRoutes::default();
    let refused = join_call(&core, &routes, &hooks(), &target())
        .await
        .unwrap_err();
    assert_eq!(refused.code, "E_CORE_STATE");
    // RED (review M4): leave was None, so the posted external commit's leaf was never removed.
    assert_eq!(refused.leave, Some(GROUP));
    assert!(routes.posted().contains(&"post_resync".to_owned()));
}

// ---- the rejoin's wait for this device's own Remove (join.ts waitForRemove; controller.test.ts "a rejoin refused
// because its own Remove stands (409 E_REMOVE_PENDING) waits, says so, and joins once the Remove is committed") ----

#[test]
fn the_rejoin_wait_is_bounded_as_join_ts_bounds_it() {
    assert_eq!(REJOIN_STEP, std::time::Duration::from_millis(2_000));
    assert_eq!(REJOIN_WAIT, std::time::Duration::from_millis(30_000));
}

/// The wait is bounded: a Remove nobody ever commits (every resync refused, the GroupInfo never moving) fails the join
/// with that refusal REJOIN_WAIT after the first one, within one step of it.
#[tokio::test(start_paused = true)]
async fn a_rejoin_whose_remove_is_never_committed_gives_up_after_rejoin_wait() {
    let core = Mutex::new(gated(vec![], vec![]));
    let routes = FakeRoutes::default();
    *routes.resync_always.lock().unwrap() = Some(err(409, "E_REMOVE_PENDING"));
    let h = hooks();
    let began = tokio::time::Instant::now();
    // On the paused clock a wait with no bound would spin forever: two bounds' worth is a failure.
    let refused = tokio::time::timeout(2 * REJOIN_WAIT, join_call(&core, &routes, &h, &target()))
        .await
        .expect("the wait ends REJOIN_WAIT after the first refusal")
        .unwrap_err();
    let waited = began.elapsed();
    assert_eq!(refused.code, "E_REMOVE_PENDING");
    assert_eq!(refused.leave, None);
    assert!(
        waited >= REJOIN_WAIT && waited < REJOIN_WAIT + REJOIN_STEP,
        "{waited:?}"
    );
    assert_eq!(
        *h.phases.lock().unwrap(),
        [
            CallPhase::Joining,
            CallPhase::WaitingForRemove,
            CallPhase::Idle
        ]
    );
    assert!(!routes.posted().contains(&"start".to_owned()));
}

/// The bound runs from the first refusal: a GroupInfo that moves (a commit, not the Remove) and a resync refused
/// again do not restart it.
#[tokio::test(start_paused = true)]
async fn the_rejoin_wait_ends_rejoin_wait_after_the_first_refusal_even_when_the_group_moves() {
    let core = Mutex::new(gated(vec![], vec![]));
    let routes = FakeRoutes::default();
    *routes.resync_always.lock().unwrap() = Some(err(409, "E_REMOVE_PENDING"));
    // The join reads "info"; step 1 sees it unchanged, step 2 sees it moved; the second join reads "moved" and is
    // refused again; from then on it never moves.
    routes
        .infos
        .lock()
        .unwrap()
        .extend([b"info".to_vec(), b"info".to_vec(), b"moved".to_vec()]);
    let h = hooks();
    let began = tokio::time::Instant::now();
    // On the paused clock a wait with no bound would spin forever: two bounds' worth is a failure.
    let refused = tokio::time::timeout(2 * REJOIN_WAIT, join_call(&core, &routes, &h, &target()))
        .await
        .expect("the wait ends REJOIN_WAIT after the first refusal")
        .unwrap_err();
    let waited = began.elapsed();
    assert_eq!(refused.code, "E_REMOVE_PENDING");
    assert!(
        waited >= REJOIN_WAIT && waited < REJOIN_WAIT + REJOIN_STEP,
        "{waited:?}"
    );
    assert_eq!(
        routes
            .posted()
            .iter()
            .filter(|p| *p == "post_resync")
            .count(),
        2,
        "the moved group was tried once more: {:?}",
        routes.posted()
    );
    assert_eq!(
        *h.phases.lock().unwrap(),
        [
            CallPhase::Joining,
            CallPhase::WaitingForRemove,
            CallPhase::Idle
        ]
    );
}

#[tokio::test(start_paused = true)]
async fn a_rejoin_refused_while_its_remove_stands_waits_then_joins_the_same_group_once_it_moves() {
    let core = Mutex::new(gated(vec![], vec![]));
    let routes = FakeRoutes::default();
    routes
        .resync
        .lock()
        .unwrap()
        .push_back(Err(err(409, "E_REMOVE_PENDING")));
    // The join reads "info"; the first step reads it unchanged (nobody committed yet), the second the group moved on
    // (the Remove committed), and the rejoin reads that.
    routes.infos.lock().unwrap().extend([
        b"info".to_vec(),
        b"info".to_vec(),
        b"moved".to_vec(),
        b"moved".to_vec(),
    ]);
    let h = hooks();
    let began = tokio::time::Instant::now();
    let joined = join_call(&core, &routes, &h, &target()).await.unwrap();
    assert_eq!(joined.group_id, GROUP, "the same call group");
    assert!(began.elapsed() >= 2 * REJOIN_STEP, "{:?}", began.elapsed());
    assert_eq!(
        routes.posted(),
        [
            "get_call",
            "get_info",
            "get_tree",
            "post_resync",
            // step 1: the call still names the group, its GroupInfo is unchanged
            "get_call",
            "get_info",
            // step 2: the GroupInfo moved
            "get_call",
            "get_info",
            // the rejoin, built on the moved group
            "get_info",
            "get_tree",
            "post_resync",
            "start",
        ]
    );
    assert_eq!(
        *h.phases.lock().unwrap(),
        [
            CallPhase::Joining,
            CallPhase::WaitingForRemove,
            CallPhase::In
        ]
    );
    // The refused commit's staged state was discarded; the second one joined.
    let log = core.lock().unwrap().inner.log.clone();
    assert_eq!(
        log.iter().filter(|l| l.starts_with("joined")).count(),
        1,
        "{log:?}"
    );
}

#[tokio::test(start_paused = true)]
async fn a_rejoin_whose_call_closed_while_it_waited_registers_a_fresh_group() {
    let core = Mutex::new(gated(vec![], vec![]));
    let routes = FakeRoutes::default();
    routes
        .resync
        .lock()
        .unwrap()
        .push_back(Err(err(409, "E_REMOVE_PENDING")));
    routes.calls.lock().unwrap().extend([
        ChannelCall {
            group_id: Some(GROUP),
            call_id: Some(CHANNEL),
            participants: vec![],
        },
        ChannelCall {
            group_id: None,
            call_id: None,
            participants: vec![],
        },
    ]);
    let h = hooks();
    let joined = join_call(&core, &routes, &h, &target()).await.unwrap();
    assert_eq!(joined.group_id, FRESH);
    assert_eq!(
        routes.posted(),
        [
            "get_call",
            "get_info",
            "get_tree",
            "post_resync",
            "get_call",
            "post_group",
            "start"
        ]
    );
}

/// The last member ended the call (`DELETE ?if_alone=1` closed its group) while this device was
/// joining: the external commit meets the closed group, 409 `E_GROUP_CLOSED`. The join reads the
/// call once more and registers a fresh group, through the core's create and registration as a
/// first joiner does; it neither waits nor retries the closed group.
#[tokio::test(start_paused = true)]
async fn a_join_whose_call_ended_under_it_registers_a_fresh_group() {
    let core = Mutex::new(gated(vec![], vec![]));
    let routes = FakeRoutes::default();
    routes
        .resync
        .lock()
        .unwrap()
        .push_back(Err(err(409, "E_GROUP_CLOSED")));
    routes.calls.lock().unwrap().extend([
        ChannelCall {
            group_id: Some(GROUP),
            call_id: Some(CHANNEL),
            participants: vec![],
        },
        ChannelCall {
            group_id: None,
            call_id: None,
            participants: vec![],
        },
    ]);
    let h = hooks();
    let began = tokio::time::Instant::now();
    let joined = join_call(&core, &routes, &h, &target()).await.unwrap();
    assert_eq!(joined.group_id, FRESH);
    assert!(
        began.elapsed() < REJOIN_STEP,
        "the closed group was waited on"
    );
    assert_eq!(
        routes.posted(),
        [
            "get_call",
            "get_info",
            "get_tree",
            "post_resync",
            "get_call",
            "post_group",
            "start"
        ]
    );
    let log = core.lock().unwrap().inner.log.clone();
    assert!(log.contains(&"create 55".to_owned()), "{log:?}");
    assert!(log.contains(&"registered 55 1".to_owned()), "{log:?}");
    assert!(!log.iter().any(|l| l.starts_with("joined")), "{log:?}");
    assert_eq!(
        *h.phases.lock().unwrap(),
        [CallPhase::Joining, CallPhase::In]
    );
}

/// The fresh read is taken once: a call read that still names the closed group, refused again,
/// fails the join with that refusal.
#[tokio::test(start_paused = true)]
async fn a_second_e_group_closed_fails_the_join() {
    let core = Mutex::new(gated(vec![], vec![]));
    let routes = FakeRoutes::default();
    *routes.resync_always.lock().unwrap() = Some(err(409, "E_GROUP_CLOSED"));
    let refused = join_call(&core, &routes, &hooks(), &target())
        .await
        .unwrap_err();
    assert_eq!(refused.code, "E_GROUP_CLOSED");
    assert_eq!(refused.leave, None);
    assert_eq!(
        routes.posted(),
        [
            "get_call",
            "get_info",
            "get_tree",
            "post_resync",
            "get_call",
            "get_info",
            "get_tree",
            "post_resync"
        ]
    );
}

#[tokio::test(start_paused = true)]
async fn only_409_e_remove_pending_waits_every_other_refusal_fails_at_once() {
    for (status, code) in [
        (403, "E_REMOVE_PENDING"),
        (409, "E_GROUP_EXISTS"),
        (403, "E_FORBIDDEN"),
        (409, "E_COMMIT_CONFLICT"),
    ] {
        let core = Mutex::new(gated(vec![], vec![]));
        let routes = FakeRoutes::default();
        routes
            .resync
            .lock()
            .unwrap()
            .push_back(Err(err(status, code)));
        let h = hooks();
        let began = tokio::time::Instant::now();
        let refused = join_call(&core, &routes, &h, &target()).await.unwrap_err();
        assert_eq!(refused.code, code);
        assert!(began.elapsed() < REJOIN_STEP, "{status} {code} waited");
        assert_eq!(
            *h.phases.lock().unwrap(),
            [CallPhase::Joining, CallPhase::Idle],
            "{status} {code}"
        );
        assert!(!routes.posted().contains(&"start".to_owned()));
    }
}

#[tokio::test(start_paused = true)]
async fn a_leave_during_the_rejoin_wait_ends_it_with_the_refusal_and_posts_nothing_more() {
    let core = Mutex::new(gated(vec![], vec![]));
    let routes = std::sync::Arc::new(FakeRoutes::default());
    routes
        .resync
        .lock()
        .unwrap()
        .push_back(Err(err(409, "E_REMOVE_PENDING")));
    let h = std::sync::Arc::new(hooks());
    let cancel = {
        let h = h.clone();
        tokio::spawn(async move {
            tokio::time::sleep(std::time::Duration::from_millis(200)).await;
            h.cancel.store(true, std::sync::atomic::Ordering::SeqCst);
        })
    };
    let refused = join_call(&core, &*routes, &*h, &target())
        .await
        .unwrap_err();
    cancel.await.unwrap();
    assert_eq!(refused.code, "E_REMOVE_PENDING");
    assert_eq!(refused.leave, None);
    // Nothing after the refused resync: the step woke cancelled and read nothing.
    assert_eq!(
        routes.posted(),
        ["get_call", "get_info", "get_tree", "post_resync"]
    );
    assert_eq!(
        *h.phases.lock().unwrap(),
        [
            CallPhase::Joining,
            CallPhase::WaitingForRemove,
            CallPhase::Idle
        ]
    );
}

// ---- against a real dillad ----

/// An HTTP transport that records every route it is handed and refuses it: the call stack's HTTP side.
#[derive(Clone, Default)]
struct CountingHttp(std::sync::Arc<Mutex<Vec<String>>>);

#[async_trait::async_trait]
impl dilla_client::seams::Http for CountingHttp {
    async fn call<R: dilla_client::seams::Route>(
        &self,
        _path_args: &[&str],
        _req: R::Request,
    ) -> Result<R::Response, dilla_client::seams::HttpError> {
        self.0
            .lock()
            .unwrap()
            .push(format!("{} {}", R::METHOD, R::PATH));
        Err(dilla_client::seams::HttpError::Transport(
            "counted, not sent".into(),
        ))
    }
}

/// e2e/web/socket-only.spec.ts "joining and leaving voice goes over the socket": a person makes a server, joins its
/// voice channel and leaves; the call's every route goes over the gateway socket (the HTTP side sees none of them,
/// and the instance serves none of them over HTTP).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn call_signalling_uses_socket_when_up() {
    use dilla_client::calls::CallTarget;
    let host = tokio::task::spawn_blocking(|| world::start_host("socket"))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let ada = world::sign_up(&host, &invite, "ada", "socket").await;
    let (host, ada, community, voice) = tokio::task::spawn_blocking(move || {
        let (c, v) = world::voice_community(&host, &ada, &[]);
        world::clear_session(&ada);
        (host, ada, c, v)
    })
    .await
    .unwrap();
    let client = world::start(&ada).await;
    world::connected(&client).await;

    let http = CountingHttp::default();
    let calls = client.calls_with(http.clone());
    let join = calls.join(CallTarget {
        channel: voice,
        community: Some(community),
        vdec: String::new(),
    });
    let phase = join.phase();
    let joined = join.outcome().await.expect("client").expect("joined");
    assert_eq!(*phase.borrow(), CallPhase::In);
    // Alone: it registered the call group and started the call (201), with its own leaf in the join epoch.
    assert!(joined.started.created);
    assert_eq!(joined.started.group_id, joined.group_id);
    assert_eq!(joined.media.roster.len(), 1);
    assert_eq!(joined.media.roster[0].1, ada.device_id);
    assert!(!joined.started.token.is_empty());

    // The hourly rotation: an empty commit over the socket, and the next epoch's keys.
    let seq = calls
        .rotate(joined.group_id)
        .await
        .unwrap()
        .expect("client")
        .expect("rotated");
    assert!(seq > 0);
    let next = calls
        .media_epoch(&joined.group_id)
        .expect("the gate passes");
    assert_eq!(next.epoch, joined.media.epoch + 1);

    // A decline of a ring that never rang is the instance's no-op, answered over the socket.
    let declined = calls
        .decline(joined.started.call_id, 0)
        .await
        .unwrap()
        .expect("client");
    assert!(
        declined.as_ref().is_ok_or_status(),
        "the decline went over the socket: {declined:?}"
    );

    // The leave: alone, so the call is ended conditionally (DELETE ?if_alone=1), then the group is forgotten.
    let left = calls
        .leave(joined.group_id, joined.started.call_id, true)
        .await
        .unwrap()
        .expect("client");
    assert!(left.ended, "{left:?}");
    let after = calls
        .routes()
        .get_channel_call(&voice)
        .await
        .expect("the call read");
    assert_eq!(after.call_id, None, "the call ended");

    let seen = http.0.lock().unwrap().clone();
    assert!(
        seen.is_empty(),
        "call routes handed to HTTP while the socket was up: {seen:?}"
    );
    drop(client);
    drop(host);
}

trait OkOrStatus {
    fn is_ok_or_status(&self) -> bool;
}

impl OkOrStatus for Result<&(), &RouteError> {
    /// Answered by the instance: a success or a refusal with its status (never a transport failure).
    fn is_ok_or_status(&self) -> bool {
        match self {
            Ok(()) => true,
            Err(e) => e.status.is_some() && e.code.is_some(),
        }
    }
}

/// e2e/voice/leave-reload.spec.ts "a rejoin whose Remove nobody has committed yet waits, says so, and joins the same
/// call once it is committed" (the task-10 fix round's wait itself): alice drops out of the call (her media leaves the
/// room, so the instance proposes the Remove of her leaf), her client forgets the group as a reloaded page's sweep
/// does, and she rejoins at once. The member that would commit the Remove is the testkit's native peer, which holds a
/// leaf of the call group, never enters the room and commits only when told. Her join waits
/// (`CallPhase::WaitingForRemove`), lands in the same call group once the peer commits the Remove, and the roster holds
/// exactly one leaf of her device.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn rejoin_waits_for_pending_remove() {
    use dilla_client::calls::CallTarget;
    use serde_json::json;
    use world::{Driver, MediaPeer, hex};

    // Built before the call starts (MediaPeer::build): a cold build between alice's join and her media is minutes in
    // the call with no room presence, which the instance acts on before the rejoin this test is about.
    tokio::task::spawn_blocking(MediaPeer::build).await.unwrap();
    let host = tokio::task::spawn_blocking(|| world::start_host("rejoin"))
        .await
        .unwrap();
    let (host, mut peer, setup) = tokio::task::spawn_blocking(move || {
        let mut peer = Driver::start(&host);
        let setup = peer.request(
            "setup",
            json!({"community": "c", "channel": "general", "voice": true}),
        );
        (host, peer, setup)
    })
    .await
    .unwrap();
    let community = world::unhex16(setup["community_id"].as_str().unwrap());
    let voice_hex = setup["voice_channel_id"].as_str().unwrap().to_owned();
    let voice = world::unhex16(&voice_hex);
    let peer_device = setup["device_id"].as_str().unwrap().to_owned();
    let code = setup["invite_code"].as_str().unwrap().to_owned();

    let invite = host.invites.last().unwrap().clone();
    let alice = world::sign_up(&host, &invite, "alice", "rejoin").await;
    let (host, alice) = tokio::task::spawn_blocking(move || {
        world::ds_of(&host, &alice)
            .join_community(&community, &code)
            .unwrap();
        world::clear_session(&alice);
        (host, alice)
    })
    .await
    .unwrap();
    let a_dev = hex(&alice.device_id);
    let client = world::start(&alice).await;
    world::connected(&client).await;
    let calls = client.calls();
    let target = CallTarget {
        channel: voice,
        community: Some(community),
        vdec: String::new(),
    };

    // The peer registers the call group; alice joins it and enters the room.
    let (mut peer, joined_peer) = tokio::task::spawn_blocking({
        let voice_hex = voice_hex.clone();
        move || {
            let j = peer.request("call_join", json!({"actor": "peer", "channel": voice_hex}));
            (peer, j)
        }
    })
    .await
    .unwrap();
    let group_hex = joined_peer["group"].as_str().unwrap().to_owned();
    let joined = calls
        .join(target.clone())
        .outcome()
        .await
        .unwrap()
        .expect("alice joins");
    assert_eq!(hex(&joined.group_id), group_hex);
    let mut media = MediaPeer::start(
        &host.rtc_url(&joined.started.livekit_url),
        &joined.started.token,
        440,
    );
    media.epoch(&joined.media);
    media.next("connected", Duration::from_secs(30));
    let read_channel = voice_hex.clone();
    let read = move |peer: &mut Driver| {
        peer.request(
            "call_read",
            json!({"actor": "peer", "channel": read_channel}),
        )
    };
    let deadline = Instant::now() + Duration::from_secs(30);
    let mut peer = tokio::task::spawn_blocking(move || {
        loop {
            let r = read(&mut peer);
            if r["participants"]
                .as_array()
                .is_some_and(|p| p.iter().any(|p| p["device"] == a_dev.as_str()))
            {
                break;
            }
            assert!(
                Instant::now() < deadline,
                "alice never reached the room: {r}"
            );
            std::thread::sleep(Duration::from_millis(250));
        }
        // Alice drops out: her media leaves the room (the reloaded page's pagehide leave), so the instance proposes
        // the Remove of her leaf. The peer has not synced since, so nobody commits it.
        media.quit();
        let deadline = Instant::now() + Duration::from_secs(30);
        loop {
            let r = read(&mut peer);
            if r["participants"].as_array().is_some_and(Vec::is_empty) {
                break;
            }
            assert!(Instant::now() < deadline, "alice never left the room: {r}");
            std::thread::sleep(Duration::from_millis(250));
        }
        peer
    })
    .await
    .unwrap();
    // The reloaded page's sweep: her own Remove (refused while the instance's stands) and the group forgotten.
    let left = calls
        .leave(joined.group_id, joined.started.call_id, false)
        .await
        .unwrap()
        .expect("client");
    assert!(!left.ended);

    // She rejoins at once: the instance refuses her resync (409 E_REMOVE_PENDING) and the join waits.
    let rejoin = calls.join(target.clone());
    let mut phase = rejoin.phase();
    tokio::time::timeout(
        Duration::from_secs(5),
        phase.wait_for(|p| *p == CallPhase::WaitingForRemove),
    )
    .await
    .expect("the rejoin waits for the Remove")
    .unwrap();
    // It stays waiting while nobody commits.
    tokio::time::sleep(REJOIN_STEP * 2).await;
    assert_eq!(*phase.borrow(), CallPhase::WaitingForRemove);

    // The peer syncs and commits the Remove: the wait sees the group move on, and the join lands in it.
    let voice_for_peer = voice_hex.clone();
    let (mut peer, members) = tokio::task::spawn_blocking(move || {
        let m = peer.request(
            "call_members",
            json!({"actor": "peer", "channel": voice_for_peer}),
        );
        (peer, m)
    })
    .await
    .unwrap();
    assert_eq!(
        members["devices"],
        json!([peer_device.clone()]),
        "the peer committed alice's Remove: {members}"
    );
    let rejoined = tokio::time::timeout(Duration::from_secs(30), rejoin.outcome())
        .await
        .expect("the rejoin ends")
        .unwrap()
        .expect("alice is in once the peer committed her Remove");
    assert_eq!(*phase.borrow(), CallPhase::In);
    assert_eq!(hex(&rejoined.group_id), group_hex, "the same call group");
    let own: Vec<_> = rejoined
        .media
        .roster
        .iter()
        .filter(|(_, d, _)| *d == alice.device_id)
        .collect();
    assert_eq!(own.len(), 1, "exactly one leaf of alice's device");
    assert_eq!(rejoined.media.roster.len(), 2);
    assert!(rejoined.media.epoch > joined.media.epoch + 1);

    // The peer's roster agrees: one leaf of each device.
    let voice_for_peer = voice_hex.clone();
    let members = tokio::task::spawn_blocking(move || {
        let m = peer.request(
            "call_members",
            json!({"actor": "peer", "channel": voice_for_peer}),
        );
        drop(peer);
        m
    })
    .await
    .unwrap();
    let mut expected = vec![hex(&alice.device_id), peer_device];
    expected.sort();
    assert_eq!(members["devices"], json!(expected));
    drop(client);
    drop(host);
}

/// Alice's calls, her group and her call: the end [`EndBeforeResync`] makes.
type AliceEnds = (dilla_client::calls::Calls, [u8; 16], [u8; 16]);

/// Bob's call routes, with one step in: just before his external commit is posted, alice (alone in the call) leaves
/// and so ends it (`DELETE ?if_alone=1` closes its group). Every answer of the commit is recorded.
struct EndBeforeResync {
    inner: std::sync::Arc<dilla_client::calls::SocketCallRoutes<dilla_client::calls::NoHttp>>,
    /// Taken by the first resync.
    end: Mutex<Option<AliceEnds>>,
    resyncs: std::sync::Arc<Mutex<Vec<Result<u64, RouteError>>>>,
}

#[async_trait::async_trait]
impl CallRoutes for EndBeforeResync {
    async fn get_channel_call(&self, c: &[u8; 16]) -> Result<ChannelCall, RouteError> {
        self.inner.get_channel_call(c).await
    }
    async fn post_group(&self, b: Vec<u8>) -> Result<u64, RouteError> {
        self.inner.post_group(b).await
    }
    async fn get_group_info(&self, g: &[u8; 16]) -> Result<Vec<u8>, RouteError> {
        self.inner.get_group_info(g).await
    }
    async fn get_group_tree(&self, g: &[u8; 16]) -> Result<Vec<u8>, RouteError> {
        self.inner.get_group_tree(g).await
    }
    async fn post_resync(&self, g: &[u8; 16], b: Vec<u8>) -> Result<u64, RouteError> {
        let end = self.end.lock().unwrap().take();
        if let Some((alice, group, call)) = end {
            let left = alice
                .leave(group, call, true)
                .await
                .unwrap()
                .expect("alice's client");
            assert!(left.ended, "alice, alone, ends the call: {left:?}");
        }
        let answer = self.inner.post_resync(g, b).await;
        self.resyncs.lock().unwrap().push(answer.clone());
        answer
    }
    async fn post_commit(
        &self,
        g: &[u8; 16],
        b: Vec<u8>,
        idem: [u8; 16],
    ) -> Result<u64, RouteError> {
        self.inner.post_commit(g, b, idem).await
    }
    async fn post_proposal(&self, g: &[u8; 16], b: Vec<u8>) -> Result<u64, RouteError> {
        self.inner.post_proposal(g, b).await
    }
    async fn get_handshakes(
        &self,
        g: &[u8; 16],
        from: u64,
        limit: u64,
    ) -> Result<HandshakePage, RouteError> {
        self.inner.get_handshakes(g, from, limit).await
    }
    async fn start_call(&self, c: &[u8; 16], v: &str) -> Result<CallStarted, RouteError> {
        self.inner.start_call(c, v).await
    }
    async fn end_call_if_alone(&self, c: &[u8; 16]) -> Result<bool, RouteError> {
        self.inner.end_call_if_alone(c).await
    }
    async fn decline_call(&self, c: &[u8; 16], t: u64) -> Result<(), RouteError> {
        self.inner.decline_call(c, t).await
    }
}

/// The call-join race against a real dillad: alice is alone in a call and leaves (ending it, `DELETE ?if_alone=1`
/// closes its group) while bob joins, after bob read the call's GroupInfo and tree and before his external commit is
/// posted. The instance refuses that commit as closed (409 `E_GROUP_CLOSED`, never `E_COMMIT_INVALID` under rule
/// `epoch`), and bob's join reads the call once more and registers a fresh call group, through the same core create and
/// registration as a first joiner: he ends in a call of his own, its only leaf.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_join_racing_the_last_members_end_registers_a_fresh_call() {
    use dilla_client::calls::CallTarget;

    let host = tokio::task::spawn_blocking(|| world::start_host("closed"))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let alice = world::sign_up(&host, &invite, "alice", "closed").await;
    let bob = world::sign_up(&host, &invite, "bob", "closed").await;
    let (host, alice, bob, community, voice) = tokio::task::spawn_blocking(move || {
        let (c, v) = world::voice_community(&host, &alice, &[&bob]);
        world::clear_session(&alice);
        world::clear_session(&bob);
        (host, alice, bob, c, v)
    })
    .await
    .unwrap();
    let a_client = world::start(&alice).await;
    let b_client = world::start(&bob).await;
    world::connected(&a_client).await;
    world::connected(&b_client).await;
    let target = CallTarget {
        channel: voice,
        community: Some(community),
        vdec: String::new(),
    };

    // Alice opens the call alone: she registers its group and starts it.
    let a_calls = a_client.calls();
    let first = a_calls
        .join(target.clone())
        .outcome()
        .await
        .expect("alice's client")
        .expect("alice joins");
    assert!(first.started.created);

    // Bob joins it; alice ends the call just before his external commit is posted.
    let b_calls = b_client.calls();
    let resyncs = std::sync::Arc::new(Mutex::new(Vec::new()));
    let join = b_calls.join_through(target.clone(), {
        let end = Some((a_calls.clone(), first.group_id, first.started.call_id));
        let resyncs = resyncs.clone();
        move |inner| EndBeforeResync {
            inner,
            end: Mutex::new(end),
            resyncs,
        }
    });
    let phase = join.phase();
    let joined = tokio::time::timeout(Duration::from_secs(60), join.outcome())
        .await
        .expect("bob's join ends")
        .expect("bob's client")
        .expect("bob is in a fresh call");
    assert_eq!(*phase.borrow(), CallPhase::In);

    let answers = resyncs.lock().unwrap().clone();
    assert_eq!(
        answers.len(),
        1,
        "one external commit, no retry of the closed group: {answers:?}"
    );
    let refusal = answers[0]
        .as_ref()
        .expect_err("the closed group refused the commit");
    assert!(refusal.is(409, "E_GROUP_CLOSED"), "{refusal:?}");

    // A fresh call group, registered by bob, with his device its only leaf.
    assert_ne!(joined.group_id, first.group_id, "a fresh call group");
    assert!(joined.started.created, "bob started the call");
    assert_eq!(joined.started.group_id, joined.group_id);
    assert_eq!(joined.media.roster.len(), 1);
    assert_eq!(joined.media.roster[0].1, bob.device_id);
    let read = b_calls
        .routes()
        .get_channel_call(&voice)
        .await
        .expect("the call read");
    assert_eq!(
        read.group_id,
        Some(joined.group_id),
        "the channel's call is bob's"
    );

    let left = b_calls
        .leave(joined.group_id, joined.started.call_id, true)
        .await
        .unwrap()
        .expect("bob's client");
    assert!(left.ended, "{left:?}");
    drop(b_client);
    drop(a_client);
    drop(host);
}

/// A leave that lands while the call's start is in flight: the start succeeded, so the device is admitted; the join
/// leaves at once (its self-Remove and the forget) and ends cancelled, never handing out the call.
#[tokio::test]
async fn a_leave_during_the_start_leaves_the_started_call_and_ends_cancelled() {
    let core = Mutex::new(gated(vec![], vec![]));
    let h = hooks();
    let routes = FakeRoutes {
        cancel_on_start: Some(h.cancel.clone()),
        ..FakeRoutes::default()
    };
    let refused = join_call(&core, &routes, &h, &target()).await.unwrap_err();
    assert_eq!(refused.code, "E_NO_CALL");
    assert_eq!(refused.leave, None, "already left");
    assert_eq!(
        routes.posted(),
        [
            "get_call",
            "get_info",
            "get_tree",
            "post_resync",
            "start",
            "post_proposal remove"
        ]
    );
    assert_eq!(
        core.lock().unwrap().inner.log,
        [
            "join_external 44 info",
            "joined 44 9",
            "leave_proposal 44",
            "forget 44"
        ]
    );
    assert_eq!(
        *h.phases.lock().unwrap(),
        [CallPhase::Joining, CallPhase::Idle]
    );
}
