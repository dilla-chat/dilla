//! Area **factors** (Task 26): TOTP with QR, backup codes, passkeys, password change and step-up.
//!
//! Ports: packages/client-core/src/account/{factors,session}.ts.
//!
//! Stub from Task 1 (area scaffold): Task 26 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The factors area's handle for factors: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Factors {
    #[expect(dead_code, reason = "Task 26 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Factors`] handle.
    pub fn factors(&self) -> Factors {
        Factors {
            inner: self.inner().clone(),
        }
    }
}
