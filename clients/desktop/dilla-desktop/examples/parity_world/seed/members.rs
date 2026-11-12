//! Seeds the statuses, bios and Bot accounts of a fixture into the real dillad. Owned by Task 14; no other file does.

use dilla_client::testing::host::Person;
use dilla_client::testing::spec::ScreenFixture;

/// Names the facet when the fixture asks for it and this world cannot seed it yet. When the area adds its seeding below,
/// it drops the refusal.
pub(super) fn unseedable(f: &ScreenFixture, out: &mut Vec<String>) {
    let w = &f.world;
    if w.people
        .iter()
        .any(|p| p.bot || !p.status.is_empty() || !p.bio.is_empty())
    {
        out.push("statuses, bios and Bot accounts".to_owned());
    }
}

/// Seeds the facet once the history is posted, with the live clients keyed by person id.
#[allow(clippy::unused_async)]
pub(super) async fn seed(_f: &ScreenFixture, _people: &[(String, Person)]) {}
