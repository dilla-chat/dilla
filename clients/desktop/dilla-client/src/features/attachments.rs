//! Area **attachments** (Task 12): encryption through `dilla_core::attachment`, upload with the 25 MB cap, download checked against `size + 16` before decrypting, thumbnails, save after a user action.
//!
//! Ports: packages/client-core/src/attachments/{crypto,download,name,thumb,tray}.ts.
//!
//! Stub from Task 1 (area scaffold): Task 12 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The attachments area's handle for attachments: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Attachments {
    #[expect(dead_code, reason = "Task 12 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Attachments`] handle.
    pub fn attachments(&self) -> Attachments {
        Attachments {
            inner: self.inner().clone(),
        }
    }
}
