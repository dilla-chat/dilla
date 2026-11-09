//! The MLS layer: the storage provider, the provider composition, dilla's group-context binding,
//! the group configuration and the transactional group wrapper.
//!
//! **Target gating — read before adding a module here.** `provider`, `storage` and `tx` (and, from
//! task 10, `config` and `group`) are built on `rusqlite`, which `core/dilla-core/Cargo.toml`
//! declares for exactly two targets: native (`cfg(not(target_arch = "wasm32"))`) and the browser
//! (`cfg(all(target_arch = "wasm32", target_os = "unknown"))`). `wasm32-wasip1` is
//! `target_arch = "wasm32"` with `target_os = "wasi"`, so it matches **neither** and has no
//! `rusqlite` in its graph at all — an ungated `mod storage;` there is
//! `error[E0433]: failed to resolve: use of undeclared crate or module rusqlite`. The cfg below is
//! written to be the same condition as those two dependency sections.
//!
//! Everything else in this module is target-agnostic and must stay ungated: `public_group` — the
//! module the whole wasi tier exists for — uses `CborCodec`, `DillaCodec` and `StorageError` from
//! here, and task 10's `DillaBinding`, `GroupKind` and the policy tables are what `dilla-core-wasi`
//! validates against.

mod binding;
mod policy;

#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
mod config;
#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
mod group;
#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
mod provider;
#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
mod storage;
#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
mod tx;

pub use binding::{
    DILLA_BINDING, DILLA_BINDING_ID, DillaBinding, GroupKind, external_senders,
    instance_credential, instance_credential_identity, instance_sender_index,
};
pub use policy::{
    CALL_SWEEP, PAST_EPOCHS_CALL, PAST_EPOCHS_TEXT, TEXT_SWEEP, past_epoch_policy,
    past_epoch_sweep, validate_staged_commit,
};
pub(crate) use policy::{
    commit_extensions_unchanged, leaf_credentials_unchanged, proposal_credential_verdict,
    proposal_extensions_unchanged,
};

#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
pub use config::{
    CIPHERSUITE, INACTIVITY_REMOVE_DAYS, KEY_PACKAGE_LIFETIME_DAYS, MAX_ADDS_PER_COMMIT,
    PADDING_SIZE, build_key_package, build_key_package_with_expiry, create_config,
    group_context_extensions, join_config, leaf_capabilities, rotate_external_senders_extensions,
};
#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
pub use group::{
    CommitBundle, DillaGroup, DillaProcessed, MediaEpoch, MlsError, ReceivedApplication,
    RosterEntry, WelcomeLabel,
};
#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
pub use provider::DillaProvider;
#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
pub use storage::{ConnHandle, DillaStorage};
#[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
pub use tx::{TxError, UnitScope};

use serde::{Serialize, de::DeserializeOwned};

/// Everything the storage layer can fail with.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum StorageError {
    #[error("sqlite: {0}")]
    Sqlite(String),
    #[error("codec: {0}")]
    Codec(String),
    #[error("lock poisoned")]
    Poisoned,
    /// The file was written by a build whose `StorageProvider` layout or serde codec is not this
    /// one's. Returned by `DillaStorage::migrate` before anything is read through it: every blob
    /// in `openmls_*` is shaped by the provider version and encoded by the codec, so opening such
    /// a file would decode another build's bytes with this build's expectations.
    #[error(
        "storage written by provider version {version} with codec {codec}; this build writes version 1 with codec cbor"
    )]
    UnsupportedStorage { version: String, codec: String },
    /// The app schema in `app_meta` is newer than the highest this build migrates to: a newer
    /// client wrote the store (a rollback). Returned before anything is written to it, so the
    /// newer client still finds its store as it left it.
    ///
    /// The numbers are `u32` (a `found` above it reads as `u32::MAX`): a `u64` field would raise the
    /// enum's alignment to 8 on wasm32 and re-lay every `Result<_, StorageError>` (about 11 kB).
    #[error("app schema {found} is newer than this build, which supports up to {supported}")]
    StoreNewer { found: u32, supported: u32 },
}

/// The serde codec the `StorageProvider` blobs use.
///
/// OpenMLS 0.9.0 **requires a self-describing format** (gap-6 section 2): `MlsGroupJoinConfig`
/// deserialises through `deserialize_any`, so postcard, bincode and every other positional binary
/// format is excluded, and the failure only shows up on a reload. The `0-8-1-storage-format`
/// feature stays off and is inert under a self-describing codec.
pub trait DillaCodec: Default {
    type Error: core::fmt::Debug + std::error::Error + Send + Sync + 'static;
    fn to_vec<T: Serialize>(value: &T) -> Result<Vec<u8>, Self::Error>;
    fn from_slice<T: DeserializeOwned>(bytes: &[u8]) -> Result<T, Self::Error>;
}

#[derive(Debug, thiserror::Error)]
pub enum CodecError {
    #[error("serialize: {0}")]
    Serialize(String),
    #[error("deserialize: {0}")]
    Deserialize(String),
}

/// CBOR through `ciborium`. `ciborium` splits (de)serialisation into two error types, which is why
/// `CodecError` has two variants rather than passing one through.
///
/// This codec is used **only** for OpenMLS's own serde blobs. Every normative dilla format goes
/// through `crate::cbor`, whose decoder is strict; `ciborium`'s is deliberately liberal and must
/// never see an envelope, a credential identity, a device list or a `dilla_binding`.
#[derive(Default)]
pub struct CborCodec;

impl DillaCodec for CborCodec {
    type Error = CodecError;

    fn to_vec<T: Serialize>(value: &T) -> Result<Vec<u8>, Self::Error> {
        let mut buf = Vec::new();
        ciborium::into_writer(value, &mut buf).map_err(|e| CodecError::Serialize(e.to_string()))?;
        Ok(buf)
    }

    fn from_slice<T: DeserializeOwned>(bytes: &[u8]) -> Result<T, Self::Error> {
        ciborium::from_reader(bytes).map_err(|e| CodecError::Deserialize(e.to_string()))
    }
}

/// Local stand-ins for the OpenMLS types every storage method is generic over, shared by the unit
/// tests of `mls::storage`, `mls::tx` and `public_group::storage`.
///
/// The 24 marker traits of `openmls_traits::storage::traits` are implemented by OpenMLS for its
/// own concrete types (facts-openmls.md section 2.4; gap-1 section 5 lists which concrete type
/// backs each one). `openmls_traits 0.6.0` carries exactly one blanket impl in `src/storage.rs`
/// — `impl<const VERSION: u16> Entity<VERSION> for Vec<u8>` — and no blanket impl of any marker
/// trait, so `Vec<u8>`, `u32` and `String` satisfy none of them; and dilla cannot add the impls,
/// because a foreign trait on a foreign type is E0117. These types are local, so they can.
///
/// `ProposalRef` is a Key **and** an Entity (gap-1 section 5), which is why `TKey` is both.
///
/// `allow(dead_code)`: the two unit-test modules that use these are gated
/// `#[cfg(not(target_arch = "wasm32"))]`, so on `wasm32-unknown-unknown` (where the crate's tests
/// still compile — step 7's `cargo test --no-run --target wasm32-unknown-unknown`) nothing
/// constructs them and every field is a `dead_code` warning. The module is kept ungated so
/// task 11's `public_group` tests can share it on every target.
#[cfg(test)]
#[allow(dead_code)]
pub(crate) mod test_entities {
    use openmls_traits::storage::{CURRENT_VERSION, Entity, Key, traits};
    use serde::{Deserialize, Serialize};

    #[derive(Clone, PartialEq, Eq, Debug, Serialize, Deserialize)]
    pub(crate) struct TKey(pub Vec<u8>);

    impl Key<CURRENT_VERSION> for TKey {}
    impl Entity<CURRENT_VERSION> for TKey {}
    impl traits::GroupId<CURRENT_VERSION> for TKey {}
    impl traits::ProposalRef<CURRENT_VERSION> for TKey {}
    impl traits::SignaturePublicKey<CURRENT_VERSION> for TKey {}
    impl traits::EncryptionKey<CURRENT_VERSION> for TKey {}
    impl traits::HashReference<CURRENT_VERSION> for TKey {}
    impl traits::PskId<CURRENT_VERSION> for TKey {}
    impl traits::EpochKey<CURRENT_VERSION> for TKey {}

    #[derive(Clone, PartialEq, Eq, Debug, Serialize, Deserialize)]
    pub(crate) struct TVal(pub u32);

    impl Entity<CURRENT_VERSION> for TVal {}
    impl traits::TreeSync<CURRENT_VERSION> for TVal {}
    impl traits::InterimTranscriptHash<CURRENT_VERSION> for TVal {}
    impl traits::GroupContext<CURRENT_VERSION> for TVal {}
    impl traits::ConfirmationTag<CURRENT_VERSION> for TVal {}
    impl traits::GroupState<CURRENT_VERSION> for TVal {}
    impl traits::MessageSecrets<CURRENT_VERSION> for TVal {}
    impl traits::ResumptionPskStore<CURRENT_VERSION> for TVal {}
    impl traits::LeafNodeIndex<CURRENT_VERSION> for TVal {}
    impl traits::GroupEpochSecrets<CURRENT_VERSION> for TVal {}
    impl traits::MlsGroupJoinConfig<CURRENT_VERSION> for TVal {}
    impl traits::QueuedProposal<CURRENT_VERSION> for TVal {}
    impl traits::LeafNode<CURRENT_VERSION> for TVal {}
    impl traits::SignatureKeyPair<CURRENT_VERSION> for TVal {}
    impl traits::HpkeKeyPair<CURRENT_VERSION> for TVal {}
    impl traits::PskBundle<CURRENT_VERSION> for TVal {}
    impl traits::KeyPackage<CURRENT_VERSION> for TVal {}
}
