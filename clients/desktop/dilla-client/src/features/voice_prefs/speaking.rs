//! Who is speaking, computed from local levels (packages/web/src/call/speaking.ts `SpeakingDetector`). The level is read
//! from what is played and stays on this device; the SFU's speaker flag is the fallback only.

/// How often a meter is sampled.
pub const SPEAK_TICK_MS: u64 = 25;
/// A level at or above [`SPEAK_RMS`] for this long switches speaking on.
pub const SPEAK_ON_MS: f64 = 50.0;
/// A level below it for this long switches speaking off.
pub const SPEAK_HOLD_MS: f64 = 300.0;
/// The RMS (0..1) from which a track counts as voiced.
pub const SPEAK_RMS: f32 = 0.02;
/// A level change reaches the state at most this often.
pub const LEVEL_PUBLISH_MS: u64 = 100;

/// On: rms >= [`SPEAK_RMS`] continuously for >= [`SPEAK_ON_MS`]. Off: below it continuously for >= [`SPEAK_HOLD_MS`].
#[derive(Clone, Debug, Default)]
pub struct SpeakingDetector {
    on: bool,
    /// Start of the run that could flip the state: above while off, below while on.
    since: Option<f64>,
}

impl SpeakingDetector {
    pub fn update(&mut self, rms: f32, now_ms: f64) -> bool {
        let voiced = rms >= SPEAK_RMS;
        if voiced == self.on {
            self.since = None;
            return self.on;
        }
        let since = *self.since.get_or_insert(now_ms);
        let needed = if self.on { SPEAK_HOLD_MS } else { SPEAK_ON_MS };
        if now_ms - since >= needed {
            self.on = voiced;
            self.since = None;
        }
        self.on
    }

    pub fn reset(&mut self) {
        self.on = false;
        self.since = None;
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn speaking_switches_on_after_50_ms_of_voice() {
        let mut d = SpeakingDetector::default();
        assert!(!d.update(0.1, 0.0));
        assert!(!d.update(0.1, 49.0));
        assert!(d.update(0.1, 50.0));
    }

    #[test]
    fn a_short_blip_does_not_switch_on() {
        let mut d = SpeakingDetector::default();
        assert!(!d.update(0.1, 0.0));
        assert!(!d.update(0.0, 30.0));
        assert!(!d.update(0.1, 40.0));
        assert!(!d.update(0.1, 80.0), "the run restarted at 40 ms");
        assert!(d.update(0.1, 90.0));
    }

    #[test]
    fn speaking_holds_300_ms_after_the_voice_stops() {
        let mut d = SpeakingDetector::default();
        d.update(0.1, 0.0);
        assert!(d.update(0.1, 60.0));
        assert!(d.update(0.0, 100.0));
        assert!(d.update(0.0, 399.0));
        assert!(!d.update(0.0, 400.0));
    }

    #[test]
    fn reset_is_silent() {
        let mut d = SpeakingDetector::default();
        d.update(0.1, 0.0);
        d.update(0.1, 60.0);
        d.reset();
        assert!(!d.update(0.0, 61.0));
    }
}
