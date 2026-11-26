//! Parity plan Package 1: a share's capture belongs to one window's call (beta.5 `screen-capture.spec.ts`).
//!
//! The windows run real instances (dillad and its SFU, as tests/voice.rs's call tests); the share's source is a
//! recording one ([`Recorder`]): it counts the sources opened and alive, so a test sees the capture start and stop
//! without a screen.

use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use dilla_client::calls::CallPhase;
use dilla_client::state::{AppState, ChannelId};
use dilla_desktop::screens::share_picker::{self, Found, Listing, apps};
use dilla_desktop::screens::voice::{self, ShareSources, TileKey};
use dilla_media::sources::desktop::{DesktopSource, SourceKind};
use dilla_media::{CaptureSource, Pull, ShareKind, ShareOptions, ShareResolution, TestPattern};
use gpui::{App, Context, IntoElement, ParentElement, Render, Styled, TestAppContext, Window, div};

#[path = "../../dilla-client/tests/support/call_world.rs"]
mod call_world;

#[path = "support/media.rs"]
mod media_slot;
use media_slot::{MediaSlot, media_slot};

const CALL_WAIT: Duration = Duration::from_secs(45);

/// Counts the share sources opened, and those alive (not yet dropped).
#[derive(Default)]
struct Recorder {
    opened: AtomicUsize,
    alive: Arc<AtomicUsize>,
    /// How many times the picker listed the sources.
    listed: Arc<AtomicUsize>,
    /// The cancel flag the picker handed its latest listing (review I2).
    cancel: Arc<std::sync::Mutex<Option<share_picker::Cancel>>>,
}

/// A moving test pattern that tells its [`Recorder`] when it goes.
struct Tracked {
    pattern: TestPattern,
    alive: Arc<AtomicUsize>,
}

impl Drop for Tracked {
    fn drop(&mut self) {
        self.alive.fetch_sub(1, Ordering::SeqCst);
    }
}

impl CaptureSource for Tracked {
    fn kind(&self) -> ShareKind {
        self.pattern.kind()
    }
    fn size(&self) -> (u32, u32) {
        self.pattern.size()
    }
    fn fps(&self) -> u32 {
        self.pattern.fps()
    }
    fn pull(&mut self) -> Pull {
        self.pattern.pull()
    }
}

impl Recorder {
    fn install(self: &Arc<Self>, cx: &mut TestAppContext) {
        let rec = self.clone();
        let listed = self.listed.clone();
        let cancel = self.cancel.clone();
        cx.update(|cx| {
            // The picker offers one screen and one window (no thumbnails: no display here).
            share_picker::set_lister(
                Arc::new(move |flag: share_picker::Cancel| {
                    listed.fetch_add(1, Ordering::SeqCst);
                    *cancel.lock().unwrap() = Some(flag);
                    vec![
                        Found {
                            source: DesktopSource {
                                kind: SourceKind::Screen,
                                id: 1,
                                title: "Display 1".to_owned(),
                                owner: None,
                            },
                            thumbnail: None,
                        },
                        Found {
                            source: DesktopSource {
                                kind: SourceKind::Window,
                                id: 7,
                                title: "Notes".to_owned(),
                                owner: None,
                            },
                            thumbnail: None,
                        },
                    ]
                    .into()
                }),
                cx,
            );
            cx.set_global(ShareSources(Arc::new(move |r| {
                rec.opened.fetch_add(1, Ordering::SeqCst);
                rec.alive.fetch_add(1, Ordering::SeqCst);
                Ok(Box::new(Tracked {
                    pattern: TestPattern::new(r.kind, 320, 180, 15),
                    alive: rec.alive.clone(),
                }) as Box<dyn CaptureSource>)
            })));
        });
    }

    fn alive(&self) -> usize {
        self.alive.load(Ordering::SeqCst)
    }

    fn opened(&self) -> usize {
        self.opened.load(Ordering::SeqCst)
    }

    fn listed(&self) -> usize {
        self.listed.load(Ordering::SeqCst)
    }

    /// Whether the latest listing was told to stop (its picker closed).
    fn listing_cancelled(&self) -> bool {
        self.cancel
            .lock()
            .unwrap()
            .as_ref()
            .expect("a listing ran")
            .load(Ordering::SeqCst)
    }
}

struct Host;

impl Render for Host {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        div()
    }
}

/// A window that draws its open picker over nothing, as the shell draws its overlays.
struct PickerHost;

impl Render for PickerHost {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .size_full()
            .children(share_picker::overlay(window, cx))
    }
}

struct World {
    rt: tokio::runtime::Runtime,
    _host: call_world::Host,
    alice: Rc<dilla_client::Client>,
    bob: Rc<dilla_client::Client>,
    voice: ChannelId,
    /// A second voice channel of the community (only [`world_with_two`]'s): where a move goes.
    upstairs: Option<ChannelId>,
    _slot: MediaSlot,
}

fn world(tag: &'static str) -> World {
    world_of(tag, false)
}

/// [`world`] with a second voice channel ([`World::upstairs`]).
fn world_with_two(tag: &'static str) -> World {
    world_of(tag, true)
}

fn world_of(tag: &'static str, two: bool) -> World {
    let slot = media_slot();
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let (host, alice, bob, voice, upstairs) = rt.block_on(async {
        let host = tokio::task::spawn_blocking(move || call_world::start_host_reachable(tag))
            .await
            .unwrap();
        let invite = host.invites.last().unwrap().clone();
        let alice = call_world::sign_up(&host, &invite, "alice", tag).await;
        let bob = call_world::sign_up(&host, &invite, "bob", tag).await;
        let (host, alice, bob, voice, upstairs) = tokio::task::spawn_blocking(move || {
            let (c, v) = call_world::voice_community(&host, &alice, &[&bob]);
            let upstairs = two.then(|| {
                let (u, _, _) = call_world::ds_of(&host, &alice)
                    .create_channel(&c, 1, "upstairs")
                    .unwrap();
                ChannelId(u)
            });
            call_world::clear_session(&alice);
            call_world::clear_session(&bob);
            (host, alice, bob, ChannelId(v), upstairs)
        })
        .await
        .unwrap();
        let a = call_world::start(&alice).await;
        call_world::connected(&a).await;
        a.voice().start();
        let b = call_world::start(&bob).await;
        call_world::connected(&b).await;
        b.voice().start();
        for c in [&a, &b] {
            until(c, "the voice channels are listed", |s| {
                s.channel(voice).is_some() && upstairs.is_none_or(|u| s.channel(u).is_some())
            })
            .await;
        }
        (host, a, b, voice, upstairs)
    });
    World {
        rt,
        _host: host,
        alice: Rc::new(alice),
        bob: Rc::new(bob),
        voice,
        upstairs,
        _slot: slot,
    }
}

async fn until(
    client: &dilla_client::Client,
    what: &str,
    pred: impl Fn(&AppState) -> bool,
) -> Arc<AppState> {
    let mut rx = client.state();
    let deadline = Instant::now() + CALL_WAIT;
    loop {
        let s = rx.borrow_and_update().clone();
        if pred(&s) {
            return s;
        }
        let left = deadline.saturating_duration_since(Instant::now());
        assert!(!left.is_zero(), "{what}: never; voice {:?}", s.voice);
        let _ = tokio::time::timeout(left.min(Duration::from_millis(500)), rx.changed()).await;
    }
}

/// Runs the UI until `pred` holds (the window's voice area follows its client's state on each sync).
fn wait(cx: &mut TestAppContext, what: &str, mut pred: impl FnMut() -> bool) {
    let deadline = Instant::now() + CALL_WAIT;
    while !pred() {
        cx.run_until_parked();
        assert!(Instant::now() < deadline, "{what}: never");
        std::thread::sleep(Duration::from_millis(50));
    }
}

/// Opens a window whose voice area follows `client`, as its shell does.
fn window_for(
    cx: &mut TestAppContext,
    client: &Rc<dilla_client::Client>,
) -> gpui::WindowHandle<Host> {
    let w = cx.add_window(|_, _| Host);
    let id = w.window_id();
    let state = client.state().borrow().clone();
    cx.update(|cx| voice::sync(id, state, Some(client), cx));
    w
}

/// Keeps `window`'s voice area on `client`'s newest state (the shell does it on every change).
fn resync(cx: &mut TestAppContext, w: gpui::WindowId, client: &Rc<dilla_client::Client>) {
    let state = client.state().borrow().clone();
    cx.update(|cx| voice::sync(w, state, Some(client), cx));
}

/// Runs the UI until `window`'s picker has listed the sources.
fn wait_listed(cx: &mut TestAppContext, window: gpui::WindowId) {
    let deadline = Instant::now() + CALL_WAIT;
    while !cx.read(|cx| share_picker::enumerated(window, cx)) {
        cx.run_until_parked();
        assert!(Instant::now() < deadline, "the picker never listed");
        std::thread::sleep(Duration::from_millis(20));
    }
}

/// The window becomes the focused one (the system's window activation).
fn focus(cx: &mut TestAppContext, w: &gpui::WindowHandle<Host>) {
    w.update(cx, |_, window, _| window.activate_window())
        .unwrap();
    cx.run_until_parked();
}

/// A press on the window's Share (the call bar's Go live, the DM box's Share).
fn press_share(cx: &mut TestAppContext, w: &gpui::WindowHandle<Host>) {
    focus(cx, w);
    w.update(cx, |_, window, cx| {
        voice::toggle_share(ShareKind::Screen, window, cx)
    })
    .unwrap();
    cx.run_until_parked();
}

/// Joins alice's window to the call and opens her call and starts a screen share there; answers once it runs.
fn alice_shares(cx: &mut TestAppContext, w: &World, rec: &Recorder) -> gpui::WindowHandle<Host> {
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    cx.update(|cx| voice::join(w.voice, wa, cx));
    cx.run_until_parked();
    w.rt.block_on(until(&w.alice, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    }));
    resync(cx, wa, &w.alice);
    press_share(cx, &a);
    wait_listed(cx, wa);
    assert_eq!(rec.opened(), 0, "nothing opens before a choice");
    assert!(cx.update(|cx| share_picker::share(wa, &"screen:1".into(), cx)));
    w.rt.block_on(until(&w.alice, "alice's share is on", |s| {
        s.voice.share.screen
    }));
    resync(cx, wa, &w.alice);
    wait(cx, "the capture runs", || rec.alive() == 1);
    a
}

/// Holds (or lets go) the window's media session before it takes up a call's media link (`voice::hold_media_pickup`).
fn hold_pickup(cx: &mut TestAppContext, w: gpui::WindowId, hold: bool) {
    assert!(
        cx.update(|cx: &mut App| voice::session_of(w, cx)).is_some(),
        "the window has a media session"
    );
    cx.update(|cx| voice::hold_media_pickup(w, hold, cx));
}

/// Presses Share in `a` and picks the first screen in its picker; answers once the pick was made.
fn press_and_pick(cx: &mut TestAppContext, a: &gpui::WindowHandle<Host>) {
    let wa = a.window_id();
    press_share(cx, a);
    wait_listed(cx, wa);
    assert!(cx.update(|cx| share_picker::share(wa, &"screen:1".into(), cx)));
    cx.run_until_parked();
}

/// Runs the UI (its clock following real time: a join from the UI waits on its timers) and keeps `window` on alice's
/// newest state until `pred` holds on it.
fn pump(
    cx: &mut TestAppContext,
    w: &World,
    window: gpui::WindowId,
    what: &str,
    pred: impl Fn(&AppState) -> bool,
) -> Arc<AppState> {
    let deadline = Instant::now() + CALL_WAIT;
    let mut moved = Instant::now();
    loop {
        let s = w.alice.state().borrow().clone();
        if pred(&s) {
            resync(cx, window, &w.alice);
            return s;
        }
        assert!(
            Instant::now() < deadline,
            "{what}: never; voice {:?}",
            s.voice
        );
        resync(cx, window, &w.alice);
        cx.run_until_parked();
        std::thread::sleep(Duration::from_millis(30));
        cx.executor().advance_clock(moved.elapsed());
        moved = Instant::now();
    }
}

/// Share-drop flake (2026-11-26): the client reports the call `In` before the window's media session has taken up the
/// call's media link. A share pressed and picked in that gap is kept for the call and starts once the media is up (it
/// was dropped without a word, and the share never came on).
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn a_share_pressed_before_the_media_takes_the_call_still_starts(cx: &mut TestAppContext) {
    let w = world("t-share-held");
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    hold_pickup(cx, wa, true);
    cx.update(|cx| voice::join(w.voice, wa, cx));
    pump(cx, &w, wa, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    });
    press_and_pick(cx, &a);
    assert!(
        w.alice.state().borrow().voice.call.ping_ms.is_none(),
        "the press came before the media took the call"
    );
    hold_pickup(cx, wa, false);
    w.rt.block_on(until(&w.alice, "alice's share is on", |s| {
        s.voice.share.screen
    }));
    wait(cx, "the capture runs", || rec.alive() == 1);
    assert_eq!(rec.opened(), 1);
}

/// As above, after a move: alice moves to another voice channel and her session takes up the new call only after a
/// while (as when the old room's close holds it). A share pressed in the new call meanwhile starts there.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn a_share_pressed_while_a_move_closes_the_old_room_starts_in_the_new_call(
    cx: &mut TestAppContext,
) {
    let w = world_with_two("t-share-move");
    let upstairs = w.upstairs.expect("a second voice channel");
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    cx.update(|cx| voice::join(w.voice, wa, cx));
    let s = pump(cx, &w, wa, "alice's media is up in the first call", |s| {
        s.voice.call.phase == CallPhase::In
            && s.voice.call.channel == Some(w.voice)
            && s.voice.call.ping_ms.is_some()
    });
    let first = s.voice.call.call_id;
    hold_pickup(cx, wa, true);
    cx.update(|cx| voice::join(upstairs, wa, cx));
    let s = pump(cx, &w, wa, "alice is in the second call", |s| {
        s.voice.call.phase == CallPhase::In && s.voice.call.channel == Some(upstairs)
    });
    assert_ne!(s.voice.call.call_id, first, "a new call");
    press_and_pick(cx, &a);
    hold_pickup(cx, wa, false);
    w.rt.block_on(until(
        &w.alice,
        "alice's share is on in the second call",
        |s| s.voice.share.screen && s.voice.call.channel == Some(upstairs),
    ));
    wait(cx, "the capture runs", || rec.alive() == 1);
    assert_eq!(rec.opened(), 1);
}

/// The other side of the same rule: a share pressed for one call never carries into the next. Alice presses and picks
/// while her session has not taken up the call, then leaves and joins another voice channel (a new call id: a rejoin of
/// the same channel within seconds is the same call, whose room has not finished): nothing opens there.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn a_press_for_an_earlier_call_does_not_carry_over(cx: &mut TestAppContext) {
    let w = world_with_two("t-share-earlier");
    let upstairs = w.upstairs.expect("a second voice channel");
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    hold_pickup(cx, wa, true);
    cx.update(|cx| voice::join(w.voice, wa, cx));
    let s = pump(cx, &w, wa, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    });
    let first = s.voice.call.call_id;
    press_and_pick(cx, &a);
    cx.update(|cx| voice::leave(wa, cx));
    pump(cx, &w, wa, "alice left the call", |s| {
        s.voice.call.phase == CallPhase::Idle
    });
    cx.update(|cx| voice::join(upstairs, wa, cx));
    let s = pump(cx, &w, wa, "alice is in the other channel's call", |s| {
        s.voice.call.phase == CallPhase::In && s.voice.call.channel == Some(upstairs)
    });
    assert!(s.voice.call.call_id.is_some() && s.voice.call.call_id != first);
    hold_pickup(cx, wa, false);
    w.rt.block_on(until(&w.alice, "the new call's media is up", |s| {
        s.voice.call.ping_ms.is_some()
    }));
    for _ in 0..20 {
        cx.run_until_parked();
        std::thread::sleep(Duration::from_millis(50));
    }
    assert_eq!(rec.opened(), 0, "the earlier call's press opened nothing");
    assert!(
        !w.alice.state().borrow().voice.share.screen,
        "nothing is shared"
    );
}

/// Security review S2: the person's leave cancels a share pressed in the call that the media session has not run yet,
/// even when the rejoin of the same channel is the same call (its room has not finished): nothing opens there.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn a_leave_cancels_a_press_the_media_has_not_run(cx: &mut TestAppContext) {
    let w = world("t-share-leave");
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    hold_pickup(cx, wa, true);
    cx.update(|cx| voice::join(w.voice, wa, cx));
    pump(cx, &w, wa, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    });
    press_and_pick(cx, &a);
    cx.update(|cx| voice::leave(wa, cx));
    pump(cx, &w, wa, "alice left the call", |s| {
        s.voice.call.phase == CallPhase::Idle
    });
    cx.update(|cx| voice::join(w.voice, wa, cx));
    pump(cx, &w, wa, "alice is in the channel's call again", |s| {
        s.voice.call.phase == CallPhase::In && s.voice.call.channel == Some(w.voice)
    });
    hold_pickup(cx, wa, false);
    w.rt.block_on(until(&w.alice, "the rejoined call's media is up", |s| {
        s.voice.call.ping_ms.is_some()
    }));
    for _ in 0..20 {
        cx.run_until_parked();
        std::thread::sleep(Duration::from_millis(50));
    }
    assert_eq!(rec.opened(), 0, "the left call's press opened nothing");
    assert!(
        !w.alice.state().borrow().voice.share.screen,
        "nothing is shared"
    );
}

/// screen-capture.spec.ts "owner navigation releases its native broker": closing the window whose call shares the
/// screen stops the capture at once (its media session goes, and with it the share's source).
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn closing_the_owner_window_stops_its_capture(cx: &mut TestAppContext) {
    let w = world("t-share-close");
    cx.update(dilla_desktop::install);
    cx.update(|cx| {
        dilla_desktop::windows::open_manager(cx, &dilla_desktop::Launch::empty(), false)
            .expect("the manager opens")
    });
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let a = alice_shares(cx, &w, &rec);
    assert_eq!(rec.opened(), 1);
    a.update(cx, |_, window, _| window.remove_window()).unwrap();
    cx.run_until_parked();
    // At once, as the window goes: its media session's drop ends the call's media before it returns (only the room's
    // close runs on after), so the source is let go and the process's media router is free for the next call.
    assert_eq!(rec.alive(), 0, "the capture outlived its window");
    let router = dilla_media::transform::Router::get().expect("the dilla frame hook installs");
    let probe = dilla_media::transform::CallMedia::new();
    assert!(
        router.begin(probe.clone()).is_ok(),
        "the closed window's call media still holds the router"
    );
    router.end(&probe);
    w.rt.block_on(until(&w.alice, "the closed window's call is left", |s| {
        s.voice.call.phase == CallPhase::Idle
    }));
}

/// screen-capture.spec.ts "a foreign instance cannot stop or reconfigure the capture and stopping its track destroys the
/// broker": alice's window shares; bob's window (another instance) asks to stop a share and to change the stream quality.
/// Its requests go to its own session only: alice's capture runs on, opened once, and her share stays on. Alice's own
/// stop ends it and lets the source go.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn another_instance_cannot_stop_or_reconfigure_a_capture(cx: &mut TestAppContext) {
    let w = world("t-share-foreign");
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let a = alice_shares(cx, &w, &rec);
    let b = window_for(cx, &w.bob);
    let wb = b.window_id();
    // Bob's window asks to share (which would stop a share it had) and to change the quality.
    press_share(cx, &b);
    cx.run_until_parked();
    // Bob's picker is bob's: a choice in it starts nothing in alice's call.
    if cx.read(|cx| share_picker::is_open(wb, cx)) {
        wait_listed(cx, wb);
        cx.update(|cx| share_picker::share(wb, &"screen:1".into(), cx));
    }
    let session_b = cx
        .update(|cx: &mut App| voice::session_of(wb, cx))
        .expect("bob's session");
    // Bob's request even names alice's call: it still reaches only his own session.
    let alices_call = w
        .alice
        .state()
        .borrow()
        .voice
        .call
        .call_id
        .expect("alice's call id");
    session_b.quality(
        ShareOptions {
            resolution: ShareResolution::P720,
            fps: 15,
        },
        alices_call,
    );
    for _ in 0..20 {
        cx.run_until_parked();
        std::thread::sleep(Duration::from_millis(50));
    }
    assert_eq!(rec.opened(), 1, "nothing else was opened");
    assert_eq!(rec.alive(), 1, "alice's capture runs on");
    assert!(
        w.alice.state().borrow().voice.share.screen,
        "alice still shares"
    );
    // Alice's own stop ends her share and its source.
    press_share(cx, &a);
    w.rt.block_on(until(&w.alice, "alice's share is off", |s| {
        !s.voice.share.screen
    }));
    wait(cx, "alice's source is let go", || rec.alive() == 0);
    drop(session_b);
}

/// share.spec.ts "cancelling native screen selection publishes no video or sound and leaves the voice call usable": alice
/// presses Share, the picker lists the sources, and she cancels. Nothing was opened, her share stays off, and the call
/// goes on (she can still mute, and her microphone stays published).
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn cancel_publishes_nothing_and_the_call_goes_on(cx: &mut TestAppContext) {
    let w = world("t-share-cancel");
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    cx.update(|cx| voice::join(w.voice, wa, cx));
    cx.run_until_parked();
    w.rt.block_on(until(&w.alice, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    }));
    resync(cx, wa, &w.alice);
    press_share(cx, &a);
    wait_listed(cx, wa);
    cx.update(|cx| share_picker::cancel(wa, cx));
    for _ in 0..10 {
        cx.run_until_parked();
        std::thread::sleep(Duration::from_millis(50));
    }
    assert!(!cx.read(|cx| share_picker::is_open(wa, cx)));
    assert_eq!(rec.opened(), 0, "no source was opened");
    let s = w.alice.state().borrow().clone();
    assert!(
        !s.voice.share.screen && !s.voice.share.camera,
        "nothing is shared"
    );
    assert_eq!(s.voice.call.phase, CallPhase::In, "the call goes on");
    w.alice.voice().set_muted(true);
    w.rt.block_on(until(&w.alice, "alice mutes", |s| s.voice.self_mute));
}

/// Jonas 2026-11-25 (the old client's session.ts `localScreen`): alice's window gets the frames her share sends, at the
/// size it sends them, for her own screen tile; once she stops, it holds none. Sizes only, never pixels.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn the_own_share_s_sent_frames_reach_its_window_until_it_stops(cx: &mut TestAppContext) {
    let w = world("t-share-self-view");
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let a = alice_shares(cx, &w, &rec);
    let session = cx
        .update(|cx: &mut App| voice::session_of(a.window_id(), cx))
        .expect("alice's session");
    let sent = |kind| {
        session
            .local_videos()
            .get(&kind)
            .map(|f| (f.width, f.height, f.rgba.len()))
    };
    wait(cx, "alice's own share frame", || {
        sent(ShareKind::Screen).is_some()
    });
    assert_eq!(
        sent(ShareKind::Screen),
        Some((320, 180, 320 * 180 * 4)),
        "the sent size"
    );
    assert_eq!(sent(ShareKind::Camera), None, "no camera is sent");
    press_share(cx, &a);
    w.rt.block_on(until(&w.alice, "alice's share is off", |s| {
        !s.voice.share.screen
    }));
    wait(cx, "the own share frame goes", || {
        session.local_videos().is_empty()
    });
    drop(session);
}

/// share.spec.ts "a share without sound says so on both sides": alice shares a screen (a source without sound). She is
/// told the share carries none (`voice.shareAudio.none.application`); bob's view of her screen tile carries the no-sound
/// mark (a remote screen without a screen-audio stream), and her own tile does not.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn a_share_without_sound_says_so_on_both_sides(cx: &mut TestAppContext) {
    let w = world("t-share-silent");
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let _a = alice_shares(cx, &w, &rec);
    w.rt.block_on(until(&w.alice, "alice is told", |s| {
        s.voice.notice == Some(dilla_client::state::voice::VoiceNotice::ShareWithoutSound)
    }));
    let alice_device = w.alice.state().borrow().account.device_id.unwrap();
    let alice_user = w.alice.state().borrow().account.user_id.unwrap();
    let screen = TileKey {
        user: alice_user,
        device: alice_device,
        screen: true,
    };
    let bob_state = w.bob.state().borrow().clone();
    let none = std::collections::BTreeSet::new();
    assert!(
        voice::silent(&bob_state, &screen, &none),
        "bob sees the no-sound mark on alice's screen"
    );
    let with_sound: std::collections::BTreeSet<String> = [alice_device.hex()].into();
    assert!(
        !voice::silent(&bob_state, &screen, &with_sound),
        "not when it carries sound"
    );
    let alice_state = w.alice.state().borrow().clone();
    assert!(
        !voice::silent(&alice_state, &screen, &none),
        "alice's own screen tile has no mark"
    );
}

/// permissions.spec.ts "cancelling the typed native screen picker returns null without creating a broker": the picker
/// opens on a press, lists, and Cancel closes it; no source is ever asked for.
#[gpui::test]
fn cancelling_the_picker_creates_no_capture(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let w = cx.add_window(|_, _| Host);
    let id = w.window_id();
    focus(cx, &w);
    let opened = w
        .update(cx, |_, window, cx| {
            share_picker::open(ShareOptions::default(), window, cx)
        })
        .unwrap();
    assert!(opened, "a focused window's press opens the picker");
    wait_listed(cx, id);
    assert!(rec.listed() >= 1);
    // A source the enumeration never offered is refused, and the picker stays.
    assert!(!cx.update(|cx| share_picker::share(id, &"screen:99".into(), cx)));
    assert!(cx.read(|cx| share_picker::is_open(id, cx)));
    assert!(
        !rec.listing_cancelled(),
        "the open picker's listing goes on"
    );
    cx.update(|cx| share_picker::cancel(id, cx));
    // Review I2: the listing in progress is told at once, so it opens no further thumbnail stream.
    assert!(rec.listing_cancelled(), "Cancel stops the listing");
    cx.run_until_parked();
    assert!(!cx.read(|cx| share_picker::is_open(id, cx)));
    assert_eq!(rec.opened(), 0, "no capture was created");
}

/// Review I2: a pick stops the picker's listing too (only the chosen source is captured from then on).
#[gpui::test]
fn a_pick_stops_the_picker_s_listing(cx: &mut TestAppContext) {
    let (w, id, _, _) = sound_world(cx);
    assert!(
        w.update(cx, |_, window, cx| {
            share_picker::open(ShareOptions::default(), window, cx)
        })
        .unwrap()
    );
    wait_listed(cx, id);
    let rec = cx
        .read(|cx| share_picker::listing_cancel(id, cx))
        .expect("a listing ran");
    assert!(!rec.load(Ordering::SeqCst));
    assert!(cx.update(|cx| share_picker::share(id, &"screen:1".into(), cx)));
    assert!(rec.load(Ordering::SeqCst), "the pick stops the listing");
}

/// permissions.spec.ts "screen capture without a real user gesture is refused without opening a picker": a window that
/// is not the focused one opens no picker and lists nothing.
#[gpui::test]
fn no_picker_without_a_press_on_a_focused_window(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let (_, vcx) = cx.add_window_view(|_, _| Host);
    vcx.update(|window, _| window.activate_window());
    vcx.run_until_parked();
    assert!(vcx.update(|window, _| window.is_window_active()));
    vcx.deactivate_window();
    assert!(!vcx.update(|window, _| window.is_window_active()));
    let opened = vcx.update(|window, cx| share_picker::open(ShareOptions::default(), window, cx));
    assert!(!opened, "no picker for an unfocused window");
    let id = vcx.update(|window, _| window.window_handle().window_id());
    vcx.run_until_parked();
    assert!(!vcx.read(|cx| share_picker::is_open(id, cx)));
    assert_eq!(rec.listed(), 0, "nothing was listed");
    assert_eq!(rec.opened(), 0);
}

/// The share menu's sound section (Jonas 2026-11-15: where the web's "Also share system audio" is): switching it on
/// opens the application chooser over the focused window only, which lists the playing applications; a choice becomes
/// the window's share sound, Cancel leaves none, and switching it off clears it.
#[gpui::test]
fn the_sound_section_chooses_an_application_for_the_share(cx: &mut TestAppContext) {
    use dilla_media::app_audio::AppSource;
    cx.update(dilla_desktop::install);
    let music = AppSource {
        serial: 40,
        pid: 4000,
        label: "Music".to_owned(),
    };
    let listed = music.clone();
    cx.update(|cx| apps::set_lister(Arc::new(move || Ok(vec![listed.clone()])), cx));
    let w = cx.add_window(|_, _| Host);
    let id = w.window_id();
    cx.update(|cx| voice::sync(id, Arc::new(AppState::default()), None, cx));
    // Not the focused window: no chooser.
    let opened = w
        .update(cx, |_, window, cx| apps::open(window, cx))
        .unwrap();
    assert!(!opened && !cx.read(|cx| apps::is_open(id, cx)));
    focus(cx, &w);
    assert!(
        w.update(cx, |_, window, cx| apps::open(window, cx))
            .unwrap()
    );
    let deadline = Instant::now() + Duration::from_secs(10);
    while cx.read(|cx| apps::listed(id, cx)).is_none() {
        cx.run_until_parked();
        assert!(Instant::now() < deadline, "never listed");
    }
    assert_eq!(
        cx.read(|cx| apps::listed(id, cx)),
        Some(vec![music.clone()])
    );
    cx.update(|cx| apps::close(id, None, cx));
    assert!(
        cx.read(|cx| voice::ui_of(id, cx).unwrap().share_sound.app().is_none()),
        "Cancel chooses nothing"
    );
    assert!(
        w.update(cx, |_, window, cx| apps::open(window, cx))
            .unwrap()
    );
    cx.update(|cx| apps::close(id, Some(music.clone()), cx));
    assert!(!cx.read(|cx| apps::is_open(id, cx)));
    assert_eq!(
        cx.read(|cx| voice::ui_of(id, cx).unwrap().share_sound.app().cloned()),
        Some(music)
    );
    cx.update(|cx| voice::set_share_app(id, None, cx));
    assert!(cx.read(|cx| voice::ui_of(id, cx).unwrap().share_sound.app().is_none()));
}

// ---- a shared window brings its application's sound (Task 44, D21) --------------------------------------------------

/// A window with a live call but no client (so no session: nothing is published, only `share_sound` is checked), the
/// recording sources, and a window lookup that finds `Music` for the window with id 7 and nothing for any other. Wayland
/// (the system's dialog chooses) never reaches `share_source`: that is `toggle_share`'s portal branch, and no lookup runs
/// there. macOS has Dilla's own picker since Task 45 (D22), so it takes this path like X11 and Windows.
fn sound_world(
    cx: &mut TestAppContext,
) -> (
    gpui::WindowHandle<Host>,
    gpui::WindowId,
    dilla_media::app_audio::AppSource,
    Arc<AtomicUsize>,
) {
    use dilla_media::app_audio::AppSource;
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    let music = AppSource {
        serial: 40,
        pid: 4000,
        label: "Music".to_owned(),
    };
    let asked = Arc::new(AtomicUsize::new(0));
    let (found, counted) = (music.clone(), asked.clone());
    cx.update(|cx| {
        apps::set_window_sound(
            Arc::new(move |source| {
                counted.fetch_add(1, Ordering::SeqCst);
                (source.id == 7).then(|| found.clone())
            }),
            cx,
        )
    });
    let w = cx.add_window(|_, _| Host);
    let id = w.window_id();
    focus(cx, &w);
    cx.update(|cx| voice::sync(id, call_state(false), None, cx));
    (w, id, music, asked)
}

/// The state of a live call, with or without a screen share.
fn call_state(screen: bool) -> Arc<AppState> {
    let mut state = AppState::default();
    state.voice.call.phase = CallPhase::In;
    state.voice.share.screen = screen;
    Arc::new(state)
}

/// The picker opens on `w` and the person picks `key`.
fn pick(cx: &mut TestAppContext, w: &gpui::WindowHandle<Host>, key: &str) {
    let id = w.window_id();
    assert!(
        w.update(cx, |_, window, cx| {
            share_picker::open(ShareOptions::default(), window, cx)
        })
        .unwrap()
    );
    wait_listed(cx, id);
    assert!(cx.update(|cx| share_picker::share(id, &key.into(), cx)));
}

fn sound(cx: &mut TestAppContext, id: gpui::WindowId) -> voice::ShareSound {
    cx.read(|cx| voice::ui_of(id, cx).unwrap().share_sound.clone())
}

#[gpui::test]
fn sharing_a_window_brings_its_application_sound(cx: &mut TestAppContext) {
    let (w, id, music, _) = sound_world(cx);
    pick(cx, &w, "window:7");
    cx.run_until_parked();
    assert_eq!(sound(cx, id), voice::ShareSound::Window(music.clone()));
    // The sound section reads it like a manual choice.
    assert_eq!(
        cx.read(|cx| voice::ui_of(id, cx).unwrap().share_sound.app().cloned()),
        Some(music)
    );
}

#[gpui::test]
fn sharing_a_screen_brings_no_application_sound(cx: &mut TestAppContext) {
    let (w, id, _, asked) = sound_world(cx);
    pick(cx, &w, "screen:1");
    cx.run_until_parked();
    assert_eq!(sound(cx, id), voice::ShareSound::Unset);
    assert_eq!(asked.load(Ordering::SeqCst), 0, "no window, no lookup");
}

/// Review I4 (Jonas 2026-11-25): every share takes the sound of what it shares. A choice in the live panel holds for
/// that share only: when it ends, and when the call ends, it is dropped, so the next window share brings its own
/// application's sound and the next screen share none.
#[gpui::test]
fn each_share_starts_from_its_own_sound(cx: &mut TestAppContext) {
    use dilla_media::app_audio::AppSource;
    let (w, id, music, _) = sound_world(cx);
    let other = AppSource {
        serial: 41,
        pid: 4100,
        label: "Radio".to_owned(),
    };
    pick(cx, &w, "window:7");
    cx.run_until_parked();
    cx.update(|cx| voice::sync(id, call_state(true), None, cx));
    assert_eq!(sound(cx, id), voice::ShareSound::Window(music.clone()));
    // The live panel's change holds for this share.
    cx.update(|cx| voice::set_share_app(id, Some(other.clone()), cx));
    assert_eq!(
        sound(cx, id),
        voice::ShareSound::Person(Some(other.clone()))
    );
    // The share ends: nothing is remembered.
    cx.update(|cx| voice::sync(id, call_state(false), None, cx));
    assert_eq!(sound(cx, id), voice::ShareSound::Unset);
    // The next window share brings its own application's sound again.
    pick(cx, &w, "window:7");
    cx.run_until_parked();
    cx.update(|cx| voice::sync(id, call_state(true), None, cx));
    assert_eq!(sound(cx, id), voice::ShareSound::Window(music));
    // Switched off in the live panel, then the call ends (with the share on): dropped as well.
    cx.update(|cx| voice::set_share_app(id, None, cx));
    assert_eq!(sound(cx, id), voice::ShareSound::Person(None));
    cx.update(|cx| voice::sync(id, Arc::new(AppState::default()), None, cx));
    assert_eq!(sound(cx, id), voice::ShareSound::Unset);
    // A choice left with no share running goes when the call ends.
    cx.update(|cx| voice::sync(id, call_state(false), None, cx));
    cx.update(|cx| voice::set_share_app(id, Some(other.clone()), cx));
    cx.update(|cx| voice::sync(id, Arc::new(AppState::default()), None, cx));
    assert_eq!(sound(cx, id), voice::ShareSound::Unset, "the call ended");
    // A screen share after a choice brings none.
    cx.update(|cx| voice::sync(id, call_state(true), None, cx));
    cx.update(|cx| voice::set_share_app(id, Some(other), cx));
    cx.update(|cx| voice::sync(id, call_state(false), None, cx));
    pick(cx, &w, "screen:1");
    cx.run_until_parked();
    assert_eq!(sound(cx, id), voice::ShareSound::Unset);
}

/// A choice made in the live panel before the window's own sound was found is kept for that share: the late lookup
/// never replaces the person's choice.
#[gpui::test]
fn the_person_s_live_choice_wins_over_a_late_window_sound(cx: &mut TestAppContext) {
    use dilla_media::app_audio::AppSource;
    let (w, id, _, _) = sound_world(cx);
    let other = AppSource {
        serial: 41,
        pid: 4100,
        label: "Radio".to_owned(),
    };
    pick(cx, &w, "window:7");
    // Before the lookup lands (it runs off the UI thread), the person chooses in the live panel.
    cx.update(|cx| voice::set_share_app(id, Some(other.clone()), cx));
    cx.run_until_parked();
    assert_eq!(sound(cx, id), voice::ShareSound::Person(Some(other)));
}

#[gpui::test]
fn a_window_without_a_playing_owner_shares_no_sound(cx: &mut TestAppContext) {
    // The lookup answers `None`: the no-owner and not-playing fallback (Wayland never gets here).
    let (w, id, _, _) = sound_world(cx);
    cx.update(|cx| apps::set_window_sound(Arc::new(|_| None), cx));
    pick(cx, &w, "window:7");
    cx.run_until_parked();
    assert_eq!(sound(cx, id), voice::ShareSound::Unset);
}

#[gpui::test]
fn a_late_window_sound_is_dropped(cx: &mut TestAppContext) {
    let (w, id, _, _) = sound_world(cx);
    // The window's lookup is still running when a screen share starts: it lands late and is dropped.
    pick(cx, &w, "window:7");
    pick(cx, &w, "screen:1");
    cx.run_until_parked();
    assert_eq!(sound(cx, id), voice::ShareSound::Unset);
    // A lookup that lands after its share ended is dropped too.
    pick(cx, &w, "window:7");
    cx.update(|cx| voice::sync(id, call_state(true), None, cx));
    cx.update(|cx| voice::sync(id, call_state(false), None, cx));
    cx.run_until_parked();
    assert_eq!(sound(cx, id), voice::ShareSound::Unset);
}

// ---- Dilla's own share picker on macOS (Task 45, D22) ---------------------------------------------------------------

/// A listing the system refused (macOS without Screen Recording) leaves the picker open with no tiles and marks it
/// denied, so its empty state says how to allow Screen Recording; the overlay still draws. A listing that was not
/// refused is not denied, even when it is empty.
#[gpui::test]
fn a_denied_listing_shows_how_to_allow_screen_recording(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let rec = Arc::new(Recorder::default());
    rec.install(cx);
    cx.update(|cx| {
        share_picker::set_lister(
            Arc::new(|_: share_picker::Cancel| Listing {
                found: vec![],
                denied: true,
            }),
            cx,
        )
    });
    let (_, vcx) = cx.add_window_view(|_, _| PickerHost);
    vcx.update(|window, _| window.activate_window());
    vcx.run_until_parked();
    let id = vcx.update(|window, _| window.window_handle().window_id());
    assert!(vcx.update(|window, cx| share_picker::open(ShareOptions::default(), window, cx)));
    wait_listed(vcx, id);
    let (denied, tiles) = vcx.read(|cx| {
        let p = share_picker::picker_of(id, cx).expect("the picker stays open");
        (p.denied, p.tiles.as_ref().map(Vec::len))
    });
    assert!(denied, "the refusal reaches the picker");
    assert_eq!(tiles, Some(0), "no tiles without Screen Recording");
    // The window draws its picker, as the shell's overlays do.
    vcx.update(|window, cx| {
        assert!(share_picker::overlay(window, cx).is_some());
        window.refresh();
        window.draw(cx).clear(cx);
    });
    vcx.update(|_, cx| share_picker::cancel(id, cx));

    cx.update(|cx| {
        share_picker::set_lister(
            Arc::new(|_: share_picker::Cancel| Vec::<Found>::new().into()),
            cx,
        )
    });
    let (_, vcx) = cx.add_window_view(|_, _| Host);
    vcx.update(|window, _| window.activate_window());
    vcx.run_until_parked();
    let id = vcx.update(|window, _| window.window_handle().window_id());
    assert!(vcx.update(|window, cx| share_picker::open(ShareOptions::default(), window, cx)));
    wait_listed(vcx, id);
    assert!(
        !vcx.read(|cx| share_picker::picker_of(id, cx).unwrap().denied),
        "an empty listing is not a refusal"
    );
}

/// The owner a window's tile was listed with (macOS: `SCWindow.owningApplication`) is the owner the window's sound is
/// looked up for: the pid travels from the listing through the choice to the lookup unchanged.
#[gpui::test]
fn the_listed_owner_reaches_the_window_sound(cx: &mut TestAppContext) {
    let (w, id, _, _) = sound_world(cx);
    cx.update(|cx| {
        share_picker::set_lister(
            Arc::new(|_: share_picker::Cancel| {
                vec![Found {
                    source: DesktopSource {
                        kind: SourceKind::Window,
                        id: 7,
                        title: "Notes".to_owned(),
                        owner: Some(4000),
                    },
                    thumbnail: None,
                }]
                .into()
            }),
            cx,
        )
    });
    let asked: Arc<std::sync::Mutex<Vec<DesktopSource>>> = Arc::default();
    let seen = asked.clone();
    cx.update(|cx| {
        apps::set_window_sound(
            Arc::new(move |source| {
                seen.lock().unwrap().push(source.clone());
                None
            }),
            cx,
        )
    });
    pick(cx, &w, "window:7");
    let deadline = Instant::now() + Duration::from_secs(10);
    while asked.lock().unwrap().is_empty() {
        cx.run_until_parked();
        assert!(
            Instant::now() < deadline,
            "the window's sound was never looked up"
        );
        std::thread::sleep(Duration::from_millis(10));
    }
    let asked = asked.lock().unwrap().clone();
    assert_eq!(asked.len(), 1);
    assert_eq!(asked[0].owner, Some(4000));
    assert_eq!(sound(cx, id), voice::ShareSound::Unset);
}
