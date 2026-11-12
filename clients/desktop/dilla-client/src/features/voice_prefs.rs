//! Area **voice settings** (Task 19): devices, sensitivity, noise mode, push-to-talk key, per-user volume.
//!
//! Ports: packages/web/src/call/{devices,keys,ptt,speaking,volume,voice-prefs}.ts.
//!
//! Stub from Task 1 (area scaffold): Task 19 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};

/// The voice settings area's handle for voice prefs: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct VoicePrefs {
    #[expect(dead_code, reason = "Task 19 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`VoicePrefs`] handle.
    pub fn voice_prefs(&self) -> VoicePrefs {
        VoicePrefs {
            inner: self.inner().clone(),
        }
    }
}
