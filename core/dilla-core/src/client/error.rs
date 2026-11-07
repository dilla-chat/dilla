//! Stable client error codes at the caller boundary.

use crate::ProtocolError;
use crate::mls::{DillaStorage, MlsError, StorageError, TxError};

pub(crate) const E_CORE_INPUT: &str = "E_CORE_INPUT";
pub(crate) const E_CORE_LIMIT: &str = "E_CORE_LIMIT";
pub(crate) const E_CREDENTIAL: &str = "E_CREDENTIAL";
pub(crate) const E_UMK_CHANGED: &str = "E_UMK_CHANGED";
pub(crate) const E_DEVICE_UNLISTED: &str = "E_DEVICE_UNLISTED";
/// A device its user's newest signed list names with `revoked_at` set: removed, not never added.
pub(crate) const E_DEVICE_REVOKED: &str = "E_DEVICE_REVOKED";
pub(crate) const E_DEVICE_LIST_STALE: &str = "E_DEVICE_LIST_STALE";
pub(crate) const E_TIER_MISMATCH: &str = "E_TIER_MISMATCH";
pub(crate) const E_CORE_STATE: &str = "E_CORE_STATE";
pub(crate) const E_CORE_NO_IDENTITY: &str = "E_CORE_NO_IDENTITY";
pub(crate) const E_CORE_NOT_FOUND: &str = "E_CORE_NOT_FOUND";
pub(crate) const E_CORE_MLS: &str = "E_CORE_MLS";
pub(crate) const E_CORE_STORAGE: &str = "E_CORE_STORAGE";
pub(crate) const E_CORE_RELOAD: &str = "E_CORE_RELOAD";
pub(crate) const E_RECOVERY_KEY: &str = "E_RECOVERY_KEY";
pub(crate) const E_BINDING: &str = "E_BINDING";
/// The store was written by a newer client (`StorageError::StoreNewer`); the detail is exactly
/// `found <n>, supported <m>`, which the worker reads at the core's boundary.
pub(crate) const E_STORE_NEWER: &str = "E_STORE_NEWER";
/// A verifying account-kind attestation that names another kind than the one stored for the user (task 20 fix round 2,
/// N3): an account's kind is final once stored, so the change is refused (and the worker logs the code).
pub(crate) const E_ACCOUNT_KIND_CHANGED: &str = "E_ACCOUNT_KIND_CHANGED";

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ClientError {
    pub code: &'static str,
    pub detail: String,
}

impl ClientError {
    pub(crate) fn new(code: &'static str, detail: impl Into<String>) -> Self {
        Self {
            code,
            detail: detail.into(),
        }
    }
}

impl core::fmt::Display for ClientError {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        if self.detail.is_empty() {
            f.write_str(self.code)
        } else {
            write!(f, "{}: {}", self.code, self.detail)
        }
    }
}
impl std::error::Error for ClientError {}

impl From<ProtocolError> for ClientError {
    fn from(e: ProtocolError) -> Self {
        Self::new(e.code(), "")
    }
}
impl From<MlsError> for ClientError {
    fn from(e: MlsError) -> Self {
        match e {
            MlsError::Protocol(e) => e.into(),
            MlsError::Storage(e) => e.into(),
            MlsError::Tx(s) => Self::new(E_CORE_STORAGE, s),
            MlsError::OpenMls(s) => Self::new(E_CORE_MLS, s),
            MlsError::NeedsReload => Self::new(E_CORE_RELOAD, ""),
            MlsError::NotFound => Self::new(E_CORE_NOT_FOUND, ""),
            MlsError::WelcomeLabel => Self::new(
                E_CORE_INPUT,
                "the Welcome's epoch or tree hash is not the served label",
            ),
        }
    }
}
impl From<StorageError> for ClientError {
    fn from(e: StorageError) -> Self {
        storage_error(e)
    }
}

/// The one mapping of a storage error, out of line so that each `?` carries one call instead of
/// its own copy of the match (wasm size).
///
/// An OpenMLS store a newer build wrote (`UnsupportedStorage` with this codec and a provider
/// version above this build's) is `E_STORE_NEWER` too, with the provider versions as its numbers.
/// Another codec, or a version that is not a newer number, stays `E_CORE_STORAGE`.
#[inline(never)]
fn storage_error(e: StorageError) -> ClientError {
    let newer = |found: u32, supported: u32| {
        ClientError::new(
            E_STORE_NEWER,
            format!("found {found}, supported {supported}"),
        )
    };
    match e {
        StorageError::StoreNewer { found, supported } => newer(found, supported),
        StorageError::UnsupportedStorage {
            ref version,
            ref codec,
        } if codec == DillaStorage::CODEC => {
            match (
                version.parse::<u32>(),
                DillaStorage::PROVIDER_VERSION.parse::<u32>(),
            ) {
                (Ok(found), Ok(supported)) if found > supported => newer(found, supported),
                _ => ClientError::new(E_CORE_STORAGE, e.to_string()),
            }
        }
        other => ClientError::new(E_CORE_STORAGE, other.to_string()),
    }
}
impl From<TxError<ClientError>> for ClientError {
    fn from(e: TxError<ClientError>) -> Self {
        match e {
            TxError::RolledBack(e) => e,
            other => Self::new(E_CORE_STORAGE, other.to_string()),
        }
    }
}
