//! The unread badge (Package 5; beta.5 `app.setBadgeCount(sum of unread)`): the macOS dock badge, the Windows overlay
//! icon, the Linux Unity `LauncherEntry` (D-Bus, `zbus`).
//!
//! To do (Package 5): register a sink with [`super::NativeShell::add_badge_sink`] that shows the total; the screens
//! report each instance's unread through [`super::NativeShell::set_badge`].

use gpui::App;

/// Called once by [`super::install`]. Task 0: nothing yet.
pub fn install(_cx: &mut App) {}
