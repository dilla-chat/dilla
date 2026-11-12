//! The tripwire for the LiveKit signalling protocol the desktop speaks. dillad's /rtc gate admits a
//! closed list of protocols (internal/sfu/rtcjoin.go `ClientProtocols`): 17 for the web client and the
//! Go SDK, 18 (`RustClientProtocol`) for this crate's pinned `livekit =0.10.1`, whose
//! livekit-signaling 0.1.9 sends `PROTOCOL_VERSION = 18`. A livekit bump that changes the number the
//! desktop sends makes every desktop call 400 at /rtc: this test fails first. Move it,
//! `RustClientProtocol` and dillad's gate tests together (packages/media/test/protocol-version.test.ts
//! is the web client's twin).
//!
//! It reads what `livekit::Room::connect` actually writes on the wire, not a constant: the first
//! signalling request's `protocol` query parameter, captured by a local listener. The call's own join
//! takes the v1 path (`/rtc/v1`, the number inside the gzip'd join_request's client_info); this test
//! asks for the v0 path, where the number is a plain query parameter. Both are the one
//! `SignalOptions::protocol_version` (livekit-signaling 0.1.9 src/lib.rs:1053 and :1177).

use std::time::Duration;

use livekit::{Room, RoomOptions};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpListener;

/// The protocol dillad's gate admits for the desktop (`sfu.RustClientProtocol`).
const RUST_CLIENT_PROTOCOL: &str = "18";

#[tokio::test]
async fn the_desktop_speaks_the_protocol_dillads_rtc_gate_admits_for_it() {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let url = format!("ws://{}", listener.local_addr().unwrap());
    let server = tokio::spawn(async move {
        let (mut socket, _) = listener.accept().await.unwrap();
        let mut head = Vec::new();
        let mut buf = [0u8; 4096];
        while !head.windows(4).any(|w| w == b"\r\n\r\n") {
            let n = socket.read(&mut buf).await.unwrap();
            if n == 0 {
                break;
            }
            head.extend_from_slice(&buf[..n]);
        }
        let _ = socket
            .write_all(b"HTTP/1.1 404 Not Found\r\ncontent-length: 0\r\nconnection: close\r\n\r\n")
            .await;
        String::from_utf8_lossy(&head).into_owned()
    });
    // The vendored webrtc-sys refuses a peer connection factory until the dilla frame hook is installed
    // (tests/hook_enforced.rs): this crate's router installs it, as every `Call` does.
    let _router = dilla_media::transform::Router::get().expect("the dilla hook");
    let mut options = RoomOptions::default();
    options.join_retries = 0;
    options.single_peer_connection = false;
    options.connect_timeout = Duration::from_secs(5);
    // The connect fails (the listener answers 404); only its request matters.
    let _ = tokio::time::timeout(
        Duration::from_secs(20),
        Room::connect(&url, "not-a-token", options),
    )
    .await;
    let head = tokio::time::timeout(Duration::from_secs(20), server)
        .await
        .expect("the SDK sent a signalling request")
        .unwrap();
    let line = head.lines().next().unwrap_or_default();
    assert!(line.starts_with("GET /rtc?"), "{line}");
    let protocol = line
        .split(['?', '&', ' '])
        .find_map(|kv| kv.strip_prefix("protocol="))
        .unwrap_or_else(|| panic!("no protocol parameter: {line}"));
    assert_eq!(
        protocol, RUST_CLIENT_PROTOCOL,
        "the livekit crate now sends protocol {protocol}: dillad's /rtc gate (internal/sfu/rtcjoin.go \
         RustClientProtocol) refuses it until both move together"
    );
}
