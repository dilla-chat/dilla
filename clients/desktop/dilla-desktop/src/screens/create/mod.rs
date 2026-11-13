//! Area **onboarding** (Task 25): the create server and create channel flows, join by invite, the invite link and the
//! add-server dialog (web `screens/{Create,JoinCommunity}.tsx`, ui `CreateServer`, `CreateChannel`, `AddServerDialog`),
//! and the shell's empty state ("You are not in a server yet").
//!
//! The dialogs live in one [`CreateFlows`] entity per window, drawn over the shell by [`overlay`] (the shell's
//! `overlays` slot). Any code with an `&mut App` opens one ([`open_add_server`], [`open_create_channel`], [`open`]); the
//! window's running client and where to go after a create or a join come from [`attach`], which the app calls when the
//! shell starts. A dialog opened before a client is attached (a fixture) draws, and its submit says so.
//!
//! Each flow sends one command at a time and says every refusal by its code, never by the instance's detail text.

mod flows;

use std::collections::HashMap;
use std::rc::Rc;

use dilla_client::Client;
use dilla_client::state::{ChannelId, CommunityId};
use gpui::{AnyElement, App, AppContext as _, Entity, Global, IntoElement, Window, WindowId};

use dilla_desktop_ui::components::empty_state::EmptyState;

pub use flows::{CreateFlows, Dialog};

use crate::slots::SlotCx;
use crate::strings::t;
use crate::tracked;

/// Where the shell goes after a flow: the server it created or joined, and the channel to open in it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Navigate {
    pub community: CommunityId,
    pub channel: Option<ChannelId>,
}

/// The shell's handler for [`Navigate`].
pub type OnNavigate = Rc<dyn Fn(Navigate, &mut Window, &mut App)>;

/// What a flow may open.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Open {
    /// The "+" tile's choice: join or create.
    AddServer,
    /// "Start a server".
    CreateServer,
    /// "Join a server", with an invite to show in its field (a pasted link handed in at launch, Task 27's
    /// `Launch.invite`, or the join a sign-up was refused).
    Join { invite: Option<String> },
    /// "New channel in {server}", inside the category `parent` (its header's "+") or at the top level.
    CreateChannel {
        community: CommunityId,
        parent: Option<ChannelId>,
    },
    /// "Invite people to {server}".
    Invite { community: CommunityId },
}

/// The window's flows, its client and its navigation, by window.
#[derive(Default)]
struct Windows {
    flows: HashMap<WindowId, Entity<CreateFlows>>,
    pending: HashMap<WindowId, Open>,
    attached: HashMap<WindowId, (Rc<Client>, OnNavigate)>,
}

impl Global for Windows {}

fn windows(cx: &mut App) -> &mut Windows {
    cx.default_global::<Windows>()
}

/// Opens `which` in the active window (on its next frame).
pub fn open(which: Open, cx: &mut App) {
    let Some(window) = cx.active_window() else {
        return;
    };
    windows(cx).pending.insert(window.window_id(), which);
    cx.refresh_windows();
}

/// Opens the create-channel flow for `community`. The channel list's "+" (Task 8) calls it. Fixed signature.
pub fn open_create_channel(community: CommunityId, cx: &mut App) {
    open(
        Open::CreateChannel {
            community,
            parent: None,
        },
        cx,
    );
}

/// Opens the add-server dialog ("join a server" or "new server"): the rail's "+" (Task 8) calls it.
pub fn open_add_server(cx: &mut App) {
    open(Open::AddServer, cx);
}

/// Opens the join dialog with `invite` in its field (Task 27's pasted invite, the GPUI form of `/welcome?invite=`).
pub fn open_join(invite: Option<String>, cx: &mut App) {
    open(Open::Join { invite }, cx);
}

/// The window's running client and the shell's navigation, for the flows' submits. The app calls it when the shell
/// starts; a later call replaces it (a client restarted after a revocation).
pub fn attach(window: &Window, client: Rc<Client>, on_navigate: OnNavigate, cx: &mut App) {
    let id = window.window_handle().window_id();
    windows(cx)
        .attached
        .insert(id, (client.clone(), on_navigate.clone()));
    if let Some(flows) = windows(cx).flows.get(&id).cloned() {
        flows.update(cx, |f, _| f.attach(client, on_navigate));
    }
}

/// The window's flows entity, made on first use.
fn flows_of(window: &mut Window, cx: &mut App) -> Entity<CreateFlows> {
    let id = window.window_handle().window_id();
    if let Some(flows) = windows(cx).flows.get(&id) {
        return flows.clone();
    }
    let attached = windows(cx).attached.get(&id).cloned();
    let flows = cx.new(|cx| CreateFlows::new(attached, window, cx));
    windows(cx).flows.insert(id, flows.clone());
    flows
}

/// The open dialog, drawn last in the window's root (the shell's `overlays` slot). Draws nothing while none is open.
pub fn overlay(window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let id = window.window_handle().window_id();
    let pending = windows(cx).pending.remove(&id);
    let has = windows(cx).flows.contains_key(&id);
    if pending.is_none() && !has {
        return None;
    }
    let flows = flows_of(window, cx);
    if let Some(which) = pending {
        flows.update(cx, |f, cx| f.open(which, window, cx));
    }
    flows.read(cx).is_open().then(|| flows.into_any_element())
}

/// The dialog open in `window`, if any.
pub fn current(window: &Window, cx: &App) -> Option<Dialog> {
    cx.try_global::<Windows>()?
        .flows
        .get(&window.window_handle().window_id())?
        .read(cx)
        .dialog()
}

/// The shell's main column when the account is in no server (Shell.tsx `noServerState`): "Start a server" and "Join a
/// server".
pub fn empty_view(_ctx: &SlotCx, _window: &mut Window, _cx: &mut App) -> AnyElement {
    tracked(
        "create-empty",
        EmptyState::new(
            "create-empty",
            t("shell.noServer.title"),
            t("shell.noServer.body"),
        )
        .action(t("shell.noServer.create"), |_, _, cx| {
            open(Open::CreateServer, cx)
        })
        .secondary(t("shell.noServer.action"), |_, _, cx| {
            open(Open::Join { invite: None }, cx)
        }),
    )
    .into_any_element()
}
