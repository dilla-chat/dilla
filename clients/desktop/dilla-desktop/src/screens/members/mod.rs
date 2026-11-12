//! Area **members** (Task 14): the members aside, the profile card, the person menu, moderation dialogs, the Roles, Self
//! roles, Members, Bans and Moderation log sections.
//!
//! Ports packages/web/src/screens/admin/{MemberPane,MemberCardHost,MemberRolesFlow,Moderation,ModerationLog,BansSection,
//! RolesSection,SelfRolesSection,MembersSection}.tsx. What the views offer comes from the client's
//! `features::members::menu_for` and the loaded `CommunityAccess`; that gate is advisory, and every refusal the instance
//! answers is said by its code in the dialog that sent it.
//!
//! The view's own state (open pane, open card or menu, the dialog in flight) is one app global, [`MembersUi`]. The aside
//! hook (`slots::aside`) receives no state of its own, so the host hands the slot context in with [`sync`] each frame.

pub mod keys;

use std::collections::HashSet;
use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::members::{
    GroupKind as Kind, MenuAction, member_card, member_groups, member_name,
};
use dilla_client::features::moderation::{BanLength, ReasonProblem, TimeoutLength, reason_problem};
use dilla_client::features::roles::perm::ROLE_PERM_GROUPS;
use dilla_client::features::roles::rank;
use dilla_client::features::roles::{Direction, RolePatch, request_id};
use dilla_client::state::members::Presence as Wire;
use dilla_client::state::roles::RoleSummary;
use dilla_client::state::{AppState, CommunityId, UserId};
use dilla_client::sync::api::ApiError;
use gpui::{
    AnyElement, App, AppContext, ClickEvent, Entity, Global, InteractiveElement, IntoElement,
    ParentElement, Pixels, Point, SharedString, StatefulInteractiveElement, Styled, Window,
    anchored, deferred, div, point,
};
use gpui_component::input::InputState;

use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem};
use dilla_desktop_ui::components::avatar::{Avatar, AvatarSize, Presence, colour_of};
use dilla_desktop_ui::components::button::{Button, ButtonVariant};
use dilla_desktop_ui::components::confirm_dialog::ConfirmDialog;
use dilla_desktop_ui::components::members::{
    CardAction, GroupKind, ListGroup, ListMember, MemberList, ProfileCard, role_chip, space,
};
use dilla_desktop_ui::components::select::Select;
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::components::toast::show_toast;
use dilla_desktop_ui::components::toggle::Toggle;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, layout, radius, text};

use crate::router::{Route, ServerSection};
use crate::slots::{SectionId, SlotCx};
use crate::strings::{t, tf};

/// A moderation dialog's kind.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum ModKind {
    Kick,
    Ban,
    Timeout,
    Warn,
    Unban,
}

/// The dialog in flight: its target, and the warning's request id (stable across a retry of the same warning).
#[derive(Clone, Debug)]
pub struct ModDialog {
    pub kind: ModKind,
    pub community: CommunityId,
    pub target: UserId,
    pub name: String,
    pub server: String,
    request: [u8; 16],
}

/// The members area's view state (one per app).
#[derive(Default)]
pub struct MembersUi {
    /// The aside is open.
    pub open: bool,
    /// The host's slot context, handed in by [`sync`].
    pub ctx: Option<SlotCx>,
    /// The member whose card is open, and where.
    pub card: Option<(UserId, Point<Pixels>)>,
    /// The member whose person menu is open, and where.
    pub menu: Option<(UserId, Point<Pixels>)>,
    pub dialog: Option<ModDialog>,
    pub busy: bool,
    pub error: Option<String>,
    pub ban_length: Option<BanLength>,
    pub timeout_length: Option<TimeoutLength>,
    reason: Option<Entity<InputState>>,
    /// The role open in the Roles section.
    pub role: Option<[u8; 16]>,
    /// What this view asked the client to read already (a section reads once per community).
    asked: HashSet<(&'static str, CommunityId)>,
    /// The Moderation log's answer (its sealed Bot read is Task 20's; until then it says the Bot could not answer).
    log_unavailable: bool,
}

impl Global for MembersUi {}

fn ui(cx: &mut App) -> &mut MembersUi {
    if !cx.has_global::<MembersUi>() {
        cx.set_global(MembersUi::default());
    }
    cx.global_mut::<MembersUi>()
}

/// The host hands its slot context in before it draws the aside (the aside hook takes none of its own).
pub fn sync(ctx: &SlotCx, cx: &mut App) {
    ui(cx).ctx = Some(ctx.clone());
}

/// Whether the aside is open.
pub fn is_open(cx: &App) -> bool {
    cx.try_global::<MembersUi>().is_some_and(|u| u.open)
}

fn presence_of(p: Wire) -> Presence {
    match p {
        Wire::Online => Presence::Online,
        Wire::Idle => Presence::Idle,
        Wire::Dnd => Presence::Dnd,
        Wire::Offline => Presence::Offline,
    }
}

fn presence_word(p: Wire) -> &'static str {
    t(match p {
        Wire::Online => "presence.online",
        Wire::Idle => "presence.idle",
        Wire::Dnd => "presence.dnd",
        Wire::Offline => "presence.offline",
    })
}

/// Asks the client once per community for what the members views read: the presence, and the access with the roles
/// and the overwrites.
fn ensure_loaded(
    what: &'static str,
    community: CommunityId,
    client: Option<&Rc<Client>>,
    cx: &mut App,
) {
    let Some(client) = client else { return };
    if !ui(cx).asked.insert((what, community)) {
        return;
    }
    let job = match what {
        "bans" => {
            let j = client
                .moderation()
                .run(move |m| async move { m.load_bans(community).await.map(|_| ()) });
            Box::pin(j)
                as std::pin::Pin<Box<dyn std::future::Future<Output = Result<(), ApiError>>>>
        }
        "self" => Box::pin(
            client
                .roles()
                .run(move |r| async move { r.load_self_roles(community).await.map(|_| ()) }),
        ),
        _ => Box::pin(
            client
                .members()
                .run(move |m| async move { m.open(community).await }),
        ),
    };
    cx.spawn(async move |cx| {
        if job.await.is_err() {
            // Read again on the next draw.
            cx.update(|cx| {
                ui(cx).asked.remove(&(what, community));
            });
        }
    })
    .detach();
}

/// The list's groups for `community` (MemberPane.tsx): the client's grouping, the status line (a custom status, else
/// the presence word; your own row has no custom status).
fn groups(state: &AppState, community: CommunityId) -> Option<Vec<ListGroup>> {
    let members = state.members.get(&community)?;
    let presence = state.people.presence.get(&community);
    let roles = state.roles.roles_of(community);
    let owner = state.communities.iter().find(|c| c.id == community)?.owner;
    let me = state.account.user_id;
    let status = |id: UserId| -> SharedString {
        let m = members.iter().find(|m| m.user_id == id);
        let custom = m
            .filter(|_| Some(id) != me)
            .map_or("", |m| m.status_msg.as_str());
        if custom.is_empty() {
            presence_word(state.people.presence_of(community, id)).into()
        } else {
            custom.to_owned().into()
        }
    };
    Some(
        member_groups(members, roles, owner, me, presence)
            .into_iter()
            .map(|g| ListGroup {
                id: g.role.as_ref().map_or_else(
                    || {
                        match g.kind {
                            Kind::Online => "online",
                            Kind::Bots => "bots",
                            Kind::Offline => "offline",
                            Kind::Role => "role",
                        }
                        .into()
                    },
                    |r| hex(&r.id).into(),
                ),
                kind: match g.kind {
                    Kind::Role => GroupKind::Role,
                    Kind::Online => GroupKind::Online,
                    Kind::Bots => GroupKind::Bots,
                    Kind::Offline => GroupKind::Offline,
                },
                title: g.role.as_ref().map_or_else(
                    || {
                        t(match g.kind {
                            Kind::Offline => "layout.members.offline",
                            Kind::Bots => "layout.members.bots",
                            _ => "layout.members.online",
                        })
                        .into()
                    },
                    |r| r.name.clone().into(),
                ),
                color: g.role.as_ref().map(|r| r.color),
                members: g
                    .members
                    .iter()
                    .map(|row| ListMember {
                        id: row.user_id.hex().into(),
                        name: row.name.clone().into(),
                        is_owner: row.user_id == owner,
                        is_bot: members
                            .iter()
                            .any(|m| m.user_id == row.user_id && m.kind == 1),
                        status: status(row.user_id),
                        color: row.color,
                        presence: presence_of(state.people.presence_of(community, row.user_id)),
                    })
                    .collect(),
            })
            .collect(),
    )
}

fn hex(bytes: &[u8]) -> String {
    bytes.iter().map(|b| format!("{b:02x}")).collect()
}

fn user_of(hex: &str) -> Option<UserId> {
    if hex.len() != 32 {
        return None;
    }
    let mut out = [0u8; 16];
    for (i, b) in out.iter_mut().enumerate() {
        *b = u8::from_str_radix(&hex[2 * i..2 * i + 2], 16).ok()?;
    }
    Some(UserId(out))
}

/// The member list, its card, its person menu and the dialog in flight, for `community`.
fn list_with_overlays(
    ctx: &SlotCx,
    community: CommunityId,
    head: bool,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    ensure_loaded("people", community, ctx.client.as_ref(), cx);
    let state = ctx.state.clone();
    let groups = groups(&state, community);
    let loading = groups.is_none() || !state.people.presence.contains_key(&community);
    let open_id = ui(cx).card.map(|(u, _)| SharedString::from(u.hex()));
    let mut list = MemberList::new("members-list", groups.unwrap_or_default())
        .loading(loading, t("members.loading"))
        .empty_label(t("members.empty"))
        .bot_label(t("layout.profile.bot"))
        .open_id(open_id)
        .on_open(|id, window, cx| {
            let Some(user) = user_of(id) else { return };
            let at = window.mouse_position();
            let u = ui(cx);
            u.menu = None;
            u.card = if u.card.is_some_and(|(c, _)| c == user) {
                None
            } else {
                Some((user, at))
            };
            cx.refresh_windows();
        })
        .on_menu(|id, at, _window, cx| {
            let Some(user) = user_of(id) else { return };
            let u = ui(cx);
            u.card = None;
            u.menu = Some((user, at));
            cx.refresh_windows();
        });
    if head {
        list = list
            .head(t("members.toggle"), Some(t("members.close").into()))
            .on_close(|_, cx| {
                ui(cx).open = false;
                cx.refresh_windows();
            });
    }
    let mut root = div().flex().flex_col().size_full().child(list);
    if let Some(card) = card_overlay(ctx, community, window, cx) {
        root = root.child(card);
    }
    if let Some(menu) = menu_overlay(ctx, community, cx) {
        root = root.child(menu);
    }
    if let Some(dialog) = dialog_overlay(ctx, window, cx) {
        root = root.child(dialog);
    }
    root.into_any_element()
}

fn action_icon(a: MenuAction) -> (IconName, bool) {
    match a {
        MenuAction::Message => (IconName::MessageSquare, false),
        MenuAction::Compare => (IconName::Shield, false),
        MenuAction::Roles => (IconName::Users, false),
        MenuAction::Warn => (IconName::Shield, false),
        MenuAction::Timeout => (IconName::AlarmClock, false),
        MenuAction::Kick => (IconName::UserX, true),
        MenuAction::Ban => (IconName::Ban, true),
    }
}

fn action_id(a: MenuAction) -> &'static str {
    match a {
        MenuAction::Message => "message",
        MenuAction::Compare => "compare",
        MenuAction::Roles => "roles",
        MenuAction::Warn => "warn",
        MenuAction::Timeout => "timeout",
        MenuAction::Kick => "kick",
        MenuAction::Ban => "ban",
    }
}

fn action_label(a: MenuAction, name: &str) -> String {
    match a {
        MenuAction::Message => t("members.menu.message").to_owned(),
        MenuAction::Compare => t("members.menu.compare").to_owned(),
        MenuAction::Roles => t("members.menu.roles").to_owned(),
        MenuAction::Warn => t("mod.warn.menu").to_owned(),
        MenuAction::Timeout => t("mod.timeout.menu").to_owned(),
        MenuAction::Kick => tf("members.menu.kickName", &[("name", name)]),
        MenuAction::Ban => tf("members.menu.banName", &[("name", name)]),
    }
}

/// Runs a chosen action on `user` (the card's buttons and the person menu's items).
fn choose(community: CommunityId, user: UserId, id: &str, cx: &mut App) {
    let Some(ctx) = ui(cx).ctx.clone() else {
        return;
    };
    let state = ctx.state.clone();
    let Some(member) = state
        .members
        .get(&community)
        .and_then(|m| m.iter().find(|m| m.user_id == user))
    else {
        return;
    };
    let server = state
        .communities
        .iter()
        .find(|c| c.id == community)
        .map_or_else(String::new, |c| c.name.clone());
    let kind = match id {
        "kick" => ModKind::Kick,
        "ban" => ModKind::Ban,
        "timeout" => ModKind::Timeout,
        "warn" => ModKind::Warn,
        // Message, compare and roles hand over to the DM, trust and roles views (Tasks 13 and 28 wire them).
        _ => return,
    };
    let u = ui(cx);
    u.card = None;
    u.menu = None;
    u.error = None;
    u.busy = false;
    u.ban_length = Some(BanLength::Day);
    u.timeout_length = Some(TimeoutLength::TenMinutes);
    u.reason = None;
    u.dialog = Some(ModDialog {
        kind,
        community,
        target: user,
        name: member_name(member).to_owned(),
        server,
        request: request_id(),
    });
    cx.refresh_windows();
}

fn card_overlay(
    ctx: &SlotCx,
    community: CommunityId,
    window: &mut Window,
    cx: &mut App,
) -> Option<AnyElement> {
    let (user, at) = ui(cx).card?;
    let state = &ctx.state;
    let member = state
        .members
        .get(&community)?
        .iter()
        .find(|m| m.user_id == user)?;
    let me = state.account.user_id?;
    let owner = state.communities.iter().find(|c| c.id == community)?.owner;
    let access = state.roles.access.get(&community);
    let card = member_card(member, access, me, owner, state.roles.roles_of(community));
    let presence = state.people.presence_of(community, user);
    let sub = if card.status.is_empty() {
        format!("@{} · {}", card.username, presence_word(presence))
    } else {
        format!("@{} · {}", card.username, card.status)
    };
    let actions: Vec<CardAction> = card
        .actions
        .iter()
        .filter(|a| **a != MenuAction::Message)
        .map(|a| {
            let (icon, danger) = action_icon(*a);
            CardAction {
                id: action_id(*a).into(),
                label: action_label(*a, &card.name).into(),
                icon,
                danger,
            }
        })
        .collect();
    let mut view = ProfileCard::new(
        "profile-card",
        card.name.clone(),
        sub,
        colour_of(&card.name).0,
    )
    .bio(card.bio.clone())
    .owner(card.is_owner)
    .bot(card.is_bot)
    .roles(
        card.roles
            .iter()
            .map(|(n, c)| (n.clone().into(), *c))
            .collect(),
    )
    .actions(actions)
    .on_action(move |id, _w, cx| choose(community, user, id, cx));
    if card.actions.contains(&MenuAction::Message) {
        view = view.message(t("members.menu.message"));
    }
    // Beside the row, left of the aside, inside the window.
    let viewport = window.viewport_size();
    let x =
        (viewport.width - layout::MEMBERS - space::CARD_W - space::CARD_EDGE).max(space::CARD_EDGE);
    let y =
        at.y.min(viewport.height - space::CARD_MAX_H)
            .max(space::CARD_EDGE);
    Some(
        deferred(anchored().position(point(x, y)).child(view))
            .with_priority(1)
            .into_any_element(),
    )
}

fn menu_overlay(ctx: &SlotCx, community: CommunityId, cx: &mut App) -> Option<AnyElement> {
    let (user, at) = ui(cx).menu?;
    let state = &ctx.state;
    let member = state
        .members
        .get(&community)?
        .iter()
        .find(|m| m.user_id == user)?;
    let me = state.account.user_id?;
    let owner = state.communities.iter().find(|c| c.id == community)?.owner;
    let access = state.roles.access.get(&community);
    let card = member_card(member, access, me, owner, state.roles.roles_of(community));
    let mut items = vec![MenuItem::new("profile", t("members.menu.profile"))];
    for a in &card.actions {
        let (icon, danger) = action_icon(*a);
        let mut item = MenuItem::new(action_id(*a), action_label(*a, &card.name)).icon(icon);
        if danger {
            item = item.danger();
        }
        items.push(item);
    }
    let head = div()
        .flex()
        .items_center()
        .gap(space::S10)
        .px(space::S8)
        .pt(space::S8)
        .pb(space::S10)
        .mb(space::S4)
        .border_b_1()
        .border_color(color::HAIRLINE)
        .child(Avatar::new("person-menu-avatar", card.name.clone()).size(AvatarSize::Member))
        .child(
            line(
                card.name.clone(),
                text::SM,
                line_height(text::SM, 1.3),
                color::FG,
            )
            .weight(font::WEIGHT_DISPLAY)
            .ellipsis(),
        );
    Some(
        ActionMenu::new(
            "person-menu",
            tf("members.menu.label", &[("name", &card.name)]),
            items,
        )
        .no_trigger(None)
        .open(true)
        .at(at)
        .width(space::PERSON_MENU_W)
        .head(head)
        .on_open_change(|open, _w, cx| {
            if !open {
                ui(cx).menu = None;
                cx.refresh_windows();
            }
        })
        .on_select(move |id, window, cx| {
            if id == "profile" {
                let at = window.mouse_position();
                let u = ui(cx);
                u.menu = None;
                u.card = Some((user, at));
                cx.refresh_windows();
            } else {
                choose(community, user, id, cx);
            }
        })
        .into_any_element(),
    )
}

/// The refusal's words, by its E_ code (Moderation.tsx).
fn refusal_text(kind: ModKind, e: &ApiError, name: &str, server: &str) -> String {
    let vars = [("name", name), ("server", server)];
    match (kind, e.code.as_str(), e.status) {
        (ModKind::Kick, "E_FORBIDDEN", _) => tf("mod.kick.error.forbidden", &vars),
        (ModKind::Kick, "E_NOT_FOUND", _) => tf("mod.kick.error.gone", &vars),
        (ModKind::Ban, "E_FORBIDDEN", _) => tf("mod.ban.error.forbidden", &vars),
        (ModKind::Ban, "E_INVALID_REQUEST", 400) => t("mod.ban.error.clock").to_owned(),
        (ModKind::Timeout, "E_FORBIDDEN", _) => tf("mod.timeout.error.forbidden", &vars),
        (ModKind::Timeout, "E_INVALID_REQUEST", 400) => t("mod.timeout.error.clock").to_owned(),
        (ModKind::Warn, "E_FORBIDDEN", _) => tf("mod.warn.error.forbidden", &vars),
        (_, code, _) => tf("mod.error.other", &[("code", code)]),
    }
}

/// Sends the open dialog's command; the error and the end of busy land together (ConfirmDialog's contract).
fn confirm(cx: &mut App) {
    let u = ui(cx);
    let Some(d) = u.dialog.clone() else { return };
    if u.busy {
        return;
    }
    let input = u.reason.clone();
    let reason = input
        .map(|r| r.read(cx).value().to_string())
        .unwrap_or_default();
    let u = ui(cx);
    let local = match d.kind {
        ModKind::Ban => reason_problem(&reason).map(|p| match p {
            ReasonProblem::Long => t("mod.ban.error.reasonLong"),
            ReasonProblem::Control => t("mod.ban.error.reasonControl"),
        }),
        ModKind::Warn => match reason_problem(&reason) {
            Some(ReasonProblem::Long) => Some(t("mod.warn.error.reasonLong")),
            Some(ReasonProblem::Control) => Some(t("mod.warn.error.reasonControl")),
            None if reason.trim().is_empty() => Some(t("mod.warn.error.reasonEmpty")),
            None => None,
        },
        _ => None,
    };
    if let Some(problem) = local {
        u.error = Some(problem.to_owned());
        cx.refresh_windows();
        return;
    }
    let Some(client) = u.ctx.as_ref().and_then(|c| c.client.clone()) else {
        return;
    };
    let (ban, timeout) = (
        u.ban_length.unwrap_or(BanLength::Day),
        u.timeout_length.unwrap_or(TimeoutLength::TenMinutes),
    );
    u.busy = true;
    u.error = None;
    cx.refresh_windows();
    let (c, user, request) = (d.community, d.target, d.request);
    let job = client.moderation().run(move |m| async move {
        match d.kind {
            ModKind::Kick => m.kick(c, user).await.map(|()| false),
            ModKind::Ban => m.ban(c, user, &reason, ban.seconds()).await.map(|()| false),
            ModKind::Unban => m.unban(c, user).await.map(|()| false),
            ModKind::Timeout => m.timeout(c, user, timeout).await.map(|()| false),
            ModKind::Warn => m
                .warn(c, user, request, &reason)
                .await
                .map(|r| r.evidence != dilla_client::features::moderation::Evidence::Accepted),
        }
    });
    cx.spawn(async move |cx| {
        let answer = job.await;
        cx.update(|cx| {
            let u = ui(cx);
            // A dialog closed or replaced meanwhile keeps its own state.
            if u.dialog.as_ref().is_none_or(|now| now.request != d.request) {
                return;
            }
            u.busy = false;
            match answer {
                Ok(pending) => {
                    u.dialog = None;
                    let vars = [("name", d.name.as_str()), ("server", d.server.as_str())];
                    let done = match d.kind {
                        ModKind::Kick => tf("mod.kick.done", &vars),
                        ModKind::Ban => tf("mod.ban.done", &vars),
                        ModKind::Unban => tf("admin.bans.unbanned", &vars),
                        ModKind::Timeout => tf("mod.timeout.done", &vars),
                        ModKind::Warn if pending => tf("mod.warn.evidencePending", &vars),
                        ModKind::Warn => tf("mod.warn.done", &vars),
                    };
                    show_toast(done, cx);
                }
                Err(e) => u.error = Some(refusal_text(d.kind, &e, &d.name, &d.server)),
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

fn dialog_overlay(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let _ = ctx;
    let d = ui(cx).dialog.clone()?;
    let vars = [("name", d.name.as_str()), ("server", d.server.as_str())];
    let (title, body, ok, busy) = match d.kind {
        ModKind::Kick => (
            "mod.kick.title",
            "mod.kick.body",
            "mod.kick.confirm",
            "mod.kick.busy",
        ),
        ModKind::Ban => (
            "mod.ban.title",
            "mod.ban.body",
            "mod.ban.confirm",
            "mod.ban.busy",
        ),
        ModKind::Timeout => (
            "mod.timeout.title",
            "mod.timeout.body",
            "mod.timeout.confirm",
            "mod.timeout.busy",
        ),
        ModKind::Warn => (
            "mod.warn.title",
            "mod.warn.body",
            "mod.warn.confirm",
            "mod.warn.busy",
        ),
        ModKind::Unban => (
            "admin.bans.unbanTitle",
            "admin.bans.unbanBody",
            "admin.bans.unbanConfirm",
            "admin.bans.unbanBusy",
        ),
    };
    if matches!(d.kind, ModKind::Ban | ModKind::Warn) && ui(cx).reason.is_none() {
        let input = cx.new(|cx| InputState::new(window, cx));
        ui(cx).reason = Some(input);
    }
    let u = ui(cx);
    let (is_busy, error, reason) = (u.busy, u.error.clone(), u.reason.clone());
    let (ban, timeout) = (u.ban_length, u.timeout_length);
    let fields: Option<AnyElement> = match d.kind {
        ModKind::Ban => {
            let reason = reason?;
            let options: Vec<(&str, String)> = BanLength::ALL
                .iter()
                .map(|l| {
                    (
                        l.key(),
                        t(&format!("mod.ban.duration.{}", l.key())).to_owned(),
                    )
                })
                .collect();
            let pairs: Vec<(&str, &str)> = options.iter().map(|(k, l)| (*k, l.as_str())).collect();
            Some(
                div()
                    .flex()
                    .flex_col()
                    .gap(space::S12)
                    .child(
                        TextField::new("mod-ban-reason", t("mod.ban.reason"), &reason)
                            .hint(tf("mod.ban.reasonHint", &[("name", &d.name)]))
                            .disabled(is_busy),
                    )
                    .child(
                        Select::new(
                            "mod-ban-duration",
                            t("mod.ban.duration"),
                            &pairs,
                            ban.unwrap_or(BanLength::Day).key(),
                        )
                        .disabled(is_busy)
                        .on_change(|v, _w, cx| {
                            ui(cx).ban_length = BanLength::ALL.into_iter().find(|l| l.key() == v);
                            cx.refresh_windows();
                        }),
                    )
                    .into_any_element(),
            )
        }
        ModKind::Warn => Some(
            TextField::new("mod-warn-reason", t("mod.warn.reason"), &reason?)
                .disabled(is_busy)
                .into_any_element(),
        ),
        ModKind::Timeout => {
            let options: Vec<(&str, String)> = TimeoutLength::ALL
                .iter()
                .map(|l| {
                    (
                        l.key(),
                        t(&format!("mod.timeout.duration.{}", l.key())).to_owned(),
                    )
                })
                .collect();
            let pairs: Vec<(&str, &str)> = options.iter().map(|(k, l)| (*k, l.as_str())).collect();
            Some(
                Select::new(
                    "mod-timeout-duration",
                    t("mod.timeout.duration"),
                    &pairs,
                    timeout.unwrap_or(TimeoutLength::TenMinutes).key(),
                )
                .disabled(is_busy)
                .on_change(|v, _w, cx| {
                    ui(cx).timeout_length = TimeoutLength::ALL.into_iter().find(|l| l.key() == v);
                    cx.refresh_windows();
                })
                .into_any_element(),
            )
        }
        _ => None,
    };
    let mut dialog = ConfirmDialog::new(
        "mod-dialog",
        tf(title, &vars),
        tf(body, &vars),
        t(ok),
        t(busy),
        t("mod.cancel"),
    )
    .person(d.name.clone())
    .busy(is_busy)
    .error(error.map(SharedString::from))
    .on_confirm(|_w, cx| confirm(cx))
    .on_close(|_w, cx| {
        let u = ui(cx);
        u.dialog = None;
        u.busy = false;
        u.error = None;
        cx.refresh_windows();
    });
    if let Some(fields) = fields {
        dialog = dialog.fields(fields, None);
    }
    Some(dialog.into_any_element())
}

/// This area's aside (`slots::aside`): `None` while it is closed.
pub fn pane(route: &Route, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let Route::Channel { community, .. } = route else {
        return None;
    };
    let community = *community;
    let u = ui(cx);
    if !u.open {
        return None;
    }
    let ctx = u.ctx.clone()?;
    Some(
        div()
            .id("members-pane")
            .w(layout::MEMBERS)
            .h_full()
            .flex_none()
            .border_l_1()
            .border_color(color::HAIRLINE)
            .bg(color::BG_2)
            .key_context("MembersPane")
            .on_action(|_: &keys::CloseCard, _w, cx| {
                let u = ui(cx);
                if u.card.take().is_some() || u.menu.take().is_some() {
                    cx.refresh_windows();
                }
            })
            .child(list_with_overlays(&ctx, community, true, window, cx))
            .into_any_element(),
    )
}

/// Opens or closes this area's aside. The channel header's button (Task 8) calls it. Fixed signature.
pub fn toggle_pane(cx: &mut App) {
    let u = ui(cx);
    u.open = !u.open;
    if !u.open {
        u.card = None;
        u.menu = None;
    }
    cx.refresh_windows();
}

// ---- settings sections ----

fn section_head(title: &str, body: Option<String>) -> gpui::Div {
    let mut head = div().flex().flex_col().gap(space::S6).mb(space::S16).child(
        line(
            title.to_owned(),
            text::HEADING,
            line_height(text::HEADING, 1.3),
            color::FG,
        )
        .weight(font::WEIGHT_DISPLAY),
    );
    if let Some(body) = body {
        head = head.child(line(
            body,
            text::UI,
            line_height(text::UI, 1.5),
            color::FG_2,
        ));
    }
    head
}

fn note(s: impl Into<SharedString>) -> AnyElement {
    line(s.into(), text::UI, line_height(text::UI, 1.5), color::FG_3).into_any_element()
}

fn sorted_roles(state: &AppState, community: CommunityId) -> Vec<RoleSummary> {
    let mut roles: Vec<RoleSummary> = state.roles.roles_of(community).to_vec();
    roles.sort_by(|a, b| b.position.cmp(&a.position).then(a.id.cmp(&b.id)));
    roles
}

/// Runs a roles command and says a refusal by its code.
fn roles_job(
    client: &Rc<Client>,
    cx: &mut App,
    job: impl FnOnce(
        dilla_client::features::roles::Roles,
    ) -> std::pin::Pin<
        Box<dyn std::future::Future<Output = Result<(), ApiError>> + Send>,
    > + 'static,
) {
    let fut = client.roles().run(job);
    cx.spawn(async move |cx| {
        if let Err(e) = fut.await {
            cx.update(|cx| show_toast(tf("roles.assign.error.other", &[("code", &e.code)]), cx));
        }
    })
    .detach();
}

fn roles_section(ctx: &SlotCx, community: CommunityId, cx: &mut App) -> AnyElement {
    ensure_loaded("people", community, ctx.client.as_ref(), cx);
    let state = ctx.state.clone();
    let access = state.roles.access_of(community);
    let roles = sorted_roles(&state, community);
    let mine = access.top;
    let can = access.loaded && access.manage_roles;
    // A role at or above my rank is locked (the instance refuses it): the owner ranks above every role.
    let locked = |r: &RoleSummary| !can || (!access.owner && r.position >= mine);
    let open = ui(cx).role.filter(|id| roles.iter().any(|r| r.id == *id));
    let list = div()
        .flex()
        .flex_col()
        .gap(space::S2)
        .children(roles.iter().map(|r| {
            let id = r.id;
            let tint = if r.color == 0 {
                color::FG_2
            } else {
                gpui::rgb(r.color).into()
            };
            div()
                .id(SharedString::from(format!("role-{}", r.name)))
                .h(layout::ROW_H_2)
                .px(space::S10)
                .flex()
                .items_center()
                .gap(space::S10)
                .rounded(radius::MD)
                .when_some_bg(open == Some(id))
                .hover(|s| s.bg(color::HOVER))
                .cursor_pointer()
                .child(div().size(space::ROLE_DOT).rounded(radius::PILL).bg(tint))
                .child(
                    div().flex_1().min_w_0().flex().child(
                        line(
                            r.name.clone(),
                            text::SM,
                            line_height(text::SM, 1.5),
                            color::FG,
                        )
                        .ellipsis(),
                    ),
                )
                .child(line(
                    r.members.to_string(),
                    text::META,
                    line_height(text::META, 1.5),
                    color::FG_3,
                ))
                .on_click(move |_: &ClickEvent, _w, cx| {
                    ui(cx).role = Some(id);
                    cx.refresh_windows();
                })
        }));
    let mut root = div()
        .id("settings-roles")
        .flex()
        .flex_col()
        .child(section_head(
            t("roles.title"),
            Some(t("roles.body").to_owned()),
        ));
    if can && let Some(client) = ctx.client.clone() {
        root = root.child(
            div().mb(space::S12).child(
                Button::new("role-create", t("roles.new"))
                    .icon(IconName::Plus)
                    .on_click(move |_, _w, cx| {
                        roles_job(&client, cx, move |r| {
                            Box::pin(async move {
                                r.create_role(community, t("roles.new"), 0, false, false, 0)
                                    .await
                                    .map(|_| ())
                            })
                        });
                    }),
            ),
        );
    }
    root = root.child(list);
    if let Some(role) = open.and_then(|id| roles.iter().find(|r| r.id == id)) {
        root = root.child(role_editor(ctx, community, role, locked(role), cx));
    }
    root.into_any_element()
}

trait OpenBg {
    fn when_some_bg(self, on: bool) -> Self;
}

impl OpenBg for gpui::Stateful<gpui::Div> {
    fn when_some_bg(self, on: bool) -> Self {
        if on { self.bg(color::HOVER) } else { self }
    }
}

/// The role editor (RoleEditor + PermissionGrid): the six swatches, hoist and mentionable, then every permission under
/// the prototype's four headings. A locked role (at or above my rank, or without manage_roles) is read-only.
fn role_editor(
    ctx: &SlotCx,
    community: CommunityId,
    role: &RoleSummary,
    locked: bool,
    _cx: &mut App,
) -> AnyElement {
    let client = ctx.client.clone();
    let id = role.id;
    let patch = move |p: RolePatch, cx: &mut App| {
        if let Some(client) = &client {
            roles_job(client, cx, move |r| {
                Box::pin(async move { r.update_role(community, id, p).await })
            });
        }
    };
    let patch = Rc::new(patch);
    let swatches = div().flex().gap(space::S8).children(
        dilla_client::features::members::signal_palette(role.color)
            .into_iter()
            .map(|(value, key)| {
                let patch = patch.clone();
                let chosen = value == role.color;
                div()
                    .id(SharedString::from(format!("role-colour-{key}")))
                    .size(space::SWATCH)
                    .rounded(radius::PILL)
                    .bg(gpui::rgb(value))
                    .when_chosen(chosen)
                    .cursor_pointer()
                    .on_click(move |_: &ClickEvent, _w, cx| {
                        if !locked {
                            patch(
                                RolePatch {
                                    color: Some(value),
                                    ..RolePatch::default()
                                },
                                cx,
                            );
                        }
                    })
            }),
    );
    let flag = |key: &'static str, label: &'static str, on: bool, which: fn(bool) -> RolePatch| {
        let patch = patch.clone();
        Toggle::new(SharedString::from(format!("role-{key}")), t(label), on)
            .disabled(locked || role.position == 0)
            .on_change(move |v, _w, cx| patch(which(v), cx))
    };
    let allow = role.allow;
    let grid = div()
        .flex()
        .flex_col()
        .gap(space::S14)
        .children(ROLE_PERM_GROUPS.iter().map(|(group, keys)| {
            div()
                .flex()
                .flex_col()
                .gap(space::S6)
                .child(
                    line(
                        t(&format!("perm.group.{group}")).to_uppercase(),
                        text::MICRO,
                        line_height(text::MICRO, 1.5),
                        color::FG_3,
                    )
                    .weight(font::WEIGHT_LABEL)
                    .tracking(dilla_desktop_ui::metrics::LABEL_TRACKING_WIDE),
                )
                .children(keys.iter().map(|k| {
                    let patch = patch.clone();
                    let bit = k.bit();
                    Toggle::new(
                        SharedString::from(format!("perm-{}", k.key())),
                        t(&format!("perm.{}", k.key())),
                        allow & bit != 0,
                    )
                    .disabled(locked)
                    .on_change(move |on, _w, cx| {
                        let next = if on { allow | bit } else { allow & !bit };
                        patch(
                            RolePatch {
                                allow: Some(next),
                                ..RolePatch::default()
                            },
                            cx,
                        );
                    })
                }))
        }));
    let mut editor = div()
        .id("role-editor")
        .mt(space::S16)
        .p(space::S16)
        .flex()
        .flex_col()
        .gap(space::S14)
        .border_1()
        .border_color(color::CARD_EDGE)
        .rounded(radius::XL)
        .child(role_chip(role.name.clone(), role.color));
    if role.position != 0 {
        editor = editor
            .child(swatches)
            .child(flag("hoist", "roles.editor.hoist", role.hoist, |v| {
                RolePatch {
                    hoist: Some(v),
                    ..RolePatch::default()
                }
            }))
            .child(flag(
                "mentionable",
                "roles.editor.mentionable",
                role.mentionable,
                |v| RolePatch {
                    mentionable: Some(v),
                    ..RolePatch::default()
                },
            ));
    }
    editor = editor.child(grid);
    if !locked
        && role.position != 0
        && let Some(client) = ctx.client.clone()
    {
        let (c2, c3) = (client.clone(), client.clone());
        editor = editor.child(
            div()
                .flex()
                .gap(space::S8)
                .child(
                    Button::new("role-up", t("roles.up"))
                        .icon(IconName::ChevronUp)
                        .on_click(move |_, _w, cx| {
                            roles_job(&client, cx, move |r| {
                                Box::pin(
                                    async move { r.move_role(community, id, Direction::Up).await },
                                )
                            });
                        }),
                )
                .child(
                    Button::new("role-down", t("roles.down")).on_click(move |_, _w, cx| {
                        roles_job(&c2, cx, move |r| {
                            Box::pin(
                                async move { r.move_role(community, id, Direction::Down).await },
                            )
                        });
                    }),
                )
                .child(
                    Button::new("role-delete", t("roles.delete"))
                        .variant(ButtonVariant::Danger)
                        .icon(IconName::Trash2)
                        .on_click(move |_, _w, cx| {
                            ui(cx).role = None;
                            roles_job(&c3, cx, move |r| {
                                Box::pin(async move { r.delete_role(community, id).await })
                            });
                        }),
                ),
        );
    }
    editor.into_any_element()
}

trait Chosen {
    fn when_chosen(self, on: bool) -> Self;
}

impl Chosen for gpui::Stateful<gpui::Div> {
    fn when_chosen(self, on: bool) -> Self {
        if on {
            self.border_2().border_color(color::FG)
        } else {
            self
        }
    }
}

fn self_roles_section(ctx: &SlotCx, community: CommunityId, cx: &mut App) -> AnyElement {
    ensure_loaded("people", community, ctx.client.as_ref(), cx);
    ensure_loaded("self", community, ctx.client.as_ref(), cx);
    let state = ctx.state.clone();
    let access = state.roles.access_of(community);
    let mut root = div()
        .id("settings-self-roles")
        .flex()
        .flex_col()
        .gap(space::S6)
        .child(section_head(
            t("admin.bot.selfRoles.title"),
            Some(t("admin.bot.selfRoles.configure").to_owned()),
        ));
    let Some(list) = state.roles.self_roles.get(&community) else {
        return root.child(note(t("members.loading"))).into_any_element();
    };
    let offered: HashSet<[u8; 16]> = list.iter().map(|r| r.id).collect();
    if access.loaded && access.manage_roles {
        // The policy: which roles below my rank members may pick for themselves.
        let roles: Vec<RoleSummary> = sorted_roles(&state, community)
            .into_iter()
            .filter(|r| r.position != 0 && (access.owner || r.position < access.top))
            .collect();
        for r in roles {
            let (id, offered_now) = (r.id, offered.clone());
            let client = ctx.client.clone();
            root = root.child(
                Toggle::new(
                    SharedString::from(format!("self-role-{}", r.name)),
                    r.name.clone(),
                    offered.contains(&r.id),
                )
                .on_change(move |on, _w, cx| {
                    let mut next: Vec<[u8; 16]> =
                        offered_now.iter().copied().filter(|x| *x != id).collect();
                    if on {
                        next.push(id);
                    }
                    if let Some(client) = &client {
                        roles_job(client, cx, move |r| {
                            Box::pin(async move { r.configure_self_roles(community, &next).await })
                        });
                    }
                }),
            );
        }
    } else if list.is_empty() {
        root = root.child(note(t("admin.bot.selfRoles.empty")));
    } else {
        for r in list {
            let (id, client) = (r.id, ctx.client.clone());
            root = root.child(
                Toggle::new(
                    SharedString::from(format!("self-role-{}", r.name)),
                    r.name.clone(),
                    r.self_selected,
                )
                .on_change(move |on, _w, cx| {
                    if let Some(client) = &client {
                        roles_job(client, cx, move |r| {
                            Box::pin(async move { r.select_self_role(community, id, on).await })
                        });
                    }
                }),
            );
        }
    }
    root.into_any_element()
}

fn bans_section(
    ctx: &SlotCx,
    community: CommunityId,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    ensure_loaded("people", community, ctx.client.as_ref(), cx);
    ensure_loaded("bans", community, ctx.client.as_ref(), cx);
    let state = ctx.state.clone();
    let server = state
        .communities
        .iter()
        .find(|c| c.id == community)
        .map_or_else(String::new, |c| c.name.clone());
    let access = state.roles.access_of(community);
    let mut root = div()
        .id("settings-bans")
        .flex()
        .flex_col()
        .gap(space::S4)
        .child(section_head(t("admin.bans.title"), None));
    match state.people.bans.get(&community) {
        None => root = root.child(note(t("admin.bans.loading"))),
        Some(bans) if bans.is_empty() => {
            root = root.child(note(tf("admin.bans.empty", &[("server", &server)])));
        }
        Some(bans) => {
            for b in bans {
                let name = if b.name.is_empty() {
                    t("admin.bans.unknownUser").to_owned()
                } else {
                    b.name.clone()
                };
                let reason = if b.reason.is_empty() {
                    t("admin.bans.noReason").to_owned()
                } else {
                    b.reason.clone()
                };
                let until = match b.expires {
                    None => t("admin.bans.forever").to_owned(),
                    Some(at) => tf(
                        "admin.bans.until",
                        &[("date", &crate::format::day_time(at))],
                    ),
                };
                let (user, name2, server2) = (b.user_id, name.clone(), server.clone());
                let mut row = div()
                    .id(SharedString::from(format!("ban-{}", b.user_id.hex())))
                    .min_h(layout::ROW_H_2)
                    .px(space::S10)
                    .py(space::S6)
                    .flex()
                    .items_center()
                    .gap(space::S10)
                    .border_b_1()
                    .border_color(color::HAIRLINE)
                    .child(
                        Avatar::new(
                            SharedString::from(format!("ban-avatar-{}", b.user_id.hex())),
                            name.clone(),
                        )
                        .size(AvatarSize::Member),
                    )
                    .child(
                        div()
                            .flex_1()
                            .min_w_0()
                            .flex()
                            .flex_col()
                            .child(
                                line(
                                    name.clone(),
                                    text::SM,
                                    line_height(text::SM, 1.5),
                                    color::FG,
                                )
                                .ellipsis(),
                            )
                            .child(
                                line(
                                    format!("{reason} · {until}"),
                                    text::META,
                                    line_height(text::META, 1.5),
                                    color::FG_3,
                                )
                                .ellipsis(),
                            ),
                    );
                if access.loaded && access.ban_members {
                    row = row.child(
                        Button::new(
                            SharedString::from(format!("unban-{}", b.user_id.hex())),
                            t("admin.bans.unban"),
                        )
                        .on_click(move |_, _w, cx| {
                            let u = ui(cx);
                            u.error = None;
                            u.busy = false;
                            u.dialog = Some(ModDialog {
                                kind: ModKind::Unban,
                                community,
                                target: user,
                                name: name2.clone(),
                                server: server2.clone(),
                                request: request_id(),
                            });
                            cx.refresh_windows();
                        }),
                    );
                }
                root = root.child(row);
            }
        }
    }
    if let Some(d) = dialog_overlay(ctx, window, cx) {
        root = root.child(d);
    }
    root.into_any_element()
}

fn moderation_log_section(ctx: &SlotCx, community: CommunityId, cx: &mut App) -> AnyElement {
    let access = ctx.state.roles.access_of(community);
    let unavailable = ui(cx).log_unavailable;
    let mut root = div()
        .id("settings-moderation-log")
        .flex()
        .flex_col()
        .gap(space::S10)
        .child(section_head(t("admin.bot.moderationLog.title"), None))
        .child(
            div()
                .flex()
                .gap(space::S8)
                .child(
                    Button::new(
                        "modlog-automatic",
                        t("admin.bot.moderationLog.loadAutomatic"),
                    )
                    .on_click(|_, _w, cx| {
                        ui(cx).log_unavailable = true;
                        cx.refresh_windows();
                    }),
                )
                .child(
                    Button::new("modlog-manual", t("admin.bot.moderationLog.load")).on_click(
                        |_, _w, cx| {
                            ui(cx).log_unavailable = true;
                            cx.refresh_windows();
                        },
                    ),
                ),
        );
    if access.loaded && !access.view_audit_log {
        root = root.child(note(t("admin.bot.moderationLog.denied")));
    } else if unavailable {
        // The pages are sealed Bot reads (Bot v2 operations); this client sends them once Task 20's Bot operations land.
        root = root.child(note(t("admin.bot.moderationLog.responseUnavailable")));
    }
    root.into_any_element()
}

/// Draws one of this area's settings sections (`slots::settings_sections` points here).
pub fn settings_section(
    section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let Some(community) = ctx.community else {
        return div().into_any_element();
    };
    ui(cx).ctx = Some(ctx.clone());
    match section {
        SectionId::Server(ServerSection::Members) => div()
            .id("settings-members")
            .flex()
            .flex_col()
            .child(section_head(t("members.toggle"), None))
            .child(list_with_overlays(ctx, community, false, window, cx))
            .into_any_element(),
        SectionId::Server(ServerSection::Roles) => roles_section(ctx, community, cx),
        SectionId::Server(ServerSection::SelfRoles) => self_roles_section(ctx, community, cx),
        SectionId::Server(ServerSection::Bans) => bans_section(ctx, community, window, cx),
        SectionId::Server(ServerSection::ModerationLog) => {
            moderation_log_section(ctx, community, cx)
        }
        _ => div().into_any_element(),
    }
}

/// Whether `me` may act on `target` at all (the person menu's gated items), for tests and hosts: the client's rank rule.
pub fn may_moderate(state: &AppState, community: CommunityId, target: UserId) -> bool {
    let Some(me) = state.account.user_id else {
        return false;
    };
    let Some(owner) = state
        .communities
        .iter()
        .find(|c| c.id == community)
        .map(|c| c.owner)
    else {
        return false;
    };
    let access = state.roles.access_of(community);
    let roles = state.roles.roles_of(community);
    let target_roles = state
        .members
        .get(&community)
        .and_then(|m| m.iter().find(|m| m.user_id == target))
        .map(|m| m.role_ids.clone())
        .unwrap_or_default();
    access.loaded
        && rank::can_moderate(
            me.0,
            target.0,
            owner.0,
            access.top,
            rank::top_of(&target_roles, roles),
        )
}
