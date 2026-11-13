//! Let's check, the microphone test (packages/web/src/call/microphone-test.ts `LocalMicrophoneTest`, README §5.9): ten
//! seconds in which you hear yourself as the others would, through the chain the call sends (the device microphone with
//! echo cancellation and the chosen noise processor, the input volume and the voice-activity gate), played on your own
//! output at your output volume (never louder than 100 %: the loopback would feed back through speakers).
//!
//! Nothing is recorded or sent: the only path is the speaker's sink, and everything is released when the test ends, on
//! stop and on drop. It runs on dilla-media's `Mic` and `Speaker` through its `AudioHost`, so a test drives it on a
//! fake host.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{self, Receiver, Sender};
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use dilla_client::features::voice_prefs::gate::{
    FRAME_MS, SensitivityConfig, SensitivityGate, db_of,
};
use dilla_client::features::voice_prefs::{InputMode, NoisePreference, VoiceValues};
use dilla_client::state::voice_prefs::{AudioNotice, Processor, ProcessorNote, ProcessorStatus};
use dilla_media::MediaEvent;
use dilla_media::apm::{Apm, ApmFlags};
use dilla_media::capture::{CaptureConfig, Mic};
use dilla_media::devices::{AudioHost, DeviceId};
use dilla_media::noise::{FRAME_SAMPLES, ModelLoader, NoiseMode, embedded_model};
use dilla_media::playout::Speaker;

/// microphone-test.ts `SETUP_TIMEOUT_MS`: how long the microphone and the speaker may take to open.
pub const SETUP_TIMEOUT: Duration = Duration::from_secs(15);
/// The loopback's length (microphone-test.ts `LOOPBACK_MS`).
pub const LOOPBACK: Duration = Duration::from_secs(10);
/// The level is reported every 5 frames (50 ms), the meter's refresh.
const LEVEL_EVERY: u64 = 5;
/// The gate's ramps (sensitivity.ts `attackRampMs` and `releaseRampMs`), in samples at 48 kHz.
const ATTACK_SAMPLES: f32 = 48.0 * 5.0;
const RELEASE_SAMPLES: f32 = 48.0 * 60.0;
const SOURCE: &str = "mic-test";

/// What the test applies, as the call would (microphone-test.ts `LoopbackSettings`).
#[derive(Clone, Debug, PartialEq)]
pub struct TestSettings {
    pub input: Option<String>,
    pub output: Option<String>,
    pub noise: NoisePreference,
    pub echo: bool,
    pub agc: bool,
    /// 0..200 %.
    pub input_volume: u32,
    pub output_volume: u32,
    pub sensitivity_auto: bool,
    pub threshold_db: i32,
    /// The call's input mode: under push to talk the gate is off, as in the call.
    pub mode: InputMode,
}

impl TestSettings {
    pub fn of(v: &VoiceValues) -> Self {
        Self {
            input: v.input.clone(),
            output: v.output.clone(),
            noise: v.noise,
            echo: v.echo,
            agc: v.agc,
            input_volume: v.input_volume,
            output_volume: v.output_volume,
            sensitivity_auto: v.sensitivity_auto,
            threshold_db: v.sensitivity_db,
            mode: v.mode,
        }
    }

    /// Whether `other` runs on the same capture chain: a change of input, output, noise processing or echo and gain ends
    /// the test (the volume, the threshold and the mode apply at once).
    pub fn same_chain(&self, other: &Self) -> bool {
        (&self.input, &self.output, self.noise, self.echo, self.agc)
            == (
                &other.input,
                &other.output,
                other.noise,
                other.echo,
                other.agc,
            )
    }

    fn gate(&self) -> SensitivityConfig {
        SensitivityConfig {
            enabled: self.mode != InputMode::Ptt,
            auto: self.sensitivity_auto,
            threshold_db: self.threshold_db as f32,
        }
    }

    /// The input volume as a gain, 0..2.
    fn gain(&self) -> f32 {
        self.input_volume.min(200) as f32 / 100.0
    }

    /// microphone-test.ts `loopbackGain`: never louder than 100 %.
    pub fn loopback_gain(&self) -> f32 {
        self.output_volume.min(100) as f32 / 100.0
    }
}

/// What the test reports.
#[derive(Clone, Debug, PartialEq)]
pub enum TestEvent {
    /// The microphone and the speaker are open: this processor runs.
    Started(ProcessorStatus),
    /// The processor changed: the chosen one could not run (the enhanced model did not load).
    Processor(ProcessorStatus),
    /// The level after the input volume (the peak of the last 50 ms), the threshold in force and the gate's decision.
    Level {
        db: f32,
        threshold_db: f32,
        open: bool,
    },
    /// The microphone could not be opened.
    Failed,
    /// An audio notice (a device was lost, the enhanced processor failed).
    Notice(AudioNotice),
    /// The test is over (ten seconds, stop, or the microphone ended) and everything is released.
    Ended,
}

/// Receives the events, from the test's thread.
pub type OnEvent = Arc<dyn Fn(TestEvent) + Send + Sync>;

enum Ctl {
    Update(TestSettings),
    Stop,
}

/// A running or ending test. Dropping it stops it.
pub struct MicTest {
    ctl: Sender<Ctl>,
    worker: Option<JoinHandle<()>>,
    over: Arc<AtomicBool>,
}

impl MicTest {
    /// Starts the test on `host`; returns at once, the microphone and the speaker open on the test's own thread (the
    /// enhanced model can take a while to load). `model` is the enhanced model's loader ([`embedded_model`] in the app).
    pub fn start(
        host: Arc<dyn AudioHost>,
        model: ModelLoader,
        settings: TestSettings,
        on_event: OnEvent,
    ) -> MicTest {
        Self::start_for(host, model, settings, on_event, LOOPBACK)
    }

    /// As [`start`](Self::start), ending by itself after `length` instead of the ten seconds.
    pub fn start_for(
        host: Arc<dyn AudioHost>,
        model: ModelLoader,
        settings: TestSettings,
        on_event: OnEvent,
        length: Duration,
    ) -> MicTest {
        let (ctl, rx) = mpsc::channel();
        let over = Arc::new(AtomicBool::new(false));
        let flag = over.clone();
        let worker = std::thread::Builder::new()
            .name("dilla-mic-test".into())
            .spawn(move || {
                run(host, model, settings, &on_event, &rx, length);
                flag.store(true, Ordering::Release);
                on_event(TestEvent::Ended);
            })
            .map_err(|e| log::warn!("dilla: the microphone test did not start: {e}"))
            .ok();
        MicTest { ctl, worker, over }
    }

    /// The system microphone chain with the embedded enhanced model.
    pub fn start_system(settings: TestSettings, on_event: OnEvent) -> MicTest {
        Self::start(
            dilla_media::devices::system(),
            Arc::new(embedded_model),
            settings,
            on_event,
        )
    }

    /// Settings changed during the test: the volume, the threshold and the mode apply at once.
    pub fn update(&self, settings: TestSettings) {
        let _ = self.ctl.send(Ctl::Update(settings));
    }

    /// Whether the test is over and everything released.
    pub fn is_over(&self) -> bool {
        self.over.load(Ordering::Acquire)
    }

    /// Stops the test and waits until the microphone and the speaker are released.
    pub fn stop(mut self) {
        self.shutdown();
    }

    fn shutdown(&mut self) {
        let _ = self.ctl.send(Ctl::Stop);
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
    }
}

impl Drop for MicTest {
    fn drop(&mut self) {
        self.shutdown();
    }
}

fn mode_of(noise: NoisePreference) -> NoiseMode {
    match noise {
        NoisePreference::Automatic => NoiseMode::Enhanced,
        NoisePreference::Lightweight => NoiseMode::Standard,
        NoisePreference::Off => NoiseMode::Off,
    }
}

fn processor_of(mode: NoiseMode) -> Processor {
    match mode {
        NoiseMode::Off => Processor::Off,
        NoiseMode::Standard => Processor::Standard,
        NoiseMode::Enhanced => Processor::Enhanced,
    }
}

/// The gate's output: the input after its ramp, one frame late so the frame that opened the gate is heard whole.
struct Stage {
    gate: SensitivityGate,
    delayed: [f32; FRAME_SAMPLES],
    env: f32,
    target: f32,
    frames: u64,
    peak: f32,
}

impl Stage {
    fn new(settings: &TestSettings) -> Self {
        Self {
            gate: SensitivityGate::new(settings.gate()),
            delayed: [0.0; FRAME_SAMPLES],
            env: 0.0,
            target: 0.0,
            frames: 0,
            peak: -100.0,
        }
    }

    /// One 10 ms frame in, one out (the previous one, through the gate's envelope). `level` is set every 50 ms.
    fn process(
        &mut self,
        input: &[i16],
        settings: &TestSettings,
        level: &mut Option<TestEvent>,
    ) -> [i16; FRAME_SAMPLES] {
        let gain = settings.gain();
        let mut now = [0.0f32; FRAME_SAMPLES];
        let mut sum = 0.0f32;
        for (slot, s) in now.iter_mut().zip(input) {
            *slot = (f32::from(*s) / 32768.0) * gain;
            sum += *slot * *slot;
        }
        let db = db_of((sum / FRAME_SAMPLES as f32).sqrt());
        self.frames += 1;
        let open = self.gate.update(db, self.frames as f64 * FRAME_MS);
        self.target = if open { 1.0 } else { 0.0 };
        self.peak = self.peak.max(db);
        if self.frames.is_multiple_of(LEVEL_EVERY) {
            *level = Some(TestEvent::Level {
                db: self.peak,
                threshold_db: self.gate.threshold(),
                open,
            });
            self.peak = -100.0;
        }
        let out_gain = settings.loopback_gain();
        let mut out = [0i16; FRAME_SAMPLES];
        for (i, slot) in out.iter_mut().enumerate() {
            if self.env < self.target {
                self.env = (self.env + 1.0 / ATTACK_SAMPLES).min(self.target);
            } else if self.env > self.target {
                self.env = (self.env - 1.0 / RELEASE_SAMPLES).max(self.target);
            }
            let sample = self.delayed[i] * self.env * out_gain;
            *slot = (sample.clamp(-1.0, 1.0) * 32767.0).round() as i16;
        }
        self.delayed = now;
        out
    }

    fn configure(&mut self, settings: &TestSettings) {
        self.gate.configure(settings.gate());
    }
}

type Opened = (
    Mic,
    Speaker,
    tokio::sync::mpsc::UnboundedReceiver<MediaEvent>,
);

/// Opens the microphone, then the speaker (one echo reference for both ends, as in a call), looking at `cancel` between the
/// two: a test that was stopped meanwhile opens nothing more.
fn open_chain(
    host: Arc<dyn AudioHost>,
    model: ModelLoader,
    settings: &TestSettings,
    cancel: &AtomicBool,
) -> Result<Opened, ()> {
    let apm = Apm::default();
    let config = CaptureConfig {
        host: host.clone(),
        apm: apm.clone(),
        model,
        flags: ApmFlags {
            echo: settings.echo,
            agc: settings.agc,
        },
    };
    let device = settings.input.clone().map(DeviceId);
    let mic = Mic::open_with(config, device, mode_of(settings.noise)).map_err(|e| {
        log::warn!("dilla: the microphone test could not open the microphone: {e}");
    })?;
    if cancel.load(Ordering::Acquire) {
        return Err(());
    }
    let (events, speaker_rx) = tokio::sync::mpsc::unbounded_channel();
    let speaker = Speaker::open_with(
        host,
        apm,
        Some(events),
        settings.output.clone().map(DeviceId),
    )
    .map_err(|e| log::warn!("dilla: the microphone test could not open the speaker: {e}"))?;
    Ok((mic, speaker, speaker_rx))
}

fn run(
    host: Arc<dyn AudioHost>,
    model: ModelLoader,
    mut settings: TestSettings,
    on_event: &OnEvent,
    rx: &Receiver<Ctl>,
    length: Duration,
) {
    // The devices open on a thread of their own: a device that hangs blocks that thread, never this one, so a stop (or a
    // drop) answers at once and the web's 15 s setup limit holds.
    let cancel = Arc::new(AtomicBool::new(false));
    let (tx, results) = mpsc::channel();
    let spawned = {
        let (cancel, settings) = (cancel.clone(), settings.clone());
        std::thread::Builder::new()
            .name("dilla-mic-test-open".into())
            .spawn(move || {
                // Whatever opened is dropped here when nobody waits for it any more.
                let _ = tx.send(open_chain(host, model, &settings, &cancel));
            })
    };
    if spawned.is_err() {
        on_event(TestEvent::Failed);
        return;
    }
    let started = Instant::now();
    let opened = loop {
        match results.recv_timeout(Duration::from_millis(25)) {
            Ok(opened) => break opened,
            Err(mpsc::RecvTimeoutError::Disconnected) => {
                on_event(TestEvent::Failed);
                return;
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {}
        }
        loop {
            match rx.try_recv() {
                Ok(Ctl::Stop) | Err(mpsc::TryRecvError::Disconnected) => {
                    cancel.store(true, Ordering::Release);
                    return;
                }
                Ok(Ctl::Update(next)) => settings = next,
                Err(mpsc::TryRecvError::Empty) => break,
            }
        }
        if started.elapsed() >= SETUP_TIMEOUT {
            log::warn!("dilla: the microphone test setup timed out");
            cancel.store(true, Ordering::Release);
            on_event(TestEvent::Failed);
            return;
        }
    };
    let Ok((mut mic, speaker, mut speaker_rx)) = opened else {
        on_event(TestEvent::Failed);
        return;
    };
    let (Some(mut frames), Some(mut mic_events)) = (mic.take_frames(), mic.take_events()) else {
        on_event(TestEvent::Failed);
        return;
    };
    let sink = speaker.sink();
    let status = ProcessorStatus {
        effective: processor_of(mic.mode()),
        note: None,
    };
    let mut stage = Stage::new(&settings);
    on_event(TestEvent::Started(status));
    let deadline = Instant::now() + length;
    'test: loop {
        if Instant::now() >= deadline {
            break;
        }
        loop {
            match rx.try_recv() {
                Ok(Ctl::Stop) | Err(mpsc::TryRecvError::Disconnected) => break 'test,
                Ok(Ctl::Update(next)) => {
                    stage.configure(&next);
                    settings = next;
                }
                Err(mpsc::TryRecvError::Empty) => break,
            }
        }
        while let Ok(event) = mic_events.try_recv() {
            match event {
                MediaEvent::EnhancedUnavailable => {
                    let status = ProcessorStatus {
                        effective: processor_of(mic.mode()),
                        note: Some(ProcessorNote::EnhancedUnavailable),
                    };
                    on_event(TestEvent::Processor(status));
                }
                MediaEvent::EnhancedFailed => {
                    on_event(TestEvent::Notice(AudioNotice::EnhancedFailed))
                }
                MediaEvent::MicLost => {
                    on_event(TestEvent::Notice(AudioNotice::MicLost));
                    break 'test;
                }
                MediaEvent::DevicesChanged | MediaEvent::SpeakerLost => {}
            }
        }
        while let Ok(event) = speaker_rx.try_recv() {
            if event == MediaEvent::SpeakerLost {
                on_event(TestEvent::Notice(AudioNotice::SpeakerLost));
            }
        }
        match frames.try_recv() {
            Ok(frame) => {
                let mut level = None;
                let out = stage.process(&frame, &settings, &mut level);
                sink.push(SOURCE, &out);
                if let Some(level) = level {
                    on_event(level);
                }
            }
            Err(_) => std::thread::sleep(Duration::from_millis(2)),
        }
    }
    sink.remove(SOURCE);
    // Dropping the speaker and the microphone joins their threads: the devices are released before `Ended`.
    drop(speaker);
    drop(mic);
}
