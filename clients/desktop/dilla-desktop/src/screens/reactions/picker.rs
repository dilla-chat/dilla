//! The emoji picker's state: at most one grid is open in the app (a global), either the message's (from the toolbar's
//! `react` or a bar's add button, Conversation.tsx `emojiFor`) or the composer's (ComposerArea.tsx `emojiOpen`).
//!
//! Opening remembers where the focus was: Escape gives it back to the opener; a pick in the composer types the emoji into
//! the field the focus came from (the web inserts at the caret) and a pick on a message toggles that reaction. Either closes
//! the grid.

use std::rc::Rc;

use dilla_client::state::ChannelId;
use dilla_desktop_ui::components::reactions::{EmojiGrid, EmojiItem, GridEvent, Part};
use gpui::{
    App, AppContext as _, Bounds, Entity, FocusHandle, Global, IntoElement as _, Keystroke, Pixels,
    SharedString, Subscription, Window,
};

use super::emoji::{REACTION_EMOJI, name_key};
use crate::slots::SlotCx;
use crate::strings::t;

/// The grid's element id (`${id}-emoji` in the web).
pub const GRID_ID: &str = "emoji-grid";

/// What the open grid is for.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Target {
    /// The composer's picker: a pick is typed into the field.
    Composer,
    /// A message's picker: a pick toggles that reaction on the message with this timeline key.
    Message { key: String },
}

struct Open {
    target: Target,
    grid: Entity<EmojiGrid>,
    /// Where Escape gives the focus back.
    back: Option<FocusHandle>,
    /// Where a composer pick is typed.
    insert: Option<FocusHandle>,
    _subscription: Subscription,
}

#[derive(Default)]
struct Picker {
    open: Option<Open>,
    /// The composer button's bounds, painted each frame (a press on it is its own toggle).
    opener: Option<Bounds<Pixels>>,
}

impl Global for Picker {}

fn picker(cx: &mut App) -> &mut Picker {
    if !cx.has_global::<Picker>() {
        cx.set_global(Picker::default());
    }
    cx.global_mut::<Picker>()
}

/// The open grid when it is for `target`.
pub fn grid_for(target: &Target, cx: &App) -> Option<Entity<EmojiGrid>> {
    cx.try_global::<Picker>()
        .and_then(|p| p.open.as_ref())
        .filter(|o| &o.target == target)
        .map(|o| o.grid.clone())
}

/// Whether a grid is open for `target`.
pub fn is_open(target: &Target, cx: &App) -> bool {
    grid_for(target, cx).is_some()
}

/// Records where the composer's emoji button is, so a press on it does not close the grid it is about to toggle.
pub fn set_opener_bounds(bounds: Bounds<Pixels>, cx: &mut App) {
    let p = picker(cx);
    p.opener = Some(bounds);
    if let Some(open) = &p.open {
        let grid = open.grid.clone();
        let target = open.target.clone();
        if target == Target::Composer {
            grid.update(cx, |g, _| g.set_opener(Some(bounds)));
        }
    }
}

/// The 32 emoji with their names.
pub fn items() -> Vec<EmojiItem> {
    REACTION_EMOJI
        .iter()
        .enumerate()
        .map(|(i, e)| EmojiItem {
            emoji: SharedString::from(*e),
            name: SharedString::from(t(&name_key(i))),
        })
        .collect()
}

/// Opens the grid for `target` (closing another), focuses its search field or first cell, and remembers `back` and
/// `insert`.
pub fn open(
    target: Target,
    ctx: &SlotCx,
    back: Option<FocusHandle>,
    insert: Option<FocusHandle>,
    window: &mut Window,
    cx: &mut App,
) {
    close(window, cx, false);
    let composer = target == Target::Composer;
    let label = SharedString::from(t(if composer {
        "shell.composer.emoji"
    } else {
        "shell.emoji.label"
    }));
    let search = composer.then(|| SharedString::from(t("shell.composer.emojiSearch")));
    let grid = cx.new(|cx| EmojiGrid::new(GRID_ID, label, items(), search, window, cx));
    grid.update(cx, |g, _| {
        g.set_decorate(Rc::new(|part, handle, element| match part {
            Part::Search => {
                crate::tracked_focus("emoji-grid-search", handle, element).into_any_element()
            }
            Part::Cell(i) => super::tag(format!("emoji-cell-{i}"), handle, element),
        }));
    });
    if composer {
        let bounds = picker(cx).opener;
        grid.update(cx, |g, _| g.set_opener(bounds));
    }
    let on_event = {
        let (ctx, target) = (ctx.clone(), target.clone());
        move |_grid: Entity<EmojiGrid>, event: &GridEvent, window: &mut Window, cx: &mut App| {
            match event {
                GridEvent::Pick(emoji) => picked(&target, &ctx, emoji, window, cx),
                GridEvent::Close => close(window, cx, true),
            }
        }
    };
    let subscription = window.subscribe(&grid, cx, on_event);
    picker(cx).open = Some(Open {
        target,
        grid: grid.clone(),
        back,
        insert,
        _subscription: subscription,
    });
    window.defer(cx, move |window, cx| {
        grid.update(cx, |g, cx| g.focus_first(window, cx));
    });
    cx.refresh_windows();
}

/// Closes the open grid; with `restore`, focus goes back to its opener.
pub fn close(window: &mut Window, cx: &mut App, restore: bool) {
    let Some(open) = picker(cx).open.take() else {
        return;
    };
    if restore && let Some(back) = open.back {
        back.focus(window, cx);
    }
    cx.refresh_windows();
}

/// What a pick does.
fn picked(target: &Target, ctx: &SlotCx, emoji: &str, window: &mut Window, cx: &mut App) {
    let open = picker(cx).open.take();
    match target {
        Target::Message { key } => {
            if let Some(back) = open.as_ref().and_then(|o| o.back.clone()) {
                back.focus(window, cx);
            }
            toggle(ctx, key, emoji);
        }
        Target::Composer => {
            let into = open
                .as_ref()
                .and_then(|o| o.insert.clone().or_else(|| o.back.clone()));
            if let Some(handle) = into {
                handle.focus(window, cx);
            }
            type_into_focus(emoji, window, cx);
        }
    }
    cx.refresh_windows();
}

/// Types `text` into the focused field, as the keyboard would.
fn type_into_focus(text: &str, window: &mut Window, cx: &mut App) {
    let stroke = Keystroke {
        modifiers: gpui::Modifiers::default(),
        key: text.to_owned(),
        key_char: Some(text.to_owned()),
    };
    window.dispatch_keystroke(stroke, cx);
}

/// Toggles the reaction `emoji` on the message of timeline key `key`, from the client's current timeline: the web decides
/// `on` from the item as it is when the grid picks (`!item.reactions.some(r => r.emoji === emoji && r.mine)`).
pub fn toggle(ctx: &SlotCx, key: &str, emoji: &str) {
    let (Some(client), Some(channel)) = (ctx.client.as_ref(), ctx.channel) else {
        return;
    };
    toggle_on(client, channel, key, emoji);
}

/// [`toggle`] for a client and channel.
pub fn toggle_on(client: &Rc<dilla_client::Client>, channel: ChannelId, key: &str, emoji: &str) {
    let state = client.state().borrow().clone();
    let Some(item) = state
        .timelines
        .get(&channel)
        .and_then(|t| t.items.iter().find(|i| i.key == key))
    else {
        return;
    };
    let Some(msg) = item.msg_id else { return };
    let on = !item.reactions.iter().any(|r| r.emoji == emoji && r.mine);
    // The answer arrives when the fold is queued; the chip moves when the core folds it. A refusal is the core's.
    drop(client.reactions().react(channel, msg, emoji, on));
}
