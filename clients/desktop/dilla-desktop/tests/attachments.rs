//! Task 12's parity, behaviour and keyboard tests for the attachments area (standard procedure S4-S6).
//!
//! The parity tests compare `parity/fixtures/attachments/*.json` with their references, shot with `shoot.mjs` (the
//! parity world posts a message's files with it: `seed::post`). The ones still parked (ruling R22) carry their measured
//! numbers and causes.
//!
//! The behaviour tests of `e2e/web/conversation.spec.ts` run at the client level against a real dillad
//! (`dilla-client/tests/attachments.rs`: `images_and_files_go_both_ways`, `file_cap_exact_passes_one_more_refused`);
//! their app-level drive needs the composer's send to carry the tray (Task 9's composer), which goes to Task 28's list.

use std::path::PathBuf;

use dilla_client::testing::spec::ScreenFixture;
use dilla_parity::screen::assert_screen;

fn fixture(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/attachments")
        .join(format!("{name}.json"))
}

const STATES: [&str; 6] = [
    "cards",
    "cards-b",
    "lightbox",
    "lightbox-b",
    "media-player",
    "media-player-b",
];

/// Every fixture parses, and each B fixture varies its A in at least three ways (S3): the window, and longer names or
/// more files.
#[test]
fn fixtures_parse_and_b_varies_a() {
    for name in STATES {
        let raw = std::fs::read_to_string(fixture(name)).unwrap();
        let f: ScreenFixture = serde_json::from_str(&raw).unwrap_or_else(|e| panic!("{name}: {e}"));
        assert_eq!(f.name, format!("attachments/{name}"));
    }
    let load = |n: &str| -> ScreenFixture {
        serde_json::from_str(&std::fs::read_to_string(fixture(n)).unwrap()).unwrap()
    };
    let (a, b) = (load("cards"), load("cards-b"));
    assert_eq!((a.window, b.window), ((1280, 800), (1440, 900)));
    let files = |f: &ScreenFixture| {
        f.world
            .messages
            .iter()
            .map(|m| m.attachments.len())
            .sum::<usize>()
    };
    assert!(
        files(&b) >= 10 && files(&a) == 2,
        "nine images and a file against two"
    );
    let longest = |f: &ScreenFixture| {
        f.world
            .messages
            .iter()
            .flat_map(|m| &m.attachments)
            .map(|x| x.name.len())
            .max()
            .unwrap_or(0)
    };
    assert!(longest(&b) > 60 && longest(&a) < 20, "long file names");
}

macro_rules! parity {
    ($($test:ident => $name:literal $(, $why:literal)?);* $(;)?) => {$(
        #[test]
        $(#[ignore = $why])?
        fn $test() {
            if let Err(e) = assert_screen(&fixture($name)) {
                panic!("{e}");
            }
        }
    )*};
}

parity! {
    parity_attachments_cards => "cards", "parity after T6c: chrome 0.9573 % (the fixture pictures are 1px checkerboards: GPUI's cover-crop downscale filters them otherwise than Chromium's, so every tile differs over its whole area)";
    parity_attachments_cards_b => "cards-b", "parity after T6c: chrome 7.0269 % (as cards, over nine tiles)";
    parity_attachments_lightbox => "lightbox";
    parity_attachments_lightbox_b => "lightbox-b";
    parity_attachments_media_player => "media-player", "parity after T6c: chrome 0.3849 % (the audio card is wider than AttachmentCard.css's, which fits its content, and its play button's label differs)";
    parity_attachments_media_player_b => "media-player-b", "parity after T6c: whole frame 0.6066 % (as media-player)";
}

mod behaviour {
    //! e2e/voice/media-attachments.spec.ts, "bob plays alice's webm and WAV inline; a truncated file shows the error and
    //! save" (the brief's `audio_attachment_plays`), against a real dillad and through this app's own audio path
    //! (symphonia, rodio). The player is rodio's, detached from a device (CI has none): the test pulls its output.

    use std::sync::Arc;
    use std::time::Duration;

    use dilla_client::Client;
    use dilla_client::features::attachments::NewFile;
    use dilla_client::state::attachments::TrayPhase;
    use dilla_client::state::{AppState, ChannelId, CommunityId, TimelineItem, TimelineItemState};
    use dilla_client::testing::host::{
        Host, Person, clear_session, community, sign_up, start, start_host, wait_for,
    };
    use dilla_desktop::screens::attachments::media::{
        MediaDecoder, Playback, open_audio, write_for_system_player,
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

    fn row(s: &AppState, channel: ChannelId, body: &str) -> Option<TimelineItem> {
        s.timelines
            .get(&channel)?
            .items
            .iter()
            .find(|i| i.body == body && i.state == TimelineItemState::Ok)
            .cloned()
    }

    /// One second of a 440 Hz tone, 16-bit mono WAV at 8 kHz.
    fn wav() -> Vec<u8> {
        let (rate, n) = (8000u32, 8000u32);
        let mut out = b"RIFF".to_vec();
        out.extend_from_slice(&(36 + n * 2).to_le_bytes());
        out.extend_from_slice(b"WAVEfmt ");
        out.extend_from_slice(&16u32.to_le_bytes());
        out.extend_from_slice(&1u16.to_le_bytes());
        out.extend_from_slice(&1u16.to_le_bytes());
        out.extend_from_slice(&rate.to_le_bytes());
        out.extend_from_slice(&(rate * 2).to_le_bytes());
        out.extend_from_slice(&2u16.to_le_bytes());
        out.extend_from_slice(&16u16.to_le_bytes());
        out.extend_from_slice(b"data");
        out.extend_from_slice(&(n * 2).to_le_bytes());
        for i in 0..n {
            let s =
                ((i as f32 * 440.0 * std::f32::consts::TAU / rate as f32).sin() * 16000.0) as i16;
            out.extend_from_slice(&s.to_le_bytes());
        }
        out
    }

    #[tokio::test(flavor = "multi_thread")]
    #[ignore = "needs dilla-testhost and the WASI core"]
    async fn audio_attachment_plays() {
        let (_host, pa, pb, c) = two_people("t12-audio").await;
        let (alice, bob) = (start(&pa).await, start(&pb).await);
        let channel = general(&alice, c).await;
        general(&bob, c).await;
        alice.open_channel(channel).await.unwrap().unwrap();
        bob.open_channel(channel).await.unwrap().unwrap();

        let tone = wav();
        let truncated = tone[..30].to_vec();
        let webm = b"\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01webm".to_vec();
        let tray = alice.attachments().tray(channel);
        tray.add(vec![
            NewFile::from_bytes("shanty.wav", "audio/wav", tone),
            NewFile::from_bytes("cut.wav", "audio/wav", truncated),
            NewFile::from_bytes("clip.webm", "video/webm", webm.clone()),
        ])
        .await
        .unwrap();
        assert!(tray.entries().iter().all(|e| e.phase == TrayPhase::Ready));
        let d = tray.take_all().unwrap();
        alice
            .attachments()
            .send(channel, "the shanty".into(), d, None)
            .await
            .unwrap()
            .unwrap();
        let s = wait_for(&bob, WAIT, "alice's three files", |s| {
            row(s, channel, "the shanty").is_some_and(|r| r.attachments.len() == 3)
        })
        .await;
        let seq = row(&s, channel, "the shanty").unwrap().seq.unwrap();

        // The WAV plays inline: the tone comes out; volume, seek and pause work.
        let opened = bob
            .attachments()
            .open(channel, seq, 0, false)
            .await
            .unwrap()
            .unwrap();
        let decoder = open_audio(Arc::new(opened.bytes), "audio/wav").expect("the WAV plays");
        assert_eq!(decoder.duration(), Some(Duration::from_secs(1)));
        let (player, mut out) = Playback::start_detached(Box::new(decoder)).unwrap();
        player.play();
        let first: Vec<f32> = out.by_ref().take(4000).collect();
        let rms = (first.iter().map(|x| x * x).sum::<f32>() / first.len() as f32).sqrt();
        assert!(rms > 0.2, "the tone plays (rms {rms})");
        player.set_volume(0.5);
        assert_eq!(player.volume(), 0.5);
        player.pause();
        assert!(player.is_paused());

        // The truncated WAV cannot play: the card shows the error and save.
        let cut = bob
            .attachments()
            .open(channel, seq, 1, false)
            .await
            .unwrap()
            .unwrap();
        assert!(open_audio(Arc::new(cut.bytes), "audio/wav").is_err());

        // The webm goes to the system player: a checked file under a random name in the instance's directory.
        let clip = bob
            .attachments()
            .open(channel, seq, 2, false)
            .await
            .unwrap()
            .unwrap();
        let dir = std::env::temp_dir().join(format!("dilla-t12-audio-{}", std::process::id()));
        let path = write_for_system_player(&dir, "video/webm", &clip.bytes).unwrap();
        assert_eq!(std::fs::read(&path).unwrap(), webm);
        assert_eq!(path.extension().and_then(|e| e.to_str()), Some("webm"));
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// packages/desktop/test/electron/shell.spec.ts, "downloads a decrypted local attachment while refusing blob document
    /// navigation", for the native shell (it has no document to navigate): the card's Save opens bob's copy of alice's
    /// file, asks the save dialog for a path, and writes the decrypted bytes there, and nothing before the person picks
    /// the path. The blob-navigation half has no native counterpart.
    #[gpui::test]
    #[ignore = "needs dilla-testhost and the WASI core"]
    fn save_writes_the_decrypted_attachment_to_disk(cx: &mut gpui::TestAppContext) {
        use dilla_desktop::screens::attachments::ui::{self, CardKey};
        cx.executor().allow_parking();
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .unwrap();
        let (_host, pa, pb, c) = rt.block_on(two_people("t28-save"));
        let (alice, bob) = rt.block_on(async { (start(&pa).await, start(&pb).await) });
        let channel = rt.block_on(general(&alice, c));
        rt.block_on(general(&bob, c));
        rt.block_on(alice.open_channel(channel)).unwrap().unwrap();
        rt.block_on(bob.open_channel(channel)).unwrap().unwrap();
        let body = b"decrypted local attachment".to_vec();
        let seq = rt.block_on(async {
            let tray = alice.attachments().tray(channel);
            tray.add(vec![NewFile::from_bytes(
                "attachment.txt",
                "text/plain",
                body.clone(),
            )])
            .await
            .unwrap();
            assert!(tray.entries().iter().all(|e| e.phase == TrayPhase::Ready));
            let d = tray.take_all().unwrap();
            alice
                .attachments()
                .send(channel, "the file".into(), d, None)
                .await
                .unwrap()
                .unwrap();
            let s = wait_for(&bob, WAIT, "alice's file", |s| {
                row(s, channel, "the file").is_some_and(|r| r.attachments.len() == 1)
            })
            .await;
            row(&s, channel, "the file").unwrap().seq.unwrap()
        });

        let dir = std::env::temp_dir().join(format!("dilla-t28-save-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let target = dir.join("saved.txt");
        cx.update(dilla_desktop::install);
        let bob = std::rc::Rc::new(bob);
        let key = CardKey {
            client: bob.serial(),
            channel,
            seq,
            index: 0,
        };
        cx.update(|cx| ui::save(&bob, key, cx));
        // The file opens, then the save dialog asks; nothing is written before the person answers it.
        let deadline = std::time::Instant::now() + WAIT;
        while !cx.did_prompt_for_new_path() {
            assert!(
                std::time::Instant::now() < deadline,
                "the save dialog never asked"
            );
            cx.run_until_parked();
            std::thread::sleep(Duration::from_millis(20));
        }
        assert_eq!(
            std::fs::read_dir(&dir).unwrap().count(),
            0,
            "nothing written before the answer"
        );
        cx.simulate_new_path_selection(|_| Some(target.clone()));
        let deadline = std::time::Instant::now() + WAIT;
        while !target.exists() {
            assert!(
                std::time::Instant::now() < deadline,
                "the file was never written"
            );
            cx.run_until_parked();
            std::thread::sleep(Duration::from_millis(20));
        }
        cx.run_until_parked();
        assert_eq!(std::fs::read(&target).unwrap(), body, "the decrypted bytes");
        let _ = std::fs::remove_dir_all(&dir);
        drop(alice);
    }
}

mod two_accounts {
    //! Task 28 items 9 and 10 (reviews M2, M4): two instance windows, two accounts of one server, against a real dillad.
    //! What a window's composer and cards hold is its own client's: the trays and their previews are keyed by client and
    //! channel, and each window's target is its own client.

    use std::rc::Rc;
    use std::time::Duration;

    use dilla_client::Client;
    use dilla_client::features::attachments::NewFile;
    use dilla_client::state::{ChannelId, CommunityId};
    use dilla_client::testing::host::{
        Host, clear_session, community, sign_up, start, start_host, wait_for,
    };
    use dilla_desktop::screens::attachments::ui::{self, Target};
    use gpui::{Context, IntoElement, Render, TestAppContext, Window, div};

    const WAIT: Duration = Duration::from_secs(60);

    struct Blank;

    impl Render for Blank {
        fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
            div()
        }
    }

    /// Alice and bob in one community, both clients running, and the general channel both see.
    struct World {
        _rt: tokio::runtime::Runtime,
        _host: Host,
        alice: Rc<Client>,
        bob: Rc<Client>,
        general: ChannelId,
    }

    fn world(tag: &'static str) -> World {
        let rt = tokio::runtime::Builder::new_multi_thread()
            .worker_threads(2)
            .enable_all()
            .build()
            .unwrap();
        let (host, alice, bob, general) = rt.block_on(async {
            let host = tokio::task::spawn_blocking(move || start_host(tag, "127.0.0.1:0", None))
                .await
                .unwrap();
            let invite = host.invites.last().unwrap().clone();
            let a = sign_up(&host, &invite, "alice", "alice", tag).await;
            let b = sign_up(&host, &invite, "bob", "bob", tag).await;
            let (host, a, b, c) = tokio::task::spawn_blocking(move || {
                let c = community(&host, &a, "gpui", &[&b]);
                clear_session(&a);
                clear_session(&b);
                (host, a, b, c)
            })
            .await
            .unwrap();
            let (alice, bob) = (start(&a).await, start(&b).await);
            let mut general = None;
            for client in [&alice, &bob] {
                general = wait_for(client, WAIT, "the channel list", |s| {
                    s.channel_named(CommunityId(c), "general").is_some()
                })
                .await
                .channel_named(CommunityId(c), "general")
                .map(|ch| ch.id);
            }
            (host, alice, bob, general.unwrap())
        });
        World {
            _rt: rt,
            _host: host,
            alice: Rc::new(alice),
            bob: Rc::new(bob),
            general,
        }
    }

    fn file(name: &str) -> NewFile {
        NewFile::from_bytes(name, "application/octet-stream", vec![7u8; 100])
    }

    /// Review M2: alice's file attached in her window never shows in bob's tray of the same channel, and a release of
    /// bob lets go of his tray only.
    #[gpui::test]
    #[ignore = "needs dilla-testhost and the WASI core"]
    fn two_accounts_in_one_channel_keep_their_own_trays(cx: &mut TestAppContext) {
        let w = world("t28-trays");
        let channel = w.general;
        cx.update(|cx| {
            ui::attach(
                Target {
                    client: Some(w.alice.clone()),
                    channel: Some(channel),
                    window: None,
                },
                vec![file("alice.bin")],
                vec![None],
                cx,
            );
            let alices = ui::tray_of(&w.alice, channel, cx);
            let bobs = ui::tray_of(&w.bob, channel, cx);
            assert_eq!(alices.entries().len(), 1, "alice's file is in her tray");
            assert!(bobs.entries().is_empty(), "bob's tray is his own");
            ui::release(&w.bob, cx);
            assert_eq!(
                ui::tray_of(&w.alice, channel, cx).entries().len(),
                1,
                "a release of bob keeps alice's tray"
            );
        });
        drop(w);
    }

    /// Review M4 (the security half of each window's target): with two clients in two windows on the same channel, a
    /// paste or a drop in each window goes through that window's own client.
    #[gpui::test]
    #[ignore = "needs dilla-testhost and the WASI core"]
    fn each_window_attaches_through_its_own_client(cx: &mut TestAppContext) {
        let w = world("t28-targets");
        let channel = w.general;
        let a = cx.add_window(|_, _| Blank);
        let b = cx.add_window(|_, _| Blank);
        a.update(cx, |_, win, cx| {
            ui::track(win, Some(w.alice.clone()), Some(channel), cx)
        })
        .unwrap();
        b.update(cx, |_, win, cx| {
            ui::track(win, Some(w.bob.clone()), Some(channel), cx)
        })
        .unwrap();
        let in_a = a.update(cx, |_, win, cx| ui::target(win, cx)).unwrap();
        let in_b = b.update(cx, |_, win, cx| ui::target(win, cx)).unwrap();
        assert!(
            in_a.client
                .as_ref()
                .is_some_and(|c| Rc::ptr_eq(c, &w.alice)),
            "A's paste goes through alice's client"
        );
        assert!(
            in_b.client.as_ref().is_some_and(|c| Rc::ptr_eq(c, &w.bob)),
            "B's paste goes through bob's client"
        );
        assert_eq!((in_a.channel, in_b.channel), (Some(channel), Some(channel)));
        // A drop in B lands in bob's tray only.
        cx.update(|cx| {
            ui::attach(in_b, vec![file("bob.bin")], vec![None], cx);
            assert_eq!(ui::tray_of(&w.bob, channel, cx).entries().len(), 1);
            assert!(ui::tray_of(&w.alice, channel, cx).entries().is_empty());
        });
        drop(w);
    }
}
