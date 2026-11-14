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
pub mod modlog;
mod self_roles;
pub mod roles;

use std::collections::HashSet;
use std::rc::Rc;

use dilla_client::Client;
use dilla_client::features::members::{
    GroupKind as Kind, MenuAction, member_card, member_groups, member_name,
};
use dilla_client::features::moderation::{BanLength, ReasonProblem, TimeoutLength, reason_problem};
use dilla_client::features::roles::perm::{PermKey, ROLE_PERM_GROUPS};
use dilla_client::features::roles::rank;
use dilla_client::features::roles::{
    AssignLock, AssignRefusal, PermLock, assign_lock, assign_refusal, assignable, held_bits,
    permission_lock, request_id,
};
use dilla_client::state::members::Presence as Wire;
use dilla_client::state::roles::{RoleId, RoleSummary};
use dilla_client::state::{AppState, CommunityId, UserId};
use dilla_client::sync::api::ApiError;
use gpui::{
    AnyElement, App, AppContext, Entity, InteractiveElement, IntoElement, ParentElement, Pixels,
    Point, SharedString, Styled, Window, WindowId, anchored, deferred, div, point,
};
use gpui_component::input::InputState;

use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem};
use dilla_desktop_ui::components::avatar::{Avatar, AvatarSize, Presence, colour_of};
use dilla_desktop_ui::components::button::Button;
use dilla_desktop_ui::components::check_list::{CheckList, CheckListItem};
use dilla_desktop_ui::components::confirm_dialog::ConfirmDialog;
use dilla_desktop_ui::components::dialog::Dialog;
use dilla_desktop_ui::components::members::{
    CardAction, GroupKind, ListGroup, ListMember, MemberList, ProfileCard, space,
};
use dilla_desktop_ui::components::select::Select;
use dilla_desktop_ui::components::text_field::TextField;
use dilla_desktop_ui::components::toast::show_toast_in;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, font, layout, text};

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
    /// The DeleteRole confirmation: the role and its name, and the request in flight.
    role_delete: Option<(CommunityId, RoleId, String)>,
    role_delete_busy: bool,
    role_delete_error: Option<String>,
    /// MemberRolesFlow: the member whose roles are being given or taken, by id.
    assign: Option<(CommunityId, UserId)>,
    assign_name: String,
    assign_busy: Option<RoleId>,
    assign_problem: Option<String>,
    /// What each community's role-bearing slices looked like when its access was last read (see [`sync`]).
    access_seen: std::collections::HashMap<CommunityId, u64>,
}

/// The members area's state of window `w` (final review sweep: per window, so one window's card, menu, dialogs, slot
/// context and its client never reach another window).
fn ui(w: WindowId, cx: &mut App) -> &mut MembersUi {
    if crate::per_window::get::<MembersUi>(w, cx).is_none() {
        // Shell.tsx: the pane starts open in the wide column (min-width 64rem, every desktop workspace window).
        crate::per_window::set(
            w,
            Some(MembersUi {
                open: true,
                ..MembersUi::default()
            }),
            cx,
        );
    }
    crate::per_window::get_mut::<MembersUi>(w, cx)
}

/// Lets go of `client` (Forget, sign-out: `crate::release::release_client`): the slot context it was handed and the
/// card, menu and dialog drawn for it go.
pub fn release(client: &Rc<Client>, cx: &mut App) {
    // The moderation log's decrypted pages this client read go with it, in every window (final review I4).
    modlog::release_reader(Rc::as_ptr(client) as usize, cx);
    crate::per_window::each_mut::<MembersUi>(cx, |_, u| {
        if u.ctx
            .as_ref()
            .is_some_and(|ctx| ctx.client.as_ref().is_some_and(|c| Rc::ptr_eq(c, client)))
        {
            u.ctx = None;
            u.card = None;
            u.menu = None;
            u.dialog = None;
            u.busy = false;
            u.assign = None;
            u.role_delete = None;
        }
    });
}

/// The host hands its slot context in before it draws the aside (the aside hook takes none of its own).
///
/// The web reads the access again on every community change (op 54: a role, a grant or an overwrite changed). The sync
/// engine consumes op 54 and reads the member list again; when a community's members, their roles or its owner change
/// here, its access is read again too.
pub fn sync(w: WindowId, ctx: &SlotCx, cx: &mut App) {
    let signatures: Vec<(CommunityId, u64)> = ctx
        .state
        .communities
        .iter()
        .map(|c| (c.id, role_signature(&ctx.state, c.id)))
        .collect();
    let u = ui(w, cx);
    u.ctx = Some(ctx.clone());
    for (community, sig) in signatures {
        let before = u.access_seen.insert(community, sig);
        if before.is_some_and(|b| b != sig) {
            u.asked.remove(&("people", community));
        }
    }
}

/// A digest of what decides `community`'s access: its owner, and each member's roles.
fn role_signature(state: &AppState, community: CommunityId) -> u64 {
    use std::hash::{Hash, Hasher};
    let mut h = std::collections::hash_map::DefaultHasher::new();
    state
        .communities
        .iter()
        .find(|c| c.id == community)
        .map(|c| c.owner)
        .hash(&mut h);
    if let Some(members) = state.members.get(&community) {
        for m in members {
            m.user_id.hash(&mut h);
            m.role_ids.hash(&mut h);
        }
    }
    h.finish()
}

/// Whether the aside is open.
pub fn is_open(w: WindowId, cx: &App) -> bool {
    crate::per_window::get::<MembersUi>(w, cx).is_none_or(|u| u.open)
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
/// Reads `community`'s presence and this account's access there once per window (the command picker lists the
/// permission-gated built-ins, `/kick` and `/ban`, only once the access is known).
pub fn ensure_access(
    w: WindowId,
    community: CommunityId,
    client: Option<&Rc<Client>>,
    cx: &mut App,
) {
    ensure_loaded(w, "people", community, client, cx);
}

fn ensure_loaded(
    w: WindowId,
    what: &'static str,
    community: CommunityId,
    client: Option<&Rc<Client>>,
    cx: &mut App,
) {
    let Some(client) = client else { return };
    if !ui(w, cx).asked.insert((what, community)) {
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
                ui(w, cx).asked.remove(&(what, community));
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

/// The automation registry keys on `&'static str`: each row id is leaked once and lives as long as the process (as
/// `channel_list::row_probe` does).
#[cfg(feature = "automation")]
fn probe_id(id: &str) -> &'static str {
    use std::collections::HashMap;
    use std::sync::Mutex;
    // App-wide by design (final review sweep): interned automation ids (element names, never a client handle).
    static IDS: Mutex<Option<HashMap<String, &'static str>>> = Mutex::new(None);
    let mut ids = IDS
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    ids.get_or_insert_with(HashMap::new)
        .entry(id.to_owned())
        .or_insert_with(|| Box::leak(id.to_owned().into_boxed_str()))
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
    let w = window.window_handle().window_id();
    ensure_loaded(w, "people", community, ctx.client.as_ref(), cx);
    let state = ctx.state.clone();
    let groups = groups(&state, community);
    let loading = groups.is_none() || !state.people.presence.contains_key(&community);
    let open_id = ui(w, cx).card.map(|(u, _)| SharedString::from(u.hex()));
    let mut list = MemberList::new("members-list", groups.unwrap_or_default())
        .loading(loading, t("members.loading"))
        .empty_label(t("members.empty"))
        .bot_label(t("layout.profile.bot"))
        .open_id(open_id)
        .on_open(move |id, window, cx| {
            let Some(user) = user_of(id) else { return };
            let at = window.mouse_position();
            let u = ui(w, cx);
            u.menu = None;
            u.card = if u.card.is_some_and(|(c, _)| c == user) {
                None
            } else {
                Some((user, at))
            };
            cx.refresh_windows();
        })
        .on_menu(move |id, at, _window, cx| {
            let Some(user) = user_of(id) else { return };
            let u = ui(w, cx);
            u.card = None;
            u.menu = Some((user, at));
            cx.refresh_windows();
        });
    #[cfg(feature = "automation")]
    {
        list = list.probe(Rc::new(|id: &str| {
            crate::automation::bounds_probe(probe_id(id)).into_any_element()
        }));
    }
    if head {
        list = list
            .head(t("members.toggle"), Some(t("members.close").into()))
            .on_close(move |_, cx| {
                ui(w, cx).open = false;
                cx.refresh_windows();
            });
    } else {
        // Without its head the list is Server settings -> Members, inside a settings section.
        list = list.in_settings_section();
    }
    let mut root = div().flex().flex_col().size_full().child(list);
    if let Some(card) = card_overlay(ctx, community, window, cx) {
        root = root.child(card);
    }
    if let Some(menu) = menu_overlay(w, ctx, community, cx) {
        root = root.child(menu);
    }
    if let Some(dialog) = dialog_overlay(ctx, window, cx) {
        root = root.child(dialog);
    }
    if let Some(dialog) = assign_dialog(w, ctx, cx) {
        root = root.child(dialog);
    }
    root.into_any_element()
}

/// MemberCardHost.tsx `CARD`: the card's own buttons are message, compare and edit roles; moderation lives only in the
/// person menu (`MOD`), where every item is danger and a rule comes before the first.
fn card_icon(a: MenuAction) -> Option<IconName> {
    match a {
        MenuAction::Message => Some(IconName::MessageSquare),
        MenuAction::Compare => Some(IconName::Shield),
        MenuAction::Roles => Some(IconName::SquarePen),
        _ => None,
    }
}

/// MemberCardHost.tsx `MOD`.
fn moderation_icon(a: MenuAction) -> Option<IconName> {
    match a {
        MenuAction::Warn => Some(IconName::TriangleAlert),
        MenuAction::Timeout => Some(IconName::AlarmClock),
        MenuAction::Kick => Some(IconName::LogOut),
        MenuAction::Ban => Some(IconName::Lock),
        _ => None,
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
        MenuAction::Roles => t("layout.profile.manageRoles").to_owned(),
        MenuAction::Warn => t("mod.warn.menu").to_owned(),
        MenuAction::Timeout => t("mod.timeout.menu").to_owned(),
        MenuAction::Kick => tf("members.menu.kickName", &[("name", name)]),
        MenuAction::Ban => tf("members.menu.banName", &[("name", name)]),
    }
}

/// Runs a chosen action on `user` (the card's buttons and the person menu's items).
fn choose(w: WindowId, community: CommunityId, user: UserId, id: &str, cx: &mut App) {
    let Some(ctx) = ui(w, cx).ctx.clone() else {
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
        "roles" => {
            open_assign(w, community, user, member_name(member).to_owned(), cx);
            return;
        }
        // Message and compare hand over to the DM and trust views (Tasks 13 and 28 wire them).
        _ => return,
    };
    let u = ui(w, cx);
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
    let w = window.window_handle().window_id();
    let (user, at) = ui(w, cx).card?;
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
        .filter_map(|a| {
            Some(CardAction {
                id: action_id(*a).into(),
                label: action_label(*a, &card.name).into(),
                icon: card_icon(*a)?,
                danger: false,
            })
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
    .on_action(move |id, _w, cx| choose(w, community, user, id, cx));
    if !card.is_bot {
        view = view.client(t("layout.profile.web"));
    }
    if card.actions.contains(&MenuAction::Message) {
        view = view.message(t("members.menu.message"));
    }
    // ProfileCard.tsx (the prototype's rule): right of the pointer by 14px, else left of it; 24px above it; 8px from the
    // window's edges.
    let viewport = window.viewport_size();
    let mut x = at.x + space::POINTER_GAP;
    if x + space::CARD_OUTER_W > viewport.width - space::CARD_EDGE {
        x = at.x - space::CARD_OUTER_W - space::POINTER_GAP;
    }
    let x = x.max(space::CARD_EDGE);
    let y = (at.y - space::POINTER_RISE).max(space::CARD_EDGE);
    // The bottom clamp uses the card's laid-out height, as the web's `box.height - h - GAP`: anchored snaps it inside
    // the window, 8px from its edges.
    Some(
        deferred(
            anchored()
                .position(point(x, y))
                .snap_to_window_with_margin(space::CARD_EDGE)
                .child(view),
        )
        .with_priority(1)
        .into_any_element(),
    )
}

fn menu_overlay(
    w: WindowId,
    ctx: &SlotCx,
    community: CommunityId,
    cx: &mut App,
) -> Option<AnyElement> {
    let (user, at) = ui(w, cx).menu?;
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
    // MemberCardHost.tsx: Profile, Message, Mention (where a composer is open), Copy username, Edit roles, then the
    // moderation the viewer may use, after a rule.
    let mut items = vec![MenuItem::new("profile", t("members.menu.profile")).icon(IconName::User)];
    if card.actions.contains(&MenuAction::Message) {
        items.push(
            MenuItem::new("message", t("members.menu.message")).icon(IconName::MessageSquare),
        );
    }
    if ctx.channel.is_some() && !card.is_you {
        items.push(MenuItem::new("mention", t("members.menu.mention")).icon(IconName::AtSign));
    }
    items.push(MenuItem::new("copy", t("members.menu.copy")).icon(IconName::Copy));
    if card.actions.contains(&MenuAction::Roles) {
        items.push(MenuItem::new("roles", t("members.menu.roles")).icon(IconName::Shield));
    }
    for (i, a) in card
        .actions
        .iter()
        .filter(|a| moderation_icon(**a).is_some())
        .enumerate()
    {
        let mut item = MenuItem::new(action_id(*a), action_label(*a, &card.name))
            .icon(moderation_icon(*a).unwrap_or(IconName::Shield))
            .danger();
        if i == 0 {
            item = item.sep();
        }
        items.push(item);
    }
    let username = card.username.clone();
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
        // `.d-person-menu__who`: the name (12.5px 700) over where they are (`@username`, 10.5px `--fg-3`), at 1.3.
        .child({
            let mut who = div().flex().flex_col().min_w_0().child(
                line(
                    card.name.clone(),
                    text::SM,
                    line_height(text::SM, 1.3),
                    color::FG,
                )
                .weight(font::WEIGHT_DISPLAY)
                .ellipsis(),
            );
            if !card.username.is_empty() {
                who = who.child(line(
                    format!("@{}", card.username),
                    text::META,
                    line_height(text::META, 1.3),
                    color::FG_3,
                ));
            }
            who
        });
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
        .on_open_change(move |open, _w, cx| {
            if !open {
                ui(w, cx).menu = None;
                cx.refresh_windows();
            }
        })
        .on_select(move |id, window, cx| {
            if id == "profile" {
                let at = window.mouse_position();
                let u = ui(w, cx);
                u.menu = None;
                u.card = Some((user, at));
                cx.refresh_windows();
            } else if id == "copy" {
                cx.write_to_clipboard(gpui::ClipboardItem::new_string(format!("@{username}")));
                ui(w, cx).menu = None;
                show_toast_in(
                    Some(w),
                    tf("members.menu.copied", &[("name", &username)]),
                    cx,
                );
            } else {
                choose(w, community, user, id, cx);
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
fn confirm(w: WindowId, cx: &mut App) {
    let u = ui(w, cx);
    let Some(d) = u.dialog.clone() else { return };
    if u.busy {
        return;
    }
    let input = u.reason.clone();
    let reason = input
        .map(|r| r.read(cx).value().to_string())
        .unwrap_or_default();
    let u = ui(w, cx);
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
            let u = ui(w, cx);
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
                    show_toast_in(Some(w), done, cx);
                }
                Err(e) => u.error = Some(refusal_text(d.kind, &e, &d.name, &d.server)),
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

fn dialog_overlay(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let w = window.window_handle().window_id();
    let _ = ctx;
    let d = ui(w, cx).dialog.clone()?;
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
    if matches!(d.kind, ModKind::Ban | ModKind::Warn) && ui(w, cx).reason.is_none() {
        let input = cx.new(|cx| InputState::new(window, cx));
        ui(w, cx).reason = Some(input);
    }
    let u = ui(w, cx);
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
                        .on_change(move |v, _w, cx| {
                            ui(w, cx).ban_length =
                                BanLength::ALL.into_iter().find(|l| l.key() == v);
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
                .on_change(move |v, _w, cx| {
                    ui(w, cx).timeout_length =
                        TimeoutLength::ALL.into_iter().find(|l| l.key() == v);
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
    .on_confirm(move |_w, cx| confirm(w, cx))
    .on_close(move |_w, cx| {
        let u = ui(w, cx);
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
    let w = window.window_handle().window_id();
    let Route::Channel { community, .. } = route else {
        return None;
    };
    let community = *community;
    let u = ui(w, cx);
    if !u.open {
        return None;
    }
    let ctx = u.ctx.clone()?;
    Some(
        div()
            .id("members-pane")
            // The shell's aside column is the pane's width and draws its border-left (`.d-app-shell__aside`).
            .size_full()
            .bg(color::BG_2)
            .key_context("MembersPane")
            .on_action(move |_: &keys::CloseCard, _w, cx| {
                let u = ui(w, cx);
                if u.card.take().is_some() || u.menu.take().is_some() {
                    cx.refresh_windows();
                }
            })
            .child(list_with_overlays(&ctx, community, true, window, cx))
            .into_any_element(),
    )
}

/// Opens or closes this area's aside. The channel header's button (Task 8) calls it. Fixed signature.
pub fn toggle_pane(w: WindowId, cx: &mut App) {
    let u = ui(w, cx);
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

/// Runs a roles command and says a refusal by its code, in window `w`.
fn roles_job(
    w: WindowId,
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
            cx.update(|cx| {
                show_toast_in(
                    Some(w),
                    tf("roles.assign.error.other", &[("code", &e.code)]),
                    cx,
                )
            });
        }
    })
    .detach();
}

fn bans_section(
    ctx: &SlotCx,
    community: CommunityId,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let w = window.window_handle().window_id();
    ensure_loaded(w, "people", community, ctx.client.as_ref(), cx);
    ensure_loaded(w, "bans", community, ctx.client.as_ref(), cx);
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
                            let u = ui(w, cx);
                            u.error = None;
                            u.busy = false;
                            u.reason = None;
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

/// Reads one page of the log (ModerationLog.tsx `load`): a sealed Bot read through Task 21's
/// `BotAdmin::moderation_page`, on the client of the section that asked (`ctx`, its own window's: final review I4); its
/// answer lands in that window's [`modlog::Log`] unless a later load or another community came first.
fn load_log(
    window: gpui::WindowId,
    ctx: &SlotCx,
    source: modlog::Source,
    after: Option<String>,
    cx: &mut App,
) {
    let (Some(client), Some(community)) = (ctx.client.clone(), ctx.community) else {
        return;
    };
    modlog::set_reader(window, Rc::as_ptr(&client) as usize, cx);
    let Some((generation, after)) = modlog::with_log(window, cx, |log| {
        log.show(community);
        log.begin(source, after)
    }) else {
        return;
    };
    let rx = client
        .bot_admin()
        .moderation_page(community, source.operation(), after);
    cx.refresh_windows();
    cx.spawn(async move |cx| {
        let answer = rx
            .await
            .unwrap_or_else(|_| Err(dilla_client::ClientError::Sync("E_DROPPED".into())));
        cx.update(|cx| {
            modlog::with_log(window, cx, |log| log.land(generation, answer));
            cx.refresh_windows();
        });
    })
    .detach();
}

/// ModerationLog.tsx's date: the stamp, or "Date unavailable" for one that is no unix second count.
fn log_date(seconds: &str) -> String {
    match seconds.parse::<u64>() {
        Ok(ts) if ts <= 8_640_000_000 => crate::format::day_time(ts),
        _ => t("admin.bot.moderationLog.unknownDate").to_owned(),
    }
}

fn moderation_log_section(
    ctx: &SlotCx,
    community: CommunityId,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let wid = window.window_handle().window_id();
    let access = ctx.state.roles.access_of(community);
    modlog::with_log(wid, cx, |log| log.show(community));
    let log = modlog::log_of(wid, cx);
    let sending = log.phase == modlog::Phase::Sending;
    let label = |source: modlog::Source, idle: &'static str| {
        if sending && log.source == source {
            t("admin.bot.moderationLog.loading")
        } else {
            t(idle)
        }
    };
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
                        label(
                            modlog::Source::Automatic,
                            "admin.bot.moderationLog.loadAutomatic",
                        ),
                    )
                    .disabled(sending)
                    .on_click({
                        let ctx = ctx.clone();
                        move |_, _w, cx| load_log(wid, &ctx, modlog::Source::Automatic, None, cx)
                    }),
                )
                .child(
                    Button::new(
                        "modlog-manual",
                        label(modlog::Source::Manual, "admin.bot.moderationLog.load"),
                    )
                    .disabled(sending)
                    .on_click({
                        let ctx = ctx.clone();
                        move |_, _w, cx| load_log(wid, &ctx, modlog::Source::Manual, None, cx)
                    }),
                ),
        );
    if access.loaded && !access.view_audit_log && log.phase == modlog::Phase::Idle {
        root = root.child(note(t("admin.bot.moderationLog.denied")));
    }
    match log.phase {
        modlog::Phase::Denied => root = root.child(note(t("admin.bot.moderationLog.denied"))),
        modlog::Phase::Error => root = root.child(note(t("admin.bot.moderationLog.error"))),
        modlog::Phase::Unavailable => {
            root = root.child(note(t("admin.bot.moderationLog.responseUnavailable")));
        }
        _ => {}
    }
    if log.empty() {
        root = root.child(note(t("admin.bot.moderationLog.empty")));
    }
    for row in &log.cases {
        let meta = crate::strings::tf(
            "admin.bot.moderationLog.caseMeta",
            &[
                ("action", row.action.as_str()),
                ("reason", row.reason.as_str()),
                ("date", log_date(&row.received_at).as_str()),
            ],
        );
        root = root.child(
            div()
                .flex()
                .flex_col()
                .child(note(meta))
                .child(note(row.excerpt.clone())),
        );
    }
    if let Some(cursor) = log.more() {
        let cursor = cursor.to_owned();
        root = root.child(
            Button::new("modlog-more", t("admin.bot.moderationLog.more")).on_click({
                let ctx = ctx.clone();
                move |_, _w, cx| {
                    load_log(wid, &ctx, modlog::Source::Manual, Some(cursor.clone()), cx)
                }
            }),
        );
    }
    if log.phase == modlog::Phase::Ready && log.automatic_more {
        root = root.child(note(t("admin.bot.moderationLog.automaticMore")));
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
    let w = window.window_handle().window_id();
    let Some(community) = ctx.community else {
        return div().into_any_element();
    };
    ui(w, cx).ctx = Some(ctx.clone());
    match section {
        SectionId::Server(ServerSection::Members) => div()
            .id("settings-members")
            .flex()
            .flex_col()
            // MembersSection.tsx: the section's h2 (20px at 1.5) with the 22px the frame keeps under it.
            .child(
                dilla_desktop_ui::components::settings::frame::section_header(
                    t("members.toggle"),
                    None,
                ),
            )
            .child(list_with_overlays(ctx, community, false, window, cx))
            .into_any_element(),
        SectionId::Server(ServerSection::Roles) => roles::section(w, ctx, community, window, cx),
        SectionId::Server(ServerSection::SelfRoles) => {
            self_roles::section(w, ctx, community, window, cx)
        }
        SectionId::Server(ServerSection::Bans) => bans_section(ctx, community, window, cx),
        SectionId::Server(ServerSection::ModerationLog) => {
            moderation_log_section(ctx, community, window, cx)
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

/// RolesSection.tsx `mine`: the bits I hold in `community` (everything for the owner and administrators).
pub fn my_bits(state: &AppState, community: CommunityId) -> u32 {
    let access = state.roles.access_of(community);
    let (Some(me), Some(server)) = (
        state.account.user_id,
        state.communities.iter().find(|c| c.id == community),
    ) else {
        return 0;
    };
    let held = state
        .members
        .get(&community)
        .and_then(|m| m.iter().find(|m| m.user_id == me))
        .map(|m| m.role_ids.clone())
        .unwrap_or_default();
    held_bits(
        access.owner,
        server.owner.0,
        access.everyone_id.unwrap_or_default(),
        state.roles.roles_of(community),
        &held,
        me.0,
    )
}

/// Whether I may edit `role` at all: roles are mine to manage and it sits below my rank (or I own the server).
fn role_editable(state: &AppState, community: CommunityId, role: &RoleSummary) -> bool {
    let access = state.roles.access_of(community);
    access.loaded && access.manage_roles && (access.owner || role.position < access.top)
}

/// The role editor's permission rows of `role`, in ROLE_PERM_GROUPS order, each with its lock (`None`: may toggle).
pub fn permission_locks(
    state: &AppState,
    community: CommunityId,
    role: &RoleSummary,
) -> Vec<(PermKey, Option<PermLock>)> {
    let editable = role_editable(state, community, role);
    let mine = my_bits(state, community);
    ROLE_PERM_GROUPS
        .iter()
        .flat_map(|(_, keys)| keys.iter())
        .map(|k| (*k, permission_lock(editable, mine, k.bit())))
        .collect()
}

/// MemberRolesFlow.tsx's list for `user`: each assignable role, whether the member holds it, and its lock; `None` once
/// the member has left (the dialog closes and says so).
pub fn assign_items(
    state: &AppState,
    community: CommunityId,
    user: UserId,
) -> Option<Vec<(RoleSummary, bool, Option<AssignLock>)>> {
    let target = state
        .members
        .get(&community)?
        .iter()
        .find(|m| m.user_id == user)?;
    let access = state.roles.access_of(community);
    let mine = my_bits(state, community);
    Some(
        assignable(state.roles.roles_of(community), access.everyone_id)
            .into_iter()
            .map(|r| {
                let held = target.role_ids.contains(&r.id);
                let lock = assign_lock(&r, &access, mine);
                (r, held, lock)
            })
            .collect(),
    )
}

/// Opens the DeleteRole confirmation for `role` (nothing is sent until it is confirmed).
pub fn request_role_delete(
    w: WindowId,
    community: CommunityId,
    role: RoleId,
    name: String,
    cx: &mut App,
) {
    let u = ui(w, cx);
    u.role_delete = Some((community, role, name));
    u.role_delete_busy = false;
    u.role_delete_error = None;
    cx.refresh_windows();
}

/// The role whose deletion awaits its confirmation.
pub fn pending_role_delete(w: WindowId, cx: &App) -> Option<(CommunityId, RoleId)> {
    crate::per_window::get::<MembersUi>(w, cx)
        .and_then(|u| u.role_delete.as_ref().map(|(c, r, _)| (*c, *r)))
}

/// Closes the DeleteRole confirmation (not while its request is in flight).
pub fn cancel_role_delete(w: WindowId, cx: &mut App) {
    let u = ui(w, cx);
    if !u.role_delete_busy {
        u.role_delete = None;
        u.role_delete_error = None;
        cx.refresh_windows();
    }
}

/// Sends the confirmed deletion; the refusal and the end of busy land together.
fn confirm_role_delete(w: WindowId, cx: &mut App) {
    let u = ui(w, cx);
    let Some((community, role, name)) = u.role_delete.clone() else {
        return;
    };
    let Some(client) = u.ctx.as_ref().and_then(|c| c.client.clone()) else {
        return;
    };
    if u.role_delete_busy {
        return;
    }
    u.role_delete_busy = true;
    u.role_delete_error = None;
    cx.refresh_windows();
    let job = client
        .roles()
        .run(move |r| async move { r.delete_role(community, role).await });
    cx.spawn(async move |cx| {
        let answer = job.await;
        cx.update(|cx| {
            let u = ui(w, cx);
            if u.role_delete.as_ref().is_none_or(|(_, r, _)| *r != role) {
                return;
            }
            u.role_delete_busy = false;
            match answer {
                Ok(()) => {
                    u.role_delete = None;
                    if u.role == Some(role) {
                        u.role = None;
                    }
                    show_toast_in(Some(w), tf("roles.deleted", &[("name", &name)]), cx);
                }
                Err(e) => {
                    u.role_delete_error = Some(tf(
                        "roles.error.delete",
                        &[("name", &name), ("code", &e.code)],
                    ));
                }
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

/// The DeleteRole confirmation, while one is open.
fn role_delete_dialog(w: WindowId, cx: &mut App) -> Option<AnyElement> {
    let u = ui(w, cx);
    let (_, _, name) = u.role_delete.clone()?;
    Some(
        ConfirmDialog::new(
            "role-delete-dialog",
            tf("roles.deleteTitle", &[("name", &name)]),
            t("roles.deleteBody"),
            t("roles.deleteConfirm"),
            t("roles.deleteBusy"),
            t("roles.editor.cancel"),
        )
        .busy(u.role_delete_busy)
        .error(u.role_delete_error.clone().map(SharedString::from))
        .on_confirm(move |_w, cx| confirm_role_delete(w, cx))
        .on_close(move |_w, cx| cancel_role_delete(w, cx))
        .into_any_element(),
    )
}

/// Opens MemberRolesFlow for `user`.
fn open_assign(w: WindowId, community: CommunityId, user: UserId, name: String, cx: &mut App) {
    let u = ui(w, cx);
    u.card = None;
    u.menu = None;
    u.assign = Some((community, user));
    u.assign_name = name;
    u.assign_busy = None;
    u.assign_problem = None;
    cx.refresh_windows();
}

/// One tick of the assign dialog: a grant or a revoke, one at a time, each said when it lands or is refused.
fn assign_toggle(w: WindowId, role: RoleId, on: bool, cx: &mut App) {
    let u = ui(w, cx);
    let Some((community, user)) = u.assign else {
        return;
    };
    if u.assign_busy.is_some() {
        return;
    }
    let Some(ctx) = u.ctx.clone() else { return };
    let Some(client) = ctx.client.clone() else {
        return;
    };
    let Some(items) = assign_items(&ctx.state, community, user) else {
        return;
    };
    // The gate the dialog draws is checked again here: a locked role is never sent.
    let Some((r, _, None)) = items.into_iter().find(|(r, _, _)| r.id == role) else {
        return;
    };
    let u = ui(w, cx);
    let name = u.assign_name.clone();
    u.assign_busy = Some(role);
    u.assign_problem = None;
    cx.refresh_windows();
    let job = client.roles().run(move |h| async move {
        if on {
            h.grant_role(community, user, role).await
        } else {
            h.revoke_role(community, user, role).await
        }
    });
    cx.spawn(async move |cx| {
        let answer = job.await;
        cx.update(|cx| {
            if ui(w, cx).assign != Some((community, user)) {
                return;
            }
            ui(w, cx).assign_busy = None;
            match answer {
                Ok(()) => {
                    let key = if on {
                        "roles.assign.given"
                    } else {
                        "roles.assign.taken"
                    };
                    show_toast_in(Some(w), tf(key, &[("role", &r.name), ("name", &name)]), cx);
                }
                Err(e) => {
                    let listed = ui(w, cx).ctx.as_ref().is_some_and(|c| {
                        c.state
                            .roles
                            .roles_of(community)
                            .iter()
                            .any(|x| x.id == role)
                    });
                    let message = match assign_refusal(&e, listed) {
                        AssignRefusal::TwoFactor => {
                            tf("roles.assign.error.twoFactor", &[("name", &name)])
                        }
                        AssignRefusal::Forbidden => {
                            tf("roles.assign.error.forbidden", &[("role", &r.name)])
                        }
                        AssignRefusal::RoleGone => t("roles.error.gone").to_owned(),
                        AssignRefusal::MemberGone => {
                            tf("roles.assign.error.gone", &[("name", &name)])
                        }
                        AssignRefusal::Other(code) => {
                            tf("roles.assign.error.other", &[("code", &code)])
                        }
                    };
                    ui(w, cx).assign_problem = Some(message);
                }
            }
            cx.refresh_windows();
        });
    })
    .detach();
}

fn close_assign(w: WindowId, cx: &mut App) {
    let u = ui(w, cx);
    u.assign = None;
    u.assign_busy = None;
    u.assign_problem = None;
    cx.refresh_windows();
}

/// MemberRolesFlow: a dialog over a check list keyed by role id; it closes, and says so, when the member leaves.
fn assign_dialog(w: WindowId, ctx: &SlotCx, cx: &mut App) -> Option<AnyElement> {
    let (community, user) = ui(w, cx).assign?;
    let name = ui(w, cx).assign_name.clone();
    let Some(items) = assign_items(&ctx.state, community, user) else {
        if ctx.state.members.contains_key(&community) {
            close_assign(w, cx);
            show_toast_in(
                Some(w),
                tf("roles.assign.error.gone", &[("name", &name)]),
                cx,
            );
        }
        return None;
    };
    let u = ui(w, cx);
    let busy = u.assign_busy.map(|r| SharedString::from(hex(&r)));
    let problem = u.assign_problem.clone();
    let list = items
        .into_iter()
        .map(|(r, held, lock)| {
            let item = CheckListItem::new(hex(&r.id), r.name.clone(), held)
                .color(r.color)
                .disabled(lock.is_some());
            match lock {
                Some(AssignLock::Rank) => {
                    item.detail(tf("roles.assign.locked", &[("role", &r.name)]))
                }
                Some(AssignLock::NotHeld) => item.detail(t("roles.editor.notHeld")),
                None => item,
            }
        })
        .collect();
    let mut body = div()
        .flex()
        .flex_col()
        .gap(space::S10)
        .child(note(tf("roles.assign.body", &[("name", &name)])))
        .child(note(t("roles.access")))
        .child(
            CheckList::new(
                "assign-roles",
                tf("roles.assign.title", &[("name", &name)]),
                list,
                t("roles.assign.none"),
            )
            .busy(busy)
            .on_toggle(move |id, on, _w, cx| {
                if let Some(role) = user_of(id) {
                    assign_toggle(w, role.0, on, cx);
                }
            }),
        );
    if let Some(problem) = problem {
        body = body.child(line(
            problem,
            text::UI,
            line_height(text::UI, 1.5),
            color::DANGER,
        ));
    }
    Some(
        Dialog::new(
            "assign-dialog",
            tf("roles.assign.title", &[("name", &name)]),
            t("roles.assign.close"),
        )
        .body(body)
        .on_close(move |_w, cx| close_assign(w, cx))
        .into_any_element(),
    )
}
