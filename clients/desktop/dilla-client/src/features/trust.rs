//! Area **dms** (Task 13): the E_UMK_CHANGED, E_CREDENTIAL and E_DEVICE_* blocks, compare by safety number and QR, verify, remove the changed device.
//!
//! Ports: packages/client-core/src/state/trust.ts.
//!
//! Stub from Task 1 (area scaffold): Task 13 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The dms area's handle for trust: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Trust {
    #[expect(dead_code, reason = "Task 13 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Trust`] handle.
    pub fn trust(&self) -> Trust {
        Trust {
            inner: self.inner().clone(),
        }
    }
}
