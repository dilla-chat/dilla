//! Task 1b: the user settings host. `/settings/<section>` draws the frame with the section selected, the nav lists the
//! registry's sections in order, Escape closes to the route the settings were opened from, a dirty draft holds a section
//! switch behind Keep editing / Discard changes, and Tab reaches the nav.

use std::cell::RefCell;
use std::rc::Rc;

use dilla_desktop::edit_state::{Flags, Section};
use dilla_desktop::router::{Route, UserSection};
use dilla_desktop::screens::settings_host::{
    self, Dest, SettingsEvent, UserSettingsHost, nav_items,
};
use dilla_desktop::slots::SlotCx;
use dilla_desktop::strings::t;
use gpui::{Entity, TestAppContext, VisualTestContext};

fn host(
    cx: &mut TestAppContext,
    section: Option<UserSection>,
    from: Option<Route>,
) -> (
    Entity<UserSettingsHost>,
    &mut VisualTestContext,
    Rc<RefCell<Vec<Route>>>,
) {
    cx.update(|cx| {
        dilla_desktop::install(cx);
    });
    let (host, vcx) =
        cx.add_window_view(|_, cx| UserSettingsHost::new(section, from, SlotCx::default(), cx));
    let routes = Rc::new(RefCell::new(Vec::new()));
    let log = routes.clone();
    vcx.update(|_, cx| {
        cx.subscribe(&host, move |_, e: &SettingsEvent, _| {
            let SettingsEvent::Navigate(route) = e;
            log.borrow_mut().push(route.clone());
        })
        .detach();
    });
    vcx.run_until_parked();
    (host, vcx, routes)
}

fn section(host: &Entity<UserSettingsHost>, cx: &mut VisualTestContext) -> UserSection {
    host.read_with(cx, |h, _| h.section())
}

struct Draft(Rc<RefCell<usize>>);
impl Section for Draft {
    fn save(&mut self) -> bool {
        true
    }
    fn reset(&mut self) {
        *self.0.borrow_mut() += 1;
    }
}

#[gpui::test]
fn every_section_route_opens_the_host_on_that_section(cx: &mut TestAppContext) {
    for &s in UserSection::ALL {
        let route = Route::parse(&format!("/settings/{}", s.id()));
        let Route::Settings(parsed) = route else {
            panic!("`/settings/{}` is a settings route", s.id());
        };
        assert_eq!(parsed, Some(s));
        let (host, vcx, _) = host(cx, parsed, None);
        assert_eq!(section(&host, vcx), s);
        vcx.update(|window, _| window.remove_window());
    }
}

#[gpui::test]
fn bare_settings_opens_profile(cx: &mut TestAppContext) {
    let (host, vcx, _) = host(cx, None, None);
    assert_eq!(section(&host, vcx), UserSection::Profile);
}

#[test]
fn the_nav_lists_the_registry_in_order_under_you() {
    let items = nav_items();
    let ids: Vec<_> = items.iter().map(|i| i.id.to_string()).collect();
    let registry: Vec<_> = UserSection::ALL.iter().map(|s| s.id().to_owned()).collect();
    assert_eq!(ids, registry);
    assert_eq!(
        ids,
        [
            "profile",
            "signin",
            "devices",
            "notifications",
            "voice",
            "keyboard",
            "appearance",
            "privacy",
            "diagnostics"
        ]
    );
    assert!(
        items
            .iter()
            .all(|i| i.group.as_deref() == Some(t("workspace.group.you")))
    );
    assert_eq!(items[1].label.as_ref(), "Sign-in");
}

#[gpui::test]
fn escape_closes_back_to_the_previous_route(cx: &mut TestAppContext) {
    let from = Route::Dms;
    let (host, vcx, routes) = host(cx, Some(UserSection::Voice), Some(from.clone()));
    vcx.simulate_keystrokes("tab");
    vcx.simulate_keystrokes("escape");
    vcx.run_until_parked();
    assert_eq!(*routes.borrow(), vec![from]);
    assert_eq!(section(&host, vcx), UserSection::Voice);
}

#[gpui::test]
fn opened_cold_escape_closes_to_the_root(cx: &mut TestAppContext) {
    let (_host, vcx, routes) = host(cx, Some(UserSection::Profile), None);
    vcx.simulate_keystrokes("escape");
    vcx.run_until_parked();
    assert_eq!(*routes.borrow(), vec![Route::Root]);
}

#[gpui::test]
fn a_clean_section_switches_at_once(cx: &mut TestAppContext) {
    let (host, vcx, routes) = host(cx, Some(UserSection::Profile), None);
    host.update(vcx, |h, cx| {
        h.request(Dest::Section(UserSection::Devices), cx)
    });
    assert_eq!(section(&host, vcx), UserSection::Devices);
    assert_eq!(
        *routes.borrow(),
        vec![Route::Settings(Some(UserSection::Devices))]
    );
}

#[gpui::test]
fn a_dirty_draft_blocks_the_switch_until_discard(cx: &mut TestAppContext) {
    let (host, vcx, routes) = host(cx, Some(UserSection::Keyboard), None);
    let resets = Rc::new(RefCell::new(0));
    let ws = host.read_with(vcx, |h, _| h.workspace());
    ws.borrow_mut().attach(Box::new(Draft(resets.clone())));
    ws.borrow_mut().publish(Flags {
        dirty: true,
        busy: false,
        can_save: true,
    });
    host.update(vcx, |_, cx| cx.notify());
    vcx.run_until_parked();

    // The nav asks; the prompt opens and nothing moved.
    host.update(vcx, |h, cx| {
        h.request(Dest::Section(UserSection::Voice), cx)
    });
    vcx.run_until_parked();
    assert_eq!(section(&host, vcx), UserSection::Keyboard);
    assert!(routes.borrow().is_empty());
    let prompt = ws.borrow().prompt().expect("the unsaved-changes prompt");
    assert_eq!(prompt.cancel_label, "Keep editing");
    assert_eq!(prompt.confirm_label, "Discard changes");

    // Keep editing (Escape in the prompt): the draft stays, still Keyboard, and focus is back in the frame.
    vcx.simulate_keystrokes("escape");
    vcx.run_until_parked();
    assert!(!ws.borrow().prompt_open());
    assert_eq!(section(&host, vcx), UserSection::Keyboard);
    assert_eq!(*resets.borrow(), 0);

    // Escape in the frame is guarded the same way.
    vcx.simulate_keystrokes("escape");
    vcx.run_until_parked();
    assert!(ws.borrow().prompt_open());
    assert!(routes.borrow().is_empty());
    vcx.simulate_keystrokes("escape");
    vcx.run_until_parked();
    assert!(!ws.borrow().prompt_open());

    // Discard: the draft resets and the section switches.
    host.update(vcx, |h, cx| {
        h.request(Dest::Section(UserSection::Voice), cx)
    });
    vcx.run_until_parked();
    // The prompt opens on Cancel; Tab reaches Discard changes, Enter presses it.
    vcx.simulate_keystrokes("tab");
    vcx.simulate_keystrokes("enter");
    vcx.simulate_event(gpui::KeyUpEvent {
        keystroke: gpui::Keystroke::parse("enter").expect("a key"),
    });
    vcx.run_until_parked();
    assert_eq!(*resets.borrow(), 1);
    assert_eq!(section(&host, vcx), UserSection::Voice);
}

/// Tab reaches the nav's one tab stop (the current section); Down moves focus without choosing; Enter chooses.
#[gpui::test]
fn tab_reaches_the_nav_and_arrows_walk_it(cx: &mut TestAppContext) {
    let (host, vcx, routes) = host(cx, Some(UserSection::Signin), None);
    vcx.simulate_keystrokes("tab");
    vcx.run_until_parked();
    assert_eq!(
        section(&host, vcx),
        UserSection::Signin,
        "focus alone selects nothing"
    );
    vcx.simulate_keystrokes("down");
    assert_eq!(section(&host, vcx), UserSection::Signin);
    vcx.simulate_keystrokes("enter");
    vcx.simulate_event(gpui::KeyUpEvent {
        keystroke: gpui::Keystroke::parse("enter").expect("a key"),
    });
    vcx.run_until_parked();
    assert_eq!(section(&host, vcx), UserSection::Devices);
    assert_eq!(
        *routes.borrow(),
        vec![Route::Settings(Some(UserSection::Devices))]
    );
    let _ = settings_host::label(UserSection::Devices);
}

#[cfg(feature = "automation")]
#[test]
fn the_fixture_draws_the_host_for_a_ready_settings_route() {
    use dilla_client::testing::spec::Phase;
    use dilla_desktop::fixture::{FixtureScreen, screen_of};
    for &s in UserSection::ALL {
        assert_eq!(
            screen_of(&Route::Settings(Some(s)), Phase::Ready),
            Ok(FixtureScreen::Settings { section: s })
        );
    }
    assert!(screen_of(&Route::Settings(None), Phase::Ready).is_err());
}

/// What a section's hook does with `SlotCx::workspace` every frame: attach (idempotent) and publish.
fn hook(ctx: &SlotCx, resets: &Rc<RefCell<usize>>) {
    let ws = ctx.workspace.as_ref().expect("the host fills the guard");
    ws.borrow_mut().attach(Box::new(Draft(resets.clone())));
    ws.borrow_mut().publish(Flags {
        dirty: true,
        busy: false,
        can_save: true,
    });
}

#[gpui::test]
fn attaching_every_frame_keeps_the_draft(cx: &mut TestAppContext) {
    let (host, vcx, _) = host(cx, Some(UserSection::Keyboard), None);
    let (first, second) = (Rc::new(RefCell::new(0)), Rc::new(RefCell::new(0)));
    let ws = host.read_with(vcx, |h, _| h.workspace());
    let ctx = SlotCx {
        workspace: Some(ws.clone()),
        ..SlotCx::default()
    };
    hook(&ctx, &first);
    hook(&ctx, &second); // the next frame's attach must not replace the first
    ws.borrow_mut().reset();
    assert_eq!((*first.borrow(), *second.borrow()), (1, 0));
}

#[gpui::test]
fn two_windows_have_two_guards(cx: &mut TestAppContext) {
    let (a, acx, _) = host(cx, Some(UserSection::Profile), None);
    let a_ws = a.read_with(acx, |h, _| h.workspace());
    hook(
        &SlotCx {
            workspace: Some(a_ws.clone()),
            ..SlotCx::default()
        },
        &Rc::new(RefCell::new(0)),
    );
    assert!(a_ws.borrow().footer().is_some());
    let (b, bcx, _) = host(cx, Some(UserSection::Voice), None);
    let b_ws = b.read_with(bcx, |h, _| h.workspace());
    assert!(!Rc::ptr_eq(&a_ws, &b_ws));
    assert!(b_ws.borrow().footer().is_none());
    assert!(
        a_ws.borrow().footer().is_some(),
        "the second host left the first's draft alone"
    );
}

#[gpui::test]
fn clicking_a_nav_row_selects_it(cx: &mut TestAppContext) {
    let (host, vcx, routes) = host(cx, Some(UserSection::Profile), None);
    // Side column: 14px in, 28px down; the group heading takes 35px, each 34px row 36px with its gap: row 3 is Devices.
    let left = panel_left(vcx);
    vcx.simulate_click(
        gpui::point(left + gpui::px(114.0), gpui::px(152.0)),
        Default::default(),
    );
    vcx.run_until_parked();
    assert_eq!(section(&host, vcx), UserSection::Devices);
    assert_eq!(
        *routes.borrow(),
        vec![Route::Settings(Some(UserSection::Devices))]
    );
}

#[gpui::test]
fn clicking_close_closes(cx: &mut TestAppContext) {
    let (_host, vcx, routes) = host(cx, Some(UserSection::Profile), Some(Route::Activity));
    let left = panel_left(vcx);
    // The close button: 16px from the panel's right edge, 22px from the top, 36px wide.
    vcx.simulate_click(
        gpui::point(left + gpui::px(1246.0), gpui::px(40.0)),
        Default::default(),
    );
    vcx.run_until_parked();
    assert_eq!(*routes.borrow(), vec![Route::Activity]);
}

/// Integration 3, step 1: the host's render hands its own guard to the section it draws (`SlotCx::workspace`), never
/// a shared or a missing one.
#[gpui::test]
fn render_hands_the_section_this_hosts_guard(cx: &mut TestAppContext) {
    let (host, vcx, _) = host(cx, Some(UserSection::Diagnostics), None);
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    let (handed, own) = host.read_with(vcx, |h, _| (h.handed_workspace(), h.workspace()));
    let handed = handed.expect("the render filled SlotCx.workspace");
    assert!(Rc::ptr_eq(&handed, &own));
}

/// Integration 3, step 3: Task 16's Profile section draws in the host and registers its draft with the host's guard (its
/// flags are published on the first frame); another section registers none.
#[gpui::test]
fn the_profile_section_registers_with_the_hosts_guard(cx: &mut TestAppContext) {
    let (host, vcx, _) = host(cx, Some(UserSection::Profile), None);
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    let ws = host.read_with(vcx, |h, _| h.workspace());
    assert!(ws.borrow().flags().is_some(), "Profile published its flags");
    let (other, ovcx, _) = super_host(cx, UserSection::Keyboard);
    ovcx.update(|window, _| window.refresh());
    ovcx.run_until_parked();
    let ws = other.read_with(ovcx, |h, _| h.workspace());
    assert!(ws.borrow().flags().is_none(), "Keyboard has no draft");
}

fn super_host(
    cx: &mut TestAppContext,
    section: UserSection,
) -> (
    Entity<UserSettingsHost>,
    &mut VisualTestContext,
    Rc<RefCell<Vec<Route>>>,
) {
    host(cx, Some(section), None)
}

/// The app (integration 3, step 1): one settings host per window, whatever opens it again; Close takes back the screen
/// it was opened over; a Settings-to-Settings route switches the section through the dirty guard.
#[cfg(feature = "automation")]
mod app {
    use super::*;
    use dilla_desktop::app::{AppView, Launch};

    fn app(cx: &mut TestAppContext) -> (Entity<AppView>, &mut VisualTestContext) {
        cx.update(dilla_desktop::install);
        let (app, vcx) = cx.add_window_view(|window, cx| AppView::new(Launch::empty(), window, cx));
        vcx.run_until_parked();
        (app, vcx)
    }

    fn open(app: &Entity<AppView>, vcx: &mut VisualTestContext, section: Option<UserSection>) {
        app.update_in(vcx, |a, window, cx| {
            a.open_route(&Route::Settings(section), window, cx)
        });
        vcx.run_until_parked();
    }

    #[gpui::test]
    fn settings_hosts_never_stack(cx: &mut TestAppContext) {
        let (app, vcx) = app(cx);
        open(&app, vcx, None);
        let first = app.read_with(vcx, |a, _| a.settings_host()).unwrap().0;
        open(&app, vcx, Some(UserSection::Devices));
        open(&app, vcx, None);
        let (now, depth) = app.read_with(vcx, |a, _| a.settings_host()).unwrap();
        assert_eq!(depth, 1, "one host");
        assert_eq!(now.entity_id(), first.entity_id(), "the same host");
    }

    #[gpui::test]
    fn close_brings_back_the_screen_behind(cx: &mut TestAppContext) {
        let (app, vcx) = app(cx);
        let before = app.read_with(vcx, |a, _| a.screen_name());
        assert_ne!(before, "settings");
        open(&app, vcx, Some(UserSection::Keyboard));
        assert_eq!(app.read_with(vcx, |a, _| a.screen_name()), "settings");
        let host = app.read_with(vcx, |a, _| a.settings_host()).unwrap().0;
        host.update(vcx, |h, cx| h.request(Dest::Close, cx));
        vcx.run_until_parked();
        assert_eq!(app.read_with(vcx, |a, _| a.screen_name()), before);
        assert!(app.read_with(vcx, |a, _| a.settings_host()).is_none());
    }

    #[gpui::test]
    fn a_settings_route_over_settings_goes_through_the_guard(cx: &mut TestAppContext) {
        let (app, vcx) = app(cx);
        open(&app, vcx, Some(UserSection::Keyboard));
        let host = app.read_with(vcx, |a, _| a.settings_host()).unwrap().0;
        let ws = host.read_with(vcx, |h, _| h.workspace());
        let resets = Rc::new(RefCell::new(0));
        hook(
            &SlotCx {
                workspace: Some(ws.clone()),
                ..SlotCx::default()
            },
            &resets,
        );
        // A dirty draft: the route waits behind the prompt, the section stays.
        open(&app, vcx, Some(UserSection::Devices));
        assert_eq!(section(&host, vcx), UserSection::Keyboard);
        assert!(ws.borrow().prompt().is_some(), "Keep editing / Discard");
        assert_eq!(
            app.read_with(vcx, |a, cx| a.settings_route(cx)),
            Some(Route::Settings(Some(UserSection::Keyboard)))
        );
        // Discard: the waiting route goes through.
        host.update(vcx, |h, cx| h.discard(cx));
        vcx.run_until_parked();
        assert_eq!(section(&host, vcx), UserSection::Devices);
        assert_eq!(*resets.borrow(), 1, "the draft was reset by Discard");
    }
}

/// The frame's panel is at most 1280px wide and centred in the window.
fn panel_left(vcx: &mut VisualTestContext) -> gpui::Pixels {
    let width = vcx.update(|w, _| w.viewport_size().width);
    assert!(
        width >= gpui::px(1280.0),
        "the test window is {width:?} wide"
    );
    (width - gpui::px(1280.0)) / 2.0
}
