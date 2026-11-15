//! Area **server settings** (Task 15): the Overview, Channels, GIFs, Invites, Welcome, Join roles and Danger zone sections
//! (packages/web/src/screens/admin/ServerSettings.tsx and its sections), hosted in Task 6's settings frame.
//!
//! [`ServerSettingsView`] is the frame for one server: the nav (the person's own sections under "You", then this
//! server's, only those its permissions allow, [`model::nav_sections`]), the section on show, and the dirty-draft guard
//! ([`crate::edit_state::Workspace`]) for the Overview, the one section with a draft (its name and AFK timeout save
//! together from the footer). Every other section acts at once, as on the web. The sections other areas own (Members,
//! Roles, Self roles, Moderation log, Bans: Task 14; Bot and Music: Task 21) are drawn by their own hooks through
//! `crate::slots::settings_sections`.
//!
//! [`Body`] holds the sections' state and draws one section. The hook [`settings_section`] that `crate::slots` calls
//! hosts a `Body` of its own, for a shell that has its own frame; [`ServerSettingsView`] hosts one inside the frame.

mod body;
pub mod keys;
pub mod model;
mod view;

pub use body::{Body, BodyEvent};
pub use view::{ServerSettingsView, SettingsEvent, open_section};

use gpui::{AnyElement, App, Empty, IntoElement, Window};

use crate::slots::{SectionId, SlotCx};

/// Draws one of this area's settings sections (`slots::settings_sections` points here), editable, in a `Body` the window
/// keeps for the server the slot's context names.
pub fn settings_section(
    section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let SectionId::Server(section) = section else {
        return Empty.into_any_element();
    };
    let Some(community) = ctx.community else {
        return Empty.into_any_element();
    };
    let body = window.use_keyed_state(
        gpui::SharedString::from(format!("server-settings-body-{}", community.hex())),
        cx,
        |window, cx| {
            Body::new(
                community,
                ctx.state.clone(),
                ctx.client.clone(),
                ctx.clock,
                window,
                cx,
            )
        },
    );
    // Release lets go of this view's client at once (review M5), not when the window next draws.
    if let Some(client) = &ctx.client {
        crate::release::hold(
            client,
            &body,
            |b, cx| b.sync(std::sync::Arc::default(), None, cx),
            cx,
        );
    }
    body.update(cx, |b, cx| {
        b.sync(ctx.state.clone(), ctx.client.clone(), cx)
    });
    body.update(cx, |b, _| b.show(section));
    body.into_any_element()
}
