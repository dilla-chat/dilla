//! Area **members** (Task 14): the member list's extras: presence and the profile card.
//!
//! Ports: packages/web/src/screens/admin/{members-model,member-card}.ts.
//!
//! Receives op 48 `presence` from the gateway (`crate::sync::route`), through [`AreaFrames`].
//!
//! Stub from Task 1 (area scaffold): Task 14 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};
use crate::gateway::frames::Frame;
use crate::sync::route::AreaFrames;

/// The members area's handle for members: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Members {
    #[expect(dead_code, reason = "Task 14 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Members`] handle.
    pub fn members(&self) -> Members {
        Members {
            inner: self.inner().clone(),
        }
    }
}

impl AreaFrames for Members {
    fn on_frame(_inner: &Arc<Inner>, _f: &Frame) {}
}
