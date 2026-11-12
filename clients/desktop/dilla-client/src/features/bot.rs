//! Area **bot** (Task 20): Bot commands over `dilla_core` (sealed), private results, polls, reminders.
//!
//! Ports: packages/client-core/src/bot/{catalog,commands,hints,operations,protocol}.ts, packages/client-core/src/commands/{classic,registry}.ts.
//!
//! Receives op 61 `bot_catalog`, op 62 `bot_reply` and op 63 `bot_state_changed` from the gateway (`crate::sync::route`), through [`AreaFrames`].
//!
//! Stub from Task 1 (area scaffold): Task 20 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};
use crate::gateway::frames::Frame;
use crate::sync::route::AreaFrames;

/// The bot area's handle for bot: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Bot {
    #[expect(dead_code, reason = "Task 20 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Bot`] handle.
    pub fn bot(&self) -> Bot {
        Bot {
            inner: self.inner().clone(),
        }
    }
}

impl AreaFrames for Bot {
    fn on_frame(_inner: &Arc<Inner>, _f: &Frame) {}
}
