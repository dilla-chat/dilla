#![cfg(target_os = "linux")]
#![allow(unsafe_code)] // CommandExt maps the test socket onto fd 3.

use dilla_core::{
    bot::{
        HelperRequest, HelperResponse,
        keypackage::{Intent, IntentResult, Resolution},
    },
    ids::MsgId,
};
use std::{
    fs,
    io::{Read, Write},
    os::{
        fd::AsRawFd,
        unix::{fs::PermissionsExt, net::UnixStream},
    },
    path::{Path, PathBuf},
    process::{Command, Stdio},
    sync::atomic::{AtomicU64, Ordering},
    thread,
    time::{Duration, Instant, SystemTime, UNIX_EPOCH},
};

const INSTANCE: [u8; 16] = [0x31; 16];
const COMMUNITY: [u8; 16] = [0x32; 16];
const INSTALLATION: [u8; 16] = [0x33; 16];
const INSTANCE_KEY: [u8; 32] = [0x77; 32];
const USER: [u8; 16] = [0x34; 16];
const KEY: [u8; 32] = [0x53; 32];

#[test]
fn op5_recovers_exact_pending_publication_after_helper_dies_before_reply_is_read_and_op14_resolves()
{
    let path = temp_dir();
    let intent = Intent {
        operation_id: MsgId::from_bytes([0x41; 16]),
        count: 2,
        last_resort: 1,
        expected_generation: 0,
    };
    let encoded_intent = intent.encode().expect("typed intent");
    {
        let (mut client, server) = UnixStream::pair().expect("socket pair");
        let mut child = spawn(&path, &server);
        drop(server);
        write_request(&mut client, [1; 16], 1, open_payload());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        write_request(&mut client, [2; 16], 2, bootstrap_payload());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        write_request(&mut client, [3; 16], 3, complete_payload());
        assert_eq!(read_response(&mut client, &mut child).status, 0);

        // Leave op5's response unread. Poll the encrypted journal through a second connection,
        // so the test kills the helper only after its native transaction has committed.
        write_request(&mut client, [4; 16], 5, encoded_intent.clone());
        wait_for_pending_row(&path, &intent.operation_id);
        child.kill().expect("kill helper after durable commit");
        assert!(!child.wait().expect("wait").success());
    }

    let stored = read_pending_result(&path, &intent.operation_id);
    let (mut client, server) = UnixStream::pair().expect("restart socket pair");
    let mut child = spawn(&path, &server);
    drop(server);
    write_request(&mut client, [5; 16], 1, open_payload());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    write_request(&mut client, [6; 16], 2, bootstrap_payload());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    write_request(&mut client, [7; 16], 5, encoded_intent);
    let recovered = read_response(&mut client, &mut child);
    assert_eq!(recovered.status, 0);
    assert_eq!(
        recovered.payload, stored,
        "op5 returns the exact persisted typed result"
    );
    let result = IntentResult::decode(&recovered.payload).expect("typed op5 result");
    assert_eq!(result.package_refs.len(), 3);

    let resolution = Resolution {
        operation_id: intent.operation_id,
        package_refs: result.package_refs,
        outcome: 0,
        now: 0,
        current_generation: 0,
    }
    .encode()
    .expect("typed op14 resolution");
    write_request(&mut client, [8; 16], 14, resolution);
    let resolved = read_response(&mut client, &mut child);
    assert_eq!(resolved.status, 0);
    assert_eq!(resolved.payload, [0x80], "op14 returns []");

    write_request(&mut client, [9; 16], 15, vec![0x80]);
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    assert!(child.wait().expect("helper shutdown").success());
    fs::remove_dir_all(path).expect("remove helper state dir");
}

fn wait_for_pending_row(path: &Path, operation_id: &MsgId) {
    let deadline = Instant::now() + Duration::from_secs(3);
    loop {
        let conn = test_cipher_conn(&path.join("state.db"), &KEY);
        let found = conn
            .query_row(
                "SELECT EXISTS(SELECT 1 FROM app_bot_keypackage_intents WHERE operation_id=?1 AND state=0 AND upload_body IS NOT NULL)",
                [operation_id.as_bytes().as_slice()],
                |r| r.get::<_, i64>(0),
            )
            .expect("read encrypted pending journal");
        drop(conn);
        if found == 1 {
            return;
        }
        assert!(
            Instant::now() < deadline,
            "op5 did not durably commit before timeout"
        );
        thread::sleep(Duration::from_millis(10));
    }
}

fn read_pending_result(path: &Path, operation_id: &MsgId) -> Vec<u8> {
    let conn = test_cipher_conn(&path.join("state.db"), &KEY);
    let (body, refs, expiry): (Vec<u8>, Vec<u8>, i64) = conn
        .query_row(
            "SELECT upload_body,package_refs,greatest_expiry FROM app_bot_keypackage_intents WHERE operation_id=?1",
            [operation_id.as_bytes().as_slice()],
            |r| Ok((r.get(0)?,r.get(1)?,r.get(2)?)),
        )
        .expect("pending result row");
    IntentResult {
        upload_body: body,
        package_refs: dilla_core::cbor::decode_strict(&refs, |d| {
            let n = d.array_len()?;
            let mut out = Vec::with_capacity(n);
            for _ in 0..n {
                out.push(d.bytes_exact::<32>()?);
            }
            Ok(out)
        })
        .expect("stored refs"),
        greatest_expiry: u64::try_from(expiry).expect("nonnegative expiry"),
    }
    .encode()
    .expect("stored typed result")
}

fn test_cipher_conn(path: &Path, key: &[u8; 32]) -> rusqlite::Connection {
    let conn = rusqlite::Connection::open(path).expect("open encrypted helper DB");
    let key_hex = key.iter().map(|b| format!("{b:02x}")).collect::<String>();
    conn.execute_batch(&format!(
        "PRAGMA cipher = 'chacha20'; PRAGMA key = 'raw:{key_hex}'; PRAGMA temp_store=MEMORY;"
    ))
    .expect("set the store key");
    conn
}

fn open_payload() -> Vec<u8> {
    let mut e = dilla_core::cbor::Encoder::new();
    e.array(4)
        .bytes(&INSTANCE)
        .bytes(&COMMUNITY)
        .bytes(&INSTALLATION)
        .bytes(&KEY);
    e.into_vec()
}

fn bootstrap_payload() -> Vec<u8> {
    let mut e = dilla_core::cbor::Encoder::new();
    e.array(4)
        .bytes(&INSTANCE)
        .bytes(&COMMUNITY)
        .bytes(&INSTALLATION)
        .bytes(&INSTANCE_KEY);
    e.into_vec()
}

fn complete_payload() -> Vec<u8> {
    let mut e = dilla_core::cbor::Encoder::new();
    e.array(3).bytes(&USER).text("native-bot").uint(100);
    e.into_vec()
}

fn spawn(path: &Path, stream: &UnixStream) -> std::process::Child {
    let fd = stream.as_raw_fd();
    unsafe {
        use std::os::unix::process::CommandExt;
        let mut command = Command::new(env!("CARGO_BIN_EXE_dilla-bot-core"));
        command
            .arg("--state-dir")
            .arg(path)
            .env_clear()
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        command.pre_exec(move || {
            if libc::dup2(fd, 3) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            let flags = libc::fcntl(3, libc::F_GETFD);
            if flags < 0 || libc::fcntl(3, libc::F_SETFD, flags & !libc::FD_CLOEXEC) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
        command.spawn().expect("spawn helper process")
    }
}

fn write_request(stream: &mut UnixStream, id: [u8; 16], operation: u64, payload: Vec<u8>) {
    let frame = HelperRequest {
        request_id: MsgId::from_bytes(id),
        operation,
        payload,
    }
    .encode_frame()
    .expect("encode helper request");
    stream.write_all(&frame).expect("write helper request");
}

fn read_response(stream: &mut UnixStream, child: &mut std::process::Child) -> HelperResponse {
    let mut prefix = [0; 4];
    if stream.read_exact(&mut prefix).is_err() {
        panic!("helper closed before reply: {:?}", child.try_wait());
    }
    let mut body = vec![0; u32::from_be_bytes(prefix) as usize];
    stream.read_exact(&mut body).expect("read helper reply");
    HelperResponse::decode(&body).expect("decode helper reply")
}

static NEXT: AtomicU64 = AtomicU64::new(0);
fn temp_dir() -> PathBuf {
    let nonce = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .expect("clock")
        .as_nanos() as u64;
    let path = std::env::temp_dir().join(format!(
        "dilla-keypackage-proc-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed) + nonce
    ));
    fs::create_dir(&path).expect("create helper state dir");
    fs::set_permissions(&path, fs::Permissions::from_mode(0o700)).expect("secure state dir");
    path
}
