//! Task 20's components (`components/commands/`): the counterparts of packages/ui `CommandPicker` (the `/` list, the
//! chips, the field bar), `BotResult` (a private result row), `PollCard`, `PollDialog` and `RemindersPopover`.
//!
//! Like every component they hold no state and carry no copy: the screen passes the strings and the handlers.

pub mod bot_result;
pub mod chips;
pub mod list;
pub mod poll_card;
pub mod poll_dialog;
pub mod reminders;

use gpui::{BoxShadow, Hsla, point, px};

/// `--shadow-2`: `0 20px 50px rgba(0,0,0,0.55)` (the command list and the suggestions; packages/design-tokens).
pub fn shadow_2() -> Vec<BoxShadow> {
    crate::metrics::css_shadow(gpui::hsla(0.0, 0.0, 0.0, 0.55), 20.0, 50.0)
}

/// `--shadow-popover`: `0 20px 50px rgba(0,0,0,0.6)` (the reminders popover).
pub fn shadow_popover() -> Vec<BoxShadow> {
    crate::metrics::css_shadow(gpui::hsla(0.0, 0.0, 0.0, 0.6), 20.0, 50.0)
}

/// `box-shadow: inset 2px 0 0 var(--accent)`: the selected row's left bar. GPUI draws no inset shadow, so it is a 2px
/// strip at the row's left edge, inside its rounded box.
pub fn accent_bar() -> gpui::Div {
    use gpui::Styled;
    gpui::div()
        .absolute()
        .left_0()
        .top_0()
        .bottom_0()
        .w(px(2.0))
        .bg(ACCENT)
}

const ACCENT: Hsla = crate::tokens::color::ACCENT;

/// Ring of `box-shadow: 0 0 0 3px <colour>` (an active chip's halo).
pub fn halo(colour: Hsla) -> Vec<BoxShadow> {
    vec![BoxShadow {
        color: colour,
        offset: point(px(0.0), px(0.0)),
        blur_radius: px(0.0),
        spread_radius: px(3.0),
        inset: false,
    }]
}

#[cfg(test)]
mod shadow_tests {
    /// Review m6: the CSS blur (50px) is drawn as GPUI's deviation, half of it (`metrics::css_blur`).
    #[test]
    fn the_command_shadows_blur_as_css_does() {
        assert_eq!(super::shadow_2()[0].blur_radius, gpui::px(25.0));
        assert_eq!(super::shadow_popover()[0].blur_radius, gpui::px(25.0));
    }
}
