//! The synthesised call sounds (packages/web/src/call/sounds.ts `Sounds`), played through dilla-media's `Speaker`
//! (Task 19). The voice area (17) plays the join and leave ticks, the ring area (18) the ring. Whether a sound may play
//! at all (the settings, deafened, one's own join or leave, busy or quiet rings) is the caller's decision, as on the
//! web (screens/voice/useSounds.tsx): this module only plays.
//!
//! No sound file ships: each sound is a few triangle tones, every one under 300 ms, shaped by a 5 ms attack and release
//! so none clicks. [`TICK`], [`RING`] and [`PEAK`] are the table of sounds.ts; [`synth_tick`] and [`synth_ping`] make the
//! samples (48 kHz mono, as the speaker takes them), [`Mixer`] schedules them and the [`Player`] thread feeds the
//! speaker, which it opens on the first sound and closes after a few quiet seconds.

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};
use std::time::Duration;

use dilla_media::devices::{AudioHost, DeviceId};
use dilla_media::noise::FRAME_SAMPLES;
use dilla_media::playout::{PlayoutSink, Speaker};
use gpui::App;

/// sounds.ts `SoundKind`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum SoundKind {
    Join,
    Leave,
}

/// The sample rate the speaker takes.
pub const RATE: u32 = 48_000;

/// A two-tone tick: 100 ms each, 20 ms apart, so the whole sound ends 220 ms after it starts (sounds.ts `TICK`).
pub struct Tick {
    pub tone_s: f64,
    pub gap_s: f64,
}

pub const TICK: Tick = Tick {
    tone_s: 0.1,
    gap_s: 0.02,
};

/// A tick's two frequencies, in order (sounds.ts `TICKS`): up for a join, down for a leave.
pub const fn tick_hz(kind: SoundKind) -> [f32; 2] {
    match kind {
        SoundKind::Join => [660.0, 880.0],
        SoundKind::Leave => [880.0, 660.0],
    }
}

/// The ring: 2 pings of 120 ms at 1000 Hz, 40 ms apart, every 2 s, at most 15 times (30 s, R5) (sounds.ts `RING`).
pub struct Ring {
    pub hz: f32,
    pub tone_s: f64,
    pub gap_s: f64,
    pub every_ms: u64,
    pub cycles: u32,
}

pub const RING: Ring = Ring {
    hz: 1000.0,
    tone_s: 0.12,
    gap_s: 0.04,
    every_ms: 2_000,
    cycles: 15,
};

/// The gain every tone peaks at.
pub const PEAK: f32 = 0.2;
/// The attack and release ramps, so a tone starts and ends without a click.
pub const EDGE_S: f64 = 0.005;

/// The WebAudio `triangle` oscillator at phase `p` (in cycles): 0 at the start, rising to 1 at a quarter.
fn triangle(p: f64) -> f64 {
    let f = p - p.floor();
    if f < 0.25 {
        4.0 * f
    } else if f < 0.75 {
        2.0 - 4.0 * f
    } else {
        4.0 * f - 4.0
    }
}

/// One tone mixed into `out` from `start_s`: a triangle at `hz`, ramped up to [`PEAK`] over [`EDGE_S`], held, and ramped
/// down to 0 over the last [`EDGE_S`] of `length_s` (sounds.ts `tone`).
fn tone(out: &mut [f32], hz: f32, start_s: f64, length_s: f64) {
    let first = (start_s * f64::from(RATE)).round() as usize;
    let count = (length_s * f64::from(RATE)).round() as usize;
    for i in 0..count {
        let Some(slot) = out.get_mut(first + i) else {
            break;
        };
        let t = i as f64 / f64::from(RATE);
        let envelope = if t < EDGE_S {
            t / EDGE_S
        } else if t > length_s - EDGE_S {
            ((length_s - t) / EDGE_S).max(0.0)
        } else {
            1.0
        };
        *slot += (triangle(f64::from(hz) * t) * envelope) as f32 * PEAK;
    }
}

fn to_i16(x: &[f32]) -> Vec<i16> {
    x.iter()
        .map(|s| (s.clamp(-1.0, 1.0) * f32::from(i16::MAX)).round() as i16)
        .collect()
}

/// A join or leave tick: 220 ms.
pub fn synth_tick(kind: SoundKind) -> Vec<i16> {
    let total = 2.0 * TICK.tone_s + TICK.gap_s;
    let mut out = vec![0.0f32; (total * f64::from(RATE)).round() as usize];
    let [low, high] = tick_hz(kind);
    tone(&mut out, low, 0.0, TICK.tone_s);
    tone(&mut out, high, TICK.tone_s + TICK.gap_s, TICK.tone_s);
    to_i16(&out)
}

/// One ring ping, two tones: 280 ms.
pub fn synth_ping() -> Vec<i16> {
    let total = 2.0 * RING.tone_s + RING.gap_s;
    let mut out = vec![0.0f32; (total * f64::from(RATE)).round() as usize];
    tone(&mut out, RING.hz, 0.0, RING.tone_s);
    tone(&mut out, RING.hz, RING.tone_s + RING.gap_s, RING.tone_s);
    to_i16(&out)
}

/// Frames (10 ms) between two pings of the ring.
const RING_FRAMES: u64 = RING.every_ms / 10;

/// The scheduler: what is queued to play, cut into the 10 ms frames the speaker is fed. It owns no clock; the caller
/// takes a frame every 10 ms.
pub struct Mixer {
    queued: VecDeque<i16>,
    ring: Option<RingRun>,
}

struct RingRun {
    /// Pings queued so far.
    pings: u32,
    /// Frames since the last ping was queued.
    frames: u64,
}

impl Default for Mixer {
    fn default() -> Self {
        Self::new()
    }
}

impl Mixer {
    pub fn new() -> Self {
        Self {
            queued: VecDeque::new(),
            ring: None,
        }
    }

    /// Plays a tick now (what still plays is cut: the new tick replaces it; a running ring goes on with its next ping).
    pub fn play(&mut self, kind: SoundKind) {
        self.queued.clear();
        self.queued.extend(synth_tick(kind));
    }

    /// Starts the ring: a ping now, then every 2 s, at most 15 times (a ring already running starts over).
    pub fn start_ring(&mut self) {
        self.queued.clear();
        self.queued.extend(synth_ping());
        self.ring = Some(RingRun {
            pings: 1,
            frames: 0,
        });
    }

    pub fn stop_ring(&mut self) {
        self.ring = None;
        self.queued.clear();
    }

    pub fn ringing(&self) -> bool {
        self.ring.is_some()
    }

    /// Nothing is queued and no ping is still to come.
    pub fn idle(&self) -> bool {
        self.queued.is_empty() && self.ring.is_none()
    }

    /// The next 10 ms, or `None` while idle.
    pub fn next_frame(&mut self) -> Option<[i16; FRAME_SAMPLES]> {
        if let Some(run) = &mut self.ring {
            run.frames += 1;
            if run.frames >= RING_FRAMES && run.pings < RING.cycles {
                run.pings += 1;
                run.frames = 0;
                self.queued.extend(synth_ping());
            }
            // The last ping has played out: the ring is over.
            if run.pings >= RING.cycles && self.queued.is_empty() {
                self.ring = None;
            }
        }
        if self.queued.is_empty() {
            return None;
        }
        let mut frame = [0i16; FRAME_SAMPLES];
        for slot in &mut frame {
            match self.queued.pop_front() {
                Some(s) => *slot = s,
                None => break,
            }
        }
        Some(frame)
    }
}

enum Cmd {
    Play(SoundKind),
    StartRing,
    StopRing,
    Output(Option<String>),
}

/// How long the speaker stays open after the last sound.
const LINGER: Duration = Duration::from_secs(5);

/// The thread that feeds the speaker.
pub struct Player {
    tx: std::sync::mpsc::Sender<Cmd>,
}

impl Player {
    /// A player on `host`, playing on `output` (`None`: the system default).
    pub fn start(host: Arc<dyn AudioHost>, output: Option<String>) -> Player {
        let (tx, rx) = std::sync::mpsc::channel();
        let spawned = std::thread::Builder::new()
            .name("dilla-sounds".into())
            .spawn(move || run(host, output, rx));
        if let Err(e) = spawned {
            log::warn!("dilla: the sounds thread did not start: {e}");
        }
        Player { tx }
    }

    pub fn play(&self, kind: SoundKind) {
        let _ = self.tx.send(Cmd::Play(kind));
    }

    pub fn start_ring(&self) {
        let _ = self.tx.send(Cmd::StartRing);
    }

    pub fn stop_ring(&self) {
        let _ = self.tx.send(Cmd::StopRing);
    }

    /// The output device changed (`None`: the system default).
    pub fn set_output(&self, output: Option<String>) {
        let _ = self.tx.send(Cmd::Output(output));
    }
}

fn run(host: Arc<dyn AudioHost>, mut output: Option<String>, rx: std::sync::mpsc::Receiver<Cmd>) {
    let mut mixer = Mixer::new();
    let mut speaker: Option<(Speaker, PlayoutSink)> = None;
    let mut quiet_for = Duration::ZERO;
    let frame_every = Duration::from_millis(10);
    let mut next = std::time::Instant::now();
    loop {
        let wait = if mixer.idle() && speaker.is_none() {
            // Nothing to do: sleep until a command.
            match rx.recv() {
                Ok(cmd) => Some(cmd),
                Err(_) => return,
            }
        } else {
            let now = std::time::Instant::now();
            match rx.recv_timeout(next.saturating_duration_since(now)) {
                Ok(cmd) => Some(cmd),
                Err(std::sync::mpsc::RecvTimeoutError::Timeout) => None,
                Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => return,
            }
        };
        if let Some(cmd) = wait {
            match cmd {
                Cmd::Play(kind) => mixer.play(kind),
                Cmd::StartRing => mixer.start_ring(),
                Cmd::StopRing => mixer.stop_ring(),
                Cmd::Output(device) => {
                    output = device;
                    speaker = None;
                }
            }
            if speaker.is_none() && !mixer.idle() {
                next = std::time::Instant::now();
            }
        }
        let now = std::time::Instant::now();
        if now < next {
            continue;
        }
        next += frame_every;
        if mixer.idle() && speaker.is_none() {
            continue;
        }
        match mixer.next_frame() {
            Some(frame) => {
                quiet_for = Duration::ZERO;
                if speaker.is_none() {
                    let device = output.clone().map(DeviceId);
                    match Speaker::open_with(
                        host.clone(),
                        dilla_media::apm::Apm::default(),
                        None,
                        device,
                    ) {
                        Ok(s) => {
                            let sink = s.sink();
                            speaker = Some((s, sink));
                        }
                        Err(e) => log::warn!("dilla: the sounds have no speaker: {e}"),
                    }
                }
                if let Some((_, sink)) = &speaker {
                    sink.push("sounds", &frame);
                }
            }
            None => {
                quiet_for += frame_every;
                if quiet_for >= LINGER {
                    speaker = None;
                }
            }
        }
    }
}

/// App-wide by design (final review sweep): the one output stream the app's sounds (rings, joins) play on, and the device it plays to;
/// the ring sound is refcounted over the windows that ring (`ring::effects`). No client is held.
static PLAYER: Mutex<Option<Player>> = Mutex::new(None);
static OUTPUT: Mutex<Option<String>> = Mutex::new(None);

fn lock<T>(m: &'static Mutex<T>) -> std::sync::MutexGuard<'static, T> {
    m.lock().unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// Uses `host` for the sounds from now on (tests give a fake one; the app keeps the system's). Any running player is
/// dropped, so the next sound opens a speaker on `host`.
pub fn set_host(host: Arc<dyn AudioHost>) {
    super::set_audio_host(host);
    *lock(&PLAYER) = None;
}

/// The output device the sounds play on (the `voice.output` setting; `None`: the system default).
pub fn set_output_device(output: Option<String>) {
    *lock(&OUTPUT) = output.clone();
    if let Some(player) = lock(&PLAYER).as_ref() {
        player.set_output(output);
    }
}

fn with_player(f: impl FnOnce(&Player)) {
    let mut player = lock(&PLAYER);
    let player =
        player.get_or_insert_with(|| Player::start(super::audio_host(), lock(&OUTPUT).clone()));
    f(player);
}

/// Plays a join or leave tick.
pub fn play(kind: SoundKind, _cx: &mut App) {
    with_player(|p| p.play(kind));
}

/// Starts the ring (2 pings every 2 s, at most 15 times).
pub fn start_ring(_cx: &mut App) {
    with_player(Player::start_ring);
}

/// Stops the ring.
pub fn stop_ring(_cx: &mut App) {
    with_player(Player::stop_ring);
}

#[cfg(test)]
mod tests {
    use super::*;

    fn peak(x: &[i16]) -> f32 {
        x.iter().map(|s| f32::from(s.abs())).fold(0.0, f32::max) / f32::from(i16::MAX)
    }

    fn rms(x: &[i16]) -> f32 {
        (x.iter().map(|s| f32::from(*s).powi(2)).sum::<f32>() / x.len() as f32).sqrt()
    }

    /// The fundamental of a stretch of samples, from its rising zero crossings (a triangle crosses zero once per cycle
    /// upwards).
    fn hz(x: &[i16]) -> f32 {
        let ups = x.windows(2).filter(|w| w[0] <= 0 && w[1] > 0).count();
        ups as f32 * RATE as f32 / x.len() as f32
    }

    fn ms(n: usize) -> usize {
        n * RATE as usize / 1000
    }

    /// The table of sounds.ts: `TICKS` (660/880 up for join, 880/660 down for leave), 100 ms tones 20 ms apart (220 ms in
    /// all), `RING` (2 x 120 ms pings at 1000 Hz, 40 ms apart), a peak gain of 0.2, every sound under 300 ms.
    #[test]
    fn sounds_match_synth_table() {
        assert_eq!(tick_hz(SoundKind::Join), [660.0, 880.0]);
        assert_eq!(tick_hz(SoundKind::Leave), [880.0, 660.0]);
        assert_eq!((TICK.tone_s, TICK.gap_s), (0.1, 0.02));
        assert_eq!(
            (RING.hz, RING.tone_s, RING.gap_s, RING.every_ms, RING.cycles),
            (1000.0, 0.12, 0.04, 2_000, 15)
        );
        assert_eq!(PEAK, 0.2);

        for (kind, [first, second]) in [
            (SoundKind::Join, [660.0, 880.0]),
            (SoundKind::Leave, [880.0, 660.0]),
        ] {
            let tick = synth_tick(kind);
            assert_eq!(tick.len(), ms(220), "100 + 20 + 100 ms");
            assert!(tick.len() < ms(300));
            let a = &tick[ms(10)..ms(90)];
            let b = &tick[ms(130)..ms(210)];
            assert!(
                (hz(a) - first).abs() < 15.0,
                "{kind:?}: {} vs {first}",
                hz(a)
            );
            assert!(
                (hz(b) - second).abs() < 15.0,
                "{kind:?}: {} vs {second}",
                hz(b)
            );
            // The gap between the tones is silent.
            assert!(tick[ms(101)..ms(119)].iter().all(|s| *s == 0));
            // Peak 0.2, reached and not exceeded.
            let p = peak(&tick);
            assert!((0.19..=0.2001).contains(&p), "{p}");
            // Each tone starts and ends at 0 (a 5 ms ramp, no click).
            assert_eq!(tick[0], 0);
            assert!(tick[ms(100) - 1].abs() < 400);
            assert!(tick.last().unwrap().abs() < 400);
            // The ramp is gradual: the first 5 ms never reach half the peak.
            assert!(peak(&tick[..ms(1)]) < 0.05);
        }

        let ping = synth_ping();
        assert_eq!(ping.len(), ms(280), "120 + 40 + 120 ms");
        assert!(ping.len() < ms(300));
        assert!((hz(&ping[ms(10)..ms(110)]) - 1000.0).abs() < 20.0);
        assert!((hz(&ping[ms(170)..ms(270)]) - 1000.0).abs() < 20.0);
        assert!(ping[ms(121)..ms(159)].iter().all(|s| *s == 0));
        assert!((0.19..=0.2001).contains(&peak(&ping)));
        // A triangle of peak 0.2 has an rms of 0.2 / sqrt(3).
        let level = rms(&ping[ms(10)..ms(110)]) / f32::from(i16::MAX);
        assert!((level - 0.2 / 3f32.sqrt()).abs() < 0.01, "{level}");
    }

    fn frames_until_idle(m: &mut Mixer, limit: u64) -> (u64, u64) {
        let (mut played, mut total) = (0, 0);
        for _ in 0..limit {
            if let Some(f) = m.next_frame()
                && f.iter().any(|s| *s != 0)
            {
                played += 1;
            }
            total += 1;
            if m.idle() {
                break;
            }
        }
        (played, total)
    }

    #[test]
    fn a_tick_plays_out_in_22_frames_then_the_mixer_is_idle() {
        let mut m = Mixer::new();
        assert!(m.idle() && m.next_frame().is_none());
        m.play(SoundKind::Join);
        assert!(!m.idle());
        let (played, total) = frames_until_idle(&mut m, 100);
        assert_eq!(total, 22);
        assert!(played >= 20);
        assert!(m.idle());
    }

    #[test]
    fn the_ring_pings_every_two_seconds_at_most_fifteen_times() {
        let mut m = Mixer::new();
        m.start_ring();
        assert!(m.ringing());
        let mut ping_starts = 0;
        let mut was_silent = true;
        let mut frames = 0u64;
        while m.ringing() || !m.idle() {
            let frame = m.next_frame();
            let sounding = frame.is_some_and(|f| f.iter().any(|s| *s != 0));
            if sounding && was_silent {
                ping_starts += 1;
            }
            was_silent = !sounding;
            frames += 1;
            assert!(frames < 10_000, "the ring never ends");
        }
        assert_eq!(ping_starts, 30, "R5: at most 15 pings of 2 tones");
        // 14 gaps of 2 s and the last ping's 280 ms: 28.28 s, a frame either way.
        assert!((2_820..=2_830).contains(&frames), "{frames}");
    }

    #[test]
    fn stop_ring_ends_the_ring_at_once() {
        let mut m = Mixer::new();
        m.start_ring();
        for _ in 0..10 {
            m.next_frame();
        }
        m.stop_ring();
        assert!(!m.ringing() && m.idle());
        assert!(m.next_frame().is_none());
    }

    #[test]
    fn a_tick_over_a_ring_leaves_the_ring_going() {
        let mut m = Mixer::new();
        m.start_ring();
        m.play(SoundKind::Leave);
        assert!(m.ringing());
    }
}
