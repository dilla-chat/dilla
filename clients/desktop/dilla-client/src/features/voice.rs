//! Area **voice** (Task 17): the voice-state roster, join and leave, mute and deafen, the speaking signal.
//!
//! Ports: packages/web/src/call/session.ts, packages/web/src/screens/voice/useCall.tsx.
//!
//! Receives op 50 `voice_state` from the gateway (`crate::sync::route`), through [`AreaFrames`].
//!
//! Stub from Task 1 (area scaffold): Task 17 fills it, keeping the names below (clients/desktop/OWNERSHIP.md).

use std::sync::Arc;

use crate::client::{Client, Inner};
use crate::gateway::frames::Frame;
use crate::sync::route::AreaFrames;

/// The voice area's handle for voice: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct Voice {
    #[expect(dead_code, reason = "Task 17 fills the area")]
    inner: Arc<Inner>,
}

impl Client {
    /// The [`Voice`] handle.
    pub fn voice(&self) -> Voice {
        Voice {
            inner: self.inner().clone(),
        }
    }
}

impl AreaFrames for Voice {
    fn on_frame(_inner: &Arc<Inner>, _f: &Frame) {}
}

impl Voice {
    /// Mutes or unmutes this device's microphone in the current call (and before one: a join starts muted). The user
    /// panel (Task 8) calls it; Task 17 implements it. Fixed signature: change it only with the controller.
    pub fn set_muted(&self, _muted: bool) {}

    /// Deafens or undeafens this device. The user panel (Task 8) calls it; Task 17 implements it. Fixed signature.
    pub fn set_deafened(&self, _deafened: bool) {}
}
