//! The desktop client's account, HTTP and gateway layer. Everything the UI and the media crate reach it through
//! is a trait in [`seams`].
//!
//! The module list is Task 1's (clients/desktop/OWNERSHIP.md): every module the screens plan's tasks create is declared
//! here already, so no task adds a line.
#![forbid(unsafe_code)]

pub mod account;
pub mod auth;
pub mod calls;
pub mod client;
pub mod features;
pub mod gateway;
pub mod http;
pub mod instances;
pub mod keychain;
pub mod prefs;
pub mod seams;
pub mod signin;
pub mod state;
pub mod store;
pub mod sync;
#[cfg(feature = "testing")]
pub mod testing;

pub use client::{Client, ClientError};
pub use features::Area;
pub use prefs::{Prefs, PrefsError};
