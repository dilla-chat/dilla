//! The server rail (packages/ui CommunityRail, Signal §5.1): the `@` tile (direct messages), a hairline, a round tile for
//! each server (the selected one a rounded square in the accent, ringed), a mention count at the tile's corner, and the
//! dashed "+" tile. One tab stop with roving focus over the tiles (Up and Down step, Home and End jump, Enter presses).

use std::collections::HashSet;
use std::rc::Rc;

use dilla_client::state::{AppState, CommunityId};
use gpui::{
    App, BoxShadow, ClickEvent, InteractiveElement, IntoElement, ParentElement, Pixels,
    SharedString, StatefulInteractiveElement, Styled, Window, div, point, prelude::FluentBuilder,
};

use dilla_desktop_ui::a11y::{Accessible, Role};
use dilla_desktop_ui::components::{Badge, BadgeKind};
use dilla_desktop_ui::metrics::focus_ring;
use dilla_desktop_ui::roving::{Roving, focus_handles};
use dilla_desktop_ui::text::{line, line_height, mono_covers, mono_width};
use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::screens::centred;
use crate::strings::{t, tf};

/// AppShell.css and CommunityRail.css.
mod css {
    /// The rail's 10px top padding and its 6px item gap.
    pub const TOP: f32 = 10.0;
    pub const GAP: f32 = 6.0;
    /// The `@` tile's rule: 20x1px with 2px margins.
    pub const RULE_W: f32 = 20.0;
    pub const RULE_H: f32 = 1.0;
    pub const RULE_MARGIN: f32 = 2.0;
    /// The selected tile's ring, `box-shadow: 0 0 0 2px var(--bg-2), 0 0 0 3.5px var(--accent)` (CommunityRail.css:15): a
    /// 2px gap in `--bg-2`, out to a ring 3.5px from the tile's edge, which is 7 device px at the web's scale 2.
    pub const RING_GAP: f32 = 2.0;
    pub const RING_OUTSET: f32 = 3.5;
    /// The rail's edge, an inset shadow on the web.
    pub const EDGE: f32 = 1.0;
    /// The glyphs of the `@` and "+" tiles.
    pub const HOME_GLYPH: f32 = 17.0;
    pub const ADD_GLYPH: f32 = 15.0;
    /// `.d-community-rail__badge`: 4px out at the bottom right.
    pub const BADGE_OUT: f32 = 4.0;
}

pub struct RailModel<'a> {
    pub state: &'a AppState,
    pub selected: Option<CommunityId>,
    /// The `@` tile is the selected one (a direct message or the DM list is open).
    pub home_selected: bool,
    /// Muted channels (hex ids): their mentions still count, their unread never does.
    pub muted: &'a HashSet<String>,
}

#[derive(Clone)]
pub struct RailHandlers {
    pub on_pick: super::shell::PickServer,
    pub on_home: super::shell::Action,
    pub on_add: super::shell::Action,
}

/// The selected tile's ring as the web draws it, two `box-shadow`s with no blur (`0 0 0 2px bg-2, 0 0 0 3.5px accent`,
/// CommunityRail.css:15): the accent ring out to 3.5px (7 device px at the web's scale 2), the `--bg-2` gap out to 2px over
/// it. Each is a layer under the tile: its spread and corner radius (the tile's `--r-xl` plus the spread, as CSS grows a
/// shadow's radius) and its colour. The layers are carrier boxes the size of the tile, because GPUI draws a shadow from
/// its element's box, grown by the spread to device pixels (a box of the ring's own size would snap to whole logical px
/// and put the ring half a pixel in), and gives it the element's own radius.
fn selected_ring() -> [(Pixels, BoxShadow); 2] {
    [
        (css::RING_OUTSET, color::ACCENT),
        (css::RING_GAP, color::BG_2),
    ]
    .map(|(spread, colour)| {
        (
            radius::XL + Pixels::from(spread),
            BoxShadow {
                color: colour,
                offset: point(Pixels::ZERO, Pixels::ZERO),
                blur_radius: Pixels::ZERO,
                spread_radius: Pixels::from(spread),
                inset: false,
            },
        )
    })
}

/// Where the initials start in the 38px tile when the web's centring puts them at a fraction of a pixel: centred by their
/// width on Chromium's layout unit, which is known only for text the embedded mono face covers (one advance a character,
/// 12.4px for two letters). Anything else (CJK, emoji, a fallback face) has the text system's own measured width, which
/// only the layout sees: `None`, and the initials are centred by a flex box instead.
fn initials_x(initials: &str) -> Option<f32> {
    initials.chars().all(mono_covers).then(|| {
        (f32::from(layout::RAIL_TILE) - mono_width(initials.chars().count(), text::XS)) / 2.0
    })
}

/// A server's initials in its tile (`.d-community-rail__item`: 11px/700 on `line-height: 1`, the tile an inline flex box
/// centring them): the 11px line box is centred in the 38px tile, and the text in it by its width. Where that width is
/// the mono advances ([`initials_x`]) the glyphs start at a fraction of a pixel (12.4px for two letters), which a flex
/// box of GPUI would snap to a whole one: the line is placed from the fraction ([`Line::exact`]). Otherwise a flex box
/// centres the measured text.
fn initials_line(initials: &str, colour: gpui::Hsla) -> gpui::AnyElement {
    let tile = f32::from(layout::RAIL_TILE);
    let height = line_height(text::XS, 1.0);
    let text = line(initials.to_owned(), text::XS, height, colour).weight(font::WEIGHT_DISPLAY);
    match initials_x(initials) {
        Some(x) => text.exact(x, (tile - height) / 2.0),
        None => div()
            .absolute()
            .inset_0()
            .flex()
            .items_center()
            .justify_center()
            .child(text)
            .into_any_element(),
    }
}

/// A tile in its slot of the rail: the slot is the tile's 38px box, and a selected tile has its ring's two layers drawn
/// under it ([`selected_ring`]); they take no room in the rail.
fn slot(selected: bool, tile: impl IntoElement) -> gpui::Div {
    let slot = div().relative().flex_none();
    let slot = if selected {
        selected_ring()
            .into_iter()
            .fold(slot, |slot, (radius, shadow)| {
                slot.child(
                    div()
                        .absolute()
                        .inset_0()
                        .rounded(radius)
                        .shadow(vec![shadow]),
                )
            })
    } else {
        slot
    };
    slot.child(tile)
}

/// Mentions (and unread) summed over a server's channels (`sumBadges`; a muted channel hides its unread).
pub fn counts(state: &AppState, community: CommunityId, muted: &HashSet<String>) -> (u32, u32) {
    state
        .channels
        .iter()
        .filter(|c| c.community_id == community)
        .fold((0, 0), |(unread, mentions), c| {
            let b = state.unread.get(&c.id).copied().unwrap_or_default();
            let unread_here = if muted.contains(&c.id.hex()) {
                0
            } else {
                b.unread
            };
            (unread + unread_here, mentions + b.mentions)
        })
}

/// `CommunityRail`'s item label: the bare name, or the name and what is new on it.
pub fn item_label(name: &str, unread: u32, mentions: u32) -> String {
    if unread > 0 || mentions > 0 {
        tf(
            "shell.rail.itemLabel",
            &[
                ("name", name),
                ("unread", &unread.to_string()),
                ("mentions", &mentions.to_string()),
            ],
        )
    } else {
        name.to_owned()
    }
}

/// A tile's focus wiring: the tile is a button of the rail's roving group.
struct Stops {
    handles: Vec<gpui::FocusHandle>,
    stop: usize,
}

fn key_handler(
    stops: Rc<Stops>,
    index: usize,
    press: super::shell::Action,
) -> impl Fn(&gpui::KeyDownEvent, &mut Window, &mut App) + 'static {
    move |event, window, cx| {
        let key = &event.keystroke;
        if let Some(next) = Roving::List.next(key, index, stops.handles.len()) {
            cx.stop_propagation();
            stops.handles[next].focus(window, cx);
            return;
        }
        let m = &key.modifiers;
        if (key.key == "enter" || key.key == "space")
            && !(m.alt || m.control || m.platform || m.shift)
        {
            cx.stop_propagation();
            press(window, cx);
        }
    }
}

pub fn render(
    model: &RailModel<'_>,
    handlers: &RailHandlers,
    window: &mut Window,
    cx: &mut App,
) -> impl IntoElement + use<> {
    let state = model.state;
    // Stops in order: the `@` tile, then the servers; the "+" tile is the last.
    let count = state.communities.len() + 2;
    let handles = focus_handles("rail-focus", count, window, cx);
    let active = if model.home_selected {
        Some(0)
    } else {
        state
            .communities
            .iter()
            .position(|c| Some(c.id) == model.selected)
            .map(|i| i + 1)
    };
    let focused = handles.iter().position(|h| h.is_focused(window));
    let stops = Rc::new(Stops {
        stop: Roving::stop(focused, active, count),
        handles,
    });
    let tab_stop = |i: usize| stops.handles[i].clone().tab_stop(i == stops.stop);

    let home = {
        let on_home = handlers.on_home.clone();
        let on_key = handlers.on_home.clone();
        let mut tile = div()
            .id("rail-home")
            .a11y(Role::Button, t("nav.tab.dms"))
            .aria_selected(model.home_selected)
            .track_focus(&tab_stop(0))
            .flex_none()
            .rounded_full()
            .bg(color::HOVER)
            .cursor_pointer()
            .focus_visible(|s| s.shadow(focus_ring()))
            .tooltip(dilla_desktop_ui::components::tooltip::tooltip(
                t("nav.tab.dms"),
                &[],
            ))
            .on_click(move |_: &ClickEvent, window, cx| on_home(window, cx))
            .on_key_down(key_handler(stops.clone(), 0, on_key))
            .relative()
            .child(centred(
                layout::RAIL_TILE,
                Icon::new(IconName::AtSign)
                    .size(Pixels::from(css::HOME_GLYPH))
                    .color(if model.home_selected {
                        color::ACCENT
                    } else {
                        color::FG_2
                    }),
            ));
        if model.home_selected {
            // CommunityRail.css: the selected `@` is a selected tile (the 10px square with its ring) on
            // `--surface-hi`, its glyph in the accent.
            tile = tile.bg(color::SURFACE_HI).rounded(radius::XL);
        }
        #[cfg(feature = "automation")]
        let tile = tile.child(crate::automation::focus_probe("rail-home", &tab_stop(0)));
        slot(model.home_selected, tile)
    };

    let tiles = state.communities.iter().enumerate().map(|(i, c)| {
        let initials = dilla_desktop_ui::components::avatar::initials_of(&c.name);
        let picked = Some(c.id) == model.selected;
        let (id, on_pick, on_key) = (c.id, handlers.on_pick.clone(), handlers.on_pick.clone());
        let press: super::shell::Action = Rc::new(move |window, cx| on_key(id, window, cx));
        let (unread, mentions) = counts(state, c.id, model.muted);
        let tile = div()
            .id(SharedString::from(format!("server-{}", c.id.hex())))
            .a11y(Role::Button, item_label(&c.name, unread, mentions))
            .aria_selected(picked)
            .track_focus(&tab_stop(i + 1))
            .relative()
            .flex_none()
            .size(layout::RAIL_TILE)
            // CommunityRail.css: a server is a round tile in `--hover`; the selected one a rounded square in the accent.
            .rounded(if picked { radius::XL } else { radius::PILL })
            .bg(if picked { color::ACCENT } else { color::HOVER })
            .cursor_pointer()
            .focus_visible(|s| s.shadow(focus_ring()))
            .tooltip(dilla_desktop_ui::components::tooltip::tooltip(
                item_label(&c.name, unread, mentions),
                &[],
            ))
            .on_click(move |_: &ClickEvent, window, cx| on_pick(id, window, cx))
            .on_key_down(key_handler(stops.clone(), i + 1, press))
            .child(initials_line(
                &initials,
                if picked {
                    color::ACCENT_INK
                } else {
                    color::FG_2
                },
            ));
        #[cfg(feature = "automation")]
        let tile = tile.child(crate::automation::focus_probe(
            crate::screens::channel_list::probe_id("rail-server", &c.name),
            &tab_stop(i + 1),
        ));
        // Unread alone is for a screen reader; a mention is a count at the bottom right.
        let tile = tile.when(mentions > 0, |tile| {
            tile.child(
                div()
                    .absolute()
                    .right(Pixels::from(-css::BADGE_OUT))
                    .bottom(Pixels::from(-css::BADGE_OUT))
                    .child(Badge::new(
                        SharedString::from(format!("rail-badge-{}", c.id.hex())),
                        BadgeKind::Mention,
                        mentions,
                    )),
            )
        });
        slot(picked, tile)
    });

    let add = {
        let (on_add, on_key) = (handlers.on_add.clone(), handlers.on_add.clone());
        let add = div()
            .id("rail-add")
            .a11y(Role::Button, t("layout.rail.add"))
            .track_focus(&tab_stop(count - 1))
            .flex_none()
            .rounded_full()
            .cursor_pointer()
            .focus_visible(|s| s.shadow(focus_ring()))
            .tooltip(dilla_desktop_ui::components::tooltip::tooltip(
                t("layout.rail.add"),
                &[],
            ))
            .on_click(move |_: &ClickEvent, window, cx| on_add(window, cx))
            .on_key_down(key_handler(stops.clone(), count - 1, on_key))
            .relative()
            .child(
                centred(
                    layout::RAIL_TILE,
                    Icon::new(IconName::Plus)
                        .size(Pixels::from(css::ADD_GLYPH))
                        .color(color::FG_2),
                )
                .rounded_full()
                .border_1()
                .border_dashed()
                .border_color(color::CONTROL_EDGE_KEY),
            );
        #[cfg(feature = "automation")]
        let add = add.child(crate::automation::focus_probe(
            "rail-add",
            &tab_stop(count - 1),
        ));
        add
    };

    let rail = div()
        .id("rail")
        .landmark(Role::Navigation, t("shell.rail.label"))
        .flex_none()
        .w(layout::RAIL)
        .h_full()
        .relative()
        .bg(color::BG_2)
        // The rail's edge is an inset shadow on the web (no layout width): a 1px line over its last column.
        .child(
            div()
                .absolute()
                .right_0()
                .top_0()
                .w(Pixels::from(css::EDGE))
                .h_full()
                .bg(color::HAIRLINE),
        )
        .flex()
        .flex_col()
        .items_center()
        .pt(Pixels::from(css::TOP))
        .gap(Pixels::from(css::GAP))
        .child(home)
        .child(
            div()
                .my(Pixels::from(css::RULE_MARGIN))
                .w(Pixels::from(css::RULE_W))
                .h(Pixels::from(css::RULE_H))
                .bg(color::HAIRLINE_2),
        )
        .children(tiles)
        .child(add);
    #[cfg(feature = "automation")]
    let rail = rail.child(crate::automation::bounds_probe("shell-rail"));
    rail
}

#[cfg(test)]
mod tests {
    use dilla_client::state::{BadgeState, Channel, ChannelGroupState, ChannelId};

    use super::*;

    fn world() -> (AppState, CommunityId) {
        let community = CommunityId([1; 16]);
        let mut s = AppState::default();
        for n in 2..4u8 {
            s.channels.push(Channel {
                id: ChannelId([n; 16]),
                community_id: community,
                kind: 0,
                mode: 0,
                name: format!("c{n}"),
                topic: String::new(),
                parent_id: None,
                position: 0,
                slowmode: 0,
                text_group_id: None,
                group: ChannelGroupState::Active,
            });
            s.unread.insert(
                ChannelId([n; 16]),
                BadgeState {
                    unread: u32::from(n),
                    mentions: 1,
                },
            );
        }
        (s, community)
    }

    #[test]
    fn a_muted_channel_hides_its_unread_and_keeps_its_mentions() {
        let (s, c) = world();
        assert_eq!(counts(&s, c, &HashSet::new()), (5, 2));
        let muted: HashSet<String> = [ChannelId([3; 16]).hex()].into();
        assert_eq!(counts(&s, c, &muted), (2, 2));
    }

    /// The selected tile's ring reaches 3.5 CSS px out (`0 0 0 2px bg-2, 0 0 0 3.5px accent`): at the web's scale 2 that is 7
    /// device px, on a half pixel of the layout grid, with the 2px `--bg-2` gap painted over the accent. Each layer's
    /// corners are the tile's `--r-xl` grown by its spread.
    #[test]
    fn selected_ring_outset_is_3_5() {
        let ring = selected_ring();
        let spreads: Vec<f32> = ring
            .iter()
            .map(|(_, s)| f32::from(s.spread_radius))
            .collect();
        assert_eq!(spreads, [3.5, 2.0], "accent ring first, the gap over it");
        assert_eq!(ring[0].1.color, color::ACCENT);
        assert_eq!(ring[1].1.color, color::BG_2);
        assert_eq!(spreads[0] * 2.0, 7.0, "7 device px at scale 2");
        assert_eq!(ring[0].0, radius::XL + Pixels::from(3.5));
        assert_eq!(ring[1].0, radius::XL + Pixels::from(2.0));
        assert!(
            ring.iter()
                .all(|(_, s)| f32::from(s.blur_radius) == 0.0 && !s.inset)
        );
    }

    /// Two letters of 11px mono are 13.2px (13.203125 on the layout unit) in the 38px `.d-community-rail__item`, so the web's
    /// flex centring starts them at 12.4px: measured from the shot's `*.layout.json` of shell/conversation (a fraction a
    /// flex box of GPUI would snap to 12). Initials the mono face does not cover (a CJK or emoji initial) have no such
    /// width here and are centred by the layout's measured one; "ß" is upper-cased to "SS" before it is drawn.
    #[test]
    fn initials_start_where_the_web_centres_them() {
        let x = |name: &str| initials_x(&dilla_desktop_ui::components::avatar::initials_of(name));
        assert!((x("Midgard Chat").unwrap() - 12.4).abs() < 0.01);
        assert!((x("M").unwrap() - 15.7).abs() < 0.01);
        assert!((x("ß").unwrap() - 12.4).abs() < 0.01, "ß draws as SS");
        assert_eq!(x("世界"), None, "CJK is measured by the layout");
        assert_eq!(x("😀"), None, "an emoji is measured by the layout");
    }

    #[test]
    fn the_label_names_what_is_new() {
        assert_eq!(item_label("Midgard", 0, 0), "Midgard");
        assert_eq!(item_label("Midgard", 3, 1), "Midgard, unread 3, mentions 1");
    }
}
