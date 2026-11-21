//! The app's reduced-motion choice (Task 36b): what the web's `data-reduce-motion` on the document root is
//! (packages/web/src/App.tsx:106). The choice goes to GPUI's own `App::reduce_motion`, which every animation in
//! dilla-desktop-ui reads ([`dilla_desktop_ui::motion`]); a change redraws every window. `On` reduces, `Off` animates, and
//! `System` follows the OS ([`OsMotion`], what the web's `prefers-reduced-motion` is): Windows' "Show animations in
//! Windows" (`SPI_GETCLIENTAREAANIMATION`), macOS' "Reduce motion" (`accessibilityDisplayShouldReduceMotion`), and on Linux
//! the GNOME `enable-animations` key read through the XDG desktop portal (the portal's `org.freedesktop.appearance` has no
//! reduced-motion key; a desktop without the key animates).
//!
//! The OS is never waited for: [`OsMotionCache`] answers with the last answer it has (animate until the first), and a read
//! on a thread of its own, one at a time, brings the next, which is applied (and the windows redrawn) when it arrives. A
//! read that cannot answer keeps the value already known. The OS is asked again when the account's state moves, at most
//! every [`OS_REREAD`], not on the OS's own change notification.

use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use dilla_client::state::AppState;
use dilla_client::state::profile::MotionSetting;
use gpui::App;

/// How long an OS answer is kept before the next state change asks again.
pub const OS_REREAD: Duration = Duration::from_secs(10);

/// The OS's reduced-motion preference; a seam so the tests do not touch the real OS.
pub trait OsMotion {
    /// Whether the OS asks for reduced motion (false until it has answered).
    fn reduces(&self) -> bool;
}

/// What the app knows of the OS and of its one read.
#[derive(Default)]
struct Shared {
    /// The last answer; a read that gives none (a portal that is absent, slow or refuses) leaves it as it was.
    os: Option<bool>,
    read_at: Option<Instant>,
    /// A read is under way: no second one starts until it answers (a portal that never answers costs one thread, once).
    in_flight: bool,
    /// The saved choice the last state change brought, which an answer arriving later is applied with.
    setting: MotionSetting,
}

/// The OS's answer, kept. [`Self::follow`] applies the answer it has and asks for a fresh one on a thread of its own.
#[derive(Clone, Default)]
pub struct OsMotionCache(Arc<Mutex<Shared>>);

impl OsMotion for OsMotionCache {
    fn reduces(&self) -> bool {
        self.lock().os.unwrap_or(false)
    }
}

impl OsMotionCache {
    fn lock(&self) -> std::sync::MutexGuard<'_, Shared> {
        self.0.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// The app's cache of the real OS.
    pub fn global() -> &'static Self {
        static OS: OnceLock<OsMotionCache> = OnceLock::new();
        OS.get_or_init(Self::default)
    }

    /// Whether a read should start now: none under way and the last answer older than [`OS_REREAD`]. Starting one marks it.
    fn begin(&self, setting: MotionSetting) -> bool {
        let mut shared = self.lock();
        shared.setting = setting;
        let fresh = shared.read_at.is_some_and(|at| at.elapsed() < OS_REREAD);
        if shared.in_flight || fresh {
            return false;
        }
        shared.in_flight = true;
        true
    }

    /// A read answered (`None`: it could not say, which keeps what was known). Gives whether the answer changed what the
    /// OS is known to ask for, and the saved choice to apply it with.
    fn finish(&self, answer: Option<bool>) -> (bool, MotionSetting) {
        let mut shared = self.lock();
        shared.in_flight = false;
        shared.read_at = Some(Instant::now());
        let changed = answer.is_some() && shared.os != answer;
        if answer.is_some() {
            shared.os = answer;
        }
        (changed, shared.setting)
    }

    /// Applies `setting` with what is known of the OS, and starts a read of the OS (`read`, on a thread of its own) when
    /// one is due; its answer is applied when it arrives, with the choice the latest call brought.
    pub fn follow(
        &self,
        setting: MotionSetting,
        read: impl FnOnce() -> Option<bool> + Send + 'static,
        cx: &mut App,
    ) {
        cx.set_reduce_motion(reduces(setting, self));
        if !self.begin(setting) {
            return;
        }
        let (tx, rx) = tokio::sync::oneshot::channel();
        std::thread::spawn(move || {
            let _ = tx.send(read());
        });
        let cache = self.clone();
        cx.spawn(async move |cx| {
            let answer = rx.await.ok().flatten();
            let (changed, setting) = cache.finish(answer);
            if changed {
                cx.update(|cx| cx.set_reduce_motion(reduces(setting, &cache)));
            }
        })
        .detach();
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
    OsMotionCache::global().follow(state.profile.reduce_motion, os_reduces, cx);
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

/// Asks the OS, on the thread [`OsMotionCache::follow`] gives it (`None` when it cannot say).
#[cfg(target_os = "linux")]
fn os_reduces() -> Option<bool> {
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
}

#[cfg(target_os = "windows")]
#[allow(unsafe_code)] // One read of a system parameter into a local BOOL; its SAFETY note is at the call.
fn os_reduces() -> Option<bool> {
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
    (ok != 0).then_some(on == 0)
}

#[cfg(target_os = "macos")]
#[allow(unsafe_code)] // Two AppKit messages with no arguments; their SAFETY note is at the call.
fn os_reduces() -> Option<bool> {
    use objc2::msg_send;
    use objc2::runtime::{AnyClass, AnyObject};
    let class = AnyClass::get(c"NSWorkspace")?;
    // SAFETY: `sharedWorkspace` returns the shared NSWorkspace (or nil), and `accessibilityDisplayShouldReduceMotion` is
    // a BOOL property of it; both take no arguments.
    unsafe {
        let workspace: *mut AnyObject = msg_send![class, sharedWorkspace];
        if workspace.is_null() {
            return None;
        }
        Some(msg_send![workspace, accessibilityDisplayShouldReduceMotion])
    }
}

#[cfg(not(any(target_os = "linux", target_os = "windows", target_os = "macos")))]
fn os_reduces() -> Option<bool> {
    None
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::atomic::{AtomicUsize, Ordering};

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
            let cache = OsMotionCache::default();
            let mut state = AppState::default();
            state.profile.reduce_motion = MotionSetting::On;
            cache.follow(state.profile.reduce_motion, || None, cx);
            assert!(cx.reduce_motion());
            state.profile.reduce_motion = MotionSetting::Off;
            cache.follow(state.profile.reduce_motion, || None, cx);
            assert!(!cx.reduce_motion());
        });
    }

    /// An answer that cannot be given (a timeout, a missing portal) keeps the value known, and a read that is under way
    /// holds off every other.
    #[test]
    fn a_read_without_an_answer_keeps_the_known_value_and_one_read_runs_at_a_time() {
        let cache = OsMotionCache::default();
        assert!(cache.begin(MotionSetting::System));
        assert!(!cache.begin(MotionSetting::System), "one in flight");
        assert_eq!(cache.finish(Some(true)), (true, MotionSetting::System));
        assert!(cache.reduces());
        // Fresh answer: no read; once it is old, a read, which finds no answer and leaves `true` as it was.
        assert!(!cache.begin(MotionSetting::System));
        cache.lock().read_at = Some(Instant::now() - OS_REREAD);
        assert!(cache.begin(MotionSetting::System));
        assert_eq!(cache.finish(None), (false, MotionSetting::System));
        assert!(cache.reduces(), "a timeout is not an answer of false");
    }

    /// A portal that is slow or never answers does not hold the window: `follow` returns with the value it had, starts one
    /// read however often the state moves, and the answer, when it comes, is applied.
    #[gpui::test]
    fn a_slow_portal_blocks_nothing_and_is_asked_once(cx: &mut gpui::TestAppContext) {
        let cache = OsMotionCache::default();
        let reads = Arc::new(AtomicUsize::new(0));
        let (release, wait) = std::sync::mpsc::channel::<Option<bool>>();
        let wait = Arc::new(Mutex::new(Some(wait)));
        cx.executor().allow_parking();
        for _ in 0..3 {
            let (reads, wait) = (reads.clone(), wait.clone());
            cx.update(|cx| {
                cache.follow(
                    MotionSetting::System,
                    move || {
                        reads.fetch_add(1, Ordering::SeqCst);
                        let wait = wait.lock().unwrap().take().expect("one read");
                        wait.recv().ok().flatten()
                    },
                    cx,
                );
                assert!(!cx.reduce_motion(), "animates until the OS has answered");
            });
        }
        // The thread has started its one read, which is still waiting.
        while reads.load(Ordering::SeqCst) == 0 {
            std::thread::sleep(Duration::from_millis(5));
        }
        assert_eq!(reads.load(Ordering::SeqCst), 1);
        release.send(Some(true)).unwrap();
        for _ in 0..200 {
            cx.run_until_parked();
            if cx.update(|cx| cx.reduce_motion()) {
                break;
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        assert!(cx.update(|cx| cx.reduce_motion()), "the answer is applied");
        assert_eq!(reads.load(Ordering::SeqCst), 1);
    }
}
