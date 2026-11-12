//! Reminder times (packages/client-core/src/bot/utility-time.ts): a duration (`1h30m`) or a date and time, read in a
//! named zone without the host's own zone. The zone is a name and a function from an instant to its UTC offset, so the
//! same rules run against the OS zone in the app and against fixed rules in tests.

use crate::features::bot::registry::iso_parts;

const MIN_REMINDER_SECONDS: i64 = 60;
const MAX_REMINDER_SECONDS: i64 = 365 * 24 * 60 * 60;
const MAX_UNIX_SECONDS: i64 = 253_402_300_799;

/// A time zone: its IANA name and the offset (seconds east of UTC) in force at each instant.
#[derive(Clone, Copy)]
pub struct ReminderZone<'a> {
    pub name: &'a str,
    pub offset_at: &'a dyn Fn(i64) -> i32,
}

/// One side of a fold (`ReminderTimeCandidate`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Candidate {
    pub unix_seconds: i64,
    pub offset_label: String,
    pub preview: String,
}

/// `ReminderTimeResult`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum ReminderTime {
    Relative { seconds: i64, preview: String },
    Absolute { unix_seconds: i64, preview: String },
    Ambiguous([Candidate; 2]),
    Invalid(&'static str),
}

/// `normalizeReminderTimezone`'s refusals: `Local`, a fixed offset, or a name outside 1..=100 bytes.
pub fn valid_zone_name(name: &str) -> bool {
    if name.is_empty() || name.len() > 100 || name == "Local" {
        return false;
    }
    let fixed = name
        .strip_prefix(['+', '-'])
        .is_some_and(|r| r.len() >= 2 && r.bytes().take(2).all(|c| c.is_ascii_digit()));
    !fixed
}

/// `parseDurationSeconds`: contiguous `<n>[.<n>]<unit>` tokens, whole seconds only.
fn duration_seconds(value: &str) -> Option<i64> {
    if value.is_empty() {
        return None;
    }
    let b = value.as_bytes();
    let mut millis = 0f64;
    let mut i = 0;
    while i < b.len() {
        let start = i;
        while i < b.len() && b[i].is_ascii_digit() {
            i += 1;
        }
        if i == start {
            return None;
        }
        if i < b.len() && b[i] == b'.' {
            i += 1;
            let fraction = i;
            while i < b.len() && b[i].is_ascii_digit() {
                i += 1;
            }
            if i == fraction {
                return None;
            }
        }
        let amount: f64 = value[start..i].parse().ok()?;
        if !amount.is_finite() {
            return None;
        }
        let scale = match b.get(i) {
            Some(b'm') if b.get(i + 1) == Some(&b's') => {
                i += 2;
                1.0
            }
            Some(&u) => {
                i += 1;
                match u {
                    b's' => 1_000.0,
                    b'm' => 60_000.0,
                    b'h' => 3_600_000.0,
                    b'd' => 86_400_000.0,
                    b'w' => 604_800_000.0,
                    _ => return None,
                }
            }
            None => return None,
        };
        millis += amount * scale;
        if !millis.is_finite() {
            return None;
        }
    }
    let seconds = millis / 1000.0;
    if seconds.fract() != 0.0 || seconds > i64::MAX as f64 {
        return None;
    }
    Some(seconds as i64)
}

/// Days from 1970-01-01 to the civil date (proleptic Gregorian).
fn days_from_civil(y: i64, m: i64, d: i64) -> i64 {
    let y = if m <= 2 { y - 1 } else { y };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let mp = (m + 9) % 12;
    let doy = (153 * mp + 2) / 5 + d - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    era * 146_097 + doe - 719_468
}

/// The civil parts of a unix second (UTC).
fn civil(secs: i64) -> (i64, i64, i64, i64, i64, i64) {
    let days = secs.div_euclid(86_400);
    let rem = secs.rem_euclid(86_400);
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = doy - (153 * mp + 2) / 5 + 1;
    let m = if mp < 10 { mp + 3 } else { mp - 9 };
    let y = yoe + era * 400 + i64::from(m <= 2);
    (y, m, d, rem / 3600, rem % 3600 / 60, rem % 60)
}

type Parts = (i64, i64, i64, i64, i64, i64);

fn local_as_utc(p: Parts) -> i64 {
    days_from_civil(p.0, p.1, p.2) * 86_400 + p.3 * 3600 + p.4 * 60 + p.5
}

fn parts_at(unix: i64, zone: ReminderZone<'_>) -> Parts {
    civil(unix + i64::from((zone.offset_at)(unix)))
}

fn offset_label(offset_seconds: i32) -> String {
    let sign = if offset_seconds < 0 { '-' } else { '+' };
    let minutes = offset_seconds.unsigned_abs() / 60;
    format!("UTC{sign}{:02}:{:02}", minutes / 60, minutes % 60)
}

/// `formatPreview`: `2026-01-01 00:01:00 (UTC, UTC+00:00)`.
pub fn preview(unix: i64, zone: ReminderZone<'_>) -> String {
    let (y, mo, d, h, mi, s) = parts_at(unix, zone);
    format!(
        "{y:04}-{mo:02}-{d:02} {h:02}:{mi:02}:{s:02} ({}, {})",
        zone.name,
        offset_label((zone.offset_at)(unix))
    )
}

fn candidate(unix: i64, zone: ReminderZone<'_>) -> Candidate {
    Candidate {
        unix_seconds: unix,
        offset_label: offset_label((zone.offset_at)(unix)),
        preview: preview(unix, zone),
    }
}

fn outside_window(unix: i64, now: i64) -> bool {
    let from_now = unix - now;
    !(MIN_REMINDER_SECONDS..=MAX_REMINDER_SECONDS).contains(&from_now)
}

/// `parseReminderTime` at `now` (unix seconds) in `zone`.
pub fn parse_reminder_time(input: &str, zone: ReminderZone<'_>, now: i64) -> ReminderTime {
    if !valid_zone_name(zone.name) {
        return ReminderTime::Invalid("invalid-timezone");
    }
    let trimmed = input.trim();
    if let Some(seconds) = duration_seconds(trimmed) {
        if !(MIN_REMINDER_SECONDS..=MAX_REMINDER_SECONDS).contains(&seconds) {
            return ReminderTime::Invalid("outside-range");
        }
        let due = now + seconds;
        if !(1..=MAX_UNIX_SECONDS).contains(&due) {
            return ReminderTime::Invalid("outside-range");
        }
        return ReminderTime::Relative {
            seconds,
            preview: preview(due, zone),
        };
    }
    let Some((y, mo, d, h, mi, s, offset, fraction_nonzero)) = iso_parts(trimmed) else {
        return ReminderTime::Invalid(if trimmed.is_empty() {
            "invalid-format"
        } else {
            "invalid-date"
        });
    };
    if fraction_nonzero {
        return ReminderTime::Invalid("invalid-date");
    }
    let parts: Parts = (
        i64::from(y),
        i64::from(mo),
        i64::from(d),
        i64::from(h),
        i64::from(mi),
        i64::from(s),
    );
    let base = local_as_utc(parts);
    let absolute = |unix: i64| {
        if !(1..=MAX_UNIX_SECONDS).contains(&unix) || outside_window(unix, now) {
            ReminderTime::Invalid("outside-range")
        } else {
            ReminderTime::Absolute {
                unix_seconds: unix,
                preview: preview(unix, zone),
            }
        }
    };
    if let Some(minutes) = offset {
        return absolute(base - i64::from(minutes) * 60);
    }
    // Every offset the zone uses within two days either side, sampled each 30 minutes (half-hour shifts included).
    let mut offsets: Vec<i32> = (-96..=96)
        .map(|k| (zone.offset_at)(base + k * 1800))
        .collect();
    offsets.sort_unstable();
    offsets.dedup();
    let mut matches: Vec<i64> = offsets
        .iter()
        .map(|&o| base - i64::from(o))
        .filter(|&c| parts_at(c, zone) == parts)
        .collect();
    matches.sort_unstable();
    matches.dedup();
    match matches.as_slice() {
        [] => ReminderTime::Invalid("nonexistent-local-time"),
        [only] => absolute(*only),
        [first, second, ..] => {
            ReminderTime::Ambiguous([candidate(*first, zone), candidate(*second, zone)])
        }
    }
}

/// `resolveReminderTimeCandidate`: one side of a fold, chosen explicitly, checked against the window at `now`.
pub fn resolve_candidate(result: &ReminderTime, unix: i64, now: i64) -> ReminderTime {
    let ReminderTime::Ambiguous(candidates) = result else {
        return ReminderTime::Invalid("not-ambiguous");
    };
    let Some(chosen) = candidates.iter().find(|c| c.unix_seconds == unix) else {
        return ReminderTime::Invalid("invalid-selection");
    };
    if outside_window(unix, now) || !(1..=MAX_UNIX_SECONDS).contains(&unix) {
        return ReminderTime::Invalid("outside-range");
    }
    ReminderTime::Absolute {
        unix_seconds: unix,
        preview: chosen.preview.clone(),
    }
}

#[cfg(test)]
mod tests {
    //! Ported from packages/client-core/src/bot/utility-time.test.ts. The zones are the 2026 rules of each IANA zone.
    use super::*;

    const NOW: i64 = 1_767_225_600; // 2026-01-01T00:00:00Z

    fn utc(_: i64) -> i32 {
        0
    }
    fn kathmandu(_: i64) -> i32 {
        5 * 3600 + 45 * 60
    }
    /// Europe/Stockholm 2026: CEST from 03-29 01:00Z to 10-25 01:00Z.
    fn stockholm(t: i64) -> i32 {
        if (1_774_746_000..1_792_890_000).contains(&t) {
            7200
        } else {
            3600
        }
    }
    /// America/New_York 2026: EDT from 03-08 07:00Z to 11-01 06:00Z.
    fn new_york(t: i64) -> i32 {
        if (1_772_953_200..1_793_512_800).contains(&t) {
            -4 * 3600
        } else {
            -5 * 3600
        }
    }
    /// Australia/Lord_Howe 2026: +11 until 04-04 15:00Z, +10:30 until 10-03 15:30Z, then +11.
    fn lord_howe(t: i64) -> i32 {
        if (1_775_314_800..1_791_041_400).contains(&t) {
            37_800
        } else {
            39_600
        }
    }

    fn zone<'a>(name: &'a str, f: &'a dyn Fn(i64) -> i32) -> ReminderZone<'a> {
        ReminderZone { name, offset_at: f }
    }

    fn parse(input: &str, name: &str, f: fn(i64) -> i32, now: i64) -> ReminderTime {
        parse_reminder_time(input, zone(name, &f), now)
    }

    #[test]
    fn relative_durations_and_previews() {
        assert_eq!(
            parse("1m", "UTC", utc, NOW),
            ReminderTime::Relative {
                seconds: 60,
                preview: "2026-01-01 00:01:00 (UTC, UTC+00:00)".into()
            }
        );
        assert_eq!(
            parse("1.5m", "Asia/Kathmandu", kathmandu, NOW),
            ReminderTime::Relative {
                seconds: 90,
                preview: "2026-01-01 05:46:30 (Asia/Kathmandu, UTC+05:45)".into()
            }
        );
        assert_eq!(
            parse("365d", "UTC", utc, NOW),
            ReminderTime::Relative {
                seconds: 31_536_000,
                preview: "2027-01-01 00:00:00 (UTC, UTC+00:00)".into()
            }
        );
    }

    #[test]
    fn duration_grammar() {
        let long = format!("{}1m", "0".repeat(4096));
        for (input, seconds) in [
            ("00001m", 60),
            ("00001.500m", 90),
            ("0w0d0h1m0s0ms", 60),
            ("1h30m", 5_400),
            ("1m1h1m", 3_720),
            (long.as_str(), 60),
            ("500ms59s500ms", 60),
            ("00060s", 60),
            ("365d0ms", 31_536_000),
            (" 1m ", 60),
        ] {
            assert!(
                matches!(parse(input, "UTC", utc, NOW), ReminderTime::Relative { seconds: s, .. } if s == seconds),
                "{input}"
            );
        }
        let bad = [
            format!("{}x", "0".repeat(4096)),
            format!("{}.x", "0".repeat(4096)),
            format!("0.{}x", "0".repeat(4096)),
            format!("1m{}x", "0".repeat(4096)),
        ];
        for input in bad.iter().map(String::as_str).chain([
            "1m 30s", "1m-30s", "1m.30s", ".5m", "1.m", "1.2.3m", "1M", "1m1", "1m30sx",
        ]) {
            assert_eq!(
                parse(input, "UTC", utc, NOW),
                ReminderTime::Invalid("invalid-date"),
                "{input}"
            );
        }
        let huge = format!("{}m", "9".repeat(400));
        for input in [
            "0m",
            "59s999ms",
            "365d1s",
            huge.as_str(),
            "59s",
            "366d",
            "1.001s",
            "1ms",
            "",
        ] {
            assert!(
                matches!(parse(input, "UTC", utc, NOW), ReminderTime::Invalid(_)),
                "{input}"
            );
        }
    }

    #[test]
    fn fixed_offset_zones_are_refused() {
        assert_eq!(
            parse("1m", "+01:00", utc, NOW),
            ReminderTime::Invalid("invalid-timezone")
        );
        assert_eq!(
            parse("1m", "Local", utc, NOW),
            ReminderTime::Invalid("invalid-timezone")
        );
    }

    #[test]
    fn absolute_bounds_and_zones() {
        assert!(matches!(
            parse("2026-01-01T00:01:00Z", "UTC", utc, NOW),
            ReminderTime::Absolute {
                unix_seconds: 1_767_225_660,
                ..
            }
        ));
        assert!(matches!(
            parse("2027-01-01T00:00:00Z", "UTC", utc, NOW),
            ReminderTime::Absolute {
                unix_seconds: 1_798_761_600,
                ..
            }
        ));
        assert_eq!(
            parse("2026-01-01 02:30", "Europe/Stockholm", stockholm, NOW),
            ReminderTime::Absolute {
                unix_seconds: 1_767_231_000,
                preview: "2026-01-01 02:30:00 (Europe/Stockholm, UTC+01:00)".into()
            }
        );
        assert_eq!(
            parse("2026-01-02T00:00+02:00", "Europe/Stockholm", stockholm, NOW),
            ReminderTime::Absolute {
                unix_seconds: 1_767_304_800,
                preview: "2026-01-01 23:00:00 (Europe/Stockholm, UTC+01:00)".into()
            }
        );
        assert_eq!(
            parse("2026-01-02T00:00Z", "UTC", utc, NOW),
            ReminderTime::Absolute {
                unix_seconds: 1_767_312_000,
                preview: "2026-01-02 00:00:00 (UTC, UTC+00:00)".into()
            }
        );
        assert_eq!(
            parse("2026-01-01T12:30", "Asia/Kathmandu", kathmandu, NOW),
            ReminderTime::Absolute {
                unix_seconds: 1_767_249_900,
                preview: "2026-01-01 12:30:00 (Asia/Kathmandu, UTC+05:45)".into()
            }
        );
    }

    #[test]
    fn nonexistent_wall_times() {
        for (name, f, input) in [
            (
                "Europe/Stockholm",
                stockholm as fn(i64) -> i32,
                "2026-03-29T02:30",
            ),
            ("America/New_York", new_york, "2026-03-08T02:30"),
            ("Australia/Lord_Howe", lord_howe, "2026-10-04T02:15"),
        ] {
            assert_eq!(
                parse(input, name, f, NOW),
                ReminderTime::Invalid("nonexistent-local-time"),
                "{name}"
            );
        }
    }

    #[test]
    fn both_overlap_choices() {
        for (name, f, input, instants, offsets) in [
            (
                "Europe/Stockholm",
                stockholm as fn(i64) -> i32,
                "2026-10-25T02:30",
                [1_792_888_200, 1_792_891_800],
                ["UTC+02:00", "UTC+01:00"],
            ),
            (
                "America/New_York",
                new_york,
                "2026-11-01T01:30",
                [1_793_511_000, 1_793_514_600],
                ["UTC-04:00", "UTC-05:00"],
            ),
            (
                "Australia/Lord_Howe",
                lord_howe,
                "2026-04-05T01:45",
                [1_775_313_900, 1_775_315_700],
                ["UTC+11:00", "UTC+10:30"],
            ),
        ] {
            let ReminderTime::Ambiguous([a, b]) = parse(input, name, f, NOW) else {
                panic!("{name}: not ambiguous");
            };
            assert_eq!([a.unix_seconds, b.unix_seconds], instants, "{name}");
            assert_eq!(
                [a.offset_label.as_str(), b.offset_label.as_str()],
                offsets,
                "{name}"
            );
        }
    }

    #[test]
    fn a_passed_fold_side_is_bounded_when_chosen() {
        let during = 1_792_889_100; // 2026-10-25T00:45:00Z
        let result = parse("2026-10-25T02:30", "Europe/Stockholm", stockholm, during);
        assert!(matches!(result, ReminderTime::Ambiguous(_)));
        assert_eq!(
            resolve_candidate(&result, 1_792_888_200, during),
            ReminderTime::Invalid("outside-range")
        );
        assert_eq!(
            resolve_candidate(&result, 1_792_891_800, during),
            ReminderTime::Absolute {
                unix_seconds: 1_792_891_800,
                preview: "2026-10-25 02:30:00 (Europe/Stockholm, UTC+01:00)".into()
            }
        );
        assert!(matches!(
            resolve_candidate(&result, 1_792_891_801, during),
            ReminderTime::Invalid(_)
        ));
    }

    #[test]
    fn invalid_dates_and_windows() {
        for input in [
            "2026-02-30T09:30",
            "2026-01-01T24:00",
            "2026-01-01T00:00+24:00",
            "2026-01-01T00:00Z",
        ] {
            assert!(
                matches!(parse(input, "UTC", utc, NOW), ReminderTime::Invalid(_)),
                "{input}"
            );
        }
        assert_eq!(
            parse("2026-01-01T00:00:30Z", "UTC", utc, NOW),
            ReminderTime::Invalid("outside-range")
        );
        assert_eq!(
            parse("2027-01-01T00:01:00Z", "UTC", utc, NOW),
            ReminderTime::Invalid("outside-range")
        );
    }
}
