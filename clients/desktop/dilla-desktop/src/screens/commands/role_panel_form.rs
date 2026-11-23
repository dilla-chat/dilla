//! `/rolepanel create` opens the role-panel editor (Task 29; bot-handlers.ts `rolePanelHandler`, BotCommandDialogs.tsx
//! `RolePanelEditor`). The web runs the command through its own handler: the editor offers the community's approved
//! self-service roles, and Publish has the Bot post a panel (sealed operation 31) and registers its mappings, both
//! under one request id (`RolePanels::publish_job`).
//!
//! - Opening ([`run`]): the command's pending row is added first and says how the command ended. Only in this community,
//!   for a member holding Manage Roles, whose Bot offers panel publication, with at least one approved self-service role
//!   ([`check`]); else the row says why (Failed). The roles are read from the instance, and the editor opens only while
//!   the window still shows the conversation and the permission is still held.
//! - The editor ([`RolePanelForm`]): a destination (text channels, the first preselected), and a check box per approved
//!   role with the emoji of its reaction (at most 16 characters). Publish checks the fields ([`panel_valid`]), then the
//!   permission, the destination and the approved roles again, and sends one request.
//! - Task 30: the editor's request id is the one the command memory remembers for its draft, and a publication that went out
//!   is kept there (`CommandMemory::role_panels`, as the web's `rolePanelAttempts`). The editor stays open, its Publish busy,
//!   until the Bot answers: an accepted panel closes it, a refusal keeps the choices with the reason and the next Publish is
//!   a new request (R29), and an answer that may have taken effect (a timeout, an answer that cannot be read: R30) keeps the
//!   id and the stored attempt with the fields locked, so the next Publish sends exactly what went out and the Bot answers
//!   what it did. That answer also puts the draft back in the composer, and a run of it with no editor open reconciles the
//!   stored attempt as well (without Manage Roles it stays pending).
//!
//! The form's Debug gives no role name and no emoji: they are community content (R20, R21).

use std::collections::{HashMap, HashSet};
use std::rc::Rc;
use std::sync::{Arc, Mutex};

use dilla_client::Client;
use dilla_client::features::bot::memory::{CommandMemory, RolePanelAttempt};
use dilla_client::features::bot::{self, Attempt, Outcome};
use dilla_client::features::role_panels::{PanelError, PanelOption, panel_valid};
use dilla_client::features::roles::perm::MANAGE_ROLES;
use dilla_client::features::server_admin::hex_id;
use dilla_client::state::bot::{Conversation, ResultProvider, ResultStatus};
use dilla_client::state::bot_admin::sealed_operation;
use dilla_client::state::roles::{RoleId, SelfRole};
use dilla_client::state::{AppState, ChannelId, CommunityId};
use gpui::{
    AnyElement, App, AppContext, ElementId, Entity, Focusable, InteractiveElement, IntoElement,
    ParentElement, StatefulInteractiveElement, Styled, Subscription, Window, WindowId, div, px,
};
use gpui_component::input::{InputEvent, InputState};

use dilla_desktop_ui::a11y::{Accessible, Live, Role};
use dilla_desktop_ui::components::button::{Button, ButtonSize};
use dilla_desktop_ui::components::check_list::check_box;
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::components::select::Select;
use dilla_desktop_ui::components::settings::frame::SECTION_GAP;
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::text::{line, root_line_height};
use dilla_desktop_ui::tokens::{color, text};

use super::{FixtureRows, add_fixture_row, announcement_form, copy, with_memory};
use crate::screens::shell::Clock;
use crate::slots::SlotCx;
use crate::strings::{t, tf};

/// The command's registry id.
pub const COMMAND: &str = "rolepanel create";

/// The sealed operation the Bot publishes a panel with: its presence in the catalog means the Bot offers it
/// (bot-handlers.ts:481 `catalogOffers(client, cid, 31)`).
const PUBLISH: u64 = 31;

/// The emoji field's `maxLength` (BotCommandDialogs.tsx:97): UTF-16 code units, as the browser counts them.
const EMOJI_UNITS: usize = 16;

/// The gap between a check box and its role's name (`label > input` and the text).
const ROLE_GAP: f32 = 8.0;

/// The gap between a role's check box row and its emoji field.
const ROLE_FIELD_GAP: f32 = 6.0;

// ---- the editor's rules (BotCommandDialogs.tsx:76-90) ----

/// The panel's options from the roles in list order, `(role, name, checked)`, and the emoji typed for each: the checked
/// ones, in the list's order, each with its emoji as typed less its white space (`emojis[role.id]?.trim() ?? ''`). A
/// missing emoji is an empty one, which [`panel_valid`] refuses.
pub fn options_of(
    selected: &[(RoleId, String, bool)],
    emojis: &HashMap<RoleId, String>,
) -> Vec<PanelOption> {
    selected
        .iter()
        .filter(|(_, _, checked)| *checked)
        .map(|(role, name, _)| PanelOption {
            emoji: emojis
                .get(role)
                .map_or_else(String::new, |e| e.trim().to_owned()),
            role: *role,
            name: name.clone(),
        })
        .collect()
}

/// `text` cut to the emoji field's [`EMOJI_UNITS`] UTF-16 code units (the `maxLength` of the web's input), or `None`
/// when it already fits. A character is never split.
pub fn clamp_emoji(text: &str) -> Option<String> {
    let mut units = 0;
    for (at, c) in text.char_indices() {
        units += c.len_utf16();
        if units > EMOJI_UNITS {
            return Some(text[..at].to_owned());
        }
    }
    None
}

/// `validateCurrent(…, 'manageRoles')`: the channel's bits are the viewer's resolved ones; none known is none held.
fn holds_manage_roles(state: &AppState, channel: ChannelId) -> bool {
    state
        .threads
        .channel_bits
        .get(&channel)
        .is_some_and(|bits| bits & u64::from(MANAGE_ROLES) != 0)
}

/// Whether `state` lets this member open the editor from `channel` of `community`, or why not: the words of the handler
/// (bot-handlers.ts:466-481). The approved roles are read after this.
pub fn check(
    state: &AppState,
    community: Option<CommunityId>,
    channel: ChannelId,
) -> Result<CommunityId, &'static str> {
    let community = community.ok_or("Role panels are available only in this community.")?;
    if !holds_manage_roles(state, channel) {
        return Err("Manage Roles permission is required.");
    }
    let offered = state.bot.catalogs.get(&community).is_some_and(|c| {
        c.commands
            .iter()
            .any(|command| u64::from(command.operation) == sealed_operation(PUBLISH))
    });
    if !offered {
        return Err("The Bot installation is unavailable for role panel publication.");
    }
    Ok(community)
}

/// bot-handlers.ts:469-470: an attempt held when the permission is gone stays uncertain.
const PENDING_PERMISSION: &str = "This role panel attempt is still pending. Restore Manage Roles permission in its original community to reconcile publication or registration.";

/// What the command's row says when the editor is left.
const CANCELLED: &str = "Role panel creation cancelled.";

/// The row's words for each answer of the Bot (bot-handlers.ts:503-509 and `resultError` :17-29).
const PUBLISHED: &str = "Role panel published and registered.";
const NOT_CONFIRMED: &str =
    "The role panel publication result is not confirmed. Retry this command to reconcile it.";
const FORBIDDEN: &str = "Manage Roles permission is required.";

/// A panel answer as the row and the editor read it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Answer {
    /// Published and registered.
    Accepted,
    /// The answer may have taken effect: the attempt and its id stay.
    Uncertain,
    /// Refused: the attempt goes, and the next publish is a new request.
    Refused(String),
}

impl Answer {
    /// The row's outcome and what the editor does, from the client's answer. A timeout and an answer that cannot be read
    /// are uncertain (R30; the client maps `OpError::Unreadable` to [`PanelError::Uncertain`]); `E_FORBIDDEN` is the
    /// permission's own words; any other refusal is `Command was refused ({code}).`.
    pub fn of(result: Result<(), PanelError>, channel: &str) -> (Self, Outcome) {
        match result {
            Ok(()) => (Self::Accepted, Outcome::Success(PUBLISHED.to_owned())),
            Err(PanelError::Uncertain { .. }) => (
                Self::Uncertain,
                Outcome::Uncertain(NOT_CONFIRMED.to_owned()),
            ),
            // A Bot status other than accepted or forbidden (busy, stale, unavailable, ...) is an answer from before the
            // post: the Bot posted nothing. Deliberately NOT the web's stuck path (bot-handlers.ts:496 keeps the attempt
            // as uncertain, and a resend under the same request id replays the intake's kept answer, so the editor could
            // never succeed): Jonas decided (2026-11-21, Task 29 fix round 2) it is a refusal that drops the attempt,
            // unlocks the fields and makes the next Publish a fresh request (R29). It keeps the web's "not confirmed"
            // copy, the web having no hint of its own for these codes. A failure after the post stays uncertain.
            // Status 5 (unavailable) is what the Bot answers for a destination nobody has opened yet, which has no text group.
            // Jonas 2026-11-21 ("Desktop retry + hint"; the Bot stays unchanged): the row names what to do, with the
            // destination's name (`botCommand.rolepanel.openChannel`, native desktop only), and the editor is free for a fresh
            // retry.
            Err(PanelError::Status(code)) if code == "E_UNAVAILABLE" => (
                Self::Refused(code),
                Outcome::Failure(tf(
                    "botCommand.rolepanel.openChannel",
                    &[("channel", channel)],
                )),
            ),
            Err(PanelError::Status(code)) => (
                Self::Refused(code),
                Outcome::Failure(NOT_CONFIRMED.to_owned()),
            ),
            Err(PanelError::Refused(code)) if code == "E_FORBIDDEN" => {
                (Self::Refused(code), Outcome::Failure(FORBIDDEN.to_owned()))
            }
            Err(PanelError::Refused(code)) => {
                let text = format!("Command was refused ({code}).");
                (Self::Refused(code), Outcome::Failure(text))
            }
            Err(PanelError::Invalid) => (
                Self::Refused("E_INVALID".to_owned()),
                Outcome::Failure("Command was refused (E_INVALID).".to_owned()),
            ),
        }
    }
}

// ---- the editor ----

/// The editor of one window: the choices, the emoji inputs and the publication in flight. Its Debug gives counts only.
pub struct RolePanelForm {
    /// The text channels a panel may go to (`kind == 0`), in the community's order.
    pub channels: Vec<(ChannelId, String)>,
    pub channel: Option<ChannelId>,
    /// The community's approved self-service roles, in the list's order.
    pub roles: Vec<SelfRole>,
    pub selected: HashSet<RoleId>,
    pub emojis: HashMap<RoleId, Entity<InputState>>,
    /// Why the last Publish did not go (a refusal, or the hint of an attempt that may have taken effect).
    pub error: Option<String>,
    /// A publication is out: Publish is disabled, and a second sends nothing.
    pub busy: bool,
    /// The editor's one request id: every publication of this editor sends it. It is the command's remembered id (Task 30)
    /// until a refusal, after which the next Publish is a new request.
    pub request: [u8; 16],
    /// The command's row.
    pub row: u64,
    pub conversation: Conversation,
    pub community: CommunityId,
    pub clock: Clock,
    /// `None` in a fixture: its Publish does nothing after the checks.
    pub client: Option<Rc<Client>>,
    /// The client's command memory (a fixture's own, which nothing writes). It holds the publication that went out and may
    /// have taken effect (`role_panels`, under `request` and the community): the one thing a Publish sends again until the
    /// Bot answers for it. The fields are locked while it is held.
    memory: Arc<Mutex<CommandMemory>>,
    /// The command's invocation key and draft (Task 30): an uncertain answer keeps them with the request.
    invocation: String,
    draft: String,
    /// Each input's subscription.
    _subs: Vec<Subscription>,
    /// The publication was accepted and the editor is to go.
    done: bool,
}

impl std::fmt::Debug for RolePanelForm {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("RolePanelForm")
            .field("channels", &self.channels.len())
            .field("roles", &self.roles.len())
            .field("selected", &self.selected.len())
            .field("error", &self.error.is_some())
            .field("busy", &self.busy)
            .field("request", &self.request)
            .field("row", &self.row)
            .field("conversation", &self.conversation)
            .field("community", &self.community)
            .field("client", &self.client.is_some())
            .finish_non_exhaustive()
    }
}

impl RolePanelForm {
    /// A Publish asks to go: the editor's request id when none is out, else `None` (a second press while one is out).
    pub fn begin_publish(&mut self) -> Option<[u8; 16]> {
        if self.busy {
            return None;
        }
        self.busy = true;
        Some(self.request)
    }

    /// The publication is answered: Publish is enabled again. `keep_open` is false when the answer closed the editor; an
    /// answer that leaves it open keeps its request id (an uncertain one resends it; a refusal mints a new one in
    /// [`RolePanelForm::refused`]).
    pub fn end_publish(&mut self, keep_open: bool) {
        self.busy = false;
        if !keep_open {
            self.done = true;
        }
    }

    /// The publication was refused: the editor stays open and the next Publish is a new request (R29: the Bot would answer
    /// a stored refusal again).
    pub fn refused(&mut self) {
        self.end_publish(true);
        self.request = dilla_client::features::roles::request_id();
    }

    /// The attempt the command memory holds for this editor's request, as it went out.
    fn held(&self) -> Option<RolePanelAttempt> {
        self.memory
            .lock()
            .unwrap_or_else(std::sync::PoisonError::into_inner)
            .role_panels
            .get(&(self.request, self.community))
            .filter(|a| a.sent)
            .cloned()
    }

    /// Whether the fields are locked (an attempt may have taken effect and is the one thing Publish sends).
    pub fn locked(&self) -> bool {
        self.held().is_some()
    }

    /// The command's run under the editor's current request id.
    fn attempt(&self) -> Attempt {
        Attempt {
            request: self.request,
            invocation: self.invocation.clone(),
            text: self.draft.clone(),
        }
    }

    /// How Cancel ends the command's row: Failed "Role panel creation cancelled." (:480), but never over an attempt that may
    /// have taken effect (the web only releases the row it holds) or a publication still out, which finish their own row.
    pub fn cancelled_row(&self) -> Option<Outcome> {
        (!self.busy && !self.locked()).then(|| Outcome::Failure(CANCELLED.to_owned()))
    }

    /// The options the editor's choices make: the checked roles in the list's order with the emoji typed for each.
    fn options(&self, cx: &App) -> Vec<PanelOption> {
        let chosen: Vec<(RoleId, String, bool)> = self
            .roles
            .iter()
            .map(|r| (r.id, r.name.clone(), self.selected.contains(&r.id)))
            .collect();
        let typed: HashMap<RoleId, String> = self
            .emojis
            .iter()
            .map(|(id, input)| (*id, input.read(cx).value().to_string()))
            .collect();
        options_of(&chosen, &typed)
    }
}

/// What the command's answer asks the window to show, until its window draws it (an input is made with a window).
struct Wanted {
    channels: Vec<(ChannelId, String)>,
    roles: Vec<SelfRole>,
    attempt: Attempt,
    memory: Arc<Mutex<CommandMemory>>,
    row: u64,
    conversation: Conversation,
    community: CommunityId,
    clock: Clock,
    client: Option<Rc<Client>>,
}

#[derive(Default)]
struct Slot {
    wanted: Option<Wanted>,
    form: Option<RolePanelForm>,
}

fn slot(window: WindowId, cx: &mut App) -> &mut Slot {
    crate::per_window::get_mut::<Slot>(window, cx)
}

/// `window` forgets what it holds (its client was released): nothing typed stays, nothing is sent.
pub fn forget_window(window: WindowId, cx: &mut App) {
    crate::per_window::set::<Slot>(window, None, cx);
}

/// Whether `window` holds an editor.
pub fn is_open(window: WindowId, cx: &App) -> bool {
    crate::per_window::get::<Slot>(window, cx)
        .is_some_and(|s| s.wanted.is_some() || s.form.is_some())
}

/// The editor of `window`, once it was drawn.
pub fn form_of(window: WindowId, cx: &App) -> Option<&RolePanelForm> {
    crate::per_window::get::<Slot>(window, cx)?.form.as_ref()
}

/// The command's row of `attempt` ends as `outcome`: its row says so and the command memory learns it
/// ([`super::settle`], which puts an uncertain run's draft back in the composer of the client that ran it, Task 30 I3). A
/// fixture keeps its row in the window.
fn ended(
    client: Option<&Rc<Client>>,
    window: WindowId,
    (conversation, row): (Conversation, u64),
    attempt: &Attempt,
    outcome: Outcome,
    cx: &mut App,
) {
    match client {
        Some(client) => super::settle(client, window, (conversation, row), attempt, outcome, cx),
        None => {
            let (status, text) = outcome.row();
            let ended = (status, text.to_owned());
            announcement_form::finish(None, window, conversation, row, ended, cx);
        }
    }
}

/// What a run needs while it reads the roles or sends: the window, the command's row and run, and the client.
#[derive(Clone)]
struct Job {
    window: WindowId,
    conversation: Conversation,
    community: CommunityId,
    row: u64,
    attempt: Attempt,
    client: Rc<Client>,
}

impl Job {
    fn end(&self, outcome: Outcome, cx: &mut App) {
        ended(
            Some(&self.client),
            self.window,
            (self.conversation, self.row),
            &self.attempt,
            outcome,
            cx,
        );
    }

    /// The window still shows the command's conversation and the viewer still holds Manage Roles there
    /// (`context.isCurrent() && validateCurrent`).
    fn current(&self, cx: &mut App) -> bool {
        crate::screens::threads::channel_of(self.window, cx)
            .is_none_or(|c| c == self.conversation.channel)
            && holds_manage_roles(&self.client.state().borrow(), self.conversation.channel)
    }
}

/// `/rolepanel create` was sent from the composer. Its row is pending from here ([`ended`] ends it); a community without
/// the rights or the Bot's offer ends it Failed ([`check`]); otherwise the approved roles are read from the instance and
/// the editor opens in [`after_roles`]. A fixture reads them from its state. A run whose request already sent a panel
/// reconciles that attempt first, with neither the catalog's offer nor the editor.
pub fn run(
    ctx: &SlotCx,
    conversation: Conversation,
    attempt: Attempt,
    window: WindowId,
    cx: &mut App,
) {
    let channel = conversation.channel;
    // The web has one path for an uncertain attempt: its dialog closes and the reopened draft reconciles it. Here the
    // editor stays open and locked on it, so the reopened draft sent again is that editor's Publish: its own row, one
    // reconcile at a time, never a second row for the same request.
    if routes_to_editor(window, &attempt, cx) {
        submit(window, cx);
        return;
    }
    let pending = copy::pending(COMMAND);
    let row = match &ctx.client {
        Some(client) => client.bot().add_result(
            conversation,
            ResultStatus::Pending,
            pending,
            ResultProvider::Bot,
        ),
        None => {
            let row = add_fixture_row(window, conversation, ctx.clock, pending, cx);
            for (_, r) in &mut crate::per_window::get_mut::<FixtureRows>(window, cx).0 {
                if r.id == row {
                    r.provider = ResultProvider::Bot;
                }
            }
            row
        }
    };
    // bot-handlers.ts:466-470 (Task 30): a run whose request already sent a panel reconciles it first.
    if let (Some(client), Some(community)) = (&ctx.client, ctx.community)
        && let Some(stored) = stored_attempt(client, community, attempt.request)
    {
        let job = Job {
            window,
            conversation,
            community,
            row,
            attempt,
            client: client.clone(),
        };
        if !holds_manage_roles(&client.state().borrow(), channel) {
            job.end(Outcome::Uncertain(PENDING_PERMISSION.to_owned()), cx);
            return;
        }
        send(job, stored.channel, stored.options, cx);
        return;
    }
    let community = match check(&ctx.state, ctx.community, channel) {
        Ok(community) => community,
        Err(why) => {
            let refused = Outcome::Failure(why.to_owned());
            return ended(
                ctx.client.as_ref(),
                window,
                (conversation, row),
                &attempt,
                refused,
                cx,
            );
        }
    };
    let Some(client) = ctx.client.clone() else {
        let roles = ctx
            .state
            .roles
            .self_roles
            .get(&community)
            .cloned()
            .unwrap_or_default();
        let started = Started {
            conversation,
            community,
            row,
            clock: ctx.clock,
            attempt,
        };
        return open_editor(&ctx.state, started, roles, None, window, cx);
    };
    let started = Started {
        conversation,
        community,
        row,
        clock: ctx.clock,
        attempt,
    };
    let read = client
        .roles()
        .run(move |r| async move { r.load_self_roles(community).await });
    cx.spawn(async move |cx| {
        let listed = read.await;
        cx.update(|cx| after_roles(window, &client, started, listed, cx));
    })
    .detach();
}

/// The role panel attempt of `request` in `community` that went out (`sent`), as the command memory keeps it.
fn stored_attempt(
    client: &Client,
    community: CommunityId,
    request: [u8; 16],
) -> Option<RolePanelAttempt> {
    with_memory(client, |m| {
        m.role_panels
            .get(&(request, community))
            .filter(|a| a.sent)
            .cloned()
    })
}

/// Whether `attempt` is the request of `window`'s open editor while that editor is locked on a sent attempt: the run is
/// then the editor's Publish (a Publish already out sends nothing more).
fn routes_to_editor(window: WindowId, attempt: &Attempt, cx: &App) -> bool {
    form_of(window, cx).is_some_and(|f| f.request == attempt.request && f.locked())
}

/// A command that is reading the approved roles.
struct Started {
    conversation: Conversation,
    community: CommunityId,
    row: u64,
    clock: Clock,
    attempt: Attempt,
}

/// The instance answered the read of the approved roles: the editor opens, or the command ends (`rolePanelHandler` after
/// `listSelfRoles`).
fn after_roles(
    w: WindowId,
    client: &Rc<Client>,
    started: Started,
    listed: Result<Vec<SelfRole>, dilla_client::sync::api::ApiError>,
    cx: &mut App,
) {
    let job = Job {
        window: w,
        conversation: started.conversation,
        community: started.community,
        row: started.row,
        attempt: started.attempt.clone(),
        client: client.clone(),
    };
    let roles = match listed {
        Ok(roles) => roles,
        Err(e) => return job.end(bot::mutation_outcome(&e), cx),
    };
    if !job.current(cx) {
        let why = "The community or permission changed before opening the role panel.";
        return job.end(Outcome::Failure(why.to_owned()), cx);
    }
    let state = client.state().borrow().clone();
    open_editor(&state, started, roles, Some(client.clone()), w, cx);
}

/// The editor is asked for with `roles` (`None` when the community approved none: the command ends): it is drawn from the
/// next frame ([`overlay`]).
fn open_editor(
    state: &AppState,
    started: Started,
    roles: Vec<SelfRole>,
    client: Option<Rc<Client>>,
    w: WindowId,
    cx: &mut App,
) {
    if roles.is_empty() {
        let why = "Approve at least one self-service role before creating a panel.";
        return ended(
            client.as_ref(),
            w,
            (started.conversation, started.row),
            &started.attempt,
            Outcome::Failure(why.to_owned()),
            cx,
        );
    }
    let channels: Vec<(ChannelId, String)> = state
        .channels
        .iter()
        .filter(|c| c.community_id == started.community && c.kind == 0)
        .map(|c| (c.id, c.name.clone()))
        .collect();
    slot(w, cx).wanted = Some(Wanted {
        channels,
        roles,
        memory: client
            .as_ref()
            .map_or_else(Arc::default, |c| c.bot().memory()),
        attempt: started.attempt,
        row: started.row,
        conversation: started.conversation,
        community: started.community,
        clock: started.clock,
        client,
    });
    cx.refresh_windows();
}

/// One of the editor's inputs: a change clears the editor's refusal.
fn clear_on_change(w: WindowId, input: &Entity<InputState>, cx: &mut App) -> Subscription {
    cx.subscribe(input, move |_, event: &InputEvent, cx| {
        if matches!(event, InputEvent::Change) {
            if let Some(form) = slot(w, cx).form.as_mut() {
                form.error = None;
            }
            cx.refresh_windows();
        }
    })
}

fn make(w: WindowId, wanted: Wanted, window: &mut Window, cx: &mut App) -> RolePanelForm {
    let mut subs = Vec::new();
    let mut emojis = HashMap::new();
    for role in &wanted.roles {
        let input = cx.new(|cx| InputState::new(window, cx));
        subs.push(clear_on_change(w, &input, cx));
        // `maxLength={16}`: what is typed or pasted past it is cut.
        subs.push(
            window.subscribe(&input, cx, |input, event: &InputEvent, window, cx| {
                if matches!(event, InputEvent::Change)
                    && let Some(cut) = clamp_emoji(&input.read(cx).value())
                {
                    input.update(cx, |s, cx| s.set_value(cut, window, cx));
                }
            }),
        );
        emojis.insert(role.id, input);
    }
    RolePanelForm {
        channel: wanted.channels.first().map(|(id, _)| *id),
        channels: wanted.channels,
        roles: wanted.roles,
        selected: HashSet::new(),
        emojis,
        error: None,
        busy: false,
        request: wanted.attempt.request,
        invocation: wanted.attempt.invocation,
        draft: wanted.attempt.text,
        memory: wanted.memory,
        row: wanted.row,
        conversation: wanted.conversation,
        community: wanted.community,
        clock: wanted.clock,
        client: wanted.client,
        _subs: subs,
        done: false,
    }
}

/// What Publish found in the fields: a first publication's destination and options, or the stored attempt to send again.
enum Planned {
    First(ChannelId, Vec<PanelOption>),
    Resend(RolePanelAttempt),
}

/// Publish: with an attempt held, that attempt as it went out, whatever the fields say now; else checks the fields
/// ([`panel_valid`], `botCommand.rolepanel.invalid`), then the permission, the destination and the approved roles again,
/// and sends the editor's one request. A fixture has no client to send with, so its Publish does nothing after the checks.
pub fn submit(w: WindowId, cx: &mut App) {
    let Some(form) = form_of(w, cx) else {
        return;
    };
    if form.busy {
        return;
    }
    let planned = match form.held() {
        Some(held) => Planned::Resend(held),
        None => {
            let options = form.options(cx);
            match form.channel {
                Some(channel) if panel_valid(&options) => Planned::First(channel, options),
                _ => {
                    if let Some(form) = slot(w, cx).form.as_mut() {
                        form.error = Some(t("botCommand.rolepanel.invalid").to_owned());
                    }
                    cx.refresh_windows();
                    return;
                }
            }
        }
    };
    let Some(form) = slot(w, cx).form.as_mut() else {
        return;
    };
    // The fields are fine: an earlier refusal's reason goes.
    form.error = None;
    let Some(client) = form.client.clone() else {
        cx.refresh_windows();
        return;
    };
    if form.begin_publish().is_none() {
        return;
    }
    let job = Job {
        window: w,
        conversation: form.conversation,
        community: form.community,
        row: form.row,
        attempt: form.attempt(),
        client,
    };
    let held = matches!(planned, Planned::Resend(_));
    let gone = !holds_manage_roles(&job.client.state().borrow(), job.conversation.channel);
    // The permission is read again as the publication goes (`validateCurrent` after the dialog). With an attempt held the
    // answer stays uncertain and the editor and the attempt stay, to be sent again once the permission is back (:469-470).
    if gone {
        let outcome = if held {
            form.end_publish(true);
            form.error = Some(PENDING_PERMISSION.to_owned());
            Outcome::Uncertain(PENDING_PERMISSION.to_owned())
        } else {
            form.end_publish(false);
            let why = "Manage Roles permission changed before publication.";
            Outcome::Failure(why.to_owned())
        };
        job.end(outcome, cx);
        return;
    }
    form.error = None;
    // The choices are what goes out: they take no change from the first Publish on (a refusal unlocks them again).
    lock_inputs(w, true, cx);
    match planned {
        Planned::Resend(a) => send(job, a.channel, a.options, cx),
        Planned::First(channel, options) => {
            // The destination is still a text channel of the community (:481).
            let still = job
                .client
                .state()
                .borrow()
                .channels
                .iter()
                .any(|c| c.id == channel && c.community_id == job.community && c.kind == 0);
            if !still {
                let why = "Choose a text channel that still exists in this community.";
                return stop(job, why, cx);
            }
            let (community, client) = (job.community, job.client.clone());
            let read = client
                .roles()
                .run(move |r| async move { r.load_self_roles(community).await });
            cx.spawn(async move |cx| {
                let listed = read.await;
                cx.update(|cx| after_reread(job, (channel, options), listed, cx));
            })
            .detach();
        }
    }
    cx.refresh_windows();
}

/// A publication that never went out ends the command Failed and closes the editor.
fn stop(job: Job, why: &str, cx: &mut App) {
    if let Some(form) = slot(job.window, cx).form.as_mut()
        && form.request == job.attempt.request
    {
        form.end_publish(false);
    }
    job.end(Outcome::Failure(why.to_owned()), cx);
}

/// The approved roles were read again as Publish went (:483-486): every chosen role must still be listed with its name.
fn after_reread(
    job: Job,
    (channel, options): (ChannelId, Vec<PanelOption>),
    listed: Result<Vec<SelfRole>, dilla_client::sync::api::ApiError>,
    cx: &mut App,
) {
    // Cancel while the roles were read: the command ends, nothing is sent.
    let open = form_of(job.window, cx).is_some_and(|f| f.request == job.attempt.request);
    if !open {
        return job.end(Outcome::Failure(CANCELLED.to_owned()), cx);
    }
    let current = match listed {
        Ok(current) => current,
        Err(e) => {
            let outcome = bot::mutation_outcome(&e);
            if let Some(form) = slot(job.window, cx).form.as_mut() {
                form.end_publish(false);
            }
            return job.end(outcome, cx);
        }
    };
    if !job.current(cx) {
        let why = "The community or permission changed before role panel publication.";
        return stop(job, why, cx);
    }
    let all_listed = options
        .iter()
        .all(|o| current.iter().any(|r| r.id == o.role && r.name == o.name));
    if !all_listed {
        let why = "A selected role is no longer approved for self-service. Reopen the role panel editor and choose current roles.";
        return stop(job, why, cx);
    }
    send(job, channel, options, cx);
}

/// Sends the panel: the attempt is kept in the command memory as sent first (so an uncertain answer finds it), the row is
/// pending, and the Bot's answer ends both ([`answered`]). A resend of a held attempt sends it unchanged.
fn send(job: Job, channel: ChannelId, options: Vec<PanelOption>, cx: &mut App) {
    with_memory(&job.client, |m| {
        m.keep_role_panel(
            (job.attempt.request, job.community),
            RolePanelAttempt {
                channel,
                options: options.clone(),
                sent: true,
            },
        );
    });
    // A second attempt's row works again.
    announcement_form::finish(
        Some(&job.client),
        job.window,
        job.conversation,
        job.row,
        (ResultStatus::Pending, copy::pending(COMMAND)),
        cx,
    );
    let publish =
        job.client
            .role_panels()
            .publish_job(job.community, channel, options, job.attempt.request);
    cx.spawn(async move |cx| {
        let result = publish.await;
        cx.update(|cx| answered(&job, channel, result, cx));
    })
    .detach();
    cx.refresh_windows();
}

/// The Bot answered: the row's end and the command memory's attempt (kept while the answer may have taken effect), and the
/// editor's, which is only touched while it is still the one that sent the request.
fn answered(job: &Job, channel: ChannelId, result: Result<(), PanelError>, cx: &mut App) {
    let name = job
        .client
        .state()
        .borrow()
        .channels
        .iter()
        .find(|c| c.id == channel)
        .map_or_else(String::new, |c| c.name.clone());
    let (answer, outcome) = Answer::of(result, &name);
    if answer != Answer::Uncertain {
        with_memory(&job.client, |m| {
            m.drop_role_panel((job.attempt.request, job.community));
        });
    }
    let text = outcome.row().1.to_owned();
    job.end(outcome, cx);
    let w = job.window;
    if let Some(form) = slot(w, cx).form.as_mut()
        && form.request == job.attempt.request
    {
        match answer {
            Answer::Accepted => form.end_publish(false),
            Answer::Uncertain => {
                form.end_publish(true);
                form.error = Some(text);
            }
            Answer::Refused(_) => {
                form.refused();
                form.error = Some(text);
            }
        }
        let locked = form.locked();
        lock_inputs(w, locked, cx);
    }
}

/// The fields take no typing while an attempt that may have taken effect is the one thing Publish sends.
fn lock_inputs(w: WindowId, locked: bool, cx: &mut App) {
    let Some(form) = slot(w, cx).form.as_ref() else {
        return;
    };
    let inputs: Vec<Entity<InputState>> = form.emojis.values().cloned().collect();
    for input in inputs {
        input.update(cx, |s, cx| s.set_disabled(locked, cx));
    }
}

/// Cancel, Escape, a click outside and leaving the conversation: the command ends Failed, "Role panel creation cancelled."
/// (:480). A publication already out finishes its own row.
pub fn cancel(w: WindowId, cx: &mut App) {
    let s = slot(w, cx);
    s.wanted = None;
    let Some(form) = s.form.take() else {
        return;
    };
    if let Some(ended_as) = form.cancelled_row() {
        let attempt = form.attempt();
        ended(
            form.client.as_ref(),
            w,
            (form.conversation, form.row),
            &attempt,
            ended_as,
            cx,
        );
    }
}

/// The destination select chose `channel` (`None`: "Choose a channel").
pub fn choose_channel(w: WindowId, channel: Option<ChannelId>, cx: &mut App) {
    if let Some(form) = slot(w, cx).form.as_mut()
        && !form.locked()
        && !form.busy
    {
        form.channel = channel;
        form.error = None;
    }
    cx.refresh_windows();
}

/// The check box of `role` was pressed.
pub fn toggle_role(w: WindowId, role: RoleId, cx: &mut App) {
    if let Some(form) = slot(w, cx).form.as_mut()
        && !form.locked()
        && !form.busy
        && form.roles.iter().any(|r| r.id == role)
    {
        if !form.selected.remove(&role) {
            form.selected.insert(role);
        }
        form.error = None;
    }
    cx.refresh_windows();
}

/// The editor over the window (`slots::overlays`), while one is open.
pub fn overlay(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let w = window.window_handle().window_id();
    if let Some(wanted) = slot(w, cx).wanted.take() {
        let form = make(w, wanted, window, cx);
        slot(w, cx).form = Some(form);
    }
    let form = slot(w, cx).form.as_ref()?;
    // Closed by its publication.
    if form.done {
        slot(w, cx).form = None;
        return None;
    }
    // Leaving the conversation closes the editor and ends the command (CommandHost's `isCurrent`).
    if ctx.channel != Some(form.conversation.channel) {
        cancel(w, cx);
        return None;
    }
    Some(editor(w, cx)?.into_any_element())
}

/// The destination select's value for no channel chosen.
const NO_CHANNEL: &str = "";

/// The automation id of an element named for a role: interned, so a name keeps one `&'static str` (the automation registry
/// keys on it).
fn role_id(prefix: &str, role: &RoleId) -> &'static str {
    // App-wide by design: interned automation ids (element names, never a client handle).
    static IDS: Mutex<Option<HashMap<String, &'static str>>> = Mutex::new(None);
    let name = format!("{prefix}-{}", hex_id(role));
    let mut ids = IDS
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    ids.get_or_insert_with(HashMap::new)
        .entry(name.clone())
        .or_insert_with(|| Box::leak(name.into_boxed_str()))
}

/// The editor's dialog (BotCommandDialogs.tsx `RolePanelEditor`): its fields in the web's order, with its labels.
fn editor(w: WindowId, cx: &mut App) -> Option<impl IntoElement> {
    let form = slot(w, cx).form.as_ref()?;
    let (channels, channel) = (form.channels.clone(), form.channel);
    let roles: Vec<(RoleId, String, bool, Entity<InputState>)> = form
        .roles
        .iter()
        .filter_map(|r| {
            Some((
                r.id,
                r.name.clone(),
                form.selected.contains(&r.id),
                form.emojis.get(&r.id)?.clone(),
            ))
        })
        .collect();
    let (busy, error) = (form.busy, form.error.clone());
    let locked = form.locked();

    let ids: Vec<(String, String)> = std::iter::once((
        NO_CHANNEL.to_owned(),
        t("botCommand.announcement.chooseDestination").to_owned(),
    ))
    .chain(channels.iter().map(|(id, name)| (id.hex(), name.clone())))
    .collect();
    let pairs: Vec<(&str, &str)> = ids.iter().map(|(v, l)| (v.as_str(), l.as_str())).collect();
    let destination = Select::new(
        "rolepanel-destination",
        t("botCommand.announcement.destination"),
        &pairs,
        channel.map_or_else(|| NO_CHANNEL.to_owned(), |c| c.hex()),
    )
    .disabled(locked || busy)
    .on_change(move |value, _, cx| {
        choose_channel(
            w,
            channels
                .iter()
                .map(|(id, _)| *id)
                .find(|id| id.hex() == value),
            cx,
        );
    });
    let mut list = div().flex().flex_col().gap(px(SECTION_GAP)).child(line(
        t("botCommand.rolepanel.roles"),
        text::SM,
        root_line_height(text::SM),
        color::FG_2,
    ));
    for (id, name, checked, input) in roles {
        let toggle_id = role_id("rolepanel-role", &id);
        let row = div()
            .id((ElementId::from(toggle_id), "row"))
            .flex()
            .items_center()
            .gap(px(ROLE_GAP))
            .cursor_pointer()
            .on_click(move |_, window, cx| {
                toggle_role(w, id, cx);
                window.refresh();
            })
            .child(check_box(
                (ElementId::from(toggle_id), "box"),
                checked,
                locked || busy,
            ))
            .child(line(
                name.clone(),
                text::SM,
                root_line_height(text::SM),
                color::FG,
            ));
        let emoji = crate::tracked_focus(
            role_id("rolepanel-emoji", &id),
            &input.focus_handle(cx),
            TextField::new(
                (ElementId::from(role_id("rolepanel-emoji", &id)), "field"),
                tf("botCommand.rolepanel.emojiFor", &[("role", &name)]),
                &input,
            )
            .disabled(locked || busy),
        );
        list = list.child(
            div()
                .flex()
                .flex_col()
                .gap(px(ROLE_FIELD_GAP))
                .child(crate::tracked_box(toggle_id, row))
                .child(emoji),
        );
    }
    let mut body = div()
        .flex()
        .flex_col()
        .gap(px(SECTION_GAP))
        .child(crate::tracked_box("rolepanel-destination", destination))
        .child(list);
    if let Some(error) = error {
        body = body.child(crate::tracked_box(
            "rolepanel-error",
            div()
                .id("rolepanel-error-text")
                .a11y(Role::Alert, error.clone())
                .live(Live::Assertive)
                .child(line(
                    error,
                    text::SM,
                    root_line_height(text::SM),
                    color::DANGER,
                )),
        ));
    }
    let cancel_button = Button::new("rolepanel-cancel-button", t("chperm.cancel"))
        .size(ButtonSize::Dialog)
        .on_click(move |_, window, cx| {
            cancel(w, cx);
            window.refresh();
        });
    let publish = Button::new(
        "rolepanel-publish-button",
        t("botCommand.rolepanel.publish"),
    )
    .size(ButtonSize::Dialog)
    .disabled(busy)
    .on_click(move |_, window, cx| {
        submit(w, cx);
        window.refresh();
    });
    Some(
        Dialog::new(
            "rolepanel-dialog",
            t("botCommand.rolepanel.title"),
            t("dialog.close"),
        )
        .body(body)
        .footer(crate::tracked_box("rolepanel-cancel", cancel_button))
        .footer(crate::tracked_box("rolepanel-publish", publish))
        .on_close(move |window, cx| {
            cancel(w, cx);
            window.refresh();
        }),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::features::bot::registry::Args;
    use dilla_client::state::channels::{Channel, ChannelGroupState};
    use dilla_client::state::{ChannelId, UserId};

    const COMMUNITY: CommunityId = CommunityId([1; 16]);
    const CHANNEL: [u8; 16] = [7; 16];
    const NOW: u64 = 1_794_481_738;
    const CREW: RoleId = [0xc1; 16];
    const ART: RoleId = [0xc2; 16];
    const GAMES: RoleId = [0xc3; 16];

    fn roles() -> Vec<(RoleId, String, bool)> {
        vec![
            (CREW, "Crew".into(), true),
            (ART, "Artists".into(), false),
            (GAMES, "Gamers".into(), true),
        ]
    }

    fn option(emoji: &str, role: RoleId, name: &str) -> PanelOption {
        PanelOption {
            emoji: emoji.into(),
            role,
            name: name.into(),
        }
    }

    #[test]
    fn role_panel_options_keep_the_chosen_roles_in_list_order() {
        // Typed in the opposite order of the list, with white space around one of them.
        let emojis: HashMap<RoleId, String> = [(GAMES, "🎨".to_owned()), (CREW, " 🎮 ".to_owned())]
            .into_iter()
            .collect();
        assert_eq!(
            options_of(&roles(), &emojis),
            vec![option("🎮", CREW, "Crew"), option("🎨", GAMES, "Gamers")],
            "the checked roles in the list's order, names kept, emoji trimmed"
        );
        assert!(
            options_of(&roles(), &HashMap::new())
                .iter()
                .all(|o| o.emoji.is_empty())
        );
    }

    #[test]
    fn an_emoji_repeated_or_missing_is_invalid() {
        let same: HashMap<RoleId, String> = [(CREW, "🎮".to_owned()), (GAMES, "🎮".to_owned())]
            .into_iter()
            .collect();
        assert!(!panel_valid(&options_of(&roles(), &same)), "repeated");
        let missing: HashMap<RoleId, String> = [(CREW, "🎮".to_owned())].into_iter().collect();
        assert!(!panel_valid(&options_of(&roles(), &missing)), "missing");
        let blank: HashMap<RoleId, String> = [(CREW, "🎮".to_owned()), (GAMES, "  ".to_owned())]
            .into_iter()
            .collect();
        assert!(!panel_valid(&options_of(&roles(), &blank)), "blank");
        let fine: HashMap<RoleId, String> = [(CREW, "🎮".to_owned()), (GAMES, "🎨".to_owned())]
            .into_iter()
            .collect();
        assert!(panel_valid(&options_of(&roles(), &fine)));
        let none: Vec<(RoleId, String, bool)> = vec![(CREW, "Crew".into(), false)];
        assert!(!panel_valid(&options_of(&none, &fine)), "no role chosen");
    }

    /// `maxLength={16}` counts UTF-16 code units: an emoji is two, and a character is never split.
    #[test]
    fn the_emoji_field_takes_sixteen_units() {
        assert_eq!(
            clamp_emoji("🎮🎮🎮🎮🎮🎮🎮🎮"),
            None,
            "eight emoji are sixteen units"
        );
        assert_eq!(
            clamp_emoji("🎮🎮🎮🎮🎮🎮🎮🎮🎮").as_deref(),
            Some("🎮🎮🎮🎮🎮🎮🎮🎮")
        );
        assert_eq!(clamp_emoji("abcdefghijklmnop"), None);
        assert_eq!(
            clamp_emoji("abcdefghijklmnopq").as_deref(),
            Some("abcdefghijklmnop")
        );
        assert_eq!(
            clamp_emoji("abcdefghijklmno🎮").as_deref(),
            Some("abcdefghijklmno"),
            "a pair that would cross the limit goes whole"
        );
    }

    fn state(offers: bool, manage: bool, approved: bool) -> AppState {
        use dilla_client::state::bot::{BotCatalogView, BotCommandSpec, BotStatus, BotStatusState};
        let mut commands = Vec::new();
        if offers {
            commands.push(BotCommandSpec {
                name: COMMAND.into(),
                description: String::new(),
                operation: 131,
                args: Vec::new(),
                permission_bit: 0,
                needs_bot_channel: false,
                handler_kind: 0,
            });
        }
        let mut s = AppState::default();
        s.account.user_id = Some(UserId([9; 16]));
        s.bot.catalogs.insert(
            COMMUNITY,
            BotCatalogView {
                revision: 1,
                commands,
                status: BotStatus {
                    state: BotStatusState::Ready,
                    since: None,
                },
                bot_channels: Some(vec![ChannelId(CHANNEL)]),
                bot_user: None,
                fingerprint: None,
            },
        );
        s.threads.channel_bits.insert(
            ChannelId(CHANNEL),
            if manage {
                1 | u64::from(MANAGE_ROLES)
            } else {
                1
            },
        );
        let channel = |id: [u8; 16], kind: u8, name: &str| Channel {
            id: ChannelId(id),
            community_id: COMMUNITY,
            kind,
            mode: 0,
            name: name.into(),
            topic: String::new(),
            parent_id: None,
            position: 0,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Active,
        };
        s.channels = vec![
            channel(CHANNEL, 0, "general"),
            channel([8; 16], 1, "Lounge"),
            channel([6; 16], 0, "news"),
        ];
        if approved {
            s.roles.self_roles.insert(
                COMMUNITY,
                vec![
                    SelfRole {
                        id: CREW,
                        name: "Crew".into(),
                        position: 1,
                        self_selected: false,
                    },
                    SelfRole {
                        id: GAMES,
                        name: "Gamers".into(),
                        position: 2,
                        self_selected: false,
                    },
                ],
            );
        }
        s
    }

    fn ctx(state: AppState) -> SlotCx {
        SlotCx {
            state: std::sync::Arc::new(state),
            community: Some(COMMUNITY),
            channel: Some(ChannelId(CHANNEL)),
            clock: Clock::Fixed {
                now: NOW,
                utc: true,
            },
            ..SlotCx::default()
        }
    }

    fn held_rows(w: WindowId, cx: &App) -> Vec<(ResultStatus, String)> {
        crate::per_window::get::<FixtureRows>(w, cx).map_or(Vec::new(), |r| {
            r.0.iter()
                .map(|(_, r)| (r.status, r.text.clone()))
                .collect()
        })
    }

    /// The words of the handler (bot-handlers.ts:466-486) where the command may not run, and a DM or another community.
    #[test]
    fn a_command_checks_where_it_may_run() {
        let ch = ChannelId(CHANNEL);
        let ok = state(true, true, true);
        assert_eq!(check(&ok, Some(COMMUNITY), ch), Ok(COMMUNITY));
        assert_eq!(
            check(&ok, None, ch),
            Err("Role panels are available only in this community.")
        );
        assert_eq!(
            check(&state(true, false, true), Some(COMMUNITY), ch),
            Err("Manage Roles permission is required.")
        );
        assert_eq!(
            check(&state(false, true, true), Some(COMMUNITY), ch),
            Err("The Bot installation is unavailable for role panel publication.")
        );
    }

    /// `/rolepanel create` in a state without bit 15 ends the row Failed and opens nothing.
    #[gpui::test]
    fn the_editor_refuses_without_manage_roles(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        // (Without the Bot's offer the composer has no such command to send: that refusal is [`check`]'s own, tested in
        // `a_command_checks_where_it_may_run`.)
        for (manage, approved, why) in [
            (false, true, "Manage Roles permission is required."),
            (
                true,
                false,
                "Approve at least one self-service role before creating a panel.",
            ),
        ] {
            let ctx = ctx(state(true, manage, approved));
            window.update(|_, cx| {
                assert_eq!(
                    super::super::on_send(&ctx, "/rolepanel create", &|t| t.to_owned(), w, cx),
                    super::super::SendDecision::Handled
                );
                assert!(!is_open(w, cx), "{why}");
                assert_eq!(held_rows(w, cx), [(ResultStatus::Failed, why.to_owned())]);
                crate::per_window::get_mut::<FixtureRows>(w, cx).0.clear();
            });
        }
    }

    /// The editor opens from the fixture's approved roles: the first text channel, a box and an emoji field per role, the
    /// row pending; Publish with nothing chosen says why; Cancel ends the row Failed.
    #[gpui::test]
    fn rolepanel_create_opens_the_editor(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        let ctx = ctx(state(true, true, true));
        window.update(|window, cx| {
            super::super::on_send(&ctx, "/rolepanel create", &|t| t.to_owned(), w, cx);
            assert!(is_open(w, cx));
            assert_eq!(
                held_rows(w, cx),
                [(ResultStatus::Pending, "/rolepanel create…".to_owned())]
            );
            assert!(overlay(&ctx, window, cx).is_some(), "the editor is drawn");
            let form = form_of(w, cx).expect("the editor was made");
            assert_eq!(
                form.channel,
                Some(ChannelId(CHANNEL)),
                "the first text channel"
            );
            assert_eq!(form.channels.len(), 2, "text channels only");
            assert_eq!(form.roles.len(), 2);
            assert_eq!(form.emojis.len(), 2);
            assert!(form.selected.is_empty());
            // Nothing chosen: refused where it is, and nothing is sent.
            submit(w, cx);
            assert_eq!(
                form_of(w, cx).unwrap().error.as_deref(),
                Some(
                    "Choose a channel, at least one approved role, and a unique emoji for each role."
                )
            );
            // Crew checked, no emoji yet: still invalid. Typed: valid, and a fixture's Publish then does nothing.
            toggle_role(w, CREW, cx);
            submit(w, cx);
            assert!(
                form_of(w, cx).unwrap().error.is_some(),
                "a role needs its emoji"
            );
            let input = form_of(w, cx).unwrap().emojis[&CREW].clone();
            input.update(cx, |s, cx| {
                s.set_value("🎮", window, cx);
                // `set_value` is silent; typing reports a change.
                cx.emit(InputEvent::Change);
            });
            submit(w, cx);
            let form = form_of(w, cx).unwrap();
            assert!(form.error.is_none() && !form.busy, "a fixture sends nothing");
            cancel(w, cx);
            assert!(!is_open(w, cx));
            assert!(overlay(&ctx, window, cx).is_none());
            assert_eq!(
                held_rows(w, cx),
                [(
                    ResultStatus::Failed,
                    "Role panel creation cancelled.".to_owned()
                )]
            );
        });
    }

    /// What is typed or pasted past 16 UTF-16 units is cut.
    #[gpui::test]
    fn the_emoji_input_cuts_at_sixteen_characters(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        let ctx = ctx(state(true, true, true));
        window.update(|window, cx| {
            super::super::on_send(&ctx, "/rolepanel create", &|t| t.to_owned(), w, cx);
            overlay(&ctx, window, cx);
            let input = form_of(w, cx).unwrap().emojis[&CREW].clone();
            input.update(cx, |s, cx| {
                s.set_value("abcdefghijklmnopqrstuvwxyz", window, cx);
                // `set_value` is silent; typing or pasting reports a change.
                cx.emit(InputEvent::Change);
            });
        });
        window.update(|_, cx| {
            let input = form_of(w, cx).unwrap().emojis[&CREW].clone();
            assert_eq!(input.read(cx).value().to_string(), "abcdefghijklmnop");
        });
    }

    #[gpui::test]
    fn leaving_the_conversation_ends_the_command(cx: &mut gpui::TestAppContext) {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        window.update(|window, cx| {
            let here = ctx(state(true, true, true));
            super::super::on_send(&here, "/rolepanel create", &|t| t.to_owned(), w, cx);
            assert!(overlay(&here, window, cx).is_some());
            let mut elsewhere = ctx(state(true, true, true));
            elsewhere.channel = Some(ChannelId([9; 16]));
            assert!(overlay(&elsewhere, window, cx).is_none());
            assert!(!is_open(w, cx));
            assert_eq!(
                held_rows(w, cx),
                [(
                    ResultStatus::Failed,
                    "Role panel creation cancelled.".to_owned()
                )]
            );
        });
    }

    fn form(cx: &mut gpui::TestAppContext) -> RolePanelForm {
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        window.update(|window, cx| {
            make(
                w,
                Wanted {
                    channels: vec![(ChannelId(CHANNEL), "general".into())],
                    roles: vec![SelfRole {
                        id: CREW,
                        name: "the secret crew".into(),
                        position: 1,
                        self_selected: false,
                    }],
                    attempt: Attempt {
                        request: [9; 16],
                        invocation: COMMAND.into(),
                        text: "/rolepanel create".into(),
                    },
                    memory: Arc::default(),
                    row: 1,
                    conversation: Conversation {
                        channel: ChannelId(CHANNEL),
                        thread: None,
                    },
                    community: COMMUNITY,
                    clock: Clock::Local,
                    client: None,
                },
                window,
                cx,
            )
        })
    }

    /// While one publication is out a second sends nothing. An accepted one closes the editor; a refusal leaves it open and
    /// the next is a new request (R29); an answer that may have taken effect leaves it open and keeps its id.
    #[gpui::test]
    fn a_second_publish_while_busy_sends_nothing(cx: &mut gpui::TestAppContext) {
        let mut form = form(cx);
        assert_eq!(form.begin_publish(), Some([9; 16]));
        assert!(form.busy);
        assert_eq!(form.begin_publish(), None, "a second press while it is out");
        form.end_publish(true);
        assert!(!form.busy && !form.done);
        assert_eq!(
            form.begin_publish(),
            Some([9; 16]),
            "an unsure answer keeps its id"
        );
        form.refused();
        assert!(!form.busy && !form.done, "a refusal leaves the editor open");
        let fresh = form.begin_publish().expect("idle again");
        assert_ne!(fresh, [9; 16], "a new request after a refusal");
        form.end_publish(false);
        assert!(form.done, "an accepted publication closes it");
    }

    /// The publication that went out, as the command memory keeps it (`send`).
    fn hold_sent(form: &RolePanelForm) {
        form.memory.lock().unwrap().keep_role_panel(
            (form.request, form.community),
            RolePanelAttempt {
                channel: ChannelId(CHANNEL),
                options: vec![option("🎮", CREW, "the secret crew")],
                sent: true,
            },
        );
    }

    fn release_sent(form: &RolePanelForm) {
        form.memory
            .lock()
            .unwrap()
            .drop_role_panel((form.request, form.community));
    }

    /// Task 30: while the stored attempt is held the fields are locked and Publish sends that attempt as it went out; once
    /// it is released the fields count again.
    #[gpui::test]
    fn an_unsure_answer_locks_the_editor_on_its_attempt(cx: &mut gpui::TestAppContext) {
        let form = form(cx);
        assert!(!form.locked() && form.held().is_none());
        hold_sent(&form);
        assert!(form.locked());
        let held = form.held().expect("the attempt");
        assert_eq!(held.channel, ChannelId(CHANNEL));
        assert_eq!(held.options, vec![option("🎮", CREW, "the secret crew")]);
        assert_eq!(form.attempt().request, [9; 16]);
        assert_eq!(form.attempt().text, "/rolepanel create");
        release_sent(&form);
        assert!(!form.locked());
    }

    /// The Bot's answers as the row and the editor read them (bot-handlers.ts:503-509).
    #[test]
    fn the_answers_read_as_the_webs_rows() {
        let (answer, outcome) = Answer::of(Ok(()), "news");
        assert_eq!(answer, Answer::Accepted);
        assert_eq!(
            outcome,
            Outcome::Success("Role panel published and registered.".into())
        );
        // A timeout and an answer that cannot be read (R30) may have taken effect.
        let (answer, outcome) = Answer::of(
            Err(PanelError::Uncertain {
                request_id: [1; 16],
            }),
            "news",
        );
        assert_eq!(answer, Answer::Uncertain);
        assert_eq!(
            outcome,
            Outcome::Uncertain(
                "The role panel publication result is not confirmed. Retry this command to reconcile it."
                    .into()
            )
        );
        let (answer, outcome) = Answer::of(Err(PanelError::Refused("E_FORBIDDEN".into())), "news");
        assert!(matches!(answer, Answer::Refused(_)));
        assert_eq!(
            outcome,
            Outcome::Failure("Manage Roles permission is required.".into())
        );
        // A Bot status other than accepted or forbidden came before any post: a refusal (Jonas's decision, see
        // `Answer::of`). Status 5 names the destination to open (Jonas 2026-11-21); any other keeps the web's "not
        // confirmed" copy.
        let (answer, outcome) = Answer::of(Err(PanelError::Status("E_UNAVAILABLE".into())), "news");
        assert_eq!(answer, Answer::Refused("E_UNAVAILABLE".into()));
        assert_eq!(
            outcome,
            Outcome::Failure("Open #news once, then try again.".into()),
            "the row says to open the destination once"
        );
        let (answer, outcome) = Answer::of(Err(PanelError::Status("E_BUSY".into())), "news");
        assert_eq!(answer, Answer::Refused("E_BUSY".into()));
        assert_eq!(
            outcome,
            Outcome::Failure(
                "The role panel publication result is not confirmed. Retry this command to reconcile it."
                    .into()
            )
        );
        // A refusal of the request itself (the transport's code) is a refusal.
        let (_, outcome) = Answer::of(Err(PanelError::Refused("E_ROUTE_INPUT".into())), "news");
        assert_eq!(
            outcome,
            Outcome::Failure("Command was refused (E_ROUTE_INPUT).".into())
        );
        let (answer, outcome) = Answer::of(Err(PanelError::Invalid), "news");
        assert!(matches!(answer, Answer::Refused(_)));
        assert_eq!(
            outcome,
            Outcome::Failure("Command was refused (E_INVALID).".into())
        );
    }

    /// Jonas's decision (fix round 2): a Bot status answer (status 5, nothing was posted) drops the stored attempt, unlocks the
    /// fields and makes the next Publish a fresh request, so the intake does not replay the kept status.
    #[gpui::test]
    fn a_bot_status_unlocks_the_editor_and_the_next_publish_is_fresh(
        cx: &mut gpui::TestAppContext,
    ) {
        let mut form = form(cx);
        hold_sent(&form);
        assert_eq!(form.begin_publish(), Some([9; 16]));
        let (answer, _) = Answer::of(Err(PanelError::Status("E_UNAVAILABLE".into())), "news");
        assert!(
            matches!(answer, Answer::Refused(_)),
            "a refusal, not uncertain"
        );
        // What `answered` does with a refusal: the attempt goes, the editor takes a new request.
        release_sent(&form);
        form.refused();
        assert!(!form.locked(), "the fields are free again");
        assert_ne!(form.request, [9; 16], "a fresh request id");
        assert_ne!(form.begin_publish(), Some([9; 16]));
    }

    /// C1: a failure after the Bot posted the panel (a registration timeout or refusal, an unreadable receipt) reaches the
    /// editor as an uncertain answer: the stored attempt and the request id stay, the fields stay locked, and the next
    /// Publish sends that attempt again under the same id, so the Bot posts no second panel.
    #[gpui::test]
    fn a_failure_after_the_post_keeps_the_attempt_and_its_id(cx: &mut gpui::TestAppContext) {
        let mut form = form(cx);
        hold_sent(&form);
        assert_eq!(form.begin_publish(), Some([9; 16]));
        // What `answered` does with the client's post-publication answer.
        let (answer, _) = Answer::of(
            Err(PanelError::Uncertain {
                request_id: [9; 16],
            }),
            "news",
        );
        assert_eq!(answer, Answer::Uncertain, "never a refusal");
        form.end_publish(true);
        assert!(form.locked(), "the attempt is still held");
        assert_eq!(form.request, [9; 16], "and its id");
        assert_eq!(
            form.begin_publish(),
            Some([9; 16]),
            "the next Publish sends it again"
        );
        assert_eq!(
            form.held().map(|a| (a.channel, a.options.len())),
            Some((ChannelId(CHANNEL), 1)),
            "exactly as it went out"
        );
    }

    /// Cancel ends the row Failed only when nothing may have taken effect; over an unsure attempt, or while a publication is
    /// out, the row stays as it is.
    #[gpui::test]
    fn cancel_leaves_the_row_of_an_unsure_attempt(cx: &mut gpui::TestAppContext) {
        let mut form = form(cx);
        assert_eq!(
            form.cancelled_row(),
            Some(Outcome::Failure("Role panel creation cancelled.".into()))
        );
        hold_sent(&form);
        assert_eq!(form.cancelled_row(), None, "an unsure attempt");
        release_sent(&form);
        form.busy = true;
        assert_eq!(form.cancelled_row(), None, "a publication still out");
    }

    /// While the window's editor is locked on a sent attempt, the reopened draft sent again is that editor's Publish: it
    /// adds no second row. Another request, or an editor no longer locked, runs as a command.
    #[gpui::test]
    fn a_resend_of_a_locked_editors_draft_is_its_publish(cx: &mut gpui::TestAppContext) {
        let editor = form(cx);
        let window = cx.add_empty_window();
        let w = window.update(|w, _| w.window_handle().window_id());
        let ctx = ctx(state(true, true, true));
        let same = editor.attempt();
        let other = Attempt {
            request: [8; 16],
            ..same.clone()
        };
        hold_sent(&editor);
        window.update(|_, cx| {
            slot(w, cx).form = Some(editor);
            assert!(routes_to_editor(w, &same, cx));
            assert!(!routes_to_editor(w, &other, cx), "another request");
            let at = Conversation {
                channel: ChannelId(CHANNEL),
                thread: None,
            };
            run(&ctx, at, same.clone(), w, cx);
            assert!(held_rows(w, cx).is_empty(), "no second row");
            release_sent(form_of(w, cx).unwrap());
            assert!(!routes_to_editor(w, &same, cx), "no longer locked");
        });
    }

    /// R20/R21: the editor and the stored attempt hold role names and emoji; their Debug says how many, never which.
    #[gpui::test]
    fn the_editor_keeps_role_names_and_emoji_out_of_debug(cx: &mut gpui::TestAppContext) {
        let form = form(cx);
        hold_sent(&form);
        let held = form.held().unwrap();
        let attempt = form.attempt();
        let args = Args::new();
        for shown in [
            format!("{form:?}"),
            format!("{form:#?}"),
            format!("{held:?}"),
            format!("{held:#?}"),
            format!("{attempt:?}"),
            format!("{args:?}"),
        ] {
            assert!(!shown.contains("secret"), "{shown}");
            assert!(!shown.contains("🎮"), "{shown}");
        }
    }
}
