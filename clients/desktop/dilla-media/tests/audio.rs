//! Device capture and playout on a fake audio host: echo cancellation, enhanced noise on the device
//! microphone, the latency budget, the visible fallback and the hot-unplug. No sound card is
//! needed; the real cpal host is only listed (it may be empty).
//!
//! The thresholds are the web client's (e2e/voice/enhanced-*.spec.ts): 10 dB of noise, 80 ms from
//! capture to the sender, a visible fallback when the model is missing.

use std::f64::consts::TAU;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use dilla_media::MediaError;
use dilla_media::MediaEvent;
use dilla_media::apm::Apm;
use dilla_media::capture::{CaptureConfig, Mic};
use dilla_media::devices::{
    AudioHost, Device, DeviceId, DeviceList, InputSink, OnLost, OutputSource, Running, watch_host,
};
use dilla_media::noise::{FRAME_SAMPLES, ModelLoader, NoiseMode, RATE, embedded_model};
use dilla_media::playout::Speaker;
use tokio::sync::mpsc::{Receiver, UnboundedReceiver};

// ---- a fake host ---------------------------------------------------------------------------------

struct InputSlot {
    id: u64,
    device: String,
    sink: Arc<Mutex<InputSink>>,
    lost: Option<OnLost>,
}

struct OutputSlot {
    id: u64,
    source: Arc<Mutex<OutputSource>>,
}

struct State {
    inputs: Vec<&'static str>,
    default_input: &'static str,
    input: Option<InputSlot>,
    output: Option<OutputSlot>,
    next: u64,
}

#[derive(Clone)]
struct Fake(Arc<Mutex<State>>);

struct Token {
    state: Arc<Mutex<State>>,
    id: u64,
}

impl Running for Token {}

impl Drop for Token {
    fn drop(&mut self) {
        let mut s = self.state.lock().unwrap();
        if s.input.as_ref().is_some_and(|i| i.id == self.id) {
            s.input = None;
        }
        if s.output.as_ref().is_some_and(|o| o.id == self.id) {
            s.output = None;
        }
    }
}

fn dev(id: &str) -> Device {
    Device {
        id: id.into(),
        name: id.into(),
    }
}

impl Fake {
    fn new() -> Fake {
        Fake(Arc::new(Mutex::new(State {
            inputs: vec!["usb", "builtin"],
            default_input: "builtin",
            input: None,
            output: None,
            next: 1,
        })))
    }

    /// Delivers microphone samples to whoever is capturing.
    fn push(&self, samples: &[i16]) {
        let sink = self
            .0
            .lock()
            .unwrap()
            .input
            .as_ref()
            .map(|i| i.sink.clone());
        if let Some(sink) = sink {
            (sink.lock().unwrap())(samples);
        }
    }

    /// What the speaker plays next.
    fn pull(&self, n: usize) -> Vec<i16> {
        let source = self
            .0
            .lock()
            .unwrap()
            .output
            .as_ref()
            .map(|o| o.source.clone());
        let mut out = vec![0i16; n];
        if let Some(source) = source {
            (source.lock().unwrap())(&mut out);
        }
        out
    }

    fn capturing_from(&self) -> Option<String> {
        self.0
            .lock()
            .unwrap()
            .input
            .as_ref()
            .map(|i| i.device.clone())
    }

    /// Pulls the device out: it leaves the list and a stream on it fails.
    fn unplug(&self, id: &str) {
        let lost = {
            let mut s = self.0.lock().unwrap();
            s.inputs.retain(|d| *d != id);
            match s.input.as_mut() {
                Some(i) if i.device == id => i.lost.take(),
                _ => None,
            }
        };
        if let Some(f) = lost {
            f();
        }
    }
}

impl AudioHost for Fake {
    fn list(&self) -> DeviceList {
        let s = self.0.lock().unwrap();
        DeviceList {
            inputs: s.inputs.iter().map(|d| dev(d)).collect(),
            outputs: vec![dev("speakers")],
            default_input: Some(dev(s.default_input)),
            default_output: Some(dev("speakers")),
        }
    }

    fn open_input(
        &self,
        device: Option<&DeviceId>,
        sink: InputSink,
        on_lost: OnLost,
    ) -> Result<Box<dyn Running>, MediaError> {
        let mut s = self.0.lock().unwrap();
        let name = device.map_or(s.default_input, |d| {
            s.inputs
                .iter()
                .copied()
                .find(|i| *i == d.as_str())
                .unwrap_or("")
        });
        if name.is_empty() {
            return Err(MediaError::Audio("microphone not found".into()));
        }
        let id = s.next;
        s.next += 1;
        s.input = Some(InputSlot {
            id,
            device: name.into(),
            sink: Arc::new(Mutex::new(sink)),
            lost: Some(on_lost),
        });
        Ok(Box::new(Token {
            state: self.0.clone(),
            id,
        }))
    }

    fn open_output(
        &self,
        _device: Option<&DeviceId>,
        source: OutputSource,
        _on_lost: OnLost,
    ) -> Result<Box<dyn Running>, MediaError> {
        let mut s = self.0.lock().unwrap();
        let id = s.next;
        s.next += 1;
        s.output = Some(OutputSlot {
            id,
            source: Arc::new(Mutex::new(source)),
        });
        Ok(Box::new(Token {
            state: self.0.clone(),
            id,
        }))
    }
}

// ---- helpers -------------------------------------------------------------------------------------

fn config(host: &Fake, apm: Apm, model: ModelLoader) -> CaptureConfig {
    CaptureConfig {
        host: Arc::new(host.clone()),
        apm,
        model,
    }
}

fn embedded() -> ModelLoader {
    Arc::new(embedded_model)
}

fn rms(x: &[i16]) -> f64 {
    (x.iter().map(|s| f64::from(*s).powi(2)).sum::<f64>() / x.len().max(1) as f64).sqrt()
}

fn dbfs(x: &[i16]) -> f64 {
    20.0 * (rms(x) / 32768.0).max(1e-9).log10()
}

/// Deterministic white noise at `rms_level` (a xorshift: the same samples on every run).
struct Noise(u64);

impl Noise {
    fn frame(&mut self, rms_level: f64) -> Vec<i16> {
        (0..FRAME_SAMPLES)
            .map(|_| {
                self.0 ^= self.0 << 13;
                self.0 ^= self.0 >> 7;
                self.0 ^= self.0 << 17;
                let u = (self.0 >> 11) as f64 / (1u64 << 53) as f64 - 0.5;
                // Uniform on [-0.5, 0.5) has rms 1/sqrt(12).
                (u * 12f64.sqrt() * rms_level) as i16
            })
            .collect()
    }
}

/// Drains every frame the microphone has produced so far.
fn drain(rx: &mut Receiver<Vec<i16>>) -> Vec<i16> {
    let mut out = Vec::new();
    while let Ok(f) = rx.try_recv() {
        out.extend(f);
    }
    out
}

/// Waits (up to 5 s) until `n` samples have come out; returns them.
fn collect(rx: &mut Receiver<Vec<i16>>, n: usize) -> Vec<i16> {
    let deadline = Instant::now() + Duration::from_secs(60);
    let mut out = Vec::new();
    while out.len() < n && Instant::now() < deadline {
        match rx.try_recv() {
            Ok(f) => out.extend(f),
            Err(_) => std::thread::sleep(Duration::from_millis(2)),
        }
    }
    out
}

/// Feeds `frames` of noise at the microphone, as fast as the chain takes them, and returns the
/// samples that came out after `skip` (the model's warm-up).
fn run_noise(mode: NoiseMode, frames: usize, skip: usize) -> Vec<i16> {
    let host = Fake::new();
    let mut mic = Mic::open_with(config(&host, Apm::default(), embedded()), None, mode).unwrap();
    let mut rx = mic.take_frames().unwrap();
    let mut noise = Noise(0x9E37_79B9_7F4A_7C15);
    let mut out = Vec::new();
    for _ in 0..frames {
        // Pace at 2x real time so the worker keeps up (its backlog is bounded).
        host.push(&noise.frame(1000.0));
        std::thread::sleep(Duration::from_millis(5));
        out.extend(drain(&mut rx));
    }
    out.extend(collect(&mut rx, 0));
    std::thread::sleep(Duration::from_millis(200));
    out.extend(drain(&mut rx));
    out.split_off(skip.min(out.len()))
}

fn events(rx: &mut UnboundedReceiver<MediaEvent>) -> Vec<MediaEvent> {
    let mut v = Vec::new();
    while let Ok(e) = rx.try_recv() {
        v.push(e);
    }
    v
}

// ---- tests ---------------------------------------------------------------------------------------

#[test]
fn aec_removes_playout_echo() {
    let host = Fake::new();
    let apm = Apm::default();
    let speaker = Speaker::open_with(Arc::new(host.clone()), apm.clone(), None, None).unwrap();
    let mut mic = Mic::open_with(config(&host, apm, embedded()), None, NoiseMode::Off).unwrap();
    let mut rx = mic.take_frames().unwrap();

    // Far-end speech stands in as broadband noise at -15 dBFS (rms 5800): a pure tone defeats
    // libwebrtc's AEC3 by design (its tone detector freezes adaptation and a periodic signal has
    // no unique delay; see the report), so the browser's own canceller would not cancel one
    // either. The "room" returns what the speaker played 40 ms later (four frames) into the
    // microphone.
    let sink = speaker.sink();
    let far_rms = 5800.0;
    let echo_dbfs = 20.0 * (far_rms / 32768.0f64).log10();
    let stop = Arc::new(AtomicBool::new(false));
    let driver = {
        let (host, stop) = (host.clone(), stop.clone());
        std::thread::spawn(move || {
            let mut line: std::collections::VecDeque<Vec<i16>> =
                std::iter::repeat_n(vec![0i16; FRAME_SAMPLES], 4).collect();
            let mut tick = Instant::now();
            let mut far = Noise(99);
            while !stop.load(Ordering::SeqCst) {
                sink.push("far", &far.frame(far_rms));
                line.push_back(host.pull(FRAME_SAMPLES));
                host.push(&line.pop_front().unwrap());
                tick += Duration::from_millis(10);
                std::thread::sleep(tick.saturating_duration_since(Instant::now()));
            }
        })
    };

    // Let the canceller converge, then measure the last second.
    let all = collect(&mut rx, 8 * RATE as usize);
    stop.store(true, Ordering::SeqCst);
    driver.join().unwrap();
    assert!(all.len() >= 8 * RATE as usize, "the microphone stalled");
    let tail = &all[all.len() - RATE as usize..];
    let after = dbfs(tail);
    println!("aec: echo {echo_dbfs:.1} dBFS -> {after:.1} dBFS");
    assert!(
        after <= echo_dbfs - 25.0 && after < -30.0,
        "echo {echo_dbfs:.1} dBFS -> {after:.1} dBFS after AEC (need 25 dB lower and below -30)"
    );
}

#[test]
fn enhanced_lowers_noise_10db_on_device_path() {
    // 6 s of noise at -30 dBFS (rms 1000); the first 2 s are the model's and the gain control's
    // warm-up.
    let frames = 600;
    let skip = 200 * FRAME_SAMPLES;
    let off = run_noise(NoiseMode::Off, frames, skip);
    let enhanced = run_noise(NoiseMode::Enhanced, frames, skip);
    assert!(off.len() > 48_000 && enhanced.len() > 48_000);
    let (off_db, enh_db) = (dbfs(&off), dbfs(&enhanced));
    println!("noise: off {off_db:.1} dBFS, enhanced {enh_db:.1} dBFS");
    assert!(
        off_db - enh_db >= 10.0,
        "noise {off_db:.1} dBFS off, {enh_db:.1} dBFS enhanced: only {:.1} dB lower",
        off_db - enh_db
    );
}

#[test]
fn enhanced_within_80ms_budget() {
    // A voiced burst (a 150 Hz pulse train through a 3 kHz low-pass: speech-like energy) after a
    // second of silence, at real-time pace. The delay is the time from the burst's first sample
    // entering the device callback to the first output frame in which the burst is audible,
    // measured by the wall clock plus the frame's own offset.
    let host = Fake::new();
    let mut mic = Mic::open_with(
        config(&host, Apm::default(), embedded()),
        None,
        NoiseMode::Enhanced,
    )
    .unwrap();
    let mut rx = mic.take_frames().unwrap();
    let silence = vec![0i16; FRAME_SAMPLES];
    let burst: Vec<Vec<i16>> = (0..30)
        .map(|k| {
            (0..FRAME_SAMPLES)
                .map(|i| {
                    let t = (k * FRAME_SAMPLES + i) as f64 / f64::from(RATE);
                    let voiced: f64 = (1..=12)
                        .map(|h| (TAU * 150.0 * f64::from(h) * t).sin() / f64::from(h))
                        .sum();
                    (voiced * 0.12 * f64::from(i16::MAX)) as i16
                })
                .collect()
        })
        .collect();
    let started = Instant::now();
    let mut tick = started;
    let mut sent_at = None;
    let mut heard_at = None;
    for k in 0..(100 + 30 + 40) {
        let frame = if (100..130).contains(&k) {
            &burst[k - 100]
        } else {
            &silence
        };
        if k == 100 {
            sent_at = Some(Instant::now());
        }
        host.push(frame);
        tick += Duration::from_millis(10);
        std::thread::sleep(tick.saturating_duration_since(Instant::now()));
        while let Ok(f) = rx.try_recv() {
            if heard_at.is_none() && sent_at.is_some() && dbfs(&f) > -50.0 {
                heard_at = Some(Instant::now());
            }
        }
    }
    let delay = heard_at.expect("the burst never came out") - sent_at.unwrap();
    // The burst's first frame is delivered at the start of its 10 ms slot and is audible at the
    // earliest when that slot has been captured; the budget is the added delay beyond that.
    let added = delay.saturating_sub(Duration::from_millis(10));
    println!("latency: {delay:?} from the burst to its first audible frame, {added:?} added");
    assert!(
        added <= Duration::from_millis(80),
        "capture to sender took {added:?} beyond the 10 ms frame (budget 80 ms)"
    );
}

#[test]
fn missing_model_falls_back_visibly() {
    let loads = Arc::new(AtomicUsize::new(0));
    let counting = |ok: bool| -> ModelLoader {
        let loads = loads.clone();
        Arc::new(move || {
            loads.fetch_add(1, Ordering::SeqCst);
            if ok {
                embedded_model()
            } else {
                Err(MediaError::Audio("no weights".into()))
            }
        })
    };

    // Enhanced without weights: Standard runs and the event says so.
    let host = Fake::new();
    let mut mic = Mic::open_with(
        config(&host, Apm::default(), counting(false)),
        None,
        NoiseMode::Enhanced,
    )
    .unwrap();
    assert_eq!(mic.mode(), NoiseMode::Standard);
    let mut ev = mic.take_events().unwrap();
    assert_eq!(events(&mut ev), vec![MediaEvent::EnhancedUnavailable]);
    // It still captures.
    let mut rx = mic.take_frames().unwrap();
    host.push(&Noise(7).frame(500.0));
    assert_eq!(collect(&mut rx, FRAME_SAMPLES).len(), FRAME_SAMPLES);
    drop(mic);

    // Off loads no model; neither does Standard.
    assert_eq!(loads.load(Ordering::SeqCst), 1);
    for mode in [NoiseMode::Off, NoiseMode::Standard] {
        let mic =
            Mic::open_with(config(&host, Apm::default(), counting(true)), None, mode).unwrap();
        assert_eq!(mic.mode(), mode);
    }
    assert_eq!(
        loads.load(Ordering::SeqCst),
        1,
        "Off and Standard load no model"
    );

    // With the weights, Enhanced runs as asked and sends nothing.
    let mut mic = Mic::open_with(
        config(&host, Apm::default(), counting(true)),
        None,
        NoiseMode::Enhanced,
    )
    .unwrap();
    assert_eq!(mic.mode(), NoiseMode::Enhanced);
    assert!(events(&mut mic.take_events().unwrap()).is_empty());
}

#[test]
fn device_hot_unplug_switches_to_default() {
    let host = Fake::new();
    let mut mic = Mic::open_with(
        config(&host, Apm::default(), embedded()),
        Some(DeviceId("usb".into())),
        NoiseMode::Off,
    )
    .unwrap();
    let mut rx = mic.take_frames().unwrap();
    let mut ev = mic.take_events().unwrap();
    assert_eq!(host.capturing_from().as_deref(), Some("usb"));
    host.push(&Noise(1).frame(800.0));
    assert_eq!(collect(&mut rx, FRAME_SAMPLES).len(), FRAME_SAMPLES);

    host.unplug("usb");
    let deadline = Instant::now() + Duration::from_secs(5);
    while host.capturing_from().as_deref() != Some("builtin") && Instant::now() < deadline {
        std::thread::sleep(Duration::from_millis(5));
    }
    assert_eq!(host.capturing_from().as_deref(), Some("builtin"));
    // Capture continues on the default.
    host.push(&Noise(2).frame(800.0));
    assert_eq!(collect(&mut rx, FRAME_SAMPLES).len(), FRAME_SAMPLES);
    std::thread::sleep(Duration::from_millis(100));
    assert_eq!(events(&mut ev), vec![MediaEvent::DevicesChanged]);

    // A device that was never there at open: the default, one event as well.
    let host = Fake::new();
    let mut mic = Mic::open_with(
        config(&host, Apm::default(), embedded()),
        Some(DeviceId("gone".into())),
        NoiseMode::Off,
    )
    .unwrap();
    assert_eq!(host.capturing_from().as_deref(), Some("builtin"));
    assert_eq!(
        events(&mut mic.take_events().unwrap()),
        vec![MediaEvent::DevicesChanged]
    );
}

#[test]
fn watch_reports_the_list_and_its_changes() {
    let host = Fake::new();
    let rx = watch_host(Arc::new(host.clone()), Duration::from_millis(20));
    let first = rx.recv_timeout(Duration::from_secs(2)).unwrap();
    assert_eq!(first.inputs.len(), 2);
    assert_eq!(first.default_input.unwrap().id, "builtin");
    host.unplug("usb");
    let next = rx.recv_timeout(Duration::from_secs(2)).unwrap();
    assert_eq!(next.inputs.len(), 1);
}

#[test]
fn the_system_host_lists_without_opening() {
    // A headless runner has no devices; the listing must still answer.
    let _ = dilla_media::devices::inputs().unwrap();
    let _ = dilla_media::devices::outputs().unwrap();
    let _ = dilla_media::devices::default_input();
    let _ = dilla_media::devices::default_output();
}
