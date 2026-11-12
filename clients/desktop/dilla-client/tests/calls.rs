//! Task 11: the call signalling (`dilla_client::calls`) over a scripted core and instance, in the
//! order join.ts and controller.ts take, and the wire decoders against bodies built as dillad and
//! dilla-core build them.

use std::collections::VecDeque;
use std::sync::Mutex;

use dilla_client::calls::wire::{decode_call_started, decode_channel_call, decode_media_epoch};
use dilla_client::calls::{
    CallCore, CallRoutes, CallStarted, ChannelCall, CoreRefusal, GroupSummary, JoinHooks,
    JoinTarget, RouteError, join_call, leave_call, media_epoch, rotate_call,
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
    fn call_media_epoch(&self, _id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        Ok(epoch_body(3, &[(0, ME), (1, PEER)]))
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
    fn call_media_epoch(&self, id: &[u8; 16]) -> Result<Vec<u8>, CoreRefusal> {
        self.media
            .lock()
            .unwrap()
            .pop_front()
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

#[derive(Default)]
struct FakeRoutes {
    log: Mutex<Vec<String>>,
    calls: Mutex<VecDeque<ChannelCall>>,
    post_group: Mutex<VecDeque<Result<u64, RouteError>>>,
    resync: Mutex<VecDeque<Result<u64, RouteError>>>,
    start: Mutex<VecDeque<Result<(), RouteError>>>,
    commit: Mutex<VecDeque<Result<u64, RouteError>>>,
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
        Ok(b"info".to_vec())
    }
    async fn get_group_tree(&self, _g: &[u8; 16]) -> Result<Vec<u8>, RouteError> {
        self.log("get_tree".into());
        Ok(b"tree".to_vec())
    }
    async fn post_resync(&self, _g: &[u8; 16], _b: Vec<u8>) -> Result<u64, RouteError> {
        self.log("post_resync".into());
        self.resync.lock().unwrap().pop_front().unwrap_or(Ok(9))
    }
    async fn post_commit(&self, _g: &[u8; 16], _b: Vec<u8>) -> Result<u64, RouteError> {
        self.log("post_commit".into());
        self.commit.lock().unwrap().pop_front().unwrap_or(Ok(10))
    }
    async fn post_proposal(&self, _g: &[u8; 16], b: Vec<u8>) -> Result<u64, RouteError> {
        self.log(format!("post_proposal {}", String::from_utf8_lossy(&b)));
        Ok(11)
    }
    async fn start_call(&self, _c: &[u8; 16], _v: &str) -> Result<CallStarted, RouteError> {
        self.log("start".into());
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
        false
    }
}

fn hooks() -> Hooks {
    Hooks {
        refreshed: Mutex::new(0),
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

#[tokio::test]
async fn a_rotation_is_confirmed_when_posted_and_aborted_when_refused() {
    let core = Mutex::new(FakeCore::default());
    let routes = FakeRoutes::default();
    assert_eq!(rotate_call(&core, &routes, &GROUP).await.unwrap(), 10);
    routes
        .commit
        .lock()
        .unwrap()
        .push_back(Err(err(409, "E_COMMIT_CONFLICT")));
    assert!(rotate_call(&core, &routes, &GROUP).await.is_err());
    assert_eq!(
        core.lock().unwrap().log,
        ["rotate 44", "confirm 44", "rotate 44", "abort 44"]
    );
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
