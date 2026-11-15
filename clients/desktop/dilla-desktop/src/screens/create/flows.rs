//! The window's create, join and invite dialogs ([`CreateFlows`]): Create.tsx's `CreateServerFlow`, `CreateChannelFlow`
//! and `InviteFlow`, JoinCommunity.tsx and the AddServerDialog.
//!
//! One dialog at a time. Each open of a dialog is one session: work still running when it closes finishes, but no longer
//! speaks to the dialog (Create.tsx review I2), and a second press while a create runs does nothing (one server per
//! press).
//!
//! A new channel can be private from birth (the **private channel** switch and the roles that may see it, for a creator
//! who may manage roles), and an announcement channel takes the roles that may post in it (Create.tsx
//! `CreateChannelFlow`); the roles on offer are those below the creator's highest (all of them for the owner). A
//! create whose answer was lost is not adopted from the server list (WB m5): the person sees the refusal and the server
//! list shows what exists.

use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::signup::{
    self as su, CHANNEL_NAME_MAX_BYTES, FlowError, INVITE_MAX_USES, INVITE_TTL_DAYS, NameProblem,
    NewChannelKind, SERVER_NAME_MAX_BYTES,
};
use dilla_client::state::roles::{CommunityAccess, RoleId, RoleSummary};
use dilla_client::state::{AppState, ChannelId, CommunityId};
use gpui::{
    AppContext as _, ClipboardItem, Context, Entity, FocusHandle, Focusable, IntoElement,
    ParentElement, Render, SharedString, Styled, Subscription, Window, div,
};
use gpui_component::input::{InputEvent, InputState};

use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::check_list::{CheckList, CheckListItem};
use dilla_desktop_ui::components::dialog::{Dialog as DialogPanel, InitialFocus};
use dilla_desktop_ui::components::onboarding::{AddServerChoice, AddServerChoices, forms};
use dilla_desktop_ui::components::segmented::Segmented;
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::components::toggle::Toggle;
use dilla_desktop_ui::components::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::text::{line, line_height, paragraph};
use dilla_desktop_ui::tokens::{color, text};

use super::{Navigate, OnNavigate, Open};
use crate::strings::{t, tf};
use crate::{tracked_box, tracked_focus};

/// Create.tsx `PRIVATE_ROLES_MAX` and `ANNOUNCE_ROLES_MAX`: a create carries at most 100 overwrites (@everyone's and the
/// creator's own among them), so the role rows past these are disabled and the instance's refusal is unreachable.
pub const PRIVATE_ROLES_MAX: usize = 98;
pub const ANNOUNCE_ROLES_MAX: usize = 99;

/// The roles a creator may name in a new channel's overwrites (Create.tsx `offered` and `announceRoles`): every role but
/// @everyone that ranks below the creator's highest (all of them for the owner), highest first.
pub fn offered_roles<'a>(
    roles: &'a [RoleSummary],
    access: &CommunityAccess,
) -> Vec<&'a RoleSummary> {
    let mut out: Vec<&RoleSummary> = roles
        .iter()
        .filter(|r| Some(r.id) != access.everyone_id && (access.owner || r.position < access.top))
        .collect();
    out.sort_by_key(|r| std::cmp::Reverse(r.position));
    out
}

/// The check list's rows for `offered`, `ticked` checked; once `max` are ticked the others are disabled, with `full`
/// as their detail when given.
pub fn role_rows(
    offered: &[&RoleSummary],
    ticked: &[RoleId],
    max: usize,
    full: Option<&str>,
) -> Vec<CheckListItem> {
    offered
        .iter()
        .map(|r| {
            let checked = ticked.contains(&r.id);
            let disabled = !checked && ticked.len() >= max;
            let item = CheckListItem::new(role_key(&r.id), r.name.clone(), checked)
                .color(r.color)
                .disabled(disabled);
            match full {
                Some(detail) if disabled => item.detail(detail.to_owned()),
                _ => item,
            }
        })
        .collect()
}

fn role_key(id: &RoleId) -> String {
    id.iter().map(|b| format!("{b:02x}")).collect()
}

/// A tick (`on`) or untick of the row `key` in `list`: a tick goes last, once; a key not on offer changes nothing.
pub fn toggle_role(list: &mut Vec<RoleId>, offered: &[&RoleSummary], key: &str, on: bool) {
    let Some(id) = offered.iter().map(|r| r.id).find(|id| role_key(id) == key) else {
        return;
    };
    list.retain(|x| *x != id);
    if on {
        list.push(id);
    }
}

/// A link whose answer took longer than this is not copied by itself (Create.tsx `AUTO_COPY_MS`).
const AUTO_COPY_MS: u128 = 1000;

/// The dialog showing.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Dialog {
    AddServer,
    CreateServer,
    Join,
    CreateChannel,
    Invite,
}

/// A server this dialog made whose starter channels are not all there yet: the notice and its retry.
#[derive(Clone, Debug)]
struct Partial {
    community: CommunityId,
    name: String,
    /// #general (text) and the voice channel general, once made.
    made: [Option<ChannelId>; 2],
    code: String,
}

#[derive(Clone, Debug, PartialEq, Eq)]
enum InviteState {
    Creating,
    Ready(String),
    Failed(SharedString),
}

/// The starter channels, in order: #general, then the voice channel general.
const STARTERS: [(NewChannelKind, &str); 2] = [
    (NewChannelKind::Text, "general"),
    (NewChannelKind::Voice, "general"),
];

pub struct CreateFlows {
    dialog: Option<Dialog>,
    client: Option<Rc<Client>>,
    on_navigate: Option<OnNavigate>,
    server_name: Entity<InputState>,
    channel_name: Entity<InputState>,
    invite: Entity<InputState>,
    kind: NewChannelKind,
    community: Option<CommunityId>,
    parent: Option<ChannelId>,
    can_announce: bool,
    /// The shell's newest state ([`super::overlay`]): what the dialog reads roles and access from.
    state: std::sync::Arc<AppState>,
    /// The private-channel switch and the roles ticked under it; the roles ticked for an announcement channel.
    private: bool,
    ticked: Vec<RoleId>,
    announce_ticked: Vec<RoleId>,
    error: Option<SharedString>,
    busy: bool,
    partial: Option<Partial>,
    invite_state: InviteState,
    invite_status: Option<SharedString>,
    /// The invite the join dialog opened with (its refusal stays until the person types).
    prefilled: Option<String>,
    /// Counts the opens; work from an earlier one is ignored once its dialog closed.
    session: u64,
    first_choice: FocusHandle,
    _subscriptions: Vec<Subscription>,
}

impl CreateFlows {
    pub fn new(
        attached: Option<(Rc<Client>, OnNavigate)>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let server_name = cx.new(|cx| InputState::new(window, cx));
        let channel_name = cx.new(|cx| InputState::new(window, cx));
        let invite = cx.new(|cx| InputState::new(window, cx));
        let mut subscriptions = Vec::new();
        for input in [&server_name, &channel_name, &invite] {
            subscriptions.push(cx.subscribe_in(
                input,
                window,
                |this: &mut Self, _, e: &InputEvent, window, cx| {
                    match e {
                        InputEvent::PressEnter { .. } => this.submit(window, cx),
                        InputEvent::Change => {
                            // Typing clears the refusal; the prefilled invite landing in the field does not.
                            let prefilled = this
                                .prefilled
                                .as_deref()
                                .is_some_and(|p| this.invite.read(cx).value() == p);
                            if !prefilled {
                                this.error = None;
                            }
                            // The channel hint follows the name.
                            cx.notify();
                        }
                        _ => {}
                    }
                },
            ));
        }
        let (client, on_navigate) = attached.map_or((None, None), |(c, n)| (Some(c), Some(n)));
        Self {
            dialog: None,
            client,
            on_navigate,
            server_name,
            channel_name,
            invite,
            kind: NewChannelKind::Text,
            community: None,
            parent: None,
            can_announce: false,
            state: std::sync::Arc::default(),
            private: false,
            ticked: Vec::new(),
            announce_ticked: Vec::new(),
            error: None,
            busy: false,
            partial: None,
            invite_state: InviteState::Creating,
            invite_status: None,
            prefilled: None,
            session: 0,
            first_choice: cx.focus_handle().tab_stop(true),
            _subscriptions: subscriptions,
        }
    }

    pub fn attach(&mut self, client: Rc<Client>, on_navigate: OnNavigate) {
        self.client = Some(client);
        self.on_navigate = Some(on_navigate);
    }

    pub fn is_open(&self) -> bool {
        self.dialog.is_some()
    }

    pub fn dialog(&self) -> Option<Dialog> {
        self.dialog
    }

    /// The refusal the open dialog shows, if any.
    pub fn refusal(&self) -> Option<SharedString> {
        self.error.clone()
    }

    /// The join dialog's field as it stands.
    pub fn invite_value(&self, cx: &gpui::App) -> String {
        self.invite.read(cx).value().to_string()
    }

    fn community_name(&self, community: CommunityId) -> String {
        let held = |s: &AppState| {
            s.communities
                .iter()
                .find(|c| c.id == community)
                .map(|c| c.name.clone())
        };
        held(&self.state)
            .or_else(|| self.client.as_ref().and_then(|c| held(&c.state().borrow())))
            .unwrap_or_default()
    }

    /// The shell's newest state (the create-channel dialog's roles and access).
    pub fn set_state(&mut self, state: std::sync::Arc<AppState>) {
        self.state = state;
    }

    /// Whether the announcement kind is offered: the instance's answer, or the roles slice's access (Create.tsx
    /// `canCreateAnnouncement`: manage channels and manage roles), which is all a fixture has.
    fn can_announce(&self) -> bool {
        self.can_announce
            || self.community.is_some_and(|c| {
                let a = self.state.roles.access_of(c);
                a.loaded && a.manage_channels && a.manage_roles
            })
    }

    pub fn open(&mut self, which: Open, window: &mut Window, cx: &mut Context<Self>) {
        self.close_quietly(window, cx);
        match which {
            Open::AddServer => self.dialog = Some(Dialog::AddServer),
            Open::CreateServer => self.dialog = Some(Dialog::CreateServer),
            Open::Join { invite, refusal } => {
                self.error = refusal.map(Into::into);
                self.dialog = Some(Dialog::Join);
                self.prefilled.clone_from(&invite);
                if let Some(code) = invite {
                    self.invite
                        .update(cx, |s, cx| s.set_value(code, window, cx));
                }
            }
            Open::CreateChannel { community, parent } => {
                self.dialog = Some(Dialog::CreateChannel);
                self.community = Some(community);
                self.parent = parent;
                self.check_announce(community, cx);
            }
            Open::Invite { community } => {
                self.dialog = Some(Dialog::Invite);
                self.community = Some(community);
                self.make_invite(community, window, cx);
            }
        }
        cx.notify();
    }

    /// Closes the dialog and forgets what it held (each open starts empty).
    fn close_quietly(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.session += 1;
        self.dialog = None;
        self.error = None;
        self.partial = None;
        self.busy = false;
        self.kind = NewChannelKind::Text;
        self.private = false;
        self.ticked.clear();
        self.announce_ticked.clear();
        self.invite_state = InviteState::Creating;
        self.invite_status = None;
        self.prefilled = None;
        for input in [&self.server_name, &self.channel_name, &self.invite] {
            input.update(cx, |s, cx| s.set_value("", window, cx));
        }
    }

    pub fn close(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        self.close_quietly(window, cx);
        cx.notify();
    }

    fn navigate(&self, to: Navigate, window: &mut Window, cx: &mut Context<Self>) {
        if let Some(f) = self.on_navigate.clone() {
            f(to, window, cx);
        }
    }

    fn fail(&mut self, text: impl Into<SharedString>, window: &mut Window, cx: &mut Context<Self>) {
        self.error = Some(text.into());
        // A refusal puts focus back in the field that holds it.
        let field = match self.dialog {
            Some(Dialog::CreateServer) => Some(&self.server_name),
            Some(Dialog::CreateChannel) => Some(&self.channel_name),
            Some(Dialog::Join) => Some(&self.invite),
            _ => None,
        };
        if let Some(field) = field {
            field.update(cx, |s, cx| s.focus(window, cx));
        }
        cx.notify();
    }

    pub fn submit(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.busy {
            return;
        }
        match self.dialog {
            Some(Dialog::CreateServer) if self.partial.is_some() => self.retry_starters(window, cx),
            Some(Dialog::CreateServer) => self.create_server(window, cx),
            Some(Dialog::CreateChannel) => self.create_channel(window, cx),
            Some(Dialog::Join) => self.join(window, cx),
            _ => {}
        }
    }

    fn no_client(&mut self, window: &mut Window, cx: &mut Context<Self>) -> Option<Rc<Client>> {
        let client = self.client.clone();
        if client.is_none() {
            self.fail(
                tf("create.server.error.other", &[("code", "E_NOT_READY")]),
                window,
                cx,
            );
        }
        client
    }

    // ---- start a server ----

    fn create_server(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let name = self.server_name.read(cx).value().trim().to_owned();
        if let Some(problem) = su::name_problem(&name, SERVER_NAME_MAX_BYTES) {
            let key = match problem {
                NameProblem::Empty => "create.server.error.empty",
                NameProblem::Long => "create.server.error.long",
                NameProblem::Control => "create.name.control",
            };
            return self.fail(t(key), window, cx);
        }
        let Some(client) = self.no_client(window, cx) else {
            return;
        };
        let flow = client.onboarding();
        let at = self.session;
        self.busy = true;
        cx.notify();
        cx.spawn_in(window, async move |this, cx| {
            let made = flow.create_server(&name).await;
            let _ = this.update_in(cx, |this, window, cx| {
                if this.session != at {
                    return;
                }
                match made {
                    Ok(community) => {
                        client.refresh();
                        let partial = Partial {
                            community,
                            name: name.clone(),
                            made: [None, None],
                            code: String::new(),
                        };
                        // The shell selects the new server at once.
                        this.navigate(
                            Navigate {
                                community,
                                channel: None,
                            },
                            window,
                            cx,
                        );
                        this.add_starters(partial, window, cx);
                    }
                    Err(e) => {
                        this.busy = false;
                        let text = if matches!(e.code.as_str(), "E_INVALID_REQUEST" | "E_BAD_INPUT")
                        {
                            t("create.server.error.refused").to_owned()
                        } else {
                            tf("create.server.error.other", &[("code", &e.code)])
                        };
                        this.fail(text, window, cx);
                    }
                }
            });
        })
        .detach();
    }

    /// The missing starters of `partial`, in order; then the shell opens #general, or the notice says what is missing.
    fn add_starters(&mut self, partial: Partial, window: &mut Window, cx: &mut Context<Self>) {
        let Some(client) = self.client.clone() else {
            return;
        };
        let flow = client.onboarding();
        let at = self.session;
        self.busy = true;
        cx.notify();
        cx.spawn_in(window, async move |this, cx| {
            let mut partial = partial;
            let mut failure: Option<FlowError> = None;
            for (i, (kind, name)) in STARTERS.iter().enumerate() {
                if partial.made[i].is_some() {
                    continue;
                }
                match flow
                    .create_channel(partial.community, *kind, name, None, &[])
                    .await
                {
                    Ok(id) => partial.made[i] = Some(id),
                    Err(e) => {
                        failure.get_or_insert(e);
                    }
                }
            }
            let _ = this.update_in(cx, |this, window, cx| {
                client.refresh();
                if this.session != at {
                    return;
                }
                this.busy = false;
                match (failure, partial.made[0]) {
                    (None, Some(general)) => {
                        let community = partial.community;
                        this.close(window, cx);
                        this.navigate(
                            Navigate {
                                community,
                                channel: Some(general),
                            },
                            window,
                            cx,
                        );
                    }
                    (failure, _) => {
                        partial.code =
                            failure.map_or_else(|| "E_BAD_RESULT".to_owned(), |e| e.code);
                        this.partial = Some(partial);
                        cx.notify();
                    }
                }
            });
        })
        .detach();
    }

    /// The notice's retry: a starter the list already shows counts as made (dillad keeps no uniqueness on channel
    /// names, and a create whose answer was lost may have made it), then only the missing ones are created.
    fn retry_starters(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let (Some(mut partial), Some(client)) = (self.partial.clone(), self.client.clone()) else {
            return;
        };
        {
            let state = client.state().borrow().clone();
            for (i, (kind, name)) in STARTERS.iter().enumerate() {
                if partial.made[i].is_none()
                    && let Some(listed) = state.channels.iter().find(|c| {
                        c.community_id == partial.community
                            && c.kind as u64 == kind.wire()
                            && c.name == *name
                    })
                {
                    partial.made[i] = Some(listed.id);
                }
            }
        }
        self.add_starters(partial, window, cx);
    }

    fn partial_text(p: &Partial) -> String {
        let missing: Vec<usize> = (0..2).filter(|i| p.made[*i].is_none()).collect();
        let key = match missing.as_slice() {
            [0] => "create.server.partialText",
            [1] => "create.server.partialVoice",
            _ => "create.server.partial",
        };
        tf(key, &[("name", &p.name), ("code", &p.code)])
    }

    // ---- a new channel ----

    fn check_announce(&mut self, community: CommunityId, cx: &mut Context<Self>) {
        self.can_announce = false;
        let Some(client) = self.client.clone() else {
            return;
        };
        let flow = client.onboarding();
        let at = self.session;
        // The roles and the creator's access (the `permissions:` and `roles:` slices), for the switch and the role lists.
        let read = client
            .roles()
            .run(move |r| async move { r.load_access(community).await });
        cx.spawn(async move |this, cx| {
            let _ = read.await;
            let _ = this.update(cx, |_, cx| cx.notify());
        })
        .detach();
        cx.spawn(async move |this, cx| {
            let can = flow.can_announce(community).await;
            let _ = this.update(cx, |this, cx| {
                if this.session == at {
                    this.can_announce = can;
                    cx.notify();
                }
            });
        })
        .detach();
    }

    fn kind(&self) -> NewChannelKind {
        // A chosen announcement kind falls back to text when the right to create one is lost.
        if self.kind == NewChannelKind::Announcement && !self.can_announce() {
            NewChannelKind::Text
        } else {
            self.kind
        }
    }

    fn create_channel(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(community) = self.community else {
            return;
        };
        let kind = self.kind();
        let shown = su::channel_name(kind, &self.channel_name.read(cx).value());
        if let Some(problem) = su::name_problem(&shown, CHANNEL_NAME_MAX_BYTES) {
            let key = match problem {
                NameProblem::Empty => "create.channel.error.empty",
                NameProblem::Long => "create.channel.error.long",
                NameProblem::Control => "create.name.control",
            };
            return self.fail(t(key), window, cx);
        }
        let Some(client) = self.no_client(window, cx) else {
            return;
        };
        // dillad keeps no uniqueness on channel names: a name this server already lists for the kind is refused here.
        let taken = client.state().borrow().channels.iter().any(|c| {
            c.community_id == community && c.kind as u64 == kind.wire() && c.name == shown
        });
        if taken {
            let key = if kind == NewChannelKind::Voice {
                "create.channel.error.takenVoice"
            } else {
                "create.channel.error.takenText"
            };
            return self.fail(tf(key, &[("name", &shown)]), window, cx);
        }
        let flow = client.onboarding();
        let parent = self.parent;
        let at = self.session;
        // Only ticks still on offer are sent; a private create needs the manage-roles right and is never an announcement.
        let (private, announce) = {
            let state = client.state().borrow().clone();
            let (offered, access) = Self::offered(&state, community);
            let keep = |list: &[RoleId]| -> Vec<RoleId> {
                list.iter()
                    .copied()
                    .filter(|id| offered.iter().any(|r| r.id == *id))
                    .collect()
            };
            let private =
                (access.manage_roles && self.private && kind != NewChannelKind::Announcement)
                    .then(|| keep(&self.ticked));
            let announce = if kind == NewChannelKind::Announcement {
                keep(&self.announce_ticked)
            } else {
                Vec::new()
            };
            (private, announce)
        };
        self.busy = true;
        cx.notify();
        cx.spawn_in(window, async move |this, cx| {
            let made = flow
                .create_channel_with(
                    community,
                    kind,
                    &shown,
                    parent,
                    &announce,
                    private.as_deref(),
                )
                .await;
            let _ = this.update_in(cx, |this, window, cx| {
                client.refresh();
                if this.session != at {
                    return;
                }
                this.busy = false;
                match made {
                    Ok(channel) => {
                        this.close(window, cx);
                        this.navigate(
                            Navigate {
                                community,
                                channel: (kind != NewChannelKind::Voice).then_some(channel),
                            },
                            window,
                            cx,
                        );
                    }
                    Err(e) => {
                        let text = match e.code.as_str() {
                            "E_FORBIDDEN" if kind == NewChannelKind::Announcement => {
                                t("announce.roleTooHigh").to_owned()
                            }
                            "E_FORBIDDEN" => t("create.channel.error.forbidden").to_owned(),
                            "E_INVALID_REQUEST" => t("create.channel.error.refused").to_owned(),
                            code => tf("create.channel.error.other", &[("code", code)]),
                        };
                        this.fail(text, window, cx);
                    }
                }
            });
        })
        .detach();
    }

    /// The roles on offer in `community` and the creator's access, from the roles slice.
    fn offered(state: &AppState, community: CommunityId) -> (Vec<RoleSummary>, CommunityAccess) {
        let access = state.roles.access_of(community);
        let offered = offered_roles(state.roles.roles_of(community), &access)
            .into_iter()
            .cloned()
            .collect();
        (offered, access)
    }

    /// Under the name: an announcement channel's posting roles and their hint; else, for a creator who may add channels
    /// but not manage roles, why there is no announcement kind; else, for one who may manage roles, the private switch
    /// and, when it is on, the roles that may see the channel (Create.tsx's children of `CreateChannel`).
    fn channel_access(&self, width: f32, cx: &mut Context<Self>) -> Vec<gpui::AnyElement> {
        let Some(community) = self.community else {
            return Vec::new();
        };
        let state = self.state.clone();
        let (offered, access) = Self::offered(&state, community);
        let refs: Vec<&RoleSummary> = offered.iter().collect();
        // `.d-select__hint`: `--text-sm`, `--fg-3`.
        let hint = |text_: &str| {
            div()
                .flex()
                .flex_col()
                .children(paragraph(
                    text_,
                    text::SM,
                    line_height(text::SM, 1.5),
                    color::FG_3,
                    width,
                ))
                .into_any_element()
        };
        let mut out = Vec::new();
        if self.kind() == NewChannelKind::Announcement {
            let rows = role_rows(&refs, &self.announce_ticked, ANNOUNCE_ROLES_MAX, None);
            let this = cx.entity().downgrade();
            let keys = offered.clone();
            out.push(
                tracked_box(
                    "create-channel-announce-roles",
                    CheckList::new(
                        "create-channel-announce-roles",
                        t("announce.roles"),
                        rows,
                        t("announce.noRoles"),
                    )
                    .on_toggle(move |id, on, _, cx| {
                        let offered: Vec<&RoleSummary> = keys.iter().collect();
                        let _ = this.update(cx, |this, cx| {
                            toggle_role(&mut this.announce_ticked, &offered, id, on);
                            cx.notify();
                        });
                    }),
                )
                .into_any_element(),
            );
            out.push(hint(t("announce.hint")));
            return out;
        }
        if access.loaded && !self.can_announce() && access.manage_channels && !access.manage_roles {
            out.push(hint(t("announce.needsRoles")));
            return out;
        }
        if !access.manage_roles {
            return out;
        }
        let this = cx.entity().downgrade();
        out.push(
            tracked_box(
                "create-channel-private",
                Toggle::new(
                    "create-channel-private",
                    t("chperm.create.private"),
                    self.private,
                )
                .show_label(true)
                .hint(t("chperm.create.privateHint"))
                .on_change(move |on, _, cx| {
                    let _ = this.update(cx, |this, cx| {
                        this.private = on;
                        this.error = None;
                        cx.notify();
                    });
                }),
            )
            .into_any_element(),
        );
        if self.private {
            let rows = role_rows(
                &refs,
                &self.ticked,
                PRIVATE_ROLES_MAX,
                Some(t("chperm.create.tooMany")),
            );
            let this = cx.entity().downgrade();
            let keys = offered.clone();
            out.push(
                tracked_box(
                    "create-channel-private-roles",
                    CheckList::new(
                        "create-channel-private-roles",
                        t("chperm.create.roles"),
                        rows,
                        t("chperm.create.noRoles"),
                    )
                    .on_toggle(move |id, on, _, cx| {
                        let offered: Vec<&RoleSummary> = keys.iter().collect();
                        let _ = this.update(cx, |this, cx| {
                            toggle_role(&mut this.ticked, &offered, id, on);
                            cx.notify();
                        });
                    }),
                )
                .into_any_element(),
            );
        }
        out
    }

    fn channel_hint(&self, cx: &gpui::App) -> String {
        let kind = self.kind();
        let shown = su::channel_name(kind, &self.channel_name.read(cx).value());
        if shown.is_empty() {
            t("create.channel.hint").to_owned()
        } else if kind == NewChannelKind::Voice {
            tf("create.channel.hintVoice", &[("name", &shown)])
        } else {
            tf("create.channel.hintText", &[("name", &shown)])
        }
    }

    // ---- join a server ----

    fn join(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let code = su::extract_invite_code(&self.invite.read(cx).value());
        if code.is_empty() {
            return self.fail(t("join.error.empty"), window, cx);
        }
        let Some(client) = self.no_client(window, cx) else {
            return;
        };
        let flow = client.onboarding();
        let at = self.session;
        self.busy = true;
        cx.notify();
        cx.spawn_in(window, async move |this, cx| {
            let joined = flow.join(&code).await;
            let _ = this.update_in(cx, |this, window, cx| {
                client.refresh();
                if this.session != at {
                    return;
                }
                this.busy = false;
                match joined {
                    Ok(community) => {
                        this.close(window, cx);
                        this.navigate(
                            Navigate {
                                community,
                                channel: None,
                            },
                            window,
                            cx,
                        );
                    }
                    Err(e) => this.fail(join_refusal(&e.code), window, cx),
                }
            });
        })
        .detach();
    }

    // ---- the invite link ----

    fn make_invite(&mut self, community: CommunityId, window: &mut Window, cx: &mut Context<Self>) {
        self.invite_state = InviteState::Creating;
        self.invite_status = None;
        let Some(client) = self.client.clone() else {
            self.invite_state =
                InviteState::Failed(tf("invite.error.other", &[("code", "E_NOT_READY")]).into());
            return;
        };
        let now = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map_or(0, |d| d.as_secs());
        let held = client
            .state()
            .borrow()
            .onboarding
            .reusable_invite(community, now, INVITE_TTL_DAYS)
            .map(|i| i.url.clone());
        if let Some(url) = held {
            self.invite_state = InviteState::Ready(url.clone());
            self.copy(&url, cx);
            return;
        }
        let flow = client.onboarding();
        let at = self.session;
        let opened = std::time::Instant::now();
        cx.spawn_in(window, async move |this, cx| {
            let made = flow.create_invite(community).await;
            let _ = this.update_in(cx, |this, _, cx| {
                if this.session != at {
                    return;
                }
                match made {
                    Ok(invite) => {
                        this.invite_state = InviteState::Ready(invite.url.clone());
                        if opened.elapsed().as_millis() <= AUTO_COPY_MS {
                            this.copy(&invite.url, cx);
                        }
                    }
                    Err(e) => {
                        this.invite_state = InviteState::Failed(
                            if e.code == "E_FORBIDDEN" {
                                t("invite.error.forbidden").to_owned()
                            } else {
                                tf("invite.error.other", &[("code", &e.code)])
                            }
                            .into(),
                        );
                    }
                }
                cx.notify();
            });
        })
        .detach();
    }

    fn copy(&mut self, url: &str, cx: &mut Context<Self>) {
        cx.write_to_clipboard(ClipboardItem::new_string(url.to_owned()));
        self.invite_status = Some(t("invite.copied").into());
        cx.notify();
    }

    fn invite_days(&self) -> u64 {
        INVITE_TTL_DAYS
    }
}

/// JoinCommunity.tsx `refusalText`.
pub fn join_refusal(code: &str) -> String {
    match code {
        "E_INVITE_INVALID" => t("join.error.invalid").to_owned(),
        "E_INVITE_NOT_COMMUNITY" => t("join.error.notCommunity").to_owned(),
        "E_FORBIDDEN" => t("join.error.forbidden").to_owned(),
        "E_INVALID_REQUEST" => t("join.error.malformed").to_owned(),
        other => tf("join.error.other", &[("code", other)]),
    }
}

fn accent(id: &'static str, label: impl Into<SharedString>, busy: bool) -> Button {
    Button::new(id, label)
        .variant(ButtonVariant::Accent)
        .size(ButtonSize::Dialog)
        .disabled(busy)
}

/// A paragraph of a dialog's body, at the dialog's text.
/// A paragraph of a dialog's body: `.d-dialog__body`'s `--text-ui`.
fn body_text(text_: &str, width: f32) -> impl IntoElement {
    div().flex().flex_col().children(paragraph(
        text_,
        text::UI,
        line_height(text::UI, 1.5),
        color::FG_2,
        width,
    ))
}

impl Render for CreateFlows {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        let Some(dialog) = self.dialog else {
            return div().into_any_element();
        };
        let body_w = forms::body_width(f32::from(window.viewport_size().width));
        let on_close = {
            let this = cx.entity().downgrade();
            move |window: &mut Window, cx: &mut gpui::App| {
                let _ = this.update(cx, |this, cx| this.close(window, cx));
            }
        };
        let submit = cx.listener(|this, _: &gpui::ClickEvent, window, cx| this.submit(window, cx));
        let busy = self.busy;
        let panel = match dialog {
            Dialog::AddServer => {
                let join = tracked_focus(
                    "add-server-join",
                    &self.first_choice,
                    AddServerChoice::new("add-server-join", t("layout.add.join"))
                        .focus(&self.first_choice)
                        .on_click(cx.listener(|this, _, window, cx| {
                            this.open(
                                Open::Join {
                                    invite: None,
                                    refusal: None,
                                },
                                window,
                                cx,
                            );
                        })),
                );
                let create = tracked_box(
                    "add-server-create",
                    AddServerChoice::new("add-server-create", t("layout.add.create")).on_click(
                        cx.listener(|this, _, window, cx| {
                            this.open(Open::CreateServer, window, cx)
                        }),
                    ),
                );
                DialogPanel::new("add-server", t("layout.add.title"), t("dialog.close"))
                    .initial_focus(InitialFocus::Handle(self.first_choice.clone()))
                    .body(AddServerChoices::new(join, create))
                    .on_close(on_close)
            }
            Dialog::CreateServer => {
                let action = if busy {
                    t("create.server.busy")
                } else if self.partial.is_some() {
                    t("create.server.retry")
                } else {
                    t("create.server.submit")
                };
                let field_focus = self.server_name.focus_handle(cx);
                let body = match &self.partial {
                    Some(p) => div()
                        .child(tracked_box(
                            "create-server-partial",
                            Banner::new(
                                "create-server-partial",
                                BannerTone::Warn,
                                Self::partial_text(p),
                            ),
                        ))
                        .into_any_element(),
                    None => div()
                        .flex()
                        .flex_col()
                        .gap(forms::SERVER_GAP)
                        // `.d-create-server__body { max-width: 52ch }`.
                        .child(body_text(
                            t("create.server.body"),
                            body_w.min(
                                52.0 * f32::from(text::UI) * dilla_desktop_ui::text::MONO_ADVANCE,
                            ),
                        ))
                        .child(tracked_focus(
                            "create-server-name",
                            &field_focus,
                            div().w_full().child({
                                let mut f = TextField::new(
                                    "create-server-name-field",
                                    t("create.server.name"),
                                    &self.server_name,
                                )
                                .hint(t("create.server.hint"));
                                if let Some(e) = self.error.clone() {
                                    f = f.error(e);
                                }
                                f
                            }),
                        ))
                        .into_any_element(),
                };
                DialogPanel::new("create-server", t("create.server.title"), t("dialog.close"))
                    .initial_focus(InitialFocus::Handle(field_focus))
                    .body(body)
                    .footer(tracked_box(
                        "create-server-submit",
                        accent("create-server-submit", action, busy).on_click(submit),
                    ))
                    .on_close(on_close)
            }
            Dialog::Join => {
                let field_focus = self.invite.focus_handle(cx);
                let mut field = TextField::new("join-invite-field", t("join.invite"), &self.invite)
                    .hint(t("join.inviteHint"));
                if let Some(e) = self.error.clone() {
                    field = field.error(e);
                }
                DialogPanel::new("join", t("join.title"), t("dialog.close"))
                    .initial_focus(InitialFocus::Handle(field_focus.clone()))
                    .body(tracked_focus(
                        "join-invite",
                        &field_focus,
                        div().w_full().child(field),
                    ))
                    .footer(tracked_box(
                        "join-submit",
                        accent(
                            "join-submit",
                            if busy {
                                t("join.joining")
                            } else {
                                t("join.submit")
                            },
                            busy,
                        )
                        .on_click(submit),
                    ))
                    .on_close(on_close)
            }
            Dialog::CreateChannel => {
                let server = self
                    .community
                    .map(|c| self.community_name(c))
                    .unwrap_or_default();
                let field_focus = self.channel_name.focus_handle(cx);
                let mut kinds: Vec<(&str, &str)> = vec![
                    ("text", t("create.channel.text")),
                    ("voice", t("create.channel.voice")),
                ];
                if self.can_announce() {
                    kinds.push(("announcement", t("announce.kind")));
                }
                let value = match self.kind() {
                    NewChannelKind::Voice => "voice",
                    NewChannelKind::Announcement => "announcement",
                    _ => "text",
                };
                let this = cx.entity().downgrade();
                let segmented = Segmented::new("create-channel-kind", &kinds, value).on_change(
                    move |v, _, cx| {
                        let kind = match v {
                            "voice" => NewChannelKind::Voice,
                            "announcement" => NewChannelKind::Announcement,
                            _ => NewChannelKind::Text,
                        };
                        let _ = this.update(cx, |this, cx| {
                            this.kind = kind;
                            this.error = None;
                            cx.notify();
                        });
                    },
                );
                let mut field = TextField::new(
                    "create-channel-name-field",
                    t("create.channel.name"),
                    &self.channel_name,
                )
                .hint(self.channel_hint(cx));
                if let Some(e) = self.error.clone() {
                    field = field.error(e);
                }
                let mut body = div()
                    .flex()
                    .flex_col()
                    .gap(forms::CHANNEL_GAP)
                    // `.d-create-channel__kind`: the group alone, at the start (`justify-items: start`); the kind's label
                    // names the radio group for a screen reader and is not drawn (CreateChannel.tsx).
                    .child(div().flex().flex_col().items_start().child(tracked_box(
                        "create-channel-kind",
                        segmented.label(t("create.channel.kind")),
                    )))
                    .child(tracked_focus(
                        "create-channel-name",
                        &field_focus,
                        div().w_full().child(field),
                    ));
                body = body.children(self.channel_access(body_w, cx));
                DialogPanel::new(
                    "create-channel",
                    tf("create.channel.title", &[("server", &server)]),
                    t("dialog.close"),
                )
                .initial_focus(InitialFocus::Handle(field_focus))
                .body(body)
                .footer(tracked_box(
                    "create-channel-submit",
                    accent(
                        "create-channel-submit",
                        if busy {
                            t("create.channel.busy")
                        } else {
                            t("create.channel.submit")
                        },
                        busy,
                    )
                    .on_click(submit),
                ))
                .on_close(on_close)
            }
            Dialog::Invite => {
                let server = self
                    .community
                    .map(|c| self.community_name(c))
                    .unwrap_or_default();
                let mut body = div().flex().flex_col().gap(forms::SERVER_GAP);
                let mut copy_button = None;
                match &self.invite_state {
                    InviteState::Creating => {
                        body = body.child(body_text(t("invite.creating"), body_w))
                    }
                    InviteState::Failed(e) => {
                        body = body.child(tracked_box(
                            "invite-error",
                            Banner::new("invite-error", BannerTone::Danger, e.clone()),
                        ));
                    }
                    InviteState::Ready(url) => {
                        body = body
                            .child(line(
                                t("invite.link"),
                                text::XS,
                                line_height(text::XS, 1.5),
                                color::FG_2,
                            ))
                            .child(tracked_box(
                                "invite-link",
                                line(
                                    url.clone(),
                                    text::INPUT,
                                    line_height(text::INPUT, 1.5),
                                    color::FG,
                                )
                                .ellipsis(),
                            ))
                            .child(body_text(
                                &tf(
                                    "invite.limits",
                                    &[
                                        ("uses", &INVITE_MAX_USES.to_string()),
                                        ("days", &self.invite_days().to_string()),
                                        ("server", &server),
                                    ],
                                ),
                                body_w,
                            ));
                        if let Some(status) = self.invite_status.clone() {
                            body = body.child(line(
                                status,
                                text::SM,
                                line_height(text::SM, 1.5),
                                color::FG_3,
                            ));
                        }
                        let url = url.clone();
                        copy_button = Some(tracked_box(
                            "invite-copy",
                            accent("invite-copy", t("invite.copy"), false)
                                .on_click(cx.listener(move |this, _, _, cx| this.copy(&url, cx))),
                        ));
                    }
                }
                let mut panel = DialogPanel::new(
                    "invite",
                    tf("invite.title", &[("server", &server)]),
                    t("dialog.close"),
                )
                .initial_focus(InitialFocus::Close)
                .body(body)
                .on_close(on_close);
                if let InviteState::Failed(_) = self.invite_state {
                    panel = panel.footer(tracked_box(
                        "invite-retry",
                        accent("invite-retry", t("invite.retry"), false).on_click(cx.listener(
                            |this, _, window, cx| {
                                if let Some(c) = this.community {
                                    this.make_invite(c, window, cx);
                                    cx.notify();
                                }
                            },
                        )),
                    ));
                }
                if let Some(b) = copy_button {
                    panel = panel.footer(b);
                }
                panel
            }
        };
        crate::tracked("create-dialog", panel.can_close(true)).into_any_element()
    }
}
