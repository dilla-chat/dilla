//! Measures that belong to one desktop-only or one-off surface and are not web design tokens (so not in the generated
//! `dilla_desktop_ui::tokens`). Screens take them from here, never from a literal (R21).

use gpui::{Pixels, px};

/// The call-code dialog's digits-to-help gap (`.dw-call-code__help { margin: 0.5rem 0 0 }`, packages/web/src/app.css:176).
pub const CALL_CODE_HELP_GAP: Pixels = px(8.0);
