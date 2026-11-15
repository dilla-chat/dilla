//! "Read aloud" for a secret the screen shows (Package 11, Jonas 2026-11-15: "readable on request"): the recovery key
//! and an authenticator's setup key are never in the accessibility tree; a press of this control reads the secret out
//! once (`a11y::announce_once`), in groups of four, with the characters that look alike spelled ("zero", "capital O").
//!
//! The control is a Tab stop named for a screen reader and drawn only while it has the keyboard's focus (it lies over
//! the corner of what it reads, so the screen keeps its layout and the Signal design).

use std::rc::Rc;

use dilla_desktop_ui::a11y::{Accessible, Role, announce_once};
use dilla_desktop_ui::metrics::focus_ring;
use dilla_desktop_ui::text::{line, line_height};
use dilla_desktop_ui::tokens::{color, radius, text};
use gpui::{
    App, InteractiveElement, IntoElement, ParentElement, StatefulInteractiveElement, Styled,
    Window, div,
};

use crate::strings::{t, tf};

/// The control's key context: Enter on it is [`Read`], deeper than a frame's own Enter binding.
pub const CONTEXT: &str = "ReadAloud";

gpui::actions!(read_aloud, [Read]);

/// Registers Enter on the control (`crate::keys::bind_all`).
pub fn bind(cx: &mut App) {
    cx.bind_keys([gpui::KeyBinding::new("enter", Read, Some(CONTEXT))]);
}

/// Characters a listener confuses: always spelled.
fn spelled(c: char, mixed_case: bool) -> String {
    match c {
        '0' => t("readAloud.zero").to_owned(),
        '1' => t("readAloud.one").to_owned(),
        'O' | 'I' => tf("readAloud.capital", &[("letter", &c.to_string())]),
        'o' | 'l' => tf("readAloud.lower", &[("letter", &c.to_string())]),
        c if mixed_case && c.is_ascii_uppercase() => {
            tf("readAloud.capital", &[("letter", &c.to_string())])
        }
        c if mixed_case && c.is_ascii_lowercase() => {
            tf("readAloud.lower", &[("letter", &c.to_string())])
        }
        c => c.to_string(),
    }
}

/// What a reader says for `secret`, named `name`: that it comes in groups of four, then each group of four with its characters
/// one by one ("Group 1: A, 7, capital O, 3."). Hyphens and spaces in `secret` are only its grouping and are dropped.
pub fn spoken(name: &str, secret: &str) -> String {
    let chars: Vec<char> = secret.chars().filter(|c| !matches!(c, '-' | ' ')).collect();
    let mixed_case =
        chars.iter().any(char::is_ascii_uppercase) && chars.iter().any(char::is_ascii_lowercase);
    let groups: Vec<String> = chars
        .chunks(4)
        .enumerate()
        .map(|(i, g)| {
            let spelled: Vec<String> = g.iter().map(|c| spelled(*c, mixed_case)).collect();
            tf(
                "readAloud.group",
                &[("n", &(i + 1).to_string()), ("chars", &spelled.join(", "))],
            )
        })
        .collect();
    let mut out = tf("readAloud.intro", &[("name", name)]);
    for g in groups {
        out.push(' ');
        out.push_str(&g);
    }
    out
}

/// The control: `label` names it for a screen reader (`readAloud.recoveryKey`); a press reads out `text()` once.
/// Absolutely placed at the top right of its parent (which must be `relative`).
pub fn button(
    id: &'static str,
    label: &'static str,
    text_of: impl Fn() -> String + 'static,
) -> impl IntoElement {
    let text_of = Rc::new(text_of);
    div()
        .id(id)
        .a11y(Role::Button, t(label))
        .tab_index(0)
        .absolute()
        .top_0()
        .right_0()
        .px_2()
        .rounded(radius::MD)
        .bg(color::SURFACE_2)
        .border_1()
        .border_color(color::CONTROL_EDGE)
        .cursor_pointer()
        // Drawn only while it holds the keyboard's focus: a pointer user has the key on screen.
        .opacity(0.0)
        .focus_visible(|s| s.opacity(1.0).shadow(focus_ring()))
        .child(line(
            t("readAloud.label"),
            text::XS,
            line_height(text::XS, 1.5),
            color::FG_2,
        ))
        // Enter reads it too, before a step's own Enter (the onboarding's Submit) hears it.
        .key_context(CONTEXT)
        .on_action({
            let text_of = text_of.clone();
            move |_: &Read, window, cx| announce_once(text_of(), window, cx)
        })
        .on_click(move |_, window: &mut Window, cx: &mut App| {
            announce_once(text_of(), window, cx);
        })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_key_is_read_as_groups_with_the_look_alikes_spelled() {
        assert_eq!(
            spoken("Recovery key", "A7O3-10BX"),
            "Recovery key, in groups of four. Group 1: A, 7, capital O, 3. Group 2: one, zero, B, X."
        );
    }

    #[test]
    fn a_mixed_case_secret_says_the_case_of_every_letter() {
        let said = spoken("Setup key", "aB2l");
        assert_eq!(
            said,
            "Setup key, in groups of four. Group 1: lower case a, capital B, 2, lower case l."
        );
    }

    #[test]
    fn hyphens_and_spaces_are_only_grouping() {
        assert_eq!(spoken("K", "ABCD-EFGH"), spoken("K", "ABCD EFGH"));
        assert_eq!(spoken("K", "ABCD-EFGH"), spoken("K", "ABCDEFGH"));
    }
}
