//! Task 20's parity, behaviour and keyboard tests for the bot area (standard procedure S4-S6), driven through the
//! automation build.
//!
//! The poll card draws from the polls facet of a fixture (`src/fixture/polls.rs`). Its web references are shot with the
//! server's test Bot (`ServerSpec::bot`): the world's creator asks it for the poll, the voters vote and it posts the card
//! (`examples/parity_world/seed/polls.rs`).

use std::path::{Path, PathBuf};
use std::time::Duration;

use dilla_parity::Driver;

fn fixture(name: &str) -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../parity/fixtures")
        .join(name)
}

/// The poll card sits under the Bot's post in a box of its own height, at most 440px of content wide (PollCard.css).
#[test]
fn poll_card_draws_under_the_bots_post() {
    let mut app = Driver::launch_fixture(&fixture("polls/card.json"), None, &[]);
    let card = app.wait_for("poll-card", Duration::from_secs(20));
    // 440px of content, 12px padding each side, 1px borders; the 6px margin above it is in its box.
    assert_eq!(card.width, 466.0, "{card:?}");
    assert!(card.height > 100.0 && card.height < 200.0, "{card:?}");
    let composer = app.element("composer").expect("the composer");
    assert!(
        card.y + card.height <= composer.y,
        "the card sits above the composer: {card:?} {composer:?}"
    );
}

/// The B fixture: a closed poll of three answers with a long question and several voters, at 1440x900 in a second
/// server. The card is as wide and its question wraps.
#[test]
fn poll_card_b_wraps_and_grows_with_its_answers() {
    let mut a = Driver::launch_fixture(&fixture("polls/card.json"), None, &[]);
    let small = a.wait_for("poll-card", Duration::from_secs(20));
    let mut b = Driver::launch_fixture(&fixture("polls/card-b.json"), None, &[]);
    let big = b.wait_for("poll-card", Duration::from_secs(20));
    assert_eq!(big.width, small.width);
    // One more answer row (36 + 8) and at least two more question lines.
    assert!(
        big.height >= small.height + 44.0 + 2.0 * 19.5,
        "{small:?} {big:?}"
    );
}

#[test]
fn parity_polls_card() {
    dilla_parity::screen::assert_screen(&fixture("polls/card.json")).unwrap();
}

#[test]
fn parity_polls_card_b() {
    dilla_parity::screen::assert_screen(&fixture("polls/card-b.json")).unwrap();
}

/// `/poll create` typed and run: the poll dialog over the channel (Task 26), shot with the server's Bot installed.
#[test]
fn parity_polls_dialog() {
    dilla_parity::screen::assert_screen(&fixture("polls/dialog.json")).unwrap();
}

/// The Bot's poll post with no poll held and no client: the web gives up at once (`botDefinitelyUnavailable`) and the row
/// shows the post's own text, the marker line gone and the `<t:…>` tag as a local time, with no card.
#[test]
fn an_unverified_poll_post_shows_its_text_and_no_card() {
    let mut app = Driver::launch_fixture(&fixture("polls/fallback.json"), None, &[]);
    app.wait_for("composer", Duration::from_secs(20));
    assert!(app.element("poll-card").is_none(), "no card without a poll");
    let tree = app.a11y_tree();
    let row = tree
        .nodes
        .iter()
        .filter_map(|n| n.label.as_deref())
        .find(|l| l.contains("Vote with /poll vote."))
        .expect("the poll post's row reads its text");
    assert!(row.contains("Closes: Fri 15 Jan, 09:00 UTC"), "{row}");
    assert!(
        !row.contains("<t:") && !row.contains("[dilla-poll"),
        "{row}"
    );
}

#[test]
fn parity_polls_fallback() {
    dilla_parity::screen::assert_screen(&fixture("polls/fallback.json")).unwrap();
}

#[test]
fn parity_polls_fallback_b() {
    dilla_parity::screen::assert_screen(&fixture("polls/fallback-b.json")).unwrap();
}

/// The reminders popover open under the bell (Signal §6.7), nothing kept: the Upcoming tab and its empty line.
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_commands_reminders() {
    dilla_parity::screen::assert_screen(&fixture("commands/reminders.json")).unwrap();
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_commands_reminders_b() {
    dilla_parity::screen::assert_screen(&fixture("commands/reminders-b.json")).unwrap();
}

/// Task 42: the command picker over the composer: `/` lists the commands, `/rem` narrows them, `/zzz` has none.
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_commands_picker_open() {
    dilla_parity::screen::assert_screen(&fixture("commands/picker-open.json")).unwrap();
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_commands_picker_filtered() {
    dilla_parity::screen::assert_screen(&fixture("commands/picker-filtered.json")).unwrap();
}

#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_commands_picker_empty() {
    dilla_parity::screen::assert_screen(&fixture("commands/picker-empty.json")).unwrap();
}

/// Task 42: `/selfrole list` run in a community that approves no role: the answer as a private row ("Only you can see
/// this"), "No self-assignable roles are available." under Done. The fixture window ends the row from its own state (no
/// Bot to ask), the web shot has the real Bot answer.
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_commands_private_result() {
    dilla_parity::screen::assert_screen(&fixture("commands/private-result.json")).unwrap();
}

/// Task 42: `/role add ` picked with Tab: the command's bar over the box, its chips in place of the field. Ignored for one
/// reason that is the web client's, not the app's: in the shooter's browser the web frame has no suggestion list under the
/// `user` chip (its Bot-world suggestion provider and presence snapshot do not settle after a key is typed: about one run in
/// two, with the browser clock frozen or not, no HTTP or console error), while the app draws the list from the members it
/// holds. The app's own differences found here are fixed (the bar over the box, the dim send, the chip input width).
/// Fallback: a native golden, as `dms/home`.
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
#[cfg_attr(
    any(target_os = "linux", target_os = "macos"),
    ignore = "the web frame has no suggestion list: its Bot-world suggestions and presence do not settle after typing (Task 42 report)"
)]
fn parity_commands_picker_chip() {
    dilla_parity::screen::assert_screen(&fixture("commands/picker-chip.json")).unwrap();
}

/// `/modlog` (CommandHost.tsx:411, Decision D1): the moderation history opens in a dialog over the channel, the row under
/// the log saying so.
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_commands_modlog_dialog() {
    dilla_parity::screen::assert_screen(&fixture("commands/modlog-dialog.json")).unwrap();
}

/// `/delete` of Ada's own message asks first (builtin.ts:142, CommandHost.tsx's `ConfirmDialog`): "Delete this message?"
/// over the channel, its row working under the log.
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_commands_confirm_delete() {
    dilla_parity::screen::assert_screen(&fixture("commands/confirm-delete.json")).unwrap();
}

/// RemindersPopover.tsx: the bell opens the popover 36px under its top, its right edge 6px past the bell's, 342px wide,
/// with the picked tab focused (Upcoming: nothing is due); ←→ move between the tabs; Escape closes it and gives the bell
/// the focus back. A second press on the bell closes it, and so does a press outside it.
#[test]
fn the_bell_opens_the_reminders_popover_and_escape_gives_the_focus_back() {
    let mut app = Driver::launch_fixture(&fixture("shell/channel-view.json"), None, &[]);
    app.wait_for("composer", Duration::from_secs(20));
    assert!(app.element("reminders-popover").is_none());
    app.click("topbar-bell");
    let pop = app.wait_for("reminders-popover", Duration::from_secs(5));
    let bell = app.element("topbar-bell").expect("the bell");
    assert_eq!(pop.width, 342.0, "{pop:?}");
    assert_eq!(pop.y, bell.y + 36.0, "{pop:?} {bell:?}");
    assert_eq!(
        pop.x + pop.width,
        bell.x + bell.width + 6.0,
        "{pop:?} {bell:?}"
    );
    assert_eq!(app.focused().as_deref(), Some("reminders-tab-upcoming"));
    app.press("left");
    app.wait_idle();
    assert_eq!(app.focused().as_deref(), Some("reminders-tab-due"));
    app.press("end");
    app.wait_idle();
    assert_eq!(app.focused().as_deref(), Some("reminders-tab-upcoming"));
    app.press("escape");
    app.wait_idle();
    assert!(
        app.element("reminders-popover").is_none(),
        "Escape closes it"
    );
    assert_eq!(app.focused().as_deref(), Some("topbar-bell"));

    app.click("topbar-bell");
    app.wait_for("reminders-popover", Duration::from_secs(5));
    app.click("topbar-bell");
    app.wait_idle();
    assert!(
        app.element("reminders-popover").is_none(),
        "the bell closes it again"
    );
    app.click("topbar-bell");
    app.wait_for("reminders-popover", Duration::from_secs(5));
    app.click("composer");
    app.wait_idle();
    assert!(
        app.element("reminders-popover").is_none(),
        "a press outside closes it"
    );
}

/// ReminderPanel: the due reminders under Due (the tab while anything is due) and the scheduled ones under Upcoming, each
/// tab counting its own; the bell's badge counts the due ones.
#[cfg(feature = "automation")]
#[gpui::test]
fn the_popover_lists_due_reminders_first_and_upcoming_ones_under_their_tab(
    cx: &mut gpui::TestAppContext,
) {
    use dilla_client::state::bot::{ReminderStatus, ReminderView};
    use dilla_desktop::app::{AppView, Launch};
    use dilla_desktop::screens::commands::reminders;
    let mut loaded =
        dilla_desktop::fixture::Fixture::load(&fixture("shell/channel-view.json"), None)
            .expect("the shell fixture");
    let community = loaded.state.communities[0].id;
    let reminder = |id: u8, due: u64, state: ReminderStatus| ReminderView {
        id: [id; 16],
        text: format!("reminder {id}"),
        due,
        timezone: "UTC".into(),
        source_channel: None,
        source_thread: None,
        state,
        revision: 1,
    };
    let now = loaded.now;
    loaded.state.bot.reminders.insert(
        community,
        vec![
            reminder(0xd1, now - 60, ReminderStatus::Due),
            reminder(0xa1, now + 3600, ReminderStatus::Scheduled),
        ],
    );
    assert_eq!(reminders::due_count(&loaded.state), 1);
    cx.update(dilla_desktop::install);
    let (_app, vcx) =
        cx.add_window_view(|window, cx| AppView::new(Launch::fixture(loaded), window, cx));
    vcx.run_until_parked();
    vcx.update(|window, cx| reminders::toggle(window.window_handle().window_id(), cx));
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    // `debug_bounds` takes a `'static` name.
    let due_row: &'static str = format!("reminder-{}", "d1".repeat(16)).leak();
    let upcoming_row: &'static str = format!("reminder-{}", "a1".repeat(16)).leak();
    assert!(vcx.debug_bounds(due_row).is_some(), "Due is the tab");
    assert!(vcx.debug_bounds(upcoming_row).is_none());
    let tab = vcx
        .debug_bounds("reminders-tab-upcoming")
        .expect("the Upcoming tab");
    vcx.simulate_click(tab.center(), gpui::Modifiers::none());
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    assert!(vcx.debug_bounds(upcoming_row).is_some(), "under Upcoming");
    assert!(vcx.debug_bounds(due_row).is_none());
}

/// Integration 3, step 5 (T20's `private_results` in Task 9's log): a private result of the open conversation is drawn
/// after the log's last message, and the log grows by it.
#[cfg(feature = "automation")]
#[gpui::test]
fn a_private_result_follows_the_last_message(cx: &mut gpui::TestAppContext) {
    use dilla_client::state::bot::{Conversation, PrivateResult, ResultProvider, ResultStatus};
    use dilla_desktop::app::{AppView, Launch};
    let mut loaded =
        dilla_desktop::fixture::Fixture::load(&fixture("shell/channel-view.json"), None)
            .expect("the shell fixture");
    let channels: Vec<_> = loaded
        .state
        .channels
        .iter()
        .filter(|c| c.kind == 0)
        .map(|c| c.id)
        .collect();
    for channel in channels {
        loaded.state.bot.private_results.insert(
            Conversation {
                channel,
                thread: None,
            },
            vec![PrivateResult {
                id: 1,
                at: 1_794_476_484_000,
                status: ResultStatus::Failed,
                text: "nosuch is not a command.".into(),
                provider: ResultProvider::Builtin,
                hold: false,
                settled_at: None,
            }],
        );
    }
    cx.update(dilla_desktop::install);
    let (_app, vcx) =
        cx.add_window_view(|window, cx| AppView::new(Launch::fixture(loaded), window, cx));
    vcx.run_until_parked();
    vcx.update(|window, _| window.refresh());
    vcx.run_until_parked();
    let result = vcx
        .debug_bounds("private-result-0")
        .expect("the private result in the log");
    // The log is pinned to its end: the result, its last entry, sits just above the composer.
    let composer = vcx.debug_bounds("composer").expect("the composer");
    assert!(
        result.origin.y + result.size.height <= composer.origin.y
            && composer.origin.y - (result.origin.y + result.size.height) < gpui::px(80.0),
        "the log's last entry, above the composer: {result:?} {composer:?}"
    );
}

/// Integration 3, step 5 (T20's composer hooks in Task 9's composer): typing `/` opens the command list (the composer
/// reports its draft), and Enter on a command that still lacks its arguments keeps the draft (the bot area decides what
/// a send does): the next keystroke finds the draft and the list again. Before the hooks the list never opened and the
/// draft was posted.
#[test]
fn the_composer_reports_its_draft_and_asks_before_it_sends() {
    let mut app = Driver::launch_fixture(&fixture("shell/channel-view.json"), None, &[]);
    app.wait_for("composer", Duration::from_secs(20));
    app.type_text("composer", "/");
    app.wait_for("command-picker", Duration::from_secs(5));
    app.type_focused("code");
    app.wait_idle();
    app.press("escape");
    app.wait_idle();
    assert!(
        app.element("command-picker").is_none(),
        "Escape closes the list"
    );
    app.press("enter");
    app.wait_idle();
    // The draft "/code" stayed: one more character changes it, and the list opens on it again.
    app.type_focused(" ");
    app.wait_for("command-picker", Duration::from_secs(5));
}

/// Task 28 follow-up 14 (CommandPicker.tsx `enterCommand`): picking a command turns the composer's field into its chips,
/// one per argument; typing fills the active one (it grows with its text), Tab moves to the next, Backspace deletes, and
/// Escape leaves the chips with the field back. Before, a pick only recorded the command.
#[test]
fn picking_a_command_turns_the_field_into_its_chips() {
    let mut app = Driver::launch_fixture(&fixture("shell/channel-view.json"), None, &[]);
    app.wait_for("composer", Duration::from_secs(20));
    app.type_text("composer", "/");
    app.wait_for("command-picker", Duration::from_secs(5));
    app.type_focused("code");
    app.wait_idle();
    assert!(
        app.element("command-active-ready").is_some(),
        "a built-in can run at once"
    );
    app.press("tab");
    app.wait_for("command-chips", Duration::from_secs(5));
    assert!(app.element("command-picker").is_none(), "the list closed");
    let language = app.wait_for("command-field-0", Duration::from_secs(5));
    let text = app.element("command-field-1").expect("the text field");
    // The first field is active: typing widens it (one input is as wide as its text, at least four characters).
    app.type_focused("typescript");
    app.wait_idle();
    let typed = app.element("command-field-0").unwrap();
    assert!(typed.width > language.width, "{language:?} {typed:?}");
    app.press("backspace");
    app.wait_idle();
    let shorter = app.element("command-field-0").unwrap();
    assert!(shorter.width < typed.width, "{typed:?} {shorter:?}");
    // Tab: the second field takes the typing.
    app.press("tab");
    app.type_focused("let rope = 2 + 2;");
    app.wait_idle();
    let after = app.element("command-field-0").unwrap();
    assert_eq!(after.width, shorter.width, "the first field kept its text");
    assert!(app.element("command-field-1").unwrap().width > text.width);
    // Escape leaves the chips: the field is back with the typed draft, its list closed.
    app.press("escape");
    app.wait_idle();
    assert!(app.element("command-chips").is_none());
    assert!(app.element("command-picker").is_none());
    app.wait_for("composer", Duration::from_secs(5));
}

/// Task 10 fix round (CommandField's `onFocus`): a press on a chip field makes it the active one and the typing goes on
/// in it; the shell never takes the keys (its search stays shut). Before, the press left the composer and typing opened
/// the quick switcher.
#[test]
fn a_pressed_chip_field_takes_the_typing() {
    let mut app = Driver::launch_fixture(&fixture("shell/channel-view.json"), None, &[]);
    app.wait_for("composer", Duration::from_secs(20));
    app.type_text("composer", "/");
    app.wait_for("command-picker", Duration::from_secs(5));
    app.type_focused("code");
    app.wait_idle();
    app.press("tab");
    app.wait_for("command-chips", Duration::from_secs(5));
    let text = app.wait_for("command-field-1", Duration::from_secs(5));
    let language = app.element("command-field-0").expect("the language field");
    app.click("command-field-1");
    app.type_focused("let rope = 2 + 2;");
    app.wait_idle();
    assert!(
        app.element("command-field-1").unwrap().width > text.width,
        "the pressed field took the text"
    );
    assert_eq!(
        app.element("command-field-0").unwrap().width,
        language.width
    );
    app.click("command-field-0");
    app.type_focused("typescript");
    app.wait_idle();
    assert!(app.element("command-field-0").unwrap().width > language.width);
    assert!(app.element("command-chips").is_some(), "still the chips");
}

// ---- behaviour: the shipping window against a real dillad and its real Bot (Task 28) ----

mod support;
use support::world;

/// A fresh data directory under `target/`.
fn scratch_dir(tag: &str) -> PathBuf {
    let dir = world::repo_root().join(format!("target/{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();
    dir
}

/// The app signs a new device of `person` in (as `tests/reactions.rs` does) and opens #general.
fn sign_in_app(app: &mut Driver, host: &world::Host, person: &mut world::Person) {
    app.wait_for("onboarding-signin", Duration::from_secs(30));
    app.click("onboarding-signin");
    app.wait_for("signin-server", Duration::from_secs(30));
    app.type_text("signin-server", &host.public);
    app.press("tab");
    app.type_focused(&person.username);
    app.press("tab");
    app.type_focused(world::PASSWORD);
    app.press("enter");
    app.wait_for("signin-code", Duration::from_secs(30));
    let code = person.next_code(host);
    app.type_text("signin-code", &code);
    app.click("signin-verify");
    app.wait_for("signin-key", Duration::from_secs(30));
    app.type_text("signin-key", &person.recovery_key);
    app.click("signin-add");
    app.wait_for("signin-open", Duration::from_secs(60));
    app.click("signin-open");
    app.wait_for("channel-general", Duration::from_secs(60));
    app.click("channel-general");
    app.wait_for("composer", Duration::from_secs(30));
}

/// e2e/web/slash-commands.spec.ts "a reminder set from the picker chips appears under Upcoming": in the window, `/remind`
/// picked from the list turns into its chips; "in 1 hour" in `when` (the bar previews it), Tab, the text, Enter: the Bot's
/// sealed answer is a private row (the chips leave), and the reminder is listed under Upcoming in the window's reminders
/// popover (the bell's; the account's first device reads it from the Bot for its id and its time).
#[test]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
fn a_reminder_set_from_the_chips_appears_under_upcoming() {
    use dilla_client::state::CommunityId;
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_bot_host("t28-chips", "127.0.0.1:0");
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t28-chips"));
    let community = world::community(&host, &ada, "Chips", &[]);
    world::clear_session(&ada);
    let first = rt.block_on(world::start(&ada));
    rt.block_on(world::bot::enable_bot(
        &first,
        community,
        world::bot::cap::REMINDERS,
    ));

    let root = scratch_dir("t28-chips-app");
    let mut app = Driver::launch(&root);
    sign_in_app(&mut app, &host, &mut ada);
    let text = format!("chip reminder {}", std::process::id());
    app.type_text("composer", "/");
    app.wait_for("command-picker", Duration::from_secs(10));
    app.type_focused("remind");
    app.wait_idle();
    // The new device reads the Bot's catalog on its first ready; until its key checks, the command is greyed
    // (t28-bot-review I5: wait for that, bounded, never a fixed sleep).
    app.wait_for("command-active-ready", Duration::from_secs(60));
    app.press("tab");
    app.wait_for("command-chips", Duration::from_secs(5));
    app.type_focused("in 1 hour");
    app.press("tab");
    app.type_focused(&text);
    app.press("enter");
    app.wait_idle();
    assert!(
        app.element("command-chips").is_none(),
        "the run left the chips"
    );

    let listed = rt.block_on(async {
        let deadline = std::time::Instant::now() + Duration::from_secs(60);
        loop {
            let all = first
                .utilities()
                .refresh_reminders(CommunityId(community))
                .await
                .unwrap_or_default();
            if all.iter().any(|r| r.text == text) {
                break all;
            }
            assert!(
                std::time::Instant::now() < deadline,
                "the chips' reminder never reached the Bot: {all:?} {:?}",
                host.bot_refusals()
            );
            tokio::time::sleep(Duration::from_secs(1)).await;
        }
    });
    let now = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_secs();
    let mine = listed.iter().find(|r| r.text == text).unwrap();
    // "in 1 hour", as the chip said.
    assert!(
        (3500..=3700).contains(&(mine.due.saturating_sub(now))),
        "{mine:?}"
    );

    // Upcoming, as the window shows it: the bell's popover reads the list when it opens. Nothing is due, so Upcoming is
    // its tab, and the reminder's row is there; the Due tab does not list it.
    let row = format!(
        "reminder-{}",
        mine.id
            .iter()
            .map(|b| format!("{b:02x}"))
            .collect::<String>()
    );
    app.click("topbar-bell");
    app.wait_for("reminders-popover", Duration::from_secs(10));
    assert_eq!(app.focused().as_deref(), Some("reminders-tab-upcoming"));
    app.wait_for(&row, Duration::from_secs(60));
    app.click("reminders-tab-due");
    app.wait_idle();
    assert!(app.element(&row).is_none(), "not under Due");
    app.click("reminders-tab-upcoming");
    app.wait_for(&row, Duration::from_secs(5));
    drop(app);
    first.stop();
    let _ = std::fs::remove_dir_all(&root);
}

/// Server settings → Join roles (BotSection.tsx `page === 'joinRoles'`), against a real dillad and its Bot: the owner's
/// community has "Arrival Crew" and a Roles-enabled Bot with no join role. In the window, the page (opened from the search
/// palette, "join role") reads the Bot's setting and offers the role in its select; picking it makes the page dirty and
/// the footer's Save sends it (`configureBotJoinRole` on the installation it read). The first device reads it back.
#[test]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
fn the_join_roles_page_saves_the_role_the_bot_gives_on_join() {
    use dilla_client::state::CommunityId;
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_bot_host("t28-joinpage", "127.0.0.1:0");
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t28-joinpage"));
    let community = world::community(&host, &ada, "Arrivals", &[]);
    world::clear_session(&ada);
    let first = rt.block_on(world::start(&ada));
    let installation = rt.block_on(world::bot::enable_bot(
        &first,
        community,
        world::bot::cap::ROLES,
    ));
    let role = rt
        .block_on(first.roles().create_role(
            CommunityId(community),
            "Arrival Crew",
            0,
            false,
            false,
            0,
        ))
        .expect("the role");
    assert_eq!(
        rt.block_on(first.roles().bot_join_role(CommunityId(community)))
            .unwrap(),
        Some((None, installation.id)),
        "no join role yet"
    );

    let root = scratch_dir("t28-joinpage-app");
    let mut app = Driver::launch(&root);
    sign_in_app(&mut app, &host, &mut ada);
    app.press("ctrl-k");
    app.wait_for("search-input", Duration::from_secs(10));
    app.type_focused("join role");
    app.wait_idle();
    app.press("enter");
    // The select is drawn once the Bot's setting and the installation are read and match.
    app.wait_for("join-role-select", Duration::from_secs(60));
    assert!(
        app.element("settings-save").is_none(),
        "nothing to save yet"
    );
    app.click("join-role-select");
    app.wait_idle();
    app.press("down");
    app.press("enter");
    app.wait_for("settings-save", Duration::from_secs(10));
    app.click("settings-save");

    let deadline = std::time::Instant::now() + Duration::from_secs(30);
    loop {
        let read = rt
            .block_on(first.roles().bot_join_role(CommunityId(community)))
            .unwrap();
        if read == Some((Some(role), installation.id)) {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "the page never saved the join role: {read:?}"
        );
        std::thread::sleep(Duration::from_millis(500));
    }
    // Saved: the draft is clean again, so the footer goes.
    let gone = std::time::Instant::now() + Duration::from_secs(10);
    while app.element("settings-save").is_some() {
        assert!(std::time::Instant::now() < gone, "the footer stayed");
        std::thread::sleep(Duration::from_millis(100));
    }
    drop(app);
    first.stop();
    let _ = std::fs::remove_dir_all(&root);
}

/// e2e/web/slash-commands.spec.ts "polls publish in channels": in the window, `/poll create` opens the poll form (no row
/// yet: a cancelled form leaves nothing). The question and two answers are typed, Post sends one `create` to the Bot, and
/// the command's row ends as success, and the Bot's post is verified when drawn: the card appears.
#[test]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
fn a_poll_posted_from_the_form_shows_its_card() {
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_bot_host("t26-poll", "127.0.0.1:0");
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t26-poll"));
    let community = world::community(&host, &ada, "Polls", &[]);
    world::clear_session(&ada);
    let first = rt.block_on(world::start(&ada));
    rt.block_on(world::bot::enable_bot(
        &first,
        community,
        world::bot::cap::POLLS,
    ));
    // `poll create` posts into a Bot channel (the catalog's `needs_bot_channel`): #general is made one.
    let community_id = dilla_client::state::CommunityId(community);
    let general = first
        .state()
        .borrow()
        .channel_named(community_id, "general")
        .map(|c| c.id)
        .expect("the community's #general");
    rt.block_on(first.bot_admin().mark_bot_channel(community_id, general))
        .expect("an answer")
        .expect("#general is marked a Bot channel");

    let root = scratch_dir("t26-poll-app");
    let mut app = Driver::launch(&root);
    sign_in_app(&mut app, &host, &mut ada);
    app.type_text("composer", "/poll create");
    app.wait_for("command-picker", Duration::from_secs(10));
    // The new device reads the Bot's catalog on its first ready; until its key checks, the command is greyed.
    app.wait_for("command-active-ready", Duration::from_secs(60));
    // Picking it runs it (CommandPicker.tsx `enterCommand`, `opensForm`): the form opens with no chips to fill.
    app.press("tab");
    app.wait_for("poll-question", Duration::from_secs(10));
    assert!(
        app.element("command-chips").is_none(),
        "poll create has no chips"
    );
    assert!(
        app.element("bot-result-pending").is_none(),
        "the form is no row until Post"
    );
    assert_eq!(
        app.focused().as_deref(),
        Some("poll-question"),
        "the form opens on its question"
    );
    app.type_focused("What should we raid on Friday?");
    app.click("poll-answer-0");
    app.type_focused("the bridge");
    app.click("poll-answer-1");
    app.type_focused("the harbour");
    app.wait_idle();
    app.click("poll-post");
    app.wait_for("bot-result-success", Duration::from_secs(60));
    assert!(
        app.element("poll-question").is_none(),
        "the form closed on the answer"
    );
    assert!(app.element("bot-result-failed").is_none(), "no row failed");
    app.wait_for("poll-card", Duration::from_secs(30));
    drop(app);
    first.stop();
    let _ = std::fs::remove_dir_all(&root);
}

/// Picks `/kick` from the list in the window, names `who` in its user chip and runs it (slash-commands.spec.ts
/// `runKickFromPicker`).
fn run_kick(app: &mut Driver, who: &str) {
    app.type_text("composer", "/");
    app.wait_for("command-picker", Duration::from_secs(10));
    app.type_focused("kick");
    app.wait_idle();
    // Greyed until this device knows its permissions here.
    app.wait_for("command-active-ready", Duration::from_secs(60));
    app.press("tab");
    app.wait_for("command-chips", Duration::from_secs(5));
    // w2 I-B1: the member is picked from the chip's list (Tab), so the command carries their id.
    app.type_focused(&format!("@{who}"));
    app.wait_for("command-suggestions", Duration::from_secs(10));
    app.press("tab");
    app.wait_idle();
    app.press("enter");
    app.wait_idle();
}

/// e2e/web/slash-commands.spec.ts "moderation requires confirmation and applies to a distinct joined member": in the
/// window, `/kick` naming a joined member asks first (the moderation dialog, its action button "Kick"). Cancel sends
/// nothing: a "Kick cancelled. Nothing changed." row, and the member is still in the server. Asked again and confirmed,
/// the member is removed (their own device loses the server) and the row says they were kicked.
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn a_moderation_command_asks_for_confirmation_then_applies_to_a_joined_member() {
    use dilla_client::state::CommunityId;
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t28-kick", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t28-kick"));
    let bea = rt.block_on(world::sign_up(&host, &invite, "bea", "Bea", "t28-kick"));
    let community = CommunityId(world::community(&host, &ada, "Midgard", &[&bea]));
    world::clear_session(&ada);
    world::clear_session(&bea);
    let member = rt.block_on(world::start(&bea));
    rt.block_on(world::wait_for(
        &member,
        Duration::from_secs(30),
        "bea in Midgard",
        |s| s.communities.iter().any(|c| c.id == community),
    ));

    let root = scratch_dir("t28-kick-app");
    let mut app = Driver::launch(&root);
    sign_in_app(&mut app, &host, &mut ada);

    run_kick(&mut app, &bea.username);
    app.wait_for("command-confirm", Duration::from_secs(30));
    assert!(
        app.element("bot-result-pending").is_none(),
        "nothing runs before the answer"
    );
    // Cancel (Escape on the dialog): nothing is sent, and the row says so.
    app.press("escape");
    app.wait_for("bot-result-cancelled", Duration::from_secs(10));
    assert!(
        app.element("command-confirm").is_none(),
        "the dialog closed"
    );
    std::thread::sleep(Duration::from_secs(2));
    assert!(
        member
            .state()
            .borrow()
            .communities
            .iter()
            .any(|c| c.id == community),
        "a cancelled kick changes nothing"
    );

    // Asked again, and confirmed.
    run_kick(&mut app, &bea.username);
    app.wait_for("command-confirm", Duration::from_secs(30));
    app.click("command-confirm");
    rt.block_on(world::wait_for(
        &member,
        Duration::from_secs(60),
        "bea's device to lose Midgard",
        |s| !s.communities.iter().any(|c| c.id == community),
    ));
    app.wait_for("bot-result-success", Duration::from_secs(30));
    assert!(app.element("command-confirm").is_none());
    drop(app);
    member.stop();
    let _ = std::fs::remove_dir_all(&root);
}

/// Final review I4: an open /kick or /ban confirmation lets go of its client when the client is released (Forget, Sign
/// out, an instance's Remove), whether or not its window draws again, so the store can close: the dialog goes, and the
/// caller's handle is the last.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn a_release_drops_an_open_moderation_confirmation(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;

    use dilla_client::features::bot::{ModAction, Moderation};
    use dilla_client::state::bot::Conversation;
    use dilla_client::state::{ChannelId, CommunityId, UserId};
    use dilla_desktop::screens::commands::confirm;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("i4-confirm", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let alice = rt.block_on(world::sign_up(&host, &invite, "alice", "Alice", "i4"));
    let client = Rc::new(rt.block_on(world::start(&alice)));
    cx.update(dilla_desktop::install);
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, _| window.window_handle().window_id());
    let confirming = confirm::Confirming {
        moderation: Moderation {
            community: CommunityId([1; 16]),
            user: UserId([2; 16]),
            name: "Bea".into(),
            username: "bea".into(),
            action: ModAction::Kick,
        },
        conversation: Conversation {
            channel: ChannelId([3; 16]),
            thread: None,
        },
        client: client.clone(),
    };
    vcx.update(|_, cx| confirm::ask(window, confirming, cx));
    assert!(vcx.update(|_, cx| confirm::is_open(window, cx)));
    assert_eq!(Rc::strong_count(&client), 2, "the dialog holds the client");

    vcx.update(|_, cx| dilla_desktop::release::release_client(&client, cx));
    assert!(
        !vcx.update(|_, cx| confirm::is_open(window, cx)),
        "the dialog went"
    );
    assert_eq!(
        Rc::strong_count(&client),
        1,
        "nothing but the caller holds the client"
    );
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}

/// Task 8 (CommandHost.tsx:294, `cancel && !moderation`): cancelling a built-in's question, or clicking outside it, takes
/// the command's pending row away; nothing is left to read "Not confirmed". Run with a client, so a row exists before
/// the cancel; the claim is checked against the row's presence first, or the test could not fail.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn a_cancelled_builtin_question_leaves_no_row(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;

    use dilla_client::features::bot::{self, Ask, Builtin};
    use dilla_client::state::bot::{Conversation, ResultProvider, ResultStatus};
    use dilla_client::state::{ChannelId, MessageId};
    use dilla_desktop::screens::commands::confirm;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t8-cancel", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let alice = rt.block_on(world::sign_up(&host, &invite, "alice", "Alice", "t8"));
    let client = Rc::new(rt.block_on(world::start(&alice)));
    cx.update(dilla_desktop::install);
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, _| window.window_handle().window_id());
    let conversation = Conversation {
        channel: ChannelId([3; 16]),
        thread: None,
    };
    // A row is published to the snapshot a moment after it is added or dismissed: wait for the count to settle.
    let rows = |client: &dilla_client::Client, want: usize| {
        for _ in 0..100 {
            let n = client.state().borrow().bot.results(&conversation).len();
            if n == want {
                return n;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        client.state().borrow().bot.results(&conversation).len()
    };
    for answer in [Some(false), None] {
        let row = client.bot().add_result(
            conversation,
            ResultStatus::Pending,
            "/delete…".into(),
            ResultProvider::Builtin,
        );
        assert_eq!(
            rows(&client, 1),
            1,
            "the pending row is there before the answer"
        );
        let asking = confirm::AskingBuiltin {
            ask: Ask {
                title: "Delete this message?".into(),
                body: "This permanently deletes your message.".into(),
            },
            run: Builtin::DeleteOwn {
                msg: MessageId([4; 16]),
            },
            row,
            attempt: bot::Attempt {
                request: dilla_client::features::roles::request_id(),
                invocation: String::new(),
                text: String::new(),
            },
            conversation,
            client: Some(client.clone()),
        };
        vcx.update(|_, cx| confirm::ask_builtin(window, asking, cx));
        assert!(vcx.update(|_, cx| confirm::is_open(window, cx)));
        vcx.update(|_, cx| confirm::answer_builtin(window, answer, cx));
        assert!(!vcx.update(|_, cx| confirm::is_open(window, cx)));
        assert_eq!(rows(&client, 0), 0, "answer {answer:?} leaves no row");
    }
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}

/// Task 8's `Plan::Builtin` arm with a client, as the composer sends (`on_send`): the command's row is pending from the
/// start (CommandHost.tsx:236). `/delete` of an own message then asks, and abandoning it takes the row away; `/pin` asks
/// nothing and runs, and its row says how it ended (`Bot::finish`): a failure here, as this account is in no such DM.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn a_builtin_sent_from_the_composer_shows_its_row_then_asks_or_runs(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;
    use std::sync::Arc;

    use dilla_client::state::bot::{Conversation, ResultStatus};
    use dilla_client::state::{
        AppState, ChannelGroupState, ChannelId, MessageId, Timeline, TimelineItem,
        TimelineItemState, UserId,
    };
    use dilla_desktop::screens::commands::{self, SendDecision, confirm};
    use dilla_desktop::slots::SlotCx;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t10-send", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let alice = rt.block_on(world::sign_up(&host, &invite, "alice", "Alice", "t10"));
    let client = Rc::new(rt.block_on(world::start(&alice)));
    cx.update(dilla_desktop::install);
    cx.executor().allow_parking();
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, _| window.window_handle().window_id());
    // A DM (no community) whose timeline the frame read: Alice's own stored message, not pinned.
    let (dm, msg) = (ChannelId([3; 16]), MessageId([4; 16]));
    let mut state = AppState::default();
    state.account.user_id = Some(UserId(alice.user_id));
    state.timelines.insert(
        dm,
        Timeline {
            channel_id: dm,
            group: ChannelGroupState::Active,
            items: vec![TimelineItem {
                key: "s1".into(),
                state: TimelineItemState::Ok,
                reason: String::new(),
                sender_user: Some(UserId(alice.user_id)),
                sender_device: None,
                own: true,
                bot: false,
                ts: 0,
                body: "hello".into(),
                msg_id: Some(msg),
                seq: Some(1),
                edited: false,
                reply: None,
                reactions: Vec::new(),
                pinned: false,
                attachments: Vec::new(),
                mention: false,
                actions: Vec::new(),
            }],
            has_earlier: false,
        },
    );
    let ctx = SlotCx {
        state: Arc::new(state),
        client: Some(client.clone()),
        channel: Some(dm),
        ..SlotCx::default()
    };
    let conversation = Conversation {
        channel: dm,
        thread: None,
    };
    // The rows as the client last published them.
    let rows = |client: &dilla_client::Client| -> Vec<ResultStatus> {
        client
            .state()
            .borrow()
            .bot
            .results(&conversation)
            .iter()
            .map(|r| r.status)
            .collect()
    };
    let settle = |cx: &mut gpui::VisualTestContext, want: &dyn Fn(&[ResultStatus]) -> bool| {
        for _ in 0..200 {
            cx.run_until_parked();
            if want(&rows(&client)) {
                return;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        panic!("rows {:?}", rows(&client));
    };
    let send = |cx: &mut gpui::VisualTestContext, draft: String| {
        cx.update(|_, cx| commands::on_send(&ctx, &draft, &|t| t.to_owned(), window, cx))
    };

    let id = msg.hex();
    assert_eq!(
        send(vcx, format!("/delete message:{id}")),
        SendDecision::Handled
    );
    settle(vcx, &|r| r == [ResultStatus::Pending]);
    assert!(vcx.update(|_, cx| confirm::is_open(window, cx)), "it asks");
    vcx.update(|_, cx| confirm::answer_builtin(window, None, cx));
    settle(vcx, &|r| r.is_empty());

    assert_eq!(
        send(vcx, format!("/pin message:{id}")),
        SendDecision::Handled
    );
    assert!(
        !vcx.update(|_, cx| confirm::is_open(window, cx)),
        "/pin asks nothing"
    );
    settle(vcx, &|r| r == [ResultStatus::Failed]);
    drop(ctx);
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}

/// builtin.ts `reply`, `edit`, `pin` and `delete` in the window: Ada's `/reply` to her own message posts a reply that
/// names it, `/edit` changes its text, `/pin` pins it and `/delete`, confirmed, removes it, as Bea's device sees; each
/// row ends in success. A command publishes only its explicit text (`text:`), never the draft.
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn message_commands_reply_edit_pin_and_delete() {
    use dilla_client::state::{AppState, ChannelId, CommunityId, TimelineItem};
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t10-msg", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t10-msg"));
    let bea = rt.block_on(world::sign_up(&host, &invite, "bea", "Bea", "t10-msg"));
    let community = CommunityId(world::community(&host, &ada, "Midgard", &[&bea]));
    world::clear_session(&ada);
    world::clear_session(&bea);
    let member = rt.block_on(world::start(&bea));
    let general: ChannelId = rt.block_on(async {
        let s = world::wait_for(&member, Duration::from_secs(30), "#general", |s| {
            s.channel_named(community, "general").is_some()
        })
        .await;
        let id = s.channel_named(community, "general").unwrap().id;
        member
            .open_channel(id)
            .await
            .unwrap()
            .expect("bea opens #general");
        id
    });
    // Bea's view of #general: the stored message whose text is `body`.
    let seen = |s: &AppState, body: &str| -> Option<TimelineItem> {
        s.timelines
            .get(&general)?
            .items
            .iter()
            .find(|i| i.body == body && i.seq.is_some())
            .cloned()
    };
    let wait = |what: &str, pred: &dyn Fn(&AppState) -> bool| {
        rt.block_on(world::wait_for(
            &member,
            Duration::from_secs(60),
            what,
            |s| pred(s),
        ))
    };

    let root = scratch_dir("t10-msg-app");
    let mut app = Driver::launch(&root);
    sign_in_app(&mut app, &host, &mut ada);
    app.type_text("composer", "first words");
    app.press("enter");
    let s = wait("Ada's message", &|s| seen(s, "first words").is_some());
    let first = seen(&s, "first words").unwrap().msg_id.unwrap();
    let id = first.hex();

    // A command typed whole: the list closes on Escape, Enter sends the draft.
    let command = |app: &mut Driver, draft: &str| {
        app.type_text("composer", draft);
        app.wait_idle();
        if app.element("command-picker").is_some() {
            app.press("escape");
            app.wait_idle();
        }
        app.press("enter");
        app.wait_idle();
    };
    let succeeded = |app: &mut Driver| {
        app.wait_for("bot-result-success", Duration::from_secs(30));
        for _ in 0..600 {
            if app.element("bot-result-pending").is_none() {
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        assert!(app.element("bot-result-pending").is_none(), "settled");
        assert!(app.element("bot-result-failed").is_none(), "no row failed");
    };

    command(&mut app, &format!("/reply message:{id} text:answer"));
    wait("the reply naming Ada's message", &|s| {
        seen(s, "answer").is_some_and(|i| i.reply.is_some_and(|r| r.msg_id == first))
    });
    succeeded(&mut app);

    command(&mut app, &format!("/edit message:{id} text:rewritten"));
    let s = wait("the edit", &|s| {
        seen(s, "rewritten").is_some_and(|i| i.msg_id == Some(first))
    });
    assert!(seen(&s, "rewritten").unwrap().edited);
    succeeded(&mut app);

    command(&mut app, &format!("/pin message:{id}"));
    wait("the pin", &|s| {
        seen(s, "rewritten").is_some_and(|i| i.pinned)
    });
    succeeded(&mut app);

    command(&mut app, &format!("/delete message:{id}"));
    app.wait_for("command-confirm", Duration::from_secs(30));
    app.click("command-confirm");
    wait("the delete", &|s| {
        s.timelines.get(&general).is_some_and(|t| {
            !t.items.iter().any(|i| {
                i.msg_id == Some(first) && i.state == dilla_client::state::TimelineItemState::Ok
            })
        })
    });
    assert!(
        app.element("command-confirm").is_none(),
        "the dialog closed"
    );
    succeeded(&mut app);

    drop(app);
    member.stop();
    let _ = std::fs::remove_dir_all(&root);
}

/// builtin.ts `dm` (:98-106) in the window: `/dm` naming Bea, from #general of Midgard, opens (or makes) the direct message
/// with her and goes there. The DM's composer names her.
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn a_dm_command_opens_the_conversation() {
    use dilla_client::state::CommunityId;
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t24-dm", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t24-dm"));
    let bea = rt.block_on(world::sign_up(&host, &invite, "bea", "Bea", "t24-dm"));
    let community = CommunityId(world::community(&host, &ada, "Midgard", &[&bea]));
    world::clear_session(&ada);
    world::clear_session(&bea);
    let member = rt.block_on(world::start(&bea));
    let bea_id = rt.block_on(world::wait_for(
        &member,
        Duration::from_secs(30),
        "Bea's own id",
        |s| s.account.user_id.is_some() && s.channel_named(community, "general").is_some(),
    ));
    let bea_hex = bea_id.account.user_id.unwrap().hex();

    let root = scratch_dir("t24-dm-app");
    let mut app = Driver::launch(&root);
    sign_in_app(&mut app, &host, &mut ada);
    app.type_text("composer", &format!("/dm user:{bea_hex}"));
    app.wait_idle();
    if app.element("command-picker").is_some() {
        app.press("escape");
        app.wait_idle();
    }
    app.press("enter");
    // The route is the DM with Bea: its composer is named for her (shell.dm.composer). The command's row stays in the
    // conversation it was typed in, so it is not on screen here: the window goes there only after the DM was opened, and
    // the row's success is written right after that.
    let deadline = std::time::Instant::now() + Duration::from_secs(30);
    while !app.a11y_tree().has("MultilineTextInput", "message Bea") {
        assert!(
            std::time::Instant::now() < deadline,
            "the window never went to the DM with Bea"
        );
        std::thread::sleep(Duration::from_millis(250));
    }
    drop(app);
    member.stop();
    let _ = std::fs::remove_dir_all(&root);
}

/// Picks `/{command}` from the list in the window and names `who` in its user chip (the member's id goes in): the next
/// field, if any, is then the active one. Nothing is sent yet.
fn pick_member_command(app: &mut Driver, command: &str, who: &str) {
    app.type_text("composer", "/");
    app.wait_for("command-picker", Duration::from_secs(10));
    app.type_focused(command);
    app.wait_idle();
    // Greyed until this device knows its permissions here.
    app.wait_for("command-active-ready", Duration::from_secs(60));
    app.press("tab");
    app.wait_for("command-chips", Duration::from_secs(5));
    app.type_focused(&format!("@{who}"));
    app.wait_for("command-suggestions", Duration::from_secs(10));
    app.press("tab");
    app.wait_idle();
}

/// builtin.ts `timeout`, `untimeout` and `warn` (:204-264) in the window: `/timeout` of Bea for 10 minutes asks first,
/// and confirmed, Bea's own device reads her timeout; `/untimeout` reads it, asks, and confirmed lifts it; `/warn`,
/// confirmed, ends in success. Every row ends in success.
#[test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn timeout_and_warn_commands_apply_after_confirmation() {
    use dilla_client::state::{CommunityId, UserId};
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t11-timeout", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let mut ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t11-timeout"));
    let bea = rt.block_on(world::sign_up(&host, &invite, "bea", "Bea", "t11-timeout"));
    let community = CommunityId(world::community(&host, &ada, "Midgard", &[&bea]));
    world::clear_session(&ada);
    world::clear_session(&bea);
    let member = rt.block_on(world::start(&bea));
    rt.block_on(world::wait_for(
        &member,
        Duration::from_secs(30),
        "bea in Midgard",
        |s| s.communities.iter().any(|c| c.id == community),
    ));
    let bea_id = UserId(bea.user_id);
    // Bea's timeout as her own device reads it, once it is `held`.
    let timed_out = |held: bool| {
        for _ in 0..600 {
            let now = rt
                .block_on(member.moderation().timeout_of(community, bea_id))
                .expect("bea reads her timeout");
            if now.is_some() == held {
                return;
            }
            std::thread::sleep(Duration::from_millis(100));
        }
        panic!(
            "bea's timeout is never {}",
            if held { "set" } else { "lifted" }
        );
    };
    let confirm = |app: &mut Driver| {
        app.wait_for("command-confirm", Duration::from_secs(30));
        app.click("command-confirm");
        app.wait_idle();
    };
    let succeeded = |app: &mut Driver| {
        app.wait_for("bot-result-success", Duration::from_secs(30));
        for _ in 0..600 {
            if app.element("bot-result-pending").is_none() {
                break;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        assert!(app.element("bot-result-pending").is_none(), "settled");
        assert!(app.element("bot-result-failed").is_none(), "no row failed");
    };

    let root = scratch_dir("t11-timeout-app");
    let mut app = Driver::launch(&root);
    sign_in_app(&mut app, &host, &mut ada);

    // The duration chip: "10 min" is completed from its suggestions to "10 minutes" (10m).
    pick_member_command(&mut app, "timeout", &bea.username);
    app.type_focused("10 min");
    app.wait_for("command-suggestions", Duration::from_secs(10));
    app.press("tab");
    app.wait_idle();
    app.press("enter");
    app.wait_idle();
    confirm(&mut app);
    timed_out(true);
    succeeded(&mut app);

    pick_member_command(&mut app, "untimeout", &bea.username);
    app.press("enter");
    app.wait_idle();
    // The question comes once her timeout was read.
    confirm(&mut app);
    timed_out(false);
    succeeded(&mut app);

    pick_member_command(&mut app, "warn", &bea.username);
    app.type_focused("spam links");
    app.press("enter");
    app.wait_idle();
    confirm(&mut app);
    succeeded(&mut app);
    assert!(
        app.element("command-confirm").is_none(),
        "the dialog closed"
    );

    drop(app);
    member.stop();
    let _ = std::fs::remove_dir_all(&root);
}

/// Final review M7: what a window's command picker and reminders popover keep goes with its client's release: a typed
/// command (a ban reason) does not stay in the picker after Forget or Sign out, and the popover's first read runs again
/// for the next sign-in.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn a_release_clears_the_windows_command_draft_and_reminders(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;

    use dilla_desktop::screens::commands::{self, reminders};
    use dilla_desktop::screens::create;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("m7-picker", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let alice = rt.block_on(world::sign_up(&host, &invite, "alice", "Alice", "m7"));
    let client = Rc::new(rt.block_on(world::start(&alice)));
    cx.update(dilla_desktop::install);
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, cx| {
        // The shell attaches its client to its window as it starts.
        create::attach(window, client.clone(), Rc::new(|_, _, _| {}), cx);
        window.window_handle().window_id()
    });
    vcx.update(|_, cx| {
        commands::draft_changed(window, "/ban bea spam", cx);
        reminders::toggle(window, cx);
    });
    let state = client.state().borrow().clone();
    assert_eq!(
        vcx.update(|_, cx| commands::picker_state(window, cx).draft),
        "/ban bea spam"
    );
    assert!(vcx.update(|_, cx| reminders::bell_state(window, &state, cx).1));

    vcx.update(|_, cx| dilla_desktop::release::release_client(&client, cx));
    assert_eq!(
        vcx.update(|_, cx| commands::picker_state(window, cx)),
        commands::PickerState::default(),
        "the typed command went"
    );
    assert!(
        !vcx.update(|_, cx| reminders::bell_state(window, &state, cx).1),
        "the popover closed and forgot its reads"
    );
    assert_eq!(Rc::strong_count(&client), 1);
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}

/// builtin.ts `selfrole list`, `selfrole add` and `selfrole remove` (:267-271, :405-413), sent as the composer sends
/// (`on_send`) by a member of a community whose owner offers "Crew" as a self role: the list shows it unchecked, `add`
/// picks it, the list then shows it checked, `remove` drops it, and asking to drop it again changes nothing. Each row's
/// text is read from the client's private results.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn selfrole_commands_list_add_and_remove(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;

    use dilla_client::features::server_admin::hex_id;
    use dilla_client::state::CommunityId;
    use dilla_client::state::bot::{Conversation, ResultStatus};
    use dilla_desktop::screens::commands::{self, SendDecision};
    use dilla_desktop::slots::SlotCx;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t25-selfrole", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t25-selfrole"));
    let bea = rt.block_on(world::sign_up(&host, &invite, "bea", "Bea", "t25-selfrole"));
    let c = CommunityId(world::community(&host, &ada, "Midgard", &[&bea]));
    world::clear_session(&ada);
    world::clear_session(&bea);
    let within = Duration::from_secs(30);
    // Ada, the owner, makes "Crew" and offers it as a self role.
    let owner = rt.block_on(world::start(&ada));
    let crew = rt.block_on(async {
        world::wait_for(&owner, within, "Midgard", |s| {
            s.communities.iter().any(|x| x.id == c)
        })
        .await;
        owner.roles().load_access(c).await.unwrap();
        let crew = owner
            .roles()
            .create_role(c, "Crew", 0, false, false, 0)
            .await
            .expect("ada makes Crew");
        owner
            .roles()
            .configure_self_roles(c, &[crew])
            .await
            .expect("ada offers Crew");
        crew
    });
    owner.stop();

    let client = Rc::new(rt.block_on(world::start(&bea)));
    let general = rt
        .block_on(world::wait_for(&client, within, "#general", |s| {
            s.channel_named(c, "general").is_some()
        }))
        .channel_named(c, "general")
        .unwrap()
        .id;
    cx.update(dilla_desktop::install);
    cx.executor().allow_parking();
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, _| window.window_handle().window_id());
    let ctx = SlotCx {
        state: client.state().borrow().clone(),
        client: Some(client.clone()),
        community: Some(c),
        channel: Some(general),
        ..SlotCx::default()
    };
    let conversation = Conversation {
        channel: general,
        thread: None,
    };
    // Sends `draft` and answers its row once it settled (each command adds one row; none asks first).
    let mut sent = 0;
    let mut run = |cx: &mut gpui::VisualTestContext, draft: &str| -> (ResultStatus, String) {
        assert_eq!(
            cx.update(|_, cx| commands::on_send(&ctx, draft, &|t| t.to_owned(), window, cx)),
            SendDecision::Handled
        );
        sent += 1;
        for _ in 0..600 {
            cx.run_until_parked();
            let last = client
                .state()
                .borrow()
                .bot
                .results(&conversation)
                .last()
                .filter(|_| client.state().borrow().bot.results(&conversation).len() == sent)
                .map(|r| (r.status, r.text.clone()));
            if let Some(last) = last.filter(|(s, _)| *s != ResultStatus::Pending) {
                return last;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        panic!("{draft}: the row never settled");
    };
    let done = |text: &str| (ResultStatus::Success, text.to_owned());
    let role = hex_id(&crew);

    assert_eq!(run(vcx, "/selfrole list"), done("[ ] Crew"));
    assert_eq!(
        run(vcx, &format!("/selfrole add role:{role}")),
        done("Crew added.")
    );
    assert_eq!(run(vcx, "/selfrole list"), done("[x] Crew"));
    assert_eq!(
        run(vcx, &format!("/selfrole remove role:{role}")),
        done("Crew removed.")
    );
    assert_eq!(
        run(vcx, &format!("/selfrole remove role:{role}")),
        done("You do not have Crew.")
    );
    assert_eq!(
        run(vcx, &format!("/selfrole add role:{}", hex_id(&[0xee; 16]))),
        (
            ResultStatus::Failed,
            "That role is not currently self-assignable.".to_owned()
        )
    );
    drop(ctx);
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}

/// builtin.ts `updateMemberRole` (:395-404) as the owner sends it: `/role add` of "Crew" to Bea asks nothing and grants
/// it; `/role remove` asks first, and run as its confirmation runs it, revokes it; the same removal run once more is
/// checked again against the live state and refused, as Bea no longer holds the role.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn role_commands_grant_then_ask_and_revoke(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;

    use dilla_client::features::bot::{self, Plan};
    use dilla_client::features::server_admin::hex_id;
    use dilla_client::state::bot::{Conversation, ResultProvider, ResultStatus};
    use dilla_client::state::{AppState, CommunityId, UserId};
    use dilla_desktop::screens::commands::{self, SendDecision, builtins, confirm};
    use dilla_desktop::slots::SlotCx;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t25-role", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t25-role"));
    let bea = rt.block_on(world::sign_up(&host, &invite, "bea", "Bea", "t25-role"));
    let c = CommunityId(world::community(&host, &ada, "Midgard", &[&bea]));
    world::clear_session(&ada);
    world::clear_session(&bea);
    let within = Duration::from_secs(30);
    let bea_id = UserId(bea.user_id);
    let client = Rc::new(rt.block_on(world::start(&ada)));
    let crew = rt.block_on(async {
        world::wait_for(&client, within, "both members", |s| {
            s.members.get(&c).is_some_and(|m| m.len() == 2)
        })
        .await;
        client.roles().load_access(c).await.unwrap();
        client
            .roles()
            .create_role(c, "Crew", 0, false, false, 0)
            .await
            .expect("ada makes Crew")
    });
    // Bea holds Crew (`held`) in Ada's view, the roles and Ada's access loaded: the state the frame reads.
    let holds = |held: bool| -> std::sync::Arc<AppState> {
        rt.block_on(world::wait_for(&client, within, "Bea's roles", |s| {
            s.roles.access_of(c).loaded
                && s.roles.roles_of(c).iter().any(|r| r.id == crew)
                && s.members.get(&c).is_some_and(|m| {
                    m.iter()
                        .any(|m| m.user_id == bea_id && m.role_ids.contains(&crew) == held)
                })
        }))
    };
    let general = holds(false).channel_named(c, "general").unwrap().id;
    cx.update(dilla_desktop::install);
    cx.executor().allow_parking();
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, _| window.window_handle().window_id());
    let conversation = Conversation {
        channel: general,
        thread: None,
    };
    let ctx_of = |state| SlotCx {
        state,
        client: Some(client.clone()),
        community: Some(c),
        channel: Some(general),
        ..SlotCx::default()
    };
    // The last row once it settled (a row is published a moment after it is added or finished).
    let settled = |cx: &mut gpui::VisualTestContext| -> (ResultStatus, String) {
        for _ in 0..600 {
            cx.run_until_parked();
            let last = client
                .state()
                .borrow()
                .bot
                .results(&conversation)
                .last()
                .map(|r| (r.status, r.text.clone()));
            if let Some(last) = last.filter(|(s, _)| *s != ResultStatus::Pending) {
                return last;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        panic!("the row never settled");
    };
    let (bea_hex, crew_hex) = (bea_id.hex(), hex_id(&crew));

    let ctx = ctx_of(client.state().borrow().clone());
    let add = format!("/role add user:{bea_hex} role:{crew_hex}");
    assert_eq!(
        vcx.update(|_, cx| commands::on_send(&ctx, &add, &|t| t.to_owned(), window, cx)),
        SendDecision::Handled
    );
    assert!(
        !vcx.update(|_, cx| confirm::is_open(window, cx)),
        "adding asks nothing"
    );
    assert_eq!(
        settled(vcx),
        (ResultStatus::Success, "Crew added to Bea.".to_owned())
    );
    drop(ctx);
    let ctx = ctx_of(holds(true));

    let remove = format!("/role remove user:{bea_hex} role:{crew_hex}");
    assert_eq!(
        vcx.update(|_, cx| commands::on_send(&ctx, &remove, &|t| t.to_owned(), window, cx)),
        SendDecision::Handled
    );
    assert!(
        vcx.update(|_, cx| confirm::is_open(window, cx)),
        "removing asks"
    );
    vcx.update(|_, cx| confirm::answer_builtin(window, None, cx));
    // The question is answered by its dialog, which this window does not draw: its run is what a confirmation runs.
    let Plan::Builtin { run, .. } = bot::plan_in(&ctx.state, Some(c), Some(general), &remove)
    else {
        panic!("/role remove plans a built-in");
    };
    let revoke = |cx: &mut gpui::VisualTestContext| {
        let row = client.bot().add_result(
            conversation,
            ResultStatus::Pending,
            "/role remove…".into(),
            ResultProvider::Builtin,
        );
        let run = run.clone();
        cx.update(|window, cx| {
            builtins::run(
                run,
                row,
                commands::attempt_of(&ctx, conversation, &remove),
                conversation,
                &ctx,
                window.window_handle().window_id(),
                cx,
            )
        });
        settled(cx)
    };
    assert_eq!(
        revoke(vcx),
        (ResultStatus::Success, "Crew removed from Bea.".to_owned())
    );
    holds(false);
    assert_eq!(
        revoke(vcx),
        (
            ResultStatus::Failed,
            "Your permissions or role rank changed.".to_owned()
        )
    );
    drop(ctx);
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}

/// Task 28: scheduled announcements against a real dillad and its Bot (bot-handlers.ts `announcementHandler`). The owner
/// schedules a daily announcement from the editor (the fields as typed, the repeat chosen), the Bot answers "Announcement
/// scheduled."; `/announcement list` lists it as `{id}: {text} — daily, UTC 09:00`; `/announcement delete` of it asks
/// first and, confirmed, answers "Scheduled announcement deleted."; the list is empty again.
#[gpui::test]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
fn an_announcement_scheduled_from_the_editor_is_listed(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;

    use dilla_client::features::bot::announcements::ScheduleKind;
    use dilla_client::features::bot::{self, Plan};
    use dilla_client::features::roles::viewer_channel_bits;
    use dilla_client::state::CommunityId;
    use dilla_client::state::bot::{Conversation, ResultStatus};
    use dilla_client::state::bot_admin::sealed_operation;
    use dilla_desktop::screens::commands::{self, SendDecision, announcement_form};
    use dilla_desktop::slots::SlotCx;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_bot_host("t28-announce", "127.0.0.1:0");
    let invite = host.invites.last().unwrap().clone();
    let ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t28-announce"));
    let c = CommunityId(world::community(&host, &ada, "Announcements", &[]));
    world::clear_session(&ada);
    let client = Rc::new(rt.block_on(world::start(&ada)));
    rt.block_on(world::bot::enable_bot(
        &client,
        c.0,
        world::bot::cap::ANNOUNCEMENTS,
    ));
    let general = rt
        .block_on(world::wait_for(
            &client,
            Duration::from_secs(30),
            "#general",
            |s| s.channel_named(c, "general").is_some(),
        ))
        .channel_named(c, "general")
        .unwrap()
        .id;
    // A schedule posts into a Bot channel: #general is made one.
    rt.block_on(client.bot_admin().mark_bot_channel(c, general))
        .expect("an answer")
        .expect("#general is marked a Bot channel");
    // The Bot's catalog lists the typed `/announcement` commands and its announcement operation, once its key is verified.
    let ready = |state: &dilla_client::state::AppState| {
        matches!(
            bot::plan_in(state, Some(c), Some(general), "/announcement list"),
            Plan::Bot { .. }
        ) && state.bot.catalogs.get(&c).is_some_and(|v| {
            v.commands
                .iter()
                .any(|x| u64::from(x.operation) == sealed_operation(50))
        })
    };
    let deadline = std::time::Instant::now() + Duration::from_secs(90);
    while !ready(&client.state().borrow()) {
        assert!(
            std::time::Instant::now() < deadline,
            "the Bot's catalog never offered announcements: {:?} {:?}",
            host.bot_refusals(),
            client.state().borrow().bot.catalogs.get(&c).map(|v| v
                .commands
                .iter()
                .map(|x| (x.name.clone(), x.operation))
                .collect::<Vec<_>>())
        );
        let _ = rt.block_on(client.bot().sync_catalog(c));
        std::thread::sleep(Duration::from_secs(1));
    }
    // The shell hands the open channel's resolved bits to the threads area; the owner holds everything.
    client.threads().set_channel_bits(
        general,
        viewer_channel_bits(&client.state().borrow(), general),
    );

    cx.update(dilla_desktop::install);
    cx.executor().allow_parking();
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, _| window.window_handle().window_id());
    let ctx = SlotCx {
        state: client.state().borrow().clone(),
        client: Some(client.clone()),
        community: Some(c),
        channel: Some(general),
        ..SlotCx::default()
    };
    let conversation = Conversation {
        channel: general,
        thread: None,
    };
    // Sends `draft`; `drive` runs on every pass while its row is pending (it draws the overlays, answers the question or
    // fills the editor); answers the row once it settled.
    let mut sent = 0;
    let mut run = |cx: &mut gpui::VisualTestContext,
                   draft: &str,
                   drive: &mut dyn FnMut(&mut gpui::VisualTestContext)|
     -> (ResultStatus, String) {
        assert_eq!(
            cx.update(|_, cx| commands::on_send(&ctx, draft, &|t| t.to_owned(), window, cx)),
            SendDecision::Handled
        );
        sent += 1;
        for _ in 0..600 {
            cx.run_until_parked();
            drive(cx);
            let last = client
                .state()
                .borrow()
                .bot
                .results(&conversation)
                .last()
                .filter(|_| client.state().borrow().bot.results(&conversation).len() == sent)
                .map(|r| (r.status, r.text.clone()));
            if let Some(last) = last.filter(|(s, _)| *s != ResultStatus::Pending) {
                return last;
            }
            std::thread::sleep(Duration::from_millis(50));
        }
        panic!("{draft}: the row never settled");
    };
    let none = &mut |_: &mut gpui::VisualTestContext| {};

    // The editor: drawn once the Bot's list was read, then filled as a person would (the message, daily at 09:00 UTC).
    let mut filled = false;
    let created = run(vcx, "/announcement create", &mut |cx| {
        cx.update(|window, cx| {
            commands::announcement_form::overlay(&ctx, window, cx);
            let Some(form) = announcement_form::form_of(window.window_handle().window_id(), cx)
            else {
                return;
            };
            if filled {
                return;
            }
            filled = true;
            let (message, hour, minute, zone) = (
                form.message.clone(),
                form.hour.clone(),
                form.minute.clone(),
                form.timezone.clone(),
            );
            message.update(cx, |s, cx| {
                s.set_value("Stand-up in the harbour", window, cx)
            });
            hour.update(cx, |s, cx| s.set_value("09", window, cx));
            minute.update(cx, |s, cx| s.set_value("00", window, cx));
            zone.update(cx, |s, cx| s.set_value("UTC", window, cx));
            announcement_form::choose_kind(
                window.window_handle().window_id(),
                ScheduleKind::Daily,
                cx,
            );
            announcement_form::submit(window.window_handle().window_id(), cx);
        });
    });
    assert_eq!(
        created,
        (ResultStatus::Success, "Announcement scheduled.".to_owned()),
        "{:?}",
        host.bot_refusals()
    );

    let (status, listed) = run(vcx, "/announcement list", none);
    assert_eq!(status, ResultStatus::Success, "{listed}");
    let (id, summary) = listed.split_once(": ").expect("{id}: {summary}");
    assert_eq!(summary, "Stand-up in the harbour — daily, UTC 09:00");
    assert_eq!(id.len(), 32);
    let id = id.to_owned();
    assert_eq!(
        client
            .state()
            .borrow()
            .bot
            .announcements
            .get(&c)
            .map(Vec::len),
        Some(1),
        "the chips hold what the list read"
    );

    // Deleting asks first; confirmed, it deletes.
    let deleted = run(
        vcx,
        &format!("/announcement delete announcement:{id}"),
        &mut |cx| {
            cx.update(|window, cx| {
                commands::announcement_form::overlay(&ctx, window, cx);
                announcement_form::answer_delete(
                    window.window_handle().window_id(),
                    Some(true),
                    cx,
                );
            });
        },
    );
    assert_eq!(
        deleted,
        (
            ResultStatus::Success,
            "Scheduled announcement deleted.".to_owned()
        ),
        "{:?}",
        host.bot_refusals()
    );
    assert_eq!(
        run(vcx, "/announcement list", none),
        (
            ResultStatus::Success,
            "No scheduled announcements.".to_owned()
        )
    );
    drop(ctx);
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}

/// A GPUI-only reference (`native/announcement-editor`, Decision D2): `/announcement create` opens the editor, drawn with
/// the Signal `Dialog`, `Select` and `TextField` where the web's `BotCommandDialogs` uses browser widgets (a native
/// `<select>`, `<input type="date">`, `<input type="number">`). The fields are the web's, in its order and with its
/// labels: Destination channel, Message, Repeat, Date, Hour, Minute, Time zone, then Close, Cancel and Save. The values
/// are the opening's: the first text channel, once, a minute after the frame's clock (2026-11-12 11:09 UTC).
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_announcement_editor() {
    dilla_parity::screen::assert_native_screen(
        &fixture("commands/announcement-editor.json"),
        "announcement-editor",
        |app| {
            // The fixture's steps are not replayed for a native frame: the same ones, by hand.
            app.type_text("composer", "/announcement create");
            app.wait_for("command-picker", Duration::from_secs(10));
            app.press("tab");
            app.wait_for("command-chips", Duration::from_secs(5));
            app.press("enter");
            app.wait_for("announcement-message", Duration::from_secs(10));
        },
    )
    .unwrap_or_else(|e| panic!("commands/announcement-editor differs from its golden shot: {e}"));
}

/// `/announcement create` typed in the composer and sent from its chips opens the editor on the server whose Bot offers
/// announcements, its row pending under the log; Cancel closes it and the row says the editing was cancelled; Save with an
/// empty message names the refusal in the editor and keeps it open.
#[test]
fn the_composer_opens_the_announcement_editor() {
    let mut app = Driver::launch_fixture(&fixture("commands/announcement-editor.json"), None, &[]);
    app.type_text("composer", "/announcement create");
    app.wait_for("command-picker", Duration::from_secs(10));
    app.press("tab");
    app.wait_for("command-chips", Duration::from_secs(5));
    app.press("enter");
    app.wait_for("announcement-message", Duration::from_secs(10));
    assert!(app.element("announcement-recurrence").is_some());
    assert!(
        app.element("announcement-date").is_some(),
        "a one-off has its date"
    );
    app.click("announcement-save");
    app.wait_for("announcement-error", Duration::from_secs(5));
    assert!(
        app.element("announcement-message").is_some(),
        "Save kept it open"
    );
    app.click("announcement-cancel");
    app.wait_idle();
    assert!(
        app.element("announcement-message").is_none(),
        "Cancel closed it"
    );
    app.wait_for("bot-result-failed", Duration::from_secs(5));
}

/// Task 29: `/rolepanel create` against a real dillad and its Bot (bot-handlers.ts `rolePanelHandler`). The owner approves
/// Crew for self service and makes #general a Bot channel; the editor opens from the command, Crew is checked with the
/// emoji 🎮 and Publish has the Bot post the panel and registers it ("Role panel published and registered."). Bea sees
/// the panel and reacts 🎮: the reaction grants her Crew.
#[gpui::test]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
fn a_role_panel_published_from_the_command_registers(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;

    use dilla_client::features::bot::{self, Plan};
    use dilla_client::features::roles::viewer_channel_bits;
    use dilla_client::state::CommunityId;
    use dilla_client::state::bot::{Conversation, ResultStatus};
    use dilla_client::state::bot_admin::sealed_operation;
    use dilla_desktop::screens::commands::{self, SendDecision, role_panel_form};
    use dilla_desktop::slots::SlotCx;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_bot_host("t29-panel", "127.0.0.1:0");
    let invite = host.invites.last().unwrap().clone();
    let ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t29-panel"));
    let bea = rt.block_on(world::sign_up(&host, &invite, "bea", "Bea", "t29-panel"));
    let c = CommunityId(world::community(&host, &ada, "Panels", &[&bea]));
    world::clear_session(&ada);
    world::clear_session(&bea);
    let within = Duration::from_secs(30);
    let client = Rc::new(rt.block_on(world::start(&ada)));
    rt.block_on(world::bot::enable_bot(&client, c.0, world::bot::cap::ROLES));
    let general = rt
        .block_on(world::wait_for(&client, within, "#general", |s| {
            s.channel_named(c, "general").is_some()
        }))
        .channel_named(c, "general")
        .unwrap()
        .id;
    // Ada, the owner, makes "Crew", offers it as a self role, and makes #general a Bot channel (a panel posts into one).
    let crew = rt.block_on(async {
        client.roles().load_access(c).await.unwrap();
        let crew = client
            .roles()
            .create_role(c, "Crew", 0, false, false, 0)
            .await
            .expect("ada makes Crew");
        client
            .roles()
            .configure_self_roles(c, &[crew])
            .await
            .expect("ada offers Crew");
        crew
    });
    rt.block_on(client.bot_admin().mark_bot_channel(c, general))
        .expect("an answer")
        .expect("#general is marked a Bot channel");
    // The Bot's catalog lists the typed `/rolepanel create` and its panel operation, once its key is verified.
    let ready = |state: &dilla_client::state::AppState| {
        matches!(
            bot::plan_in(state, Some(c), Some(general), "/rolepanel create"),
            Plan::Bot { .. }
        ) && state.bot.catalogs.get(&c).is_some_and(|v| {
            v.commands
                .iter()
                .any(|x| u64::from(x.operation) == sealed_operation(31))
        })
    };
    let deadline = std::time::Instant::now() + Duration::from_secs(90);
    while !ready(&client.state().borrow()) {
        assert!(
            std::time::Instant::now() < deadline,
            "the Bot's catalog never offered the role panel: {:?} {:?}",
            host.bot_refusals(),
            client.state().borrow().bot.catalogs.get(&c).map(|v| v
                .commands
                .iter()
                .map(|x| (x.name.clone(), x.operation))
                .collect::<Vec<_>>())
        );
        let _ = rt.block_on(client.bot().sync_catalog(c));
        std::thread::sleep(Duration::from_secs(1));
    }
    // The shell hands the open channel's resolved bits to the threads area; the owner holds everything.
    client.threads().set_channel_bits(
        general,
        viewer_channel_bits(&client.state().borrow(), general),
    );
    // A channel has its text group once someone opens it; the Bot posts a panel only into a channel that has one (and
    // waits itself for its join of a newly marked Bot channel).
    rt.block_on(client.open_channel(general))
        .unwrap()
        .expect("ada opens general");
    // Bea is in the channel's group before the panel is posted, so she receives it.
    let b = rt.block_on(world::start(&bea));
    let s = rt.block_on(world::wait_for(&b, within, "#general at bea", |s| {
        s.channel_named(c, "general").is_some()
    }));
    assert_eq!(s.channel_named(c, "general").unwrap().id, general);
    rt.block_on(b.open_channel(general))
        .unwrap()
        .expect("bea opens general");

    cx.update(dilla_desktop::install);
    cx.executor().allow_parking();
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, _| window.window_handle().window_id());
    let ctx = SlotCx {
        state: client.state().borrow().clone(),
        client: Some(client.clone()),
        community: Some(c),
        channel: Some(general),
        ..SlotCx::default()
    };
    let conversation = Conversation {
        channel: general,
        thread: None,
    };
    assert_eq!(
        vcx.update(|_, cx| commands::on_send(
            &ctx,
            "/rolepanel create",
            &|t| t.to_owned(),
            window,
            cx
        )),
        SendDecision::Handled
    );
    // The editor is drawn once the approved roles were read, then filled as a person would: Crew, with 🎮.
    let mut filled = false;
    let mut answered = None;
    for _ in 0..1200 {
        vcx.run_until_parked();
        vcx.update(|window, cx| {
            commands::role_panel_form::overlay(&ctx, window, cx);
            let w = window.window_handle().window_id();
            let Some(form) = role_panel_form::form_of(w, cx) else {
                return;
            };
            if filled {
                return;
            }
            filled = true;
            let emoji = form.emojis[&crew].clone();
            role_panel_form::toggle_role(w, crew, cx);
            emoji.update(cx, |s, cx| s.set_value("🎮", window, cx));
            role_panel_form::submit(w, cx);
        });
        answered = client
            .state()
            .borrow()
            .bot
            .results(&conversation)
            .last()
            .filter(|r| r.status != ResultStatus::Pending)
            .map(|r| (r.status, r.text.clone()));
        if answered.is_some() {
            break;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    assert_eq!(
        answered,
        Some((
            ResultStatus::Success,
            "Role panel published and registered.".to_owned()
        )),
        "{:?}",
        host.bot_refusals()
    );
    assert!(
        !vcx.update(|_, cx| role_panel_form::is_open(window, cx)),
        "the accepted panel closed the editor"
    );

    // Bea sees the Bot's panel and reacts: the reaction grants her Crew.
    let s = rt.block_on(world::wait_for(
        &b,
        Duration::from_secs(60),
        "the Bot's panel at bea",
        |s| {
            s.timelines.get(&general).is_some_and(|t| {
                t.items
                    .iter()
                    .any(|i| i.bot && i.body.contains("🎮 Crew") && i.seq.is_some())
            })
        },
    ));
    assert_eq!(
        s.timelines[&general]
            .items
            .iter()
            .filter(|i| i.bot && i.body.contains("🎮 Crew"))
            .count(),
        1,
        "a resend of an unconfirmed publication posts no second panel"
    );
    let panel = s.timelines[&general]
        .items
        .iter()
        .find(|i| i.bot && i.body.contains("🎮 Crew"))
        .unwrap();
    let panel_msg = panel.msg_id.expect("the panel has a message id");
    let bea_id = s.account.user_id.unwrap();
    rt.block_on(b.reactions().react(general, panel_msg, "🎮", true))
        .unwrap()
        .expect("bea reacts");
    rt.block_on(world::wait_for(
        &b,
        Duration::from_secs(60),
        "the panel's grant",
        |s| {
            s.members
                .get(&c)
                .and_then(|m| m.iter().find(|m| m.user_id == bea_id))
                .is_some_and(|m| m.role_ids.contains(&crew))
        },
    ));
    b.stop();
    drop(ctx);
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}

/// A GPUI-only reference (`native/rolepanel-editor`, Decision D2): `/rolepanel create` opens the editor, drawn with the
/// Signal `Dialog`, `Select`, the 22px check box (Decision D20) and `TextField` where the web's `BotCommandDialogs` uses
/// browser widgets. The fields are the web's, in its order and with its labels: Destination channel, the approved roles
/// (a check box and "Reaction for {role}" each), then Close, Cancel and Publish panel.
#[test]
#[cfg_attr(
    not(any(target_os = "linux", target_os = "macos")),
    ignore = "references are Linux and macOS only"
)]
fn parity_rolepanel_editor() {
    dilla_parity::screen::assert_native_screen(
        &fixture("commands/rolepanel-editor.json"),
        "rolepanel-editor",
        |app| {
            // The fixture's steps are not replayed for a native frame: the same ones, by hand.
            app.type_text("composer", "/rolepanel create");
            app.wait_for("command-picker", Duration::from_secs(10));
            app.press("tab");
            app.wait_for("command-chips", Duration::from_secs(5));
            app.press("enter");
            app.wait_for("rolepanel-publish", Duration::from_secs(10));
        },
    )
    .unwrap_or_else(|e| panic!("commands/rolepanel-editor differs from its golden shot: {e}"));
}

/// `/rolepanel create` typed in the composer and sent from its chips opens the editor on the server whose Bot offers role
/// panels, its row pending under the log; Publish with no role chosen names the refusal in the editor and keeps it open;
/// Cancel closes it and the row says the creation was cancelled.
#[test]
fn the_composer_opens_the_role_panel_editor() {
    let mut app = Driver::launch_fixture(&fixture("commands/rolepanel-editor.json"), None, &[]);
    app.type_text("composer", "/rolepanel create");
    app.wait_for("command-picker", Duration::from_secs(10));
    app.press("tab");
    app.wait_for("command-chips", Duration::from_secs(5));
    app.press("enter");
    app.wait_for("rolepanel-publish", Duration::from_secs(10));
    assert!(app.element("rolepanel-destination").is_some());
    app.click("rolepanel-publish");
    app.wait_for("rolepanel-error", Duration::from_secs(5));
    assert!(
        app.element("rolepanel-publish").is_some(),
        "Publish kept it open"
    );
    app.click("rolepanel-cancel");
    app.wait_idle();
    assert!(
        app.element("rolepanel-publish").is_none(),
        "Cancel closed it"
    );
    app.wait_for("bot-result-failed", Duration::from_secs(5));
}

// ---- Task 30: an uncertain command keeps its request id, and a resend reconciles (dillad) ----

/// The last private row of `conversation` once it settled (not Pending), after `sent` rows in all.
fn settled_row(
    client: &dilla_client::Client,
    conversation: dilla_client::state::bot::Conversation,
    sent: usize,
    cx: &mut gpui::VisualTestContext,
    drive: &mut dyn FnMut(&mut gpui::VisualTestContext),
) -> (dilla_client::state::bot::ResultStatus, String) {
    use dilla_client::state::bot::ResultStatus;
    for _ in 0..1200 {
        cx.run_until_parked();
        drive(cx);
        let state = client.state().borrow().clone();
        let rows = state.bot.results(&conversation);
        if rows.len() == sent
            && let Some(last) = rows.last()
            && last.status != ResultStatus::Pending
        {
            return (last.status, last.text.clone());
        }
        std::thread::sleep(Duration::from_millis(50));
    }
    panic!("the row never settled");
}

/// Task 30 (builtin.ts:217-243, :256, :267; CommandHost.tsx:243-307), against dillad: the draft of a run whose answer was
/// uncertain is the same request when it is sent again.
/// - `/timeout`: a request that already went out asks nothing again and is never sent again; the member's timeout is
///   read, and the expiry this attempt asked for settles it. Another expiry is uncertain, and its draft is kept to reopen.
/// - `/warn`: the resend carries the remembered request id, so the instance answers the warning it already recorded.
/// - Fix round 2 (I3): a reopen is only for the session that ran the command: another account's composer never takes it,
///   and an answer that lands after the client was released (sign-out) puts nothing back in the window.
#[gpui::test]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
fn an_uncertain_timeout_or_warn_resend_reconciles_under_its_request(cx: &mut gpui::TestAppContext) {
    use std::rc::Rc;

    use dilla_client::features::bot::memory::TimeoutAttempt;
    use dilla_client::features::bot::{self, Outcome, Plan};
    use dilla_client::features::moderation;
    use dilla_client::state::bot::{Conversation, ResultProvider, ResultStatus};
    use dilla_client::state::{CommunityId, UserId};
    use dilla_desktop::screens::commands::{self, SendDecision, builtins, confirm};
    use dilla_desktop::screens::create;
    use dilla_desktop::slots::SlotCx;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_host("t30-replay", "127.0.0.1:0", None);
    let invite = host.invites.last().unwrap().clone();
    let ada = rt.block_on(world::sign_up(&host, &invite, "ada", "Ada", "t30-replay"));
    let bea = rt.block_on(world::sign_up(&host, &invite, "bea", "Bea", "t30-replay"));
    let c = CommunityId(world::community(&host, &ada, "Midgard", &[&bea]));
    world::clear_session(&ada);
    world::clear_session(&bea);
    let within = Duration::from_secs(30);
    let bea_id = UserId(bea.user_id);
    let client = Rc::new(rt.block_on(world::start(&ada)));
    let general = rt
        .block_on(async {
            world::wait_for(&client, within, "both members", |s| {
                s.members.get(&c).is_some_and(|m| m.len() == 2)
            })
            .await;
            client.roles().load_access(c).await.unwrap();
            world::wait_for(&client, within, "access, #general", |s| {
                s.roles.access_of(c).loaded && s.channel_named(c, "general").is_some()
            })
            .await
        })
        .channel_named(c, "general")
        .unwrap()
        .id;
    cx.update(dilla_desktop::install);
    cx.executor().allow_parking();
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, cx| {
        // The shell attaches its client to its window as it starts.
        create::attach(window, client.clone(), Rc::new(|_, _, _| {}), cx);
        window.window_handle().window_id()
    });
    let at = Conversation {
        channel: general,
        thread: None,
    };
    let ctx = SlotCx {
        state: client.state().borrow().clone(),
        client: Some(client.clone()),
        community: Some(c),
        channel: Some(general),
        ..SlotCx::default()
    };
    let key = |draft: &str| {
        bot::invocation_key(&ctx.state, Some(c), draft).unwrap_or_else(|| {
            panic!(
                "{draft}: {:?}",
                bot::plan_in(&ctx.state, Some(c), Some(general), draft)
            )
        })
    };
    let none = &mut |_: &mut gpui::VisualTestContext| {};
    let mut sent = 0;

    // /timeout: the first send went out and its answer was lost (uncertain): the attempt and the draft are remembered.
    let timeout = format!("/timeout user:{} duration:10m", bea_id.hex());
    let request = [0x30; 16];
    let expires = moderation::now_s() + 600;
    rt.block_on(
        client
            .moderation()
            .timeout_for(c, bea_id, request, Some(expires)),
    )
    .expect("the first send");
    let held = |expires| TimeoutAttempt {
        community: c,
        user: bea_id,
        expires,
        seconds: 600,
        settled: false,
    };
    commands::with_memory(&client, |m| {
        m.timeouts.begin(request, held(expires)).unwrap();
        m.uncertain(at, &timeout, request, &key(&timeout));
    });
    assert_eq!(
        vcx.update(|_, cx| commands::on_send(&ctx, &timeout, &|t| t.to_owned(), window, cx)),
        SendDecision::Handled
    );
    sent += 1;
    assert!(
        !vcx.update(|_, cx| confirm::is_open(window, cx)),
        "a sent request asks nothing again"
    );
    let (status, text) = settled_row(&client, at, sent, vcx, none);
    assert_eq!(status, ResultStatus::Success, "{text}");
    assert!(text.ends_with(" was timed out."), "{text}");
    assert_eq!(
        rt.block_on(client.moderation().timeout_of(c, bea_id))
            .unwrap(),
        Some(expires),
        "read, never sent again"
    );
    // Another expiry than this attempt's: uncertain, nothing sent, and the draft kept to reopen.
    commands::with_memory(&client, |m| {
        m.timeouts.begin(request, held(expires + 7)).unwrap();
        m.uncertain(at, &timeout, request, &key(&timeout));
    });
    assert_eq!(
        vcx.update(|_, cx| commands::on_send(&ctx, &timeout, &|t| t.to_owned(), window, cx)),
        SendDecision::Handled
    );
    sent += 1;
    let (status, text) = settled_row(&client, at, sent, vcx, none);
    assert_eq!(status, ResultStatus::Failed);
    assert!(
        text.starts_with("The current timeout differs from this attempt."),
        "{text}"
    );
    assert_eq!(
        commands::with_memory(&client, |m| m.take_reopen(at)),
        Some(timeout.clone()),
        "the uncertain draft is kept to reopen"
    );

    // /warn: the first send was recorded, its answer lost; the resend carries the remembered request id.
    let warn = format!("/warn user:{} reason:spam", bea_id.hex());
    let request = [0x31; 16];
    let first = rt
        .block_on(client.moderation().warn(c, bea_id, request, "spam"))
        .expect("the first send");
    commands::with_memory(&client, |m| m.uncertain(at, &warn, request, &key(&warn)));
    assert_eq!(
        vcx.update(|_, cx| commands::on_send(&ctx, &warn, &|t| t.to_owned(), window, cx)),
        SendDecision::Handled
    );
    assert!(vcx.update(|_, cx| confirm::is_open(window, cx)), "it asks");
    // The question is answered by its dialog, which this window does not draw: its run is what a confirmation runs.
    vcx.update(|_, cx| confirm::answer_builtin(window, None, cx));
    let Plan::Builtin { run, .. } = bot::plan_in(&ctx.state, Some(c), Some(general), &warn) else {
        panic!("/warn plans a built-in");
    };
    let attempt = commands::attempt_of(&ctx, at, &warn);
    assert_eq!(attempt.request, request, "the remembered request id");
    let row = client.bot().add_result(
        at,
        ResultStatus::Pending,
        "/warn…".into(),
        ResultProvider::Builtin,
    );
    vcx.update(|window, cx| {
        builtins::run(
            run,
            row,
            attempt.clone(),
            at,
            &ctx,
            window.window_handle().window_id(),
            cx,
        );
    });
    let (status, text) = settled_row(&client, at, sent + 1, vcx, none);
    assert_eq!(status, ResultStatus::Success, "{text}");
    assert!(text.ends_with(" was warned."), "{text}");
    let again = rt
        .block_on(client.moderation().warn(c, bea_id, request, "spam"))
        .expect("the same request");
    assert_eq!(
        again.action_id, first.action_id,
        "one warning: the instance answered the request it recorded"
    );

    // I3: the window's reopen is this session's only. Another account's composer never takes it, and drops it.
    let other = rt.block_on(world::start(&bea));
    vcx.update(|_, cx| {
        commands::settle(
            &client,
            window,
            (at, row),
            &attempt,
            Outcome::Uncertain("x".into()),
            cx,
        );
        assert!(
            commands::picker_state(window, cx).reopen.is_some(),
            "the session that ran it is handed its draft"
        );
        assert_eq!(
            commands::take_reopen(window, Some(other.serial()), at, cx),
            None
        );
        assert!(
            commands::picker_state(window, cx).reopen.is_none(),
            "dropped unread"
        );
    });
    // An answer that lands after the sign-out released the client puts nothing back.
    vcx.update(|_, cx| {
        dilla_desktop::release::release_client(&client, cx);
        commands::settle(
            &client,
            window,
            (at, row),
            &attempt,
            Outcome::Uncertain("x".into()),
            cx,
        );
        assert!(
            commands::picker_state(window, cx).reopen.is_none(),
            "no reopen after the release"
        );
        assert_eq!(
            commands::take_reopen(window, Some(client.serial()), at, cx),
            None,
            "no draft for the released session either"
        );
    });
    drop(ctx);
    other.stop();
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}

/// Task 30 (bot-handlers.ts:382-400, UtilityCommandProvider.tsx:364-377), against dillad and its Bot: a run whose request
/// already sent a schedule save or a poll reconciles it from the pending branch, with no editor and no form. The Bot
/// answers the request it recorded: the schedule and the poll exist once.
#[gpui::test]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
fn an_uncertain_schedule_or_poll_is_reconciled_from_the_pending_branch(
    cx: &mut gpui::TestAppContext,
) {
    use std::rc::Rc;

    use dilla_client::features::bot::announcements::{
        self, Schedule, ScheduleKind, child_id, encode_schedule,
    };
    use dilla_client::features::bot::memory::{AnnouncementAttempt, PollAttempt};
    use dilla_client::features::bot::{self, Plan};
    use dilla_client::features::bot_admin::BotArg;
    use dilla_client::features::roles::viewer_channel_bits;
    use dilla_client::features::server_admin::hex_id as hex;
    use dilla_client::features::utilities::UtilityError;
    use dilla_client::features::utilities::polls::{PollCreate, final_poll_marker};
    use dilla_client::state::CommunityId;
    use dilla_client::state::bot::{Conversation, ResultStatus};
    use dilla_client::state::bot_admin::sealed_operation;
    use dilla_desktop::screens::commands::{self, SendDecision, announcement_form, poll_form};
    use dilla_desktop::screens::create;
    use dilla_desktop::slots::SlotCx;

    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(2)
        .enable_all()
        .build()
        .unwrap();
    let host = world::start_bot_host("t30-reconcile", "127.0.0.1:0");
    let invite = host.invites.last().unwrap().clone();
    let ada = rt.block_on(world::sign_up(
        &host,
        &invite,
        "ada",
        "Ada",
        "t30-reconcile",
    ));
    let c = CommunityId(world::community(&host, &ada, "Harbour", &[]));
    world::clear_session(&ada);
    let client = Rc::new(rt.block_on(world::start(&ada)));
    rt.block_on(world::bot::enable_bot(
        &client,
        c.0,
        world::bot::cap::ANNOUNCEMENTS | world::bot::cap::POLLS,
    ));
    let general = rt
        .block_on(world::wait_for(
            &client,
            Duration::from_secs(30),
            "#general",
            |s| s.channel_named(c, "general").is_some(),
        ))
        .channel_named(c, "general")
        .unwrap()
        .id;
    rt.block_on(client.bot_admin().mark_bot_channel(c, general))
        .expect("an answer")
        .expect("#general is marked a Bot channel");
    let ready = |state: &dilla_client::state::AppState| {
        matches!(
            bot::plan_in(state, Some(c), Some(general), "/announcement list"),
            Plan::Bot { .. }
        ) && state.bot.catalogs.get(&c).is_some_and(|v| {
            [50, 70].iter().all(|op| {
                v.commands
                    .iter()
                    .any(|x| u64::from(x.operation) == sealed_operation(*op))
            })
        }) && state
            .bot
            .catalogs
            .get(&c)
            .is_some_and(|v| v.bot_user.is_some())
    };
    let deadline = std::time::Instant::now() + Duration::from_secs(90);
    while !ready(&client.state().borrow()) {
        assert!(
            std::time::Instant::now() < deadline,
            "the Bot's catalog never offered announcements and polls: {:?}",
            host.bot_refusals()
        );
        let _ = rt.block_on(client.bot().sync_catalog(c));
        std::thread::sleep(Duration::from_secs(1));
    }
    client.threads().set_channel_bits(
        general,
        viewer_channel_bits(&client.state().borrow(), general),
    );
    let _ = rt.block_on(client.open_channel(general));

    cx.update(dilla_desktop::install);
    cx.executor().allow_parking();
    let vcx = cx.add_empty_window();
    let window = vcx.update(|window, cx| {
        create::attach(window, client.clone(), Rc::new(|_, _, _| {}), cx);
        window.window_handle().window_id()
    });
    let at = Conversation {
        channel: general,
        thread: None,
    };
    let ctx = |client: &Rc<dilla_client::Client>| SlotCx {
        state: client.state().borrow().clone(),
        client: Some(client.clone()),
        community: Some(c),
        channel: Some(general),
        ..SlotCx::default()
    };
    let none = &mut |_: &mut gpui::VisualTestContext| {};
    let mut sent = 0;

    // A schedule save went out and the Bot stored it; its answer was lost, so the attempt is held as sent.
    let schedule = Schedule {
        channel: general,
        text: "Harbour watch".into(),
        kind: ScheduleKind::Daily,
        timezone: "UTC".into(),
        date: String::new(),
        hour: 9,
        minute: 0,
        // A daily schedule carries no weekday (recurrence.go:146).
        weekday: 0,
    };
    let parent = [0x40; 16];
    let (_, list_revision) = rt
        .block_on(client.bot().list_announcements(c, [0x41; 16]))
        .expect("the list");
    assert_eq!(
        rt.block_on(
            client
                .bot()
                .save_announcement(c, parent, None, &schedule, list_revision)
        ),
        Ok("Announcement scheduled.".to_owned()),
        "{:?}",
        host.bot_refusals()
    );
    let parent_hex = hex(&parent);
    let document = encode_schedule(child_id(&parent_hex, "announcement:id"), &schedule, true, 1);
    let draft = "/announcement create";
    let key = bot::invocation_key(&ctx(&client).state, Some(c), draft).expect("an invocation");
    commands::with_memory(&client, |m| {
        m.keep_announcement(
            (parent, c),
            AnnouncementAttempt {
                operation: announcements::SAVE,
                request: child_id(&parent_hex, "announcement:save"),
                args: vec![BotArg::Null, BotArg::Bytes(document)],
                revision: list_revision,
                sent: true,
            },
        );
        m.uncertain(at, draft, parent, &key);
    });
    let here = ctx(&client);
    assert_eq!(
        vcx.update(|_, cx| commands::on_send(&here, draft, &|t| t.to_owned(), window, cx)),
        SendDecision::Handled
    );
    sent += 1;
    assert!(
        !vcx.update(|_, cx| announcement_form::is_open(window, cx)),
        "no editor"
    );
    assert_eq!(
        settled_row(&client, at, sent, vcx, none),
        (ResultStatus::Success, "Announcement scheduled.".to_owned()),
        "{:?}",
        host.bot_refusals()
    );
    let (rows, _) = rt
        .block_on(client.bot().list_announcements(c, [0x42; 16]))
        .expect("the list");
    assert_eq!(rows.len(), 1, "the schedule exists once");

    // A poll create went out and the Bot made it; its answer was lost, so the attempt is held.
    let request = [0x50; 16];
    let value = PollCreate {
        channel: general.0,
        thread: None,
        question: "Raid on Friday?".into(),
        answers: vec!["the bridge".into(), "the harbour".into()],
        multi: false,
        duration: 86_400,
    };
    let bot_user = client
        .utilities()
        .bot_identity(c)
        .expect("the pinned Bot")
        .user;
    // The first create may itself be answered uncertainly (the Bot's first poll can outlast the wait): either way it
    // is the attempt a resend of the draft reconciles.
    let made = value.clone();
    let first = rt.block_on(
        client
            .utilities()
            .run(move |u| async move { u.create_poll(c, made, request).await }),
    );
    assert!(
        matches!(first, Ok(_) | Err(UtilityError::Uncertain { .. })),
        "{first:?}"
    );
    let draft = "/poll create";
    let key = bot::invocation_key(&ctx(&client).state, Some(c), draft).expect("an invocation");
    commands::with_memory(&client, |m| {
        m.keep_poll(
            request,
            PollAttempt {
                at,
                community: c,
                bot_user,
                value: value.clone(),
            },
        );
        m.uncertain(at, draft, request, &key);
    });
    // The draft sent again reconciles: no form, the same request. Until the Bot's post is in the conversation the
    // answer stays uncertain (`pollPublished`) and the draft is kept, so it is sent again, as a person would.
    let mut made = None;
    for _ in 0..30 {
        let here = ctx(&client);
        assert_eq!(
            vcx.update(|_, cx| commands::on_send(&here, draft, &|t| t.to_owned(), window, cx)),
            SendDecision::Handled
        );
        sent += 1;
        assert!(
            !vcx.update(|_, cx| poll_form::is_open(window, cx)),
            "no form"
        );
        let row = settled_row(&client, at, sent, vcx, none);
        if row.0 == ResultStatus::Success {
            made = Some(row);
            break;
        }
        assert_eq!(
            row,
            (
                ResultStatus::Failed,
                dilla_desktop::strings::t("poll.createdUncertain").to_owned()
            ),
            "{:?}",
            host.bot_refusals()
        );
        assert_eq!(
            commands::attempt_of(&ctx(&client), at, draft).request,
            request,
            "kept"
        );
        std::thread::sleep(Duration::from_secs(1));
    }
    assert_eq!(
        made.map(|r| r.1),
        Some(dilla_desktop::strings::t("poll.created").to_owned()),
        "{:?}",
        host.bot_refusals()
    );
    assert!(
        commands::with_memory(&client, |m| m.polls.is_empty()),
        "settled"
    );
    let posts = client
        .state()
        .borrow()
        .timelines
        .get(&general)
        .map_or(0, |t| {
            t.items
                .iter()
                .filter(|i| i.bot && final_poll_marker(&i.body) == Some(request))
                .count()
        });
    assert_eq!(posts, 1, "the poll was made once");
    drop(here);
    vcx.update(|_, cx| dilla_desktop::release::release_client(&client, cx));
    Rc::try_unwrap(client).ok().expect("the last handle").stop();
}
