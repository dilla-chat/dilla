//! Settings -> Keyboard (packages/web/src/screens/settings/Keyboard.tsx and packages/ui/src/KeyboardTable).
//!
//! The section is a table of the shortcuts of [`super::keys::SHORTCUTS`]: each row is a setting row (at least 56px, 10px 16px
//! of padding) in one 10px-radius group with hairlines between; the action (12.5/600) on the left and its keycaps (`KeyHint`)
//! on the right. A chord such as `⌘+⇧+M` is one keycap per key; off macOS `⌘` is Ctrl and `⌥` is Alt.

use gpui::{AnyElement, App, IntoElement, ParentElement, Styled, Window, div};

use dilla_desktop_ui::components::key_hint::KeyHint;
use dilla_desktop_ui::components::settings::frame::section_header;
use dilla_desktop_ui::components::settings::row::{GroupPosition, SettingsRow};
use dilla_desktop_ui::metrics::Density;

use super::keys::{SHORTCUTS, Shortcut};
use crate::strings::t;

/// `Keyboard.tsx` `caps`: a chord such as `⌘+⇧+M` is drawn as one keycap per key (`+` followed by nothing is a key).
pub fn caps(chord: &str) -> Vec<String> {
    let chars: Vec<char> = chord.chars().collect();
    let mut out = Vec::new();
    let mut current = String::new();
    for (i, &c) in chars.iter().enumerate() {
        if c == '+' && i + 1 < chars.len() {
            out.push(std::mem::take(&mut current));
        } else {
            current.push(c);
        }
    }
    out.push(current);
    out
}

/// `Keyboard.tsx` `platformKeys`: ⌘ and ⌥ on macOS, Ctrl and Alt on Windows and Linux.
pub fn platform_keys(chord: &str, apple: bool) -> Vec<String> {
    caps(chord)
        .into_iter()
        .map(|k| {
            if apple {
                k
            } else {
                match k.as_str() {
                    "⌘" => "Ctrl".to_owned(),
                    "⌥" => "Alt".to_owned(),
                    _ => k,
                }
            }
        })
        .collect()
}

/// Whether this build runs on macOS (`isApplePlatform`).
pub fn is_apple() -> bool {
    cfg!(target_os = "macos")
}

/// One printed row: the action's name and its keycaps.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Row {
    pub action: &'static str,
    pub keys: Vec<String>,
}

/// The rows of the table, in the order of [`SHORTCUTS`].
pub fn rows(apple: bool) -> Vec<Row> {
    SHORTCUTS
        .iter()
        .map(|s: &Shortcut| Row {
            action: t(s.action),
            keys: platform_keys(t(s.chord), apple),
        })
        .collect()
}

/// The section: its heading and body, then the table.
pub fn view(_window: &mut Window, _cx: &mut App) -> AnyElement {
    let rows = rows(is_apple());
    let n = rows.len();
    let table =
        rows.into_iter()
            .enumerate()
            .fold(div().flex().flex_col().w_full(), |table, (i, row)| {
                let keys: Vec<&str> = row.keys.iter().map(String::as_str).collect();
                table.child(
                    SettingsRow::new(("keyboard-row", i), row.action)
                        .position(GroupPosition::of(i, n))
                        .child(KeyHint::new(("keyboard-keys", i), &keys)),
                )
            });
    let _ = Density::Regular;
    div()
        .flex()
        .flex_col()
        .w_full()
        .child(section_header(
            t("keyboard.title"),
            Some(t("signalSettings.keyboard.body")),
        ))
        .child(table)
        .into_any_element()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_chord_is_one_keycap_per_key() {
        assert_eq!(caps("⌘+⇧+M"), ["⌘", "⇧", "M"]);
        assert_eq!(caps("Ctrl+/"), ["Ctrl", "/"]);
        assert_eq!(caps("Esc"), ["Esc"]);
        assert_eq!(caps("Ctrl++"), ["Ctrl", "+"]);
    }

    #[test]
    fn off_macos_command_and_option_become_ctrl_and_alt() {
        assert_eq!(platform_keys("⌘+⌥+K", false), ["Ctrl", "Alt", "K"]);
        assert_eq!(platform_keys("⌘+⇧+M", true), ["⌘", "⇧", "M"]);
    }

    /// The Keyboard section lists exactly the bindings `keys.rs` registers: the same rows in the same order, each chord a
    /// keystroke GPUI parses, no two rows on one keystroke.
    #[test]
    fn keyboard_table_matches_bindings() {
        let apple = false;
        let printed = rows(apple);
        assert_eq!(printed.len(), SHORTCUTS.len());
        for (row, s) in printed.iter().zip(SHORTCUTS.iter()) {
            assert_eq!(row.action, t(s.action));
            assert_eq!(row.keys, platform_keys(t(s.chord), apple));
            assert!(!row.keys.is_empty());
            gpui::Keystroke::parse(s.keystroke)
                .unwrap_or_else(|e| panic!("{} does not parse: {e:?}", s.keystroke));
        }
        let mut seen: Vec<&str> = SHORTCUTS.iter().map(|s| s.keystroke).collect();
        seen.sort_unstable();
        seen.dedup();
        assert_eq!(seen.len(), SHORTCUTS.len(), "two rows share a keystroke");
        // The printed keycaps of each row are the keystroke's keys: Ctrl for secondary, Shift, then the key.
        let search = &printed[0];
        assert_eq!(search.keys, ["Ctrl", "K"]);
        assert_eq!(printed[1].keys, ["Ctrl", "⇧", "M"]);
        assert_eq!(printed[3].keys, ["Esc"]);
    }

    /// The seven rows of `Keyboard.tsx`, by their copy.
    #[test]
    fn the_rows_are_the_web_sections_seven() {
        let actions: Vec<&str> = rows(false).iter().map(|r| r.action).collect();
        assert_eq!(
            actions,
            [
                "Search",
                "Mute",
                "Deafen",
                "Close a window",
                "Jump to present",
                "Open search",
                "Call the open direct message",
            ]
        );
    }
}
