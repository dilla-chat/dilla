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
