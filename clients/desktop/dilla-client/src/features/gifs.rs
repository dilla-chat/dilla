//! Area **attachments** (Task 12): GIF search through the instance's GIF routes, and DM GIF consent.
//!
//! Ports: packages/web/src/screens/conversation/{use-gif-community,dm-gif-consent}.ts.
//!
//! Stub from Task 1 (area scaffold): Task 12 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The attachments area's handle for gifs: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Gifs {
    #[expect(dead_code, reason = "Task 12 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Gifs`] handle.
    pub fn gifs(&self) -> Gifs {
        Gifs {
            inner: self.inner().clone(),
        }
    }
}
