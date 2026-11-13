//! The microphone: a `cpal` input (48 kHz mono, cut into 10 ms frames) through the audio processing
//! module (echo cancellation against what the speaker plays, gain control, high-pass filter and,
//! for `Standard`, the standard noise suppressor) and, for `Enhanced`, DeepFilterNet3 after it.
//! The processed frames go to whoever feeds the call's `NativeAudioSource`
//! ([`Mic::take_frames`]).
//!
//! The device callback is real-time: it only writes the samples into a preallocated SPSC
//! [`Ring`] (a full ring drops them: a stalled worker never blocks the callback). The worker
//! thread, which also owns the device stream (cpal's streams are not `Send` everywhere), polls the
//! ring every few milliseconds and does all the processing.
//!
//! Visible fallbacks, never silent ones:
//! - `Enhanced` without a loadable model runs `Standard` and sends
//!   [`MediaEvent::EnhancedUnavailable`]; `Off` loads no model;
//! - an enhanced frame that fails is silenced (never passed on un-suppressed) and
//!   [`MediaEvent::EnhancedFailed`] is sent once per mode;
//! - a device that is gone (unplugged, or never there) moves capture to the system default and
//!   sends one [`MediaEvent::DevicesChanged`]; with no default either, [`MediaEvent::MicLost`].

use std::sync::{Arc, Mutex, mpsc};
use std::thread::JoinHandle;
use std::time::Duration;

use tokio::sync::mpsc::{Receiver, UnboundedReceiver, UnboundedSender};

use crate::MediaError;
use crate::MediaEvent;
use crate::apm::{Apm, ApmFlags};
use crate::devices::{self, AudioHost, DeviceId, Running};
use crate::noise::{FRAME_SAMPLES, ModelLoader, NoiseMode, NoiseProcessor, embedded_model};
use crate::ring::Ring;

/// Capacity of the device-to-worker ring: 1 s. Beyond it new samples are dropped (the worker fell
/// behind) rather than the delay growing.
const RING_SAMPLES: usize = 48_000;
/// Processed frames waiting for the consumer.
const OUTPUT_FRAMES: usize = 100;
/// How often the worker empties the ring and reads the stream's latency.
const TICK: Duration = Duration::from_millis(5);

/// What a microphone runs on.
#[derive(Clone)]
pub struct CaptureConfig {
    pub host: Arc<dyn AudioHost>,
    /// Shared with the call's [`crate::playout::Speaker`] so the echo canceller has its reference.
    pub apm: Apm,
    pub model: ModelLoader,
    /// Echo cancellation and gain control (Settings -> Voice); both on by default.
    pub flags: ApmFlags,
}

impl Default for CaptureConfig {
    fn default() -> Self {
        Self {
            host: devices::system(),
            apm: Apm::default(),
            model: Arc::new(embedded_model),
            flags: ApmFlags::default(),
        }
    }
}

enum Msg {
    Lost(u64),
    Switch {
        device: Option<DeviceId>,
        mode: NoiseMode,
        reply: mpsc::SyncSender<Result<NoiseMode, MediaError>>,
    },
    Stop,
}

pub struct Mic {
    ctl: mpsc::Sender<Msg>,
    frames: Option<Receiver<Vec<i16>>>,
    events: Option<UnboundedReceiver<MediaEvent>>,
    mode: Arc<Mutex<NoiseMode>>,
    worker: Option<JoinHandle<()>>,
}

struct Worker {
    config: CaptureConfig,
    ctl: mpsc::Sender<Msg>,
    frames: tokio::sync::mpsc::Sender<Vec<i16>>,
    events: UnboundedSender<MediaEvent>,
    mode: Arc<Mutex<NoiseMode>>,
    generation: u64,
    stream: Option<Box<dyn Running>>,
    /// The current stream's ring (a new stream gets a new one).
    ring: Arc<Ring>,
    dfn: Option<NoiseProcessor>,
    dfn_failed: bool,
    pending: Vec<i16>,
}

impl Worker {
    fn event(&self, e: MediaEvent) {
        let _ = self.events.send(e);
    }

    /// Opens the device; one that cannot be opened is replaced by the system default (one
    /// `DevicesChanged`). Only a machine without a working default fails.
    fn open(&mut self, device: Option<&DeviceId>) -> Result<(), MediaError> {
        self.stream = None;
        self.pending.clear();
        match self.try_open(device) {
            Ok(()) => Ok(()),
            Err(e) if device.is_some() => {
                log::warn!("dilla-media: microphone unavailable ({e}); using the default");
                self.try_open(None)?;
                self.event(MediaEvent::DevicesChanged);
                Ok(())
            }
            Err(e) => Err(e),
        }
    }

    fn try_open(&mut self, device: Option<&DeviceId>) -> Result<(), MediaError> {
        self.generation += 1;
        let generation = self.generation;
        let ring = Arc::new(Ring::new(RING_SAMPLES));
        let sink_ring = ring.clone();
        // Real-time: a ring write, nothing else.
        let sink = Box::new(move |samples: &[i16]| {
            sink_ring.push(samples);
        });
        let tx = self.ctl.clone();
        let on_lost = Box::new(move || {
            let _ = tx.send(Msg::Lost(generation));
        });
        self.stream = Some(self.config.host.open_input(device, sink, on_lost)?);
        self.ring = ring;
        Ok(())
    }

    /// Builds the chain for `mode`; returns the mode that actually runs.
    fn set_mode(&mut self, mode: NoiseMode) -> NoiseMode {
        self.config.apm.set_flags(self.config.flags);
        self.dfn = None;
        self.dfn_failed = false;
        let mut effective = mode;
        if mode == NoiseMode::Enhanced {
            let loaded = (self.config.model)().and_then(NoiseProcessor::enhanced_from);
            match loaded {
                Ok(p) => self.dfn = Some(p),
                Err(e) => {
                    log::warn!("dilla-media: enhanced noise suppression unavailable: {e}");
                    self.event(MediaEvent::EnhancedUnavailable);
                    effective = NoiseMode::Standard;
                }
            }
        }
        self.config
            .apm
            .set_noise_suppression(effective == NoiseMode::Standard);
        *self.mode.lock().unwrap_or_else(|p| p.into_inner()) = effective;
        effective
    }

    /// Empties the ring, processes every whole 10 ms frame.
    fn drain(&mut self) {
        if let Some(stream) = &self.stream {
            self.config.apm.set_capture_latency_ms(stream.latency_ms());
        }
        let mut chunk = [0i16; FRAME_SAMPLES];
        loop {
            let n = self.ring.pop(&mut chunk);
            if n == 0 {
                break;
            }
            self.pending.extend_from_slice(&chunk[..n]);
            self.frames_out();
        }
    }

    fn frames_out(&mut self) {
        while self.pending.len() >= FRAME_SAMPLES {
            let mut frame: Vec<i16> = self.pending.drain(..FRAME_SAMPLES).collect();
            // A frame that cannot be processed is silenced: never the raw microphone.
            if let Err(e) = self.config.apm.process_capture(&mut frame) {
                log::warn!("dilla-media: capture processing: {e}");
                frame.fill(0);
            } else if let Some(dfn) = &mut self.dfn
                && let Err(e) = dfn.process(&mut frame)
            {
                // `process` silenced the frame.
                log::warn!("dilla-media: noise processor: {e}");
                if !self.dfn_failed {
                    self.dfn_failed = true;
                    self.event(MediaEvent::EnhancedFailed);
                }
            }
            if self.frames.try_send(frame).is_err() {
                log::debug!("dilla-media: a microphone frame dropped (consumer behind)");
            }
        }
    }

    fn run(mut self, rx: mpsc::Receiver<Msg>) {
        loop {
            match rx.recv_timeout(TICK) {
                Ok(Msg::Stop) | Err(mpsc::RecvTimeoutError::Disconnected) => break,
                Err(mpsc::RecvTimeoutError::Timeout) => {}
                Ok(Msg::Lost(g)) if g == self.generation => {
                    // The device is gone: the system default takes over, once.
                    match self.open(None) {
                        Ok(()) => self.event(MediaEvent::DevicesChanged),
                        Err(e) => {
                            log::warn!("dilla-media: no microphone after a device loss: {e}");
                            self.event(MediaEvent::MicLost);
                        }
                    }
                }
                Ok(Msg::Lost(_)) => {}
                Ok(Msg::Switch {
                    device,
                    mode,
                    reply,
                }) => {
                    let result = self.open(device.as_ref()).map(|()| self.set_mode(mode));
                    let _ = reply.send(result);
                }
            }
            self.drain();
        }
    }
}

impl Mic {
    /// Opens `device` (`None`: the system default) on the system host, with an echo reference of
    /// its own.
    pub fn open(device: Option<DeviceId>, mode: NoiseMode) -> Result<Mic, MediaError> {
        Self::open_with(CaptureConfig::default(), device, mode)
    }

    pub fn open_with(
        config: CaptureConfig,
        device: Option<DeviceId>,
        mode: NoiseMode,
    ) -> Result<Mic, MediaError> {
        let (ctl, rx) = mpsc::channel();
        let (frames_tx, frames) = tokio::sync::mpsc::channel(OUTPUT_FRAMES);
        let (events_tx, events) = tokio::sync::mpsc::unbounded_channel();
        let mode_cell = Arc::new(Mutex::new(mode));
        let (ready_tx, ready_rx) = mpsc::sync_channel(1);
        let (worker_ctl, worker_mode) = (ctl.clone(), mode_cell.clone());
        let handle = std::thread::Builder::new()
            .name("dilla-capture".into())
            .spawn(move || {
                // Built here: the stream it will own is not `Send`.
                let mut worker = Worker {
                    config,
                    ctl: worker_ctl,
                    frames: frames_tx,
                    events: events_tx,
                    mode: worker_mode,
                    generation: 0,
                    stream: None,
                    ring: Arc::new(Ring::new(2)),
                    dfn: None,
                    dfn_failed: false,
                    pending: Vec::with_capacity(2 * FRAME_SAMPLES),
                };
                let opened = worker.open(device.as_ref()).map(|()| worker.set_mode(mode));
                let failed = opened.is_err();
                let _ = ready_tx.send(opened);
                if !failed {
                    worker.run(rx);
                }
            })
            .map_err(|e| MediaError::Audio(e.to_string()))?;
        ready_rx
            .recv()
            .map_err(|_| MediaError::Audio("the capture thread ended".into()))??;
        Ok(Mic {
            ctl,
            frames: Some(frames),
            events: Some(events),
            mode: mode_cell,
            worker: Some(handle),
        })
    }

    /// The mode that runs: `Standard` after an `Enhanced` that could not load.
    pub fn mode(&self) -> NoiseMode {
        *self.mode.lock().unwrap_or_else(|p| p.into_inner())
    }

    /// The processed 10 ms frames (48 kHz mono), once.
    pub fn take_frames(&mut self) -> Option<Receiver<Vec<i16>>> {
        self.frames.take()
    }

    /// The fallback and device events, once.
    pub fn take_events(&mut self) -> Option<UnboundedReceiver<MediaEvent>> {
        self.events.take()
    }

    /// Captures from another device and/or in another mode under the same consumer; returns the
    /// mode that runs. Blocks until the worker has switched (loading the enhanced model can take
    /// a while): call it from a blocking context (`Call::set_mic` does).
    pub fn switch(
        &self,
        device: Option<DeviceId>,
        mode: NoiseMode,
    ) -> Result<NoiseMode, MediaError> {
        let (tx, rx) = mpsc::sync_channel(1);
        self.ctl
            .send(Msg::Switch {
                device,
                mode,
                reply: tx,
            })
            .map_err(|_| MediaError::Closed)?;
        rx.recv().map_err(|_| MediaError::Closed)?
    }
}

impl Drop for Mic {
    fn drop(&mut self) {
        let _ = self.ctl.send(Msg::Stop);
        if let Some(w) = self.worker.take() {
            let _ = w.join();
        }
    }
}
