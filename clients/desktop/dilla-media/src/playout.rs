//! The speaker: mixes every remote audio source into one 48 kHz mono stream for the output device
//! and hands every rendered frame to the audio processing module as the echo reference
//! ([`crate::apm::Apm::process_render`]), so the microphone's echo canceller hears what the
//! speaker plays.
//!
//! The device stream lives on the worker thread (cpal's streams are not `Send` everywhere). When
//! the output device goes away the worker moves to the system default and sends
//! [`MediaEvent::DevicesChanged`].

use std::collections::{HashMap, VecDeque};
use std::sync::mpsc;
use std::sync::{Arc, Mutex};
use std::thread::JoinHandle;

use tokio::sync::mpsc::UnboundedSender;

use crate::MediaError;
use crate::MediaEvent;
use crate::apm::Apm;
use crate::devices::{self, AudioHost, DeviceId, Running};
use crate::noise::{FRAME_SAMPLES, RATE};

/// The most a source may queue: 200 ms. A source that delivers faster than the device plays (a
/// burst after a stall) loses its oldest samples instead of adding delay.
const MAX_QUEUED: usize = RATE as usize / 5;

#[derive(Default)]
struct Mixer {
    sources: HashMap<String, VecDeque<i16>>,
}

/// Where decoded remote audio goes. Cheap to clone; every clone feeds the same speaker.
#[derive(Clone, Default)]
pub struct PlayoutSink {
    mixer: Arc<Mutex<Mixer>>,
}

impl PlayoutSink {
    /// Queues 48 kHz mono samples of one remote source (any stable key: device and track).
    pub fn push(&self, source: &str, samples: &[i16]) {
        let mut m = self.mixer.lock().unwrap_or_else(|p| p.into_inner());
        let q = m.sources.entry(source.to_owned()).or_default();
        q.extend(samples.iter().copied());
        while q.len() > MAX_QUEUED {
            q.pop_front();
        }
    }

    /// Forgets a source (its track ended).
    pub fn remove(&self, source: &str) {
        self.mixer
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .sources
            .remove(source);
    }

    /// Mixes the next `out.len()` samples of every source, clamped.
    fn mix(&self, out: &mut [i16]) {
        let mut m = self.mixer.lock().unwrap_or_else(|p| p.into_inner());
        let mut acc = vec![0i32; out.len()];
        for q in m.sources.values_mut() {
            for a in acc.iter_mut() {
                match q.pop_front() {
                    Some(s) => *a += i32::from(s),
                    None => break,
                }
            }
        }
        for (o, a) in out.iter_mut().zip(acc) {
            *o = a.clamp(i32::from(i16::MIN), i32::from(i16::MAX)) as i16;
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
}

impl Worker {
    fn open(&mut self, device: Option<&DeviceId>) -> Result<(), MediaError> {
        self.stream = None;
        self.generation += 1;
        let generation = self.generation;
        let (sink, apm) = (self.sink.clone(), self.apm.clone());
        let mut reference: Vec<i16> = Vec::with_capacity(2 * FRAME_SAMPLES);
        let source = Box::new(move |out: &mut [i16]| {
            sink.mix(out);
            reference.extend_from_slice(out);
            while reference.len() >= FRAME_SAMPLES {
                if let Err(e) = apm.process_render(&reference[..FRAME_SAMPLES]) {
                    log::warn!("dilla-media: echo reference: {e}");
                }
                reference.drain(..FRAME_SAMPLES);
            }
        });
        let ctl = self.ctl.clone();
        let on_lost = Box::new(move || {
            let _ = ctl.send(Ctl::Lost(generation));
        });
        self.stream = Some(self.host.open_output(device, source, on_lost)?);
        Ok(())
    }

    fn changed(&self) {
        if let Some(events) = &self.events {
            let _ = events.send(MediaEvent::DevicesChanged);
        }
    }

    fn run(mut self, rx: mpsc::Receiver<Ctl>) {
        while let Ok(msg) = rx.recv() {
            match msg {
                Ctl::Stop => break,
                Ctl::Switch(device, reply) => {
                    let _ = reply.send(self.open(device.as_ref()));
                }
                Ctl::Lost(g) if g == self.generation => {
                    // The device is gone: the system default takes over.
                    match self.open(None) {
                        Ok(()) => self.changed(),
                        Err(e) => log::warn!("dilla-media: no speaker after a device loss: {e}"),
                    }
                }
                Ctl::Lost(_) => {}
            }
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

    /// Plays on another output device (`None`: the system default).
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
