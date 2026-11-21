//! The app's reduced-motion choice (Task 36b): what the web's `data-reduce-motion` on the document root is
//! (packages/web/src/App.tsx:106). The saved choice goes to GPUI's own `App::reduce_motion`, which every animation in
//! dilla-desktop-ui reads ([`dilla_desktop_ui::motion`]); a change redraws every window. `On` reduces, `Off` animates, and
//! `System` animates too: GPUI reports no system preference on this platform layer, so the OS's own setting is not read
//! (recorded in TOLERANCES.md).

use dilla_client::state::AppState;
use dilla_client::state::profile::MotionSetting;
use gpui::App;

/// Whether a saved choice reduces motion.
pub fn reduces(setting: MotionSetting) -> bool {
    matches!(setting, MotionSetting::On)
}

/// The account's state moved: the app takes the motion it saved.
pub fn follow(state: &AppState, cx: &mut App) {
    cx.set_reduce_motion(reduces(state.profile.reduce_motion));
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Only "On" reduces, and a change reaches the app at once.
    #[gpui::test]
    fn the_saved_choice_sets_the_apps_reduced_motion(cx: &mut gpui::TestAppContext) {
        cx.update(|cx| {
            let mut state = AppState::default();
            follow(&state, cx);
            assert!(!cx.reduce_motion());
            state.profile.reduce_motion = MotionSetting::On;
            follow(&state, cx);
            assert!(cx.reduce_motion());
            state.profile.reduce_motion = MotionSetting::System;
            follow(&state, cx);
            assert!(!cx.reduce_motion());
        });
    }
}
