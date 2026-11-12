//! Area **diagnostics** (Task 24): the routes table, connection and wire info, the app build the instance announces.
//!
//! Ports: packages/web/src/screens/settings/Diagnostics.tsx, packages/web/src/{update-view,update-checkpoint}.ts.
//!
//! Receives op 56 `app_build` from the gateway (`crate::sync::route`), through [`AreaFrames`].
//!
//! Stub from Task 1 (area scaffold): Task 24 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};
use crate::gateway::frames::Frame;
use crate::sync::route::AreaFrames;

/// The diagnostics area's handle for diagnostics: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Diagnostics {
    #[expect(dead_code, reason = "Task 24 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Diagnostics`] handle.
    pub fn diagnostics(&self) -> Diagnostics {
        Diagnostics {
            inner: self.inner().clone(),
        }
    }
}

impl AreaFrames for Diagnostics {
    fn on_frame(_inner: &Arc<Inner>, _f: &Frame) {}
}
