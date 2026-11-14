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
use dilla_desktop_ui::tokens::{color, text};

use super::copy;
use crate::slots::SlotCx;
use crate::strings::{t, tf};

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

/// The dialog over the window (`slots::overlays`), while one is asked for.
pub fn overlay(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let w = window.window_handle().window_id();
    let c = crate::per_window::get::<Open>(w, cx)?.0.clone()?;
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
            copy::ban_body(&m.name, duration),
            duration.clone(),
            Some(if reason.is_empty() {
                copy::NO_REASON.to_owned()
            } else {
                reason.clone()
            }),
        ),
    };
    // The body is the head's 12px --fg-2 line, beside the avatar (ConfirmDialog's measure).
    let description = div().flex().flex_col().children(paragraph(
        &body,
        text::UI,
        line_height(text::UI, 1.5),
        color::FG_2,
        440.0 - 36.0 - 54.0,
    ));
    // CommandHost.tsx `.dw-command-confirm`: label and value rows.
    let row = |label: &str, value: String| {
        div()
            .flex()
            .gap(px(8.0))
            .child(div().w(px(72.0)).flex_none().child(line(
                label.to_owned(),
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_3,
            )))
            .child(line(value, text::UI, line_height(text::UI, 1.5), color::FG))
    };
    let mut rows = div()
        .flex()
        .flex_col()
        .gap(px(4.0))
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
