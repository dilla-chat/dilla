//! The Privacy section's contact states (packages/web app.css `dw-trust-*`): a state line with a toned 3px left edge
//! (`.dw-trust-state[data-tone]`: `.5rem` in, `--fg` text; warn or danger edge), the compared public key in mono
//! (`.dw-trust-key`, breaking anywhere), and the conversation's removed note (`.dw-trust-removed`, a status line).

use gpui::{InteractiveElement, IntoElement, ParentElement, SharedString, Styled, div, px};

use crate::text::{line, line_height};
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
    let lh = line_height(text::SM, 1.5);
    match tone {
        TrustTone::Label => div().child(line(
            text_,
            text::XS,
            line_height(text::XS, 1.5),
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
                .child(
                    div()
                        .text_size(text::SM)
                        .line_height(px(lh))
                        .text_color(color::FG)
                        .child(text_.into()),
                )
        }
    }
}

/// Plain body text of a contact row (`<p>`), wrapping.
pub fn body_text(text_: impl Into<SharedString>) -> gpui::Div {
    div()
        .text_size(text::SM)
        .line_height(px(line_height(text::SM, 1.5)))
        .text_color(color::FG)
        .child(text_.into())
}

/// The compared public key (`code.dw-trust-key`), 64 hex digits that break anywhere.
pub fn trust_key(key: impl Into<SharedString>) -> impl IntoElement {
    div()
        .id("trust-key")
        .text_size(text::XS)
        .line_height(px(line_height(text::XS, 1.5)))
        .text_color(color::FG)
        .child(key.into())
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
