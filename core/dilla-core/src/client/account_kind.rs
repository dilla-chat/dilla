//! The instance-signed account-kind attestation (protocol/03 § Account kind attestation, Task S1
//! part 2; Ruling AW: independent of the key).
//!
//! A leaf credential's `kind` is signed by its own user's SSK, so it is the user's claim. The
//! instance states each account's kind in an attestation signed with its external-sender key,
//! which this client pinned (`schema::INSTANCE_SIGNER_KEY`; a managed Bot's store pins the same
//! key as `schema::BOT_INSTANCE_KEY`, see `pinned_instance`):
//!
//! ```text
//! message     = "dilla account kind v1" || CBOR([1, instance_id(16), user_id(16), kind])
//! attestation = CBOR([1, key_id(16), kind, sig(64)])
//! ```
//!
//! An account's kind never changes, so the attestation binds the instance, the user and the kind,
//! not a key: it applies to every key of that user. Key trust (pins, UMK change detection, the
//! person's verification) is separate and unchanged.
//!
//! `account_kind_ingest` verifies an attestation against the pinned instance key, this store's
//! instance id and the user id, stores it (one row per user) and records that this client has seen
//! one from its instance. `leaf_kind_verdict`, called from `trust::leaf_verdict`, judges every leaf:
//! with a verifying stored attestation the credential's kind must be the attested kind; without
//! one, a client that has seen an attestation leaves the leaf pending (`E_DEVICE_LIST_STALE`) and a
//! client that has not keeps the earlier rule (the transition window `protocol/07` describes).

use super::error::{E_CORE_INPUT, E_CORE_STATE, E_CREDENTIAL, E_DEVICE_LIST_STALE};
use super::{ClientCore, ClientError, schema};
use crate::identity::CredentialIdentity;
pub use crate::identity::{AccountKindAttestation, account_kind_message};
use crate::mls::StorageError;
use ed25519_dalek::{Signature, VerifyingKey};
use rusqlite::{OptionalExtension, params};

/// `app_meta` key: this client has verified and stored an attestation from its instance (the
/// per-instance "attestation seen" flag, set by `account_kind_ingest` and never cleared; a store
/// holds one instance's identity).
pub(super) const ACCOUNT_KIND_SEEN: &str = "account_kind_seen";

/// `app_account_kinds` holds one verified attestation per user.
pub(super) const ACCOUNT_KINDS_SCHEMA: &str = "
CREATE TABLE IF NOT EXISTS app_account_kinds (
  user_id BLOB PRIMARY KEY CHECK(length(user_id)=16),
  kind INTEGER NOT NULL CHECK(kind IN (0,1)),
  key_id BLOB NOT NULL CHECK(length(key_id)=16),
  signature BLOB NOT NULL CHECK(length(signature)=64),
  instance_key BLOB NOT NULL CHECK(length(instance_key)=32),
  instance_id BLOB NOT NULL CHECK(length(instance_id)=16)
);
";

/// The pinned instance key and the store's instance id.
type PinnedInstance = ([u8; 32], [u8; 16]);
/// A stored attestation row: kind, signature, instance key, instance id.
type StoredAttestation = (i64, Vec<u8>, Vec<u8>, Vec<u8>);

/// The pinned instance key and this store's instance id, or `None` before either exists.
fn pinned_instance(c: &rusqlite::Connection) -> Result<Option<PinnedInstance>, StorageError> {
    let key = match schema::meta_get(c, schema::INSTANCE_SIGNER_KEY)? {
        Some(k) => Some(k),
        None => schema::meta_get(c, schema::BOT_INSTANCE_KEY)?,
    };
    let Some(key) = key else { return Ok(None) };
    let key: [u8; 32] = key
        .try_into()
        .map_err(|_| StorageError::Codec("malformed pinned instance key".into()))?;
    let Some(identity) = schema::meta_get(c, schema::IDENTITY)? else {
        return Ok(None);
    };
    let record = super::identity::IdentityRecord::decode(&identity)
        .map_err(|_| StorageError::Codec("app_meta identity is malformed".into()))?;
    Ok(Some((key, record.instance_id)))
}

/// Whether `signature` is the instance's statement (under `key`, for `instance`) that `user`'s
/// account is of `kind`.
fn verifies(
    key: &[u8; 32],
    instance: &[u8; 16],
    user: &[u8; 16],
    kind: u8,
    signature: &[u8; 64],
) -> bool {
    VerifyingKey::from_bytes(key).is_ok_and(|k| {
        k.verify_strict(
            &account_kind_message(instance, user, kind),
            &Signature::from_bytes(signature),
        )
        .is_ok()
    })
}

/// The kind a stored row states, when it is the instance's statement about `user`.
fn stored_kind(stored: &StoredAttestation, user: &[u8; 16]) -> Option<u8> {
    let (kind, signature, key, instance) = stored;
    let signature = <[u8; 64]>::try_from(signature.as_slice()).ok()?;
    let key = <[u8; 32]>::try_from(key.as_slice()).ok()?;
    let instance = <[u8; 16]>::try_from(instance.as_slice()).ok()?;
    let kind = u8::try_from(*kind).ok()?;
    verifies(&key, &instance, user, kind, &signature).then_some(kind)
}

/// The leaf's account-kind verdict: `None` when it passes (or the transition window keeps the
/// earlier rule), else the code that refuses it.
pub(super) fn leaf_kind_verdict(
    c: &rusqlite::Connection,
    leaf: &CredentialIdentity,
) -> Result<Option<&'static str>, StorageError> {
    let user = leaf.user_id.as_bytes();
    let row: Option<StoredAttestation> = c
        .query_row(
            "SELECT kind,signature,instance_key,instance_id FROM app_account_kinds WHERE user_id=?1",
            [user.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
        )
        .optional()?;
    if let Some(kind) = row.as_ref().and_then(|r| stored_kind(r, user)) {
        // The instance's statement of the account's kind, whatever key the leaf is under.
        return Ok(if kind == leaf.kind.as_u8() {
            None
        } else {
            Some(E_CREDENTIAL)
        });
    }
    // No verifying attestation: pending once this client has seen one from its instance (fail
    // closed); before that, the earlier rule.
    Ok(if schema::meta_get(c, ACCOUNT_KIND_SEEN)?.is_some() {
        Some(E_DEVICE_LIST_STALE)
    } else {
        None
    })
}

impl ClientCore {
    /// Store the instance's account-kind attestation for `user_id` (the fifth element of a wire-4
    /// device-list head) before that user's signed history is ingested. It is verified first,
    /// against the pinned instance key, this store's instance id and the user id; one that does
    /// not verify, or is malformed, is refused (`E_CORE_INPUT`) and stored nowhere, and it never
    /// replaces a verified one. A verified one sets the "attestation seen" flag; an identical one
    /// is no write (no trust-generation bump).
    pub fn account_kind_ingest(
        &mut self,
        user_id: &[u8; 16],
        attestation: &[u8],
    ) -> Result<(), ClientError> {
        if *user_id == [0; 16] {
            return Err(ClientError::new(E_CORE_INPUT, "user_id is all zero"));
        }
        // A malformed attestation is the server's input error (E_CORE_INPUT), never E_CREDENTIAL:
        // that code is the leaf verdict of a verified attestation of another kind, and a server must
        // not be able to steer a client into showing it.
        let a = AccountKindAttestation::decode(attestation)
            .map_err(|_| ClientError::new(E_CORE_INPUT, "malformed account-kind attestation"))?;
        self.write(|_, u| {
            let (key, instance) = u.with_conn(pinned_instance)?.ok_or_else(|| {
                ClientError::new(E_CORE_STATE, "instance signing key is not pinned")
            })?;
            if !verifies(&key, &instance, user_id, a.kind, &a.signature) {
                return Err(ClientError::new(
                    E_CORE_INPUT,
                    "account-kind attestation does not verify",
                ));
            }
            u.with_conn(|c| {
                let held: Option<StoredAttestation> = c
                    .query_row(
                        "SELECT kind,signature,instance_key,instance_id FROM app_account_kinds WHERE user_id=?1",
                        [user_id.as_slice()],
                        |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
                    )
                    .optional()?;
                let same = held.is_some_and(|(kind, sig, k, i)| {
                    kind == i64::from(a.kind) && sig == a.signature && k == key && i == instance
                });
                let mut changed = false;
                if !same {
                    c.execute(
                        "INSERT INTO app_account_kinds(user_id,kind,key_id,signature,instance_key,instance_id) \
                         VALUES(?1,?2,?3,?4,?5,?6) ON CONFLICT(user_id) DO UPDATE SET kind=excluded.kind, \
                         key_id=excluded.key_id,signature=excluded.signature,instance_key=excluded.instance_key, \
                         instance_id=excluded.instance_id",
                        params![
                            user_id.as_slice(),
                            i64::from(a.kind),
                            a.key_id.as_slice(),
                            a.signature.as_slice(),
                            key.as_slice(),
                            instance.as_slice()
                        ],
                    )?;
                    changed = true;
                }
                if schema::meta_get(c, ACCOUNT_KIND_SEEN)?.is_none() {
                    schema::meta_put(c, ACCOUNT_KIND_SEEN, &[1])?;
                    changed = true;
                }
                if changed {
                    super::trust::bump_generation(c)?;
                }
                Ok(())
            })?;
            Ok(())
        })
    }

    /// Whether this client has seen an account-kind attestation from its instance.
    pub fn account_kind_seen(&self) -> Result<bool, ClientError> {
        self.read(|c| Ok(schema::meta_get(c, ACCOUNT_KIND_SEEN)?.is_some()))
    }
}
