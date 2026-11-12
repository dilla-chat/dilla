//! The voice-activity gate on the input level in dB (packages/media/src/audio/sensitivity.ts `SensitivityGate`,
//! README §5.9 Input sensitivity). It runs per 10 ms frame; the Settings meter's line shows the threshold the call
//! uses, because both ask this one gate.

use std::collections::VecDeque;

/// The automatic line before any room noise was measured (the prototype's resting -48 dB).
pub const AUTO_START_DB: f32 = -48.0;
pub const MIN_DB: f32 = -100.0;
pub const MAX_DB: f32 = 0.0;
/// The automatic threshold sits this far above the noise floor...
const AUTO_MARGIN_DB: f32 = 15.0;
/// ...but never below or above these: a dead-silent input still needs a voice, a loud room never needs a shout.
const AUTO_MIN_DB: f32 = -70.0;
const AUTO_MAX_DB: f32 = -30.0;
/// The floor is the quietest frame of the last 1.5 s (15 blocks of 10 frames).
const FLOOR_BLOCK_FRAMES: u32 = 10;
const FLOOR_BLOCKS: usize = 15;
/// The floor rises toward a louder room at most this fast.
const FLOOR_RISE_DB_PER_S: f32 = 6.0;
/// Attack is one frame; the gate stays open this long after the last frame over the line.
pub const HOLD_MS: f64 = 300.0;
/// One frame.
pub const FRAME_MS: f64 = 10.0;

/// What the gate is told: off (push to talk), the automatic threshold, or a fixed one.
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct SensitivityConfig {
    /// False: the gate stays open (push to talk governs the microphone instead).
    pub enabled: bool,
    /// Automatic sensitivity: the threshold follows the room's noise floor.
    pub auto: bool,
    /// The manual threshold, -100..0 dB; ignored while `auto`.
    pub threshold_db: f32,
}

impl SensitivityConfig {
    /// A config with every value usable: a non-finite threshold reads as -50.
    pub fn sanitized(self) -> Self {
        Self {
            threshold_db: if self.threshold_db.is_finite() {
                self.threshold_db.clamp(MIN_DB, MAX_DB)
            } else {
                -50.0
            },
            ..self
        }
    }
}

impl Default for SensitivityConfig {
    fn default() -> Self {
        Self {
            enabled: true,
            auto: true,
            threshold_db: -50.0,
        }
    }
}

/// A frame's RMS (0..1 full scale) in dBFS, clamped to -100..0.
pub fn db_of(rms: f32) -> f32 {
    if rms > 0.0 {
        (20.0 * rms.log10()).clamp(MIN_DB, MAX_DB)
    } else {
        MIN_DB
    }
}

/// JavaScript's `Math.round`: halves go up.
fn js_round(x: f32) -> f32 {
    (x + 0.5).floor()
}

#[derive(Clone, Debug)]
pub struct SensitivityGate {
    cfg: SensitivityConfig,
    floor: Option<f32>,
    blocks: VecDeque<f32>,
    block_min: f32,
    block_frames: u32,
    open: bool,
    last_over_ms: Option<f64>,
    last_ms: Option<f64>,
}

impl Default for SensitivityGate {
    fn default() -> Self {
        Self::new(SensitivityConfig::default())
    }
}

impl SensitivityGate {
    pub fn new(cfg: SensitivityConfig) -> Self {
        Self {
            cfg: cfg.sanitized(),
            floor: None,
            blocks: VecDeque::new(),
            block_min: MAX_DB,
            block_frames: 0,
            open: false,
            last_over_ms: None,
            last_ms: None,
        }
    }

    pub fn configure(&mut self, cfg: SensitivityConfig) {
        self.cfg = cfg.sanitized();
    }

    /// The threshold in force: the manual one, or the automatic one from the measured floor.
    pub fn threshold(&self) -> f32 {
        if !self.cfg.auto {
            return self.cfg.threshold_db;
        }
        match self.floor {
            None => AUTO_START_DB,
            Some(floor) => js_round((floor + AUTO_MARGIN_DB).clamp(AUTO_MIN_DB, AUTO_MAX_DB)),
        }
    }

    /// The floor from every frame: each 100 ms block's minimum, the floor the minimum of the last 15 blocks.
    fn learn(&mut self, db: f32, dt_ms: f64) {
        if db < self.block_min {
            self.block_min = db;
        }
        self.block_frames += 1;
        if self.block_frames < FLOOR_BLOCK_FRAMES {
            return;
        }
        self.blocks.push_back(self.block_min);
        if self.blocks.len() > FLOOR_BLOCKS {
            self.blocks.pop_front();
        }
        self.block_min = MAX_DB;
        let elapsed = f64::from(self.block_frames) * dt_ms;
        self.block_frames = 0;
        let window_min = self.blocks.iter().copied().fold(f32::INFINITY, f32::min);
        self.floor = Some(match self.floor {
            None => window_min,
            Some(floor) if window_min <= floor => window_min,
            Some(floor) => window_min.min(floor + FLOOR_RISE_DB_PER_S * (elapsed / 1000.0) as f32),
        });
    }

    /// One frame's level in dB at `now_ms`; true while the microphone may be heard.
    pub fn update(&mut self, db: f32, now_ms: f64) -> bool {
        let dt = self
            .last_ms
            .map_or(FRAME_MS, |last| (now_ms - last).clamp(0.0, 1000.0));
        self.last_ms = Some(now_ms);
        self.learn(if db.is_finite() { db } else { MIN_DB }, dt);
        let threshold = self.threshold();
        if !self.cfg.enabled {
            self.open = true;
            return true;
        }
        if db >= threshold {
            self.open = true;
            self.last_over_ms = Some(now_ms);
        } else if self.open && self.last_over_ms.is_none_or(|over| now_ms - over > HOLD_MS) {
            self.open = false;
        }
        self.open
    }

    /// A mute: closed, the hold forgotten (the floor is kept: it is the room's).
    pub fn reset(&mut self) {
        self.open = false;
        self.last_over_ms = None;
        self.last_ms = None;
    }

    /// A new capture (another device, other processing): the room is measured again from the start.
    pub fn reset_floor(&mut self) {
        self.reset();
        self.floor = None;
        self.blocks.clear();
        self.block_min = MAX_DB;
        self.block_frames = 0;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn run(gate: &mut SensitivityGate, db: f32, frames: u32, from_ms: f64) -> (bool, f64) {
        let mut now = from_ms;
        let mut open = false;
        for _ in 0..frames {
            now += FRAME_MS;
            open = gate.update(db, now);
        }
        (open, now)
    }

    fn manual(threshold_db: f32) -> SensitivityGate {
        SensitivityGate::new(SensitivityConfig {
            enabled: true,
            auto: false,
            threshold_db,
        })
    }

    #[test]
    fn a_level_under_a_manual_threshold_is_not_sent() {
        // sensitivity.spec.ts: the fake microphone's tone (about -9 dBFS) under a line at 0 dB is not heard.
        let mut gate = manual(0.0);
        let (open, _) = run(&mut gate, -9.0, 100, 0.0);
        assert!(!open);
        // With the line at -100 dB it is heard.
        gate.configure(SensitivityConfig {
            enabled: true,
            auto: false,
            threshold_db: -100.0,
        });
        assert!(gate.update(-9.0, 1010.0));
    }

    #[test]
    fn one_frame_over_the_line_opens_and_the_hold_keeps_it_open() {
        let mut gate = manual(-50.0);
        let (_, now) = run(&mut gate, -90.0, 5, 0.0);
        assert!(gate.update(-40.0, now + 10.0));
        // Quiet again: open for the hold (300 ms), closed after it.
        let (open, _) = run(&mut gate, -90.0, 30, now + 10.0);
        assert!(open, "30 frames = 300 ms: still held");
        assert!(!gate.update(-90.0, now + 10.0 + 310.0 + 10.0));
    }

    #[test]
    fn a_disabled_gate_stays_open() {
        let mut gate = SensitivityGate::new(SensitivityConfig {
            enabled: false,
            auto: false,
            threshold_db: 0.0,
        });
        assert!(gate.update(-100.0, 10.0));
    }

    #[test]
    fn automatic_starts_at_minus_48_then_follows_the_floor_plus_15() {
        let mut gate = SensitivityGate::default();
        assert_eq!(gate.threshold(), -48.0);
        // A room at -60 dB: after the first 100 ms block the floor is -60, the line -45.
        run(&mut gate, -60.0, 10, 0.0);
        assert_eq!(gate.threshold(), -45.0);
        // Never below -70 or above -30.
        let mut quiet = SensitivityGate::default();
        run(&mut quiet, -100.0, 10, 0.0);
        assert_eq!(quiet.threshold(), -70.0);
        let mut loud = SensitivityGate::default();
        run(&mut loud, -5.0, 10, 0.0);
        assert_eq!(loud.threshold(), -30.0);
    }

    #[test]
    fn the_floor_rises_slowly_toward_a_louder_room() {
        let mut gate = SensitivityGate::default();
        run(&mut gate, -80.0, 10, 0.0);
        assert_eq!(gate.threshold(), -65.0);
        // The window still holds the -80 block for 1.5 s, so the floor cannot rise yet.
        run(&mut gate, -40.0, 100, 100.0);
        assert_eq!(gate.threshold(), -65.0);
        // After the window moved on, the floor rises at most 6 dB per second.
        run(&mut gate, -40.0, 100, 1100.0);
        let t = gate.threshold();
        assert!(t > -65.0 && t < -25.0, "{t}");
    }

    #[test]
    fn reset_floor_measures_the_room_again() {
        let mut gate = SensitivityGate::default();
        run(&mut gate, -60.0, 10, 0.0);
        gate.reset_floor();
        assert_eq!(gate.threshold(), AUTO_START_DB);
    }

    #[test]
    fn db_of_clamps_and_a_silent_frame_is_minus_100() {
        assert_eq!(db_of(0.0), -100.0);
        assert_eq!(db_of(1.0), 0.0);
        assert_eq!(db_of(2.0), 0.0);
        assert!((db_of(0.1) + 20.0).abs() < 1e-4);
        assert_eq!(db_of(1e-9), -100.0);
    }

    #[test]
    fn a_non_finite_threshold_reads_as_the_default() {
        let cfg = SensitivityConfig {
            enabled: true,
            auto: false,
            threshold_db: f32::NAN,
        };
        assert_eq!(cfg.sanitized().threshold_db, -50.0);
        assert_eq!(
            SensitivityConfig {
                threshold_db: 40.0,
                ..cfg
            }
            .sanitized()
            .threshold_db,
            0.0
        );
    }
}
