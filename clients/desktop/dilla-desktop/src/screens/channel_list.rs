//! The sidebar's channel list (packages/ui ChannelList, Signal §5.3): the server's name in the serif italic, the online and
//! voice counts, then the channels in position order, a category's channels under its head (a head folds them), the AFK
//! channel last. The rows are Task 6's `Row`. One tab stop with roving focus over the rows and the category heads; a row
//! has a context menu (right click, the Menu key, Shift+F10).

use std::collections::HashSet;
use std::rc::Rc;

use dilla_client::features::notify::{self, SettingsSource};
use dilla_client::state::{AppState, Channel, ChannelId, CommunityId};
use gpui::{
    App, ClickEvent, InteractiveElement, IntoElement, MouseButton, MouseDownEvent, ParentElement,
    Pixels, Point, SharedString, StatefulInteractiveElement, Styled, Window, div,
};

use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem};
use dilla_desktop_ui::components::{Row, RowKind};
use dilla_desktop_ui::metrics::focus_ring;
use dilla_desktop_ui::roving::{Roving, focus_handles, opens_context_menu};
use dilla_desktop_ui::text::{Face, line, line_height};
use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::screens::{LABEL_TRACKING_WIDE, centred};
use crate::strings::{t, tf};

/// ChannelList.css and ChannelHead: the lengths of the list's own boxes.
pub(crate) mod css {
    /// `.d-channel-list__head`: padding 14px 12px 8px 14px, gap 8px.
    pub const HEAD_TOP: f32 = 14.0;
    pub const HEAD_BOTTOM: f32 = 8.0;
    pub const HEAD_LEFT: f32 = 14.0;
    pub const HEAD_RIGHT: f32 = 12.0;
    pub const HEAD_GAP: f32 = 8.0;
    /// The head's "+" and the server menu's chevron: 28 and 26px hit areas around a 15px glyph.
    pub const ADD_HIT: f32 = 28.0;
    pub const MENU_HIT: f32 = 26.0;
    pub const GLYPH: f32 = 15.0;
    /// `.d-channel-list__stats`: padding-left 14px, bottom 10px, gap 10px (5px inside a stat), a 6px live dot.
    pub const STATS_LEFT: f32 = 14.0;
    pub const STATS_BOTTOM: f32 = 10.0;
    pub const STATS_GAP: f32 = 10.0;
    pub const STAT_GAP: f32 = 5.0;
    pub const LIVE_DOT: f32 = 6.0;
    /// `.d-channel-list__list`: padding 4px 8px 0.
    pub const LIST_TOP: f32 = 4.0;
    pub const LIST_SIDE: f32 = 8.0;
    /// `.d-channel-list__cat-head`: margin-top 10px, padding 0 8px, gap 6px, an 11px chevron.
    pub const CAT_TOP: f32 = 10.0;
    pub const CAT_SIDE: f32 = 8.0;
    pub const CAT_GAP: f32 = 6.0;
    pub const CHEVRON: f32 = 11.0;
    pub const CAT_RULE_MIN: f32 = 8.0;
    /// A hairline is 1px.
    pub const HAIRLINE_W: f32 = 1.0;
    /// A voice channel's item is 30px: its 28px button 1px in (VoiceChannelRow.css).
    pub const VOICE_PAD: f32 = 1.0;
    /// `.d-voice-row__join`: the join icon over the row's slot, 0.5rem from the right edge.
    pub const JOIN_RIGHT: f32 = 8.0;
}

/// What the sidebar shows.
pub struct ListModel<'a> {
    pub state: &'a AppState,
    pub community: Option<CommunityId>,
    pub selected: Option<ChannelId>,
    /// People online (the slice has no presence: this device's own account while connected).
    pub online: usize,
    /// People in the server's voice channels (the slice's state carries no voice occupancy: 0).
    pub in_voice: usize,
    /// The head's "+" (a new channel): for someone who can manage channels, in a server without categories (Shell.tsx;
    /// the slice knows no roles: the owner).
    pub can_add: bool,
    /// The "+" on a category's head (shown while the head is hovered or focused): for the same people, in a server that
    /// has categories.
    pub can_add_in_category: bool,
    /// The categories this device folded (hex ids, `layout.collapsedCategories`).
    pub collapsed: &'a HashSet<String>,
    /// The notify settings (Task 22): a muted channel or server (`notify.mute.*`, the account setting the web keeps) hides
    /// a row's unread count, its mentions still show (`notify::row_counts`).
    pub settings: &'a dyn SettingsSource,
    /// Whether the head's server menu (the ⌄) is open: the shell's, as the web keeps `headMenu`.
    pub server_menu_open: bool,
}

/// What a click on a channel row does.
pub type OnSelect = Rc<dyn Fn(ChannelId, &mut Window, &mut App)>;
/// A category head was pressed (fold or unfold).
pub type OnToggle = Rc<dyn Fn(ChannelId, &mut Window, &mut App)>;
/// A "+": a new channel, in the category when one is given.
pub type OnAdd = Rc<dyn Fn(Option<ChannelId>, &mut Window, &mut App)>;
/// The head's server menu chose "Server settings".
pub type OnServerSettings = Rc<dyn Fn(&mut Window, &mut App)>;
/// The head's server menu opens (`true`) or closes.
pub type OnServerMenuOpen = Rc<dyn Fn(bool, &mut Window, &mut App)>;
/// The head's server menu chose an item other than the settings: [`SERVER_MUTE`] or a [`SERVER_MODE`]`<mode>` id.
pub type OnServerMenuPick = Rc<dyn Fn(&str, &mut Window, &mut App)>;
/// A row's context menu was asked for, at this point.
pub type OnMenu = Rc<dyn Fn(ChannelId, Point<Pixels>, &mut Window, &mut App)>;

/// What the list does with its rows' events.
#[derive(Clone)]
pub struct ListHandlers {
    pub on_select: OnSelect,
    pub on_toggle: OnToggle,
    pub on_add: OnAdd,
    pub on_menu: OnMenu,
    /// The head's server menu `settings`: the server's settings (Shell.tsx; Task 15's host).
    pub on_server_settings: OnServerSettings,
    pub on_server_menu_open: OnServerMenuOpen,
    /// The head's server menu's Mute server and notify modes.
    pub on_server_menu_pick: OnServerMenuPick,
}

/// The server menu's item ids (Shell.tsx `serverMenu`): `settings`, `mute`, then `mode:<value>` per notify mode.
pub const SERVER_SETTINGS: &str = "settings";
pub const SERVER_MUTE: &str = "mute";
pub const SERVER_MODE: &str = "mode:";

/// Shell.tsx `NOTIFY_MODES`: a server's own notify mode, `default` deletes the stored value.
const NOTIFY_MODES: [(&str, &str); 4] = [
    ("default", "signalSettings.menu.notifyDefault"),
    ("all", "signalSettings.menu.notifyAll"),
    ("mentions", "signalSettings.menu.notifyMentions"),
    ("nothing", "signalSettings.menu.notifyNothing"),
];

/// The head's server menu (Shell.tsx `serverMenu`): Server settings, Mute server (a check that keeps the menu open), then
/// the server's notify mode (one checked), read from the same stored keys Settings writes.
pub fn server_menu_items(settings: &dyn SettingsSource, community: CommunityId) -> Vec<MenuItem> {
    let muted = settings
        .get(&notify::key_community_mute(community))
        .is_some_and(|v| v == "1");
    let mode = settings
        .get(&notify::key_community_mode(community))
        .unwrap_or_else(|| "default".to_owned());
    let mut items = vec![
        MenuItem::new(SERVER_SETTINGS, t("signalSettings.menu.serverSettings")),
        MenuItem::new(SERVER_MUTE, t("signalSettings.menu.muteServer")).check(muted),
    ];
    items.extend(NOTIFY_MODES.iter().map(|(value, key)| {
        MenuItem::new(format!("{SERVER_MODE}{value}"), t(key)).checked(mode == *value)
    }));
    items
}

/// Whether `c` is the server's AFK channel (the built-in voice channel; the slice's state does not mark it).
pub fn is_afk(c: &Channel) -> bool {
    c.kind == 1 && c.name == "AFK"
}

/// The channels of `community` in the web's order (shell-model.ts `orderChannels`): the rows outside categories by
/// position (then id), then each category (by position) with its rows, the AFK channel last.
pub fn ordered(state: &AppState, community: CommunityId) -> Vec<&Channel> {
    let by_position = |a: &&Channel, b: &&Channel| (a.position, a.id).cmp(&(b.position, b.id));
    let in_server: Vec<&Channel> = state
        .channels
        .iter()
        .filter(|c| c.community_id == community)
        .collect();
    let mut cats: Vec<&Channel> = in_server.iter().copied().filter(|c| c.kind == 2).collect();
    cats.sort_by(by_position);
    let is_cat = |id: Option<ChannelId>| id.is_some_and(|id| cats.iter().any(|c| c.id == id));
    let mut top: Vec<&Channel> = in_server
        .iter()
        .copied()
        .filter(|c| c.kind != 2 && !is_cat(c.parent_id))
        .collect();
    top.sort_by(by_position);
    let mut out: Vec<&Channel> = top.iter().copied().filter(|c| !is_afk(c)).collect();
    for cat in &cats {
        out.push(cat);
        let mut kids: Vec<&Channel> = in_server
            .iter()
            .copied()
            .filter(|c| c.kind != 2 && c.parent_id == Some(cat.id))
            .collect();
        kids.sort_by(by_position);
        out.extend(kids.into_iter().filter(|c| !is_afk(c)));
    }
    out.extend(in_server.iter().copied().filter(|c| is_afk(c)));
    out
}

/// shell-model.ts `stepChannel`: the listed channel `delta` places from `current` (categories are never listed), clamped at
/// both ends; the first one when none is current.
pub fn step_channel(
    state: &AppState,
    community: CommunityId,
    current: Option<ChannelId>,
    delta: i32,
) -> Option<ChannelId> {
    let rows: Vec<&Channel> = ordered(state, community)
        .into_iter()
        .filter(|c| c.kind != 2)
        .collect();
    let first = rows.first()?;
    let Some(at) = current.and_then(|c| rows.iter().position(|r| r.id == c)) else {
        return Some(first.id);
    };
    let to = (at as i32 + delta).clamp(0, rows.len() as i32 - 1);
    Some(rows[to as usize].id)
}

/// One entry of the list as it is drawn.
#[derive(Clone, Copy, Debug)]
pub enum Item<'a> {
    Category { channel: &'a Channel, folded: bool },
    Channel(&'a Channel),
}

/// ChannelList.tsx `entriesOf`: the entries in display order. A folded category still shows the children that are the
/// selected one or that have unread or mentions; the AFK channel is never folded away.
pub fn items<'a>(
    state: &'a AppState,
    community: CommunityId,
    selected: Option<ChannelId>,
    collapsed: &HashSet<String>,
) -> Vec<Item<'a>> {
    let mut out = Vec::new();
    let mut folded = false;
    let mut parent: Option<ChannelId> = None;
    for c in ordered(state, community) {
        if c.kind == 2 {
            folded = collapsed.contains(&c.id.hex());
            parent = Some(c.id);
            out.push(Item::Category { channel: c, folded });
            continue;
        }
        let in_folded = folded && c.parent_id == parent && !is_afk(c);
        let busy = state
            .unread
            .get(&c.id)
            .is_some_and(|b| b.unread + b.mentions > 0);
        if in_folded && selected != Some(c.id) && !busy {
            continue;
        }
        out.push(Item::Channel(c));
    }
    out
}

fn kind_of(c: &Channel) -> RowKind {
    match c.kind {
        1 if c.name == "AFK" => RowKind::Afk,
        1 => RowKind::Voice,
        _ => RowKind::Text,
    }
}

/// The element id of a channel's row: `channel-<name>`.
pub fn row_id(name: &str) -> SharedString {
    format!("channel-{name}").into()
}

/// The probe ids of rows live as long as the process (the automation registry keys on `&'static str`).
#[cfg(feature = "automation")]
pub(crate) fn probe_id(prefix: &str, name: &str) -> &'static str {
    use std::collections::HashMap;
    use std::sync::Mutex;
    // App-wide by design (final review sweep): interned automation ids (element names, never a client handle).
    static IDS: Mutex<Option<HashMap<String, &'static str>>> = Mutex::new(None);
    let mut ids = IDS.lock().expect("row ids");
    ids.get_or_insert_with(HashMap::new)
        .entry(format!("{prefix}-{name}"))
        .or_insert_with(|| Box::leak(format!("{prefix}-{name}").into_boxed_str()))
}

/// Roving focus over `handles` (Up and Down step and stop at the ends, Home and End jump), Enter and Space press, the Menu
/// key and Shift+F10 ask for the context menu at the focused entry's corner.
struct Roves {
    handles: Vec<gpui::FocusHandle>,
}

impl Roves {
    fn key(
        &self,
        index: usize,
        event: &gpui::KeyDownEvent,
        window: &mut Window,
        cx: &mut App,
    ) -> Key {
        let key = &event.keystroke;
        if let Some(next) = Roving::List.next(key, index, self.handles.len()) {
            cx.stop_propagation();
            self.handles[next].focus(window, cx);
            return Key::Moved;
        }
        if opens_context_menu(key) {
            cx.stop_propagation();
            return Key::Menu;
        }
        let m = &key.modifiers;
        if (key.key == "enter" || key.key == "space")
            && !(m.alt || m.control || m.platform || m.shift)
        {
            cx.stop_propagation();
            return Key::Press;
        }
        Key::None
    }
}

enum Key {
    None,
    Moved,
    Press,
    Menu,
}

/// A channel row's accessible name (`shell.channels.rowLabel` / `rowLabelMuted`): the bare name, or the name and what is
/// new in it; a voice row the device is in says so (`voice.row.joined`).
pub fn row_label(name: &str, unread: u32, mentions: u32, muted: bool, joined: bool) -> String {
    let mut label = if muted {
        tf(
            "shell.channels.rowLabelMuted",
            &[("name", name), ("mentions", &mentions.to_string())],
        )
    } else if unread > 0 || mentions > 0 {
        tf(
            "shell.channels.rowLabel",
            &[
                ("name", name),
                ("unread", &unread.to_string()),
                ("mentions", &mentions.to_string()),
            ],
        )
    } else {
        name.to_owned()
    };
    if joined {
        label.push_str(", ");
        label.push_str(t("voice.row.joined"));
    }
    label
}

#[allow(clippy::too_many_arguments)]
fn row(
    c: &Channel,
    index: usize,
    roves: &Rc<Roves>,
    stop: usize,
    active: bool,
    badge: (u32, u32),
    muted: bool,
    voice: (usize, bool),
    after_text_row: bool,
    handlers: &ListHandlers,
    window: &mut Window,
    cx: &mut App,
) -> gpui::AnyElement {
    let id = c.id;
    let on_select = handlers.on_select.clone();
    let on_click = handlers.on_select.clone();
    let row = Row::new(row_id(&c.name), c.name.clone(), kind_of(c))
        .voice(voice.0, voice.1)
        .active(active)
        .unread(badge.0)
        .unread_label(tf(
            "layout.row.new",
            &[("n", &dilla_desktop_ui::components::badge::capped(badge.0))],
        ))
        .mentions(badge.1)
        .muted(muted)
        .on_click(move |_: &ClickEvent, window, cx| on_click(id, window, cx));
    // The item is a block: the row, then what other areas draw under it (an empty slot takes no height).
    let focus = roves.handles[index].clone().tab_stop(index == stop);
    let (on_menu, on_menu_key) = (handlers.on_menu.clone(), handlers.on_menu.clone());
    let roves = roves.clone();
    let wrapper = div()
        .id(SharedString::from(format!("channel-focus-{}", c.id.hex())))
        .a11y(
            Role::Link,
            row_label(&c.name, badge.0, badge.1, muted, voice.1),
        )
        .aria_selected(active)
        // The row inside takes the pointer's click; a screen reader's "activate" lands here.
        .a11y_activate({
            let on_select = handlers.on_select.clone();
            move |window, cx| on_select(id, window, cx)
        })
        .track_focus(&focus)
        .relative()
        .rounded(radius::ROW)
        .focus_visible(|s| s.shadow(focus_ring()))
        .on_mouse_down(MouseButton::Right, move |e: &MouseDownEvent, window, cx| {
            on_menu(id, e.position, window, cx);
        })
        .on_key_down(move |event, window, cx| {
            match roves.key(index, event, window, cx) {
                Key::Press => on_select(id, window, cx),
                // The keyboard asks for the menu where the pointer last was (the row's own box is not known here).
                Key::Menu => on_menu_key(id, window.mouse_position(), window, cx),
                Key::Moved | Key::None => {}
            }
        })
        .child(row);
    #[cfg(feature = "automation")]
    let wrapper = wrapper.child(crate::automation::focus_probe(
        probe_id("focus-channel", &c.name),
        &focus,
    ));
    let group = SharedString::from(format!("voice-item-{}", c.id.hex()));
    // `--list-gap` above the item: a text row's margin collapses out of its `<li>` (above it, and below it onto the next
    // item); a voice row's margins stay inside its `flow-root` item (VoiceChannelRow.css), so a voice item has a gap
    // above it only under a text row.
    let gap = if c.kind != 1 || after_text_row {
        layout::LIST_GAP
    } else {
        Pixels::ZERO
    };
    let mut item = div().relative().group(group.clone()).mt(gap).child(wrapper);
    // A voice row the device is not in offers its join icon over the row's slot while hovered (VoiceChannelRow.css
    // `.d-voice-row__join`); its click joins (17) and never opens the row.
    if c.kind == 1 && !voice.1 {
        item = item.child(
            div()
                .absolute()
                .top_0()
                .right(Pixels::from(css::JOIN_RIGHT))
                .h(layout::ROW)
                .flex()
                .items_center()
                .invisible()
                .group_hover(group, |s| s.visible())
                .child(
                    dilla_desktop_ui::components::icon_button::IconButton::new(
                        SharedString::from(format!("voice-join-{}", c.name)),
                        IconName::Phone,
                        crate::strings::t("voice.join"),
                    )
                    .size(dilla_desktop_ui::components::icon_button::IconButtonSize::S28)
                    .on_click(move |_, window, cx| {
                        crate::screens::voice::join(id, crate::screens::voice::wid(window), cx)
                    }),
                ),
        );
    }
    let item = item.child(crate::slots::channel_row_extras(c, window, cx));
    #[cfg(feature = "automation")]
    let item = item.child(crate::automation::bounds_probe(probe_id(
        "channel", &c.name,
    )));
    if c.kind == 1 {
        item.py(Pixels::from(css::VOICE_PAD)).into_any_element()
    } else {
        item.into_any_element()
    }
}

/// The gap above a category's head: its own 10px (`.d-channel-list__cat-head`), plus `--list-gap` under a text row, whose
/// bottom margin collapses out of its `<li>`; a voice row keeps its margins inside its `flow-root` item and a category
/// head's list ends at its last item, so nothing is added after them.
pub fn category_top_margin(after_text_row: bool) -> f32 {
    css::CAT_TOP
        + if after_text_row {
            f32::from(layout::LIST_GAP)
        } else {
            0.0
        }
}

/// A category's head (ChannelList.css `.d-channel-list__cat-head`): the chevron, the name in tracked capitals, a rule, and
/// on hover or focus the "+" that makes a channel in it.
#[allow(clippy::too_many_arguments)]
fn category(
    c: &Channel,
    folded: bool,
    after_text_row: bool,
    index: usize,
    roves: &Rc<Roves>,
    stop: usize,
    can_add: bool,
    handlers: &ListHandlers,
) -> gpui::AnyElement {
    let id = c.id;
    let (on_toggle, on_key_toggle, on_add) = (
        handlers.on_toggle.clone(),
        handlers.on_toggle.clone(),
        handlers.on_add.clone(),
    );
    let focus = roves.handles[index].clone().tab_stop(index == stop);
    let roves = roves.clone();
    let group = SharedString::from(format!("category-group-{}", c.id.hex()));
    let chevron = if folded {
        IconName::ChevronRight
    } else {
        IconName::ChevronDown
    };
    let head = div()
        .id(SharedString::from(format!("category-{}", c.name)))
        .a11y(Role::Button, c.name.clone())
        .aria_expanded(!folded)
        .track_focus(&focus)
        .relative()
        .w_full()
        .h(layout::ROW_H_SUB)
        .px(Pixels::from(css::CAT_SIDE))
        .rounded(radius::ROW)
        .flex()
        .items_center()
        .gap(Pixels::from(css::CAT_GAP))
        .cursor_pointer()
        .focus_visible(|s| s.shadow(focus_ring()))
        .on_click(move |_: &ClickEvent, window, cx| on_toggle(id, window, cx))
        .on_key_down(move |event, window, cx| {
            if let Key::Press = roves.key(index, event, window, cx) {
                on_key_toggle(id, window, cx);
            }
        })
        .child(
            Icon::new(chevron)
                .size(Pixels::from(css::CHEVRON))
                .color(color::FG_3),
        )
        .child(
            line(
                c.name.to_uppercase(),
                text::MICRO,
                line_height(text::MICRO, 1.5),
                color::FG_3,
            )
            .weight(font::WEIGHT_LABEL)
            .tracking(LABEL_TRACKING_WIDE)
            .ellipsis(),
        )
        .child(
            div()
                .flex_1()
                .min_w(Pixels::from(css::CAT_RULE_MIN))
                .h(Pixels::from(css::HAIRLINE_W))
                .bg(color::HAIRLINE),
        )
        // With a "+" to show, the head keeps its trailing slot (`<TrailingSlot />`, --slot-w) so the rule stops short of it.
        .children(can_add.then(|| div().flex_none().w(layout::SLOT)));
    #[cfg(feature = "automation")]
    let head = head.child(crate::automation::focus_probe(
        probe_id("focus-category", &c.name),
        &focus,
    ));
    let mut cell = div()
        .id(SharedString::from(format!("category-cell-{}", c.id.hex())))
        .group(group.clone())
        .relative()
        .mt(Pixels::from(category_top_margin(after_text_row)))
        .child(head);
    if can_add {
        let label = tf("layout.category.add", &[("name", c.name.as_str())]);
        cell = cell.child(
            div()
                .absolute()
                .top_0()
                .right_0()
                .h(layout::ROW_H_SUB)
                .flex()
                .items_center()
                .opacity(0.0)
                .group_hover(group, |s| s.opacity(1.0))
                .child(
                    div()
                        .id(SharedString::from(format!("category-add-{}", c.name)))
                        .a11y(Role::Button, label.clone())
                        .size(Pixels::from(css::ADD_HIT))
                        .flex_none()
                        .flex()
                        .items_center()
                        .justify_center()
                        .cursor_pointer()
                        .tooltip(dilla_desktop_ui::components::tooltip::tooltip(label, &[]))
                        .on_click(move |_: &ClickEvent, window, cx| on_add(Some(id), window, cx))
                        .child(
                            Icon::new(IconName::Plus)
                                .size(Pixels::from(css::GLYPH))
                                .color(color::FG_2),
                        ),
                ),
        );
    }
    #[cfg(feature = "automation")]
    let cell = cell.child(crate::automation::bounds_probe(probe_id(
        "category", &c.name,
    )));
    cell.into_any_element()
}

/// The sidebar above the user panel.
pub fn render(
    model: &ListModel<'_>,
    handlers: &ListHandlers,
    window: &mut Window,
    cx: &mut App,
) -> impl IntoElement + use<> {
    let state = model.state;
    let community = model
        .community
        .and_then(|id| state.communities.iter().find(|c| c.id == id));
    let title = community.map_or_else(String::new, |c| c.name.clone());
    let on_add = handlers.on_add.clone();
    let on_server_settings = handlers.on_server_settings.clone();
    let on_server_menu_open = handlers.on_server_menu_open.clone();
    let on_server_menu_pick = handlers.on_server_menu_pick.clone();
    let server_items = model
        .community
        .map(|c| server_menu_items(model.settings, c))
        .unwrap_or_default();
    let title_for_tip = title.clone();
    let head = div()
        .flex_none()
        .flex()
        .items_center()
        .pt(Pixels::from(css::HEAD_TOP))
        .pb(Pixels::from(css::HEAD_BOTTOM))
        .pl(Pixels::from(css::HEAD_LEFT))
        .pr(Pixels::from(css::HEAD_RIGHT))
        .gap(Pixels::from(css::HEAD_GAP))
        .child(
            div().flex_1().min_w_0().flex().child(
                line(title, text::TITLE, line_height(text::TITLE, 1.5), color::FG)
                    .face(Face::SerifItalic)
                    .clip(),
            ),
        )
        .children(model.can_add.then(|| {
            crate::tracked_box(
                "channel-list-add-button",
                div()
                    .id("channel-list-add")
                    .tab_index(0)
                    .rounded(radius::MD)
                    .focus_visible(|s| s.shadow(focus_ring()))
                    .a11y(
                        Role::Button,
                        tf(
                            "create.channel.title",
                            &[("server", title_for_tip.as_str())],
                        ),
                    )
                    .cursor_pointer()
                    .tooltip(dilla_desktop_ui::components::tooltip::tooltip(
                        tf(
                            "create.channel.title",
                            &[("server", title_for_tip.as_str())],
                        ),
                        &[],
                    ))
                    .on_click(move |_: &ClickEvent, window, cx| on_add(None, window, cx))
                    .child(centred(
                        Pixels::from(css::ADD_HIT),
                        Icon::new(IconName::Plus)
                            .size(Pixels::from(css::GLYPH))
                            .color(color::FG_2),
                    )),
            )
        }))
        .child(crate::tracked_box(
            "server-settings-open",
            // Shell.tsx `serverMenu`: the ⌄ is the server's menu, labelled `admin.open`, in the head's 26px button.
            ActionMenu::new("channel-list-server-menu", t("admin.open"), server_items)
                .icon(IconName::ChevronDown)
                .icon_size(Pixels::from(css::GLYPH))
                .trigger_size(Pixels::from(css::MENU_HIT))
                .open(model.server_menu_open)
                .on_open_change(move |open, window, cx| on_server_menu_open(open, window, cx))
                .on_select(move |item, window, cx| {
                    if item == SERVER_SETTINGS {
                        on_server_settings(window, cx);
                    } else {
                        on_server_menu_pick(item, window, cx);
                    }
                }),
        ));
    let meta = |s: String| line(s, text::META, line_height(text::META, 1.5), color::FG_3);
    let stats = div()
        .flex_none()
        .flex()
        .items_center()
        .pl(Pixels::from(css::STATS_LEFT))
        .pb(Pixels::from(css::STATS_BOTTOM))
        .gap(Pixels::from(css::STATS_GAP))
        .child(
            div()
                .flex()
                .items_center()
                .gap(Pixels::from(css::STAT_GAP))
                .child(
                    div()
                        .size(Pixels::from(css::LIVE_DOT))
                        .rounded_full()
                        .bg(color::OK),
                )
                .child(meta(tf(
                    "layout.sidebar.online",
                    &[("n", &model.online.to_string())],
                ))),
        )
        .child(meta("·".to_owned()))
        .child(meta(format!("{} in voice", model.in_voice)));
    let entries = community.map_or_else(Vec::new, |c| {
        items(state, c.id, model.selected, model.collapsed)
    });
    // One tab stop: the selected entry, else the first.
    let handles = focus_handles("channel-list-focus", entries.len(), window, cx);
    let selected_at = entries
        .iter()
        .position(|e| matches!(e, Item::Channel(c) if Some(c.id) == model.selected));
    let focused_at = handles.iter().position(|h| h.is_focused(window));
    let stop = Roving::stop(focused_at, selected_at, entries.len());
    let roves = Rc::new(Roves { handles });
    // Whether the entry above `i` is a text row (its `--list-gap` bottom margin collapses onto the next item).
    let after_text = |i: usize| {
        i.checked_sub(1)
            .is_some_and(|p| matches!(entries[p], Item::Channel(prev) if prev.kind != 1))
    };
    let children: Vec<gpui::AnyElement> = entries
        .iter()
        .enumerate()
        .map(|(i, e)| match *e {
            Item::Category { channel, folded } => category(
                channel,
                folded,
                after_text(i),
                i,
                &roves,
                stop,
                model.can_add_in_category,
                handlers,
            ),
            Item::Channel(ch) => row(
                ch,
                i,
                &roves,
                stop,
                model.selected == Some(ch.id),
                notify::row_counts(state, model.settings, ch.id),
                notify::is_muted(model.settings, ch.id, Some(ch.community_id)),
                (
                    crate::screens::voice::count(state, ch.id),
                    crate::screens::voice::joined(state, ch.id),
                ),
                after_text(i),
                handlers,
                window,
                cx,
            ),
        })
        .collect();
    div()
        .size_full()
        .flex()
        .flex_col()
        .child(head)
        .child(stats)
        .child(
            div()
                .id("channel-list")
                .flex_1()
                .min_h_0()
                .overflow_y_scroll()
                .border_t_1()
                .border_color(color::HAIRLINE)
                .pt(Pixels::from(css::LIST_TOP))
                .px(Pixels::from(css::LIST_SIDE))
                .flex()
                .flex_col()
                .children(children),
        )
}

#[cfg(test)]
mod tests {
    use dilla_client::state::{BadgeState, ChannelGroupState, Community};

    use super::*;

    fn ch(n: u8, kind: u8, name: &str, parent: Option<u8>, position: u32) -> Channel {
        Channel {
            id: ChannelId([n; 16]),
            community_id: CommunityId([1; 16]),
            kind,
            mode: 0,
            name: name.to_owned(),
            topic: String::new(),
            parent_id: parent.map(|p| ChannelId([p; 16])),
            position,
            slowmode: 0,
            text_group_id: None,
            group: ChannelGroupState::Active,
        }
    }

    fn world() -> AppState {
        let mut s = AppState::default();
        s.communities.push(Community {
            id: CommunityId([1; 16]),
            name: "Midgard".into(),
            owner: dilla_client::state::UserId([9; 16]),
            policy_version: 0,
        });
        s.channels = vec![
            ch(2, 0, "general", None, 0),
            ch(3, 2, "raids", None, 1),
            ch(4, 0, "loot", Some(3), 0),
            ch(5, 1, "harbour", Some(3), 1),
            ch(6, 1, "AFK", None, 1 << 30),
        ];
        s
    }

    fn names(items: &[Item<'_>]) -> Vec<String> {
        items
            .iter()
            .map(|i| match i {
                Item::Category { channel, folded } => {
                    format!("[{}{}]", channel.name, if *folded { "-" } else { "" })
                }
                Item::Channel(c) => c.name.clone(),
            })
            .collect()
    }

    #[test]
    fn the_server_menu_reads_the_stored_mute_and_mode() {
        let server = CommunityId([1; 16]);
        let mut settings = std::collections::HashMap::<String, String>::new();
        let ids = |items: &[MenuItem]| items.iter().map(|i| i.id.to_string()).collect::<Vec<_>>();
        let items = server_menu_items(&settings, server);
        assert_eq!(
            ids(&items),
            [
                "settings",
                "mute",
                "mode:default",
                "mode:all",
                "mode:mentions",
                "mode:nothing"
            ]
        );
        assert_eq!(items[1].check, Some(false));
        assert_eq!(items[2].checked, Some(true), "no stored mode is `default`");
        settings.insert(notify::key_community_mute(server), "1".into());
        settings.insert(notify::key_community_mode(server), "mentions".into());
        let items = server_menu_items(&settings, server);
        assert_eq!(items[1].check, Some(true));
        assert_eq!(
            items
                .iter()
                .filter(|i| i.checked == Some(true))
                .map(|i| i.id.to_string())
                .collect::<Vec<_>>(),
            ["mode:mentions"]
        );
    }

    #[test]
    fn a_categorys_gap_is_ten_px_and_a_text_rows_list_gap() {
        assert_eq!(category_top_margin(false), 10.0);
        assert_eq!(category_top_margin(true), 11.0);
    }

    #[test]
    fn afk_is_listed_last_whatever_its_position() {
        let s = world();
        let list = items(&s, CommunityId([1; 16]), None, &HashSet::new());
        assert_eq!(
            names(&list),
            ["general", "[raids]", "loot", "harbour", "AFK"]
        );
    }

    #[test]
    fn a_folded_category_keeps_the_selected_and_the_unread_rows() {
        let mut s = world();
        let folded: HashSet<String> = [ChannelId([3; 16]).hex()].into();
        let community = CommunityId([1; 16]);
        let list = items(&s, community, None, &folded);
        assert_eq!(names(&list), ["general", "[raids-]", "AFK"]);
        let list = items(&s, community, Some(ChannelId([4; 16])), &folded);
        assert_eq!(names(&list), ["general", "[raids-]", "loot", "AFK"]);
        s.unread.insert(
            ChannelId([5; 16]),
            BadgeState {
                unread: 0,
                mentions: 2,
            },
        );
        let list = items(&s, community, None, &folded);
        assert_eq!(names(&list), ["general", "[raids-]", "harbour", "AFK"]);
    }

    #[test]
    fn stepping_clamps_at_both_ends_and_skips_categories() {
        let s = world();
        let c = CommunityId([1; 16]);
        let at = |cur: Option<u8>, d| {
            step_channel(&s, c, cur.map(|n| ChannelId([n; 16])), d).map(|i| i.0[0])
        };
        assert_eq!(at(None, 1), Some(2), "none current: the first");
        assert_eq!(at(Some(2), 1), Some(4), "the category is not a stop");
        assert_eq!(at(Some(2), -1), Some(2));
        assert_eq!(at(Some(6), 1), Some(6));
        assert_eq!(at(Some(6), -1), Some(5));
    }
}
