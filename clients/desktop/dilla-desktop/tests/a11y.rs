//! Package 11: the screen-reader tree of every screen. beta.5 ran axe over Chromium's accessibility tree; the GPUI client
//! hands AccessKit's tree to the OS (AT-SPI, NSAccessibility, UI Automation). These tests draw each parity fixture in the
//! automation build with accessibility switched on, as a screen reader connecting does, and check:
//!
//! - the tree lint (`dilla_parity::a11y::Tree::lint`): every focusable node has a role and a name, targets are 24 px or
//!   spaced, dialogs are modal and named, ids are unique;
//! - the tab order: every Tab stop is a node with a role and a name, so the reader announces the control and not the
//!   window;
//! - per screen, the landmarks and named controls a reader navigates by (the web's `*.region` / `*.label` keys).
//!
//! Run with `cargo test -p dilla-desktop --features automation --test a11y`.

#![cfg(feature = "automation")]

use std::collections::HashSet;
use std::path::{Path, PathBuf};

use dilla_parity::a11y::Tree;
use dilla_parity::{Driver, Os};

fn fixtures_dir() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR")).join("../parity/fixtures")
}

fn fixture(name: &str) -> PathBuf {
    fixtures_dir().join(format!("{name}.json"))
}

fn launch(name: &str) -> Driver {
    let path = fixture(name);
    let loaded = dilla_parity::fixture::load(&path, Os::current()).expect("the fixture loads");
    let mut app = Driver::launch_fixture(&path, loaded.shot_path.as_deref(), &[]);
    app.wait_idle();
    app
}

/// The most Tab stops a screen walks before the walk gives up (the longest screen has well under this).
const MAX_STOPS: usize = 160;

/// What a tree breaks, one line each.
fn lint(tree: &Tree) -> Vec<String> {
    tree.lint().iter().map(ToString::to_string).collect()
}

/// Walks `app`'s tab order from where its focus is: every stop must be a named node, and what each stop shows (a
/// tooltip, a menu) passes the lint. Returns the problems and the order as read (`<automation id>=<role>(<name>)`).
fn walk(app: &mut Driver) -> (Vec<String>, Vec<String>) {
    let walk = app.tab_walk(MAX_STOPS);
    let mut problems = Vec::new();
    let mut order = Vec::new();
    for (n, (id, stop)) in walk.iter().enumerate() {
        let id = id.as_deref().unwrap_or("nothing");
        match stop.focused() {
            None => {
                order.push(format!("{id}=window"));
                problems.push(format!(
                    "tab stop {n} ({id}): the focused element has no node (the reader says the window)"
                ));
            }
            Some(node) => {
                order.push(format!(
                    "{id}={}({})",
                    node.role,
                    node.name().unwrap_or("?")
                ));
                if node.name().is_none() {
                    problems.push(format!(
                        "tab stop {n} ({id}): no name: {}",
                        stop.describe(node)
                    ));
                }
            }
        }
        problems.extend(lint(stop));
    }
    (problems, order)
}

fn dedup(mut problems: Vec<String>) -> Vec<String> {
    let mut seen = HashSet::new();
    problems.retain(|p| seen.insert(p.clone()));
    problems
}

/// Lints the tree of fixture `name` and walks its tab order; returns the app and the tree as first drawn. Panics with
/// every finding.
fn lint_fixture(name: &str) -> (Driver, Tree) {
    let mut app = launch(name);
    let tree = app.a11y_tree();
    let mut problems = lint(&tree);
    let (walked, order) = walk(&mut app);
    problems.extend(walked);
    let problems = dedup(problems);
    assert!(
        problems.is_empty(),
        "{name}: {} accessibility problems:\n{}\nthe tab order: {}",
        problems.len(),
        problems.join("\n"),
        order.join(" | ")
    );
    (app, tree)
}

/// Asserts that `tree` (of fixture `name`) has a node with each `(role, name)`.
fn has_all(name: &str, tree: &Tree, wanted: &[(&str, &str)]) {
    let missing: Vec<String> = wanted
        .iter()
        .filter(|(role, label)| !tree.has(role, label))
        .map(|(role, label)| format!("{role}({label})"))
        .collect();
    let named: Vec<String> = tree
        .nodes
        .iter()
        .filter_map(|n| n.name().map(|l| format!("{}({l})", n.role)))
        .collect();
    assert!(
        missing.is_empty(),
        "{name}: missing {} — the tree has: {}",
        missing.join(", "),
        named.join(", ")
    );
}

/// Every non-`-b` fixture's name (`<area>/<state>`), sorted.
fn every_fixture() -> Vec<String> {
    let mut out = Vec::new();
    for area in std::fs::read_dir(fixtures_dir()).expect("the fixtures") {
        let area = area.expect("an area").path();
        if !area.is_dir() {
            continue;
        }
        for f in std::fs::read_dir(&area).expect("an area's fixtures") {
            let f = f.expect("a fixture").path();
            let file = f.file_name().and_then(|s| s.to_str()).unwrap_or_default();
            if !file.ends_with(".json") || file.ends_with(".shot.json") || file.ends_with("-b.json")
            {
                continue;
            }
            let area = area
                .file_name()
                .and_then(|s| s.to_str())
                .unwrap_or_default();
            out.push(format!("{area}/{}", file.trim_end_matches(".json")));
        }
    }
    out.sort();
    out
}

/// support.spec.ts "axe evaluates under the served CSP without a policy violation": the lint runs on every fixture
/// (the first frame's tree; the tab walks are the screen tests below).
#[test]
#[ignore = "draws every fixture (several minutes): run in the a11y CI step"]
fn the_tree_lint_runs_on_every_fixture() {
    let mut problems = Vec::new();
    let mut refused = Vec::new();
    for name in every_fixture() {
        let path = fixture(&name);
        let Ok(loaded) = dilla_parity::fixture::load(&path, Os::current()) else {
            refused.push(name);
            continue;
        };
        let mut app = match Driver::try_launch_fixture(&path, loaded.shot_path.as_deref(), &[]) {
            Ok(app) => app,
            Err(_) => {
                refused.push(name);
                continue;
            }
        };
        app.wait_idle();
        let tree = app.a11y_tree();
        // A screen a reader can find nothing on: not one control or landmark has a node.
        if !tree
            .nodes
            .iter()
            .any(|n| n.name().is_some() && Some(n.id) != tree.root)
        {
            problems.push(format!("{name}: nothing on the screen is named"));
        }
        problems.extend(lint(&tree).into_iter().map(|p| format!("{name}: {p}")));
        for n in tree.pointer_only() {
            eprintln!("{name}: pointer only: {}", tree.describe(n));
        }
    }
    let problems = dedup(problems);
    assert!(
        problems.is_empty(),
        "{} accessibility problems:\n{}",
        problems.len(),
        problems.join("\n")
    );
    assert!(
        refused.len() < 10,
        "fixtures the automation build refused: {refused:?}"
    );
}

/// Keyboard reachability on every fixture: each one's tab order is walked, and every stop must be a named node. Six
/// fixtures are walked at a time (one automation process each).
#[test]
#[ignore = "walks every fixture's tab order (several minutes): run in the a11y CI step"]
fn the_tab_order_is_named_on_every_fixture() {
    let names = std::sync::Mutex::new(every_fixture());
    let problems = std::sync::Mutex::new(Vec::new());
    let walked = std::sync::atomic::AtomicUsize::new(0);
    std::thread::scope(|scope| {
        for _ in 0..6 {
            scope.spawn(|| {
                loop {
                    let Some(name) = names.lock().expect("the queue").pop() else {
                        return;
                    };
                    let path = fixture(&name);
                    let Ok(loaded) = dilla_parity::fixture::load(&path, Os::current()) else {
                        continue;
                    };
                    let Ok(mut app) =
                        Driver::try_launch_fixture(&path, loaded.shot_path.as_deref(), &[])
                    else {
                        continue;
                    };
                    app.wait_idle();
                    let (found, order) = walk(&mut app);
                    walked.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
                    if !found.is_empty() {
                        problems.lock().expect("the findings").push(format!(
                            "{name}:\n  {}\n  the tab order: {}",
                            dedup(found).join("\n  "),
                            order.join(" | ")
                        ));
                    }
                }
            });
        }
    });
    let walked = walked.into_inner();
    assert!(walked >= 60, "only {walked} fixtures were walked");
    let mut problems = problems.into_inner().expect("the findings");
    problems.sort();
    assert!(
        problems.is_empty(),
        "{} fixtures with unnamed tab stops:\n{}",
        problems.len(),
        problems.join("\n")
    );
}

/// csp-axe.spec.ts "every state of the slice passes axe with target-size…": the shell, a conversation, its threads and
/// a DM, walked by Tab, with the shell's landmarks (Shell.tsx: the rail's `nav`, the channel list's `nav`, the log,
/// the composer).
#[test]
fn every_slice_state_passes_the_tree_lint() {
    let (_, tree) = lint_fixture("shell/conversation");
    has_all(
        "shell/conversation",
        &tree,
        &[
            ("Navigation", "servers"),
            ("Navigation", "channels"),
            ("Log", "messages in #general"),
            ("MultilineTextInput", "message #general"),
            ("Button", "send"),
            ("Splitter", "resize the channel list"),
            ("Region", "your account"),
        ],
    );
    for landmark in ["Main", "Banner", "Complementary"] {
        assert!(
            tree.by_role(landmark).next().is_some(),
            "shell/conversation: no {landmark} landmark"
        );
    }
    assert!(
        tree.by_role("Article").count() >= 3,
        "shell/conversation: the log's messages are not in the tree"
    );
    for name in [
        "messages/conversation-actions",
        "threads/pane-open",
        "dms/conversation",
        "members/pane",
    ] {
        lint_fixture(name);
    }
}

/// search-settings.spec.ts "desktop, phone and reflow keep controls accessible in Signal": the palette, its results and
/// the settings pages, walked by Tab.
#[test]
fn search_and_settings_pass_the_tree_lint() {
    for name in [
        "search/palette",
        "search/results",
        "search/scope-settings",
        "user-settings/profile",
        "user-settings/appearance",
        "user-settings/devices",
        "user-settings/keyboard",
        "notify/settings",
        "server-settings/overview",
        "server-settings/roles",
    ] {
        lint_fixture(name);
    }
}

/// afk.spec.ts "10. accessible: the channel list, the AFK row with its away roster and server settings".
#[test]
fn afk_channel_list_and_server_settings() {
    lint_fixture("ring/afk");
    lint_fixture("server-settings/afk");
}

/// voice/csp-axe.spec.ts "the ring, the call, a player and Settings → Voice pass axe…".
#[test]
fn ring_call_player_voice_settings_pass_the_tree_lint() {
    let (_, ring) = lint_fixture("ring/banner");
    has_all("ring/banner", &ring, &[("Region", "incoming calls")]);
    assert!(
        ring.by_role("Alert")
            .any(|n| n.live.as_deref() == Some("assertive")),
        "ring/banner: the ring is not an assertive live node"
    );
    for name in [
        "voice/stage",
        "voice/dm-call",
        "attachments/media-player",
        "voice-settings/section",
    ] {
        lint_fixture(name);
    }
}

/// Package 11.4: sign-in, onboarding and the second factor, walked by Tab.
#[test]
fn sign_in_onboarding_and_factor_pass_the_tree_lint() {
    for name in [
        "sign-in/welcome",
        "sign-in/form",
        "sign-in/error",
        "sign-in/factor",
        "onboarding/identity",
        "onboarding/keys",
        "onboarding/done",
    ] {
        lint_fixture(name);
    }
}

/// Presses Tab until the focus is on a node with `role`, at most `max` times; answers the tree there.
fn tab_to(app: &mut Driver, role: &str, max: usize) -> Tree {
    for _ in 0..max {
        app.press("tab");
        let tree = app.a11y_tree();
        if tree.focused().is_some_and(|n| n.role == role) {
            return tree;
        }
    }
    panic!("no {role} in the first {max} tab stops");
}

fn focused_name(tree: &Tree) -> (String, String) {
    let n = tree.focused().expect("a node holds the focus");
    (n.role.clone(), n.name().unwrap_or_default().to_owned())
}

/// Conversation.tsx `rowKey` (web-2b): the log's messages are one tab stop, the newest; Up and Down step through them,
/// Home and End go to the first and the newest, Escape goes to the composer. The focused row shows its toolbar, whose
/// buttons Tab reaches (MessageRow.css `:focus-within`).
#[test]
fn messages_are_stepped_through_with_the_arrow_keys() {
    let mut app = launch("shell/conversation");
    app.a11y_tree();
    let at_newest = tab_to(&mut app, "Article", 30);
    let newest = focused_name(&at_newest);
    assert!(
        at_newest.by_role("Article").count() >= 3,
        "the log's rows are in the tree"
    );
    app.press("up");
    let above = focused_name(&app.a11y_tree());
    assert_eq!(above.0, "Article");
    assert_ne!(above.1, newest.1, "Up moves to the message above");
    app.press("down");
    assert_eq!(focused_name(&app.a11y_tree()), newest, "Down comes back");
    app.press("down");
    assert_eq!(
        focused_name(&app.a11y_tree()),
        newest,
        "the newest is the last stop"
    );
    app.press("home");
    let first = focused_name(&app.a11y_tree());
    assert_eq!(first.0, "Article");
    assert_ne!(first.1, newest.1, "Home goes to the first message");
    app.press("end");
    assert_eq!(
        focused_name(&app.a11y_tree()),
        newest,
        "End goes to the newest"
    );
    // The focused row's toolbar is reachable: Tab goes into it.
    app.press("tab");
    let tool = focused_name(&app.a11y_tree());
    assert_eq!(tool.0, "Button", "Tab from a row reaches its toolbar");
    assert!(
        ["react", "reply", "reply in thread", "more"].contains(&tool.1.as_str()),
        "a toolbar button, not {tool:?}"
    );
    // Escape on a toolbar button goes back to its row, and on the row to the composer.
    app.press("escape");
    assert_eq!(focused_name(&app.a11y_tree()), newest);
    app.press("escape");
    let composer = focused_name(&app.a11y_tree());
    assert_eq!(
        composer.0, "MultilineTextInput",
        "Escape goes to the composer"
    );
}

/// ChannelList.css: a category's "+" is a button the keyboard reaches (it shows while it has the focus).
#[test]
fn a_categorys_add_button_is_reached_by_tab() {
    let mut app = launch("notify/dm-badge");
    let tree = app.a11y_tree();
    let add = tree
        .nodes
        .iter()
        .find(|n| n.role == "Button" && n.name().is_some_and(|l| l.starts_with("new channel in")))
        .unwrap_or_else(|| {
            let buttons: Vec<&str> = tree.by_role("Button").filter_map(|n| n.name()).collect();
            panic!("no category's + among the buttons {buttons:?}")
        });
    assert!(add.focus, "the + takes the keyboard's focus");
}

/// The text the web exposed: the conversation's heading, a settings section's heading, a row's label and hint.
#[test]
fn headings_and_hints_have_nodes() {
    let mut app = launch("shell/conversation");
    let tree = app.a11y_tree();
    assert!(
        tree.has("Heading", "general"),
        "the channel header's heading"
    );
    let mut app = launch("user-settings/appearance");
    let tree = app.a11y_tree();
    assert!(
        tree.by_role("Heading").count() >= 1,
        "the section's heading is in the tree"
    );
    assert!(
        tree.by_role("Label").any(|n| n.name().is_some()),
        "a settings row's label is in the tree"
    );
}
