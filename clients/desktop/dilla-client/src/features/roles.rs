//! Area **members** (Task 14): roles, the permission grid, assigning roles and self roles.
//!
//! Ports: packages/client-core/src/admin/{authority,perm,rank,roles}.ts.
//!
//! Stub from Task 1 (area scaffold): Task 14 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The members area's handle for roles: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Roles {
    #[expect(dead_code, reason = "Task 14 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Roles`] handle.
    pub fn roles(&self) -> Roles {
        Roles {
            inner: self.inner().clone(),
        }
    }
}
