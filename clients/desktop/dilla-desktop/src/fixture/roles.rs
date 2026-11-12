//! The roles facet of a fixture: `AppState::roles`, from `ServerSpec::roles`. Owned by Task 14; no other file maps it.

use dilla_client::state::AppState;
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`. An empty stub until its area's slice has fields for it.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}
