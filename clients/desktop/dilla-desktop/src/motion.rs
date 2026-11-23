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
//!
//! The cache is the app's (a GPUI global), not the process's, and the read goes through an [`OsReader`]: the shipping app
//! reads on a thread of its own ([`ThreadReader`]); an app on GPUI's test scheduler never asks the OS (its reads answer
//! nothing), and a test gives the cache a reader it answers itself, on the test's thread.

use std::sync::{Arc, Mutex};
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

/// How a read of the OS runs: [`Self::start`] returns at once and the answer (`None`: it could not say) is sent on
/// `answer` when it comes; a dropped `answer` is no answer.
pub trait OsReader: Send + Sync + 'static {
    fn start(&self, answer: tokio::sync::oneshot::Sender<Option<bool>>);
}

/// The shipping read: the OS asked on a thread of its own, so a portal that is slow or never answers holds no window and
/// no executor thread.
pub struct ThreadReader;

impl OsReader for ThreadReader {
    fn start(&self, answer: tokio::sync::oneshot::Sender<Option<bool>>) {
        std::thread::spawn(move || {
            let _ = answer.send(os_reduces());
        });
    }
}

/// The read of an app on GPUI's test scheduler: the OS is not asked (no thread, no portal), so nothing answers.
struct NoOs;

impl OsReader for NoOs {
    fn start(&self, _answer: tokio::sync::oneshot::Sender<Option<bool>>) {}
}

/// The OS's answer, kept, and how it is read. [`Self::follow`] applies the answer it has and starts a fresh read.
#[derive(Clone)]
pub struct OsMotionCache {
    shared: Arc<Mutex<Shared>>,
    reader: Arc<dyn OsReader>,
}

impl gpui::Global for OsMotionCache {}

impl OsMotion for OsMotionCache {
    fn reduces(&self) -> bool {
        self.lock().os.unwrap_or(false)
    }
}

impl OsMotionCache {
    /// A cache that reads the OS with `reader`, knowing nothing yet.
    pub fn new(reader: impl OsReader) -> Self {
        Self {
            shared: Arc::default(),
            reader: Arc::new(reader),
        }
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, Shared> {
        self.shared.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// The app's cache: the real OS read on a thread of its own, or, for an app on GPUI's test scheduler, no read.
    pub fn of(cx: &mut App) -> Self {
        if !cx.has_global::<Self>() {
            let cache = if cx.is_test() {
                Self::new(NoOs)
            } else {
                Self::new(ThreadReader)
            };
            cx.set_global(cache);
        }
        cx.global::<Self>().clone()
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

    /// Applies `setting` with what is known of the OS, and starts a read of the OS (the cache's [`OsReader`], which does
    /// not wait for it) when one is due; its answer is applied when it arrives, with the choice the latest call brought.
    pub fn follow(&self, setting: MotionSetting, cx: &mut App) {
        cx.set_reduce_motion(reduces(setting, self));
        if !self.begin(setting) {
            return;
        }
        let (tx, rx) = tokio::sync::oneshot::channel();
        self.reader.start(tx);
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
    OsMotionCache::of(cx).follow(state.profile.reduce_motion, cx);
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

/// Asks the OS, on the thread [`ThreadReader`] gives it (`None` when it cannot say).
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

    /// A reader the test answers itself, on its own thread: it keeps the answer channel of each read under way.
    #[derive(Clone, Default)]
    struct Scripted(Arc<Mutex<Vec<tokio::sync::oneshot::Sender<Option<bool>>>>>);

    impl OsReader for Scripted {
        fn start(&self, answer: tokio::sync::oneshot::Sender<Option<bool>>) {
            self.0.lock().unwrap().push(answer);
        }
    }

    impl Scripted {
        fn pending(&self) -> usize {
            self.0.lock().unwrap().len()
        }

        fn answer(&self, value: Option<bool>) {
            let tx = self.0.lock().unwrap().pop().expect("a read under way");
            tx.send(value).unwrap();
        }
    }

    /// The saved choice reaches the app at once ("On" and "Off", which do not wait for the OS).
    #[gpui::test]
    fn the_saved_choice_sets_the_apps_reduced_motion(cx: &mut gpui::TestAppContext) {
        let cache = OsMotionCache::new(Scripted::default());
        cx.update(|cx| {
            cache.follow(MotionSetting::On, cx);
            assert!(cx.reduce_motion());
            cache.follow(MotionSetting::Off, cx);
            assert!(!cx.reduce_motion());
        });
    }

    /// An app on the test scheduler has its own cache, which never asks the real OS: a read answers nothing, and the app
    /// animates.
    #[gpui::test]
    fn an_app_under_test_never_asks_the_os(cx: &mut gpui::TestAppContext) {
        cx.update(|cx| {
            let mut state = AppState::default();
            state.profile.reduce_motion = MotionSetting::System;
            follow(&state, cx);
            assert!(!cx.reduce_motion());
        });
        cx.run_until_parked();
        assert!(!cx.update(|cx| cx.reduce_motion()));
        assert!(
            cx.update(|cx| OsMotionCache::of(cx).lock().read_at.is_some()),
            "the read ended"
        );
    }

    /// An answer that cannot be given (a timeout, a missing portal) keeps the value known, and a read that is under way
    /// holds off every other.
    #[test]
    fn a_read_without_an_answer_keeps_the_known_value_and_one_read_runs_at_a_time() {
        let cache = OsMotionCache::new(Scripted::default());
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
        let reader = Scripted::default();
        let cache = OsMotionCache::new(reader.clone());
        for _ in 0..3 {
            cx.update(|cx| {
                cache.follow(MotionSetting::System, cx);
                assert!(!cx.reduce_motion(), "animates until the OS has answered");
            });
            cx.run_until_parked();
        }
        assert_eq!(reader.pending(), 1, "one read under way");
        assert!(!cx.update(|cx| cx.reduce_motion()), "still waiting");
        reader.answer(Some(true));
        cx.run_until_parked();
        assert!(cx.update(|cx| cx.reduce_motion()), "the answer is applied");
        assert_eq!(reader.pending(), 0);
    }
}
