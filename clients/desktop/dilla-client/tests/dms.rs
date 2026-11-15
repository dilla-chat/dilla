//! Task 13's client tests for the dms area (standard procedure S2), against a real dillad.
//!
//! The trust behaviours port `e2e/web/trust-polish.spec.ts`: the browser is a Rust [`Client`] signed up the way the app
//! signs up, and the peer is the testkit's [`WebDriver`] (the same native peer the Playwright suite drives over stdio:
//! `setup`, `register`, `join`, `send`, `sync`, `members`, `rotate_umk`, `trust_list`, `leave`). Every verdict the tests
//! read is the published trust slice, which the client reads from the core only.
//!
//! Needs `target/dilla-testhost` (`go build -o target/dilla-testhost ./cmd/dilla-testhost`, or `DILLA_TESTHOST`) and the
//! WASI core (`DILLA_CORE_WASI`, else `target/wasm32-wasip1/release/dilla_core_wasi.wasm`); the tests are `#[ignore]`d.

use std::io::{BufRead, BufReader, Write};
use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::sync::mpsc;
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::state::trust::{TrustSummary, privacy_row};
use dilla_client::state::{AppState, ChannelId, CommunityId, TimelineItemState, UserId};
use dilla_client::testing::host::{
    Host, Person, clear_session, community, repo_root, sign_up, start, start_host, wait_for,
};
use serde_json::{Value, json};

const WAIT: Duration = Duration::from_secs(60);

/// The `dilla-testkit` binary (`DILLA_TESTKIT`, else `target/{release,debug}/dilla-testkit`): `cargo build -p
/// dilla-testkit --bin dilla-testkit`.
fn testkit_binary() -> PathBuf {
    if let Some(bin) = std::env::var_os("DILLA_TESTKIT") {
        return PathBuf::from(bin);
    }
    let root = repo_root();
    let release = root.join("target/release/dilla-testkit");
    if release.exists() {
        return release;
    }
    let debug = root.join("target/debug/dilla-testkit");
    assert!(
        debug.exists(),
        "dilla-testkit missing: cargo build -p dilla-testkit --bin dilla-testkit (or set DILLA_TESTKIT)"
    );
    debug
}

/// The testkit's native peer (`dilla-testkit web-driver`, the one the Playwright suite drives), one JSON request per
/// line. It is a process of its own because the testkit reads the host's bootstrap invites and control address from
/// its environment: set on the child, never on this test binary.
struct Peer {
    tx: mpsc::Sender<(Value, tokio::sync::oneshot::Sender<Value>)>,
    inbox: Vec<Value>,
}

impl Peer {
    fn start(host: &Host, seed: u64) -> Peer {
        let mut child = Command::new(testkit_binary())
            .args([
                "web-driver",
                "--ds",
                &host.public,
                "--seed",
                &seed.to_string(),
            ])
            .env("DILLA_TESTKIT_INVITE", host.invites.join(","))
            .env("DILLA_TESTKIT_CONTROL", &host.control)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .expect("the web driver starts");
        let mut stdin = child.stdin.take().unwrap();
        let mut stdout = BufReader::new(child.stdout.take().unwrap());
        let (tx, rx) = mpsc::channel::<(Value, tokio::sync::oneshot::Sender<Value>)>();
        std::thread::spawn(move || {
            for (req, answer) in rx {
                if writeln!(stdin, "{req}")
                    .and_then(|()| stdin.flush())
                    .is_err()
                {
                    break;
                }
                let mut line = String::new();
                if stdout.read_line(&mut line).unwrap_or(0) == 0 {
                    break;
                }
                let _ = answer.send(serde_json::from_str(&line).unwrap_or(Value::Null));
            }
            let _ = child.kill();
            let _ = child.wait();
        });
        Peer {
            tx,
            inbox: Vec::new(),
        }
    }

    async fn call(&self, op: &str, mut args: Value) -> Value {
        args["op"] = json!(op);
        let (tx, rx) = tokio::sync::oneshot::channel();
        self.tx.send((args, tx)).expect("the peer thread runs");
        let v = rx.await.expect("the peer answers");
        assert_eq!(v["ok"], json!(true), "peer {op}: {v}");
        v
    }

    async fn send(&self, body: &str) -> Value {
        self.call("send", json!({ "body": body })).await
    }

    /// Syncs until the peer decrypted `body`.
    async fn wait_for(&mut self, body: &str, channel: Option<&str>) {
        let deadline = Instant::now() + WAIT;
        loop {
            let args = channel.map_or(json!({}), |c| json!({ "channel_id": c }));
            let v = self.call("sync", args).await;
            if let Some(rows) = v["received"].as_array() {
                self.inbox.extend(rows.iter().cloned());
            }
            if self.inbox.iter().any(|m| m["body"] == json!(body)) {
                return;
            }
            assert!(
                Instant::now() < deadline,
                "the peer never decrypted {body:?}"
            );
            tokio::time::sleep(Duration::from_millis(300)).await;
        }
    }

    /// Syncs until the text group's roster holds `count` devices.
    async fn wait_for_devices(&self, count: usize, channel: Option<&str>) {
        let deadline = Instant::now() + WAIT;
        loop {
            let args = channel.map_or(json!({}), |c| json!({ "channel_id": c }));
            self.call("sync", args.clone()).await;
            let v = self.call("members", args).await;
            if v["devices"].as_array().map_or(0, Vec::len) == count {
                return;
            }
            assert!(
                Instant::now() < deadline,
                "the group never held {count} devices: {v}"
            );
            tokio::time::sleep(Duration::from_millis(300)).await;
        }
    }
}

fn hex16(s: &str) -> [u8; 16] {
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).unwrap();
    }
    out
}

async fn host(tag: &str) -> Host {
    let tag = tag.to_owned();
    tokio::task::spawn_blocking(move || start_host(&tag, "127.0.0.1:0", None))
        .await
        .unwrap()
}

/// The browser: a person signed up the way the app signs up, its session cleared so the client makes its own.
async fn browser(host: &Host, name: &str, tag: &str) -> Person {
    let invite = host.invites.last().unwrap().clone();
    let p = sign_up(host, &invite, name, name, tag).await;
    clear_session(&p);
    p
}

/// `joinCommunity` + `openChannel` of the e2e helpers: the browser joins the peer's community by its invite, reads its
/// lists again and opens the channel (joining the peer's group).
async fn join_and_open(host: &Host, person: &Person, client: &Client, setup: &Value) -> ChannelId {
    let community = hex16(setup["community_id"].as_str().unwrap());
    let channel = ChannelId(hex16(setup["channel_id"].as_str().unwrap()));
    let code = setup["invite_code"].as_str().unwrap().to_owned();
    let (public, device, token) = (host.public.clone(), person.device_id, person.token.clone());
    tokio::task::spawn_blocking(move || {
        use dilla_core::ids::DeviceId;
        let mut ds =
            dilla_testkit::HttpDs::with_session(&public, DeviceId::from_bytes(device), token)
                .unwrap();
        ds.join_community(&community, &code).unwrap();
    })
    .await
    .unwrap();
    client.refresh();
    wait_for(client, WAIT, "the peer's channel", |s| {
        s.channel(channel).is_some()
    })
    .await;
    let _ = CommunityId(community);
    let deadline = Instant::now() + WAIT;
    loop {
        match client.open_channel(channel).await.unwrap() {
            Ok(()) => break,
            Err(e) if Instant::now() < deadline => {
                eprintln!("open retried: {e}");
                tokio::time::sleep(Duration::from_millis(500)).await;
            }
            Err(e) => panic!("the browser never opened the channel: {e}"),
        }
    }
    channel
}

/// `enter` of trust-polish.spec.ts: the peer sets up its community and registers the channel's group; the browser joins
/// it and the peer sees both devices.
async fn enter(host: &Host, person: &Person, client: &Client, peer: &Peer) -> (Value, ChannelId) {
    let setup = peer
        .call(
            "setup",
            json!({ "community": "trust", "channel": "general" }),
        )
        .await;
    peer.call("register", json!({})).await;
    let channel = join_and_open(host, person, client, &setup).await;
    peer.wait_for_devices(2, setup["channel_id"].as_str()).await;
    // The browser has pinned the peer's key (first seen) before anything rotates it: otherwise the rotated key is the
    // first this device sees, which is trust on first use, not a change (the Playwright suite pins it while joining).
    let user = user_of(&setup);
    wait_for(client, WAIT, "the peer's key pinned", |s| {
        contact(s, user).is_some_and(|c| !c.unlisted)
    })
    .await;
    (setup, channel)
}

fn shows(s: &AppState, channel: ChannelId, body: &str) -> bool {
    s.timelines.get(&channel).is_some_and(|t| {
        t.items
            .iter()
            .any(|i| i.state == TimelineItemState::Ok && i.body == body)
    })
}

fn contact(s: &AppState, user: UserId) -> Option<&TrustSummary> {
    s.trust.contact(user)
}

fn changed_and_blocked(s: &AppState, user: UserId, channel: ChannelId) -> bool {
    contact(s, user).is_some_and(TrustSummary::is_changed)
        && s.trust.send_blocks.contains_key(&channel)
}

fn user_of(setup: &Value) -> UserId {
    UserId(hex16(setup["user_id"].as_str().unwrap()))
}

/// e2e/web/trust-polish.spec.ts "rotation keeps already visible text; an independent trusted group still works".
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn rotation_keeps_visible_text_independent_group_works() {
    let host = host("t13-rotation").await;
    let me = browser(&host, "browser", "t13-rotation").await;
    let client = start(&me).await;
    client.trust().start();
    let peer = Peer::start(&host, 0x1301);
    let (setup, channel) = enter(&host, &me, &client, &peer).await;
    let peer_user = user_of(&setup);
    peer.send("before rotation").await;
    wait_for(&client, WAIT, "the text before rotation", |s| {
        shows(s, channel, "before rotation")
    })
    .await;

    let mut control = Peer::start(&host, 0x1302);
    let (other, control_channel) = enter(&host, &me, &client, &control).await;
    control.send("trusted baseline").await;
    wait_for(&client, WAIT, "the control's baseline", |s| {
        shows(s, control_channel, "trusted baseline")
    })
    .await;

    peer.call("rotate_umk", json!({})).await;
    peer.send("after rotation").await;
    let s = wait_for(&client, WAIT, "the changed alert and the send block", |s| {
        changed_and_blocked(s, peer_user, channel)
    })
    .await;
    // Requirement 1: sending is blocked in the changed contact's group, and the composer says why.
    assert_eq!(
        s.trust.block_reason(channel, &[peer_user]),
        Some("privacy.changed")
    );
    assert!(
        client
            .send_text(channel, "refused".into())
            .await
            .unwrap()
            .is_err(),
        "the core refuses a send while the key is changed"
    );
    let t = &s.timelines[&channel];
    let before = t
        .items
        .iter()
        .find(|i| i.body == "before rotation")
        .unwrap();
    assert_eq!(
        before.state,
        TimelineItemState::Ok,
        "visible text stays readable"
    );
    assert!(!t.items.iter().any(|i| i.body == "after rotation"));
    // The independent group still works.
    control.send("positive control").await;
    wait_for(&client, WAIT, "the positive control", |s| {
        shows(s, control_channel, "positive control")
    })
    .await;
    let s = client.state().borrow().clone();
    assert!(
        !s.timelines[&channel]
            .items
            .iter()
            .any(|i| i.body == "after rotation")
    );
    assert!(!s.trust.send_blocks.contains_key(&control_channel));
    client
        .send_text(control_channel, "still sending".into())
        .await
        .unwrap()
        .expect("the control's group is open");
    control
        .wait_for("still sending", other["channel_id"].as_str())
        .await;
}

/// e2e/web/trust-polish.spec.ts "Copy alone does not approve; explicit exact-key comparison admits new traffic". The
/// dialog's Copy is a UI action (the desktop's `copy_alone_does_not_approve` test); here: only the exact comparison key
/// approves, and nothing else moves the core.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn copy_alone_does_not_approve_exact_compare_admits() {
    let host = host("t13-compare").await;
    let me = browser(&host, "browser", "t13-compare").await;
    let client = start(&me).await;
    client.trust().start();
    let mut peer = Peer::start(&host, 0x1303);
    let (setup, channel) = enter(&host, &me, &client, &peer).await;
    let peer_user = user_of(&setup);
    let rotated = peer.call("rotate_umk", json!({})).await;
    peer.send("pending rotation").await;
    let s = wait_for(&client, WAIT, "the changed contact with a key", |s| {
        changed_and_blocked(s, peer_user, channel)
            && contact(s, peer_user).is_some_and(|c| c.changed_umk.is_some())
    })
    .await;
    let row = contact(&s, peer_user).unwrap().clone();
    assert!(privacy_row(&row).alert_compare, "Privacy offers Compare");
    let witness = peer
        .call("trust_list", json!({ "user": rotated["user"] }))
        .await;
    let pending = witness["pendingUmk"]
        .as_str()
        .expect("a pending key")
        .to_owned();
    assert_eq!(row.changed_umk.as_deref(), Some(pending.as_str()));
    let groups = client
        .trust()
        .safety_number(peer_user, pending.clone())
        .await
        .unwrap()
        .expect("the number for the pending key");
    assert_eq!(groups.len(), 12);
    assert!(
        groups
            .iter()
            .all(|g| g.len() == 5 && g.bytes().all(|b| b.is_ascii_digit()))
    );
    // The pinned (old) key is no comparison key: neither its number nor an approval of it is given.
    assert!(
        client
            .trust()
            .safety_number(peer_user, row.umk.clone())
            .await
            .unwrap()
            .is_err()
    );
    assert!(
        client
            .trust()
            .verify(peer_user, row.umk.clone())
            .await
            .unwrap()
            .is_err()
    );
    client.trust().refresh().await.unwrap().unwrap();
    let s = client.state().borrow().clone();
    assert!(
        changed_and_blocked(&s, peer_user, channel),
        "nothing was approved"
    );
    assert!(!contact(&s, peer_user).unwrap().verified);

    client
        .trust()
        .verify(peer_user, pending.clone())
        .await
        .unwrap()
        .expect("the exact key approves");
    // P2: what the contact sent under the new key before the comparison was held, and is readable once verified.
    wait_for(&client, WAIT, "the held text", |s| {
        shows(s, channel, "pending rotation")
            && !s.trust.send_blocks.contains_key(&channel)
            && contact(s, peer_user).is_some_and(|c| c.verified && !c.is_changed())
    })
    .await;
    peer.send("validated new text").await;
    wait_for(&client, WAIT, "new text", |s| {
        shows(s, channel, "validated new text")
    })
    .await;
    client
        .send_text(channel, "browser after verification".into())
        .await
        .unwrap()
        .expect("sending resumed");
    peer.wait_for("browser after verification", None).await;
}

/// e2e/web/trust-polish.spec.ts "after a reload Privacy has names and Compare; a changed contact can be compared and
/// verified".
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn privacy_names_and_compare_after_restart() {
    let host = host("t13-restart").await;
    let me = browser(&host, "browser", "t13-restart").await;
    let client = start(&me).await;
    client.trust().start();
    let mut peer = Peer::start(&host, 0x1304);
    let (setup, channel) = enter(&host, &me, &client, &peer).await;
    let peer_user = user_of(&setup);
    let name = setup["display"].as_str().unwrap().to_owned();
    peer.send("before reload").await;
    wait_for(&client, WAIT, "the text", |s| {
        shows(s, channel, "before reload")
    })
    .await;

    drop(client);
    let client = start(&me).await;
    client.trust().start();
    let s = wait_for(&client, WAIT, "Privacy's names and Compare", |s| {
        contact(s, peer_user).is_some_and(|c| c.name == name && privacy_row(c).compare)
    })
    .await;
    assert!(contact(&s, peer_user).unwrap().shared);

    let rotated = peer.call("rotate_umk", json!({})).await;
    peer.send("changed while away").await;
    // Restart at once, in the middle of the ingest of the rotation and the message.
    drop(client);
    let client = start(&me).await;
    client.trust().start();
    let _ = client.open_channel(channel).await.unwrap();
    let s = wait_for(&client, WAIT, "the changed alert after the restart", |s| {
        changed_and_blocked(s, peer_user, channel)
            && contact(s, peer_user).is_some_and(|c| c.changed_umk.is_some())
    })
    .await;
    let row = contact(&s, peer_user).unwrap();
    assert_eq!(row.name, name);
    let witness = peer
        .call("trust_list", json!({ "user": rotated["user"] }))
        .await;
    let pending = witness["pendingUmk"].as_str().unwrap().to_owned();
    assert_eq!(row.changed_umk.as_deref(), Some(pending.as_str()));
    let groups = client
        .trust()
        .safety_number(peer_user, pending.clone())
        .await
        .unwrap()
        .unwrap();
    assert_eq!(groups.len(), 12);
    client
        .trust()
        .verify(peer_user, pending)
        .await
        .unwrap()
        .expect("approved");
    wait_for(&client, WAIT, "the text sent while away", |s| {
        shows(s, channel, "changed while away")
    })
    .await;
    client
        .send_text(channel, "browser after reload verification".into())
        .await
        .unwrap()
        .expect("sending resumed");
    peer.wait_for("browser after reload verification", None)
        .await;
}

/// e2e/web/trust-polish.spec.ts "a validated Remove of the changed device restores sending here only; the contact stays
/// changed".
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn remove_changed_device_restores_sending_here_only() {
    let host = host("t13-remove").await;
    let me = browser(&host, "browser", "t13-remove").await;
    let client = start(&me).await;
    client.trust().start();
    let mut peer = Peer::start(&host, 0x1305);
    let setup = peer
        .call(
            "setup",
            json!({ "community": "trust", "channel": "general" }),
        )
        .await;
    let registered = peer.call("register", json!({})).await;
    let channel = join_and_open(&host, &me, &client, &setup).await;
    let channel_hex = setup["channel_id"].as_str().unwrap().to_owned();
    peer.wait_for_devices(2, Some(&channel_hex)).await;

    let contact_peer = Peer::start(&host, 0x1306);
    let own = contact_peer
        .call("setup", json!({ "community": "own", "channel": "own" }))
        .await;
    contact_peer
        .call(
            "join",
            json!({
                "community_id": setup["community_id"],
                "channel_id": setup["channel_id"],
                "group_id": registered["group_id"],
                "invite_code": setup["invite_code"],
            }),
        )
        .await;
    peer.wait_for_devices(3, Some(&channel_hex)).await;
    let contact_user = user_of(&own);
    let name = own["display"].as_str().unwrap().to_owned();
    contact_peer.send("hello from the contact").await;
    wait_for(&client, WAIT, "the contact's hello", |s| {
        shows(s, channel, "hello from the contact")
    })
    .await;
    contact_peer.call("rotate_umk", json!({})).await;
    contact_peer.send("contact under a new key").await;
    let s = wait_for(&client, WAIT, "the contact changed and the block", |s| {
        changed_and_blocked(s, contact_user, channel)
    })
    .await;
    assert_eq!(contact(&s, contact_user).unwrap().name, name);

    // The changed device leaves; this device commits that Remove as any member would.
    contact_peer.call("leave", json!({})).await;
    let s = wait_for(&client, WAIT, "sending to resume here", |s| {
        !s.trust.send_blocks.contains_key(&channel) && s.trust.removed_here(channel)
    })
    .await;
    assert_eq!(
        s.trust
            .block_reason(channel, &[contact_user, user_of(&setup)]),
        None
    );
    client
        .send_text(channel, "sending again after the Remove".into())
        .await
        .unwrap()
        .expect("sending resumed");
    peer.wait_for("sending again after the Remove", Some(&channel_hex))
        .await;
    // The contact is still changed and unverified: Privacy keeps the changed alert, with no verified state.
    let s = client.state().borrow().clone();
    let row = contact(&s, contact_user).unwrap();
    assert!(row.is_changed() && !row.verified);
    let privacy = privacy_row(row);
    assert!(privacy.changed.is_some() && privacy.verified_key.is_none());
}

/// Task 28 item 17 (engine.ts `onUnexpectedWelcome`, controller.ts `reloadDms` on it): a Welcome for a group this device
/// does not expect (a DM another person just opened) reads the DM list at once, so the DM is listed and joined without
/// waiting for the list's poll. Bob's client runs no DM watcher (no poll, no ready read): only the Welcome can list it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn an_unexpected_welcome_reads_the_dm_list() {
    let host = host("t28-welcome-dm").await;
    let invite = host.invites.last().unwrap().clone();
    let pa = sign_up(&host, &invite, "alice", "Alice", "t28-welcome-dm").await;
    let pb = sign_up(&host, &invite, "bob", "Bob", "t28-welcome-dm").await;
    let (host, pa, pb) = tokio::task::spawn_blocking(move || {
        community(&host, &pa, "dms", &[&pb]);
        clear_session(&pa);
        clear_session(&pb);
        (host, pa, pb)
    })
    .await
    .unwrap();
    let _keep = host;
    let a = start(&pa).await;
    let b = start(&pb).await;
    wait_for(&b, WAIT, "b online", |s| {
        s.connection.status == dilla_client::state::ConnectionStatus::Online
    })
    .await;
    assert!(!b.dms().started(), "bob's client runs no DM watcher");
    let dm = a
        .dms()
        .open_dm(UserId(pb.user_id))
        .await
        .unwrap()
        .expect("the DM opens");
    a.dms()
        .open(dm)
        .await
        .unwrap()
        .expect("a registers the DM's group");
    wait_for(&b, WAIT, "b lists and joins the DM on the Welcome", |s| {
        s.dms
            .get(dm)
            .is_some_and(|d| d.group == dilla_client::state::ChannelGroupState::Active)
    })
    .await;
}

/// Requirements 15-16 (flow 07): a DM opened from one side is listed on the other, its text arrives and counts as
/// unread there until it is read.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn dm_open_list_send_and_unread() {
    let host = host("t13-dm").await;
    let invite = host.invites.last().unwrap().clone();
    let pa = sign_up(&host, &invite, "alice", "Alice", "t13-dm").await;
    let pb = sign_up(&host, &invite, "bob", "Bob", "t13-dm").await;
    let (host, pa, pb) = tokio::task::spawn_blocking(move || {
        community(&host, &pa, "dms", &[&pb]);
        clear_session(&pa);
        clear_session(&pb);
        (host, pa, pb)
    })
    .await
    .unwrap();
    let _keep = host;
    let a = start(&pa).await;
    let b = start(&pb).await;
    a.dms().start();
    b.dms().start();
    wait_for(&a, WAIT, "a's member lists", |s| {
        s.members.values().any(|m| m.len() == 2)
    })
    .await;
    let picked = dilla_client::features::dms::picker_members(&a.state().borrow(), None);
    assert_eq!(
        picked
            .iter()
            .map(|m| m.display.as_str())
            .collect::<Vec<_>>(),
        ["Bob"]
    );
    let dm = a
        .dms()
        .open_dm(UserId(pb.user_id))
        .await
        .unwrap()
        .expect("the DM opens");
    // Opening again answers the same DM.
    let again = a.dms().open_dm(UserId(pb.user_id)).await.unwrap().unwrap();
    assert_eq!(again, dm);
    let s = wait_for(&a, WAIT, "a's DM list", |s| s.dms.get(dm).is_some()).await;
    assert_eq!(s.dms.get(dm).unwrap().name, "Bob");
    a.dms()
        .open(dm)
        .await
        .unwrap()
        .expect("a registers the DM's group");
    // B finds the DM another person opened (the list read on its interval) and joins its group.
    wait_for(&b, WAIT, "b to list and join the DM", |s| {
        s.dms
            .get(dm)
            .is_some_and(|d| d.group == dilla_client::state::ChannelGroupState::Active)
    })
    .await;
    a.dms()
        .send_text(dm, "hello in private".into())
        .await
        .unwrap()
        .expect("queued");
    let s = wait_for(&b, WAIT, "b's DM, its text and its badge", |s| {
        s.dms.get(dm).is_some_and(|d| d.name == "Alice")
            && shows(s, dm, "hello in private")
            && s.unread_of(dm) == 1
    })
    .await;
    assert_eq!(dilla_client::features::dms::home_counts(&s).unread, 1);
    b.mark_read(dm).await.unwrap().expect("read");
    wait_for(&b, WAIT, "the DM read", |s| s.unread_of(dm) == 0).await;
    b.dms()
        .send_text(dm, "hello back".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(&a, WAIT, "a sees the answer", |s| {
        shows(s, dm, "hello back")
    })
    .await;
}
