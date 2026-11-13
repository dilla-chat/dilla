//! Area **onboarding** (Task 25): enrolling this device by password and recovery key.
//!
//! Ports: packages/client-core/src/account/enrol.ts (its key checks), packages/client-core/src/recovery-key.ts.
//!
//! It runs before a [`crate::Client`] exists (like [`crate::signin`]), so it has no `Client` accessor. The ceremony
//! itself (login, the code it owes, the registration and the recovery from the sealed backup) is
//! [`crate::signin::SignIn`]'s; [`Enrol`] drives it with the recovery key as the person typed or pasted it: any case,
//! with hyphens, spaces or dashes, I and L for 1 and O for 0 (the core's `recovery_key_normalise`), and refuses a key of
//! the wrong form before it costs a device registration (REGISTRATION-DEVICES-02, the core's `recovery_key_check`).
//!
//! The typed key is held only in [`Zeroizing`] buffers and never reaches a log, an error or a `Debug`.

use std::sync::Arc;

use zeroize::Zeroizing;

use crate::features::signup::normalise_recovery_key;
use crate::seams::{InstanceConfig, Keychain};
use crate::signin::{Owed, SignIn, SignInError};

/// A recovery key has this many characters once normalised.
pub const RECOVERY_KEY_LEN: usize = 52;

/// What the key field says while the person types (the key step's length line): how many of the 52 characters are
/// there, and whether the key has the form of a recovery key (only the form: whether it opens the account is known
/// only after the backup is fetched).
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct KeyCheck {
    pub count: usize,
    pub well_formed: bool,
}

/// The live check of a typed key.
pub fn check_key(typed: &str) -> KeyCheck {
    let key = normalise_recovery_key(typed);
    let count = key.chars().count();
    KeyCheck {
        count,
        well_formed: count == RECOVERY_KEY_LEN
            && dilla_core::client::recovery_key_check(&key).is_ok(),
    }
}

/// One enrolment of this device: a [`SignIn`] whose last step takes the key as typed.
pub struct Enrol {
    flow: SignIn,
}

impl std::fmt::Debug for Enrol {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Enrol").field("flow", &self.flow).finish()
    }
}

impl Enrol {
    /// An enrolment against `cfg`'s instance; nothing is sent yet.
    pub fn new(cfg: InstanceConfig, kc: Arc<dyn Keychain>) -> Result<Self, SignInError> {
        Ok(Self {
            flow: SignIn::new(cfg, kc)?,
        })
    }

    /// The instance this enrolment is for.
    pub fn config(&self) -> &InstanceConfig {
        self.flow.config()
    }

    /// enrol.ts `login`: what the password login still owes.
    pub async fn login(&mut self, username: &str, password: &str) -> Result<Owed, SignInError> {
        self.flow.login(username, password).await
    }

    /// enrol.ts `totp`: the authenticator code the login owes.
    pub async fn code(&mut self, code: &str) -> Result<(), SignInError> {
        self.flow.code(code).await
    }

    /// enrol.ts `register`, `fetch` and `complete` with the typed key: refused as [`SignInError::KeyLength`] (wrong
    /// length) or [`SignInError::WrongKey`] (not a key) before any registration; a well-formed key that does not open
    /// the account's backup is [`SignInError::WrongKey`] after it. On success the store holds a ready identity.
    pub async fn add_device(&mut self, typed_key: &str) -> Result<(), SignInError> {
        let key: Zeroizing<String> = normalise_recovery_key(typed_key);
        let check = check_key(&key);
        if check.count != RECOVERY_KEY_LEN {
            return Err(SignInError::KeyLength);
        }
        if !check.well_formed {
            return Err(SignInError::WrongKey);
        }
        self.flow.add_device(&key).await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn typed_keys_are_counted_and_checked_by_form() {
        let key = "ABCD".repeat(13);
        assert_eq!(
            check_key(&key),
            KeyCheck {
                count: 52,
                well_formed: dilla_core::client::recovery_key_check(&key).is_ok()
            }
        );
        let lower_hyphens: String = key
            .to_lowercase()
            .as_bytes()
            .chunks(4)
            .map(|c| std::str::from_utf8(c).unwrap())
            .collect::<Vec<_>>()
            .join("-");
        assert_eq!(check_key(&lower_hyphens), check_key(&key));
        assert_eq!(check_key("abcd").count, 4);
        assert!(!check_key("abcd").well_formed);
        // U is not in Crockford base32: right length, wrong form.
        assert!(!check_key(&"U".repeat(52)).well_formed);
    }
}
