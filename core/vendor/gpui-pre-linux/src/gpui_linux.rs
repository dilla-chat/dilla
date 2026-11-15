#![cfg(any(target_os = "linux", target_os = "freebsd"))]
mod linux;

pub use linux::{current_platform, linux_platform};
// DILLA PATCH: AltGr's state (linux/platform.rs).
pub use linux::{level3_shift_active, set_level3_shift_active};
