//! Where each instance's database key lives: the OS keychain ([`OsKeychain`]), or memory for tests
//! ([`MemoryKeychain`]). There is no file fallback: a keychain that fails is an error.

use std::collections::BTreeMap;
use std::sync::Mutex;

use zeroize::{Zeroize, Zeroizing};

use crate::seams::{Keychain, KeychainError};

/// The keychain service every preview-client secret is stored under (never the Electron app's).
pub const SERVICE: &str = "dev.dilla.desktop.preview";

/// The platform credential store: Keychain Services on macOS, Credential Manager on Windows, the
/// Secret Service on Linux. Every platform error is reported as [`KeychainError::Unavailable`];
/// the underlying error is dropped unprinted because some of its variants carry the secret's bytes.
#[derive(Clone, Copy, Debug, Default)]
pub struct OsKeychain;

impl OsKeychain {
    pub fn new() -> Self {
        Self
    }

    fn entry(account: &str) -> Result<keyring::Entry, KeychainError> {
        keyring::Entry::new(SERVICE, account).map_err(unavailable)
    }
}

impl Keychain for OsKeychain {
    fn get(&self, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        match Self::entry(account)?.get_secret() {
            Ok(secret) => Ok(Some(Zeroizing::new(secret))),
            Err(keyring::Error::NoEntry) => Ok(None),
            Err(e) => Err(unavailable(e)),
        }
    }

    fn set(&self, account: &str, secret: &[u8]) -> Result<(), KeychainError> {
        Self::entry(account)?
            .set_secret(secret)
            .map_err(unavailable)
    }

    fn delete(&self, account: &str) -> Result<(), KeychainError> {
        match Self::entry(account)?.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(unavailable(e)),
        }
    }
}

/// Maps a platform error to `Unavailable`, wiping the bytes the decoding variants carry.
fn unavailable(e: keyring::Error) -> KeychainError {
    match e {
        keyring::Error::BadEncoding(mut bytes) | keyring::Error::BadDataFormat(mut bytes, _) => {
            bytes.zeroize()
        }
        _ => {}
    }
    KeychainError::Unavailable
}

/// An in-process keychain for tests. Its `Debug` prints account names only.
#[derive(Default)]
pub struct MemoryKeychain {
    entries: Mutex<BTreeMap<String, Zeroizing<Vec<u8>>>>,
}

impl MemoryKeychain {
    pub fn new() -> Self {
        Self::default()
    }

    /// The accounts that hold a secret, in order.
    pub fn accounts(&self) -> Vec<String> {
        self.lock().keys().cloned().collect()
    }

    fn lock(&self) -> std::sync::MutexGuard<'_, BTreeMap<String, Zeroizing<Vec<u8>>>> {
        // A panic while holding the lock cannot leave the map half-updated.
        self.entries.lock().unwrap_or_else(|p| p.into_inner())
    }
}

impl std::fmt::Debug for MemoryKeychain {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("MemoryKeychain")
            .field("accounts", &self.accounts())
            .finish()
    }
}

impl Keychain for MemoryKeychain {
    fn get(&self, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        Ok(self.lock().get(account).cloned())
    }

    fn set(&self, account: &str, secret: &[u8]) -> Result<(), KeychainError> {
        self.lock()
            .insert(account.to_owned(), Zeroizing::new(secret.to_vec()));
        Ok(())
    }

    fn delete(&self, account: &str) -> Result<(), KeychainError> {
        self.lock().remove(account);
        Ok(())
    }
}
