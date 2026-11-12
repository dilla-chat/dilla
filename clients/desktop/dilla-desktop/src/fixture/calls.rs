//! The calls facet of a fixture: `AppState::voice` and `AppState::ring`, from `WorldSpec::calls`. Owned by Tasks 17 and 18; no other file maps it.

use dilla_client::state::AppState;
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`. An empty stub until its area's slice has fields for it.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}
