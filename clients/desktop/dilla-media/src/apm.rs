//! libwebrtc's audio processing module (AEC3 echo cancellation, gain control, high-pass filter and
//! the standard noise suppressor), the one LiveKit's prebuilt libwebrtc already links and the
//! vendored webrtc-sys exposes (`livekit::webrtc::native::apm`): no new native build and no
//! network fetch.
//!
//! One [`Apm`] is shared by a call's microphone ([`crate::capture::Mic`], the capture side) and
//! its speaker ([`crate::playout::Speaker`], the render side: every frame that is played is handed
//! over as the echo reference). The capture side runs on the microphone worker thread and the
//! render side in the playout callback; the short critical sections serialise on one mutex.

use std::sync::{Arc, Mutex, MutexGuard};

use livekit::webrtc::native::apm::AudioProcessingModule;

use crate::MediaError;
use crate::noise::{FRAME_SAMPLES, RATE};

struct Inner {
    module: AudioProcessingModule,
    noise_suppression: bool,
}

fn build(noise_suppression: bool) -> AudioProcessingModule {
    // Echo cancellation, gain control and the high-pass filter always; noise suppression only for
    // `NoiseMode::Standard` (the web client's `noiseSuppression` constraint).
    AudioProcessingModule::new(true, true, true, noise_suppression)
}

#[derive(Clone)]
pub struct Apm {
    inner: Arc<Mutex<Inner>>,
}

impl Default for Apm {
    fn default() -> Self {
        Self::new(false)
    }
}

impl Apm {
    pub fn new(noise_suppression: bool) -> Self {
        Self {
            inner: Arc::new(Mutex::new(Inner {
                module: build(noise_suppression),
                noise_suppression,
            })),
        }
    }

    fn lock(&self) -> MutexGuard<'_, Inner> {
        self.inner.lock().unwrap_or_else(|p| p.into_inner())
    }

    /// Switches the standard noise suppressor on or off. A change rebuilds the module (the echo
    /// canceller re-converges); the same setting is a no-op.
    pub fn set_noise_suppression(&self, on: bool) {
        let mut inner = self.lock();
        if inner.noise_suppression != on {
            inner.module = build(on);
            inner.noise_suppression = on;
        }
    }

    /// Processes one 10 ms microphone frame in place.
    pub fn process_capture(&self, frame: &mut [i16]) -> Result<(), MediaError> {
        check(frame)?;
        self.lock()
            .module
            .process_stream(frame, RATE as i32, 1)
            .map_err(|e| MediaError::Audio(e.message))
    }

    /// Hands one 10 ms frame that is about to be played to the echo canceller as its reference.
    pub fn process_render(&self, frame: &[i16]) -> Result<(), MediaError> {
        check(frame)?;
        // The module wants a mutable buffer; the played samples stay as they are.
        let mut copy = [0i16; FRAME_SAMPLES];
        copy.copy_from_slice(frame);
        self.lock()
            .module
            .process_reverse_stream(&mut copy, RATE as i32, 1)
            .map_err(|e| MediaError::Audio(e.message))
    }
}

fn check(frame: &[i16]) -> Result<(), MediaError> {
    if frame.len() == FRAME_SAMPLES {
        Ok(())
    } else {
        Err(MediaError::Audio(format!(
            "a frame is {FRAME_SAMPLES} samples, not {}",
            frame.len()
        )))
    }
}
