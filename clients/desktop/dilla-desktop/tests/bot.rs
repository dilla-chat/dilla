//! Task 20's parity, behaviour and keyboard tests for the bot area (standard procedure S4-S6), driven through the
//! automation build.
//!
//! The poll card draws from the polls facet of a fixture (`src/fixture/polls.rs`). Its web references cannot be shot
//! yet: the parity world has no Bot to post and keep a poll (`examples/parity_world/seed/polls.rs` refuses the facet),
//! so the parity tests are ignored with that reason until the world can seed one.

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
#[ignore = "no reference: the parity world cannot seed a poll yet (it needs a running Bot; seed/polls.rs refuses the facet)"]
fn parity_polls_card() {
    dilla_parity::screen::assert_screen(&fixture("polls/card.json")).unwrap();
}

#[test]
#[ignore = "no reference: the parity world cannot seed a poll yet (it needs a running Bot; seed/polls.rs refuses the facet)"]
fn parity_polls_card_b() {
    dilla_parity::screen::assert_screen(&fixture("polls/card-b.json")).unwrap();
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
/// sealed answer is a private row (the chips leave), and the reminder is listed under Upcoming (`split_reminders`, the bell's first tab;
/// read on the account's first device: the desktop has no reminders popover yet).
#[test]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
fn a_reminder_set_from_the_chips_appears_under_upcoming() {
    use dilla_client::features::utilities::split_reminders;
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
    let (upcoming, due) = split_reminders(&listed, now);
    let mine = upcoming
        .iter()
        .find(|r| r.text == text)
        .expect("under Upcoming");
    assert!(due.iter().all(|r| r.text != text));
    // "in 1 hour", as the chip said.
    assert!(
        (3500..=3700).contains(&(mine.due.saturating_sub(now))),
        "{mine:?}"
    );
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
    app.type_focused(who);
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
