//! [`ServerSettingsView`]: the frame of one server's settings (ServerSettings.tsx): Task 6's settings frame with its nav,
//! the open section, the footer of the dirty-draft guard and its unsaved-changes prompt.
//!
//! The nav is SettingsNavigation.tsx's: the person's own sections under "You", then the server's own under its name,
//! only those its permissions allow. A person's section hands the host a [`SettingsEvent::OpenUser`]; leaving a
//! section with an unsaved Overview draft, or closing, goes through [`Workspace::request`], which asks first.

use std::cell::RefCell;
use std::rc::Rc;
use std::sync::Arc;

use dilla_client::Client;
use dilla_client::state::{AppState, CommunityId};
use gpui::{
    AnyElement, App, AppContext as _, Context, Entity, EventEmitter, InteractiveElement,
    IntoElement, ParentElement, Render, SharedString, Styled, Subscription, Window, div,
};

use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::button::{Button, ButtonVariant};
use dilla_desktop_ui::components::confirm_dialog::{ConfirmDialog, Tone};
use dilla_desktop_ui::components::settings::footer::SettingsFooter;
use dilla_desktop_ui::components::settings::frame::SettingsFrame;
use dilla_desktop_ui::components::settings::nav::{NavItem, SettingsNav};

use super::body::{Body, BodyEvent, JoinRoleDraft, OverviewDraft};
use super::model;
use crate::edit_state::{Flags, Nav, Section, Workspace};
use crate::router::{ServerSection, UserSection};
use crate::screens::shell::Clock;
use crate::slots::{SectionId, SlotCx};
use crate::strings::t;

/// What the settings tell the window they sit in.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum SettingsEvent {
    /// The settings close (Escape, the close button, a server left or deleted).
    Close,
    /// A section of the person's own settings was chosen in the nav.
    OpenUser(UserSection),
    /// A success to say aloud.
    Announce(String),
}

impl EventEmitter<SettingsEvent> for ServerSettingsView {}

/// Where a request to leave the open section goes.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Leave {
    Server(ServerSection),
    User(UserSection),
    Close,
}

/// The Overview's draft as the workspace's section: Save only marks the request, [`Body::flush_save`] sends it.
struct OverviewSection(Rc<RefCell<OverviewDraft>>);

impl Section for OverviewSection {
    fn save(&mut self) -> bool {
        self.0.borrow_mut().save_requested = true;
        true
    }

    fn reset(&mut self) {
        self.0.borrow_mut().reset();
    }
}

/// The Join roles page's draft as the workspace's section (BotSection.tsx `botEditState`): Save marks the request.
struct JoinRoleSection(Rc<RefCell<JoinRoleDraft>>);

impl Section for JoinRoleSection {
    fn save(&mut self) -> bool {
        self.0.borrow_mut().save_requested = true;
        true
    }

    fn reset(&mut self) {
        self.0.borrow_mut().reset();
    }
}

pub struct ServerSettingsView {
    community: CommunityId,
    state: Arc<AppState>,
    client: Option<Rc<Client>>,
    clock: Clock,
    requested: Option<ServerSection>,
    body: Entity<Body>,
    workspace: Workspace<Leave>,
    /// The section whose draft is attached to the workspace.
    attached: Option<ServerSection>,
    /// The settings' root: it holds the focus when they open, so Tab and Escape reach them.
    focus: gpui::FocusHandle,
    /// The section on show at the last state (ServerSettings.tsx `previousActive`), and the requested section whose loss
    /// was already said (`announcedLossFor`).
    last_active: Option<ServerSection>,
    announced_loss: Option<ServerSection>,
    /// `workspace.permissionChanged`, waiting for the next frame to be read to a screen reader (ServerSettings.tsx's
    /// polite live region: never drawn).
    pending_announce: bool,
    _subscriptions: Vec<Subscription>,
}

/// What a change of the person's access does to open server settings (ServerSettings.tsx's access effect).
#[derive(Debug, Default, PartialEq, Eq)]
pub struct AccessChange {
    /// The section on show went (or the access is no longer read): the unsaved draft is dropped.
    pub clear: bool,
    /// `workspace.permissionChanged` is said: the access is no longer read, or the requested section is no longer
    /// allowed (once per section).
    pub announce: bool,
}

/// ServerSettings.tsx: compares the section on show before (`previous`) with what `access` allows now, for the
/// `requested` section whose loss was said already (`announced`).
pub fn access_change(
    previous: Option<ServerSection>,
    requested: Option<ServerSection>,
    announced: Option<ServerSection>,
    access: &dilla_client::state::server_admin::Access,
) -> AccessChange {
    if !access.loaded {
        return AccessChange {
            clear: previous.is_some(),
            announce: previous.is_some(),
        };
    }
    let allowed = |s: ServerSection| model::active_of(Some(s), access) == Some(s);
    let lost = previous.is_some_and(|p| !allowed(p));
    let requested_loss = requested.is_some_and(|r| !allowed(r) && announced != Some(r));
    AccessChange {
        clear: lost || requested_loss,
        announce: requested_loss,
    }
}

impl gpui::Focusable for ServerSettingsView {
    fn focus_handle(&self, _cx: &App) -> gpui::FocusHandle {
        self.focus.clone()
    }
}

/// settings/registry.ts `SETTINGS_ICONS` for the person's own sections.
fn user_icon(section: UserSection) -> IconName {
    match section {
        UserSection::Profile => IconName::User,
        UserSection::Signin => IconName::Lock,
        UserSection::Devices => IconName::MonitorSmartphone,
        UserSection::Appearance => IconName::Palette,
        UserSection::Notifications => IconName::Bell,
        UserSection::Voice => IconName::Mic,
        UserSection::Keyboard => IconName::Keyboard,
        UserSection::Privacy => IconName::Fingerprint,
        UserSection::Diagnostics => IconName::Activity,
    }
}

/// SettingsNavigation.tsx's order of the person's own sections: the prototype's six, then the rest.
fn user_sections() -> Vec<UserSection> {
    use UserSection::*;
    let first = [Profile, Signin, Devices, Notifications, Voice, Keyboard];
    first
        .iter()
        .copied()
        .chain(
            UserSection::ALL
                .iter()
                .copied()
                .filter(|s| !first.contains(s)),
        )
        .collect()
}

impl ServerSettingsView {
    pub fn new(
        community: CommunityId,
        state: Arc<AppState>,
        client: Option<Rc<Client>>,
        clock: Clock,
        requested: Option<ServerSection>,
        window: &mut Window,
        cx: &mut Context<Self>,
    ) -> Self {
        let body =
            cx.new(|cx| Body::new(community, state.clone(), client.clone(), clock, window, cx));
        let subscription = cx.subscribe_in(
            &body,
            window,
            |this: &mut Self, _, event: &BodyEvent, _window, cx| match event {
                BodyEvent::Announce(message) => cx.emit(SettingsEvent::Announce(message.clone())),
                BodyEvent::Gone => {
                    this.workspace.clear();
                    cx.emit(SettingsEvent::Close);
                }
                BodyEvent::OpenSection(section) => this.leave(Leave::Server(*section), cx),
            },
        );
        let last_active = {
            let access = dilla_client::state::server_admin::access_in(&state, community);
            access
                .loaded
                .then(|| model::active_of(requested, &access))
                .flatten()
        };
        Self {
            community,
            state,
            client,
            clock,
            requested,
            body,
            workspace: Workspace::new(),
            attached: None,
            focus: cx.focus_handle(),
            last_active,
            announced_loss: None,
            pending_announce: false,
            _subscriptions: vec![subscription],
        }
    }

    /// The window's newest state (the client's snapshot).
    pub fn set_state(&mut self, state: Arc<AppState>, cx: &mut Context<Self>) {
        self.state = state.clone();
        self.follow_access(cx);
        let client = self.client.clone();
        self.body.update(cx, |b, cx| b.sync(state, client, cx));
        cx.notify();
    }

    /// ServerSettings.tsx's access effect: the person's access changed while the settings are open. A section that went
    /// takes its unsaved draft with it, the view falls back to the first allowed section ([`Self::active`]), and a lost
    /// requested section or a lost read says `workspace.permissionChanged` (Package 10d).
    fn follow_access(&mut self, cx: &mut Context<Self>) {
        let access = dilla_client::state::server_admin::access_in(&self.state, self.community);
        let change = access_change(
            self.last_active,
            self.requested,
            self.announced_loss,
            &access,
        );
        if change.clear {
            self.workspace.clear();
            self.attached = None;
        }
        if change.announce {
            if access.loaded {
                self.announced_loss = self.requested;
            }
            self.pending_announce = true;
            cx.notify();
        } else if self
            .requested
            .is_none_or(|r| model::active_of(Some(r), &access) == Some(r))
        {
            self.announced_loss = None;
        }
        self.last_active = access
            .loaded
            .then(|| model::active_of(self.requested, &access))
            .flatten();
    }

    /// The server the settings are for.
    pub fn community(&self) -> CommunityId {
        self.community
    }

    /// The section on show, once the permissions are read.
    pub fn active(&self) -> Option<ServerSection> {
        let access = dilla_client::state::server_admin::access_in(&self.state, self.community);
        model::active_of(self.requested, &access)
    }

    /// Close, as the frame's close button and Escape ask it (an unsaved draft asks first).
    pub fn request_close(&mut self, cx: &mut Context<Self>) {
        self.leave(Leave::Close, cx);
    }

    /// A request to leave the open section: nothing to lose goes at once, an unsaved draft asks first.
    fn leave(&mut self, to: Leave, cx: &mut Context<Self>) {
        match self.workspace.request(to) {
            Nav::Go(to) => self.go(to, cx),
            Nav::Prompt => cx.notify(),
        }
    }

    fn go(&mut self, to: Leave, cx: &mut Context<Self>) {
        match to {
            Leave::Server(section) => {
                self.workspace.detach();
                self.attached = None;
                self.requested = Some(section);
            }
            Leave::User(section) => cx.emit(SettingsEvent::OpenUser(section)),
            Leave::Close => cx.emit(SettingsEvent::Close),
        }
        cx.notify();
    }

    fn nav(&self, active: Option<ServerSection>, cx: &mut Context<Self>) -> AnyElement {
        let access = dilla_client::state::server_admin::access_in(&self.state, self.community);
        let server_name = self
            .state
            .communities
            .iter()
            .find(|c| c.id == self.community)
            .map(|c| c.name.clone());
        let mut items: Vec<NavItem> = user_sections()
            .into_iter()
            .map(|s| {
                NavItem::new(s.id(), t(&format!("workspace.{}", s.id())))
                    .icon(user_icon(s))
                    .group(t("workspace.group.you"))
            })
            .collect();
        if let (Some(name), true) = (server_name, access.loaded) {
            items.extend(model::nav_sections(&access).into_iter().map(|s| {
                NavItem::new(format!("server:{}", s.id()), model::label_of(s))
                    .icon(model::icon_of(s))
                    .group(name.clone())
            }));
        }
        let active_id: SharedString = active
            .map(|s| format!("server:{}", s.id()))
            .unwrap_or_default()
            .into();
        let view = cx.entity().downgrade();
        SettingsNav::new("server-settings-nav", items, active_id)
            .label(t("settings.nav.label"))
            .on_select(move |id: &str, _window: &mut Window, app: &mut App| {
                let to = match id.strip_prefix("server:") {
                    Some(s) => ServerSection::from_id(s).map(Leave::Server),
                    None => UserSection::from_id(id).map(Leave::User),
                };
                if let Some(to) = to {
                    let _ = view.update(app, |this, cx| this.leave(to, cx));
                }
            })
            .into_any_element()
    }

    fn footer(&self, cx: &mut Context<Self>) -> Option<AnyElement> {
        let footer = self.workspace.footer()?;
        let view = cx.entity().downgrade();
        let (save_view, reset_view) = (view.clone(), view);
        Some(
            SettingsFooter::new("server-settings-footer")
                .status(footer.status)
                .action(
                    Button::new("settings-reset", t("settings.editing.reset"))
                        .disabled(!footer.reset_enabled)
                        .on_click(move |_, _window, app| {
                            let _ = reset_view.update(app, |this, cx| {
                                this.workspace.reset();
                                cx.notify();
                            });
                        }),
                )
                .action({
                    let save = Button::new("settings-save", t("settings.editing.save"))
                        .variant(ButtonVariant::Accent)
                        .disabled(!footer.save_enabled)
                        .on_click(move |_, window, app| {
                            let _ = save_view.update(app, |this, cx| {
                                this.workspace.save();
                                let body = this.body.clone();
                                body.update(cx, |b, cx| b.flush_save(window, cx));
                                cx.notify();
                            });
                        });
                    #[cfg(feature = "automation")]
                    let save = div()
                        .relative()
                        .child(save)
                        .child(crate::automation::bounds_probe("settings-save"));
                    save
                })
                .into_any_element(),
        )
    }

    fn prompt(&self, cx: &mut Context<Self>) -> AnyElement {
        let prompt = self.workspace.prompt();
        let open = prompt.is_some();
        let p = prompt.unwrap_or(crate::edit_state::Prompt {
            title: "",
            body: "",
            confirm_label: "",
            cancel_label: "",
        });
        let (keep_view, discard_view) = (cx.entity().downgrade(), cx.entity().downgrade());
        ConfirmDialog::new(
            "unsaved-changes",
            p.title,
            p.body,
            p.confirm_label,
            p.confirm_label,
            p.cancel_label,
        )
        .tone(Tone::Accent)
        .open(open)
        .on_confirm(move |_window, app| {
            let _ = discard_view.update(app, |this, cx| {
                for dest in this.workspace.discard() {
                    this.go(dest, cx);
                }
                cx.notify();
            });
        })
        .on_close(move |_window, app| {
            let _ = keep_view.update(app, |this, cx| {
                this.workspace.keep_editing();
                cx.notify();
            });
        })
        .into_any_element()
    }
}

/// Each window's open server settings, for a section drawn through a slot (Task 21's Bot and Music pages) that leads to
/// another section of the same settings.
#[derive(Default)]
struct OpenHosts(std::collections::HashMap<gpui::WindowId, gpui::WeakEntity<ServerSettingsView>>);

impl gpui::Global for OpenHosts {}

/// Shows `section` in this window's server settings, as the nav does (an unsaved draft asks first): the Bot page's
/// "Edit in Roles", the Music page's "Open Bot settings" (BotSection.tsx's `navigate`). `false`: no settings are open
/// in this window.
pub fn open_section(section: ServerSection, window: &mut Window, cx: &mut gpui::App) -> bool {
    let id = window.window_handle().window_id();
    let host = cx
        .try_global::<OpenHosts>()
        .and_then(|h| h.0.get(&id))
        .and_then(gpui::WeakEntity::upgrade);
    match host {
        Some(host) => {
            host.update(cx, |v, cx| v.leave(Leave::Server(section), cx));
            true
        }
        None => false,
    }
}

impl Render for ServerSettingsView {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        if std::mem::take(&mut self.pending_announce) {
            dilla_desktop_ui::a11y::announce(t("workspace.permissionChanged"), window, cx);
        }
        let me = cx.entity().downgrade();
        let hosts = &mut cx.default_global::<OpenHosts>().0;
        hosts.retain(|_, h| h.upgrade().is_some());
        hosts.insert(window.window_handle().window_id(), me);
        // The last drawn frame keeps these settings until the window draws again: release lets go of their client at
        // once (review M5).
        if let Some(client) = self.client.clone() {
            crate::release::hold(
                &client,
                &cx.entity(),
                |v: &mut Self, cx| {
                    v.client = None;
                    v.body.update(cx, |b, cx| b.sync(Arc::default(), None, cx));
                },
                cx,
            );
        }
        let active = self.active();
        // The Overview and Join roles keep drafts: each registers with the workspace while it is open.
        if self.attached.is_some() && self.attached != active {
            self.workspace.detach();
            self.attached = None;
        }
        match active {
            Some(ServerSection::Overview) => {
                let draft = self.body.read(cx).overview.clone();
                if self.attached.is_none() {
                    self.workspace
                        .attach(Box::new(OverviewSection(draft.clone())));
                    self.attached = active;
                }
                let d = draft.borrow();
                self.workspace.publish(Flags {
                    dirty: d.dirty(),
                    busy: d.busy,
                    can_save: d.dirty(),
                });
            }
            Some(ServerSection::JoinRoles) => {
                let draft = self.body.read(cx).join_role.clone();
                if self.attached.is_none() {
                    self.workspace
                        .attach(Box::new(JoinRoleSection(draft.clone())));
                    self.attached = active;
                }
                let d = draft.borrow();
                self.workspace.publish(Flags {
                    dirty: d.dirty(),
                    busy: d.busy,
                    can_save: d.can_save(),
                });
            }
            _ => {}
        }

        let nav = self.nav(active, cx);
        let footer = self.footer(cx);
        let close_view = cx.entity().downgrade();
        let mut frame = SettingsFrame::new("server-settings", t("settings.close"))
            .title(active.map_or_else(
                || t("admin.title").to_owned(),
                |s| model::label_of(s).to_string(),
            ))
            .nav(nav)
            .wide(active.is_some_and(model::wide))
            .on_close(move |_window, app| {
                let _ = close_view.update(app, |this, cx| this.leave(Leave::Close, cx));
            });
        if let Some(footer) = footer {
            frame = frame.footer(footer);
        }
        match active {
            None => {
                frame = frame.child(
                    div()
                        .child(dilla_desktop_ui::text::line(
                            t("admin.loading"),
                            dilla_desktop_ui::tokens::text::SM,
                            dilla_desktop_ui::text::line_height(
                                dilla_desktop_ui::tokens::text::SM,
                                1.5,
                            ),
                            dilla_desktop_ui::tokens::color::FG_3,
                        ))
                        .into_any_element(),
                );
            }
            Some(section) if owned(section) => {
                self.body.update(cx, |b, _| b.show(section));
                frame = frame.child(self.body.clone().into_any_element());
            }
            Some(section) => {
                let ctx = SlotCx {
                    state: self.state.clone(),
                    client: self.client.clone(),
                    community: Some(self.community),
                    channel: None,
                    clock: self.clock,
                    // The server settings keep their own guard (Overview's draft); a slot section has none here.
                    workspace: None,
                };
                let view = crate::slots::settings_sections()
                    .into_iter()
                    .find(|e| e.id == SectionId::Server(section))
                    .map(|e| (e.view)(SectionId::Server(section), &ctx, window, cx));
                if let Some(view) = view {
                    frame = frame.child(view);
                }
            }
        }
        div()
            .id("server-settings-root")
            .track_focus(&self.focus)
            .size_full()
            // Tab and Shift+Tab walk the settings as they walk the shell (`crate::screens::bind_keys`).
            .on_action(|_: &crate::screens::FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &crate::screens::FocusPrevious, window, cx| window.focus_prev(cx))
            .child(frame)
            .child(self.prompt(cx))
    }
}

/// The sections this area draws itself.
fn owned(section: ServerSection) -> bool {
    matches!(
        section,
        ServerSection::Overview
            | ServerSection::Channels
            | ServerSection::Gifs
            | ServerSection::Invites
            | ServerSection::Welcome
            | ServerSection::JoinRoles
            | ServerSection::Danger
    )
}
