//! Area **members** (Task 14): kick, ban, unban, timeout and warn with evidence.
//!
//! Ports: packages/client-core/src/admin/controller.ts, packages/web/src/screens/admin/Moderation.tsx.
//!
//! Stub from Task 1 (area scaffold): Task 14 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The members area's handle for moderation: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Moderation {
    #[expect(dead_code, reason = "Task 14 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Moderation`] handle.
    pub fn moderation(&self) -> Moderation {
        Moderation {
            inner: self.inner().clone(),
        }
    }
}
