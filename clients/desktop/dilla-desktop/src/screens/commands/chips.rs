//! The chips a picked command turns the composer's field into (packages/web/src/screens/commands/CommandPicker.tsx,
//! picker-when.ts; Task 28 follow-up 14): the accent `/name` token and one field per argument, the bar above them (the
//! command's description, `/remind`'s "Reminds you …" preview or why its time does not read), and the keys while they
//! are shown (`super::keys`): typing fills the active field, Tab and Shift+Tab move between fields, Backspace deletes,
//! Escape leaves them, and Enter sends what they hold through the composer's own send ([`draft`]), so a Bot command runs
//! sealed and a built-in posts or answers exactly as its typed form would.
//!
//! Not ported yet: the suggestions list above the active chip (entity choices, the when chips' phrases, a DST fold's two
//! candidates) and the music commands' room bar.

use dilla_client::features::bot::registry::{ArgKind, CommandDefinition};
use dilla_client::features::utilities::time::{ReminderTime, ReminderZone, parse_reminder_time};
use dilla_client::state::{AppState, CommunityId};
use gpui::{AnyElement, App, IntoElement, ParentElement, Styled, Window, div};

use dilla_desktop_ui::components::commands::chips::{
    BarTone, CommandBar, CommandChips, CommandField,
};

use super::copy;
use super::picker::{self, Entry};
use crate::slots::SlotCx;

/// The automation ids of the fields (the registry's commands have at most a handful of arguments).
pub const FIELD_IDS: [&str; 8] = [
    "command-field-0",
    "command-field-1",
    "command-field-2",
    "command-field-3",
    "command-field-4",
    "command-field-5",
    "command-field-6",
    "command-field-7",
];

/// The chips of a picked command, per window (in [`super::PickerState`]).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Chips {
    /// The picked command's id.
    pub command: String,
    /// Each field's text, in the command's option order.
    pub values: Vec<String>,
    /// The active field.
    pub active: usize,
    /// How many fields the last frame drew (what Tab wraps over; 0 for a command with no arguments).
    pub fields: usize,
}

impl Chips {
    pub fn new(command: String) -> Self {
        Self {
            command,
            ..Self::default()
        }
    }

    /// The active field's text, made if the frame has not drawn it yet.
    pub fn active_value(&mut self) -> &mut String {
        if self.values.len() <= self.active {
            self.values.resize(self.active + 1, String::new());
        }
        &mut self.values[self.active]
    }

    /// Tab (`back`: Shift+Tab): the next field, wrapping.
    pub fn step(&mut self, back: bool) {
        let n = self.fields.max(1);
        self.active = if back {
            (self.active + n - 1) % n
        } else {
            (self.active + 1) % n
        };
    }
}

/// picker-when.ts `registryWhen`: the registry's form of a `when` phrase (a duration such as `1h`, or an ISO local date
/// and time), or `None` for one the picker does not know. `now` is unix seconds, `offset_at` the zone's offset (seconds
/// east of UTC) at an instant.
pub fn registry_when(text: &str, now: i64, offset_at: &dyn Fn(i64) -> i32) -> Option<String> {
    let value = text.trim().to_lowercase();
    if value.is_empty() {
        return None;
    }
    let words: Vec<&str> = value.split_whitespace().collect();
    // `in 20 minutes`, `in 1 hour`: the number and the unit's first letter.
    let spoken = match words.as_slice() {
        ["in", n, unit] => Some((*n, *unit)),
        ["in", rest] => {
            let split = rest
                .find(|c: char| !c.is_ascii_digit())
                .unwrap_or(rest.len());
            Some((&rest[..split], &rest[split..]))
        }
        _ => None,
    };
    const UNITS: [&str; 15] = [
        "minute", "minutes", "min", "mins", "m", "hour", "hours", "hr", "hrs", "h", "day", "days",
        "d", "week", "weeks",
    ];
    if let Some((n, unit)) = spoken
        && !n.is_empty()
        && n.bytes().all(|b| b.is_ascii_digit())
        && (UNITS.contains(&unit) || unit == "w")
    {
        return Some(format!("{n}{}", &unit[..1]));
    }
    // `90m`, `1h30m`, `in 2h`.
    let short = value.strip_prefix("in ").unwrap_or(&value).trim();
    if is_short_duration(short) {
        return Some(short.to_owned());
    }
    // `tonight 20:00`, `tomorrow 09:00`, `18:30`: the next such time in the zone.
    let (day, clock) = match words.as_slice() {
        [d @ ("tonight" | "today" | "tomorrow"), c] => (Some(*d), *c),
        [c] => (None, *c),
        _ => (None, ""),
    };
    if let Some((h, m)) = clock.split_once(':')
        && (1..=2).contains(&h.len())
        && m.len() == 2
        && h.bytes().chain(m.bytes()).all(|b| b.is_ascii_digit())
    {
        let (hour, minute): (i64, i64) = (h.parse().ok()?, m.parse().ok()?);
        if hour > 23 || minute > 59 {
            return None;
        }
        let local = now + i64::from(offset_at(now));
        let (days, secs) = (local.div_euclid(86_400), local.rem_euclid(86_400));
        let later =
            day == Some("tomorrow") || hour * 60 + minute <= secs / 3600 * 60 + secs % 3600 / 60;
        let (y, mo, d) = civil(days + i64::from(later));
        return Some(format!("{y:04}-{mo:02}-{d:02}T{hour:02}:{minute:02}"));
    }
    // `2026-11-14 09:00` or `2026-11-14T09:00[:ss]`.
    if value.len() >= 16 {
        let (date, rest) = value.split_at(10);
        let sep = rest.as_bytes()[0];
        let time = &rest[1..];
        let date_ok = date.len() == 10
            && date.bytes().enumerate().all(|(i, b)| {
                if i == 4 || i == 7 {
                    b == b'-'
                } else {
                    b.is_ascii_digit()
                }
            });
        let time_ok = (time.len() == 5 || time.len() == 8)
            && time.bytes().enumerate().all(|(i, b)| {
                if i == 2 || i == 5 {
                    b == b':'
                } else {
                    b.is_ascii_digit()
                }
            });
        if (sep == b't' || sep == b' ') && date_ok && time_ok {
            return Some(format!("{date}T{time}"));
        }
    }
    None
}

/// `(?:\d+(?:\.\d+)?(?:ms|s|m|h|d|w))+`.
fn is_short_duration(s: &str) -> bool {
    let b = s.as_bytes();
    let mut i = 0;
    if b.is_empty() {
        return false;
    }
    while i < b.len() {
        let start = i;
        while i < b.len() && b[i].is_ascii_digit() {
            i += 1;
        }
        if i == start {
            return false;
        }
        if i < b.len() && b[i] == b'.' {
            i += 1;
            let frac = i;
            while i < b.len() && b[i].is_ascii_digit() {
                i += 1;
            }
            if i == frac {
                return false;
            }
        }
        if b[i..].starts_with(b"ms") {
            i += 2;
        } else if i < b.len() && b"smhdw".contains(&b[i]) {
            i += 1;
        } else {
            return false;
        }
    }
    true
}

/// Days since 1970-01-01 as a civil date (Howard Hinnant's `civil_from_days`).
fn civil(days: i64) -> (i64, u32, u32) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let doe = z - era * 146_097;
    let yoe = (doe - doe / 1460 + doe / 36_524 - doe / 146_096) / 365;
    let doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
    let mp = (5 * doy + 2) / 153;
    let d = (doy - (153 * mp + 2) / 5 + 1) as u32;
    let m = if mp < 10 { mp + 3 } else { mp - 9 } as u32;
    (yoe + era * 400 + i64::from(m <= 2), m, d)
}

/// What the bar says of `/remind`'s `when` (`resolveWhen`): its preview, or why it does not read (`None`: empty).
pub fn when_line(text: &str, zone: ReminderZone<'_>, now: i64) -> Option<Result<String, ()>> {
    if text.trim().is_empty() {
        return None;
    }
    let Some(when) = registry_when(text, now, zone.offset_at) else {
        return Some(Err(()));
    };
    Some(match parse_reminder_time(&when, zone, now) {
        ReminderTime::Relative { preview, .. } | ReminderTime::Absolute { preview, .. } => {
            Ok(preview)
        }
        _ => Err(()),
    })
}

/// A value as one token of a typed command: quoted, its quotes and backslashes escaped.
fn quoted(value: &str) -> String {
    let mut out = String::with_capacity(value.len() + 2);
    out.push('"');
    for c in value.chars() {
        if c == '"' || c == '\\' {
            out.push('\\');
        }
        out.push(c);
    }
    out.push('"');
    out
}

/// The typed form of the chips (`/name option:"value" …`, empty fields left out): what Enter sends through the composer.
/// `/remind`'s `when` goes in the registry's form.
pub fn typed(
    definition: &CommandDefinition,
    values: &[String],
    now: i64,
    offset_at: &dyn Fn(i64) -> i32,
) -> String {
    let mut out = format!("/{}", definition.id);
    for (option, value) in definition.options.iter().zip(values) {
        if value.trim().is_empty() {
            continue;
        }
        let value = if option.kind == ArgKind::Time && definition.id == "remind" {
            registry_when(value, now, offset_at).unwrap_or_else(|| value.clone())
        } else {
            value.clone()
        };
        out.push(' ');
        out.push_str(&option.name);
        out.push(':');
        out.push_str(&quoted(&value));
    }
    out
}

fn definition(
    state: &AppState,
    community: Option<CommunityId>,
    id: &str,
) -> Option<CommandDefinition> {
    picker::definitions(state, community)
        .into_iter()
        .find(|d| d.id == id)
}

fn now_s() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .map_or(0, |d| i64::try_from(d.as_secs()).unwrap_or(0))
}

fn offset_at(t: i64) -> i32 {
    crate::format::local_zone(t).local_minus_utc()
}

/// The zone a reminder's time is read in (the OS zone, as `super::local_zone`).
fn zone_name() -> String {
    std::env::var("TZ")
        .ok()
        .filter(|z| dilla_client::features::utilities::time::valid_zone_name(z))
        .unwrap_or_else(|| "UTC".to_owned())
}

/// What Enter sends while `window`'s composer shows chips: their typed form (`None` with no chips).
pub fn draft(ctx: &SlotCx, window: gpui::WindowId, cx: &App) -> Option<String> {
    let chips = super::picker_state(window, cx).chips?;
    let definition = definition(&ctx.state, ctx.community, &chips.command)?;
    Some(typed(&definition, &chips.values, now_s(), &offset_at))
}

/// The chips in place of the composer's field (`None` while no command is picked).
pub fn view(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let wid = window.window_handle().window_id();
    let chips = super::picker_state(wid, cx).chips?;
    let Some(definition) = definition(&ctx.state, ctx.community, &chips.command) else {
        // The command went (the Bot's catalog changed): back to the typed draft.
        super::picker_state_mut(wid, cx).chips = None;
        return None;
    };
    let bot_name = ctx
        .community
        .and_then(|c| ctx.state.bot.catalogs.get(&c))
        .map_or("", |v| v.bot_name.as_str());
    let entry: Entry = picker::entry(&definition, bot_name);
    let n = entry.fields.len();
    {
        let s = super::picker_state_mut(wid, cx);
        if let Some(c) = s.chips.as_mut() {
            c.fields = n;
            c.values.resize(n.max(c.values.len()), String::new());
            c.active = c.active.min(n.saturating_sub(1));
        }
    }
    let active = chips.active.min(n.saturating_sub(1));
    let value = |i: usize| chips.values.get(i).cloned().unwrap_or_default();
    // The bar: `/remind`'s when, read as the reminder would be; else the command's description.
    let name = zone_name();
    let when = definition
        .options
        .iter()
        .position(|o| o.kind == ArgKind::Time && definition.id == "remind")
        .and_then(|i| {
            when_line(
                &value(i),
                ReminderZone {
                    name: &name,
                    offset_at: &offset_at,
                },
                now_s(),
            )
            .map(|r| (i, r))
        });
    let (text, tone) = match &when {
        Some((_, Ok(preview))) => (copy::reminds_you(preview), BarTone::Info),
        Some((_, Err(()))) => (copy::WHEN_HINT.to_owned(), BarTone::Danger),
        None => (entry.description.clone(), BarTone::Info),
    };
    let bar = CommandBar::new("command-bar", entry.icon, text, tone).hint(copy::FIELD_HINT);
    let mut row = CommandChips::new("command-chips", entry.icon, definition.id.clone());
    if n == 0 {
        row = row.field(
            CommandField::new(FIELD_IDS[0], "", "", copy::PRESS_TO_RUN)
                .ghost(true)
                .active(true),
        );
    }
    for (i, field) in entry.fields.iter().enumerate() {
        let invalid = when.as_ref().is_some_and(|(w, r)| *w == i && r.is_err());
        let el = CommandField::new(
            FIELD_IDS.get(i).copied().unwrap_or("command-field"),
            field.label.clone(),
            value(i),
            field.placeholder.clone(),
        )
        .optional(field.optional)
        .active(i == active)
        .invalid(invalid);
        row = row.field(crate::tracked_box(
            FIELD_IDS.get(i).copied().unwrap_or("command-field"),
            div().child(el),
        ));
    }
    Some(
        crate::tracked_box(
            "command-chips",
            div()
                .flex_1()
                .min_w_0()
                .flex()
                .flex_col()
                .child(bar)
                .child(row),
        )
        .into_any_element(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn utc(_: i64) -> i32 {
        0
    }

    #[test]
    fn spoken_and_short_durations_take_the_registry_form() {
        let now = 1_800_000_000;
        assert_eq!(registry_when("in 1 hour", now, &utc).as_deref(), Some("1h"));
        assert_eq!(
            registry_when("in 20 minutes", now, &utc).as_deref(),
            Some("20m")
        );
        assert_eq!(registry_when("In 2 Days", now, &utc).as_deref(), Some("2d"));
        assert_eq!(registry_when("in 3w", now, &utc).as_deref(), Some("3w"));
        assert_eq!(registry_when("1h30m", now, &utc).as_deref(), Some("1h30m"));
        assert_eq!(registry_when("in 90m", now, &utc).as_deref(), Some("90m"));
        assert_eq!(registry_when("soon", now, &utc), None);
        assert_eq!(registry_when("", now, &utc), None);
    }

    #[test]
    fn a_clock_time_is_the_next_one_in_the_zone() {
        // 2027-01-15 08:00:00 UTC.
        let now = 1_799_913_600 + 8 * 3600;
        assert_eq!(
            registry_when("09:00", now, &utc).as_deref(),
            Some("2027-01-15T09:00")
        );
        assert_eq!(
            registry_when("07:00", now, &utc).as_deref(),
            Some("2027-01-16T07:00")
        );
        assert_eq!(
            registry_when("tomorrow 09:00", now, &utc).as_deref(),
            Some("2027-01-16T09:00")
        );
        assert_eq!(
            registry_when("tonight 20:00", now, &utc).as_deref(),
            Some("2027-01-15T20:00")
        );
        assert_eq!(registry_when("25:00", now, &utc), None);
        assert_eq!(
            registry_when("2027-02-01 10:30", now, &utc).as_deref(),
            Some("2027-02-01T10:30")
        );
    }

    #[test]
    fn the_typed_form_quotes_values_and_leaves_out_empty_fields() {
        use dilla_client::features::bot::registry::{CommandOption, Provider};
        let opt = |name: &str, kind| CommandOption {
            name: name.into(),
            kind,
            description: String::new(),
            required: true,
            free_text: false,
            choices: None,
            min: None,
            max: None,
        };
        let remind = CommandDefinition {
            id: "remind".into(),
            description: String::new(),
            community_only: true,
            provider: Provider::Bot,
            permission_bit: None,
            options: vec![opt("when", ArgKind::Time), opt("text", ArgKind::Text)],
            bot: None,
        };
        assert_eq!(
            typed(&remind, &["in 1 hour".into(), "say \"hi\"".into()], 0, &utc),
            r#"/remind when:"1h" text:"say \"hi\"""#
        );
        assert_eq!(
            typed(&remind, &[String::new(), "x".into()], 0, &utc),
            r#"/remind text:"x""#
        );
    }

    #[test]
    fn tab_wraps_over_the_fields() {
        let mut c = Chips::new("remind".into());
        c.fields = 2;
        c.step(false);
        assert_eq!(c.active, 1);
        c.step(false);
        assert_eq!(c.active, 0);
        c.step(true);
        assert_eq!(c.active, 1);
        c.active_value().push('x');
        assert_eq!(c.values, ["", "x"]);
    }
}
