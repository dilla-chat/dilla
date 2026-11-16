//! The macOS traffic lights per window mode (beta.5 `setWindowMode`, packages/desktop/src/main.ts:138 and :186):
//! hidden in the compact (starting and authentication) windows, which draw their own close button
//! ([`crate::screens::titlebar::compact_chrome`]); shown in the workspace at [`crate::MAC_WINDOW_BUTTONS`], inside the
//! top bar. GPUI can move the buttons but not hide them, so the hiding goes through AppKit. Elsewhere this does nothing:
//! Linux and Windows draw their own window buttons.

use gpui::Window;

use crate::screens::shell::window::WindowMode;

/// Shows or hides `window`'s traffic lights for `mode`, and places them where [`crate::mac_window_buttons`] says.
#[cfg(target_os = "macos")]
pub fn apply(window: &mut Window, mode: WindowMode) {
    use objc2_app_kit::{NSView, NSWindowButton};
    use raw_window_handle::{HasWindowHandle, RawWindowHandle};

    let position = crate::mac_window_buttons(mode);
    if let Some((x, y)) = position {
        window.set_traffic_light_position(gpui::point(gpui::px(x), gpui::px(y)));
    }
    let Ok(handle) = HasWindowHandle::window_handle(&*window) else {
        return;
    };
    let RawWindowHandle::AppKit(appkit) = handle.as_raw() else {
        return;
    };
    // SAFETY: GPUI's AppKit handle is the window's live content NSView (gpui-pre-macos `MacWindow::window_handle`); it is
    // used here on the main thread, inside the window's own update, while GPUI holds the view.
    let view: &NSView = unsafe { appkit.ns_view.cast::<NSView>().as_ref() };
    let Some(ns_window) = view.window() else {
        return;
    };
    for kind in [
        NSWindowButton::CloseButton,
        NSWindowButton::MiniaturizeButton,
        NSWindowButton::ZoomButton,
    ] {
        if let Some(button) = ns_window.standardWindowButton(kind) {
            button.setHidden(position.is_none());
        }
    }
}

/// Linux and Windows: the window draws its own buttons (client decorations); nothing to do.
#[cfg(not(target_os = "macos"))]
pub fn apply(window: &mut Window, mode: WindowMode) {
    let _ = (window, mode);
}
