//! The HTTP client against the TypeScript client's route table, a mock server, and a real dillad.

use std::collections::BTreeMap;
use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::path::Path;
use std::process::Command;
use std::thread::JoinHandle;
use std::time::Duration;

use dilla_client::http::routes::{
    self, Answer, BodyEncoding, GetAccountMe, GetBackup, ListCommunities, NegotiateSession,
    NegotiatedAnswer, NoBody, PasswordLogin, PasswordLoginAnswer, Reply, Transport,
};
use dilla_client::http::{AuthToken, ReqwestHttp, fill_path};
use dilla_client::seams::{Http, HttpError, Route};
use url::Url;

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
        "{} | {} | {} | auth={} | ok={:?} | max={:?} | {}",
        e.name,
        request_name(e.request),
        answer_name(e.answer),
        e.auth,
        e.ok,
        e.max_body,
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
        "{} | {} | {} | auth={} | ok={:?} | max={:?} | {}",
        v["name"].as_str().expect("name"),
        v["request"].as_str().expect("request"),
        v["response"].as_str().expect("response"),
        v["auth"].as_bool().expect("auth"),
        ok,
        v["maxBody"].as_u64(),
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
        .call::<routes::GetDeviceListHistory>(&["0123456789abcdef0123456789abcdef", "ab"], NoBody)
        .await
        .expect("answered");
    let request = served.join().expect("mock");
    assert!(
        request.starts_with(
            "GET /v1/users/0123456789abcdef0123456789abcdef/device-list?after=ab HTTP/1.1"
        ),
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
        .call::<routes::GetDeviceListHistory>(&["0123456789abcdef0123456789abcdef"], NoBody)
        .await
        .expect("answered");
    assert!(
        served
            .join()
            .expect("mock")
            .starts_with("GET /v1/users/0123456789abcdef0123456789abcdef/device-list HTTP/1.1")
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
        fill_path(
            "/v1/devices/{device_id}/sessions",
            &["0123456789abcdef0123456789abcdef"]
        )
        .unwrap(),
        "/v1/devices/0123456789abcdef0123456789abcdef/sessions"
    );
    assert_eq!(fill_path("/i/{code}", &["a b/c"]).unwrap(), "/i/a%20b%2Fc");
    assert_eq!(
        fill_path(
            "/v1/groups/{group_id}/messages?from={from}&limit={limit}",
            &["0123456789abcdef0123456789abcdef", "0", "50"]
        )
        .unwrap(),
        "/v1/groups/0123456789abcdef0123456789abcdef/messages?from=0&limit=50"
    );
    assert_eq!(
        fill_path(
            "/v1/communities/{community_id}/members?after={after}",
            &["0123456789abcdef0123456789abcdef"]
        )
        .unwrap(),
        "/v1/communities/0123456789abcdef0123456789abcdef/members"
    );
    assert!(fill_path("/v1/devices/{device_id}", &[]).is_err());
    assert!(fill_path("/v1/devices/{device_id}", &["a", "b"]).is_err());
    assert!(fill_path("/v1/communities", &["a"]).is_err());
}

#[test]
fn a_path_argument_cannot_leave_its_segment() {
    // Empty, `.` and `..` are dot segments to the url crate (`%2e` too): they would address another route.
    for bad in ["", ".", ".."] {
        for template in [
            "/v1/devices/{device_id}/sessions",
            "/i/{code}",
            "/v1/welcomes/{welcome_id}",
        ] {
            assert!(
                fill_path(template, &[bad]).is_err(),
                "{template} accepted {bad:?}"
            );
        }
    }
    let channel = "0123456789abcdef0123456789abcdef";
    assert!(
        fill_path(
            "/v1/channels/{channel_id}/blobs/{blob_id}",
            &[channel, ".."]
        )
        .is_err()
    );
    // An id is 16 bytes of lowercase hex, a digest 32, a count digits, a kind 0 or 1.
    assert!(fill_path("/v1/channels/{channel_id}", &["abc"]).is_err());
    assert!(
        fill_path(
            "/v1/channels/{channel_id}",
            &["0123456789ABCDEF0123456789ABCDEF"]
        )
        .is_err()
    );
    assert!(
        fill_path(
            "/v1/channels/{channel_id}",
            &["../0123456789abcdef0123456789abc"]
        )
        .is_err()
    );
    assert!(
        fill_path(
            "/v1/channels/{channel_id}/blobs/{blob_id}",
            &[channel, channel]
        )
        .is_err()
    );
    assert!(
        fill_path(
            "/v1/channels/{channel_id}/blobs/{blob_id}",
            &[channel, &"ab".repeat(32)]
        )
        .is_ok()
    );
    assert!(fill_path("/v1/welcomes/{welcome_id}", &["12x"]).is_err());
    assert!(fill_path("/v1/backups/{kind}/0", &["2"]).is_err());
    assert!(
        fill_path(
            "/v1/communities/{community_id}/gifs/media/{gif_id}?r={variant}",
            &[channel, "a/b", "x"]
        )
        .is_err()
    );
    // A query value is data, not a segment: encoded, and `..` in it is harmless.
    assert_eq!(
        fill_path(
            "/v1/communities/{community_id}/members?after={after}",
            &[channel, ".."]
        )
        .unwrap(),
        format!("/v1/communities/{channel}/members?after=..")
    );
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
            assertion: zeroize::Zeroizing::new("assertion".into()),
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
    // The assertion is a credential: it is not printed.
    assert!(!format!("{a:?}").contains("assertion\""), "{a:?}");
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

#[tokio::test]
async fn a_token_that_is_not_a_header_value_is_an_error_not_an_anonymous_request() {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
    listener.set_nonblocking(true).expect("nonblocking");
    let url = Url::parse(&format!("http://{}", listener.local_addr().expect("addr"))).expect("url");
    let err = http_at(&url, "bad\ntoken")
        .call::<GetAccountMe>(&[], NoBody)
        .await
        .expect_err("an invalid token");
    assert!(
        matches!(err, HttpError::Transport(ref m) if m.contains("token")),
        "{err:?}"
    );
    assert!(listener.accept().is_err(), "nothing was sent");
}

/// Serves one 200 answer that declares `declared` bytes and sends `sent` of them, then holds the connection open.
fn mock_big(declared: usize, sent: usize) -> (Url, JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
    let url = Url::parse(&format!("http://{}", listener.local_addr().expect("addr"))).expect("url");
    let handle = std::thread::spawn(move || {
        let (mut stream, _) = listener.accept().expect("accept");
        read_request(&mut stream);
        let head =
            format!("HTTP/1.1 200 OK\r\ncontent-length: {declared}\r\nconnection: close\r\n\r\n");
        let _ = stream.write_all(head.as_bytes());
        let chunk = vec![0x80u8; 64 * 1024];
        let mut left = sent;
        while left > 0 {
            let n = left.min(chunk.len());
            if stream.write_all(&chunk[..n]).is_err() {
                return; // the client stopped reading
            }
            left -= n;
        }
        // Hold the connection until the client hangs up or gives up.
        stream.set_read_timeout(Some(Duration::from_secs(5))).ok();
        let _ = stream.read(&mut [0u8; 1]);
    });
    (url, handle)
}

#[tokio::test]
async fn a_body_over_the_route_cap_is_refused_without_reading_it() {
    // GetLimits-style cap: maxBodyBytes 65536 for list_bot_webhooks is not an HTTP route;
    let entry =
        routes::lookup(&http::Method::GET, <routes::GetDeviceList as Route>::PATH).expect("route");
    assert_eq!(entry.max_body, Some(1_048_576));
    let channel = "0123456789abcdef0123456789abcdef";

    // A declared length over the cap is refused before a byte is read: the server sends no body at all.
    let (url, served) = mock_big(1_048_577, 0);
    let err = http_at(&url, "t")
        .call::<routes::GetDeviceList>(&[channel], NoBody)
        .await
        .expect_err("over the cap");
    assert!(
        matches!(err, HttpError::Decode(ref m) if m.contains("larger than")),
        "{err:?}"
    );
    served.join().expect("mock");

    // A body at the cap is read.
    let (url, served) = mock_big(1_048_576, 1_048_576);
    let Reply { body, .. } = http_at(&url, "t")
        .call::<routes::GetDeviceList>(&[channel], NoBody)
        .await
        .expect("at the cap");
    assert_eq!(body.len(), 1_048_576);
    served.join().expect("mock");
}

#[tokio::test]
async fn a_body_that_outgrows_the_cap_while_streaming_stops_being_read() {
    // The server promises 1 MiB (within the cap) via chunked encoding it never ends: it streams 3 MiB, the cap is 1 MiB.
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind");
    let url = Url::parse(&format!("http://{}", listener.local_addr().expect("addr"))).expect("url");
    let handle = std::thread::spawn(move || {
        let (mut stream, _) = listener.accept().expect("accept");
        read_request(&mut stream);
        // Once the client stops reading, a write blocks as soon as the socket buffers fill. Linux's buffers hold
        // the whole stream; Windows's don't, and an unbounded write would hang this thread (and the join) forever.
        stream
            .set_write_timeout(Some(Duration::from_secs(2)))
            .expect("write timeout");
        let _ = stream.write_all(
            b"HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\nconnection: close\r\n\r\n",
        );
        let chunk = vec![0x80u8; 64 * 1024];
        let mut sent = 0usize;
        for _ in 0..32 {
            let head = format!("{:x}\r\n", chunk.len());
            if stream.write_all(head.as_bytes()).is_err()
                || stream.write_all(&chunk).is_err()
                || stream.write_all(b"\r\n").is_err()
            {
                break;
            }
            sent += chunk.len();
        }
        // Never ends the body: a client that reads to the end waits here.
        std::thread::sleep(Duration::from_secs(4));
        sent
    });
    let started = std::time::Instant::now();
    let channel = "0123456789abcdef0123456789abcdef";
    let err = http_at(&url, "t")
        .call::<routes::GetDeviceList>(&[channel], NoBody)
        .await
        .expect_err("over the cap");
    assert!(
        matches!(err, HttpError::Decode(ref m) if m.contains("larger than")),
        "{err:?}"
    );
    assert!(
        started.elapsed() < Duration::from_secs(3),
        "the client waited for the end of the body"
    );
    handle.join().expect("mock");
}

#[tokio::test]
async fn a_route_with_no_cap_still_has_the_transport_ceiling() {
    let channel = "0123456789abcdef0123456789abcdef";
    let blob = "ab".repeat(32);
    let (url, served) = mock_big(33 * 1024 * 1024, 0);
    let err = http_at(&url, "t")
        .call::<routes::GetBlob>(&[channel, &blob], NoBody)
        .await
        .expect_err("over the ceiling");
    assert!(
        matches!(err, HttpError::Decode(ref m) if m.contains("larger than")),
        "{err:?}"
    );
    served.join().expect("mock");
}
