//! Area **diagnostics** (Task 24): the Diagnostics section, the offline link banner and the version-refused banner.
//!
//! Every file under `screens/diagnostics/` is Task 24's. The hooks below are where `crate::slots` draws this area inside
//! another area's view; their signatures are Task 1's.
//!
//! - [`banners`]: the link line and the version refusals (`banner.rs`), over the shell.
//! - [`settings_section`]: the Diagnostics section (`section.rs`), in the settings frame.
//!
//! The boot splash and the fatal screen are `crate::screens::boot`.

pub mod banner;
pub mod keys;
pub mod section;

use crate::slots::{SectionId, SlotCx};
use gpui::{AnyElement, App, Window};

/// The banners over the main column (`slots::banners`): the offline link and the version refusal.
pub fn banners(ctx: &SlotCx, window: &mut Window, cx: &mut App) -> Vec<AnyElement> {
    banner::banners(ctx, window, cx)
}

/// Draws one of this area's settings sections (`slots::settings_sections` points here).
pub fn settings_section(
    _section: SectionId,
    ctx: &SlotCx,
    window: &mut Window,
    cx: &mut App,
) -> AnyElement {
    section::render(ctx, window, cx)
}
