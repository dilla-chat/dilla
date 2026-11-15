//! Task 25's components (`components/onboarding/`): the onboarding frame and its step parts, the recovery key, the
//! recovery key field and the add-server dialog's choices. Every file under this directory is Task 25's.
//!
//! They draw only: the host owns the state (the field's `InputState`, the tick, busy) and the automation ids (the
//! desktop wraps what a test presses in `tracked_box`).

pub mod add_server;
pub mod forms;
pub mod frame;
pub mod key_field;
pub mod recovery_key;

pub use add_server::{AddServerChoice, AddServerChoices};
pub use frame::{BrandMark, NavLink, OnboardingField, OnboardingFrame, PrimaryAction, ReadyPanel};
pub use key_field::RecoveryKeyField;
pub use recovery_key::{KeyAck, KeyAction, KeyGroups, RecoveryKeyLayout};
