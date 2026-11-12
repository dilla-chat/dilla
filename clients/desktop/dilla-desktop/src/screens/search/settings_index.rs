//! The settings the palette finds (`packages/web/src/settings/registry.ts`: `SETTINGS_REGISTRY` and `searchSettings`):
//! static metadata only, so a search never indexes a value of the configuration, the account or a member. Each entry names
//! its section, its group and the words that find it; a section may carry anchors (rows inside it).

use dilla_client::state::{AppState, CommunityId};

use crate::router::{ServerSection, UserSection};
use crate::strings::t;

/// Which section a result opens.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Section {
    User(UserSection),
    Server(ServerSection),
}

/// What the viewer may do in a server (`SettingsAccess`): the advisory gate on which server sections are listed. The
/// instance checks every operation itself.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Access {
    pub loaded: bool,
    pub manage_community: bool,
    pub manage_channels: bool,
    pub manage_roles: bool,
    pub create_invite: bool,
    pub ban_members: bool,
    pub view_audit_log: bool,
}

impl Access {
    /// The viewer's access in `community`. A member sees the sections every member sees; the owner has every right. The
    /// roles area (Task 14) refines this for the other members when its slice lands: until then they are listed only the
    /// sections the registry shows every member.
    pub fn of(state: &AppState, community: CommunityId) -> Self {
        let Some(c) = state.communities.iter().find(|c| c.id == community) else {
            return Self::default();
        };
        let owner = Some(c.owner) == state.account.user_id;
        Self {
            loaded: true,
            manage_community: owner,
            manage_channels: owner,
            manage_roles: owner,
            create_invite: owner,
            ban_members: owner,
            view_audit_log: owner,
        }
    }
}

/// `SettingsAccess` keys a server section may need.
#[derive(Clone, Copy)]
enum Need {
    Member,
    ManageCommunity,
    ManageChannels,
    ManageRoles,
    CreateInvite,
    BanMembers,
    ViewAuditLog,
    /// Manage the community and manage roles.
    JoinRoles,
}

impl Need {
    fn met(self, a: &Access) -> bool {
        a.loaded
            && match self {
                Self::Member => true,
                Self::ManageCommunity => a.manage_community,
                Self::ManageChannels => a.manage_channels,
                Self::ManageRoles => a.manage_roles,
                Self::CreateInvite => a.create_invite,
                Self::BanMembers => a.ban_members,
                Self::ViewAuditLog => a.view_audit_log,
                Self::JoinRoles => a.manage_community && a.manage_roles,
            }
    }
}

/// A row inside a section: its label key and the words that find it.
struct Anchor {
    id: &'static str,
    label: &'static str,
    keywords: &'static [&'static str],
}

struct Entry {
    section: Section,
    /// `workspace.group.*`.
    group: &'static str,
    /// `workspace.<id>`.
    label: &'static str,
    keywords: &'static [&'static str],
    need: Need,
    anchors: &'static [Anchor],
}

const fn anchor(
    id: &'static str,
    label: &'static str,
    keywords: &'static [&'static str],
) -> Anchor {
    Anchor {
        id,
        label,
        keywords,
    }
}

const fn user(
    id: UserSection,
    label: &'static str,
    keywords: &'static [&'static str],
    anchors: &'static [Anchor],
) -> Entry {
    Entry {
        section: Section::User(id),
        group: "workspace.group.you",
        label,
        keywords,
        need: Need::Member,
        anchors,
    }
}

const fn server(
    id: ServerSection,
    group: &'static str,
    label: &'static str,
    need: Need,
    keywords: &'static [&'static str],
    anchors: &'static [Anchor],
) -> Entry {
    Entry {
        section: Section::Server(id),
        group,
        label,
        keywords,
        need,
        anchors,
    }
}

/// `SETTINGS_REGISTRY`, in its order.
static REGISTRY: &[Entry] = &[
    user(
        UserSection::Profile,
        "workspace.profile",
        &["account", "identity"],
        &[
            anchor(
                "profile-display",
                "profile.display.label",
                &["name", "nickname"],
            ),
            anchor(
                "profile-status",
                "profile.status.label",
                &["presence", "message"],
            ),
            anchor("profile-bio", "profile.bio.label", &["biography", "about"]),
        ],
    ),
    user(
        UserSection::Signin,
        "workspace.signin",
        &[
            "password",
            "authentication",
            "totp",
            "passkey",
            "recovery",
            "security",
            "two factor",
        ],
        &[
            anchor(
                "signin-password",
                "factorSettings.password",
                &["change", "password"],
            ),
            anchor(
                "signin-totp",
                "factorSettings.totp",
                &["authenticator", "two factor"],
            ),
            anchor(
                "signin-passkeys",
                "factorSettings.passkeys",
                &["security key", "webauthn"],
            ),
            anchor(
                "signin-codes",
                "factorSettings.codes",
                &["backup", "recovery"],
            ),
        ],
    ),
    user(
        UserSection::Devices,
        "workspace.devices",
        &[
            "rename",
            "revoke",
            "recovery key",
            "sign out",
            "forget",
            "sessions",
        ],
        &[],
    ),
    user(
        UserSection::Notifications,
        "workspace.notifications",
        &[
            "sound",
            "mute",
            "browser",
            "permission",
            "mentions",
            "channel",
            "server",
        ],
        &[
            anchor(
                "notify-server",
                "notify.server.hint",
                &["server", "mentions", "default"],
            ),
            anchor(
                "notify-everyone",
                "notify.everyone.label",
                &["everyone", "here"],
            ),
            anchor(
                "notify-desktop",
                "notify.desktop.label",
                &["browser", "permission", "desktop"],
            ),
            anchor(
                "notify-sounds",
                "notify.sounds.label",
                &["join", "leave", "ring", "sound"],
            ),
        ],
    ),
    user(
        UserSection::Voice,
        "workspace.voice",
        &[
            "microphone",
            "input",
            "output",
            "speaker",
            "permission",
            "push to talk",
            "ptt",
            "noise",
            "test",
        ],
        &[anchor(
            "voice-test",
            "signalSettings.voice.testStart",
            &[
                "microphone",
                "input",
                "test",
                "sensitivity",
                "threshold",
                "volume",
            ],
        )],
    ),
    user(
        UserSection::Keyboard,
        "workspace.keyboard",
        &["shortcut", "binding", "keys", "push to talk"],
        &[],
    ),
    user(
        UserSection::Appearance,
        "workspace.appearance",
        &["theme", "dark", "light", "contrast", "density", "motion"],
        &[
            anchor(
                "appearance-theme",
                "appearance.theme.label",
                &["dark", "light", "system", "contrast"],
            ),
            anchor(
                "appearance-density",
                "workspace.density",
                &["compact", "regular"],
            ),
            anchor(
                "appearance-motion",
                "workspace.motion",
                &["animation", "accessibility"],
            ),
        ],
    ),
    user(
        UserSection::Privacy,
        "workspace.privacy",
        &[
            "contact",
            "verification",
            "safety number",
            "changed key",
            "compare",
        ],
        &[],
    ),
    user(
        UserSection::Diagnostics,
        "workspace.diagnostics",
        &["connection", "instance", "device", "error", "copy", "debug"],
        &[],
    ),
    server(
        ServerSection::Overview,
        "workspace.group.server",
        "workspace.overview",
        Need::ManageCommunity,
        &["name", "afk", "timeout", "idle"],
        &[],
    ),
    server(
        ServerSection::Channels,
        "workspace.group.server",
        "workspace.channels",
        Need::ManageChannels,
        &["category", "text", "voice", "order", "permissions"],
        &[],
    ),
    server(
        ServerSection::Gifs,
        "workspace.group.server",
        "workspace.gifs",
        Need::ManageCommunity,
        &["giphy", "provider", "key", "rating"],
        &[],
    ),
    server(
        ServerSection::Members,
        "workspace.group.people",
        "workspace.members",
        Need::Member,
        &["people", "role", "kick", "ban", "timeout", "warn"],
        &[],
    ),
    server(
        ServerSection::Roles,
        "workspace.group.people",
        "workspace.roles",
        Need::ManageRoles,
        &["create", "edit", "order", "permissions"],
        &[],
    ),
    server(
        ServerSection::Invites,
        "workspace.group.people",
        "workspace.invites",
        Need::CreateInvite,
        &["invitation", "join", "link"],
        &[],
    ),
    server(
        ServerSection::SelfRoles,
        "workspace.group.people",
        "workspace.selfRoles",
        Need::Member,
        &["self service", "personal roles", "role panel", "publish"],
        &[],
    ),
    server(
        ServerSection::Bot,
        "workspace.group.automation",
        "workspace.bot",
        Need::ManageCommunity,
        &[
            "installation",
            "enable",
            "disable",
            "display name",
            "capabilities",
        ],
        &[],
    ),
    server(
        ServerSection::Welcome,
        "workspace.group.automation",
        "workspace.welcome",
        Need::ManageCommunity,
        &["greeting", "destination", "preview", "join message"],
        &[],
    ),
    server(
        ServerSection::JoinRoles,
        "workspace.group.automation",
        "workspace.joinRoles",
        Need::JoinRoles,
        &["automatic", "new member", "join role"],
        &[],
    ),
    server(
        ServerSection::Music,
        "workspace.group.automation",
        "workspace.music",
        Need::ManageCommunity,
        &["dj roles", "youtube", "provider", "playback"],
        &[anchor(
            "music-providers",
            "workspace.providers.title",
            &["youtube", "playback"],
        )],
    ),
    server(
        ServerSection::ModerationLog,
        "workspace.group.moderation",
        "workspace.moderationLog",
        Need::ViewAuditLog,
        &["audit", "cases", "evidence", "manual"],
        &[],
    ),
    server(
        ServerSection::Bans,
        "workspace.group.moderation",
        "workspace.bans",
        Need::BanMembers,
        &["unban", "banned"],
        &[],
    ),
    server(
        ServerSection::Danger,
        "workspace.group.actions",
        "workspace.danger",
        Need::Member,
        &["ownership", "transfer", "delete", "leave"],
        &[],
    ),
];

/// One setting the palette lists.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Found {
    pub section: Section,
    /// The section's or the anchor's label.
    pub label: String,
    /// `{group} / {section}`.
    pub breadcrumb: String,
    /// The anchor's id; `None` for the section itself.
    pub anchor: Option<&'static str>,
}

/// The sections of `scope` the viewer is shown (`availableSettings`): the user's own, or the server's the access allows.
fn available(server_scope: bool, access: &Access) -> impl Iterator<Item = &'static Entry> + '_ {
    REGISTRY.iter().filter(move |e| match e.section {
        Section::User(_) => !server_scope,
        Section::Server(_) => server_scope && e.need.met(access),
    })
}

/// `searchSettings`: the sections and rows whose words (label, description, breadcrumb, keywords) hold every term of
/// `query`, best first: an exact label, then one that starts with the query, then the rest, each in registry order.
pub fn search(server_scope: bool, query: &str, access: &Access) -> Vec<Found> {
    let normalized = query.trim().to_lowercase();
    if normalized.is_empty() {
        return Vec::new();
    }
    let terms: Vec<&str> = normalized.split_whitespace().collect();
    let mut found: Vec<(u8, Found)> = Vec::new();
    for entry in available(server_scope, access) {
        let breadcrumb = format!("{} / {}", t(entry.group), t(entry.label));
        let own = std::iter::once((None, entry.label, entry.keywords)).chain(
            entry
                .anchors
                .iter()
                .map(|a| (Some(a.id), a.label, a.keywords)),
        );
        for (anchor, label_key, keywords) in own {
            let label = t(label_key);
            let lower = label.to_lowercase();
            let haystack = format!("{label} {breadcrumb} {}", keywords.join(" ")).to_lowercase();
            if !terms.iter().all(|term| haystack.contains(term)) {
                continue;
            }
            let rank = if lower == normalized {
                0
            } else if lower.starts_with(&normalized) {
                1
            } else {
                2
            };
            found.push((
                rank,
                Found {
                    section: entry.section,
                    label: label.to_owned(),
                    breadcrumb: breadcrumb.clone(),
                    anchor,
                },
            ));
        }
    }
    // A stable sort keeps registry order within a rank, as the web's sort does.
    found.sort_by_key(|(rank, _)| *rank);
    found.into_iter().map(|(_, f)| f).collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::strings::try_t;

    fn everything() -> Access {
        Access {
            loaded: true,
            manage_community: true,
            manage_channels: true,
            manage_roles: true,
            create_invite: true,
            ban_members: true,
            view_audit_log: true,
        }
    }

    #[test]
    fn every_key_the_registry_names_is_in_the_copy() {
        for entry in REGISTRY {
            assert!(try_t(entry.group).is_some(), "{}", entry.group);
            assert!(try_t(entry.label).is_some(), "{}", entry.label);
            for a in entry.anchors {
                assert!(try_t(a.label).is_some(), "{}", a.label);
            }
        }
    }

    /// e2e/web/search-settings.spec.ts: "microphone test" finds the Voice section ("You / Voice") first.
    #[test]
    fn microphone_test_finds_the_voice_section() {
        let found = search(false, "microphone test", &Access::default());
        assert_eq!(found[0].breadcrumb, "You / Voice");
        assert_eq!(found[0].section, Section::User(UserSection::Voice));
        // The "test" row inside it, by anchor, follows or leads; every result is in Voice.
        assert!(
            found
                .iter()
                .all(|f| f.section == Section::User(UserSection::Voice))
        );
    }

    #[test]
    fn an_empty_query_finds_nothing_and_a_label_beats_a_keyword() {
        assert!(search(false, "   ", &Access::default()).is_empty());
        let found = search(false, "voice", &Access::default());
        assert_eq!(found[0].label, "Voice");
        assert_eq!(found[0].anchor, None);
    }

    #[test]
    fn server_sections_follow_the_access() {
        let none = Access {
            loaded: true,
            ..Access::default()
        };
        let sections = |a: &Access| -> Vec<_> {
            search(true, "a", a)
                .into_iter()
                .map(|f| f.section)
                .collect::<Vec<_>>()
        };
        assert!(!sections(&Access::default()).iter().any(|_| true));
        let member = sections(&none);
        assert!(member.contains(&Section::Server(ServerSection::Members)));
        assert!(!member.contains(&Section::Server(ServerSection::Roles)));
        assert!(sections(&everything()).contains(&Section::Server(ServerSection::Roles)));
    }
}
