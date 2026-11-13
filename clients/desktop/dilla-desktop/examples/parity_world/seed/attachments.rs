//! Seeds the attachments of a fixture into the real dillad. Owned by Task 12; no other file does.
//!
//! A message's attachments are part of its post, so they cannot be added after the history is posted: the history loop
//! of `parity_world/main.rs` posts a message with attachments through [`send`] (by way of `seed::post`) instead of
//! `send_text`.

use std::time::Duration;

use dilla_client::features::attachments::NewFile;
use dilla_client::state::attachments::TrayPhase;
use dilla_client::state::{ChannelId, MessageId};
use dilla_client::testing::host::Person;
use dilla_client::testing::spec::{AttachmentSpec, MessageSpec, ScreenFixture};
use dilla_client::{Client, ClientError};

/// Names what this world cannot seed. Nothing: a message's files go out with its post ([`send`]).
pub(super) fn unseedable(_f: &ScreenFixture, _out: &mut Vec<String>) {}

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
        _ => (0..a.size)
            .map(|i| (i.wrapping_mul(31) % 251) as u8)
            .collect(),
    }
}

/// Posts `m` with its attachments: each file through the tray (sealed, previewed, uploaded), then the post. `None` for a
/// message without attachments.
pub(super) async fn send(
    client: &Client,
    channel: ChannelId,
    m: &MessageSpec,
) -> Option<Result<MessageId, ClientError>> {
    if m.attachments.is_empty() {
        return None;
    }
    let tray = client.attachments().tray(channel);
    let files = m
        .attachments
        .iter()
        .map(|a| NewFile::from_bytes(&a.name, &a.mime, bytes_of(a)))
        .collect();
    tray.add(files)
        .await
        .expect("the tray takes the fixture's files");
    let deadline = std::time::Instant::now() + Duration::from_secs(60);
    while !tray.entries().iter().all(|e| e.phase == TrayPhase::Ready) {
        assert!(
            std::time::Instant::now() < deadline,
            "the fixture's files did not upload"
        );
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
    let descriptors = tray.take_all().expect("every file is ready");
    Some(
        client
            .attachments()
            .send(channel, m.body.clone(), descriptors)
            .await
            .unwrap_or(Err(ClientError::Stopped)),
    )
}
