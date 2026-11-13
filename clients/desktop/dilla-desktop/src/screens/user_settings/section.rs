//! The entity behind the Profile, Devices and Appearance sections: the form's fields, the open dialog and what is in flight.
//! The views of each section are `impl UserSettings` in [`super::profile`], [`super::appearance`] and below (Devices).

use std::cell::RefCell;
use std::rc::Rc;
use std::time::Duration;

use gpui::{
    AnyElement, App, AppContext, Context, Entity, Focusable, IntoElement, ParentElement, Render,
    SharedString, Styled, Subscription, Window, div,
};
use gpui_component::input::{InputEvent, InputState, TextareaState};
use zeroize::Zeroizing;

use dilla_client::ClientError;
use dilla_client::state::DeviceId;
use dilla_client::state::profile::DeviceEntry;
use dilla_desktop_ui::components::action_menu::MenuItem;
use dilla_desktop_ui::components::banner::{Banner, BannerTone};
use dilla_desktop_ui::components::button::{Button, ButtonSize, ButtonVariant};
use dilla_desktop_ui::components::confirm_dialog::{ConfirmDialog, Tone};
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::components::settings::frame::section_header;
use dilla_desktop_ui::components::settings::row::GroupPosition;
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::components::toast::show_toast;
use dilla_desktop_ui::metrics::Density;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, text};

use super::device_row::DeviceRow;
use super::devices::{
    KeyEntry, RowAction, action_of, can_rename, meta_of, ordered, own_tag, rename_label,
    rename_start, short_id, title_of,
};
use super::draft::{Fields, ProfileDraft, SAVE_AFTER_MS};
use crate::router::UserSection;
use crate::slots::SlotCx;
use crate::strings::{t, tf};

/// The dialog open over Settings -> Devices.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum DeviceDialog {
    Rename(DeviceId),
    /// A device the signed list names: removed with the recovery key.
    Revoke(DeviceId),
    /// A live device in no signed list: removed without a key.
    RemoveUnlisted(DeviceId),
    /// This device: sign out and remove it (with the recovery key).
    SignOut,
}

pub struct UserSettings {
    pub(super) which: UserSection,
    pub(super) ctx: SlotCx,
    /// What was asked of the client when the section opened.
    requested: Vec<UserSection>,
    // Profile
    pub(super) draft: Rc<RefCell<ProfileDraft>>,
    pub(super) display: Entity<InputState>,
    pub(super) status: Entity<InputState>,
    pub(super) bio: Entity<TextareaState>,
    /// Set when the account's fields moved the draft: the inputs take its text at the next draw.
    sync_inputs: bool,
    autosave: u64,
    // Devices
    pub(super) dialog: Option<DeviceDialog>,
    rename: Entity<InputState>,
    key: Entity<InputState>,
    key_entry: KeyEntry,
    rename_error: Option<String>,
    rename_busy: bool,
    plain_busy: bool,
    plain_error: Option<String>,
    load_error: Option<String>,
    // Appearance
    pub(super) appearance_pending: bool,
    pub(super) appearance_failed: bool,
    _subscriptions: Vec<Subscription>,
}

fn code_of(e: &ClientError) -> String {
    match e {
        ClientError::Sync(code) => code.clone(),
        ClientError::Revoked => "E_REVOKED".to_owned(),
        ClientError::Network(_) => "E_NETWORK".to_owned(),
        other => other.to_string(),
    }
}

impl UserSettings {
    pub fn new(window: &mut Window, cx: &mut Context<Self>) -> Self {
        let display = cx.new(|cx| InputState::new(window, cx));
        let status = cx.new(|cx| InputState::new(window, cx));
        let bio = cx.new(|cx| TextareaState::new(window, cx));
        let rename = cx.new(|cx| InputState::new(window, cx));
        let key = cx.new(|cx| InputState::new(window, cx));
        let mut subs = Vec::new();
        for input in [&display, &status] {
            subs.push(cx.subscribe_in(
                input,
                window,
                |this: &mut Self, _, event: &InputEvent, window, cx| match event {
                    InputEvent::Change => this.edited(cx),
                    InputEvent::PressEnter { .. } => this.save_now(window, cx),
                    _ => {}
                },
            ));
        }
        subs.push(cx.subscribe_in(
            &bio,
            window,
            |this: &mut Self, _, event: &InputEvent, _window, cx| {
                if matches!(event, InputEvent::Change) {
                    this.edited(cx);
                }
            },
        ));
        subs.push(cx.subscribe_in(
            &key,
            window,
            |this: &mut Self, _, event: &InputEvent, window, cx| match event {
                InputEvent::Change => {
                    let typed = this.key.read(cx).value();
                    this.key_entry.typed(&typed);
                    cx.notify();
                }
                InputEvent::PressEnter { .. } => this.confirm_key(window, cx),
                _ => {}
            },
        ));
        subs.push(cx.subscribe_in(
            &rename,
            window,
            |this: &mut Self, _, event: &InputEvent, window, cx| {
                if matches!(event, InputEvent::PressEnter { .. }) {
                    this.save_rename(window, cx);
                }
            },
        ));
        Self {
            which: UserSection::Profile,
            ctx: SlotCx::default(),
            requested: Vec::new(),
            draft: Rc::new(RefCell::new(ProfileDraft::default())),
            display,
            status,
            bio,
            sync_inputs: true,
            autosave: 0,
            dialog: None,
            rename,
            key,
            key_entry: KeyEntry::default(),
            rename_error: None,
            rename_busy: false,
            plain_busy: false,
            plain_error: None,
            load_error: None,
            appearance_pending: false,
            appearance_failed: false,
            _subscriptions: subs,
        }
    }

    /// The frame's view for this draw: the section to draw and what the client holds.
    pub fn show(&mut self, which: UserSection, ctx: SlotCx, cx: &mut Context<Self>) {
        self.which = which;
        let saved = Fields::saved(ctx.state.profile.me.as_ref(), &ctx.state.account.username);
        self.ctx = ctx;
        let moved = {
            let mut draft = self.draft.borrow_mut();
            let before = draft.fields.clone();
            if draft.saved == Fields::default() && draft.fields == Fields::default() {
                *draft = ProfileDraft::new(saved);
            } else {
                draft.follow_account(saved);
            }
            draft.fields != before
        };
        if moved {
            self.sync_inputs = true;
        }
        self.request(which, cx);
    }

    /// What a section needs from the client, asked once per opening.
    fn request(&mut self, which: UserSection, cx: &mut Context<Self>) {
        if self.requested.contains(&which) {
            return;
        }
        let Some(client) = self.ctx.client.clone() else {
            return;
        };
        self.requested.push(which);
        match which {
            UserSection::Profile => {
                let rx = client.profile().load();
                cx.spawn(async move |_, _| {
                    let _ = rx.await;
                })
                .detach();
            }
            UserSection::Devices => {
                let rx = client.devices().refresh();
                cx.spawn(async move |this, cx| {
                    let r = rx.await;
                    let _ = this.update(cx, |this, cx| {
                        if let Ok(Err(e)) = r {
                            this.load_error = Some(code_of(&e));
                        }
                        cx.notify();
                    });
                })
                .detach();
            }
            UserSection::Appearance => {
                let rx = client.profile().load_appearance();
                cx.spawn(async move |_, _| {
                    let _ = rx.await;
                })
                .detach();
            }
            _ => {}
        }
    }

    pub(super) fn density(&self) -> Density {
        match self.ctx.state.profile.density {
            dilla_client::state::profile::DensitySetting::Compact => Density::Compact,
            dilla_client::state::profile::DensitySetting::Regular => Density::Regular,
        }
    }

    /// The Profile form's three controls in tab order: display name, status message, bio (the keyboard test reads them).
    pub fn profile_focus_order(&self, cx: &App) -> Vec<gpui::FocusHandle> {
        use gpui::Focusable;
        vec![
            self.display.focus_handle(cx),
            self.status.focus_handle(cx),
            self.bio.focus_handle(cx),
        ]
    }

    // ---- Profile ----

    fn fields_of_inputs(&self, cx: &App) -> Fields {
        Fields {
            display: self.display.read(cx).value().to_string(),
            // A status is one line: line breaks become spaces.
            status_msg: self.status.read(cx).value().replace(['\r', '\n'], " "),
            bio: self.bio.read(cx).value().to_string(),
        }
    }

    fn edited(&mut self, cx: &mut Context<Self>) {
        let fields = self.fields_of_inputs(cx);
        if fields == self.draft.borrow().fields {
            return;
        }
        self.draft.borrow_mut().edit(fields);
        self.schedule_autosave(cx);
        cx.notify();
    }

    /// A valid change is saved [`SAVE_AFTER_MS`] after the last keystroke.
    fn schedule_autosave(&mut self, cx: &mut Context<Self>) {
        self.autosave += 1;
        let generation = self.autosave;
        if !self.draft.borrow().autosaves() {
            return;
        }
        cx.spawn(async move |this, cx| {
            cx.background_executor()
                .timer(Duration::from_millis(SAVE_AFTER_MS))
                .await;
            let _ = this.update(cx, |this, cx| {
                if this.autosave == generation {
                    this.start_save(cx);
                }
            });
        })
        .detach();
    }

    fn save_now(&mut self, _window: &mut Window, cx: &mut Context<Self>) {
        self.autosave += 1;
        self.start_save(cx);
    }

    /// Sends the form as it is now; `true` when there was nothing left to send.
    pub(super) fn start_save(&mut self, cx: &mut Context<Self>) -> bool {
        let Some(patch) = self.draft.borrow_mut().begin_save() else {
            cx.notify();
            return !self.draft.borrow().evaluate().dirty;
        };
        let Some(client) = self.ctx.client.clone() else {
            self.draft.borrow_mut().save_failed("E_NETWORK");
            cx.notify();
            return false;
        };
        let rx = client.profile().update(patch);
        cx.spawn(async move |this, cx| {
            let r = rx.await;
            let _ = this.update(cx, |this, cx| {
                match r {
                    Ok(Ok(())) => {
                        if this.draft.borrow_mut().save_landed() {
                            this.schedule_autosave(cx);
                        }
                    }
                    Ok(Err(e)) => this.draft.borrow_mut().save_failed(&code_of(&e)),
                    Err(_) => this.draft.borrow_mut().save_failed("E_NETWORK"),
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
        false
    }

    // ---- Devices ----

    fn open_dialog(&mut self, dialog: DeviceDialog, window: &mut Window, cx: &mut Context<Self>) {
        self.plain_error = None;
        self.rename_error = None;
        self.key_entry = KeyEntry::default();
        if let DeviceDialog::Rename(id) = &dialog {
            let start = self
                .ctx
                .state
                .profile
                .devices
                .iter()
                .find(|d| d.id == *id)
                .map(rename_start)
                .unwrap_or_default();
            self.rename
                .update(cx, |s, cx| s.set_value(start, window, cx));
        }
        self.dialog = Some(dialog);
        cx.notify();
    }

    /// Closing drops the key text and every error with it (a closed dialog keeps none).
    fn close_dialog(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        if self.key_entry.busy || self.rename_busy || self.plain_busy {
            return;
        }
        self.dialog = None;
        self.key.update(cx, |s, cx| s.set_value("", window, cx));
        self.key_entry = KeyEntry::default();
        self.rename_error = None;
        self.plain_error = None;
        cx.notify();
    }

    fn name_of(&self, id: DeviceId) -> String {
        self.ctx
            .state
            .profile
            .devices
            .iter()
            .find(|d| d.id == id)
            .map_or_else(|| id.hex().chars().take(8).collect(), title_of)
    }

    fn removed_toast(&self, name: &str, cx: &mut Context<Self>) {
        show_toast(tf("devices.removed", &[("device", name)]), cx);
    }

    fn confirm_key(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(dialog) = self.dialog.clone() else {
            return;
        };
        if !matches!(dialog, DeviceDialog::Revoke(_) | DeviceDialog::SignOut) {
            return;
        }
        if !self.key_entry.submit() {
            cx.notify();
            return;
        }
        let Some(client) = self.ctx.client.clone() else {
            self.key_entry.refused("E_NETWORK");
            cx.notify();
            return;
        };
        let key = Zeroizing::new(self.key.read(cx).value().to_string());
        let (rx, name) = match &dialog {
            DeviceDialog::Revoke(id) => (
                client.devices().revoke(*id, Some(key)),
                Some(self.name_of(*id)),
            ),
            _ => (client.devices().sign_out_and_remove(key), None),
        };
        cx.spawn_in(window, async move |this, cx| {
            let r = rx.await;
            let _ = this.update_in(cx, |this, window, cx| {
                this.key_entry.busy = false;
                match r {
                    Ok(Ok(())) => {
                        if let Some(name) = &name {
                            this.removed_toast(name, cx);
                        }
                        this.close_dialog(window, cx);
                    }
                    Ok(Err(e)) => this.key_entry.refused(&code_of(&e)),
                    Err(_) => this.key_entry.refused("E_NETWORK"),
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    fn confirm_plain(&mut self, id: DeviceId, window: &mut Window, cx: &mut Context<Self>) {
        if self.plain_busy {
            return;
        }
        let Some(client) = self.ctx.client.clone() else {
            return;
        };
        self.plain_busy = true;
        self.plain_error = None;
        let name = self.name_of(id);
        let rx = client.devices().revoke(id, None);
        cx.spawn_in(window, async move |this, cx| {
            let r = rx.await;
            let _ = this.update_in(cx, |this, window, cx| {
                this.plain_busy = false;
                match r {
                    Ok(Ok(())) => {
                        this.removed_toast(&name, cx);
                        this.close_dialog(window, cx);
                    }
                    Ok(Err(e)) => {
                        this.plain_error =
                            Some(tf("settings.error.other", &[("code", &code_of(&e))]));
                    }
                    Err(_) => {
                        this.plain_error =
                            Some(tf("settings.error.other", &[("code", "E_NETWORK")]));
                    }
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    fn save_rename(&mut self, window: &mut Window, cx: &mut Context<Self>) {
        let Some(DeviceDialog::Rename(id)) = self.dialog.clone() else {
            return;
        };
        if self.rename_busy {
            return;
        }
        let typed = self.rename.read(cx).value().to_string();
        let label = match rename_label(&typed) {
            Ok(l) => l,
            Err(_) => {
                self.rename_error = Some(tf("settings.error.other", &[("code", "E_BAD_INPUT")]));
                cx.notify();
                return;
            }
        };
        let Some(client) = self.ctx.client.clone() else {
            return;
        };
        self.rename_busy = true;
        self.rename_error = None;
        let rx = client.devices().rename(id, label);
        cx.spawn_in(window, async move |this, cx| {
            let r = rx.await;
            let _ = this.update_in(cx, |this, window, cx| {
                this.rename_busy = false;
                match r {
                    Ok(Ok(())) => this.close_dialog(window, cx),
                    Ok(Err(e)) => {
                        this.rename_error =
                            Some(tf("settings.error.other", &[("code", &code_of(&e))]));
                    }
                    Err(_) => {
                        this.rename_error =
                            Some(tf("settings.error.other", &[("code", "E_NETWORK")]));
                    }
                }
                cx.notify();
            });
        })
        .detach();
        cx.notify();
    }

    /// The device list, this device first. While the list is missing this device's own row is drawn from the local identity,
    /// so Sign out stays reachable.
    fn rows(&self) -> Vec<DeviceEntry> {
        let mut rows = ordered(&self.ctx.state.profile.devices);
        if !rows.iter().any(|d| d.own)
            && let Some(id) = self.ctx.state.account.device_id
        {
            rows.insert(
                0,
                DeviceEntry {
                    id,
                    tier: 1,
                    signer_tier: 1,
                    last_seen: 0,
                    revoked_at: None,
                    label: None,
                    listed: true,
                    own: true,
                },
            );
        }
        rows
    }

    fn devices_view(&mut self, cx: &mut Context<Self>) -> AnyElement {
        let now = self.ctx.clock.now();
        let density = self.density();
        let rows = self.rows();
        let n = rows.len();
        let this = cx.entity();
        let mut list = div().flex().flex_col().gap(density.row_gap());
        for (i, d) in rows.iter().enumerate() {
            let id = d.id;
            let meta = meta_of(d, now);
            let mut row = DeviceRow::new(("device", i), title_of(d))
                .position(GroupPosition::of(i, n))
                .seen(meta.seen);
            if let Some(state) = meta.state {
                row = row.state(state);
            }
            if d.own {
                row = row
                    .platform_label(own_tag())
                    .own_label(t("devices.thisBrowser"));
            }
            if let Some(action) = action_of(d) {
                let this = this.clone();
                let (label, danger) = match action {
                    RowAction::SignOut => (t("workspace.signOut"), false),
                    RowAction::Revoke | RowAction::RemoveUnlisted => (t("devices.revoke"), true),
                };
                row = row.action(label, danger, move |_, window, cx| {
                    this.update(cx, |v, cx| {
                        let dialog = match action {
                            RowAction::SignOut => DeviceDialog::SignOut,
                            RowAction::Revoke => DeviceDialog::Revoke(id),
                            RowAction::RemoveUnlisted => DeviceDialog::RemoveUnlisted(id),
                        };
                        v.open_dialog(dialog, window, cx);
                    });
                });
            }
            if can_rename(d) {
                let this = this.clone();
                row = row.menu(
                    tf("signalSettings.menu.device", &[("name", &title_of(d))]),
                    vec![MenuItem::new("rename", t("devices.rename"))],
                    move |_, window, cx| {
                        this.update(cx, |v, cx| {
                            v.open_dialog(DeviceDialog::Rename(id), window, cx);
                        });
                    },
                );
            }
            list = list.child(row);
        }

        let mut col = div()
            .flex()
            .flex_col()
            .w_full()
            .child(section_header(t("devices.title"), Some(t("devices.body"))));
        if let Some(code) = &self.load_error {
            col = col.child(div().pb(density.group_gap()).child(Banner::new(
                "devices-load-error",
                BannerTone::Danger,
                tf("settings.error.other", &[("code", code)]),
            )));
        }
        col.child(list).into_any_element()
    }

    /// The subject line of a remove dialog: the device's name and the first characters of its id (a label is the server's to
    /// change, so the id stays the identifier).
    fn subject(&self, id: Option<DeviceId>) -> AnyElement {
        let Some(id) = id else {
            return div().into_any_element();
        };
        let device = self.ctx.state.profile.devices.iter().find(|d| d.id == id);
        let (name, id8) = match device {
            Some(d) => (title_of(d), short_id(d, 8)),
            None => (String::new(), id.hex().chars().take(8).collect()),
        };
        div()
            .flex()
            .child(line(
                format!("{name} "),
                text::BASE,
                line_height(text::BASE, 1.5),
                color::FG,
            ))
            .child(line(
                id8,
                text::BASE,
                line_height(text::BASE, 1.5),
                color::FG_3,
            ))
            .into_any_element()
    }

    fn dialogs(&mut self, cx: &mut Context<Self>) -> Vec<AnyElement> {
        let this = cx.entity();
        let close = {
            let this = this.clone();
            move |window: &mut Window, cx: &mut App| {
                this.update(cx, |v, cx| v.close_dialog(window, cx));
            }
        };
        let mut out: Vec<AnyElement> = Vec::new();

        // The name dialog.
        let renaming = matches!(self.dialog, Some(DeviceDialog::Rename(_)));
        let mut field = TextField::new(
            "devices-rename-name",
            t("devices.renameLabel"),
            &self.rename,
        )
        .hint(t("devices.renameBody"))
        .note(t("devices.renameServerReads"));
        if let Some(error) = &self.rename_error {
            field = field.error(error.clone());
        }
        let save = {
            let this = this.clone();
            Button::new(
                "devices-rename-save",
                if self.rename_busy {
                    t("devices.working")
                } else {
                    t("devices.renameSave")
                },
            )
            .variant(ButtonVariant::Accent)
            .size(ButtonSize::Dialog)
            .on_click(move |_, window, cx| {
                this.update(cx, |v, cx| v.save_rename(window, cx));
            })
        };
        out.push(
            Dialog::new(
                "devices-rename",
                t("devices.renameTitle"),
                t("dialog.close"),
            )
            .open(renaming)
            .can_close(!self.rename_busy)
            .body(field)
            .footer(save)
            .on_close(close.clone())
            .into_any_element(),
        );

        // The two key dialogs.
        let (open_key, subject_id, title, body, confirm) = match &self.dialog {
            Some(DeviceDialog::Revoke(id)) => (
                true,
                Some(*id),
                t("devices.revokeTitle"),
                t("devices.revokeBody"),
                t("devices.confirmRevoke"),
            ),
            Some(DeviceDialog::SignOut) => (
                true,
                None,
                t("devices.signOutTitle"),
                t("devices.signOutBody"),
                t("devices.confirmSignOut"),
            ),
            _ => (
                false,
                None,
                t("devices.revokeTitle"),
                t("devices.revokeBody"),
                t("devices.confirmRevoke"),
            ),
        };
        let mut key_field = TextField::new("devices-key", t("devices.keyLabel"), &self.key)
            .hint(self.key_entry.hint());
        if let Some(e) = self.key_entry.field_error {
            key_field = key_field.error(t(e));
        }
        let fields = div()
            .flex()
            .flex_col()
            .child(self.subject(subject_id))
            .child(key_field);
        let first = self.key.read(cx).focus_handle(cx);
        out.push(
            ConfirmDialog::new(
                "devices-key-dialog",
                title,
                body,
                confirm,
                t("devices.working"),
                t("settings.editing.keepEditing"),
            )
            .open(open_key)
            .tone(Tone::Danger)
            .busy(self.key_entry.busy)
            .error(self.key_entry.banner.clone().map(SharedString::from))
            .fields(fields, Some(&first))
            .on_confirm({
                let this = this.clone();
                move |window, cx| {
                    this.update(cx, |v, cx| v.confirm_key(window, cx));
                }
            })
            .on_close(close.clone())
            .into_any_element(),
        );

        // The plain removal of a device no signed list names.
        let unlisted = match &self.dialog {
            Some(DeviceDialog::RemoveUnlisted(id)) => Some(*id),
            _ => None,
        };
        out.push(
            ConfirmDialog::new(
                "devices-remove-unlisted",
                t("devices.removeUnlistedTitle"),
                t("devices.removeUnlistedBody"),
                t("devices.confirmRevoke"),
                t("devices.working"),
                t("settings.editing.keepEditing"),
            )
            .open(unlisted.is_some())
            .tone(Tone::Danger)
            .busy(self.plain_busy)
            .error(self.plain_error.clone().map(SharedString::from))
            .fields(self.subject(unlisted), None)
            .on_confirm({
                let this = this.clone();
                move |window, cx| {
                    if let Some(id) = unlisted {
                        this.update(cx, |v, cx| v.confirm_plain(id, window, cx));
                    }
                }
            })
            .on_close(close)
            .into_any_element(),
        );
        out
    }
}

impl Render for UserSettings {
    fn render(&mut self, window: &mut Window, cx: &mut Context<Self>) -> impl IntoElement {
        if std::mem::take(&mut self.sync_inputs) {
            let f = self.draft.borrow().fields.clone();
            self.display
                .update(cx, |s, cx| s.set_value(f.display, window, cx));
            self.status
                .update(cx, |s, cx| s.set_value(f.status_msg, window, cx));
            self.bio.update(cx, |s, cx| s.set_value(f.bio, window, cx));
        }
        let body = match self.which {
            UserSection::Profile => self.profile_view(cx),
            UserSection::Devices => self.devices_view(cx),
            UserSection::Appearance => self.appearance_view(cx),
            _ => div().into_any_element(),
        };
        let dialogs = if self.which == UserSection::Devices {
            self.dialogs(cx)
        } else {
            Vec::new()
        };
        div()
            .flex()
            .flex_col()
            .w_full()
            .child(body)
            .children(dialogs)
    }
}
