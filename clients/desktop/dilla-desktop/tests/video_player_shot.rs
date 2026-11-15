//! The parity shot of the inline video player (parity plan Package 12): the ready video drawn headless at scale 2,
//! paused at 0:03 of testdata/video-shot.webm (decoded by the app's own path), against Chromium's `<video controls>`
//! in the same 480 x 270 frame (parity/references/linux/attachments/video-player-ready-2x.png, shot by the script
//! beside it). It writes the GPUI shot and a difference image to `target/parity-video/` for review.
//!
//! Run: `cargo test -p dilla-desktop --features automation --test video_player_shot`.
#![cfg(feature = "automation")]

use std::rc::Rc;
use std::sync::Arc;
use std::time::{Duration, Instant};

use dilla_desktop::screens::attachments::video::{self, VideoPlayback};
use dilla_desktop_ui::components::attachments::video::{MenuView, PipMode, VideoView, video_frame};
use gpui::{
    AppContext, Bounds, Context, HeadlessAppContext, IntoElement, ParentElement, Render, Styled,
    Window, div, px, rgb,
};
use zeroize::Zeroizing;

struct Shot {
    view: VideoView,
}

impl Render for Shot {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        div()
            .size_full()
            .bg(rgb(0x111111))
            .p(px(20.0))
            .child(video_frame(self.view.clone(), 480.0, 270.0))
    }
}

fn reference(name: &str) -> image::RgbaImage {
    let p = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/references/linux/attachments")
        .join(name);
    image::open(p).expect("the reference").to_rgba8()
}

/// The picture at 0:03, decoded as the card decodes it.
fn picture_at_three_seconds() -> (VideoPlayback, Duration) {
    let bytes = include_bytes!("../src/screens/attachments/testdata/video-shot.webm");
    let opened = video::open(Arc::new(Zeroizing::new(bytes.to_vec())), "video/webm").unwrap();
    let (mut p, out) = VideoPlayback::start_detached(opened);
    if let Some(out) = out {
        std::thread::spawn(move || {
            for _ in out {
                std::thread::sleep(Duration::from_micros(5));
            }
        });
    }
    p.pause();
    p.seek(Duration::from_secs(3));
    let start = Instant::now();
    while p.frame().pts != Duration::from_secs(3) {
        assert!(start.elapsed() < Duration::from_secs(10), "the seek lands");
        let _ = p.tick();
        std::thread::sleep(Duration::from_millis(5));
    }
    let d = p.duration().unwrap();
    (p, d)
}

fn view(p: &VideoPlayback, duration: Duration, menu_open: bool) -> VideoView {
    let f = p.frame().clone();
    let noop = Rc::new(|_: &gpui::ClickEvent, _: &mut Window, _: &mut gpui::App| {});
    let frac = Rc::new(|_: f32, _: &mut Window, _: &mut gpui::App| {});
    VideoView {
        id: "shot".into(),
        image: f.image.clone(),
        picture: (f.width, f.height),
        playing: false,
        position: Duration::from_secs(3),
        duration: Some(duration),
        volume: 1.0,
        muted: false,
        sound: true,
        controls: true,
        fullscreen: false,
        label: "video-shot.webm".into(),
        play_label: "play".into(),
        pause_label: "pause".into(),
        mute_label: "mute".into(),
        unmute_label: "unmute".into(),
        fullscreen_label: "enter full screen".into(),
        exit_fullscreen_label: "exit full screen".into(),
        on_toggle: noop.clone(),
        on_mute: noop.clone(),
        on_fullscreen: noop,
        on_seek: frac.clone(),
        on_volume: frac,
        on_motion: Rc::new(|_, _| {}),
        menu: MenuView {
            open: menu_open,
            // The ⋮ button's centre, where a click opens it.
            at: gpui::point(px(20.0 + 456.0), px(20.0 + 222.0)),
            speed_page: false,
            speed: 1.0,
            more_label: "show more media controls".into(),
            download_label: "Download".into(),
            speed_label: "Playback speed".into(),
            normal_label: "Normal".into(),
            pip_label: "Picture in picture".into(),
            on_open: Rc::new(|_, _, _, _| {}),
            on_pick: Rc::new(|_, _, _| {}),
        },
        pip: PipMode::Inline,
    }
}

/// The view drawn headless at scale 2 in a 520 x 310 window on the reference page's background.
fn shoot(view: VideoView) -> image::RgbaImage {
    let platform = gpui_platform::current_platform(true);
    let mut cx = HeadlessAppContext::with_platform(
        platform.text_system(),
        Arc::new(dilla_desktop_ui::theme::Assets),
        gpui_platform::current_headless_renderer,
    );
    let window = cx
        .update(|cx| {
            dilla_desktop::install(cx);
            let bounds = Bounds::centered(None, gpui::size(px(520.), px(310.)), cx);
            cx.open_window(dilla_desktop::main_window_options(bounds), |_, cx| {
                cx.new(|_| Shot { view })
            })
        })
        .unwrap();
    cx.run_until_parked();
    cx.capture_screenshot(window.into()).expect("a shot")
}

/// Writes the shot and its difference from `reference` to `target/parity-video/<name>`, and answers the share of
/// pixels whose largest channel gap is over 48.
fn compare(shot: &image::RgbaImage, reference: &image::RgbaImage, name: &str) -> f64 {
    assert_eq!(shot.dimensions(), reference.dimensions());
    let (w, h) = shot.dimensions();
    let mut diff = image::RgbaImage::new(w, h);
    let mut over = 0u64;
    for (x, y, a) in shot.enumerate_pixels() {
        let b = reference.get_pixel(x, y);
        let d = (0..3).map(|i| a[i].abs_diff(b[i])).max().unwrap_or(0);
        if d > 48 {
            over += 1;
        }
        diff.put_pixel(x, y, image::Rgba([d, d, d, 255]));
    }
    let out = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("../../../target/parity-video");
    std::fs::create_dir_all(&out).unwrap();
    shot.save(out.join(format!("gpui-{name}-2x.png"))).unwrap();
    diff.save(out.join(format!("diff-{name}-2x.png"))).unwrap();
    let share = over as f64 / f64::from(w * h);
    println!(
        "video player {name}: {:.2} % of pixels differ by more than 48",
        share * 100.0
    );
    share
}

#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "the reference is Linux only")]
fn the_ready_video_matches_chromium_s_player() {
    let (p, duration) = picture_at_three_seconds();
    let shot = shoot(view(&p, duration, false));
    let share = compare(&shot, &reference("video-player-ready-2x.png"), "ready");
    // The picture is the same frame of the same file, scaled by another filter; the controls are drawn in the app's
    // font and Lucide glyphs (Jonas 2026-11-15: Signal style). A loose bound that still fails a missing picture, a
    // missing shade or controls in the wrong place.
    assert!(share < 0.08, "{:.2} % differ", share * 100.0);
}

/// The ⋮ menu open, against Chromium's overflow menu opened from the same button: the same three items in the app's
/// own menu (Signal style), so the menu's own box differs by design; everything around it matches as above.
#[test]
#[cfg_attr(not(target_os = "linux"), ignore = "the reference is Linux only")]
fn the_more_menu_opens_with_chromium_s_three_items() {
    let (p, duration) = picture_at_three_seconds();
    let closed = shoot(view(&p, duration, false));
    let open = shoot(view(&p, duration, true));
    let share = compare(&open, &reference("video-player-menu-2x.png"), "menu");
    // The menu is drawn: a sizeable box differs from the closed player.
    let changed = open
        .pixels()
        .zip(closed.pixels())
        .filter(|(a, b)| (0..3).any(|i| a[i].abs_diff(b[i]) > 48))
        .count();
    assert!(
        changed > 20_000,
        "the menu is drawn ({changed} pixels changed)"
    );
    // 14.5 % on the reference machine: nearly all of it the menu box itself (Chromium's is white, the app's dark).
    assert!(share < 0.2, "{:.2} % differ", share * 100.0);
}
