//! Times, day dividers and relative times, as the web client draws them (`packages/ui/src/strings/index.ts` and
//! `packages/web/src/screens/conversation/log-model.ts`).
//!
//! - **Times are always 24-hour `HH:MM`** (hour cycle h23), whatever the region: the design draws them so.
//! - **Dates follow the system region** ([`date_locale`], the counterpart of the web's `dateLocale()`): `Oct 3, 2026` under
//!   en-US, `3 Oct 2026` under en-GB, `3 okt. 2026` under sv-SE. A region this table does not know draws en-GB's form.
//!
//! The public functions read the OS locale (`sys-locale`) and the local zone. The `*_in` functions take both, for tests and
//! fixtures.

use chrono::{DateTime, Datelike, FixedOffset, TimeZone, Timelike};

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

fn local(ts: u64) -> DateTime<FixedOffset> {
    at(ts, *chrono::Local::now().offset())
}

fn at(ts: u64, zone: FixedOffset) -> DateTime<FixedOffset> {
    let secs = i64::try_from(ts).unwrap_or(i64::MAX);
    zone.from_utc_datetime(
        &chrono::DateTime::from_timestamp(secs, 0)
            .unwrap_or_default()
            .naive_utc(),
    )
}

/// A message time: 24-hour `HH:MM` in the local zone.
pub fn time(ts: u64) -> String {
    time_in(&local(ts))
}

/// The date of `ts` in the system region (the web's `formatDay`): `Oct 3, 2026`.
pub fn day(ts: u64) -> String {
    day_in(&local(ts), &date_locale())
}

/// The date and the 24-hour time (the web's `formatDayTime`): `Oct 3, 2026, 14:05`.
pub fn day_time(ts: u64) -> String {
    let when = local(ts);
    format!("{}, {}", day_in(&when, &date_locale()), time_in(&when))
}

/// A diagnostics clock, 24-hour `HH:MM:SS` from milliseconds (the web's `formatClock`).
pub fn clock(ms: u64) -> String {
    clock_in(&local(ms / 1000))
}

/// The day divider of a message log: `today`, `yesterday`, else the date (the web's `dayLabel`). `now` is the clock the
/// log was rendered with, in unix seconds.
pub fn day_divider(ts: u64, now: u64) -> String {
    let zone = *chrono::Local::now().offset();
    day_divider_in(&at(ts, zone), &at(now, zone), &date_locale())
}

/// A time relative to `now`: the time today, `yesterday, 14:05` the day before, else the date and the time. Both are unix
/// seconds.
pub fn relative(ts: u64, now: u64) -> String {
    let zone = *chrono::Local::now().offset();
    relative_in(&at(ts, zone), &at(now, zone), &date_locale())
}

pub fn time_in(when: &DateTime<FixedOffset>) -> String {
    format!("{:02}:{:02}", when.hour(), when.minute())
}

pub fn clock_in(when: &DateTime<FixedOffset>) -> String {
    format!(
        "{:02}:{:02}:{:02}",
        when.hour(),
        when.minute(),
        when.second()
    )
}

/// Whole days from the day of `ts` to the day of `now`, by local calendar day (log-model.ts `dayLabel`).
fn days_between(ts: &DateTime<FixedOffset>, now: &DateTime<FixedOffset>) -> i64 {
    i64::from(now.date_naive().num_days_from_ce()) - i64::from(ts.date_naive().num_days_from_ce())
}

pub fn day_divider_in(
    ts: &DateTime<FixedOffset>,
    now: &DateTime<FixedOffset>,
    locale: &str,
) -> String {
    match days_between(ts, now) {
        0 => t("layout.log.today").to_owned(),
        1 => t("layout.log.yesterday").to_owned(),
        _ => day_in(ts, locale),
    }
}

pub fn relative_in(
    ts: &DateTime<FixedOffset>,
    now: &DateTime<FixedOffset>,
    locale: &str,
) -> String {
    let hhmm = time_in(ts);
    match days_between(ts, now) {
        0 => hhmm,
        1 => format!("{}, {hhmm}", t("layout.log.yesterday")),
        _ => format!("{}, {hhmm}", day_in(ts, locale)),
    }
}

/// `Today, 14:05`, the reminders' form (`reminder.today` and `reminder.tomorrow`), for a time at or after `now`.
pub fn reminder_in(ts: &DateTime<FixedOffset>, now: &DateTime<FixedOffset>, locale: &str) -> String {
    let hhmm = time_in(ts);
    match days_between(now, ts) {
        0 => tf("reminder.today", &[("time", &hhmm)]),
        1 => tf("reminder.tomorrow", &[("time", &hhmm)]),
        _ => format!("{}, {hhmm}", day_in(ts, locale)),
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

pub fn day_in(when: &DateTime<FixedOffset>, locale: &str) -> String {
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

    fn utc(y: i32, m: u32, d: u32, h: u32, min: u32) -> DateTime<FixedOffset> {
        FixedOffset::east_opt(0)
            .unwrap()
            .with_ymd_and_hms(y, m, d, h, min, 0)
            .unwrap()
    }

    /// log-model.dates.test.ts: an older day in the system region, a Swedish month under sv-SE, an English one under en-US.
    #[test]
    fn format_matches_log_model_dates() {
        let ts = utc(2026, 10, 3, 12, 0);
        let now = utc(2026, 11, 3, 12, 0);
        assert!(day_divider_in(&ts, &now, "sv-SE").contains("okt"));
        assert_eq!(day_divider_in(&ts, &now, "en-US"), "Oct 3, 2026");
        assert_eq!(day_divider_in(&ts, &now, "en-GB"), "3 Oct 2026");
    }

    #[test]
    fn the_divider_names_today_and_yesterday_by_calendar_day() {
        let now = utc(2026, 11, 3, 0, 5);
        assert_eq!(day_divider_in(&utc(2026, 11, 3, 23, 59), &now, "en-US"), "today");
        assert_eq!(day_divider_in(&utc(2026, 11, 2, 23, 59), &now, "en-US"), "yesterday");
        assert_eq!(day_divider_in(&utc(2026, 11, 1, 12, 0), &now, "en-US"), "Nov 1, 2026");
    }

    #[test]
    fn times_are_24_hour_in_every_region() {
        assert_eq!(time_in(&utc(2026, 10, 3, 0, 7)), "00:07");
        assert_eq!(time_in(&utc(2026, 10, 3, 14, 5)), "14:05");
        assert_eq!(time_in(&utc(2026, 10, 3, 23, 59)), "23:59");
        assert_eq!(clock_in(&utc(2026, 10, 3, 9, 8)), "09:08:00");
        // en-US dates do not make the time 12-hour: relative() takes the locale for the date only.
        assert_eq!(
            relative_in(&utc(2026, 11, 3, 15, 30), &utc(2026, 11, 3, 16, 0), "en-US"),
            "15:30"
        );
    }

    #[test]
    fn dates_follow_the_locale() {
        let when = utc(2026, 3, 9, 12, 0);
        assert_eq!(day_in(&when, "sv-SE"), "9 mars 2026");
        assert_eq!(day_in(&when, "de-DE"), "9. März 2026");
        assert_eq!(day_in(&when, "fr-FR"), "9 mars 2026");
        assert_eq!(day_in(&when, "ja-JP"), "2026年3月9日");
        assert_eq!(day_in(&when, "xx-YY"), "9 Mar 2026");
        assert_eq!(day_in(&when, "en"), "Mar 9, 2026");
    }

    #[test]
    fn relative_names_yesterday_and_older_days() {
        let now = utc(2026, 11, 3, 10, 0);
        assert_eq!(relative_in(&utc(2026, 11, 2, 22, 15), &now, "en-US"), "yesterday, 22:15");
        assert_eq!(relative_in(&utc(2026, 10, 3, 8, 0), &now, "sv-SE"), "3 okt. 2026, 08:00");
    }

    #[test]
    fn reminders_name_today_and_tomorrow() {
        let now = utc(2026, 11, 3, 10, 0);
        assert_eq!(reminder_in(&utc(2026, 11, 3, 18, 0), &now, "en-US"), "Today, 18:00");
        assert_eq!(reminder_in(&utc(2026, 11, 4, 9, 30), &now, "en-US"), "Tomorrow, 09:30");
    }

    #[test]
    fn os_locales_become_bcp47() {
        assert_eq!(canonical("sv_SE.UTF-8"), "sv-SE");
        assert_eq!(canonical("en_US@posix"), "en-US");
        assert_eq!(canonical("en-GB"), "en-GB");
        assert_eq!(canonical(""), "");
    }
}
