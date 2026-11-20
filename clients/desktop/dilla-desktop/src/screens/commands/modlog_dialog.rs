//! `/modlog` (Decision D1; CommandHost.tsx:411): the moderation history in a dialog over the conversation the command was
//! typed in. Its body is the one the server's moderation-log settings page draws
//! ([`crate::screens::members::moderation_log_section`]), the web's `ModerationLog`; the title is
//! `admin.bot.moderationLog.title`.
//!
//! Escape and the close button dismiss it. It closes itself as the window leaves that conversation (CommandHost.tsx:130,
//! `setModlogOpen(false)` on a key change). Per window ([`crate::per_window`]), so one window's history never opens in
//! another. The members area's own `modlog` module holds what the history has loaded; this one only says whether, and for
//! which conversation, the dialog is open.

use dilla_client::state::CommunityId;
use dilla_client::state::bot::Conversation;
use dilla_desktop_ui::components::dialog::Dialog;
use gpui::{AnyElement, App, IntoElement, WindowId};

use crate::slots::SlotCx;
use crate::strings::t;

/// The window's open history: its community and the conversation it was asked for in.
#[derive(Default)]
struct OpenModlog(Option<(CommunityId, Conversation)>);

/// Opens `community`'s moderation history over `conversation` in `window` (the command's run). It stays open until it is
/// closed or the window leaves that conversation.
pub fn open(window: WindowId, community: CommunityId, conversation: Conversation, cx: &mut App) {
    crate::per_window::get_mut::<OpenModlog>(window, cx).0 = Some((community, conversation));
    cx.refresh_windows();
}

/// Closes `window`'s history.
pub fn close(window: WindowId, cx: &mut App) {
    crate::per_window::set::<OpenModlog>(window, None, cx);
    cx.refresh_windows();
}

/// What `window`'s open history shows, for tests and tools: its community and conversation.
pub fn open_of(window: WindowId, cx: &App) -> Option<(CommunityId, Conversation)> {
    crate::per_window::get::<OpenModlog>(window, cx).and_then(|o| o.0)
}

/// Lets go of `windows`' histories (`commands::release_windows`: Forget, sign-out, an instance's Remove).
pub fn release_windows(windows: &[WindowId], cx: &mut App) {
    for &window in windows {
        crate::per_window::set::<OpenModlog>(window, None, cx);
    }
}

/// The dialog over the window (`slots::overlays`), while one is open.
pub fn overlay(ctx: &SlotCx, window: &mut gpui::Window, cx: &mut App) -> Option<AnyElement> {
    let w = window.window_handle().window_id();
    let (community, conversation) = open_of(w, cx)?;
    // The window left the conversation the command was typed in: the dialog goes (CommandHost.tsx:130).
    if ctx.channel != Some(conversation.channel) {
        crate::per_window::set::<OpenModlog>(w, None, cx);
        return None;
    }
    let body = crate::screens::members::moderation_log_section(ctx, community, true, window, cx);
    Some(
        Dialog::new(
            "command-modlog-dialog",
            t("admin.bot.moderationLog.title"),
            t("dialog.close"),
        )
        .body(body)
        .on_close(move |window, cx| {
            close(w, cx);
            window.refresh();
        })
        .into_any_element(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::ChannelId;

    /// Decision D1: the history opens in the window that asked, over the conversation it was typed in; closing it, and a
    /// release of the window's client, end it, and another window's stays.
    #[gpui::test]
    fn the_history_is_per_window_and_ends_on_close_and_release(cx: &mut gpui::TestAppContext) {
        let id = |cx: &mut gpui::TestAppContext| {
            cx.add_empty_window()
                .update(|w, _| w.window_handle().window_id())
        };
        let (a, b) = (id(cx), id(cx));
        let asked = (
            CommunityId([1; 16]),
            Conversation {
                channel: ChannelId([2; 16]),
                thread: None,
            },
        );
        cx.update(|cx| {
            open(a, asked.0, asked.1, cx);
            open(b, asked.0, asked.1, cx);
            assert_eq!(open_of(a, cx), Some(asked));
            close(a, cx);
            assert_eq!(open_of(a, cx), None, "closed");
            assert_eq!(open_of(b, cx), Some(asked), "the other window's stays");
            release_windows(&[b], cx);
            assert_eq!(open_of(b, cx), None, "released");
        });
    }
}
