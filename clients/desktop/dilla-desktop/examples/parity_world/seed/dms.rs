//! Seeds the dms of a fixture into the real dillad. Owned by Task 13; no other file does.

use dilla_client::testing::host::Person;
use dilla_client::testing::spec::ScreenFixture;

/// Names the facet when the fixture asks for it and this world cannot seed it yet. When the area adds its seeding below,
/// it drops the refusal.
pub(super) fn unseedable(f: &ScreenFixture, out: &mut Vec<String>) {
    let w = &f.world;
    if !w.dms.is_empty() {
        out.push("dms".to_owned());
    }
}

/// Seeds the facet once the history is posted, with the live clients keyed by person id.
#[allow(clippy::unused_async)]
pub(super) async fn seed(_f: &ScreenFixture, _people: &[(String, Person)]) {}
