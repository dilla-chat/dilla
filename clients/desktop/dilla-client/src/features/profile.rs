//! Area **user settings** (Task 16): the account's profile.
//!
//! Ports: packages/web/src/screens/settings/Profile.tsx.
//!
//! Stub from Task 1 (area scaffold): Task 16 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The user settings area's handle for profile: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Profile {
    #[expect(dead_code, reason = "Task 16 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Profile`] handle.
    pub fn profile(&self) -> Profile {
        Profile {
            inner: self.inner().clone(),
        }
    }
}
