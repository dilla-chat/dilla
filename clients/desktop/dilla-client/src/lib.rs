//! The desktop client's account, HTTP and gateway layer. Everything the UI and the media crate reach it through
//! is a trait in [`seams`].
#![forbid(unsafe_code)]

pub mod auth;
pub mod calls;
pub mod client;
pub mod gateway;
pub mod http;
pub mod instances;
pub mod keychain;
pub mod seams;
pub mod signin;
pub mod state;
pub mod store;
pub mod sync;

pub use client::{Client, ClientError};
