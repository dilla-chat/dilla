//! The desktop client's GPUI views.
#![forbid(unsafe_code)]

pub mod components;
pub mod icon;
#[rustfmt::skip]
pub mod icon_names;
pub mod text;
pub mod theme;
#[rustfmt::skip]
pub mod tokens;

pub use icon::Icon;
pub use icon_names::IconName;
