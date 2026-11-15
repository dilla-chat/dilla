//! What tests and the parity harness share (Task 5), behind the `testing` features and never in a shipping build.
//!
//! - [`spec`] (feature `testing`): the screen fixture schema, one JSON file both shooters read: the web shooter seeds a
//!   real dillad from it and shoots Chromium; the GPUI automation build draws the same world without a network.
//! - `host` (feature `testing-host`): a real dillad (dilla-testhost), accounts made the way the app makes them,
//!   communities, started clients and state waits: the helpers `tests/sync.rs`, `examples/peer.rs`, the desktop's
//!   `tests/support/world.rs` and `examples/parity_world.rs` used to carry each their own copy of.

pub mod spec;

#[cfg(feature = "testing-host")]
pub mod host;
