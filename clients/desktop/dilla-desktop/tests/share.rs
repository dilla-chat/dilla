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
use dilla_desktop::screens::voice::{self, ShareSources};
use dilla_media::{CaptureSource, Pull, ShareKind, ShareOptions, ShareResolution, TestPattern};
use gpui::{App, Context, IntoElement, Render, TestAppContext, Window, div};

#[path = "../../dilla-client/tests/support/call_world.rs"]
mod call_world;

/// One process, one call's media (see tests/voice.rs `MEDIA_SLOT`): the tests here take turns.
static MEDIA_SLOT: std::sync::Mutex<()> = std::sync::Mutex::new(());

const CALL_WAIT: Duration = Duration::from_secs(45);

/// Counts the share sources opened, and those alive (not yet dropped).
#[derive(Default)]
struct Recorder {
    opened: AtomicUsize,
    alive: Arc<AtomicUsize>,
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
        cx.update(|cx| {
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
}

struct Host;

impl Render for Host {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        div()
    }
}

struct World {
    rt: tokio::runtime::Runtime,
    _host: call_world::Host,
    alice: Rc<dilla_client::Client>,
    bob: Rc<dilla_client::Client>,
    voice: ChannelId,
    _slot: std::sync::MutexGuard<'static, ()>,
}

fn world(tag: &'static str) -> World {
    let slot = MEDIA_SLOT.lock().unwrap_or_else(|p| p.into_inner());
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let (host, alice, bob, voice) = rt.block_on(async {
        let host = tokio::task::spawn_blocking(move || call_world::start_host_reachable(tag))
            .await
            .unwrap();
        let invite = host.invites.last().unwrap().clone();
        let alice = call_world::sign_up(&host, &invite, "alice", tag).await;
        let bob = call_world::sign_up(&host, &invite, "bob", tag).await;
        let (host, alice, bob, voice) = tokio::task::spawn_blocking(move || {
            let (_, v) = call_world::voice_community(&host, &alice, &[&bob]);
            call_world::clear_session(&alice);
            call_world::clear_session(&bob);
            (host, alice, bob, ChannelId(v))
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
            until(c, "the voice channel is listed", |s| {
                s.channel(voice).is_some()
            })
            .await;
        }
        (host, a, b, voice)
    });
    World {
        rt,
        _host: host,
        alice: Rc::new(alice),
        bob: Rc::new(bob),
        voice,
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

/// Joins alice's window to the call and starts a screen share there; answers once it runs.
fn alice_shares(cx: &mut TestAppContext, w: &World, rec: &Recorder) -> gpui::WindowHandle<Host> {
    let a = window_for(cx, &w.alice);
    let wa = a.window_id();
    cx.update(|cx| voice::join(w.voice, wa, cx));
    cx.run_until_parked();
    w.rt.block_on(until(&w.alice, "alice is in the call", |s| {
        s.voice.call.phase == CallPhase::In
    }));
    resync(cx, wa, &w.alice);
    cx.update(|cx: &mut App| voice::toggle_share(ShareKind::Screen, wa, cx));
    w.rt.block_on(until(&w.alice, "alice's share is on", |s| {
        s.voice.share.screen
    }));
    resync(cx, wa, &w.alice);
    wait(cx, "the capture runs", || rec.alive() == 1);
    a
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
    let deadline = Instant::now() + Duration::from_secs(3);
    while rec.alive() != 0 {
        cx.run_until_parked();
        assert!(Instant::now() < deadline, "the capture outlived its window");
        std::thread::sleep(Duration::from_millis(20));
    }
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
    let wa = a.window_id();
    let b = window_for(cx, &w.bob);
    let wb = b.window_id();
    // Bob's window asks to share (which would stop a share it had) and to change the quality.
    cx.update(|cx: &mut App| voice::toggle_share(ShareKind::Screen, wb, cx));
    let session_b = cx
        .update(|cx: &mut App| voice::session_of(wb, cx))
        .expect("bob's session");
    session_b.quality(ShareOptions {
        resolution: ShareResolution::P720,
        fps: 15,
    });
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
    cx.update(|cx: &mut App| voice::toggle_share(ShareKind::Screen, wa, cx));
    w.rt.block_on(until(&w.alice, "alice's share is off", |s| {
        !s.voice.share.screen
    }));
    wait(cx, "alice's source is let go", || rec.alive() == 0);
    drop(session_b);
}
