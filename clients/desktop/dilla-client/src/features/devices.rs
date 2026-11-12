//! Area **user settings** (Task 16): the device list: rename, sign out and remove, the key dialog.
//!
//! Ports: packages/web/src/screens/settings/{device-name,device-platform}.ts.
//!
//! Stub from Task 1 (area scaffold): Task 16 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The user settings area's handle for devices: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Devices {
    #[expect(dead_code, reason = "Task 16 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Devices`] handle.
    pub fn devices(&self) -> Devices {
        Devices {
            inner: self.inner().clone(),
        }
    }
}
