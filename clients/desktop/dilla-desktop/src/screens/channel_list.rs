//! The sidebar's channel list (packages/ui ChannelList and ChannelRow, Signal §5.3): the server's name in the serif
//! italic, the online and voice counts, then the channels in position order. Geometry is the web client's at 1280x800
//! (`clients/desktop/parity/references/linux`).

use dilla_client::state::{AppState, Channel, ChannelId, CommunityId};
use gpui::{
    App, ClickEvent, InteractiveElement, IntoElement, ParentElement, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::screens::centred;
use crate::text::{Edge, Face, line};

/// What the sidebar shows.
pub struct ListModel<'a> {
    pub state: &'a AppState,
    pub community: Option<CommunityId>,
    pub selected: Option<ChannelId>,
    /// People online (the slice has no presence: this device's own account while connected).
    pub online: usize,
}

/// What a click on a channel row does.
pub type OnSelect = std::rc::Rc<dyn Fn(ChannelId, &mut Window, &mut App)>;

/// The channels of `community` in the web's order: by position, categories' children under them.
pub fn ordered(state: &AppState, community: CommunityId) -> Vec<&Channel> {
    let mut top: Vec<&Channel> = state
        .channels
        .iter()
        .filter(|c| c.community_id == community && c.parent_id.is_none())
        .collect();
    top.sort_by_key(|c| (c.position, c.id));
    let mut out = Vec::new();
    for c in top {
        out.push(c);
        if c.kind == 2 {
            let mut kids: Vec<&Channel> = state
                .channels
                .iter()
                .filter(|k| k.parent_id == Some(c.id))
                .collect();
            kids.sort_by_key(|k| (k.position, k.id));
            out.extend(kids);
        }
    }
    out
}

fn glyph(c: &Channel) -> IconName {
    match c.kind {
        1 if c.name == "AFK" => IconName::Moon,
        1 => IconName::Volume2,
        _ => IconName::Hash,
    }
}

/// The element id of a channel's row: `channel-<name>`.
pub fn row_id(name: &str) -> SharedString {
    format!("channel-{name}").into()
}

/// `name_top`: the web line box top of the row's name in the window.
fn row(
    c: &Channel,
    active: bool,
    unread: u32,
    name_top: f32,
    on_select: OnSelect,
) -> impl IntoElement {
    let id = c.id;
    let (name_color, glyph_color, weight) = if active {
        (color::FG, color::ACCENT, font::WEIGHT_LABEL)
    } else if unread > 0 {
        (color::FG, color::FG_2, font::WEIGHT_LABEL)
    } else {
        (color::FG_2, color::FG_4, font::WEIGHT_BODY)
    };
    let mut button = div()
        .id(row_id(&c.name))
        .relative()
        .w_full()
        .h(layout::ROW)
        .rounded(radius::ROW)
        .flex()
        .items_center()
        .pl(px(8.))
        .gap(px(8.))
        .cursor_pointer()
        .on_click(move |_: &ClickEvent, window, cx| on_select(id, window, cx))
        .child(Icon::new(glyph(c)).size(px(14.)).color(glyph_color))
        .child(
            line(c.name.clone(), text::INPUT, 19.5, name_color)
                .weight(weight)
                .at(Edge::Start(94.0), Edge::Start(name_top)),
        );
    if active {
        button = button.bg(color::SURFACE_HI).child(
            div()
                .absolute()
                .left_0()
                .top(px(6.))
                .w(px(2.))
                .h(px(16.))
                .rounded(px(2.))
                .bg(color::ACCENT),
        );
    }
    #[cfg(feature = "automation")]
    let button = button.child(crate::automation::bounds_probe(row_probe(&c.name)));
    // A voice row sits in a 30px item (1px above and below its 28px button).
    if c.kind == 1 {
        div().py(px(1.)).child(button).into_any_element()
    } else {
        button.into_any_element()
    }
}

/// The probe ids of rows live as long as the process (the automation registry keys on `&'static str`).
#[cfg(feature = "automation")]
fn row_probe(name: &str) -> &'static str {
    use std::collections::HashMap;
    use std::sync::Mutex;
    static IDS: Mutex<Option<HashMap<String, &'static str>>> = Mutex::new(None);
    let mut ids = IDS.lock().expect("row ids");
    ids.get_or_insert_with(HashMap::new)
        .entry(name.to_owned())
        .or_insert_with(|| Box::leak(format!("channel-{name}").into_boxed_str()))
}

/// The web positions in the window of the stats line and of the channel list's top rule (under the 44px top bar, the
/// 50.5px head and the 25.75px stats).
const STATS_TOP: f32 = 94.5;
const LIST_TOP: f32 = 120.25;

/// The sidebar above the user panel.
pub fn render(model: &ListModel<'_>, on_select: OnSelect) -> impl IntoElement + use<> {
    let state = model.state;
    let community = model
        .community
        .and_then(|id| state.communities.iter().find(|c| c.id == id));
    let title = community.map_or_else(String::new, |c| c.name.clone());
    let head = div()
        .flex_none()
        .flex()
        .items_center()
        .pt(px(14.))
        .pb(px(8.))
        .pl(px(14.))
        .pr(px(12.))
        .gap(px(8.))
        .child(
            div()
                .flex_1()
                .min_w_0()
                .overflow_hidden()
                .child(line(title, text::TITLE, 28.5, color::FG).face(Face::SerifItalic)),
        )
        .child(centred(
            px(28.),
            Icon::new(IconName::Plus).size(px(15.)).color(color::FG_2),
        ))
        .child(centred(
            px(26.),
            Icon::new(IconName::ChevronDown)
                .size(px(15.))
                .color(color::FG_2),
        ));
    let online = format!("{} online", model.online);
    let dot_x = 81.0 + online.chars().count() as f32 * 6.3 + 10.0;
    let voice_x = dot_x + 6.3 + 10.0;
    let stats = div()
        .flex_none()
        .flex()
        .items_center()
        .pl(px(14.))
        .pb(px(10.))
        .gap(px(10.))
        .child(
            div()
                .flex()
                .items_center()
                .gap(px(5.))
                .child(div().size(px(6.)).rounded_full().bg(color::OK))
                .child(
                    line(online, text::META, 15.75, color::FG_3)
                        .at(Edge::Start(81.0), Edge::Start(STATS_TOP)),
                ),
        )
        .child(
            line("·", text::META, 15.75, color::FG_3)
                .at(Edge::Start(dot_x), Edge::Start(STATS_TOP)),
        )
        .child(
            line("0 in voice", text::META, 15.75, color::FG_3)
                .at(Edge::Start(voice_x), Edge::Start(STATS_TOP)),
        );
    // The rows' web positions: the list's first row 6px below its top rule; a text row 28px, a voice item 30px (its
    // button 1px in), 1px apart; the name 4.25px into its button.
    let mut top = LIST_TOP + 6.0;
    let rows = community.map_or_else(Vec::new, |c| {
        ordered(state, c.id)
            .into_iter()
            .filter(|ch| ch.kind != 2)
            .map(|ch| {
                let (inset, height) = if ch.kind == 1 {
                    (1.0, 30.0)
                } else {
                    (0.0, 28.0)
                };
                let name_top = top + inset + 4.25;
                top += height + 1.0;
                row(
                    ch,
                    model.selected == Some(ch.id),
                    state.unread_of(ch.id),
                    name_top,
                    on_select.clone(),
                )
                .into_any_element()
            })
            .collect()
    });
    div()
        .size_full()
        .flex()
        .flex_col()
        .child(head)
        .child(stats)
        .child(
            div()
                .flex_1()
                .min_h_0()
                .border_t_1()
                .border_color(color::HAIRLINE)
                .pt(px(5.))
                .px(px(8.))
                .flex()
                .flex_col()
                .gap(layout::LIST_GAP)
                .children(rows),
        )
}
