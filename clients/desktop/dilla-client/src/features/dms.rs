//! Area **dms** (Task 13): open or create a DM, the DM list and DM unread.
//!
//! Ports: the DM list and DmPicker in packages/web/src/screens/Shell.tsx.
//!
//! Stub from Task 1 (area scaffold): Task 13 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The dms area's handle for dms: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Dms {
    #[expect(dead_code, reason = "Task 13 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Dms`] handle.
    pub fn dms(&self) -> Dms {
        Dms {
            inner: self.inner().clone(),
        }
    }
}
