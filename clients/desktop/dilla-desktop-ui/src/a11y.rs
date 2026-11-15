//! Screen-reader semantics (Package 11): the one place a component or screen gets its role, name, states, landmark,
//! modality and live announcements.
//!
//! GPUI builds an AccessKit tree each frame from the elements that have both an `.id(..)` and a role, and hands it to
//! the platform adapter: AT-SPI on Linux (Orca), NSAccessibility on macOS (VoiceOver), UI Automation on Windows (NVDA,
//! Narrator). An element without a role is not in the tree; text a component paints is not either, so every control
//! carries its name as a label ([`Accessible::a11y`]). Focus follows GPUI's own: an element with a focus handle, an id
//! and a role is the focused node while its handle holds the focus.
//!
//! Rules (the tree lint, `dilla-parity`'s `a11y` module, checks them per screen):
//! - every focusable or clickable element has a role and a non-empty name;
//! - ids are stable across frames (a list's rows take the row's own key, never an index that shifts);
//! - a dialog is [`Accessible::modal_dialog`]: modal and named;
//! - nothing secret goes into a name or value: the recovery key, a TOTP secret and a typed password are never labels
//!   (the web hid these from axe too).
//!
//! What the web announced through `aria-live` (a joined or left call, a ring, "Copied", a permission change, a new
//! message in the open channel) goes through [`announce`]: each window draws one [`announcer`] region, and every
//! announcement is a new node in it, which each adapter reads out once (AT-SPI `Announcement`, NSAccessibility
//! announcement, UIA `LiveRegionChanged`).

use std::collections::HashMap;

use gpui::{
    App, Global, InteractiveElement, IntoElement, ParentElement, SharedString,
    StatefulInteractiveElement, Styled, Window, WindowId, div, px,
};

pub use gpui::accesskit::{Live, Node};
pub use gpui::{Role, Toggled};

/// The role-and-name builder every component and screen uses.
pub trait Accessible: StatefulInteractiveElement + Sized {
    /// A role and its accessible name (what the reader says for the element).
    fn a11y(self, role: Role, name: impl Into<SharedString>) -> Self {
        self.role(role).aria_label(name)
    }

    /// A role with no name of its own (the window's one `main`, an `aside`, a list a heading names).
    fn a11y_unnamed(self, role: Role) -> Self {
        self.role(role)
    }

    /// A landmark the reader can jump to (`<nav>`, `<main>`, `<aside>`, `role=region` on the web).
    fn landmark(self, role: Role, name: impl Into<SharedString>) -> Self {
        self.a11y(role, name)
    }

    /// A named group (`role=group` / `fieldset`).
    fn a11y_group(self, name: impl Into<SharedString>) -> Self {
        self.a11y(Role::Group, name)
    }

    /// What a screen reader's "activate" does on an element that takes the keyboard but leaves the pointer's click to a
    /// child (a roving list's focus wrapper around its row). An element with `on_click` gets this from GPUI already.
    fn a11y_activate(self, f: impl Fn(&mut Window, &mut App) + 'static) -> Self {
        self.on_a11y_action(gpui::AccessibleAction::Click, move |_, window, cx| {
            f(window, cx)
        })
    }

    /// A toggle's state (`aria-pressed` / `aria-checked`).
    fn toggled(self, on: bool) -> Self {
        self.aria_toggled(if on { Toggled::True } else { Toggled::False })
    }

    /// AccessKit properties GPUI has no builder for (modality, live politeness), set on this element's node. One per
    /// element: it takes GPUI's synthetic-children slot, so a second call replaces the first.
    fn node_props(self, f: impl FnOnce(&mut Node) + 'static) -> Self {
        self.a11y_synthetic_children(move |builder| f(builder.parent_node()))
    }

    /// A modal dialog named by its title (`role=dialog aria-modal=true aria-labelledby=title`).
    fn modal_dialog(self, name: impl Into<SharedString>) -> Self {
        self.a11y(Role::Dialog, name).node_props(|n| n.set_modal())
    }

    /// An alert dialog (a confirm that asks before something destructive), modal.
    fn modal_alert(self, name: impl Into<SharedString>) -> Self {
        self.a11y(Role::AlertDialog, name)
            .node_props(|n| n.set_modal())
    }

    /// A live region: changes to its name are read out without moving the focus.
    fn live(self, politeness: Live) -> Self {
        self.node_props(move |n| n.set_live(politeness))
    }
}

impl<T: StatefulInteractiveElement + Sized> Accessible for T {}

/// Text the web exposed as an element of its own (an `<h2>`, a hint `<p>`): `child` (the painted text) wrapped in a node
/// with `role` and `text` as its name, so a reader's own cursor and heading navigation find it. Its id is the text: one
/// heading of a given text per container.
fn text_node(
    role: Role,
    text: SharedString,
    level: Option<usize>,
    child: impl IntoElement,
) -> gpui::Stateful<gpui::Div> {
    let node = div()
        .id(gpui::ElementId::Name(
            format!("a11y-{role:?}-{text}").into(),
        ))
        .a11y(role, text);
    let node = match level {
        Some(level) => node.aria_level(level),
        None => node,
    };
    node.child(child)
}

/// A heading (`<h1>`…`<h3>`): `child` is the painted title.
pub fn heading(
    level: usize,
    text: impl Into<SharedString>,
    child: impl IntoElement,
) -> gpui::Stateful<gpui::Div> {
    text_node(Role::Heading, text.into(), Some(level), child)
}

/// A paragraph a reader reads (a hint, a section's intro, an empty state's body): `child` is the painted text.
pub fn paragraph(
    text: impl Into<SharedString>,
    child: impl IntoElement,
) -> gpui::Stateful<gpui::Div> {
    text_node(Role::Paragraph, text.into(), None, child)
}

/// The announcements of each window: the latest one and a counter that makes each a new node.
#[derive(Default)]
struct Announcements {
    by_window: HashMap<WindowId, (u64, SharedString, Live)>,
    seq: u64,
}

impl Global for Announcements {}

/// Reads `text` out in `window` (`aria-live="polite"`), without moving the focus.
pub fn announce(text: impl Into<SharedString>, window: &mut Window, cx: &mut App) {
    announce_with(text, Live::Polite, window, cx);
}

/// Reads `text` out at once, interrupting (`aria-live="assertive"`): an incoming ring.
pub fn announce_now(text: impl Into<SharedString>, window: &mut Window, cx: &mut App) {
    announce_with(text, Live::Assertive, window, cx);
}

fn announce_with(text: impl Into<SharedString>, live: Live, window: &mut Window, cx: &mut App) {
    let id = window.window_handle().window_id();
    let state = cx.default_global::<Announcements>();
    state.seq += 1;
    let seq = state.seq;
    state.by_window.insert(id, (seq, text.into(), live));
    window.refresh();
}

/// The latest announcement in `window`, for tests: its text.
pub fn last_announcement(window: &Window, cx: &App) -> Option<SharedString> {
    let id = window.window_handle().window_id();
    cx.try_global::<Announcements>()
        .and_then(|a| a.by_window.get(&id))
        .map(|(_, text, _)| text.clone())
}

/// The window's live region: one invisible node holding the latest announcement. Each window's root view draws it
/// once. The node's id changes with every announcement, so the adapter sees a new live node and reads it even when the
/// text repeats ("Copied" twice).
pub fn announcer(window: &Window, cx: &App) -> impl IntoElement {
    let id = window.window_handle().window_id();
    let latest = cx
        .try_global::<Announcements>()
        .and_then(|a| a.by_window.get(&id))
        .cloned();
    let region = div()
        .id("a11y-announcer")
        .absolute()
        .top_0()
        .left_0()
        .size(px(1.))
        .overflow_hidden();
    match latest {
        Some((seq, text, live)) => region.child(
            div()
                .id(("a11y-announcement", seq))
                .size(px(1.))
                .role(Role::Status)
                .aria_label(text.clone())
                .aria_value(text)
                .live(live),
        ),
        None => region,
    }
}
