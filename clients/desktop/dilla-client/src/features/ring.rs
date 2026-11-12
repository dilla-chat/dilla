//! Area **ring** (Task 18): ring, answer, decline, busy and timeout; DM call start; the blocked-call flows; the AFK move and its notice.
//!
//! Ports: packages/client-core/src/calls/{ring,sweep,voice}.ts, packages/client-core/src/admin/afk.ts.
//!
//! Receives op 53 `call_ring` and op 55 `afk_away` from the gateway (`crate::sync::route`), through [`AreaFrames`].
//!
//! Stub from Task 1 (area scaffold): Task 18 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};
use crate::gateway::frames::Frame;
use crate::sync::route::AreaFrames;

/// The ring area's handle for ring: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Ring {
    #[expect(dead_code, reason = "Task 18 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Ring`] handle.
    pub fn ring(&self) -> Ring {
        Ring {
            inner: self.inner().clone(),
        }
    }
}

impl AreaFrames for Ring {
    fn on_frame(_inner: &Arc<Inner>, _f: &Frame) {}
}
