//! The command picker's model (packages/web/src/screens/commands/{CommandPicker,picker-catalog,run-bot}.ts): which
//! commands a `/` draft lists, each row's icon, argument line, description and source tag, why a Bot command is greyed,
//! and what a key does. Pure: the view ([`super::picker`]) draws it and [`super::keys`] feeds it the keys.

use dilla_client::features::bot::registry::{
    self, ArgKind, CommandContext, CommandDefinition, CommandOption, Parsed, Provider,
};
use dilla_client::state::bot::{BotCatalogView, BotStatusState};
use dilla_client::state::{AppState, CommunityId};
use dilla_desktop_ui::IconName;

use super::copy;
use crate::strings::{t, tf};

/// One field of a picked command (`PickerField`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Field {
    pub name: String,
    pub label: String,
    pub placeholder: String,
    pub optional: bool,
}

/// A command as the picker shows it (`PickerEntry`).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Entry {
    pub icon: IconName,
    pub description: String,
    /// `<required> [optional]`, or "opens a form".
    pub args: String,
    /// "built-in", or the Bot's name.
    pub source: String,
    pub opens_form: bool,
    pub fields: Vec<Field>,
}

/// A field override: its name, its label (else the option's) and its placeholder.
type FieldOverride = (&'static str, Option<&'static str>, &'static str);
/// An override: icon, description, whether it opens a form, and its fields.
type Override = (IconName, &'static str, bool, &'static [FieldOverride]);

/// The prototype's CMDS overrides: icon, description, form, and per field (label, placeholder).
fn prototype(id: &str) -> Option<Override> {
    use IconName as I;
    Some(match id {
        "help" => (
            I::CircleHelp,
            "List commands, or explain one",
            false,
            &[("command", None, "optional")],
        ),
        "me" => (
            I::Sparkles,
            "Post an action in italics",
            false,
            &[("text", None, "packs the rope")],
        ),
        "shrug" => (
            I::Smile,
            "Appends ¯\\_(ツ)_/¯ to your message",
            false,
            &[("text", None, "optional")],
        ),
        "play" => (
            I::Play,
            "Play the first YouTube result, or a link, in your voice channel",
            false,
            &[("query-or-url", None, "song, video or playlist")],
        ),
        "pause" => (I::Pause, "Pause playback", false, &[]),
        "resume" => (I::Play, "Resume playback", false, &[]),
        "skip" => (I::SkipForward, "Skip to the next track", false, &[]),
        "stop" => (I::Square, "Stop and clear the queue", false, &[]),
        "queue" => (I::ListMusic, "Show what plays next", false, &[]),
        "volume" => (
            I::Volume2,
            "Set playback volume",
            false,
            &[("level", Some("0..100"), "60")],
        ),
        "poll create" => (I::Vote, "Ask the channel a question", true, &[]),
        "remind" => (
            I::AlarmClock,
            "A private one-time reminder",
            false,
            &[
                ("when", None, "in 1 hour"),
                ("text", None, "bring two ropes"),
            ],
        ),
        "reminders list" => (I::Bell, "Open your reminders inbox", false, &[]),
        "kick" => (
            I::UserX,
            "Remove someone from the server",
            false,
            &[("user", None, "")],
        ),
        "ban" => (
            I::Ban,
            "Ban for a while, or for good",
            false,
            &[
                ("user", None, ""),
                ("duration", None, "permanent"),
                ("reason", None, "optional"),
            ],
        ),
        "timeout" => (
            I::Timer,
            "Stop someone sending or speaking for a while",
            false,
            &[("user", None, ""), ("duration", None, "10 minutes")],
        ),
        "warn" => (
            I::TriangleAlert,
            "A private warning, logged in modlog",
            false,
            &[("user", None, ""), ("reason", None, "what happened")],
        ),
        _ => return None,
    })
}

/// The plan's Q2 icons for the commands the prototype does not list.
fn icon_for(id: &str) -> IconName {
    use IconName as I;
    match id {
        "tableflip" | "unflip" => I::Table,
        "code" => I::Code,
        "giphy" => I::Image,
        "dm" => I::MessageSquare,
        "invite" => I::UserPlus,
        "search" => I::Search,
        "topic" => I::Hash,
        "thread" => I::MessagesSquare,
        "reply" => I::Reply,
        "edit" => I::Pencil,
        "delete" => I::Trash2,
        "pin" | "unpin" => I::Pin,
        "join" => I::Volume2,
        "leave" => I::PhoneOff,
        "mute" => I::MicOff,
        "unmute" => I::Mic,
        "deafen" => I::HeadphoneOff,
        "undeafen" => I::Headphones,
        "nowplaying" => I::Music,
        "seek" => I::FastForward,
        "shuffle" => I::Shuffle,
        "repeat" => I::Repeat,
        "untimeout" => I::TimerOff,
        "unban" => I::Ban,
        "modlog" => I::ScrollText,
        _ if id.starts_with("queue ") || id == "music" || id.starts_with("music ") => I::ListMusic,
        _ if id.starts_with("role ") || id.starts_with("selfrole ") || id == "rolepanel create" => {
            I::Shield
        }
        _ if id.starts_with("announcement ") => I::Megaphone,
        _ if id.starts_with("poll ") => I::Vote,
        _ if id.starts_with("reminders ") => I::Bell,
        _ => I::Hash,
    }
}

fn label_for(option: &CommandOption, over: Option<&str>) -> String {
    if let Some(label) = over {
        return label.to_owned();
    }
    match (option.kind, option.min, option.max) {
        (ArgKind::Number, Some(min), Some(max)) => format!("{min}..{max}"),
        _ => option.name.clone(),
    }
}

/// `pickerEntry`.
pub fn entry(command: &CommandDefinition, bot_name: &str) -> Entry {
    let over = prototype(&command.id);
    let fields: Vec<Field> = command
        .options
        .iter()
        .map(|o| {
            let field = over.and_then(|(_, _, _, f)| f.iter().find(|(n, _, _)| *n == o.name));
            Field {
                name: o.name.clone(),
                label: label_for(o, field.and_then(|(_, l, _)| *l)),
                placeholder: field.map_or_else(
                    || {
                        if o.required {
                            String::new()
                        } else {
                            copy::OPTIONAL_PLACEHOLDER.to_owned()
                        }
                    },
                    |(_, _, p)| (*p).to_owned(),
                ),
                optional: !o.required,
            }
        })
        .collect();
    let opens_form = over.is_some_and(|(_, _, form, _)| form);
    let args = if opens_form {
        copy::OPENS_A_FORM.to_owned()
    } else {
        fields
            .iter()
            .map(|f| {
                if f.optional {
                    format!("[{}]", f.label)
                } else {
                    format!("<{}>", f.label)
                }
            })
            .collect::<Vec<_>>()
            .join(" ")
    };
    Entry {
        icon: over.map_or_else(|| icon_for(&command.id), |(i, _, _, _)| i),
        description: over.map_or_else(|| command.description.clone(), |(_, d, _, _)| d.to_owned()),
        args,
        source: if command.provider == Provider::Builtin {
            copy::BUILT_IN_SOURCE.to_owned()
        } else {
            bot_name.to_owned()
        },
        opens_form,
        fields,
    }
}

/// Whether the community's GIF search is on (Task 12's setting); until that area publishes it, `/giphy` stays unlisted,
/// as the web lists it only with GIFs enabled.
fn gifs_enabled(_state: &AppState, _community: Option<CommunityId>) -> bool {
    false
}

/// The commands the picker offers in `community` (the host's `definitions`): the available built-ins, then the Bot's.
pub fn definitions(state: &AppState, community: Option<CommunityId>) -> Vec<CommandDefinition> {
    let context = CommandContext {
        in_community: community.is_some(),
        bits: None,
    };
    let mut out: Vec<CommandDefinition> = registry::commands()
        .iter()
        .filter(|c| {
            (c.id != "giphy" || gifs_enabled(state, community))
                && registry::command_availability(c, &context).is_ok()
        })
        .cloned()
        .collect();
    if let Some(view) = community.and_then(|c| state.bot.catalogs.get(&c)) {
        out.extend(registry::bot_command_definitions(&view.commands));
    }
    out
}

/// `botSince`: the 24-hour local time of a Bot status's since, or "—".
fn since(since: Option<u64>, zone: crate::format::Zone) -> String {
    since.map_or_else(|| "—".to_owned(), |s| crate::format::time_in(s, zone))
}

/// `botCommandReason`: why a Bot command is greyed for this member, or `None` when it can run.
pub fn greyed(
    view: Option<&BotCatalogView>,
    command: &CommandDefinition,
    zone: crate::format::Zone,
) -> Option<String> {
    let (view, bot) = (view?, command.bot.as_ref()?);
    let time = since(view.status.since, zone);
    let reason = match view.status.state {
        BotStatusState::Degraded => tf("commands.botDegraded", &[("time", &time)]),
        BotStatusState::Offline => tf("commands.botOffline", &[("time", &time)]),
        BotStatusState::Disabled => t("commands.botDisabled").to_owned(),
        BotStatusState::Absent => t("commands.botNotOnInstance").to_owned(),
        BotStatusState::Unverified => t("commands.botUnverified").to_owned(),
        BotStatusState::Changed => t("commands.botChanged").to_owned(),
        BotStatusState::Ready => {
            if bot.needs_bot_channel && view.bot_channels.as_ref().is_none_or(Vec::is_empty) {
                t("commands.needsBotChannel").to_owned()
            } else {
                return None;
            }
        }
    };
    Some(reason)
}

/// One row of the open list.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Row {
    pub id: String,
    pub entry: Entry,
    /// Greyed: its description says why and it does not run.
    pub reason: Option<String>,
}

/// `SLASH_LIST`: a draft that is only `/` and letters and spaces lists commands.
fn slash_list(draft: &str) -> bool {
    draft
        .strip_prefix('/')
        .is_some_and(|r| r.chars().all(|c| c.is_ascii_alphabetic() || c == ' '))
}

/// The list a draft opens: `None` when closed (no `/`, `//`, or a typed command with arguments), else its rows (empty:
/// "No command matches"). Rows whose id starts with the query come first, then the ones that contain it.
pub fn list(
    state: &AppState,
    community: Option<CommunityId>,
    draft: &str,
    zone: crate::format::Zone,
) -> Option<Vec<Row>> {
    if !draft.starts_with('/') || draft.starts_with("//") {
        return None;
    }
    let defs = definitions(state, community);
    let mut parse_set: Vec<&CommandDefinition> = defs.iter().collect();
    parse_set.extend(
        registry::commands()
            .iter()
            .filter(|c| !defs.iter().any(|d| d.id == c.id)),
    );
    let typed = match registry::parse_command(draft, &parse_set) {
        Parsed::Invocation { command_id, args } => Some((command_id, args.len())),
        Parsed::Incomplete {
            command_id,
            validation,
            ..
        } => Some((command_id, validation.args.len())),
        _ => None,
    };
    if typed.is_some_and(|(id, n)| n > 0 && defs.iter().any(|d| d.id == id)) {
        return None;
    }
    if !slash_list(draft) {
        return Some(Vec::new());
    }
    let query = draft[1..]
        .to_lowercase()
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ");
    let view = community.and_then(|c| state.bot.catalogs.get(&c));
    let bot_name = view.map_or("", |v| v.bot_name.as_str());
    let starts = defs.iter().filter(|c| c.id.starts_with(&query));
    let contains = defs
        .iter()
        .filter(|c| !c.id.starts_with(&query) && c.id.contains(&query));
    Some(
        starts
            .chain(contains)
            .map(|c| Row {
                id: c.id.clone(),
                entry: entry(c, bot_name),
                reason: greyed(view, c, zone),
            })
            .collect(),
    )
}

/// A key the open list reacts to.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Key {
    Up,
    Down,
    Tab,
    Enter,
    Escape,
}

/// What a key does to the open list.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Effect {
    /// The selection moved to this row.
    Select(usize),
    /// Enter this row's command (its chips, or its form).
    Pick(usize),
    /// Close the list and keep the draft (Escape; the prototype clears it only while the list showed commands).
    Close { clear: bool },
    /// Not the list's key: the composer handles it.
    Pass,
}

/// `draftKeyDown` while the list is open with `rows` rows and `selected` highlighted.
pub fn key(rows: usize, selected: usize, key: Key) -> Effect {
    let selected = selected.min(rows.saturating_sub(1));
    match key {
        Key::Down if rows > 0 => Effect::Select((selected + 1) % rows),
        Key::Up if rows > 0 => Effect::Select((selected + rows - 1) % rows),
        Key::Tab | Key::Enter if rows > 0 => Effect::Pick(selected),
        Key::Escape => Effect::Close { clear: rows > 0 },
        _ => Effect::Pass,
    }
}

#[cfg(test)]
mod tests {
    //! Ported from picker-catalog.test.ts and the list cases of CommandPicker.test.tsx.
    use super::*;
    use dilla_client::state::bot::{ArgShape, ArgType, BotCommandSpec, BotStatus};
    use std::collections::BTreeMap;

    fn utc(_: i64) -> chrono::FixedOffset {
        chrono::FixedOffset::east_opt(0).unwrap()
    }

    fn state(
        status: BotStatusState,
        channels: Option<Vec<dilla_client::state::ChannelId>>,
    ) -> (AppState, CommunityId) {
        let c = CommunityId([1; 16]);
        let mut s = AppState::default();
        let spec =
            |name: &str, op: u32, args: &[(&str, ArgType, bool)], needs: bool| BotCommandSpec {
                name: name.into(),
                description: format!("{name} it"),
                operation: op,
                args: args
                    .iter()
                    .map(|&(n, ty, required)| ArgShape {
                        name: n.into(),
                        ty,
                        required,
                    })
                    .collect(),
                permission_bit: 0,
                needs_bot_channel: needs,
                handler_kind: 0,
            };
        let mut catalogs = BTreeMap::new();
        catalogs.insert(
            c,
            BotCatalogView {
                revision: 1,
                commands: vec![
                    spec(
                        "remind",
                        80,
                        &[
                            ("when", ArgType::Duration, true),
                            ("text", ArgType::Text, true),
                        ],
                        false,
                    ),
                    spec(
                        "poll create",
                        70,
                        &[("question", ArgType::Text, true)],
                        true,
                    ),
                ],
                status: BotStatus {
                    state: status,
                    since: Some(1_700_000_000),
                },
                bot_channels: channels,
                bot_user: None,
                bot_name: "Dilla Bot".into(),
            },
        );
        s.bot.catalogs = catalogs;
        (s, c)
    }

    #[test]
    fn the_slash_lists_built_ins_first_in_registry_order() {
        let (s, c) = state(BotStatusState::Ready, Some(vec![]));
        let rows = list(&s, Some(c), "/", &utc).unwrap();
        let ids: Vec<&str> = rows.iter().map(|r| r.id.as_str()).collect();
        assert_eq!(
            ids,
            [
                "help",
                "me",
                "shrug",
                "tableflip",
                "unflip",
                "code",
                "dm",
                "search",
                "thread",
                "reply",
                "edit",
                "delete",
                "join",
                "leave",
                "mute",
                "unmute",
                "deafen",
                "undeafen",
                "selfrole list",
                "selfrole add",
                "selfrole remove",
                "remind",
                "poll create"
            ]
        );
        let rows: Vec<Row> = rows
            .into_iter()
            .filter(|r| ["help", "code", "remind", "poll create"].contains(&r.id.as_str()))
            .collect();
        assert_eq!(rows[0].entry.args, "[command]");
        assert_eq!(rows[0].entry.description, "List commands, or explain one");
        assert_eq!(rows[0].entry.source, "built-in");
        assert_eq!(rows[1].entry.args, "<language> <text>");
        assert_eq!(rows[2].entry.source, "Dilla Bot");
        assert_eq!(rows[2].entry.args, "<when> <text>");
        assert_eq!(rows[3].entry.args, "opens a form");
    }

    #[test]
    fn a_query_lists_prefix_matches_then_substrings() {
        let (s, c) = state(BotStatusState::Ready, Some(vec![]));
        let ids: Vec<String> = list(&s, Some(c), "/re", &utc)
            .unwrap()
            .into_iter()
            .map(|r| r.id)
            .collect();
        assert_eq!(
            ids,
            [
                "reply",
                "remind",
                "thread",
                "selfrole remove",
                "poll create"
            ]
        );
        let ids: Vec<String> = list(&s, Some(c), "/flip", &utc)
            .unwrap()
            .into_iter()
            .map(|r| r.id)
            .collect();
        assert_eq!(ids, ["tableflip", "unflip"]);
        assert_eq!(list(&s, Some(c), "/zzz", &utc).unwrap(), vec![]);
    }

    #[test]
    fn the_list_closes_on_text_double_slash_and_a_typed_command_with_arguments() {
        let (s, c) = state(BotStatusState::Ready, None);
        assert!(list(&s, Some(c), "hello", &utc).is_none());
        assert!(list(&s, Some(c), "//shrug", &utc).is_none());
        assert!(list(&s, Some(c), "/me waves", &utc).is_none());
        assert_eq!(list(&s, Some(c), "/me 1", &utc), None);
        assert_eq!(list(&s, Some(c), "/x1", &utc), Some(vec![]));
    }

    #[test]
    fn bot_commands_are_greyed_with_the_reason_and_stay_listed() {
        let (s, c) = state(BotStatusState::Offline, None);
        let rows = list(&s, Some(c), "/", &utc).unwrap();
        let remind = rows.iter().find(|r| r.id == "remind").unwrap();
        assert_eq!(
            remind.reason.as_deref(),
            Some("The Bot is offline since 22:13")
        );
        assert!(
            rows.iter()
                .find(|r| r.id == "help")
                .unwrap()
                .reason
                .is_none()
        );
        let (s, c) = state(BotStatusState::Ready, Some(vec![]));
        let rows = list(&s, Some(c), "/poll", &utc).unwrap();
        assert_eq!(
            rows[0].reason.as_deref(),
            Some("Mark a channel as a Bot channel first")
        );
        let (s, c) = state(
            BotStatusState::Ready,
            Some(vec![dilla_client::state::ChannelId([2; 16])]),
        );
        assert!(
            list(&s, Some(c), "/poll", &utc).unwrap()[0]
                .reason
                .is_none()
        );
    }

    #[test]
    fn a_dm_lists_no_community_commands() {
        let (s, _) = state(BotStatusState::Ready, None);
        let ids: Vec<String> = list(&s, None, "/", &utc)
            .unwrap()
            .into_iter()
            .map(|r| r.id)
            .collect();
        assert_eq!(
            ids,
            [
                "help",
                "me",
                "shrug",
                "tableflip",
                "unflip",
                "code",
                "dm",
                "search",
                "reply",
                "edit",
                "delete",
                "pin",
                "unpin",
                "leave",
                "mute",
                "unmute",
                "deafen",
                "undeafen"
            ]
        );
    }

    #[test]
    fn keys_move_wrap_pick_and_close() {
        assert_eq!(key(3, 0, Key::Down), Effect::Select(1));
        assert_eq!(key(3, 2, Key::Down), Effect::Select(0));
        assert_eq!(key(3, 0, Key::Up), Effect::Select(2));
        assert_eq!(key(3, 1, Key::Tab), Effect::Pick(1));
        assert_eq!(key(3, 9, Key::Enter), Effect::Pick(2));
        assert_eq!(key(3, 1, Key::Escape), Effect::Close { clear: true });
        assert_eq!(key(0, 0, Key::Escape), Effect::Close { clear: false });
        assert_eq!(key(0, 0, Key::Enter), Effect::Pass);
    }

    #[test]
    fn entries_follow_the_prototype() {
        let volume = CommandDefinition {
            id: "volume".into(),
            description: "x".into(),
            community_only: true,
            provider: Provider::Bot,
            permission_bit: None,
            options: vec![CommandOption {
                name: "level".into(),
                kind: ArgKind::Number,
                description: String::new(),
                required: true,
                free_text: false,
                choices: None,
                min: Some(0),
                max: None,
            }],
            bot: None,
        };
        let e = entry(&volume, "Glenn");
        assert_eq!(
            (e.icon, e.args.as_str(), e.source.as_str()),
            (IconName::Volume2, "<0..100>", "Glenn")
        );
        assert_eq!(e.fields[0].placeholder, "60");
        assert_eq!(icon_for("queue remove"), IconName::ListMusic);
        assert_eq!(icon_for("selfrole add"), IconName::Shield);
    }
}
