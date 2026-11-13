//! The create and join dialogs' form measures (CreateServer.css, CreateChannel.css, TextField.css), so the screens draw
//! them from here and hold no literal of their own.

use gpui::{Pixels, px};

use crate::components::dialog::DialogKind;

/// A form dialog's body padding (Dialog.css `__body`: 16px).
pub const BODY_PAD: f32 = 16.0;

/// `.d-create-server { gap: 0.75rem }`.
pub const SERVER_GAP: Pixels = px(12.0);
/// `.d-create-channel { gap: 0.875rem }`.
pub const CHANNEL_GAP: Pixels = px(14.0);
/// `.d-create-channel__kind { gap: 0.375rem }`, the label over the kind group.
pub const LABEL_GAP: Pixels = px(6.0);

/// The width of a form dialog's body text at `viewport_width`.
pub fn body_width(viewport_width: f32) -> f32 {
    DialogKind::Form.width(viewport_width) - 2.0 * BODY_PAD - 2.0
}
