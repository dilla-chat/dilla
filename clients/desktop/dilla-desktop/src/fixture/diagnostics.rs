//! The diagnostics facet of a fixture: `AppState::diagnostics` and the connection facts the Diagnostics section shows.
//! Owned by Task 24; no other file maps it.
//!
//! The facts are those of the world the web shooter reads from: one dilla-testhost instance (generation 1, wire 6, the
//! instance's name as `GET /v1/instance` answers it) and a viewer who signed up on one device and signed in on a fresh
//! one, so two live devices. No error was recorded and no socket request is slow, which is the "No errors recorded" state
//! of the section.

use dilla_client::state::AppState;
use dilla_client::testing::spec::WorldSpec;

/// The generation a fresh dilla-testhost instance starts at (`internal/dillad/dilladtest/host.go`, `Generation: 1`).
const GENERATION: u64 = 1;
/// The wire version the testhost offers when no `-wire` flag is given.
const WIRE: u64 = 6;
/// The viewer's live devices in a shot: the sign-up device and the one the shooter signs in.
const DEVICES: u32 = 2;
/// The instance's name in the web client's instance list for a testhost world.
const INSTANCE_NAME: &str = "dilla.test";

/// Maps the facet into `state`.
pub(super) fn apply(_world: &WorldSpec, _viewer: &str, state: &mut AppState) {
    state.connection.generation = Some(GENERATION);
    state.connection.wire_version = Some(WIRE);
    state.connection.readies = 1;
    state.diagnostics.instance_name = Some(INSTANCE_NAME.to_owned());
    state.diagnostics.devices = Some(DEVICES);
}
