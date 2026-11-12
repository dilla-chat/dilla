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
use gpui::{AnyElement, App, Global, Window};

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
}

/// The shell's navigation, as the area calls it.
pub type Navigator = Rc<dyn Fn(Nav, &mut Window, &mut App)>;

struct NavigatorGlobal(Navigator);
impl Global for NavigatorGlobal {}

/// Installs the shell's navigation (Task 8 calls this once per app).
pub fn set_navigator(cx: &mut App, navigator: Navigator) {
    cx.set_global(NavigatorGlobal(navigator));
}

/// Asks the shell to go to `to`.
pub fn navigate(to: Nav, window: &mut Window, cx: &mut App) {
    if let Some(nav) = cx.try_global::<NavigatorGlobal>().map(|g| g.0.clone()) {
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
