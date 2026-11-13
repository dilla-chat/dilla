//! libwebrtc's audio processing module (AEC3 echo cancellation, gain control, high-pass filter and
//! the standard noise suppressor), the one LiveKit's prebuilt libwebrtc already links and the
//! vendored webrtc-sys exposes (`livekit::webrtc::native::apm`): no new native build and no
//! network fetch.
//!
//! One [`Apm`] is shared by a call's microphone ([`crate::capture::Mic`], the capture side) and
//! its speaker ([`crate::playout::Speaker`], the render side: every frame that is played is handed
//! over as the echo reference). Both sides run on worker threads, never in a device's real-time
//! callback; the short critical sections serialise on one mutex.

use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};

use livekit::webrtc::native::apm::AudioProcessingModule;

use crate::MediaError;
use crate::noise::{FRAME_SAMPLES, RATE};

struct Inner {
    module: AudioProcessingModule,
    noise_suppression: bool,
    flags: ApmFlags,
    /// The stream delay last handed to the module (`None`: not yet, or the module was rebuilt).
    applied_delay_ms: Option<u32>,
}

struct Shared {
    inner: Mutex<Inner>,
    /// The devices' own latencies, as the host reports them (the capture stream's age of a sample,
    /// the render stream's time to the speaker). Their sum is the stream delay AEC3 starts from.
    capture_ms: AtomicU32,
    render_ms: AtomicU32,
}

/// libwebrtc rejects a delay beyond this.
const MAX_DELAY_MS: u32 = 500;

/// Which of the browser constraints Settings -> Voice switches (`echoCancellation`, `autoGainControl`): both on by default.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ApmFlags {
    pub echo: bool,
    pub agc: bool,
}

impl Default for ApmFlags {
    fn default() -> Self {
        Self {
            echo: true,
            agc: true,
        }
    }
}

fn build(noise_suppression: bool, flags: ApmFlags) -> AudioProcessingModule {
    // Echo cancellation and gain control as `flags` say, the high-pass filter always; noise suppression only for
    // `NoiseMode::Standard` (the web client's `noiseSuppression` constraint).
    AudioProcessingModule::new(flags.echo, flags.agc, true, noise_suppression)
}

#[derive(Clone)]
pub struct Apm {
    shared: Arc<Shared>,
}

impl Default for Apm {
    fn default() -> Self {
        Self::new(false)
    }
}

impl Apm {
    pub fn new(noise_suppression: bool) -> Self {
        Self {
            shared: Arc::new(Shared {
                inner: Mutex::new(Inner {
                    module: build(noise_suppression, ApmFlags::default()),
                    noise_suppression,
                    flags: ApmFlags::default(),
                    applied_delay_ms: None,
                }),
                capture_ms: AtomicU32::new(0),
                render_ms: AtomicU32::new(0),
            }),
        }
    }

    fn lock(&self) -> MutexGuard<'_, Inner> {
        self.shared.inner.lock().unwrap_or_else(|p| p.into_inner())
    }

    /// Switches the standard noise suppressor on or off. A change rebuilds the module (the echo
    /// canceller re-converges); the same setting is a no-op.
    pub fn set_noise_suppression(&self, on: bool) {
        let mut inner = self.lock();
        if inner.noise_suppression != on {
            inner.module = build(on, inner.flags);
            inner.noise_suppression = on;
            inner.applied_delay_ms = None;
        }
    }

    /// Switches echo cancellation and gain control. A change rebuilds the module; the same flags are a no-op.
    pub fn set_flags(&self, flags: ApmFlags) {
        let mut inner = self.lock();
        if inner.flags != flags {
            inner.module = build(inner.noise_suppression, flags);
            inner.flags = flags;
            inner.applied_delay_ms = None;
        }
    }

    /// The capture device's latency (the age of a sample when its callback runs).
    pub fn set_capture_latency_ms(&self, ms: u32) {
        self.shared.capture_ms.store(ms, Ordering::Relaxed);
    }

    /// The render device's latency (from the callback to the sound leaving the speaker).
    pub fn set_render_latency_ms(&self, ms: u32) {
        self.shared.render_ms.store(ms, Ordering::Relaxed);
    }

    /// The stream delay last given to the module: capture plus render latency, or `None` before
    /// the first frame.
    pub fn applied_delay_ms(&self) -> Option<u32> {
        self.lock().applied_delay_ms
    }

    /// Processes one 10 ms microphone frame in place.
    pub fn process_capture(&self, frame: &mut [i16]) -> Result<(), MediaError> {
        check(frame)?;
        let delay = (self.shared.capture_ms.load(Ordering::Relaxed)
            + self.shared.render_ms.load(Ordering::Relaxed))
        .min(MAX_DELAY_MS);
        let mut inner = self.lock();
        if inner.applied_delay_ms != Some(delay) {
            inner
                .module
                .set_stream_delay_ms(delay as i32)
                .map_err(|e| MediaError::Audio(e.message))?;
            inner.applied_delay_ms = Some(delay);
        }
        inner
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
