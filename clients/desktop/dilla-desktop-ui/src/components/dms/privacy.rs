//! The Privacy section's contact states (packages/web app.css `dw-trust-*`): a state line with a toned 3px left edge
//! (`.dw-trust-state[data-tone]`: `.5rem` in, `--fg` text; warn or danger edge), the compared public key in mono
//! (`.dw-trust-key`, breaking anywhere), and the conversation's removed note (`.dw-trust-removed`, a status line).
//!
//! Every text here goes through the text layer ([`line`], [`flow_paragraph`]), never a bare text child: a bare child
//! names no family, so GPUI drew it in the host's `.SystemUIFont` (its width moved every column; tests/dms.rs
//! `native_trust_screens_do_not_depend_on_system_fonts`), and it reports no line box, so the parity text mask left its
//! glyphs in the chrome gate, where macOS's rasteriser failed the Linux goldens (`privacy_text_is_masked` in tests/dms.rs).

use gpui::{InteractiveElement, IntoElement, ParentElement, SharedString, Styled, div, px};

use crate::text::{flow_paragraph, line, root_line_height};
use crate::tokens::{color, text};

/// `data-tone` of a state line.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum TrustTone {
    /// No tone: the plain label (`d-label`, `--fg-2`).
    Label,
    Warn,
    Danger,
}

/// A state line (`p.dw-trust-state`).
pub fn trust_state(text_: impl Into<SharedString>, tone: TrustTone) -> gpui::Div {
    match tone {
        TrustTone::Label => div().child(line(
            text_,
            text::XS,
            root_line_height(text::XS),
            color::FG_2,
        )),
        TrustTone::Warn | TrustTone::Danger => {
            let edge = if tone == TrustTone::Warn {
                color::WARN
            } else {
                color::DANGER
            };
            div()
                .pl(px(8.0))
                .border_l(px(3.0))
                .border_color(edge)
                .child(flow_paragraph(
                    text_,
                    text::SM,
                    root_line_height(text::SM),
                    color::FG,
                ))
        }
    }
}

/// Plain body text of a contact row (`<p>`), wrapping.
pub fn body_text(text_: impl Into<SharedString>) -> gpui::Div {
    div().child(flow_paragraph(
        text_,
        text::SM,
        root_line_height(text::SM),
        color::FG,
    ))
}

/// The compared public key (`code.dw-trust-key`), 64 hex digits (a line narrower than the key breaks it mid-word).
pub fn trust_key(key: impl Into<SharedString>) -> impl IntoElement {
    div().id("trust-key").child(flow_paragraph(
        key,
        text::XS,
        root_line_height(text::XS),
        color::FG,
    ))
}

/// `p.dw-trust-removed` (role status): sending resumed here after a validated Remove.
pub fn removed_note(text_: impl Into<SharedString>) -> gpui::Div {
    div()
        .w_full()
        .px(px(12.0))
        .py(px(8.0))
        .bg(color::BG_2)
        .border_b_1()
        .border_color(color::HAIRLINE_2)
        .child(body_text(text_))
}
