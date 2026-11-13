//! Area **instances** (Task 27): the "Dilla - your instances" manager window.
//!
//! The manager runs before any `Client` exists, so the area has no state slice: [`model::Manager`] works on the
//! instance list (`dilla_client::instances`) and the keychain, [`view::ManagerView`] draws it, and
//! `crate::windows` opens the manager and one window per instance.

pub mod copy;
pub mod keys;
pub mod model;
pub mod view;

pub use view::{ManagerEvent, ManagerView};
