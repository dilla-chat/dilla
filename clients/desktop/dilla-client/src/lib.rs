//! The desktop client's account, HTTP and gateway layer. Everything the UI and the media crate reach it through
//! is a trait in [`seams`].
#![forbid(unsafe_code)]

pub mod gateway;
pub mod http;
pub mod instances;
pub mod keychain;
pub mod seams;
pub mod store;
