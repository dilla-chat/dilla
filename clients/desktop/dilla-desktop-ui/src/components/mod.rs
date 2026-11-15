//! The base components, each the counterpart of a `packages/ui` component and styled from `tokens` only.
//!
//! Every component takes an `id` (the constructor's first argument) that becomes its GPUI element id, stable across renders, for
//! automation and tests. Components are `RenderOnce` and hold no state.

// The shared components (Task 5's base set; Task 6's).
pub mod action_menu;
pub mod audio_meter;
pub mod avatar;
pub mod badge;
pub mod banner;
pub mod button;
pub mod check_list;
pub mod choice_cards;
pub mod confirm_dialog;
pub mod dialog;
pub mod empty_state;
pub mod icon_button;
pub mod key_hint;
pub mod link_banner;
pub mod pill;
pub mod qr_code;
pub mod row;
pub mod segmented;
pub mod select;
pub mod settings;
pub mod state_dot;
pub mod tag;
pub mod text_field;
pub mod toast;
pub mod toggle;
pub mod tooltip;
pub mod trailing_slot;
// One directory per area (clients/desktop/OWNERSHIP.md): each is its task's. The list is Task 1's; no task adds a line.
pub mod attachments;
pub mod bot_admin;
pub mod commands;
pub mod dms;
pub mod factor;
pub mod members;
pub mod messages;
pub mod onboarding;
pub mod reactions;
pub mod ring;
pub mod search;
pub mod server_settings;
pub mod shell;
pub mod threads;
pub mod voice;
pub mod voice_settings;

pub use avatar::{Avatar, AvatarShape, AvatarSize, Presence};
pub use badge::{Badge, BadgeKind};
pub use button::{Button, ButtonSize, ButtonVariant};
pub use row::{Row, RowKind};
