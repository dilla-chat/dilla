//! Area **bot** (Task 20): the built-in utility commands.
//!
//! Ports: packages/client-core/src/bot/{utilities,utility-time}.ts.
//!
//! Stub from Task 1 (area scaffold): Task 20 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The bot area's handle for utilities: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Utilities {
    #[expect(dead_code, reason = "Task 20 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Utilities`] handle.
    pub fn utilities(&self) -> Utilities {
        Utilities {
            inner: self.inner().clone(),
        }
    }
}
