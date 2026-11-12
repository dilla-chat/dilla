//! Area **user settings** (Task 16): the Profile, Devices, Appearance and Keyboard sections.
//!
//! Ports: `packages/web/src/screens/settings/{Profile,Devices,RenameDialog,SignOut,KeyDialog,Appearance,Keyboard}.tsx`,
//! `{device-name,device-platform}.ts` and `packages/ui/src/KeyboardTable`. The other user sections belong to other areas:
//! Privacy to 13, Voice to 19, Notifications to 22, Diagnostics to 24 and Sign-in to 26 (`crate::slots::settings_sections`).
//!
//! [`settings_section`] is the hook the settings frame calls for each of this area's sections. The sections keep their state
//! (the form's fields, the open dialog) in one [`UserSettings`] entity per window, kept by `Window::use_keyed_state`; the hook
//! hands it the frame's view (`SlotCx`) on every draw and returns its element.
//!
//! The pure parts are testable without a window: [`draft`] (the Profile form's rules), [`devices`] (what each device row and
//! dialog says) and [`keyboard`] (the shortcuts table).

pub mod appearance;
pub mod device_row;
pub mod devices;
pub mod draft;
pub mod keyboard;
pub mod keys;
pub mod profile;
pub mod section;

use gpui::{AnyElement, App, IntoElement, Window};

use crate::router::UserSection;
use crate::slots::{SectionId, SlotCx};

pub use section::UserSettings;

/// Draws one of this area's settings sections (`slots::settings_sections` points here). A section id of another area draws
/// nothing.
pub fn settings_section(
    section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    let SectionId::User(which) = section else {
        return gpui::Empty.into_any_element();
    };
    if !matches!(
        which,
        UserSection::Profile
            | UserSection::Devices
            | UserSection::Appearance
            | UserSection::Keyboard
    ) {
        return gpui::Empty.into_any_element();
    }
    if which == UserSection::Keyboard {
        // No state: the table is a pure function of the shortcuts.
        return keyboard::view(window, cx);
    }
    let view = window.use_keyed_state("user-settings", cx, |window, cx| {
        UserSettings::new(window, cx)
    });
    view.update(cx, |v, cx| v.show(which, ctx.clone(), cx));
    view.into_any_element()
}
