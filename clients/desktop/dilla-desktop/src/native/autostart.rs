//! "Start Dilla at login" (Package 5; beta.5 `autostart.ts`): Linux writes `~/.config/autostart/dilla-desktop.desktop`
//! with `Exec="…" --hidden` (mode 0600); macOS and Windows register a login item with `--hidden`.
//!
//! To do (Package 5): [`hidden_start`] answers `--hidden`; [`install`] adds the [`super::TraySlot::Autostart`] check
//! row.

use gpui::App;

/// Whether this launch starts in the tray with no window (`--hidden`, autostart). `argv` is the whole command line,
/// the program name first. `main.rs` skips the first windows of a hidden start only while
/// [`super::NativeShell::stays_in_background`] is on (a tray to come back from). Task 0: never.
pub fn hidden_start(_argv: &[String]) -> bool {
    false
}

/// Called once by [`super::install`]. Task 0: nothing yet.
pub fn install(_cx: &mut App) {}
