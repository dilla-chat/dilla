//! What the parity world can seed beyond plain text history, one file per Wave 1 area (ruling R11): this file only lists
//! them, one per line in sorted order, and each area edits its own file.

use dilla_client::testing::host::Person;
use dilla_client::testing::spec::ScreenFixture;

mod attachments;
mod calls;
mod dms;
mod members;
mod messages;
mod polls;
mod reactions;
mod roles;
mod threads;

/// What this world cannot seed yet, by facet; empty when it can seed the fixture.
pub fn unseedable(f: &ScreenFixture) -> Vec<String> {
    let mut out = Vec::new();
    attachments::unseedable(f, &mut out);
    calls::unseedable(f, &mut out);
    dms::unseedable(f, &mut out);
    members::unseedable(f, &mut out);
    messages::unseedable(f, &mut out);
    polls::unseedable(f, &mut out);
    reactions::unseedable(f, &mut out);
    roles::unseedable(f, &mut out);
    threads::unseedable(f, &mut out);
    out
}

/// Seeds every facet once the history is posted.
pub async fn seed(f: &ScreenFixture, people: &[(String, Person)]) {
    attachments::seed(f, people).await;
    calls::seed(f, people).await;
    dms::seed(f, people).await;
    members::seed(f, people).await;
    messages::seed(f, people).await;
    polls::seed(f, people).await;
    reactions::seed(f, people).await;
    roles::seed(f, people).await;
    threads::seed(f, people).await;
}
