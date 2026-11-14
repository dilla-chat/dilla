//! The moderation commands' confirmation (Task 28 wave 2, lane B): CommandHost.tsx's moderation `Dialog`. `/kick` and
//! `/ban` ([`dilla_client::features::bot::Plan::Moderate`]) ask before anything is sent: the person's avatar, "Kick Bea
//! from Midgard?", what it does, the duration, the reason and where it is logged, Cancel and the danger action.
//!
//! - The action: a pending private row, the kick or ban, and the row says how it ended ([`Bot::moderate`]).
//! - Cancel (the close button, Escape): a "cancelled" row ("Kick cancelled. Nothing changed."); nothing is sent.
//! - A click outside (the web's `abandonDialog`): the dialog goes and nothing is recorded.
//!
//! [`Bot::moderate`]: dilla_client::features::bot::Bot::moderate

use dilla_client::Client;
use dilla_client::features::bot::{ModAction, Moderation, ModerationTexts};
use dilla_client::state::bot::Conversation;
use gpui::{AnyElement, App, IntoElement, ParentElement, Styled, Window, div, px};

use dilla_desktop_ui::components::avatar::{Avatar, AvatarSize};
use dilla_desktop_ui::components::button::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::components::dialog::{Dialog, DialogKind, InitialFocus};
use dilla_desktop_ui::text::{line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, radius, text};

use super::copy;
use crate::slots::SlotCx;
use crate::strings::{t, tf};

/// CommandHost.css `.dw-command-confirm`.
const CONFIRM_PAD_Y: f32 = 10.0;
const CONFIRM_PAD_X: f32 = 12.0;
const CONFIRM_ROWS_GAP: f32 = 4.0;
const CONFIRM_ROW_GAP: f32 = 10.0;
const CONFIRM_LABEL_W: f32 = 64.0;

/// A moderation command waiting for its answer, in one window.
#[derive(Clone)]
pub struct Confirming {
    pub moderation: Moderation,
    pub conversation: Conversation,
    pub client: std::rc::Rc<Client>,
}

#[derive(Default)]
struct Open(Option<Confirming>);

/// `window`'s command asks for confirmation (the composer's send of a `/kick` or `/ban`).
pub fn ask(window: gpui::WindowId, confirming: Confirming, cx: &mut App) {
    crate::per_window::get_mut::<Open>(window, cx).0 = Some(confirming);
}

/// Lets go of `client` (Forget, sign-out, an instance's Remove: `crate::release::release_client`, final review I4): a
/// confirmation it asks in any window goes, unanswered (nothing is sent or recorded), so the store can close though the
/// window does not draw again.
pub fn release(client: &std::rc::Rc<Client>, cx: &mut App) {
    crate::per_window::each_mut::<Open>(cx, |_, open| {
        if open
            .0
            .as_ref()
            .is_some_and(|c| std::rc::Rc::ptr_eq(&c.client, client))
        {
            open.0 = None;
        }
    });
}

/// Whether `window` shows the confirmation.
pub fn is_open(window: gpui::WindowId, cx: &App) -> bool {
    crate::per_window::get::<Open>(window, cx).is_some_and(|o| o.0.is_some())
}

/// The person answered: `Some(true)` the action, `Some(false)` Cancel, `None` a click outside (nothing recorded).
pub fn answer(window: gpui::WindowId, choice: Option<bool>, cx: &mut App) {
    let Some(c) = crate::per_window::get_mut::<Open>(window, cx).0.take() else {
        return;
    };
    let Some(confirmed) = choice else {
        return;
    };
    let id = c.moderation.action.id();
    let texts = ModerationTexts {
        cancelled: t(&format!("privateResult.cancelled.{id}")).to_owned(),
        pending: copy::pending(id),
        done: tf(&format!("mod.{id}.done"), &[("name", &c.moderation.name)]),
    };
    c.client
        .bot()
        .moderate(c.conversation, c.moderation, confirmed, texts);
}

/// Whether the conversation a moderation was typed in is still the window's (the web's `isCurrent`).
fn still_current(
    conversation: &Conversation,
    channel: Option<dilla_client::state::ChannelId>,
) -> bool {
    channel == Some(conversation.channel)
}

/// The member's `@username` the dialog shows under the title.
pub fn handle(m: &Moderation) -> String {
    format!("@{}", m.username)
}

/// The dialog over the window (`slots::overlays`), while one is asked for.
pub fn overlay(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let w = window.window_handle().window_id();
    let c = crate::per_window::get::<Open>(w, cx)?.0.clone()?;
    // w2-bot-review M-B3 (CommandHost.tsx `isCurrent()`, else 'abandon'): the person left the conversation the command
    // was typed in; the dialog goes and nothing is sent or recorded.
    if !still_current(&c.conversation, ctx.channel) {
        answer(w, None, cx);
        return None;
    }
    let m = &c.moderation;
    let server = ctx
        .state
        .communities
        .iter()
        .find(|s| s.id == m.community)
        .map_or(copy::SERVER.to_owned(), |s| s.name.clone());
    let id = m.action.id();
    let (body, duration, reason) = match &m.action {
        ModAction::Kick => (copy::kick_body(&m.name), "—".to_owned(), None),
        ModAction::Ban {
            duration, reason, ..
        } => (
            // CommandHost.tsx: the length as the chips suggested it (`1d` is "1 day").
            copy::ban_body(&m.name, &super::chips::suggestion_of(duration)),
            super::chips::suggestion_of(duration),
            Some(if reason.is_empty() {
                copy::NO_REASON.to_owned()
            } else {
                reason.clone()
            }),
        ),
    };
    // The body is the head's 12px --fg-2 line, beside the avatar (ConfirmDialog's measure). Above it the member's
    // `@username`, so two members with one name are told apart (w2-bot-review I-B1).
    let description = div()
        .flex()
        .flex_col()
        .child(line(
            handle(m),
            text::UI,
            line_height(text::UI, 1.5),
            color::FG_3,
        ))
        .children(paragraph(
            &body,
            text::UI,
            line_height(text::UI, 1.5),
            color::FG_2,
            440.0 - 36.0 - 54.0,
        ));
    // CommandHost.css `.dw-command-confirm`: a hairline box, `padding: 10px 12px`, rows 4px apart in `--text-btn`, each
    // a 64px `--fg-3` label 10px before its value.
    let row = |label: &str, value: String| {
        div()
            .flex()
            .gap(px(CONFIRM_ROW_GAP))
            .child(div().w(px(CONFIRM_LABEL_W)).flex_none().child(line(
                label.to_owned(),
                text::BTN,
                line_height(text::BTN, 1.5),
                color::FG_3,
            )))
            .child(line(
                value,
                text::BTN,
                line_height(text::BTN, 1.5),
                color::FG,
            ))
    };
    let mut rows = div()
        .flex()
        .flex_col()
        .gap(px(CONFIRM_ROWS_GAP))
        .py(px(CONFIRM_PAD_Y))
        .px(px(CONFIRM_PAD_X))
        .border_1()
        .border_color(color::HAIRLINE)
        .rounded(radius::LG)
        .child(row(copy::DURATION, duration));
    if let Some(reason) = reason {
        rows = rows.child(row(copy::REASON, reason));
    }
    rows = rows.child(row(copy::LOGGED, copy::LOGGED_NOTE.to_owned()));
    let confirm = Button::new("command-confirm", copy::action(id))
        .variant(ButtonVariant::Danger)
        .size(ButtonSize::Dialog)
        .on_click(move |_, window, cx| {
            answer(w, Some(true), cx);
            window.refresh();
        });
    Some(
        Dialog::new(
            "command-confirm-dialog",
            copy::moderation_title(copy::action(id), &m.name, &server),
            t("chperm.cancel"),
        )
        .kind(DialogKind::Confirm)
        .lead(
            div().flex_none().child(
                Avatar::new("command-confirm-person", m.name.clone()).size(AvatarSize::Confirm),
            ),
        )
        .description(description)
        .body(rows)
        // Tracked, so a pointer can find it (the tests' and the automation build's bounds).
        .footer(crate::tracked_box("command-confirm", confirm))
        .initial_focus(InitialFocus::Close)
        .on_close(move |window, cx| {
            answer(w, Some(false), cx);
            window.refresh();
        })
        .on_backdrop(move |window, cx| {
            answer(w, None, cx);
            window.refresh();
        })
        .into_any_element(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::{CommunityId, UserId};

    /// w2-bot-review I-B1: the dialog names the member by `@username` too, so two members called "Alex" are told apart.
    #[test]
    fn the_dialog_names_the_member_by_username() {
        let m = Moderation {
            community: CommunityId([7; 16]),
            user: UserId([3; 16]),
            name: "Alex".into(),
            username: "troll".into(),
            action: ModAction::Kick,
        };
        assert_eq!(handle(&m), "@troll");
    }

    /// w2-bot-review M-B3: a confirmation is answered only while its conversation is the window's; after a switch it is
    /// abandoned (nothing sent, nothing recorded).
    #[test]
    fn a_confirmation_is_abandoned_once_its_conversation_is_left() {
        use dilla_client::state::ChannelId;
        let here = Conversation {
            channel: ChannelId([1; 16]),
            thread: None,
        };
        assert!(still_current(&here, Some(ChannelId([1; 16]))));
        assert!(!still_current(&here, Some(ChannelId([2; 16]))));
        assert!(!still_current(&here, None));
    }
}
