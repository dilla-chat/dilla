//! The fixtures (testdata/video-*, made by testdata/make-video-fixtures.sh) are 2 s of 96x64 at 10 fps whose luma
//! steps by 10 every frame (16 + 100 t), so a picture's grey tells which frame it is: frame k is about 11.64 k after the
//! limited-range conversion.

use super::*;

const VP8: &[u8] = include_bytes!("../testdata/video-vp8-opus.webm");
const VP9: &[u8] = include_bytes!("../testdata/video-vp9.webm");
const AV1_WEBM: &[u8] = include_bytes!("../testdata/video-av1.webm");
const AV1_MP4: &[u8] = include_bytes!("../testdata/video-av1.mp4");
const H264: &[u8] = include_bytes!("../testdata/video-h264-aac.mp4");
const HEVC: &[u8] = include_bytes!("../testdata/video-hevc.mp4");
const THEORA: &[u8] = include_bytes!("../testdata/video-theora.ogv");
const ROT90: &[u8] = include_bytes!("../testdata/video-h264-rot90.mp4");

fn bytes(b: &[u8]) -> Arc<Zeroizing<Vec<u8>>> {
    Arc::new(Zeroizing::new(b.to_vec()))
}

/// The grey at the picture's centre (its blue byte).
fn grey(f: &Frame) -> u8 {
    let b = f.image.as_bytes(0).expect("one frame");
    let (x, y) = (f.width as usize / 2, f.height as usize / 2);
    b[(y * f.width as usize + x) * 4]
}

/// The frame number a picture's grey says, and the one its time says.
fn frame_numbers(f: &Frame) -> (f32, f32) {
    (f32::from(grey(f)) / 11.64, f.pts.as_secs_f32() * 10.0)
}

/// Every picture of `file`, decoded from the start, in the order the decoder hands them out.
fn decode_all(file: &[u8], mime: &str) -> Vec<Frame> {
    let opened = open(bytes(file), mime).expect("opens");
    let track = opened.track.clone();
    let mut f = Feeder::new(&track).expect("a decoder");
    (0..track.samples.len())
        .flat_map(|i| f.feed(&track, i))
        .collect()
}

const INLINE: [(&[u8], &str, bool); 5] = [
    (VP8, "video/webm", true),
    (VP9, "video/webm", false),
    (AV1_WEBM, "video/webm", false),
    (AV1_MP4, "video/mp4", false),
    (H264, "video/mp4", true),
];

/// Whether this build decodes `codec`: H.264 everywhere but macOS (whose libwebrtc FFmpeg lacks it; VideoToolbox is
/// behind webrtc-sys's `dilla-video-toolbox`, off), H.265 only with that feature.
fn decodes(codec: webrtc_sys::dilla_video_decoder::VideoCodec) -> bool {
    webrtc_sys::dilla_video_decoder::VideoDecoder::new(codec, 2, 2).is_some()
}

fn h264_here() -> bool {
    let here = decodes(webrtc_sys::dilla_video_decoder::VideoCodec::H264);
    if !cfg!(target_os = "macos") {
        assert!(
            here,
            "libwebrtc's FFmpeg decodes H.264 on Linux and Windows"
        );
    }
    here
}

/// The inline formats this build plays: [`INLINE`] less H.264 where it has no decoder.
fn inline() -> Vec<(&'static [u8], &'static str, bool)> {
    let h264 = h264_here();
    INLINE
        .into_iter()
        .filter(|(f, ..)| h264 || !std::ptr::eq(*f, H264))
        .collect()
}

/// The child process of [`every_inline_format_decodes_its_frames_in_presentation_order`] decodes the format this names.
const CHILD: &str = "DILLA_VIDEO_TEST_FORMAT";
const THIS_TEST: &str = "screens::attachments::video::tests::every_inline_format_decodes_its_frames_in_presentation_order";

/// Each codec the web played inline decodes here, every picture is the frame its time names (H.264's B-frames come
/// back in presentation order), and the poster is the first frame. Each format decodes in a child process of this test
/// binary, so a crash inside a native decoder (macOS CI's VideoToolbox SIGSEGV) fails this test with the format and the
/// signal named instead of taking the whole binary down.
#[test]
fn every_inline_format_decodes_its_frames_in_presentation_order() {
    if let Ok(i) = std::env::var(CHILD) {
        let (file, mime, sound) = inline()[i.parse::<usize>().expect("an index")];
        decodes_in_order(file, mime, sound);
        return;
    }
    for (i, (_, mime, _)) in inline().into_iter().enumerate() {
        let out = std::process::Command::new(std::env::current_exe().expect("the test binary"))
            .args([THIS_TEST, "--exact", "--nocapture", "--test-threads", "1"])
            .env(CHILD, i.to_string())
            .output()
            .expect("the child runs");
        let how = match out.status.code() {
            Some(c) => format!("exit code {c}"),
            None => {
                #[cfg(unix)]
                {
                    use std::os::unix::process::ExitStatusExt;
                    format!("signal {:?} (a crash in native code)", out.status.signal())
                }
                #[cfg(not(unix))]
                {
                    "no exit code".to_owned()
                }
            }
        };
        assert!(
            out.status.success(),
            "format #{i} ({mime}) failed: {how}\n{}",
            String::from_utf8_lossy(&out.stdout)
        );
    }
}

fn decodes_in_order(file: &[u8], mime: &str, sound: bool) {
    {
        let opened = open(bytes(file), mime).expect(mime);
        assert_eq!(opened.has_audio(), sound, "{mime} sound");
        assert_eq!((opened.poster.width, opened.poster.height), (96, 64));
        assert_eq!(opened.poster.pts, Duration::ZERO);
        assert!(grey(&opened.poster) < 6, "the poster is frame 0");
        let d = opened.duration().expect("a duration");
        assert!((d.as_secs_f32() - 2.0).abs() < 0.15, "{mime}: {d:?}");
        let frames = decode_all(file, mime);
        // FFmpeg keeps its last reordered pictures (two B-frames deep) when no flush comes.
        assert!(frames.len() >= 18, "{mime}: {} pictures", frames.len());
        for f in &frames {
            let (by_grey, by_time) = frame_numbers(f);
            assert!(
                (by_grey - by_time).abs() < 0.5,
                "{mime}: the picture at {:?} is frame {by_grey}",
                f.pts
            );
        }
        let mut times: Vec<_> = frames.iter().map(|f| f.pts).collect();
        times.dedup();
        assert_eq!(times.len(), frames.len(), "{mime}: one picture per time");
    }
}

/// A codec or container this cannot play is Unsupported, so the card falls back to the system player: Theora (Chromium
/// dropped it too), H.265 (no VideoToolbox by default), H.264 where the build has no decoder (macOS), a type the
/// bytes do not match.
#[test]
fn what_cannot_play_inline_is_unsupported() {
    let unsupported =
        |b: &[u8], mime: &str| matches!(open(bytes(b), mime), Err(MediaError::Unsupported(_)));
    assert!(unsupported(THEORA, "video/ogg"));
    if !decodes(webrtc_sys::dilla_video_decoder::VideoCodec::H265) {
        assert!(unsupported(HEVC, "video/mp4"));
    }
    if !h264_here() {
        assert!(unsupported(H264, "video/mp4"), "macOS: the system player");
        assert!(unsupported(ROT90, "video/mp4"));
    }
    assert!(unsupported(VP8, "video/mp4"), "a WebM typed as MP4");
    assert!(unsupported(H264, "video/webm"), "an MP4 typed as WebM");
    assert!(unsupported(H264, "application/octet-stream"));
}

/// A phone's portrait clip is shown turned, as the track header says (90 degrees anticlockwise here: the source's
/// right half, white, ends up on top).
#[test]
fn a_rotated_track_is_shown_turned() {
    if !h264_here() {
        return;
    }
    let opened = open(bytes(ROT90), "video/mp4").expect("opens");
    let f = &opened.poster;
    assert_eq!((f.width, f.height), (64, 96));
    let b = f.image.as_bytes(0).unwrap();
    let at = |x: usize, y: usize| b[(y * 64 + x) * 4];
    assert!(at(32, 10) > 230, "top {}", at(32, 10));
    assert!(at(32, 85) < 25, "bottom {}", at(32, 85));
}

/// Starts `video` on a detached player whose sound a thread pulls, as a device would (rodio's seek waits for the
/// output to take it).
fn playing(video: OpenedVideo) -> VideoPlayback {
    let (p, out) = VideoPlayback::start_detached(video);
    if let Some(out) = out {
        std::thread::spawn(move || {
            for _ in out {
                std::thread::sleep(Duration::from_micros(5));
            }
        });
    }
    p
}

fn wait_for(mut done: impl FnMut() -> bool) {
    let start = Instant::now();
    while !done() {
        assert!(start.elapsed() < Duration::from_secs(10), "timed out");
        std::thread::sleep(Duration::from_millis(5));
    }
}

/// Play, pause, seek: the clock moves only while playing, and a seek shows the frame at its target (from the key frame
/// before it), in every inline format.
#[test]
fn a_seek_shows_the_frame_at_its_target_and_pause_holds_the_clock() {
    for (file, mime, _) in inline() {
        let mut p = playing(open(bytes(file), mime).unwrap());
        p.pause();
        let at = p.position();
        std::thread::sleep(Duration::from_millis(60));
        assert_eq!(p.position(), at, "{mime}: paused");
        // 1.25 s is between frames 12 and 13, after the key frame at 1.0 s: frame 12 shows.
        p.seek(Duration::from_millis(1250));
        wait_for(|| {
            let _ = p.tick();
            p.frame().pts == Duration::from_millis(1200)
        });
        // Paused: nothing later is due, so the decoded pictures ahead wait in the queue.
        std::thread::sleep(Duration::from_millis(50));
        let _ = p.tick();
        assert_eq!(p.frame().pts, Duration::from_millis(1200), "{mime}");
        assert!(
            p.queued().0 <= AHEAD,
            "{mime}: the thread stays a few pictures ahead"
        );
        let (by_grey, _) = frame_numbers(p.frame());
        assert!((by_grey - 12.0).abs() < 0.5, "{mime}: frame {by_grey}");
        // Back to the start.
        p.seek(Duration::ZERO);
        wait_for(|| {
            let _ = p.tick();
            p.frame().pts.is_zero()
        });
        assert!(grey(p.frame()) < 6, "{mime}");
    }
}

#[test]
fn playing_advances_the_pictures_and_reaches_the_end() {
    let mut p = playing(open(bytes(VP9), "video/webm").unwrap());
    assert!(!p.is_paused());
    p.seek(Duration::from_millis(1700));
    wait_for(|| {
        let _ = p.tick();
        p.ended()
    });
    assert_eq!(p.position(), p.duration().unwrap());
    assert!(
        p.frame().pts >= Duration::from_millis(1700),
        "{:?}",
        p.frame().pts
    );
    // Play at the end starts again from the beginning.
    p.play();
    assert!(p.position() < Duration::from_millis(500));
}

#[test]
fn volume_and_mute_reach_the_sound() {
    let mut p = playing(open(bytes(VP8), "video/webm").unwrap());
    assert!(p.has_audio());
    p.set_volume(0.25);
    assert_eq!((p.volume(), p.muted()), (0.25, false));
    assert_eq!(p.audio.as_ref().unwrap().volume(), 0.25);
    p.toggle_mute();
    assert!(p.muted());
    assert_eq!(p.audio.as_ref().unwrap().volume(), 0.0);
    p.toggle_mute();
    assert_eq!(p.audio.as_ref().unwrap().volume(), 0.25);
    p.set_volume(0.0);
    assert!(p.muted());
}

/// Every truncation and corruption of every fixture, and garbage, opens or is Unsupported: never a panic or a hang;
/// whatever opens also plays to its end.
#[test]
fn truncated_and_garbage_video_never_panics() {
    let files: [(&[u8], &str); 7] = [
        (VP8, "video/webm"),
        (VP9, "video/webm"),
        (AV1_WEBM, "video/webm"),
        (AV1_MP4, "video/mp4"),
        (H264, "video/mp4"),
        (HEVC, "video/mp4"),
        (ROT90, "video/mp4"),
    ];
    let mut corpus: Vec<(Vec<u8>, &str)> = Vec::new();
    for (f, mime) in files {
        for cut in [
            0,
            4,
            12,
            40,
            100,
            300,
            f.len() / 3,
            f.len() / 2,
            f.len() - 1,
        ] {
            corpus.push((f[..cut.min(f.len())].to_vec(), mime));
        }
        let mut flipped = f.to_vec();
        for b in flipped.iter_mut().skip(f.len() / 3).take(f.len() / 3) {
            *b ^= 0x5a;
        }
        corpus.push((flipped, mime));
    }
    let mut seed = 0x9e37_79b9_u32;
    for len in [16, 512, 4096] {
        for (magic, mime) in [
            (&b"\0\0\0\x18ftypisom"[..], "video/mp4"),
            (&[0x1a, 0x45, 0xdf, 0xa3][..], "video/webm"),
        ] {
            let mut b = magic.to_vec();
            b.extend((0..len).map(|_| {
                seed ^= seed << 13;
                seed ^= seed >> 17;
                seed ^= seed << 5;
                seed as u8
            }));
            corpus.push((b, mime));
        }
    }
    for (i, (b, mime)) in corpus.into_iter().enumerate() {
        if let Ok(opened) = open(bytes(&b), mime) {
            let track = opened.track.clone();
            let mut f = Feeder::new(&track).expect("a decoder");
            for s in 0..track.samples.len() {
                let _ = f.feed(&track, s);
            }
            assert!(track.samples.len() <= demux::MAX_SAMPLES, "#{i}");
        }
    }
}

/// The ⋮ menu end to end through the card's state: Playback speed turns the page and the menu's own close that
/// follows the pick keeps it open; a speed sets the rate; Download runs the card's save; Picture in picture opens the
/// window and the card shows the notice; back closes the window; a released player closes it too.
#[gpui::test]
fn the_more_menu_sets_speed_downloads_and_moves_to_picture_in_picture(
    cx: &mut gpui::TestAppContext,
) {
    use super::super::ui::{self, CardKey, Player, VideoMenu, VideoState};
    let key = CardKey {
        client: 7,
        channel: dilla_client::state::ChannelId([3; 16]),
        seq: 1,
        index: 0,
    };
    let saved = std::rc::Rc::new(std::cell::Cell::new(0));
    cx.update(|cx| {
        let s = saved.clone();
        ui::ui(cx).players.insert(
            key,
            Player::Video(Box::new(VideoState {
                playback: playing(open(bytes(VP9), "video/webm").unwrap()),
                motion: None,
                menu: VideoMenu::default(),
                save: Some(std::rc::Rc::new(move |_| s.set(s.get() + 1))),
            })),
        );
        let menu = |cx: &mut gpui::App| match ui::ui(cx).players.get(&key) {
            Some(Player::Video(v)) => (v.menu.open, v.menu.speed_page, v.playback.speed()),
            _ => panic!("the player"),
        };
        ui::open_video_menu(key, true, gpui::Point::default(), cx);
        assert_eq!(menu(cx), (true, false, 1.0));
        // ActionMenu answers a pick with the pick, then its own close.
        ui::pick_video_menu(key, "speed", cx);
        ui::open_video_menu(key, false, gpui::Point::default(), cx);
        assert_eq!(menu(cx), (true, true, 1.0), "the speed page, still open");
        ui::pick_video_menu(key, "speed-1.5", cx);
        ui::open_video_menu(key, false, gpui::Point::default(), cx);
        assert_eq!(menu(cx), (false, false, 1.5));
        ui::pick_video_menu(key, "download", cx);
        assert_eq!(saved.get(), 1, "Download is the card's save");
        let windows = cx.windows().len();
        ui::pick_video_menu(key, "pip", cx);
        assert!(ui::ui(cx).pip.as_ref().is_some_and(|p| p.key == key));
        assert_eq!(cx.windows().len(), windows + 1, "its own window");
        ui::close_pip(cx);
        assert!(ui::ui(cx).pip.is_none());
        ui::open_pip(key, cx);
        ui::ui(cx).players.remove(&key);
    });
    cx.run_until_parked();
    // The window's view finds no player at its next draw and closes.
    cx.update(|cx| {
        if let Some(p) = ui::ui(cx).pip.as_ref() {
            let _ = p.window.update(cx, |_, w, _| w.refresh());
        }
    });
    cx.run_until_parked();
    cx.update(|cx| {
        assert!(
            ui::ui(cx).pip.is_none(),
            "a released player takes its window along"
        )
    });
}

/// The ⋮ menu's playback speed: the clock (and the sound) run at the chosen rate from where they were; a rate not in
/// Chromium's list is ignored.
#[test]
fn playback_speed_scales_the_clock() {
    let mut p = playing(open(bytes(VP8), "video/webm").unwrap());
    p.pause();
    p.seek(Duration::ZERO);
    p.set_speed(2.0);
    assert_eq!(p.speed(), 2.0);
    p.play();
    std::thread::sleep(Duration::from_millis(200));
    let at = p.position().as_secs_f32();
    assert!((0.35..0.6).contains(&at), "{at} s after 200 ms at 2x");
    p.set_speed(3.0);
    assert_eq!(p.speed(), 2.0, "not one of Chromium's speeds");
    p.set_speed(0.25);
    let from = p.position();
    std::thread::sleep(Duration::from_millis(200));
    let moved = (p.position() - from).as_secs_f32();
    assert!(
        (0.03..0.1).contains(&moved),
        "{moved} s after 200 ms at 0.25x"
    );
}
