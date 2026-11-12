//! Seeds the edits and pins of a fixture into the real dillad. Owned by Task 9; no other file does.

use dilla_client::testing::host::Person;
use dilla_client::testing::spec::ScreenFixture;

/// Names the facet when the fixture asks for it and this world cannot seed it yet. When the area adds its seeding below,
/// it drops the refusal.
pub(super) fn unseedable(f: &ScreenFixture, out: &mut Vec<String>) {
    let w = &f.world;
    if w.messages.iter().any(|m| m.edited || m.pinned) {
        out.push("edits and pins".to_owned());
    }
}

/// Seeds the facet once the history is posted, with the live clients keyed by person id.
#[allow(clippy::unused_async)]
pub(super) async fn seed(_f: &ScreenFixture, _people: &[(String, Person)]) {}
