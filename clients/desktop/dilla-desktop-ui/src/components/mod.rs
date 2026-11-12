//! The base components, each the counterpart of a `packages/ui` component and styled from `tokens` only.
//!
//! Every component takes an `id` (the constructor's first argument) that becomes its GPUI element id, stable across renders, for
//! automation and tests. Components are `RenderOnce` and hold no state.

pub mod avatar;
pub mod badge;
pub mod button;
pub mod row;

pub use avatar::{Avatar, AvatarShape, AvatarSize, Presence};
pub use badge::{Badge, BadgeKind};
pub use button::{Button, ButtonSize, ButtonVariant};
pub use row::{Row, RowKind};
