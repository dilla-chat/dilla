//! Task 8's parity, behaviour and keyboard tests for the shell area (standard procedure S4-S6).
//!
//! The parity tests compare each shell fixture with the web client's frame of it. The behaviour tests drive the automation
//! build on the same fixtures (a signed-in device's state, no network): the layout, the splitters and the channel list are
//! the app's own and need no server. What needs a client's preferences (the width a splitter keeps across a reload, the
//! folds and mutes) is tested in the shell's own unit tests (`screens/shell.rs`), and the window shapes in
//! `screens/shell/window.rs`. Run with `cargo test -p dilla-desktop --features automation --test shell`.

#![cfg(feature = "automation")]

use std::path::{Path, PathBuf};

use dilla_parity::screen::assert_screen;
use dilla_parity::{Driver, Os};
use gpui::{TestAppContext, VisualTestContext};

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures")
        .join(format!("{name}.json"))
}

fn launch(name: &str) -> Driver {
    let path = fixture(name);
    let loaded = dilla_parity::fixture::load(&path, Os::current()).expect("the fixture loads");
    Driver::launch_fixture(&path, loaded.shot_path.as_deref(), &[])
}

/// One parity test per fixture: `parity_shell_<state>[_b]`.
macro_rules! parity {
    ($($test:ident => $name:literal),* $(,)?) => {$(
        #[test]
        #[cfg_attr(not(target_os = "linux"), ignore = "references are Linux only")]
        fn $test() {
            assert_screen(&fixture($name))
                .unwrap_or_else(|e| panic!("{} differs from its web reference: {e}", $name));
        }
    )*};
}

parity! {
    parity_shell_conversation => "shell/conversation",
    parity_shell_conversation_b => "shell/conversation-b",
    parity_shell_categories => "shell/categories",
    parity_shell_categories_b => "shell/categories-b",
    parity_shell_panes_resized => "shell/panes-resized",
    parity_shell_panes_resized_b => "shell/panes-resized-b",
    parity_shell_header_tooltip => "shell/header-tooltip",
    parity_shell_header_tooltip_b => "shell/header-tooltip-b",
}

fn rect(app: &mut Driver, id: &str) -> dilla_parity::ElementInfo {
    app.element(id)
        .unwrap_or_else(|| panic!("`{id}` is not on screen"))
}

/// The sidebar's width as the splitter's position gives it: the splitter's hit area is centred on the sidebar's edge.
fn sidebar_width(app: &mut Driver) -> f32 {
    let sidebar = rect(app, "shell-sidebar");
    sidebar.width
}

/// e2e/web/panes.spec.ts `the sidebar splitter drags and takes keys, the width survives a reload, and the aside splitter
/// exists only as a column` (the keys: Enter resets to 244, ArrowRight moves one rem, Home goes to the minimum, End to the
/// maximum; the width across a reload is `sidebar_splitter_drags_takes_keys_and_persists` in `screens/shell.rs`).
#[test]
fn sidebar_splitter_drags_takes_keys_and_persists() {
    let mut app = launch("shell/categories");
    app.wait_idle();
    assert_eq!(sidebar_width(&mut app), 244.0, "the default width");
    app.click("splitter-sidebar");
    assert_eq!(app.focused().as_deref(), Some("splitter-sidebar"));
    app.press("right");
    app.press("right");
    app.wait_idle();
    assert_eq!(
        sidebar_width(&mut app),
        276.0,
        "two ArrowRights are two rem"
    );
    app.press("home");
    app.wait_idle();
    assert_eq!(sidebar_width(&mut app), 192.0, "Home: the 12rem minimum");
    app.press("end");
    app.wait_idle();
    assert_eq!(sidebar_width(&mut app), 384.0, "End: the 24rem maximum");
    app.press("enter");
    app.wait_idle();
    assert_eq!(sidebar_width(&mut app), 244.0, "Enter resets");
    app.press("shift-right");
    app.wait_idle();
    assert_eq!(sidebar_width(&mut app), 308.0, "Shift moves four rem");
}

/// e2e/web/panes.spec.ts `aside_splitter_only_with_aside`: with no aside open there is no aside splitter, and the sidebar's
/// is there.
#[test]
fn aside_splitter_only_with_aside() {
    let mut app = launch("shell/categories");
    app.wait_idle();
    assert!(app.element("splitter-sidebar").is_some());
    assert!(
        app.element("splitter-aside").is_none(),
        "no aside is drawn: no splitter beside it"
    );
}

/// e2e/web/layout.spec.ts, desktop cases `layout_grammar_holds_at_1280_and_1440`: the rail is 56 wide, the sidebar 244, the
/// top bar and the channel header 44 high, the user panel 64, and the main column takes what is left, at both sizes.
#[test]
fn layout_grammar_holds_at_1280_and_1440() {
    for (name, width, height) in [
        ("shell/conversation", 1280.0, 800.0),
        ("shell/conversation-b", 1440.0, 900.0),
    ] {
        let mut app = launch(name);
        app.wait_idle();
        let rail = rect(&mut app, "shell-rail");
        let sidebar = rect(&mut app, "shell-sidebar");
        let main = rect(&mut app, "shell-main");
        let header = rect(&mut app, "shell-header");
        let panel = rect(&mut app, "shell-user-panel");
        let bar = rect(&mut app, "shell-topbar");
        // The probes measure inside the 1px hairline under the bar and the header and over the panel.
        assert_eq!(
            (bar.height, header.height, panel.height),
            (43.0, 43.0, 63.0),
            "{name}"
        );
        assert_eq!((rail.width, sidebar.width), (56.0, 244.0), "{name}");
        assert_eq!(
            sidebar.x,
            rail.x + rail.width,
            "{name}: the sidebar follows the rail"
        );
        assert_eq!(
            main.x,
            sidebar.x + sidebar.width,
            "{name}: main follows the sidebar"
        );
        assert_eq!(
            main.x + main.width,
            width,
            "{name}: main reaches the window's edge"
        );
        assert_eq!(
            panel.y + panel.height,
            height,
            "{name}: the panel ends the window"
        );
        assert_eq!(
            rail.y,
            bar.height + 1.0,
            "{name}: the rail starts under the bar and its hairline"
        );
    }
}

/// e2e/web/afk.spec.ts test 1 `afk_channel_listed_last_for_owner_and_member`: the AFK channel is the list's last row for the
/// server's owner (Ada, in the second fixture) and for a member (Mira, in the first).
#[test]
fn afk_channel_listed_last_for_owner_and_member() {
    for name in ["shell/categories", "shell/categories-b"] {
        let mut app = launch(name);
        app.wait_idle();
        let afk = rect(&mut app, "channel-AFK");
        for other in ["channel-general", "channel-loot"] {
            let row = rect(&mut app, other);
            assert!(afk.y > row.y, "{name}: AFK is below {other}");
        }
    }
}

/// e2e/web/keyboard.spec.ts `messaging_flow_on_keyboard_alone_in_fixed_order` (the shell's part): Tab goes rail, then the
/// channel list, one stop each (the rail and the list rove with the arrows), and the list's arrows step its rows.
#[test]
fn keyboard_shell_focus_order() {
    let mut app = launch("shell/categories");
    app.wait_idle();
    let mut seen = Vec::new();
    for _ in 0..12 {
        app.press("tab");
        if let Some(id) = app.focused()
            && seen.last() != Some(&id)
        {
            seen.push(id);
        }
    }
    let at = |id: &str| seen.iter().position(|s| s == id);
    let rail = at("rail-server-Midgard Crew")
        .unwrap_or_else(|| panic!("the rail's stop is the selected server: {seen:?}"));
    let list = at("focus-channel-general")
        .unwrap_or_else(|| panic!("the list's stop is the selected channel: {seen:?}"));
    assert!(rail < list, "the rail comes before the list: {seen:?}");
    // The arrows rove inside the list: Down from the stop goes to the next row, Up back.
    while app.focused().as_deref() != Some("focus-channel-general") {
        app.press("tab");
    }
    // The order of equal positions is the server's (by id): the arrows step to a neighbour, Home and End jump.
    app.press("down");
    let next = app.focused().expect("a row holds the focus");
    assert!(
        next.starts_with("focus-") && next != "focus-channel-general",
        "{next}"
    );
    app.press("end");
    assert_eq!(app.focused().as_deref(), Some("focus-channel-AFK"));
    app.press("up");
    assert_ne!(app.focused().as_deref(), Some("focus-channel-AFK"));
    app.press("home");
    let first = app.focused().expect("a row holds the focus");
    assert!(first.starts_with("focus-"), "{first}");
}

/// A category's head folds its channels (the web: `aria-expanded`), and the fold is only a view: the selected channel stays.
#[test]
fn a_category_head_folds_its_channels() {
    let mut app = launch("shell/categories");
    app.wait_idle();
    assert!(app.element("channel-harbour").is_some());
    app.click("category-boats");
    app.wait_idle();
    assert!(
        app.element("channel-harbour").is_none(),
        "folded: harbour is hidden"
    );
    assert!(app.element("channel-general").is_some());
    app.click("category-boats");
    app.wait_idle();
    assert!(app.element("channel-harbour").is_some(), "unfolded again");
}

/// e2e/web/layout.spec.ts desktop `desktop_home_title_is_not_server_name`: the window's title is the product's, whatever
/// server is shown.
#[gpui::test]
fn desktop_home_title_is_not_server_name(cx: &mut TestAppContext) {
    let loaded = dilla_desktop::fixture::Fixture::load(&fixture("shell/categories"), None)
        .expect("the fixture loads");
    let handle = cx
        .update(|cx| {
            dilla_desktop::open_main_window_with(cx, dilla_desktop::Launch::fixture(loaded))
        })
        .expect("the main window opens");
    let mut window_cx = VisualTestContext::from_window(handle.into(), cx);
    assert_eq!(window_cx.window_title().as_deref(), Some("Dilla Preview"));
}
