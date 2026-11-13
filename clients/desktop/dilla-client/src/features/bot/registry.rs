//! The typed command catalog and the slash parser: a port of packages/client-core/src/commands/{registry,classic}.ts.
//!
//! The built-in commands are fixed here ([`commands`]); a community Bot's commands come from its catalog
//! ([`bot_command_definitions`]), which can never shadow or extend a built-in. Nothing here does I/O.

use std::collections::BTreeMap;
use std::sync::OnceLock;

use crate::state::bot::{ArgType, BotCommandSpec};

/// `CommandArgumentKind`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ArgKind {
    Text,
    User,
    Channel,
    Role,
    Message,
    QueueItem,
    Poll,
    Announcement,
    Reminder,
    Choice,
    Number,
    Duration,
    Time,
}

/// Where a command runs: on this device, or sealed to the community's Bot.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Provider {
    Builtin,
    Bot,
}

/// `CommandOption`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CommandOption {
    pub name: String,
    pub kind: ArgKind,
    pub description: String,
    pub required: bool,
    /// A free-text tail takes every remaining positional token.
    pub free_text: bool,
    pub choices: Option<Vec<String>>,
    pub min: Option<i64>,
    pub max: Option<i64>,
}

/// A Bot command's catalog binding: the operation it is sealed as and each option's argument type.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct BotBinding {
    pub operation: u32,
    pub arg_types: Vec<ArgType>,
    pub needs_bot_channel: bool,
    pub handler_kind: u32,
}

/// `CommandDefinition`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct CommandDefinition {
    /// Canonical, space-separated path (`role add`).
    pub id: String,
    pub description: String,
    /// `true`: only in a community (`scope: 'community'`).
    pub community_only: bool,
    pub provider: Provider,
    pub permission_bit: Option<u64>,
    pub options: Vec<CommandOption>,
    pub bot: Option<BotBinding>,
}

fn opt(name: &str, kind: ArgKind, required: bool) -> CommandOption {
    CommandOption {
        name: name.to_owned(),
        kind,
        description: format!("{name} argument"),
        required,
        free_text: false,
        choices: None,
        min: None,
        max: None,
    }
}

fn text(name: &str, required: bool) -> CommandOption {
    CommandOption {
        free_text: true,
        ..opt(name, ArgKind::Text, required)
    }
}

fn def(
    id: &str,
    description: &str,
    any: bool,
    permission: Option<u32>,
    options: Vec<CommandOption>,
) -> CommandDefinition {
    CommandDefinition {
        id: id.to_owned(),
        description: description.to_owned(),
        community_only: !any,
        provider: Provider::Builtin,
        permission_bit: permission.map(|b| 1u64 << b),
        options,
        bot: None,
    }
}

/// `COMMANDS`: the 35 built-in commands, in the registry's order.
pub fn commands() -> &'static [CommandDefinition] {
    static ALL: OnceLock<Vec<CommandDefinition>> = OnceLock::new();
    ALL.get_or_init(|| {
        use ArgKind::{Channel, Duration, Message, Role, User};
        let msg = || opt("message", Message, true);
        let user = || opt("user", User, true);
        let role = || opt("role", Role, true);
        vec![
            def(
                "help",
                "Show command help",
                true,
                None,
                vec![opt("command", ArgKind::Text, false)],
            ),
            def(
                "me",
                "Send an action message",
                true,
                None,
                vec![text("text", true)],
            ),
            def(
                "shrug",
                "Send a shrug",
                true,
                None,
                vec![text("text", false)],
            ),
            def(
                "tableflip",
                "Flip a table",
                true,
                None,
                vec![text("text", false)],
            ),
            def(
                "unflip",
                "Put a table back",
                true,
                None,
                vec![text("text", false)],
            ),
            def(
                "code",
                "Send a code block",
                true,
                None,
                vec![opt("language", ArgKind::Text, true), text("text", true)],
            ),
            def(
                "giphy",
                "Find a GIF",
                false,
                None,
                vec![text("query", false)],
            ),
            def("dm", "Open a direct message", true, None, vec![user()]),
            def(
                "invite",
                "Create a community invite",
                false,
                Some(11),
                vec![],
            ),
            def(
                "search",
                "Search messages",
                true,
                None,
                vec![text("query", true), opt("scope", ArgKind::Text, false)],
            ),
            def(
                "topic",
                "Set the channel topic",
                false,
                Some(14),
                vec![text("text", true)],
            ),
            def(
                "thread",
                "Start a thread from a message",
                false,
                None,
                vec![msg()],
            ),
            def(
                "reply",
                "Reply to a message",
                true,
                None,
                vec![msg(), text("text", true)],
            ),
            def(
                "edit",
                "Edit a message",
                true,
                None,
                vec![msg(), text("text", true)],
            ),
            def(
                "delete",
                "Delete a message",
                true,
                None,
                vec![msg(), text("reason", false)],
            ),
            def("pin", "Pin a message", true, Some(3), vec![msg()]),
            def("unpin", "Unpin a message", true, Some(3), vec![msg()]),
            def(
                "join",
                "Join a voice channel",
                false,
                None,
                vec![opt("voice-channel", Channel, true)],
            ),
            def("leave", "Leave voice", true, None, vec![]),
            def("mute", "Mute your microphone", true, None, vec![]),
            def("unmute", "Unmute your microphone", true, None, vec![]),
            def("deafen", "Deafen yourself", true, None, vec![]),
            def("undeafen", "Stop deafening yourself", true, None, vec![]),
            def(
                "kick",
                "Remove a member from the community",
                false,
                Some(12),
                vec![user()],
            ),
            def(
                "ban",
                "Ban a member",
                false,
                Some(13),
                vec![
                    user(),
                    opt("duration", Duration, false),
                    text("reason", false),
                ],
            ),
            def("unban", "Unban a member", false, Some(13), vec![user()]),
            def(
                "timeout",
                "Temporarily restrict a member",
                false,
                Some(24),
                vec![user(), opt("duration", Duration, true)],
            ),
            def(
                "untimeout",
                "Remove a member timeout",
                false,
                Some(24),
                vec![user()],
            ),
            def(
                "warn",
                "Warn a member",
                false,
                Some(24),
                vec![user(), text("reason", true)],
            ),
            def("modlog", "Show moderation history", false, Some(17), vec![]),
            def(
                "role add",
                "Add a role to a member",
                false,
                Some(15),
                vec![user(), role()],
            ),
            def(
                "role remove",
                "Remove a role from a member",
                false,
                Some(15),
                vec![user(), role()],
            ),
            def(
                "selfrole list",
                "List self-assignable roles",
                false,
                None,
                vec![],
            ),
            def(
                "selfrole add",
                "Add one of your self-assignable roles",
                false,
                None,
                vec![role()],
            ),
            def(
                "selfrole remove",
                "Remove one of your self-assignable roles",
                false,
                None,
                vec![role()],
            ),
        ]
    })
}

/// `commandById`: a built-in command.
pub fn command_by_id(id: &str) -> Option<&'static CommandDefinition> {
    commands().iter().find(|c| c.id == id)
}

fn builtin_first_word(word: &str) -> bool {
    commands()
        .iter()
        .any(|c| c.id.split(' ').next() == Some(word))
}

/// `ENTITY_TEXT`: a text argument the picker can offer from what this device shows, by argument name.
fn entity_text(name: &str) -> Option<ArgKind> {
    Some(match name {
        "poll" => ArgKind::Poll,
        "reminder" => ArgKind::Reminder,
        "announcement" => ArgKind::Announcement,
        "role" => ArgKind::Role,
        "message" => ArgKind::Message,
        "item" => ArgKind::QueueItem,
        _ => return None,
    })
}

fn kind_of_type(t: ArgType) -> ArgKind {
    match t {
        ArgType::Text => ArgKind::Text,
        ArgType::Uint => ArgKind::Number,
        ArgType::User => ArgKind::User,
        ArgType::Channel => ArgKind::Channel,
        ArgType::Duration => ArgKind::Duration,
        ArgType::Bool => ArgKind::Choice,
    }
}

/// `botCommandDefinitions`: the picker's definitions of a Bot catalog's commands. Only handler kind 0 is a picker
/// command; a name whose first word is a built-in's first word is dropped (M9), so a Bot cannot shadow or extend one.
pub fn bot_command_definitions(catalog: &[BotCommandSpec]) -> Vec<CommandDefinition> {
    catalog
        .iter()
        .filter(|c| {
            c.handler_kind == 0 && !builtin_first_word(c.name.split(' ').next().unwrap_or(""))
        })
        .map(|c| {
            let last_text = c.args.iter().rposition(|a| a.ty == ArgType::Text);
            CommandDefinition {
                id: c.name.clone(),
                description: c.description.clone(),
                community_only: true,
                provider: Provider::Bot,
                permission_bit: None,
                options: c
                    .args
                    .iter()
                    .enumerate()
                    .map(|(index, arg)| {
                        let kind = if arg.ty == ArgType::Duration
                            && c.name == "remind"
                            && arg.name == "when"
                        {
                            ArgKind::Time
                        } else if arg.ty == ArgType::Text {
                            entity_text(&arg.name).unwrap_or(ArgKind::Text)
                        } else {
                            kind_of_type(arg.ty)
                        };
                        CommandOption {
                            free_text: arg.ty == ArgType::Text
                                && kind == ArgKind::Text
                                && Some(index) == last_text,
                            min: (arg.ty == ArgType::Uint).then_some(0),
                            choices: (arg.ty == ArgType::Bool)
                                .then(|| vec!["on".to_owned(), "off".to_owned()]),
                            ..opt(&arg.name, kind, arg.required)
                        }
                    })
                    .collect(),
                bot: Some(BotBinding {
                    operation: c.operation,
                    arg_types: c.args.iter().map(|a| a.ty).collect(),
                    needs_bot_channel: c.needs_bot_channel,
                    handler_kind: c.handler_kind,
                }),
            }
        })
        .collect()
}

/// `CommandContext`: where a command would run.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct CommandContext {
    pub in_community: bool,
    /// The member's permission bits there; `None` while unknown.
    pub bits: Option<u64>,
}

/// `commandAvailability.reason`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Unavailable {
    CommunityRequired,
    PermissionUnknown,
    PermissionDenied,
}

/// `commandAvailability`: fails closed while a community permission is unknown.
pub fn command_availability(
    command: &CommandDefinition,
    context: &CommandContext,
) -> Result<(), Unavailable> {
    if command.community_only && !context.in_community {
        return Err(Unavailable::CommunityRequired);
    }
    if let Some(bit) = command.permission_bit {
        if !command.community_only && !context.in_community {
            return Ok(());
        }
        let Some(bits) = context.bits else {
            return Err(Unavailable::PermissionUnknown);
        };
        if bits & bit != bit {
            return Err(Unavailable::PermissionDenied);
        }
    }
    Ok(())
}

/// `rankCommands`: exact, then prefix, then substring of the id or description; ties keep catalog order; at most 25.
pub fn rank_commands<'a>(
    commands: impl IntoIterator<Item = &'a CommandDefinition>,
    query: &str,
    limit: usize,
) -> Vec<&'a CommandDefinition> {
    let needle = query.trim().to_lowercase();
    let mut ranked: Vec<(u8, usize, &CommandDefinition)> = commands
        .into_iter()
        .enumerate()
        .filter_map(|(index, command)| {
            let id = command.id.to_lowercase();
            let description = command.description.to_lowercase();
            let rank = if needle.is_empty() {
                3
            } else if id == needle {
                0
            } else if id.starts_with(&needle) {
                1
            } else if id.contains(&needle) || description.contains(&needle) {
                2
            } else {
                return None;
            };
            Some((rank, index, command))
        })
        .collect();
    ranked.sort_by_key(|&(rank, index, _)| (rank, index));
    ranked
        .into_iter()
        .take(limit.min(25))
        .map(|(_, _, c)| c)
        .collect()
}

/// `ArgumentIssue.code`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum IssueCode {
    Missing,
    Unexpected,
    Empty,
    Invalid,
    Duplicate,
}

/// `ArgumentIssue`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ArgumentIssue {
    pub code: IssueCode,
    pub name: String,
    pub message: String,
}

fn issue(code: IssueCode, name: &str, message: String) -> ArgumentIssue {
    ArgumentIssue {
        code,
        name: name.to_owned(),
        message,
    }
}

/// Named arguments, by option name.
pub type Args = BTreeMap<String, String>;

/// `CommandValidation`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Validation {
    pub valid: bool,
    pub issues: Vec<ArgumentIssue>,
    pub args: Args,
}

/// `isDuration`: `10m`, `1h30m`, `1.5h`.
pub fn is_duration(value: &str) -> bool {
    duration_millis(value).is_some()
}

/// `durationMillis`: the sum of `<number><unit>` parts that make up the whole (trimmed) value.
pub fn duration_millis(value: &str) -> Option<f64> {
    let v = value.trim();
    if v.is_empty() {
        return None;
    }
    let bytes = v.as_bytes();
    let (mut i, mut total) = (0, 0.0);
    while i < bytes.len() {
        let start = i;
        while i < bytes.len() && bytes[i].is_ascii_digit() {
            i += 1;
        }
        if i == start {
            return None;
        }
        if i < bytes.len() && bytes[i] == b'.' {
            i += 1;
            let frac = i;
            while i < bytes.len() && bytes[i].is_ascii_digit() {
                i += 1;
            }
            if i == frac {
                return None;
            }
        }
        let amount: f64 = v[start..i].parse().ok()?;
        let scale = if v[i..].starts_with("ms") {
            i += 2;
            1.0
        } else {
            let unit = *bytes.get(i)?;
            i += 1;
            match unit {
                b's' => 1_000.0,
                b'm' => 60_000.0,
                b'h' => 3_600_000.0,
                b'd' => 86_400_000.0,
                b'w' => 604_800_000.0,
                _ => return None,
            }
        };
        total += amount * scale;
    }
    Some(total)
}

/// `isIsoLocalDateTime`: `2026-10-26T09:30`, optional seconds, fraction and offset.
pub fn is_iso_local_date_time(value: &str) -> bool {
    iso_parts(value.trim()).is_some()
}

/// The parts of an ISO date-time: `(y, mo, d, h, mi, s, offset minutes)`.
/// `(year, month, day, hour, minute, second, offset minutes, a nonzero fraction)`.
pub(crate) type IsoParts = (i32, u32, u32, u32, u32, u32, Option<i32>, bool);

pub(crate) fn iso_parts(v: &str) -> Option<IsoParts> {
    let b = v.as_bytes();
    let num = |s: usize, n: usize| -> Option<u32> {
        let part = v.get(s..s + n)?;
        part.bytes()
            .all(|c| c.is_ascii_digit())
            .then(|| part.parse().ok())?
    };
    if b.len() < 16
        || b[4] != b'-'
        || b[7] != b'-'
        || !(b[10] == b'T' || b[10] == b' ')
        || b[13] != b':'
    {
        return None;
    }
    let (y, mo, d, h, mi) = (num(0, 4)?, num(5, 2)?, num(8, 2)?, num(11, 2)?, num(14, 2)?);
    let mut i = 16;
    let mut s = 0;
    let mut fraction_nonzero = false;
    if b.get(i) == Some(&b':') {
        s = num(i + 1, 2)?;
        i += 3;
        if b.get(i) == Some(&b'.') {
            let start = i + 1;
            let mut j = start;
            while j < b.len() && b[j].is_ascii_digit() {
                j += 1;
            }
            if j == start || j - start > 9 {
                return None;
            }
            fraction_nonzero = v[start..j].bytes().any(|c| c != b'0');
            i = j;
        }
    }
    let offset = match b.get(i) {
        None => None,
        Some(b'Z') if i + 1 == b.len() => Some(0),
        Some(&sign @ (b'+' | b'-')) if b.len() == i + 6 && b[i + 3] == b':' => {
            let (oh, om) = (num(i + 1, 2)?, num(i + 4, 2)?);
            if oh > 23 || om > 59 {
                return None;
            }
            let m = (oh * 60 + om) as i32;
            Some(if sign == b'+' { m } else { -m })
        }
        _ => return None,
    };
    let days = days_in_month(y as i32, mo)?;
    if d < 1 || d > days || h > 23 || mi > 59 || s > 59 {
        return None;
    }
    Some((y as i32, mo, d, h, mi, s, offset, fraction_nonzero))
}

pub(crate) fn days_in_month(y: i32, m: u32) -> Option<u32> {
    let leap = (y % 4 == 0 && y % 100 != 0) || y % 400 == 0;
    Some(match m {
        1 | 3 | 5 | 7 | 8 | 10 | 12 => 31,
        4 | 6 | 9 | 11 => 30,
        2 if leap => 29,
        2 => 28,
        _ => return None,
    })
}

fn is_reminder_when(value: &str) -> bool {
    duration_millis(value).is_some_and(|ms| (60_000.0..=365.0 * 86_400_000.0).contains(&ms))
        || is_iso_local_date_time(value)
}

fn is_clock(value: &str) -> bool {
    let parts: Vec<&str> = value.trim().split(':').collect();
    (2..=3).contains(&parts.len())
        && !parts[0].is_empty()
        && parts[0].bytes().all(|c| c.is_ascii_digit())
        && parts[1..]
            .iter()
            .all(|p| (1..=2).contains(&p.len()) && p.bytes().all(|c| c.is_ascii_digit()))
}

fn is_time_value(value: &str) -> bool {
    is_duration(value) || is_clock(value) || is_iso_local_date_time(value)
}

/// `/^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$/`.
fn is_decimal(v: &str) -> bool {
    let v = v.strip_prefix(['+', '-']).unwrap_or(v);
    let (int, frac) = v.split_once('.').map_or((v, None), |(a, b)| (a, Some(b)));
    let digits = |s: &str| s.bytes().all(|c| c.is_ascii_digit());
    match frac {
        None => !int.is_empty() && digits(int),
        Some(f) => digits(int) && digits(f) && !(int.is_empty() && f.is_empty()),
    }
}

/// `parseSeekPosition`: whole seconds or `mm:ss` / `hh:mm:ss`, as milliseconds.
pub fn parse_seek_position(raw: &str) -> Option<u64> {
    let parts: Vec<&str> = raw.split(':').collect();
    if parts.len() > 3
        || parts
            .iter()
            .any(|p| p.is_empty() || !p.bytes().all(|c| c.is_ascii_digit()))
    {
        return None;
    }
    let numbers: Vec<u64> = parts
        .iter()
        .map(|p| p.parse().ok())
        .collect::<Option<_>>()?;
    if numbers[1..].iter().any(|&n| n >= 60) {
        return None;
    }
    numbers
        .iter()
        .try_fold(0u64, |s, &p| s.checked_mul(60)?.checked_add(p))?
        .checked_mul(1000)
}

/// `validateCommandArgs`.
pub fn validate_command_args(command: &CommandDefinition, input: &Args) -> Validation {
    let mut issues = Vec::new();
    let mut args = Args::new();
    for (name, value) in input {
        let Some(option) = command.options.iter().find(|o| &o.name == name) else {
            issues.push(issue(
                IssueCode::Unexpected,
                name,
                format!("Unexpected argument: {name}"),
            ));
            continue;
        };
        if value.trim().is_empty() {
            issues.push(issue(
                IssueCode::Empty,
                name,
                format!("{name} cannot be empty"),
            ));
            continue;
        }
        args.insert(name.clone(), value.clone());
        let invalid = match option.kind {
            ArgKind::Choice => option
                .choices
                .as_ref()
                .is_some_and(|c| !c.iter().any(|x| x == value))
                .then(|| {
                    format!(
                        "{name} must be one of: {}",
                        option.choices.as_deref().unwrap_or_default().join(", ")
                    )
                }),
            ArgKind::Number => {
                let n: Option<f64> = value.trim().parse().ok();
                let bad = !is_decimal(value.trim())
                    || n.is_none_or(|n| {
                        !n.is_finite()
                            || option.min.is_some_and(|m| n < m as f64)
                            || option.max.is_some_and(|m| n > m as f64)
                    });
                bad.then(|| {
                    let range = if option.min.is_some() || option.max.is_some() {
                        format!(
                            " from {} to {}",
                            option.min.map_or("−∞".to_owned(), |m| m.to_string()),
                            option.max.map_or("∞".to_owned(), |m| m.to_string())
                        )
                    } else {
                        String::new()
                    };
                    format!("{name} must be a number{range}")
                })
            }
            ArgKind::Duration
                if !is_duration(value) && !(command.id == "ban" && value == "permanent") =>
            {
                Some(format!("{name} must be a duration such as 10m or 2h"))
            }
            _ if command.id == "seek"
                && name == "position"
                && parse_seek_position(value).is_none() =>
            {
                Some("position must be seconds, mm:ss or hh:mm:ss".to_owned())
            }
            ArgKind::Time if command.id != "seek" => {
                let ok = if command.id == "remind" && name == "when" {
                    is_reminder_when(value)
                } else {
                    is_time_value(value)
                };
                (!ok).then(|| {
                    format!(
                        "{name} must be a duration or ISO local date and time{}",
                        if command.id == "remind" {
                            " (from 1 minute to 365 days for durations)"
                        } else {
                            ""
                        }
                    )
                })
            }
            _ => None,
        };
        if let Some(message) = invalid {
            issues.push(issue(IssueCode::Invalid, name, message));
        }
    }
    for option in &command.options {
        if option.required && !args.contains_key(&option.name) {
            issues.push(issue(
                IssueCode::Missing,
                &option.name,
                format!("Missing required argument: {}", option.name),
            ));
        }
    }
    Validation {
        valid: issues.is_empty(),
        issues,
        args,
    }
}

/// `CommandParseResult`.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Parsed {
    /// Not a command: post this text (`//x` posts `/x`).
    Literal(String),
    /// `/xyz` names no command here.
    Unknown {
        command_text: String,
    },
    /// A command whose arguments are not complete yet.
    Incomplete {
        command_id: String,
        validation: Validation,
        subcommands: Vec<String>,
    },
    Invocation {
        command_id: String,
        args: Args,
    },
    Invalid {
        issues: Vec<ArgumentIssue>,
    },
}

#[derive(Clone, Debug)]
struct Token {
    value: String,
    start: usize,
    end: usize,
    unquoted_colon: bool,
}

fn tokenize(source: &str) -> Option<Vec<Token>> {
    let mut tokens = Vec::new();
    let (mut value, mut quote, mut escaped) = (String::new(), None::<char>, false);
    let (mut start, mut active, mut colon) = (0, false, false);
    for (i, ch) in source.char_indices() {
        if !active && ch.is_whitespace() {
            continue;
        }
        if !active {
            active = true;
            start = i;
        }
        if escaped {
            value.push(ch);
            escaped = false;
            continue;
        }
        if ch == '\\' && quote.is_some() {
            escaped = true;
            continue;
        }
        if let Some(q) = quote {
            if ch == q {
                quote = None;
            } else {
                value.push(ch);
            }
            continue;
        }
        if ch == '"' || ch == '\'' {
            quote = Some(ch);
            continue;
        }
        if ch.is_whitespace() {
            tokens.push(Token {
                value: std::mem::take(&mut value),
                start,
                end: i,
                unquoted_colon: colon,
            });
            active = false;
            colon = false;
            continue;
        }
        if ch == ':' {
            colon = true;
        }
        value.push(ch);
    }
    if escaped || quote.is_some() {
        return None;
    }
    if active {
        tokens.push(Token {
            value,
            start,
            end: source.len(),
            unquoted_colon: colon,
        });
    }
    Some(tokens)
}

/// `/^[a-z][a-z0-9_-]*:/i` and not `/^[a-z][a-z0-9+.-]*:\/\//i`.
fn option_shaped(v: &str) -> bool {
    let Some((head, rest)) = v.split_once(':') else {
        return false;
    };
    let mut chars = head.chars();
    let lead = chars.next().is_some_and(|c| c.is_ascii_alphabetic());
    let named = lead
        && chars
            .clone()
            .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-');
    let scheme = lead
        && head
            .chars()
            .skip(1)
            .all(|c| c.is_ascii_alphanumeric() || matches!(c, '+' | '.' | '-'))
        && rest.starts_with("//");
    named && !scheme
}

/// `parseCommand` against `commands` (the built-ins, plus the community Bot's catalog commands in the picker).
pub fn parse_command(input: &str, commands: &[&CommandDefinition]) -> Parsed {
    if !input.starts_with('/') || input.starts_with("//") {
        let literal = input
            .strip_prefix("//")
            .map_or_else(|| input.to_owned(), |rest| format!("/{rest}"));
        return Parsed::Literal(literal);
    }
    let raw = &input[1..];
    let Some(tokens) = tokenize(raw) else {
        return Parsed::Invalid {
            issues: vec![issue(
                IssueCode::Invalid,
                "",
                "Unclosed quote or escape".to_owned(),
            )],
        };
    };
    if tokens.is_empty() {
        return Parsed::Unknown {
            command_text: String::new(),
        };
    }
    let words: Vec<String> = tokens
        .iter()
        .take_while(|t| !t.value.contains(':'))
        .map(|t| t.value.to_lowercase())
        .collect();
    let mut found: Option<(&CommandDefinition, usize)> = None;
    for count in 1..=words.len() {
        let path = words[..count].join(" ");
        if let Some(c) = commands.iter().find(|c| c.id == path) {
            found = Some((c, count));
        }
    }
    let Some((command, word_count)) = found else {
        let path = words.join(" ");
        let mut subcommands: Vec<String> = Vec::new();
        if !path.is_empty() {
            let prefix = format!("{path} ");
            for c in commands {
                if let Some(rest) = c.id.strip_prefix(&prefix) {
                    let first = rest.split(' ').next().unwrap_or("").to_owned();
                    if !subcommands.contains(&first) {
                        subcommands.push(first);
                    }
                }
            }
        }
        if !subcommands.is_empty() && tokens.len() == words.len() {
            let missing = issue(
                IssueCode::Missing,
                "subcommand",
                format!("Choose a subcommand: {}", subcommands.join(", ")),
            );
            return Parsed::Incomplete {
                command_id: path,
                validation: Validation {
                    valid: false,
                    issues: vec![missing],
                    args: Args::new(),
                },
                subcommands,
            };
        }
        return Parsed::Unknown { command_text: path };
    };

    let mut args = Args::new();
    let mut issues = Vec::new();
    let mut positional: Vec<&Token> = Vec::new();
    let mut named_tokens: Vec<&Token> = Vec::new();
    let free_text_only = command.options.len() == 1 && command.options[0].free_text;
    for token in &tokens[word_count..] {
        let colon = token.value.find(':');
        let name = colon.map(|c| token.value[..c].to_lowercase());
        let named = token.unquoted_colon
            && name
                .as_ref()
                .is_some_and(|n| command.options.iter().any(|o| &o.name == n));
        if named {
            named_tokens.push(token);
            let (name, colon) = (name.unwrap_or_default(), colon.unwrap_or(0));
            let value = &token.value[colon + 1..];
            if args.contains_key(&name) {
                issues.push(issue(
                    IssueCode::Duplicate,
                    &name,
                    format!("Argument supplied more than once: {name}"),
                ));
                continue;
            }
            if value.is_empty() {
                issues.push(issue(
                    IssueCode::Empty,
                    &name,
                    format!("{name} cannot be empty"),
                ));
                continue;
            }
            args.insert(name, value.to_owned());
        } else if token.unquoted_colon && option_shaped(&token.value) && !free_text_only {
            let name = name.unwrap_or_default();
            issues.push(issue(
                IssueCode::Unexpected,
                &name,
                format!("Unknown named argument: {name}"),
            ));
        } else {
            positional.push(token);
        }
    }
    let mut pos = 0;
    for option in &command.options {
        if args.contains_key(&option.name) || pos >= positional.len() {
            continue;
        }
        let value = if option.free_text {
            positional_free_text(raw, &positional[pos..], &named_tokens)
        } else {
            positional[pos].value.clone()
        };
        args.insert(option.name.clone(), value);
        pos = if option.free_text {
            positional.len()
        } else {
            pos + 1
        };
    }
    if pos < positional.len() {
        let rest: Vec<&str> = positional[pos..].iter().map(|t| t.value.as_str()).collect();
        issues.push(issue(
            IssueCode::Unexpected,
            "",
            format!("Unexpected argument: {}", rest.join(" ")),
        ));
    }
    if !issues.is_empty() {
        return Parsed::Invalid { issues };
    }
    let validation = validate_command_args(command, &args);
    if !validation.valid {
        return Parsed::Incomplete {
            command_id: command.id.clone(),
            validation,
            subcommands: Vec::new(),
        };
    }
    Parsed::Invocation {
        command_id: command.id.clone(),
        args: validation.args,
    }
}

/// Positional free-text tokens joined with their original whitespace, leaving out named arguments between them.
fn positional_free_text(source: &str, tokens: &[&Token], named: &[&Token]) -> String {
    let Some(first) = tokens.first() else {
        return String::new();
    };
    let mut result = first.value.clone();
    for pair in tokens.windows(2) {
        let (previous, current) = (pair[0], pair[1]);
        let mut cursor = previous.end;
        for n in named {
            if n.start < cursor || n.end > current.start {
                continue;
            }
            result.push_str(&source[cursor..n.start]);
            cursor = n.end;
        }
        result.push_str(&source[cursor..current.start]);
        result.push_str(&current.value);
    }
    result
}

/// classic.ts `CLASSIC_COMMAND_PAYLOADS` (Markdown-escaped arms for the shrug).
pub fn classic_payload(command: &str) -> Option<&'static str> {
    Some(match command {
        "shrug" => "¯\\\\\\_(ツ)\\_/¯",
        "tableflip" => "(╯°□°)╯︵ ┻━┻",
        "unflip" => "┬─┬ ノ( ゜-゜ノ)",
        _ => return None,
    })
}

/// `formatClassicCommandMessage`.
pub fn classic_message(command: &str, text: Option<&str>) -> Option<String> {
    let payload = classic_payload(command)?;
    Some(match text.filter(|t| !t.is_empty()) {
        Some(t) => format!("{payload} {t}"),
        None => payload.to_owned(),
    })
}

/// builtin.ts `helpText`: usage, description and each option.
pub fn help_text(id: &str) -> Option<String> {
    let d = command_by_id(id.trim())?;
    let usage: Vec<String> = d
        .options
        .iter()
        .map(|o| {
            if o.required {
                format!("<{}>", o.name)
            } else {
                format!("[{}]", o.name)
            }
        })
        .collect();
    let details: Vec<String> = d
        .options
        .iter()
        .map(|o| {
            let mut line = format!(
                "• {} ({}, {}): {}",
                o.name,
                kind_name(o.kind),
                if o.required { "required" } else { "optional" },
                o.description
            );
            if let Some(c) = &o.choices {
                line.push_str(&format!(" Allowed: {}.", c.join(", ")));
            }
            if o.min.is_some() || o.max.is_some() {
                line.push_str(&format!(
                    " Range: {} to {}.",
                    o.min.map_or("−∞".to_owned(), |m| m.to_string()),
                    o.max.map_or("∞".to_owned(), |m| m.to_string())
                ));
            }
            line
        })
        .collect();
    let mut out = format!("/{}", d.id);
    if !usage.is_empty() {
        out.push(' ');
        out.push_str(&usage.join(" "));
    }
    out.push('\n');
    out.push_str(&d.description);
    if !details.is_empty() {
        out.push('\n');
        out.push_str(&details.join("\n"));
    }
    Some(out)
}

/// The help for every built-in command (`COMMANDS_HELP`).
pub fn all_help() -> String {
    commands()
        .iter()
        .filter_map(|c| help_text(&c.id))
        .collect::<Vec<_>>()
        .join("\n\n")
}

fn kind_name(kind: ArgKind) -> &'static str {
    match kind {
        ArgKind::Text => "text",
        ArgKind::User => "user",
        ArgKind::Channel => "channel",
        ArgKind::Role => "role",
        ArgKind::Message => "message",
        ArgKind::QueueItem => "queue-item",
        ArgKind::Poll => "poll",
        ArgKind::Announcement => "announcement",
        ArgKind::Reminder => "reminder",
        ArgKind::Choice => "choice",
        ArgKind::Number => "number",
        ArgKind::Duration => "duration",
        ArgKind::Time => "time",
    }
}

#[cfg(test)]
mod tests {
    //! Ported from packages/client-core/src/commands/registry.test.ts and classic.test.ts.
    use super::*;
    use crate::state::bot::ArgShape;

    fn spec(
        name: &str,
        operation: u32,
        args: &[(&str, ArgType, bool)],
        needs: bool,
    ) -> BotCommandSpec {
        BotCommandSpec {
            name: name.to_owned(),
            description: String::new(),
            operation,
            args: args
                .iter()
                .map(|&(n, ty, required)| ArgShape {
                    name: n.to_owned(),
                    ty,
                    required,
                })
                .collect(),
            permission_bit: 0,
            needs_bot_channel: needs,
            handler_kind: 0,
        }
    }

    fn catalog() -> Vec<BotCommandSpec> {
        use ArgType::*;
        vec![
            spec("play", 12, &[("query-or-url", Text, true)], false),
            spec("queue", 10, &[], false),
            spec("queue remove", 21, &[("item", Uint, true)], false),
            spec(
                "remind",
                80,
                &[("when", Duration, true), ("text", Text, true)],
                false,
            ),
            spec(
                "poll create",
                70,
                &[
                    ("question", Text, true),
                    ("multi", Bool, false),
                    ("channel", Channel, false),
                    ("for", Duration, false),
                    ("who", User, false),
                ],
                true,
            ),
        ]
    }

    fn all() -> Vec<CommandDefinition> {
        let mut v = commands().to_vec();
        v.extend(bot_command_definitions(&catalog()));
        v
    }

    fn parse(input: &str, defs: &[CommandDefinition]) -> Parsed {
        let refs: Vec<&CommandDefinition> = defs.iter().collect();
        parse_command(input, &refs)
    }

    fn builtin(input: &str) -> Parsed {
        parse(input, commands())
    }

    fn args(pairs: &[(&str, &str)]) -> Args {
        pairs
            .iter()
            .map(|(k, v)| ((*k).to_owned(), (*v).to_owned()))
            .collect()
    }

    #[test]
    fn unique_builtin_ids_only() {
        let ids: Vec<&str> = commands().iter().map(|c| c.id.as_str()).collect();
        assert_eq!(ids.len(), 35);
        let mut sorted = ids.clone();
        sorted.sort_unstable();
        sorted.dedup();
        assert_eq!(sorted.len(), 35);
        for id in [
            "help",
            "me",
            "code",
            "dm",
            "search",
            "reply",
            "join",
            "kick",
            "timeout",
            "role add",
            "selfrole list",
        ] {
            assert!(command_by_id(id).is_some(), "{id}");
        }
        for id in ["play", "queue", "remind", "poll create", "reminders list"] {
            assert!(command_by_id(id).is_none(), "{id}");
        }
    }

    #[test]
    fn catalog_to_bot_definitions() {
        let bot = bot_command_definitions(&catalog());
        let ids: Vec<(&str, u32)> = bot
            .iter()
            .map(|c| (c.id.as_str(), c.bot.as_ref().unwrap().operation))
            .collect();
        assert_eq!(
            ids,
            [
                ("play", 12),
                ("queue", 10),
                ("queue remove", 21),
                ("remind", 80),
                ("poll create", 70)
            ]
        );
        assert!(bot[0].options[0].free_text);
        assert_eq!(bot[2].options[0].kind, ArgKind::Number);
        assert_eq!(bot[2].options[0].min, Some(0));
        let kinds: Vec<ArgKind> = bot[3].options.iter().map(|o| o.kind).collect();
        assert_eq!(kinds, [ArgKind::Time, ArgKind::Text]);
        let poll: Vec<(&str, ArgKind, bool)> = bot[4]
            .options
            .iter()
            .map(|o| (o.name.as_str(), o.kind, o.required))
            .collect();
        assert_eq!(
            poll,
            [
                ("question", ArgKind::Text, true),
                ("multi", ArgKind::Choice, false),
                ("channel", ArgKind::Channel, false),
                ("for", ArgKind::Duration, false),
                ("who", ArgKind::User, false)
            ]
        );
        assert_eq!(
            bot[4].options[1].choices.as_deref(),
            Some(&["on".to_owned(), "off".to_owned()][..])
        );
        let vote = bot_command_definitions(&[spec(
            "poll vote",
            73,
            &[
                ("poll", ArgType::Text, true),
                ("choice", ArgType::Text, false),
            ],
            false,
        )]);
        assert_eq!(
            vote[0]
                .options
                .iter()
                .map(|o| (o.kind, o.free_text))
                .collect::<Vec<_>>(),
            [(ArgKind::Poll, false), (ArgKind::Text, true)]
        );
        let mut help = catalog()[0].clone();
        help.name = "help".to_owned();
        assert!(bot_command_definitions(&[help]).is_empty());
        let shadows: Vec<BotCommandSpec> = [
            "shrug hello",
            "giphy cat",
            "role list",
            "selfrole x",
            "queue clear",
        ]
        .iter()
        .map(|n| BotCommandSpec {
            name: (*n).to_owned(),
            ..catalog()[0].clone()
        })
        .collect();
        let kept: Vec<String> = bot_command_definitions(&shadows)
            .into_iter()
            .map(|c| c.id)
            .collect();
        assert_eq!(kept, ["queue clear"]);
    }

    #[test]
    fn only_handler_kind_zero_is_offered() {
        let mut all = catalog();
        for kind in [1, 2, 7] {
            all.push(BotCommandSpec {
                name: format!("music state {kind}"),
                handler_kind: kind,
                ..catalog()[1].clone()
            });
        }
        let ids: Vec<String> = bot_command_definitions(&all)
            .into_iter()
            .map(|c| c.id)
            .collect();
        assert_eq!(
            ids,
            ["play", "queue", "queue remove", "remind", "poll create"]
        );
    }

    #[test]
    fn validates_arguments_across_kinds() {
        let bot = bot_command_definitions(&catalog());
        assert!(!validate_command_args(command_by_id("dm").unwrap(), &args(&[])).valid);
        assert!(
            validate_command_args(command_by_id("dm").unwrap(), &args(&[("user", "user-123")]))
                .valid
        );
        assert!(
            validate_command_args(
                command_by_id("ban").unwrap(),
                &args(&[("user", "u"), ("duration", "1h"), ("reason", "spam")])
            )
            .valid
        );
        assert!(validate_command_args(&bot[2], &args(&[("item", "2")])).valid);
        assert!(
            validate_command_args(&bot[3], &args(&[("when", "1h"), ("text", "stretch")])).valid
        );
        assert!(
            validate_command_args(&bot[4], &args(&[("question", "Lunch?"), ("multi", "on")])).valid
        );
        assert!(
            !validate_command_args(
                &bot[4],
                &args(&[("question", "Lunch?"), ("multi", "maybe")])
            )
            .valid
        );
        assert!(command_by_id("modlog").unwrap().options.is_empty());
    }

    #[test]
    fn parses_free_text_named_and_quoted_unicode() {
        let all = all();
        assert_eq!(
            parse("/play jazz 東京", &all),
            Parsed::Invocation {
                command_id: "play".into(),
                args: args(&[("query-or-url", "jazz 東京")])
            }
        );
        assert!(matches!(
            parse("/play https://example.test/a:b", &all),
            Parsed::Invocation { .. }
        ));
        assert!(matches!(builtin("/play jazz"), Parsed::Unknown { .. }));
        assert_eq!(
            parse("/remind when:\"2026-10-26T09:00\" text:\"call Åsa\"", &all),
            Parsed::Invocation {
                command_id: "remind".into(),
                args: args(&[("when", "2026-10-26T09:00"), ("text", "call Åsa")])
            }
        );
        assert!(matches!(
            builtin("/warn user:user-1 reaso:spam"),
            Parsed::Invalid { .. }
        ));
        assert!(matches!(
            builtin("/warn user:user-1 reason:spam"),
            Parsed::Invocation { .. }
        ));
        assert!(matches!(
            builtin("/warn user:user-1 reason:\"see https://example.test/a:b at 09:30\""),
            Parsed::Invocation { .. }
        ));
        assert_eq!(
            builtin("/warn user-1 \"note: spam\""),
            Parsed::Invocation {
                command_id: "warn".into(),
                args: args(&[("user", "user-1"), ("reason", "note: spam")])
            }
        );
        assert_eq!(
            parse("/remind 1h \"call: Åsa\"", &all),
            Parsed::Invocation {
                command_id: "remind".into(),
                args: args(&[("when", "1h"), ("text", "call: Åsa")])
            }
        );
        assert_eq!(
            builtin("/me \"say \\\"hi\\\" and C:\\\\music\""),
            Parsed::Invocation {
                command_id: "me".into(),
                args: args(&[("text", "say \"hi\" and C:\\music")])
            }
        );
    }

    #[test]
    fn multiline_free_text_keeps_named_options_out() {
        assert_eq!(
            builtin("/search hello scope:all"),
            Parsed::Invocation {
                command_id: "search".into(),
                args: args(&[("query", "hello"), ("scope", "all")])
            }
        );
        assert_eq!(
            builtin("/code js first line\nsecond line"),
            Parsed::Invocation {
                command_id: "code".into(),
                args: args(&[("language", "js"), ("text", "first line\nsecond line")])
            }
        );
        assert_eq!(
            builtin("/search query:\"hello\nworld\" scope:all"),
            Parsed::Invocation {
                command_id: "search".into(),
                args: args(&[("query", "hello\nworld"), ("scope", "all")])
            }
        );
    }

    #[test]
    fn reminder_durations_and_iso_local_times() {
        let bot = bot_command_definitions(&catalog());
        let remind = &bot[3];
        for (when, ok) in [
            ("banana", false),
            ("30s", false),
            ("366d", false),
            ("1m", true),
            ("2026-10-26T09:30", true),
            ("2026-10-26T09:30+02:00", true),
            ("2026-02-30T09:30", false),
        ] {
            assert_eq!(
                validate_command_args(remind, &args(&[("when", when), ("text", "call")])).valid,
                ok,
                "{when}"
            );
        }
        assert!(matches!(
            parse("/remind when:banana text:call", &all()),
            Parsed::Incomplete { .. }
        ));
    }

    #[test]
    fn literals_unknowns_and_subcommands() {
        assert_eq!(builtin("hello"), Parsed::Literal("hello".into()));
        assert_eq!(builtin("//shrug"), Parsed::Literal("/shrug".into()));
        assert_eq!(
            builtin("/xyz"),
            Parsed::Unknown {
                command_text: "xyz".into()
            }
        );
        match builtin("/role") {
            Parsed::Incomplete { subcommands, .. } => assert_eq!(subcommands, ["add", "remove"]),
            other => panic!("{other:?}"),
        }
        assert!(matches!(builtin("/me \"open"), Parsed::Invalid { .. }));
    }

    #[test]
    fn availability_fails_closed() {
        let dm = CommandContext {
            in_community: false,
            bits: None,
        };
        let community = |bits| CommandContext {
            in_community: true,
            bits,
        };
        let kick = command_by_id("kick").unwrap();
        let pin = command_by_id("pin").unwrap();
        assert_eq!(
            command_availability(kick, &dm),
            Err(Unavailable::CommunityRequired)
        );
        assert_eq!(command_availability(pin, &dm), Ok(()));
        assert_eq!(
            command_availability(kick, &community(None)),
            Err(Unavailable::PermissionUnknown)
        );
        assert_eq!(
            command_availability(kick, &community(Some(0))),
            Err(Unavailable::PermissionDenied)
        );
        assert_eq!(
            command_availability(kick, &community(Some(1 << 12))),
            Ok(())
        );
    }

    #[test]
    fn ranking_prefers_exact_then_prefix_then_substring() {
        let ranked: Vec<&str> = rank_commands(commands(), "un", 25)
            .iter()
            .map(|c| c.id.as_str())
            .collect();
        assert_eq!(&ranked[..4], ["unflip", "unpin", "unmute", "undeafen"]);
        let all: Vec<&str> = rank_commands(commands(), "", 25)
            .iter()
            .map(|c| c.id.as_str())
            .collect();
        assert_eq!(all.len(), 25);
        assert_eq!(&all[..3], ["help", "me", "shrug"]);
    }

    #[test]
    fn classic_payloads() {
        assert_eq!(classic_message("shrug", None).unwrap(), "¯\\\\\\_(ツ)\\_/¯");
        assert_eq!(
            classic_message("tableflip", Some("now")).unwrap(),
            "(╯°□°)╯︵ ┻━┻ now"
        );
        assert_eq!(
            classic_message("unflip", Some("")).unwrap(),
            "┬─┬ ノ( ゜-゜ノ)"
        );
    }

    #[test]
    fn help_text_lists_usage_and_options() {
        assert_eq!(
            help_text("code").unwrap(),
            "/code <language> <text>\nSend a code block\n• language (text, required): language argument\n• text (text, required): text argument"
        );
        assert!(all_help().starts_with("/help [command]\nShow command help"));
    }
}
