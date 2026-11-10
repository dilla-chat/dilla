//! `dilla-testkit netprobe`: the real-network voice probe (L-PROBE-1).
//!
//! Three verbs, because a call group does not wait: a device whose leaf is in a live call's group
//! but which stays out of the room past `admissionJoinWindow` (30 s) loses its leaf to the room
//! sweep; a room nobody joins is reaped after 300 s; an ended call closes its group; and the
//! instance closes an open call group no live call names 600 s after it was created (L-HTTP-105).
//! So the accounts and their DM are made once per pair, and the call group once per run, on
//! demand, on each side's own machine: no secret crosses machines after the pair's first copy.
//!
//! - `prepare` signs up two throwaway native accounts through an invite (`netprobe-a-<6 hex>` and
//!   `netprobe-b-<6 hex>`, one random suffix per pair: a deleted handle stays reserved), opens a
//!   1:1 DM from A to B, registers the DM's call group as A and joins it by external commit as B,
//!   so both leaves are in one epoch. No call is started. Each side's device state (identity seed,
//!   ids, MLS store) is written to `<out>/side-a/` and `<out>/side-b/`.
//! - `arm` readies one side for the next run against `GET /v1/channels/{dm}/call` (L-HTTP-100):
//!   side a registers a fresh call group unless its own is still the open one, side b joins the
//!   open group by external commit unless it already holds it. Re-running it is a no-op, and it
//!   refuses while a call is live. Each run arms side a, then side b, then starts both.
//! - `start` loads one side, signs in with a fresh challenge-response session, syncs the call
//!   group (side a merges side b's external commit here), starts (or joins) the DM's call and
//!   writes the start JSON the probe page loads.
//!
//! Public routes only, with one exception: the account's authenticator-app enrolment (every account
//! owes a second factor) reads `/debug/clock` when `DILLA_TESTKIT_CONTROL` names a test host's
//! control listener, and the machine's clock otherwise. A real instance runs on real time, and the
//! one confirm is the only code spent (`start` re-establishes with the device key), so the probe
//! runs against a real instance as it runs against `dilla-testhost`. The testkit speaks
//! plain `http://` only; against a TLS instance both verbs go through the probe page's dev server,
//! which proxies `/v1`, `/gateway` and `/rtc` to it (`packages/media/vite.probe.config.ts`).

use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use dilla_core::identity::{Kind, Tier};
use dilla_core::ids::{InstanceId, UserId};
use dilla_core::mls::{
    DillaBinding, DillaGroup, DillaProcessed, DillaProvider, GroupKind, RosterEntry,
    external_senders,
};
use openmls::prelude::{GroupId, MlsMessageIn, ProtocolMessage};
use openmls_rust_crypto::RustCrypto;
use openmls_traits::random::OpenMlsRand;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};

use crate::ds::remote::{CallStarted, instance_document};
use crate::{DeliveryService, DsError, HttpDs, TestClient, TestkitError};

/// The side's MLS store inside its state directory: a SQLite copy of the in-memory store
/// `prepare` built, which `start` opens in place.
const MLS_FILE: &str = "mls.sqlite";
/// The side's identity and ids inside its state directory.
const SIDE_FILE: &str = "side.json";
/// What a refusal of an unusable state directory tells the operator.
const PREPARE_AGAIN: &str = "run prepare again";
/// What a refusal of a closed or replaced call group tells the operator.
const ARM_AGAIN: &str = "run arm again (side a, then side b)";

fn scenario(msg: impl Into<String>) -> TestkitError {
    TestkitError::Scenario(msg.into())
}

/// One side's identity and ids, `side.json`. The seed regenerates every key of the device, so the
/// directory is a throwaway device's secret and is deleted after the session.
#[derive(Clone, Debug, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "camelCase", deny_unknown_fields)]
pub(crate) struct SideState {
    pub(crate) v: u32,
    pub(crate) side: String,
    pub(crate) username: String,
    /// Decimal: a u64 does not survive every JSON reader.
    pub(crate) seed: String,
    pub(crate) user_id: String,
    pub(crate) device_id: String,
    /// The DM's channel id: the call group's binding target and the calls route's channel.
    pub(crate) dm: String,
    /// The call group as the instance registered it.
    pub(crate) group_id: String,
}

impl SideState {
    pub(crate) fn write(&self, dir: &Path) -> Result<(), TestkitError> {
        let body = serde_json::to_vec_pretty(self).map_err(|e| scenario(e.to_string()))?;
        write_private(&dir.join(SIDE_FILE), &body)
    }

    fn read(dir: &Path) -> Result<Self, TestkitError> {
        let path = dir.join(SIDE_FILE);
        let body = std::fs::read(&path).map_err(|e| {
            scenario(format!(
                "{}: {e}; is this a side directory netprobe prepare wrote? {PREPARE_AGAIN}",
                path.display()
            ))
        })?;
        let s: Self = serde_json::from_slice(&body)
            .map_err(|e| scenario(format!("{}: {e}", path.display())))?;
        if s.v != 1 {
            return Err(scenario(format!(
                "{}: version {} is not 1; {PREPARE_AGAIN}",
                path.display(),
                s.v
            )));
        }
        Ok(s)
    }

    fn seed(&self) -> Result<u64, TestkitError> {
        self.seed
            .parse()
            .map_err(|e| scenario(format!("side.json seed: {e}")))
    }
}

fn hex16(s: &str, what: &str) -> Result<[u8; 16], TestkitError> {
    let bytes = hex::decode(s).map_err(|e| scenario(format!("{what}: {e}")))?;
    bytes
        .try_into()
        .map_err(|_| scenario(format!("{what} is not 16 bytes")))
}

/// Creates `path` readable by its owner only: a side's files are a device's secrets.
fn write_private(path: &Path, body: &[u8]) -> Result<(), TestkitError> {
    use std::io::Write;
    let mut opts = std::fs::OpenOptions::new();
    opts.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::OpenOptionsExt;
        opts.mode(0o600);
    }
    let mut f = opts
        .open(path)
        .map_err(|e| scenario(format!("{}: {e}", path.display())))?;
    f.write_all(body)
        .map_err(|e| scenario(format!("{}: {e}", path.display())))
}

fn create_private_dir(path: &Path) -> Result<(), TestkitError> {
    let mut b = std::fs::DirBuilder::new();
    b.recursive(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        b.mode(0o700);
    }
    b.create(path)
        .map_err(|e| scenario(format!("{}: {e}", path.display())))
}

/// The two account names of one run: `netprobe-a-<suffix>` and `netprobe-b-<suffix>`.
pub(crate) fn probe_names(suffix: [u8; 3]) -> (String, String) {
    let s = hex::encode(suffix);
    (format!("netprobe-a-{s}"), format!("netprobe-b-{s}"))
}

/// A fresh suffix from the OS RNG (through the crypto provider the MLS stack already uses).
pub(crate) fn random_suffix() -> Result<[u8; 3], TestkitError> {
    RustCrypto::default()
        .random_array::<3>()
        .map_err(|e| scenario(format!("random suffix: {e:?}")))
}

fn random_seed() -> Result<u64, TestkitError> {
    RustCrypto::default()
        .random_array::<8>()
        .map(u64::from_be_bytes)
        .map_err(|e| scenario(format!("random seed: {e:?}")))
}

/// The LiveKit signalling URL the page dials. A loopback `http://` instance is the automated check
/// against `dilla-testhost`, or the probe page's own dev server proxying a real instance (the
/// session checklist): the instance's own origin with `http` replaced by `ws`, the rule
/// `media_driver.rs` uses, keeps the `/rtc` gate in the path. Anything else is what the calls
/// route answered.
pub(crate) fn livekit_url(instance: &str, started: &CallStarted) -> String {
    let base = instance.trim_end_matches('/');
    if let Some(rest) = base.strip_prefix("http://") {
        let host = rest.split('/').next().unwrap_or(rest);
        let name = host.rsplit_once(':').map_or(host, |(h, _)| h);
        if matches!(name, "127.0.0.1" | "localhost" | "[::1]") {
            return format!("ws://{rest}");
        }
    }
    started.livekit_url.clone()
}

/// What `start` hands the page: the call group's media epoch, read from the merged state.
pub(crate) struct MediaKeys {
    pub(crate) epoch: u64,
    pub(crate) base_key: [u8; 16],
    pub(crate) own_leaf: u32,
    pub(crate) roster: Vec<RosterEntry>,
}

/// The start JSON (L-PROBE-1): `{ v: 1, livekitUrl, token, iceServers, callId, groupId,
/// keys: { epoch, baseKeyHex, selfLeaf, roster: [{ leaf, deviceId }] } }`.
pub(crate) fn start_json(livekit_url: &str, started: &CallStarted, keys: &MediaKeys) -> Value {
    let ice: Vec<Value> = started
        .ice_servers
        .iter()
        .map(|(urls, user, cred)| json!([urls, user, cred]))
        .collect();
    let roster: Vec<Value> = keys
        .roster
        .iter()
        .map(|r| json!({"leaf": r.leaf_index, "deviceId": r.device_id.to_hex()}))
        .collect();
    json!({
        "v": 1,
        "livekitUrl": livekit_url,
        "token": started.token,
        "iceServers": ice,
        "callId": hex::encode(started.call_id),
        "groupId": hex::encode(started.group_id),
        "keys": {
            "epoch": keys.epoch.to_string(),
            "baseKeyHex": hex::encode(keys.base_key),
            "selfLeaf": keys.own_leaf,
            "roster": roster,
        },
    })
}

/// The calls route's answer, judged against the state's call group. A closed group is refused
/// `E_LEAF_NOT_CURRENT`; a channel whose only call group is closed has none (`E_NOT_FOUND`); and a
/// call opened on another, newer group means this one is closed too. Each needs a fresh call group:
/// `arm` on side a, then on side b.
pub(crate) fn check_started(
    group: &[u8; 16],
    result: Result<CallStarted, DsError>,
) -> Result<CallStarted, TestkitError> {
    match result {
        Ok(s) if &s.group_id == group => Ok(s),
        Ok(s) => Err(scenario(format!(
            "the call opened on call group {}, not this state's {}: its group is closed; {ARM_AGAIN}",
            hex::encode(s.group_id),
            hex::encode(group)
        ))),
        Err(e) if matches!(e.code(), "E_LEAF_NOT_CURRENT" | "E_NOT_FOUND") => {
            Err(scenario(format!(
                "the call group {} is closed or this device's leaf left it ({e}); {ARM_AGAIN}",
                hex::encode(group)
            )))
        }
        Err(e) => Err(e.into()),
    }
}

/// `start` hands out keys only for an epoch that holds both sides.
pub(crate) fn check_roster(members: usize) -> Result<(), TestkitError> {
    if members < 2 {
        return Err(scenario(format!(
            "the call group holds {members} leaf: arm the other side, then run start again"
        )));
    }
    Ok(())
}

/// Which side of a pair a state directory is.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum Side {
    /// Registers the run's call group.
    A,
    /// Joins it by external commit.
    B,
}

/// What `GET /v1/channels/{id}/call` (L-HTTP-100) answers, as far as `arm` reads it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub(crate) struct ChannelCall {
    /// The live call's group, else the channel's newest open call group, else none.
    pub(crate) group_id: Option<[u8; 16]>,
    pub(crate) call_id: Option<[u8; 16]>,
    pub(crate) participants: usize,
}

/// `[group_id(bstr16|null), call_id(bstr16|null), participants([[user_id, device_id, flags]])]`.
pub(crate) fn decode_channel_call(body: &[u8]) -> Result<ChannelCall, TestkitError> {
    dilla_core::cbor::decode_strict(body, |d| {
        d.array(3)?;
        let group_id = d.opt_bytes_exact::<16>()?;
        let call_id = d.opt_bytes_exact::<16>()?;
        // A Go nil slice is encoded as null.
        let n = if d.try_null()? { 0 } else { d.array_len()? };
        for _ in 0..n {
            d.array(3)?;
            d.bytes_exact::<16>()?;
            d.bytes_exact::<16>()?;
            d.uint()?;
        }
        Ok(ChannelCall {
            group_id,
            call_id,
            participants: n,
        })
    })
    .map_err(|e| scenario(format!("GET /v1/channels/{{id}}/call: {e:?}")))
}

/// What `arm` does to a side.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum ArmAction {
    /// The side already holds the open group: nothing to do.
    Keep,
    /// Side a registers a fresh call group.
    Register,
    /// Side b joins this open group by external commit.
    Join([u8; 16]),
}

/// The arm decision. `stored` is the group `side.json` names, `held` whether the side's MLS store
/// holds it, `served` what the instance answers now. While a call is live, a new group would not
/// be the one `start` lands in (the live call's group wins), so arm waits for it to end.
pub(crate) fn arm_action(
    side: Side,
    stored: Option<[u8; 16]>,
    held: bool,
    served: &ChannelCall,
) -> Result<ArmAction, TestkitError> {
    if let Some(call) = served.call_id {
        return Err(scenario(format!(
            "the previous call {} is still live ({} in its room); wait for it to end (about 20 s \
             after the last page leaves), then run arm again",
            hex::encode(call),
            served.participants
        )));
    }
    let current = held && stored.is_some() && stored == served.group_id;
    match (side, served.group_id) {
        _ if current => Ok(ArmAction::Keep),
        (Side::A, _) => Ok(ArmAction::Register),
        (Side::B, Some(open)) => Ok(ArmAction::Join(open)),
        (Side::B, None) => Err(scenario(
            "the DM has no open call group: arm side a first, then side b",
        )),
    }
}

/// A side's own session; every route it calls is a socket request on `ds`'s gateway socket.
struct Session {
    ds: HttpDs,
}

impl Session {
    fn open(base: &str, client: &TestClient) -> Result<Self, TestkitError> {
        let est = HttpDs::establish_scoped(base, &client.device(), &client.credential_blob())?;
        let mut ds = HttpDs::with_session(base, client.device_id(), est.token)?;
        ds.set_session_expires(est.expires);
        Ok(Self { ds })
    }

    /// `DELETE /v1/calls/{call_id}` → `204`: ends the call for everyone and closes its group, as a
    /// current leaf may. A socket request (Bot v2): the route has no HTTP form.
    fn end_call(&mut self, call: &[u8; 16]) -> Result<(), TestkitError> {
        let route = format!("/v1/calls/{}", hex::encode(call));
        let (status, body) = self.ds.socket_call("DELETE", &route, &[])?;
        if status != 204 {
            return Err(scenario(format!(
                "DELETE /v1/calls/{{id}} answered {status}: {}",
                String::from_utf8_lossy(&body)
            )));
        }
        Ok(())
    }

    /// `POST /v1/groups [group_id, binding, group_info, ratchet_tree]` → `[group_id, next_seq]`
    /// (invariant 1: the group id is the creator's choice), a socket request. Answers the registered id.
    fn register_group(
        &mut self,
        id: &[u8; 16],
        binding: &[u8],
        group_info: &[u8],
        ratchet_tree: &[u8],
    ) -> Result<[u8; 16], TestkitError> {
        let mut e = dilla_core::cbor::Encoder::new();
        e.array(4)
            .bytes(id)
            .bytes(binding)
            .bytes(group_info)
            .bytes(ratchet_tree);
        let body = e.into_vec();
        let (status, out) = self.ds.socket_call("POST", "/v1/groups", &body)?;
        if status != 201 && status != 200 {
            return Err(scenario(format!(
                "POST /v1/groups answered {status}: {}",
                String::from_utf8_lossy(&out)
            )));
        }
        dilla_core::cbor::decode_strict(&out, |d| {
            d.array(2)?;
            let g = d.bytes_exact::<16>()?;
            d.uint()?;
            Ok(g)
        })
        .map_err(|e| scenario(format!("POST /v1/groups: {e:?}")))
    }

    /// `GET /v1/channels/{dm}/call` (L-HTTP-100), a socket request.
    fn channel_call(&mut self, dm: &[u8; 16]) -> Result<ChannelCall, TestkitError> {
        let route = format!("/v1/channels/{}/call", hex::encode(dm));
        let (status, body) = self.ds.socket_call("GET", &route, &[])?;
        if status != 200 {
            return Err(scenario(format!(
                "GET /v1/channels/{{dm}}/call answered {status}: does this instance run the web-3 \
                 build (L-HTTP-100)?"
            )));
        }
        decode_channel_call(&body)
    }
}

/// The DM's call-group binding, with the instance as external sender (protocol/01), as a remote
/// scenario builds it.
fn call_binding(
    base: &str,
    dm: [u8; 16],
) -> Result<(DillaBinding, openmls::extensions::ExternalSendersExtension), TestkitError> {
    let doc = instance_document(base)?;
    let instance_id = InstanceId::from_bytes(doc.instance_id);
    let binding = DillaBinding {
        v: 1,
        instance_id,
        community_id: None,
        target_id: dm,
        kind: GroupKind::Call,
        policy_version: 1,
        e2ee_version: 1,
        media_version: GroupKind::Call.media_version(),
    };
    // protocol/01: every call group carries the instance as its external sender, so the instance
    // can propose the leaf removals the room sweep and a leave make.
    let senders = external_senders(doc.external_sender_pub.to_vec().into(), &instance_id);
    Ok((binding, senders))
}

/// Registers a call group under a fresh random group id, as the browser core does
/// (`call_group_create`): `prepare`'s first call group took the DM id itself (`HttpDs` registers
/// under the binding's target), and a group id is registered once. The registration is
/// `Session::register_group`'s own `POST /v1/groups`, so `ds/remote.rs` stays untouched. The group
/// lives in the client's store only (not in `TestClient`'s own map); `replace_store` persists it.
/// Answers the group id and its epoch (0).
fn register_call_group(
    client: &TestClient,
    session: &mut Session,
    binding: DillaBinding,
    senders: openmls::extensions::ExternalSendersExtension,
) -> Result<(Vec<u8>, u64), TestkitError> {
    use tls_codec::Serialize as _;
    let id = RustCrypto::default()
        .random_array::<16>()
        .map_err(|e| scenario(format!("random group id: {e:?}")))?;
    let group = DillaGroup::create(
        client.provider(),
        client.signer(),
        client.credential(),
        GroupId::from_slice(&id),
        binding.clone(),
        Some(senders),
    )?;
    let ser = |e: tls_codec::Error| scenario(format!("{e:?}"));
    let group_info = group
        .export_group_info(client.provider(), client.signer())?
        .tls_serialize_detached()
        .map_err(ser)?;
    let ratchet_tree = group
        .export_ratchet_tree()
        .tls_serialize_detached()
        .map_err(ser)?;
    let registered = session.register_group(&id, &binding.encode(), &group_info, &ratchet_tree)?;
    if registered != id {
        return Err(scenario(format!(
            "registered group {} but the instance answered {}",
            hex::encode(id),
            hex::encode(registered)
        )));
    }
    Ok((id.to_vec(), group.epoch()))
}

/// What `arm` did.
pub struct Armed {
    pub action: &'static str,
    pub group_id: String,
    pub epoch: u64,
}

/// Whether the side's store holds the call group `side.json`'s `groupId` names (the registered id
/// is also the group's local MLS id: the DM id for `prepare`'s first group, a random id for every
/// group `arm` registers). `arm` writes the store and `groupId` together.
fn holds(dir: &Path, state: &SideState) -> bool {
    if state.group_id.is_empty() {
        return false;
    }
    load_side(dir).is_ok()
}

/// The side's client, rebuilt from its seed around the user id the instance minted, with a fresh
/// in-memory store.
fn client_of(state: &SideState) -> Result<TestClient, TestkitError> {
    let user = UserId::from_bytes(hex16(&state.user_id, "side.json userId")?);
    let client = TestClient::new(
        &state.username,
        user,
        Tier::Native,
        Kind::User,
        state.seed()?,
    )?;
    if client.device_id().to_hex() != state.device_id {
        return Err(scenario(format!(
            "side.json names device {} but its seed yields {}; {PREPARE_AGAIN}",
            state.device_id,
            client.device_id().to_hex()
        )));
    }
    Ok(client)
}

/// `dilla-testkit netprobe end --instance <url> --state <dir>/side-<x>`: ends the DM's live call
/// (`DELETE /v1/calls/{id}`, as a current leaf), which closes its group. A real instance ends a
/// call by itself about 20 s after its room empties; this is for a call that does not end (a page
/// that crashed and left a ghost participant) and for a test host, which has no LiveKit webhooks.
/// Answers the call it ended, or `None` when no call was live.
pub fn end(instance: &str, dir: &Path) -> Result<Option<String>, TestkitError> {
    let base = instance.trim_end_matches('/');
    let state = SideState::read(dir)?;
    let client = client_of(&state)?;
    let dm = hex16(&state.dm, "side.json dm")?;
    let mut session = Session::open(base, &client)?;
    match session.channel_call(&dm)?.call_id {
        None => Ok(None),
        Some(call) => {
            session.end_call(&call)?;
            Ok(Some(hex::encode(call)))
        }
    }
}

/// `dilla-testkit netprobe arm --instance <url> --state <dir>/side-<x>`.
pub fn arm(instance: &str, dir: &Path) -> Result<Armed, TestkitError> {
    let base = instance.trim_end_matches('/');
    let mut state = SideState::read(dir)?;
    let side = match state.side.as_str() {
        "a" => Side::A,
        "b" => Side::B,
        other => {
            return Err(scenario(format!(
                "side.json names side {other:?}; {PREPARE_AGAIN}"
            )));
        }
    };
    // A fresh client from the seed: its in-memory store replaces the side's whole store when the
    // group is rebuilt, so the stale group (under whatever local MLS id it had) is gone with it.
    let mut client = client_of(&state)?;
    let dm = hex16(&state.dm, "side.json dm")?;
    let stored = if state.group_id.is_empty() {
        None
    } else {
        Some(hex16(&state.group_id, "side.json groupId")?)
    };
    let held = holds(dir, &state);
    let mut session = Session::open(base, &client)?;
    let served = session.channel_call(&dm)?;
    let action = arm_action(side, stored, held, &served)?;
    let (group, epoch) = match action {
        ArmAction::Keep => {
            let loaded = load_side(dir)?;
            return Ok(Armed {
                action: "kept",
                group_id: state.group_id,
                epoch: loaded.group.epoch(),
            });
        }
        ArmAction::Register => {
            let (binding, senders) = call_binding(base, dm)?;
            register_call_group(&client, &mut session, binding, senders)?
        }
        ArmAction::Join(open) => {
            let (binding, _) = call_binding(base, dm)?;
            client.join_external(&mut session.ds, &open, &binding)?;
            let epoch = client
                .epoch_of(&open)
                .ok_or_else(|| scenario("the side holds no state for the group it just joined"))?;
            (open.to_vec(), epoch)
        }
    };
    replace_store(&client, dir)?;
    state.group_id = hex::encode(&group);
    state.write(dir)?;
    Ok(Armed {
        action: if action == ArmAction::Register {
            "registered"
        } else {
            "joined"
        },
        group_id: state.group_id,
        epoch,
    })
}

/// A side as `start` uses it: its ids, its client (for the device keys and the credential), and
/// the provider and call group over the side's own MLS store.
pub(crate) struct LoadedSide {
    state: SideState,
    client: TestClient,
    provider: DillaProvider,
    group: DillaGroup,
}

/// Loads a side directory. The client is rebuilt from the seed around the user id the instance
/// minted, exactly as `prepare` built it; the call group is read from the side's MLS store.
pub(crate) fn load_side(dir: &Path) -> Result<LoadedSide, TestkitError> {
    let state = SideState::read(dir)?;
    let user = UserId::from_bytes(hex16(&state.user_id, "side.json userId")?);
    let client = TestClient::new(
        &state.username,
        user,
        Tier::Native,
        Kind::User,
        state.seed()?,
    )?;
    if client.device_id().to_hex() != state.device_id {
        return Err(scenario(format!(
            "side.json names device {} but its seed yields {}; {PREPARE_AGAIN}",
            state.device_id,
            client.device_id().to_hex()
        )));
    }
    let path = dir.join(MLS_FILE);
    if !path.is_file() {
        return Err(scenario(format!(
            "{} is missing; {PREPARE_AGAIN}",
            path.display()
        )));
    }
    let conn = rusqlite::Connection::open(&path)
        .map_err(|e| scenario(format!("{}: {e}", path.display())))?;
    let provider = DillaProvider::new(Arc::new(Mutex::new(conn)));
    provider.storage().migrate()?;
    let dm = hex16(&state.dm, "side.json dm")?;
    if state.group_id.is_empty() {
        return Err(scenario(format!(
            "side.json names no call group; {ARM_AGAIN}"
        )));
    }
    // The instance registers a group under its MLS group id, so the stored id is the local one too.
    let id = hex16(&state.group_id, "side.json groupId")?;
    let group = DillaGroup::load(&provider, &GroupId::from_slice(&id))?.ok_or_else(|| {
        scenario(format!(
            "{} holds no call group {} for DM {}; {ARM_AGAIN}",
            path.display(),
            state.group_id,
            state.dm
        ))
    })?;
    let binding = group
        .binding()
        .map_err(|e| scenario(format!("call group binding: {e}")))?;
    if binding.kind != GroupKind::Call || binding.target_id != dm {
        return Err(scenario(format!(
            "{} holds another group under DM {}; {ARM_AGAIN}",
            path.display(),
            state.dm
        )));
    }
    Ok(LoadedSide {
        state,
        client,
        provider,
        group,
    })
}

/// Copies a client's in-memory MLS store to `path` (`VACUUM INTO`), owner-only.
fn save_store(client: &TestClient, path: &Path) -> Result<(), TestkitError> {
    let target = path
        .to_str()
        .ok_or_else(|| scenario(format!("{}: not UTF-8", path.display())))?;
    if path.exists() {
        return Err(scenario(format!("{} exists already", path.display())));
    }
    {
        let conn = client
            .provider()
            .storage()
            .conn()
            .lock()
            .map_err(|_| scenario("the MLS store lock is poisoned"))?;
        conn.execute("VACUUM INTO ?1", [target])
            .map_err(|e| scenario(format!("{}: {e}", path.display())))?;
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))
            .map_err(|e| scenario(format!("{}: {e}", path.display())))?;
    }
    Ok(())
}

/// Replaces the side's MLS store with the client's: written beside it, then renamed over it.
fn replace_store(client: &TestClient, dir: &Path) -> Result<(), TestkitError> {
    let next = dir.join(format!("{MLS_FILE}.next"));
    if next.exists() {
        std::fs::remove_file(&next).map_err(|e| scenario(format!("{}: {e}", next.display())))?;
    }
    save_store(client, &next)?;
    let path = dir.join(MLS_FILE);
    std::fs::rename(&next, &path).map_err(|e| scenario(format!("{}: {e}", path.display())))
}

fn now_unix() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map(|d| d.as_secs())
        .unwrap_or(0)
}

/// Signs `name` up through `invite` with a device drawn from a fresh seed, and publishes the
/// user's signed device list (invariant 4 refuses any leaf whose DSK no list names).
fn enrol(
    base: &str,
    invite: &str,
    name: &str,
    on_account: &mut dyn FnMut(&str),
) -> Result<(TestClient, HttpDs, u64), TestkitError> {
    let seed = random_seed()?;
    let draft = TestClient::new(
        name,
        UserId::from_bytes([0; 16]),
        Tier::Native,
        Kind::User,
        seed,
    )?;
    // One code, or several separated by commas as `dilla-testhost`'s banner prints them: a spent
    // code answers E_INVITE_INVALID and the next one is tried.
    let mut codes = invite.split(',').filter(|c| !c.is_empty()).peekable();
    let enrolled = loop {
        let code = codes
            .next()
            .ok_or_else(|| scenario("the invite names no code"))?;
        match HttpDs::redeem_invite(base, code, draft.new_account(name, name)) {
            Err(e) if e.code() == "E_INVITE_INVALID" && codes.peek().is_some() => {}
            other => break other?,
        }
    };
    // The account exists from here on, whatever fails next.
    on_account(name);
    // The instance mints the user id and the MLS credential must name it; the seed fixes the
    // device and every key, so the client is rebuilt around the minted id unchanged otherwise.
    let client = TestClient::new(
        name,
        UserId::from_bytes(enrolled.user_id),
        Tier::Native,
        Kind::User,
        seed,
    )?;
    if enrolled.device_id != client.device_id() {
        return Err(DsError::Protocol(format!(
            "registered device {} but the instance answered {}",
            client.device_id().to_hex(),
            enrolled.device_id.to_hex()
        ))
        .into());
    }
    // Every account owes a second factor, and an owed session reaches nothing else (not even the
    // gateway): enrol an authenticator app before the first connection.
    let totp = HttpDs::enrol_second_factor(base, &enrolled.token, true)?;
    let mut ds = HttpDs::with_session(base, client.device_id(), enrolled.token)?;
    ds.set_totp(totp);
    ds.set_session_expires(enrolled.expires);
    ds.put_device_list(&enrolled.user_id, &client.signed_device_list(now_unix()))?;
    Ok((client, ds, seed))
}

/// What `prepare` made: the account names, the call group and where the two sides were written.
pub struct Prepared {
    pub names: (String, String),
    pub side_a: PathBuf,
    pub side_b: PathBuf,
    pub group_id: String,
    pub epoch: u64,
}

/// `dilla-testkit netprobe prepare --instance <url> --invite <code> --out <dir>`. `on_account` is
/// called with each account's name the moment its signup succeeds, before anything else can fail,
/// so a prepare that fails half-way still names every account it made for the cleanup.
pub fn prepare(
    instance: &str,
    invite: &str,
    out: &Path,
    on_account: &mut dyn FnMut(&str),
) -> Result<Prepared, TestkitError> {
    let base = instance.trim_end_matches('/');
    let side_a = out.join("side-a");
    let side_b = out.join("side-b");
    for dir in [&side_a, &side_b] {
        if dir.exists() {
            return Err(scenario(format!(
                "{} exists: a side directory is written once (delete it, or choose another --out)",
                dir.display()
            )));
        }
    }
    let (name_a, name_b) = probe_names(random_suffix()?);
    let (mut a, mut ds_a, seed_a) = enrol(base, invite, &name_a, on_account)?;
    let (mut b, mut ds_b, seed_b) = enrol(base, invite, &name_b, on_account)?;

    let (dm, _) = ds_a.post_dm(&[*b.user_id().as_bytes()])?;
    let (binding, senders) = call_binding(base, dm)?;
    let group = a.create_group_with(&mut ds_a, binding.clone(), Some(senders))?;
    b.join_external(&mut ds_b, &group, &binding)?;
    // A processes B's external commit, so both sides hold the same epoch.
    a.sync(&mut ds_a)?;
    b.sync(&mut ds_b)?;
    let (ea, eb) = (a.epoch_of(&group), b.epoch_of(&group));
    let epoch = match (ea, eb) {
        (Some(x), Some(y)) if x == y => x,
        _ => {
            return Err(scenario(format!(
                "the two sides disagree on the call group's epoch ({ea:?} and {eb:?})"
            )));
        }
    };
    let group_hex = hex::encode(&group);

    for (dir, side, client, seed) in [(&side_a, "a", &a, seed_a), (&side_b, "b", &b, seed_b)] {
        create_private_dir(dir)?;
        SideState {
            v: 1,
            side: side.into(),
            username: client.name().to_owned(),
            seed: seed.to_string(),
            user_id: hex::encode(client.user_id().as_bytes()),
            device_id: client.device_id().to_hex(),
            dm: hex::encode(dm),
            group_id: group_hex.clone(),
        }
        .write(dir)?;
        save_store(client, &dir.join(MLS_FILE))?;
    }
    Ok(Prepared {
        names: (name_a, name_b),
        side_a,
        side_b,
        group_id: group_hex,
        epoch,
    })
}

fn deserialize_protocol(bytes: &[u8]) -> Result<ProtocolMessage, TestkitError> {
    use tls_codec::Deserialize as _;
    MlsMessageIn::tls_deserialize_exact(bytes)
        .map_err(|e| scenario(format!("{e:?}")))?
        .try_into_protocol_message()
        .map_err(|e| scenario(format!("{e:?}")))
}

/// Applies the call group's handshakes, read from the group's own log from its start rather than
/// from the gateway (a fresh connection's catch-up need not reach back to a commit made before it
/// opened): commits are merged, proposals queued. Nothing is committed: the probe never moves the
/// epoch itself. A group is at most a few handshakes old here (one external join per run), far
/// inside one page.
fn sync_group(side: &mut LoadedSide, ds: &mut HttpDs) -> Result<(), TestkitError> {
    let group_id = hex16(&side.state.group_id, "side.json groupId")?;
    for item in ds.handshakes(&group_id, 0)? {
        let message = deserialize_protocol(&item.blob)?;
        if message.epoch().as_u64() < side.group.epoch() {
            continue;
        }
        match side.group.process_message(&side.provider, message)? {
            DillaProcessed::StagedCommit(staged, _) => {
                side.group.merge_staged_commit(&side.provider, *staged)?;
            }
            DillaProcessed::Proposal(p) | DillaProcessed::ExternalJoinProposal(p) => {
                side.group.store_pending_proposal(&side.provider, *p)?;
            }
            _ => {}
        }
    }
    Ok(())
}

/// `dilla-testkit netprobe start --instance <url> --state <dir>/side-<x> --out <file>`: writes the
/// start JSON to `out` and answers it.
pub fn start(instance: &str, state: &Path, out: &Path) -> Result<Value, TestkitError> {
    let base = instance.trim_end_matches('/');
    let mut side = load_side(state)?;
    let group = hex16(&side.state.group_id, "side.json groupId")?;
    let dm = hex16(&side.state.dm, "side.json dm")?;
    let mut ds = HttpDs::connect(base, &side.client.device(), &side.client.credential_blob())?;
    sync_group(&mut side, &mut ds)?;
    // Both leaves in one epoch before any key leaves this machine: a side a started before side b
    // armed would hand its page the one-leaf epoch's key.
    check_roster(side.group.member_count())?;
    let started = check_started(&group, ds.start_call(&dm, ""))?;
    let m = side.group.media_epoch(&side.provider)?;
    let keys = MediaKeys {
        epoch: m.epoch,
        base_key: *m.base_key,
        own_leaf: m.own_leaf,
        roster: m.roster.clone(),
    };
    let v = start_json(&livekit_url(base, &started), &started, &keys);
    let body = serde_json::to_vec_pretty(&v).map_err(|e| scenario(e.to_string()))?;
    write_private(out, &body)?;
    Ok(v)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::ds::remote::CallStarted;
    use dilla_core::ids::{DeviceId, UserId};
    use dilla_core::mls::RosterEntry;

    fn started(group: [u8; 16]) -> CallStarted {
        CallStarted {
            call_id: [0xca; 16],
            group_id: group,
            livekit_url: "wss://dilla.test".into(),
            token: "jwt".into(),
            ice_servers: vec![(
                vec!["turn:dilla.test:3478?transport=udp".into()],
                "user".into(),
                "cred".into(),
            )],
            caps: [64_000, 2_500_000, 0],
            created: true,
        }
    }

    fn media(epoch: u64) -> MediaKeys {
        MediaKeys {
            epoch,
            base_key: [0xab; 16],
            own_leaf: 1,
            roster: vec![
                RosterEntry {
                    leaf_index: 0,
                    device_id: DeviceId::from_bytes([0x0a; 16]),
                    user_id: UserId::from_bytes([0x01; 16]),
                },
                RosterEntry {
                    leaf_index: 1,
                    device_id: DeviceId::from_bytes([0x0b; 16]),
                    user_id: UserId::from_bytes([0x02; 16]),
                },
            ],
        }
    }

    #[test]
    fn netprobe_start_json_carries_the_pinned_field_names_a_32_hex_key_and_the_roster() {
        let v = start_json("ws://127.0.0.1:8485", &started([7; 16]), &media(3));
        let mut top: Vec<&str> = v.as_object().unwrap().keys().map(String::as_str).collect();
        top.sort_unstable();
        assert_eq!(
            top,
            [
                "callId",
                "groupId",
                "iceServers",
                "keys",
                "livekitUrl",
                "token",
                "v"
            ]
        );
        assert_eq!(v["v"], 1);
        assert_eq!(v["livekitUrl"], "ws://127.0.0.1:8485");
        assert_eq!(v["token"], "jwt");
        assert_eq!(v["callId"], hex::encode([0xca; 16]));
        assert_eq!(v["groupId"], hex::encode([7; 16]));
        assert_eq!(
            v["iceServers"],
            serde_json::json!([[["turn:dilla.test:3478?transport=udp"], "user", "cred"]])
        );
        let mut keys: Vec<&str> = v["keys"]
            .as_object()
            .unwrap()
            .keys()
            .map(String::as_str)
            .collect();
        keys.sort_unstable();
        assert_eq!(keys, ["baseKeyHex", "epoch", "roster", "selfLeaf"]);
        // A decimal string, as the page's BigInt wants it (MD-20).
        assert_eq!(v["keys"]["epoch"], "3");
        let key = v["keys"]["baseKeyHex"].as_str().unwrap();
        assert_eq!(key.len(), 32);
        assert!(
            key.bytes()
                .all(|b| b.is_ascii_hexdigit() && !b.is_ascii_uppercase())
        );
        assert_eq!(v["keys"]["selfLeaf"], 1);
        assert_eq!(
            v["keys"]["roster"],
            serde_json::json!([
                {"leaf": 0, "deviceId": hex::encode([0x0a; 16])},
                {"leaf": 1, "deviceId": hex::encode([0x0b; 16])},
            ])
        );
    }

    #[test]
    fn netprobe_livekit_url_is_the_ws_instance_only_on_loopback_http() {
        let s = started([1; 16]);
        assert_eq!(
            livekit_url("http://127.0.0.1:8485", &s),
            "ws://127.0.0.1:8485"
        );
        assert_eq!(
            livekit_url("http://localhost:5191/", &s),
            "ws://localhost:5191"
        );
        assert_eq!(livekit_url("http://192.0.2.7:8080", &s), "wss://dilla.test");
    }

    #[test]
    fn netprobe_prepare_names_share_one_six_hex_suffix() {
        let (a, b) = probe_names([0x0f, 0xa0, 0x3c]);
        assert_eq!(a, "netprobe-a-0fa03c");
        assert_eq!(b, "netprobe-b-0fa03c");
        for name in [&a, &b] {
            let (head, suffix) = name.split_at("netprobe-a-".len());
            assert!(head == "netprobe-a-" || head == "netprobe-b-", "{name}");
            assert_eq!(suffix.len(), 6);
            assert!(
                suffix
                    .bytes()
                    .all(|c| c.is_ascii_digit() || (b'a'..=b'f').contains(&c))
            );
        }
        let (c, _) = probe_names(random_suffix().unwrap());
        assert!(
            c.starts_with("netprobe-a-") && c.len() == "netprobe-a-".len() + 6,
            "{c}"
        );
    }

    #[test]
    fn netprobe_start_refuses_a_closed_call_group_and_says_run_arm_again() {
        let mine = [9; 16];
        let closed = DsError::Remote {
            status: 409,
            code: "E_LEAF_NOT_CURRENT",
            detail: "the call group is closed".into(),
        };
        let e = check_started(&mine, Err(closed)).unwrap_err().to_string();
        assert!(e.contains("run arm again"), "{e}");
        let gone = DsError::NotFound;
        let e = check_started(&mine, Err(gone)).unwrap_err().to_string();
        assert!(e.contains("run arm again"), "{e}");
        // The channel's newest open call group is another one: this state's group was closed.
        let e = check_started(&mine, Ok(started([8; 16])))
            .unwrap_err()
            .to_string();
        assert!(e.contains("run arm again"), "{e}");
        // Anything else is reported as it is, without the hint.
        let other = DsError::RateLimited { retry_after_ms: 5 };
        let e = check_started(&mine, Err(other)).unwrap_err().to_string();
        assert!(!e.contains("run arm again"), "{e}");
        assert!(check_started(&mine, Ok(started(mine))).is_ok());
    }

    #[test]
    fn netprobe_start_refuses_a_call_group_the_other_side_has_not_joined() {
        let e = check_roster(1).unwrap_err().to_string();
        assert!(e.contains("arm the other side"), "{e}");
        assert!(check_roster(2).is_ok());
    }

    fn served(group: Option<[u8; 16]>, call: Option<[u8; 16]>) -> ChannelCall {
        ChannelCall {
            group_id: group,
            call_id: call,
            participants: 0,
        }
    }

    #[test]
    fn netprobe_arm_refuses_while_a_call_is_live() {
        for side in [Side::A, Side::B] {
            let e = arm_action(
                side,
                Some([1; 16]),
                true,
                &served(Some([1; 16]), Some([7; 16])),
            )
            .unwrap_err()
            .to_string();
            assert!(e.contains("still live"), "{e}");
        }
    }

    #[test]
    fn netprobe_arm_keeps_a_current_group_and_rebuilds_a_stale_one() {
        let g = [1; 16];
        let other = [2; 16];
        // Side a: the stored group is the open one and the store holds it.
        assert_eq!(
            arm_action(Side::A, Some(g), true, &served(Some(g), None)).unwrap(),
            ArmAction::Keep
        );
        // Side a: closed (none open), replaced, or lost locally: a fresh group.
        for (stored, held, open) in [
            (Some(g), true, None),
            (Some(g), true, Some(other)),
            (Some(g), false, Some(g)),
            (None, false, None),
        ] {
            assert_eq!(
                arm_action(Side::A, stored, held, &served(open, None)).unwrap(),
                ArmAction::Register,
                "{stored:?} {held} {open:?}"
            );
        }
        // Side b joins whatever side a opened, unless it already holds it.
        assert_eq!(
            arm_action(Side::B, Some(g), true, &served(Some(g), None)).unwrap(),
            ArmAction::Keep
        );
        assert_eq!(
            arm_action(Side::B, Some(g), true, &served(Some(other), None)).unwrap(),
            ArmAction::Join(other)
        );
        assert_eq!(
            arm_action(Side::B, Some(g), false, &served(Some(g), None)).unwrap(),
            ArmAction::Join(g)
        );
        let e = arm_action(Side::B, Some(g), true, &served(None, None))
            .unwrap_err()
            .to_string();
        assert!(e.contains("arm side a first"), "{e}");
    }

    #[test]
    fn netprobe_channel_call_decodes_l_http_100() {
        use dilla_core::cbor::Encoder;
        let mut e = Encoder::new();
        e.array(3).bytes(&[1; 16]).bytes(&[2; 16]).array(1);
        e.array(3).bytes(&[3; 16]).bytes(&[4; 16]).uint(1);
        let v = decode_channel_call(&e.into_vec()).unwrap();
        assert_eq!(v.group_id, Some([1; 16]));
        assert_eq!(v.call_id, Some([2; 16]));
        assert_eq!(v.participants, 1);
        let mut e = Encoder::new();
        e.array(3).null().null().array(0);
        let v = decode_channel_call(&e.into_vec()).unwrap();
        assert_eq!((v.group_id, v.call_id, v.participants), (None, None, 0));
        // A Go nil slice arrives as null.
        let mut e = Encoder::new();
        e.array(3).bytes(&[1; 16]).null().null();
        assert_eq!(decode_channel_call(&e.into_vec()).unwrap().participants, 0);
        assert!(decode_channel_call(&[0x80]).is_err());
    }

    #[test]
    fn netprobe_start_refuses_a_state_directory_without_its_call_group() {
        let dir = std::env::temp_dir().join(format!("netprobe-test-{}", std::process::id()));
        let side = dir.join("side-a");
        std::fs::create_dir_all(&side).unwrap();
        let state = SideState {
            v: 1,
            side: "a".into(),
            username: "netprobe-a-000000".into(),
            seed: "1".into(),
            user_id: hex::encode([1; 16]),
            device_id: hex::encode(
                crate::TestClient::new(
                    "x",
                    UserId::from_bytes([1; 16]),
                    dilla_core::identity::Tier::Native,
                    dilla_core::identity::Kind::User,
                    1,
                )
                .unwrap()
                .device_id()
                .as_bytes(),
            ),
            dm: hex::encode([2; 16]),
            group_id: hex::encode([3; 16]),
        };
        state.write(&side).unwrap();
        // An empty MLS store: the group this state names is not in it.
        let store = rusqlite::Connection::open(side.join(MLS_FILE)).unwrap();
        drop(store);
        let e = load_side(&side).err().unwrap().to_string();
        std::fs::remove_dir_all(&dir).unwrap();
        assert!(e.contains("run arm again"), "{e}");
    }
}
