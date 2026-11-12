//! Area **threads** (Task 11): reply, open a thread, page it, thread drafts and thread mention badges.
//!
//! Ports: packages/web/src/thread-request.ts, packages/web/src/screens/conversation/ThreadView.tsx.
//!
//! Stub from Task 1 (area scaffold): Task 11 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The threads area's handle for threads: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Threads {
    #[expect(dead_code, reason = "Task 11 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Threads`] handle.
    pub fn threads(&self) -> Threads {
        Threads {
            inner: self.inner().clone(),
        }
    }
}
