//! The dms facet of a fixture: `AppState::dms` and `AppState::trust`, from `WorldSpec::dms` and the messages in `dm/<id>`. Owned by Task 13; no other file maps it.

use dilla_client::state::AppState;
use dilla_client::testing::spec::WorldSpec;

/// Maps the facet into `state`. An empty stub until its area's slice has fields for it.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, _state: &mut AppState) {}
