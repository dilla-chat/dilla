//! Seeds the attachments of a fixture into the real dillad. Owned by Task 12; no other file does.
//!
//! A message's attachments are part of its post, so they cannot be added after the history is posted: the posting
//! loop in `parity_world/main.rs` (not Task 12's file) has to call [`send`] instead of `send_text` for a message with
//! attachments. Until the controller wires that one line (Task 12's report), the facet stays refused.

#![allow(dead_code, reason = "send is called once main.rs's posting loop is wired (Task 12's report)")]

use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::attachments::NewFile;
use dilla_client::state::ChannelId;
use dilla_client::state::attachments::TrayPhase;
use dilla_client::testing::host::Person;
use dilla_client::testing::spec::{AttachmentSpec, MessageSpec, ScreenFixture};

/// Names the facet when the fixture asks for it and this world cannot seed it yet.
pub(super) fn unseedable(f: &ScreenFixture, out: &mut Vec<String>) {
    let w = &f.world;
    if w.messages.iter().any(|m| !m.attachments.is_empty()) {
        out.push("attachments".to_owned());
    }
}

/// Nothing to add after the history: attachments go out with their post ([`send`]).
#[allow(clippy::unused_async)]
pub(super) async fn seed(_f: &ScreenFixture, _people: &[(String, Person)]) {}

/// The file a spec names: a picture of its size for an image (the same bytes on every run, so the GPUI side draws
/// what the web drew), else `size` deterministic bytes.
pub(super) fn bytes_of(a: &AttachmentSpec) -> Vec<u8> {
    match (a.mime.as_str(), a.width, a.height) {
        ("image/png" | "image/jpeg" | "image/webp" | "image/gif", Some(w), Some(h)) => {
            dilla_client::features::attachments::fixture_picture(&a.name, w, h)
        }
        _ => (0..a.size).map(|i| (i.wrapping_mul(31) % 251) as u8).collect(),
    }
}

/// Posts `m` with its attachments: each file through the tray (sealed, previewed, uploaded), then the post.
pub(super) async fn send(client: &Client, channel: ChannelId, m: &MessageSpec) {
    let tray = client.attachments().tray(channel);
    let files = m
        .attachments
        .iter()
        .map(|a| NewFile::from_bytes(&a.name, &a.mime, bytes_of(a)))
        .collect();
    tray.add(files).await.expect("the tray takes the fixture's files");
    let deadline = std::time::Instant::now() + Duration::from_secs(60);
    while !tray.entries().iter().all(|e| e.phase == TrayPhase::Ready) {
        assert!(std::time::Instant::now() < deadline, "the fixture's files did not upload");
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let descriptors = tray.take_all().expect("every file is ready");
    client
        .attachments()
        .send(channel, m.body.clone(), descriptors)
        .await
        .unwrap()
        .expect("queued");
}
