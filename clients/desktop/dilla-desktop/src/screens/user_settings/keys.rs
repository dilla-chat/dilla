//! The user settings area's key bindings (Task 16): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! Settings -> Keyboard lists the app's shortcuts. [`SHORTCUTS`] is the one list: the Keyboard section draws exactly these
//! rows ([`super::keyboard`]), in this order, and a test (`keyboard_table_matches_bindings`) holds the section to it and each
//! chord to a keystroke GPUI parses. The rows are `packages/web/src/screens/settings/Keyboard.tsx`'s seven (the prototype's
//! five, then the second search chord and the open DM's call).
//!
//! Each shortcut is acted on by the area that owns the action (search 23, the voice controls and the call 8 and 17, the
//! conversation's jump 9, Esc by the dialogs of Task 6); that area registers the binding in its own `keys.rs`. This area owns
//! the list and registers no key of its own: [`bind`] is empty on purpose.

use gpui::App;

/// One row of Settings -> Keyboard.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Shortcut {
    /// The `strings.rs` key of the action's name.
    pub action: &'static str,
    /// The `strings.rs` key of the chord as the web prints it: `⌘` and `⌥` become Ctrl and Alt off macOS.
    pub chord: &'static str,
    /// The same chord as a GPUI keystroke (the Ctrl form; the owning area binds Cmd for it on macOS).
    pub keystroke: &'static str,
}

/// The shortcuts, in the order Settings -> Keyboard lists them.
pub const SHORTCUTS: [Shortcut; 7] = [
    Shortcut {
        action: "signalSettings.keyboard.search",
        chord: "signalSettings.keyboard.searchKeys",
        keystroke: "ctrl-k",
    },
    Shortcut {
        action: "signalSettings.keyboard.mute",
        chord: "signalSettings.keyboard.muteKeys",
        keystroke: "ctrl-shift-m",
    },
    Shortcut {
        action: "signalSettings.keyboard.deafen",
        chord: "signalSettings.keyboard.deafenKeys",
        keystroke: "ctrl-shift-d",
    },
    Shortcut {
        action: "signalSettings.keyboard.close",
        chord: "signalSettings.keyboard.closeKeys",
        keystroke: "escape",
    },
    Shortcut {
        action: "signalSettings.keyboard.jump",
        chord: "signalSettings.keyboard.jumpKeys",
        keystroke: "ctrl-end",
    },
    Shortcut {
        action: "signalSettings.keyboard.searchSlash",
        chord: "signalSettings.keyboard.searchSlashKeys",
        keystroke: "ctrl-/",
    },
    Shortcut {
        action: "keyboard.dmCallAction",
        chord: "signalSettings.keyboard.dmCallKeys",
        keystroke: "ctrl-shift-enter",
    },
];

/// Registers this area's bindings: none (see the module's note).
pub fn bind(_cx: &mut App) {}
