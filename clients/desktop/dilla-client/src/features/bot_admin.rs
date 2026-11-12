//! Area **bot admin** (Task 21): the Bot server settings: health, Bot channels, webhooks, the moderation log page.
//!
//! Ports: packages/client-core/src/bot/{admin,moderation-log,provider-status}.ts.
//!
//! Stub from Task 1 (area scaffold): Task 21 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The bot admin area's handle for bot admin: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct BotAdmin {
    #[expect(dead_code, reason = "Task 21 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`BotAdmin`] handle.
    pub fn bot_admin(&self) -> BotAdmin {
        BotAdmin {
            inner: self.inner().clone(),
        }
    }
}
