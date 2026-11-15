//! The message toolbar (packages/ui `MessageToolbar`, MessageToolbar.css): 2px padding, 1px gaps, `--surface-2` in the
//! `--line-strong` edge, 6px corners. React, reply and thread are icon buttons; every other action (edit, pin, delete) sits
//! in the `⋯` menu. The entries come from [`crate::slots::message_actions`]. It floats at the row's top −12px, 16px from
//! its right, and shows while the row is hovered or the menu is open.

use dilla_client::state::TimelineItem;
use gpui::{
    AnyElement, App, ElementId, InteractiveElement as _, IntoElement, ParentElement,
    StatefulInteractiveElement as _, Styled, Window, div, px,
};

use dilla_desktop_ui::Icon;
use dilla_desktop_ui::IconName;
use dilla_desktop_ui::components::action_menu::{ActionMenu, MenuItem};
use dilla_desktop_ui::tokens::{color, radius};

use super::ui::{self, MessageUi};
use crate::screens::messages::css::len;
use crate::slots::{self, MessageAction, SlotCx};
use crate::strings::t;

/// The ids shown as icon buttons; the rest go into the menu.
const DIRECT: [&str; 3] = ["react", "reply", "thread"];

/// A button's width: a 14px icon between 6px of padding.
const BUTTON_W: f32 = 26.0;
const BUTTON_H: f32 = 24.0;

fn is_direct(a: &MessageAction) -> bool {
    DIRECT.contains(&a.id.as_ref())
}

/// MessageToolbar.tsx `ICON`: the toolbar's own glyphs (the menus keep each action's icon).
fn direct_icon(a: &MessageAction) -> IconName {
    match a.id.as_ref() {
        "react" => IconName::Smile,
        "reply" => IconName::Reply,
        "thread" => IconName::MessagesSquare,
        _ => a.icon,
    }
}

/// The icon buttons in MessageToolbar.tsx's order (react, reply, thread), whatever order the areas offer them in.
fn direct_of(actions: &[MessageAction]) -> Vec<&MessageAction> {
    let mut direct: Vec<&MessageAction> = actions.iter().filter(|a| is_direct(a)).collect();
    direct.sort_by_key(|a| DIRECT.iter().position(|d| *d == a.id.as_ref()));
    direct
}

/// The toolbar's width for `buttons` buttons: 2px padding and a 1px edge each side, 1px between.
pub fn width(buttons: usize) -> f32 {
    buttons as f32 * BUTTON_W + buttons.saturating_sub(1) as f32 + 2.0 * (2.0 + 1.0)
}

/// The toolbar for `item`, `None` when no area offers an action. `group` is the row's hover group.
pub fn toolbar(
    item: &TimelineItem,
    index: usize,
    group: &str,
    ctx: &SlotCx,
    ui: &gpui::Entity<MessageUi>,
    window: &mut Window,
    cx: &mut App,
) -> Option<AnyElement> {
    let _ = window;
    let actions = slots::message_actions(item, ctx);
    if actions.is_empty() {
        return None;
    }
    let menu_open = ui.read(cx).menu.as_deref() == Some(item.key.as_str());
    let direct = direct_of(&actions);
    let more: Vec<&MessageAction> = actions.iter().filter(|a| !is_direct(a)).collect();
    let mut bar = div()
        .flex()
        .gap(len(1.))
        .p(len(2.))
        .bg(color::SURFACE_2)
        .border_1()
        .border_color(color::LINE_STRONG)
        .rounded(radius::MD)
        // MessageToolbar.css: `box-shadow: var(--shadow-1)`.
        .shadow(dilla_desktop_ui::metrics::shadow_1());
    for a in &direct {
        let run = a.run.clone();
        let ctx = ctx.clone();
        bar = bar.child(
            div()
                .id(ElementId::Name(
                    format!("message-action-{}-{index}", a.id).into(),
                ))
                .w(px(BUTTON_W))
                .h(px(BUTTON_H))
                .flex()
                .flex_none()
                .items_center()
                .justify_center()
                .rounded(radius::SM)
                .cursor_pointer()
                .hover(|s| s.bg(color::HOVER_HI))
                .child(Icon::new(direct_icon(a)).size(len(14.)).color(color::FG_2))
                .on_click(move |_, window, cx| run(&ctx, window, cx)),
        );
    }
    if !more.is_empty() {
        let items: Vec<MenuItem> = more
            .iter()
            .map(|a| {
                let item = MenuItem::new(a.id.clone(), t(a.label));
                if a.id == "delete" || a.id == super::actions::MODERATE_REMOVE {
                    item.danger()
                } else {
                    item
                }
            })
            .collect();
        let runs: Vec<(gpui::SharedString, crate::slots::OnAction)> =
            more.iter().map(|a| (a.id.clone(), a.run.clone())).collect();
        let key = item.key.clone();
        let key_open = item.key.clone();
        let ui_open = ui.clone();
        let ui_pick = ui.clone();
        let ctx = ctx.clone();
        // The menu's trigger reports its bounds (`message-more-<n>`) so a fixture can open it, as it hovers a row.
        bar = bar.child(
            div()
                .relative()
                .flex()
                .flex_none()
                .child(super::probe(super::id_of("message-more", index)))
                .child(
                    ActionMenu::new(
                        ElementId::Name(format!("message-more-{index}").into()),
                        t("shell.message.more"),
                        items,
                    )
                    .icon(IconName::Ellipsis)
                    .icon_size(len(14.))
                    .open(menu_open)
                    .on_open_change(move |open, _, cx| {
                        let key = open.then(|| key_open.clone());
                        ui_open.update(cx, |ui, cx| ui.set_menu(key, cx));
                    })
                    .on_select(move |id, window, cx| {
                        ui_pick.update(cx, |ui, cx| ui.set_menu(None, cx));
                        if let Some((_, run)) = runs.iter().find(|(rid, _)| rid.as_ref() == id) {
                            run(&ctx, window, cx);
                        }
                        let _ = &key;
                    }),
                ),
        );
    }
    // Shown while the row is hovered (or the menu is open); hidden otherwise.
    let shown = div()
        .absolute()
        .top(len(-12.))
        .right(len(16.))
        .child(bar)
        .child(super::probe(super::id_of("message-toolbar", index)));
    let _ = ui::handle;
    Some(if menu_open {
        shown.into_any_element()
    } else {
        shown
            .invisible()
            .group_hover(group.to_owned(), |s| s.visible())
            .into_any_element()
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_bar_is_its_buttons_gaps_padding_and_edge() {
        assert_eq!(width(1), 26.0 + 6.0);
        assert_eq!(width(4), 4.0 * 26.0 + 3.0 + 6.0);
    }

    fn action(id: &'static str) -> MessageAction {
        MessageAction {
            id: id.into(),
            label: "shell.message.react",
            icon: IconName::SmilePlus,
            area: dilla_client::Area::Reactions,
            run: std::rc::Rc::new(|_, _, _| {}),
        }
    }

    #[test]
    fn the_buttons_are_react_reply_thread_in_the_webs_order_and_glyphs() {
        // The slots offer them messages, threads, reactions: reply, thread, then react.
        let offered = [
            action("reply"),
            action("thread"),
            action("edit"),
            action("react"),
        ];
        let direct = direct_of(&offered);
        let ids: Vec<&str> = direct.iter().map(|a| a.id.as_ref()).collect();
        assert_eq!(ids, ["react", "reply", "thread"]);
        let icons: Vec<IconName> = direct.iter().map(|a| direct_icon(a)).collect();
        assert_eq!(
            icons,
            [IconName::Smile, IconName::Reply, IconName::MessagesSquare]
        );
    }

    #[test]
    fn react_reply_and_thread_are_the_icon_buttons() {
        assert_eq!(DIRECT, ["react", "reply", "thread"]);
    }
}
