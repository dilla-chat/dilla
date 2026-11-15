//! Seeds the diagnostics facet of a fixture into the real dillad. Owned by Task 24; no other file does.
//!
//! Nothing: the web client's Diagnostics shows the facts of the world as it is (the connection, the instance, the
//! viewer's two devices), so a fixture needs no seeding of its own and this world can seed every one.

use dilla_client::testing::host::Person;
use dilla_client::testing::spec::ScreenFixture;

/// Nothing a fixture can ask for here is unseedable.
pub(super) fn unseedable(_f: &ScreenFixture, _out: &mut Vec<String>) {}

/// Nothing to seed.
#[allow(clippy::unused_async)]
pub(super) async fn seed(_f: &ScreenFixture, _people: &[(String, Person)]) {}
