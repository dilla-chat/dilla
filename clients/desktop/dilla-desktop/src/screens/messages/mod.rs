//! Area **messages** (Task 9): message row extras' host, markdown, code blocks, mentions, edit and delete, pins, the
//! composer's own parts.
//!
//! Every file under `screens/messages/` is Task 9's. The hooks below are where `crate::slots` draws this area inside
//! another area's view.

pub mod actions;
pub mod body;
pub mod cache;
pub mod composing;
pub mod dialogs;
pub mod editor;
pub mod highlight;
pub mod keys;
pub mod markdown;
pub mod mentions;
pub mod slow;
pub mod toolbar;
pub mod typing;
pub mod ui;

use crate::slots::MessageAction;
use dilla_client::state::TimelineItem;
use gpui::{AnyElement, App, Window};

pub use ui::open_pins;

/// Where gpui-component's textarea draws its first glyph inside its own box: 10px right of and 8px below the box's
/// corner. A field that CSS pads by its own amount moves the textarea back by this much, so the text lands where the
/// CSS puts it (the placeholder, drawn by the field, already does).
pub const TEXTAREA_PAD_X: f32 = 10.0;
pub const TEXTAREA_PAD_Y: f32 = 8.0;

/// `child` (a textarea) shifted back by the component's own padding.
pub fn textarea_origin(child: impl gpui::IntoElement) -> gpui::Div {
    use gpui::{ParentElement, Styled};
    gpui::div()
        .relative()
        .left(gpui::px(-TEXTAREA_PAD_X))
        .top(gpui::px(-TEXTAREA_PAD_Y))
        .child(child)
}

/// The stylesheet's own lengths. Every length this area draws is written in the CSS it ports (MessageRow.css,
/// Composer.css, CodeBlock.css, MentionList.css, MessageToolbar.css, MessageEditor.css, PinsDialog.css), so it is a named
/// CSS value read from the stylesheet, never a window coordinate measured from a screenshot (R21).
pub mod css {
    use gpui::{Pixels, px};

    /// A length of the web stylesheets, in their px.
    #[must_use]
    pub fn len(css_px: f32) -> Pixels {
        px(css_px)
    }
}

/// A `'static` id for a numbered element (`message-row-3`): the automation interface looks ids up by name, so the few
/// that are numbered are kept once. At most one per row of the open log.
pub fn id_of(prefix: &str, n: usize) -> &'static str {
    use std::collections::HashMap;
    use std::sync::{Mutex, OnceLock};
    static IDS: OnceLock<Mutex<HashMap<String, &'static str>>> = OnceLock::new();
    let key = format!("{prefix}-{n}");
    let mut ids = IDS
        .get_or_init(Mutex::default)
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner);
    ids.entry(key.clone())
        .or_insert_with(|| Box::leak(key.into_boxed_str()))
}

/// An invisible overlay that reports its parent's bounds under `id` in an automation build; nothing otherwise.
#[cfg(feature = "automation")]
pub fn probe(id: &'static str) -> gpui::AnyElement {
    gpui::IntoElement::into_any_element(crate::automation::bounds_probe(id))
}

#[cfg(not(feature = "automation"))]
pub fn probe(_id: &'static str) -> gpui::AnyElement {
    gpui::IntoElement::into_any_element(gpui::div())
}

/// This area's entries in the message toolbar and context menu (`slots::message_actions`).
pub fn message_actions(item: &TimelineItem) -> Vec<MessageAction> {
    actions::message_actions(item)
}

/// The delete confirmation and the pins dialog, over the whole window. `slots::overlays` adds them to the shell's other
/// overlays.
pub fn overlays(window: &mut Window, cx: &mut App) -> Vec<AnyElement> {
    dialogs::overlays(window, cx)
}
