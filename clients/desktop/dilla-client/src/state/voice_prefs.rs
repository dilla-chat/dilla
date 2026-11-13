//! `voice_prefs`: Task 19's slice of [`super::AppState`]: the voice settings, the devices the host lists, the microphone
//! test and the audio notices the Voice section shows. Nothing here holds key material.

use crate::features::voice_prefs::VoiceValues;
use crate::features::voice_prefs::gate::AUTO_START_DB;

/// One audio device as the settings list it: `id` is what the choice is stored as, `name` what the select shows.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct AudioDevice {
    pub id: String,
    pub name: String,
}

/// The microphones and speakers the host lists (the system default is always the first choice of a select and is not
/// listed here).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct AudioDevices {
    pub inputs: Vec<AudioDevice>,
    pub outputs: Vec<AudioDevice>,
}

/// What the microphone test (Let's check) shows.
#[derive(Clone, Debug, PartialEq)]
pub struct MicTestState {
    /// The loopback runs: the meter is live and the pill says whether you are heard.
    pub testing: bool,
    /// The test could not start (the microphone could not be opened): `settings.voice.testFailed`.
    pub failed: bool,
    /// The level after the input volume, -100..0 dB (-100 while not testing).
    pub level_db: f32,
    /// The gate is open (its own decision, with its hold).
    pub open: bool,
    /// The automatic threshold measured last, or the resting -48 dB.
    pub auto_db: f32,
    /// The processor line: what runs and why it fell back, if it did.
    pub processor: Option<ProcessorStatus>,
}

impl Default for MicTestState {
    fn default() -> Self {
        Self {
            testing: false,
            failed: false,
            level_db: -100.0,
            open: false,
            auto_db: AUTO_START_DB,
            processor: None,
        }
    }
}

/// Which noise processor runs (`ProcessorStatus.effective`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Processor {
    Off,
    /// libwebrtc's own suppressor.
    Standard,
    /// DeepFilterNet3.
    Enhanced,
}

/// Why the running processor is not the chosen one (`ProcessorStatus.reason`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ProcessorNote {
    /// `Automatic` was chosen but its model did not load: Lightweight runs (`enhanced-failed`).
    EnhancedUnavailable,
}

/// What the processor line says (`settings.voice.processor`, `settings.voice.fallback.<reason>`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ProcessorStatus {
    pub effective: Processor,
    pub note: Option<ProcessorNote>,
}

/// Something the audio devices did that the Voice section shows (never swallowed): `MediaEvent` of dilla-media.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum AudioNotice {
    /// The microphone is gone and there is no default to take over: nothing is captured.
    MicLost,
    /// The speaker is gone and there is no default to take over: nothing is played.
    SpeakerLost,
    /// The enhanced noise processor failed on a frame (that frame was silenced).
    EnhancedFailed,
}

/// Task 19's slice.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct VoicePrefsState {
    /// The stored settings, as validated; defaults until [`crate::features::voice_prefs::VoicePrefs::load`].
    pub values: VoiceValues,
    pub devices: AudioDevices,
    pub test: MicTestState,
    /// Notices still showing, oldest first, each once.
    pub notices: Vec<AudioNotice>,
}
