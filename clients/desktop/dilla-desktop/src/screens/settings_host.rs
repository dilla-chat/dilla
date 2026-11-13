//! The user settings host (Task 1b): what draws `/settings/<section>`. Task 6's settings frame with the nav of the user
//! sections in registry order (`Settings.tsx` + `SettingsNavigation.tsx`), the selected section's body from its owning
//! area's hook (`slots::settings_sections`), the dirty-draft guard (`edit_state`) in front of every way out, and the
//! footer and the unsaved-changes prompt it drives. No section's content lives here.
//!
//! **Sections and the guard.** A section that has a draft registers its operations with the open [`Workspace`] (see
//! `SlotCx::workspace`, one per host) from its hook and publishes its flags; leaving through the nav, Escape or the close button goes through
//! [`Workspace::request`]. The host detaches the section when another one opens.
//!
//! **Routes.** The host does not own history: it emits [`SettingsEvent::Navigate`] with the route to show next (the
//! chosen section's `/settings/<id>`, or the route the settings were opened from on close), and the app applies it.

use std::cell::RefCell;
use std::rc::Rc;

use gpui::{
    AppContext as _, Context, EventEmitter, InteractiveElement, IntoElement, ParentElement, Render,
    Styled, Window, div,
};

use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::confirm_dialog::{ConfirmDialog, Tone};
use dilla_desktop_ui::components::settings::footer::SettingsFooter;
use dilla_desktop_ui::components::settings::frame::SettingsFrame;
use dilla_desktop_ui::components::settings::nav::{NavItem, SettingsNav};
use dilla_desktop_ui::components::{Button, ButtonVariant};

use crate::edit_state::{Nav, Workspace};
use crate::router::{Route, UserSection};
use crate::slots::{self, SectionId, SlotCx};
use crate::strings::t;

/// Where a request to leave the open section is headed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Dest {
    /// Another section of the user settings.
    Section(UserSection),
    /// Out of the settings, to the route they were opened from.
    Close,
}

/// The guard of the open settings host, as the sections see it.
pub type SettingsWorkspace = Rc<RefCell<Workspace<Dest>>>;

/// What the host asks of the app.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum SettingsEvent {
    Navigate(Route),
}

/// The section's nav glyph (SETTINGS_ICONS).
fn icon(section: UserSection) -> IconName {
    match section {
        UserSection::Profile => IconName::User,
        UserSection::Signin => IconName::Lock,
        UserSection::Devices => IconName::MonitorSmartphone,
        UserSection::Notifications => IconName::Bell,
        UserSection::Voice => IconName::Mic,
        UserSection::Keyboard => IconName::Keyboard,
        UserSection::Appearance => IconName::Palette,
        UserSection::Privacy => IconName::Fingerprint,
        UserSection::Diagnostics => IconName::Activity,
    }
}

/// The nav's label of a section (`workspace.<id>`).
pub fn label(section: UserSection) -> &'static str {
    match section {
        UserSection::Profile => t("workspace.profile"),
        UserSection::Signin => t("workspace.signin"),
        UserSection::Devices => t("workspace.devices"),
        UserSection::Notifications => t("workspace.notifications"),
        UserSection::Voice => t("workspace.voice"),
        UserSection::Keyboard => t("workspace.keyboard"),
        UserSection::Appearance => t("workspace.appearance"),
        UserSection::Privacy => t("workspace.privacy"),
        UserSection::Diagnostics => t("workspace.diagnostics"),
    }
}

/// The nav items, in the registry's order, all under the group "You".
pub fn nav_items() -> Vec<NavItem> {
    UserSection::ALL
        .iter()
        .map(|&s| {
            NavItem::new(s.id(), label(s))
                .icon(icon(s))
                .group(t("workspace.group.you"))
        })
        .collect()
}

/// The user settings workspace.
pub struct UserSettingsHost {
    section: UserSection,
    /// The route to return to on close (`/settings`'s `from`; the root when it was opened cold).
    from: Route,
    ctx: SlotCx,
    workspace: SettingsWorkspace,
    /// The host's root: it holds the focus when it opens, so Tab and the frame's keys reach it.
    focus: gpui::FocusHandle,
    /// The guard the last render handed its section in `SlotCx::workspace` (always this host's own).
    handed: Option<SettingsWorkspace>,
}

impl EventEmitter<SettingsEvent> for UserSettingsHost {}

impl gpui::Focusable for UserSettingsHost {
    fn focus_handle(&self, _cx: &gpui::App) -> gpui::FocusHandle {
        self.focus.clone()
    }
}

impl UserSettingsHost {
    /// `section` is `None` for `/settings`, which opens Profile (Settings.tsx `activeSection`).
    pub fn new(
        section: Option<UserSection>,
        from: Option<Route>,
        ctx: SlotCx,
        cx: &mut Context<Self>,
    ) -> Self {
        let workspace: SettingsWorkspace = Rc::new(RefCell::new(Workspace::new()));
        Self {
            section: section.unwrap_or(UserSection::Profile),
            from: from.unwrap_or(Route::Root),
            ctx,
            workspace,
            focus: cx.focus_handle(),
            handed: None,
        }
    }

    pub fn section(&self) -> UserSection {
        self.section
    }

    /// The client's newest state, for the sections (the app hands each one in while the host shows).
    pub fn set_state(
        &mut self,
        state: std::sync::Arc<dilla_client::state::AppState>,
        cx: &mut Context<Self>,
    ) {
        self.ctx.state = state;
        cx.notify();
    }

    pub fn workspace(&self) -> SettingsWorkspace {
        self.workspace.clone()
    }

    /// The guard the last render put in its section's `SlotCx::workspace` (`None` before the first frame).
    pub fn handed_workspace(&self) -> Option<SettingsWorkspace> {
        self.handed.clone()
    }

    /// Leaves the open section for `dest` unless the guard holds it behind the prompt.
    pub fn request(&mut self, dest: Dest, cx: &mut Context<Self>) {
        let outcome = self.workspace.borrow_mut().request(dest);
        match outcome {
            Nav::Go(dest) => self.go(dest, cx),
            Nav::Prompt => cx.notify(),
        }
    }

    fn go(&mut self, dest: Dest, cx: &mut Context<Self>) {
        match dest {
            Dest::Section(section) => {
                if section != self.section {
                    self.workspace.borrow_mut().detach();
                    self.section = section;
                    cx.emit(SettingsEvent::Navigate(Route::Settings(Some(section))));
                }
            }
            Dest::Close => {
                self.workspace.borrow_mut().detach();
                cx.emit(SettingsEvent::Navigate(self.from.clone()));
            }
        }
        cx.notify();
    }

    /// Discard changes (the prompt's confirm): the draft resets and the oldest waiting way out goes through.
    pub fn discard(&mut self, cx: &mut Context<Self>) {
        let waiting = self.workspace.borrow_mut().discard();
        // The oldest request wins, as in the web's `confirmation`.
        if let Some(dest) = waiting.into_iter().next() {
            self.go(dest, cx);
        }
        cx.notify();
    }
}

impl Render for UserSettingsHost {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        // The last drawn frame keeps this host until the window draws again: release lets go of its client at once
        // (review M5).
        if let Some(client) = self.ctx.client.clone() {
            crate::release::hold(
                &client,
                &cx.entity(),
                |h: &mut Self, _| {
                    h.ctx.client = None;
                    h.handed = None;
                },
                cx,
            );
        }
        let section = self.section;
        let this = cx.entity();
        let nav = SettingsNav::new("user-settings-nav", nav_items(), section.id()).on_select({
            let this = this.clone();
            move |id, _, cx| {
                if let Some(s) = UserSection::from_id(id) {
                    this.update(cx, |h, cx| h.request(Dest::Section(s), cx));
                }
            }
        });

        // Each host hands its own guard to its sections, so several windows never share one.
        let ctx = SlotCx {
            workspace: Some(self.workspace.clone()),
            ..self.ctx.clone()
        };
        self.handed = ctx.workspace.clone();
        let body = slots::settings_sections()
            .into_iter()
            .find(|s| s.id == SectionId::User(section))
            .map(|s| (s.view)(s.id, &ctx, window, cx));

        let (footer, prompt) = {
            let ws = self.workspace.borrow();
            (ws.footer(), ws.prompt())
        };
        let mut frame = SettingsFrame::new("user-settings", t("settings.close"))
            .nav(nav)
            .wide(matches!(
                section,
                UserSection::Privacy | UserSection::Diagnostics
            ))
            .on_close({
                let this = this.clone();
                move |_, cx| this.update(cx, |h, cx| h.request(Dest::Close, cx))
            });
        if let Some(f) = footer {
            let (reset, save) = (this.clone(), this.clone());
            frame = frame.footer(
                SettingsFooter::new("user-settings-footer")
                    .status(f.status)
                    .action(
                        Button::new("user-settings-reset", t("settings.editing.reset"))
                            .disabled(!f.reset_enabled)
                            .on_click(move |_, _, cx| {
                                reset.update(cx, |h, cx| {
                                    h.workspace.borrow_mut().reset();
                                    cx.notify();
                                })
                            }),
                    )
                    .action(
                        Button::new("user-settings-save", t("settings.editing.save"))
                            .variant(ButtonVariant::Accent)
                            .disabled(!f.save_enabled)
                            .on_click(move |_, _, cx| {
                                save.update(cx, |h, cx| {
                                    h.workspace.borrow_mut().save();
                                    cx.notify();
                                })
                            }),
                    ),
            );
        }
        let frame = frame.child(body.unwrap_or_else(|| div().into_any_element()));

        // Always in the tree: closing hands focus back to what had it only when the dialog sees `open` go false.
        let open = prompt.is_some();
        let (keep, discard) = (this.clone(), this);
        let dialog = ConfirmDialog::new(
            "user-settings-leave",
            prompt.map_or(t("settings.editing.leaveTitle"), |p| p.title),
            prompt.map_or(t("settings.editing.leaveBody"), |p| p.body),
            prompt.map_or(t("settings.editing.discard"), |p| p.confirm_label),
            prompt.map_or(t("settings.editing.discard"), |p| p.confirm_label),
            prompt.map_or(t("settings.editing.keepEditing"), |p| p.cancel_label),
        )
        .open(open)
        .tone(Tone::Accent)
        .on_close(move |_, cx| {
            keep.update(cx, |h, cx| {
                h.workspace.borrow_mut().keep_editing();
                cx.notify();
            })
        })
        .on_confirm(move |_, cx| discard.update(cx, |h, cx| h.discard(cx)));
        div()
            .id("user-settings-host")
            .track_focus(&self.focus)
            .size_full()
            // Tab and Shift+Tab walk the settings as they walk the shell (`crate::screens::bind_keys`).
            .on_action(|_: &crate::screens::FocusNext, window, cx| window.focus_next(cx))
            .on_action(|_: &crate::screens::FocusPrevious, window, cx| window.focus_prev(cx))
            .child(frame)
            .child(dialog)
    }
}

/// A host for the app to hold: `section` of `/settings/<section>`, closing back to `from`.
pub fn open(
    section: Option<UserSection>,
    from: Option<Route>,
    ctx: SlotCx,
    cx: &mut gpui::App,
) -> gpui::Entity<UserSettingsHost> {
    cx.new(|cx| UserSettingsHost::new(section, from, ctx, cx))
}
