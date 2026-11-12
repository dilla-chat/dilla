//! The desktop client's account, HTTP and gateway layer. Everything the UI and the media crate reach it through
//! is a trait in [`seams`].
#![forbid(unsafe_code)]

pub mod http;
pub mod seams;
