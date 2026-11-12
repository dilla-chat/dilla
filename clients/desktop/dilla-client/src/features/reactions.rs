//! Area **reactions** (Task 10): react and unreact through `ClientCore`, and who reacted.
//!
//! Ports: the reaction parts of packages/web/src/screens/conversation/MessageItem.tsx.
//!
//! Stub from Task 1 (area scaffold): Task 10 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The reactions area's handle for reactions: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Reactions {
    #[expect(dead_code, reason = "Task 10 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Reactions`] handle.
    pub fn reactions(&self) -> Reactions {
        Reactions {
            inner: self.inner().clone(),
        }
    }
}
