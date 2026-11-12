//! Area **reactions** (Task 10): the reaction bar, the emoji grid and the who-reacted tooltip.
//!
//! Ports: packages/ui/src/{ReactionBar,ReactionTooltip,EmojiGrid}, packages/web/src/emoji.ts and the reaction parts of
//! `screens/conversation/{MessageItem,Conversation,ComposerArea}.tsx`. The hooks below are where `crate::slots` draws this
//! area inside another area's view.
//!
//! - [`reaction_bar`] (`slots::message_extras`, last): the chips under a message, and, while a grid is open for the message,
//!   the grid.
//! - [`message_actions`] (`slots::message_actions`): the toolbar's and context menu's `react`.
//! - [`emoji_button`] (`slots::composer_buttons`): the composer's emoji button; [`emoji_popup`] is its grid, drawn by the
//!   composer's popup slot (`slots::composer_popup`) where the web draws `.d-composer-pop`.
//!
//! The chips' wrapping is laid out here, from the message column's width and the chips' own widths: the timeline adds the
//! height this hook answers to the row's, so the rows below move with it.

pub mod emoji;
pub mod keys;
pub mod picker;

use std::collections::HashSet;
use std::rc::Rc;
use std::sync::Mutex;

use dilla_client::Area;
use dilla_client::state::{AppState, CommunityId, Member, TimelineItem, TimelineItemState, UserId};
use dilla_desktop_ui::components::reactions::{
    Chip, ReactionBar, Slot, bar_height, chip_width, tooltip::tooltip as reaction_tooltip, wrap_lines,
};
use dilla_desktop_ui::components::tooltip::SHOW_DELAY;
use dilla_desktop_ui::tokens::{color, font, layout, radius, text};
use dilla_desktop_ui::{Icon, IconName};
use gpui::{
    AnyElement, App, InteractiveElement, IntoElement, ParentElement, SharedString,
    StatefulInteractiveElement, Styled, TextRun, Window, canvas, deferred, div, px,
};

use self::emoji::{chip_display, reaction_name};
use self::picker::Target;
use crate::slots::{Extra, MessageAction, SlotCx};
use crate::strings::{t, tf};
use crate::{tracked_box, tracked_focus};

/// MessageRow.css: the content's left edge (44px gutter, 10px, 30px avatar, 10px) and the 16px right of it, and the 1px gap
/// of the row's grid between the body and what follows.
const CONTENT_X: f32 = 44.0 + 10.0 + 30.0 + 10.0;
const ROW_RIGHT: f32 = 16.0;
const MAIN_GAP: f32 = 1.0;
/// Composer.css `.d-composer__tool`: 26 x 24 with a 14px glyph (`Icon size={14}`).
const TOOL_WIDTH: f32 = 26.0;
const TOOL_HEIGHT: f32 = 24.0;
const TOOL_GLYPH: f32 = 14.0;
/// The grid paints above the rows after it (the log's later rows), as the web's `z-index` does.
const GRID_PRIORITY: usize = 6;
/// The composer's pop (`.d-composer-pop > *`): 16px from the left, 6px above the composer.
const POP_LEFT: f32 = 16.0;
const POP_BOTTOM: f32 = 6.0;

/// An automation id that lives as long as the app: the interface keys elements by `&'static str`, and a message's chips
/// are as many as its reactions.
pub(super) fn leak_id(id: String) -> &'static str {
    static IDS: Mutex<Option<HashSet<&'static str>>> = Mutex::new(None);
    let mut guard = IDS.lock().expect("id registry");
    let set = guard.get_or_insert_with(HashSet::new);
    if let Some(known) = set.get(id.as_str()) {
        return known;
    }
    let leaked: &'static str = Box::leak(id.into_boxed_str());
    set.insert(leaked);
    leaked
}

/// The width of `display` at the chip's emoji size, as the text system shapes it (the emoji comes from the system's
/// fallback face, so no table of widths can hold it).
fn emoji_width(window: &Window, display: &str) -> f32 {
    let run = TextRun {
        len: display.len(),
        font: gpui::font(font::MONO),
        color: color::FG,
        background_color: None,
        underline: None,
        strikethrough: None,
    };
    let shaped = window.text_system().shape_line(
        SharedString::from(display.to_owned()),
        text::BTN,
        &[run],
        None,
    );
    f32::from(shaped.width)
}

/// How wide the message column is: the window less the rail and the sidebar (what the timeline's log is) less the row's
/// gutter columns and right padding.
fn column_width(window: &Window) -> f32 {
    f32::from(window.viewport_size().width - layout::RAIL - layout::SIDEBAR) - CONTENT_X - ROW_RIGHT
}

fn display_name(members: &[Member], user: UserId) -> Option<&str> {
    members.iter().find(|m| m.user_id == user).map(|m| {
        if m.display.is_empty() {
            m.username.as_str()
        } else {
            m.display.as_str()
        }
    })
}

fn members_of<'a>(state: &'a AppState, community: Option<CommunityId>) -> &'a [Member] {
    community
        .and_then(|c| state.members.get(&c))
        .map_or(&[], Vec::as_slice)
}

/// The tooltip over a chip: its emoji, the sentence `{name}, {count}` (the web's only reaction sentence) and the faces of
/// the reactors the client knows by name.
fn chip_tooltip(
    item: &TimelineItem,
    chip: &Chip,
    ctx: &SlotCx,
) -> impl Fn(&mut Window, &mut App) -> gpui::AnyView + 'static {
    let who = ctx.state.reactions.who(&item.key, &chip.emoji);
    let members = members_of(&ctx.state, ctx.community);
    let names: Vec<SharedString> = who
        .iter()
        .filter_map(|u| display_name(members, *u))
        .map(|n| SharedString::from(n.to_owned()))
        .collect();
    reaction_tooltip(chip.display.clone(), chip.name.clone(), names)
}

/// The reaction bar under a message, with its height, and the open grid of this message.
pub fn reaction_bar(
    item: &TimelineItem,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> Option<Extra> {
    let grid = picker::grid_for(&Target::Message { key: item.key.clone() }, cx);
    if item.reactions.is_empty() && grid.is_none() {
        return None;
    }
    let chips: Vec<Chip> = item
        .reactions
        .iter()
        .map(|r| Chip {
            display: chip_display(&r.emoji).into(),
            count: r.count,
            mine: r.mine,
            name: tf(
                "shell.message.reaction",
                &[
                    ("name", reaction_name(&r.emoji).as_str()),
                    ("count", r.count.to_string().as_str()),
                ],
            )
            .into(),
            emoji: r.emoji.clone().into(),
        })
        .collect();

    // The height: the chips and the add button wrapped in the column, as `flex-wrap` does, under the row's 1px grid gap.
    let width = column_width(window);
    let mut widths: Vec<f32> = chips
        .iter()
        .map(|c| chip_width(emoji_width(window, &c.display), c.count))
        .collect();
    if !chips.is_empty() {
        widths.push(dilla_desktop_ui::components::reactions::bar::ADD_WIDTH);
    }
    let lines = wrap_lines(&widths, width);
    let height = if chips.is_empty() {
        0.0
    } else {
        MAIN_GAP + bar_height(lines)
    };

    let key = item.key.clone();
    let mut column = div().relative().w(px(width.max(0.0)));
    if !chips.is_empty() {
        let tooltips: Vec<_> = chips
            .iter()
            .map(|c| chip_tooltip(item, c, ctx))
            .collect();
        let tooltips = Rc::new(std::cell::RefCell::new(
            tooltips.into_iter().map(Some).collect::<Vec<_>>(),
        ));
        let (toggle_ctx, add_ctx) = (ctx.clone(), ctx.clone());
        let (toggle_key, add_key) = (key.clone(), key.clone());
        let id_key = key.clone();
        let bar = ReactionBar::new(
            SharedString::from(format!("reactions-{key}")),
            t("shell.message.reactions"),
            t("shell.message.reactionAdd"),
            chips,
        )
        .on_toggle(move |emoji, _window, _cx| picker::toggle(&toggle_ctx, &toggle_key, emoji))
        .on_add(move |window, cx| {
            let back = window.focused(cx);
            picker::open(
                Target::Message {
                    key: add_key.clone(),
                },
                &add_ctx,
                back,
                None,
                window,
                cx,
            );
        })
        .decorate(Rc::new(move |slot, handle, element| match slot {
            Slot::Chip(i) => {
                let tip = tooltips.borrow_mut().get_mut(i).and_then(Option::take);
                let tracked =
                    tracked_focus(leak_id(format!("reaction-{id_key}-{i}")), handle, element);
                let wrapper = div().id(SharedString::from(format!("reaction-tip-{id_key}-{i}")));
                match tip {
                    Some(tip) => wrapper
                        .tooltip(tip)
                        .tooltip_show_delay(SHOW_DELAY)
                        .child(tracked)
                        .into_any_element(),
                    None => wrapper.child(tracked).into_any_element(),
                }
            }
            Slot::Add => tracked_focus(leak_id(format!("reaction-{id_key}-add")), handle, element)
                .into_any_element(),
        }));
        column = column.child(div().pt(px(MAIN_GAP)).child(bar));
    }
    if let Some(grid) = grid {
        column = column.child(deferred(div().absolute().top_0().left_0().child(grid)).with_priority(GRID_PRIORITY));
    }
    Some(Extra {
        element: column.into_any_element(),
        height,
    })
}

/// The toolbar's and the context menu's `react`: it opens the message's grid.
pub fn message_actions(item: &TimelineItem) -> Vec<MessageAction> {
    if item.state != TimelineItemState::Ok || item.msg_id.is_none() {
        return Vec::new();
    }
    let key = item.key.clone();
    vec![MessageAction {
        id: "react".into(),
        label: "shell.message.react",
        icon: IconName::SmilePlus,
        area: Area::Reactions,
        run: Rc::new(move |ctx, window, cx| {
            let back = window.focused(cx);
            picker::open(Target::Message { key: key.clone() }, ctx, back, None, window, cx);
        }),
    }]
}

/// The composer's emoji button: 26 x 24, the smile, accent while its grid is open (`aria-expanded`).
pub fn emoji_button(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let handle = window
        .use_keyed_state("composer-emoji-focus", cx, |_, cx| cx.focus_handle())
        .read(cx)
        .clone();
    let from = window.use_keyed_state("composer-emoji-from", cx, |_, _| None::<gpui::FocusHandle>);
    let open = picker::is_open(&Target::Composer, cx);
    let (fg, bg) = if open {
        (color::ACCENT, color::ACCENT_SOFT)
    } else {
        (color::FG_3, gpui::transparent_black())
    };
    let (toggle_ctx, toggle_handle, toggle_from) = (ctx.clone(), handle.clone(), from.clone());
    let remember = from.clone();
    let button = div()
        .id("composer-emoji-button")
        .track_focus(&handle)
        .tab_stop(true)
        .flex()
        .flex_none()
        .items_center()
        .justify_center()
        .w(px(TOOL_WIDTH))
        .h(px(TOOL_HEIGHT))
        .rounded(radius::SM)
        .cursor_pointer()
        .bg(bg)
        .hover(|s| s.bg(color::SURFACE_HI))
        .focus_visible(|s| s.shadow(dilla_desktop_ui::metrics::focus_ring()))
        .child(Icon::new(IconName::Smile).size(px(TOOL_GLYPH)).color(fg))
        .child(
            canvas(
                |bounds, _window, cx| picker::set_opener_bounds(bounds, cx),
                |_, (), _, _| {},
            )
            .absolute()
            .size_full(),
        )
        // The press moves the focus onto the button; the field the person came from is where the emoji is typed.
        .capture_any_mouse_down(move |_, window, cx| {
            let focused = window.focused(cx);
            remember.update(cx, |from, _| *from = focused);
        })
        .on_click(move |_, window, cx| {
            if picker::is_open(&Target::Composer, cx) {
                picker::close(window, cx, true);
                return;
            }
            let came_from = toggle_from
                .update(cx, |from, _| from.take())
                .filter(|h| h != &toggle_handle);
            picker::open(
                Target::Composer,
                &toggle_ctx,
                Some(toggle_handle.clone()),
                came_from,
                window,
                cx,
            );
        });
    Some(tracked_focus("composer-emoji", &handle, button).into_any_element())
}

/// The composer's grid, where the web draws `.d-composer-pop`: 16px from the composer's left, 6px above it.
pub fn emoji_popup(_ctx: &SlotCx, _window: &mut Window, cx: &mut App) -> Option<AnyElement> {
    let grid = picker::grid_for(&Target::Composer, cx)?;
    Some(
        div()
            .ml(px(POP_LEFT))
            .mb(px(POP_BOTTOM))
            .child(tracked_box("emoji-grid", grid))
            .into_any_element(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_client::state::TimelineItemState;

    fn item(reactions: &[(&str, u32, bool)]) -> TimelineItem {
        TimelineItem {
            key: "s1".into(),
            state: TimelineItemState::Ok,
            reason: String::new(),
            sender_user: None,
            sender_device: None,
            own: false,
            bot: false,
            ts: 0,
            body: "hi".into(),
            msg_id: Some(dilla_client::state::MessageId([1; 16])),
            seq: Some(1),
            edited: false,
            reply: None,
            reactions: reactions
                .iter()
                .map(|(e, n, m)| dilla_client::state::messages::Reaction {
                    emoji: (*e).to_owned(),
                    count: *n,
                    mine: *m,
                })
                .collect(),
            pinned: false,
            attachments: Vec::new(),
            mention: false,
        }
    }

    #[test]
    fn react_is_offered_on_a_stored_readable_message_only() {
        assert_eq!(message_actions(&item(&[])).len(), 1);
        assert_eq!(message_actions(&item(&[]))[0].id, "react");
        let mut pending = item(&[]);
        pending.msg_id = None;
        assert!(message_actions(&pending).is_empty());
        let mut deleted = item(&[]);
        deleted.state = TimelineItemState::Deleted;
        assert!(message_actions(&deleted).is_empty());
    }

    #[test]
    fn ids_are_interned() {
        let a = leak_id("reaction-s1-0".to_owned());
        let b = leak_id("reaction-s1-0".to_owned());
        assert!(std::ptr::eq(a, b));
    }
}
