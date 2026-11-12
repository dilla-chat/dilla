//! The real-dillad helpers the desktop tests share. They live in `dilla-client` (feature `testing-host`, Task 5), where
//! `tests/sync.rs` and `examples/peer.rs` use the same copy.
pub use dilla_client::testing::host as world;
