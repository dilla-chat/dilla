//! Area **messages** (Task 9): edit, delete, pin and unpin, typing (sent and received), slow mode and the announcement read-only rule, per-channel drafts' sends.
//!
//! Ports: packages/web/src/screens/conversation/{slow,manual-removal,webhook}.ts, packages/client-core/src/worker/controller.ts (typing).
//!
//! Receives op 49 `typing` from the gateway (`crate::sync::route`), through [`AreaFrames`].
//!
//! Stub from Task 1 (area scaffold): Task 9 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};
use crate::gateway::frames::Frame;
use crate::sync::route::AreaFrames;

/// The messages area's handle for messages: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Messages {
    #[expect(dead_code, reason = "Task 9 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Messages`] handle.
    pub fn messages(&self) -> Messages {
        Messages {
            inner: self.inner().clone(),
        }
    }
}

impl AreaFrames for Messages {
    fn on_frame(_inner: &Arc<Inner>, _f: &Frame) {}
}
