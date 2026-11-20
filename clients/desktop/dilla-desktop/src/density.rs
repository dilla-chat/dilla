//! The window's density (Task 19b): what the web's `data-density` on the document root is (packages/web/src/App.tsx:105).
//! The web sets it from the saved choice and every rule that reads `--row-pad-y`, `--row-pad-x`, `--row-gap`,
//! `--group-gap`, `--avatar-size` or `--line-height` steps with it at once; here every view that draws one of those reads
//! the window's [`Density`] (`Density::of`, kept per window in dilla-desktop-ui so its components read it themselves).
//!
//! Each window keeps its own, since each can hold another account. The shell's state loop sets it from the account's
//! saved choice ([`follow`]); Appearance sets it in a window without a client, where its choice stays in the window; a
//! fixture sets its own (`"density"` in the fixture JSON). A window nobody set is regular, as the web's root is before
//! its settings load (`regular` unless the setting is `compact`). A change redraws the window, so the whole app takes it
//! without a reload. A closed window's density goes with its other per-window state ([`crate::per_window`]).

use dilla_client::state::AppState;
use dilla_client::state::profile::DensitySetting;
use dilla_desktop_ui::metrics::Density;
use gpui::{App, Window};

/// Sets `window`'s density; a change redraws the whole window.
pub fn set(window: &mut Window, density: Density, cx: &mut App) {
    if Density::set_for(window.window_handle().window_id(), density, cx) {
        window.refresh();
    }
}

/// The density of a saved setting (App.tsx:105: `compact`, and `regular` for anything else).
pub fn of_setting(setting: DensitySetting) -> Density {
    match setting {
        DensitySetting::Compact => Density::Compact,
        DensitySetting::Regular => Density::Regular,
    }
}

/// The account's state moved: the window takes the density it saved.
pub fn follow(window: &mut Window, state: &AppState, cx: &mut App) {
    set(window, of_setting(state.profile.density), cx);
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The window follows the account's saved choice, and each window keeps its own.
    #[gpui::test]
    fn a_window_follows_the_saved_density(cx: &mut gpui::TestAppContext) {
        let a = cx.add_empty_window().update(|w, _| w.window_handle());
        let b = cx.add_empty_window().update(|w, _| w.window_handle());
        cx.update(|cx| {
            a.update(cx, |_, window, cx| {
                let mut state = AppState::default();
                follow(window, &state, cx);
                assert_eq!(Density::of(window, cx), Density::Regular);
                state.profile.density = DensitySetting::Compact;
                follow(window, &state, cx);
                assert_eq!(Density::of(window, cx), Density::Compact);
            })
            .unwrap();
            b.update(cx, |_, window, cx| {
                assert_eq!(Density::of(window, cx), Density::Regular)
            })
            .unwrap();
        });
    }
}
