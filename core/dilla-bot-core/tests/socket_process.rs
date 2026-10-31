#![cfg(target_os = "linux")]
#![allow(unsafe_code)] // CommandExt::pre_exec is used only to map the test socket onto fd 3.

use dilla_core::{
    bot::{
        HelperRequest, HelperResponse,
        keypackage::{Intent, IntentResult, Resolution},
    },
    cbor::{Encoder, decode_strict},
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
};

#[path = "../../dilla-core/tests/client_support/mod.rs"]
mod client_support;

fn test_cipher_conn(path: &Path, key: &[u8; 32]) -> rusqlite::Connection {
    let conn = rusqlite::Connection::open(path).expect("open encrypted database");
    let key_hex = key.iter().map(|b| format!("{b:02x}")).collect::<String>();
    conn.execute_batch(&format!(
        "PRAGMA key = \"x'{key_hex}'\"; PRAGMA cipher_memory_security=ON; PRAGMA temp_store=MEMORY;"
    ))
    .expect("set SQLCipher key");
    conn
}

fn large_fixture_candidate(index: u32) -> client_support::Core {
    let conn = client_support::memory();
    let probe = conn.clone();
    let mut user = [0u8; 16];
    user[0] = 0x92;
    user[12..].copy_from_slice(&index.to_be_bytes());
    let mut core = dilla_core::client::ClientCore::open(conn).expect("open Add candidate");
    core.signup_begin(&client_support::INSTANCE)
        .expect("candidate signup");
    core.signup_complete(
        &user,
        &format!("welcome-candidate-{index}"),
        client_support::NOW,
        &client_support::INSTANCE_SIGNER_PUB,
    )
    .expect("candidate enrollment");
    let device = decode_strict(&core.identity().expect("candidate identity"), |d| {
        d.array(6)?;
        let _phase = d.uint()?;
        d.opt_bytes_exact::<16>()?;
        d.opt_bytes_exact::<16>()?;
        let device = d.opt_bytes_exact::<16>()?;
        d.text()?;
        d.uint()?;
        Ok(device)
    })
    .expect("candidate identity shape")
    .expect("candidate device id");
    client_support::Core {
        core,
        probe,
        user,
        device,
    }
}

fn stage_group_operation(
    client: &mut UnixStream,
    child: &mut std::process::Child,
    request_base: u8,
    input: &[u8],
) -> ([u8; 16], (u64, u64, [u8; 32])) {
    use sha2::{Digest, Sha256};
    let digest: [u8; 32] = Sha256::digest(input).into();
    let mut begin = Encoder::new();
    begin
        .array(4)
        .uint(0)
        .uint(6)
        .uint(input.len() as u64)
        .bytes(&digest);
    write_request(client, [request_base; 16], 16, begin.into_vec());
    let started = read_response(client, child);
    assert_eq!(started.status, 0);
    let transfer_id = decode_strict(&started.payload, |d| {
        d.array(1)?;
        d.bytes_exact::<16>()
    })
    .expect("transfer id");

    for (index, chunk) in input.chunks(700_000).enumerate() {
        let offset = index * 700_000;
        let mut append = Encoder::new();
        append
            .array(4)
            .uint(1)
            .bytes(&transfer_id)
            .uint(offset as u64)
            .bytes(chunk);
        write_request(
            client,
            [request_base.wrapping_add(1 + index as u8); 16],
            16,
            append.into_vec(),
        );
        assert_eq!(read_response(client, child).status, 0);
    }

    let mut execute = Encoder::new();
    execute.array(2).uint(2).bytes(&transfer_id);
    write_request(
        client,
        [request_base.wrapping_add(250); 16],
        16,
        execute.into_vec(),
    );
    let response = read_response(client, child);
    assert_eq!(response.status, 0);
    let descriptor = decode_strict(&response.payload, |d| {
        d.array(3)?;
        Ok((d.uint()?, d.uint()?, d.bytes_exact::<32>()?))
    })
    .expect("transfer result descriptor");
    (transfer_id, descriptor)
}

fn read_staged_result(
    client: &mut UnixStream,
    child: &mut std::process::Child,
    request_base: u8,
    transfer_id: [u8; 16],
    result_len: u64,
) -> Vec<u8> {
    let mut output = Vec::with_capacity(result_len as usize);
    let mut offset = 0u64;
    while offset < result_len {
        let len = (result_len - offset).min(700_000);
        let mut read = Encoder::new();
        read.array(4)
            .uint(3)
            .bytes(&transfer_id)
            .uint(offset)
            .uint(len);
        write_request(
            client,
            [request_base.wrapping_add((offset / 700_000) as u8); 16],
            16,
            read.into_vec(),
        );
        let response = read_response(client, child);
        assert_eq!(response.status, 0);
        let chunk = decode_strict(&response.payload, |d| {
            d.array(1)?;
            Ok(d.bytes()?.to_vec())
        })
        .expect("staged result chunk");
        assert_eq!(chunk.len(), len as usize);
        output.extend_from_slice(&chunk);
        offset += len;
    }
    output
}

static NEXT: AtomicU64 = AtomicU64::new(0);
fn temp_dir() -> PathBuf {
    let p = std::env::temp_dir().join(format!(
        "dilla-bot-proc-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
            + std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos() as u64
    ));
    fs::create_dir(&p).unwrap();
    fs::set_permissions(&p, fs::Permissions::from_mode(0o700)).unwrap();
    p
}
fn spawn(path: &PathBuf, stream: &UnixStream) -> std::process::Child {
    let fd = stream.as_raw_fd();
    unsafe {
        use std::os::unix::process::CommandExt;
        let mut c = Command::new(env!("CARGO_BIN_EXE_dilla-bot-core"));
        c.arg("--state-dir")
            .arg(path)
            .env_clear()
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            // Nobody reads child.stderr in this fixture. SQLCipher diagnostics
            // under a small memlock budget can fill that pipe and deadlock the
            // helper while the parent waits for its protocol response.
            .stderr(Stdio::inherit());
        c.pre_exec(move || {
            if libc::dup2(fd, 3) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            let flags = libc::fcntl(3, libc::F_GETFD);
            if flags < 0 || libc::fcntl(3, libc::F_SETFD, flags & !libc::FD_CLOEXEC) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
        c.spawn().unwrap()
    }
}
fn write_request(stream: &mut UnixStream, id: [u8; 16], op: u64, payload: Vec<u8>) {
    let frame = HelperRequest {
        request_id: MsgId::from_bytes(id),
        operation: op,
        payload,
    }
    .encode_frame()
    .unwrap();
    stream.write_all(&frame).unwrap()
}
fn write_raw(stream: &mut UnixStream, body: Vec<u8>) {
    stream
        .write_all(&(body.len() as u32).to_be_bytes())
        .unwrap();
    stream.write_all(&body).unwrap();
}
fn read_response(stream: &mut UnixStream, child: &mut std::process::Child) -> HelperResponse {
    let mut p = [0; 4];
    if stream.read_exact(&mut p).is_err() {
        let status = child.wait().unwrap();
        panic!("helper closed before response: {status}");
    }
    let mut body = vec![0; u32::from_be_bytes(p) as usize];
    stream.read_exact(&mut body).unwrap();
    HelperResponse::decode(&body).unwrap()
}

fn staging_fixture(
    path: &Path,
    ids: ([u8; 16], [u8; 16], [u8; 16]),
    key: [u8; 32],
    stage_exists: bool,
    committed: bool,
    published: bool,
) -> (PathBuf, PathBuf) {
    const MAGIC: &[u8] = b"DILLA-BOT-INIT-v1\0";
    let nonce = [0xA7; 16];
    let stage = path.join(format!(
        "state.db.init-{}",
        nonce.iter().map(|b| format!("{b:02x}")).collect::<String>()
    ));
    let marker = path.join("state.init");
    assert!(!committed || stage_exists);
    if stage_exists {
        let c = rusqlite::Connection::open(&stage).unwrap();
        let key_hex = key.iter().map(|b| format!("{b:02x}")).collect::<String>();
        c.execute_batch(&format!(
            "PRAGMA key = \"x'{key_hex}'\"; PRAGMA cipher_memory_security=ON; PRAGMA temp_store=MEMORY;"
        ))
            .unwrap();
        if committed {
            c.execute_batch("CREATE TABLE bot_helper_meta (k TEXT PRIMARY KEY, v BLOB NOT NULL); CREATE TABLE bot_state (area INTEGER NOT NULL, key BLOB NOT NULL, revision INTEGER NOT NULL, value BLOB, PRIMARY KEY(area,key)); CREATE TABLE bot_state_receipts (id BLOB PRIMARY KEY, digest BLOB NOT NULL, result BLOB NOT NULL);")
                .unwrap();
            c.execute(
                "INSERT INTO bot_helper_meta(k,v) VALUES('version',?1),('instance',?2),('community',?3),('installation',?4)",
                rusqlite::params![1i64, ids.0.as_slice(), ids.1.as_slice(), ids.2.as_slice()],
            )
            .unwrap();
        }
        drop(c);
        fs::set_permissions(&stage, fs::Permissions::from_mode(0o600)).unwrap();
    }
    let mut marker_bytes = Vec::new();
    marker_bytes.extend_from_slice(MAGIC);
    marker_bytes.extend_from_slice(&1i64.to_be_bytes());
    marker_bytes.extend_from_slice(&ids.0);
    marker_bytes.extend_from_slice(&ids.1);
    marker_bytes.extend_from_slice(&ids.2);
    marker_bytes.extend_from_slice(&nonce);
    fs::write(&marker, marker_bytes).unwrap();
    fs::set_permissions(&marker, fs::Permissions::from_mode(0o600)).unwrap();
    if published {
        fs::hard_link(&stage, path.join("state.db")).unwrap();
    }
    (stage, marker)
}

fn open_payload(ids: ([u8; 16], [u8; 16], [u8; 16]), key: [u8; 32]) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(4)
        .bytes(&ids.0)
        .bytes(&ids.1)
        .bytes(&ids.2)
        .bytes(&key);
    e.into_vec()
}

#[test]
fn real_binary_opens_keyed_store_over_inherited_socket_and_shuts_down() {
    let path = temp_dir();
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    let sentinel = b"socket-key-secret-not-for-logs";
    let mut key = [0u8; 32];
    key[..sentinel.len()].copy_from_slice(sentinel);
    let mut payload = Encoder::new();
    payload
        .array(4)
        .bytes(&[1; 16])
        .bytes(&[2; 16])
        .bytes(&[3; 16])
        .bytes(&key);
    write_request(&mut client, [9; 16], 1, payload.into_vec());
    let opened = read_response(&mut client, &mut child);
    assert_eq!(opened.status, 0);
    assert_eq!(opened.request_id.as_bytes(), &[9; 16]);
    write_request(&mut client, [10; 16], 15, {
        let mut e = Encoder::new();
        e.array(0);
        e.into_vec()
    });
    let stopped = read_response(&mut client, &mut child);
    assert_eq!(stopped.status, 0);
    let output = child.wait_with_output().unwrap();
    assert!(output.status.success());
    assert!(
        !String::from_utf8_lossy(&output.stdout).contains(std::str::from_utf8(sentinel).unwrap())
    );
    assert!(
        !String::from_utf8_lossy(&output.stderr).contains(std::str::from_utf8(sentinel).unwrap())
    );
    assert_eq!(
        fs::metadata(path.join("state.db"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o600
    );
    assert_eq!(
        fs::metadata(path.join("state.lock"))
            .unwrap()
            .permissions()
            .mode()
            & 0o777,
        0o600
    );
    let _ = fs::remove_dir_all(path);
}

#[test]
fn oversized_prefix_closes_without_reading_an_announced_body() {
    let path = temp_dir();
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    client.write_all(&(1_048_577u32).to_be_bytes()).unwrap();
    let _ = client.shutdown(std::net::Shutdown::Write);
    let mut byte = [0];
    assert_eq!(client.read(&mut byte).unwrap(), 0);
    assert!(child.wait().unwrap().success());
    let _ = fs::remove_dir_all(path);
}

#[test]
fn real_process_transfer_is_bounded_busy_until_drop_and_keeps_group_unavailable() {
    use sha2::{Digest, Sha256};
    let path = temp_dir();
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    let key = [0x31; 32];
    write_request(
        &mut client,
        [9; 16],
        1,
        open_payload(([1; 16], [2; 16], [3; 16]), key),
    );
    assert_eq!(read_response(&mut client, &mut child).status, 0);

    let sentinel = b"transfer-group-info-secret";
    let mut target = Encoder::new();
    target
        .array(8)
        .uint(5)
        .bytes(&[4; 16])
        .bytes(&[5; 16])
        .bytes(&[6; 16])
        .uint(0)
        .uint(0)
        .bytes(sentinel)
        .bytes(&[0x80]);
    let input = target.into_vec();
    let digest: [u8; 32] = Sha256::digest(&input).into();
    let mut begin = Encoder::new();
    begin
        .array(4)
        .uint(0)
        .uint(6)
        .uint(input.len() as u64)
        .bytes(&digest);
    write_request(&mut client, [10; 16], 16, begin.into_vec());
    let started = read_response(&mut client, &mut child);
    assert_eq!(started.status, 0);
    let id = decode_strict(&started.payload, |d| {
        d.array(1)?;
        d.bytes_exact::<16>()
    })
    .unwrap();

    write_request(&mut client, [11; 16], 16, {
        let mut e = Encoder::new();
        e.array(4).uint(1).bytes(&id).uint(0).bytes(&input);
        e.into_vec()
    });
    assert_eq!(read_response(&mut client, &mut child).status, 0);

    write_request(&mut client, [12; 16], 6, {
        let mut e = Encoder::new();
        e.array(0);
        e.into_vec()
    });
    assert_eq!(
        read_response(&mut client, &mut child).status,
        6,
        "unrelated op6 must report Busy while transfer owns the mutation slot"
    );

    write_request(&mut client, [13; 16], 16, {
        let mut e = Encoder::new();
        e.array(2).uint(2).bytes(&id);
        e.into_vec()
    });
    let executed = read_response(&mut client, &mut child);
    assert_eq!(executed.status, 0);
    let descriptor = decode_strict(&executed.payload, |d| {
        d.array(3)?;
        Ok((d.uint()?, d.uint()?, d.bytes_exact::<32>()?))
    })
    .unwrap();
    assert_eq!(
        descriptor.0, 5,
        "production group adapters remain unavailable until Task5B"
    );
    assert_eq!(descriptor.1, 1);
    assert_eq!(descriptor.2, Sha256::digest([0x80]).as_slice());
    write_request(&mut client, [14; 16], 16, {
        let mut e = Encoder::new();
        e.array(2).uint(2).bytes(&id);
        e.into_vec()
    });
    assert_eq!(
        read_response(&mut client, &mut child).payload,
        executed.payload,
        "repeat execute returns the cached descriptor"
    );

    write_request(&mut client, [15; 16], 16, {
        let mut e = Encoder::new();
        e.array(4).uint(3).bytes(&id).uint(0).uint(1);
        e.into_vec()
    });
    let read = read_response(&mut client, &mut child);
    assert_eq!(
        decode_strict(&read.payload, |d| {
            d.array(1)?;
            Ok(d.bytes()?.to_vec())
        })
        .unwrap(),
        [0x80]
    );
    write_request(&mut client, [16; 16], 16, {
        let mut e = Encoder::new();
        e.array(2).uint(4).bytes(&id);
        e.into_vec()
    });
    assert_eq!(read_response(&mut client, &mut child).status, 0);

    // An invalid declared length is rejected before allocating and leaves no active transfer.
    let mut oversized = Encoder::new();
    oversized
        .array(4)
        .uint(0)
        .uint(6)
        .uint(32 * 1024 * 1024 + 1)
        .bytes(&digest);
    write_request(&mut client, [17; 16], 16, oversized.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 1);
    write_request(&mut client, [18; 16], 15, {
        let mut e = Encoder::new();
        e.array(0);
        e.into_vec()
    });
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let output = child.wait_with_output().unwrap();
    assert!(output.status.success());
    let sentinel = std::str::from_utf8(sentinel).unwrap();
    assert!(!String::from_utf8_lossy(&output.stdout).contains(sentinel));
    assert!(!String::from_utf8_lossy(&output.stderr).contains(sentinel));
    let _ = fs::remove_dir_all(path);
}

#[test]
fn real_process_disconnect_and_restart_invalidate_transfer_id() {
    let path = temp_dir();
    let key = [0x42; 32];
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    write_request(
        &mut client,
        [9; 16],
        1,
        open_payload(([1; 16], [2; 16], [3; 16]), key),
    );
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let mut begin = Encoder::new();
    begin.array(4).uint(0).uint(6).uint(1).bytes(&[0; 32]);
    write_request(&mut client, [10; 16], 16, begin.into_vec());
    let started = read_response(&mut client, &mut child);
    let id = decode_strict(&started.payload, |d| {
        d.array(1)?;
        d.bytes_exact::<16>()
    })
    .unwrap();
    drop(client);
    assert!(child.wait().unwrap().success());

    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    write_request(
        &mut client,
        [11; 16],
        1,
        open_payload(([1; 16], [2; 16], [3; 16]), key),
    );
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    write_request(&mut client, [12; 16], 16, {
        let mut e = Encoder::new();
        e.array(2).uint(4).bytes(&id);
        e.into_vec()
    });
    assert_eq!(read_response(&mut client, &mut child).status, 1);
    write_request(&mut client, [13; 16], 15, {
        let mut e = Encoder::new();
        e.array(0);
        e.into_vec()
    });
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    assert!(child.wait().unwrap().success());
    let _ = fs::remove_dir_all(path);
}

#[test]
fn request_before_open_and_second_open_are_refused() {
    let path = temp_dir();
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    let mut pre = Encoder::new();
    pre.array(3).bytes(&[1; 16]).bytes(&[2; 16]).bytes(&[3; 16]);
    write_request(&mut client, [1; 16], 2, pre.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 1);
    let mut open = Encoder::new();
    open.array(4)
        .bytes(&[1; 16])
        .bytes(&[2; 16])
        .bytes(&[3; 16])
        .bytes(&[4; 32]);
    write_request(&mut client, [2; 16], 1, open.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let mut open_again = Encoder::new();
    open_again
        .array(4)
        .bytes(&[9; 16])
        .bytes(&[2; 16])
        .bytes(&[3; 16])
        .bytes(&[4; 32]);
    write_request(&mut client, [3; 16], 1, open_again.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 1);
    let mut shutdown = Encoder::new();
    shutdown.array(0);
    write_request(&mut client, [4; 16], 15, shutdown.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    assert!(child.wait().unwrap().success());
    let _ = fs::remove_dir_all(path);
}

#[test]
fn separate_helper_processes_cannot_open_the_same_identity() {
    let path = temp_dir();
    let (mut first_client, first_server) = UnixStream::pair().unwrap();
    let mut first = spawn(&path, &first_server);
    drop(first_server);
    let mut open = Encoder::new();
    open.array(4)
        .bytes(&[1; 16])
        .bytes(&[2; 16])
        .bytes(&[3; 16])
        .bytes(&[4; 32]);
    write_request(&mut first_client, [1; 16], 1, open.into_vec());
    assert_eq!(read_response(&mut first_client, &mut first).status, 0);

    let (mut second_client, second_server) = UnixStream::pair().unwrap();
    let mut second = spawn(&path, &second_server);
    drop(second_server);
    let mut open = Encoder::new();
    open.array(4)
        .bytes(&[1; 16])
        .bytes(&[2; 16])
        .bytes(&[3; 16])
        .bytes(&[4; 32]);
    write_request(&mut second_client, [2; 16], 1, open.into_vec());
    assert_eq!(read_response(&mut second_client, &mut second).status, 6);
    drop(second_client);
    assert!(second.wait().unwrap().success());

    let mut shutdown = Encoder::new();
    shutdown.array(0);
    write_request(&mut first_client, [3; 16], 15, shutdown.into_vec());
    assert_eq!(read_response(&mut first_client, &mut first).status, 0);
    assert!(first.wait().unwrap().success());
    let _ = fs::remove_dir_all(path);
}

#[test]
fn staged_first_open_crash_windows_recover_only_marker_owned_state() {
    let ids = ([41; 16], [42; 16], [43; 16]);
    let key = [0x5A; 32];

    for (stage_exists, committed, published) in [
        (false, false, false),
        (true, false, false),
        (true, true, false),
        (true, true, true),
    ] {
        let path = temp_dir();
        let (stage, marker) = staging_fixture(&path, ids, key, stage_exists, committed, published);
        let before_stage = stage_exists.then(|| fs::read(&stage).unwrap());
        let before_marker = fs::read(&marker).unwrap();
        let (mut client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);

        if committed && !published {
            write_request(
                &mut client,
                [1; 16],
                1,
                open_payload(([99; 16], ids.1, ids.2), key),
            );
            assert_eq!(read_response(&mut client, &mut child).status, 4);
            write_request(&mut client, [2; 16], 1, open_payload(ids, [0x6B; 32]));
            assert_eq!(read_response(&mut client, &mut child).status, 1);
            assert_eq!(
                fs::read(&stage).unwrap(),
                before_stage.as_ref().unwrap().as_slice()
            );
            assert_eq!(fs::read(&marker).unwrap(), before_marker);
        }

        write_request(&mut client, [3; 16], 1, open_payload(ids, key));
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(path.join("state.db").exists());
        assert!(!stage.exists());
        assert!(!marker.exists());
        let mut shutdown = Encoder::new();
        shutdown.array(0);
        write_request(&mut client, [4; 16], 15, shutdown.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
        let _ = fs::remove_dir_all(path);
    }
}

#[test]
fn missing_or_wrong_database_key_is_refused_without_key_logs() {
    let path = temp_dir();
    {
        let (mut client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);
        let mut missing = Encoder::new();
        missing
            .array(3)
            .bytes(&[1; 16])
            .bytes(&[2; 16])
            .bytes(&[3; 16]);
        write_request(&mut client, [15; 16], 1, missing.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 1);
        let mut zero = Encoder::new();
        zero.array(4)
            .bytes(&[1; 16])
            .bytes(&[2; 16])
            .bytes(&[3; 16])
            .bytes(&[0; 32]);
        write_request(&mut client, [16; 16], 1, zero.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 1);
        let mut open = Encoder::new();
        open.array(4)
            .bytes(&[1; 16])
            .bytes(&[2; 16])
            .bytes(&[3; 16])
            .bytes(&[0x51; 32]);
        write_request(&mut client, [1; 16], 1, open.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut shutdown = Encoder::new();
        shutdown.array(0);
        write_request(&mut client, [2; 16], 15, shutdown.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
    }
    let sentinel = b"wrong-key-never-log";
    let mut wrong = [0x66; 32];
    wrong[..sentinel.len()].copy_from_slice(sentinel);
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    let mut open = Encoder::new();
    open.array(4)
        .bytes(&[1; 16])
        .bytes(&[2; 16])
        .bytes(&[3; 16])
        .bytes(&wrong);
    write_request(&mut client, [3; 16], 1, open.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 1);
    drop(client);
    let output = child.wait_with_output().unwrap();
    assert!(
        !String::from_utf8_lossy(&output.stdout).contains(std::str::from_utf8(sentinel).unwrap())
    );
    assert!(
        !String::from_utf8_lossy(&output.stderr).contains(std::str::from_utf8(sentinel).unwrap())
    );
    let _ = fs::remove_dir_all(path);
}

#[test]
fn truncated_frame_and_unsupported_envelopes_close_cleanly() {
    {
        let raw = {
            let mut e = Encoder::new();
            e.array(4).uint(2).bytes(&[1; 16]).uint(1).bytes(&[0x80]);
            e.into_vec()
        };
        let path = temp_dir();
        let (mut client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);
        write_raw(&mut client, raw);
        let mut byte = [0];
        assert_eq!(client.read(&mut byte).unwrap(), 0);
        assert!(child.wait().unwrap().success());
        let _ = fs::remove_dir_all(path);
    }
    let path = temp_dir();
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    let mut empty = Encoder::new();
    empty.array(0);
    let mut unknown = Encoder::new();
    unknown
        .array(4)
        .uint(1)
        .bytes(&[7; 16])
        .uint(99)
        .bytes(&empty.into_vec());
    write_raw(&mut client, unknown.into_vec());
    let invalid = read_response(&mut client, &mut child);
    assert_eq!(invalid.status, 1);
    assert_eq!(invalid.request_id.as_bytes(), &[7; 16]);
    drop(client);
    assert!(child.wait().unwrap().success());
    let _ = fs::remove_dir_all(path);

    let path = temp_dir();
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    client.write_all(&32u32.to_be_bytes()).unwrap();
    client.write_all(&[0x81; 5]).unwrap();
    let _ = client.shutdown(std::net::Shutdown::Write);
    let mut byte = [0];
    assert_eq!(client.read(&mut byte).unwrap(), 0);
    assert!(child.wait().unwrap().success());
    let _ = fs::remove_dir_all(path);
}

#[test]
fn invalid_socket_fd_is_rejected() {
    use std::os::fd::AsRawFd;
    let path = temp_dir();
    let null = std::fs::File::open("/dev/null").unwrap();
    let fd = null.as_raw_fd();
    let child = unsafe {
        use std::os::unix::process::CommandExt;
        let mut c = Command::new(env!("CARGO_BIN_EXE_dilla-bot-core"));
        c.arg("--state-dir")
            .arg(&path)
            .env_clear()
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        c.pre_exec(move || {
            if libc::dup2(fd, 3) < 0 {
                return Err(std::io::Error::last_os_error());
            }
            Ok(())
        });
        c.spawn().unwrap()
    };
    assert!(!child.wait_with_output().unwrap().status.success());
    let _ = fs::remove_dir_all(path);
}

#[test]
fn bot_identity_and_key_packages_reopen_across_processes() {
    let path = temp_dir();
    let registration;
    let intent = Intent {
        operation_id: MsgId::from_bytes([0x61; 16]),
        count: 1,
        last_resort: 0,
        expected_generation: 0,
    }
    .encode()
    .expect("typed key package intent");
    let original_result;
    {
        let (mut client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);
        let mut open = Encoder::new();
        open.array(4)
            .bytes(&[11; 16])
            .bytes(&[12; 16])
            .bytes(&[13; 16])
            .bytes(&[0x73; 32]);
        write_request(&mut client, [1; 16], 1, open.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut bootstrap = Encoder::new();
        bootstrap
            .array(4)
            .bytes(&[11; 16])
            .bytes(&[12; 16])
            .bytes(&[13; 16])
            .bytes(&[0x77; 32]);
        write_request(&mut client, [2; 16], 2, bootstrap.into_vec());
        let first = read_response(&mut client, &mut child);
        assert_eq!(first.status, 0);
        registration = first.payload;
        let mut complete = Encoder::new();
        complete
            .array(3)
            .bytes(&[14; 16])
            .text("native-bot")
            .uint(100);
        write_request(&mut client, [3; 16], 3, complete.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        write_request(&mut client, [7; 16], 5, intent.clone());
        let generated = read_response(&mut client, &mut child);
        assert_eq!(generated.status, 0);
        original_result = generated.payload;
        let mut changes = Encoder::new();
        changes
            .array(1)
            .array(4)
            .uint(0)
            .bytes(b"setting")
            .uint(0)
            .bytes(&[0x81, 9]);
        let mut tx = Encoder::new();
        tx.array(2).bytes(&[21; 16]).raw(&changes.into_vec());
        write_request(&mut client, [30; 16], 12, tx.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut shut = Encoder::new();
        shut.array(0);
        write_request(&mut client, [4; 16], 15, shut.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
    }
    {
        let (mut client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);
        let mut open = Encoder::new();
        open.array(4)
            .bytes(&[11; 16])
            .bytes(&[12; 16])
            .bytes(&[13; 16])
            .bytes(&[0x73; 32]);
        write_request(&mut client, [5; 16], 1, open.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut bootstrap = Encoder::new();
        bootstrap
            .array(4)
            .bytes(&[11; 16])
            .bytes(&[12; 16])
            .bytes(&[13; 16])
            .bytes(&[0x77; 32]);
        write_request(&mut client, [6; 16], 2, bootstrap.into_vec());
        let reopened = read_response(&mut client, &mut child);
        assert_eq!(reopened.status, 0);
        assert_eq!(reopened.payload, registration);
        write_request(&mut client, [7; 16], 5, intent);
        let reopened = read_response(&mut client, &mut child);
        assert_eq!(reopened.status, 0);
        assert_eq!(reopened.payload, original_result);
        let result = IntentResult::decode(&reopened.payload).expect("typed intent result");
        let resolution = Resolution {
            operation_id: MsgId::from_bytes([0x61; 16]),
            package_refs: result.package_refs,
            outcome: 0,
            now: 1,
            current_generation: 0,
        }
        .encode()
        .expect("typed resolution");
        write_request(&mut client, [71; 16], 14, resolution);
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut get = Encoder::new();
        get.array(3).uint(0).uint(0).bytes(b"setting");
        write_request(&mut client, [70; 16], 11, get.into_vec());
        let saved = read_response(&mut client, &mut child);
        assert_eq!(saved.status, 0);
        let mut expected = Encoder::new();
        expected.array(2).uint(1).bytes(&[0x81, 9]);
        assert_eq!(saved.payload, expected.into_vec());
        let mut shut = Encoder::new();
        shut.array(0);
        write_request(&mut client, [8; 16], 15, shut.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
    }
    let _ = fs::remove_dir_all(path);
}

#[test]
fn helper_group_create_intent_recovers_identical_upload_after_process_restart() {
    let path = temp_dir();
    let ids = ([0x21; 16], [0x22; 16], [0x23; 16]);
    let key = [0x73; 32];
    let operation = [0x31; 16];
    let group = [0x41; 16];
    let channel = [0x51; 16];
    let discard_operation = [0x37; 16];
    let discarded_group = [0x47; 16];
    let create = {
        let mut e = Encoder::new();
        e.array(7)
            .uint(2)
            .bytes(&operation)
            .bytes(&group)
            .bytes(&channel)
            .uint(0)
            .uint(0)
            .bytes(&[0x88; 32]);
        e.into_vec()
    };
    let original;
    {
        let (mut client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);
        write_request(&mut client, [1; 16], 1, open_payload(ids, key));
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut bootstrap = Encoder::new();
        bootstrap
            .array(4)
            .bytes(&ids.0)
            .bytes(&ids.1)
            .bytes(&ids.2)
            .bytes(&[0x77; 32]);
        write_request(&mut client, [2; 16], 2, bootstrap.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut complete = Encoder::new();
        complete
            .array(3)
            .bytes(&[0x24; 16])
            .text("group-bot")
            .uint(100);
        write_request(&mut client, [3; 16], 3, complete.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);

        write_request(&mut client, [4; 16], 6, create.clone());
        let created = read_response(&mut client, &mut child);
        assert_eq!(created.status, 0);
        original = created.payload;

        // Exact duplicate is an intent retry and returns the exact same server upload bytes.
        write_request(&mut client, [5; 16], 6, create.clone());
        assert_eq!(read_response(&mut client, &mut child).payload, original);

        // Reusing the operation id with different input refuses before another group mutation.
        let mut changed = Encoder::new();
        changed
            .array(7)
            .uint(2)
            .bytes(&operation)
            .bytes(&group)
            .bytes(&[0x52; 16])
            .uint(0)
            .uint(0)
            .bytes(&[0x88; 32]);
        write_request(&mut client, [6; 16], 6, changed.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 4);

        // Interaction groups are explicitly refused at this adapter boundary.
        let mut interaction = Encoder::new();
        interaction
            .array(7)
            .uint(2)
            .bytes(&[0x32; 16])
            .bytes(&[0x42; 16])
            .bytes(&channel)
            .uint(2)
            .uint(0)
            .bytes(&[0x88; 32]);
        write_request(&mut client, [7; 16], 6, interaction.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 1);

        // A discard retry remains a valid typed-finalizer retry after it has removed the local
        // group row. The finalizer's native intent, not the current group list, is authoritative.
        let mut second_create = Encoder::new();
        second_create
            .array(7)
            .uint(2)
            .bytes(&discard_operation)
            .bytes(&discarded_group)
            .bytes(&[0x52; 16])
            .uint(0)
            .uint(0)
            .bytes(&[0x89; 32]);
        write_request(&mut client, [16; 16], 6, second_create.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut discard_payload = Encoder::new();
        discard_payload
            .array(3)
            .uint(4)
            .bytes(&discard_operation)
            .bytes(&discarded_group);
        write_request(&mut client, [17; 16], 6, discard_payload.clone().into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        write_request(&mut client, [18; 16], 6, discard_payload.clone().into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);

        let mut shutdown = Encoder::new();
        shutdown.array(0);
        write_request(&mut client, [8; 16], 15, shutdown.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
    }
    {
        let (mut client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);
        write_request(&mut client, [9; 16], 1, open_payload(ids, key));
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut discard_payload = Encoder::new();
        discard_payload
            .array(3)
            .uint(4)
            .bytes(&discard_operation)
            .bytes(&discarded_group);
        write_request(&mut client, [19; 16], 6, discard_payload.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut result = Encoder::new();
        result.array(2).uint(20).bytes(&operation);
        write_request(&mut client, [10; 16], 6, result.into_vec());
        let recovered = read_response(&mut client, &mut child);
        assert_eq!(recovered.status, 0);
        let stored = decode_strict(&recovered.payload, |d| {
            d.array(1)?;
            let intent = d.bytes()?;
            decode_strict(intent, |i| {
                i.array(4)?;
                let _kind = i.uint()?;
                let _group = i.bytes_exact::<16>()?;
                let state = i.uint()?;
                let bytes = i.opt_bytes()?.map(ToOwned::to_owned);
                Ok((state, bytes))
            })
        })
        .expect("native intent result");
        assert_eq!(stored.0, 0);
        let original_body = decode_strict(&original, |d| {
            d.array(1)?;
            Ok(d.bytes()?.to_vec())
        })
        .expect("wrapped create result");
        assert_eq!(stored.1.as_deref(), Some(original_body.as_slice()));

        // Registration resolves the original native intent, and the server cursor ack uses its
        // own operation and range checks, separate from journal delivery acknowledgements.
        let mut registered = Encoder::new();
        registered
            .array(4)
            .uint(3)
            .bytes(&operation)
            .bytes(&group)
            .uint(1);
        write_request(&mut client, [13; 16], 6, registered.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut registered_retry = Encoder::new();
        registered_retry
            .array(4)
            .uint(3)
            .bytes(&operation)
            .bytes(&group)
            .uint(1);
        write_request(&mut client, [20; 16], 6, registered_retry.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);

        let mut cursor_ack = Encoder::new();
        cursor_ack.array(3).bytes(&group).uint(0).uint(0);
        write_request(&mut client, [14; 16], 13, cursor_ack.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);

        let mut future_event_ack = Encoder::new();
        future_event_ack.array(2).uint(19).uint(1);
        write_request(&mut client, [15; 16], 6, future_event_ack.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 1);

        let mut list = Encoder::new();
        list.array(1).uint(0);
        write_request(&mut client, [11; 16], 6, list.into_vec());
        let listed = read_response(&mut client, &mut child);
        assert_eq!(listed.status, 0);
        let count = decode_strict(&listed.payload, |d| {
            d.array(1)?;
            let rows = d.bytes()?;
            decode_strict(rows, |r| {
                let count = r.array_len()?;
                for _ in 0..count {
                    r.skip()?;
                }
                Ok(count)
            })
        })
        .expect("owned groups only");
        assert_eq!(count, 1);

        // Staged op16 executes the same typed op6 adapter and returns its exact encoded result.
        use sha2::{Digest, Sha256};
        let staged_operation = [0x35; 16];
        let staged_group = [0x45; 16];
        let mut staged = Encoder::new();
        staged
            .array(7)
            .uint(2)
            .bytes(&staged_operation)
            .bytes(&staged_group)
            .bytes(&[0x55; 16])
            .uint(0)
            .uint(0)
            .bytes(&[0x88; 32]);
        let staged = staged.into_vec();
        let digest: [u8; 32] = Sha256::digest(&staged).into();
        let mut begin = Encoder::new();
        begin
            .array(4)
            .uint(0)
            .uint(6)
            .uint(staged.len() as u64)
            .bytes(&digest);
        write_request(&mut client, [16; 16], 16, begin.into_vec());
        let started = read_response(&mut client, &mut child);
        assert_eq!(started.status, 0);
        let transfer_id = decode_strict(&started.payload, |d| {
            d.array(1)?;
            d.bytes_exact::<16>()
        })
        .unwrap();
        let mut chunk = Encoder::new();
        chunk
            .array(4)
            .uint(1)
            .bytes(&transfer_id)
            .uint(0)
            .bytes(&staged);
        write_request(&mut client, [17; 16], 16, chunk.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut execute = Encoder::new();
        execute.array(2).uint(2).bytes(&transfer_id);
        write_request(&mut client, [18; 16], 16, execute.into_vec());
        let descriptor = read_response(&mut client, &mut child);
        assert_eq!(descriptor.status, 0);
        let (target_status, result_len, result_digest) = decode_strict(&descriptor.payload, |d| {
            d.array(3)?;
            Ok((d.uint()?, d.uint()?, d.bytes_exact::<32>()?))
        })
        .unwrap();
        assert_eq!(target_status, 0);
        let mut read = Encoder::new();
        read.array(4)
            .uint(3)
            .bytes(&transfer_id)
            .uint(0)
            .uint(result_len);
        write_request(&mut client, [19; 16], 16, read.into_vec());
        let staged_result = read_response(&mut client, &mut child);
        let staged_result = decode_strict(&staged_result.payload, |d| {
            d.array(1)?;
            Ok(d.bytes()?.to_vec())
        })
        .unwrap();
        assert_eq!(Sha256::digest(&staged_result).as_slice(), result_digest);

        let mut shutdown = Encoder::new();
        shutdown.array(0);
        write_request(&mut client, [12; 16], 15, shutdown.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
    }
    let _ = fs::remove_dir_all(path);
}

#[test]
fn helper_accepts_only_expected_call_welcome_binding_and_kind() {
    use dilla_core::mls::{DillaBinding, GroupKind};

    let path = temp_dir();
    let ids = (
        client_support::INSTANCE,
        client_support::COMMUNITY,
        [0x23; 16],
    );
    let key = [0x6a; 32];
    let call_group = [0x4a; 16];
    let text_group = [0x4c; 16];
    let device;
    let welcome_body;
    let call_welcome_body;
    let text_welcome_body;
    let call_info_body;
    let call_tree_body;

    // Enroll the helper Bot, then mint its real private KeyPackage in the same encrypted store.
    {
        let (mut client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);
        write_request(&mut client, [1; 16], 1, open_payload(ids, key));
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut bootstrap = Encoder::new();
        bootstrap
            .array(4)
            .bytes(&ids.0)
            .bytes(&ids.1)
            .bytes(&ids.2)
            .bytes(&[0x77; 32]);
        write_request(&mut client, [2; 16], 2, bootstrap.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut complete = Encoder::new();
        complete
            .array(3)
            .bytes(&[0x24; 16])
            .text("welcome-call-bot")
            .uint(100);
        write_request(&mut client, [3; 16], 3, complete.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut shutdown = Encoder::new();
        shutdown.array(0);
        write_request(&mut client, [4; 16], 15, shutdown.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
    }
    {
        let connection = std::sync::Arc::new(std::sync::Mutex::new(test_cipher_conn(
            &path.join("state.db"),
            &key,
        )));
        let native = dilla_core::client::ClientCore::open(connection.clone()).unwrap();
        device = decode_strict(&native.identity().unwrap(), |d| {
            d.array(6)?;
            let _phase = d.uint()?;
            d.opt_bytes_exact::<16>()?;
            d.opt_bytes_exact::<16>()?;
            let device = d.opt_bytes_exact::<16>()?;
            d.text()?;
            d.uint()?;
            Ok(device)
        })
        .unwrap()
        .unwrap();
        let mut bot = client_support::Core {
            core: native,
            probe: connection,
            user: [0x24; 16],
            device,
        };
        let instance = client_support::Instance::generate();
        let mut relay_call = client_support::Relay::new(call_group);
        let mut relay_text = client_support::Relay::new(text_group);
        let call_binding = DillaBinding {
            v: 1,
            instance_id: dilla_core::ids::InstanceId::from_bytes(client_support::INSTANCE),
            community_id: Some(dilla_core::ids::CommunityId::from_bytes(
                client_support::COMMUNITY,
            )),
            target_id: client_support::CHANNEL,
            kind: GroupKind::Call,
            policy_version: client_support::POLICY,
            e2ee_version: 1,
            media_version: GroupKind::Call.media_version(),
        };
        let mut call_peer = client_support::RawPeer::new(0x5a, 0x5b);
        call_peer.create_as(&mut relay_call, &instance, &call_binding);
        let key_packages = decode_strict(&bot.core.key_packages(2, false).unwrap(), |d| {
            d.array(2)?;
            let n = d.array_len()?;
            let mut out = Vec::with_capacity(n);
            for _ in 0..n {
                out.push(d.bytes()?.to_vec());
            }
            d.null()?;
            Ok(out)
        })
        .unwrap();
        assert_eq!(key_packages.len(), 2);
        call_peer.add(&mut relay_call, &[key_packages[0].as_slice()]);
        call_info_body = relay_call.info_body();
        call_tree_body = relay_call.tree_body();

        let text_binding = client_support::text_binding(client_support::CHANNEL);
        let mut text_peer = client_support::RawPeer::new(0x5c, 0x5d);
        text_peer.create_as(&mut relay_text, &instance, &text_binding);
        text_peer.add(&mut relay_text, &[key_packages[1].as_slice()]);

        let mut items = Vec::new();
        let call_welcome = relay_call.welcomes_body(device);
        call_welcome_body = call_welcome.clone();
        let text_welcome = relay_text.welcomes_body(device);
        text_welcome_body = text_welcome.clone();
        for bytes in [call_welcome, text_welcome] {
            let mut decoded = decode_strict(&bytes, |d| {
                let n = d.array_len()?;
                let mut out = Vec::with_capacity(n);
                for _ in 0..n {
                    d.array(7)?;
                    out.push((
                        d.uint()?,
                        d.bytes_exact::<16>()?,
                        d.uint()?,
                        d.uint()?,
                        d.bytes()?.to_vec(),
                        d.bytes()?.to_vec(),
                        d.bytes_exact::<32>()?,
                    ));
                }
                Ok(out)
            })
            .unwrap();
            items.append(&mut decoded);
        }
        let mut mixed = Encoder::new();
        mixed.array(items.len());
        for item in items {
            mixed
                .array(7)
                .uint(item.0)
                .bytes(&item.1)
                .uint(item.2)
                .uint(item.3)
                .bytes(&item.4)
                .bytes(&item.5)
                .bytes(&item.6);
        }
        welcome_body = mixed.into_vec();
    }

    let expected = |call_community: [u8; 16], call_kind: u64| {
        let mut e = Encoder::new();
        // Deliberately order expectations differently from Welcome items. The adapter maps by
        // group id and preserves the Welcome input order in its outcome body.
        e.array(2)
            .array(5)
            .bytes(&text_group)
            .bytes(&client_support::COMMUNITY)
            .bytes(&client_support::CHANNEL)
            .uint(0)
            .uint(client_support::POLICY)
            .array(5)
            .bytes(&call_group)
            .bytes(&call_community)
            .bytes(&client_support::CHANNEL)
            .uint(call_kind)
            .uint(client_support::POLICY);
        e.into_vec()
    };
    let welcome_request = |want: Vec<u8>| {
        let mut e = Encoder::new();
        e.array(3).uint(7).bytes(&welcome_body).bytes(&want);
        e.into_vec()
    };
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    write_request(&mut client, [5; 16], 1, open_payload(ids, key));
    assert_eq!(read_response(&mut client, &mut child).status, 0);

    // A caller cannot substitute another community on either member of the mixed batch.
    write_request(
        &mut client,
        [6; 16],
        6,
        welcome_request(expected([0x99; 16], 1)),
    );
    assert_eq!(read_response(&mut client, &mut child).status, 2);
    write_request(
        &mut client,
        [7; 16],
        6,
        welcome_request(expected(client_support::COMMUNITY, 0)),
    );
    let wrong_kind = read_response(&mut client, &mut child);
    assert_eq!(wrong_kind.status, 0);
    assert_eq!(wrong_kind.status, 0);
    let wrong_kind_outcomes = decode_strict(&wrong_kind.payload, |d| {
        d.array(1)?;
        let body = d.bytes()?;
        decode_strict(body, |r| {
            let n = r.array_len()?;
            let mut out = Vec::with_capacity(n);
            for _ in 0..n {
                r.array(4)?;
                r.uint()?;
                r.bytes_exact::<16>()?;
                out.push((r.uint()?, r.text()?.to_owned()));
            }
            Ok(out)
        })
    })
    .unwrap();
    assert_eq!(
        wrong_kind_outcomes,
        vec![(2, "E_BINDING".to_owned()), (0, "".to_owned())]
    );

    let call_welcome = decode_strict(&call_welcome_body, |d| {
        let n = d.array_len()?;
        let mut out = Vec::with_capacity(n);
        for _ in 0..n {
            d.array(7)?;
            out.push((
                d.uint()?,
                d.bytes_exact::<16>()?,
                d.uint()?,
                d.uint()?,
                d.bytes()?.to_vec(),
                d.bytes()?.to_vec(),
                d.bytes_exact::<32>()?,
            ));
        }
        Ok(out)
    })
    .unwrap();
    let mut duplicate_call_body = Encoder::new();
    duplicate_call_body.array(2);
    for (index, item) in call_welcome.iter().cycle().take(2).enumerate() {
        duplicate_call_body
            .array(7)
            // Receipt matching is content-based and does not depend on delivery identity.
            .uint(item.0 + index as u64)
            .bytes(&item.1)
            .uint(item.2)
            .uint(item.3)
            .bytes(&item.4)
            .bytes(&item.5)
            .bytes(&item.6);
    }
    let duplicate_call_body = duplicate_call_body.into_vec();
    let call_expected = || {
        let mut e = Encoder::new();
        e.array(1)
            .array(5)
            .bytes(&call_group)
            .bytes(&client_support::COMMUNITY)
            .bytes(&client_support::CHANNEL)
            .uint(1)
            .uint(client_support::POLICY);
        e.into_vec()
    };
    let mut extra_expected = Encoder::new();
    extra_expected
        .array(3)
        .uint(7)
        .bytes(&duplicate_call_body)
        .bytes(&expected(client_support::COMMUNITY, 1));
    write_request(&mut client, [26; 16], 6, extra_expected.into_vec());
    assert_eq!(
        read_response(&mut client, &mut child).status,
        1,
        "duplicate Call Welcome rows cannot make an unused Text expectation valid"
    );

    let mut duplicate_call = Encoder::new();
    duplicate_call
        .array(3)
        .uint(7)
        .bytes(&duplicate_call_body)
        .bytes(&call_expected());
    write_request(&mut client, [27; 16], 6, duplicate_call.into_vec());
    let duplicate_call_result = read_response(&mut client, &mut child);
    assert_eq!(duplicate_call_result.status, 0);
    assert_eq!(
        decode_strict(&duplicate_call_result.payload, |d| {
            d.array(1)?;
            let body = d.bytes()?;
            decode_strict(body, |r| {
                let n = r.array_len()?;
                let mut out = Vec::with_capacity(n);
                for _ in 0..n {
                    r.array(4)?;
                    r.uint()?;
                    r.bytes_exact::<16>()?;
                    out.push(r.uint()?);
                    r.text()?;
                }
                Ok(out)
            })
        })
        .unwrap(),
        vec![0, 1],
        "duplicate Welcome rows for one expected group are idempotent and preserve order"
    );

    write_request(
        &mut client,
        [8; 16],
        6,
        welcome_request(expected(client_support::COMMUNITY, 1)),
    );
    let joined = read_response(&mut client, &mut child);
    assert_eq!(joined.status, 0);
    assert_eq!(
        decode_strict(&joined.payload, |d| {
            d.array(1)?;
            let body = d.bytes()?;
            decode_strict(body, |r| {
                let n = r.array_len()?;
                let mut out = Vec::with_capacity(n);
                for _ in 0..n {
                    r.array(4)?;
                    r.uint()?;
                    r.bytes_exact::<16>()?;
                    out.push(r.uint()?);
                    r.text()?;
                }
                Ok(out)
            })
        })
        .unwrap(),
        vec![1, 1]
    );

    type WelcomeItem = (u64, [u8; 16], u64, u64, Vec<u8>, Vec<u8>, [u8; 32]);
    let encode_items = |items: &[WelcomeItem]| {
        let mut e = Encoder::new();
        e.array(items.len());
        for item in items {
            e.array(7)
                .uint(item.0)
                .bytes(&item.1)
                .uint(item.2)
                .uint(item.3)
                .bytes(&item.4)
                .bytes(&item.5)
                .bytes(&item.6);
        }
        e.into_vec()
    };
    let decode_one_outcome = |payload: &[u8]| {
        decode_strict(payload, |d| {
            d.array(1)?;
            let body = d.bytes()?;
            decode_strict(body, |r| {
                r.array(1)?;
                r.array(4)?;
                r.uint()?;
                r.bytes_exact::<16>()?;
                Ok((r.uint()?, r.text()?.to_owned()))
            })
        })
        .unwrap()
    };
    let mut malformed = call_welcome.clone();
    malformed[0].4 = vec![0x01];
    for (changed, description) in [
        (encode_items(&malformed), "malformed active Welcome"),
        (
            {
                let mut changed = call_welcome.clone();
                changed[0].5[0] ^= 1;
                encode_items(&changed)
            },
            "changed active ratchet tree",
        ),
        (
            {
                let mut changed = call_welcome.clone();
                changed[0].2 += 1;
                encode_items(&changed)
            },
            "changed active Welcome epoch label",
        ),
    ] {
        let mut request = Encoder::new();
        request
            .array(3)
            .uint(7)
            .bytes(&changed)
            .bytes(&call_expected());
        write_request(&mut client, [0x31; 16], 6, request.into_vec());
        let rejected = read_response(&mut client, &mut child);
        assert_eq!(rejected.status, 0, "{description} is a per-item refusal");
        assert_eq!(
            decode_one_outcome(&rejected.payload),
            (2, "E_BINDING".to_owned()),
            "{description}"
        );
    }

    // A real Welcome for the Text group cannot be presented as an already-processed Call Welcome.
    let mut foreign = decode_strict(&text_welcome_body, |d| {
        let n = d.array_len()?;
        let mut out = Vec::with_capacity(n);
        for _ in 0..n {
            d.array(7)?;
            out.push((
                d.uint()?,
                d.bytes_exact::<16>()?,
                d.uint()?,
                d.uint()?,
                d.bytes()?.to_vec(),
                d.bytes()?.to_vec(),
                d.bytes_exact::<32>()?,
            ));
        }
        Ok(out)
    })
    .unwrap();
    foreign[0].1 = call_group;
    let mut foreign_request = Encoder::new();
    foreign_request
        .array(3)
        .uint(7)
        .bytes(&encode_items(&foreign))
        .bytes(&call_expected());
    write_request(&mut client, [0x32; 16], 6, foreign_request.into_vec());
    let foreign_result = read_response(&mut client, &mut child);
    assert_eq!(foreign_result.status, 0);
    assert_eq!(
        decode_one_outcome(&foreign_result.payload),
        (2, "E_BINDING".to_owned()),
        "a valid Welcome for a different group must not receive the duplicate ACK"
    );

    // An externally-created active group has no receipt and cannot use the duplicate shortcut.
    let external_group = [0x53; 16];
    let external_channel = [0x54; 16];
    let mut create = Encoder::new();
    create
        .array(7)
        .uint(2)
        .bytes(&[0xa2; 16])
        .bytes(&external_group)
        .bytes(&external_channel)
        .uint(0)
        .uint(1)
        .bytes(&[0x88; 32]);
    write_request(&mut client, [0x33; 16], 6, create.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let mut registered = Encoder::new();
    registered
        .array(4)
        .uint(3)
        .bytes(&[0xa2; 16])
        .bytes(&external_group)
        .uint(1);
    write_request(&mut client, [0x34; 16], 6, registered.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let mut unreceipted = call_welcome.clone();
    unreceipted[0].1 = external_group;
    let mut external_expected = Encoder::new();
    external_expected
        .array(1)
        .array(5)
        .bytes(&external_group)
        .bytes(&client_support::COMMUNITY)
        .bytes(&external_channel)
        .uint(0)
        .uint(1);
    let mut external_request = Encoder::new();
    external_request
        .array(3)
        .uint(7)
        .bytes(&encode_items(&unreceipted))
        .bytes(&external_expected.into_vec());
    write_request(&mut client, [0x35; 16], 6, external_request.into_vec());
    let unreceipted_result = read_response(&mut client, &mut child);
    assert_eq!(unreceipted_result.status, 0);
    assert_eq!(
        decode_one_outcome(&unreceipted_result.payload),
        (2, "E_BINDING".to_owned()),
        "active groups created outside Welcome admission have no duplicate receipt"
    );

    let mut shutdown = Encoder::new();
    shutdown.array(0);
    write_request(&mut client, [0x38; 16], 15, shutdown.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    assert!(child.wait().unwrap().success());

    // The exact validated crypto input remains idempotent after the helper process and store
    // connection are reopened; the receipt, rather than an in-memory flag, proves prior success.
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    write_request(&mut client, [0x36; 16], 1, open_payload(ids, key));
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let mut reopened_retry = Encoder::new();
    reopened_retry
        .array(3)
        .uint(7)
        .bytes(&call_welcome_body)
        .bytes(&call_expected());
    write_request(&mut client, [0x37; 16], 6, reopened_retry.into_vec());
    let reopened = read_response(&mut client, &mut child);
    assert_eq!(reopened.status, 0);
    assert_eq!(decode_one_outcome(&reopened.payload), (1, "".to_owned()));

    // A later externally-joined MLS state replaces the original Welcome-admitted state and
    // clears its receipt atomically. Retrying that old Welcome after the replacement is active
    // must not be acknowledged from stale app_meta evidence.
    let mut external_join = Encoder::new();
    external_join
        .array(8)
        .uint(5)
        .bytes(&[0xa3; 16])
        .bytes(&call_group)
        .bytes(&client_support::CHANNEL)
        .uint(1)
        .uint(client_support::POLICY)
        .bytes(&call_info_body)
        .bytes(&call_tree_body);
    write_request(&mut client, [0x39; 16], 6, external_join.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let mut external_joined = Encoder::new();
    external_joined
        .array(4)
        .uint(6)
        .bytes(&[0xa3; 16])
        .bytes(&call_group)
        .uint(1);
    write_request(&mut client, [0x3a; 16], 6, external_joined.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let mut replaced_retry = Encoder::new();
    replaced_retry
        .array(3)
        .uint(7)
        .bytes(&call_welcome_body)
        .bytes(&call_expected());
    write_request(&mut client, [0x3b; 16], 6, replaced_retry.into_vec());
    let after_replacement = read_response(&mut client, &mut child);
    assert_eq!(after_replacement.status, 0);
    assert_eq!(
        decode_one_outcome(&after_replacement.payload),
        (2, "E_BINDING".to_owned()),
        "the old Welcome receipt is invalidated by external join replacement"
    );

    let mut inspect = Encoder::new();
    inspect.array(2).uint(1).bytes(&call_group);
    write_request(&mut client, [9; 16], 6, inspect.into_vec());
    let row = read_response(&mut client, &mut child);
    assert_eq!(row.status, 0);
    assert_eq!(
        decode_strict(&row.payload, |d| {
            d.array(1)?;
            let encoded = d.bytes()?;
            decode_strict(encoded, |r| {
                r.array(9)?;
                r.bytes_exact::<16>()?;
                let kind = r.uint()?;
                r.skip()?;
                for _ in 0..6 {
                    r.skip()?;
                }
                Ok(kind)
            })
        })
        .unwrap(),
        1
    );

    let mut inspect_text = Encoder::new();
    inspect_text.array(2).uint(1).bytes(&text_group);
    write_request(&mut client, [10; 16], 6, inspect_text.into_vec());
    let text_row = read_response(&mut client, &mut child);
    assert_eq!(text_row.status, 0);
    assert_eq!(
        decode_strict(&text_row.payload, |d| {
            d.array(1)?;
            let encoded = d.bytes()?;
            decode_strict(encoded, |r| {
                r.array(9)?;
                r.bytes_exact::<16>()?;
                let kind = r.uint()?;
                r.opt_bytes_exact::<16>()?;
                r.bytes_exact::<16>()?;
                r.uint()?;
                r.uint()?;
                r.uint()?;
                r.uint()?;
                r.uint()?;
                Ok(kind)
            })
        })
        .unwrap(),
        0
    );

    let mut shutdown = Encoder::new();
    shutdown.array(0);
    write_request(&mut client, [0x3c; 16], 15, shutdown.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    assert!(child.wait().unwrap().success());
    let _ = fs::remove_dir_all(path);
}

#[test]
fn helper_applies_real_text_message_and_acknowledges_authenticated_journal_after_reopen() {
    use client_support::{
        Instance, RawPeer, Relay, encode_handshakes, encode_messages, sec_entry, sec_next,
        text_binding,
    };
    use dilla_core::identity::DeviceList;

    let path = temp_dir();
    let ids = (
        client_support::INSTANCE,
        client_support::COMMUNITY,
        [0x23; 16],
    );
    let key = [0x6b; 32];
    let group = [0x4b; 16];
    let peer_user = [0x71; 16];
    let peer_device = [0x72; 16];
    let instance = Instance::generate();
    let mut relay = Relay::new(group);
    let mut peer = RawPeer::new(peer_user[0], peer_device[0]);
    peer.create_as(
        &mut relay,
        &instance,
        &text_binding(client_support::CHANNEL),
    );
    // This is a second, independently signed current MLS device for the same user. Add it before
    // the helper joins so later device-list transitions do not skip relay handshakes.
    let second_peer = RawPeer::new_with_keys(
        peer_user[0],
        peer_device[0].wrapping_add(1),
        peer_user[0],
        peer_user[0].wrapping_add(0x40),
    );
    let second_package = second_peer.key_package();
    peer.add(&mut relay, &[second_package.as_slice()]);
    let second_leaf = relay.leaf_of(second_peer.device);

    // Bootstrap the helper Bot in its encrypted database, then use that same Bot identity when
    // it joins the real peer-created MLS group below.
    {
        let (mut client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);
        write_request(&mut client, [1; 16], 1, open_payload(ids, key));
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut bootstrap = Encoder::new();
        bootstrap
            .array(4)
            .bytes(&ids.0)
            .bytes(&ids.1)
            .bytes(&ids.2)
            .bytes(&[0x77; 32]);
        write_request(&mut client, [2; 16], 2, bootstrap.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut complete = Encoder::new();
        complete
            .array(3)
            .bytes(&[0x24; 16])
            .text("journal-bot")
            .uint(100);
        write_request(&mut client, [3; 16], 3, complete.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut shutdown = Encoder::new();
        shutdown.array(0);
        write_request(&mut client, [4; 16], 15, shutdown.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
    }

    let open = || {
        let (client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);
        let mut client = client;
        write_request(&mut client, [5; 16], 1, open_payload(ids, key));
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        (client, child)
    };
    let group_join_operation = [0x5c; 16];
    let delivery_id;
    {
        let (mut client, mut child) = open();
        let mut join = Encoder::new();
        join.array(8)
            .uint(5)
            .bytes(&group_join_operation)
            .bytes(&group)
            .bytes(&client_support::CHANNEL)
            .uint(0)
            .uint(client_support::POLICY)
            .bytes(&relay.info_body())
            .bytes(&relay.tree_body());
        write_request(&mut client, [6; 16], 6, join.into_vec());
        let external_commit = read_response(&mut client, &mut child);
        assert_eq!(external_commit.status, 0);
        let upload = decode_strict(&external_commit.payload, |d| {
            d.array(1)?;
            Ok(d.bytes()?.to_vec())
        })
        .unwrap();
        let accepted = relay
            .resync(&upload)
            .expect("accept real helper external commit");
        let next_sequence = decode_strict(&accepted, |d| {
            d.array(2)?;
            let seq = d.uint()?;
            d.uint()?;
            Ok(seq)
        })
        .unwrap();
        let mut joined = Encoder::new();
        joined
            .array(4)
            .uint(6)
            .bytes(&group_join_operation)
            .bytes(&group)
            .uint(next_sequence);
        write_request(&mut client, [7; 16], 6, joined.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut joined_retry = Encoder::new();
        joined_retry
            .array(4)
            .uint(6)
            .bytes(&group_join_operation)
            .bytes(&group)
            .uint(next_sequence);
        write_request(&mut client, [23; 16], 6, joined_retry.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        peer.apply_commit(&relay, next_sequence);

        // Signed current device-list evidence is the only route from pending sender to journal.
        let signed_list = peer.device_list_body();
        let (peer_version, peer_list, peer_signature, peer_previous) =
            decode_strict(&signed_list, |d| {
                d.array(4)?;
                Ok((
                    d.uint()?,
                    d.bytes()?.to_vec(),
                    d.bytes()?.to_vec(),
                    d.bytes()?.to_vec(),
                ))
            })
            .unwrap();
        let mut history = Encoder::new();
        history
            .array(1)
            .array(4)
            .uint(peer_version)
            .bytes(&peer_list)
            .bytes(&peer_signature)
            .bytes(&peer_previous);
        let mut trust = Encoder::new();
        trust
            .array(7)
            .uint(14)
            .bytes(&group)
            .uint(relay.leaf_of(peer_device) as u64)
            .bytes(&peer_user)
            .bytes(&history.into_vec())
            .bytes(&peer_list)
            .uint(client_support::NOW);
        write_request(&mut client, [8; 16], 6, trust.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);

        let second_list = sec_next(
            &DeviceList::decode(&peer_list).unwrap(),
            vec![sec_entry(&peer), sec_entry(&second_peer)],
            peer_user[0].wrapping_add(0x40),
        );
        let mut two_device_history = Encoder::new();
        two_device_history
            .array(2)
            .array(4)
            .uint(peer_version)
            .bytes(&peer_list)
            .bytes(&peer_signature)
            .bytes(&peer_previous)
            .array(4)
            .uint(2)
            .bytes(&second_list.encode())
            .bytes(&second_list.sig_ssk)
            .bytes(&second_list.unsigned.prev_hash);
        let mut trust_two_devices = Encoder::new();
        trust_two_devices
            .array(7)
            .uint(14)
            .bytes(&group)
            .uint(second_leaf as u64)
            .bytes(&peer_user)
            .bytes(&two_device_history.into_vec())
            .bytes(&second_list.encode())
            .uint(client_support::NOW + 2);
        write_request(&mut client, [25; 16], 6, trust_two_devices.into_vec());
        let two_device_trust = read_response(&mut client, &mut child);
        assert_eq!(two_device_trust.status, 0, "{:?}", two_device_trust.payload);

        // Exercise the adapter's actual Add proposal parser and trust gate. A duplicate proposal
        // for the same signed device may be listed, but cannot be trusted twice as a new Add.
        let mut candidate = large_fixture_candidate(777);
        let package = candidate.first_key_package();
        instance.propose_add(&mut relay, &package);
        let proposals = relay.proposals_body();
        let mut proposed = Encoder::new();
        proposed.array(3).uint(15).bytes(&group).bytes(&proposals);
        write_request(&mut client, [19; 16], 6, proposed.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let signed = candidate
            .core
            .device_list_body()
            .expect("signed candidate list");
        let (version, list, signature, previous) = decode_strict(&signed, |d| {
            d.array(4)?;
            Ok((
                d.uint()?,
                d.bytes()?.to_vec(),
                d.bytes()?.to_vec(),
                d.bytes()?.to_vec(),
            ))
        })
        .unwrap();
        let mut history = Encoder::new();
        history
            .array(1)
            .array(4)
            .uint(version)
            .bytes(&list)
            .bytes(&signature)
            .bytes(&previous);
        let history = history.into_vec();
        let mut add_trust = Encoder::new();
        add_trust
            .array(8)
            .uint(16)
            .bytes(&group)
            .bytes(&proposals)
            .uint(0)
            .bytes(&candidate.user)
            .bytes(&history)
            .bytes(&list)
            .uint(client_support::NOW);
        write_request(&mut client, [20; 16], 6, add_trust.into_vec());
        let candidate_trust_result = read_response(&mut client, &mut child);
        assert_eq!(
            candidate_trust_result.status, 0,
            "{:?}",
            candidate_trust_result.payload
        );

        instance.propose_add(&mut relay, &package);
        let duplicate_proposals = relay.proposals_body();
        let mut duplicate_list = Encoder::new();
        duplicate_list
            .array(3)
            .uint(15)
            .bytes(&group)
            .bytes(&duplicate_proposals);
        write_request(&mut client, [21; 16], 6, duplicate_list.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut duplicate_add_trust = Encoder::new();
        duplicate_add_trust
            .array(8)
            .uint(16)
            .bytes(&group)
            .bytes(&duplicate_proposals)
            .uint(1)
            .bytes(&candidate.user)
            .bytes(&history)
            .bytes(&list)
            .uint(client_support::NOW);
        write_request(&mut client, [22; 16], 6, duplicate_add_trust.into_vec());
        let duplicate_result = read_response(&mut client, &mut child);
        assert_eq!(duplicate_result.status, 0);
        assert_eq!(
            decode_strict(&duplicate_result.payload, |d| {
                d.array(1)?;
                let body = d.bytes()?;
                decode_strict(body, |r| {
                    r.array(3)?;
                    Ok((r.uint()?, r.uint()?, r.uint()?))
                })
            })
            .unwrap(),
            (1, 0, 0),
            "the repeated signed Add is an unchanged native trust outcome"
        );

        // A server-provided uploader spoof cannot mint an event; the authentic next message can.
        peer.send(&mut relay, "spoof rejected");
        let forged = relay.messages.last().unwrap().clone();
        let mut forged = forged;
        forged.uploader = [0x99; 16];
        let forged_rows = encode_messages(&[&forged]);
        let mut apply_forged = Encoder::new();
        apply_forged
            .array(5)
            .uint(8)
            .bytes(&group)
            .bytes(&encode_handshakes(&[]))
            .bytes(&forged_rows)
            .uint(forged.seq);
        write_request(&mut client, [9; 16], 6, apply_forged.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);

        peer.send(&mut relay, "authenticated event");
        let authentic = relay.messages.last().unwrap().clone();
        let authentic_rows = encode_messages(&[&authentic]);
        let mut apply_authentic = Encoder::new();
        apply_authentic
            .array(5)
            .uint(8)
            .bytes(&group)
            .bytes(&encode_handshakes(&[]))
            .bytes(&authentic_rows)
            .uint(authentic.seq);
        write_request(&mut client, [10; 16], 6, apply_authentic.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);

        let mut read = Encoder::new();
        read.array(3).uint(18).uint(0).uint(10);
        write_request(&mut client, [11; 16], 6, read.into_vec());
        let page = read_response(&mut client, &mut child);
        assert_eq!(page.status, 0);
        let rows = decode_strict(&page.payload, |d| {
            d.array(1)?;
            let body = d.bytes()?;
            decode_strict(body, |r| {
                r.array(3)?;
                let n = r.array_len()?;
                let mut rows = Vec::with_capacity(n);
                for _ in 0..n {
                    r.array(17)?;
                    let id = r.uint()?;
                    for _ in 0..13 {
                        r.skip()?;
                    }
                    let text = r.text()?.to_owned();
                    r.skip()?;
                    r.skip()?;
                    rows.push((id, text));
                }
                r.uint()?;
                r.uint()?;
                Ok(rows)
            })
        })
        .unwrap();
        assert_eq!(rows.len(), 1);
        delivery_id = rows[0].0;
        assert_eq!(rows[0].1, "authenticated event");

        let mut revoked_entry = sec_entry(&peer);
        revoked_entry.revoked_at = Some(client_support::NOW + 5);
        let third = sec_next(
            &second_list,
            vec![revoked_entry, sec_entry(&second_peer)],
            peer_user[0].wrapping_add(0x40),
        );
        let mut revoked_history = Encoder::new();
        revoked_history
            .array(2)
            .array(4)
            .uint(2)
            .bytes(&second_list.encode())
            .bytes(&second_list.sig_ssk)
            .bytes(&second_list.unsigned.prev_hash)
            .array(4)
            .uint(3)
            .bytes(&third.encode())
            .bytes(&third.sig_ssk)
            .bytes(&third.unsigned.prev_hash);
        let revoked_history_bytes = revoked_history.into_vec();
        let mut revoke = Encoder::new();
        revoke
            .array(7)
            .uint(14)
            .bytes(&group)
            .uint(second_leaf as u64)
            .bytes(&peer_user)
            .bytes(&revoked_history_bytes)
            .bytes(&third.encode())
            .uint(client_support::NOW + 6);
        write_request(&mut client, [16; 16], 6, revoke.into_vec());
        let revoked_result = read_response(&mut client, &mut child);
        assert_eq!(
            revoked_result.status, 0,
            "the still-current second device can authenticate revocation of the first sender"
        );
        let mut reread = Encoder::new();
        reread.array(3).uint(18).uint(0).uint(10);
        write_request(&mut client, [17; 16], 6, reread.into_vec());
        let suppressed = read_response(&mut client, &mut child);
        assert_eq!(suppressed.status, 0);
        assert_eq!(
            decode_strict(&suppressed.payload, |d| {
                d.array(1)?;
                let body = d.bytes()?;
                decode_strict(body, |r| {
                    r.array(3)?;
                    let count = r.array_len()?;
                    for _ in 0..count {
                        r.skip()?;
                    }
                    r.uint()?;
                    r.uint()?;
                    Ok(count)
                })
            })
            .unwrap(),
            0,
            "revoking the sender suppresses its unacknowledged journal event"
        );

        let mut ack = Encoder::new();
        ack.array(2).uint(19).uint(delivery_id);
        write_request(&mut client, [12; 16], 6, ack.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut shutdown = Encoder::new();
        shutdown.array(0);
        write_request(&mut client, [13; 16], 15, shutdown.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
    }
    {
        let (mut client, mut child) = open();
        let mut read = Encoder::new();
        read.array(3).uint(18).uint(0).uint(10);
        write_request(&mut client, [14; 16], 6, read.into_vec());
        let page = read_response(&mut client, &mut child);
        assert_eq!(page.status, 0);
        assert_eq!(
            decode_strict(&page.payload, |d| {
                d.array(1)?;
                let body = d.bytes()?;
                decode_strict(body, |r| {
                    r.array(3)?;
                    let count = r.array_len()?;
                    for _ in 0..count {
                        r.skip()?;
                    }
                    r.uint()?;
                    r.uint()?;
                    Ok(count)
                })
            })
            .unwrap(),
            0
        );
        let mut shutdown = Encoder::new();
        shutdown.array(0);
        write_request(&mut client, [15; 16], 15, shutdown.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
    }
    let _ = fs::remove_dir_all(path);
    assert!(delivery_id > 0);
}

#[test]
fn helper_large_authenticated_commit_recovers_after_unobserved_execute_and_reads_in_chunks() {
    use sha2::{Digest, Sha256};
    use std::{thread, time::Duration};

    let path = temp_dir();
    let ids = ([0x71; 16], [0x72; 16], [0x73; 16]);
    let key = [0x74; 32];
    let group_id = [0x75; 16];
    let create_operation = [0x76; 16];
    let commit_operation = [0x77; 16];
    let proposals_body;

    // Enroll the real helper identity first, then shut it down so the MLS fixture can use the
    // same encrypted database without a competing process or connection.
    {
        let (mut client, server) = UnixStream::pair().unwrap();
        let mut child = spawn(&path, &server);
        drop(server);
        write_request(&mut client, [1; 16], 1, open_payload(ids, key));
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut bootstrap = Encoder::new();
        bootstrap
            .array(4)
            .bytes(&ids.0)
            .bytes(&ids.1)
            .bytes(&ids.2)
            .bytes(&[0x77; 32]);
        write_request(&mut client, [2; 16], 2, bootstrap.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut complete = Encoder::new();
        complete
            .array(3)
            .bytes(&[0x78; 16])
            .text("large-native-bot")
            .uint(100);
        write_request(&mut client, [3; 16], 3, complete.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        let mut shutdown = Encoder::new();
        shutdown.array(0);
        write_request(&mut client, [4; 16], 15, shutdown.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
        assert!(child.wait().unwrap().success());
    }

    // Build 256 real signed Add proposals with independently enrolled peers and current signed
    // device lists. Their Welcome bodies make the valid commit result cross many transfer chunks.
    {
        let connection = std::sync::Arc::new(std::sync::Mutex::new(test_cipher_conn(
            &path.join("state.db"),
            &key,
        )));
        let native = dilla_core::client::ClientCore::open(connection.clone()).unwrap();
        let device = decode_strict(&native.identity().unwrap(), |d| {
            d.array(6)?;
            let _phase = d.uint()?;
            d.opt_bytes_exact::<16>()?;
            d.opt_bytes_exact::<16>()?;
            let device = d.opt_bytes_exact::<16>()?;
            d.text()?;
            d.uint()?;
            Ok(device)
        })
        .unwrap()
        .unwrap();
        let mut bot = client_support::Core {
            core: native,
            probe: connection.clone(),
            user: [0x78; 16],
            device,
        };
        let instance = client_support::Instance::generate();
        let mut relay = client_support::Relay::new(group_id);
        let create = bot
            .core
            .bot_group_create(
                &create_operation,
                &group_id,
                &client_support::CHANNEL,
                0,
                1,
                &instance.public(),
            )
            .expect("create real text group");
        relay.register(&create).expect("register public group");
        bot.core
            .bot_group_registered(&create_operation, &group_id, 1)
            .expect("register native intent");

        let mut candidates = (0..256u32).map(large_fixture_candidate).collect::<Vec<_>>();
        let packages = candidates
            .iter_mut()
            .map(client_support::Core::first_key_package)
            .collect::<Vec<_>>();
        for package in &packages {
            instance.propose_add(&mut relay, package);
        }
        bot.sync(&relay);
        for (index, candidate) in candidates.iter().enumerate() {
            client_support::ingest_add_candidate_proposal(
                &mut bot, &relay, index, candidate, &group_id,
            );
        }
        proposals_body = relay.proposals_body();
        assert!(!proposals_body.is_empty());
        drop(candidates);
        drop(bot);
        drop(connection);
    }

    let mut execute_input = Encoder::new();
    execute_input
        .array(4)
        .uint(9)
        .bytes(&commit_operation)
        .bytes(&group_id)
        .bytes(&proposals_body);
    let execute_input = execute_input.into_vec();
    assert!(
        execute_input.len() < 1_048_576,
        "the MLS result is the large value"
    );

    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    write_request(&mut client, [5; 16], 1, open_payload(ids, key));
    assert_eq!(read_response(&mut client, &mut child).status, 0);

    // Begin and upload the valid op6/sub9 call, then execute without reading its descriptor.
    // A second encrypted connection observes the committed durable intent before we kill the
    // process, proving the lost reply happened after the MLS/storage unit committed.
    let digest: [u8; 32] = Sha256::digest(&execute_input).into();
    let mut begin = Encoder::new();
    begin
        .array(4)
        .uint(0)
        .uint(6)
        .uint(execute_input.len() as u64)
        .bytes(&digest);
    write_request(&mut client, [6; 16], 16, begin.into_vec());
    let started = read_response(&mut client, &mut child);
    assert_eq!(started.status, 0);
    let transfer_id = decode_strict(&started.payload, |d| {
        d.array(1)?;
        d.bytes_exact::<16>()
    })
    .unwrap();
    for (index, chunk) in execute_input.chunks(700_000).enumerate() {
        let mut append = Encoder::new();
        append
            .array(4)
            .uint(1)
            .bytes(&transfer_id)
            .uint((index * 700_000) as u64)
            .bytes(chunk);
        write_request(&mut client, [7 + index as u8; 16], 16, append.into_vec());
        assert_eq!(read_response(&mut client, &mut child).status, 0);
    }
    let mut execute = Encoder::new();
    execute.array(2).uint(2).bytes(&transfer_id);
    write_request(&mut client, [8; 16], 16, execute.into_vec());

    let probe = test_cipher_conn(&path.join("state.db"), &key);
    probe.busy_timeout(Duration::from_secs(2)).unwrap();
    let mut expected_result = None;
    for _ in 0..3600 {
        let found: Option<(i64, Vec<u8>)> = probe
            .query_row(
                "SELECT state,result FROM app_bot_intents WHERE operation_id=?1",
                [commit_operation.as_slice()],
                |r| Ok((r.get(0)?, r.get(1)?)),
            )
            .ok();
        if let Some((0, result)) = found.filter(|(_, result)| result.len() > 1_048_576) {
            expected_result = Some(result);
            break;
        }
        thread::sleep(Duration::from_millis(50));
    }
    let expected_result = expected_result.expect("multi-megabyte native intent committed");
    assert!(expected_result.len() > 1_048_576);
    child
        .kill()
        .expect("kill after commit, before reading execute reply");
    let _ = child.wait();
    drop(probe);
    drop(client);

    // The transfer cache died with the process. Query the native intent after restart through a
    // new staged request, then read the multi-megabyte response in bounded chunks.
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    write_request(&mut client, [9; 16], 1, open_payload(ids, key));
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let mut lookup = Encoder::new();
    lookup.array(2).uint(20).bytes(&commit_operation);
    let (transfer_id, (status, result_len, digest)) =
        stage_group_operation(&mut client, &mut child, 20, &lookup.into_vec());
    assert_eq!(status, 0);
    assert!(result_len > 1_048_576);
    let recovered = read_staged_result(&mut client, &mut child, 40, transfer_id, result_len);
    assert_eq!(Sha256::digest(&recovered).as_slice(), digest);
    let recovered_upload = decode_strict(&recovered, |d| {
        d.array(1)?;
        let intent = d.bytes()?;
        decode_strict(intent, |i| {
            i.array(4)?;
            let _kind = i.uint()?;
            let _group = i.bytes_exact::<16>()?;
            let state = i.uint()?;
            let result = i.opt_bytes()?.map(ToOwned::to_owned);
            Ok((state, result))
        })
    })
    .expect("recovered native intent tuple");
    assert_eq!(recovered_upload.0, 0);
    assert_eq!(
        recovered_upload.1.as_deref(),
        Some(expected_result.as_slice())
    );

    // Release the completed staging owner before issuing the native typed finalizer directly.
    let mut drop_transfer = Encoder::new();
    drop_transfer.array(2).uint(4).bytes(&transfer_id);
    write_request(&mut client, [239; 16], 16, drop_transfer.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);

    // The server rejected this built commit. Resolve it through the typed abort finalizer and
    // repeat the exact finalizer after restart, even though the staged result was already read.
    let mut abort = Encoder::new();
    abort
        .array(3)
        .uint(11)
        .bytes(&commit_operation)
        .bytes(&group_id);
    write_request(&mut client, [240; 16], 6, abort.clone().into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    write_request(&mut client, [241; 16], 6, abort.clone().into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);

    let mut shutdown = Encoder::new();
    shutdown.array(0);
    write_request(&mut client, [250; 16], 15, shutdown.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    assert!(child.wait().unwrap().success());

    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    write_request(&mut client, [242; 16], 1, open_payload(ids, key));
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    write_request(&mut client, [243; 16], 6, abort.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let mut shutdown = Encoder::new();
    shutdown.array(0);
    write_request(&mut client, [244; 16], 15, shutdown.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    assert!(child.wait().unwrap().success());
    let _ = fs::remove_dir_all(path);
}

#[test]
fn crash_with_pending_encrypted_wal_keeps_plaintext_reader_out() {
    let path = temp_dir();
    let (mut client, server) = UnixStream::pair().unwrap();
    let mut child = spawn(&path, &server);
    drop(server);
    let mut open = Encoder::new();
    open.array(4)
        .bytes(&[31; 16])
        .bytes(&[32; 16])
        .bytes(&[33; 16])
        .bytes(&[0x44; 32]);
    write_request(&mut client, [1; 16], 1, open.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    let mut changes = Encoder::new();
    changes
        .array(1)
        .array(4)
        .uint(0)
        .bytes(b"settings")
        .uint(0)
        .bytes(&[0x81, 0x07]);
    let mut tx = Encoder::new();
    tx.array(2).bytes(&[34; 16]).raw(&changes.into_vec());
    write_request(&mut client, [2; 16], 12, tx.into_vec());
    assert_eq!(read_response(&mut client, &mut child).status, 0);
    child.kill().unwrap();
    let _ = child.wait();
    drop(client);
    let plain = rusqlite::Connection::open(path.join("state.db")).unwrap();
    assert!(
        plain
            .query_row("SELECT count(*) FROM sqlite_schema", [], |r| r
                .get::<_, i64>(0))
            .is_err()
    );
    drop(plain);

    let (mut client, server) = UnixStream::pair().unwrap();
    let mut recovered = spawn(&path, &server);
    drop(server);
    let mut open = Encoder::new();
    open.array(4)
        .bytes(&[31; 16])
        .bytes(&[32; 16])
        .bytes(&[33; 16])
        .bytes(&[0x44; 32]);
    write_request(&mut client, [3; 16], 1, open.into_vec());
    assert_eq!(read_response(&mut client, &mut recovered).status, 0);
    let mut get = Encoder::new();
    get.array(3).uint(0).uint(0).bytes(b"settings");
    write_request(&mut client, [4; 16], 11, get.into_vec());
    let saved = read_response(&mut client, &mut recovered);
    assert_eq!(saved.status, 0);
    let mut expected = Encoder::new();
    expected.array(2).uint(1).bytes(&[0x81, 0x07]);
    assert_eq!(saved.payload, expected.into_vec());
    let mut shutdown = Encoder::new();
    shutdown.array(0);
    write_request(&mut client, [5; 16], 15, shutdown.into_vec());
    assert_eq!(read_response(&mut client, &mut recovered).status, 0);
    assert!(recovered.wait().unwrap().success());
    let _ = fs::remove_dir_all(path);
}
