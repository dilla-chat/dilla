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
