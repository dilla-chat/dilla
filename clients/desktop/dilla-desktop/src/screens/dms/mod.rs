//! Area **dms** (Task 13): the Home/DMs view, DM conversations, trust alerts and the compare dialog, the Privacy section.
//!
//! Every file under `screens/dms/` is Task 13's. What the area draws inside other areas' views:
//!
//! - [`settings_section`]: the Privacy section (`slots::settings_sections` points here);
//! - [`compare_overlay`]: the safety-number comparison dialog, drawn over the window (to join `slots::overlays`);
//! - [`picker_overlay`]: the DM picker dialog (likewise);
//! - [`conversation::banners`]: a conversation's trust alerts, the removed note and the own-account banner (to join
//!   `slots::banners`);
//! - [`conversation::composer_block`]: why the composer is paused in a conversation (for the composer, Task 9);
//! - [`home::sidebar`]: the Home/DMs list (for the shell's sidebar on `/dm` routes, Task 8).
//!
//! Navigation is the shell's: the area asks for it through the [`Navigator`] the shell installs ([`set_navigator`]); until
//! one is installed, a request goes nowhere. Every trust decision drawn here is read from `AppState::trust`, which the
//! client reads from the core; nothing here caches a verdict.

pub mod compare;
pub mod conversation;
pub mod home;
pub mod keys;
pub mod picker;
pub mod privacy;

use std::rc::Rc;

use dilla_client::state::ChannelId;
use gpui::{AnyElement, App, Window};

use crate::slots::{SectionId, SlotCx};

pub use compare::compare_overlay;
pub use picker::picker_overlay;

/// Where the area asks the shell to go.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Nav {
    /// Settings → Privacy (a trust alert's Compare).
    Privacy,
    /// Settings → Devices (the own-account banner's action).
    Devices,
    /// The DM, open (after the picker opened or made it).
    Dm(ChannelId),
    /// A server's channel, open (an OS notification's click, GPUI parity Package 4).
    Channel {
        community: dilla_client::state::CommunityId,
        channel: ChannelId,
    },
}

/// The shell's navigation, as the area calls it.
pub type Navigator = Rc<dyn Fn(Nav, &mut Window, &mut App)>;

/// One window's shell navigation (final review sweep: per window, so a DM opened in one window never moves another
/// window's shell).
struct NavigatorOf(Navigator);

/// Installs the shell's navigation for `window` (the shell calls this as it draws).
pub fn set_navigator(window: gpui::WindowId, navigator: Navigator, cx: &mut App) {
    crate::per_window::set(window, Some(NavigatorOf(navigator)), cx);
}

/// Asks this window's shell to go to `to`.
pub fn navigate(to: Nav, window: &mut Window, cx: &mut App) {
    let id = window.window_handle().window_id();
    if let Some(nav) = crate::per_window::get::<NavigatorOf>(id, cx).map(|g| g.0.clone()) {
        nav(to, window, cx);
    }
}

/// Draws one of this area's settings sections (`slots::settings_sections` points here): Privacy.
pub fn settings_section(
    _section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    privacy::section(ctx, window, cx)
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::rc::Rc;

    use super::*;

    /// Final review sweep: the DM picker and the safety-number comparison opened in window A are A's: window B shows
    /// neither, and B's close leaves A's open.
    #[gpui::test]
    fn the_dm_picker_and_the_comparison_stay_in_their_window(cx: &mut gpui::TestAppContext) {
        let a = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        let b = cx
            .add_empty_window()
            .update(|w, _| w.window_handle().window_id());
        cx.update(|cx| {
            picker::open(a, None, None, cx);
            let record = dilla_client::state::trust::TrustSummary {
                user_id: dilla_client::state::UserId([1; 16]),
                name: "Ada".into(),
                shared: true,
                umk: "01".repeat(32),
                first_seen: 1,
                verified: false,
                changed_umk: None,
                unlisted: false,
                removed_in: Vec::new(),
                blocked: None,
            };
            compare::open(a, &record, None, cx);
            assert!(picker::model_in(a, cx).open && compare::model_in(a, cx).open);
            assert!(!picker::model_in(b, cx).open, "B shows no picker");
            assert!(!compare::model_in(b, cx).open, "B shows no comparison");
            picker::close(b, cx);
            compare::close(b, cx);
            assert!(picker::model_in(a, cx).open && compare::model_in(a, cx).open);
        });
    }

    /// Final review sweep: each window's shell installs its own navigation: a DM opened in window A moves A's shell,
    /// never B's (the last one drawn).
    #[gpui::test]
    fn each_windows_dm_navigation_moves_its_own_shell(cx: &mut gpui::TestAppContext) {
        let went: Rc<RefCell<Vec<&'static str>>> = Rc::default();
        let a = cx.add_empty_window();
        let a_went = went.clone();
        a.update(|window, cx| {
            set_navigator(
                window.window_handle().window_id(),
                Rc::new(move |_, _, _| a_went.borrow_mut().push("a")),
                cx,
            );
        });
        let b = cx.add_empty_window();
        let b_went = went.clone();
        b.update(|window, cx| {
            set_navigator(
                window.window_handle().window_id(),
                Rc::new(move |_, _, _| b_went.borrow_mut().push("b")),
                cx,
            );
        });
        let a = cx.windows()[0];
        a.update(cx, |_, window, cx| {
            navigate(Nav::Privacy, window, cx);
        })
        .unwrap();
        assert_eq!(*went.borrow(), vec!["a"], "A's shell moved, not B's");
    }
}
