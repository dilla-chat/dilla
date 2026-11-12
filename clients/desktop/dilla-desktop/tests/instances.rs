//! Task 27's behaviour and keyboard tests for the instances area (standard procedure S5, S6): the manager window's
//! list, connecting by address and by pasted invite, removing, and the one-window-per-instance registry.
//!
//! Parity (S3/S4) is not here: the manager is a window of the Electron shell (`manager.html`), not a route of the web
//! client, so the route-driven fixtures of `dilla-parity` cannot draw or shoot it (see the task report).

mod support;

use std::cell::RefCell;
use std::path::{Path, PathBuf};
use std::rc::Rc;
use std::sync::Arc;
use std::sync::atomic::{AtomicU64, Ordering};

use dilla_client::instances::{Instances, MAX_INSTANCES};
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::{InstanceConfig, InstanceId, Keychain};
use dilla_client::store::{keychain_account, open_store};
use dilla_desktop::screens::instances::model::label_of;
use dilla_desktop::screens::instances::{ManagerEvent, ManagerView, copy, model::Manager};
use gpui::{TestAppContext, VisualTestContext, WindowHandle};

static NEXT: AtomicU64 = AtomicU64::new(0);

fn root() -> PathBuf {
    let p = Path::new(env!("CARGO_TARGET_TMPDIR")).join(format!(
        "desktop-instances-{}-{}",
        std::process::id(),
        NEXT.fetch_add(1, Ordering::Relaxed)
    ));
    let _ = std::fs::remove_dir_all(&p);
    p
}

type Opened = Rc<RefCell<Vec<(InstanceConfig, Option<String>)>>>;
type Released = Rc<RefCell<Vec<InstanceId>>>;

struct Fixture {
    window: WindowHandle<ManagerView>,
    opened: Opened,
    released: Released,
    root: PathBuf,
}

fn open_manager(cx: &mut TestAppContext, root: &Path, kc: Arc<MemoryKeychain>) -> Fixture {
    cx.update(dilla_desktop::install);
    let opened: Opened = Rc::default();
    let released: Released = Rc::default();
    let (r, k, rel) = (root.to_path_buf(), kc.clone(), released.clone());
    let window = cx.add_window(move |window, cx| {
        let model = Manager::load(&r, k, false);
        let release: dilla_desktop::screens::instances::view::Release = Rc::new(move |id, _| {
            rel.borrow_mut().push(id);
        });
        ManagerView::new(model, release, window, cx)
    });
    let view = window.root(cx).expect("the view");
    let sink = opened.clone();
    cx.update(|cx| {
        cx.subscribe(&view, move |_, event: &ManagerEvent, _| {
            let ManagerEvent::Open { instance, invite } = event;
            sink.borrow_mut().push((instance.clone(), invite.clone()));
        })
        .detach();
    });
    Fixture {
        window,
        opened,
        released,
        root: root.to_path_buf(),
    }
}

impl Fixture {
    fn type_address(&self, cx: &mut TestAppContext, text: &str) {
        self.window
            .update(cx, |view, window, cx| view.set_address(text, window, cx))
            .unwrap();
    }

    fn type_invite(&self, cx: &mut TestAppContext, text: &str) {
        self.window
            .update(cx, |view, window, cx| view.set_invite(text, window, cx))
            .unwrap();
    }

    fn connect(&self, cx: &mut TestAppContext) {
        self.window
            .update(cx, |view, window, cx| view.connect_url(window, cx))
            .unwrap();
    }

    fn join(&self, cx: &mut TestAppContext) {
        self.window
            .update(cx, |view, window, cx| view.connect_invite(window, cx))
            .unwrap();
    }

    fn entries(&self, cx: &mut TestAppContext) -> Vec<String> {
        self.window
            .read_with(cx, |view, _| view.entries())
            .unwrap()
            .into_iter()
            .map(|e| e.label)
            .collect()
    }

    fn error(&self, cx: &mut TestAppContext) -> Option<String> {
        self.window
            .read_with(cx, |view, _| view.error().map(str::to_owned))
            .unwrap()
    }

    fn confirming(&self, cx: &mut TestAppContext) -> bool {
        self.window
            .read_with(cx, |view, _| view.confirming())
            .unwrap()
    }
}

/// From `manager.ts`: an address is connected and listed, and the window asks to open it; a pasted invite does the same
/// with its code, and asks first when it names an instance not listed yet.
#[gpui::test]
fn manager_lists_and_connects_by_url_and_invite(cx: &mut TestAppContext) {
    let root = root();
    let app = open_manager(cx, &root, Arc::new(MemoryKeychain::new()));
    assert!(app.entries(cx).is_empty(), "a new install lists nothing");

    app.type_address(cx, "https://chat.example.org");
    app.connect(cx);
    assert_eq!(app.entries(cx), ["chat.example.org"]);
    assert_eq!(app.error(cx), None);
    {
        let opened = app.opened.borrow();
        assert_eq!(opened.len(), 1);
        assert_eq!(opened[0].0.server.as_str(), "https://chat.example.org/");
        assert_eq!(opened[0].1, None);
    }

    // A page link of an invite names its server and carries its code.
    app.type_address(cx, "https://chat.example.org/i/abc_DEF-123");
    app.connect(cx);
    assert_eq!(
        app.entries(cx),
        ["chat.example.org"],
        "the same instance is not added twice"
    );
    assert_eq!(
        app.opened.borrow()[1].1.as_deref(),
        Some("abc_DEF-123"),
        "the code of an /i/ link rides along"
    );

    // A pasted invite for the listed instance opens at once; one for a new instance asks first.
    app.type_invite(cx, "dilla://invite/chat.example.org/CODE-1");
    app.join(cx);
    assert!(!app.confirming(cx));
    assert_eq!(app.opened.borrow()[2].1.as_deref(), Some("CODE-1"));

    app.type_invite(cx, "dilla://invite/new.example.org/CODE-2");
    app.join(cx);
    assert!(
        app.confirming(cx),
        "an unlisted instance is confirmed before it is added"
    );
    assert_eq!(app.entries(cx), ["chat.example.org"]);
    assert_eq!(app.opened.borrow().len(), 3);
    // Cancel leaves everything as it was.
    app.window
        .update(cx, |view, window, cx| view.cancel_confirmation(window, cx))
        .unwrap();
    assert!(!app.confirming(cx));
    assert_eq!(app.entries(cx), ["chat.example.org"]);
    app.join(cx);
    app.window
        .update(cx, |view, window, cx| view.confirm(window, cx))
        .unwrap();
    assert_eq!(app.entries(cx), ["chat.example.org", "new.example.org"]);
    let opened = app.opened.borrow();
    assert_eq!(opened[3].0.server.as_str(), "https://new.example.org/");
    assert_eq!(opened[3].1.as_deref(), Some("CODE-2"));
    drop(opened);

    // Refusals never echo the input and change nothing.
    for bad in [
        "http://insecure.example",
        "https://user:pw@h.example/",
        "nonsense",
        "",
    ] {
        app.type_address(cx, bad);
        app.connect(cx);
        assert_eq!(
            app.error(cx).as_deref(),
            Some(copy::ERROR_CONNECT),
            "{bad:?}"
        );
    }
    app.type_invite(cx, "https://chat.example.org/i/not-a-dilla-link");
    app.join(cx);
    assert_eq!(app.error(cx).as_deref(), Some(copy::ERROR_CONNECT));
    assert_eq!(app.entries(cx).len(), 2);
}

/// The 33rd instance is refused in the window, with its own words, and the list stays at 32.
#[gpui::test]
fn thirty_third_instance_refused_in_ui(cx: &mut TestAppContext) {
    let root = root();
    let app = open_manager(cx, &root, Arc::new(MemoryKeychain::new()));
    for n in 0..MAX_INSTANCES {
        app.type_address(cx, &format!("https://h{n}.example"));
        app.connect(cx);
        assert_eq!(app.error(cx), None, "instance {n}");
    }
    assert_eq!(app.entries(cx).len(), 32);
    app.type_address(cx, "https://one-too-many.example");
    app.connect(cx);
    assert_eq!(
        app.error(cx).as_deref(),
        Some(copy::ERROR_TOO_MANY.replace("{max}", "32").as_str())
    );
    assert_eq!(app.entries(cx).len(), 32);
    assert_eq!(
        app.opened.borrow().len(),
        32,
        "no window was asked for the refused one"
    );
    // An instance that is listed still opens at the cap.
    app.type_address(cx, "https://h7.example/");
    app.connect(cx);
    assert_eq!(app.error(cx), None);
    assert_eq!(app.opened.borrow().len(), 33);
}

/// From `forwarding.spec.ts` "isolates storage for two complete URLs on the same host": two complete URLs of one host
/// are two instances, with a data dir and a key each.
#[gpui::test]
fn two_instances_same_host_isolated(cx: &mut TestAppContext) {
    let root = root();
    let kc = Arc::new(MemoryKeychain::new());
    let app = open_manager(cx, &root, kc.clone());
    app.type_address(cx, "https://h.test/team-a");
    app.connect(cx);
    app.type_address(cx, "https://h.test/team-b");
    app.connect(cx);
    assert_eq!(app.entries(cx), ["h.test/team-a/", "h.test/team-b/"]);
    let opened = app.opened.borrow();
    let (a, b) = (&opened[0].0, &opened[1].0);
    assert_ne!(a.id, b.id);
    assert_ne!(a.data_dir, b.data_dir);

    // Each opens its own store under its own key; what one holds the other cannot read.
    let mut store_a = open_store(a, kc.as_ref()).unwrap();
    let store_b = open_store(b, kc.as_ref()).unwrap();
    store_a.setting_put("secret", "only in a").unwrap();
    let has = |core: &dilla_core::client::ClientCore| {
        core.settings()
            .unwrap()
            .windows(9)
            .any(|w| w == b"only in a")
    };
    assert!(has(&store_a));
    assert!(!has(&store_b));
    let install = Instances::load(&root).unwrap().install_id().to_owned();
    assert_ne!(
        kc.get(&keychain_account(&install, a.id))
            .unwrap()
            .unwrap()
            .as_slice(),
        kc.get(&keychain_account(&install, b.id))
            .unwrap()
            .unwrap()
            .as_slice(),
    );
}

/// A removal deletes the instance's window, its data and its key, and a stale window never writes it back.
#[gpui::test]
fn removed_instance_not_resurrected(cx: &mut TestAppContext) {
    let root = root();
    let kc = Arc::new(MemoryKeychain::new());
    let first = open_manager(cx, &root, kc.clone());
    for server in ["https://one.example", "https://two.example"] {
        first.type_address(cx, server);
        first.connect(cx);
    }
    // A key exists for instance 0, as it does once its client has started.
    let cfg0 = first.opened.borrow()[0].0.clone();
    drop(open_store(&cfg0, kc.as_ref()).unwrap());
    assert_eq!(kc.accounts().len(), 1);

    // A second manager window loads the list, then the first one removes instance 0.
    let second = open_manager(cx, &first.root, kc.clone());
    assert_eq!(second.entries(cx), ["one.example", "two.example"]);

    first
        .window
        .update(cx, |view, _, cx| view.ask_remove(0, cx))
        .unwrap();
    assert!(first.confirming(cx));
    first
        .window
        .update(cx, |view, window, cx| view.confirm(window, cx))
        .unwrap();
    cx.run_until_parked();
    assert_eq!(first.entries(cx), ["two.example"]);
    assert_eq!(
        first.released.borrow().as_slice(),
        [InstanceId(0)],
        "its window and client are released first"
    );
    assert!(
        kc.accounts().is_empty(),
        "the key is deleted from the keychain"
    );
    assert!(!cfg0.data_dir.exists(), "the data is deleted");

    // The stale window connects a third address: the removed instance does not come back.
    second.type_address(cx, "https://three.example");
    second.connect(cx);
    assert_eq!(second.entries(cx), ["three.example", "two.example"]);
    let on_disk = Instances::load(&root).unwrap();
    let servers: Vec<_> = on_disk.list().iter().map(|c| label_of(&c.server)).collect();
    assert_eq!(servers, ["three.example", "two.example"]);
    // ... and opening the instance the first window removed opens nothing stale.
    second
        .window
        .update(cx, |view, window, cx| view.open_at(5, window, cx))
        .unwrap();
}

/// Delete asks first; Cancel keeps everything, and a refusal of the keychain keeps the instance and says so.
#[gpui::test]
fn remove_needs_a_confirmation(cx: &mut TestAppContext) {
    let root = root();
    let kc = Arc::new(MemoryKeychain::new());
    let app = open_manager(cx, &root, kc);
    app.type_address(cx, "https://one.example");
    app.connect(cx);
    app.window
        .update(cx, |view, _, cx| view.ask_remove(0, cx))
        .unwrap();
    assert!(app.confirming(cx));
    app.window
        .update(cx, |view, window, cx| view.cancel_confirmation(window, cx))
        .unwrap();
    cx.run_until_parked();
    assert_eq!(app.entries(cx), ["one.example"]);
    assert!(app.released.borrow().is_empty());
}

/// An open window holds its instance: removing it releases it first (the window and the client), and a second hold,
/// as another process's window would take, is refused while the first is open.
#[gpui::test]
fn an_open_instance_is_held_until_released(cx: &mut TestAppContext) {
    let root = root();
    let app = open_manager(cx, &root, Arc::new(MemoryKeychain::new()));
    app.type_address(cx, "https://one.example");
    app.connect(cx);
    let id = app.opened.borrow()[0].0.id;
    let instances = Instances::load(&root).unwrap();
    let guard = instances.hold(id).unwrap();
    assert!(matches!(
        instances.hold(id),
        Err(dilla_client::instances::InstanceError::InUse)
    ));
    // The manager's removal while another window holds it is refused with the in-use text.
    app.window
        .update(cx, |view, _, cx| view.ask_remove(0, cx))
        .unwrap();
    app.window
        .update(cx, |view, window, cx| view.confirm(window, cx))
        .unwrap();
    cx.run_until_parked();
    assert_eq!(app.entries(cx), ["one.example"], "a held instance stays");
    assert_eq!(app.error(cx).as_deref(), Some(copy::ERROR_IN_USE));
    drop(guard);
}

/// Keyboard: Tab runs the address field, Connect, the invite field, Join, and then the list, which is one tab stop that
/// Up, Down, Home and End rove.
#[gpui::test]
fn keyboard_instances_focus_order(cx: &mut TestAppContext) {
    let root = root();
    let app = open_manager(cx, &root, Arc::new(MemoryKeychain::new()));
    for server in [
        "https://a.example",
        "https://b.example",
        "https://c.example",
    ] {
        app.type_address(cx, server);
        app.connect(cx);
    }
    let [address, invite] = app.window.read_with(cx, |v, cx| v.field_focus(cx)).unwrap();
    let mut vcx = VisualTestContext::from_window(app.window.into(), cx);
    vcx.run_until_parked();
    assert!(
        vcx.update(|window, _| address.is_focused(window)),
        "the address field starts focused"
    );
    let mut order = Vec::new();
    for _ in 0..6 {
        vcx.simulate_keystrokes("tab");
        let (a, i) =
            vcx.update(|window, _| (address.is_focused(window), invite.is_focused(window)));
        order.push(match (a, i) {
            (true, _) => "address",
            (_, true) => "invite",
            _ => "other",
        });
    }
    // Connect (a button), the invite field, Join (a button), the list's one stop, then back around.
    assert_eq!(order[..3], ["other", "invite", "other"], "{order:?}");
    assert_eq!(order[3], "other", "the list is one stop: {order:?}");
    assert_eq!(order[4], "address", "then back around: {order:?}");
}

/// Keyboard: the list is roving; Enter opens; Delete removes after a confirmation.
#[gpui::test]
fn keyboard_list_roves_opens_and_deletes(cx: &mut TestAppContext) {
    let root = root();
    let kc = Arc::new(MemoryKeychain::new());
    let app = open_manager(cx, &root, kc);
    for server in [
        "https://a.example",
        "https://b.example",
        "https://c.example",
    ] {
        app.type_address(cx, server);
        app.connect(cx);
    }
    let before = app.opened.borrow().len();
    let mut vcx = VisualTestContext::from_window(app.window.into(), cx);
    // Focus the first row: tab to the list's stop.
    for _ in 0..4 {
        vcx.simulate_keystrokes("tab");
    }
    vcx.simulate_keystrokes("down down");
    vcx.simulate_keystrokes("enter");
    assert_eq!(app.opened.borrow().len(), before + 1);
    assert_eq!(
        app.opened.borrow()[before].0.server.as_str(),
        "https://c.example/",
        "Down twice, Enter opens the third"
    );
    vcx.simulate_keystrokes("home enter");
    assert_eq!(
        app.opened.borrow()[before + 1].0.server.as_str(),
        "https://a.example/"
    );
    vcx.simulate_keystrokes("end down enter");
    assert_eq!(
        app.opened.borrow()[before + 2].0.server.as_str(),
        "https://c.example/",
        "the list stops at its ends"
    );

    // Delete asks; Escape keeps it.
    vcx.simulate_keystrokes("up delete");
    assert!(app.confirming(&mut vcx));
    vcx.simulate_keystrokes("escape");
    assert!(!app.confirming(&mut vcx));
    assert_eq!(app.entries(&mut vcx).len(), 3);
}

/// From `shell.spec.ts` "closing a window keeps the client alive for background work": closing an instance's window
/// closes only the window. Its client keeps running and its hold stays, the manager takes the window's place, and
/// opening the instance again shows a window on the same client. Against a real dillad.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn closing_a_window_keeps_client_alive(cx: &mut TestAppContext) {
    use dilla_desktop::{Launch, windows};
    use std::time::{Duration, Instant};

    use support::world;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t27-close", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let alice = rt.block_on(world::sign_up(&host, &invite, "alice", "Alice", "t27"));
    world::clear_session(&alice);
    let root = alice
        .cfg
        .data_dir
        .parent()
        .and_then(Path::parent)
        .unwrap()
        .to_path_buf();
    let launch = Launch::live(Some(root.clone()), alice.kc.clone());
    cx.update(dilla_desktop::install);
    cx.executor().allow_parking();

    cx.update(|cx| windows::open_instance(cx, &launch, alice.cfg.clone(), None, false))
        .expect("the instance window opens");
    let id = alice.cfg.id;
    let started = Instant::now();
    let client = loop {
        cx.run_until_parked();
        if let Some(c) = cx.update(|cx| windows::running_client(cx, id)) {
            break c;
        }
        assert!(
            started.elapsed() < Duration::from_secs(60),
            "the client started"
        );
        std::thread::sleep(Duration::from_millis(50));
    };
    assert_eq!(cx.windows().len(), 1);

    // Close the instance's window.
    let window = cx.windows()[0];
    window
        .update(cx, |_, window, _| window.remove_window())
        .unwrap();
    cx.run_until_parked();
    let again = cx
        .update(|cx| windows::running_client(cx, id))
        .expect("the client survives its window");
    assert!(Rc::ptr_eq(&client, &again));
    assert_eq!(
        cx.windows().len(),
        1,
        "the manager takes the closed window's place"
    );
    // The instance is still held, so no other window or process opens it.
    let instances = Instances::load(&root).unwrap();
    assert!(matches!(
        instances.hold(id),
        Err(dilla_client::instances::InstanceError::InUse)
    ));

    // Opening it again shows a window on the same client.
    cx.update(|cx| windows::open_instance(cx, &launch, alice.cfg.clone(), None, false))
        .unwrap();
    cx.run_until_parked();
    let third = cx.update(|cx| windows::running_client(cx, id)).unwrap();
    assert!(Rc::ptr_eq(&client, &third), "no second client was started");
    drop((client, again, third));
    cx.update(windows::release_all);
}
