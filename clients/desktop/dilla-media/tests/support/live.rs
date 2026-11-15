//! The live tests' world (tests/live.rs, tests/capture.rs): dilla-testhost's in-process LiveKit SFU, its debug tokens,
//! and `call-peer` processes driven over stdin/stdout, each a desktop `dilla_media::Call` (the hook is process-wide).

#![allow(dead_code)]

use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, Command, Stdio};
use std::sync::mpsc::{Receiver, channel};
use std::time::{Duration, Instant};

use crate::client_support::{CALL, Core, RawPeer, decode_media_epoch};
use serde_json::{Value, json};

pub fn repo_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .canonicalize()
        .unwrap()
}

pub fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0")
        .unwrap()
        .local_addr()
        .unwrap()
        .port()
}

pub fn testhost_binary(root: &Path) -> PathBuf {
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

pub fn wasi_core(root: &Path) -> PathBuf {
    std::env::var_os("DILLA_CORE_WASI")
        .map(PathBuf::from)
        .unwrap_or_else(|| root.join("internal/mlswasi/testdata/dilla_core_wasi.wasm"))
}

pub struct Host {
    child: Child,
    pub control: String,
}

impl Drop for Host {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

pub fn start_host() -> Host {
    start_host_with(&[])
}

/// [`start_host`] with more flags for dilla-testhost (`-sfu-av1`: the SFU offers AV1 too).
pub fn start_host_with(flags: &[&str]) -> Host {
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
        .args(flags)
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
pub fn token(control: &str, room: &str, identity: &str) -> (String, String) {
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

pub fn hex(b: &[u8]) -> String {
    b.iter().map(|x| format!("{x:02x}")).collect()
}

/// One `call-peer` process.
pub struct Peer {
    child: Child,
    stdin: ChildStdin,
    lines: Receiver<Value>,
    seen: Vec<Value>,
}

impl Peer {
    pub fn start(url: &str, token: &str, tone: u32) -> Peer {
        Self::start_with(url, token, &["--tone", &tone.to_string()])
    }

    /// A peer that publishes no microphone (listen only), and whatever else it is told to.
    pub fn listening(url: &str, token: &str) -> Peer {
        Self::start_with(url, token, &[])
    }

    pub fn start_with(url: &str, token: &str, extra: &[&str]) -> Peer {
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
            .args(["--url", url, "--token", token])
            .args(extra)
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

    pub fn send(&mut self, line: &str) {
        writeln!(self.stdin, "{line}").unwrap();
        self.stdin.flush().unwrap();
    }

    pub fn next(&mut self, event: &str, timeout: Duration) -> Value {
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

    pub fn epoch(&mut self, e: &Epoch) {
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

    pub fn stats(&mut self) -> Value {
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

pub struct Epoch {
    pub epoch: u64,
    pub key: [u8; 16],
    pub own_leaf: u64,
    pub roster: Vec<(u64, [u8; 16])>,
}

pub fn alice_epoch(alice: &Core) -> Epoch {
    let m = decode_media_epoch(&alice.core.call_media_epoch(&CALL).unwrap());
    Epoch {
        epoch: m.epoch,
        key: m.base_key,
        own_leaf: m.own_leaf,
        roster: m.roster.iter().map(|(l, d, _)| (*l, *d)).collect(),
    }
}

pub fn bob_epoch(bob: &RawPeer) -> Epoch {
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

pub fn opened(stats: &Value, kid: &str) -> u64 {
    stats["media"]["opened"][kid].as_u64().unwrap_or(0)
}

pub fn failed(stats: &Value) -> u64 {
    stats["media"]["failed"]
        .as_object()
        .map(|o| o.values().filter_map(Value::as_u64).sum())
        .unwrap_or(0)
}

/// Waits until `peer` has decoded audio from `device` peaking at `hz`.
pub fn hears(peer: &mut Peer, device: &str, hz: f64, timeout: Duration) -> Value {
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
