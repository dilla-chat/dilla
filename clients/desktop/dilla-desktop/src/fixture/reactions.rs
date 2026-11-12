//! The reactions facet of a fixture: `AppState::reactions` (who reacted, picker state). The counts on timeline items are already set. Owned by Task 10; no other file maps it.

use dilla_client::state::AppState;
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`. An empty stub until its area's slice has fields for it.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}
