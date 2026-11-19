//! GPUI gaps Task 3b: a hidden start (`--hidden`, the autostart at login) opens no window but connects every listed
//! instance whose database key the keychain gives without asking (`windows::start_headless_clients`), so rings and
//! messages notify from the tray, as beta.5's hidden window did. A window opened later shows the running client.
//!
//! The tests that need a running client run against a real dillad and are `#[ignore]`d (the desktop-gpui CI job runs
//! them with `--ignored` on Linux): needs `target/dilla-testhost` and the WASI core.

mod support;

use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, AtomicUsize, Ordering};
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::instances::Instances;
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::{Keychain, KeychainError};
use dilla_client::store::open_store;
use dilla_desktop::native::notifier;
use dilla_desktop::screens::ring::effects;
use dilla_desktop::{Launch, windows};
use gpui::TestAppContext;
use zeroize::Zeroizing;

static NEXT: AtomicU64 = AtomicU64::new(0);

fn root() -> PathBuf {
    let p = Path::new(env!("CARGO_TARGET_TMPDIR")).join(format!(
        "desktop-hidden-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    let _ = std::fs::remove_dir_all(&p);
    p
}

/// A keychain whose collection is locked: a plain read would show the unlock dialog (counted in `asked`), a quiet one
/// answers `Unavailable`.
#[derive(Default)]
struct Locked {
    inner: MemoryKeychain,
    asked: AtomicUsize,
    quiet: AtomicUsize,
}

impl Keychain for Locked {
    fn get(&self, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        self.asked.fetch_add(1, Ordering::SeqCst);
        self.inner.get(account)
    }
    fn set(&self, account: &str, secret: &[u8]) -> Result<(), KeychainError> {
        self.asked.fetch_add(1, Ordering::SeqCst);
        self.inner.set(account, secret)
    }
    fn delete(&self, account: &str) -> Result<(), KeychainError> {
        self.asked.fetch_add(1, Ordering::SeqCst);
        self.inner.delete(account)
    }
    fn get_quietly(&self, _account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        self.quiet.fetch_add(1, Ordering::SeqCst);
        Err(KeychainError::Unavailable)
    }
}

/// Runs the app until `done`, for at most `within`.
fn run_until(
    cx: &mut TestAppContext,
    within: Duration,
    mut done: impl FnMut(&mut TestAppContext) -> bool,
) -> bool {
    let started = Instant::now();
    loop {
        cx.run_until_parked();
        if done(cx) {
            return true;
        }
        if started.elapsed() > within {
            return false;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
}

/// A hidden start at login never asks for the keychain: an instance whose key sits in a locked keyring is skipped
/// (its store and key untouched), holds nothing, and starts when its window opens, as before.
#[gpui::test]
fn a_locked_key_skips_the_instance_without_a_prompt(cx: &mut TestAppContext) {
    let root = root();
    let kc = Arc::new(Locked::default());
    let mut list = Instances::load(&root).unwrap();
    let cfg = list
        .add(url::Url::parse("https://locked.example/").unwrap())
        .unwrap();
    // A signed-in device's store and key (the key in the keyring that is now locked).
    drop(open_store(&cfg, &kc.inner).unwrap());
    let accounts = kc.inner.accounts();
    let launch = Launch::live(Some(root), kc.clone());
    cx.update(dilla_desktop::install);
    cx.executor().allow_parking();

    cx.update(|cx| windows::start_headless(cx, &launch));
    let id = cfg.id;
    let settled = run_until(cx, Duration::from_secs(10), |cx| {
        kc.quiet.load(Ordering::SeqCst) > 0
            && cx.update(|cx| !windows::is_starting(cx, id) && !windows::held_here(cx, id))
    });
    assert!(settled, "the start asked the keychain quietly, then let go");
    assert_eq!(kc.asked.load(Ordering::SeqCst), 0, "no prompt, no write");
    assert!(cx.update(|cx| windows::running_client(cx, id)).is_none());
    assert!(cx.windows().is_empty(), "no window opened");
    assert_eq!(kc.inner.accounts(), accounts, "the key is untouched");
}

// ---- against a real dillad ----

fn runtime() -> tokio::runtime::Runtime {
    tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap()
}

fn root_of(p: &support::world::Person) -> PathBuf {
    p.cfg
        .data_dir
        .parent()
        .and_then(Path::parent)
        .unwrap()
        .to_path_buf()
}

/// A hidden start of `person`'s data root (the shell's notifier installed, as `native::install` does): answers the
/// client it started.
fn hidden_start(cx: &mut TestAppContext, person: &support::world::Person) -> (Launch, Rc<Client>) {
    let launch = Launch::live(Some(root_of(person)), person.kc.clone());
    cx.update(dilla_desktop::install);
    cx.update(notifier::install);
    cx.executor().allow_parking();
    cx.update(|cx| windows::start_headless(cx, &launch));
    let id = person.cfg.id;
    let mut client = None;
    let started = run_until(cx, Duration::from_secs(60), |cx| {
        client = cx.update(|cx| windows::running_client(cx, id));
        client.is_some()
    });
    assert!(started, "the hidden start started the client");
    (launch, client.unwrap())
}

/// The instance's client runs with no window, its state is what the ring observer follows, and a ring sounds and
/// raises the OS notification (with Answer and Decline) under the running client's permission and settings.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn a_hidden_start_connects_and_rings(cx: &mut TestAppContext) {
    let rt = runtime();
    let host = support::world::start_host("t3b-ring", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let alice = rt.block_on(support::world::sign_up(
        &host, &invite, "alice", "Alice", "t3b-ring",
    ));
    support::world::clear_session(&alice);
    let (_launch, client) = hidden_start(cx, &alice);
    let id = alice.cfg.id;
    assert!(cx.windows().is_empty(), "no window opened");
    assert_eq!(
        cx.update(|cx| effects::watched(id, cx)),
        Some(Rc::as_ptr(&client) as usize),
        "the ring observer follows the hidden client"
    );
    // A ring, as the client's state lists it.
    let dm = dilla_client::state::ChannelId([0xd4; 16]);
    let ring = dilla_client::state::ring::IncomingRing {
        call_id: dm.0,
        channel: dm,
        caller: dilla_client::state::UserId([0xb2; 16]),
        caller_name: "Bob".into(),
        ring_ts: 1_700_000_030,
        expires_at: tokio::time::Instant::now() + dilla_client::state::ring::RING,
        busy: false,
        quiet: false,
    };
    cx.update(|cx| effects::sync_instance(id, &[ring], false, cx));
    assert!(cx.update(effects::sound_on), "the ring sounds");
    let shown = cx.shown_system_notifications();
    assert_eq!(shown.len(), 1, "one ring notification");
    assert!(shown[0].tag.starts_with(&format!("dilla:{}:", id.0)));
    assert_eq!(shown[0].actions.len(), 2, "Answer and Decline");
    drop(client);
    cx.update(windows::release_all);
}

/// A message to the hidden client notifies: its notify engine started with the client, not with a first window frame.
/// Bob opens a DM with alice and writes in it; alice's OS notification names him.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn a_hidden_start_raises_message_notifications(cx: &mut TestAppContext) {
    use dilla_client::state::{CommunityId, UserId};
    let rt = runtime();
    let host = support::world::start_host("t3b-msg", "127.0.0.1:0", None);
    host.follow_wall_clock();
    let invite = host.invites.last().unwrap().clone();
    let alice = rt.block_on(support::world::sign_up(
        &host, &invite, "alice", "alice", "t3b-msg",
    ));
    let bob = rt.block_on(support::world::sign_up(
        &host, &invite, "bob", "bob", "t3b-msg",
    ));
    let community = support::world::community_with(&host, &alice, "gpui", &[&bob], &[]);
    support::world::clear_session(&alice);
    support::world::clear_session(&bob);
    let (_launch, a) = hidden_start(cx, &alice);
    assert!(
        cx.update(|cx| notifier::engine_runs(alice.cfg.id, cx)),
        "the engine runs"
    );
    let b = rt.block_on(support::world::start(&bob));
    // Both hold #general's group first, as the client's own notify tests do.
    let general = rt.block_on(support::world::wait_for(
        &b,
        Duration::from_secs(30),
        "the channel list",
        |s| s.channel_named(CommunityId(community), "general").is_some(),
    ));
    let general = general
        .channel_named(CommunityId(community), "general")
        .unwrap()
        .id;
    let opened = a.open_channel(general);
    rt.block_on(opened).unwrap().expect("alice opens #general");
    rt.block_on(b.open_channel(general))
        .unwrap()
        .expect("bob joins #general");
    b.dms().start();
    let dm = rt
        .block_on(b.dms().open_dm(UserId(alice.user_id)))
        .unwrap()
        .unwrap();
    rt.block_on(b.dms().send_text(dm, "dm from peer".into()))
        .unwrap()
        .unwrap();
    let shown = run_until(cx, Duration::from_secs(30), |cx| {
        !cx.shown_system_notifications().is_empty()
    });
    assert!(shown, "alice's OS notification arrived with no window");
    let shown = cx.shown_system_notifications();
    assert_eq!(shown[0].title.as_ref(), "bob");
    assert_eq!(shown[0].body.as_ref(), "dm from peer");
    assert!(
        shown[0]
            .tag
            .starts_with(&format!("dilla:{}:", alice.cfg.id.0))
    );
    assert!(cx.windows().is_empty());
    drop((a, b));
    cx.update(windows::release_all);
}

/// The window opened after a hidden start (the tray's instance row) shows the client that runs: exactly one client per
/// instance, and the ring observer still follows it.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn opening_the_window_after_a_hidden_start_reuses_the_client(cx: &mut TestAppContext) {
    let rt = runtime();
    let host = support::world::start_host("t3b-open", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let alice = rt.block_on(support::world::sign_up(
        &host, &invite, "alice", "Alice", "t3b-open",
    ));
    support::world::clear_session(&alice);
    let (launch, client) = hidden_start(cx, &alice);
    let id = alice.cfg.id;
    cx.update(|cx| windows::open_instance(cx, &launch, alice.cfg.clone(), None, false))
        .expect("the instance window opens");
    let mut shown = None;
    let drawn = run_until(cx, Duration::from_secs(30), |cx| {
        shown = cx.update(|cx| {
            cx.windows()
                .into_iter()
                .filter_map(|w| w.downcast::<dilla_desktop::AppView>())
                .find_map(|w| {
                    w.read_with(cx, |app, cx| {
                        app.shell().and_then(|s| s.read(cx).client().cloned())
                    })
                    .ok()
                    .flatten()
                })
        });
        shown.is_some()
    });
    assert!(drawn, "the window shows the workspace");
    assert!(
        Rc::ptr_eq(&shown.unwrap(), &client),
        "the window shows the hidden start's client"
    );
    let now = cx.update(|cx| windows::running_client(cx, id)).unwrap();
    assert!(Rc::ptr_eq(&now, &client), "no second client was started");
    assert_eq!(
        cx.update(|cx| effects::watched(id, cx)),
        Some(Rc::as_ptr(&client) as usize)
    );
    drop((client, now));
    cx.update(windows::release_all);
}
