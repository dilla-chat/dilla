//! `factors`: Task 26's slice of [`super::AppState`]: the account's sign-in factors (Settings, Sign-in), as
//! client-core `factorsState` builds it from `GET /v1/auth/factors`.
//!
//! Nothing here is secret: the TOTP secret, the otpauth URI, the backup codes and the step-up tokens are answered to the
//! caller of [`crate::features::factors::Factors`] and never put in this slice.

/// One passkey of the account (credential id in lower-case hex).
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct PasskeyRow {
    pub id: String,
    pub name: String,
    /// Unix seconds.
    pub created: u64,
    /// Unix seconds; `None` when never used.
    pub last_used: Option<u64>,
}

/// What the instance says the account holds.
#[derive(Clone, Debug, PartialEq, Eq, Default)]
pub struct Factors {
    /// The account owes a second factor.
    pub owed: bool,
    /// When the authenticator app was confirmed (unix seconds); `None` without one.
    pub totp: Option<u64>,
    pub passkeys: Vec<PasskeyRow>,
    pub backup_codes_left: u32,
    pub has_password: bool,
    /// When the password last changed (unix seconds), when the instance knows it.
    pub password_changed: Option<u64>,
}

impl Factors {
    /// The account holds a second factor (an app or a passkey).
    pub fn holds_factor(&self) -> bool {
        self.totp.is_some() || !self.passkeys.is_empty()
    }

    /// The number of second factors (the app counts as one; each passkey as one).
    pub fn count(&self) -> usize {
        usize::from(self.totp.is_some()) + self.passkeys.len()
    }
}

/// The slice: `None` until the factors were read (web `useSlice('factors')` undefined).
#[derive(Clone, Debug, Default, PartialEq)]
pub struct FactorState {
    pub factors: Option<Factors>,
}
