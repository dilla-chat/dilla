//! The HTTP client against the TypeScript client's route table, a mock server, and a real dillad.

use std::collections::BTreeMap;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use dilla_client::http::routes::{
    self, Answer, BodyEncoding, ChallengeAnswer, GetAccountMe, GetBackup, GetDeviceList,
    GetInstance, ListCommunities, NegotiateSession, NegotiatedAnswer, NoBody, PasswordLogin,
    PasswordLoginAnswer, PostChallenge, PostSession, Raw, Reply, SessionAnswer, TotpVerify,
    TotpVerifyAnswer, Transport,
};
use dilla_client::http::{AuthToken, ReqwestHttp, fill_path};
use dilla_client::seams::{Http, HttpError};
use url::Url;

fn repo_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .canonicalize()
        .expect("repo root")
}

// ---- route_table_matches_ts ----

fn request_name(e: BodyEncoding) -> &'static str {
    match e {
        BodyEncoding::None => "none",
        BodyEncoding::Cbor => "cbor",
        BodyEncoding::Octet => "octet",
    }
}

fn answer_name(a: Answer) -> &'static str {
    match a {
        Answer::None => "none",
        Answer::Body => "body",
        Answer::Status => "status",
    }
}

/// `name | request | answer | auth | ok | transport`: everything the TypeScript source says of a route.
fn describe_rust(e: &routes::Entry) -> String {
    format!(
        "{} | {} | {} | auth={} | ok={:?} | {}",
        e.name,
        request_name(e.request),
        answer_name(e.answer),
        e.auth,
        e.ok,
        if e.transport == Transport::Socket {
            "socket"
        } else {
            "http"
        }
    )
}

fn describe_ts(v: &serde_json::Value) -> String {
    let ok: Vec<u64> = v["ok"]
        .as_array()
        .expect("ok")
        .iter()
        .map(|n| n.as_u64().expect("status"))
        .collect();
    format!(
        "{} | {} | {} | auth={} | ok={:?} | {}",
        v["name"].as_str().expect("name"),
        v["request"].as_str().expect("request"),
        v["response"].as_str().expect("response"),
        v["auth"].as_bool().expect("auth"),
        ok,
        v["transport"].as_str().expect("transport")
    )
}

#[test]
fn route_table_matches_ts() {
    let script = Path::new(env!("CARGO_MANIFEST_DIR")).join("tests/support/dump-routes.mjs");
    let out = Command::new("node")
        .arg(&script)
        .output()
        .expect("node runs the route dump");
    assert!(
        out.status.success(),
        "the route dump failed:\n{}",
        String::from_utf8_lossy(&out.stderr)
    );
    let ts: Vec<serde_json::Value> =
        serde_json::from_slice(&out.stdout).expect("the dump is a JSON array");
    assert!(ts.len() > 100, "the dump found only {} routes", ts.len());

    let key = |method: &str, path: &str| format!("{method} {path}");
    let ts_rows: BTreeMap<String, String> = ts
        .iter()
        .map(|v| {
            (
                key(
                    v["method"].as_str().expect("method"),
                    v["path"].as_str().expect("path"),
                ),
                describe_ts(v),
            )
        })
        .collect();
    let rust_rows: BTreeMap<String, String> = routes::TABLE
        .iter()
        .map(|e| (key(e.method.as_str(), e.path), describe_rust(e)))
        .collect();
    assert_eq!(
        rust_rows.len(),
        routes::TABLE.len(),
        "two Rust routes share a method and path"
    );

    let missing: Vec<_> = ts_rows
        .keys()
        .filter(|k| !rust_rows.contains_key(*k))
        .collect();
    let extra: Vec<_> = rust_rows
        .keys()
        .filter(|k| !ts_rows.contains_key(*k))
        .collect();
    assert!(
        missing.is_empty(),
        "TypeScript routes with no Rust Route: {missing:#?}"
    );
    assert!(
        extra.is_empty(),
        "Rust routes the TypeScript client does not have: {extra:#?}"
    );
    let different: Vec<_> = ts_rows
        .iter()
        .filter(|(k, v)| rust_rows[*k] != **v)
        .map(|(k, v)| format!("{k}\n  ts:   {v}\n  rust: {}", rust_rows[k]))
        .collect();
    assert!(
        different.is_empty(),
        "routes that differ:\n{}",
        different.join("\n")
    );
}

// ---- a mock server ----

/// Serves one connection: reads the request, answers with `response`, and returns the request it read.
fn mock(response: &'static [u8]) -> (Url, JoinHandle<String>) {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
    let url = Url::parse(&format!("http://{}", listener.local_addr().expect("addr"))).expect("url");
    let handle = std::thread::spawn(move || {
        let (mut stream, _) = listener.accept().expect("accept");
        let request = read_request(&mut stream);
        stream.write_all(response).expect("write");
        request
    });
    (url, handle)
}

/// Bytes as one char each, so a binary body survives in the request text.
fn latin1(bytes: &[u8]) -> String {
    bytes.iter().map(|&b| char::from(b)).collect()
}

fn read_request(stream: &mut TcpStream) -> String {
    stream
        .set_read_timeout(Some(Duration::from_secs(10)))
        .expect("timeout");
    let mut data = Vec::new();
    let mut buf = [0u8; 1024];
    loop {
        let n = stream.read(&mut buf).expect("read");
        data.extend_from_slice(&buf[..n]);
        let text = latin1(&data);
        if let Some(head_end) = text.find("\r\n\r\n") {
            let length = text[..head_end]
                .lines()
                .find_map(|l| {
                    l.to_ascii_lowercase()
                        .strip_prefix("content-length:")
                        .map(|v| v.trim().parse::<usize>().expect("length"))
                })
                .unwrap_or(0);
            if data.len() >= head_end + 4 + length || n == 0 {
                return text;
            }
        } else if n == 0 {
            return text;
        }
    }
}

fn http_at(url: &Url, token: &str) -> ReqwestHttp {
    let auth = if token.is_empty() {
        AuthToken::anonymous()
    } else {
        AuthToken::new(token)
    };
    ReqwestHttp::new(url.clone(), auth).expect("client")
}

#[tokio::test]
async fn error_status_maps() {
    // A 403 is HttpError::Status(403).
    let (url, served) =
        mock(b"HTTP/1.1 403 Forbidden\r\ncontent-length: 0\r\nconnection: close\r\n\r\n");
    let err = http_at(&url, "tok")
        .call::<GetAccountMe>(&[], NoBody)
        .await
        .expect_err("403 is an error");
    assert!(matches!(err, HttpError::Status(403)), "{err:?}");
    served.join().expect("mock");

    // A dropped connection is HttpError::Transport.
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
    let url = Url::parse(&format!("http://{}", listener.local_addr().expect("addr"))).expect("url");
    let dropper = std::thread::spawn(move || drop(listener.accept().expect("accept")));
    let err = http_at(&url, "tok")
        .call::<GetAccountMe>(&[], NoBody)
        .await
        .expect_err("a dropped connection is an error");
    assert!(matches!(err, HttpError::Transport(_)), "{err:?}");
    dropper.join().expect("dropper");

    // Nothing listening at all is Transport too.
    let gone = {
        let l = TcpListener::bind("127.0.0.1:0").expect("bind");
        Url::parse(&format!("http://{}", l.local_addr().expect("addr"))).expect("url")
    };
    let err = http_at(&gone, "tok")
        .call::<GetAccountMe>(&[], NoBody)
        .await
        .expect_err("refused");
    assert!(matches!(err, HttpError::Transport(_)), "{err:?}");
}

#[tokio::test]
async fn a_status_the_route_does_not_accept_is_an_error_even_when_another_route_accepts_it() {
    // 404 is a success for GetBackup (ok [200, 404]) and read back as the status; for GetAccountMe it is not.
    let (url, served) =
        mock(b"HTTP/1.1 404 Not Found\r\ncontent-length: 0\r\nconnection: close\r\n\r\n");
    let reply = http_at(&url, "tok")
        .call::<GetBackup>(&["0"], NoBody)
        .await
        .expect("404 is accepted");
    assert_eq!(
        reply,
        Reply {
            status: 404,
            body: vec![]
        }
    );
    served.join().expect("mock");

    let (url, served) =
        mock(b"HTTP/1.1 404 Not Found\r\ncontent-length: 0\r\nconnection: close\r\n\r\n");
    let err = http_at(&url, "tok")
        .call::<GetAccountMe>(&[], NoBody)
        .await
        .expect_err("404");
    assert!(matches!(err, HttpError::Status(404)), "{err:?}");
    served.join().expect("mock");
}

#[tokio::test]
async fn the_request_is_what_the_typescript_client_sends() {
    // A route with auth: false sends no bearer even when the transport holds one; the body is application/cbor.
    let (url, served) =
        mock(b"HTTP/1.1 200 OK\r\ncontent-length: 3\r\nconnection: close\r\n\r\n\x82\x61a");
    let body = routes::password_login_body("alice", "pw");
    let err = http_at(&url, "secret-token")
        .call::<PasswordLogin>(&[], body.clone())
        .await
        .expect_err("a short answer does not decode");
    assert!(matches!(err, HttpError::Decode(_)), "{err:?}");
    let request = served.join().expect("mock").to_ascii_lowercase();
    assert!(
        request.starts_with("post /v1/auth/password/login http/1.1"),
        "{request}"
    );
    assert!(
        request.contains("content-type: application/cbor"),
        "{request}"
    );
    assert!(!request.contains("authorization"), "{request}");

    // An authenticated route sends the bearer, with its query group when its argument is given and without it when not.
    let (url, served) =
        mock(b"HTTP/1.1 200 OK\r\ncontent-length: 1\r\nconnection: close\r\n\r\n\x80");
    http_at(&url, "secret-token")
        .call::<routes::GetDeviceListHistory>(&["c0ffee", "ab"], NoBody)
        .await
        .expect("answered");
    let request = served.join().expect("mock");
    assert!(
        request.starts_with("GET /v1/users/c0ffee/device-list?after=ab HTTP/1.1"),
        "{request}"
    );
    assert!(
        request
            .to_ascii_lowercase()
            .contains("authorization: bearer secret-token"),
        "{request}"
    );
    let (url, served) =
        mock(b"HTTP/1.1 200 OK\r\ncontent-length: 1\r\nconnection: close\r\n\r\n\x80");
    http_at(&url, "t")
        .call::<routes::GetDeviceListHistory>(&["c0ffee"], NoBody)
        .await
        .expect("answered");
    assert!(
        served
            .join()
            .expect("mock")
            .starts_with("GET /v1/users/c0ffee/device-list HTTP/1.1")
    );
}

#[tokio::test]
async fn negotiate_offers_the_versions_of_the_typescript_client() {
    let (url, served) =
        mock(b"HTTP/1.1 200 OK\r\ncontent-length: 4\r\nconnection: close\r\n\r\n\x83\x06\x01\x01");
    let answer = http_at(&url, "t")
        .call::<NegotiateSession>(&[], routes::negotiate_body())
        .await
        .expect("negotiated");
    assert_eq!(
        answer,
        NegotiatedAnswer {
            wire: 6,
            e2ee: 1,
            media: 1
        }
    );
    let request = served.join().expect("mock");
    // [[6], [1], [1]], as negotiate-session.ts encodes it.
    assert!(
        request.ends_with("\u{83}\u{81}\u{6}\u{81}\u{1}\u{81}\u{1}"),
        "{request:?}"
    );
    assert!(
        request
            .to_ascii_lowercase()
            .contains("content-type: application/cbor"),
        "{request}"
    );
}

#[tokio::test]
async fn a_gateway_route_is_not_sent_over_http() {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
    listener.set_nonblocking(true).expect("nonblocking");
    let url = Url::parse(&format!("http://{}", listener.local_addr().expect("addr"))).expect("url");
    let err = http_at(&url, "t")
        .call::<ListCommunities>(&[], NoBody)
        .await
        .expect_err("socket route");
    assert!(
        matches!(err, HttpError::Transport(ref m) if m.contains("gateway")),
        "{err:?}"
    );
    assert!(listener.accept().is_err(), "nothing was sent to the server");
}

#[tokio::test]
async fn a_redirect_is_not_followed() {
    let (url, served) = mock(b"HTTP/1.1 302 Found\r\nlocation: http://127.0.0.1:1/\r\ncontent-length: 0\r\nconnection: close\r\n\r\n");
    let err = http_at(&url, "t")
        .call::<GetAccountMe>(&[], NoBody)
        .await
        .expect_err("redirect");
    assert!(matches!(err, HttpError::Status(302)), "{err:?}");
    served.join().expect("mock");
}

#[test]
fn path_arguments_fill_the_template() {
    assert_eq!(
        fill_path("/v1/devices/{device_id}/sessions", &["ab12"]).unwrap(),
        "/v1/devices/ab12/sessions"
    );
    assert_eq!(fill_path("/i/{code}", &["a b/c"]).unwrap(), "/i/a%20b%2Fc");
    assert_eq!(
        fill_path(
            "/v1/groups/{group_id}/messages?from={from}&limit={limit}",
            &["aa", "0", "50"]
        )
        .unwrap(),
        "/v1/groups/aa/messages?from=0&limit=50"
    );
    assert_eq!(
        fill_path("/v1/communities/{c}/members?after={after}", &["aa"]).unwrap(),
        "/v1/communities/aa/members"
    );
    assert!(fill_path("/v1/devices/{device_id}", &[]).is_err());
    assert!(fill_path("/v1/devices/{device_id}", &["a", "b"]).is_err());
    assert!(fill_path("/v1/communities", &["a"]).is_err());
}

#[test]
fn typed_answers_decode_strictly() {
    use dilla_core::cbor::Encoder;
    let mut e = Encoder::new();
    e.array(3).text("assertion").uint(1).uint(1).text("extra");
    // A longer array than needed is read as `atLeast` reads it; here the array length 3 is a lie, so it is truncated.
    assert!(
        ciborium::value::Value::Bytes(e.into_vec())
            .deserialized::<PasswordLoginAnswer>()
            .is_err()
    );
    let mut e = Encoder::new();
    e.array(4).text("assertion").uint(1).uint(1).text("extra");
    let a = ciborium::value::Value::Bytes(e.into_vec())
        .deserialized::<PasswordLoginAnswer>()
        .expect("decodes");
    assert_eq!(
        a,
        PasswordLoginAnswer {
            assertion: "assertion".into(),
            needs: 1,
            passkey: true
        }
    );
    let mut e = Encoder::new();
    e.array(2).text("a").uint(3);
    assert!(
        ciborium::value::Value::Bytes(e.into_vec())
            .deserialized::<PasswordLoginAnswer>()
            .is_err(),
        "needs 3 is out of range"
    );
    // The negotiate answer is exactly what was offered.
    let ok = routes::negotiate_body();
    assert!(!ok.0.is_empty());
    let mut e = Encoder::new();
    e.array(3).uint(6).uint(1).uint(1);
    assert!(
        ciborium::value::Value::Bytes(e.into_vec())
            .deserialized::<NegotiatedAnswer>()
            .is_ok()
    );
    let mut e = Encoder::new();
    e.array(3).uint(5).uint(1).uint(1);
    assert!(
        ciborium::value::Value::Bytes(e.into_vec())
            .deserialized::<NegotiatedAnswer>()
            .is_err()
    );
}

// ---- sign_in_round_trip: a real dillad ----

/// A started dilla-testhost: its public and control origins and its bootstrap invite. It is stopped and its data removed
/// on drop.
struct TestHost {
    child: Child,
    base: String,
    control: String,
    invite: String,
    data_dir: PathBuf,
}

impl Drop for TestHost {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
        let _ = std::fs::remove_dir_all(&self.data_dir);
    }
}

fn free_port() -> u16 {
    TcpListener::bind("127.0.0.1:0")
        .expect("bind")
        .local_addr()
        .expect("addr")
        .port()
}

/// The dilla-testhost binary: `DILLA_TESTHOST` when set (CI's prebuilt tools), else built from `cmd/dilla-testhost`.
fn testhost_binary(root: &Path) -> PathBuf {
    if let Some(path) = std::env::var_os("DILLA_TESTHOST") {
        return PathBuf::from(path);
    }
    let out = root.join("target/dilla-testhost");
    let status = Command::new("go")
        .args(["build", "-o"])
        .arg(&out)
        .arg("./cmd/dilla-testhost")
        .current_dir(root)
        .status()
        .expect("go builds the test host");
    assert!(status.success(), "go build ./cmd/dilla-testhost failed");
    out
}

/// The WASI core the host loads: `DILLA_WASI_CORE` when set, else the repo's `dilla_core_wasi.wasm`, built the way the
/// e2e host builds it when it is missing.
fn wasi_core(root: &Path) -> PathBuf {
    if let Some(path) = std::env::var_os("DILLA_WASI_CORE") {
        return PathBuf::from(path);
    }
    let wasm = root.join("internal/mlswasi/testdata/dilla_core_wasi.wasm");
    if !wasm.exists() {
        let cargo = std::env::var("CARGO").unwrap_or_else(|_| "cargo".into());
        let status = Command::new(cargo)
            .args([
                "build",
                "-p",
                "dilla-core-wasi",
                "--target",
                "wasm32-wasip1",
                "--release",
                "--locked",
            ])
            .current_dir(root)
            .status()
            .expect("cargo builds the WASI core");
        assert!(status.success(), "building dilla-core-wasi failed");
        let target =
            std::env::var_os("CARGO_TARGET_DIR").map_or_else(|| root.join("target"), PathBuf::from);
        std::fs::copy(
            target.join("wasm32-wasip1/release/dilla_core_wasi.wasm"),
            &wasm,
        )
        .expect("copy the WASI core");
    }
    wasm
}

impl TestHost {
    fn start() -> Self {
        let root = repo_root();
        let binary = testhost_binary(&root);
        let core = wasi_core(&root);
        let (port, control_port) = (free_port(), free_port());
        let data_dir =
            std::env::temp_dir().join(format!("dilla-client-http-{}-{port}", std::process::id()));
        let mut child = Command::new(binary)
            .args([
                "-listen",
                &format!("127.0.0.1:{port}"),
                "-control",
                &format!("127.0.0.1:{control_port}"),
            ])
            .arg("-core")
            .arg(core)
            .args(["-log-level", "warn", "-production-acl", "-data-dir"])
            .arg(&data_dir)
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .spawn()
            .expect("start dilla-testhost");
        let stdout = child.stdout.take().expect("stdout");
        let (tx, rx) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            for line in BufReader::new(stdout).lines().map_while(Result::ok) {
                if let Some(code) = line.split("DILLA_TESTKIT_INVITE=").nth(1) {
                    // A comma-separated list of codes (the e2e `instanceInvite` takes the first).
                    let _ = tx.send(
                        code.split_whitespace()
                            .next()
                            .unwrap_or_default()
                            .split(',')
                            .next()
                            .unwrap_or_default()
                            .to_owned(),
                    );
                }
            }
        });
        let mut host = Self {
            child,
            base: format!("http://127.0.0.1:{port}"),
            control: format!("http://127.0.0.1:{control_port}"),
            invite: String::new(),
            data_dir,
        };
        let invite: String = rx
            .recv_timeout(Duration::from_secs(90))
            .expect("dilla-testhost printed its invite");
        let deadline = Instant::now() + Duration::from_secs(60);
        while TcpStream::connect(("127.0.0.1", port)).is_err() {
            assert!(Instant::now() < deadline, "dilla-testhost does not answer");
            std::thread::sleep(Duration::from_millis(100));
        }
        host.invite = invite;
        host
    }
}

/// RFC 6238 (HMAC-SHA1, 6 digits) for a base32 secret.
fn totp_code(secret_b32: &str, counter: u64) -> String {
    use hmac::{Hmac, KeyInit, Mac};
    let mut bits = 0u32;
    let mut n = 0;
    let mut key = Vec::new();
    for c in secret_b32.bytes().filter(|c| *c != b'=') {
        let v = match c.to_ascii_uppercase() {
            c @ b'A'..=b'Z' => c - b'A',
            c @ b'2'..=b'7' => c - b'2' + 26,
            other => panic!("not base32: {other}"),
        };
        bits = (bits << 5) | u32::from(v);
        n += 5;
        if n >= 8 {
            n -= 8;
            key.push(u8::try_from((bits >> n) & 0xff).expect("byte"));
        }
    }
    let mut mac = Hmac::<sha1::Sha1>::new_from_slice(&key).expect("key");
    mac.update(&counter.to_be_bytes());
    let d = mac.finalize().into_bytes();
    let o = usize::from(d[d.len() - 1] & 0x0f);
    let word = u32::from_be_bytes([d[o] & 0x7f, d[o + 1], d[o + 2], d[o + 3]]);
    format!("{:06}", word % 1_000_000)
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

#[test]
fn sign_in_round_trip() {
    use dilla_core::cbor::{Encoder, decode_strict};
    use dilla_core::identity::{Kind, Tier};
    use dilla_core::ids::UserId;
    use dilla_testkit::{HttpDs, TestClient};

    let host = TestHost::start();
    // The testkit reads the host's fake clock from here when it enrols the authenticator app.
    // SAFETY: nothing else in this test process reads or writes the environment while this runs.
    #[allow(unsafe_code)]
    unsafe {
        std::env::set_var("DILLA_TESTKIT_CONTROL", &host.control);
    }

    // An account, enrolled the way every native peer is: the bootstrap invite, an authenticator app, a password.
    let seed = 0x51_61_6e;
    let first = TestClient::new(
        "alice",
        UserId::from_bytes([0; 16]),
        Tier::Native,
        Kind::User,
        seed,
    )
    .expect("client");
    let enrolled = HttpDs::redeem_invite(
        &host.base,
        &host.invite,
        first.new_account("alice", "Alice"),
    )
    .expect("invite redeems");
    let alice = TestClient::new(
        "alice",
        UserId::from_bytes(enrolled.user_id),
        Tier::Native,
        Kind::User,
        seed,
    )
    .expect("client");
    let totp = HttpDs::enrol_second_factor(&host.base, &enrolled.token, false)
        .expect("enrols")
        .expect("a factor is owed");
    let mut ds = HttpDs::with_session(&host.base, alice.device_id(), enrolled.token.clone())
        .expect("session");
    ds.set_totp(Some(totp.clone()));
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .expect("clock")
        .as_secs();
    ds.put_device_list(&enrolled.user_id, &alice.signed_device_list(now))
        .expect("device list");
    ds.set_password("correct horse battery staple")
        .expect("password");

    // A second device signs in with the dilla-client transport: password, TOTP, challenge, session.
    let second = alice
        .second_device("alice-2", seed + 1)
        .expect("second device");
    let registration = second.registration();
    let device_hex = hex(registration.device_id.as_bytes());
    let url = Url::parse(&host.base).expect("url");
    let rt = tokio::runtime::Runtime::new().expect("runtime");
    rt.block_on(async {
        let http = ReqwestHttp::new(url.clone(), AuthToken::anonymous()).expect("client");

        // A wrong password is refused with a status.
        let err = http
            .call::<PasswordLogin>(
                &[],
                routes::password_login_body("alice", "not the password"),
            )
            .await
            .expect_err("a wrong password");
        assert!(
            matches!(err, HttpError::Status(s) if (400..500).contains(&s)),
            "{err:?}"
        );

        let login = http
            .call::<PasswordLogin>(
                &[],
                routes::password_login_body("alice", "correct horse battery staple"),
            )
            .await
            .expect("password login");
        assert_eq!(
            login.needs, 1,
            "an account with an authenticator app owes its code"
        );

        // The instance refuses a counter at or below the last it took: the next one is inside its window.
        let code = totp_code(&totp.secret, totp.last_counter + 1);
        let verified = http
            .call::<TotpVerify>(&[], routes::totp_verify_body(&login.assertion, &code))
            .await
            .expect("totp verify");
        let TotpVerifyAnswer { assertion } = verified;

        let challenge: ChallengeAnswer = http
            .call::<PostChallenge>(&[&device_hex], Raw::empty_list())
            .await
            .expect("challenge");
        let Raw(instance) = http
            .call::<GetInstance>(&[], NoBody)
            .await
            .expect("instance");
        let instance_id = decode_strict(&instance, |d| {
            let len = d.array_len()?;
            for _ in 0..3 {
                d.skip()?;
            }
            let id = d.bytes_exact::<16>()?;
            for _ in 4..len {
                d.skip()?;
            }
            Ok(id)
        })
        .expect("the instance document");

        // "dilla session v1" || instance_id || device_id || nonce || purpose 0 (protocol/09)
        let mut preimage = b"dilla session v1".to_vec();
        preimage.extend_from_slice(&instance_id);
        preimage.extend_from_slice(registration.device_id.as_bytes());
        preimage.extend_from_slice(&challenge.nonce);
        preimage.push(0);
        let signature = second.device().sign(&preimage);
        let mut e = Encoder::new();
        e.array(5).bytes(&challenge.nonce).uint(0).bytes(&signature);
        e.array(5)
            .bytes(registration.device_id.as_bytes())
            .bytes(&registration.dsk_pub)
            .uint(u64::from(registration.tier))
            .uint(u64::from(registration.signer_tier))
            .bytes(&registration.credential);
        e.bytes(assertion.as_bytes());

        let session: SessionAnswer = http
            .call::<PostSession>(&[&device_hex], Raw(e.into_vec()))
            .await
            .expect("session");
        assert!(!session.token.is_empty());
        assert_eq!(
            session.user_id, enrolled.user_id,
            "the session is the account's"
        );
        assert_eq!(&session.device_id, registration.device_id.as_bytes());
        assert_eq!(
            session.scope, 1,
            "a sign-in that owes the account's setup reaches the enrolment routes"
        );

        // The session's token authenticates the next call: a pending session reads its own user's device list.
        let user_hex = hex(&session.user_id);
        let authed =
            ReqwestHttp::new(url.clone(), AuthToken::new(session.token.clone())).expect("client");
        let list = authed
            .call::<GetDeviceList>(&[&user_hex], NoBody)
            .await
            .expect("device list with the new session");
        assert_eq!(list.status, 200);
        assert!(!list.body.is_empty());
        let anonymous = http
            .call::<GetDeviceList>(&[&user_hex], NoBody)
            .await
            .expect_err("no bearer, no device list");
        assert!(matches!(anonymous, HttpError::Status(401)), "{anonymous:?}");
    });
}
