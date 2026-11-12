//! Area **server settings** (Task 15): overview, AFK channel and timeout, channels and their permissions, GIF settings, invites, welcome and join roles, the danger zone.
//!
//! Ports: packages/web/src/screens/admin/{welcome-settings,shared}.ts.
//!
//! Stub from Task 1 (area scaffold): Task 15 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The server settings area's handle for server admin: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct ServerAdmin {
    #[expect(dead_code, reason = "Task 15 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`ServerAdmin`] handle.
    pub fn server_admin(&self) -> ServerAdmin {
        ServerAdmin {
            inner: self.inner().clone(),
        }
    }
}
