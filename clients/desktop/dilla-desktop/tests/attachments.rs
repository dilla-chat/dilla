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
    ($($test:ident => $name:literal $(, macos $why:literal)?);* $(;)?) => {$(
        #[test]
        #[cfg_attr(not(any(target_os = "linux", target_os = "macos")), ignore = "references are Linux and macOS only")]
        $(#[cfg_attr(target_os = "macos", ignore = $why)])?  // elsewhere the line above already ignores it
        fn $test() {
            if let Err(e) = assert_screen(&fixture($name)) {
                panic!("{e}");
            }
        }
    )*};
}

parity! {
    parity_attachments_cards => "cards", macos "no macOS limit measured yet for the cards screens (Linux: attachments/cards in tolerance.toml [linux_screens])";
    parity_attachments_cards_b => "cards-b", macos "no macOS limit measured yet for the cards screens (Linux: attachments/cards-b in tolerance.toml [linux_screens])";
    parity_attachments_lightbox => "lightbox";
    parity_attachments_lightbox_b => "lightbox-b";
    parity_attachments_media_player => "media-player";
    parity_attachments_media_player_b => "media-player-b";
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

    /// e2e/voice/media-attachments.spec.ts, "bob plays alice's webm … inline" (parity plan Package 12): a real VP8 and
    /// Opus WebM goes through a real dillad, bob's copy plays in its card through the app's own path (libwebrtc's VP8
    /// decoder, Opus through rodio): its pictures advance with the clock, its sound plays and pause holds.
    #[tokio::test(flavor = "multi_thread")]
    #[ignore = "needs dilla-testhost and the WASI core"]
    async fn webm_plays_inline() {
        use dilla_desktop::screens::attachments::video::{self, VideoPlayback};

        let (_host, pa, pb, c) = two_people("p12-video").await;
        let (alice, bob) = (start(&pa).await, start(&pb).await);
        let channel = general(&alice, c).await;
        general(&bob, c).await;
        alice.open_channel(channel).await.unwrap().unwrap();
        bob.open_channel(channel).await.unwrap().unwrap();

        let clip =
            include_bytes!("../src/screens/attachments/testdata/video-vp8-opus.webm").to_vec();
        let tray = alice.attachments().tray(channel);
        tray.add(vec![NewFile::from_bytes(
            "clip.webm",
            "video/webm",
            clip.clone(),
        )])
        .await
        .unwrap();
        let d = tray.take_all().unwrap();
        alice
            .attachments()
            .send(channel, "the clip".into(), d, None)
            .await
            .unwrap()
            .unwrap();
        let s = wait_for(&bob, WAIT, "alice's clip", |s| {
            row(s, channel, "the clip").is_some_and(|r| r.attachments.len() == 1)
        })
        .await;
        let seq = row(&s, channel, "the clip").unwrap().seq.unwrap();
        let opened = bob
            .attachments()
            .open(channel, seq, 0, false)
            .await
            .unwrap()
            .unwrap();
        assert_eq!(opened.bytes.as_slice(), clip.as_slice());

        let video =
            video::open(Arc::new(opened.bytes), "video/webm").expect("the WebM plays inline");
        assert!(video.has_audio());
        assert_eq!((video.poster.width, video.poster.height), (96, 64));
        let (mut p, out) = VideoPlayback::start_detached(video);
        // The sound: the tone comes out of the detached player (CI has no device).
        let samples: Vec<f32> = out.expect("a sound track").take(24_000).collect();
        let rms = (samples.iter().map(|x| x * x).sum::<f32>() / samples.len() as f32).sqrt();
        assert!(rms > 0.05, "the tone plays (rms {rms})");
        // The pictures follow the clock.
        let start = std::time::Instant::now();
        while p.frame().pts < Duration::from_millis(500) {
            assert!(
                start.elapsed() < Duration::from_secs(10),
                "the pictures advance"
            );
            let _ = p.tick();
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        p.pause();
        let held = p.position();
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert_eq!(p.position(), held, "pause holds the clock");
    }

    /// The former Electron shell (tag desktop-v0.1.0-beta.5, packages/desktop/test/electron/shell.spec.ts), "downloads a decrypted local attachment while refusing blob document
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
        cx.update(|cx| ui::save(&bob, key, "the file.txt", cx));
        // The save dialog asks (before the file is opened); nothing is written before the person answers it.
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
    use dilla_client::state::{AppState, ChannelId, CommunityId};
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

    /// The test GIF (two 1x1 frames).
    fn gif_bytes() -> Vec<u8> {
        include_bytes!("../src/screens/attachments/testdata/two-frames.gif").to_vec()
    }

    /// Runs the UI's (test) executor, whose timers move only with its clock, while the client's runtime works, until
    /// `done` holds for alice's state.
    fn pump(cx: &mut TestAppContext, w: &World, what: &str, done: impl Fn(&AppState) -> bool) {
        let start = std::time::Instant::now();
        while !done(&w.alice.state().borrow()) {
            assert!(start.elapsed() < WAIT, "{what}");
            cx.executor().advance_clock(Duration::from_millis(50));
            cx.run_until_parked();
            std::thread::sleep(Duration::from_millis(20));
        }
    }

    /// The channel's stored messages that carry files, as (body, the files' names).
    fn posts(s: &AppState, channel: ChannelId) -> Vec<(String, Vec<String>)> {
        s.timelines.get(&channel).map_or_else(Vec::new, |t| {
            t.items
                .iter()
                .filter(|i| !i.attachments.is_empty() && i.seq.is_some())
                .map(|i| {
                    (
                        i.body.clone(),
                        i.attachments.iter().map(|a| a.name.clone()).collect(),
                    )
                })
                .collect()
        })
    }

    /// A world with alice's channel open (the picker is drawn in an open conversation) and `draft.bin` ready in her
    /// composer's tray.
    fn with_a_file_in_the_tray(cx: &mut TestAppContext, tag: &'static str) -> (World, Target) {
        // The client's runtime answers from its own thread.
        cx.executor().allow_parking();
        let w = world(tag);
        let channel = w.general;
        w._rt
            .block_on(w.alice.open_channel(channel))
            .unwrap()
            .unwrap();
        let to = Target {
            client: Some(w.alice.clone()),
            channel: Some(channel),
            window: None,
        };
        cx.update(|cx| ui::attach(to.clone(), vec![file("draft.bin")], vec![None], cx));
        let start = std::time::Instant::now();
        while !cx.update(|cx| ui::tray_of(&w.alice, channel, cx).all_ready()) {
            assert!(start.elapsed() < WAIT, "the file is ready");
            cx.run_until_parked();
            std::thread::sleep(Duration::from_millis(20));
        }
        (w, to)
    }

    /// Task 11 ("Click sends at once", Jonas 2026-11-26): a pick sends the GIF at once, alone, as its own message with no
    /// text, and a second pick while the first is fetched is ignored (one fetch, one message); the file in the composer's
    /// tray stays there, ready, and the tray never holds the GIF (the draft's text is the message box's and the pick
    /// never touches it). The fetch is given (no Giphy in tests): `pick_gif` fetches the same way through the instance.
    #[gpui::test]
    #[ignore = "needs dilla-testhost and the WASI core"]
    fn a_picked_gif_is_sent_alone_at_once(cx: &mut TestAppContext) {
        use dilla_client::features::gifs::GifItem;
        use dilla_client::state::CommunityId;
        use dilla_desktop::screens::attachments::ui::GifState;

        let (w, _) = with_a_file_in_the_tray(cx, "t11-gifsend");
        let channel = w.general;
        cx.update(|cx| {
            ui::ui(cx).gif = Some(GifState {
                channel: Some(channel),
                community: Some(CommunityId([1; 16])),
                items: vec![GifItem {
                    id: "abc".into(),
                    title: "cat".into(),
                    width: 1,
                    height: 1,
                }],
                client: Some(w.alice.clone()),
                ..GifState::default()
            });
        });
        let fetches = std::rc::Rc::new(std::cell::Cell::new(0));
        for _ in 0..2 {
            let fetches = fetches.clone();
            cx.update(|cx| {
                ui::pick_gif_with(
                    0,
                    move |_, _, _| {
                        fetches.set(fetches.get() + 1);
                        let (tx, rx) = tokio::sync::oneshot::channel();
                        let _ = tx.send(Ok(gif_bytes()));
                        rx
                    },
                    cx,
                )
            });
        }
        assert_eq!(fetches.get(), 1, "the second pick is ignored");
        pump(cx, &w, "the GIF's message", |s| {
            !posts(s, channel).is_empty()
        });
        // Long enough for a second message to have come, had there been one.
        for _ in 0..50 {
            cx.executor().advance_clock(Duration::from_millis(50));
            cx.run_until_parked();
            std::thread::sleep(Duration::from_millis(20));
        }
        let posted = posts(&w.alice.state().borrow(), channel);
        assert_eq!(
            posted,
            [(String::new(), vec!["cat.gif".to_owned()])],
            "the GIF alone, once"
        );
        assert!(
            cx.read(|cx| cx.global::<ui::AttachmentsUi>().gif.is_none()),
            "the picker closed"
        );
        cx.update(|cx| {
            let left = ui::tray_of(&w.alice, channel, cx).entries();
            assert_eq!(left.len(), 1, "the other file stays in the tray");
            assert_eq!(left[0].name, "draft.bin");
            assert!(ui::tray_of(&w.alice, channel, cx).all_ready());
        });
        drop(w);
    }

    /// Reviewer I1: Enter on the draft while a picked GIF uploads sends the draft at once (not "not ready"), with the
    /// tray's own file and without the GIF, which then goes as its own message.
    #[gpui::test]
    #[ignore = "needs dilla-testhost and the WASI core"]
    fn enter_during_a_gif_upload_sends_the_draft_alone(cx: &mut TestAppContext) {
        let (w, to) = with_a_file_in_the_tray(cx, "t11-gifenter");
        let channel = w.general;
        cx.update(|cx| ui::send_gif(to.clone(), "cat.gif".to_owned(), gif_bytes(), cx));
        // The composer's send, as `Shell::send` does it, at once.
        let files = cx
            .update(|cx| ui::take_for_send(&w.alice, channel, cx))
            .expect("the draft's files are ready");
        assert_eq!(files.len(), 1, "only the tray's own file");
        let rx = w
            .alice
            .attachments()
            .send(channel, "the draft".into(), files, None);
        pump(cx, &w, "both messages", |s| posts(s, channel).len() == 2);
        let mut posted = posts(&w.alice.state().borrow(), channel);
        posted.sort();
        assert_eq!(
            posted,
            [
                (String::new(), vec!["cat.gif".to_owned()]),
                ("the draft".to_owned(), vec!["draft.bin".to_owned()]),
            ]
        );
        drop(rx);
        drop(w);
    }

    /// A GIF that could not be sent goes into the composer's tray, so nothing is lost (here: a channel this account
    /// has no group of, which refuses the upload).
    #[gpui::test]
    #[ignore = "needs dilla-testhost and the WASI core"]
    fn a_gif_not_sent_goes_into_the_tray(cx: &mut TestAppContext) {
        cx.executor().allow_parking();
        let w = world("t11-giffail");
        let nowhere = ChannelId([9; 16]);
        let to = Target {
            client: Some(w.alice.clone()),
            channel: Some(nowhere),
            window: None,
        };
        cx.update(|cx| ui::send_gif(to, "cat.gif".to_owned(), gif_bytes(), cx));
        let start = std::time::Instant::now();
        while cx.update(|cx| ui::tray_of(&w.alice, nowhere, cx).entries().is_empty()) {
            assert!(start.elapsed() < WAIT, "the GIF went into the tray");
            cx.executor().advance_clock(Duration::from_millis(50));
            cx.run_until_parked();
            std::thread::sleep(Duration::from_millis(20));
        }
        let names: Vec<String> = cx.update(|cx| {
            ui::tray_of(&w.alice, nowhere, cx)
                .entries()
                .into_iter()
                .map(|e| e.name)
                .collect()
        });
        assert_eq!(names, ["cat.gif"]);
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

/// Final review I5: the DM GIF notice belongs to the window it asks in, and a release of its client (Forget, Sign out,
/// an instance's Remove) drops it, so the store can close; an answer that comes back after the release shows nothing.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn the_dm_gif_notice_is_per_window_and_goes_with_its_client(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;

    use dilla_client::state::{ChannelId, CommunityId};
    use dilla_client::testing::host as world;
    use dilla_desktop::screens::attachments::ui::{self, DmNotice};

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("i5-notice", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let alice = rt.block_on(world::sign_up(&host, &invite, "alice", "Alice", "i5"));
    let client = Rc::new(rt.block_on(world::start(&alice)));
    cx.update(dilla_desktop::install);
    let one = cx
        .add_empty_window()
        .update(|window, _| window.window_handle().window_id());
    let two = cx
        .add_empty_window()
        .update(|window, _| window.window_handle().window_id());
    let notice = |owner| DmNotice {
        channel: ChannelId([3; 16]),
        community: CommunityId([1; 16]),
        server: "Midgard".into(),
        client: client.clone(),
        owner,
    };
    let (a, b) = (notice(one), notice(two));
    cx.update(|cx| ui::show_dm_notice(a, cx));
    assert!(cx.update(|cx| ui::dm_notice(one, cx)).is_some());
    assert!(
        cx.update(|cx| ui::dm_notice(two, cx)).is_none(),
        "only the window it asked in shows it"
    );
    // The other window's own notice does not replace or close the first.
    cx.update(|cx| ui::show_dm_notice(b, cx));
    assert!(cx.update(|cx| ui::dm_notice(one, cx)).is_some());
    cx.update(|cx| ui::dismiss_dm_notice(two, cx));
    assert!(cx.update(|cx| ui::dm_notice(one, cx)).is_some());
    assert!(cx.update(|cx| ui::dm_notice(two, cx)).is_none());

    cx.update(|cx| dilla_desktop::release::release_client(&client, cx));
    assert!(
        cx.update(|cx| ui::dm_notice(one, cx)).is_none(),
        "the notice went"
    );
    // A notice whose read answered after the release is not shown.
    let late = notice(one);
    cx.update(|cx| ui::show_dm_notice(late, cx));
    assert!(cx.update(|cx| ui::dm_notice(one, cx)).is_none());
    assert_eq!(
        Rc::strong_count(&client),
        1,
        "nothing but the caller holds the client"
    );
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}

/// Re-review N2: a DM GIF notice whose read answers after its window closed is not kept anywhere (not in the closed
/// window's spare slot either), so it holds no client: a later release of that client finds nothing holding it.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn a_dm_gif_notice_for_a_closed_window_holds_no_client(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;

    use dilla_client::state::{ChannelId, CommunityId};
    use dilla_client::testing::host as world;
    use dilla_desktop::screens::attachments::ui::{self, DmNotice};

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("n2-notice", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let alice = rt.block_on(world::sign_up(&host, &invite, "alice", "Alice", "n2"));
    let client = Rc::new(rt.block_on(world::start(&alice)));
    cx.update(dilla_desktop::install);
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, _| window.window_handle().window_id());
    // The window closes (the close hook's fan-out) while the notice's read is out.
    vcx.update(|window, cx| {
        dilla_desktop::windows::on_closed(cx, window.window_handle().window_id());
        window.remove_window();
    });
    cx.run_until_parked();
    let late = DmNotice {
        channel: ChannelId([3; 16]),
        community: CommunityId([1; 16]),
        server: "Midgard".into(),
        client: client.clone(),
        owner: window,
    };
    cx.update(|cx| ui::show_dm_notice(late, cx));
    assert!(cx.update(|cx| ui::dm_notice(window, cx)).is_none());
    assert_eq!(
        Rc::strong_count(&client),
        1,
        "nothing holds the client for a closed window"
    );
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}
