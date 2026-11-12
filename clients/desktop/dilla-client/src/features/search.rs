//! Area **search** (Task 23): search over channels, people, messages, settings and actions.
//!
//! Ports: packages/client-core/src/state/search.ts.
//!
//! Stub from Task 1 (area scaffold): Task 23 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The search area's handle for search: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Search {
    #[expect(dead_code, reason = "Task 23 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Search`] handle.
    pub fn search(&self) -> Search {
        Search {
            inner: self.inner().clone(),
        }
    }
}
