//! Area **notify** (Task 22): mention, DM and ring decisions, mutes per channel and server, the device-wide default, the channel-on-screen suppression, the total badge count.
//!
//! Ports: packages/web/src/{notify,notify-permission}.ts, packages/client-core/src/state/{badges,mentions}.ts.
//!
//! Stub from Task 1 (area scaffold): Task 22 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The notify area's handle for notify: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Notify {
    #[expect(dead_code, reason = "Task 22 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Notify`] handle.
    pub fn notify(&self) -> Notify {
        Notify {
            inner: self.inner().clone(),
        }
    }
}

/// What an OS notification says. Task 22 owns this type and may add fields; Task 18 (ring) delivers through it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Notification {
    pub kind: NotificationKind,
    pub title: String,
    pub body: String,
}

/// Why a notification is shown.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NotificationKind {
    Mention,
    Dm,
    Ring,
}

/// Where notifications go: an in-app recording implementation in this plan (Task 22), the OS in the next. Task 18's
/// ring notifications go through it. Fixed signature: change it only with the controller.
pub trait Notifier: Send + Sync {
    fn deliver(&self, notification: &Notification);
}
