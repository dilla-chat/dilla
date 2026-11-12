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
        let sub = cx.subscribe(&host, move |_, e: &SettingsEvent, _| {
            let SettingsEvent::Navigate(route) = e;
            log.borrow_mut().push(route.clone());
        });
        std::mem::forget(sub);
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
    let (host, vcx, routes) = host(cx, Some(UserSection::Profile), None);
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
    assert_eq!(section(&host, vcx), UserSection::Profile);
    assert!(routes.borrow().is_empty());
    let prompt = ws.borrow().prompt().expect("the unsaved-changes prompt");
    assert_eq!(prompt.cancel_label, "Keep editing");
    assert_eq!(prompt.confirm_label, "Discard changes");

    // Keep editing (Escape in the prompt): the draft stays, still Profile, and focus is back in the frame.
    vcx.simulate_keystrokes("escape");
    vcx.run_until_parked();
    assert!(!ws.borrow().prompt_open());
    assert_eq!(section(&host, vcx), UserSection::Profile);
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
