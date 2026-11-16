//! Task 19's behaviour and keyboard tests for the voice settings area (standard procedure S4-S6), on a fake audio host:
//! no sound card, no dillad. The parity tests are at the end of the file.
//!
//! Run with `cargo test -p dilla-desktop --features automation --test voice_settings`.

#![cfg(feature = "automation")]

use std::f64::consts::TAU;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use dilla_client::features::voice_prefs::ptt::PttHold;
use dilla_client::features::voice_prefs::{InputMode, NoisePreference, VoiceValues};
use dilla_client::state::voice_prefs::{AudioNotice, Processor, ProcessorNote, ProcessorStatus};
use dilla_desktop::screens::voice_settings::mic_test::{MicTest, OnEvent, TestEvent, TestSettings};
use dilla_desktop::screens::voice_settings::sounds::{Player, SoundKind};
use dilla_desktop::screens::voice_settings::testing::FakeHost;
use dilla_media::MediaError;
use dilla_media::noise::{FRAME_SAMPLES, ModelLoader, embedded_model};

// ---- helpers --------------------------------------------------------------------------------------

/// A steady 1 kHz tone of amplitude 0.5 (about -9 dBFS rms, the fake microphone of the web's e2e specs).
struct Tone(f64);

impl Tone {
    fn frame(&mut self) -> Vec<i16> {
        (0..FRAME_SAMPLES)
            .map(|_| {
                self.0 += 1000.0 / 48_000.0;
                ((self.0 * TAU).sin() * 0.5 * 32767.0) as i16
            })
            .collect()
    }
}

fn rms(x: &[i16]) -> f64 {
    if x.is_empty() {
        return 0.0;
    }
    (x.iter().map(|s| f64::from(*s).powi(2)).sum::<f64>() / x.len() as f64).sqrt()
}

fn settings() -> TestSettings {
    TestSettings {
        input: None,
        output: None,
        noise: NoisePreference::Off,
        echo: true,
        agc: true,
        input_volume: 100,
        output_volume: 100,
        sensitivity_auto: false,
        threshold_db: -100,
        mode: InputMode::Vad,
    }
}

/// A model loader that must never be asked, or fails when asked: what the web's route interception does.
fn counting_loader(calls: Arc<AtomicUsize>, fail: bool) -> ModelLoader {
    Arc::new(move || {
        calls.fetch_add(1, Ordering::SeqCst);
        if fail {
            Err(MediaError::Audio("fixture: model unavailable".into()))
        } else {
            embedded_model()
        }
    })
}

#[derive(Clone, Default)]
struct Events(Arc<Mutex<Vec<TestEvent>>>);

impl Events {
    fn sink(&self) -> OnEvent {
        let me = self.0.clone();
        Arc::new(move |e| me.lock().unwrap().push(e))
    }

    fn all(&self) -> Vec<TestEvent> {
        self.0.lock().unwrap().clone()
    }

    fn levels(&self) -> Vec<(f32, f32, bool)> {
        self.all()
            .into_iter()
            .filter_map(|e| match e {
                TestEvent::Level {
                    db,
                    threshold_db,
                    open,
                } => Some((db, threshold_db, open)),
                _ => None,
            })
            .collect()
    }

    fn ended(&self) -> bool {
        self.all().contains(&TestEvent::Ended)
    }

    fn started(&self) -> Option<ProcessorStatus> {
        self.all().into_iter().find_map(|e| match e {
            TestEvent::Started(s) => Some(s),
            _ => None,
        })
    }
}

/// Runs the fake machine for `ms` as a sound card would: a tone into the microphone every 10 ms, the speaker pulled every
/// 10 ms. Returns what the speaker played.
fn run(host: &FakeHost, tone: &mut Tone, ms: u64) -> Vec<i16> {
    let mut played = Vec::new();
    let end = Instant::now() + Duration::from_millis(ms);
    let mut next = Instant::now();
    while Instant::now() < end {
        host.push(&tone.frame());
        played.extend(host.pull(FRAME_SAMPLES));
        next += Duration::from_millis(10);
        std::thread::sleep(next.saturating_duration_since(Instant::now()));
    }
    played
}

fn wait_until(what: &str, mut f: impl FnMut() -> bool) {
    let end = Instant::now() + Duration::from_secs(10);
    while Instant::now() < end {
        if f() {
            return;
        }
        std::thread::sleep(Duration::from_millis(10));
    }
    panic!("timed out waiting for {what}");
}

fn start(host: &FakeHost, settings: TestSettings, loader: ModelLoader, ev: &Events) -> MicTest {
    MicTest::start(Arc::new(host.clone()), loader, settings, ev.sink())
}

fn no_model() -> (ModelLoader, Arc<AtomicUsize>) {
    let calls = Arc::new(AtomicUsize::new(0));
    (counting_loader(calls.clone(), true), calls)
}

// ---- behaviour: e2e/voice ----------------------------------------------------------------------

/// e2e/voice/microphone-test.spec.ts: Let's check plays you back live, shows the level and releases the microphone when it
/// stops (and the ten seconds end it by themselves).
#[test]
fn lets_check_plays_back_shows_level_releases_mic() {
    let host = FakeHost::new();
    let events = Events::default();
    let (loader, _) = no_model();
    // At rest nothing is captured.
    assert_eq!((host.inputs_open(), host.outputs_open()), (0, 0));

    let test = start(&host, settings(), loader.clone(), &events);
    wait_until("the devices to open", || {
        host.inputs_open() == 1 && host.outputs_open() == 1
    });
    let played = run(&host, &mut Tone(0.0), 1500);
    // The pill's "heard": the level is the tone's (about -9 dB) and the gate is open.
    let levels = events.levels();
    assert!(!levels.is_empty(), "the meter follows the level");
    assert!(
        levels.iter().any(|(db, _, open)| *db > -20.0 && *open),
        "{levels:?}"
    );
    // Played back live, through the speaker's sink: the loopback is the tone, not silence.
    let tail = rms(&played[played.len() / 2..]);
    assert!(tail > 2000.0, "the second half's rms: {tail}");
    assert_eq!(events.started().map(|s| s.effective), Some(Processor::Off));

    // Stop testing: the microphone and the speaker are released, and the test says it ended.
    test.stop();
    assert!(events.ended());
    assert_eq!((host.inputs_open(), host.outputs_open()), (0, 0));

    // The ten seconds end it by themselves (here shortened): everything is released without a stop.
    let again = Events::default();
    let (loader, _) = no_model();
    let test = MicTest::start_for(
        Arc::new(host.clone()),
        loader,
        settings(),
        again.sink(),
        Duration::from_millis(400),
    );
    wait_until("the test to open", || host.inputs_open() == 1);
    let _ = run(&host, &mut Tone(0.0), 800);
    wait_until("the test to end", || test.is_over());
    assert!(again.ended());
    assert_eq!((host.inputs_open(), host.outputs_open()), (0, 0));
    assert_eq!(host.inputs_opened(), 2, "one microphone stream per test");
}

/// e2e/voice/sensitivity.spec.ts: a voice under the threshold is not sent (the gate plays silence), lowering the line
/// during the test opens it without a restart, and the threshold persists.
#[test]
fn under_threshold_not_sent_threshold_persists() {
    let host = FakeHost::new();
    let events = Events::default();
    let (loader, _) = no_model();
    let mut manual = settings();
    manual.threshold_db = 0;
    // The gate is under test, not the echo canceller. Once the gate opens, the loopback plays the tone, and the speaker
    // feeds that tone to the canceller as its reference. A steady sine looks like its own delayed echo, so the canceller
    // adapts and cancels it (the microphone level drops from about -4 to -49 dB). How long it takes to re-converge depends
    // on the timing, so under CPU load the measured tail could be silent (main CI after #176).
    manual.echo = false;
    let test = start(&host, manual.clone(), loader, &events);
    wait_until("the microphone", || host.inputs_open() == 1);
    let mut tone = Tone(0.0);
    let played = run(&host, &mut tone, 1200);
    // The meter sees the tone, the gate stays shut: nothing is heard.
    assert!(events.levels().iter().any(|(db, _, _)| *db > -20.0));
    assert!(
        events
            .levels()
            .iter()
            .all(|(_, thr, open)| *thr == 0.0 && !*open)
    );
    assert!(rms(&played) < 50.0, "under the threshold: {}", rms(&played));

    // Lowering the line (Home on the slider: -100 dB) opens the gate without rejoining.
    let mut open = manual.clone();
    open.threshold_db = -100;
    test.update(open);
    let played = run(&host, &mut tone, 1200);
    let tail = rms(&played[played.len() / 2..]);
    assert!(tail > 2000.0, "the second half's rms: {tail}");
    test.stop();

    // The threshold and the levels persist across a restart: -100 dB, 150 %.
    let dir = std::env::temp_dir().join(format!("dilla-voice-settings-{}", std::process::id()));
    let prefs = dilla_client::prefs::Prefs::instance(&dir);
    use dilla_client::features::voice_prefs::Setting;
    VoiceValues::apply(&prefs, &Setting::SensitivityAuto(false)).unwrap();
    VoiceValues::apply(&prefs, &Setting::Sensitivity(-100)).unwrap();
    VoiceValues::apply(&prefs, &Setting::InputVolume(150)).unwrap();
    let reopened = VoiceValues::read(&dilla_client::prefs::Prefs::instance(&dir));
    assert!(!reopened.sensitivity_auto);
    assert_eq!(
        (reopened.sensitivity_db, reopened.input_volume),
        (-100, 150)
    );
    let _ = std::fs::remove_dir_all(dir);
}

/// e2e/voice/noise-beta.spec.ts: Automatic is an opt-in (the default loads no model), it enhances the preview, and a change
/// of processing ends the test, so the person can leave it again (call leave during a call is Task 28's list: it needs
/// Task 17's call).
#[test]
fn enhanced_opt_in_preview_and_call_leave_during_call() {
    // Lightweight is what a person who never chose gets, and it loads no model.
    assert_eq!(VoiceValues::default().noise, NoisePreference::Lightweight);
    let host = FakeHost::new();
    let calls = Arc::new(AtomicUsize::new(0));
    let events = Events::default();
    let mut light = settings();
    light.noise = NoisePreference::Lightweight;
    let test = start(
        &host,
        light.clone(),
        counting_loader(calls.clone(), false),
        &events,
    );
    wait_until("the microphone", || host.inputs_open() == 1);
    let _ = run(&host, &mut Tone(0.0), 300);
    assert_eq!(
        events.started(),
        Some(ProcessorStatus {
            effective: Processor::Standard,
            note: None
        })
    );
    assert_eq!(
        calls.load(Ordering::SeqCst),
        0,
        "no model before the opt-in"
    );
    test.stop();

    // The opt-in: Automatic loads the model and the preview is enhanced.
    let events = Events::default();
    let mut auto = light.clone();
    auto.noise = NoisePreference::Automatic;
    assert!(
        !light.same_chain(&auto),
        "a change of processing ends the test"
    );
    let test = start(&host, auto, counting_loader(calls.clone(), false), &events);
    wait_until("the enhanced test", || events.started().is_some());
    let _ = run(&host, &mut Tone(0.0), 300);
    assert_eq!(
        events.started(),
        Some(ProcessorStatus {
            effective: Processor::Enhanced,
            note: None
        })
    );
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    test.stop();

    // Leaving it: back to Lightweight, no fallback note, nothing more loaded.
    let events = Events::default();
    let test = start(&host, light, counting_loader(calls.clone(), false), &events);
    wait_until("the lightweight test", || events.started().is_some());
    assert_eq!(events.started().unwrap().note, None);
    assert_eq!(calls.load(Ordering::SeqCst), 1);
    test.stop();
}

/// e2e/voice/enhanced-noise.spec.ts: Automatic enhances the local test, and push to talk is enforced: under push to talk
/// the test's gate is off (the key governs the microphone, not the level), and the key's hold and release delay decide
/// what is sent. The loaded-call measurements of the spec (p99 under 8 ms) are dilla-media's `aec`/`enhanced` tests.
#[test]
fn automatic_enhances_test_and_call_enforces_ptt() {
    let host = FakeHost::new();
    let events = Events::default();
    let mut auto = settings();
    auto.noise = NoisePreference::Automatic;
    let calls = Arc::new(AtomicUsize::new(0));
    let test = start(&host, auto.clone(), counting_loader(calls, false), &events);
    wait_until("the test", || events.started().is_some());
    let _ = run(&host, &mut Tone(0.0), 400);
    assert_eq!(events.started().unwrap().effective, Processor::Enhanced);
    test.stop();

    // Voice activity with the line at 0 dB sends nothing; push to talk has no line (the gate is off): the same tone is
    // sent while the key governs.
    let (loader, _) = no_model();
    let mut shut = settings();
    shut.threshold_db = 0;
    let events = Events::default();
    let test = start(&host, shut.clone(), loader.clone(), &events);
    wait_until("the microphone", || host.inputs_open() == 1);
    let mut tone = Tone(0.0);
    let vad = run(&host, &mut tone, 800);
    assert!(rms(&vad) < 50.0);
    let mut ptt = shut;
    ptt.mode = InputMode::Ptt;
    test.update(ptt);
    let held = run(&host, &mut tone, 1000);
    assert!(rms(&held[held.len() / 2..]) > 2000.0);
    test.stop();

    // The key: sent while held, then for the release delay, and never after the window lost focus.
    let mut hold = PttHold::default();
    assert!(!hold.sending(0.0, 200));
    hold.press();
    assert!(hold.sending(50.0, 200));
    hold.let_go(100.0);
    assert!(hold.sending(250.0, 200) && !hold.sending(300.0, 200));
    hold.press();
    hold.release();
    assert!(!hold.sending(310.0, 200));
}

/// e2e/voice/enhanced-fallback.spec.ts: a missing enhanced model falls back visibly (the line says Lightweight and why),
/// and Off loads nothing.
#[test]
fn missing_model_falls_back_visibly_off_loads_nothing() {
    let host = FakeHost::new();
    let calls = Arc::new(AtomicUsize::new(0));
    let events = Events::default();
    let mut auto = settings();
    auto.noise = NoisePreference::Automatic;
    let test = start(&host, auto, counting_loader(calls.clone(), true), &events);
    wait_until("the fallback", || {
        events.all().iter().any(|e| {
            matches!(
                e,
                TestEvent::Processor(ProcessorStatus {
                    effective: Processor::Standard,
                    note: Some(ProcessorNote::EnhancedUnavailable)
                })
            )
        })
    });
    assert!(calls.load(Ordering::SeqCst) >= 1, "the model was asked for");
    test.stop();

    // Off: the processor is off and the model is never asked for.
    let calls = Arc::new(AtomicUsize::new(0));
    let events = Events::default();
    let test = start(
        &host,
        settings(),
        counting_loader(calls.clone(), true),
        &events,
    );
    wait_until("the off test", || events.started().is_some());
    assert_eq!(
        events.started(),
        Some(ProcessorStatus {
            effective: Processor::Off,
            note: None
        })
    );
    test.stop();
    assert_eq!(calls.load(Ordering::SeqCst), 0, "Off loads nothing");
}

/// The ledger follow-up of Task 4: a microphone that is gone with no default to take over is shown, not swallowed.
#[test]
fn a_lost_microphone_ends_the_test_and_says_so() {
    let host = FakeHost::new();
    let events = Events::default();
    let (loader, _) = no_model();
    let test = start(&host, settings(), loader, &events);
    wait_until("the microphone", || host.capturing_from().is_some());
    host.remove_default_input();
    host.unplug_input("builtin-mic");
    wait_until("the notice", || {
        events
            .all()
            .contains(&TestEvent::Notice(AudioNotice::MicLost))
    });
    wait_until("the test to end", || events.ended());
    assert_eq!(host.inputs_open(), 0);
    drop(test);
}

/// The sounds reach the speaker: a join tick rises, a leave tick falls, the ring pings and stops, and nothing plays between.
#[test]
fn sounds_reach_the_speaker() {
    let host = FakeHost::new();
    let player = Player::start(Arc::new(host.clone()), None);
    // Silent until asked.
    assert_eq!(host.outputs_open(), 0);
    player.play(SoundKind::Join);
    wait_until("the speaker", || host.outputs_open() == 1);
    let mut played = Vec::new();
    let end = Instant::now() + Duration::from_millis(600);
    while Instant::now() < end {
        played.extend(host.pull(FRAME_SAMPLES));
        std::thread::sleep(Duration::from_millis(10));
    }
    let loud = played.iter().filter(|s| s.abs() > 1000).count();
    assert!(loud > 2000, "the tick is audible: {loud} loud samples");
    // Ring, then stop: nothing but silence after the stop.
    player.start_ring();
    std::thread::sleep(Duration::from_millis(150));
    let ping: Vec<i16> = (0..30).flat_map(|_| host.pull(FRAME_SAMPLES)).collect();
    assert!(rms(&ping) > 500.0, "the ring pings at once");
    player.stop_ring();
    std::thread::sleep(Duration::from_millis(100));
    let after: Vec<i16> = (0..20).flat_map(|_| host.pull(FRAME_SAMPLES)).collect();
    assert!(rms(&after) < 100.0, "the ring stopped: {}", rms(&after));
}

// ---- keyboard: the section in a real (test) window ---------------------------------------------

mod window {
    use super::*;
    use dilla_desktop::screens::voice_settings::section::VoiceSection;
    use dilla_desktop::screens::voice_settings::{section_view, set_audio_host};
    use dilla_desktop::screens::{FocusNext, FocusPrevious};
    use dilla_desktop::slots::SlotCx;
    use gpui::{
        Context, Entity, FocusHandle, InteractiveElement, IntoElement, ParentElement, Render,
        Styled, TestAppContext, VisualTestContext, Window, div,
    };

    /// The section in a window with the shared focus keys, on a fake audio host.
    pub struct Host {
        pub root: FocusHandle,
        pub view: Option<Entity<VoiceSection>>,
        /// False: the section is not drawn (the person left it).
        pub show: bool,
        ctx: SlotCx,
    }

    impl Render for Host {
        fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
            if !self.show {
                self.view = None;
                return div().id("host").track_focus(&self.root).size_full();
            }
            let view = section_view(&self.ctx, window, cx);
            self.view = Some(view.clone());
            div()
                .id("host")
                .track_focus(&self.root)
                .size_full()
                .on_action(|_: &FocusNext, window, cx| window.focus_next(cx))
                .on_action(|_: &FocusPrevious, window, cx| window.focus_prev(cx))
                .child(view)
        }
    }

    /// The audio host is one per process, so the window tests take turns.
    pub static SERIAL: Mutex<()> = Mutex::new(());

    pub fn open(cx: &mut TestAppContext) -> (Entity<Host>, &mut VisualTestContext, FakeHost) {
        cx.update(dilla_desktop::install);
        let fake = FakeHost::new();
        set_audio_host(Arc::new(fake.clone()));
        let (host, cx) = cx.add_window_view(|_, cx| Host {
            root: cx.focus_handle(),
            view: None,
            show: true,
            ctx: SlotCx::default(),
        });
        cx.run_until_parked();
        (host, cx, fake)
    }

    pub fn section(host: &Entity<Host>, cx: &mut VisualTestContext) -> Entity<VoiceSection> {
        host.read_with(cx, |h, _| h.view.clone().expect("the section is drawn"))
    }

    /// A key as a keyboard sends it: down, then up.
    pub fn press(cx: &mut VisualTestContext, key: &'static str) {
        cx.simulate_keystrokes(key);
        cx.simulate_event(gpui::KeyUpEvent {
            keystroke: gpui::Keystroke::parse(key).expect("a key"),
        });
        cx.run_until_parked();
    }
}

use dilla_client::features::voice_prefs::Setting;

/// Which of the section's own focus handles holds the focus, else `-` (a shared component: a select, a segmented group or
/// a switch, each Task 6's own tab stop).
fn focus_name(host: &gpui::Entity<window::Host>, cx: &mut gpui::VisualTestContext) -> &'static str {
    let view = window::section(host, cx);
    let h = view.read_with(cx, |v, _| v.handles.clone());
    cx.update(|w, _| {
        for (name, handle) in [
            ("input-volume", &h.input_volume),
            ("output-volume", &h.output_volume),
            ("check", &h.check),
            ("threshold", &h.threshold),
            ("release", &h.release),
            ("capture", &h.capture),
        ] {
            if handle.is_focused(w) {
                return name;
            }
        }
        "-"
    })
}

/// Tabs from the window's root through `stops` stops of the section and lists them.
fn tab_order(
    host: &gpui::Entity<window::Host>,
    cx: &mut gpui::VisualTestContext,
    stops: usize,
) -> Vec<&'static str> {
    let root = host.read_with(cx, |h, _| h.root.clone());
    cx.update(|w, cx| root.focus(w, cx));
    (0..stops)
        .map(|_| {
            window::press(cx, "tab");
            focus_name(host, cx)
        })
        .collect()
}

/// Focus order, voice activity: the input select (-), its volume, the output select (-), its volume, Let's check, the
/// automatic switch (-), the mode group (-), the noise group (-), echo (-), gain (-). With automatic sensitivity the line
/// is fixed and not a tab stop (the web's range is disabled).
#[gpui::test]
fn keyboard_voice_settings_focus_order(cx: &mut gpui::TestAppContext) {
    let _turn = window::SERIAL.lock().unwrap_or_else(|p| p.into_inner());
    let (host, cx, _fake) = window::open(cx);
    let order = tab_order(&host, cx, 10);
    let expect = [
        "-",
        "input-volume",
        "-",
        "output-volume",
        "check",
        "-",
        "-",
        "-",
        "-",
        "-",
    ];
    assert_eq!(order, expect, "{order:?}");

    // A manual threshold puts the line in the order, after Let's check; push to talk adds its key button and its release
    // delay after the mode group.
    let section = window::section(&host, cx);
    section.update(cx, |s, cx| {
        s.set(Setting::SensitivityAuto(false), cx);
        s.set(Setting::Mode(InputMode::Ptt), cx);
    });
    cx.run_until_parked();
    let order = tab_order(&host, cx, 13);
    let expect = [
        "-",
        "input-volume",
        "-",
        "output-volume",
        "check",
        "threshold",
        "-",
        "-",
        "capture",
        "release",
        "-",
        "-",
        "-",
    ];
    assert_eq!(order, expect, "{order:?}");
}

/// Binding the push-to-talk key captures the next chord, Escape cancels the capture, and a key that types text or needs
/// another key is refused with the capture still listening.
#[gpui::test]
fn keyboard_voice_settings_ptt_capture_and_escape(cx: &mut gpui::TestAppContext) {
    let _turn = window::SERIAL.lock().unwrap_or_else(|p| p.into_inner());
    let (host, cx, _fake) = window::open(cx);
    let section = window::section(&host, cx);
    section.update(cx, |s, cx| s.set(Setting::Mode(InputMode::Ptt), cx));
    cx.run_until_parked();
    let chord = |cx: &mut gpui::VisualTestContext| {
        section.read_with(cx, |s, _| s.local.values.ptt_chord.clone())
    };
    let capturing = |cx: &mut gpui::VisualTestContext| section.read_with(cx, |s, _| s.capturing);
    assert_eq!(chord(cx), None, "there is no default key");

    // Enter on the set-key button starts the capture; the button holds the focus.
    let h = section.read_with(cx, |s, _| s.handles.clone());
    cx.update(|w, cx| h.capture.focus(w, cx));
    window::press(cx, "enter");
    assert!(capturing(cx));
    assert!(cx.update(|w, _| h.capture.is_focused(w)));

    // A key that types text is refused and the capture goes on; a chord with a modifier too.
    window::press(cx, "a");
    assert!(capturing(cx) && chord(cx).is_none());
    window::press(cx, "ctrl-f14");
    assert!(capturing(cx) && chord(cx).is_none());

    // The next accepted key is the chord.
    window::press(cx, "f14");
    assert!(!capturing(cx));
    assert_eq!(chord(cx).as_deref(), Some("F14"));

    // Escape cancels a second capture and keeps the key.
    window::press(cx, "enter");
    assert!(capturing(cx));
    window::press(cx, "escape");
    assert!(!capturing(cx));
    assert_eq!(chord(cx).as_deref(), Some("F14"));

    // Insert binds on its own, too.
    window::press(cx, "enter");
    window::press(cx, "insert");
    assert_eq!(chord(cx).as_deref(), Some("Insert"));
}

/// Let's check through the window: the button starts the loopback on the fake machine, shows the level, and Stop testing
/// releases both devices; a lost microphone shows its notice and ends the test.
#[gpui::test]
fn lets_check_in_the_window_shows_level_and_notices(cx: &mut gpui::TestAppContext) {
    let _turn = window::SERIAL.lock().unwrap_or_else(|p| p.into_inner());
    let (host, cx, fake) = window::open(cx);
    let section = window::section(&host, cx);
    cx.executor().allow_parking();
    section.update(cx, |s, cx| {
        s.set(Setting::Noise(NoisePreference::Off), cx);
        s.toggle_test(cx);
    });
    let testing =
        |cx: &mut gpui::VisualTestContext| section.read_with(cx, |s, _| s.local.test.testing);
    assert!(testing(cx));
    let mut tone = Tone(0.0);
    let deadline = Instant::now() + Duration::from_secs(10);
    // The section drains the device threads on its own timer: advance it as the machine runs.
    while Instant::now() < deadline && !section.read_with(cx, |s, _| s.local.test.level_db > -30.0)
    {
        let _ = run(&fake, &mut tone, 100);
        cx.executor().advance_clock(Duration::from_millis(100));
        cx.run_until_parked();
    }
    assert!(
        section.read_with(cx, |s, _| s.local.test.level_db > -30.0),
        "the meter shows the level: {:?}",
        section.read_with(cx, |s, _| s.local.test.clone())
    );
    assert_eq!((fake.inputs_open(), fake.outputs_open()), (1, 1));

    // Stop testing releases the microphone and the speaker.
    section.update(cx, |s, cx| s.toggle_test(cx));
    assert!(!testing(cx));
    assert_eq!((fake.inputs_open(), fake.outputs_open()), (0, 0));

    // A microphone lost mid-test: its notice is shown (never swallowed) and the test is over.
    section.update(cx, |s, cx| s.toggle_test(cx));
    wait_until("the microphone", || fake.capturing_from().is_some());
    fake.remove_default_input();
    fake.unplug_input("builtin-mic");
    let deadline = Instant::now() + Duration::from_secs(10);
    while Instant::now() < deadline
        && !section.read_with(cx, |s, _| s.local.notices.contains(&AudioNotice::MicLost))
    {
        std::thread::sleep(Duration::from_millis(20));
        cx.executor().advance_clock(Duration::from_millis(100));
        cx.run_until_parked();
    }
    assert!(section.read_with(cx, |s, _| s.local.notices.contains(&AudioNotice::MicLost)));
    assert!(!testing(cx));
    assert_eq!(fake.inputs_open(), 0);
}

// ---- parity: the Voice section against the web client's frame (standard procedure S3 and S4) ---------

mod parity {
    use std::path::{Path, PathBuf};

    use dilla_parity::screen::assert_screen;

    fn fixture(state: &str) -> PathBuf {
        Path::new(env!("CARGO_MANIFEST_DIR"))
            .join("../parity/fixtures/voice-settings")
            .join(format!("{state}.json"))
    }

    /// Compares the screen of `voice-settings/<state>` with the reference shot of the web client, under the unchanged
    /// `tolerance.toml`.
    fn check(state: &str) {
        if let Err(failure) = assert_screen(&fixture(state)) {
            panic!("voice-settings/{state}: {failure}");
        }
    }

    #[test]
    #[cfg_attr(
        not(any(target_os = "linux", target_os = "macos")),
        ignore = "references are Linux and macOS only"
    )]
    fn parity_voice_settings_section() {
        check("section");
    }

    #[test]
    #[cfg_attr(
        not(any(target_os = "linux", target_os = "macos")),
        ignore = "references are Linux and macOS only"
    )]
    fn parity_voice_settings_section_b() {
        check("section-b");
    }

    #[test]
    #[cfg_attr(
        not(any(target_os = "linux", target_os = "macos")),
        ignore = "references are Linux and macOS only"
    )]
    fn parity_voice_settings_mic_test_running() {
        check("mic-test-running");
    }

    #[test]
    #[cfg_attr(
        not(any(target_os = "linux", target_os = "macos")),
        ignore = "references are Linux and macOS only"
    )]
    fn parity_voice_settings_mic_test_running_b() {
        check("mic-test-running-b");
    }

    #[test]
    #[cfg_attr(
        not(any(target_os = "linux", target_os = "macos")),
        ignore = "references are Linux and macOS only"
    )]
    fn parity_voice_settings_enhanced_unavailable() {
        check("enhanced-unavailable");
    }

    #[test]
    #[cfg_attr(
        not(any(target_os = "linux", target_os = "macos")),
        ignore = "references are Linux and macOS only"
    )]
    fn parity_voice_settings_enhanced_unavailable_b() {
        check("enhanced-unavailable-b");
    }
}

/// R19 fix 1: Let's check does nothing while a call holds the microphone (the call's phase is injected).
#[gpui::test]
fn lets_check_does_nothing_while_in_a_call(cx: &mut gpui::TestAppContext) {
    let _turn = window::SERIAL.lock().unwrap_or_else(|p| p.into_inner());
    let (host, cx, fake) = window::open(cx);
    let section = window::section(&host, cx);
    section.update(cx, |s, cx| {
        s.in_call = |_| true;
        s.toggle_test(cx);
    });
    cx.run_until_parked();
    assert!(!section.read_with(cx, |s, _| s.local.test.testing));
    assert_eq!(
        (fake.inputs_opened(), fake.outputs_opened()),
        (0, 0),
        "no device was opened"
    );
    // Out of the call it starts again.
    section.update(cx, |s, cx| {
        s.in_call = |_| false;
        s.toggle_test(cx);
    });
    assert!(section.read_with(cx, |s, _| s.local.test.testing));
    section.update(cx, |s, _| s.stop_test());
}

/// R19 fix 3: leaving the section mid-test releases the microphone and the speaker.
#[gpui::test]
fn leaving_the_section_mid_test_releases_the_devices(cx: &mut gpui::TestAppContext) {
    let _turn = window::SERIAL.lock().unwrap_or_else(|p| p.into_inner());
    let (host, cx, fake) = window::open(cx);
    let section = window::section(&host, cx);
    section.update(cx, |s, cx| s.toggle_test(cx));
    wait_until("the devices to open", || {
        fake.inputs_open() == 1 && fake.outputs_open() == 1
    });
    drop(section);
    host.update(cx, |h, cx| {
        h.show = false;
        cx.notify();
    });
    cx.run_until_parked();
    cx.update(|window, _| window.refresh());
    cx.run_until_parked();
    wait_until("the devices to be released", || {
        fake.inputs_open() == 0 && fake.outputs_open() == 0
    });
}

/// R19 fix 2: the loopback never plays louder than 100 %, whatever the output volume (the feedback guard).
#[test]
fn the_loopback_is_capped_at_100_percent() {
    // Without gain control and echo cancellation the chain adds no gain of its own (R19's switches), so the levels compare.
    let mut normal = settings();
    normal.agc = false;
    normal.echo = false;
    normal.output_volume = 100;
    let mut loud = normal.clone();
    loud.output_volume = 200;
    assert_eq!(loud.loopback_gain(), 1.0);
    assert_eq!(normal.loopback_gain(), 1.0);
    let mut half = normal.clone();
    half.output_volume = 50;
    assert_eq!(half.loopback_gain(), 0.5);

    let played = |s: TestSettings| {
        let host = FakeHost::new();
        let events = Events::default();
        let (loader, _) = no_model();
        let test = start(&host, s, loader, &events);
        wait_until("the microphone", || host.inputs_open() == 1);
        let out = run(&host, &mut Tone(0.0), 1500);
        test.stop();
        rms(&out[out.len() / 2..])
    };
    let (at_200, at_100) = (played(loud), played(normal));
    assert!(at_100 > 2000.0, "{at_100}");
    assert!(
        (at_200 - at_100).abs() < at_100 * 0.1,
        "200 % played {at_200}, 100 % played {at_100}"
    );
}

/// R19 fix 4: a device that hangs while opening cannot freeze a stop or a drop, and nothing stays open once it answers.
#[test]
fn a_hung_device_never_freezes_stop() {
    let host = FakeHost::new();
    host.hold_opens();
    let events = Events::default();
    let (loader, _) = no_model();
    let test = start(&host, settings(), loader, &events);
    std::thread::sleep(Duration::from_millis(200));
    let t = Instant::now();
    test.stop();
    assert!(
        t.elapsed() < Duration::from_secs(2),
        "stop waited {:?} for a hung device",
        t.elapsed()
    );
    assert!(events.ended());
    // The device answers late: what it opened is closed again.
    host.release_opens();
    wait_until("the late device to be released", || {
        host.inputs_open() == 0 && host.outputs_open() == 0
    });
}
