//! The sidebar's channel list (packages/ui ChannelList, Signal §5.3): the server's name in the serif italic, the online and
//! voice counts, then the channels in position order, a category's channels under its head. The rows are Task 5's `Row`.

use dilla_client::state::{AppState, Channel, ChannelId, CommunityId};
use gpui::{
    App, ClickEvent, InteractiveElement, IntoElement, ParentElement, SharedString,
    StatefulInteractiveElement, Styled, Window, div, px,
};

use dilla_desktop_ui::components::{Row, RowKind};
use dilla_desktop_ui::text::{Face, line, line_height};
use dilla_desktop_ui::tokens::{color, font, layout, text};
use dilla_desktop_ui::{Icon, IconName};

use crate::screens::{LABEL_TRACKING_WIDE, centred};

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
}

/// What a click on a channel row does.
pub type OnSelect = std::rc::Rc<dyn Fn(ChannelId, &mut Window, &mut App)>;

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
fn row_probe(name: &str) -> &'static str {
    use std::collections::HashMap;
    use std::sync::Mutex;
    static IDS: Mutex<Option<HashMap<String, &'static str>>> = Mutex::new(None);
    let mut ids = IDS.lock().expect("row ids");
    ids.get_or_insert_with(HashMap::new)
        .entry(name.to_owned())
        .or_insert_with(|| Box::leak(format!("channel-{name}").into_boxed_str()))
}

fn row(c: &Channel, active: bool, unread: u32, on_select: OnSelect) -> gpui::AnyElement {
    let id = c.id;
    let row = Row::new(row_id(&c.name), c.name.clone(), kind_of(c))
        .active(active)
        .unread(unread)
        .on_click(move |_: &ClickEvent, window, cx| on_select(id, window, cx));
    let item = div().relative().mt(layout::LIST_GAP).child(row);
    #[cfg(feature = "automation")]
    let item = item.child(crate::automation::bounds_probe(row_probe(&c.name)));
    // A voice channel's item is 30px: its 28px button 1px in (VoiceChannelRow.css).
    if c.kind == 1 {
        item.py(px(1.)).into_any_element()
    } else {
        item.into_any_element()
    }
}

/// A category's head (ChannelList.css `.d-channel-list__cat-head`): the chevron, the name in tracked capitals, a rule.
fn category(c: &Channel) -> gpui::AnyElement {
    div()
        .id(SharedString::from(format!("category-{}", c.name)))
        .mt(px(10.))
        .h(layout::ROW_H_SUB)
        .px(px(8.))
        .flex()
        .items_center()
        .gap(px(6.))
        .child(
            Icon::new(IconName::ChevronDown)
                .size(px(11.))
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
        .child(div().flex_1().min_w(px(8.)).h(px(1.)).bg(color::HAIRLINE))
        .into_any_element()
}

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
            div().flex_1().min_w_0().flex().child(
                line(title, text::TITLE, line_height(text::TITLE, 1.5), color::FG)
                    .face(Face::SerifItalic)
                    .clip(),
            ),
        )
        .children(model.can_add.then(|| {
            centred(
                px(28.),
                Icon::new(IconName::Plus).size(px(15.)).color(color::FG_2),
            )
        }))
        .child(centred(
            px(26.),
            Icon::new(IconName::ChevronDown)
                .size(px(15.))
                .color(color::FG_2),
        ));
    let meta = |s: String| line(s, text::META, line_height(text::META, 1.5), color::FG_3);
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
                .child(meta(format!("{} online", model.online))),
        )
        .child(meta("·".to_owned()))
        .child(meta(format!("{} in voice", model.in_voice)));
    let items = community.map_or_else(Vec::new, |c| {
        ordered(state, c.id)
            .into_iter()
            .map(|ch| {
                if ch.kind == 2 {
                    category(ch)
                } else {
                    row(
                        ch,
                        model.selected == Some(ch.id),
                        state.unread_of(ch.id),
                        on_select.clone(),
                    )
                }
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
                .id("channel-list")
                .flex_1()
                .min_h_0()
                .overflow_y_scroll()
                .border_t_1()
                .border_color(color::HAIRLINE)
                .pt(px(4.))
                .px(px(8.))
                .flex()
                .flex_col()
                .children(items),
        )
}
