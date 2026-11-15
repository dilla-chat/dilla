//! Server settings → Roles: packages/web/src/screens/admin/RolesSection.tsx. The role list beside the selected role's
//! editor (`RoleEditor`): its name, colour and permission toggles, every change saved at once through the client's roles
//! API, one after another. Moving, deleting and the two display options sit in each role's context menu (Ruling X).
//!
//! The gates are the web's, and advisory (the instance decides every request again): a role at or above my highest role
//! (when I am not the owner) shows but cannot change, and a permission I do not hold, as the one resolver
//! (`features::roles::perm::resolve_community`, through `held_bits`) answers it, cannot be given or taken.

use std::collections::VecDeque;
use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::roles::perm::{self, PermKey, ROLE_PERM_GROUPS};
use dilla_client::features::roles::{
    Direction, PermLock, ROLE_NAME_MAX_BYTES, RolePatch, permission_lock,
};
use dilla_client::state::roles::{RoleId, RoleSummary};
use dilla_client::state::{AppState, CommunityId};
use dilla_client::sync::api::ApiError;
use gpui::{
    AnyElement, App, AppContext, Entity, FocusHandle, InteractiveElement, IntoElement,
    ParentElement, Pixels, Point, SharedString, Styled, Subscription, Window, WindowId, div,
};
use gpui_component::input::{InputEvent, InputState};

use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem};
use dilla_desktop_ui::components::members::role_editor::{
    EditorLabels, EditorRole, PermGroup, PermToggle, RoleEditor,
};
use dilla_desktop_ui::components::settings::frame::section_header_full;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, text};

use crate::slots::SlotCx;
use crate::strings::{t, tf};

use super::{ensure_loaded, hex, my_bits, request_role_delete, role_delete_dialog, ui};

/// RolesSection.tsx `NEW_ROLE_COLOR`: a new role is grey, with View channels and Send messages.
pub const NEW_ROLE_COLOR: u32 = 0xB0_BEC5;
pub const NEW_ROLE_ALLOW: u32 = perm::VIEW_CHANNEL | perm::SEND_MESSAGES;

/// RolesSection.tsx `orderRoles`: @everyone first, then the other roles from the highest down (ties by id).
pub fn order_roles(all: &[RoleSummary], everyone: Option<RoleId>) -> Vec<RoleSummary> {
    let is_everyone = |r: &RoleSummary| Some(r.id) == everyone || r.position == 0;
    let mut rest: Vec<RoleSummary> = all.iter().filter(|r| !is_everyone(r)).cloned().collect();
    rest.sort_by(|a, b| b.position.cmp(&a.position).then(a.id.cmp(&b.id)));
    all.iter()
        .filter(|r| is_everyone(r))
        .cloned()
        .chain(rest)
        .collect()
}

/// One permission row of the editor: the key, whether the role allows it, and its lock (`None`: may toggle).
pub type PermRow = (PermKey, bool, Option<PermLock>);

/// What the section draws for the viewer, from the state alone (RolesSection.tsx's derived values).
#[derive(Clone, Debug)]
pub struct RolesView {
    pub ordered: Vec<RoleSummary>,
    /// The role open in the editor: the chosen one while it is listed, else the top role below @everyone, else @everyone.
    pub role: Option<RoleSummary>,
    pub everyone: Option<RoleId>,
    pub owner: bool,
    pub top: u32,
    /// The access is read and carries manage_roles.
    pub can_manage: bool,
    /// The open role may change: I may manage roles, it is below my rank, and no new role is awaited (fix round 1, M4).
    pub editable: bool,
    /// The bits I hold community-wide (everything for the owner and administrators).
    pub mine: u32,
}

/// Why a typed name is not saved.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum NameProblem {
    Empty,
    Long,
}

/// A role's context-menu entry (RolesSection.tsx `menuItems`).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum RoleMenu {
    Up,
    Down,
    Hoist(bool),
    Mentionable(bool),
    Delete,
}

impl RolesView {
    /// The view of `community` with `chosen` asked for, while `awaiting` (a role New role made) is not listed yet.
    pub fn of(
        state: &AppState,
        community: CommunityId,
        chosen: Option<RoleId>,
        awaiting: Option<RoleId>,
    ) -> Self {
        let access = state.roles.access_of(community);
        let everyone = access.everyone_id.or_else(|| {
            state
                .roles
                .roles_of(community)
                .iter()
                .find(|r| r.position == 0)
                .map(|r| r.id)
        });
        let ordered = order_roles(state.roles.roles_of(community), everyone);
        let role = chosen
            .and_then(|id| ordered.iter().find(|r| r.id == id))
            .or_else(|| ordered.iter().find(|r| Some(r.id) != everyone))
            .or_else(|| ordered.first())
            .cloned();
        let awaiting = awaiting.filter(|id| !ordered.iter().any(|r| r.id == *id));
        let can_manage = access.loaded && access.manage_roles;
        let mut view = Self {
            ordered,
            role: None,
            everyone,
            owner: access.owner,
            top: access.top,
            can_manage,
            editable: false,
            mine: my_bits(state, community),
        };
        view.editable = role
            .as_ref()
            .is_some_and(|r| can_manage && !view.locked(r) && awaiting.is_none_or(|a| a == r.id));
        view.role = role;
        view
    }

    /// RolesSection.tsx `locked`: at or above my highest role, unless I own the server.
    pub fn locked(&self, r: &RoleSummary) -> bool {
        !self.owner && r.position >= self.top
    }

    pub fn is_everyone(&self, r: &RoleSummary) -> bool {
        Some(r.id) == self.everyone || r.position == 0
    }

    /// The open role's permission rows in the prototype's groups: on, and the lock (`None`: may toggle).
    pub fn permission_rows(&self, allow: u32) -> Vec<(&'static str, Vec<PermRow>)> {
        ROLE_PERM_GROUPS
            .iter()
            .map(|(group, keys)| {
                (
                    *group,
                    keys.iter()
                        .map(|k| {
                            (
                                *k,
                                allow & k.bit() != 0,
                                permission_lock(self.editable, self.mine, k.bit()),
                            )
                        })
                        .collect(),
                )
            })
            .collect()
    }

    /// A toggle of `key` to `on` on the open role, from its current `allow`: the patch to save, or `None` when the gate
    /// refuses it (the screen checks it again before anything is sent).
    pub fn permission_patch(&self, allow: u32, key: PermKey, on: bool) -> Option<RolePatch> {
        if permission_lock(self.editable, self.mine, key.bit()).is_some() {
            return None;
        }
        let next = if on {
            allow | key.bit()
        } else {
            allow & !key.bit()
        };
        (next != allow).then(|| RolePatch {
            allow: Some(next),
            ..RolePatch::default()
        })
    }

    /// A colour picked for the open role (@everyone has none).
    pub fn color_patch(&self, current: u32, chosen: u32) -> Option<RolePatch> {
        let role = self.role.as_ref()?;
        (self.editable && !self.is_everyone(role) && chosen != current).then(|| RolePatch {
            color: Some(chosen),
            ..RolePatch::default()
        })
    }

    /// RolesSection.tsx `commitName`: the typed name, NFC and trimmed. `Ok(None)` when nothing changes (or it may not).
    pub fn name_patch(&self, typed: &str) -> Result<Option<RolePatch>, NameProblem> {
        let Some(role) = self.role.as_ref() else {
            return Ok(None);
        };
        if !self.editable || self.is_everyone(role) {
            return Ok(None);
        }
        let shown = dilla_client::features::server_admin::nfc(typed)
            .trim()
            .to_owned();
        if shown == role.name {
            return Ok(None);
        }
        if shown.is_empty() {
            return Err(NameProblem::Empty);
        }
        if shown.len() > ROLE_NAME_MAX_BYTES {
            return Err(NameProblem::Long);
        }
        Ok(Some(RolePatch {
            name: Some(shown),
            ..RolePatch::default()
        }))
    }

    /// The context menu of `id`: empty for @everyone, without manage_roles, or for a locked role.
    pub fn menu(&self, id: RoleId) -> Vec<RoleMenu> {
        let Some(r) = self.ordered.iter().find(|r| r.id == id) else {
            return Vec::new();
        };
        if self.is_everyone(r) || !self.can_manage || self.locked(r) {
            return Vec::new();
        }
        let movable: Vec<&RoleSummary> = self
            .ordered
            .iter()
            .filter(|x| !self.is_everyone(x) && !self.locked(x))
            .collect();
        let at = movable.iter().position(|x| x.id == id);
        let mut items = Vec::new();
        if at.is_some_and(|i| i > 0) {
            items.push(RoleMenu::Up);
        }
        if at.is_some_and(|i| i + 1 < movable.len()) {
            items.push(RoleMenu::Down);
        }
        items.extend([
            RoleMenu::Hoist(r.hoist),
            RoleMenu::Mentionable(r.mentionable),
            RoleMenu::Delete,
        ]);
        items
    }
}

/// A change saved after the ones before it (a fast run of toggles applies in order).
struct Save {
    community: CommunityId,
    role: RoleId,
    patch: RolePatch,
    shown: String,
}

/// An optimistic change, shown until the instance's own answer replaces the role (or the save fails).
#[derive(Clone)]
struct Override {
    base: RoleSummary,
    allow: u32,
    color: u32,
}

/// The section's state of one window.
#[derive(Default)]
pub struct RolesUi {
    name: Option<Entity<InputState>>,
    _name_events: Option<Subscription>,
    /// The role the name field shows, and the text the section last put in it.
    field: Option<(CommunityId, RoleId, String)>,
    /// The role whose name is being typed (its draft is the field's text).
    draft: Option<RoleId>,
    name_error: Option<String>,
    status: Option<(String, bool)>,
    over: Option<Override>,
    awaiting: Option<RoleId>,
    menu: Option<(CommunityId, RoleId, Point<Pixels>)>,
    queue: VecDeque<Save>,
    saving: bool,
    /// A New role is on its way: another click is ignored until it answers, so one click makes one role (review I-E1).
    creating: bool,
    focus: Vec<FocusHandle>,
}

fn roles_ui(w: WindowId, cx: &mut App) -> &mut RolesUi {
    crate::per_window::get_mut::<RolesUi>(w, cx)
}

/// The section's status line in window `w` (its text, and whether it is a refusal): what the editor draws under the
/// permissions.
pub fn status(w: WindowId, cx: &App) -> Option<(String, bool)> {
    crate::per_window::get::<RolesUi>(w, cx).and_then(|r| r.status.clone())
}

/// The role open in window `w` (the members area's own choice).
pub fn selected_role(w: WindowId, cx: &App) -> Option<RoleId> {
    crate::per_window::get::<super::MembersUi>(w, cx).and_then(|u| u.role)
}

/// Opens `role` in the editor (RolesSection.tsx `select`): the draft, the refusal and the status line go.
pub fn select_role(w: WindowId, role: RoleId, cx: &mut App) {
    ui(w, cx).role = Some(role);
    let r = roles_ui(w, cx);
    r.draft = None;
    r.name_error = None;
    r.status = None;
    r.menu = None;
    cx.refresh_windows();
}

fn unhex16(s: &str) -> Option<RoleId> {
    if s.len() != 32 {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(s.get(2 * i..2 * i + 2)?, 16).ok()?;
    }
    Some(out)
}

/// RolesSection.tsx `refuse`.
fn refusal(e: &ApiError) -> String {
    match e.status {
        403 => t("roles.error.forbidden").to_owned(),
        409 => t("roles.error.conflict").to_owned(),
        404 => t("roles.error.gone").to_owned(),
        _ => tf("roles.error.other", &[("code", &e.code)]),
    }
}

fn say(w: WindowId, text: String, alert: bool, cx: &mut App) {
    roles_ui(w, cx).status = Some((text, alert));
    cx.refresh_windows();
}

fn client_of(w: WindowId, cx: &mut App) -> Option<(Rc<Client>, std::sync::Arc<AppState>)> {
    let ctx = ui(w, cx).ctx.clone()?;
    Some((ctx.client?, ctx.state))
}

/// Queues `patch` of `role` and starts the queue.
fn save(
    w: WindowId,
    community: CommunityId,
    role: RoleId,
    patch: RolePatch,
    shown: String,
    cx: &mut App,
) {
    roles_ui(w, cx).queue.push_back(Save {
        community,
        role,
        patch,
        shown,
    });
    say(w, t("serverSettings.roles.saving").to_owned(), false, cx);
    pump(w, cx);
}

/// Sends the next queued change once the one before it has answered.
fn pump(w: WindowId, cx: &mut App) {
    let Some((client, _)) = client_of(w, cx) else {
        let r = roles_ui(w, cx);
        r.queue.clear();
        r.status = None;
        return;
    };
    let r = roles_ui(w, cx);
    if r.saving {
        return;
    }
    let Some(next) = r.queue.pop_front() else {
        return;
    };
    r.saving = true;
    let Save {
        community,
        role,
        patch,
        shown,
    } = next;
    let job = client
        .roles()
        .run(move |h| async move { h.update_role(community, role, patch).await });
    cx.spawn(async move |cx| {
        let answer = job.await;
        cx.update(|cx| {
            let r = roles_ui(w, cx);
            r.saving = false;
            match answer {
                Ok(()) => say(w, tf("roles.editor.saved", &[("name", &shown)]), false, cx),
                Err(e) => {
                    r.over = None;
                    say(w, refusal(&e), true, cx);
                }
            }
            pump(w, cx);
        });
    })
    .detach();
}

/// Saves the typed name of the open role (on Enter and when the field loses focus).
fn commit_name(w: WindowId, cx: &mut App) {
    let Some(ctx) = ui(w, cx).ctx.clone() else {
        return;
    };
    let Some((community, role, _)) = roles_ui(w, cx).field.clone() else {
        return;
    };
    if roles_ui(w, cx).draft != Some(role) {
        return;
    }
    let Some(input) = roles_ui(w, cx).name.clone() else {
        return;
    };
    let typed = input.read(cx).value().to_string();
    let awaiting = roles_ui(w, cx).awaiting;
    let view = RolesView::of(&ctx.state, community, Some(role), awaiting);
    if view.role.as_ref().is_none_or(|r| r.id != role) {
        return;
    }
    match view.name_patch(&typed) {
        Ok(None) => {}
        Err(p) => {
            roles_ui(w, cx).name_error = Some(
                match p {
                    NameProblem::Empty => t("roles.error.empty"),
                    NameProblem::Long => t("roles.error.long"),
                }
                .to_owned(),
            );
            cx.refresh_windows();
        }
        Ok(Some(patch)) => {
            roles_ui(w, cx).name_error = None;
            let shown = patch.name.clone().unwrap_or_default();
            save(w, community, role, patch, shown, cx);
        }
    }
}

/// RolesSection.tsx `newRole`: makes "new role" and opens it once the list carries it.
fn new_role(w: WindowId, community: CommunityId, cx: &mut App) {
    let Some((client, _)) = client_of(w, cx) else {
        return;
    };
    if std::mem::replace(&mut roles_ui(w, cx).creating, true) {
        return;
    }
    say(w, t("serverSettings.roles.saving").to_owned(), false, cx);
    let name = t("serverSettings.roles.newName");
    let job = client.roles().run(move |h| async move {
        h.create_role(
            community,
            name,
            NEW_ROLE_COLOR,
            false,
            false,
            NEW_ROLE_ALLOW,
        )
        .await
    });
    cx.spawn(async move |cx| {
        let answer = job.await;
        cx.update(|cx| roles_ui(w, cx).creating = false);
        cx.update(|cx| match answer {
            Ok(id) => {
                roles_ui(w, cx).awaiting = Some(id);
                select_role(w, id, cx);
                say(w, tf("roles.editor.created", &[("name", name)]), false, cx);
            }
            Err(e) => {
                let text = match e.status {
                    403 => t("roles.error.forbidden").to_owned(),
                    400 => t("roles.error.full").to_owned(),
                    _ => tf("roles.error.other", &[("code", &e.code)]),
                };
                say(w, text, true, cx);
            }
        });
    })
    .detach();
}

/// Moves `role` one step (the role's context menu).
fn move_role(
    w: WindowId,
    community: CommunityId,
    role: RoleSummary,
    direction: Direction,
    cx: &mut App,
) {
    let Some((client, _)) = client_of(w, cx) else {
        return;
    };
    let id = role.id;
    let job = client
        .roles()
        .run(move |h| async move { h.move_role(community, id, direction).await });
    cx.spawn(async move |cx| {
        if let Err(e) = job.await {
            cx.update(|cx| {
                say(
                    w,
                    tf(
                        "roles.error.move",
                        &[("name", &role.name), ("code", &e.code)],
                    ),
                    true,
                    cx,
                );
            });
        }
    })
    .detach();
}

fn on_menu(w: WindowId, community: CommunityId, id: RoleId, item: &str, cx: &mut App) {
    roles_ui(w, cx).menu = None;
    cx.refresh_windows();
    let Some(ctx) = ui(w, cx).ctx.clone() else {
        return;
    };
    let view = RolesView::of(&ctx.state, community, Some(id), None);
    let Some(r) = view.ordered.iter().find(|r| r.id == id).cloned() else {
        return;
    };
    let items = view.menu(id);
    let has = |m: RoleMenu| items.contains(&m);
    match item {
        "up" if has(RoleMenu::Up) => move_role(w, community, r, Direction::Up, cx),
        "down" if has(RoleMenu::Down) => move_role(w, community, r, Direction::Down, cx),
        "hoist" if has(RoleMenu::Hoist(r.hoist)) => {
            let patch = RolePatch {
                hoist: Some(!r.hoist),
                ..RolePatch::default()
            };
            save(w, community, id, patch, r.name.clone(), cx);
        }
        "mentionable" if has(RoleMenu::Mentionable(r.mentionable)) => {
            let patch = RolePatch {
                mentionable: Some(!r.mentionable),
                ..RolePatch::default()
            };
            save(w, community, id, patch, r.name.clone(), cx);
        }
        "delete" if has(RoleMenu::Delete) => request_role_delete(w, community, id, r.name, cx),
        _ => {}
    }
}

/// The name a role is shown by (@everyone's is the copy's).
fn shown_name(view: &RolesView, r: &RoleSummary) -> String {
    if view.is_everyone(r) {
        t("roles.everyone").to_owned()
    } else {
        r.name.clone()
    }
}

/// The name field of window `w`, made once; Enter and losing focus save the typed name.
fn name_input(w: WindowId, window: &mut Window, cx: &mut App) -> Entity<InputState> {
    if let Some(input) = roles_ui(w, cx).name.clone() {
        return input;
    }
    let input = cx.new(|cx| InputState::new(window, cx));
    let sub = cx.subscribe(&input, move |input, event: &InputEvent, cx| match event {
        InputEvent::Change => {
            let value = input.read(cx).value().to_string();
            let r = roles_ui(w, cx);
            // A change the section made itself (a role opened) is not a draft.
            if let Some((_, role, set)) = &r.field
                && *set != value
            {
                r.draft = Some(*role);
                r.name_error = None;
            }
        }
        InputEvent::PressEnter { .. } | InputEvent::Blur => commit_name(w, cx),
        InputEvent::Focus => {}
    });
    let r = roles_ui(w, cx);
    r.name = Some(input.clone());
    r._name_events = Some(sub);
    input
}

/// The Roles section (`slots::settings_sections` → `members::settings_section`).
pub fn section(
    w: WindowId,
    ctx: &SlotCx,
    community: CommunityId,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    ensure_loaded(w, "people", community, ctx.client.as_ref(), cx);
    let state = ctx.state.clone();
    let chosen = ui(w, cx).role;
    let awaiting = roles_ui(w, cx).awaiting;
    let view = RolesView::of(&state, community, chosen, awaiting);
    if awaiting.is_some_and(|a| view.ordered.iter().any(|r| r.id == a)) {
        roles_ui(w, cx).awaiting = None;
    }
    let head = section_header_full(t("roles.title"), Some(t("serverSettings.roles.body")));
    let Some(role) = view.role.clone() else {
        return div()
            .id("settings-roles")
            .flex()
            .flex_col()
            .child(head)
            .child(line(
                t("roles.loading"),
                text::UI,
                line_height(text::UI, 1.5),
                color::FG_2,
            ))
            .into_any_element();
    };

    // The optimistic change holds while the role is as it was when it was made.
    let over = roles_ui(w, cx).over.clone();
    let over = match over {
        Some(o) if o.base == role => Some(o),
        Some(_) => {
            roles_ui(w, cx).over = None;
            None
        }
        None => None,
    };
    let allow = over.as_ref().map_or(role.allow, |o| o.allow);
    let colour = over.as_ref().map_or(role.color, |o| o.color);
    let is_everyone = view.is_everyone(&role);

    // The name field shows the open role's name unless it is being typed.
    let input = name_input(w, window, cx);
    let shown = shown_name(&view, &role);
    let (draft, field) = {
        let r = roles_ui(w, cx);
        (r.draft, r.field.clone())
    };
    let typing = draft == Some(role.id)
        && field
            .as_ref()
            .is_some_and(|(c, id, _)| *c == community && *id == role.id);
    if !typing {
        let current = input.read(cx).value().to_string();
        let r = roles_ui(w, cx);
        r.draft = None;
        r.field = Some((community, role.id, shown.clone()));
        if current != shown {
            input.update(cx, |s, cx| s.set_value(shown.clone(), window, cx));
        }
    }
    let read_only = is_everyone || !view.editable;
    input.update(cx, |s, cx| s.set_disabled(read_only, cx));

    let rows: Vec<EditorRole> = view
        .ordered
        .iter()
        .map(|r| EditorRole {
            id: hex(&r.id).into(),
            name: shown_name(&view, r).into(),
            color: r.color,
            count: r.members,
        })
        .collect();
    let groups: Vec<PermGroup> = view
        .permission_rows(allow)
        .into_iter()
        .map(|(group, keys)| PermGroup {
            title: t(&format!("perm.group.{group}")).into(),
            rows: keys
                .into_iter()
                .map(|(k, on, lock)| PermToggle {
                    id: k.key().into(),
                    label: t(&format!("perm.{}", k.key())).into(),
                    hint: t(&format!("perm.{}.hint", k.key())).into(),
                    on,
                    disabled: lock.is_some(),
                    // RolesSection.tsx: the reason is drawn only on a role I may edit (a bit I do not hold).
                    reason: (lock == Some(PermLock::NotHeld))
                        .then(|| t("roles.editor.notHeld").into()),
                })
                .collect(),
        })
        .collect();

    let n = rows.len() + usize::from(view.can_manage);
    let focus = {
        let have = roles_ui(w, cx).focus.len();
        let more: Vec<FocusHandle> = (have..n).map(|_| cx.focus_handle()).collect();
        let r = roles_ui(w, cx);
        r.focus.extend(more);
        r.focus[..n].to_vec()
    };
    // The automation build names each row's focus by its element id.
    #[cfg(feature = "automation")]
    let focus_probes: Vec<AnyElement> = view
        .ordered
        .iter()
        .map(|r| format!("role-{}", hex(&r.id)))
        .chain(view.can_manage.then(|| "role-new".to_owned()))
        .zip(focus.iter())
        .map(|(id, h)| crate::automation::focus_probe(super::probe_id(&id), h).into_any_element())
        .collect();

    let status = roles_ui(w, cx)
        .status
        .clone()
        .map(|(s, alert)| (SharedString::from(s), alert));
    let name_error = roles_ui(w, cx).name_error.clone().map(SharedString::from);
    let colours = (!is_everyone).then(|| {
        dilla_client::features::members::signal_palette(colour)
            .into_iter()
            .map(|(value, key)| (value, SharedString::from(t(&format!("roles.color.{key}")))))
            .collect()
    });

    let labels = EditorLabels {
        new_role: t("serverSettings.roles.new").into(),
        name: t("roles.editor.name").into(),
        color: t("roles.editor.color").into(),
    };
    let menu_view = Rc::new(view.clone());
    let perm_view = Rc::new(view.clone());
    let colour_view = Rc::new(view.clone());
    let role_id = role.id;
    let (base, shown_for_save) = (
        role.clone(),
        if is_everyone {
            t("roles.everyone").to_owned()
        } else {
            role.name.clone()
        },
    );
    let base2 = base.clone();
    let shown2 = shown_for_save.clone();
    let mut editor = RoleEditor::new("role-editor", rows, hex(&role.id), &input, labels)
        .focus(focus)
        .on_select(move |id, _w, cx| {
            if let Some(id) = unhex16(id) {
                select_role(w, id, cx);
            }
        })
        .name_read_only(read_only)
        .name_error(name_error)
        .colors(colours, colour)
        .on_color(move |c, _w, cx| {
            if let Some(patch) = colour_view.color_patch(colour, c) {
                roles_ui(w, cx).over = Some(Override {
                    base: base.clone(),
                    allow,
                    color: c,
                });
                save(w, community, role_id, patch, shown_for_save.clone(), cx);
            }
        })
        .groups(groups)
        .on_permission(move |key, on, _w, cx| {
            let Some(k) = PermKey::ALL.iter().find(|k| k.key() == key) else {
                return;
            };
            if let Some(patch) = perm_view.permission_patch(allow, *k, on) {
                roles_ui(w, cx).over = Some(Override {
                    base: base2.clone(),
                    allow: patch.allow.unwrap_or(allow),
                    color: colour,
                });
                save(w, community, role_id, patch, shown2.clone(), cx);
            }
        })
        .menu(
            {
                let v = menu_view.clone();
                move |id| unhex16(id).is_some_and(|id| !v.menu(id).is_empty())
            },
            move |id, at, _w, cx| {
                if let Some(id) = unhex16(id) {
                    roles_ui(w, cx).menu = Some((community, id, at));
                    cx.refresh_windows();
                }
            },
        )
        .status(status);
    if view.can_manage {
        editor = editor.on_new(move |_w, cx| new_role(w, community, cx));
    }
    #[cfg(feature = "automation")]
    {
        editor = editor.probe(Rc::new(|id: &str| {
            crate::automation::bounds_probe(super::probe_id(id)).into_any_element()
        }));
    }

    let mut root = div()
        .id("settings-roles")
        .flex()
        .flex_col()
        .child(head)
        .child(editor);
    #[cfg(feature = "automation")]
    {
        root = root.relative().children(focus_probes);
    }
    if let Some(menu) = role_menu(w, &menu_view, cx) {
        root = root.child(menu);
    }
    if let Some(dialog) = role_delete_dialog(w, cx) {
        root = root.child(dialog);
    }
    root.into_any_element()
}

/// The open role context menu, if any.
fn role_menu(w: WindowId, view: &RolesView, cx: &mut App) -> Option<AnyElement> {
    let (community, id, at) = roles_ui(w, cx).menu?;
    let r = view.ordered.iter().find(|r| r.id == id)?;
    let items: Vec<MenuItem> = view
        .menu(id)
        .into_iter()
        .map(|m| match m {
            RoleMenu::Up => MenuItem::new("up", t("serverSettings.roles.menu.up")),
            RoleMenu::Down => MenuItem::new("down", t("serverSettings.roles.menu.down")),
            RoleMenu::Hoist(on) => {
                MenuItem::new("hoist", t("serverSettings.roles.menu.hoist")).check(on)
            }
            RoleMenu::Mentionable(on) => {
                MenuItem::new("mentionable", t("serverSettings.roles.menu.mentionable")).check(on)
            }
            RoleMenu::Delete => MenuItem::new("delete", t("serverSettings.roles.menu.delete"))
                .danger()
                .sep(),
        })
        .collect();
    if items.is_empty() {
        roles_ui(w, cx).menu = None;
        return None;
    }
    Some(
        ActionMenu::new(
            "role-menu",
            tf("serverSettings.roles.menu", &[("name", &r.name)]),
            items,
        )
        .no_trigger(None)
        .open(true)
        .at(at)
        .on_open_change(move |open, _w, cx| {
            if !open {
                roles_ui(w, cx).menu = None;
                cx.refresh_windows();
            }
        })
        .on_select(move |item, _w, cx| on_menu(w, community, id, item, cx))
        .into_any_element(),
    )
}
