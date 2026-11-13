//! The app's routes, as packages/web/src/router.ts names them: [`Route::parse`] is `routeOf` (a path that may carry a
//! `?search`, no history state) and [`Route::path`] is `routePath`. The settings sections are settings/registry.ts's ids.
//!
//! What router.ts keeps in the history state (a settings route's `from`, the privacy `compare`) is the shell's history
//! here, not part of the route. Task 8 owns this file from Wave 1 on (route history, window modes).

use dilla_client::state::{ChannelId, CommunityId};

/// `SignInNotice`: why the welcome screen is shown again.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum SignInNotice {
    Race,
    Evicted,
}

macro_rules! sections {
    ($(#[$doc:meta])* $name:ident { $($variant:ident = $id:literal),* $(,)? }) => {
        $(#[$doc])*
        #[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
        pub enum $name { $($variant),* }

        impl $name {
            /// Every section, in the registry's order.
            pub const ALL: &'static [$name] = &[$($name::$variant),*];

            /// The section's id in paths and in the registry.
            pub fn id(self) -> &'static str {
                match self { $($name::$variant => $id),* }
            }

            pub fn from_id(id: &str) -> Option<Self> {
                match id { $($id => Some($name::$variant),)* _ => None }
            }
        }
    };
}

sections!(
    /// settings/registry.ts `SettingsSection` (scope `user`).
    UserSection {
        Profile = "profile",
        Signin = "signin",
        Devices = "devices",
        Notifications = "notifications",
        Voice = "voice",
        Keyboard = "keyboard",
        Appearance = "appearance",
        Privacy = "privacy",
        Diagnostics = "diagnostics",
    }
);

sections!(
    /// settings/registry.ts `ServerSettingsSection` (scope `server`).
    ServerSection {
        Overview = "overview",
        Channels = "channels",
        Gifs = "gifs",
        Members = "members",
        Roles = "roles",
        Invites = "invites",
        SelfRoles = "selfRoles",
        Bot = "bot",
        Welcome = "welcome",
        JoinRoles = "joinRoles",
        Music = "music",
        ModerationLog = "moderationLog",
        Bans = "bans",
        Danger = "danger",
    }
);

/// router.ts `Route`.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub enum Route {
    Root,
    Welcome {
        invite: Option<String>,
        signin: Option<SignInNotice>,
    },
    Channel {
        community: CommunityId,
        channel: Option<ChannelId>,
    },
    Dm(ChannelId),
    /// Home with the `@` tile selected and no DM open (`/dm`).
    Dms,
    Activity,
    You,
    Settings(Option<UserSection>),
    ServerSettings {
        community: CommunityId,
        section: Option<ServerSection>,
    },
}

/// 32 lower-case hex digits (router.ts `HEX32`), as the 16 bytes they spell.
fn hex32(s: &str) -> Option<[u8; 16]> {
    if s.len() != 32 || !s.bytes().all(|b| matches!(b, b'0'..=b'9' | b'a'..=b'f')) {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, byte) in out.iter_mut().enumerate() {
        *byte = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).ok()?;
    }
    Some(out)
}

/// `encodeURIComponent`.
fn encode_component(s: &str) -> String {
    let mut out = String::with_capacity(s.len());
    for b in s.bytes() {
        if b.is_ascii_alphanumeric() || b"-_.!~*'()".contains(&b) {
            out.push(char::from(b));
        } else {
            out.push_str(&format!("%{b:02X}"));
        }
    }
    out
}

impl Route {
    /// router.ts `routeOf`: the route of `path`, which may carry a `?search`. Anything unknown is [`Route::Root`].
    pub fn parse(path: &str) -> Route {
        let (path, search) = match path.find('?') {
            Some(mark) => (&path[..mark], &path[mark + 1..]),
            None => (path, ""),
        };
        if path == "/" {
            return Route::Root;
        }
        if path == "/welcome" {
            // `URLSearchParams.get`: the first value, `+` and percent escapes decoded.
            let param = |name: &str| {
                url::form_urlencoded::parse(search.as_bytes())
                    .find(|(k, _)| k == name)
                    .map(|(_, v)| v.into_owned())
            };
            let signin = match param("signin").as_deref() {
                Some("race") => Some(SignInNotice::Race),
                Some("evicted") => Some(SignInNotice::Evicted),
                _ => None,
            };
            return Route::Welcome {
                invite: param("invite").filter(|i| !i.is_empty()),
                signin,
            };
        }
        let parts: Vec<&str> = path.split('/').collect();
        match parts.as_slice() {
            // `/c/<id>/settings[/<section>]`: an unknown section is the settings' first.
            ["", "c", c, "settings", rest @ ..] if rest.len() <= 1 => {
                if let Some(community) = hex32(c) {
                    return Route::ServerSettings {
                        community: CommunityId(community),
                        section: rest.first().and_then(|s| ServerSection::from_id(s)),
                    };
                }
            }
            ["", "c", c] => {
                if let Some(community) = hex32(c) {
                    return Route::Channel {
                        community: CommunityId(community),
                        channel: None,
                    };
                }
            }
            ["", "c", c, ch] => {
                if let (Some(community), Some(channel)) = (hex32(c), hex32(ch)) {
                    return Route::Channel {
                        community: CommunityId(community),
                        channel: Some(ChannelId(channel)),
                    };
                }
            }
            ["", "dm", ch] => {
                if let Some(channel) = hex32(ch) {
                    return Route::Dm(ChannelId(channel));
                }
            }
            ["", "dm"] => return Route::Dms,
            ["", "activity"] => return Route::Activity,
            ["", "you"] => return Route::You,
            ["", "settings"] => return Route::Settings(None),
            // `/settings/<section>`: an unknown section is the settings' first; `/settings/` is no route.
            ["", "settings", section] if !section.is_empty() => {
                return Route::Settings(UserSection::from_id(section));
            }
            _ => {}
        }
        Route::Root
    }

    /// router.ts `routePath`.
    pub fn path(&self) -> String {
        match self {
            Route::Root => "/".to_owned(),
            Route::Welcome { invite, signin } => {
                let mut parts = Vec::new();
                if let Some(invite) = invite.as_deref().filter(|i| !i.is_empty()) {
                    parts.push(format!("invite={}", encode_component(invite)));
                }
                match signin {
                    Some(SignInNotice::Race) => parts.push("signin=race".to_owned()),
                    Some(SignInNotice::Evicted) => parts.push("signin=evicted".to_owned()),
                    None => {}
                }
                if parts.is_empty() {
                    "/welcome".to_owned()
                } else {
                    format!("/welcome?{}", parts.join("&"))
                }
            }
            Route::Channel { community, channel } => match channel {
                Some(channel) => format!("/c/{community}/{channel}"),
                None => format!("/c/{community}"),
            },
            Route::Dm(channel) => format!("/dm/{channel}"),
            Route::Dms => "/dm".to_owned(),
            Route::Activity => "/activity".to_owned(),
            Route::You => "/you".to_owned(),
            Route::Settings(None) => "/settings".to_owned(),
            Route::Settings(Some(section)) => format!("/settings/{}", section.id()),
            Route::ServerSettings { community, section } => match section {
                Some(section) => format!("/c/{community}/settings/{}", section.id()),
                None => format!("/c/{community}/settings"),
            },
        }
    }
}

impl Route {
    /// Whether this route is one of the settings screens (user or server), which the shell draws over the route it was
    /// opened from ([`History::settings_from`]).
    pub fn is_settings(&self) -> bool {
        matches!(self, Route::Settings(_) | Route::ServerSettings { .. })
    }
}

/// The route history of a window (the browser's `history` of router.ts): the entries visited and where the window stands
/// among them. A push drops the entries ahead; pushing the route the window is already at changes nothing, as
/// `navigate` does for an identical path.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct History {
    entries: Vec<Route>,
    at: usize,
}

/// The most entries a window keeps (the oldest fall away).
const HISTORY_CAP: usize = 100;

impl History {
    /// A history standing on `route`.
    pub fn new(route: Route) -> Self {
        Self {
            entries: vec![route],
            at: 0,
        }
    }

    pub fn current(&self) -> &Route {
        &self.entries[self.at]
    }

    /// Goes to `route` as a new entry. Answers whether the window moved.
    pub fn push(&mut self, route: Route) -> bool {
        if *self.current() == route {
            return false;
        }
        self.entries.truncate(self.at + 1);
        self.entries.push(route);
        if self.entries.len() > HISTORY_CAP {
            self.entries.remove(0);
        }
        self.at = self.entries.len() - 1;
        true
    }

    /// Replaces the current entry (a redirect, a route that settled on its default).
    pub fn replace(&mut self, route: Route) {
        let at = self.at;
        self.entries[at] = route;
    }

    pub fn can_back(&self) -> bool {
        self.at > 0
    }

    pub fn can_forward(&self) -> bool {
        self.at + 1 < self.entries.len()
    }

    /// One entry back; the route now current, or `None` at the first entry.
    pub fn back(&mut self) -> Option<&Route> {
        if !self.can_back() {
            return None;
        }
        self.at -= 1;
        Some(self.current())
    }

    pub fn forward(&mut self) -> Option<&Route> {
        if !self.can_forward() {
            return None;
        }
        self.at += 1;
        Some(self.current())
    }

    /// Where closing settings goes: the nearest earlier entry that is not a settings route, else `fallback`.
    pub fn settings_from(&self, fallback: Route) -> Route {
        self.entries[..self.at]
            .iter()
            .rev()
            .find(|r| !r.is_settings() && !matches!(r, Route::Welcome { .. }))
            .cloned()
            .unwrap_or(fallback)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const C: &str = "0123456789abcdef0123456789abcdef";
    const CH: &str = "fedcba9876543210fedcba9876543210";
    const DM: &str = "9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a";

    fn community(s: &str) -> CommunityId {
        CommunityId(hex32(s).unwrap())
    }
    fn channel(s: &str) -> ChannelId {
        ChannelId(hex32(s).unwrap())
    }
    fn welcome(invite: Option<&str>, signin: Option<SignInNotice>) -> Route {
        Route::Welcome {
            invite: invite.map(str::to_owned),
            signin,
        }
    }

    /// router.test.tsx `parseRoute` cases (the history state, `from` and `compare`, is the shell's here).
    #[test]
    fn parse_matches_router_ts_examples() {
        let c_upper = C.to_uppercase();
        let dm_upper = DM.to_uppercase();
        let cases: Vec<(String, Route)> = vec![
            ("/".into(), Route::Root),
            ("/welcome".into(), welcome(None, None)),
            (
                "/welcome?invite=ABCD-EFGH".into(),
                welcome(Some("ABCD-EFGH"), None),
            ),
            ("/welcome?invite=".into(), welcome(None, None)),
            ("/welcome?invite=a%20b".into(), welcome(Some("a b"), None)),
            (
                "/welcome?signin=race".into(),
                welcome(None, Some(SignInNotice::Race)),
            ),
            (
                "/welcome?invite=ABCD-EFGH&signin=race".into(),
                welcome(Some("ABCD-EFGH"), Some(SignInNotice::Race)),
            ),
            (
                "/welcome?signin=evicted".into(),
                welcome(None, Some(SignInNotice::Evicted)),
            ),
            ("/welcome?signin=other".into(), welcome(None, None)),
            (
                format!("/c/{C}"),
                Route::Channel {
                    community: community(C),
                    channel: None,
                },
            ),
            (
                format!("/c/{C}/{CH}?x=1"),
                Route::Channel {
                    community: community(C),
                    channel: Some(channel(CH)),
                },
            ),
            (format!("/c/{c_upper}"), Route::Root),
            (format!("/c/{C}/"), Route::Root),
            (format!("/c/{C}/{CH}/x"), Route::Root),
            ("/c/abc".into(), Route::Root),
            ("/welcome/".into(), Route::Root),
            ("/v1/instance".into(), Route::Root),
            (format!("/dm/{DM}"), Route::Dm(channel(DM))),
            (format!("/dm/{dm_upper}"), Route::Root),
            ("/dm/".into(), Route::Root),
            (format!("/dm/{DM}/x"), Route::Root),
            ("/settings".into(), Route::Settings(None)),
            (
                "/settings/devices".into(),
                Route::Settings(Some(UserSection::Devices)),
            ),
            (
                "/settings/notifications".into(),
                Route::Settings(Some(UserSection::Notifications)),
            ),
            (
                "/settings/appearance".into(),
                Route::Settings(Some(UserSection::Appearance)),
            ),
            (
                "/settings/privacy".into(),
                Route::Settings(Some(UserSection::Privacy)),
            ),
            (
                "/settings/keyboard".into(),
                Route::Settings(Some(UserSection::Keyboard)),
            ),
            (
                "/settings/voice".into(),
                Route::Settings(Some(UserSection::Voice)),
            ),
            (
                "/settings/diagnostics".into(),
                Route::Settings(Some(UserSection::Diagnostics)),
            ),
            (
                "/settings/signin".into(),
                Route::Settings(Some(UserSection::Signin)),
            ),
            ("/settings/unrecognized".into(), Route::Settings(None)),
            (
                format!("/c/{C}/settings"),
                Route::ServerSettings {
                    community: community(C),
                    section: None,
                },
            ),
            (
                format!("/c/{C}/settings/channels"),
                Route::ServerSettings {
                    community: community(C),
                    section: Some(ServerSection::Channels),
                },
            ),
            (
                format!("/c/{C}/settings/roles"),
                Route::ServerSettings {
                    community: community(C),
                    section: Some(ServerSection::Roles),
                },
            ),
            (
                format!("/c/{C}/settings/future"),
                Route::ServerSettings {
                    community: community(C),
                    section: None,
                },
            ),
            ("/settings/".into(), Route::Root),
            // routePath's `routeOf` examples.
            (
                "/welcome?invite=a%20b%26c".into(),
                welcome(Some("a b&c"), None),
            ),
            (
                "/welcome?invite=a%20b%26c&signin=race".into(),
                welcome(Some("a b&c"), Some(SignInNotice::Race)),
            ),
            ("/dm".into(), Route::Dms),
            ("/activity".into(), Route::Activity),
            ("/you".into(), Route::You),
        ];
        for (path, want) in cases {
            assert_eq!(Route::parse(&path), want, "{path}");
        }
    }

    /// router.test.tsx `routePath` cases, and every route form back through `parse`.
    #[test]
    fn path_inverts_parse() {
        let cases: Vec<(Route, String)> = vec![
            (Route::Root, "/".into()),
            (welcome(None, None), "/welcome".into()),
            (
                welcome(Some("a b&c"), None),
                "/welcome?invite=a%20b%26c".into(),
            ),
            (
                welcome(Some("a b&c"), Some(SignInNotice::Race)),
                "/welcome?invite=a%20b%26c&signin=race".into(),
            ),
            (
                welcome(None, Some(SignInNotice::Race)),
                "/welcome?signin=race".into(),
            ),
            (
                welcome(None, Some(SignInNotice::Evicted)),
                "/welcome?signin=evicted".into(),
            ),
            (
                Route::Channel {
                    community: community(C),
                    channel: None,
                },
                format!("/c/{C}"),
            ),
            (
                Route::Channel {
                    community: community(C),
                    channel: Some(channel(CH)),
                },
                format!("/c/{C}/{CH}"),
            ),
            (Route::Dm(channel(DM)), format!("/dm/{DM}")),
            (Route::Dms, "/dm".into()),
            (Route::Activity, "/activity".into()),
            (Route::You, "/you".into()),
            (
                Route::Settings(Some(UserSection::Notifications)),
                "/settings/notifications".into(),
            ),
            (Route::Settings(None), "/settings".into()),
            (
                Route::ServerSettings {
                    community: community(C),
                    section: None,
                },
                format!("/c/{C}/settings"),
            ),
            (
                Route::ServerSettings {
                    community: community(C),
                    section: Some(ServerSection::Roles),
                },
                format!("/c/{C}/settings/roles"),
            ),
        ];
        for (route, want) in cases {
            assert_eq!(route.path(), want);
            assert_eq!(Route::parse(&want), route, "{want}");
        }
        for &section in UserSection::ALL {
            let route = Route::Settings(Some(section));
            assert_eq!(Route::parse(&route.path()), route);
        }
        for &section in ServerSection::ALL {
            let route = Route::ServerSettings {
                community: community(C),
                section: Some(section),
            };
            assert_eq!(Route::parse(&route.path()), route);
        }
    }

    /// The section ids are exactly settings/registry.ts's, in its order.
    #[test]
    fn sections_match_the_web_registry() {
        let registry = std::fs::read_to_string(
            std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
                .join("../../../packages/web/src/settings/registry.ts"),
        )
        .expect("the web settings registry");
        let mut user = Vec::new();
        let mut server = Vec::new();
        for line in registry.lines() {
            let line = line.trim_start();
            for (prefix, out) in [("user('", &mut user), ("server('", &mut server)] {
                if let Some(rest) = line.strip_prefix(prefix) {
                    out.push(rest.split('\'').next().unwrap_or_default().to_owned());
                }
            }
        }
        let ids = |all: Vec<&'static str>| all.into_iter().map(str::to_owned).collect::<Vec<_>>();
        assert_eq!(user, ids(UserSection::ALL.iter().map(|s| s.id()).collect()));
        assert_eq!(
            server,
            ids(ServerSection::ALL.iter().map(|s| s.id()).collect())
        );
    }

    #[test]
    fn history_pushes_steps_and_drops_the_forward_entries() {
        let a = Route::Channel {
            community: community(C),
            channel: Some(channel(CH)),
        };
        let mut h = History::new(Route::Root);
        assert!(!h.can_back() && !h.can_forward());
        assert!(h.push(a.clone()));
        assert!(!h.push(a.clone()), "the same route is no new entry");
        assert!(h.push(Route::Activity));
        assert_eq!(h.back(), Some(&a));
        assert_eq!(h.back(), Some(&Route::Root));
        assert_eq!(h.back(), None);
        assert_eq!(h.forward(), Some(&a));
        // A push from the middle drops what was ahead.
        assert!(h.push(Route::You));
        assert!(!h.can_forward());
        assert_eq!(h.back(), Some(&a));
    }

    #[test]
    fn closing_settings_goes_to_the_route_it_was_opened_from() {
        let a = Route::Channel {
            community: community(C),
            channel: Some(channel(CH)),
        };
        let mut h = History::new(a.clone());
        h.push(Route::Settings(None));
        h.push(Route::Settings(Some(UserSection::Voice)));
        assert_eq!(h.settings_from(Route::Root), a);
        let h = History::new(Route::Settings(None));
        assert_eq!(h.settings_from(Route::Dms), Route::Dms);
    }

    #[test]
    fn history_is_capped() {
        let mut h = History::new(Route::Root);
        for n in 0..(HISTORY_CAP + 20) {
            let mut id = [0u8; 16];
            id[0] = (n % 250) as u8;
            id[1] = (n / 250) as u8;
            h.push(Route::Dm(ChannelId(id)));
        }
        assert_eq!(h.entries.len(), HISTORY_CAP);
        assert!(!h.can_forward());
    }
}
