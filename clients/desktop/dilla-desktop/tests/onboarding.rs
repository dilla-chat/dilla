//! Task 25's parity, behaviour and keyboard tests for the onboarding area (standard procedure S4-S6).
//!
//! - Keyboard (S6): the sign-up's focus order and Enter on each step, Escape out of every create and join dialog, and
//!   the recovery key field taking a pasted key with hyphens.
//! - Behaviour (S5): `invite_name_written_key_lands_in_server` drives the app's sign-up against a real dillad up to the
//!   account on the instance. The factor (Task 26's views), the done step and the server landing it ends on, and the
//!   other e2e ports (`create.spec.ts`, `enrol.spec.ts`, `admin-keypackages.spec.ts`, `sidebar-actions.spec.ts`) need
//!   the shell's "+" and empty state (Task 8), the sign-in screen (Task 26) or a Bot (ruling R16): their client halves
//!   run in `dilla-client/tests/onboarding.rs`, and the window halves are on Task 28's list.
//! - Parity (S4): not yet (see the task report): the pre-client screens need a fixture route (`/welcome` draws the
//!   sign-in today) and the create states need the shell's "+".

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use dilla_client::features::enrol::check_key;
use dilla_desktop::screens::create::{self, Dialog, Open};
use dilla_desktop::screens::{FocusNext, FocusPrevious};
use dilla_desktop_ui::components::onboarding::RecoveryKeyField;
use dilla_parity::Driver;
use gpui::{
    AppContext as _, ClipboardItem, Context, Entity, InteractiveElement, IntoElement,
    ParentElement, Render, Styled, TestAppContext, VisualTestContext, Window, div,
};
use gpui_component::input::InputState;

mod support;
use support::world;

const WAIT: Duration = Duration::from_secs(60);

fn install(cx: &mut TestAppContext) {
    cx.update(|cx| {
        dilla_desktop::install(cx);
    });
}

/// Saves the app's frame as `<target>/onboarding-screens/<name>.png`.
fn keep(app: &mut Driver, name: &str) {
    let dir = dilla_parity::target_dir().join("onboarding-screens");
    std::fs::create_dir_all(&dir).unwrap();
    app.screenshot()
        .save(dir.join(format!("{name}.png")))
        .unwrap();
}

fn scratch_dir(tag: &str) -> PathBuf {
    let dir = world::repo_root().join(format!("target/t25-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// A window whose root draws the create flows' overlay, as the shell's `overlays` slot does.
struct OverlayHost;

impl Render for OverlayHost {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .id("host")
            .size_full()
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
            .children(create::overlay(window, cx))
    }
}

fn dialog_open(cx: &mut VisualTestContext) -> bool {
    cx.update(|window, cx| create::overlay(window, cx).is_some())
}

/// A frame whose body is `tall` px of content, with a probe at its foot that keeps where it was painted.
struct TallFrame {
    tall: f32,
    foot: std::rc::Rc<std::cell::Cell<Option<gpui::Bounds<gpui::Pixels>>>>,
}

impl Render for TallFrame {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        let foot = self.foot.clone();
        dilla_desktop_ui::components::onboarding::OnboardingFrame::new("tall", "Tall")
            .child(div().h(gpui::px(self.tall)))
            .child(
                gpui::canvas(move |b, _, _| foot.set(Some(b)), |_, _, _, _| {})
                    .w(gpui::px(10.0))
                    .h(gpui::px(10.0)),
            )
    }
}

/// The compact authentication window is fixed (Task 8): a step taller than it scrolls, as the web's sign-up card does,
/// so its foot (the action, the links) can be reached; a step that fits stays centred.
#[gpui::test]
fn a_step_taller_than_the_window_scrolls(cx: &mut TestAppContext) {
    install(cx);
    let foot = std::rc::Rc::new(std::cell::Cell::new(None));
    let probe = foot.clone();
    let (view, cx) = cx.add_window_view(move |_, _| TallFrame {
        tall: 900.0,
        foot: probe,
    });
    cx.simulate_resize(gpui::size(gpui::px(400.0), gpui::px(620.0)));
    cx.run_until_parked();
    let at = |cx: &mut VisualTestContext, view: &Entity<TallFrame>| {
        view.update(cx, |_, cx| cx.notify());
        cx.run_until_parked();
        cx.update(|window, cx| {
            window.refresh();
            window.draw(cx).clear(cx);
        });
        foot.get().expect("the foot was painted")
    };
    let before = at(cx, &view);
    assert!(
        f32::from(before.origin.y) > 620.0,
        "the foot starts below the window ({before:?})"
    );
    cx.simulate_event(gpui::ScrollEvent {
        position: gpui::point(gpui::px(200.0), gpui::px(300.0)),
        delta: gpui::ScrollDelta::Pixels(gpui::point(gpui::px(0.0), gpui::px(-2000.0))),
        ..Default::default()
    });
    let after = at(cx, &view);
    assert!(
        f32::from(after.origin.y + after.size.height) <= 620.0,
        "scrolled to the end, the foot is in the window ({after:?})"
    );
    // A short step stays centred: the foot's probe sits in the middle third of the window.
    view.update(cx, |v, cx| {
        v.tall = 0.0;
        cx.notify();
    });
    let short = at(cx, &view);
    let y = f32::from(short.origin.y);
    assert!(
        (620.0 / 3.0..620.0 * 2.0 / 3.0).contains(&y),
        "a step that fits is centred ({short:?})"
    );
}

/// Keyboard: Escape backs out of every create and join dialog, and the dialog's field holds focus when it opens.
#[gpui::test]
fn escape_backs_out_of_create_dialogs(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, _| OverlayHost);
    let community = dilla_client::state::CommunityId([7; 16]);
    for which in [
        Open::AddServer,
        Open::CreateServer,
        Open::Join {
            invite: Some("https://dilla.test/i/AB12".into()),
            refusal: None,
        },
        Open::CreateChannel {
            community,
            parent: None,
        },
        Open::Invite { community },
    ] {
        cx.update(|window, cx| {
            window.activate_window();
            create::open(which.clone(), cx);
        });
        host.update(cx, |_, cx| cx.notify());
        cx.run_until_parked();
        assert!(dialog_open(cx), "{which:?} opened");
        assert!(
            cx.update(|window, cx| window.focused(cx).is_some()),
            "{which:?} put focus in the dialog"
        );
        cx.simulate_keystrokes("escape");
        cx.run_until_parked();
        assert!(!dialog_open(cx), "Escape backed out of {which:?}");
    }
}

/// Keyboard: a choice of the add-server dialog, taken with Enter, opens its flow in the same overlay.
#[gpui::test]
fn add_server_choices_open_their_flows(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|_, _| OverlayHost);
    cx.update(|window, cx| {
        window.activate_window();
        create::open_add_server(cx);
    });
    host.update(cx, |_, cx| cx.notify());
    cx.run_until_parked();
    let current = |cx: &mut VisualTestContext| cx.update(|window, cx| create::current(window, cx));
    assert_eq!(current(cx), Some(Dialog::AddServer));
    // Focus starts on the first choice ("join a server"): Enter takes it.
    cx.simulate_keystrokes("enter");
    cx.simulate_event(gpui::KeyUpEvent {
        keystroke: gpui::Keystroke::parse("enter").unwrap(),
    });
    cx.run_until_parked();
    assert_eq!(
        current(cx),
        Some(Dialog::Join),
        "the join flow replaced the choice"
    );
    cx.simulate_keystrokes("escape");
    cx.run_until_parked();
    assert!(!dialog_open(cx));
}

/// Fix round 1 (item 3): a sign-up's join refused after the factor is offered again on "Open dilla": the join dialog
/// with the invite in its field and the refusal under it (App.tsx), which stays until the person types.
#[gpui::test]
fn a_refused_join_is_offered_again_with_its_invite(cx: &mut TestAppContext) {
    use dilla_client::features::signup::{FlowError, SignupResult};
    install(cx);
    let (host, cx) = cx.add_window_view(|_, _| OverlayHost);
    let joined = SignupResult {
        community: Some(dilla_client::state::CommunityId([1; 16])),
        join_error: None,
        invite: Some("AB12".into()),
    };
    let refused = SignupResult {
        community: None,
        join_error: Some(FlowError {
            code: "E_INVITE_INVALID".into(),
            status: 404,
        }),
        invite: Some("AB12".into()),
    };
    cx.update(|window, cx| {
        window.activate_window();
        assert!(
            !create::offer_refused_join(window, &joined, cx),
            "a join that worked offers nothing"
        );
        assert!(create::offer_refused_join(window, &refused, cx));
    });
    host.update(cx, |_, cx| cx.notify());
    cx.run_until_parked();
    let (dialog, refusal, value) = cx.update(|window, cx| {
        let flows = create::flows(window, cx).expect("the flows");
        let f = flows.read(cx);
        (f.dialog(), f.refusal(), f.invite_value(cx))
    });
    assert_eq!(dialog, Some(Dialog::Join));
    assert_eq!(value, "AB12");
    assert_eq!(
        refusal.as_deref(),
        Some(create::join_refusal("E_INVITE_INVALID").as_str())
    );
}

/// A window with the recovery key field.
struct KeyHost {
    input: Entity<InputState>,
}

impl Render for KeyHost {
    fn render(&mut self, _: &mut Window, _: &mut Context<Self>) -> impl IntoElement {
        div()
            .size_full()
            .child(RecoveryKeyField::new("key", "Recovery key", &self.input))
    }
}

/// Keyboard: the recovery key field takes a pasted key with hyphens in lower case, nothing cut off, and the client's
/// check counts all 52 characters.
#[gpui::test]
fn recovery_key_field_accepts_paste_with_hyphens(cx: &mut TestAppContext) {
    install(cx);
    let (host, cx) = cx.add_window_view(|window, cx| KeyHost {
        input: cx.new(|cx| InputState::new(window, cx)),
    });
    let input = host.read_with(cx, |h, _| h.input.clone());
    cx.update(|window, cx| input.update(cx, |s, cx| s.focus(window, cx)));
    let key = "abcd".repeat(13);
    let pasted = key
        .as_bytes()
        .chunks(4)
        .map(|c| std::str::from_utf8(c).unwrap())
        .collect::<Vec<_>>()
        .join("-");
    cx.update(|_, cx| cx.write_to_clipboard(ClipboardItem::new_string(pasted.clone())));
    cx.simulate_keystrokes("ctrl-v");
    cx.run_until_parked();
    let value = input.read_with(cx, |s, _| s.value().to_string());
    assert_eq!(value, pasted, "the paste is kept as typed, hyphens and all");
    assert_eq!(check_key(&value).count, 52);
}

/// Keyboard: the sign-up's focus order on an empty device (Tab from the invite field: Continue, then the sign-in link;
/// Shift+Tab back), and Enter with no invite stays on step 1 with the field's refusal.
#[test]
#[ignore = "launches the automation build (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn keyboard_onboarding_focus_order() {
    let root = scratch_dir("focus");
    let mut app = Driver::launch(&root);
    app.wait_for("onboarding-invite", WAIT);
    assert_eq!(app.focused().as_deref(), Some("onboarding-invite"));
    app.press("tab");
    assert_eq!(app.focused().as_deref(), Some("onboarding-next"));
    app.press("tab");
    assert_eq!(app.focused().as_deref(), Some("onboarding-signin"));
    app.press("shift-tab");
    assert_eq!(app.focused().as_deref(), Some("onboarding-next"));
    app.press("shift-tab");
    assert_eq!(app.focused().as_deref(), Some("onboarding-invite"));
    // Enter with no invite: the field's refusal, focus back in it, still step 1.
    app.press("enter");
    app.wait_idle();
    assert!(
        app.element("onboarding-username").is_none(),
        "still on step 1"
    );
    assert_eq!(app.focused().as_deref(), Some("onboarding-invite"));
    // The sign-in link leaves for the sign-in screen.
    app.click("onboarding-signin");
    app.wait_for("signin-server", WAIT);
    drop(app);
    let _ = std::fs::remove_dir_all(&root);
}

/// Fix round 1 (item 1): a pasted invite link names a host the person has not seen. An http link to a non-loopback host
/// and a link with user info are refused on step 1; a good link shows "Connect to <host>?" first, and until "Connect"
/// nothing is added to the device's instances and no request reaches the host. Cancel goes back with nothing stored.
#[test]
#[ignore = "launches the automation build (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn an_invite_link_is_confirmed_before_anything_is_stored_or_sent() {
    let listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    listener.set_nonblocking(true).unwrap();
    let port = listener.local_addr().unwrap().port();
    let root = scratch_dir("confirm");
    let instances = |root: &std::path::Path| {
        dilla_client::instances::Instances::load(root)
            .map(|l| l.list().len())
            .unwrap_or(0)
    };
    let mut app = Driver::launch(&root);
    app.wait_for("onboarding-invite", WAIT);
    for refused in [
        "http://dilla.example/i/AB12",
        "https://evil@dilla.example/i/AB12",
    ] {
        app.type_text("onboarding-invite", refused);
        app.press("enter");
        app.wait_idle();
        assert!(
            app.element("onboarding-confirm").is_none(),
            "{refused} refused"
        );
        assert!(app.element("onboarding-username").is_none());
        app.press("ctrl-a");
        app.press("backspace");
    }
    // The debug build takes http to the loopback (policy.ts allowLoopback).
    app.type_text(
        "onboarding-invite",
        &format!("http://127.0.0.1:{port}/i/AB12"),
    );
    app.press("enter");
    app.wait_for("onboarding-confirm", WAIT);
    std::thread::sleep(Duration::from_millis(500));
    assert_eq!(instances(&root), 0, "nothing stored before Connect");
    assert!(listener.accept().is_err(), "no request before Connect");
    app.click("onboarding-back");
    app.wait_for("onboarding-invite", WAIT);
    assert_eq!(instances(&root), 0, "Cancel stores nothing");
    assert!(listener.accept().is_err());
    // Connect: now the instance is added and asked.
    app.press("enter");
    app.wait_for("onboarding-confirm", WAIT);
    app.click("onboarding-next");
    let deadline = std::time::Instant::now() + WAIT;
    loop {
        if listener.accept().is_ok() {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "Connect reached the host"
        );
        std::thread::sleep(Duration::from_millis(100));
    }
    assert_eq!(instances(&root), 1);
    drop(app);
    let _ = std::fs::remove_dir_all(&root);
}

/// `e2e/web/signup.spec.ts` "a server invite, a name and a written-down key make an account that lands in that
/// server", in the window up to the account on the instance: the invite link, Enter; the name and password, Enter; the
/// key step refuses Enter until the key is ticked as saved; then the account exists (its owner signs in with the
/// password) and the app goes on to the client. The factor, the done step and the landing are Task 28's (Task 26's
/// factor views).
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
fn invite_name_written_key_lands_in_server() {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t25-dt-signup", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let (owner_client, link) = rt.block_on(async {
        let owner = world::sign_up(&host, &invite, "owner", "Owner", "t25-dt").await;
        world::clear_session(&owner);
        let client = world::start(&owner).await;
        world::wait_for(&client, WAIT, "ready", |s| {
            s.account.phase == dilla_client::state::AccountPhase::Ready
        })
        .await;
        let community = client
            .onboarding()
            .create_server("Midgard")
            .await
            .expect("server");
        client.refresh();
        world::wait_for(&client, WAIT, "listed", |s| {
            s.communities.iter().any(|c| c.id == community)
        })
        .await;
        let made = client
            .onboarding()
            .create_invite(community)
            .await
            .expect("invite");
        // The instance names itself `dilla.test` in its links; the person's link, as pasted, points at this host.
        (
            client,
            format!("{}/i/{}", host.public.trim_end_matches('/'), made.code),
        )
    });

    let root = scratch_dir("dt-signup");
    let mut app = Driver::launch(&root);
    app.wait_for("onboarding-invite", WAIT);
    app.type_text("onboarding-invite", &link);
    keep(&mut app, "1-invite");
    app.press("enter");
    // The host is shown for confirmation first; Enter (on Connect) goes on.
    app.wait_for("onboarding-confirm", WAIT);
    keep(&mut app, "1-confirm");
    app.press("enter");
    app.wait_idle();
    keep(&mut app, "1-after-enter");
    app.wait_for("onboarding-username", WAIT);
    keep(&mut app, "2-name");
    assert_eq!(app.focused().as_deref(), Some("onboarding-username"));
    app.type_focused("newbie");
    app.press("tab");
    assert_eq!(app.focused().as_deref(), Some("onboarding-password"));
    app.type_focused(world::PASSWORD);
    app.press("enter");
    app.wait_for("onboarding-key", WAIT);
    keep(&mut app, "3-key");
    assert_eq!(
        app.focused().as_deref(),
        Some("onboarding-ack"),
        "the key step starts on its tick"
    );
    // Not ticked: I saved it refuses, the key stays.
    app.click("onboarding-next");
    app.wait_idle();
    assert!(app.element("onboarding-key").is_some());
    // Ticked, I saved it creates the account.
    app.click("onboarding-ack");
    app.click("onboarding-next");
    let deadline = std::time::Instant::now() + WAIT;
    while app.element("onboarding-key").is_some() {
        assert!(
            std::time::Instant::now() < deadline,
            "the account was created"
        );
        std::thread::sleep(Duration::from_millis(200));
    }
    // The account exists: its password signs in (the login owes the factor the sign-up did not pay yet, or nothing).
    rt.block_on(async {
        let (cfg, kc) = {
            let other = scratch_dir("dt-signup-check");
            let mut instances = dilla_client::instances::Instances::load(&other).unwrap();
            let cfg = instances
                .add(url::Url::parse(&host.public).unwrap())
                .unwrap();
            (cfg, Arc::new(dilla_client::keychain::MemoryKeychain::new()))
        };
        let mut login = dilla_client::features::enrol::Enrol::new(cfg, kc).unwrap();
        login
            .login("newbie", world::PASSWORD)
            .await
            .expect("the new account signs in");
    });
    drop(app);
    let stopped = owner_client;
    std::thread::spawn(move || stopped.stop()).join().unwrap();
    let _ = std::fs::remove_dir_all(&root);
}

/// Final review M11: a create or join opened from a window's own control opens in that window, whichever window is
/// active: with two instance windows, the "+" of the one behind never runs through the front one's client.
#[gpui::test]
fn a_flow_opened_in_a_window_opens_there_not_in_the_active_one(cx: &mut TestAppContext) {
    install(cx);
    let community = dilla_client::state::CommunityId([7; 16]);
    let front = cx.add_window(|_, _| OverlayHost);
    let back = cx.add_window(|_, _| OverlayHost);
    front
        .update(cx, |_, window, _| window.activate_window())
        .unwrap();
    back.update(cx, |_, window, cx| {
        create::open_create_channel_in(window, community, cx)
    })
    .unwrap();
    for w in [front, back] {
        w.update(cx, |_, _, cx| cx.notify()).unwrap();
    }
    cx.run_until_parked();
    let current = |w: gpui::WindowHandle<OverlayHost>, cx: &mut TestAppContext| {
        w.update(cx, |_, window, cx| create::current(window, cx))
            .unwrap()
    };
    assert_eq!(current(back, cx), Some(Dialog::CreateChannel));
    assert_eq!(current(front, cx), None, "the active window opened nothing");
}
