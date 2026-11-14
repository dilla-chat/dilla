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
use gpui::{AnyElement, App, IntoElement, ParentElement, SharedString, Styled, Window, div, px};

use dilla_desktop_ui::IconName;

use crate::router::{Route, ServerSection, UserSection};
use crate::screens::settings_host::SettingsWorkspace;
use crate::screens::shell::Clock;
use crate::screens::{
    attachments, bot_admin, commands, diagnostics, dms, factor, members, messages, notify,
    reactions, search, server_settings, threads, user_settings, voice, voice_settings,
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
    /// The dirty-draft guard of the settings host that draws a settings section; `None` elsewhere. A section's hook
    /// attaches and publishes through it (the host fills it per render).
    pub workspace: Option<SettingsWorkspace>,
}

impl Default for SlotCx {
    fn default() -> Self {
        Self {
            state: Arc::default(),
            client: None,
            community: None,
            channel: None,
            clock: Clock::Local,
            workspace: None,
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

/// One guest's part under a message's body, with the height it takes there (logical px). The timeline lays its rows
/// out from heights, so a guest says its own: computed from its content and the tokens, never measured from a
/// reference (R21). The element is drawn in a box of exactly that height.
pub struct Extra {
    pub element: AnyElement,
    pub height: f32,
}

/// Under a message's body (after its attachments slot): the attachments (12), the poll card (20), the reactions (10),
/// the thread count (11), in this order, each [`EXTRA_GAP`] below what is above it. Answers the stack and its height (the sum of the parts';
/// 0 when every guest has nothing), which the timeline adds to the row's height.
pub fn message_extras(
    item: &TimelineItem,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> (AnyElement, f32) {
    let parts = [
        attachments::attachment_cards(item, ctx, window, cx),
        commands::poll_card(item, ctx, window, cx),
        reactions::reaction_bar(item, ctx, window, cx),
        threads::thread_count(item, ctx, window, cx),
    ];
    stack(parts.into_iter().flatten())
}

/// Above a message's head (the row's grid `order: -1`): the reply line (11). The timeline moves the head, the body and
/// what follows down by its height and the grid's gap, and the avatar and the gutter's time by `data-reply`'s 20px.
pub fn message_before_head(
    item: &TimelineItem,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    threads::reply_chip(item, ctx, window, cx)
}

/// MessageRow.css `.d-message-row__main`: a grid with `gap: 1px`, so every extra under the body sits 1px below the item
/// above it.
pub const EXTRA_GAP: f32 = 1.0;

/// [`Extra`]s stacked in order, each in a box of its height [`EXTRA_GAP`] below the item above it (the body, then each
/// other), and the total height. A part with no height (a popover anchored to the row, such as the emoji picker on a
/// message with no reactions) is no row of the web's grid, so it takes no gap.
pub fn stack(parts: impl IntoIterator<Item = Extra>) -> (AnyElement, f32) {
    let mut height = 0.0;
    let column = div()
        .flex()
        .flex_col()
        .children(parts.into_iter().map(|part| {
            let gap = if part.height > 0.0 { EXTRA_GAP } else { 0.0 };
            height += gap + part.height;
            div()
                .flex_none()
                .pt(px(gap))
                .h(px(gap + part.height))
                .child(part.element)
        }));
    (column.into_any_element(), height)
}

/// The message toolbar's and context menu's entries: the message's own (9), then reply and thread (11), then react
/// (10).
pub fn message_actions(item: &TimelineItem, ctx: &SlotCx) -> Vec<MessageAction> {
    let bits = ctx
        .channel
        .and_then(|c| dilla_client::features::roles::viewer_channel_bits(&ctx.state, c));
    let mut out = messages::message_actions(item, bits);
    out.extend(threads::message_actions(item, ctx));
    out.extend(reactions::message_actions(item));
    out
}

/// Over the composer's field: the reply bar (11), then the attachment tray (12). The composer places it in normal flow,
/// so a non-empty guest makes the composer taller and the log above it shorter.
pub fn composer_leading(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> AnyElement {
    column([
        threads::reply_bar(ctx, window, cx),
        attachments::tray(ctx, window, cx),
    ])
}

/// The composer's tool buttons: attach and GIF (12), then emoji (10).
pub fn composer_buttons(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> AnyElement {
    div()
        .flex()
        .items_center()
        // The tools are the composer foot's siblings on the web: its 2px gap lies between each area's buttons too.
        .gap(px(COMPOSER_TOOL_GAP))
        .children(
            [
                attachments::composer_buttons(ctx, window, cx),
                reactions::emoji_button(ctx, window, cx),
            ]
            .into_iter()
            .flatten(),
        )
        .into_any_element()
}

/// Composer.css `.d-composer__foot`: `gap: 2px` between the tools.
const COMPOSER_TOOL_GAP: f32 = 2.0;

/// The popup over the composer: the command picker (20), else the emoji grid (10). The mention popup stays the
/// composer's own (9).
pub fn composer_popup(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    commands::picker(ctx, window, cx).or_else(|| reactions::emoji_popup(ctx, window, cx))
}

/// In place of the composer's field: a picked command's chips (20).
pub fn composer_chips(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    commands::chips::view(ctx, window, cx)
}

/// Under a channel's row in the list: a voice channel's people (17).
pub fn channel_row_extras(ch: &Channel, window: &mut Window, cx: &mut App) -> AnyElement {
    column([voice::channel_people(ch, window, cx)])
}

/// The aside right of the main column: the thread pane (11), else the members pane (14).
pub fn aside(route: &Route, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    threads::pane(route, window, cx).or_else(|| members::pane(route, window, cx))
}

/// Over the user panel: the voice dock (17), then now playing (21).
pub fn dock(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    match (voice::dock(window, cx), bot_admin::now_playing(window, cx)) {
        (None, None) => None,
        (voice, music) => Some(column([voice, music])),
    }
}

/// The modal surfaces over everything, in paint order: the create, join and invite dialogs (25), the search palette
/// (23), the DM picker and the safety-number comparison (13), the step-up (26), then the delete confirmation and the pins
/// dialog (9). Toasts (Task 6) and other dialogs join at merge. While any is drawn, Ctrl+K does not open the palette
/// (the shell's guard).
pub fn overlays(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Vec<AnyElement> {
    let mut out: Vec<AnyElement> = [
        crate::screens::create::overlay(window, cx),
        search::palette(window, cx),
        dms::picker_overlay(&ctx.state, window, cx),
        dms::compare_overlay(&ctx.state, window, cx),
        // The step-up dialog (26), asked for by Task 21's bot page once it merges.
        factor::step_up_overlay(window, cx),
    ]
    .into_iter()
    .flatten()
    .collect();
    out.extend(messages::overlays(window, cx));
    // The moderation commands' confirmation (Task 28 wave 2).
    out.extend(commands::confirm::overlay(ctx, window, cx));
    out
}

/// What takes over the main column for `channel` in place of its header, log and composer: a voice channel's stage
/// (17). `None` for a conversation.
pub fn main_takeover(channel: &Channel, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    voice::stage(channel, window, cx)
}

/// Over the main column: the offline link banner and the version refusal (24), then the conversation's trust surfaces (13).
pub fn banners(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Vec<AnyElement> {
    let mut out = diagnostics::banners(ctx, window, cx);
    // A conversation's trust alerts, the removed note and the own-account banner (13).
    out.extend(dms::conversation::banners(ctx, window, cx));
    out
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

#[cfg(test)]
mod tests {
    use gpui::{IntoElement, div};

    use super::{EXTRA_GAP, Extra, stack};

    fn part(height: f32) -> Extra {
        Extra {
            element: div().into_any_element(),
            height,
        }
    }

    /// Review m1: a part with no height in the row (the emoji picker open on a message with no reactions: a popover, not a
    /// row of the grid) takes no grid gap; the others each sit [`EXTRA_GAP`] below the item above.
    #[test]
    fn a_part_with_no_height_takes_no_gap() {
        assert_eq!(stack([part(0.0)]).1, 0.0);
        assert_eq!(
            stack([part(28.0), part(0.0), part(24.0)]).1,
            28.0 + 24.0 + 2.0 * EXTRA_GAP
        );
    }
}
