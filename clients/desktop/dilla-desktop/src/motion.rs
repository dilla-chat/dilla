//! The app's reduced-motion choice (Task 36b): what the web's `data-reduce-motion` on the document root is
//! (packages/web/src/App.tsx:106). The choice goes to GPUI's own `App::reduce_motion`, which every animation in
//! dilla-desktop-ui reads ([`dilla_desktop_ui::motion`]); a change redraws every window. `On` reduces, `Off` animates, and
//! `System` follows the OS ([`OsMotion`], what the web's `prefers-reduced-motion` is): Windows' "Show animations in
//! Windows" (`SPI_GETCLIENTAREAANIMATION`), macOS' "Reduce motion" (`accessibilityDisplayShouldReduceMotion`), and on Linux
//! the GNOME `enable-animations` key read through the XDG desktop portal (the portal's `org.freedesktop.appearance` has no
//! reduced-motion key; a desktop without the key animates). The OS value is read again whenever the account's state
//! moves, at most every [`OS_REREAD`], not on the OS's own change notification.

use std::sync::Mutex;
use std::time::{Duration, Instant};

use dilla_client::state::AppState;
use dilla_client::state::profile::MotionSetting;
use gpui::App;

/// How long an OS reading is kept before the next state change reads it again.
pub const OS_REREAD: Duration = Duration::from_secs(10);

/// The OS's reduced-motion preference; a seam so the tests do not touch the real OS.
pub trait OsMotion {
    /// Whether the OS asks for reduced motion (false when it cannot say).
    fn reduces(&self) -> bool;
}

/// The real OS, read at most every [`OS_REREAD`].
pub struct Os;

impl OsMotion for Os {
    fn reduces(&self) -> bool {
        static LAST: Mutex<Option<(Instant, bool)>> = Mutex::new(None);
        let mut last = LAST.lock().unwrap_or_else(|e| e.into_inner());
        match *last {
            Some((at, value)) if at.elapsed() < OS_REREAD => value,
            _ => {
                let value = os_reduces();
                *last = Some((Instant::now(), value));
                value
            }
        }
    }
}

/// Whether a saved choice reduces motion, with `os` answering for "Use system setting".
pub fn reduces(setting: MotionSetting, os: &impl OsMotion) -> bool {
    match setting {
        MotionSetting::On => true,
        MotionSetting::Off => false,
        MotionSetting::System => os.reduces(),
    }
}

/// The account's state moved: the app takes the motion it saved.
pub fn follow(state: &AppState, cx: &mut App) {
    cx.set_reduce_motion(reduces(state.profile.reduce_motion, &Os));
}

/// The `enable-animations` answer of the portal's `Read` (a bool, wrapped in a variant once or twice by the portal's
/// version): the OS reduces motion when animations are off. Anything else is no answer.
#[cfg(target_os = "linux")]
fn animations_off(value: &zbus::zvariant::Value<'_>) -> Option<bool> {
    use zbus::zvariant::Value;
    match value {
        Value::Bool(enabled) => Some(!enabled),
        Value::Value(inner) => animations_off(inner),
        _ => None,
    }
}

#[cfg(target_os = "linux")]
fn os_reduces() -> bool {
    // The call runs off the UI thread and is given half a second: a session with no portal must not stall the window.
    let (tx, rx) = std::sync::mpsc::channel();
    std::thread::spawn(move || {
        let read = || -> Option<bool> {
            let conn = zbus::blocking::Connection::session().ok()?;
            let reply = conn
                .call_method(
                    Some("org.freedesktop.portal.Desktop"),
                    "/org/freedesktop/portal/desktop",
                    Some("org.freedesktop.portal.Settings"),
                    "Read",
                    &("org.gnome.desktop.interface", "enable-animations"),
                )
                .ok()?;
            let value: zbus::zvariant::OwnedValue = reply.body().deserialize().ok()?;
            animations_off(&value)
        };
        let _ = tx.send(read().unwrap_or(false));
    });
    rx.recv_timeout(Duration::from_millis(500)).unwrap_or(false)
}

#[cfg(target_os = "windows")]
#[allow(unsafe_code)] // One read of a system parameter into a local BOOL; its SAFETY note is at the call.
fn os_reduces() -> bool {
    use windows_sys::Win32::UI::WindowsAndMessaging::{
        SPI_GETCLIENTAREAANIMATION, SystemParametersInfoW,
    };
    let mut on: i32 = 1;
    // SAFETY: SPI_GETCLIENTAREAANIMATION writes one BOOL (an i32) through pvParam, which points at `on`.
    let ok = unsafe {
        SystemParametersInfoW(
            SPI_GETCLIENTAREAANIMATION,
            0,
            (&mut on as *mut i32).cast(),
            0,
        )
    };
    ok != 0 && on == 0
}

#[cfg(target_os = "macos")]
#[allow(unsafe_code)] // Two AppKit messages with no arguments; their SAFETY note is at the call.
fn os_reduces() -> bool {
    use objc2::msg_send;
    use objc2::runtime::{AnyClass, AnyObject};
    let Some(class) = AnyClass::get(c"NSWorkspace") else {
        return false;
    };
    // SAFETY: `sharedWorkspace` returns the shared NSWorkspace (or nil), and `accessibilityDisplayShouldReduceMotion` is
    // a BOOL property of it; both take no arguments.
    unsafe {
        let workspace: *mut AnyObject = msg_send![class, sharedWorkspace];
        !workspace.is_null() && msg_send![workspace, accessibilityDisplayShouldReduceMotion]
    }
}

#[cfg(not(any(target_os = "linux", target_os = "windows", target_os = "macos")))]
fn os_reduces() -> bool {
    false
}

#[cfg(test)]
mod tests {
    use super::*;

    struct Fake(bool);
    impl OsMotion for Fake {
        fn reduces(&self) -> bool {
            self.0
        }
    }

    /// "On" and "Off" ignore the OS; "Use system setting" is the OS's answer.
    #[test]
    fn the_setting_maps_to_reduced_motion_with_the_os_for_system() {
        for os in [false, true] {
            assert!(reduces(MotionSetting::On, &Fake(os)));
            assert!(!reduces(MotionSetting::Off, &Fake(os)));
            assert_eq!(reduces(MotionSetting::System, &Fake(os)), os);
        }
    }

    /// The portal's `enable-animations`: false reduces, true animates, wrapped once or twice; a non-bool is no answer.
    #[cfg(target_os = "linux")]
    #[test]
    fn the_portals_enable_animations_maps_to_reduced_motion() {
        use zbus::zvariant::Value;
        assert_eq!(animations_off(&Value::Bool(false)), Some(true));
        assert_eq!(animations_off(&Value::Bool(true)), Some(false));
        let wrapped = Value::Value(Box::new(Value::Value(Box::new(Value::Bool(false)))));
        assert_eq!(animations_off(&wrapped), Some(true));
        assert_eq!(animations_off(&Value::U32(1)), None);
    }

    /// The saved choice reaches the app at once ("On" and "Off", which do not ask the OS).
    #[gpui::test]
    fn the_saved_choice_sets_the_apps_reduced_motion(cx: &mut gpui::TestAppContext) {
        cx.update(|cx| {
            let mut state = AppState::default();
            state.profile.reduce_motion = MotionSetting::On;
            follow(&state, cx);
            assert!(cx.reduce_motion());
            state.profile.reduce_motion = MotionSetting::Off;
            follow(&state, cx);
            assert!(!cx.reduce_motion());
        });
    }
}
