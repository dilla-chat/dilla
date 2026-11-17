//! Times, day dividers and relative times, as the web client draws them (`packages/ui/src/strings/index.ts` and
//! `packages/web/src/screens/conversation/log-model.ts`).
//!
//! - **Times are always 24-hour `HH:MM`** (hour cycle h23), whatever the region: the design draws them so.
//! - **Dates follow the system region** ([`date_locale`], the counterpart of the web's `dateLocale()`): `Oct 3, 2026` under
//!   en-US, `3 Oct 2026` under en-GB, `3 okt. 2026` under sv-SE. A region this table does not know draws en-GB's form.
//!
//! The public functions read the OS locale (`sys-locale`) and the local zone. The `*_in` functions take both, for tests and
//! fixtures.

use jiff::{
    Timestamp,
    civil::{Date, DateTime},
    tz::{Offset, TimeZone},
};

use crate::strings::{t, tf};

/// The locale dates follow: the OS region as a BCP 47 tag, `en-GB` when the OS gives none (the web's fallback).
pub fn date_locale() -> String {
    sys_locale::get_locale()
        .map(|raw| canonical(&raw))
        .filter(|tag| !tag.is_empty())
        .unwrap_or_else(|| "en-GB".to_owned())
}

/// `sv_SE.UTF-8` and `en_US@posix` to `sv-SE` and `en-US`.
pub fn canonical(raw: &str) -> String {
    let base = raw.split(['.', '@']).next().unwrap_or("");
    base.replace('_', "-")
}

/// A time zone as a function from an instant (unix seconds) to the offset in force at it: each instant gets its own offset, so
/// a message sent in summer time and read in winter shows the time it was sent at.
pub type Zone<'a> = &'a dyn Fn(i64) -> Offset;

/// The OS local zone: the offset the local rules give at that very instant.
pub fn local_zone(secs: i64) -> Offset {
    Timestamp::from_second(secs).map_or(Offset::UTC, |t| TimeZone::system().to_offset(t))
}

/// An instant (unix seconds) as the wall clock `offset` gives; an instant out of range reads as the epoch.
pub fn wall_clock(secs: i64, offset: Offset) -> DateTime {
    offset.to_datetime(Timestamp::from_second(secs).unwrap_or(Timestamp::UNIX_EPOCH))
}

/// A calendar day as a day count (days since 1970-01-01): two days' difference is the days between them.
pub fn day_number(date: Date) -> i64 {
    date.until((jiff::Unit::Day, Date::constant(1970, 1, 1)))
        .map_or(0, |span| -i64::from(span.get_days()))
}

fn at(ts: u64, zone: Zone) -> DateTime {
    let secs = i64::try_from(ts).unwrap_or(i64::MAX);
    wall_clock(secs, zone(secs))
}

/// A message time: 24-hour `HH:MM` in the local zone.
pub fn time(ts: u64) -> String {
    time_in(ts, &local_zone)
}

/// The date of `ts` in the system region (the web's `formatDay`): `Oct 3, 2026`.
pub fn day(ts: u64) -> String {
    day_in(ts, &local_zone, &date_locale())
}

/// The date and the 24-hour time (the web's `formatDayTime`): `Oct 3, 2026, 14:05`.
pub fn day_time(ts: u64) -> String {
    let when = at(ts, &local_zone);
    format!("{}, {}", fmt_day(&when, &date_locale()), fmt_time(&when))
}

/// A diagnostics clock, 24-hour `HH:MM:SS` from milliseconds (the web's `formatClock`).
pub fn clock(ms: u64) -> String {
    clock_in(ms / 1000, &local_zone)
}

/// The day divider of a message log: `today`, `yesterday`, else the date (the web's `dayLabel`). `now` is the clock the
/// log was rendered with, in unix seconds.
pub fn day_divider(ts: u64, now: u64) -> String {
    day_divider_in(ts, now, &local_zone, &date_locale())
}

/// A time relative to `now`: the time today, `yesterday, 14:05` the day before, else the date and the time. Both are unix
/// seconds.
pub fn relative(ts: u64, now: u64) -> String {
    relative_in(ts, now, &local_zone, &date_locale())
}

fn fmt_time(when: &DateTime) -> String {
    format!("{:02}:{:02}", when.hour(), when.minute())
}

pub fn time_in(ts: u64, zone: Zone) -> String {
    fmt_time(&at(ts, zone))
}

pub fn clock_in(ts: u64, zone: Zone) -> String {
    let when = at(ts, zone);
    format!(
        "{:02}:{:02}:{:02}",
        when.hour(),
        when.minute(),
        when.second()
    )
}

/// Whole days from the day of `ts` to the day of `now`, by calendar day, each in the offset of its own instant
/// (log-model.ts `dayLabel`).
fn days_between(ts: &DateTime, now: &DateTime) -> i64 {
    day_number(now.date()) - day_number(ts.date())
}

pub fn day_divider_in(ts: u64, now: u64, zone: Zone, locale: &str) -> String {
    let (when, now) = (at(ts, zone), at(now, zone));
    match days_between(&when, &now) {
        0 => t("layout.log.today").to_owned(),
        1 => t("layout.log.yesterday").to_owned(),
        _ => fmt_day(&when, locale),
    }
}

pub fn relative_in(ts: u64, now: u64, zone: Zone, locale: &str) -> String {
    let (when, now) = (at(ts, zone), at(now, zone));
    let hhmm = fmt_time(&when);
    match days_between(&when, &now) {
        0 => hhmm,
        1 => format!("{}, {hhmm}", t("layout.log.yesterday")),
        _ => format!("{}, {hhmm}", fmt_day(&when, locale)),
    }
}

/// `Today, 14:05`, the reminders' form (`reminder.today` and `reminder.tomorrow`), for a time at or after `now`.
pub fn reminder_in(ts: u64, now: u64, zone: Zone, locale: &str) -> String {
    let (when, now) = (at(ts, zone), at(now, zone));
    let hhmm = fmt_time(&when);
    match days_between(&now, &when) {
        0 => tf("reminder.today", &[("time", &hhmm)]),
        1 => tf("reminder.tomorrow", &[("time", &hhmm)]),
        _ => format!("{}, {hhmm}", fmt_day(&when, locale)),
    }
}

/// When a reminder is due, as the reminders popover writes it (UtilityCommandProvider.tsx `reminderWhen`): `Today,
/// 20:00`, `Tomorrow, 08:30`, else `Fri 30 Oct, 20:00` (en-GB's short weekday, day and month). `at` is the due time in
/// the reader's zone, `days` the calendar days from today to it.
pub fn reminder_when(at: DateTime, days: i64) -> String {
    const WEEKDAYS: [&str; 7] = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
    let hhmm = fmt_time(&at);
    match days {
        0 => tf("reminder.today", &[("time", &hhmm)]),
        1 => tf("reminder.tomorrow", &[("time", &hhmm)]),
        _ => format!(
            "{} {} {}, {hhmm}",
            WEEKDAYS[at.weekday().to_monday_zero_offset() as usize],
            at.day(),
            EN[(at.month() - 1) as usize]
        ),
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Order {
    /// `Oct 3, 2026`.
    MonthDayYear,
    /// `3 Oct 2026`.
    DayMonthYear,
    /// `3. Okt. 2026`.
    DayDotMonthYear,
    /// `2026年10月3日`.
    Cjk,
}

const EN: [&str; 12] = [
    "Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
];
const SV: [&str; 12] = [
    "jan.", "feb.", "mars", "apr.", "maj", "juni", "juli", "aug.", "sep.", "okt.", "nov.", "dec.",
];
const DE: [&str; 12] = [
    "Jan.", "Feb.", "März", "Apr.", "Mai", "Juni", "Juli", "Aug.", "Sept.", "Okt.", "Nov.", "Dez.",
];
const FR: [&str; 12] = [
    "janv.", "févr.", "mars", "avr.", "mai", "juin", "juil.", "août", "sept.", "oct.", "nov.",
    "déc.",
];
const ES: [&str; 12] = [
    "ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sept", "oct", "nov", "dic",
];
const NL: [&str; 12] = [
    "jan", "feb", "mrt", "apr", "mei", "jun", "jul", "aug", "sep", "okt", "nov", "dec",
];
const NB: [&str; 12] = [
    "jan.", "feb.", "mars", "apr.", "mai", "juni", "juli", "aug.", "sep.", "okt.", "nov.", "des.",
];
const DA: [&str; 12] = [
    "jan.", "feb.", "mar.", "apr.", "maj", "jun.", "jul.", "aug.", "sep.", "okt.", "nov.", "dec.",
];

/// The pattern and the short month names of `locale`'s region (CLDR's medium date skeleton for the languages the table
/// knows; en-GB's form for the rest).
fn pattern(locale: &str) -> (Order, &'static [&'static str; 12]) {
    let mut parts = locale.split('-');
    let language = parts.next().unwrap_or("").to_ascii_lowercase();
    let region = parts
        .find(|p| p.len() == 2)
        .unwrap_or("")
        .to_ascii_uppercase();
    match language.as_str() {
        "en" if region == "US" || region.is_empty() && locale.eq_ignore_ascii_case("en") => {
            (Order::MonthDayYear, &EN)
        }
        "sv" => (Order::DayMonthYear, &SV),
        "de" => (Order::DayDotMonthYear, &DE),
        "fr" => (Order::DayMonthYear, &FR),
        "es" => (Order::DayMonthYear, &ES),
        "nl" => (Order::DayMonthYear, &NL),
        "nb" | "no" => (Order::DayDotMonthYear, &NB),
        "da" => (Order::DayDotMonthYear, &DA),
        "ja" | "zh" => (Order::Cjk, &EN),
        _ => (Order::DayMonthYear, &EN),
    }
}

pub fn day_in(ts: u64, zone: Zone, locale: &str) -> String {
    fmt_day(&at(ts, zone), locale)
}

fn fmt_day(when: &DateTime, locale: &str) -> String {
    let (order, months) = pattern(locale);
    let (y, m, d) = (when.year(), when.month(), when.day());
    let name = months[(m - 1) as usize];
    match order {
        Order::MonthDayYear => format!("{name} {d}, {y}"),
        Order::DayMonthYear => format!("{d} {name} {y}"),
        Order::DayDotMonthYear => format!("{d}. {name} {y}"),
        Order::Cjk => format!("{y}年{m}月{d}日"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const UTC: &dyn Fn(i64) -> Offset = &|_| Offset::UTC;

    /// Unix seconds of a UTC wall-clock time.
    fn utc(y: i32, m: u32, d: u32, h: u32, min: u32) -> u64 {
        jiff::civil::date(y as i16, m as i8, d as i8)
            .at(h as i8, min as i8, 0, 0)
            .to_zoned(TimeZone::UTC)
            .unwrap()
            .timestamp()
            .as_second() as u64
    }

    /// Europe/Stockholm 2026: CEST (+2) from the last Sunday of March 01:00 UTC to the last Sunday of October 01:00 UTC,
    /// else CET (+1).
    fn stockholm(secs: i64) -> Offset {
        let start = utc(2026, 3, 29, 1, 0) as i64;
        let end = utc(2026, 10, 25, 1, 0) as i64;
        let hours = if (start..end).contains(&secs) { 2 } else { 1 };
        Offset::from_seconds(hours * 3600).unwrap()
    }

    /// log-model.dates.test.ts: an older day in the system region, a Swedish month under sv-SE, an English one under en-US.
    #[test]
    fn format_matches_log_model_dates() {
        let ts = utc(2026, 10, 3, 12, 0);
        let now = utc(2026, 11, 3, 12, 0);
        assert!(day_divider_in(ts, now, UTC, "sv-SE").contains("okt"));
        assert_eq!(day_divider_in(ts, now, UTC, "en-US"), "Oct 3, 2026");
        assert_eq!(day_divider_in(ts, now, UTC, "en-GB"), "3 Oct 2026");
    }

    /// A message sent at 00:30 summer time on 3 Oct, read in November: the time stays 00:30 and it stays in the 3 Oct group
    /// (applying today's winter offset would make it 23:30 on 2 Oct).
    #[test]
    fn each_instant_keeps_its_own_offset_across_dst() {
        let sent = utc(2026, 10, 2, 22, 30); // 00:30 CEST on 3 Oct
        let now = utc(2026, 11, 3, 12, 0);
        assert_eq!(time_in(sent, &stockholm), "00:30");
        assert_eq!(day_in(sent, &stockholm, "en-US"), "Oct 3, 2026");
        assert_eq!(
            day_divider_in(sent, now, &stockholm, "en-US"),
            "Oct 3, 2026"
        );
        assert_eq!(
            relative_in(sent, now, &stockholm, "en-US"),
            "Oct 3, 2026, 00:30"
        );
        // A winter message read in summer likewise.
        let winter = utc(2026, 1, 15, 23, 30); // 00:30 CET on 16 Jan
        assert_eq!(time_in(winter, &stockholm), "00:30");
        assert_eq!(day_in(winter, &stockholm, "en-US"), "Jan 16, 2026");
        // `now` is resolved in its own offset too: 23:30 UTC on 25 Oct is 00:30 CET on the 26th, "today" for a 00:10 CET message.
        assert_eq!(
            day_divider_in(
                utc(2026, 10, 25, 23, 10),
                utc(2026, 10, 25, 23, 30),
                &stockholm,
                "en-US"
            ),
            "today"
        );
    }

    #[test]
    fn the_divider_names_today_and_yesterday_by_calendar_day() {
        let now = utc(2026, 11, 3, 0, 5);
        assert_eq!(
            day_divider_in(utc(2026, 11, 3, 23, 59), now, UTC, "en-US"),
            "today"
        );
        assert_eq!(
            day_divider_in(utc(2026, 11, 2, 23, 59), now, UTC, "en-US"),
            "yesterday"
        );
        assert_eq!(
            day_divider_in(utc(2026, 11, 1, 12, 0), now, UTC, "en-US"),
            "Nov 1, 2026"
        );
    }

    #[test]
    fn times_are_24_hour_in_every_region() {
        assert_eq!(time_in(utc(2026, 10, 3, 0, 7), UTC), "00:07");
        assert_eq!(time_in(utc(2026, 10, 3, 14, 5), UTC), "14:05");
        assert_eq!(time_in(utc(2026, 10, 3, 23, 59), UTC), "23:59");
        assert_eq!(clock_in(utc(2026, 10, 3, 9, 8), UTC), "09:08:00");
        assert_eq!(
            relative_in(
                utc(2026, 11, 3, 15, 30),
                utc(2026, 11, 3, 16, 0),
                UTC,
                "en-US"
            ),
            "15:30"
        );
    }

    #[test]
    fn dates_follow_the_locale() {
        let when = utc(2026, 3, 9, 12, 0);
        assert_eq!(day_in(when, UTC, "sv-SE"), "9 mars 2026");
        assert_eq!(day_in(when, UTC, "de-DE"), "9. März 2026");
        assert_eq!(day_in(when, UTC, "fr-FR"), "9 mars 2026");
        assert_eq!(day_in(when, UTC, "ja-JP"), "2026年3月9日");
        assert_eq!(day_in(when, UTC, "xx-YY"), "9 Mar 2026");
        assert_eq!(day_in(when, UTC, "en"), "Mar 9, 2026");
    }

    #[test]
    fn relative_names_yesterday_and_older_days() {
        let now = utc(2026, 11, 3, 10, 0);
        assert_eq!(
            relative_in(utc(2026, 11, 2, 22, 15), now, UTC, "en-US"),
            "yesterday, 22:15"
        );
        assert_eq!(
            relative_in(utc(2026, 10, 3, 8, 0), now, UTC, "sv-SE"),
            "3 okt. 2026, 08:00"
        );
    }

    /// UtilityCommandProvider.tsx `reminderWhen`: today and tomorrow by name, else en-GB's "Fri 30 Oct".
    #[test]
    fn the_popover_names_today_tomorrow_else_the_weekday_and_date() {
        let at = |ts: u64| at(ts, UTC);
        assert_eq!(
            reminder_when(at(utc(2026, 10, 30, 20, 0)), 0),
            "Today, 20:00"
        );
        assert_eq!(
            reminder_when(at(utc(2026, 10, 31, 8, 30)), 1),
            "Tomorrow, 08:30"
        );
        assert_eq!(
            reminder_when(at(utc(2026, 10, 30, 20, 0)), 3),
            "Fri 30 Oct, 20:00"
        );
    }

    #[test]
    fn reminders_name_today_and_tomorrow() {
        let now = utc(2026, 11, 3, 10, 0);
        assert_eq!(
            reminder_in(utc(2026, 11, 3, 18, 0), now, UTC, "en-US"),
            "Today, 18:00"
        );
        assert_eq!(
            reminder_in(utc(2026, 11, 4, 9, 30), now, UTC, "en-US"),
            "Tomorrow, 09:30"
        );
    }

    /// Europe/Stockholm from the zone database, as the zone function the screens pass (an offset per instant).
    fn tz_stockholm(secs: i64) -> Offset {
        TimeZone::get("Europe/Stockholm")
            .unwrap()
            .to_offset(Timestamp::from_second(secs).unwrap())
    }

    /// The real zone rules across both 2026 changes: the clocks go from 02:00 to 03:00 (CET to CEST) at 01:00 UTC on 29 March
    /// and from 03:00 back to 02:00 at 01:00 UTC on 25 October. Every string is pinned.
    #[test]
    fn times_and_days_across_the_dst_changes_in_stockholm() {
        let z = &tz_stockholm;
        // Spring: 00:59:59 UTC is 01:59:59 CET, 01:00:00 UTC is 03:00:00 CEST (02:xx does not exist that night).
        assert_eq!(time_in(utc(2026, 3, 29, 0, 59), z), "01:59");
        assert_eq!(time_in(utc(2026, 3, 29, 1, 0), z), "03:00");
        assert_eq!(clock_in(utc(2026, 3, 29, 1, 0), z), "03:00:00");
        // Autumn: 00:59 UTC is 02:59 CEST, 01:00 UTC is 02:00 CET (02:xx happens twice).
        assert_eq!(time_in(utc(2026, 10, 25, 0, 59), z), "02:59");
        assert_eq!(time_in(utc(2026, 10, 25, 1, 0), z), "02:00");
        assert_eq!(time_in(utc(2026, 10, 25, 1, 59), z), "02:59");
        // Midnight: 22:59 UTC on 28 March is 23:59 CET, 23:00 UTC is 00:00 CET on the 29th.
        assert_eq!(day_in(utc(2026, 3, 28, 22, 59), z, "en-US"), "Mar 28, 2026");
        assert_eq!(day_in(utc(2026, 3, 28, 23, 0), z, "en-US"), "Mar 29, 2026");
        assert_eq!(time_in(utc(2026, 3, 28, 23, 0), z), "00:00");
        // Summer midnight: 21:59 UTC on 24 October is 23:59 CEST, 22:00 UTC is 00:00 CEST on the 25th.
        assert_eq!(
            day_in(utc(2026, 10, 24, 21, 59), z, "sv-SE"),
            "24 okt. 2026"
        );
        assert_eq!(day_in(utc(2026, 10, 24, 22, 0), z, "sv-SE"), "25 okt. 2026");
        assert_eq!(time_in(utc(2026, 10, 24, 22, 0), z), "00:00");
    }

    /// Days are counted on the calendar in each instant's own offset: the day of the 25-hour autumn change is one day, the
    /// 23-hour spring one too, and a message 23 hours old can still be from "yesterday".
    #[test]
    fn day_dividers_count_calendar_days_across_the_dst_changes() {
        let z = &tz_stockholm;
        // Spring change day (23 hours long): 28 March 23:30 CET is yesterday for 29 March 00:10 CET, and 29 March is today at 23:30 CEST.
        let now = utc(2026, 3, 28, 23, 10); // 00:10 CET on 29 March
        assert_eq!(
            day_divider_in(utc(2026, 3, 28, 22, 30), now, z, "en-US"),
            "yesterday"
        );
        assert_eq!(
            day_divider_in(utc(2026, 3, 28, 23, 5), now, z, "en-US"),
            "today"
        );
        assert_eq!(
            relative_in(utc(2026, 3, 28, 22, 30), now, z, "en-US"),
            "yesterday, 23:30"
        );
        // Autumn change day (25 hours long): 24 Oct 23:30 CEST is yesterday for 25 Oct 23:30 CET.
        let now = utc(2026, 10, 25, 22, 30); // 23:30 CET on 25 October
        assert_eq!(
            day_divider_in(utc(2026, 10, 24, 21, 30), now, z, "en-US"),
            "yesterday"
        );
        assert_eq!(
            day_divider_in(utc(2026, 10, 24, 22, 0), now, z, "en-US"),
            "today"
        );
        // Reminders: due 09:00 CEST on 26 Oct while it is 23:30 CET on the 25th is tomorrow.
        assert_eq!(
            reminder_in(utc(2026, 10, 26, 8, 0), now, z, "en-US"),
            "Tomorrow, 09:00"
        );
        assert_eq!(
            reminder_in(utc(2026, 10, 25, 22, 45), now, z, "en-US"),
            "Today, 23:45"
        );
    }

    /// The day count differs by one per calendar day, over month, year and leap-day ends.
    #[test]
    fn day_numbers_step_by_one_per_calendar_day() {
        let n = |y: i16, m: i8, d: i8| day_number(Date::constant(y, m, d));
        assert_eq!(n(1970, 1, 1), 0);
        assert_eq!(n(2026, 11, 17) - n(2026, 11, 16), 1);
        assert_eq!(n(2027, 1, 1) - n(2026, 12, 31), 1);
        assert_eq!(n(2028, 3, 1) - n(2028, 2, 28), 2);
        assert_eq!(n(2027, 3, 1) - n(2027, 2, 28), 1);
        assert_eq!(n(2026, 1, 1) - n(1969, 12, 31), 20_455);
    }

    /// An instant outside the calendar reads as the epoch, shifted by the offset: it never panics.
    #[test]
    fn an_out_of_range_instant_reads_as_the_epoch() {
        assert_eq!(time_in(u64::MAX, UTC), "00:00");
        assert_eq!(day_in(u64::MAX, UTC, "en-US"), "Jan 1, 1970");
    }

    /// The weekday and month names of the reminders popover, for the days around a month and a year end.
    #[test]
    fn the_popover_names_the_weekday_and_month_at_the_ends() {
        let at = |ts: u64| at(ts, UTC);
        assert_eq!(
            reminder_when(at(utc(2026, 12, 31, 23, 59)), 2),
            "Thu 31 Dec, 23:59"
        );
        assert_eq!(
            reminder_when(at(utc(2027, 1, 1, 0, 0)), 2),
            "Fri 1 Jan, 00:00"
        );
        assert_eq!(
            reminder_when(at(utc(2026, 11, 15, 12, 5)), 5),
            "Sun 15 Nov, 12:05"
        );
        assert_eq!(
            reminder_when(at(utc(2026, 11, 16, 12, 5)), 5),
            "Mon 16 Nov, 12:05"
        );
    }

    #[test]
    fn os_locales_become_bcp47() {
        assert_eq!(canonical("sv_SE.UTF-8"), "sv-SE");
        assert_eq!(canonical("en_US@posix"), "en-US");
        assert_eq!(canonical("en-GB"), "en-GB");
        assert_eq!(canonical(""), "");
    }
}
