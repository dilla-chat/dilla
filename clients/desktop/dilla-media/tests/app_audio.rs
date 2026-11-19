//! Parity plan Package 3: application audio (beta.5 `application-audio.spec.ts`).
//!
//! - Through a real SFU and a real PipeWire: two applications play (two `pw-play` processes into a throwaway null sink,
//!   so nothing is heard in the room); the sharer publishes the first one's sound, and the listener hears exactly that
//!   application, sealed and opened as the sharer's verified device.
//! - Synthetic frames: an application-audio sender that is not bound to the screen-audio slot sends nothing, and a bound
//!   one is opened only as screen audio.
//!
//! The live test needs dilla-testhost, the WASI core and a PipeWire session with `pw-play` and `pactl`
//! (`cargo test -p dilla-media --test app_audio -- --ignored`); without PipeWire it says so and passes.
//!
//! - Windows only (WASAPI process loopback): `captures_a_tone_from_a_child_process`, ignored, for a manual run on a
//!   Windows machine with an audio endpoint.
//! - macOS only (CoreAudio process taps, 14.2+): `captures_a_tone_from_afplay` and
//!   `ended_fires_once_when_the_app_exits` capture a real `afplay`; they run only with `DILLA_MAC_CAPTURE=1`
//!   (`DILLA_MAC_CAPTURE=1 cargo test -p dilla-media --test app_audio`), since the first process tap asks for the
//!   "system audio recording" permission (TCC AudioCapture), granted once by hand; without it they say so and pass.

#[allow(dead_code, unused_imports, clippy::all)]
#[path = "../../../../core/dilla-core/tests/client_support/mod.rs"]
mod client_support;

mod support {
    pub mod live;
}

use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use client_support::{CALL, call_raw_bob};
use dilla_core::sframe::{Codec, KeyRing, NK, Slot};
use dilla_media::transform::{CallMedia, Router};
use serde_json::Value;
use support::live::*;
use webrtc_sys::dilla_transform_testing as testing;

/// A 16-bit stereo WAV of a `hz` sine, `secs` long, at 48 kHz.
fn sine_wav(path: &Path, hz: f64, secs: u32) {
    let rate = 48_000u32;
    let frames = rate * secs;
    let mut data = Vec::with_capacity(frames as usize * 4);
    for n in 0..frames {
        let v = ((n as f64 / f64::from(rate) * hz * std::f64::consts::TAU).sin() * 12_000.0) as i16;
        data.extend_from_slice(&v.to_le_bytes());
        data.extend_from_slice(&v.to_le_bytes());
    }
    let mut wav = Vec::new();
    wav.extend_from_slice(b"RIFF");
    wav.extend_from_slice(&(36 + data.len() as u32).to_le_bytes());
    wav.extend_from_slice(b"WAVEfmt ");
    wav.extend_from_slice(&16u32.to_le_bytes());
    wav.extend_from_slice(&1u16.to_le_bytes());
    wav.extend_from_slice(&2u16.to_le_bytes());
    wav.extend_from_slice(&rate.to_le_bytes());
    wav.extend_from_slice(&(rate * 4).to_le_bytes());
    wav.extend_from_slice(&4u16.to_le_bytes());
    wav.extend_from_slice(&16u16.to_le_bytes());
    wav.extend_from_slice(b"data");
    wav.extend_from_slice(&(data.len() as u32).to_le_bytes());
    wav.extend_from_slice(&data);
    std::fs::write(path, wav).unwrap();
}

/// A null sink the players play into, unloaded when dropped.
struct NullSink {
    module: String,
    name: String,
}

impl NullSink {
    fn load() -> Option<NullSink> {
        let name = format!("dilla_test_null_{}", std::process::id());
        let out = Command::new("pactl")
            .args([
                "load-module",
                "module-null-sink",
                &format!("sink_name={name}"),
            ])
            .output()
            .ok()?;
        out.status.success().then(|| NullSink {
            module: String::from_utf8_lossy(&out.stdout).trim().to_owned(),
            name,
        })
    }
}

impl Drop for NullSink {
    fn drop(&mut self) {
        let _ = Command::new("pactl")
            .args(["unload-module", &self.module])
            .status();
    }
}

/// A `pw-play` process outside this process's tree (a shell starts it in the background and exits, so it is
/// reparented): this test process stands in for Dilla, whose whole tree the list leaves out.
struct Player(u32);

impl Player {
    fn start(sink: &str, file: &Path) -> Player {
        let out = Command::new("sh")
            .arg("-c")
            .arg("pw-play --target \"$1\" \"$2\" >/dev/null 2>&1 & echo $!")
            .args(["sh", sink])
            .arg(file)
            .stderr(Stdio::null())
            .output()
            .expect("sh");
        Player(
            String::from_utf8_lossy(&out.stdout)
                .trim()
                .parse()
                .expect("a pid"),
        )
    }

    fn id(&self) -> u32 {
        self.0
    }
}

impl Drop for Player {
    fn drop(&mut self) {
        let _ = Command::new("kill").arg(self.0.to_string()).status();
    }
}

fn scratch(name: &str) -> PathBuf {
    let dir = std::env::temp_dir().join(format!("dilla-app-audio-{}", std::process::id()));
    std::fs::create_dir_all(&dir).unwrap();
    dir.join(name)
}

/// application-audio.spec.ts "selected application reaches an encrypted listener while other applications and Dilla call
/// audio stay excluded": alice (listen only, no microphone) shares the sound of the 440 Hz player while a 1000 Hz player
/// runs too. Bob hears alice's device at 440 Hz, never 1000 Hz; every frame was opened under alice's key, none refused.
/// The list alice chose from never named her own process.
#[test]
#[ignore = "needs dilla-testhost (Go), the WASI core and a PipeWire session (pw-play, pactl)"]
fn only_the_chosen_app_reaches_the_listener_encrypted() {
    if dilla_media::app_audio::sources().is_err() {
        eprintln!("no PipeWire session here: application audio is not measured");
        return;
    }
    let Some(sink) = NullSink::load() else {
        eprintln!("no pactl / null sink here: application audio is not measured");
        return;
    };
    let (low, high) = (scratch("440.wav"), scratch("1000.wav"));
    sine_wav(&low, 440.0, 60);
    sine_wav(&high, 1000.0, 60);
    let chosen = Player::start(&sink.name, &low);
    let other = Player::start(&sink.name, &high);
    // Both players appear in the list.
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        let list = dilla_media::app_audio::sources().unwrap();
        let pids: Vec<u32> = list.iter().map(|s| s.pid).collect();
        if pids.contains(&chosen.id()) && pids.contains(&other.id()) {
            assert!(!pids.contains(&std::process::id()), "never Dilla itself");
            break;
        }
        assert!(
            Instant::now() < deadline,
            "the players never appeared: {list:?}"
        );
        std::thread::sleep(Duration::from_millis(200));
    }

    let host = start_host();
    let (_call, alice, bob, _) = call_raw_bob();
    let (a_dev, b_dev) = (hex(&alice.device), hex(&bob.device));
    let room = hex(&CALL);
    let (url, a_token) = token(&host.control, &room, &a_dev);
    let (_, b_token) = token(&host.control, &room, &b_dev);
    let mut a = Peer::listening(&url, &a_token);
    let mut b = Peer::listening(&url, &b_token);
    let e = alice_epoch(&alice);
    a.epoch(&e);
    b.epoch(&bob_epoch(&bob));
    a.next("connected", Duration::from_secs(30));
    b.next("connected", Duration::from_secs(30));
    a.send(&format!("appaudio {}", chosen.id()));
    let shared = a.next("appaudio", Duration::from_secs(15));
    assert_eq!(shared["error"], Value::Null, "{shared}");

    hears(&mut b, &a_dev, 440.0, Duration::from_secs(20));
    let a_kid = format!("{:x}", (e.own_leaf << 8) | (e.epoch & 0xff));
    // Two more seconds (50 Opus frames a second): still only the chosen application, every frame opened.
    std::thread::sleep(Duration::from_secs(2));
    let s = b.stats();
    assert!(opened(&s, &a_kid) > 50, "{s}");
    assert_eq!(failed(&s), 0, "{s}");
    let peak = s["peaks"][a_dev.as_str()].as_f64().unwrap_or(0.0);
    assert!(
        (peak - 440.0).abs() <= 20.0,
        "only the chosen application: {s}"
    );
    let sa = a.stats();
    assert!(
        sa["media"]["encrypted"][&a_kid].as_u64().unwrap_or(0) > 50,
        "{sa}"
    );
    // Stopping it stops the sound.
    a.send("unappaudio");
    a.next("unappaudio", Duration::from_secs(5));
    drop(other);
    drop(chosen);
    let _ = std::fs::remove_dir_all(low.parent().unwrap());
}

/// application-audio.spec.ts "native application audio without an authenticated source slot cannot fall back to
/// plaintext": an application-audio sender whose track this call never announced for the screen-audio slot drops every
/// frame; one that was is sealed under the screen-audio slot, and a listener opens it only as screen audio, never as a
/// microphone.
#[test]
fn no_source_slot_sends_nothing() {
    const BASE: [u8; NK] = [0x52; NK];
    const ROSTER: [(u16, [u8; 16]); 2] = [(0, [0xaa; 16]), (1, [0xbb; 16])];
    let opus: &[u8] = &[0xfc, 0xff, 0xfe, 0x10, 0x20, 0x30, 0x40];
    let router = Router::get().expect("hook");
    let media = CallMedia::new();
    router.begin(media.clone()).expect("no other call");
    media.install_epoch(6, &BASE, &ROSTER, 0).expect("keys");
    media.expect_local("app-audio", Slot::ScreenAudio);
    let unbound = testing::attach(true, false, "app-audio-unannounced");
    let bound = testing::attach(true, false, "app-audio");
    for _ in 0..20 {
        testing::frame(unbound, "audio/opus", opus, false);
    }
    testing::frame(bound, "audio/opus", opus, false);
    assert_eq!(testing::emitted_count(unbound), 0, "no plaintext fallback");
    assert_eq!(media.stats().failed.get("unbound"), Some(&20));
    let sealed = testing::take_emitted(bound);
    assert_eq!(sealed.len(), 1);
    let mut listener = KeyRing::new();
    listener.install_epoch(6, &BASE, &ROSTER, Some(1), 0);
    assert!(
        listener
            .decrypt(
                Codec::Opus,
                &sealed[0],
                Some(&[0xaa; 16]),
                Slot::Microphone,
                0
            )
            .is_err(),
        "the slot is authenticated: not a microphone"
    );
    let opened = listener
        .decrypt(
            Codec::Opus,
            &sealed[0],
            Some(&[0xaa; 16]),
            Slot::ScreenAudio,
            0,
        )
        .expect("opened as screen audio");
    assert_eq!(opened.frame, opus);
    testing::release(unbound);
    testing::release(bound);
    router.end(&media);
}

/// The strongest frequency of `mono` (48 kHz) between 100 and 2000 Hz, in 5 Hz steps (Goertzel).
#[cfg(any(target_os = "windows", target_os = "macos"))]
fn peak_hz(mono: &[f32]) -> f64 {
    let power = |hz: f64| {
        let k = 2.0 * (std::f64::consts::TAU * hz / 48_000.0).cos();
        let (mut s1, mut s2) = (0.0f64, 0.0f64);
        for &x in mono {
            let s0 = f64::from(x) + k * s1 - s2;
            s2 = s1;
            s1 = s0;
        }
        s1 * s1 + s2 * s2 - k * s1 * s2
    };
    (20..=400)
        .map(|step| f64::from(step) * 5.0)
        .map(|hz| (hz, power(hz)))
        .fold(
            (0.0, f64::MIN),
            |best, (hz, p)| if p > best.1 { (hz, p) } else { best },
        )
        .0
}

/// GPUI gaps Task 4 (WASAPI process loopback): a PowerShell outside this process's tree (another one starts it and
/// exits, as `sh … &` does on Linux) plays a 440 Hz tone; it is listed, this test process (Dilla's stand-in) is not, and
/// capturing it yields 48 kHz stereo frames whose peak is within 20 Hz of 440. Unverified until run on a Windows machine
/// (build 19041+) with an audio endpoint:
/// `cargo test -p dilla-media --test app_audio captures_a_tone_from_a_child_process -- --ignored`.
#[cfg(target_os = "windows")]
#[test]
#[ignore = "needs Windows (build 19041+) with an audio endpoint; PowerShell plays the tone"]
fn captures_a_tone_from_a_child_process() {
    use std::sync::{Arc, Mutex};

    struct Kill(u32);
    impl Drop for Kill {
        fn drop(&mut self) {
            let _ = Command::new("taskkill")
                .args(["/PID", &self.0.to_string(), "/F"])
                .status();
        }
    }

    let wav = scratch("440.wav");
    sine_wav(&wav, 440.0, 30);
    let path = wav.display().to_string();
    assert!(
        !path.contains('\''),
        "the temp path is quoted for PowerShell: {path}"
    );
    let script = format!(
        "(Start-Process powershell -WindowStyle Hidden -PassThru -ArgumentList '-NoProfile','-Command',\
         '(New-Object Media.SoundPlayer ''{path}'').PlaySync()').Id"
    );
    let out = Command::new("powershell")
        .args(["-NoProfile", "-Command", &script])
        .output()
        .expect("powershell");
    let pid: u32 = String::from_utf8_lossy(&out.stdout)
        .trim()
        .parse()
        .expect("the player's pid");
    let _player = Kill(pid);

    let deadline = Instant::now() + Duration::from_secs(10);
    let source = loop {
        let list = dilla_media::app_audio::sources().expect("process loopback on this build");
        assert!(
            list.iter().all(|s| s.pid != std::process::id()),
            "never Dilla itself"
        );
        if let Some(s) = list.into_iter().find(|s| s.pid == pid) {
            break s;
        }
        assert!(Instant::now() < deadline, "the player never appeared");
        std::thread::sleep(Duration::from_millis(200));
    };

    // Collected on the capture thread, asserted here (a panic there would only end that thread).
    let mono = Arc::new(Mutex::new(Vec::<f32>::new()));
    let lengths = Arc::new(Mutex::new(Vec::<usize>::new()));
    let (sink, sizes) = (mono.clone(), lengths.clone());
    let capture = dilla_media::app_audio::start(
        &source,
        Box::new(move |frame: Vec<f32>| {
            sizes.lock().unwrap().push(frame.len());
            sink.lock()
                .unwrap()
                .extend(frame.as_chunks::<2>().0.iter().map(|[l, r]| (l + r) / 2.0));
        }),
        Box::new(|| {}),
    )
    .expect("capture");
    let deadline = Instant::now() + Duration::from_secs(10);
    while mono.lock().unwrap().len() < 96_000 {
        assert!(Instant::now() < deadline, "two seconds of sound never came");
        std::thread::sleep(Duration::from_millis(50));
    }
    drop(capture);
    assert!(
        lengths
            .lock()
            .unwrap()
            .iter()
            .all(|&n| n == dilla_media::app_audio::FRAME_SAMPLES),
        "every frame is 960 samples"
    );
    let samples = mono.lock().unwrap();
    // The second second: playback has surely started.
    let peak = peak_hz(&samples[48_000..96_000]);
    assert!((peak - 440.0).abs() <= 20.0, "peak at {peak} Hz");
    drop(samples);
    let _ = std::fs::remove_dir_all(wav.parent().unwrap());
}

/// Runs `f` on a thread of its own and waits at most `secs` for its answer.
#[cfg(target_os = "windows")]
fn within<T: Send + 'static>(secs: u64, f: impl FnOnce() -> T + Send + 'static) -> T {
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let _ = tx.send(f());
    });
    rx.recv_timeout(Duration::from_secs(secs))
        .expect("answered in time")
}

/// GPUI gaps Task 4, in CI (no audio device needed): listing the sessions answers, a list or a clean error, never a
/// crash or a hang, and never names this process.
#[cfg(target_os = "windows")]
#[test]
fn sources_lists_without_crashing() {
    match within(10, dilla_media::app_audio::sources) {
        Ok(list) => assert!(
            list.iter().all(|s| s.pid != std::process::id()),
            "never Dilla itself: {list:?}"
        ),
        Err(e) => eprintln!("no application audio here: {e}"),
    }
}

/// GPUI gaps Task 4, in CI: starting a capture of a live (silent) process runs the whole process-loopback activation
/// (the PROPVARIANT's VT_BLOB parameters, the completion handler, the client's initialisation) and answers within 10 s,
/// a capture or a clean error; dropping the capture is clean too. The sleeper is started by a PowerShell that exits, so
/// it is outside this process's tree (a child would be refused as Dilla's own, `Gone`, before any activation).
#[cfg(target_os = "windows")]
#[test]
fn start_on_a_live_process_returns_cleanly() {
    let out = Command::new("powershell")
        .args([
            "-NoProfile",
            "-Command",
            "(Start-Process powershell -WindowStyle Hidden -PassThru -ArgumentList '-NoProfile','-Command','Start-Sleep 30').Id",
        ])
        .output()
        .expect("powershell");
    let pid: u32 = String::from_utf8_lossy(&out.stdout)
        .trim()
        .parse()
        .expect("the sleeper's pid");
    struct Kill(u32);
    impl Drop for Kill {
        fn drop(&mut self) {
            let _ = Command::new("taskkill")
                .args(["/PID", &self.0.to_string(), "/F"])
                .status();
        }
    }
    let _kill = Kill(pid);
    let source = dilla_media::app_audio::AppSource {
        serial: 1,
        pid,
        label: "sleeper".to_owned(),
    };
    let answer = within(10, move || {
        dilla_media::app_audio::start(&source, Box::new(|_| {}), Box::new(|| {})).map(|capture| {
            std::thread::sleep(Duration::from_millis(300));
            drop(capture);
        })
    });
    if let Err(e) = answer {
        eprintln!("no capture here (a clean error): {e}");
    }
}

/// Whether the macOS hardware tests run here (`DILLA_MAC_CAPTURE=1`): the first process tap needs the "system audio
/// recording" permission, which is granted by hand once.
#[cfg(target_os = "macos")]
fn mac_capture() -> bool {
    let on = std::env::var("DILLA_MAC_CAPTURE").as_deref() == Ok("1");
    if !on {
        eprintln!("DILLA_MAC_CAPTURE is not 1: macOS application audio is not measured");
    }
    on
}

/// An `afplay` outside this process's tree (a shell starts it in the background and exits, so launchd adopts and
/// reaps it): this test process stands in for Dilla, whose whole tree is never captured. Quiet (`-v 0.1`): it still
/// plays on the Mac's output.
#[cfg(target_os = "macos")]
struct Afplay(u32);

#[cfg(target_os = "macos")]
impl Afplay {
    fn start(file: &Path) -> Afplay {
        let out = Command::new("sh")
            .arg("-c")
            .arg("afplay -v 0.1 \"$1\" >/dev/null 2>&1 & echo $!")
            .arg("sh")
            .arg(file)
            .stderr(Stdio::null())
            .output()
            .expect("sh");
        Afplay(
            String::from_utf8_lossy(&out.stdout)
                .trim()
                .parse()
                .expect("a pid"),
        )
    }

    /// The player as a source, once CoreAudio lists it as playing (and never this process).
    fn source(&self) -> dilla_media::app_audio::AppSource {
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            let list = dilla_media::app_audio::sources().expect("process taps on this macOS");
            assert!(
                list.iter().all(|s| s.pid != std::process::id()),
                "never Dilla itself: {list:?}"
            );
            if let Some(s) = list.into_iter().find(|s| s.pid == self.0) {
                return s;
            }
            assert!(Instant::now() < deadline, "afplay never appeared");
            std::thread::sleep(Duration::from_millis(100));
        }
    }
}

#[cfg(target_os = "macos")]
impl Drop for Afplay {
    fn drop(&mut self) {
        let _ = Command::new("kill")
            .arg(self.0.to_string())
            .stderr(Stdio::null())
            .status();
    }
}

/// GPUI gaps Task 5 (CoreAudio process taps): `afplay` plays a 440 Hz tone outside this process's tree; it is listed
/// by pid, this test process (Dilla's stand-in) is not, and capturing it yields 960-sample frames (48 kHz stereo) whose
/// peak is within 20 Hz of 440. Dropping the capture never calls `ended`.
#[cfg(target_os = "macos")]
#[test]
fn captures_a_tone_from_afplay() {
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::{Arc, Mutex};

    if !mac_capture() {
        return;
    }
    let wav = scratch("mac-440.wav");
    sine_wav(&wav, 440.0, 30);
    let player = Afplay::start(&wav);
    let source = player.source();

    // Collected on the capture thread, asserted here (a panic there would only end that thread).
    let mono = Arc::new(Mutex::new(Vec::<f32>::new()));
    let lengths = Arc::new(Mutex::new(Vec::<usize>::new()));
    let ended = Arc::new(AtomicUsize::new(0));
    let (sink, sizes, ends) = (mono.clone(), lengths.clone(), ended.clone());
    let capture = dilla_media::app_audio::start(
        &source,
        Box::new(move |frame: Vec<f32>| {
            sizes.lock().unwrap().push(frame.len());
            sink.lock()
                .unwrap()
                .extend(frame.as_chunks::<2>().0.iter().map(|[l, r]| (l + r) / 2.0));
        }),
        Box::new(move || {
            ends.fetch_add(1, Ordering::SeqCst);
        }),
    )
    .expect("capture");
    assert_eq!(capture.label(), source.label);
    // Three seconds: the last two are measured.
    let deadline = Instant::now() + Duration::from_secs(15);
    while mono.lock().unwrap().len() < 144_000 {
        assert!(
            Instant::now() < deadline,
            "three seconds of sound never came ({} samples)",
            mono.lock().unwrap().len()
        );
        std::thread::sleep(Duration::from_millis(50));
    }
    drop(capture);
    std::thread::sleep(Duration::from_millis(300));
    assert_eq!(ended.load(Ordering::SeqCst), 0, "a drop never calls ended");
    assert!(
        lengths
            .lock()
            .unwrap()
            .iter()
            .all(|&n| n == dilla_media::app_audio::FRAME_SAMPLES),
        "every frame is 960 samples"
    );
    let samples = mono.lock().unwrap();
    let window = &samples[48_000..144_000];
    let rms = (window.iter().map(|x| x * x).sum::<f32>() / window.len() as f32).sqrt();
    assert!(
        rms > 1e-4,
        "the capture is silent (rms {rms}): is the system audio recording permission granted?"
    );
    let peak = peak_hz(window);
    eprintln!("captured afplay: peak {peak} Hz, rms {rms}");
    assert!(
        (peak - 440.0).abs() <= 20.0,
        "peak at {peak} Hz (rms {rms})"
    );
    drop(samples);
    drop(player);
    let _ = std::fs::remove_file(&wav);
}

/// GPUI gaps Task 5, Review Focus 3: an application that exits mid-share ends the capture once, and the capture's
/// drop afterwards neither calls `ended` again nor hangs.
#[cfg(target_os = "macos")]
#[test]
fn ended_fires_once_when_the_app_exits() {
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};

    if !mac_capture() {
        return;
    }
    let wav = scratch("mac-440-short.wav");
    sine_wav(&wav, 440.0, 3);
    let player = Afplay::start(&wav);
    let source = player.source();
    let ended = Arc::new(AtomicUsize::new(0));
    let ends = ended.clone();
    let capture = dilla_media::app_audio::start(
        &source,
        Box::new(|_| {}),
        Box::new(move || {
            ends.fetch_add(1, Ordering::SeqCst);
        }),
    )
    .expect("capture");
    let deadline = Instant::now() + Duration::from_secs(15);
    while ended.load(Ordering::SeqCst) == 0 {
        assert!(
            Instant::now() < deadline,
            "ended never came after afplay exited"
        );
        std::thread::sleep(Duration::from_millis(50));
    }
    std::thread::sleep(Duration::from_millis(500));
    let dropped = Instant::now();
    drop(capture);
    assert!(
        dropped.elapsed() < Duration::from_secs(1),
        "the drop does not block"
    );
    std::thread::sleep(Duration::from_millis(300));
    assert_eq!(ended.load(Ordering::SeqCst), 1, "ended exactly once");
    drop(player);
    let _ = std::fs::remove_file(&wav);
}
