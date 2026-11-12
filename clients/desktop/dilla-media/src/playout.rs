//! The speaker: mixes every remote audio source into one 48 kHz mono stream for the output device
//! and hands every rendered frame to the audio processing module as the echo reference
//! ([`crate::apm::Apm::process_render`]), so the microphone's echo canceller hears what the
//! speaker plays.
//!
//! The device callback is real-time: it neither allocates nor takes a lock a network or worker
//! thread takes. Each remote source has its own preallocated SPSC [`Ring`] in a fixed pool (the
//! callback reads them; the map from source key to slot is only touched by the threads that push
//! and remove), and the mixed output goes to a render ring that the worker thread drains into the
//! APM, so the APM's mutex is never taken in the callback.
//!
//! The device stream lives on the worker thread (cpal's streams are not `Send` everywhere). When
//! the output device goes away the worker moves to the system default and sends
//! [`MediaEvent::DevicesChanged`], or [`MediaEvent::SpeakerLost`] when there is none.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::Duration;

use tokio::sync::mpsc::UnboundedSender;

use crate::MediaError;
use crate::MediaEvent;
use crate::apm::Apm;
use crate::devices::{self, AudioHost, DeviceId, Running};
use crate::noise::{FRAME_SAMPLES, RATE};
use crate::ring::Ring;

/// The most a source may queue: 200 ms. A source that delivers faster than the device plays (a
/// burst after a stall) loses its oldest samples instead of adding delay.
const MAX_QUEUED: usize = RATE as usize / 5;
/// How many remote sources can play at once; one more is dropped with a warning.
const SLOTS: usize = 16;
/// The mixing step: the callback works in chunks of this size on the stack.
const CHUNK: usize = 1024;
/// How often the worker moves the render reference into the APM and reads the stream's latency.
const TICK: Duration = Duration::from_millis(5);

struct Slot {
    ring: Ring,
    active: AtomicBool,
    /// The callback empties the ring (a source ended, or the slot was handed to another).
    clear: AtomicBool,
}

struct Pool {
    slots: Vec<Slot>,
    /// Source key to slot. Taken by `push` and `remove` only, never by the audio callback; it also
    /// serialises the producers, so each ring has one at a time.
    keys: Mutex<HashMap<String, usize>>,
}

/// Where decoded remote audio goes. Cheap to clone; every clone feeds the same speaker.
#[derive(Clone)]
pub struct PlayoutSink {
    pool: Arc<Pool>,
}

impl Default for PlayoutSink {
    fn default() -> Self {
        Self {
            pool: Arc::new(Pool {
                slots: (0..SLOTS)
                    .map(|_| Slot {
                        ring: Ring::new(2 * MAX_QUEUED),
                        active: AtomicBool::new(false),
                        clear: AtomicBool::new(false),
                    })
                    .collect(),
                keys: Mutex::new(HashMap::new()),
            }),
        }
    }
}

impl PlayoutSink {
    /// Queues 48 kHz mono samples of one remote source (any stable key: device and track).
    pub fn push(&self, source: &str, samples: &[i16]) {
        let mut keys = self.pool.keys.lock().unwrap_or_else(|p| p.into_inner());
        let index = match keys.get(source) {
            Some(i) => *i,
            None => {
                let used: Vec<usize> = keys.values().copied().collect();
                let Some(i) = (0..SLOTS).find(|i| !used.contains(i)) else {
                    log::warn!("dilla-media: more than {SLOTS} remote sources: one is not played");
                    return;
                };
                let slot = &self.pool.slots[i];
                slot.clear.store(true, Ordering::Release);
                slot.active.store(true, Ordering::Release);
                keys.insert(source.to_owned(), i);
                i
            }
        };
        self.pool.slots[index].ring.push(samples);
    }

    /// Forgets a source (its track ended).
    pub fn remove(&self, source: &str) {
        let mut keys = self.pool.keys.lock().unwrap_or_else(|p| p.into_inner());
        if let Some(i) = keys.remove(source) {
            let slot = &self.pool.slots[i];
            slot.active.store(false, Ordering::Release);
            slot.clear.store(true, Ordering::Release);
        }
    }

    /// Mixes the next `out.len()` samples of every source, clamped. Real-time safe: no allocation
    /// and no lock.
    pub fn mix(&self, out: &mut [i16]) {
        for part in out.chunks_mut(CHUNK) {
            let mut acc = [0i32; CHUNK];
            let mut tmp = [0i16; CHUNK];
            for slot in &self.pool.slots {
                if slot.clear.swap(false, Ordering::AcqRel) {
                    slot.ring.skip(usize::MAX);
                }
                if !slot.active.load(Ordering::Acquire) {
                    continue;
                }
                let excess = slot.ring.len().saturating_sub(MAX_QUEUED);
                if excess > 0 {
                    slot.ring.skip(excess);
                }
                let n = slot.ring.pop(&mut tmp[..part.len()]);
                for (a, s) in acc.iter_mut().zip(&tmp[..n]) {
                    *a += i32::from(*s);
                }
            }
            for (o, a) in part.iter_mut().zip(acc) {
                *o = a.clamp(i32::from(i16::MIN), i32::from(i16::MAX)) as i16;
            }
        }
    }
}

enum Ctl {
    Lost(u64),
    Switch(Option<DeviceId>, mpsc::SyncSender<Result<(), MediaError>>),
    Stop,
}

pub struct Speaker {
    sink: PlayoutSink,
    ctl: mpsc::Sender<Ctl>,
    worker: Option<JoinHandle<()>>,
}

struct Worker {
    host: Arc<dyn AudioHost>,
    sink: PlayoutSink,
    apm: Apm,
    ctl: mpsc::Sender<Ctl>,
    events: Option<UnboundedSender<MediaEvent>>,
    generation: u64,
    stream: Option<Box<dyn Running>>,
    /// What the callback rendered, waiting to become the APM's echo reference.
    reference: Arc<Ring>,
}

impl Worker {
    fn open(&mut self, device: Option<&DeviceId>) -> Result<(), MediaError> {
        self.stream = None;
        self.generation += 1;
        let generation = self.generation;
        let sink = self.sink.clone();
        let reference = Arc::new(Ring::new(32 * FRAME_SAMPLES));
        self.reference = reference.clone();
        let source = Box::new(move |out: &mut [i16]| {
            sink.mix(out);
            // Full means the worker is behind; the reference loses the newest samples.
            reference.push(out);
        });
        let ctl = self.ctl.clone();
        let on_lost = Box::new(move || {
            let _ = ctl.send(Ctl::Lost(generation));
        });
        self.stream = Some(self.host.open_output(device, source, on_lost)?);
        Ok(())
    }

    fn send(&self, e: MediaEvent) {
        if let Some(events) = &self.events {
            let _ = events.send(e);
        }
    }

    /// Moves what was played into the APM, a frame at a time, and passes the stream's latency on.
    fn feed_reference(&self, frame: &mut [i16; FRAME_SAMPLES]) {
        if let Some(stream) = &self.stream {
            self.apm.set_render_latency_ms(stream.latency_ms());
        }
        while self.reference.len() >= FRAME_SAMPLES {
            self.reference.pop(frame);
            if let Err(e) = self.apm.process_render(frame) {
                log::warn!("dilla-media: echo reference: {e}");
            }
        }
    }

    fn run(mut self, rx: mpsc::Receiver<Ctl>) {
        let mut frame = [0i16; FRAME_SAMPLES];
        loop {
            match rx.recv_timeout(TICK) {
                Ok(Ctl::Stop) | Err(mpsc::RecvTimeoutError::Disconnected) => break,
                Err(mpsc::RecvTimeoutError::Timeout) => {}
                Ok(Ctl::Switch(device, reply)) => {
                    let _ = reply.send(self.open(device.as_ref()));
                }
                Ok(Ctl::Lost(g)) if g == self.generation => {
                    // The device is gone: the system default takes over.
                    match self.open(None) {
                        Ok(()) => self.send(MediaEvent::DevicesChanged),
                        Err(e) => {
                            log::warn!("dilla-media: no speaker after a device loss: {e}");
                            self.send(MediaEvent::SpeakerLost);
                        }
                    }
                }
                Ok(Ctl::Lost(_)) => {}
            }
            self.feed_reference(&mut frame);
        }
    }
}

impl Speaker {
    /// Opens `device` (`None`: the system default) on the system host with an echo reference of
    /// its own (use [`Speaker::open_with`] to share a call's [`Apm`]).
    pub fn open(device: Option<DeviceId>) -> Result<Speaker, MediaError> {
        Self::open_with(devices::system(), Apm::default(), None, device)
    }

    pub fn open_with(
        host: Arc<dyn AudioHost>,
        apm: Apm,
        events: Option<UnboundedSender<MediaEvent>>,
        device: Option<DeviceId>,
    ) -> Result<Speaker, MediaError> {
        let sink = PlayoutSink::default();
        let (ctl, rx) = mpsc::channel();
        let (ready_tx, ready_rx) = mpsc::sync_channel(1);
        let worker_sink = sink.clone();
        let worker_ctl = ctl.clone();
        let handle = std::thread::Builder::new()
            .name("dilla-playout".into())
            .spawn(move || {
                // Built here: the stream it will own is not `Send`.
                let mut worker = Worker {
                    host,
                    sink: worker_sink,
                    apm,
                    ctl: worker_ctl,
                    events,
                    generation: 0,
                    stream: None,
                    reference: Arc::new(Ring::new(2)),
                };
                let opened = worker.open(device.as_ref());
                let failed = opened.is_err();
                let _ = ready_tx.send(opened);
                if !failed {
                    worker.run(rx);
                }
            })
            .map_err(|e| MediaError::Audio(e.to_string()))?;
        ready_rx
            .recv()
            .map_err(|_| MediaError::Audio("the playout thread ended".into()))??;
        Ok(Speaker {
            sink,
            ctl,
            worker: Some(handle),
        })
    }

    /// Where remote audio is pushed.
    pub fn sink(&self) -> PlayoutSink {
        self.sink.clone()
    }

    /// Plays on another output device (`None`: the system default). Blocks until the worker has
    /// switched: call it from a blocking context.
    pub fn switch(&self, device: Option<DeviceId>) -> Result<(), MediaError> {
        let (tx, rx) = mpsc::sync_channel(1);
        self.ctl
            .send(Ctl::Switch(device, tx))
            .map_err(|_| MediaError::Closed)?;
        rx.recv().map_err(|_| MediaError::Closed)?
    }
}

impl Drop for Speaker {
    fn drop(&mut self) {
        let _ = self.ctl.send(Ctl::Stop);
        if let Some(w) = self.worker.take() {
            let _ = w.join();
        }
    }
}
