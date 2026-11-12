//! A settings window around one section: the frame, the nav of `settings/SettingsNavigation.tsx` (the group "You", then
//! the selected server's own group) and the section drawn by the area that owns it through `slots::settings_sections`.
//!
//! This is the controller hook of Task 21's fixtures: the shell's settings route (Task 8/15) is not wired yet, so
//! `AppView` shows this view for a fixture whose route is a server settings section (`fixture::screen_of`). The controller
//! replaces it with the real settings host at merge; the nav and the frame are the shared components either way.

use std::sync::Arc;

use dilla_client::state::{AppState, CommunityId};
use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::settings::frame::SettingsFrame;
use dilla_desktop_ui::components::settings::nav::{NavItem, SettingsNav};
use gpui::{Context, IntoElement, ParentElement, Render, Window};

use crate::router::{ServerSection, UserSection};
use crate::screens::shell::Clock;
use crate::slots::{self, SectionId, SlotCx};
use crate::strings::t;

fn user_icon(s: UserSection) -> IconName {
    match s {
        UserSection::Profile => IconName::User,
        UserSection::Signin => IconName::Lock,
        UserSection::Devices => IconName::MonitorSmartphone,
        UserSection::Notifications => IconName::Bell,
        UserSection::Voice => IconName::Mic,
        UserSection::Keyboard => IconName::Keyboard,
        UserSection::Appearance => IconName::Palette,
        UserSection::Privacy => IconName::Fingerprint,
        UserSection::Diagnostics => IconName::Activity,
    }
}

fn server_icon(s: ServerSection) -> IconName {
    match s {
        ServerSection::Overview => IconName::Server,
        ServerSection::Channels => IconName::Hash,
        ServerSection::Gifs => IconName::Smile,
        ServerSection::Members => IconName::Users,
        ServerSection::Roles => IconName::Shield,
        ServerSection::Invites => IconName::Link,
        ServerSection::SelfRoles => IconName::User,
        ServerSection::Bot => IconName::Bot,
        ServerSection::Welcome => IconName::Megaphone,
        ServerSection::JoinRoles => IconName::UserPlus,
        ServerSection::Music => IconName::Music,
        ServerSection::ModerationLog => IconName::ScrollText,
        ServerSection::Bans => IconName::Ban,
        ServerSection::Danger => IconName::TriangleAlert,
    }
}

/// Sections the content pane draws at 1024px (`wide` in the registry).
fn wide(s: ServerSection) -> bool {
    matches!(
        s,
        ServerSection::Channels
            | ServerSection::Members
            | ServerSection::ModerationLog
            | ServerSection::Bans
    )
}

pub struct SettingsHost {
    state: Arc<AppState>,
    community: CommunityId,
    section: ServerSection,
    clock: Clock,
}

impl SettingsHost {
    pub fn new(
        state: Arc<AppState>,
        community: CommunityId,
        section: ServerSection,
        clock: Clock,
    ) -> Self {
        Self {
            state,
            community,
            section,
            clock,
        }
    }
}

impl Render for SettingsHost {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let server = self
            .state
            .communities
            .iter()
            .find(|c| c.id == self.community)
            .map(|c| c.name.clone())
            .unwrap_or_default();
        // Prototype navItems order first, then the live sections it does not draw (SettingsNavigation.tsx).
        let first_user = [
            UserSection::Profile,
            UserSection::Signin,
            UserSection::Devices,
            UserSection::Notifications,
            UserSection::Voice,
            UserSection::Keyboard,
        ];
        let first_server = [
            ServerSection::Overview,
            ServerSection::Roles,
            ServerSection::Invites,
            ServerSection::Bot,
            ServerSection::Music,
        ];
        let users = first_user.iter().copied().chain(
            UserSection::ALL
                .iter()
                .copied()
                .filter(|s| !first_user.contains(s)),
        );
        let servers = first_server.iter().copied().chain(
            ServerSection::ALL
                .iter()
                .copied()
                .filter(|s| !first_server.contains(s)),
        );
        let mut items: Vec<NavItem> = users
            .map(|s| {
                NavItem::new(s.id(), t(&format!("workspace.{}", s.id())))
                    .icon(user_icon(s))
                    .group(t("workspace.group.you"))
            })
            .collect();
        items.extend(servers.map(|s| {
            NavItem::new(
                format!("server:{}", s.id()),
                t(&format!("workspace.{}", s.id())),
            )
            .icon(server_icon(s))
            .group(server.clone())
        }));
        let ctx = SlotCx {
            state: self.state.clone(),
            client: None,
            community: Some(self.community),
            channel: None,
            clock: self.clock,
        };
        let id = SectionId::Server(self.section);
        let body = slots::settings_sections()
            .into_iter()
            .find(|s| s.id == id)
            .map(|s| (s.view)(id, &ctx, window, cx))
            .unwrap_or_else(|| gpui::Empty.into_any_element());
        SettingsFrame::new("settings-host", t("settings.close"))
            .wide(wide(self.section))
            .nav(SettingsNav::new(
                "settings-nav",
                items,
                format!("server:{}", self.section.id()),
            ))
            .child(body)
    }
}
