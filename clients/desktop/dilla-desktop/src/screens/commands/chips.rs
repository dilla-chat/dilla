//! The chips a picked command turns the composer's field into (packages/web/src/screens/commands/CommandPicker.tsx,
//! picker-when.ts; Task 28 follow-up 14): the accent `/name` token and one field per argument, the bar above them (the
//! command's description, `/remind`'s "Reminds you …" preview or why its time does not read), and the keys while they
//! are shown (`super::keys`): typing fills the active field, Tab and Shift+Tab move between fields, Backspace deletes,
//! Escape leaves them, and Enter sends what they hold through the composer's own send ([`draft`]), so a Bot command runs
//! sealed and a built-in posts or answers exactly as its typed form would.
//!
//! Task 28 wave 2 (lane C): the suggestions above the active chip ([`offered`], drawn by [`popup`]: durations, `/remind`'s
//! `when` phrases with their previews, an option's fixed choices, `/help`'s commands; ↑↓ move, Tab and Enter accept,
//! as `chipKeyDown`), and the music commands' bar naming the voice room this device is in ([`music_bar`]).
//!
//! w2-bot-review I-B1: a member field lists the community's members ([`member_suggestions`]); a pick keeps the member's
//! id, which is what the command sends, so `/kick` and `/ban` never act on a name.
//!
//! Not ported yet: the other entity suggestions (what the Bot or the instance looks up: channels, roles, queue items,
//! search results, `/unban`'s banned members) and a DST fold's two `when` candidates.

use dilla_client::features::bot::registry::{ArgKind, CommandDefinition, CommandOption};
use dilla_client::features::bot::room;
use dilla_client::features::utilities::time::{ReminderTime, ReminderZone, parse_reminder_time};
use dilla_client::state::{AppState, CommunityId};
use gpui::{AnyElement, App, IntoElement, ParentElement, Styled, Window, div};

use dilla_desktop_ui::IconName;
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
    /// The highlighted suggestion of the active field.
    pub sug: usize,
    /// The suggestions the active field's last frame offered (what ↑↓, Tab and Enter act on).
    pub shown: Vec<String>,
    /// The active field's text those suggestions were for: a new text moves the highlight back to the first.
    pub sug_for: String,
    /// Tab (and, unless the field takes free text, Enter) puts the highlighted suggestion in the field
    /// (CommandPicker.tsx `canAccept`, `noAccept`).
    pub can_accept: bool,
    pub enter_accepts: bool,
    /// What the shown suggestions' rows say (a member's name), beside their values in `shown` (the member's id).
    pub labels: Vec<String>,
    /// The active field suggests members: a pick shows the member's name and keeps their id ([`Chips::picked`]).
    pub member: bool,
    /// Each field's picked member id (CommandPicker.tsx `field.selected`), until its text changes.
    pub picked: Vec<Option<String>>,
}

impl Chips {
    pub fn new(command: String) -> Self {
        Self {
            command,
            ..Self::default()
        }
    }

    /// The active field's text, made if the frame has not drawn it yet. Its text is about to change, so a member picked
    /// in it is forgotten (CommandPicker.tsx `setField`).
    pub fn active_value(&mut self) -> &mut String {
        if self.values.len() <= self.active {
            self.values.resize(self.active + 1, String::new());
        }
        if let Some(p) = self.picked.get_mut(self.active) {
            *p = None;
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

    /// CommandPicker.tsx `accept`: suggestion `i`'s value goes in the active field, and the next field becomes active.
    /// A member's name goes in the field and their id is kept as the field's pick.
    pub fn accept(&mut self, i: usize) {
        let Some(value) = self.shown.get(i).cloned() else {
            return;
        };
        if self.member {
            let label = self.labels.get(i).cloned().unwrap_or_else(|| value.clone());
            *self.active_value() = label;
            if self.picked.len() <= self.active {
                self.picked.resize(self.active + 1, None);
            }
            self.picked[self.active] = Some(value);
        } else {
            *self.active_value() = value;
        }
        self.active = (self.active + 1).min(self.fields.saturating_sub(1));
        self.sug = 0;
        self.can_accept = false;
    }

    /// A key on the suggestions (CommandPicker.tsx `chipKeyDown`): ↑↓ move the highlight (wrapping) while there are
    /// suggestions; Tab, and Enter where the field does not take free text, accept the highlighted one while one can be
    /// accepted. `false`: not the suggestions' key (the chips' own keys go on).
    pub fn suggestion_key(&mut self, key: &str, shift: bool) -> bool {
        let n = self.shown.len();
        match (key, shift) {
            ("up" | "down", false) if n > 0 => {
                self.sug = if key == "down" {
                    (self.sug + 1) % n
                } else {
                    (self.sug + n - 1) % n
                };
                true
            }
            ("tab", false) if self.can_accept => {
                self.accept(self.sug.min(n.saturating_sub(1)));
                true
            }
            ("enter", false) if self.can_accept && self.enter_accepts => {
                self.accept(self.sug.min(n.saturating_sub(1)));
                true
            }
            _ => false,
        }
    }
}

/// What a chip suggests (picker-catalog.ts `PickerSuggest`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Suggest {
    Member,
    Duration,
    When,
    Provider,
    Choice,
    Command,
    None,
}

/// picker-catalog.ts `suggestFor`.
pub fn suggest_for(command: &CommandDefinition, option: &CommandOption) -> Suggest {
    const ENTITY: [ArgKind; 7] = [
        ArgKind::Channel,
        ArgKind::Role,
        ArgKind::Message,
        ArgKind::QueueItem,
        ArgKind::Poll,
        ArgKind::Announcement,
        ArgKind::Reminder,
    ];
    match option.kind {
        _ if command.id == "help" && option.name == "command" => Suggest::Command,
        ArgKind::User => Suggest::Member,
        ArgKind::Duration => Suggest::Duration,
        ArgKind::Time if command.id == "remind" => Suggest::When,
        ArgKind::Time => Suggest::None,
        ArgKind::Choice if option.choices.as_ref().is_some_and(|c| !c.is_empty()) => {
            Suggest::Choice
        }
        ArgKind::Choice => Suggest::Provider,
        k if ENTITY.contains(&k) => Suggest::Provider,
        _ if command.id == "play" && option.name == "query-or-url" => Suggest::Provider,
        _ => Suggest::None,
    }
}

/// picker-catalog.ts `DURATION_SUGGESTIONS` (`/ban` adds `PERMANENT`) and `WHEN_SUGGESTIONS`.
pub const DURATION_SUGGESTIONS: [&str; 4] = ["10 minutes", "1 hour", "1 day", "7 days"];
pub const PERMANENT: &str = "permanent";
pub const WHEN_SUGGESTIONS: [&str; 5] = [
    "in 1 minute",
    "in 10 minutes",
    "in 1 hour",
    "tonight 20:00",
    "tomorrow 09:00",
];
/// CommandPicker.tsx `MAX_SUGGESTIONS`.
pub const MAX_SUGGESTIONS: usize = 6;

/// One suggestion (`Sug`): what its row shows, and what a pick puts in the field (its label, or a command's id).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Suggestion {
    pub label: String,
    pub value: String,
    pub sub: String,
    pub icon: IconName,
}

/// The suggestions of the active field.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Offered {
    pub title: &'static str,
    pub items: Vec<Suggestion>,
    pub can_accept: bool,
    pub enter_accepts: bool,
}

/// CommandPicker.tsx `suggestionsFor` for the kinds this device can answer by itself: durations, `/remind`'s `when`
/// phrases (each with its preview, from `preview`), an option's fixed choices and `/help`'s commands (`listed`: the
/// commands of the picker, with their entries). A member or provider field (an entity the Bot or the instance looks up)
/// offers nothing here yet. Then the accept rules (`canAccept`, `noAccept`) for `text`.
pub fn offered(
    definition: &CommandDefinition,
    option: &CommandOption,
    text: &str,
    listed: &[(String, Entry)],
    preview: &dyn Fn(&str) -> Option<String>,
) -> Offered {
    offered_with(definition, option, text, listed, &[], false, preview)
}

/// The members a member field may name (CommandHost.tsx `suggest` for a `user` option): the community's people but this
/// account and the Bot, and for `/kick` and `/ban` only those this account may moderate (`eligibleModerationTarget`).
/// All of them, never a truncated list. Nothing outside a community, and nothing for `/unban` (its banned members are
/// not ported yet).
fn eligible_members<'a>(
    state: &'a AppState,
    community: Option<CommunityId>,
    command: &str,
) -> Vec<&'a dilla_client::state::members::Member> {
    use dilla_client::features::bot::moderation_target;
    let Some(community) = community.filter(|_| command != "unban") else {
        return Vec::new();
    };
    let Some(members) = state.members.get(&community) else {
        return Vec::new();
    };
    let me = state.account.user_id;
    members
        .iter()
        .filter(|m| Some(m.user_id) != me && m.kind != 1)
        .filter(|m| match command {
            "kick" | "ban" => {
                moderation_target(state, community, &m.user_id.hex(), command == "kick").is_some()
            }
            _ => true,
        })
        .collect()
}

/// The members a member field offers for `query` ([`eligible_members`] matching the id, the name or the username, at
/// most 20). Each value is the member's user id, its label the name and its second line the `@username`.
pub fn member_suggestions(
    state: &AppState,
    community: Option<CommunityId>,
    command: &str,
    query: &str,
) -> Vec<Suggestion> {
    let needle = query.to_lowercase();
    let needle = needle.trim_start_matches('@');
    eligible_members(state, community, command)
        .into_iter()
        .filter(|m| {
            let name = if m.display.is_empty() {
                &m.username
            } else {
                &m.display
            };
            m.user_id.hex() == query
                || name.to_lowercase().contains(needle)
                || m.username.to_lowercase().contains(needle)
        })
        .take(20)
        .map(|m| Suggestion {
            label: if m.display.is_empty() {
                m.username.clone()
            } else {
                m.display.clone()
            },
            value: m.user_id.hex(),
            sub: format!("@{}", m.username),
            icon: IconName::User,
        })
        .collect()
}

/// The member a member field's typed `text` names when none was picked: a literal user id, or an `@username`, each
/// unique by definition, among all [`eligible_members`]. A display name is never resolved, not even one that looks
/// unique (w2-bot-review I-B1 and the coordinator's ruling, as the web): the person picks a member from the list.
pub fn resolve_member(
    state: &AppState,
    community: Option<CommunityId>,
    command: &str,
    text: &str,
) -> Option<String> {
    let text = text.trim();
    let username = text.strip_prefix('@').map(str::to_lowercase);
    let eligible = eligible_members(state, community, command);
    eligible
        .iter()
        .find(|m| m.user_id.hex() == text)
        .or_else(|| {
            let username = username.as_deref().filter(|u| !u.is_empty())?;
            eligible
                .iter()
                .find(|m| m.username.to_lowercase() == username)
        })
        .map(|m| m.user_id.hex())
}

/// [`offered`], with the members a member field offers ([`member_suggestions`]) and whether its member was `picked`:
/// only a pick is an exact match there (a typed name never is, so Tab or Enter picks from the list).
pub fn offered_with(
    definition: &CommandDefinition,
    option: &CommandOption,
    text: &str,
    listed: &[(String, Entry)],
    members: &[Suggestion],
    picked: bool,
    preview: &dyn Fn(&str) -> Option<String>,
) -> Offered {
    let kind = suggest_for(definition, option);
    let needle = text.trim().to_lowercase();
    let has = |v: &str| needle.is_empty() || v.to_lowercase().contains(&needle);
    let icon = |icon: IconName, label: &str, sub: String| Suggestion {
        label: label.to_owned(),
        value: label.to_owned(),
        sub,
        icon,
    };
    let (title, items): (&'static str, Vec<Suggestion>) = match kind {
        Suggest::Duration => (
            copy::SUG_DURATIONS,
            DURATION_SUGGESTIONS
                .iter()
                .copied()
                .chain((definition.id == "ban").then_some(PERMANENT))
                .filter(|v| has(v))
                .map(|v| icon(IconName::Timer, v, String::new()))
                .collect(),
        ),
        Suggest::When => (
            copy::SUG_WHEN,
            WHEN_SUGGESTIONS
                .iter()
                .filter(|v| has(v))
                .map(|v| icon(IconName::AlarmClock, v, preview(v).unwrap_or_default()))
                .collect(),
        ),
        Suggest::Choice => (
            copy::SUG_SUGGESTIONS,
            option
                .choices
                .iter()
                .flatten()
                .filter(|v| has(v))
                .map(|v| icon(IconName::Hash, v, String::new()))
                .collect(),
        ),
        Suggest::Command => (
            copy::SUG_COMMANDS,
            listed
                .iter()
                .filter(|(id, _)| has(id))
                .map(|(id, e)| Suggestion {
                    value: id.clone(),
                    ..icon(e.icon, &format!("/{id}"), e.description.clone())
                })
                .collect(),
        ),
        Suggest::Member => (copy::SUG_MEMBERS, members.to_vec()),
        Suggest::Provider | Suggest::None => (copy::SUG_SUGGESTIONS, Vec::new()),
    };
    let items: Vec<Suggestion> = items.into_iter().take(MAX_SUGGESTIONS).collect();
    let typed = text.to_lowercase();
    let exact = if kind == Suggest::Member {
        picked
    } else {
        items
            .iter()
            .any(|s| s.label.to_lowercase() == typed || s.value.to_lowercase() == typed)
    };
    let can_accept = !items.is_empty() && !exact && !text.is_empty();
    let no_accept =
        kind == Suggest::None || definition.id == "play" || option.kind == ArgKind::Number;
    Offered {
        title,
        items,
        can_accept,
        enter_accepts: !no_accept,
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
    // `2026-11-14 09:00` or `2026-11-14T09:00[:ss]`. ASCII only, so the byte slices below fall on char boundaries
    // (t28-bot-review C1: `imorgon förmiddag` crashed the render).
    if value.len() >= 16 && value.is_ascii() {
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
/// `/remind`'s `when` goes in the registry's form. A member field sends the picked member's id (`picked`), else the id
/// `member` resolves its text to ([`resolve_member`]), else its text, which names nobody (CommandPicker.tsx: "No member
/// called …"; a moderation never acts on a name, w2-bot-review I-B1).
pub fn typed(
    definition: &CommandDefinition,
    values: &[String],
    picked: &[Option<String>],
    member: &dyn Fn(&str) -> Option<String>,
    now: i64,
    offset_at: &dyn Fn(i64) -> i32,
) -> String {
    let mut out = format!("/{}", definition.id);
    for (i, (option, value)) in definition.options.iter().zip(values).enumerate() {
        if value.trim().is_empty() {
            continue;
        }
        let value = if option.kind == ArgKind::Time && definition.id == "remind" {
            registry_when(value, now, offset_at).unwrap_or_else(|| value.clone())
        } else if suggest_for(definition, option) == Suggest::Member {
            picked
                .get(i)
                .cloned()
                .flatten()
                .or_else(|| member(value))
                .unwrap_or_else(|| value.clone())
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
    let member = |text: &str| resolve_member(&ctx.state, ctx.community, &chips.command, text);
    Some(typed(
        &definition,
        &chips.values,
        &chips.picked,
        &member,
        now_s(),
        &offset_at,
    ))
}

/// A music command's bar (CommandPicker.tsx): its description, then the voice room this device is in, else "join voice
/// first".
pub fn music_bar(description: &str, room: Option<&str>) -> String {
    format!(
        "{description}{}{}",
        copy::SEPARATOR,
        room.unwrap_or(copy::JOIN_VOICE_FIRST)
    )
}

/// The suggestions of `chips`' active field in the conversation of `ctx` (`None`: the command has no field, or went).
pub fn offer(ctx: &SlotCx, chips: &Chips) -> Option<Offered> {
    let definition = definition(&ctx.state, ctx.community, &chips.command)?;
    let last = definition.options.len().checked_sub(1)?;
    let option = &definition.options[chips.active.min(last)];
    let text = chips
        .values
        .get(chips.active.min(last))
        .cloned()
        .unwrap_or_default();
    let listed: Vec<(String, Entry)> = if suggest_for(&definition, option) == Suggest::Command {
        let bot_name = ctx
            .community
            .and_then(|c| ctx.state.bot.catalogs.get(&c))
            .map_or("", |v| v.bot_name.as_str());
        picker::definitions(&ctx.state, ctx.community)
            .iter()
            .map(|d| (d.id.clone(), picker::entry(d, bot_name)))
            .collect()
    } else {
        Vec::new()
    };
    let name = zone_name();
    let preview = |v: &str| {
        when_line(
            v,
            ReminderZone {
                name: &name,
                offset_at: &offset_at,
            },
            now_s(),
        )
        .and_then(Result::ok)
    };
    let members = if suggest_for(&definition, option) == Suggest::Member {
        member_suggestions(&ctx.state, ctx.community, &definition.id, &text)
    } else {
        Vec::new()
    };
    Some(offered_with(
        &definition,
        option,
        &text,
        &listed,
        &members,
        chips
            .picked
            .get(chips.active.min(last))
            .is_some_and(Option::is_some),
        &preview,
    ))
}

/// The first member field of `chips` that names nobody: typed, no member picked, and not a user id or `@username`
/// ([`resolve_member`]).
pub fn unnamed_member(
    state: &AppState,
    community: Option<CommunityId>,
    definition: &CommandDefinition,
    chips: &Chips,
) -> Option<usize> {
    definition
        .options
        .iter()
        .enumerate()
        .position(|(i, option)| {
            let text = chips.values.get(i).map_or("", String::as_str);
            suggest_for(definition, option) == Suggest::Member
                && !text.trim().is_empty()
                && chips.picked.get(i).is_none_or(Option::is_none)
                && resolve_member(state, community, &chips.command, text).is_none()
        })
}

/// Enter while `window`'s chips hold a member field that names nobody (a display name typed, no member picked): nothing
/// runs; that field becomes the active one, so its members are listed to pick from (coordinator's ruling on I-B1, as the
/// web's "No member called …"). `true`: held.
pub fn hold_unnamed_member(ctx: &SlotCx, window: gpui::WindowId, cx: &mut App) -> bool {
    let Some(chips) = super::picker_state(window, cx).chips else {
        return false;
    };
    let Some(definition) = definition(&ctx.state, ctx.community, &chips.command) else {
        return false;
    };
    let Some(i) = unnamed_member(&ctx.state, ctx.community, &definition, &chips) else {
        return false;
    };
    if let Some(c) = super::picker_state_mut(window, cx).chips.as_mut() {
        c.active = i;
    }
    true
}

/// The automation ids of the suggestion rows (at most [`MAX_SUGGESTIONS`]).
pub const SUGGESTION_IDS: [&str; MAX_SUGGESTIONS] = [
    "command-suggestion-0",
    "command-suggestion-1",
    "command-suggestion-2",
    "command-suggestion-3",
    "command-suggestion-4",
    "command-suggestion-5",
];

/// The active chip's suggestions over the composer (`CommandSuggestions`, in `slots::composer_popup` while chips show):
/// a row clicked goes in the field as Tab would put it; `None` with nothing to suggest.
pub fn popup(ctx: &SlotCx, chips: &Chips, window: gpui::WindowId) -> Option<AnyElement> {
    use dilla_desktop_ui::components::commands::list::{PICKER_GAP, PICKER_INSET_X};
    use dilla_desktop_ui::components::commands::suggestions::{CommandSuggestions, SuggestionItem};
    let offer = offer(ctx, chips)?;
    if offer.items.is_empty() {
        return None;
    }
    let items = offer
        .items
        .iter()
        .map(|s| SuggestionItem {
            icon: s.icon,
            label: s.label.clone().into(),
            sub: s.sub.clone().into(),
        })
        .collect();
    let active = chips.sug.min(offer.items.len() - 1);
    let list = CommandSuggestions::new("command-suggestions", offer.title, items)
        .active(Some(active))
        .row_ids(|i| {
            gpui::ElementId::Name(
                SUGGESTION_IDS
                    .get(i)
                    .copied()
                    .unwrap_or("command-suggestion")
                    .into(),
            )
        })
        .on_active(move |i, window, cx| {
            if let Some(c) = super::picker_state_mut(window_id(window), cx)
                .chips
                .as_mut()
                && c.sug != i
            {
                c.sug = i;
                window.refresh();
            }
        })
        .on_pick(move |i, w, cx| {
            if let Some(c) = super::picker_state_mut(window, cx).chips.as_mut() {
                c.accept(i);
            }
            w.refresh();
        });
    Some(
        div()
            .px(PICKER_INSET_X)
            .pb(PICKER_GAP)
            .child(crate::tracked_box("command-suggestions", list))
            .into_any_element(),
    )
}

fn window_id(window: &Window) -> gpui::WindowId {
    window.window_handle().window_id()
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
        // A music command says where the Bot plays: the voice room this device is in (CommandPicker.tsx).
        None if room::is_music(&definition.id) => (
            music_bar(
                &entry.description,
                room::room_name(&ctx.state, ctx.community).as_deref(),
            ),
            BarTone::Info,
        ),
        None => (entry.description.clone(), BarTone::Info),
    };
    // The active field's suggestions, kept for the keys (`Chips::suggestion_key`); `popup` draws them.
    let offer = offer(ctx, &chips);
    if let Some(c) = super::picker_state_mut(wid, cx).chips.as_mut() {
        let text = c.values.get(c.active).cloned().unwrap_or_default();
        if c.sug_for != text {
            c.sug_for = text;
            c.sug = 0;
        }
        c.shown = offer
            .as_ref()
            .map(|o| o.items.iter().map(|s| s.value.clone()).collect())
            .unwrap_or_default();
        c.labels = offer
            .as_ref()
            .map(|o| o.items.iter().map(|s| s.label.clone()).collect())
            .unwrap_or_default();
        c.member = definition
            .options
            .get(c.active)
            .is_some_and(|o| suggest_for(&definition, o) == Suggest::Member);
        c.sug = c.sug.min(c.shown.len().saturating_sub(1));
        c.can_accept = offer.as_ref().is_some_and(|o| o.can_accept);
        c.enter_accepts = offer.as_ref().is_some_and(|o| o.enter_accepts);
    }
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
        let now = 1_799_971_200 + 8 * 3600;
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
        let nobody = |_: &str| None;
        assert_eq!(
            typed(
                &remind,
                &["in 1 hour".into(), "say \"hi\"".into()],
                &[],
                &nobody,
                0,
                &utc
            ),
            r#"/remind when:"1h" text:"say \"hi\"""#
        );
        assert_eq!(
            typed(&remind, &[String::new(), "x".into()], &[], &nobody, 0, &utc),
            r#"/remind text:"x""#
        );
    }

    /// A community of Ada (this account, the owner), two members both called "Alex", Bea and the Bot.
    fn alexes() -> (AppState, CommunityId) {
        use dilla_client::state::members::Member;
        use dilla_client::state::{Community, UserId};
        let c = CommunityId([7; 16]);
        let member = |n: u8, username: &str, display: &str, kind: u8| Member {
            user_id: UserId([n; 16]),
            username: username.into(),
            display: display.into(),
            kind,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        };
        let mut s = AppState::default();
        s.account.user_id = Some(UserId([1; 16]));
        s.communities.push(Community {
            id: c,
            name: "Midgard".into(),
            owner: UserId([1; 16]),
            policy_version: 1,
        });
        s.members.insert(
            c,
            vec![
                member(1, "ada", "Ada", 0),
                member(2, "alex", "Alex", 0),
                member(3, "troll", "Alex", 0),
                member(4, "bea", "", 0),
                member(5, "glenn", "Glenn", 1),
            ],
        );
        let mut access = s.roles.access_of(c);
        access.loaded = true;
        access.owner = true;
        access.kick_members = true;
        access.ban_members = true;
        s.roles.access.insert(c, access);
        (s, c)
    }

    /// w2-bot-review I-B1 (CommandHost.tsx `suggest` for a `user` option): a member field lists the people but this
    /// account and the Bot, each by name with their `@username` and their id as the value.
    #[test]
    fn a_member_field_lists_the_people_by_name_and_username_with_their_id() {
        let (s, c) = alexes();
        let all = member_suggestions(&s, Some(c), "kick", "");
        let rows: Vec<(&str, &str)> = all
            .iter()
            .map(|m| (m.label.as_str(), m.sub.as_str()))
            .collect();
        assert_eq!(
            rows,
            [("Alex", "@alex"), ("Alex", "@troll"), ("bea", "@bea")]
        );
        assert_eq!(all[1].value, dilla_client::state::UserId([3; 16]).hex());
        let troll = member_suggestions(&s, Some(c), "ban", "@trol");
        assert_eq!(troll.len(), 1, "@trol matches a username: {troll:?}");
        assert_eq!(troll[0].sub, "@troll");
        assert!(member_suggestions(&s, None, "kick", "").is_empty(), "a DM");
        // Without Kick Members nobody can be kicked, so nobody is offered.
        let mut no_kick = s.clone();
        let mut access = no_kick.roles.access_of(c);
        access.owner = false;
        access.kick_members = false;
        no_kick.roles.access.insert(c, access);
        no_kick.communities[0].owner = dilla_client::state::UserId([9; 16]);
        assert!(member_suggestions(&no_kick, Some(c), "kick", "").is_empty());
        let o = offered_with(
            &command("kick", vec![]),
            &option("user", ArgKind::User, None),
            "",
            &[],
            &all,
            false,
            &|_| None,
        );
        assert_eq!(o.title, "Members");
        assert_eq!(labels(&o), ["Alex", "Alex", "bea"]);
        let user = option("user", ArgKind::User, None);
        let typed_name = offered_with(
            &command("kick", vec![]),
            &user,
            "bea",
            &[],
            &all,
            false,
            &|_| None,
        );
        assert!(
            typed_name.can_accept,
            "a typed name is never exact: Tab or Enter picks from the list"
        );
        let picked = offered_with(
            &command("kick", vec![]),
            &user,
            "bea",
            &[],
            &all,
            true,
            &|_| None,
        );
        assert!(!picked.can_accept, "a pick is: Enter runs");
    }

    /// w2-bot-review I-B1, coordinator's ruling: only a literal user id or an `@username` names a member; a display name
    /// never does, not even one only one member seems to have.
    #[test]
    fn only_an_id_or_username_names_a_member() {
        let (s, c) = alexes();
        let resolve = |text: &str| resolve_member(&s, Some(c), "ban", text);
        let id = |n: u8| dilla_client::state::UserId([n; 16]).hex();
        assert_eq!(resolve("Alex"), None, "a display name");
        assert_eq!(resolve("@troll"), Some(id(3)));
        assert_eq!(resolve("@TROLL"), Some(id(3)));
        assert_eq!(resolve("troll"), None, "without @ it could be a name");
        assert_eq!(resolve("bea"), None);
        assert_eq!(resolve(&id(2)), Some(id(2)));
        assert_eq!(resolve("@glenn"), None, "the Bot");
        assert_eq!(resolve("@ada"), None, "oneself");
        assert_eq!(resolve(""), None);
        assert_eq!(resolve("@"), None);
    }

    /// The coordinator's ruling: two members share a display name, one of them ranked past the 20 suggestions; typing
    /// that name names neither, and the chips hold Enter on that field.
    #[test]
    fn a_shared_name_past_the_suggestion_cut_names_neither() {
        use dilla_client::state::UserId;
        use dilla_client::state::members::Member;
        let (mut s, c) = alexes();
        let rows = s.members.get_mut(&c).unwrap();
        rows.retain(|m| m.username != "troll" && m.username != "alex");
        rows.push(Member {
            user_id: UserId([0x20; 16]),
            username: "sam".into(),
            display: "Sam".into(),
            kind: 0,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        });
        for n in 0..25u8 {
            rows.push(Member {
                user_id: UserId([0x40 + n; 16]),
                username: format!("samfan{n}"),
                display: format!("Samfan {n}"),
                kind: 0,
                role_ids: Vec::new(),
                status_msg: String::new(),
                bio: String::new(),
            });
        }
        rows.push(Member {
            user_id: UserId([0x7f; 16]),
            username: "impostor".into(),
            display: "Sam".into(),
            kind: 0,
            role_ids: Vec::new(),
            status_msg: String::new(),
            bio: String::new(),
        });
        let listed = member_suggestions(&s, Some(c), "ban", "Sam");
        assert_eq!(listed.len(), 20);
        assert!(
            !listed.iter().any(|m| m.sub == "@impostor"),
            "the second Sam is past the cut"
        );
        assert_eq!(resolve_member(&s, Some(c), "ban", "Sam"), None);
        assert_eq!(
            resolve_member(&s, Some(c), "ban", "@impostor"),
            Some(UserId([0x7f; 16]).hex())
        );
        let ban = command("ban", vec![option("user", ArgKind::User, None)]);
        let mut chips = Chips::new("ban".into());
        chips.values = vec!["Sam".into()];
        assert_eq!(unnamed_member(&s, Some(c), &ban, &chips), Some(0));
        chips.values = vec!["@sam".into()];
        assert_eq!(unnamed_member(&s, Some(c), &ban, &chips), None);
        chips.values = vec!["Sam".into()];
        chips.picked = vec![Some(UserId([0x20; 16]).hex())];
        assert_eq!(unnamed_member(&s, Some(c), &ban, &chips), None, "picked");
    }

    /// w2-bot-review I-B1: picking a member puts their name in the chip and keeps their id, which the typed form sends;
    /// typing in the chip forgets the pick.
    #[test]
    fn a_picked_member_is_sent_by_id_until_the_chip_is_typed_in() {
        let (s, c) = alexes();
        let kick = command("kick", vec![option("user", ArgKind::User, None)]);
        let all = member_suggestions(&s, Some(c), "kick", "");
        let mut chips = Chips::new("kick".into());
        chips.fields = 1;
        chips.member = true;
        chips.shown = all.iter().map(|m| m.value.clone()).collect();
        chips.labels = all.iter().map(|m| m.label.clone()).collect();
        chips.accept(1);
        assert_eq!(chips.values, ["Alex"]);
        let troll = dilla_client::state::UserId([3; 16]).hex();
        assert_eq!(chips.picked, [Some(troll.clone())]);
        let member = |text: &str| resolve_member(&s, Some(c), "kick", text);
        assert_eq!(
            typed(&kick, &chips.values, &chips.picked, &member, 0, &utc),
            format!("/kick user:\"{troll}\"")
        );
        chips.active_value().push('!');
        chips.active_value().pop();
        assert_eq!(chips.picked, [None]);
        assert_eq!(
            typed(&kick, &chips.values, &chips.picked, &member, 0, &utc),
            "/kick user:\"Alex\"",
            "an unpicked Alex names nobody"
        );
    }

    fn option(name: &str, kind: ArgKind, choices: Option<Vec<String>>) -> CommandOption {
        CommandOption {
            name: name.into(),
            kind,
            description: String::new(),
            required: true,
            free_text: false,
            choices,
            min: None,
            max: None,
        }
    }

    fn command(id: &str, options: Vec<CommandOption>) -> CommandDefinition {
        use dilla_client::features::bot::registry::Provider;
        CommandDefinition {
            id: id.into(),
            description: String::new(),
            community_only: true,
            provider: Provider::Bot,
            permission_bit: None,
            options,
            bot: None,
        }
    }

    fn labels(o: &Offered) -> Vec<&str> {
        o.items.iter().map(|s| s.label.as_str()).collect()
    }

    /// picker-catalog.ts `suggestFor`: what each field suggests.
    #[test]
    fn each_field_suggests_by_its_kind() {
        let kinds = [
            (
                command("ban", vec![]),
                option("user", ArgKind::User, None),
                Suggest::Member,
            ),
            (
                command("ban", vec![]),
                option("d", ArgKind::Duration, None),
                Suggest::Duration,
            ),
            (
                command("remind", vec![]),
                option("when", ArgKind::Time, None),
                Suggest::When,
            ),
            (
                command("other", vec![]),
                option("at", ArgKind::Time, None),
                Suggest::None,
            ),
            (
                command("vote", vec![]),
                option("multi", ArgKind::Choice, Some(vec!["on".into()])),
                Suggest::Choice,
            ),
            (
                command("vote", vec![]),
                option("x", ArgKind::Choice, None),
                Suggest::Provider,
            ),
            (
                command("role", vec![]),
                option("r", ArgKind::Role, None),
                Suggest::Provider,
            ),
            (
                command("play", vec![]),
                option("query-or-url", ArgKind::Text, None),
                Suggest::Provider,
            ),
            (
                command("help", vec![]),
                option("command", ArgKind::Text, None),
                Suggest::Command,
            ),
            (
                command("me", vec![]),
                option("text", ArgKind::Text, None),
                Suggest::None,
            ),
        ];
        for (c, o, want) in kinds {
            assert_eq!(suggest_for(&c, &o), want, "{} {}", c.id, o.name);
        }
    }

    /// CommandPicker.tsx `suggestionsFor`: the prototype's durations (`/ban` adds "permanent"), filtered by the typed
    /// text; `/remind`'s phrases each with its preview; Tab accepts only a typed, not yet exact, match.
    #[test]
    fn durations_and_when_phrases_are_suggested_and_filtered() {
        let ban = command("ban", vec![]);
        let duration = option("duration", ArgKind::Duration, None);
        let none = |_: &str| None;
        let all = offered(&ban, &duration, "", &[], &none);
        assert_eq!(
            labels(&all),
            ["10 minutes", "1 hour", "1 day", "7 days", "permanent"]
        );
        assert_eq!(all.title, "Durations");
        assert!(!all.can_accept, "nothing typed: Tab moves on");
        let typed = offered(&ban, &duration, "day", &[], &none);
        assert_eq!(labels(&typed), ["1 day", "7 days"]);
        assert!(typed.can_accept && typed.enter_accepts);
        let exact = offered(&ban, &duration, "1 Day", &[], &none);
        assert!(!exact.can_accept, "an exact match is already in the field");
        let timeout = offered(&command("timeout", vec![]), &duration, "", &[], &none);
        assert!(!labels(&timeout).contains(&"permanent"));

        let remind = command("remind", vec![]);
        let when = option("when", ArgKind::Time, None);
        let preview = |v: &str| Some(format!("preview of {v}"));
        let w = offered(&remind, &when, "in", &[], &preview);
        assert_eq!(
            labels(&w),
            ["in 1 minute", "in 10 minutes", "in 1 hour"],
            "{w:?}"
        );
        assert_eq!(w.items[2].sub, "preview of in 1 hour");
    }

    /// `/help`'s command field lists the commands (their id goes in the field); `/play`'s query takes free text, so
    /// Enter runs it rather than accepting; a fixed choice is offered as is.
    #[test]
    fn commands_and_choices_are_suggested_and_play_never_accepts_on_enter() {
        let help = command("help", vec![]);
        let field = option("command", ArgKind::Text, None);
        let entry = |icon| Entry {
            icon,
            description: "Play music".into(),
            args: String::new(),
            source: String::new(),
            opens_form: false,
            fields: Vec::new(),
        };
        let listed = vec![
            ("play".to_owned(), entry(IconName::Play)),
            ("remind".to_owned(), entry(IconName::AlarmClock)),
        ];
        let o = offered(&help, &field, "pl", &listed, &|_| None);
        assert_eq!(labels(&o), ["/play"]);
        assert_eq!(o.items[0].value, "play");
        assert!(o.can_accept && o.enter_accepts);

        let play = command("play", vec![]);
        let q = offered(
            &play,
            &option("query-or-url", ArgKind::Text, None),
            "x",
            &[],
            &|_| None,
        );
        assert!(q.items.is_empty() && !q.enter_accepts);

        let vote = command("vote", vec![]);
        let multi = option(
            "multi",
            ArgKind::Choice,
            Some(vec!["on".into(), "off".into()]),
        );
        assert_eq!(
            labels(&offered(&vote, &multi, "", &[], &|_| None)),
            ["on", "off"]
        );
    }

    /// `chipKeyDown`: ↑↓ wrap over the suggestions; Tab puts the highlighted one in the field and moves on; Enter does
    /// only where the field accepts on Enter; with nothing to accept the keys are the chips' own.
    #[test]
    fn the_suggestion_keys_move_and_accept() {
        let mut c = Chips::new("ban".into());
        c.fields = 3;
        c.active = 1;
        c.shown = vec!["1 day".into(), "7 days".into()];
        c.can_accept = true;
        c.enter_accepts = true;
        assert!(c.suggestion_key("up", false));
        assert_eq!(c.sug, 1);
        assert!(c.suggestion_key("down", false));
        assert_eq!(c.sug, 0);
        assert!(c.suggestion_key("down", false));
        assert!(c.suggestion_key("tab", false));
        assert_eq!(c.values, ["", "7 days"]);
        assert_eq!((c.active, c.sug, c.can_accept), (2, 0, false));
        assert!(
            !c.suggestion_key("tab", false),
            "nothing to accept: Tab moves"
        );
        assert!(!c.suggestion_key("tab", true), "Shift+Tab is the chips'");
        c.can_accept = true;
        c.enter_accepts = false;
        assert!(!c.suggestion_key("enter", false), "Enter runs");
        c.shown.clear();
        assert!(!c.suggestion_key("down", false));
    }

    /// CommandPicker.tsx: a music command's bar names the voice room, else asks to join one.
    #[test]
    fn a_music_commands_bar_names_the_room() {
        assert_eq!(
            music_bar("Play music", Some("Lounge")),
            "Play music · Lounge"
        );
        assert_eq!(
            music_bar("Play music", None),
            "Play music · join voice first"
        );
    }

    #[test]
    fn non_ascii_when_text_never_panics() {
        let now = 1_800_000_000;
        for text in [
            "imorgon förmiddag",
            "imorgon förmidd",
            "2026-11-14ä09:00",
            "2026-11-1ä 09:00",
            "🎉🎉🎉🎉🎉🎉🎉🎉",
            "in 2🎉",
            "in ö h",
            "ö:00",
            "1ö:00",
            "tomorrow 0ö:00",
        ] {
            assert_eq!(registry_when(text, now, &utc), None, "{text}");
        }
        // Every prefix of every mixed string, as a user types it one character at a time.
        let alphabet = ['a', '1', ':', '-', ' ', 't', 'ö', 'ä', '🎉', 'İ', '\u{301}'];
        let mut seed: u64 = 0x9e37_79b9_7f4a_7c15;
        for _ in 0..4000 {
            let mut text = String::new();
            for _ in 0..24 {
                seed ^= seed << 13;
                seed ^= seed >> 7;
                seed ^= seed << 17;
                text.push(alphabet[(seed % alphabet.len() as u64) as usize]);
                let _ = registry_when(&text, now, &utc);
                let _ = registry_when(&format!("2026-11-14{text}"), now, &utc);
                let _ = registry_when(&format!("in {text}"), now, &utc);
            }
        }
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
