//! Where each instance's database key lives: the OS keychain ([`OsKeychain`]), or memory for tests
//! ([`MemoryKeychain`]). There is no file fallback: a keychain that fails is an error.
//!
//! A start with no window (an autostart at login) reads keys through [`QuietKeychain`]: never a prompt or an unlock
//! dialog ([`Keychain::get_quietly`]), and never a write.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

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
        let _serial = serial();
        read(account)
    }

    fn set(&self, account: &str, secret: &[u8]) -> Result<(), KeychainError> {
        let _serial = serial();
        Self::entry(account)?
            .set_secret(secret)
            .map_err(unavailable)
    }

    fn delete(&self, account: &str) -> Result<(), KeychainError> {
        let _serial = serial();
        match Self::entry(account)?.delete_credential() {
            Ok(()) | Err(keyring::Error::NoEntry) => Ok(()),
            Err(e) => Err(unavailable(e)),
        }
    }

    /// The Secret Service: only an item that is unlocked already is read. A locked one is `Unavailable`, and its
    /// collection is never asked to unlock (`keyring`'s read calls `Unlock`, which shows the unlock dialog).
    #[cfg(all(
        unix,
        not(any(target_os = "macos", target_os = "ios", target_os = "android"))
    ))]
    fn get_quietly(&self, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        secret_service_quietly(account)
    }

    /// Keychain Services: the read runs with the keychain's user interaction off for the process
    /// (`SecKeychainSetUserInteractionAllowed`), so a locked keychain or an access prompt fails instead of asking.
    /// Every other call here waits while it is off ([`serial`]), so none of them loses its prompt.
    #[cfg(target_os = "macos")]
    fn get_quietly(&self, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        let _serial = serial();
        let _off = security_framework::os::macos::keychain::SecKeychain::disable_user_interaction()
            .map_err(|_| KeychainError::Unavailable)?;
        read(account)
    }

    /// The Credential Manager never asks the person.
    #[cfg(target_os = "windows")]
    fn get_quietly(&self, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        read(account)
    }
}

/// The secret of `account`, through `keyring`.
fn read(account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
    match OsKeychain::entry(account)?.get_secret() {
        Ok(secret) => Ok(Some(Zeroizing::new(secret))),
        Err(keyring::Error::NoEntry) => Ok(None),
        Err(e) => Err(unavailable(e)),
    }
}

/// Every keychain call holds this: on macOS none runs while [`OsKeychain::get_quietly`] has the process's keychain user
/// interaction off (elsewhere it only orders the calls, which are few: a start, a sign-in, a removal).
fn serial() -> std::sync::MutexGuard<'static, ()> {
    static SERIAL: Mutex<()> = Mutex::new(());
    SERIAL
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}

/// The item `keyring` stores for `account` (attributes `service` and `username`), read only when the Secret Service
/// holds it unlocked. More than one match is refused, as `keyring` refuses an ambiguous entry.
#[cfg(all(
    unix,
    not(any(target_os = "macos", target_os = "ios", target_os = "android"))
))]
fn secret_service_quietly(account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
    use secret_service::EncryptionType;
    use secret_service::blocking::SecretService;
    // The service's errors carry no secret bytes; they are dropped, as `keyring`'s are.
    let ss = SecretService::connect(EncryptionType::Dh).map_err(|_| KeychainError::Unavailable)?;
    let found = ss
        .search_items(std::collections::HashMap::from([
            ("service", SERVICE),
            ("username", account),
        ]))
        .map_err(|_| KeychainError::Unavailable)?;
    match (found.unlocked.as_slice(), found.locked.is_empty()) {
        ([], true) => Ok(None),
        ([item], true) => item
            .get_secret()
            .map(|secret| Some(Zeroizing::new(secret)))
            .map_err(|_| KeychainError::Unavailable),
        // Locked (reading it would ask the person to unlock), or ambiguous.
        _ => Err(KeychainError::Unavailable),
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

/// A keychain for a start no one is watching (an autostart at login): every read is the wrapped keychain's
/// [`Keychain::get_quietly`], and it writes and deletes nothing (a start that would store a new key fails instead).
pub struct QuietKeychain(Arc<dyn Keychain>);

impl QuietKeychain {
    pub fn new(inner: Arc<dyn Keychain>) -> Self {
        Self(inner)
    }
}

impl Keychain for QuietKeychain {
    fn get(&self, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        self.0.get_quietly(account)
    }

    fn set(&self, _account: &str, _secret: &[u8]) -> Result<(), KeychainError> {
        Err(KeychainError::Unavailable)
    }

    fn delete(&self, _account: &str) -> Result<(), KeychainError> {
        Err(KeychainError::Unavailable)
    }

    fn get_quietly(&self, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        self.0.get_quietly(account)
    }
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

    /// Memory never asks anyone.
    fn get_quietly(&self, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        self.get(account)
    }
}
