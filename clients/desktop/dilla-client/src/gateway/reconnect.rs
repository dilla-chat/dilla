//! The reconnect backoff of gateway.ts `fail()`: `max(after_ms, min(cap, base * 2^failures) + random * jitter)`,
//! floored to whole milliseconds. `failures` counts attempts since the last `ready`; `after_ms` is a `reconnect`
//! frame's own wait, used once.

use std::time::Duration;

/// gateway.ts `GATEWAY.reconnectBaseMs`.
pub const BASE_MS: u64 = 500;
/// gateway.ts `GATEWAY.reconnectCapMs`.
pub const CAP_MS: u64 = 30_000;
/// gateway.ts `GATEWAY.reconnectJitterMs`.
pub const JITTER_MS: u64 = 500;

#[derive(Clone, Debug, Default)]
pub struct Backoff {
    failures: u32,
    after_ms: u64,
}

impl Backoff {
    pub fn new() -> Self {
        Self::default()
    }

    /// A `ready` arrived: the next failure starts again at the base.
    pub fn ready(&mut self) {
        self.failures = 0;
    }

    /// A `reconnect` frame asked for at least this wait before the next attempt (an `invalid_session` resets it to 0).
    pub fn set_after(&mut self, ms: u64) {
        self.after_ms = ms;
    }

    /// The wait before the next attempt, given `random` in `[0, 1)`; counts the failure.
    pub fn next_delay(&mut self, random: f64) -> Duration {
        let exp = BASE_MS.saturating_mul(1u64.checked_shl(self.failures).unwrap_or(u64::MAX));
        let backoff = exp.min(CAP_MS) as f64 + random.clamp(0.0, 1.0) * JITTER_MS as f64;
        let ms = (self.after_ms as f64).max(backoff).floor() as u64;
        self.failures = self.failures.saturating_add(1);
        self.after_ms = 0;
        Duration::from_millis(ms)
    }
}

/// A uniform value in `[0, 1)` for the jitter. Not cryptographic: it only spreads reconnects.
pub fn jitter() -> f64 {
    use std::hash::{BuildHasher, Hasher};
    let mut h = std::collections::hash_map::RandomState::new().build_hasher();
    h.write_u128(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_nanos()),
    );
    (h.finish() >> 11) as f64 / (1u64 << 53) as f64
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn doubles_from_base_to_cap_with_jitter() {
        let mut b = Backoff::new();
        let ms: Vec<u128> = (0..9).map(|_| b.next_delay(0.0).as_millis()).collect();
        assert_eq!(
            ms,
            [500, 1000, 2000, 4000, 8000, 16000, 30000, 30000, 30000]
        );
        let mut b = Backoff::new();
        assert_eq!(b.next_delay(0.999).as_millis(), 999);
        for _ in 0..80 {
            b.next_delay(0.5);
        }
        assert_eq!(
            b.next_delay(0.5).as_millis(),
            30_250,
            "no overflow far past the cap"
        );
    }

    #[test]
    fn ready_resets_and_after_ms_wins_once() {
        let mut b = Backoff::new();
        b.next_delay(0.0);
        b.next_delay(0.0);
        b.ready();
        b.set_after(5_000);
        assert_eq!(b.next_delay(0.0).as_millis(), 5_000);
        assert_eq!(
            b.next_delay(0.0).as_millis(),
            1_000,
            "after_ms is used once; failures counted on"
        );
    }

    #[test]
    fn jitter_in_range() {
        for _ in 0..100 {
            let j = jitter();
            assert!((0.0..1.0).contains(&j));
        }
    }
}
