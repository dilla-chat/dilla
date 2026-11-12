//! Area **bot admin** (Task 21): the Bot and Music server settings and the now-playing card.
//!
//! Every file under `screens/bot_admin/` is Task 21's. The hooks below are where `crate::slots` draws this area inside
//! another area's view; their signatures are Task 1's.
//!
//! - [`now_playing`] is the compact row in the shell's dock, drawn from the [`player::MusicFeed`] the shell and the voice
//!   area provide while this device is in a call with the Bot in it.
//! - [`settings_section`] draws the Bot and Music sections. Each keeps its drafts and dialogs in a view of its own, kept
//!   in the window under the section's id, so a section left and entered again finds them as they were.

pub mod bot_page;
pub mod keys;
pub mod music_page;
pub mod player;

use gpui::{AnyElement, App, Empty, IntoElement, Window};

use crate::router::ServerSection;
use crate::slots::{SectionId, SlotCx};

/// The now-playing row in the dock (`slots::dock`, after the voice dock).
pub fn now_playing(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    player::dock(window, cx)
}

/// Draws one of this area's settings sections (`slots::settings_sections` points here).
pub fn settings_section(
    section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    match section {
        SectionId::Server(ServerSection::Bot) => {
            let page = window.use_keyed_state("bot-admin-bot-page", cx, |window, cx| {
                bot_page::BotPage::new(ctx.clone(), window, cx)
            });
            page.update(cx, |p, _| p.set_ctx(ctx.clone()));
            page.into_any_element()
        }
        SectionId::Server(ServerSection::Music) => {
            let page = window.use_keyed_state("bot-admin-music-page", cx, |_, _| {
                music_page::MusicPage::new(ctx.clone())
            });
            page.update(cx, |p, _| p.set_ctx(ctx.clone()));
            page.into_any_element()
        }
        _ => Empty.into_any_element(),
    }
}
