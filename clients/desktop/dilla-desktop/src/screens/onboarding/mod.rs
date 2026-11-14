//! Area **onboarding** (Task 25): the sign-up steps and enrolling a device.
//!
//! - [`signup::SignupView`]: steps 1 to 3 (the invite, the account, the recovery key) and the closed frame; it emits
//!   [`signup::SignedUp`] once the account exists, and [`signup::WantsSignIn`] for "Already have an account? Sign in".
//! - Steps 4 and 5 are Task 26's factor views, on the running client (the account starts in `FactorSetup`).
//! - [`done::DoneView`]: step 6, after the factor, once [`dilla_client::Client::onboarding`]'s `finish_signup` joined
//!   the invite's server.
//!
//! Enrolling a second device by password and recovery key is the sign-in screen's (Task 26), on
//! `dilla_client::features::enrol::Enrol`; its key field is `components::onboarding::RecoveryKeyField`.
//!
//! The add-server dialog, the create flows and join by invite are [`crate::screens::create`]'s.

pub mod done;
pub mod keys;
pub mod signup;

pub use done::{DoneView, OpenDilla, SignupCodes};
pub use signup::{SignedUp, SignupView, WantsSignIn};

/// Opens the add-server dialog ("join a server" or "new server") in the active window: the rail's "+" (Task 8) calls
/// it. The same as [`crate::screens::create::open_add_server`].
pub fn open_add_server(cx: &mut gpui::App) {
    crate::screens::create::open_add_server(cx);
}

/// [`open_add_server`] in `window`, whichever window is active (final review M11: the rail's "+" of that window).
pub fn open_add_server_in(window: &gpui::Window, cx: &mut gpui::App) {
    crate::screens::create::open_add_server_in(window, cx);
}
