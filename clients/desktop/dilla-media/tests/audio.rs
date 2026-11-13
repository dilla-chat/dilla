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
use dilla_media::apm::{Apm, ApmFlags};
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
    lost: Option<OnLost>,
}

struct State {
    inputs: Vec<&'static str>,
    default_input: &'static str,
    input: Option<InputSlot>,
    output: Option<OutputSlot>,
    next: u64,
    in_latency: u32,
    out_latency: u32,
    /// No output can be opened (every speaker is gone).
    no_output: bool,
}

#[derive(Clone)]
struct Fake(Arc<Mutex<State>>);

struct Token {
    state: Arc<Mutex<State>>,
    id: u64,
    input: bool,
}

impl Running for Token {
    fn latency_ms(&self) -> u32 {
        let s = self.state.lock().unwrap();
        if self.input {
            s.in_latency
        } else {
            s.out_latency
        }
    }
}

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
            in_latency: 0,
            out_latency: 0,
            no_output: false,
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

    fn set_latency(&self, input: u32, output: u32) {
        let mut s = self.0.lock().unwrap();
        s.in_latency = input;
        s.out_latency = output;
    }

    /// The machine has no default microphone any more.
    fn remove_default_input(&self) {
        self.0.lock().unwrap().default_input = "";
    }

    fn capturing_from(&self) -> Option<String> {
        self.0
            .lock()
            .unwrap()
            .input
            .as_ref()
            .map(|i| i.device.clone())
    }

    /// Pulls the speaker out; with `none_left` there is no default to take over either.
    fn unplug_output(&self, none_left: bool) {
        let lost = {
            let mut s = self.0.lock().unwrap();
            s.no_output = none_left;
            s.output.as_mut().and_then(|o| o.lost.take())
        };
        if let Some(f) = lost {
            f();
        }
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
            input: true,
        }))
    }

    fn open_output(
        &self,
        _device: Option<&DeviceId>,
        source: OutputSource,
        on_lost: OnLost,
    ) -> Result<Box<dyn Running>, MediaError> {
        let mut s = self.0.lock().unwrap();
        if s.no_output {
            return Err(MediaError::Audio("speaker not found".into()));
        }
        let id = s.next;
        s.next += 1;
        s.output = Some(OutputSlot {
            id,
            source: Arc::new(Mutex::new(source)),
            lost: Some(on_lost),
        });
        Ok(Box::new(Token {
            state: self.0.clone(),
            id,
            input: false,
        }))
    }
}

// ---- helpers -------------------------------------------------------------------------------------

fn config(host: &Fake, apm: Apm, model: ModelLoader) -> CaptureConfig {
    CaptureConfig {
        host: Arc::new(host.clone()),
        apm,
        model,
        flags: ApmFlags::default(),
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

/// The playout echo that reaches the microphone's output after 8 s, in dBFS, with these echo and gain switches.
fn echo_after(flags: ApmFlags) -> (f64, f64) {
    let host = Fake::new();
    let apm = Apm::default();
    let speaker = Speaker::open_with(Arc::new(host.clone()), apm.clone(), None, None).unwrap();
    let mut cfg = config(&host, apm, embedded());
    cfg.flags = flags;
    let mut mic = Mic::open_with(cfg, None, NoiseMode::Off).unwrap();
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
    (echo_dbfs, dbfs(tail))
}

#[test]
fn aec_removes_playout_echo() {
    let (echo_dbfs, after) = echo_after(ApmFlags::default());
    println!("aec: echo {echo_dbfs:.1} dBFS -> {after:.1} dBFS");
    assert!(
        after <= echo_dbfs - 25.0 && after < -30.0,
        "echo {echo_dbfs:.1} dBFS -> {after:.1} dBFS after AEC (need 25 dB lower and below -30)"
    );
}

/// R19: with echo cancellation switched off the playout echo stays in the microphone, as the web's switch leaves it.
#[test]
fn echo_off_leaves_the_playout_echo_in() {
    let (echo_dbfs, after) = echo_after(ApmFlags {
        echo: false,
        ..ApmFlags::default()
    });
    println!("echo off: echo {echo_dbfs:.1} dBFS -> {after:.1} dBFS");
    assert!(
        after > echo_dbfs - 6.0,
        "echo {echo_dbfs:.1} dBFS -> {after:.1} dBFS with the canceller off (it must stay within 6 dB)"
    );
    assert_eq!(
        ApmFlags::default(),
        ApmFlags {
            echo: true,
            agc: true
        }
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

/// Pushes `input` frame by frame (faster than real time) and returns every output frame.
fn run_frames(mode: NoiseMode, input: &[Vec<i16>]) -> Vec<Vec<i16>> {
    let host = Fake::new();
    let mut mic = Mic::open_with(config(&host, Apm::default(), embedded()), None, mode).unwrap();
    let mut rx = mic.take_frames().unwrap();
    let mut out = Vec::new();
    for f in input {
        host.push(f);
        std::thread::sleep(Duration::from_millis(5));
        while let Ok(o) = rx.try_recv() {
            out.push(o);
        }
    }
    let deadline = Instant::now() + Duration::from_secs(10);
    while out.len() < input.len() && Instant::now() < deadline {
        match rx.try_recv() {
            Ok(o) => out.push(o),
            Err(_) => std::thread::sleep(Duration::from_millis(5)),
        }
    }
    out
}

/// A voiced, speech-like frame: 150 Hz harmonics with a 4 Hz syllable envelope.
fn speech_frame(k: usize, level: f64) -> Vec<i16> {
    (0..FRAME_SAMPLES)
        .map(|i| {
            let t = (k * FRAME_SAMPLES + i) as f64 / f64::from(RATE);
            let voiced: f64 = (1..=12)
                .map(|h| (TAU * 150.0 * f64::from(h) * t).sin() / f64::from(h))
                .sum();
            let envelope = 0.6 + 0.4 * (TAU * 4.0 * t).sin();
            (voiced * envelope * level) as i16
        })
        .collect()
}

#[test]
fn enhanced_keeps_speech_while_noise_drops() {
    // One-second segments alternate noise only / speech plus noise (noise at -30 dBFS rms, speech
    // about -20 dBFS rms). Levels are measured inside each segment, away from its edges and from
    // the first two segments (warm-up of the model and the gain control).
    let mut noise = Noise(0xD1CE_5EED);
    let input: Vec<Vec<i16>> = (0..800)
        .map(|k| {
            let mut f = noise.frame(1000.0);
            if (k / 100) % 2 == 1 {
                for (a, b) in f
                    .iter_mut()
                    .zip(speech_frame(k, 0.12 * f64::from(i16::MAX)))
                {
                    *a = a.saturating_add(b);
                }
            }
            f
        })
        .collect();
    let level = |out: &[Vec<i16>], parity: usize| -> f64 {
        let samples: Vec<i16> = (200..800)
            .filter(|k| (k / 100) % 2 == parity && (k % 100) >= 20 && (k % 100) < 80)
            .flat_map(|k| out[k].iter().copied())
            .collect();
        dbfs(&samples)
    };
    let off = run_frames(NoiseMode::Off, &input);
    let enhanced = run_frames(NoiseMode::Enhanced, &input);
    assert!(off.len() == input.len() && enhanced.len() == input.len());
    let (noise_off, noise_enh) = (level(&off, 0), level(&enhanced, 0));
    let (speech_off, speech_enh) = (level(&off, 1), level(&enhanced, 1));
    println!(
        "speech+noise: noise {noise_off:.1} -> {noise_enh:.1} dBFS, speech {speech_off:.1} -> {speech_enh:.1} dBFS"
    );
    assert!(
        noise_off - noise_enh >= 10.0,
        "noise only {:.1} dB lower",
        noise_off - noise_enh
    );
    // Speech keeps within SPEECH_LOSS_DB of what the chain without suppression gives.
    // Measured 4.1 dB on the reference run (the synthetic voice loses some body to the model); 6 dB
    // leaves margin for another CPU without letting a muted-speech regression through.
    const SPEECH_LOSS_DB: f64 = 6.0;
    assert!(
        speech_off - speech_enh <= SPEECH_LOSS_DB,
        "speech lost {:.1} dB (limit {SPEECH_LOSS_DB})",
        speech_off - speech_enh
    );
}

#[test]
fn a_stalled_consumer_never_blocks_the_capture_callback() {
    let host = Fake::new();
    // Stall the worker: switching to Enhanced waits in the model loader.
    let (release_tx, release_rx) = std::sync::mpsc::channel::<()>();
    let release_rx = Mutex::new(release_rx);
    let loader: ModelLoader = Arc::new(move || {
        let _ = release_rx.lock().unwrap().recv();
        embedded_model()
    });
    let mut mic =
        Mic::open_with(config(&host, Apm::default(), loader), None, NoiseMode::Off).unwrap();
    let _frames = mic.take_frames().unwrap();
    let mic = Arc::new(mic);
    let switching = {
        let mic = mic.clone();
        std::thread::spawn(move || mic.switch(None, NoiseMode::Enhanced))
    };
    std::thread::sleep(Duration::from_millis(200));
    // Two seconds of audio into the callback while the worker is stuck.
    let frame = Noise(5).frame(500.0);
    let mut slowest = Duration::ZERO;
    for _ in 0..200 {
        let t = Instant::now();
        host.push(&frame);
        slowest = slowest.max(t.elapsed());
    }
    println!("stalled worker: slowest callback {slowest:?}");
    assert!(
        slowest < Duration::from_millis(5),
        "a callback took {slowest:?} with the worker stalled"
    );
    release_tx.send(()).unwrap();
    assert_eq!(switching.join().unwrap().unwrap(), NoiseMode::Enhanced);
}

#[test]
fn the_playout_callback_never_waits_for_a_pusher() {
    let host = Fake::new();
    let speaker = Speaker::open_with(Arc::new(host.clone()), Apm::default(), None, None).unwrap();
    let sink = speaker.sink();
    let stop = Arc::new(AtomicBool::new(false));
    // Several remote sources push as fast as they can while the callback runs.
    let pushers: Vec<_> = (0..4)
        .map(|n| {
            let (sink, stop) = (sink.clone(), stop.clone());
            std::thread::spawn(move || {
                let burst = vec![1000i16; 4800];
                while !stop.load(Ordering::SeqCst) {
                    sink.push(&format!("src{n}"), &burst);
                }
            })
        })
        .collect();
    let mut slowest = Duration::ZERO;
    for _ in 0..500 {
        let t = Instant::now();
        let out = host.pull(FRAME_SAMPLES);
        slowest = slowest.max(t.elapsed());
        assert_eq!(out.len(), FRAME_SAMPLES);
    }
    stop.store(true, Ordering::SeqCst);
    for p in pushers {
        p.join().unwrap();
    }
    println!("busy pushers: slowest playout callback {slowest:?}");
    assert!(
        slowest < Duration::from_millis(5),
        "a playout callback took {slowest:?}"
    );
}

#[test]
fn dropping_the_watcher_ends_its_thread() {
    let host = Fake::new();
    let arc: Arc<dyn AudioHost> = Arc::new(host.clone());
    let base = Arc::strong_count(&arc);
    // The list never changes after the first message: only the stop signal can end the poller.
    let watcher = watch_host(arc.clone(), Duration::from_secs(60));
    assert!(watcher.recv_timeout(Duration::from_secs(2)).is_ok());
    assert!(Arc::strong_count(&arc) > base, "the poller holds the host");
    drop(watcher);
    let deadline = Instant::now() + Duration::from_secs(2);
    while Arc::strong_count(&arc) > base && Instant::now() < deadline {
        std::thread::sleep(Duration::from_millis(10));
    }
    assert_eq!(
        Arc::strong_count(&arc),
        base,
        "the poller thread is still running"
    );
}

#[test]
fn a_mic_with_no_default_to_fall_back_to_says_so() {
    let host = Fake::new();
    let mut mic = Mic::open_with(
        config(&host, Apm::default(), embedded()),
        Some(DeviceId("usb".into())),
        NoiseMode::Off,
    )
    .unwrap();
    let mut ev = mic.take_events().unwrap();
    host.remove_default_input();
    host.unplug("usb");
    let deadline = Instant::now() + Duration::from_secs(5);
    let mut seen = Vec::new();
    while seen.is_empty() && Instant::now() < deadline {
        std::thread::sleep(Duration::from_millis(10));
        seen = events(&mut ev);
    }
    assert_eq!(seen, vec![MediaEvent::MicLost]);
}

/// Task 4 ledger (Task 17): a speaker pulled out moves to the default (`DevicesChanged`), and one with no default to
/// take over says `SpeakerLost`.
#[test]
fn a_speaker_unplugged_moves_to_the_default_or_says_it_is_lost() {
    let host = Fake::new();
    let (tx, mut ev) = tokio::sync::mpsc::unbounded_channel();
    let speaker =
        Speaker::open_with(Arc::new(host.clone()), Apm::default(), Some(tx), None).unwrap();
    let wait = |ev: &mut UnboundedReceiver<MediaEvent>| {
        let deadline = Instant::now() + Duration::from_secs(5);
        let mut seen = Vec::new();
        while seen.is_empty() && Instant::now() < deadline {
            std::thread::sleep(Duration::from_millis(10));
            seen = events(ev);
        }
        seen
    };
    host.unplug_output(false);
    assert_eq!(wait(&mut ev), vec![MediaEvent::DevicesChanged]);
    // The default took over: what is pushed plays again.
    speaker.sink().push("a", &[1000; 480]);
    assert!(host.pull(480).iter().any(|s| *s != 0));
    host.unplug_output(true);
    assert_eq!(wait(&mut ev), vec![MediaEvent::SpeakerLost]);
}

/// Task 4 ledger (Task 17): the 17th source is refused (and warned about once), a removed source's slot is free again
/// once the callback emptied it, and the first samples of the source that takes it are played, not emptied with the
/// old one's.
#[test]
fn slots_are_freed_on_track_end_and_a_new_source_keeps_its_first_samples() {
    let host = Fake::new();
    let speaker = Speaker::open_with(Arc::new(host.clone()), Apm::default(), None, None).unwrap();
    let sink = speaker.sink();
    for i in 0..16 {
        sink.push(&format!("s{i}"), &[0; 480]);
    }
    assert_eq!(sink.sources(), 16);
    for _ in 0..100 {
        sink.push("s16", &[0; 480]);
    }
    assert_eq!(sink.sources(), 16, "a 17th source is not played");
    sink.remove("s0");
    // Before the callback emptied the freed slot it is not handed out.
    sink.push("late", &[7000; 480]);
    assert_eq!(sink.sources(), 15);
    let _ = host.pull(480);
    sink.push("late", &[7000; 480]);
    assert_eq!(sink.sources(), 16);
    let played = host.pull(480);
    assert!(
        played.iter().filter(|s| **s == 7000).count() > 400,
        "the new source's first samples play"
    );
}

/// Deafen plays silence but keeps draining the sources.
#[test]
fn deafen_plays_silence_and_queues_nothing() {
    let host = Fake::new();
    let speaker = Speaker::open_with(Arc::new(host.clone()), Apm::default(), None, None).unwrap();
    let sink = speaker.sink();
    sink.set_deafened(true);
    sink.push("a", &[5000; 480]);
    assert!(host.pull(480).iter().all(|s| *s == 0));
    sink.set_deafened(false);
    assert!(
        host.pull(480).iter().all(|s| *s == 0),
        "nothing queued meanwhile"
    );
    sink.push("a", &[5000; 480]);
    assert!(host.pull(480).iter().any(|s| *s == 5000));
}

#[test]
fn the_device_latencies_set_the_apm_stream_delay() {
    let host = Fake::new();
    host.set_latency(20, 15);
    let apm = Apm::default();
    let _speaker = Speaker::open_with(Arc::new(host.clone()), apm.clone(), None, None).unwrap();
    let _mic =
        Mic::open_with(config(&host, apm.clone(), embedded()), None, NoiseMode::Off).unwrap();
    // The delay is applied with the first captured frame once both workers have read the latencies.
    let deadline = Instant::now() + Duration::from_secs(5);
    while apm.applied_delay_ms() != Some(35) && Instant::now() < deadline {
        host.push(&vec![0i16; FRAME_SAMPLES]);
        std::thread::sleep(Duration::from_millis(10));
    }
    assert_eq!(apm.applied_delay_ms(), Some(35));
    // A changed latency follows.
    host.set_latency(20, 40);
    let deadline = Instant::now() + Duration::from_secs(5);
    while apm.applied_delay_ms() != Some(60) && Instant::now() < deadline {
        host.push(&vec![0i16; FRAME_SAMPLES]);
        std::thread::sleep(Duration::from_millis(10));
    }
    assert_eq!(apm.applied_delay_ms(), Some(60));
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
