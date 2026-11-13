//! What the messages area keeps between frames: the row being edited, the delete asked about, the open "more" menu, the
//! pins dialog and the row a jump flashes. The timeline is a function of the client's state, so this lives in one keyed
//! entity of the window ([`ui`]); a message action (a closure the toolbar calls) reaches it through [`handle`].

use std::rc::Rc;
use std::time::{Duration, Instant};

use dilla_client::Client;
use dilla_client::state::{ChannelId, MessageId, TimelineItem};
use gpui::{
    App, AppContext as _, Context, Entity, EntityInputHandler as _, Focusable as _, Subscription,
    Window,
};
use gpui_component::input::{InputEvent, TextareaState};

use super::mentions::{
    MentionMember, MentionRole, RoleInfo, edit_roles, editable_text, encode_mentions, mention_roles,
};
use crate::slots::SlotCx;

/// The message editor's budget (MessageEditor `maxLength`).
pub const EDIT_MAX: usize = 4000;
/// How long a jump's flash lasts (`--duration-flash`).
pub const FLASH: Duration = Duration::from_millis(1400);

/// The row being edited.
pub struct Editing {
    pub key: String,
    pub channel: ChannelId,
    pub msg: MessageId,
    /// Set from the item when the editor opened; only an own message gets one.
    pub own: bool,
    pub input: Entity<TextareaState>,
    /// `None` while a fixture is drawn: saving then only closes the editor.
    client: Option<Rc<Client>>,
    members: Vec<MentionMember>,
    roles: Vec<MentionRole>,
    broadcast: bool,
    _subscription: Subscription,
}

impl Editing {
    /// The text as it will be sent: names encoded back to their tokens.
    pub fn encoded(&self, cx: &App) -> String {
        encode_mentions(
            &self.input.read(cx).value(),
            &self.members,
            self.broadcast,
            &self.roles,
        )
    }

    /// The budget used, in UTF-8 bytes of the encoded text (the web's `measure`).
    pub fn used(&self, cx: &App) -> usize {
        self.encoded(cx).len()
    }
}

/// A delete waiting for its confirmation.
#[derive(Clone, Copy)]
pub struct Deleting {
    pub channel: ChannelId,
    pub msg: MessageId,
}

#[derive(Default)]
pub struct MessageUi {
    pub editing: Option<Editing>,
    pub deleting: Option<Deleting>,
    /// The row whose "more" menu is open.
    pub menu: Option<String>,
    /// The channel whose pins dialog is open.
    pub pins: Option<ChannelId>,
    /// The row a jump flashes, and since when.
    pub flash: Option<(String, Instant)>,
    /// A row the log still has to scroll into view (a jump from the pins or a reply).
    pub jump: Option<String>,
    /// What the timeline was last drawn with: the dialogs (drawn over the whole window by the shell's overlays) read it.
    pub ctx: Option<SlotCx>,
    /// The log's focus: PageUp and PageDown act while it holds the focus.
    pub log_focus: Option<gpui::FocusHandle>,
}

/// The window's message state, created on first use. Notifying it redraws the view that drew the timeline.
pub fn ui(window: &mut Window, cx: &mut App) -> Entity<MessageUi> {
    let e = window.use_keyed_state("messages-ui", cx, |_, _| MessageUi::default());
    let id = window.window_handle().window_id();
    if crate::per_window::get::<UiHandle>(id, cx).is_none_or(|h| h.0.entity_id() != e.entity_id()) {
        crate::per_window::set(id, Some(UiHandle(e.clone())), cx);
    }
    e
}

/// Each window's message state (final review sweep: per window; release walks them all).
#[derive(Clone)]
struct UiHandle(Entity<MessageUi>);

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client`): the context the dialogs read and an open
/// editor that holds it go.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    let mut handles = Vec::new();
    crate::per_window::each_mut::<UiHandle>(cx, |_, h| handles.push(h.clone()));
    let holds = |c: &Option<Rc<Client>>| c.as_ref().is_some_and(|c| Rc::ptr_eq(c, client));
    for handle in handles {
        handle.0.update(cx, |ui, cx| {
            if ui.ctx.as_ref().is_some_and(|ctx| holds(&ctx.client)) {
                ui.ctx = None;
                ui.pins = None;
                ui.deleting = None;
                ui.menu = None;
            }
            if ui.editing.as_ref().is_some_and(|e| holds(&e.client)) {
                ui.editing = None;
            }
            cx.notify();
        });
    }
}

/// The state of `window`'s log that a closure of an action reaches (`None` before its timeline's first frame).
pub fn handle(window: &Window, cx: &App) -> Option<Entity<MessageUi>> {
    crate::per_window::get::<UiHandle>(window.window_handle().window_id(), cx).map(|h| h.0.clone())
}

impl MessageUi {
    /// Whether the row `key` is the flashing one.
    pub fn flashing(&self, key: &str) -> bool {
        self.flash
            .as_ref()
            .is_some_and(|(k, at)| k == key && at.elapsed() < FLASH)
    }

    /// Starts editing `item`: the field holds the text as `@names`, focused with the caret at its end.
    pub fn begin_edit(
        &mut self,
        ctx: &SlotCx,
        item: &TimelineItem,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        // Only the own message can be edited (the instance refuses another's fold, and a foreign row must never show an editor).
        if !item.own {
            return;
        }
        let (Some(channel), Some(msg)) = (ctx.channel, item.msg_id) else {
            return;
        };
        let client = ctx.client.clone();
        let state = &ctx.state;
        let community = state.channel(channel).map(|c| c.community_id);
        let members = community
            .and_then(|c| state.members.get(&c))
            .map(Vec::as_slice)
            .unwrap_or_default();
        let roles: Vec<RoleInfo> = Vec::new();
        // T14: the right to mention @everyone and @here comes from the channel's permission bits; until the roles slice
        // exists nobody is offered it, so an edit never turns text into a broadcast.
        let broadcast = false;
        let self_user = state.account.user_id.map(|u| u.hex());
        let text = editable_text(
            &item.body,
            members,
            self_user
                .as_deref()
                .map(|id| (id, state.account.username.as_str())),
            &roles,
        );
        let usernames: Vec<&str> = members.iter().map(|m| m.username.as_str()).collect();
        let offered = edit_roles(&roles, broadcast, &item.body);
        let input = cx.new(|cx| TextareaState::new(window, cx).submit_on_enter(true));
        input.update(cx, |s, cx| s.set_value(text, window, cx));
        let focus = input.read(cx).focus_handle(cx);
        window.focus(&focus, cx);
        let sub = cx.subscribe_in(&input, window, Self::on_edit_event);
        self.menu = None;
        self.editing = Some(Editing {
            key: item.key.clone(),
            channel,
            msg,
            own: item.own,
            input,
            client,
            members: members
                .iter()
                .filter(|m| !m.username.is_empty())
                .map(|m| MentionMember {
                    user_id: m.user_id.hex(),
                    username: m.username.clone(),
                })
                .collect(),
            roles: mention_roles(&offered, &usernames),
            broadcast,
            _subscription: sub,
        });
        cx.notify();
    }

    fn on_edit_event(
        &mut self,
        input: &Entity<TextareaState>,
        event: &InputEvent,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        match event {
            InputEvent::Change => cx.notify(),
            InputEvent::PressEnter { shift: false, .. } => {
                // Enter during an IME composition confirms it and never saves (the web's `isComposing`).
                let composing = input
                    .update(cx, |s, cx| s.marked_text_range(window, cx))
                    .is_some();
                if !composing {
                    self.save_edit(cx);
                }
            }
            _ => {}
        }
    }

    /// Whether the edit may be saved: not blank, not over the budget.
    pub fn can_save(&self, cx: &App) -> bool {
        self.editing
            .as_ref()
            .is_some_and(|e| !e.input.read(cx).value().trim().is_empty() && e.used(cx) <= EDIT_MAX)
    }

    /// Saves the edit: the fold is queued and the editor closes (the row changes when the core folds it).
    pub fn save_edit(&mut self, cx: &mut Context<Self>) {
        // Own messages only (begin_edit refuses the rest; checked again here against the timeline the editor was opened on).
        if !self.can_save(cx) || !self.editing.as_ref().is_some_and(|e| e.own) {
            return;
        }
        let Some(e) = self.editing.take() else { return };
        let text = e.encoded(cx);
        if let Some(client) = &e.client {
            let done = client.messages().edit(e.channel, e.msg, text);
            cx.background_spawn(async move {
                let _ = done.await;
            })
            .detach();
        }
        cx.notify();
    }

    /// Escape: the editor closes and nothing changes.
    pub fn cancel_edit(&mut self, cx: &mut Context<Self>) {
        if self.editing.take().is_some() {
            cx.notify();
        }
    }

    pub fn ask_delete(&mut self, channel: ChannelId, msg: MessageId, cx: &mut Context<Self>) {
        self.menu = None;
        self.deleting = Some(Deleting { channel, msg });
        cx.notify();
    }

    pub fn cancel_delete(&mut self, cx: &mut Context<Self>) {
        self.deleting = None;
        cx.notify();
    }

    pub fn set_menu(&mut self, key: Option<String>, cx: &mut Context<Self>) {
        if self.menu != key {
            self.menu = key;
            cx.notify();
        }
    }

    pub fn open_pins(&mut self, channel: ChannelId, cx: &mut Context<Self>) {
        self.pins = Some(channel);
        cx.notify();
    }

    pub fn close_pins(&mut self, cx: &mut Context<Self>) {
        self.pins = None;
        cx.notify();
    }

    /// A jump to `key`: the log scrolls it into view and the row flashes the accent tint for 1.4 s.
    pub fn jump_to(&mut self, key: String, cx: &mut Context<Self>) {
        self.flash = Some((key.clone(), Instant::now()));
        self.jump = Some(key);
        cx.notify();
    }
}

/// Opens the pins dialog of `channel` (the channel header's pin button calls this).
pub fn open_pins(channel: ChannelId, client: Option<&Rc<Client>>, window: &Window, cx: &mut App) {
    if let Some(client) = client {
        let loaded = client.messages().load_pins(channel);
        cx.background_spawn(async move {
            let _ = loaded.await;
        })
        .detach();
    }
    if let Some(ui) = handle(window, cx) {
        ui.update(cx, |ui, cx| ui.open_pins(channel, cx));
    }
}
