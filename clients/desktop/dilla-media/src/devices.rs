//! The machine's microphones and speakers, through `cpal` (ALSA/PulseAudio/PipeWire, CoreAudio,
//! WASAPI), behind the small [`AudioHost`] trait so the capture and playout code runs the same on a
//! fake host in tests (a hot-unplug, a loopback with a delay). Everything that crosses this
//! boundary is 48 kHz mono 16-bit: the host adapter converts (channels, sample format, rate).
//!
//! Listing opens no device. [`watch`] polls, because cpal reports no device-change events.

use std::collections::VecDeque;
use std::sync::Arc;
use std::sync::mpsc;
use std::time::Duration;

use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};

use crate::MediaError;
use crate::noise::{FRAME_SAMPLES, RATE};

/// A device's stable id as the platform gives it (cpal's `DeviceId`, in its string form).
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct DeviceId(pub String);

impl DeviceId {
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

/// One audio device: `id` is what [`DeviceId`] holds, `name` what the settings show.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Device {
    pub id: String,
    pub name: String,
}

impl Device {
    pub fn device_id(&self) -> DeviceId {
        DeviceId(self.id.clone())
    }
}

/// Every device the host lists, and which ones it defaults to.
#[derive(Clone, Debug, PartialEq, Eq, Default)]
pub struct DeviceList {
    pub inputs: Vec<Device>,
    pub outputs: Vec<Device>,
    pub default_input: Option<Device>,
    pub default_output: Option<Device>,
}

/// A running stream; dropping it stops the stream. Not `Send`: cpal's streams are not on every
/// platform, so the capture and playout workers create, own and drop theirs on their own thread.
pub trait Running {}

/// Receives 48 kHz mono samples as the device delivers them (any chunk size).
pub type InputSink = Box<dyn FnMut(&[i16]) + Send>;
/// Fills the buffer with the 48 kHz mono samples the device plays next.
pub type OutputSource = Box<dyn FnMut(&mut [i16]) + Send>;
/// Called once when a stream's device is gone or the stream failed.
pub type OnLost = Box<dyn FnOnce() + Send>;

/// The platform's audio devices.
pub trait AudioHost: Send + Sync + 'static {
    fn list(&self) -> DeviceList;
    fn open_input(
        &self,
        device: Option<&DeviceId>,
        sink: InputSink,
        on_lost: OnLost,
    ) -> Result<Box<dyn Running>, MediaError>;
    fn open_output(
        &self,
        device: Option<&DeviceId>,
        source: OutputSource,
        on_lost: OnLost,
    ) -> Result<Box<dyn Running>, MediaError>;
}

/// The system's audio host (cpal's default host).
pub fn system() -> Arc<dyn AudioHost> {
    Arc::new(CpalHost)
}

/// The microphones. Empty on a machine without any (a headless CI runner).
pub fn inputs() -> Result<Vec<Device>, MediaError> {
    Ok(system().list().inputs)
}

/// The speakers and headsets.
pub fn outputs() -> Result<Vec<Device>, MediaError> {
    Ok(system().list().outputs)
}

pub fn default_input() -> Option<Device> {
    system().list().default_input
}

pub fn default_output() -> Option<Device> {
    system().list().default_output
}

/// Sends the current [`DeviceList`] at once and again whenever it changes (a headset plugged in or
/// pulled out, a new default). The poller ends when the receiver is dropped.
pub fn watch() -> mpsc::Receiver<DeviceList> {
    watch_host(system(), Duration::from_secs(1))
}

pub fn watch_host(host: Arc<dyn AudioHost>, every: Duration) -> mpsc::Receiver<DeviceList> {
    let (tx, rx) = mpsc::channel();
    let spawned = std::thread::Builder::new()
        .name("dilla-device-watch".into())
        .spawn(move || {
            let mut last: Option<DeviceList> = None;
            loop {
                let now = host.list();
                if last.as_ref() != Some(&now) {
                    if tx.send(now.clone()).is_err() {
                        return;
                    }
                    last = Some(now);
                }
                std::thread::sleep(every);
            }
        });
    if let Err(e) = spawned {
        log::warn!("dilla-media: device watcher not started: {e}");
    }
    rx
}

// ---- cpal ---------------------------------------------------------------------------------------

struct CpalHost;

fn audio_err(e: impl std::fmt::Display) -> MediaError {
    MediaError::Audio(e.to_string())
}

fn describe(d: &cpal::Device) -> Option<Device> {
    let id = d.id().ok()?.to_string();
    let name = d
        .description()
        .map(|x| x.name().to_owned())
        .unwrap_or_else(|_| id.clone());
    Some(Device { id, name })
}

fn find(host: &cpal::Host, id: &DeviceId) -> Option<cpal::Device> {
    let id: cpal::DeviceId = id.as_str().parse().ok()?;
    host.device_by_id(&id)
}

/// A failure that means "this device is gone": a route change keeps the stream alive.
fn lost(e: &cpal::Error) -> bool {
    e.kind() != cpal::ErrorKind::DeviceChanged
}

/// A streaming linear resampler (mono). Voice-grade; the common case (48 kHz devices) bypasses it.
struct Linear {
    step: f64,
    pos: f64,
    last: f32,
}

impl Linear {
    fn new(from: u32, to: u32) -> Self {
        Self {
            step: f64::from(from) / f64::from(to),
            pos: 0.0,
            last: 0.0,
        }
    }

    fn run(&mut self, input: &[f32], out: &mut Vec<f32>) {
        let mut buf = Vec::with_capacity(input.len() + 1);
        buf.push(self.last);
        buf.extend_from_slice(input);
        loop {
            let i = self.pos as usize;
            if i + 1 >= buf.len() {
                break;
            }
            let frac = (self.pos - i as f64) as f32;
            out.push(buf[i] + (buf[i + 1] - buf[i]) * frac);
            self.pos += self.step;
        }
        self.pos -= (buf.len() - 1) as f64;
        self.last = buf[buf.len() - 1];
    }
}

fn to_i16(x: f32) -> i16 {
    (x.clamp(-1.0, 1.0) * 32767.0) as i16
}

struct Stream(#[allow(dead_code)] cpal::Stream);
impl Running for Stream {}

/// Wraps the one-shot "lost" callback as a cpal error callback.
fn on_error(on_lost: OnLost) -> impl FnMut(cpal::Error) + Send + 'static {
    let mut on_lost = Some(on_lost);
    move |e| {
        if lost(&e)
            && let Some(f) = on_lost.take()
        {
            f();
        }
    }
}

impl AudioHost for CpalHost {
    fn list(&self) -> DeviceList {
        let host = cpal::default_host();
        DeviceList {
            inputs: host
                .input_devices()
                .map(|d| d.filter_map(|d| describe(&d)).collect())
                .unwrap_or_default(),
            outputs: host
                .output_devices()
                .map(|d| d.filter_map(|d| describe(&d)).collect())
                .unwrap_or_default(),
            default_input: host.default_input_device().and_then(|d| describe(&d)),
            default_output: host.default_output_device().and_then(|d| describe(&d)),
        }
    }

    fn open_input(
        &self,
        device: Option<&DeviceId>,
        mut sink: InputSink,
        on_lost: OnLost,
    ) -> Result<Box<dyn Running>, MediaError> {
        let host = cpal::default_host();
        let dev = match device {
            Some(id) => find(&host, id).ok_or_else(|| audio_err("microphone not found"))?,
            None => host
                .default_input_device()
                .ok_or_else(|| audio_err("no microphone"))?,
        };
        let supported = dev.default_input_config().map_err(audio_err)?;
        let config = supported.config();
        let channels = usize::from(config.channels.max(1));
        let mut resampler =
            (config.sample_rate != RATE).then(|| Linear::new(config.sample_rate, RATE));
        let mut mono: Vec<f32> = Vec::new();
        let mut resampled: Vec<f32> = Vec::new();
        let mut out: Vec<i16> = Vec::new();
        let mut deliver = move |samples: &mut dyn Iterator<Item = f32>| {
            mono.clear();
            let (mut acc, mut n) = (0.0f32, 0usize);
            for s in samples {
                acc += s;
                n += 1;
                if n == channels {
                    mono.push(acc / channels as f32);
                    (acc, n) = (0.0, 0);
                }
            }
            out.clear();
            match resampler.as_mut() {
                Some(r) => {
                    resampled.clear();
                    r.run(&mono, &mut resampled);
                    out.extend(resampled.iter().map(|s| to_i16(*s)));
                }
                None => out.extend(mono.iter().map(|s| to_i16(*s))),
            }
            if !out.is_empty() {
                sink(&out);
            }
        };
        let stream = match supported.sample_format() {
            cpal::SampleFormat::F32 => dev.build_input_stream::<f32, _, _>(
                config,
                move |data: &[f32], _| deliver(&mut data.iter().copied()),
                on_error(on_lost),
                None,
            ),
            cpal::SampleFormat::I16 => dev.build_input_stream::<i16, _, _>(
                config,
                move |data: &[i16], _| deliver(&mut data.iter().map(|s| f32::from(*s) / 32768.0)),
                on_error(on_lost),
                None,
            ),
            other => return Err(audio_err(format!("microphone sample format {other:?}"))),
        }
        .map_err(audio_err)?;
        stream.play().map_err(audio_err)?;
        Ok(Box::new(Stream(stream)))
    }

    fn open_output(
        &self,
        device: Option<&DeviceId>,
        mut source: OutputSource,
        on_lost: OnLost,
    ) -> Result<Box<dyn Running>, MediaError> {
        let host = cpal::default_host();
        let dev = match device {
            Some(id) => find(&host, id).ok_or_else(|| audio_err("speaker not found"))?,
            None => host
                .default_output_device()
                .ok_or_else(|| audio_err("no speaker"))?,
        };
        let supported = dev.default_output_config().map_err(audio_err)?;
        let config = supported.config();
        let channels = usize::from(config.channels.max(1));
        let mut resampler =
            (config.sample_rate != RATE).then(|| Linear::new(RATE, config.sample_rate));
        let mut pending: VecDeque<f32> = VecDeque::new();
        let mut chunk = vec![0i16; FRAME_SAMPLES];
        let mut tmp: Vec<f32> = Vec::new();
        let mut resampled: Vec<f32> = Vec::new();
        // Makes sure `pending` holds `frames` device-rate mono samples.
        let mut produce = move |frames: usize, pending: &mut VecDeque<f32>| {
            while pending.len() < frames {
                source(&mut chunk);
                tmp.clear();
                tmp.extend(chunk.iter().map(|s| f32::from(*s) / 32768.0));
                match resampler.as_mut() {
                    Some(r) => {
                        resampled.clear();
                        r.run(&tmp, &mut resampled);
                        pending.extend(resampled.iter().copied());
                    }
                    None => pending.extend(tmp.iter().copied()),
                }
            }
        };
        let stream = match supported.sample_format() {
            cpal::SampleFormat::F32 => dev.build_output_stream::<f32, _, _>(
                config,
                move |data: &mut [f32], _| {
                    produce(data.len() / channels, &mut pending);
                    for frame in data.chunks_mut(channels) {
                        frame.fill(pending.pop_front().unwrap_or(0.0));
                    }
                },
                on_error(on_lost),
                None,
            ),
            cpal::SampleFormat::I16 => dev.build_output_stream::<i16, _, _>(
                config,
                move |data: &mut [i16], _| {
                    produce(data.len() / channels, &mut pending);
                    for frame in data.chunks_mut(channels) {
                        frame.fill(to_i16(pending.pop_front().unwrap_or(0.0)));
                    }
                },
                on_error(on_lost),
                None,
            ),
            other => return Err(audio_err(format!("speaker sample format {other:?}"))),
        }
        .map_err(audio_err)?;
        stream.play().map_err(audio_err)?;
        Ok(Box::new(Stream(stream)))
    }
}
