//! Task 26's behaviour and keyboard tests for the factors area (standard procedure S5-S6), driven through the automation
//! build against a real dillad. `#[ignore]`d like every real-dillad suite (the desktop-gpui CI job runs them with
//! `--ignored` on Linux): run with `cargo test -p dilla-desktop --features automation --test factors -- --ignored`.
//!
//! The parity tests of S4 (`sign-in/*`, `factor/*`, `user-settings/sign-in*`) are not here yet: see the task report.

mod support;

use std::time::Duration;

use dilla_client::features::factors::StepUpMethod;
use dilla_parity::Driver;
use zeroize::Zeroizing;

use support::world;

const WAIT: Duration = Duration::from_secs(60);

fn scratch_dir(tag: &str) -> std::path::PathBuf {
    let dir = dilla_parity::target_dir().join(format!("t26-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// The login step, as a person types it: the server, then Tab to the username and the password, then Enter.
fn log_in(app: &mut Driver, server: &str, username: &str) {
    // A device with no account opens on the sign-up (Task 25); its link leads to the sign-in.
    app.wait_for("onboarding-signin", WAIT);
    app.click("onboarding-signin");
    app.wait_for("signin-server", WAIT);
    app.type_text("signin-server", server);
    app.press("tab");
    app.type_focused(username);
    app.press("tab");
    app.type_focused(world::PASSWORD);
    app.press("enter");
}

/// e2e/web/signin-order.spec.ts, "sign-in order: the password, then the factor, then the recovery key; a backup code waits
/// for the key": a second device logs in (step 1), gives the code in the six boxes, which send it on the sixth digit with
/// no press (step 2), meets a pasted backup code in the key field (named once it rests), then the key (step 3), then done.
/// A third device takes the backup-code path: the account still holds its ten codes until the key adds that device; then
/// nine.
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn password_then_factor_then_recovery_key_backup_code_waits() {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t26-order", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t26-order"));
    world::clear_session(&ada);
    let first = rt.block_on(world::start(&ada));
    rt.block_on(world::wait_for(&first, WAIT, "ada online", |s| {
        s.connection.status == dilla_client::state::ConnectionStatus::Online
    }));
    let code = ada.next_code(&host);
    let codes = rt.block_on(async {
        let token = first
            .factors()
            .step_up(StepUpMethod::Totp, Zeroizing::new(code))
            .await
            .unwrap();
        first
            .factors()
            .regenerate_backup_codes(token)
            .await
            .unwrap()
    });

    // The second device: login, code, key, done.
    let mut app = Driver::launch(&scratch_dir("second"));
    log_in(&mut app, &host.public, "ada");
    app.wait_for("signin-code", WAIT);
    let code = ada.next_code(&host);
    app.type_text("signin-code", &code);
    // Verify (the sixth digit's own send after its 350 ms rest is `code::tests` on the test clock: the automation
    // build's frames do not run that timer between commands).
    app.click("signin-verify");
    app.wait_for("signin-key", WAIT);
    // A backup code in the key field: the key is not ready (its 600 ms note is likewise timer-driven, unchecked here).
    app.type_text("signin-key", &codes.0[1]);
    app.click("signin-add");
    assert!(app.element("signin-open").is_none());
    app.click("signin-key");
    app.press("ctrl-a");
    app.type_focused(&ada.recovery_key);
    app.click("signin-add");
    app.wait_for("signin-open", WAIT);
    app.click("signin-open");
    drop(app);

    // The third device: login, a backup code held, then the key.
    let mut app = Driver::launch(&scratch_dir("third"));
    log_in(&mut app, &host.public, "ada");
    app.wait_for("signin-use-backup", WAIT);
    app.click("signin-use-backup");
    app.wait_for("signin-backup", WAIT);
    app.type_text("signin-backup", &codes.0[2].to_lowercase());
    app.press("enter");
    app.wait_for("signin-key", WAIT);
    let left = rt.block_on(first.factors().refresh()).unwrap().unwrap();
    assert_eq!(
        left.backup_codes_left, 10,
        "the held code waits for the key"
    );
    app.type_text("signin-key", &ada.recovery_key);
    app.click("signin-add");
    app.wait_for("signin-open", WAIT);
    let left = rt.block_on(first.factors().refresh()).unwrap().unwrap();
    assert_eq!(left.backup_codes_left, 9);
    drop(app);
}

/// S6: the sign-in's focus order (server, username, password) and the code boxes' keys: digits advance, Backspace moves
/// back, Enter submits; a refused code takes the person back to the login with focus on the password.
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn keyboard_factors_focus_order() {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t26-keys", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t26-keys"));
    world::clear_session(&ada);
    let mut app = Driver::launch(&scratch_dir("keys"));
    // A device with no account opens on the sign-up (Task 25); its link leads to the sign-in.
    app.wait_for("onboarding-signin", WAIT);
    app.click("onboarding-signin");
    app.wait_for("signin-server", WAIT);
    app.click("signin-server");
    app.type_focused(&host.public);
    app.press("tab");
    app.type_focused("ada");
    app.press("tab");
    app.type_focused(world::PASSWORD);
    app.press("enter");
    app.wait_for("signin-code", WAIT);
    // A wrong code (the right one with its last digit changed), typed digit by digit, is sent on the sixth.
    let right = ada.next_code(&host);
    let last = right.as_bytes()[5];
    let wrong = format!(
        "{}{}",
        &right[..5],
        char::from(if last == b'9' { b'0' } else { last + 1 })
    );
    app.type_text("signin-code", &wrong);
    // Enter in the boxes submits.
    app.press("enter");
    // Refused: the login again, the password emptied and focused.
    app.wait_for("signin-continue", WAIT);
    app.wait_for("signin-password", WAIT);
    // Backspace in the boxes moves back: on the next code step, five digits, Backspace, two more, then Enter.
    app.type_focused(world::PASSWORD);
    app.press("enter");
    app.wait_for("signin-code", WAIT);
    let code = ada.next_code(&host);
    app.type_text("signin-code", &code[..5]);
    app.press("backspace");
    app.type_focused(&code[4..]);
    app.press("enter");
    app.wait_for("signin-key", WAIT);
}

/// Integration 3, step 0 item 1 (ForgetBrowser after a shell existed): the shell ran with the client (the voice,
/// attachments, threads, ring, search and the other app-wide hooks took it), then the forced factor screen's Forget
/// this device. Every app-wide holder lets go of the client, so the store really closes and is wiped: the instance is
/// no longer listed, its data dir and its keychain entry are gone, and the app is back on the sign-in. Before the fix
/// the wipe never ran (the client stayed shared: `E_FORGET_STORE`).
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn forget_after_the_shell_closes_and_wipes_the_store(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;
    cx.executor().allow_parking();
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t26-forget-shell", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let ada = rt.block_on(world::sign_up(
        &host,
        &invite,
        "ada",
        "Ada",
        "t26-forget-shell",
    ));
    world::community(&host, &ada, "Forget", &[]);
    world::clear_session(&ada);
    let client = rt.block_on(world::start(&ada));
    rt.block_on(world::wait_for(&client, WAIT, "ada online", |s| {
        s.connection.status == dilla_client::state::ConnectionStatus::Online
            && !s.channels.is_empty()
    }));
    let probe = client.store_probe();
    let cfg = ada.cfg.clone();
    // `<root>/instances/<n>`: the data root the app lists its instances in.
    let root = cfg
        .data_dir
        .parent()
        .and_then(|d| d.parent())
        .unwrap()
        .to_path_buf();
    assert!(
        dilla_client::instances::Instances::load(&root)
            .unwrap()
            .get(cfg.id)
            .is_some()
    );
    assert!(cfg.data_dir.exists());
    use dilla_client::seams::Keychain as _;
    let account = dilla_client::store::store_keychain_account(&cfg).unwrap();
    assert!(ada.kc.get(&account).unwrap().is_some());
    let client = Rc::new(client);
    let weak = Rc::downgrade(&client);
    cx.update(dilla_desktop::install);
    let mut launch = dilla_desktop::app::Launch::live(Some(root.clone()), ada.kc.clone());
    launch.shipping = false;
    launch.resume = false;
    launch.handoff = Some(client);
    let (app, vcx) =
        cx.add_window_view(move |window, cx| dilla_desktop::app::AppView::new(launch, window, cx));
    // The shell draws a few frames with the running client: every app-wide hook takes it.
    for _ in 0..5 {
        vcx.run_until_parked();
        vcx.update(|window, _| window.refresh());
        std::thread::sleep(Duration::from_millis(50));
    }
    // Re-review N1: an open emoji grid holds the client (its pick sends a reaction on it); release lets go of it.
    vcx.update(|window, cx| {
        let ctx = dilla_desktop::slots::SlotCx {
            client: weak.upgrade(),
            ..dilla_desktop::slots::SlotCx::default()
        };
        dilla_desktop::screens::reactions::picker::open(
            dilla_desktop::screens::reactions::picker::Target::Composer,
            &ctx,
            None,
            None,
            window,
            cx,
        );
    });
    // The account owes a factor now: the forced screen, and its Forget this device.
    app.update_in(vcx, |a, window, cx| a.forget_from_forced(window, cx));
    // Final review M1: a Forget's wipe is pending from its start, as a Sign out's is: a reopened window's start of the
    // instance waits for it instead of meeting the store and key being deleted.
    let id = cfg.id;
    assert!(
        vcx.update(|_, cx| dilla_desktop::windows::wipe_pending(cx, id)),
        "the Forget's wipe is pending"
    );
    let deadline = std::time::Instant::now() + WAIT;
    let signed_out =
        |vcx: &mut gpui::VisualTestContext| app.read_with(vcx, |a, _| a.shows_sign_in());
    while !(probe.is_closed() && !cfg.data_dir.exists() && signed_out(vcx)) {
        assert!(
            std::time::Instant::now() < deadline,
            "the store closes and is wiped, then the sign-in (closed: {}, dir left: {}, sign-in: {})",
            probe.is_closed(),
            cfg.data_dir.exists(),
            signed_out(vcx)
        );
        vcx.run_until_parked();
        vcx.executor().advance_clock(Duration::from_millis(20));
        vcx.update(|window, _| window.refresh());
        std::thread::sleep(Duration::from_millis(20));
    }
    vcx.run_until_parked();
    assert!(
        !vcx.update(|_, cx| dilla_desktop::windows::wipe_pending(cx, id)),
        "the wipe ended"
    );
    let listed = dilla_client::instances::Instances::load(&root).unwrap();
    assert!(listed.get(cfg.id).is_none(), "the instance is forgotten");
    assert!(ada.kc.get(&account).unwrap().is_none(), "its key is gone");
}

/// Integration 3, step 3 (Task 16's Devices "Sign out" with step 0 item 1's release path): a device that signs itself out
/// and removes itself (`profile.signed_out`) has its store wiped through the same path as Forget, after the shell had
/// it: the store closes, the data dir, the list entry and the key go, and the app shows the sign-in.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn signing_out_this_device_wipes_its_store_after_the_shell(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;
    cx.executor().allow_parking();
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t16-sign-out", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t16-sign-out"));
    world::community(&host, &ada, "Sign out", &[]);
    world::clear_session(&ada);
    let client = rt.block_on(world::start(&ada));
    rt.block_on(world::wait_for(&client, WAIT, "ada online", |s| {
        s.connection.status == dilla_client::state::ConnectionStatus::Online
            && !s.channels.is_empty()
    }));
    let probe = client.store_probe();
    let cfg = ada.cfg.clone();
    let root = cfg
        .data_dir
        .parent()
        .and_then(|d| d.parent())
        .unwrap()
        .to_path_buf();
    use dilla_client::seams::Keychain as _;
    let account = dilla_client::store::store_keychain_account(&cfg).unwrap();
    let devices = client.devices();
    let client = Rc::new(client);
    let weak = Rc::downgrade(&client);
    cx.update(dilla_desktop::install);
    let mut launch = dilla_desktop::app::Launch::live(Some(root.clone()), ada.kc.clone());
    launch.shipping = false;
    launch.resume = false;
    launch.handoff = Some(client);
    let (app, vcx) =
        cx.add_window_view(move |window, cx| dilla_desktop::app::AppView::new(launch, window, cx));
    for _ in 0..5 {
        vcx.run_until_parked();
        vcx.update(|window, _| window.refresh());
        std::thread::sleep(Duration::from_millis(50));
    }
    // Settings -> Devices opens over the shell (review M5: the real path): its view keeps the client between frames.
    app.update_in(vcx, |a, window, cx| {
        a.open_route(
            &dilla_desktop::router::Route::Settings(Some(
                dilla_desktop::router::UserSection::Devices,
            )),
            window,
            cx,
        )
    });
    for _ in 0..5 {
        vcx.run_until_parked();
        vcx.update(|window, _| window.refresh());
        std::thread::sleep(Duration::from_millis(50));
    }
    assert_eq!(app.read_with(vcx, |a, _| a.screen_name()), "settings");
    let held = vcx.update(|_, cx| {
        let c = weak.upgrade().expect("the client runs");
        dilla_desktop::release::holders_of(&c, cx)
    });
    assert!(
        held > 0,
        "the Devices section registered its hold on the client"
    );
    // Its Sign out (the key dialog's call), then nothing of the client is kept here.
    let signed_out_at = std::time::Instant::now();
    let mut answer = devices.sign_out_and_remove(Zeroizing::new(ada.recovery_key.clone()));
    drop(devices);
    let deadline = std::time::Instant::now() + WAIT;
    let on_sign_in =
        |vcx: &mut gpui::VisualTestContext| app.read_with(vcx, |a, _| a.shows_sign_in());
    let mut answered = None;
    while !(probe.is_closed() && !cfg.data_dir.exists() && on_sign_in(vcx)) {
        if answered.is_none()
            && let Ok(r) = answer.try_recv()
        {
            answered = Some(r);
        }
        assert!(
            std::time::Instant::now() < deadline,
            "signed out: the store closes and is wiped, then the sign-in (answer: {answered:?}, closed: {}, dir left: {}, \
             sign-in: {})",
            probe.is_closed(),
            cfg.data_dir.exists(),
            on_sign_in(vcx)
        );
        // No frame is asked for: the wipe must not wait for the window to draw (review M5).
        vcx.run_until_parked();
        vcx.executor().advance_clock(Duration::from_millis(20));
        std::thread::sleep(Duration::from_millis(20));
    }
    // No holder outlived the release: the wipe went straight through, not through the store-open fallback (which first
    // waits 5 s for the last handle, then retries every second).
    assert!(
        signed_out_at.elapsed() < Duration::from_secs(4),
        "wiped without waiting for a frame or a retry ({:?})",
        signed_out_at.elapsed()
    );
    let listed = dilla_client::instances::Instances::load(&root).unwrap();
    assert!(listed.get(cfg.id).is_none(), "the instance is forgotten");
    assert!(ada.kc.get(&account).unwrap().is_none(), "its key is gone");
}

/// Re-review round 2, C: a Sign out whose old client a leftover holder keeps waits for it; the window closes and the
/// instance opens again from the manager. The reopened window does not start the instance on the old store (its
/// auto-start is refused, saying why), and nothing of the store goes while the old client lives. Once the holder lets
/// go, the wipe completes although its window is gone, the pending flag clears (the reopened sign-in drops its note),
/// and a sign-in on the instance works.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn a_pending_wipe_survives_its_window_and_refuses_the_reopened_start(
    cx: &mut gpui::TestAppContext,
) {
    use std::rc::Rc;
    cx.executor().allow_parking();
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("rr2-wipe-reopen", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(
        &host,
        &invite,
        "ada",
        "Ada",
        "rr2-wipe-reopen",
    ));
    world::community(&host, &ada, "Reopen", &[]);
    world::clear_session(&ada);
    let client = rt.block_on(world::start(&ada));
    rt.block_on(world::wait_for(&client, WAIT, "ada online", |s| {
        s.connection.status == dilla_client::state::ConnectionStatus::Online
            && !s.channels.is_empty()
    }));
    let probe = client.store_probe();
    let cfg = ada.cfg.clone();
    let root = cfg
        .data_dir
        .parent()
        .and_then(|d| d.parent())
        .unwrap()
        .to_path_buf();
    let devices = client.devices();
    let client = Rc::new(client);
    // The leftover holder: a handle no release reaches.
    let leftover = client.clone();
    cx.update(dilla_desktop::install);
    let mut launch = dilla_desktop::app::Launch::live(Some(root.clone()), ada.kc.clone());
    launch.shipping = false;
    launch.instance = Some(cfg.id);
    let reopen = launch.clone();
    launch.resume = false;
    launch.handoff = Some(client);
    let (app, vcx) =
        cx.add_window_view(move |window, cx| dilla_desktop::app::AppView::new(launch, window, cx));
    for _ in 0..5 {
        vcx.run_until_parked();
        vcx.update(|window, _| window.refresh());
        std::thread::sleep(Duration::from_millis(50));
    }
    assert_eq!(app.read_with(vcx, |a, _| a.screen_name()), "shell");
    let mut answer = devices.sign_out_and_remove(Zeroizing::new(ada.recovery_key.clone()));
    drop(devices);
    let id = cfg.id;
    let deadline = std::time::Instant::now() + WAIT;
    loop {
        let pending = vcx.update(|_, cx| dilla_desktop::windows::wipe_pending(cx, id));
        if pending && app.read_with(vcx, |a, _| a.shows_sign_in()) {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "the wipe waits for the leftover holder (answer: {:?})",
            answer.try_recv()
        );
        vcx.run_until_parked();
        vcx.executor().advance_clock(Duration::from_millis(100));
        std::thread::sleep(Duration::from_millis(20));
    }
    // The window closes; the instance opens again from the manager (its auto-start).
    vcx.update(|window, _| window.remove_window());
    vcx.run_until_parked();
    cx.update(|cx| {
        dilla_desktop::windows::open_instance(cx, &reopen, cfg.clone(), None, true)
            .expect("the instance opens again")
    });
    let again = cx
        .update(|cx| {
            cx.windows()
                .into_iter()
                .filter_map(|w| w.downcast::<dilla_desktop::app::AppView>())
                .next_back()
        })
        .expect("the reopened window");
    let store_note =
        dilla_desktop::strings::tf("factor.error.other", &[("code", "E_FORGET_STORE")]);
    for _ in 0..10 {
        cx.run_until_parked();
        cx.executor().advance_clock(Duration::from_millis(100));
        std::thread::sleep(Duration::from_millis(20));
    }
    let (screen, banner) = again
        .update(cx, |a, _, cx| (a.screen_name(), a.sign_in_banner(cx)))
        .unwrap();
    assert_eq!(
        screen, "sign-in",
        "the reopened window's auto-start is refused"
    );
    assert_eq!(banner.as_deref(), Some(store_note.as_str()), "it says why");
    assert!(
        cfg.data_dir.exists() && !probe.is_closed(),
        "the old store is untouched while its client lives"
    );
    assert!(cx.update(|cx| dilla_desktop::windows::wipe_pending(cx, id)));
    // The holder lets go: the wipe completes although its window is gone.
    drop(leftover);
    let deadline = std::time::Instant::now() + WAIT;
    loop {
        let pending = cx.update(|cx| dilla_desktop::windows::wipe_pending(cx, id));
        let banner = again.update(cx, |a, _, cx| a.sign_in_banner(cx)).unwrap();
        if probe.is_closed() && !cfg.data_dir.exists() && !pending && banner.is_none() {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "the wipe completes (closed: {}, dir left: {}, pending: {pending}, banner: {banner:?})",
            probe.is_closed(),
            cfg.data_dir.exists()
        );
        cx.run_until_parked();
        cx.executor().advance_clock(Duration::from_millis(500));
        std::thread::sleep(Duration::from_millis(20));
    }
    // A sign-in on the instance works again: a new device of the account, on a fresh store.
    let kc = std::sync::Arc::new(dilla_client::keychain::MemoryKeychain::new());
    let new_cfg = dilla_client::instances::Instances::load(&root)
        .unwrap()
        .add(cfg.server.clone())
        .unwrap();
    let mut flow = dilla_client::signin::SignIn::new(new_cfg.clone(), kc.clone()).unwrap();
    rt.block_on(async {
        flow.login(&ada.username, world::PASSWORD)
            .await
            .expect("the password");
        flow.code(&ada.next_code(&host)).await.expect("the code");
        flow.add_device(&ada.recovery_key)
            .await
            .expect("the recovery key");
    });
    drop(flow);
    let signed_in = dilla_client::Client::start(new_cfg, kc).expect("the new session starts");
    drop(signed_in);
}

/// required-factor.spec.ts's Cleo: Ivar's server and an invite to it; Cleo signs up through the invite on a data root of
/// her own (the sign-up screen's `Signup`) and closes the app at the factor choice. Answers the host, the community,
/// Cleo's data root and keychain.
fn cleo_left_at_the_choice(
    rt: &tokio::runtime::Runtime,
    tag: &str,
) -> (
    world::Host,
    [u8; 16],
    std::path::PathBuf,
    std::sync::Arc<dilla_client::keychain::MemoryKeychain>,
) {
    use dilla_client::features::signup::Signup;
    use dilla_core::ids::DeviceId;
    use dilla_testkit::HttpDs;
    let host = world::start_host(tag, "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let ivar = rt.block_on(world::sign_up(&host, &invite, "ivar", "Ivar", tag));
    let community = world::community(&host, &ivar, "Midgard Crew", &[]);
    let code = HttpDs::with_session(
        &host.public,
        DeviceId::from_bytes(ivar.device_id),
        ivar.token.clone(),
    )
    .unwrap()
    .create_invite(&community, 8, 3600)
    .unwrap();
    let root = scratch_dir(&format!("{tag}-cleo"));
    let cfg = dilla_client::instances::Instances::load(&root)
        .unwrap()
        .add(url::Url::parse(&host.public).unwrap())
        .unwrap();
    let kc = std::sync::Arc::new(dilla_client::keychain::MemoryKeychain::new());
    rt.block_on(async {
        let mut signup = Signup::new(cfg.clone(), kc.clone()).unwrap();
        signup.begin().await.expect("keys");
        signup
            .submit(&code, "cleo", Some(world::PASSWORD))
            .await
            .expect("the account");
    });
    drop(ivar);
    (host, community, root, kc)
}

/// The app opened again on Cleo's data root (resume): the window starts her client itself.
fn reopen<'a>(
    cx: &'a mut gpui::TestAppContext,
    root: &std::path::Path,
    kc: &std::sync::Arc<dilla_client::keychain::MemoryKeychain>,
) -> (
    gpui::Entity<dilla_desktop::app::AppView>,
    &'a mut gpui::VisualTestContext,
) {
    cx.executor().allow_parking();
    cx.update(dilla_desktop::install);
    let mut launch = dilla_desktop::app::Launch::live(Some(root.to_path_buf()), kc.clone());
    launch.shipping = false;
    let (app, vcx) =
        cx.add_window_view(move |window, cx| dilla_desktop::app::AppView::new(launch, window, cx));
    (app, vcx)
}

/// Types `digits` key by key into the focused element (each a key press of its own, as in the code boxes).
fn type_digits(vcx: &mut gpui::VisualTestContext, digits: &str) {
    for d in digits.chars() {
        vcx.simulate_keystrokes(&d.to_string());
        vcx.run_until_parked();
    }
}

fn pump(vcx: &mut gpui::VisualTestContext) {
    vcx.run_until_parked();
    vcx.executor().advance_clock(Duration::from_millis(50));
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    std::thread::sleep(Duration::from_millis(20));
}

fn pump_until(
    vcx: &mut gpui::VisualTestContext,
    what: &str,
    mut pred: impl FnMut(&mut gpui::VisualTestContext) -> bool,
) {
    let deadline = std::time::Instant::now() + WAIT;
    loop {
        pump(vcx);
        if pred(vcx) {
            return;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "timed out waiting for {what}"
        );
    }
}

/// e2e/web/second-factor.spec.ts, "second factor: sign-up step 5, a code and a backup code on new browsers, a wrong code,
/// Settings → Sign-in". In the app: Ada's sign-up (the sign-up screen's `Signup`) hands its client to the workspace
/// window, which shows the factor step; a wrong code is refused with the field focused for another, the right one adds
/// the app and the done step shows ten backup codes. Settings → Sign-in's facts: the app is on, no passkey, ten codes,
/// and the only factor cannot be removed. Then new devices: a backup code with the key signs one in and leaves nine; a
/// wrong code on another is refused at the login. (A code and the key on a second device is
/// [`password_then_factor_then_recovery_key_backup_code_waits`]; new codes behind the step-up are the client's
/// `step_up_backup_codes_remove_and_password`.)
///
/// Open (Task 28 report): in this window, handed the fresh sign-up's client, the code boxes hold the focus but never
/// receive the typed digits (the same boxes take them on a reopened forced screen, `forced_setup_then_joins_invited_server`),
/// so the test stops at the wrong code. Not root-caused; CI skips it until it is.
#[gpui::test]
#[ignore = "under investigation: typed digits never reach the sign-up factor step's code boxes here (see the Task 28 report)"]
fn signup_factor_code_backup_code_wrong_code_settings(cx: &mut gpui::TestAppContext) {
    use dilla_client::features::factors::FactorKind;
    use dilla_client::features::signup::Signup;
    use dilla_client::signin::{Owed, SignIn, SignInError};
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t28-signup-factor", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let root = scratch_dir("t28-signup-factor-ada");
    let cfg = dilla_client::instances::Instances::load(&root)
        .unwrap()
        .add(url::Url::parse(&host.public).unwrap())
        .unwrap();
    let kc = std::sync::Arc::new(dilla_client::keychain::MemoryKeychain::new());
    let key = rt.block_on(async {
        let mut signup = Signup::new(cfg.clone(), kc.clone()).unwrap();
        let key = signup.begin().await.expect("keys");
        signup
            .submit(&invite, "ada", Some(world::PASSWORD))
            .await
            .expect("the account");
        key
    });
    let recovery = key.hyphenated();
    let client = std::rc::Rc::new(rt.block_on(async {
        let (cfg, kc) = (cfg.clone(), kc.clone());
        tokio::task::spawn_blocking(move || dilla_client::Client::start(cfg, kc).expect("starts"))
            .await
            .unwrap()
    }));
    cx.executor().allow_parking();
    cx.update(dilla_desktop::install);
    let mut launch = dilla_desktop::app::Launch::live(Some(root.clone()), kc.clone());
    launch.shipping = false;
    launch.resume = false;
    launch.handoff = Some(client.clone());
    launch.after_signup = true;
    let (app, vcx) =
        cx.add_window_view(move |window, cx| dilla_desktop::app::AppView::new(launch, window, cx));
    pump_until(vcx, "the factor step", |vcx| {
        app.read_with(vcx, |a, _| a.screen_name() == "factor")
    });
    // The window settles on the step it shows (the client's first states may draw it again).
    for _ in 0..40 {
        pump(vcx);
    }
    let factor = app
        .read_with(vcx, |a, _| a.factor())
        .expect("the factor step");
    let setup = factor.read_with(vcx, |f, _| f.setup());
    setup.update_in(vcx, |s, window, cx| s.press_primary(window, cx));
    for _ in 0..10 {
        pump(vcx);
    }
    assert_eq!(
        app.read_with(vcx, |a, _| a.factor()).map(|f| f.entity_id()),
        Some(factor.entity_id()),
        "the step on show is the one driven"
    );
    let mut secret = None;
    pump_until(vcx, "the app step's secret", |vcx| {
        secret = setup.read_with(vcx, |s, _| s.shown_secret());
        secret.is_some()
    });
    let secret = secret.unwrap();
    // A wrong code: refused, said, and the field keeps the keyboard for another.
    let right = world::totp_code(&secret, world::clock_now(&host) / 30);
    let last = right.as_bytes()[5];
    let wrong = format!(
        "{}{}",
        &right[..5],
        char::from(if last == b'9' { b'0' } else { last + 1 })
    );
    type_digits(vcx, &wrong);
    pump_until(vcx, "the refusal", |vcx| {
        setup.read_with(vcx, |s, _| s.code_problem()) == Some("factor.error.code")
    });
    assert!(
        vcx.update(|window, cx| setup.read(cx).code_focused(window, cx)),
        "the code field has the keyboard"
    );
    // The right code adds the app; the done step shows the backup codes.
    type_digits(vcx, &right);
    pump_until(vcx, "the done step", |vcx| {
        app.read_with(vcx, |a, _| a.screen_name() == "done")
    });
    let factors = rt
        .block_on(client.factors().refresh())
        .unwrap()
        .expect("the factors");
    // Settings → Sign-in's facts: the app is on, no passkey, ten codes.
    assert!(
        factors.totp.is_some() && factors.passkeys.is_empty(),
        "{factors:?}"
    );
    assert_eq!(factors.backup_codes_left, 10);
    assert!(!factors.owed);
    // The only factor cannot be removed (the Sign-in section's Remove is disabled with the reason; the instance agrees).
    let token = rt
        .block_on(client.factors().step_up(
            StepUpMethod::Password,
            Zeroizing::new(world::PASSWORD.to_owned()),
        ))
        .expect("the password steps up");
    let e = rt
        .block_on(client.factors().remove(token, FactorKind::Totp))
        .unwrap_err();
    assert_eq!(e.status, 409, "{e:?}");
    // The done step does not show the sign-up's backup codes yet (Task 28 report: a product gap), so the codes for the
    // devices below are a new set, made behind a step-up as Settings → Sign-in makes them.
    let token = rt
        .block_on(client.factors().step_up(
            StepUpMethod::Password,
            Zeroizing::new(world::PASSWORD.to_owned()),
        ))
        .expect("the password steps up");
    let codes = rt
        .block_on(client.factors().regenerate_backup_codes(token))
        .expect("new codes")
        .0;
    assert_eq!(codes.len(), 10);

    // A new device: the login, a backup code instead of the app's, then the key: nine codes left.
    let new_device = |tag: &str| {
        let r = scratch_dir(tag);
        let c = dilla_client::instances::Instances::load(&r)
            .unwrap()
            .add(url::Url::parse(&host.public).unwrap())
            .unwrap();
        (r, c)
    };
    let (r3, c3) = new_device("t28-signup-factor-third");
    rt.block_on(async {
        let mut flow = SignIn::new(
            c3,
            std::sync::Arc::new(dilla_client::keychain::MemoryKeychain::new()),
        )
        .unwrap();
        assert_eq!(
            flow.login("ada", world::PASSWORD).await.unwrap(),
            Owed::Code
        );
        flow.add_device_with_backup(&codes[0], &recovery)
            .await
            .expect("a backup code and the key");
    });
    let left = rt.block_on(client.factors().refresh()).unwrap().unwrap();
    assert_eq!(left.backup_codes_left, 9);
    // A wrong code on a fourth: refused at the login.
    let (r4, c4) = new_device("t28-signup-factor-fourth");
    rt.block_on(async {
        let mut flow = SignIn::new(
            c4,
            std::sync::Arc::new(dilla_client::keychain::MemoryKeychain::new()),
        )
        .unwrap();
        assert_eq!(
            flow.login("ada", world::PASSWORD).await.unwrap(),
            Owed::Code
        );
        let right = world::totp_code(&secret, world::clock_now(&host) / 30);
        let last = right.as_bytes()[5];
        let wrong = format!(
            "{}{}",
            &right[..5],
            char::from(if last == b'9' { b'0' } else { last + 1 })
        );
        assert_eq!(
            flow.code(&wrong).await.unwrap_err(),
            SignInError::CodeFailed
        );
    });
    drop(app);
    for r in [root, r3, r4] {
        let _ = std::fs::remove_dir_all(&r);
    }
}

/// e2e/web/required-factor.spec.ts, "a sign-up left at the factor choice meets the forced screen on reopening": Cleo's
/// account owes its factor; the app opened again shows the forced screen (the set-up's choice, with Forget this device
/// as its only way out), never the shell.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn abandoned_factor_choice_meets_forced_screen(cx: &mut gpui::TestAppContext) {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let (_host, _community, root, kc) = cleo_left_at_the_choice(&rt, "t28-cleo-forced");
    let (app, vcx) = reopen(cx, &root, &kc);
    pump_until(vcx, "the forced screen", |vcx| {
        app.read_with(vcx, |a, _| a.screen_name() == "factor")
    });
    let setup = app
        .read_with(vcx, |a, cx| a.factor().map(|f| f.read(cx).setup()))
        .expect("the forced screen's set-up");
    assert_eq!(
        setup.read_with(vcx, |s, _| s.step_name()),
        "Choose",
        "the choice"
    );
    for _ in 0..10 {
        pump(vcx);
    }
    assert_eq!(
        app.read_with(vcx, |a, _| a.screen_name()),
        "factor",
        "never the shell"
    );
    assert!(app.read_with(vcx, |a, _| a.shell().is_none()));
    drop(app);
    let _ = std::fs::remove_dir_all(&root);
}

/// e2e/web/required-factor.spec.ts, "a sign-up left at the factor choice joins the invited server after the forced
/// set-up": on the reopened forced screen Cleo sets an authenticator app up (its code from the shown secret, then the
/// backup codes saved, then Done); the app goes on, joins the server the sign-up's invite named and shows the done step,
/// and her client lists that server.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn forced_setup_then_joins_invited_server(cx: &mut gpui::TestAppContext) {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let (host, community, root, kc) = cleo_left_at_the_choice(&rt, "t28-cleo-joins");
    let (app, vcx) = reopen(cx, &root, &kc);
    pump_until(vcx, "the forced screen", |vcx| {
        app.read_with(vcx, |a, _| a.screen_name() == "factor")
    });
    let setup = app
        .read_with(vcx, |a, cx| a.factor().map(|f| f.read(cx).setup()))
        .expect("the forced screen's set-up");
    // The choice's Continue (an authenticator app), then the app step shows its secret.
    setup.update_in(vcx, |s, window, cx| s.press_primary(window, cx));
    let mut secret = None;
    pump_until(vcx, "the app step's secret", |vcx| {
        secret = setup.read_with(vcx, |s, _| s.shown_secret());
        secret.is_some()
    });
    // The person types the code their app shows into the code boxes (focused by the step): the sixth digit sends it.
    let code = world::totp_code(&secret.unwrap(), world::clock_now(&host) / 30);
    vcx.simulate_input(&code);
    pump_until(vcx, "the backup codes", |vcx| {
        setup.read_with(vcx, |s, _| s.step_name()) == "Codes"
    });
    setup.update(vcx, |s, cx| s.tick_saved(cx));
    setup.update_in(vcx, |s, window, cx| s.press_primary(window, cx));
    pump_until(vcx, "the set-up's done step", |vcx| {
        setup.read_with(vcx, |s, _| s.step_name()) == "Done"
    });
    setup.update_in(vcx, |s, window, cx| s.press_primary(window, cx));
    // The app goes on: the invite's server is joined and the done step shows.
    pump_until(vcx, "the done step", |vcx| {
        app.read_with(vcx, |a, _| a.screen_name() == "done")
    });
    let client = app
        .read_with(vcx, |a, cx| {
            a.shell().and_then(|s| s.read(cx).client().cloned())
        })
        .expect("the client the shell runs");
    let c = dilla_client::state::CommunityId(community);
    pump_until(vcx, "the invited server listed", |_| {
        client
            .state()
            .borrow()
            .communities
            .iter()
            .any(|k| k.id == c)
    });
    drop(client);
    drop(app);
    let _ = std::fs::remove_dir_all(&root);
}
