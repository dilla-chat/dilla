//! Slow mode in the composer (web `conversation/slow.ts`, flow 11): the wait as text, and when the countdown is spoken.

/// The steps a channel's slow mode takes, in seconds, with the string key of each (flow 11, in order).
pub const SLOW_STEPS: [(u32, &str); 9] = [
    (0, "slow.off"),
    (5, "slow.s5"),
    (10, "slow.s10"),
    (30, "slow.s30"),
    (60, "slow.m1"),
    (300, "slow.m5"),
    (900, "slow.m15"),
    (3600, "slow.h1"),
    (21_600, "slow.h6"),
];

/// A wait as `m:ss`, or `h:mm:ss` from an hour; rounded up to whole seconds (R7).
pub fn format_wait(ms: i64) -> String {
    let total = (ms.max(0) + 999) / 1000;
    let (h, m, s) = (total / 3600, (total % 3600) / 60, total % 60);
    if h > 0 {
        format!("{h}:{m:02}:{s:02}")
    } else {
        format!("{m}:{s:02}")
    }
}

/// The spoken countdown changes only when this value does: the minute while more than a minute is left, else the 10 s
/// step (flow 11: a polite status, never every second). Empty when the wait is over.
pub fn spoken_step(remaining_ms: i64) -> String {
    if remaining_ms <= 0 {
        return String::new();
    }
    let seconds = (remaining_ms + 999) / 1000;
    if seconds > 60 {
        format!("m{}", (seconds + 59) / 60)
    } else {
        format!("s{}", (seconds + 9) / 10)
    }
}

/// When the next post may go, in ms: the channel's own latest confirmed post plus its slow-mode seconds; 0 when slow mode
/// is off, the person bypasses it, or the wait has passed. `last_own` is that post's time in unix seconds.
pub fn wait_until_ms(slowmode: u32, last_own: Option<u64>, bypass: bool, now_ms: u64) -> u64 {
    if slowmode == 0 || bypass {
        return 0;
    }
    let Some(last) = last_own else { return 0 };
    let until = (last + u64::from(slowmode)) * 1000;
    if until > now_ms { until } else { 0 }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::strings::try_t;

    #[test]
    fn the_wait_is_clock_text_rounded_up() {
        assert_eq!(format_wait(0), "0:00");
        assert_eq!(format_wait(1), "0:01");
        assert_eq!(format_wait(59_001), "1:00");
        assert_eq!(format_wait(61_000), "1:01");
        assert_eq!(format_wait(3_600_000), "1:00:00");
        assert_eq!(format_wait(3_661_000), "1:01:01");
        assert_eq!(format_wait(-5), "0:00");
    }

    #[test]
    fn the_spoken_step_changes_by_the_minute_then_by_ten_seconds() {
        assert_eq!(spoken_step(0), "");
        assert_eq!(spoken_step(61_000), "m2");
        assert_eq!(spoken_step(60_000), "s6");
        assert_eq!(spoken_step(59_000), "s6");
        assert_eq!(spoken_step(50_000), "s5");
        assert_eq!(spoken_step(1), "s1");
        // The same minute speaks once.
        assert_eq!(spoken_step(120_000 - 999), spoken_step(61_001));
    }

    #[test]
    fn every_step_has_its_string() {
        for (_, key) in SLOW_STEPS {
            assert!(try_t(key).is_some(), "{key}");
        }
        assert!(SLOW_STEPS.windows(2).all(|w| w[0].0 < w[1].0));
    }

    #[test]
    fn the_wait_runs_from_the_last_own_post() {
        let now = 1_000_000_000;
        assert_eq!(wait_until_ms(0, Some(now / 1000), false, now), 0);
        assert_eq!(wait_until_ms(30, None, false, now), 0);
        assert_eq!(
            wait_until_ms(30, Some(now / 1000 - 10), false, now),
            (now / 1000 + 20) * 1000
        );
        assert_eq!(wait_until_ms(30, Some(now / 1000 - 31), false, now), 0);
        assert_eq!(wait_until_ms(30, Some(now / 1000), true, now), 0, "bypass");
    }
}
