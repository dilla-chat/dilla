//! Area **voice settings** (Task 19): devices, sensitivity, noise mode, push-to-talk key, per-user volume.
//!
//! Ports: packages/web/src/call/{devices,keys,ptt,speaking,volume,voice-prefs}.ts and
//! packages/media/src/audio/sensitivity.ts. The pure rules live in the submodules ([`gate`], [`ptt`], [`speaking`],
//! [`volume`], [`values`]); this file is the handle the UI and the call use: it reads and writes the device's `voice.*`
//! preferences and publishes them, the devices the host lists, the microphone test and the audio notices into
//! [`AppState::voice_prefs`](crate::state::AppState).
//!
//! The device audio itself (listing, the microphone test, the sounds) is dilla-media's and runs in the desktop crate;
//! it reports here through [`VoicePrefs::set_devices`], [`VoicePrefs::set_test`] and [`VoicePrefs::note`].

use std::sync::Arc;

use crate::client::{Client, Inner};
use crate::state::voice_prefs::{AudioDevices, AudioNotice, MicTestState};

pub mod gate;
pub mod ptt;
pub mod speaking;
pub mod values;
pub mod volume;

pub use values::{InputMode, NoisePreference, Setting, VoiceError, VoiceValues};

/// Whether a call holds this device's microphone, so Let's check cannot start. Task 17's `AppState::voice` has no call
/// phase yet, so this answers false until it adds one (the one place to change then).
pub fn in_call(_state: &crate::state::AppState) -> bool {
    false
}

/// The voice settings area's handle for voice prefs: cheap to clone, no I/O on construction.
#[derive(Clone)]
pub struct VoicePrefs {
    inner: Arc<Inner>,
}

impl VoicePrefs {
    /// The stored values now (read from the preferences file's cache).
    pub fn values(&self) -> VoiceValues {
        VoiceValues::read(self.inner.prefs())
    }

    /// Reads the stored values and publishes them: the Voice section calls it when it opens.
    pub fn load(&self) -> VoiceValues {
        let values = self.values();
        self.publish_values(values.clone());
        values
    }

    /// Writes one setting (checked) and publishes the values after it.
    pub fn set(&self, setting: Setting) -> Result<VoiceValues, VoiceError> {
        let values = VoiceValues::apply(self.inner.prefs(), &setting)?;
        self.publish_values(values.clone());
        Ok(values)
    }

    fn publish_values(&self, values: VoiceValues) {
        self.inner.publish(move |s| s.voice_prefs.values = values);
    }

    /// The devices the host lists now.
    pub fn set_devices(&self, devices: AudioDevices) {
        self.inner.publish(move |s| s.voice_prefs.devices = devices);
    }

    /// The microphone test's state.
    pub fn set_test(&self, test: MicTestState) {
        self.inner.publish(move |s| s.voice_prefs.test = test);
    }

    /// Something the audio devices did that the Voice section shows. A notice already showing is not added twice.
    pub fn note(&self, notice: AudioNotice) {
        self.inner.publish(move |s| {
            if !s.voice_prefs.notices.contains(&notice) {
                s.voice_prefs.notices.push(notice);
            }
        });
    }

    /// The person has read a notice.
    pub fn dismiss(&self, notice: AudioNotice) {
        self.inner
            .publish(move |s| s.voice_prefs.notices.retain(|n| *n != notice));
    }
}

impl Client {
    /// The [`VoicePrefs`] handle.
    pub fn voice_prefs(&self) -> VoicePrefs {
        VoicePrefs {
            inner: self.inner().clone(),
        }
    }
}
