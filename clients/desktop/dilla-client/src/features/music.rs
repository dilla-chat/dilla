//! Area **bot admin** (Task 21): the music provider and the now-playing queue.
//!
//! Ports: packages/client-core/src/voice/music.ts.
//!
//! Stub from Task 1 (area scaffold): Task 21 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The bot admin area's handle for music: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Music {
    #[expect(dead_code, reason = "Task 21 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Music`] handle.
    pub fn music(&self) -> Music {
        Music {
            inner: self.inner().clone(),
        }
    }
}
