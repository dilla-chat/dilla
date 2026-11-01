//! The instance-signed account-kind attestation (protocol/03-identity.md § Account kind
//! attestation, wire 4):
//!
//! ```text
//! message     = "dilla account kind v1" || CBOR([1, instance_id(16), user_id(16), kind])
//! attestation = CBOR([1, key_id(16), kind, sig(64)])
//! ```
//!
//! The structure lives here, target-agnostic, so the vectors and every binding share it; the
//! client's trust judgement is `client::account_kind`.

use crate::cbor::{Encoder, decode_strict};
use crate::error::ProtocolError;

/// The attestation's signature domain.
pub const DOMAIN_ACCOUNT_KIND: &[u8] = b"dilla account kind v1";

/// A decoded attestation `[1, key_id, kind, sig]`.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct AccountKindAttestation {
    pub key_id: [u8; 16],
    pub kind: u8,
    pub signature: [u8; 64],
}

impl AccountKindAttestation {
    /// Every shape failure (a version other than 1, a kind other than 0 or 1, a wrong length,
    /// trailing bytes, a non-minimal encoding) is `ProtocolError::Credential`; the client maps it
    /// to `E_CORE_INPUT` (`client::account_kind`), never to the leaf verdict `E_CREDENTIAL`.
    /// `key_id` is carried but bound by nothing the signature covers: it names which instance key
    /// signed for diagnostics only and must not be trusted for key rotation.
    pub fn decode(bytes: &[u8]) -> Result<Self, ProtocolError> {
        let (v, key_id, kind, signature) = decode_strict(bytes, |d| {
            d.array(4)?;
            Ok((
                d.uint()?,
                d.bytes_exact::<16>()?,
                d.uint()?,
                d.bytes_exact::<64>()?,
            ))
        })
        .map_err(|_| ProtocolError::Credential)?;
        if v != 1 || kind > 1 {
            return Err(ProtocolError::Credential);
        }
        Ok(Self {
            key_id,
            kind: kind as u8,
            signature,
        })
    }

    pub fn encode(&self) -> Vec<u8> {
        let mut e = Encoder::new();
        e.array(4)
            .uint(1)
            .bytes(&self.key_id)
            .uint(u64::from(self.kind))
            .bytes(&self.signature);
        e.into_vec()
    }
}

/// The signed message of an attestation: the instance, the account and its kind. No key: an
/// account's kind never changes, so the statement holds for every key of the account (Ruling AW).
pub fn account_kind_message(instance_id: &[u8; 16], user_id: &[u8; 16], kind: u8) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(4)
        .uint(1)
        .bytes(instance_id)
        .bytes(user_id)
        .uint(u64::from(kind));
    let mut m = DOMAIN_ACCOUNT_KIND.to_vec();
    m.extend_from_slice(&e.into_vec());
    m
}
