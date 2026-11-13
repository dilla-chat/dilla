//! The palette's result groups (`packages/web/src/screens/palette.ts`): Channels, People, Settings, Messages and Actions, in
//! the order the prototype draws them. Each is a provider: a function of the query that answers one group or nothing; the
//! palette lists the groups with items, in order. Pure: it reads a state and answers rows, so the same inputs give the same
//! rows as `palette.test.ts`'s.

use std::collections::HashSet;

use dilla_client::state::search::SearchHit;
use dilla_client::state::{AppState, ChannelId, CommunityId};
use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::search::Glyph;

use super::Navigate;
use super::settings_index::{self, Access, Section};
use crate::screens::shell::Clock;
use crate::strings::{t, tf};

/// A result and what choosing it does.
#[derive(Clone, Debug)]
pub struct Item {
    /// Unique across the groups: `ch:<id>`, `dm:<id>`, `set:<scope>|<section>|<anchor>`, `msg:<id>`, `act:<id>`.
    pub id: String,
    pub label: String,
    pub sub: Option<String>,
    pub glyph: Glyph,
    pub target: Navigate,
}

/// A group of results under its heading.
#[derive(Clone, Debug)]
pub struct Group {
    #[allow(dead_code, reason = "the group's name: tests and tools read it")]
    pub id: &'static str,
    pub label: String,
    pub items: Vec<Item>,
}

/// A direct message as the People group lists it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Person {
    pub channel: ChannelId,
    pub name: String,
    /// The line under the name: `@username · status` for a 1:1, `group DM · n people` for a group.
    pub line: String,
}

/// Everything a query is answered from.
pub struct Inputs<'a> {
    pub state: &'a AppState,
    /// The server on screen (its settings are searched too).
    pub community: Option<CommunityId>,
    /// The conversation on screen: its message hits come first.
    pub current: Option<ChannelId>,
    pub clock: Clock,
    /// The newest message search's hits, for the query being answered (empty while it runs).
    pub hits: &'a [SearchHit],
    pub people: &'a [Person],
    /// Channels that are a server's AFK destination: never listed.
    pub afk: &'a HashSet<ChannelId>,
    /// This device is muted in a call (the action reads "Unmute").
    pub muted: bool,
}

/// `paletteGroups`: the groups of the providers for `query` (trimmed, lower case), the empty ones left out.
pub fn groups(inputs: &Inputs<'_>, query: &str) -> Vec<Group> {
    let q = query.trim().to_lowercase();
    [
        channels(inputs, &q),
        people(inputs, &q),
        settings(inputs, &q),
        messages(inputs, &q),
        actions(inputs, &q),
    ]
    .into_iter()
    .flatten()
    .filter(|g| !g.items.is_empty())
    .collect()
}

/// Channels: every text and voice channel of every server (not categories, not the AFK channel) whose name, or whose
/// server's name, holds the query; "{server}" or "{server} · voice" under it. At most 10; all of them while nothing is
/// typed, as the prototype.
fn channels(inputs: &Inputs<'_>, q: &str) -> Option<Group> {
    let mut items = Vec::new();
    // The server's own order: by position, equal positions by id (the channel list's rule).
    let mut ordered: Vec<_> = inputs.state.channels.iter().collect();
    ordered.sort_by_key(|c| (c.position, c.id));
    for c in ordered {
        if c.kind == 2 || inputs.afk.contains(&c.id) || items.len() >= 10 {
            continue;
        }
        let server = inputs
            .state
            .communities
            .iter()
            .find(|s| s.id == c.community_id)
            .map_or("", |s| s.name.as_str());
        if !q.is_empty() && !c.name.to_lowercase().contains(q) && !server.to_lowercase().contains(q)
        {
            continue;
        }
        let voice = c.kind == 1;
        items.push(Item {
            id: format!("ch:{}", c.id),
            label: c.name.clone(),
            sub: Some(if voice {
                tf("search.palette.voiceSub", &[("server", server)])
            } else {
                server.to_owned()
            }),
            glyph: Glyph::Icon(if voice {
                IconName::Volume2
            } else {
                IconName::Hash
            }),
            target: Navigate::Channel {
                community: c.community_id,
                channel: c.id,
            },
        });
    }
    Some(Group {
        id: "channels",
        label: t("search.group.channels").to_owned(),
        items,
    })
}

/// People: the direct messages, by name (at most 6).
fn people(inputs: &Inputs<'_>, q: &str) -> Option<Group> {
    Some(Group {
        id: "people",
        label: t("search.group.people").to_owned(),
        items: inputs
            .people
            .iter()
            .filter(|p| q.is_empty() || p.name.to_lowercase().contains(q))
            .take(6)
            .map(|p| Item {
                id: format!("dm:{}", p.channel),
                label: p.name.clone(),
                sub: Some(p.line.clone()).filter(|l| !l.is_empty()),
                glyph: Glyph::Avatar {
                    name: p.name.clone().into(),
                    bot: false,
                },
                target: Navigate::Dm(p.channel),
            })
            .collect(),
    })
}

/// Settings: up to 8 sections or rows whose words hold the query, "{group} / {section}" under each; nothing while the
/// query is empty. The viewer's own sections first, then the selected server's allowed ones.
fn settings(inputs: &Inputs<'_>, q: &str) -> Option<Group> {
    if q.is_empty() {
        return None;
    }
    let mut items: Vec<Item> = settings_index::search(false, q, &Access::default())
        .into_iter()
        .map(|f| settings_item("user", &f, f.breadcrumb.clone(), inputs.community))
        .collect();
    if let Some(community) = inputs.community {
        let access = Access::of(inputs.state, community);
        if access.loaded {
            let name = inputs
                .state
                .communities
                .iter()
                .find(|c| c.id == community)
                .map(|c| c.name.clone());
            items.extend(
                settings_index::search(true, q, &access)
                    .into_iter()
                    .map(|f| {
                        let section = f.breadcrumb.rsplit(" / ").next().unwrap_or_default();
                        let crumb = format!(
                            "{} / {section}",
                            name.clone().unwrap_or_else(|| {
                                f.breadcrumb
                                    .split(" / ")
                                    .next()
                                    .unwrap_or_default()
                                    .to_owned()
                            })
                        );
                        settings_item("server", &f, crumb, Some(community))
                    }),
            );
        }
    }
    items.truncate(8);
    Some(Group {
        id: "settings",
        label: t("search.group.settings").to_owned(),
        items,
    })
}

fn settings_item(
    scope: &str,
    f: &settings_index::Found,
    breadcrumb: String,
    community: Option<CommunityId>,
) -> Item {
    let (section, target) = match f.section {
        Section::User(s) => (s.id(), Navigate::Settings(s)),
        Section::Server(s) => (
            s.id(),
            Navigate::ServerSettings {
                community: community.unwrap_or(CommunityId([0; 16])),
                section: s,
            },
        ),
    };
    Item {
        id: format!("set:{scope}|{section}|{}", f.anchor.unwrap_or("")),
        label: f.label.clone(),
        sub: Some(breadcrumb),
        glyph: Glyph::Icon(IconName::Settings),
        target,
    }
}

/// `messageTime`: a row's local time, with the day in front when it is not today's date.
pub fn message_time(ts: u64, clock: Clock) -> String {
    let time = clock.format(ts, |d| {
        use chrono::Timelike;
        format!("{:02}:{:02}", d.hour(), d.minute())
    });
    if clock.day(ts) == clock.day(clock.now()) {
        return time;
    }
    let day = clock.format(ts, |d| {
        let offset = *d.offset();
        crate::format::day_in(ts, &|_| offset, &crate::format::date_locale())
    });
    format!("{day} {time}")
}

/// Messages: the hits for two or more characters, the open conversation's first (the scope is implicit: here, then
/// everywhere). Each reads "#place · author · time" under the snippet.
fn messages(inputs: &Inputs<'_>, q: &str) -> Option<Group> {
    if q.encode_utf16().count() < 2 {
        return None;
    }
    let current = |h: &&SearchHit| Some(h.channel) == inputs.current;
    let ordered = inputs
        .hits
        .iter()
        .filter(current)
        .chain(inputs.hits.iter().filter(|h| !current(h)));
    Some(Group {
        id: "messages",
        label: t("search.group").to_owned(),
        items: ordered
            .take(50)
            .map(|h| {
                let place = if h.community.is_none() {
                    inputs
                        .people
                        .iter()
                        .find(|p| p.channel == h.channel)
                        .map_or_else(|| h.channel.hex(), |p| p.name.clone())
                } else {
                    let name = inputs
                        .state
                        .channel(h.channel)
                        .map_or_else(|| h.channel.hex(), |c| c.name.clone());
                    format!("#{name}")
                };
                let sub: Vec<String> = [
                    Some(place),
                    h.author.clone(),
                    h.ts.map(|ts| message_time(ts, inputs.clock)),
                ]
                .into_iter()
                .flatten()
                .filter(|s| !s.is_empty())
                .collect();
                Item {
                    id: format!("msg:{}", h.id()),
                    label: h.snippet.split_whitespace().collect::<Vec<_>>().join(" "),
                    sub: Some(sub.join(" · ")),
                    glyph: Glyph::Icon(IconName::MessageSquare),
                    target: Navigate::Message(h.clone()),
                }
            })
            .collect(),
    })
}

/// Actions: things the app really does from here (open settings, mute in a call, message someone), found by their words.
fn actions(inputs: &Inputs<'_>, q: &str) -> Option<Group> {
    let shortcut = if cfg!(target_os = "macos") {
        "⌘⇧M"
    } else {
        "Ctrl+Shift+M"
    };
    let all = [
        (
            "settings",
            t("search.action.settings").to_owned(),
            Some(t("search.action.settingsSub").to_owned()),
            IconName::Settings,
            "profile devices voice",
            Navigate::OpenSettings,
        ),
        (
            "mute",
            t(if inputs.muted {
                "search.action.unmute"
            } else {
                "search.action.mute"
            })
            .to_owned(),
            Some(shortcut.to_owned()),
            if inputs.muted {
                IconName::Mic
            } else {
                IconName::MicOff
            },
            "microphone",
            Navigate::ToggleMute,
        ),
        (
            "dm",
            t("search.action.dm").to_owned(),
            Some(t("search.action.dmSub").to_owned()),
            IconName::MessageSquare,
            "direct message dm",
            Navigate::StartDm,
        ),
    ];
    Some(Group {
        id: "actions",
        label: t("search.group.actions").to_owned(),
        items: all
            .into_iter()
            .filter(|(_, label, _, _, keywords, _)| {
                q.is_empty() || format!("{label} {keywords}").to_lowercase().contains(q)
            })
            .map(|(id, label, sub, icon, _, target)| Item {
                id: format!("act:{id}"),
                label,
                sub,
                glyph: Glyph::Icon(icon),
                target,
            })
            .collect(),
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::{Channel, ChannelGroupState, Community, UserId};

    fn ch(id: u8, community: u8, name: &str, kind: u8) -> Channel {
        Channel {
            id: ChannelId([id; 16]),
            community_id: CommunityId([community; 16]),
            kind,
            mode: 0,
            name: name.into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Active,
        }
    }

    fn community(id: u8, name: &str) -> Community {
        Community {
            id: CommunityId([id; 16]),
            name: name.into(),
            owner: UserId([9; 16]),
            policy_version: 0,
        }
    }

    fn world() -> AppState {
        AppState {
            communities: vec![
                community(1, "Midgard Crew"),
                community(2, "Valhalla Kitchen"),
            ],
            channels: vec![
                ch(1, 1, "general", 0),
                ch(2, 1, "harbour", 1),
                ch(3, 1, "raids", 2),
                ch(4, 1, "AFK", 1),
                ch(5, 2, "recipes", 0),
            ],
            ..AppState::default()
        }
    }

    fn person(id: u8, name: &str, line: &str) -> Person {
        Person {
            channel: ChannelId([id; 16]),
            name: name.into(),
            line: line.into(),
        }
    }

    fn hit(id: u8, seq: u64, snippet: &str, author: Option<&str>) -> SearchHit {
        SearchHit {
            channel: ChannelId([id; 16]),
            community: Some(CommunityId([1; 16])),
            seq,
            snippet: snippet.into(),
            author: author.map(str::to_owned),
            ts: None,
            thread_root: None,
        }
    }

    struct Fixture {
        state: AppState,
        people: Vec<Person>,
        afk: HashSet<ChannelId>,
        hits: Vec<SearchHit>,
    }

    impl Fixture {
        fn new() -> Self {
            Self {
                state: world(),
                people: vec![
                    person(10, "Ada", "@ada"),
                    person(11, "raid crew", "group DM"),
                ],
                afk: HashSet::from([ChannelId([4; 16])]),
                hits: vec![
                    hit(5, 1, "elsewhere rope", None),
                    hit(1, 2, "here rope", Some("Ada")),
                ],
            }
        }

        fn groups(&self, query: &str) -> Vec<Group> {
            let inputs = Inputs {
                state: &self.state,
                community: None,
                current: Some(ChannelId([1; 16])),
                clock: Clock::Fixed { now: 0, utc: true },
                hits: &self.hits,
                people: &self.people,
                afk: &self.afk,
                muted: false,
            };
            groups(&inputs, query)
        }
    }

    fn ids(gs: &[Group]) -> Vec<&str> {
        gs.iter().map(|g| g.id).collect()
    }

    /// palette.test.ts: "lists channels (no categories, no AFK), people and actions with nothing typed, in the prototype
    /// order, and no messages".
    #[test]
    fn nothing_typed_lists_channels_people_and_actions() {
        let f = Fixture::new();
        let gs = f.groups("");
        assert_eq!(ids(&gs), ["channels", "people", "actions"]);
        let rows: Vec<_> = gs[0]
            .items
            .iter()
            .map(|i| (i.label.as_str(), i.sub.as_deref().unwrap()))
            .collect();
        assert_eq!(
            rows,
            [
                ("general", "Midgard Crew"),
                ("harbour", "Midgard Crew · voice"),
                ("recipes", "Valhalla Kitchen"),
            ]
        );
        assert_eq!(gs[1].items[1].sub.as_deref(), Some("group DM"));
        assert!(matches!(
            gs[0].items[1].glyph,
            Glyph::Icon(IconName::Volume2)
        ));
    }

    /// palette.test.ts: "filters by name, server and keywords, and puts the open conversation's messages first".
    #[test]
    fn a_query_filters_and_puts_the_open_conversations_messages_first() {
        let f = Fixture::new();
        let gs = f.groups("Rope");
        assert_eq!(ids(&gs), ["messages"]);
        let rows: Vec<_> = gs[0]
            .items
            .iter()
            .map(|i| (i.label.as_str(), i.sub.as_deref().unwrap()))
            .collect();
        assert_eq!(
            rows,
            [
                ("here rope", "#general · Ada"),
                ("elsewhere rope", "#recipes")
            ]
        );
        let valhalla = f.groups("valhalla");
        assert_eq!(valhalla[0].items.len(), 1);
        assert_eq!(valhalla[0].items[0].label, "recipes");
        let ids_of_profile: Vec<_> = f
            .groups("profile")
            .iter()
            .map(|g| g.id)
            .filter(|id| *id != "messages")
            .collect();
        assert_eq!(ids_of_profile, ["settings", "actions"]);
    }

    /// palette.test.ts: "a channel result runs its opener": the target is the channel and its server.
    #[test]
    fn a_channel_result_opens_that_channel() {
        let f = Fixture::new();
        let item = &f.groups("general")[0].items[0];
        assert!(matches!(
            item.target,
            Navigate::Channel { channel, community }
                if channel == ChannelId([1; 16]) && community == CommunityId([1; 16])
        ));
    }

    /// palette.test.ts "settings provider": nothing while nothing is typed; at most 8, "{group} / {section}" under each.
    #[test]
    fn settings_say_nothing_when_empty_and_list_at_most_eight() {
        let f = Fixture::new();
        assert!(!ids(&f.groups("")).contains(&"settings"));
        let gs = f.groups("a");
        let settings = gs.iter().find(|g| g.id == "settings").unwrap();
        assert_eq!(settings.label, "Settings");
        assert!(settings.items.len() <= 8);
        assert!(settings.items.iter().all(|i| i.id.starts_with("set:user|")));
        assert!(
            settings.items[0]
                .sub
                .as_deref()
                .unwrap()
                .starts_with("You / ")
        );
        // Messages need two characters.
        assert!(!ids(&f.groups("r")).contains(&"messages"));
    }

    #[test]
    fn a_message_row_reads_place_author_time_and_a_dm_hit_names_its_person() {
        let mut f = Fixture::new();
        f.hits = vec![SearchHit {
            channel: ChannelId([10; 16]),
            community: None,
            seq: 3,
            snippet: "see you\n  at   the bridge".into(),
            author: Some("Mira".into()),
            ts: Some(1_794_481_738),
            thread_root: None,
        }];
        let gs = f.groups("bridge");
        let row = &gs.iter().find(|g| g.id == "messages").unwrap().items[0];
        assert_eq!(row.label, "see you at the bridge");
        // The clock's "now" is 0, so the message is on another day: the day comes first.
        assert!(row.sub.as_deref().unwrap().starts_with("Ada · Mira · "));
        assert!(row.sub.as_deref().unwrap().ends_with(" 11:08"));
    }

    #[test]
    fn today_shows_the_time_alone() {
        let clock = Clock::Fixed {
            now: 1_794_481_800,
            utc: true,
        };
        assert_eq!(message_time(1_794_481_738, clock), "11:08");
        let other = Clock::Fixed {
            now: 1_794_481_800 + 86_400 * 3,
            utc: true,
        };
        assert!(message_time(1_794_481_738, other).ends_with(" 11:08"));
    }

    #[test]
    fn the_mute_action_follows_the_call() {
        let f = Fixture::new();
        let inputs = Inputs {
            state: &f.state,
            community: None,
            current: None,
            clock: Clock::Local,
            hits: &[],
            people: &[],
            afk: &f.afk,
            muted: true,
        };
        let gs = groups(&inputs, "microphone");
        let act = gs.iter().find(|g| g.id == "actions").unwrap();
        assert_eq!(act.items[0].label, "Unmute");
    }
}
