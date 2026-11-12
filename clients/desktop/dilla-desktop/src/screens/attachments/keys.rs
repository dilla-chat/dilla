//! The attachments area's key bindings (Task 12): `crate::keys::bind_all` calls [`bind`] once per app.
//!
//! - Ctrl+V (Cmd+V on macOS) with a picture on the clipboard attaches it to the open conversation (Composer.tsx
//!   `onPasteFiles`); with text, the field pastes as before.
//! - In the lightbox: ArrowLeft and ArrowRight move, Escape closes (Lightbox.tsx); a modified arrow is left alone.
//! - In the GIF picker: the arrows move the active result, Enter picks it, Escape closes (GifPicker.tsx).
//!
//! They are keystroke interceptors, not bindings: the composer's field (Task 9's) owns the focus and its own
//! Ctrl+V, and an interceptor sees the keystroke first and lets it through unless it handled it.

use gpui::{App, ClipboardEntry, KeystrokeEvent, Window};

use super::ui;
use dilla_desktop_ui::components::attachments::gif_picker::COLUMNS;

/// What a keystroke does here.
#[derive(Debug, PartialEq, Eq)]
pub enum Key {
    Paste,
    LightboxMove(isize),
    LightboxClose,
    GifMove(isize),
    GifPick,
    GifClose,
    None,
}

/// The keystroke's meaning while the lightbox or the GIF picker is open (or neither).
pub fn classify(key: &str, ctrl: bool, platform: bool, alt: bool, shift: bool, lightbox: bool, gifs: bool) -> Key {
    let modified = ctrl || platform || alt || shift;
    let paste = key == "v" && !alt && !shift && (if cfg!(target_os = "macos") { platform } else { ctrl });
    if lightbox {
        return match (key, modified) {
            ("escape", false) => Key::LightboxClose,
            ("left", false) => Key::LightboxMove(-1),
            ("right", false) => Key::LightboxMove(1),
            _ => Key::None,
        };
    }
    if gifs {
        match (key, modified) {
            ("escape", false) => return Key::GifClose,
            ("enter", false) => return Key::GifPick,
            ("left", false) => return Key::GifMove(-1),
            ("right", false) => return Key::GifMove(1),
            ("up", false) => return Key::GifMove(-(COLUMNS as isize)),
            ("down", false) => return Key::GifMove(COLUMNS as isize),
            _ => {}
        }
    }
    if paste { Key::Paste } else { Key::None }
}

fn intercept(e: &KeystrokeEvent, _window: &mut Window, cx: &mut App) {
    let k = &e.keystroke;
    let (lightbox, gifs) = {
        let u = ui::ui(cx);
        (u.lightbox.is_some(), u.gif.is_some())
    };
    let m = &k.modifiers;
    match classify(&k.key, m.control, m.platform, m.alt, m.shift, lightbox, gifs) {
        Key::None => {}
        Key::LightboxClose => {
            ui::close_lightbox(cx);
            cx.stop_propagation();
        }
        Key::LightboxMove(d) => {
            ui::step_lightbox(d, cx);
            cx.stop_propagation();
        }
        Key::GifClose => {
            ui::ui(cx).gif = None;
            cx.refresh_windows();
            cx.stop_propagation();
        }
        Key::GifPick => {
            if let Some(at) = ui::ui(cx).gif.as_ref().and_then(|g| g.active) {
                ui::pick_gif(at, cx);
                cx.stop_propagation();
            }
        }
        Key::GifMove(d) => {
            if let Some(g) = ui::ui(cx).gif.as_mut()
                && !g.items.is_empty()
            {
                let last = g.items.len() as isize - 1;
                let at = g.active.map_or(0, |a| (a as isize + d).clamp(0, last));
                g.active = Some(at as usize);
                cx.refresh_windows();
                cx.stop_propagation();
            }
        }
        Key::Paste => {
            if ui::ui(cx).channel.is_none() {
                return;
            }
            let Some(item) = cx.read_from_clipboard() else { return };
            let images: Vec<gpui::Image> = item
                .entries()
                .iter()
                .filter_map(|e| match e {
                    ClipboardEntry::Image(i) => Some(i.clone()),
                    _ => None,
                })
                .collect();
            if !images.is_empty() {
                ui::attach_images(images, cx);
                cx.stop_propagation();
            }
        }
    }
}

/// Registers this area's keystroke handling.
pub fn bind(cx: &mut App) {
    cx.intercept_keystrokes(intercept).detach();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_lightbox_takes_bare_arrows_and_escape_only() {
        assert_eq!(classify("left", false, false, false, false, true, false), Key::LightboxMove(-1));
        assert_eq!(classify("right", false, false, false, false, true, false), Key::LightboxMove(1));
        assert_eq!(classify("escape", false, false, false, false, true, false), Key::LightboxClose);
        assert_eq!(classify("left", false, false, true, false, true, false), Key::None, "a modified arrow is left alone");
    }

    #[test]
    fn the_gif_picker_moves_by_cells_and_rows_and_picks_on_enter() {
        assert_eq!(classify("down", false, false, false, false, false, true), Key::GifMove(3));
        assert_eq!(classify("up", false, false, false, false, false, true), Key::GifMove(-3));
        assert_eq!(classify("enter", false, false, false, false, false, true), Key::GifPick);
        assert_eq!(classify("escape", false, false, false, false, false, true), Key::GifClose);
    }

    #[test]
    fn paste_is_the_platform_paste_chord() {
        let (ctrl, cmd) = if cfg!(target_os = "macos") { (false, true) } else { (true, false) };
        assert_eq!(classify("v", ctrl, cmd, false, false, false, false), Key::Paste);
        assert_eq!(classify("v", false, false, false, false, false, false), Key::None);
        assert_eq!(classify("v", ctrl, cmd, false, true, false, false), Key::None, "shift+paste is not ours");
    }
}
