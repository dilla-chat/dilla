//! Stable protocol error codes.
//!
//! `code()` returns the exact string the protocol documents publish; those strings cross the wasi
//! ABI, the HTTP API and the gateway, so they are part of the compatibility surface and change
//! only through protocol/07-versioning.md's change process.

/// A rejection named by one of the protocol documents.
#[derive(Clone, Copy, PartialEq, Eq, Debug, thiserror::Error)]
pub enum ProtocolError {
    // protocol/01-groups.md
    #[error("E_BINDING")]
    Binding,
    #[error("E_EXTERNAL_SENDER_FORBIDDEN")]
    ExternalSenderForbidden,
    #[error("E_MEMBER_REMOVE_FORBIDDEN")]
    MemberRemoveForbidden,
    #[error("E_EXTERNAL_COMMIT_REMOVE")]
    ExternalCommitRemove,
    #[error("E_UNSUPPORTED_VERSION")]
    UnsupportedVersion,
    #[error("E_UNSUPPORTED_SUITE")]
    UnsupportedSuite,
    // protocol/03-identity.md
    #[error("E_CREDENTIAL")]
    Credential,
    #[error("E_UMK_CHANGED")]
    UmkChanged,
    #[error("E_DEVICE_UNLISTED")]
    DeviceUnlisted,
    #[error("E_DEVICE_REVOKED")]
    DeviceRevoked,
    #[error("E_DEVICE_LIST_STALE")]
    DeviceListStale,
    #[error("E_PAIRING_LEAVES")]
    PairingLeaves,
    #[error("E_PAIRING_FINGERPRINT")]
    PairingFingerprint,
    #[error("E_TIER_MISMATCH")]
    TierMismatch,
    #[error("E_PROVISIONAL_OUTSIDE_PAIRING")]
    ProvisionalOutsidePairing,
    // protocol/04-envelope-and-franking.md
    #[error("E_ENVELOPE_SHAPE")]
    EnvelopeShape,
    #[error("E_ENVELOPE_TYPE")]
    EnvelopeType,
    #[error("E_ENVELOPE_LIMIT")]
    EnvelopeLimit,
    #[error("E_FRANK_MISMATCH")]
    FrankMismatch,
    #[error("E_BLOB_HASH")]
    BlobHash,
    #[error("E_BLOB_OPEN")]
    BlobOpen,
    // protocol/07-versioning.md
    #[error("E_VERSION")]
    Version,
}

impl ProtocolError {
    pub const fn code(self) -> &'static str {
        match self {
            Self::Binding => "E_BINDING",
            Self::ExternalSenderForbidden => "E_EXTERNAL_SENDER_FORBIDDEN",
            Self::MemberRemoveForbidden => "E_MEMBER_REMOVE_FORBIDDEN",
            Self::ExternalCommitRemove => "E_EXTERNAL_COMMIT_REMOVE",
            Self::UnsupportedVersion => "E_UNSUPPORTED_VERSION",
            Self::UnsupportedSuite => "E_UNSUPPORTED_SUITE",
            Self::Credential => "E_CREDENTIAL",
            Self::UmkChanged => "E_UMK_CHANGED",
            Self::DeviceUnlisted => "E_DEVICE_UNLISTED",
            Self::DeviceRevoked => "E_DEVICE_REVOKED",
            Self::DeviceListStale => "E_DEVICE_LIST_STALE",
            Self::PairingLeaves => "E_PAIRING_LEAVES",
            Self::PairingFingerprint => "E_PAIRING_FINGERPRINT",
            Self::TierMismatch => "E_TIER_MISMATCH",
            Self::ProvisionalOutsidePairing => "E_PROVISIONAL_OUTSIDE_PAIRING",
            Self::EnvelopeShape => "E_ENVELOPE_SHAPE",
            Self::EnvelopeType => "E_ENVELOPE_TYPE",
            Self::EnvelopeLimit => "E_ENVELOPE_LIMIT",
            Self::FrankMismatch => "E_FRANK_MISMATCH",
            Self::BlobHash => "E_BLOB_HASH",
            Self::BlobOpen => "E_BLOB_OPEN",
            Self::Version => "E_VERSION",
        }
    }

    pub fn from_code(s: &str) -> Option<Self> {
        Some(match s {
            "E_BINDING" => Self::Binding,
            "E_EXTERNAL_SENDER_FORBIDDEN" => Self::ExternalSenderForbidden,
            "E_MEMBER_REMOVE_FORBIDDEN" => Self::MemberRemoveForbidden,
            "E_EXTERNAL_COMMIT_REMOVE" => Self::ExternalCommitRemove,
            "E_UNSUPPORTED_VERSION" => Self::UnsupportedVersion,
            "E_UNSUPPORTED_SUITE" => Self::UnsupportedSuite,
            "E_CREDENTIAL" => Self::Credential,
            "E_UMK_CHANGED" => Self::UmkChanged,
            "E_DEVICE_UNLISTED" => Self::DeviceUnlisted,
            "E_DEVICE_REVOKED" => Self::DeviceRevoked,
            "E_DEVICE_LIST_STALE" => Self::DeviceListStale,
            "E_PAIRING_LEAVES" => Self::PairingLeaves,
            "E_PAIRING_FINGERPRINT" => Self::PairingFingerprint,
            "E_TIER_MISMATCH" => Self::TierMismatch,
            "E_PROVISIONAL_OUTSIDE_PAIRING" => Self::ProvisionalOutsidePairing,
            "E_ENVELOPE_SHAPE" => Self::EnvelopeShape,
            "E_ENVELOPE_TYPE" => Self::EnvelopeType,
            "E_ENVELOPE_LIMIT" => Self::EnvelopeLimit,
            "E_FRANK_MISMATCH" => Self::FrankMismatch,
            "E_BLOB_HASH" => Self::BlobHash,
            "E_BLOB_OPEN" => Self::BlobOpen,
            "E_VERSION" => Self::Version,
            _ => return None,
        })
    }

    /// True for the two rejections protocol/03 and /04 mark as hard: a changed user master key
    /// (`E_UMK_CHANGED`) and a franking commitment that does not match the decrypted envelope
    /// (`E_FRANK_MISMATCH`). A hard reject is never retried and never rendered partially.
    pub const fn is_hard_reject(self) -> bool {
        matches!(self, Self::UmkChanged | Self::FrankMismatch)
    }
}

/// Everything `dilla-core` can return to a caller.
#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum CoreError {
    #[error(transparent)]
    Protocol(#[from] ProtocolError),
    #[error(transparent)]
    Cbor(#[from] crate::cbor::CborError),
    #[error("crypto: {0}")]
    Crypto(String),
    /// A Bot codec refusal (sealed commands: a shape, context or size the seal does not allow).
    #[error(transparent)]
    Bot(#[from] crate::bot::CodecError),
    #[error(transparent)]
    Storage(#[from] crate::mls::StorageError),
    #[cfg(any(not(target_arch = "wasm32"), target_os = "unknown"))]
    #[error(transparent)]
    Mls(#[from] crate::mls::MlsError),
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The 22 stable strings of protocol/01, /03, /04 and /07 (facts-repo.md section 1.11).
    const ALL: [(ProtocolError, &str); 22] = [
        (ProtocolError::Binding, "E_BINDING"),
        (
            ProtocolError::ExternalSenderForbidden,
            "E_EXTERNAL_SENDER_FORBIDDEN",
        ),
        (
            ProtocolError::MemberRemoveForbidden,
            "E_MEMBER_REMOVE_FORBIDDEN",
        ),
        (
            ProtocolError::ExternalCommitRemove,
            "E_EXTERNAL_COMMIT_REMOVE",
        ),
        (ProtocolError::UnsupportedVersion, "E_UNSUPPORTED_VERSION"),
        (ProtocolError::UnsupportedSuite, "E_UNSUPPORTED_SUITE"),
        (ProtocolError::Credential, "E_CREDENTIAL"),
        (ProtocolError::UmkChanged, "E_UMK_CHANGED"),
        (ProtocolError::DeviceUnlisted, "E_DEVICE_UNLISTED"),
        (ProtocolError::DeviceRevoked, "E_DEVICE_REVOKED"),
        (ProtocolError::DeviceListStale, "E_DEVICE_LIST_STALE"),
        (ProtocolError::PairingLeaves, "E_PAIRING_LEAVES"),
        (ProtocolError::PairingFingerprint, "E_PAIRING_FINGERPRINT"),
        (ProtocolError::TierMismatch, "E_TIER_MISMATCH"),
        (
            ProtocolError::ProvisionalOutsidePairing,
            "E_PROVISIONAL_OUTSIDE_PAIRING",
        ),
        (ProtocolError::EnvelopeShape, "E_ENVELOPE_SHAPE"),
        (ProtocolError::EnvelopeType, "E_ENVELOPE_TYPE"),
        (ProtocolError::EnvelopeLimit, "E_ENVELOPE_LIMIT"),
        (ProtocolError::FrankMismatch, "E_FRANK_MISMATCH"),
        (ProtocolError::BlobHash, "E_BLOB_HASH"),
        (ProtocolError::BlobOpen, "E_BLOB_OPEN"),
        (ProtocolError::Version, "E_VERSION"),
    ];

    #[test]
    fn codes_round_trip_and_match_the_protocol_documents() {
        for (err, code) in ALL {
            assert_eq!(err.code(), code);
            assert_eq!(ProtocolError::from_code(code), Some(err));
            assert_eq!(err.to_string(), code, "Display must be the code itself");
        }
        assert_eq!(ProtocolError::from_code("E_NOT_A_CODE"), None);
    }

    #[test]
    fn only_umk_changed_and_frank_mismatch_are_hard_rejects() {
        for (err, code) in ALL {
            let hard = matches!(
                err,
                ProtocolError::UmkChanged | ProtocolError::FrankMismatch
            );
            assert_eq!(err.is_hard_reject(), hard, "{code}");
        }
    }
}
