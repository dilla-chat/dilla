//! Where one area draws inside another area's view. The host (the shell, the timeline, the composer, the channel list)
//! calls a slot here; the slot calls each guest area's hook in `crate::screens::<area>`, in a fixed order. A guest task
//! implements only its own hook, in its own files; this file and the order are Task 1's, and in Wave 1 only the
//! controller edits it, at merge (clients/desktop/OWNERSHIP.md).
//!
//! Every hook is a stub that draws nothing until its task fills it, and every host places a slot where an empty one
//! changes no pixel (no gap, no padding of its own): the slice's parity tests pass unchanged with every slot stubbed.

use std::rc::Rc;
use std::sync::Arc;

use dilla_client::state::{AppState, Channel, ChannelId, CommunityId, TimelineItem};
use dilla_client::{Area, Client};
use gpui::{AnyElement, App, IntoElement, ParentElement, SharedString, Styled, Window, div};

use dilla_desktop_ui::IconName;

use crate::router::{Route, ServerSection, UserSection};
use crate::screens::shell::Clock;
use crate::screens::{
    attachments, bot_admin, commands, diagnostics, dms, factor, members, messages, notify,
    reactions, ring, search, server_settings, threads, user_settings, voice, voice_settings,
};

/// What a slot's guests may read about the view they draw in. Cheap to clone.
#[derive(Clone)]
pub struct SlotCx {
    pub state: Arc<AppState>,
    /// The running client; `None` while drawing a fixture.
    pub client: Option<Rc<Client>>,
    pub community: Option<CommunityId>,
    pub channel: Option<ChannelId>,
    pub clock: Clock,
}

impl Default for SlotCx {
    fn default() -> Self {
        Self {
            state: Arc::default(),
            client: None,
            community: None,
            channel: None,
            clock: Clock::Local,
        }
    }
}

/// What a message action does when chosen.
pub type OnAction = Rc<dyn Fn(&SlotCx, &mut Window, &mut App)>;

/// One entry of the message toolbar and context menu.
#[derive(Clone)]
pub struct MessageAction {
    /// Stable, for automation: `message-action-<id>`.
    pub id: SharedString,
    /// The `strings.rs` key of its label.
    pub label: &'static str,
    pub icon: IconName,
    pub area: Area,
    pub run: OnAction,
}

/// A settings section's id, in the user or the server registry.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum SectionId {
    User(UserSection),
    Server(ServerSection),
}

/// Draws a settings section.
pub type SectionView = fn(SectionId, &SlotCx, &mut Window, &mut App) -> AnyElement;

/// One entry of the settings registry: the section, the area that owns it and its view in that area's module.
#[derive(Clone, Copy)]
pub struct SettingsSection {
    pub id: SectionId,
    pub area: Area,
    pub view: SectionView,
}

/// The guests of a slot, in order, gathered into one element that is empty when they all are: a flex column without
/// gap, padding or size of its own.
fn column(parts: impl IntoIterator<Item = Option<AnyElement>>) -> AnyElement {
    div()
        .flex()
        .flex_col()
        .children(parts.into_iter().flatten())
        .into_any_element()
}

/// Under a message's body: the reply chip (11), the attachments (12), the poll card (20), the reactions (10), the thread
/// count (11), in this order.
pub fn message_extras(item: &TimelineItem, ctx: &SlotCx, cx: &mut App) -> AnyElement {
    column([
        threads::reply_chip(item, ctx, cx),
        attachments::attachment_cards(item, ctx, cx),
        commands::poll_card(item, ctx, cx),
        reactions::reaction_bar(item, ctx, cx),
        threads::thread_count(item, ctx, cx),
    ])
}

/// The message toolbar's and context menu's entries: the message's own (9), then reply and thread (11), then react
/// (10).
pub fn message_actions(item: &TimelineItem) -> Vec<MessageAction> {
    let mut out = messages::message_actions(item);
    out.extend(threads::message_actions(item));
    out.extend(reactions::message_actions(item));
    out
}

/// Over the composer's field: the reply bar (11), then the attachment tray (12).
pub fn composer_leading(ctx: &SlotCx, cx: &mut App) -> AnyElement {
    column([threads::reply_bar(ctx, cx), attachments::tray(ctx, cx)])
}

/// The composer's tool buttons: attach and GIF (12), then emoji (10).
pub fn composer_buttons(ctx: &SlotCx, cx: &mut App) -> AnyElement {
    div()
        .flex()
        .items_center()
        .children(
            [
                attachments::composer_buttons(ctx, cx),
                reactions::emoji_button(ctx, cx),
            ]
            .into_iter()
            .flatten(),
        )
        .into_any_element()
}

/// The popup over the composer: the command picker (20). The mention popup stays the composer's own (9).
pub fn composer_popup(ctx: &SlotCx, cx: &mut App) -> Option<AnyElement> {
    commands::picker(ctx, cx)
}

/// Under a channel's row in the list: a voice channel's people (17).
pub fn channel_row_extras(ch: &Channel, cx: &mut App) -> AnyElement {
    column([voice::channel_people(ch, cx)])
}

/// The aside right of the main column: the thread pane (11), else the members pane (14).
pub fn aside(route: &Route, cx: &mut App) -> Option<AnyElement> {
    threads::pane(route, cx).or_else(|| members::pane(route, cx))
}

/// Over the user panel: the voice dock (17), then now playing (21).
pub fn dock(cx: &mut App) -> Option<AnyElement> {
    match (voice::dock(cx), bot_admin::now_playing(cx)) {
        (None, None) => None,
        (voice, music) => Some(column([voice, music])),
    }
}

/// Over everything, in paint order: the search palette (23), then the ring banner (18). Toasts (Task 6) and dialogs
/// join at merge.
pub fn overlays(cx: &mut App) -> Vec<AnyElement> {
    [search::palette(cx), ring::ring_banner(cx)]
        .into_iter()
        .flatten()
        .collect()
}

/// Over the main column: the offline link banner and the version refusal (24).
pub fn banners(ctx: &SlotCx, cx: &mut App) -> Vec<AnyElement> {
    diagnostics::banners(ctx, cx)
}

/// The user and server settings registries, in settings/registry.ts's order; each entry points into its area's module.
pub fn settings_sections() -> Vec<SettingsSection> {
    let user = |section: UserSection| {
        let (area, view): (Area, SectionView) = match section {
            UserSection::Profile
            | UserSection::Devices
            | UserSection::Appearance
            | UserSection::Keyboard => (Area::UserSettings, user_settings::settings_section),
            UserSection::Signin => (Area::Factors, factor::settings_section),
            UserSection::Notifications => (Area::Notify, notify::settings_section),
            UserSection::Voice => (Area::VoiceSettings, voice_settings::settings_section),
            UserSection::Privacy => (Area::Dms, dms::settings_section),
            UserSection::Diagnostics => (Area::Diagnostics, diagnostics::settings_section),
        };
        SettingsSection {
            id: SectionId::User(section),
            area,
            view,
        }
    };
    let server = |section: ServerSection| {
        let (area, view): (Area, SectionView) = match section {
            ServerSection::Overview
            | ServerSection::Channels
            | ServerSection::Gifs
            | ServerSection::Invites
            | ServerSection::Welcome
            | ServerSection::JoinRoles
            | ServerSection::Danger => (Area::ServerSettings, server_settings::settings_section),
            ServerSection::Members
            | ServerSection::Roles
            | ServerSection::SelfRoles
            | ServerSection::ModerationLog
            | ServerSection::Bans => (Area::Members, members::settings_section),
            ServerSection::Bot | ServerSection::Music => {
                (Area::BotAdmin, bot_admin::settings_section)
            }
        };
        SettingsSection {
            id: SectionId::Server(section),
            area,
            view,
        }
    };
    UserSection::ALL
        .iter()
        .map(|&s| user(s))
        .chain(ServerSection::ALL.iter().map(|&s| server(s)))
        .collect()
}
