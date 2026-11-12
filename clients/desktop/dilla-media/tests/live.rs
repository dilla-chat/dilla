//! Live calls through a real LiveKit SFU (dilla-testhost's in-process one, as the e2e media suite
//! runs it), each participant a desktop `dilla_media::Call` in its own process
//! (examples/call-peer.rs; the hook is process-wide), with the keys a real `ClientCore` and a real
//! `DillaGroup` member derive for one call group. Covers what the synthetic-frame tests cannot:
//! LiveKit's publish and subscribe path through the vendored glue, the receiver bound by its own
//! track object in `OnTrack`, the room's SIF trailer arriving from the join response, and an epoch
//! change mid-call.
//!
//! Needs Go (to build `cmd/dilla-testhost`, or `DILLA_TESTHOST`) and the WASI core
//! (`internal/mlswasi/testdata/dilla_core_wasi.wasm`, or `DILLA_WASI_CORE`):
//! `cargo test -p dilla-media --test live -- --ignored`.

#[allow(dead_code, unused_imports, clippy::all)]
#[path = "../../../../core/dilla-core/tests/client_support/mod.rs"]
mod client_support;

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::mpsc::{Receiver, channel};
use std::time::{Duration, Instant};

use client_support::{CALL, Core, RawPeer, call_raw_bob, decode_media_epoch};
use serde_json::{Value, json};

fn repo_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .canonicalize()
        .unwrap()
}

fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

fn testhost_binary(root: &Path) -> PathBuf {
    if let Some(p) = std::env::var_os("DILLA_TESTHOST") {
        return PathBuf::from(p);
    }
    let out = root.join("target/dilla-testhost");
    let status = Command::new("go")
        .args(["build", "-o"])
        .arg(&out)
        .arg("./cmd/dilla-testhost")
        .current_dir(root)
        .status()
        .expect("go");
    assert!(status.success(), "go build ./cmd/dilla-testhost");
    out
}

fn wasi_core(root: &Path) -> PathBuf {
    std::env::var_os("DILLA_WASI_CORE")
        .map(PathBuf::from)
        .unwrap_or_else(|| root.join("internal/mlswasi/testdata/dilla_core_wasi.wasm"))
}

struct Host {
    child: Child,
    control: String,
}

impl Drop for Host {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn start_host() -> Host {
    let root = repo_root();
    let core = wasi_core(&root);
    assert!(core.exists(), "{} is missing", core.display());
    let control = format!("127.0.0.1:{}", free_port());
    let mut child = Command::new(testhost_binary(&root))
        .args(["-listen", &format!("127.0.0.1:{}", free_port())])
        .args(["-control", &control])
        .arg("-core")
        .arg(&core)
        .args(["-log-level", "warn", "-sfu"])
        .args(["-sfu-port", &free_port().to_string()])
        .args(["-sfu-udp-port", &free_port().to_string()])
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .expect("dilla-testhost starts");
    let mut out = BufReader::new(child.stdout.take().unwrap());
    let deadline = Instant::now() + Duration::from_secs(90);
    let mut line = String::new();
    loop {
        line.clear();
        assert!(Instant::now() < deadline, "dilla-testhost did not start");
        if out.read_line(&mut line).unwrap() == 0 {
            panic!("dilla-testhost exited");
        }
        if line.contains("DILLA_TESTKIT_INVITE=") {
            break;
        }
    }
    std::thread::spawn(move || {
        let mut sink = Vec::new();
        let _ = out.read_to_end(&mut sink);
    });
    Host {
        child,
        control: format!("http://{control}"),
    }
}

/// `POST /debug/sfu/token` (e2e/media/support/lk.ts `debugToken`).
fn token(control: &str, room: &str, identity: &str) -> (String, String) {
    let addr = control.trim_start_matches("http://");
    let body = json!({"room": room, "identity": identity, "create": true}).to_string();
    let mut s = TcpStream::connect(addr).unwrap();
    write!(
        s,
        "POST /debug/sfu/token HTTP/1.1\r\nHost: {addr}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    )
    .unwrap();
    let mut answer = String::new();
    s.read_to_string(&mut answer).unwrap();
    let (head, json_body) = answer.split_once("\r\n\r\n").expect("an HTTP answer");
    assert!(head.starts_with("HTTP/1.1 200"), "{head}");
    // Chunked or not, the JSON object is the one {...} in the body.
    let start = json_body.find('{').unwrap();
    let end = json_body.rfind('}').unwrap();
    let v: Value = serde_json::from_str(&json_body[start..=end]).unwrap();
    (
        v["url"].as_str().unwrap().to_owned(),
        v["token"].as_str().unwrap().to_owned(),
    )
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// One `call-peer` process.
struct Peer {
    child: Child,
    stdin: ChildStdin,
    lines: Receiver<Value>,
    seen: Vec<Value>,
}

impl Peer {
    fn start(url: &str, token: &str, tone: u32) -> Peer {
        let exe = std::env::current_exe().unwrap();
        let bin = exe
            .parent()
            .unwrap()
            .parent()
            .unwrap()
            .join("examples/call-peer");
        static BUILT: std::sync::Once = std::sync::Once::new();
        BUILT.call_once(|| {
            // `cargo test --test live` builds no example (and would leave a stale one): build it
            // once per run, with this build's profile.
            let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".into());
            let mut build = Command::new(cargo);
            build.args(["build", "-p", "dilla-media", "--example", "call-peer"]);
            if !cfg!(debug_assertions) {
                build.arg("--release");
            }
            assert!(
                build.status().unwrap().success(),
                "cargo build --example call-peer"
            );
        });
        let mut child = Command::new(bin)
            .args(["--url", url, "--token", token, "--tone", &tone.to_string()])
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        let stdin = child.stdin.take().unwrap();
        let stdout = BufReader::new(child.stdout.take().unwrap());
        let (tx, lines) = channel();
        std::thread::spawn(move || {
            for line in stdout.lines().map_while(Result::ok) {
                if let Ok(v) = serde_json::from_str::<Value>(&line)
                    && tx.send(v).is_err()
                {
                    break;
                }
            }
        });
        Peer {
            child,
            stdin,
            lines,
            seen: Vec::new(),
        }
    }

    fn send(&mut self, line: &str) {
        writeln!(self.stdin, "{line}").unwrap();
        self.stdin.flush().unwrap();
    }

    fn next(&mut self, event: &str, timeout: Duration) -> Value {
        let deadline = Instant::now() + timeout;
        loop {
            let left = deadline.saturating_duration_since(Instant::now());
            match self.lines.recv_timeout(left) {
                Ok(v) if v["event"] == event => return v,
                Ok(v) => {
                    assert_ne!(v["event"], "failed", "{v}");
                    self.seen.push(v);
                }
                Err(_) => panic!("no {event} within {timeout:?}; saw {:?}", self.seen),
            }
        }
    }

    fn epoch(&mut self, e: &Epoch) {
        let roster: Vec<String> = e
            .roster
            .iter()
            .map(|(l, d)| format!("{l}:{}", hex(d)))
            .collect();
        self.send(&format!(
            "epoch {} {} {} {}",
            e.epoch,
            hex(&e.key),
            e.own_leaf,
            roster.join(",")
        ));
        self.next("epoch", Duration::from_secs(5));
    }

    fn stats(&mut self) -> Value {
        self.send("stats");
        self.next("stats", Duration::from_secs(5))
    }
}

impl Drop for Peer {
    fn drop(&mut self) {
        let _ = writeln!(self.stdin, "quit");
        let deadline = Instant::now() + Duration::from_secs(5);
        while Instant::now() < deadline {
            if let Ok(Some(_)) = self.child.try_wait() {
                return;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

struct Epoch {
    epoch: u64,
    key: [u8; 16],
    own_leaf: u64,
    roster: Vec<(u64, [u8; 16])>,
}

fn alice_epoch(alice: &Core) -> Epoch {
    let m = decode_media_epoch(&alice.core.call_media_epoch(&CALL).unwrap());
    Epoch {
        epoch: m.epoch,
        key: m.base_key,
        own_leaf: m.own_leaf,
        roster: m.roster.iter().map(|(l, d, _)| (*l, *d)).collect(),
    }
}

fn bob_epoch(bob: &RawPeer) -> Epoch {
    let m = bob
        .group
        .as_ref()
        .unwrap()
        .media_epoch(&bob.provider)
        .unwrap();
    Epoch {
        epoch: m.epoch,
        key: *m.base_key,
        own_leaf: u64::from(m.own_leaf),
        roster: m
            .roster
            .iter()
            .map(|r| (u64::from(r.leaf_index), *r.device_id.as_bytes()))
            .collect(),
    }
}

fn opened(stats: &Value, kid: &str) -> u64 {
    stats["media"]["opened"][kid].as_u64().unwrap_or(0)
}

fn failed(stats: &Value) -> u64 {
    stats["media"]["failed"]
        .as_object()
        .map(|o| o.values().filter_map(Value::as_u64).sum())
        .unwrap_or(0)
}

/// Waits until `peer` has decoded audio from `device` peaking at `hz`.
fn hears(peer: &mut Peer, device: &str, hz: f64, timeout: Duration) -> Value {
    let deadline = Instant::now() + timeout;
    loop {
        let s = peer.stats();
        if s["peaks"][device]
            .as_f64()
            .is_some_and(|p| (p - hz).abs() <= 20.0)
        {
            return s;
        }
        assert!(
            Instant::now() < deadline,
            "no {hz} Hz from {device} within {timeout:?}: {s}"
        );
        std::thread::sleep(Duration::from_millis(250));
    }
}

#[test]
#[ignore = "needs dilla-testhost (Go) and the WASI core"]
fn desktop_to_desktop_through_the_sfu_with_client_core_keys_and_an_epoch_change() {
    let host = start_host();
    let (mut call, mut alice, mut bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);

    let mut a = Peer::start(&url, &a_token, 440);
    let mut b = Peer::start(&url, &b_token, 1000);
    a.epoch(&alice_epoch(&alice));
    b.epoch(&bob_epoch(&bob));
    assert_eq!(
        a.next("connected", Duration::from_secs(30))["identity"],
        a_dev
    );
    assert_eq!(
        b.next("connected", Duration::from_secs(30))["identity"],
        b_dev
    );

    // A PCM microphone cannot be switched like a device one (review M3).
    a.send("mic default");
    let switched = a.next("mic", Duration::from_secs(5));
    assert!(
        switched["error"]
            .as_str()
            .is_some_and(|e| e.starts_with("E_UNSUPPORTED")),
        "{switched}"
    );

    // Each hears the other's tone, from the other's verified device.
    hears(&mut a, &b_dev, 1000.0, Duration::from_secs(20));
    hears(&mut b, &a_dev, 440.0, Duration::from_secs(20));
    // Two more seconds of audio (50 Opus frames a second) before counting.
    std::thread::sleep(Duration::from_secs(2));
    let (sa, sb) = (a.stats(), b.stats());
    let e1 = alice_epoch(&alice);
    let a_kid = format!("{:x}", (e1.own_leaf << 8) | (e1.epoch & 0xff));
    let b_leaf = e1.roster.iter().find(|(_, d)| hex(d) == b_dev).unwrap().0;
    let b_kid = format!("{:x}", (b_leaf << 8) | (e1.epoch & 0xff));
    assert!(opened(&sa, &b_kid) > 50, "{sa}");
    assert!(opened(&sb, &a_kid) > 50, "{sb}");
    // The room's SIF trailer reached the hook from LiveKit's join response (43 to 52 bytes).
    for s in [&sa, &sb] {
        let len = s["media"]["sif_trailer_len"].as_u64().unwrap();
        assert!((43..=52).contains(&len), "{s}");
    }
    assert_eq!(failed(&sa), 0, "{sa}");
    assert_eq!(failed(&sb), 0, "{sb}");

    // An epoch change mid-call: bob commits, both install the new epoch; audio carries on under
    // the new KIDs with no frame accepted under another key.
    bob.self_update(&mut call);
    alice.sync(&call);
    let (ae2, be2) = (alice_epoch(&alice), bob_epoch(&bob));
    assert_eq!(ae2.epoch, be2.epoch);
    assert_eq!(ae2.key, be2.key);
    b.epoch(&be2);
    a.epoch(&ae2);
    let kid2 = |leaf: u64| format!("{:x}", (leaf << 8) | (ae2.epoch & 0xff));
    let deadline = Instant::now() + Duration::from_secs(15);
    let (sa2, sb2) = loop {
        let (sa2, sb2) = (a.stats(), b.stats());
        if opened(&sa2, &kid2(b_leaf)) > 50 && opened(&sb2, &kid2(ae2.own_leaf)) > 50 {
            break (sa2, sb2);
        }
        assert!(
            Instant::now() < deadline,
            "no audio under epoch {}: {sa2} {sb2}",
            ae2.epoch
        );
        std::thread::sleep(Duration::from_millis(250));
    };
    assert_eq!(failed(&sa2), 0, "{sa2}");
    assert_eq!(failed(&sb2), 0, "{sb2}");
    hears(&mut a, &b_dev, 1000.0, Duration::from_secs(5));
    hears(&mut b, &a_dev, 440.0, Duration::from_secs(5));
    println!("LIVE alice {sa2}");
    println!("LIVE bob {sb2}");
}
