//! The HTTP client against a real dillad: password and TOTP sign-in.
//!
//! This is its own test binary because the testkit reads the host's control address from the process environment
//! (`DILLA_TESTKIT_CONTROL`): the variable is set once, before any other thread of this binary exists, so it cannot race
//! the environment reads (proxy settings) of a parallel test. Keep it the only test here.

use std::io::{BufRead, BufReader};
use std::net::{TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::{Duration, Instant};

use dilla_client::http::routes::{
    self, ChallengeAnswer, GetDeviceList, GetInstance, NoBody, PasswordLogin, PostChallenge,
    PostSession, Raw, SessionAnswer, TotpVerify, TotpVerifyAnswer,
};
use dilla_client::http::{AuthToken, ReqwestHttp};
use dilla_client::seams::{Http, HttpError};
use url::Url;

fn repo_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../..")
        .canonicalize()
        .expect("repo root")
}

// ---- sign_in_round_trip: a real dillad ----

/// A started dilla-testhost: its public and control origins and its bootstrap invite. It is stopped and its data removed
/// on drop.
struct TestHost {
    child: Child,
    base: String,
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

/// The WASI core the host loads: `DILLA_CORE_WASI` (the old name `DILLA_WASI_CORE` still works for one release) when set, else the repo's `dilla_core_wasi.wasm`, built the way the
/// e2e host builds it when it is missing.
fn wasi_core(root: &Path) -> PathBuf {
    if let Some(path) =
        std::env::var_os("DILLA_CORE_WASI").or_else(|| std::env::var_os("DILLA_WASI_CORE"))
    {
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
    fn start(port: u16, control_port: u16) -> Self {
        let root = repo_root();
        let binary = testhost_binary(&root);
        let core = wasi_core(&root);
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
#[ignore = "needs dillad: the Go test host (run with --ignored; desktop-gpui runs it on Linux)"]
fn sign_in_round_trip() {
    use dilla_core::cbor::{Encoder, decode_strict};
    use dilla_core::identity::{Kind, Tier};
    use dilla_core::ids::UserId;
    use dilla_testkit::{HttpDs, TestClient};

    // The testkit reads the host's fake clock from this variable when it enrols the authenticator app. It is set first,
    // before the host, its reader thread or a runtime exist.
    let (port, control_port) = (free_port(), free_port());
    // SAFETY: this binary holds only this test, and no thread of ours has started; nothing reads or writes the
    // environment concurrently.
    #[allow(unsafe_code)]
    unsafe {
        std::env::set_var(
            "DILLA_TESTKIT_CONTROL",
            format!("http://127.0.0.1:{control_port}"),
        );
    }
    let host = TestHost::start(port, control_port);

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
            ReqwestHttp::new(url.clone(), AuthToken::new(session.token.as_str())).expect("client");
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
