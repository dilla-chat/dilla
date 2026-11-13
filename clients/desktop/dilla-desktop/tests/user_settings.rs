//! Task 16's parity, behaviour and keyboard tests for the user settings area (standard procedure S4-S6).
//!
//! - **Fixtures** (`parity/fixtures/user-settings/*.json`): every one parses, is named by its file, has a `-b` pair and a
//!   desktop window; [`parity_user_settings`] compares each with its web reference.
//! - **Behaviour** that needs no dillad lives with the pure models (`screens/user_settings/{draft,devices,keyboard}.rs`:
//!   `dirty_profile_draft_keep_editing_then_discard`; the table against the keymap is
//!   `keyboard_table_follows_the_registered_bindings` below); the two device flows (`remove_other_device_from_devices`,
//!   `sign_out_and_remove_leaves_account_and_group`) run against a real dillad in
//!   `dilla-client/tests/user_settings.rs`, where the client's calls are made (the app drives the same calls).
//! - **Keyboard**: the Profile form's tab order and the shortcuts table.

use std::path::PathBuf;

use dilla_client::testing::spec::ScreenFixture;
use dilla_desktop::router::UserSection;
use dilla_desktop::screens::FocusNext;
use dilla_desktop::screens::user_settings::UserSettings;
use dilla_desktop::screens::user_settings::keys::SHORTCUTS;
use dilla_desktop::slots::SlotCx;
use gpui::{
    AppContext, Context, InteractiveElement, IntoElement, ParentElement, Render, Styled,
    TestAppContext, Window, div,
};

const NAMES: [&str; 12] = [
    "appearance",
    "appearance-b",
    "devices",
    "devices-b",
    "devices-sign-out",
    "devices-sign-out-b",
    "keyboard",
    "keyboard-b",
    "profile",
    "profile-b",
    "profile-short-wide",
    "profile-short-wide-b",
];

fn fixture_path(name: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures/user-settings")
        .join(format!("{name}.json"))
}

/// S3: each state group has fixture A at 1280x800 and a varied fixture B, both of the plan's desktop sizes (the one short,
/// wide state is 1440x600: the docs' 844x390 phone-landscape shot is out of scope, plan Global Constraints).
#[test]
fn fixtures_are_valid_named_by_their_file_and_paired() {
    for name in NAMES {
        let text =
            std::fs::read_to_string(fixture_path(name)).unwrap_or_else(|e| panic!("{name}: {e}"));
        let f = ScreenFixture::parse(&text).unwrap_or_else(|e| panic!("{name}: {e}"));
        assert_eq!(f.name, format!("user-settings/{name}"));
        assert!(f.route.starts_with("/settings/"), "{name}: {}", f.route);
        assert!(
            f.window.0 >= 1280 && f.window.1 >= 560,
            "{name}: {:?}",
            f.window
        );
        if let Some(a) = name.strip_suffix("-b") {
            assert!(NAMES.contains(&a), "{name} has no A fixture");
        } else {
            assert!(
                NAMES.contains(&format!("{name}-b").as_str()),
                "{name} has no B fixture"
            );
        }
    }
    // Fixture B differs from A in at least 3 ways (window, a second server, long text or a step).
    let a =
        ScreenFixture::parse(&std::fs::read_to_string(fixture_path("profile")).unwrap()).unwrap();
    let b =
        ScreenFixture::parse(&std::fs::read_to_string(fixture_path("profile-b")).unwrap()).unwrap();
    assert_ne!(a.window, b.window);
    assert_eq!(a.world.servers.len() + 1, b.world.servers.len());
    assert_ne!(a.world.people[0].bio, b.world.people[0].bio);
}

/// S4: one parity test per fixture, drawn in the settings host (Task 1b, since integration 3) against the references
/// `shoot.mjs` shot. The ones still parked (ruling R22) carry their measured numbers; the two `-b` Profile fixtures have
/// no reference yet: the parity world cannot seed statuses and bios.
macro_rules! parity {
    ($($test:ident => $name:literal $(, $why:literal)?);* $(;)?) => {$(
        #[test]
        $(#[ignore = $why])?
        #[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
        fn $test() {
            let report = dilla_parity::screen::assert_screen(&fixture_path($name))
                .unwrap_or_else(|e| panic!("{} differs from its web reference: {e}", $name));
            assert!(report.text_lines > 0);
        }
    )*};
}

parity! {
    parity_user_settings_profile => "profile", "parity after T6c: whole frame 0.6915 % (the settings host's layout)";
    parity_user_settings_profile_b => "profile-b", "parity after T6c: n/a (no reference: the parity world cannot seed statuses and bios)";
    parity_user_settings_profile_short_wide => "profile-short-wide", "parity after T6c: whole frame 0.6555 % (the settings host's layout)";
    parity_user_settings_profile_short_wide_b => "profile-short-wide-b", "parity after T6c: n/a (no reference: the parity world cannot seed statuses and bios)";
    parity_user_settings_devices => "devices", "parity after T6c: whole frame 0.5385 % (device row text and times)";
    parity_user_settings_devices_b => "devices-b", "parity after T6c: chrome 0.7149 % (device rows)";
    parity_user_settings_devices_sign_out => "devices-sign-out", "parity after T6c: whole frame 1.6979 % (the key dialog)";
    parity_user_settings_devices_sign_out_b => "devices-sign-out-b", "parity after T6c: whole frame 1.3327 % (the key dialog)";
    parity_user_settings_appearance => "appearance";
    parity_user_settings_appearance_b => "appearance-b";
    parity_user_settings_keyboard => "keyboard";
    parity_user_settings_keyboard_b => "keyboard-b";
}

/// A window with the Profile section in it and Tab bound to the next focus, as the app's root does.
struct Host {
    view: gpui::Entity<UserSettings>,
}

impl Render for Host {
    fn render(&mut self, _window: &mut Window, _cx: &mut Context<Self>) -> impl IntoElement {
        div()
            .id("host")
            .size_full()
            .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
            .child(self.view.clone())
    }
}

/// S6: Tab goes through the Profile form in its order: display name, status message, bio.
#[gpui::test]
fn keyboard_user_settings_focus_order(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    let (host, cx) = cx.add_window_view(|window, cx| {
        let view = cx.new(|cx| {
            let mut v = UserSettings::new(window, cx);
            v.show(UserSection::Profile, SlotCx::default(), cx);
            v
        });
        Host { view }
    });
    cx.run_until_parked();
    let order = host.read_with(cx, |h, cx| h.view.read(cx).profile_focus_order(cx));
    assert_eq!(order.len(), 3);
    cx.update(|window, cx| order[0].focus(window, cx));
    for (i, want) in order.iter().enumerate().skip(1) {
        cx.simulate_keystrokes("tab");
        assert!(
            cx.update(|window, _| want.is_focused(window)),
            "Tab {i} should land on field {i} of display name, status message, bio"
        );
    }
}

/// S6: every shortcut of the Keyboard section is a distinct keystroke GPUI parses (the section's rows are tested against this
/// list in `keyboard_table_matches_bindings`).
#[test]
fn keyboard_user_settings_every_shortcut_is_a_keystroke() {
    assert_eq!(SHORTCUTS.len(), 7);
    for s in SHORTCUTS {
        let k = gpui::Keystroke::parse(s.keystroke).expect(s.keystroke);
        assert!(!k.key.is_empty(), "{}", s.keystroke);
    }
}

/// The shortcuts whose owning area has not registered its binding yet (jump 9, DM call 13), by action. The test fails when
/// one of them appears in the keymap or when a listed one that is not here goes missing, so the Keyboard table cannot drift
/// from the real registrations unnoticed. Integration 3: search (23) and the voice controls (17) are registered on the
/// integration branch. Task 28 empties this list.
const EXPECTED_MISSING: [&str; 2] = ["signalSettings.keyboard.jump", "keyboard.dmCallAction"];

#[gpui::test]
fn keyboard_table_follows_the_registered_bindings(cx: &mut TestAppContext) {
    cx.update(dilla_desktop::install);
    for s in SHORTCUTS {
        let input = [gpui::Keystroke::parse(s.keystroke).unwrap()];
        let names: Vec<String> = cx.update(|cx| {
            cx.all_bindings_for_input(&input)
                .iter()
                .map(|b| b.action().name().to_owned())
                .collect()
        });
        let registered = names.iter().any(|n| !n.starts_with("input::"));
        let expected_missing = EXPECTED_MISSING.contains(&s.action);
        assert_eq!(
            registered, !expected_missing,
            "{} ({}): registered = {registered} {names:?}, but the expected-missing list says {expected_missing}",
            s.action, s.keystroke
        );
    }
}
