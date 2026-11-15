//! The pre-account facet (Task 28 lane F): what a device with no account shows for a `signed_out` fixture, and the
//! instance's answers its steps got.
//!
//! The web shooter drives these screens against a real dillad: it presses "Already have an account? Sign in", types a
//! login, the instance answers. The automation build has no network, so the screen a fixture names starts where those
//! answers left the web, and each answer goes through the same handler a live one takes (the sign-in's login refusal,
//! its factor owed, the sign-up's created keys). Which screen is the fixture's name, as other facets key on it
//! (`invites`, `gifs`); a `signed_out` fixture this file does not name draws the sign-up's first step, as `/welcome`
//! does on the web.

use dilla_client::testing::spec::ScreenFixture;

/// Where a device with no account starts.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum PreAccount {
    /// The sign-up's step 1, "Join with an invite" (web `/welcome`).
    SignUp,
    /// The sign-up's step 2, the username and password (an invite typed, Continue).
    SignUpIdentity,
    /// The sign-up's step 3, the recovery key (the account's keys made, not yet sent).
    SignUpKeys,
    /// The sign-up's step 6, "You're in", the backup codes collapsed.
    SignUpDone,
    /// The sign-in's login form.
    SignIn,
    /// The sign-in's second step: the instance answered the login with "a code is owed".
    SignInFactor,
    /// The login refused: the instance answered the password with a refusal.
    SignInError,
}

/// The instance the web shooter's browser is served by: dilla-testhost's name (`dilla.test`), which the web's pre-account
/// screens name in their headings.
pub const INSTANCE_NAME: &str = "dilla.test";

/// The backup codes an account's authenticator is given (internal/auth: ten), shown collapsed on the done step.
pub const BACKUP_CODES: usize = 10;

/// The pre-account screen `fixture` starts on.
pub fn start(fixture: &ScreenFixture) -> PreAccount {
    match fixture.name.as_str() {
        "sign-in/form" => PreAccount::SignIn,
        "sign-in/factor" => PreAccount::SignInFactor,
        "sign-in/error" => PreAccount::SignInError,
        "onboarding/identity" => PreAccount::SignUpIdentity,
        "onboarding/keys" => PreAccount::SignUpKeys,
        "onboarding/done" => PreAccount::SignUpDone,
        _ => PreAccount::SignUp,
    }
}

/// The username the viewer typed into the sign-in (the web steps type the viewer's own).
pub fn username(fixture: &ScreenFixture) -> String {
    fixture
        .world
        .people
        .iter()
        .find(|p| p.id == fixture.viewer)
        .map_or(fixture.viewer.as_str(), |p| p.username())
        .to_owned()
}
