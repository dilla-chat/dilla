//! Task 12's client tests for the attachments area (standard procedure S2), against a real dillad: two Rust clients in
//! one channel send files both ways, and the 25 MB cap admits exactly the cap.
//!
//! Needs `target/dilla-testhost` and the WASI core, as `tests/sync.rs` does; `#[ignore]`d so a plain `cargo test` needs
//! no Go toolchain (`cargo test -p dilla-client --test attachments -- --include-ignored`).

use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::attachments::{ATTACHMENT_CAP, NewFile, is_thumbnail_bytes};
use dilla_client::features::gifs::dm_gif_candidates;
use dilla_client::state::attachments::TrayPhase;
use dilla_client::state::{AppState, ChannelId, CommunityId, TimelineItem, TimelineItemState};
use dilla_client::testing::host::{
    Host, Person, clear_session, community, sign_up, start, start_host, wait_for,
};

const WAIT: Duration = Duration::from_secs(60);

async fn two_people(tag: &str) -> (Host, Person, Person, [u8; 16]) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_host(&tag, "127.0.0.1:0", None)
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let a = sign_up(&host, &invite, "alice", "alice", tag).await;
    let b = sign_up(&host, &invite, "bob", "bob", tag).await;
    tokio::task::spawn_blocking(move || {
        let c = community(&host, &a, "gpui", &[&b]);
        clear_session(&a);
        clear_session(&b);
        (host, a, b, c)
    })
    .await
    .unwrap()
}

async fn general(client: &Client, c: [u8; 16]) -> ChannelId {
    wait_for(client, WAIT, "the channel list", |s| {
        s.channel_named(CommunityId(c), "general").is_some()
    })
    .await
    .channel_named(CommunityId(c), "general")
    .unwrap()
    .id
}

async fn both_in(a: &Client, b: &Client, channel: ChannelId) {
    a.open_channel(channel)
        .await
        .unwrap()
        .expect("a opens the channel");
    b.open_channel(channel)
        .await
        .unwrap()
        .expect("b joins the channel");
}

fn row_with(s: &AppState, channel: ChannelId, body: &str) -> Option<TimelineItem> {
    s.timelines
        .get(&channel)?
        .items
        .iter()
        .find(|i| i.body == body && i.state == TimelineItemState::Ok)
        .cloned()
}

fn sha(b: &[u8]) -> [u8; 32] {
    dilla_core::identity::sha256(b)
}

fn png(w: u32, h: u32, seed: u8) -> Vec<u8> {
    let img = image::RgbImage::from_fn(w, h, |x, y| {
        image::Rgb([
            (x as u8).wrapping_mul(seed),
            (y as u8).wrapping_add(seed),
            seed,
        ])
    });
    let mut out = std::io::Cursor::new(Vec::new());
    img.write_to(&mut out, image::ImageFormat::Png).unwrap();
    out.into_inner()
}

/// DmGifPrivacy.test.tsx (the brief's `dm_gif_needs_consent`): a 1:1 DM finds its GIF server among the servers both
/// people share, with `gifStatus` only; that server needs this device's consent, naming it, before any search; consent
/// from Privacy alone ("on") is not consent for a server; revoking removes it.
#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs dilla-testhost and the WASI core"]
async fn dm_gif_needs_consent() {
    let (_host, pa, pb, c) = two_people("t12-dmgif").await;
    let (a, b) = (start(&pa).await, start(&pb).await);
    general(&a, c).await;
    let peer = wait_for(&b, WAIT, "b's account", |s| s.account.user_id.is_some())
        .await
        .account
        .user_id
        .unwrap();
    let s = wait_for(&a, WAIT, "the members of the server", |s| {
        s.members.get(&CommunityId(c)).is_some_and(|m| m.iter().any(|x| x.user_id == peer))
    })
    .await;
    let me = s.account.user_id.unwrap();
    assert_eq!(dm_gif_candidates(&s, me, peer), [CommunityId(c)]);
    // No key: no server to proxy through.
    assert_eq!(a.gifs().find_dm_community(peer).await.unwrap().unwrap(), None);
    a.gifs().testing_set_key(CommunityId(c), "abcdefgh12345678").await.unwrap().unwrap();
    let found = a.gifs().find_dm_community(peer).await.unwrap().unwrap();
    assert_eq!(found, Some(CommunityId(c)));
    assert!(!a.gifs().dm_consent_for(CommunityId(c)), "no consent before the notice");
    a.gifs().set_dm_consent(None).unwrap();
    assert!(!a.gifs().dm_consent_for(CommunityId(c)));
    a.gifs().set_dm_consent(Some("on")).unwrap();
    assert!(!a.gifs().dm_consent_for(CommunityId(c)), "Privacy's on still asks with the server named");
    a.gifs().set_dm_consent(Some(&CommunityId(c).hex())).unwrap();
    assert!(a.gifs().dm_consent_for(CommunityId(c)));
    a.gifs().set_dm_consent(None).unwrap();
    assert!(!a.gifs().dm_consent_for(CommunityId(c)), "revoked");
    drop(b);
}

/// download.ts / routes.ts `getBlob(.., size + 16)`: a stored object longer than its descriptor's sealed size is
/// refused once the bound is passed, while the instance is still sending, never read to its end.
#[tokio::test]
async fn a_blob_one_byte_over_its_sealed_size_is_refused_without_reading_it_all() {
    use std::io::{Read, Write};
    use dilla_client::http::routes::{GetBlob, NoBody};
    use dilla_client::http::{AuthToken, ReqwestHttp};
    use dilla_client::seams::HttpError;

    let size: u64 = 1000;
    let cap = size + 16;
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let url = url::Url::parse(&format!("http://{}", listener.local_addr().unwrap())).unwrap();
    let server = std::thread::spawn(move || {
        let (mut s, _) = listener.accept().unwrap();
        let mut buf = [0u8; 4096];
        let _ = s.read(&mut buf);
        s.set_write_timeout(Some(Duration::from_secs(2))).unwrap();
        // One byte over the bound first, then a body that never ends: a client reading to the end waits forever.
        let _ = s.write_all(b"HTTP/1.1 200 OK\r\ntransfer-encoding: chunked\r\nconnection: close\r\n\r\n");
        let first = vec![7u8; (cap + 1) as usize];
        let _ = s.write_all(format!("{:x}\r\n", first.len()).as_bytes());
        let _ = s.write_all(&first);
        let _ = s.write_all(b"\r\n");
        let chunk = vec![7u8; 64 * 1024];
        for _ in 0..64 {
            if s.write_all(format!("{:x}\r\n", chunk.len()).as_bytes()).is_err()
                || s.write_all(&chunk).is_err()
                || s.write_all(b"\r\n").is_err()
            {
                break;
            }
        }
        std::thread::sleep(Duration::from_secs(4));
    });
    let started = std::time::Instant::now();
    let http = ReqwestHttp::new(url, AuthToken::new("t")).unwrap();
    let err = http
        .call_capped::<GetBlob>(&["0123456789abcdef0123456789abcdef", &"ab".repeat(32)], NoBody, cap)
        .await
        .expect_err("one byte over the sealed size");
    assert!(matches!(err, HttpError::Status { status: 200, code: Some(ref c) } if c == "E_BODY_TOO_LARGE"), "{err:?}");
    assert!(started.elapsed() < Duration::from_secs(3), "the client read on to the end of the body");
    server.join().unwrap();
}

/// Attaches `files` in `channel`, waits until each is ready, sends `body` with them and waits until the sender's row is
/// stored.
async fn send_files(client: &Client, channel: ChannelId, files: Vec<NewFile>, body: &str) {
    let tray = client.attachments().tray(channel);
    let n = files.len();
    tray.add(files).await.expect("the tray takes the files");
    let s = wait_for(client, WAIT, "the tray is ready", |s| {
        let t = s.attachments.tray(channel);
        t.len() == n && t.iter().all(|e| e.phase == TrayPhase::Ready)
    })
    .await;
    assert_eq!(s.attachments.tray(channel).len(), n);
    let descriptors = tray.take_all().expect("every file is ready");
    client
        .attachments()
        .send(channel, body.to_owned(), descriptors)
        .await
        .unwrap()
        .expect("the post is queued");
    wait_for(client, WAIT, "the sender's row is stored", |s| {
        row_with(s, channel, body).is_some()
    })
    .await;
    let s = client.state().borrow().clone();
    assert!(
        s.attachments.tray(channel).is_empty(),
        "the sent files left the tray"
    );
}

/// e2e/web/conversation.spec.ts "images and files go both ways".
#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs dilla-testhost and the WASI core"]
async fn images_and_files_go_both_ways() {
    let (_host, pa, pb, c) = two_people("t12-both").await;
    let (a, b) = (start(&pa).await, start(&pb).await);
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;

    for (from, to, tag) in [(&a, &b, "a"), (&b, &a, "b")] {
        let drawn = png(320, 240, if tag == "a" { 1 } else { 2 });
        let notes: Vec<u8> = (0..70_000u32).map(|i| (i * 31 % 251) as u8).collect();
        let body = format!("two files {tag}");
        send_files(
            from,
            channel,
            vec![
                NewFile::from_bytes("drawn.png", "image/png", drawn.clone()),
                NewFile::from_bytes("notes.bin", "application/octet-stream", notes.clone()),
            ],
            &body,
        )
        .await;
        let s = wait_for(to, WAIT, "the other side's two files", |s| {
            row_with(s, channel, &body).is_some_and(|i| i.attachments.len() == 2)
        })
        .await;
        let row = row_with(&s, channel, &body).unwrap();
        let got: Vec<(u32, &str, &str, u64)> = row
            .attachments
            .iter()
            .map(|x| (x.index, x.name.as_str(), x.mime.as_str(), x.size))
            .collect();
        assert_eq!(
            got,
            [
                (0, "drawn.png", "image/png", drawn.len() as u64),
                (1, "notes.bin", "application/octet-stream", 70_000),
            ]
        );
        let seq = row.seq.expect("a stored row");
        let image = to
            .attachments()
            .open(channel, seq, 0, false)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(sha(&image.bytes), sha(&drawn));
        assert_eq!(
            (image.name.as_str(), image.mime.as_str()),
            ("drawn.png", "image/png")
        );
        let thumb = to
            .attachments()
            .open(channel, seq, 0, true)
            .await
            .unwrap()
            .unwrap();
        assert!(
            is_thumbnail_bytes(&thumb.bytes),
            "the image carries its preview"
        );
        let file = to
            .attachments()
            .open(channel, seq, 1, false)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(sha(&file.bytes), sha(&notes));
        let no_thumb = to.attachments().open(channel, seq, 1, true).await.unwrap();
        assert_eq!(
            no_thumb.err().map(|e| e.to_string()),
            Some("E_ATTACHMENT_MISSING".to_owned())
        );
    }
}

/// e2e/web/conversation.spec.ts "a 25 MB file goes through and one byte more stays as an oversized card with a toast":
/// the cap is inclusive; the file one byte over it fails at once, is never read and never uploaded.
#[tokio::test(flavor = "multi_thread")]
#[ignore = "needs dilla-testhost and the WASI core"]
async fn file_cap_exact_passes_one_more_refused() {
    let (_host, pa, pb, c) = two_people("t12-cap").await;
    let (a, b) = (start(&pa).await, start(&pb).await);
    let channel = general(&a, c).await;
    general(&b, c).await;
    both_in(&a, &b, channel).await;

    let big: Vec<u8> = (0..ATTACHMENT_CAP as u32)
        .map(|i| (i.wrapping_mul(2_654_435_761) >> 24) as u8)
        .collect();
    send_files(
        &a,
        channel,
        vec![NewFile::from_bytes(
            "big.bin",
            "application/octet-stream",
            big.clone(),
        )],
        "big",
    )
    .await;
    let s = wait_for(&b, WAIT, "the 25 MB file", |s| {
        row_with(s, channel, "big").is_some()
    })
    .await;
    let row = row_with(&s, channel, "big").unwrap();
    assert_eq!(row.attachments[0].size, ATTACHMENT_CAP);
    let opened = b
        .attachments()
        .open(channel, row.seq.unwrap(), 0, false)
        .await
        .unwrap()
        .unwrap();
    assert_eq!(opened.bytes.len() as u64, ATTACHMENT_CAP);
    assert_eq!(sha(&opened.bytes), sha(&big));
    drop(big);

    let dir = std::env::temp_dir().join(format!("dilla-t12-cap-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    let over = dir.join("over.bin");
    std::fs::write(&over, vec![7u8; ATTACHMENT_CAP as usize + 1]).unwrap();
    let file = NewFile::from_path(&over).unwrap();
    // Gone before the tray's turn: had the tray read it, the entry would fail with E_ATTACHMENT_READ instead.
    std::fs::remove_dir_all(&dir).unwrap();
    let tray = a.attachments().tray(channel);
    tray.add(vec![file]).await.unwrap();
    let entries = tray.entries();
    assert_eq!(entries.len(), 1);
    assert_eq!(
        (
            entries[0].phase,
            entries[0].reason.as_str(),
            entries[0].name.as_str()
        ),
        (TrayPhase::Failed, "E_ATTACHMENT_TOO_LARGE", "over.bin")
    );
    assert!(tray.take_all().is_err(), "a failed file is never sent");
    tray.discard(&entries[0].id).await;
    assert!(tray.entries().is_empty());
}
