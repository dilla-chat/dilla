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
//!
//! A call's speaker ([`Speaker::open_waiting`]) never fails for want of a device: with none (a
//! machine with no output, or one that lost its last), it plays nowhere as beta.5's call did, keeps
//! every source drained so nothing queues up, and opens the system default as soon as one appears.

use std::collections::HashMap;
use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

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
/// How often a speaker with no device tries the system default again.
const RETRY: Duration = Duration::from_secs(1);

struct Slot {
    ring: Ring,
    active: AtomicBool,
    /// Set when the source ended; the callback empties the ring and then resets it. A slot is
    /// handed to a new source only once this is false again, so a new source's first samples are
    /// never emptied with the old one's (the clear-versus-first-push race).
    clear: AtomicBool,
    /// The source's gain (f32 bits): its person's volume, 1.0 by default.
    gain: AtomicU32,
}

/// The most a source is amplified: 200 % of the person's volume times 200 % of the output volume.
pub const MAX_GAIN: f32 = 4.0;

struct Pool {
    slots: Vec<Slot>,
    /// Source key to slot. Taken by `push` and `remove` only, never by the audio callback; it also
    /// serialises the producers, so each ring has one at a time.
    keys: Mutex<Keys>,
    deafened: AtomicBool,
}

#[derive(Default)]
struct Keys {
    slots: HashMap<String, usize>,
    /// The sources refused for want of a slot: each is warned about once, not per frame.
    refused: std::collections::HashSet<String>,
    /// Each source's gain group ([`PlayoutSink::group`]): a person's voice, or their stream's sound.
    /// Kept after the source ends (a resubscribed track keeps its key; a call has few).
    groups: HashMap<String, String>,
    /// Each gain group's gain ([`PlayoutSink::set_gain`]); a group without one plays at 1.0.
    gains: HashMap<String, f32>,
}

impl Keys {
    fn gain_of(&self, source: &str) -> f32 {
        self.groups
            .get(source)
            .and_then(|g| self.gains.get(g))
            .copied()
            .unwrap_or(1.0)
    }
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
                        gain: AtomicU32::new(1.0f32.to_bits()),
                    })
                    .collect(),
                keys: Mutex::new(Keys::default()),
                deafened: AtomicBool::new(false),
            }),
        }
    }
}

impl PlayoutSink {
    /// Queues 48 kHz mono samples of one remote source (any stable key: device and track).
    pub fn push(&self, source: &str, samples: &[i16]) {
        let mut keys = self.pool.keys.lock().unwrap_or_else(|p| p.into_inner());
        let index = match keys.slots.get(source) {
            Some(i) => *i,
            None => {
                let free = (0..SLOTS).find(|i| {
                    !keys.slots.values().any(|u| u == i)
                        && !self.pool.slots[*i].clear.load(Ordering::Acquire)
                });
                let Some(i) = free else {
                    if keys.refused.insert(source.to_owned()) {
                        log::warn!(
                            "dilla-media: more than {SLOTS} remote sources: {source} is not played"
                        );
                    }
                    return;
                };
                keys.refused.remove(source);
                let gain = keys.gain_of(source);
                self.pool.slots[i]
                    .gain
                    .store(gain.to_bits(), Ordering::Release);
                self.pool.slots[i].active.store(true, Ordering::Release);
                keys.slots.insert(source.to_owned(), i);
                i
            }
        };
        self.pool.slots[index].ring.push(samples);
    }

    /// Forgets a source (its track ended): its slot is emptied by the device callback and then
    /// free again.
    pub fn remove(&self, source: &str) {
        let mut keys = self.pool.keys.lock().unwrap_or_else(|p| p.into_inner());
        keys.refused.remove(source);
        if let Some(i) = keys.slots.remove(source) {
            let slot = &self.pool.slots[i];
            slot.active.store(false, Ordering::Release);
            slot.clear.store(true, Ordering::Release);
        }
    }

    /// Puts `source` in the gain group `group` (a person's voice or their stream's sound, any stable
    /// key): it plays at that group's gain from now on.
    pub fn group(&self, source: &str, group: &str) {
        let mut keys = self.pool.keys.lock().unwrap_or_else(|p| p.into_inner());
        keys.groups.insert(source.to_owned(), group.to_owned());
        let gain = keys.gain_of(source);
        if let Some(i) = keys.slots.get(source) {
            self.pool.slots[*i]
                .gain
                .store(gain.to_bits(), Ordering::Release);
        }
    }

    /// Plays every source of `group` at `gain` (0 is silence, 1 as received, at most [`MAX_GAIN`]):
    /// the per-person volume (call/volume.ts `gainOf`).
    pub fn set_gain(&self, group: &str, gain: f32) {
        let gain = if gain.is_finite() {
            gain.clamp(0.0, MAX_GAIN)
        } else {
            1.0
        };
        let mut keys = self.pool.keys.lock().unwrap_or_else(|p| p.into_inner());
        if gain == 1.0 {
            keys.gains.remove(group);
        } else {
            keys.gains.insert(group.to_owned(), gain);
        }
        for (source, i) in &keys.slots {
            if keys.groups.get(source).is_some_and(|g| g == group) {
                self.pool.slots[*i]
                    .gain
                    .store(gain.to_bits(), Ordering::Release);
            }
        }
    }

    /// How many sources hold a slot (tests).
    pub fn sources(&self) -> usize {
        self.pool
            .keys
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .slots
            .len()
    }

    /// Mutes or unmutes every source (deafen): the callback still drains the rings, so nothing
    /// queues up meanwhile, but plays silence.
    pub fn set_deafened(&self, on: bool) {
        self.pool.deafened.store(on, Ordering::Release);
    }

    /// Drops everything every source has queued (a speaker with no device: nothing plays, so nothing may wait to play
    /// late). Only for a sink nothing mixes meanwhile: it consumes the rings as the device callback would.
    fn discard(&self) {
        for slot in &self.pool.slots {
            slot.ring.skip(usize::MAX);
            if slot.clear.load(Ordering::Acquire) {
                slot.clear.store(false, Ordering::Release);
            }
        }
    }

    /// Mixes the next `out.len()` samples of every source, clamped. Real-time safe: no allocation
    /// and no lock.
    pub fn mix(&self, out: &mut [i16]) {
        for part in out.chunks_mut(CHUNK) {
            let mut acc = [0i32; CHUNK];
            let mut tmp = [0i16; CHUNK];
            for slot in &self.pool.slots {
                if slot.clear.load(Ordering::Acquire) {
                    slot.ring.skip(usize::MAX);
                    // Reset only once the ring is empty: the slot is free from here.
                    slot.clear.store(false, Ordering::Release);
                }
                if !slot.active.load(Ordering::Acquire) {
                    continue;
                }
                let excess = slot.ring.len().saturating_sub(MAX_QUEUED);
                if excess > 0 {
                    slot.ring.skip(excess);
                }
                let n = slot.ring.pop(&mut tmp[..part.len()]);
                let gain = f32::from_bits(slot.gain.load(Ordering::Acquire));
                if gain == 1.0 {
                    for (a, s) in acc.iter_mut().zip(&tmp[..n]) {
                        *a += i32::from(*s);
                    }
                } else {
                    for (a, s) in acc.iter_mut().zip(&tmp[..n]) {
                        *a += (f32::from(*s) * gain) as i32;
                    }
                }
            }
            if self.pool.deafened.load(Ordering::Acquire) {
                acc = [0; CHUNK];
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
    playing: Arc<AtomicBool>,
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
    /// A device stream is open.
    playing: Arc<AtomicBool>,
    /// With no stream: when the system default was last tried, and whether its return is said
    /// ([`MediaEvent::DevicesChanged`], after a loss that said [`MediaEvent::SpeakerLost`]).
    retry: Option<(Instant, bool)>,
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
        let opened = self.host.open_output(device, source, on_lost);
        self.playing.store(opened.is_ok(), Ordering::Release);
        self.stream = Some(opened?);
        Ok(())
    }

    /// With no device: the sources are drained as a device would (nothing queues up, nothing is
    /// played), and the system default is tried again every [`RETRY`].
    fn wait_for_device(&mut self) {
        let Some((tried, announce)) = self.retry else {
            return;
        };
        // Everything queued is dropped, not drained a tick's worth at a time: a tick is only as regular as the OS's
        // timers (macOS coalesces them well past 5 ms), and what was not drained would play late once a device opened.
        self.sink.discard();
        if tried.elapsed() < RETRY {
            return;
        }
        match self.open(None) {
            Ok(()) => {
                log::info!("dilla-media: an output device appeared: playing again");
                self.retry = None;
                if announce {
                    self.send(MediaEvent::DevicesChanged);
                }
            }
            Err(_) => self.retry = Some((Instant::now(), announce)),
        }
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

    fn run(mut self, rx: mpsc::Receiver<Ctl>, waits: bool) {
        let mut frame = [0i16; FRAME_SAMPLES];
        loop {
            match rx.recv_timeout(TICK) {
                Ok(Ctl::Stop) | Err(mpsc::RecvTimeoutError::Disconnected) => break,
                Err(mpsc::RecvTimeoutError::Timeout) => {}
                Ok(Ctl::Switch(device, reply)) => {
                    let opened = self.open(device.as_ref());
                    if opened.is_ok() {
                        self.retry = None;
                    } else if waits && self.retry.is_none() {
                        self.retry = Some((Instant::now(), false));
                    }
                    let _ = reply.send(opened);
                }
                Ok(Ctl::Lost(g)) if g == self.generation => {
                    // The device is gone: the system default takes over.
                    match self.open(None) {
                        Ok(()) => self.send(MediaEvent::DevicesChanged),
                        Err(e) => {
                            log::warn!("dilla-media: no speaker after a device loss: {e}");
                            self.send(MediaEvent::SpeakerLost);
                            if waits {
                                self.retry = Some((Instant::now(), true));
                            }
                        }
                    }
                }
                Ok(Ctl::Lost(_)) => {}
            }
            self.wait_for_device();
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
        Self::start(host, apm, events, device, false)
    }

    /// As [`open_with`](Speaker::open_with) for a call: no output device (none at all, or none
    /// left after a loss) is not a failure. The speaker then plays nowhere, as beta.5's call did,
    /// with no echo reference, and opens the system default once one appears; until then
    /// [`is_playing`](Speaker::is_playing) is false. Fails only when its thread cannot start.
    pub fn open_waiting(
        host: Arc<dyn AudioHost>,
        apm: Apm,
        events: Option<UnboundedSender<MediaEvent>>,
        device: Option<DeviceId>,
    ) -> Result<Speaker, MediaError> {
        Self::start(host, apm, events, device, true)
    }

    fn start(
        host: Arc<dyn AudioHost>,
        apm: Apm,
        events: Option<UnboundedSender<MediaEvent>>,
        device: Option<DeviceId>,
        waits: bool,
    ) -> Result<Speaker, MediaError> {
        let sink = PlayoutSink::default();
        let playing = Arc::new(AtomicBool::new(false));
        let worker_playing = playing.clone();
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
                    playing: worker_playing,
                    retry: None,
                };
                let opened = match worker.open(device.as_ref()) {
                    Err(e) if waits => {
                        log::warn!("dilla-media: no output device ({e}): playing nowhere");
                        worker.retry = Some((Instant::now(), false));
                        Ok(())
                    }
                    other => other,
                };
                let failed = opened.is_err();
                let _ = ready_tx.send(opened);
                if !failed {
                    worker.run(rx, waits);
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
            playing,
        })
    }

    /// An output device is open (false while a waiting speaker has none).
    pub fn is_playing(&self) -> bool {
        self.playing.load(Ordering::Acquire)
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
