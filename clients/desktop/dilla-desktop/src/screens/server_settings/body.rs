//! The sections of the server settings: their state ([`Body`]) and how each draws and acts. One [`Body`] belongs to one
//! server; it shows one section at a time ([`Body::show`]).
//!
//! Against the web: the Overview keeps a draft (name and AFK timeout, saved together by the frame's footer); every other
//! section acts at once and keeps no draft. Every request goes through `dilla_client::features::server_admin`; a
//! refusal reaches the section as its code and status ([`AdminError`]) and is worded as the web words it. With no client
//! (a fixture) a section only draws.

use std::cell::RefCell;
use std::rc::Rc;
use std::sync::Arc;

use dilla_client::Client;
use dilla_client::features::server_admin::{
    AFK_DEFAULT_TIMEOUT, AdminError, CHANNEL_NAME_MAX_BYTES, COMMUNITY_NAME_MAX_BYTES, Direction,
    NameProblem, Parent, Reply, TOPIC_MAX_BYTES, gif_key_valid, name_problem, nfc,
};
use dilla_client::state::server_admin::{GifRating, InviteId};
use dilla_client::state::{AppState, Channel, ChannelId, CommunityId, UserId};
use gpui::{
    AnyElement, AppContext as _, Context, Entity, EventEmitter, IntoElement, ParentElement, Render,
    SharedString, Styled, Subscription, Window, div,
};
use gpui_component::input::{InputEvent, InputState};

use super::model;
mod draw;
use crate::router::ServerSection;
use crate::screens::shell::Clock;
use crate::strings::{t, tf};

/// What a section tells its host.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum BodyEvent {
    /// A success to say aloud (the shell's polite announcer).
    Announce(String),
    /// The server was left, deleted or taken away: the settings close.
    Gone,
    /// A section asks the host to open another (Welcome's "Open Bot settings").
    OpenSection(ServerSection),
}

impl EventEmitter<BodyEvent> for Body {}

/// The Overview's draft (OverviewSection.tsx): the name and the AFK timeout, with their baselines, which stay independent
/// so a partial save retries only what failed.
#[derive(Clone, Debug)]
pub struct OverviewDraft {
    pub name_base: String,
    pub name: String,
    pub timeout: u64,
    /// `None` until the read lands.
    pub timeout_base: Option<u64>,
    pub busy: bool,
    pub error: Option<String>,
    /// The timeout's own save failed (its message sits under the timeout, not under the name).
    pub afk_failure: bool,
    /// Set by the workspace's `save`; [`Body::flush_save`] sends it.
    pub save_requested: bool,
    pub can_rename: bool,
}

impl OverviewDraft {
    fn new(name: &str) -> Self {
        Self {
            name_base: name.to_owned(),
            name: name.to_owned(),
            timeout: AFK_DEFAULT_TIMEOUT,
            timeout_base: None,
            busy: false,
            error: None,
            afk_failure: false,
            save_requested: false,
            can_rename: false,
        }
    }

    pub fn name_changed(&self) -> bool {
        self.can_rename && nfc(&self.name) != nfc(&self.name_base)
    }

    pub fn timeout_changed(&self) -> bool {
        self.timeout_base.is_some_and(|b| b != self.timeout)
    }

    pub fn dirty(&self) -> bool {
        self.name_changed() || self.timeout_changed()
    }

    /// Throws the draft away.
    pub fn reset(&mut self) {
        self.name = self.name_base.clone();
        if let Some(base) = self.timeout_base {
            self.timeout = base;
        }
        self.error = None;
        self.afk_failure = false;
    }
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum Dialogue {
    None,
    Edit(ChannelId),
    Delete(ChannelId),
    Category,
    Leave,
    DeleteServer,
    Transfer,
    PrivateAsk(ChannelId, bool),
}

/// A link made and not yet copied (shown once).
struct Made {
    url: String,
    note: String,
}

pub struct Body {
    community: CommunityId,
    state: Arc<AppState>,
    client: Option<Rc<Client>>,
    clock: Clock,
    section: ServerSection,
    pub overview: Rc<RefCell<OverviewDraft>>,
    name: Entity<InputState>,
    // The dialogs: one at a time, with their one action's state.
    dialogue: Dialogue,
    dialogue_busy: bool,
    dialogue_error: Option<SharedString>,
    edit_name: Entity<InputState>,
    edit_topic: Entity<InputState>,
    edit_parent: String,
    edit_slow: String,
    category_name: Entity<InputState>,
    typed: Entity<InputState>,
    transfer_to: Option<UserId>,
    // The Channels list.
    move_error: Option<SharedString>,
    moving: bool,
    // The overview's leave note for an owner.
    owner_note: bool,
    // GIFs.
    gif_key: Entity<InputState>,
    gif_editing: bool,
    gif_busy: bool,
    gif_error: Option<SharedString>,
    // Invites.
    ttl: u64,
    uses: u64,
    made: Option<Made>,
    making: bool,
    invite_message: Option<(SharedString, bool)>,
    invite_load_error: Option<SharedString>,
    busy_invite: Option<InviteId>,
    host: String,
    requested: Vec<ServerSection>,
    _subscriptions: Vec<Subscription>,
}

fn community_name(state: &AppState, community: CommunityId) -> String {
    state
        .communities
        .iter()
        .find(|c| c.id == community)
        .map_or_else(String::new, |c| c.name.clone())
}

impl Body {
    pub fn new(
        community: CommunityId,
        state: Arc<AppState>,
        client: Option<Rc<Client>>,
        clock: Clock,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let field =
            |placeholder: &str, masked: bool, window: &mut Window, cx: &mut Context<Self>| {
                let placeholder = placeholder.to_owned();
                cx.new(|cx| {
                    InputState::new(window, cx)
                        .placeholder(placeholder)
                        .masked(masked)
                })
            };
        let name = field("", false, window, cx);
        let initial = community_name(&state, community);
        name.update(cx, |s, cx| s.set_value(initial.clone(), window, cx));
        let overview = Rc::new(RefCell::new(OverviewDraft::new(&initial)));
        let edit_name = field("", false, window, cx);
        let edit_topic = field("", false, window, cx);
        let category_name = field("", false, window, cx);
        let typed = field("", false, window, cx);
        let gif_key = field("", true, window, cx);
        let mut subscriptions = Vec::new();
        {
            let draft = overview.clone();
            subscriptions.push(cx.subscribe_in(
                &name,
                window,
                move |this: &mut Self, input, event: &InputEvent, _window, cx| {
                    if matches!(event, InputEvent::Change) {
                        let value = input.read(cx).value().to_string();
                        let mut d = draft.borrow_mut();
                        if d.name != value {
                            d.name = value;
                            d.error = None;
                            d.afk_failure = false;
                        }
                        drop(d);
                        this.notify_flags(cx);
                    }
                },
            ));
        }
        for input in [&edit_name, &edit_topic, &category_name, &typed, &gif_key] {
            subscriptions.push(cx.subscribe_in(
                input,
                window,
                |this: &mut Self, _, event: &InputEvent, _window, cx| {
                    if matches!(event, InputEvent::Change) {
                        this.dialogue_error = None;
                        this.gif_error = None;
                        cx.notify();
                    }
                },
            ));
        }
        let host = "dilla.example".to_owned();
        let mut this = Self {
            community,
            state,
            client,
            clock,
            section: ServerSection::Overview,
            overview,
            name,
            dialogue: Dialogue::None,
            dialogue_busy: false,
            dialogue_error: None,
            edit_name,
            edit_topic,
            edit_parent: "top".into(),
            edit_slow: "0".into(),
            category_name,
            typed,
            transfer_to: None,
            move_error: None,
            moving: false,
            owner_note: false,
            gif_key,
            gif_editing: false,
            gif_busy: false,
            gif_error: None,
            ttl: model::DEFAULT_TTL_S,
            uses: model::DEFAULT_USES,
            made: None,
            making: false,
            invite_message: None,
            invite_load_error: None,
            busy_invite: None,
            host,
            requested: Vec::new(),
            _subscriptions: subscriptions,
        };
        this.load_access(cx);
        this
    }

    /// The window's newest state and client. A renamed server shows its name in the Overview unless it is being edited.
    pub fn sync(
        &mut self,
        state: Arc<AppState>,
        client: Option<Rc<Client>>,
        cx: &mut Context<Self>,
    ) {
        let new_name = community_name(&state, self.community);
        let access = state
            .server_admin
            .access
            .get(&self.community)
            .copied()
            .unwrap_or_default();
        let afk = state.server_admin.afk.get(&self.community).copied();
        self.state = state;
        if self.client.is_none() && client.is_some() {
            self.client = client;
            self.load_access(cx);
        }
        let mut d = self.overview.borrow_mut();
        d.can_rename = access.loaded && access.manage_community;
        if d.name_base != new_name {
            let was_untouched = d.name == d.name_base;
            d.name_base = new_name.clone();
            if was_untouched {
                d.name = new_name;
            }
        }
        if let Some(afk) = afk {
            let edited = d.timeout_changed();
            d.timeout_base = Some(afk.timeout_seconds);
            if !edited {
                d.timeout = afk.timeout_seconds;
            }
        }
    }

    /// The section on show; its data is read the first time it shows.
    pub fn show(&mut self, section: ServerSection) {
        if self.section != section {
            self.section = section;
            self.dialogue = Dialogue::None;
            self.owner_note = false;
        }
    }

    pub fn current(&self) -> ServerSection {
        self.section
    }

    fn notify_flags(&mut self, cx: &mut Context<Self>) {
        cx.notify();
    }

    fn access(&self) -> dilla_client::state::server_admin::Access {
        self.state
            .server_admin
            .access
            .get(&self.community)
            .copied()
            .unwrap_or_default()
    }

    fn server_name(&self) -> String {
        community_name(&self.state, self.community)
    }

    fn channels(&self) -> Vec<&Channel> {
        self.state
            .channels
            .iter()
            .filter(|c| c.community_id == self.community)
            .collect()
    }

    fn afk_channel(&self) -> Option<ChannelId> {
        self.state
            .server_admin
            .afk
            .get(&self.community)
            .and_then(|a| a.channel)
    }

    // ---- requests ----

    /// Runs `reply` and hands its answer to `then` on the UI thread.
    fn run<T: 'static>(
        &mut self,
        reply: Reply<T>,
        window: &mut Window,
        cx: &mut Context<Self>,
        then: impl FnOnce(&mut Self, Result<T, AdminError>, &mut Window, &mut Context<Self>) + 'static,
    ) {
        cx.spawn_in(window, async move |this, cx| {
            let result = reply.wait().await;
            let _ = this.update_in(cx, |this, window, cx| then(this, result, window, cx));
        })
        .detach();
    }

    fn load_access(&mut self, cx: &mut Context<Self>) {
        let Some(client) = self.client.clone() else {
            return;
        };
        let reply = client.server_admin().load_access(self.community);
        let afk = client.server_admin().load_afk(self.community);
        cx.spawn(async move |_, _| {
            let _ = reply.wait().await;
            let _ = afk.wait().await;
        })
        .detach();
    }

    /// Reads what `section` shows, once per opening.
    pub fn load_for(
        &mut self,
        section: ServerSection,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(client) = self.client.clone() else {
            return;
        };
        let admin = client.server_admin();
        let community = self.community;
        match section {
            ServerSection::Gifs => {
                let reply = admin.load_gifs(community);
                self.run(reply, window, cx, |_, _, _, cx| cx.notify());
            }
            ServerSection::Invites => {
                self.invite_load_error = None;
                let reply = admin.load_invites(community, self.clock.now());
                self.run(reply, window, cx, |this, result, _, cx| {
                    if let Err(e) = result {
                        this.invite_load_error =
                            Some(tf("admin.invites.error.load", &[("code", &e.code)]).into());
                    }
                    cx.notify();
                });
            }
            ServerSection::Channels => {
                let reply = admin.load_access(community);
                self.run(reply, window, cx, |_, _, _, cx| cx.notify());
            }
            _ => {}
        }
    }

    /// Opens a dialog that asks for the server's name typed again (hand over, delete).
    fn open_typed(&mut self, dialogue: Dialogue, window: &mut Window, cx: &mut Context<Self>) {
        self.typed.update(cx, |s, cx| s.set_value("", window, cx));
        self.transfer_to = None;
        self.dialogue_error = None;
        self.dialogue_busy = false;
        self.dialogue = dialogue;
        cx.notify();
    }

    fn say(&mut self, message: String, cx: &mut Context<Self>) {
        cx.emit(BodyEvent::Announce(message));
    }

    /// Sends the Overview's save (the footer's Save): the name first, then the timeout; a partial save retries only what
    /// failed (OverviewSection.tsx `save`).
    pub fn flush_save(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let (rename, timeout) = {
            let mut d = self.overview.borrow_mut();
            if !d.save_requested {
                return;
            }
            d.save_requested = false;
            if d.busy || !d.dirty() {
                return;
            }
            if d.name_changed() {
                let max = COMMUNITY_NAME_MAX_BYTES;
                if let Some(problem) = name_problem(&d.name, max) {
                    d.error = Some(
                        match problem {
                            NameProblem::Empty => t("admin.overview.error.empty"),
                            NameProblem::Long => t("admin.overview.error.long"),
                            NameProblem::Control => t("admin.overview.error.refused"),
                        }
                        .to_owned(),
                    );
                    cx.notify();
                    return;
                }
            }
            d.error = None;
            d.afk_failure = false;
            d.busy = true;
            (
                d.name_changed().then(|| d.name.trim().to_owned()),
                d.timeout_changed().then_some(d.timeout),
            )
        };
        cx.notify();
        let Some(client) = self.client.clone() else {
            self.overview.borrow_mut().busy = false;
            return;
        };
        let admin = client.server_admin();
        let community = self.community;
        let draft = self.overview.clone();
        cx.spawn_in(window, async move |this, cx| {
            let mut renamed = None;
            let mut failure: Option<(AdminError, bool)> = None;
            if let Some(name) = &rename {
                match admin.rename(community, name).wait().await {
                    Ok(()) => renamed = Some(name.clone()),
                    Err(e) => failure = Some((e, false)),
                }
            }
            let mut saved_timeout = None;
            if failure.is_none()
                && let Some(seconds) = timeout
            {
                match admin.set_afk(community, seconds).wait().await {
                    Ok(()) => saved_timeout = Some(seconds),
                    Err(e) => failure = Some((e, true)),
                }
            }
            let _ = this.update_in(cx, |this, _window, cx| {
                let mut d = draft.borrow_mut();
                d.busy = false;
                if let Some(name) = renamed.clone() {
                    d.name_base = name;
                }
                if let Some(seconds) = saved_timeout {
                    d.timeout_base = Some(seconds);
                }
                let mut partial = false;
                match failure {
                    None => {}
                    Some((e, in_timeout)) => {
                        let timeout_only = in_timeout && rename.is_none();
                        if renamed.is_some() {
                            partial = true;
                            d.afk_failure = true;
                            d.error = Some(tf("admin.afk.error", &[("code", &e.code)]));
                        } else if in_timeout {
                            d.afk_failure = true;
                            d.error = Some(if timeout_only {
                                tf("admin.afk.error", &[("code", &e.code)])
                            } else {
                                overview_error(&e)
                            });
                        } else {
                            d.error = Some(overview_error(&e));
                        }
                    }
                }
                drop(d);
                if partial {
                    this.say(t("workspace.overview.partial").to_owned(), cx);
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn leave(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(client) = self.client.clone() else {
            return;
        };
        self.dialogue_busy = true;
        self.dialogue_error = None;
        let name = self.server_name();
        let reply = client.server_admin().leave(self.community);
        self.run(reply, window, cx, move |this, result, _, cx| {
            this.dialogue_busy = false;
            match result {
                Ok(()) => {
                    this.dialogue = Dialogue::None;
                    this.say(tf("admin.left", &[("server", &name)]), cx);
                    cx.emit(BodyEvent::Gone);
                }
                Err(e) => {
                    this.dialogue_error =
                        Some(tf("admin.leave.error.other", &[("code", &e.code)]).into());
                }
            }
            cx.notify();
        });
        cx.notify();
    }

    fn delete_server(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(client) = self.client.clone() else {
            return;
        };
        self.dialogue_busy = true;
        self.dialogue_error = None;
        let name = self.server_name();
        let reply = client.server_admin().delete_community(self.community);
        self.run(reply, window, cx, move |this, result, _, cx| {
            this.dialogue_busy = false;
            match result {
                Ok(()) => {
                    this.dialogue = Dialogue::None;
                    this.say(tf("admin.delete.done", &[("server", &name)]), cx);
                    cx.emit(BodyEvent::Gone);
                }
                Err(e) => {
                    this.dialogue_error = Some(
                        if e.forbidden() {
                            t("admin.delete.error.forbidden").to_owned()
                        } else {
                            tf("admin.delete.error.other", &[("code", &e.code)])
                        }
                        .into(),
                    );
                }
            }
            cx.notify();
        });
        cx.notify();
    }

    fn transfer(
        &mut self,
        to: UserId,
        to_name: String,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(client) = self.client.clone() else {
            return;
        };
        self.dialogue_busy = true;
        self.dialogue_error = None;
        let name = self.server_name();
        let reply = client.server_admin().transfer_ownership(self.community, to);
        self.run(reply, window, cx, move |this, result, _, cx| {
            this.dialogue_busy = false;
            match result {
                Ok(()) => {
                    this.dialogue = Dialogue::None;
                    this.say(
                        tf(
                            "admin.transfer.done",
                            &[("name", &to_name), ("server", &name)],
                        ),
                        cx,
                    );
                }
                Err(e) => {
                    this.dialogue_error = Some(
                        if e.status_is(404) {
                            tf("admin.transfer.error.gone", &[("server", &name)])
                        } else if e.forbidden() {
                            t("admin.transfer.error.forbidden").to_owned()
                        } else {
                            tf("admin.transfer.error.other", &[("code", &e.code)])
                        }
                        .into(),
                    );
                }
            }
            cx.notify();
        });
        cx.notify();
    }

    fn move_channel(
        &mut self,
        channel: ChannelId,
        direction: Direction,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(client) = self.client.clone() else {
            return;
        };
        if self.moving {
            return;
        }
        self.moving = true;
        self.move_error = None;
        let name = self
            .state
            .channel(channel)
            .map_or_else(String::new, |c| c.name.clone());
        let reply = client.server_admin().move_channel(channel, direction);
        self.run(reply, window, cx, move |this, result, _, cx| {
            this.moving = false;
            match result {
                Ok(()) => this.say(tf("admin.channels.moved", &[("name", &name)]), cx),
                Err(e) => {
                    this.move_error = Some(
                        tf(
                            "admin.channels.error.move",
                            &[("name", &name), ("code", &e.code)],
                        )
                        .into(),
                    );
                }
            }
            cx.notify();
        });
    }

    fn open_edit(&mut self, channel: ChannelId, window: &mut Window, cx: &mut Context<Self>) {
        let Some(c) = self.state.channel(channel).cloned() else {
            return;
        };
        let categories: Vec<ChannelId> = self
            .channels()
            .iter()
            .filter(|x| x.kind == 2)
            .map(|x| x.id)
            .collect();
        self.edit_name
            .update(cx, |s, cx| s.set_value(c.name.clone(), window, cx));
        self.edit_topic
            .update(cx, |s, cx| s.set_value(c.topic.clone(), window, cx));
        self.edit_parent = match c.parent_id {
            Some(p) if categories.contains(&p) => p.hex(),
            _ => "top".into(),
        };
        self.edit_slow = c.slowmode.to_string();
        self.dialogue_error = None;
        self.dialogue_busy = false;
        self.dialogue = Dialogue::Edit(channel);
        if let Some(client) = self.client.clone() {
            let reply = client.server_admin().load_access(self.community);
            self.run(reply, window, cx, |_, _, _, cx| cx.notify());
        }
        cx.notify();
    }

    /// ChannelEditFlow.tsx `submit`: only the fields that changed are sent; nothing changed closes the dialog.
    fn save_edit(&mut self, channel: ChannelId, window: &mut Window, cx: &mut Context<Self>) {
        let Some(c) = self.state.channel(channel).cloned() else {
            return;
        };
        let kind = c.kind;
        let typed_name = self.edit_name.read(cx).value().to_string();
        let shown = channel_name(kind, &typed_name);
        if let Some(problem) = name_problem(&shown, CHANNEL_NAME_MAX_BYTES) {
            self.dialogue_error = Some(
                match problem {
                    NameProblem::Empty => t("admin.channel.error.empty"),
                    NameProblem::Long => t("admin.channel.error.long"),
                    NameProblem::Control => t("create.name.control"),
                }
                .into(),
            );
            cx.notify();
            return;
        }
        let topic_text = self.edit_topic.read(cx).value().to_string();
        if kind == 0 {
            if topic_text.len() > TOPIC_MAX_BYTES {
                self.dialogue_error = Some(t("admin.channel.error.topicLong").into());
                cx.notify();
                return;
            }
            if topic_text
                .chars()
                .any(|ch| ch != '\n' && ch != '\t' && ch.is_control())
            {
                self.dialogue_error = Some(t("admin.channel.error.topicControl").into());
                cx.notify();
                return;
            }
        }
        let name = (shown != c.name).then_some(shown);
        let topic = (kind == 0 && topic_text != c.topic).then_some(topic_text);
        let initial_parent = c
            .parent_id
            .filter(|p| {
                self.state
                    .channels
                    .iter()
                    .any(|x| x.id == *p && x.kind == 2)
            })
            .map_or_else(|| "top".to_owned(), |p| p.hex());
        let parent = if kind == 2 || self.edit_parent == initial_parent {
            Parent::Keep
        } else if self.edit_parent == "top" {
            Parent::Top
        } else {
            self.state
                .channels
                .iter()
                .find(|x| x.id.hex() == self.edit_parent)
                .map_or(Parent::Keep, |x| Parent::Category(x.id))
        };
        let slowmode = (kind == 0 && self.edit_slow != c.slowmode.to_string())
            .then(|| self.edit_slow.parse::<u32>().ok())
            .flatten();
        if name.is_none() && topic.is_none() && parent == Parent::Keep && slowmode.is_none() {
            self.dialogue = Dialogue::None;
            cx.notify();
            return;
        }
        let Some(client) = self.client.clone() else {
            return;
        };
        self.dialogue_busy = true;
        self.dialogue_error = None;
        let reply = client.server_admin().update_channel(
            channel,
            name.as_deref(),
            topic.as_deref(),
            parent,
            slowmode,
        );
        self.run(reply, window, cx, |this, result, _, cx| {
            this.dialogue_busy = false;
            match result {
                Ok(()) => this.dialogue = Dialogue::None,
                Err(e) => {
                    this.dialogue_error = Some(
                        if e.forbidden() {
                            t("admin.channel.error.forbidden").to_owned()
                        } else if e.status_is(404) {
                            t("admin.channel.error.gone").to_owned()
                        } else if e.refused() {
                            t("admin.channel.error.refused").to_owned()
                        } else {
                            tf("admin.channel.error.other", &[("code", &e.code)])
                        }
                        .into(),
                    );
                }
            }
            cx.notify();
        });
        cx.notify();
    }

    fn delete_channel(&mut self, channel: ChannelId, window: &mut Window, cx: &mut Context<Self>) {
        let Some(client) = self.client.clone() else {
            return;
        };
        self.dialogue_busy = true;
        self.dialogue_error = None;
        let reply = client.server_admin().delete_channel(channel);
        self.run(reply, window, cx, |this, result, _, cx| {
            this.dialogue_busy = false;
            match result {
                Ok(()) => this.dialogue = Dialogue::None,
                Err(e) => {
                    this.dialogue_error =
                        Some(tf("admin.channelDelete.error.other", &[("code", &e.code)]).into());
                }
            }
            cx.notify();
        });
        cx.notify();
    }

    fn create_category(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let typed = self.category_name.read(cx).value().to_string();
        let value = channel_name(1, &typed);
        if let Some(problem) = name_problem(&value, CHANNEL_NAME_MAX_BYTES) {
            self.dialogue_error = Some(
                match problem {
                    NameProblem::Empty => t("admin.category.error.empty"),
                    NameProblem::Long => t("admin.channel.error.long"),
                    NameProblem::Control => t("create.name.control"),
                }
                .into(),
            );
            cx.notify();
            return;
        }
        let Some(client) = self.client.clone() else {
            return;
        };
        self.dialogue_busy = true;
        self.dialogue_error = None;
        let reply = client
            .server_admin()
            .create_category(self.community, &value);
        self.run(reply, window, cx, |this, result, window, cx| {
            this.dialogue_busy = false;
            match result {
                Ok(_) => {
                    this.dialogue = Dialogue::None;
                    this.category_name
                        .update(cx, |s, cx| s.set_value("", window, cx));
                }
                Err(e) => {
                    this.dialogue_error =
                        Some(tf("admin.category.error.other", &[("code", &e.code)]).into());
                }
            }
            cx.notify();
        });
        cx.notify();
    }

    fn set_private(
        &mut self,
        channel: ChannelId,
        make_private: bool,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) {
        let Some(client) = self.client.clone() else {
            return;
        };
        self.dialogue_busy = true;
        self.dialogue_error = None;
        let name = self
            .state
            .channel(channel)
            .map_or_else(String::new, |c| c.name.clone());
        let reply = client
            .server_admin()
            .set_channel_private(channel, make_private);
        self.run(reply, window, cx, move |this, result, _, cx| {
            this.dialogue_busy = false;
            match result {
                Ok(()) => {
                    this.dialogue = Dialogue::Edit(channel);
                    this.say(tf("chperm.saved", &[("name", &name)]), cx);
                }
                Err(e) => {
                    this.dialogue = Dialogue::Edit(channel);
                    this.dialogue_error = Some(permission_error(&e).into());
                }
            }
            cx.notify();
        });
        cx.notify();
    }

    // ---- GIFs ----

    fn save_gif_key(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let key = self.gif_key.read(cx).value().to_string();
        if !gif_key_valid(&key) {
            self.gif_error = Some(t("gif.settings.invalid").into());
            cx.notify();
            return;
        }
        let Some(client) = self.client.clone() else {
            return;
        };
        if self.gif_busy {
            return;
        }
        self.gif_busy = true;
        self.gif_error = None;
        let reply = client.server_admin().set_gif_key(self.community, &key);
        self.run(reply, window, cx, |this, result, window, cx| {
            this.gif_busy = false;
            match result {
                Ok(()) => {
                    this.gif_key.update(cx, |s, cx| s.set_value("", window, cx));
                    this.gif_editing = false;
                    this.say(t("gif.settings.saved").to_owned(), cx);
                }
                Err(e) => this.gif_error = Some(gif_error(&e).into()),
            }
            cx.notify();
        });
        cx.notify();
    }

    fn remove_gif_key(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(client) = self.client.clone() else {
            return;
        };
        if self.gif_busy {
            return;
        }
        self.gif_busy = true;
        self.gif_error = None;
        let reply = client.server_admin().clear_gif_key(self.community);
        self.run(reply, window, cx, |this, result, _, cx| {
            this.gif_busy = false;
            match result {
                Ok(()) => {
                    this.gif_editing = false;
                    this.say(t("gif.settings.removed").to_owned(), cx);
                }
                Err(e) => this.gif_error = Some(gif_error(&e).into()),
            }
            cx.notify();
        });
        cx.notify();
    }

    fn pick_rating(&mut self, rating: GifRating, window: &mut Window, cx: &mut Context<Self>) {
        let Some(client) = self.client.clone() else {
            return;
        };
        if self.gif_busy {
            return;
        }
        self.gif_busy = true;
        self.gif_error = None;
        let reply = client.server_admin().set_gif_rating(self.community, rating);
        self.run(reply, window, cx, |this, result, _, cx| {
            this.gif_busy = false;
            match result {
                Ok(()) => this.say(t("gif.settings.ratingSaved").to_owned(), cx),
                Err(e) => this.gif_error = Some(gif_error(&e).into()),
            }
            cx.notify();
        });
        cx.notify();
    }

    // ---- invites ----

    fn make_invite(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(client) = self.client.clone() else {
            return;
        };
        if self.making {
            return;
        }
        self.making = true;
        self.invite_message = None;
        let (max_uses, ttl) = (self.uses, self.ttl);
        let note = model::made_note(max_uses, ttl);
        let reply = client
            .server_admin()
            .create_invite(self.community, max_uses, ttl);
        self.run(reply, window, cx, move |this, result, window, cx| {
            this.making = false;
            match result {
                Ok(created) => {
                    this.host = host_of(&created.url);
                    this.made = Some(Made {
                        url: created.url,
                        note,
                    });
                    this.load_for(ServerSection::Invites, window, cx);
                }
                Err(e) => {
                    this.invite_message = Some((
                        if e.forbidden() {
                            t("invite.error.forbidden").to_owned()
                        } else {
                            tf("invite.error.other", &[("code", &e.code)])
                        }
                        .into(),
                        true,
                    ));
                }
            }
            cx.notify();
        });
        cx.notify();
    }

    fn copy_invite(&mut self, cx: &mut Context<Self>) {
        let Some(made) = self.made.take() else {
            return;
        };
        cx.write_to_clipboard(gpui::ClipboardItem::new_string(made.url));
        self.say(t("serverSettings.invites.copied").to_owned(), cx);
        cx.notify();
    }

    fn revoke_invite(&mut self, invite: InviteId, window: &mut Window, cx: &mut Context<Self>) {
        let Some(client) = self.client.clone() else {
            return;
        };
        if self.busy_invite.is_some() {
            return;
        }
        self.busy_invite = Some(invite);
        self.invite_message = None;
        let reply = client.server_admin().revoke_invite(self.community, invite);
        self.run(reply, window, cx, |this, result, _, cx| {
            this.busy_invite = None;
            match result {
                Ok(()) => this.say(t("admin.invites.revoked").to_owned(), cx),
                Err(e) => {
                    this.invite_message = Some((
                        if e.forbidden() {
                            t("admin.invites.error.forbidden").to_owned()
                        } else {
                            tf("admin.invites.error.other", &[("code", &e.code)])
                        }
                        .into(),
                        true,
                    ));
                }
            }
            cx.notify();
        });
        cx.notify();
    }
}

fn overview_error(e: &AdminError) -> String {
    if e.status_is(409) {
        t("admin.overview.error.conflict").to_owned()
    } else if e.forbidden() {
        t("admin.overview.error.forbidden").to_owned()
    } else if e.refused() {
        t("admin.overview.error.refused").to_owned()
    } else {
        tf("admin.overview.error.other", &[("code", &e.code)])
    }
}

fn gif_error(e: &AdminError) -> String {
    if e.forbidden() {
        t("gif.settings.forbidden").to_owned()
    } else {
        tf("gif.settings.error", &[("code", &e.code)])
    }
}

/// ChannelPermissions.tsx `refusalText`.
fn permission_error(e: &AdminError) -> String {
    if e.forbidden() {
        t("chperm.error.forbidden").to_owned()
    } else if e.code == "E_WOULD_HIDE" {
        t("chperm.error.hidden").to_owned()
    } else if e.code == "E_BAD_INPUT" {
        t("chperm.error.unread").to_owned()
    } else if e.status_is(404) {
        t("chperm.error.gone").to_owned()
    } else {
        tf("chperm.error.other", &[("code", &e.code)])
    }
}

/// Create.tsx `channelName`: a text channel as a lower-case slug (`ship-talk`), a voice channel or category as typed, trimmed.
fn channel_name(kind: u8, raw: &str) -> String {
    let slug = kind == 0;
    let words: Vec<&str> = raw.split_whitespace().collect();
    let joined = words.join(if slug { "-" } else { " " });
    if slug { joined.to_lowercase() } else { joined }
}

/// The host of an invite link.
fn host_of(url: &str) -> String {
    url::Url::parse(url)
        .ok()
        .and_then(|u| {
            u.host_str().map(|h| {
                u.port()
                    .map_or_else(|| h.to_owned(), |p| format!("{h}:{p}"))
            })
        })
        .unwrap_or_default()
}

impl Render for Body {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let section = self.section;
        if !self.requested.contains(&section) {
            self.requested.push(section);
            self.load_for(section, window, cx);
        }
        let content: Vec<AnyElement> = match section {
            ServerSection::Overview => self.overview_section(window, cx),
            ServerSection::Channels => self.channels_section(window, cx),
            ServerSection::Gifs => self.gifs_section(window, cx),
            ServerSection::Invites => self.invites_section(window, cx),
            ServerSection::Welcome | ServerSection::JoinRoles => self.bot_page(section, cx),
            ServerSection::Danger => self.danger_section(window, cx),
            // Other areas' sections: their own hooks draw them.
            _ => Vec::new(),
        };
        let mut column = div().flex().flex_col().w_full();
        for part in content {
            column = column.child(part);
        }
        column.into_any_element()
    }
}
