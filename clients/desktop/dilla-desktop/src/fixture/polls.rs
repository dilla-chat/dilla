//! The polls facet of a fixture: `AppState::bot` (polls), from `MessageSpec::poll`. Owned by Task 20; no other file maps it.

use dilla_client::state::AppState;
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`. An empty stub until its area's slice has fields for it.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}
