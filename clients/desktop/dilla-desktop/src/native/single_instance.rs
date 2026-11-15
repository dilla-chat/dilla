//! One process for all instances (Package 6, decision D2; beta.5 `requestSingleInstanceLock`, `second-instance`).
//!
//! To do (Package 6): [`claim`] takes a per-user lock (a socket of mode 0600 in the data root, or a per-user named pipe
//! on Windows); a second launch sends its argv there and answers [`Claim::HandedOver`]. [`install`] listens on the
//! [`Primary`]'s endpoint and accepts only argv-shaped messages from the same user: a link goes to
//! [`super::NativeShell::open_link`] (parsed with [`super::links::parse`]), anything else calls
//! [`super::NativeShell::show_app`].

use gpui::App;

/// This process holds the lock: the lock and the endpoint second launches talk to. Task 0: nothing held.
#[derive(Debug, Default)]
pub struct Primary {}

/// What [`claim`] found.
#[derive(Debug)]
pub enum Claim {
    /// This process is the app.
    Primary(Primary),
    /// The running app took this launch's argv: this process exits.
    HandedOver,
}

/// Before the run loop, with the whole command line (the program name first). Task 0: always the app.
pub fn claim(_argv: &[String]) -> Claim {
    Claim::Primary(Primary::default())
}

/// Called once by [`super::install`], last, with what [`claim`] took. Task 0: nothing yet.
pub fn install(_primary: Primary, _cx: &mut App) {}
