//! Group configuration: the ciphersuite, the three GroupContext extensions, the leaf capabilities
//! and the KeyPackage builder (protocol/01-groups.md, R11).

use super::{DILLA_BINDING, DillaBinding, DillaProvider, GroupKind, MlsError, past_epoch_policy};
use crate::error::ProtocolError;
use openmls::extensions::ExternalSendersExtension;
use openmls::prelude::*;
use openmls_basic_credential::SignatureKeyPair;

/// The mandatory-to-implement suite, the only one at v1 (protocol/01-groups.md).
pub const CIPHERSUITE: Ciphersuite = Ciphersuite::MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519;
/// Application ciphertext is padded to a multiple of this (RFC 9420 section 6.3.1).
pub const PADDING_SIZE: usize = 256;
/// Clients publish KeyPackages with a 90-day lifetime (protocol/01 "Joining").
pub const KEY_PACKAGE_LIFETIME_DAYS: u64 = 90;
/// A device silent this long is removed by the instance (R15; the spec's chaos list says 30 and is
/// wrong - protocol/01-groups.md and spec line 716 both say 90).
pub const INACTIVITY_REMOVE_DAYS: u64 = 90;
/// The DS batches at most this many Adds into one commit.
pub const MAX_ADDS_PER_COMMIT: usize = 256;

/// Builds the three GroupContext extensions, in this order:
///   1. `RequiredCapabilities` naming only `ExtensionType::Unknown(0xF001)` (D2)
///   2. `ExternalSenders`, only when `ext_senders` is `Some`
///   3. `Unknown(0xF001, UnknownExtension(binding.encode()))` - the payload
///
/// `Extensions` preserves insertion order and rejects duplicate types, so this order is the wire
/// order; do not build a second set and expect byte-identity unless you preserve it.
pub fn group_context_extensions(
    binding: &DillaBinding,
    ext_senders: Option<ExternalSendersExtension>,
) -> Result<Extensions<GroupContext>, MlsError> {
    if ext_senders.is_some() && !binding.kind.has_external_sender() {
        return Err(ProtocolError::ExternalSenderForbidden.into());
    }
    let mut items = vec![Extension::RequiredCapabilities(
        RequiredCapabilitiesExtension::new(&[DILLA_BINDING], &[], &[CredentialType::Basic]),
    )];
    if let Some(senders) = ext_senders {
        items.push(Extension::ExternalSenders(senders));
    }
    items.push(binding.to_extension());
    Extensions::try_from(items).map_err(|e| MlsError::OpenMls(format!("{e:?}")))
}

/// Every dilla leaf advertises 0xF001. A KeyPackage that does not is rejected on Add with
/// `ProposalValidationError::InsufficientCapabilities` (gap-4 section 5).
///
/// It also advertises `last_resort` (type 10). RFC 9420 section 10.1 requires every extension a
/// KeyPackage carries to be listed in its leaf's capabilities, and `last_resort` is not one of the
/// default types exempt from that (gap-5 section 4.2): without it `KeyPackageIn::validate` refuses
/// every package `build_key_package(.., true)` builds with `UnsupportedExtension`, and each device
/// must keep one last-resort package on the instance (protocol/01, "Joining").
pub fn leaf_capabilities() -> Capabilities {
    Capabilities::new(
        None,
        // Preserve the three suites advertised by OpenMLS 0.9.0's default list on the wire.
        // Dilla creates groups with CIPHERSUITE; the other two remain capability declarations.
        Some(&[
            CIPHERSUITE,
            Ciphersuite::MLS_128_DHKEMP256_AES128GCM_SHA256_P256,
            Ciphersuite::MLS_128_DHKEMX25519_CHACHA20POLY1305_SHA256_Ed25519,
        ]),
        Some(&[DILLA_BINDING, ExtensionType::LastResort]),
        None,
        Some(&[CredentialType::Basic]),
    )
}

pub fn create_config(
    binding: &DillaBinding,
    ext_senders: Option<ExternalSendersExtension>,
) -> Result<MlsGroupCreateConfig, MlsError> {
    Ok(MlsGroupCreateConfig::builder()
        .ciphersuite(CIPHERSUITE)
        .use_ratchet_tree_extension(false)
        .padding_size(PADDING_SIZE)
        .wire_format_policy(PURE_PLAINTEXT_WIRE_FORMAT_POLICY)
        .set_past_epoch_deletion_policy(past_epoch_policy(binding.kind))
        .with_group_context_extensions(group_context_extensions(binding, ext_senders)?)
        .capabilities(leaf_capabilities())
        .build())
}

pub fn join_config(kind: GroupKind) -> MlsGroupJoinConfig {
    MlsGroupJoinConfig::builder()
        .use_ratchet_tree_extension(false)
        .padding_size(PADDING_SIZE)
        .wire_format_policy(PURE_PLAINTEXT_WIRE_FORMAT_POLICY)
        .set_past_epoch_deletion_policy(past_epoch_policy(kind))
        .build()
}

pub fn build_key_package(
    provider: &DillaProvider,
    signer: &SignatureKeyPair,
    credential: CredentialWithKey,
    last_resort: bool,
) -> Result<KeyPackageBundle, MlsError> {
    build_key_package_with_expiry(provider, signer, credential, last_resort)
        .map(|(bundle, _)| bundle)
}

/// Builds a KeyPackage and returns the expiry written into that exact package's lifetime.
/// Native bot publication persists this value with the corresponding public body and MLS ref.
pub fn build_key_package_with_expiry(
    provider: &DillaProvider,
    signer: &SignatureKeyPair,
    credential: CredentialWithKey,
    last_resort: bool,
) -> Result<(KeyPackageBundle, u64), MlsError> {
    let lifetime = Lifetime::new(KEY_PACKAGE_LIFETIME_DAYS * 24 * 60 * 60);
    let expires = lifetime.not_after();
    let mut builder = KeyPackage::builder()
        .leaf_node_capabilities(leaf_capabilities())
        .key_package_lifetime(lifetime);
    if last_resort {
        builder = builder.mark_as_last_resort();
    }
    let bundle = builder
        .build(CIPHERSUITE, provider, signer, credential)
        .map_err(|e| MlsError::OpenMls(format!("{e:?}")))?;
    Ok((bundle, expires))
}

/// A `GroupContextExtensions` proposal **replaces** the whole extension set, so a rotation has to
/// re-state required_capabilities and the unchanged binding alongside the new senders. Dropping
/// the binding silently erases it from the group context; dropping required_capabilities makes the
/// commit invalid (gap-4 section 4.1). At most one such proposal per commit.
///
/// **This builder has no reachable consumer on this branch.** `extension_change_verdict`
/// (`mls/policy.rs`) rejects **every** `GroupContextExtensions` proposal, external sender included,
/// because `protocol/03-identity.md` defines the rotation preimage but names nowhere for the
/// signature over it to travel — no companion GroupContext extension, no `authenticated_data`,
/// nothing. A verifier cannot check a signature it cannot locate, so the safe default refuses the
/// proposal outright rather than accepting one nothing can authenticate. The extension set this
/// function builds is therefore never handed to a receiver that would accept it. See follow-up
/// card (p) in `docs/superpowers/plans/2026-09-23-dilla-core.md`: once protocol/03 names the
/// carrier, `extension_change_verdict` can accept a rotation whose other extensions are
/// byte-identical to the current group context's and whose signature verifies.
pub fn rotate_external_senders_extensions(
    binding: &DillaBinding,
    new_senders: ExternalSendersExtension,
) -> Result<Extensions<GroupContext>, MlsError> {
    group_context_extensions(binding, Some(new_senders))
}

#[cfg(test)]
mod tests {
    use super::*;
    // Named only by the tests below: the module's own code reaches the type through
    // `DILLA_BINDING`, so importing the id at module level would be an unused import under
    // `-D warnings`.
    use crate::ids::InstanceId;
    use crate::mls::DILLA_BINDING_ID;

    fn binding(kind: GroupKind) -> DillaBinding {
        DillaBinding {
            v: 1,
            instance_id: InstanceId::from_bytes([0x11; 16]),
            community_id: None,
            target_id: [0x33; 16],
            kind,
            policy_version: 1,
            e2ee_version: 1,
            media_version: kind.media_version(),
        }
    }

    /// D2: only `ExtensionType::Unknown(0xF001)` goes in `required_capabilities.extension_types`.
    /// `ExternalSenders` is a default type and listing it makes every commit invalid.
    #[test]
    fn required_capabilities_lists_only_the_dilla_binding_type() {
        let exts = group_context_extensions(&binding(GroupKind::Text), None).unwrap();
        let rc = exts
            .required_capabilities()
            .expect("required_capabilities present");
        assert_eq!(rc.extension_types(), &[DILLA_BINDING]);
        assert!(rc.proposal_types().is_empty());
        assert_eq!(rc.credential_types(), &[CredentialType::Basic]);
        assert!(
            exts.unknown(DILLA_BINDING_ID).is_some(),
            "the payload is a sibling extension"
        );
    }

    #[test]
    fn pairing_groups_refuse_an_external_sender() {
        let senders = ExternalSendersExtension::new();
        {
            let kind = GroupKind::Pairing;
            let err = group_context_extensions(&binding(kind), Some(senders.clone())).unwrap_err();
            assert!(
                matches!(
                    err,
                    MlsError::Protocol(ProtocolError::ExternalSenderForbidden)
                ),
                "{kind:?}: {err:?}"
            );
        }
        // text and call accept one
        assert!(group_context_extensions(&binding(GroupKind::Text), Some(senders)).is_ok());
    }

    #[test]
    fn every_leaf_advertises_the_binding_extension_and_basic_credentials() {
        let caps = leaf_capabilities();
        assert!(caps.extensions().contains(&DILLA_BINDING));
        assert!(caps.extensions().contains(&ExtensionType::LastResort));
        assert!(caps.credentials().contains(&CredentialType::Basic));
    }

    #[test]
    fn a_rotation_proposal_restates_all_three_extensions() {
        // A GroupContextExtensions proposal REPLACES the set; dropping the binding from a rotation
        // silently erases it from the group context (gap-4 section 4.1).
        let b = binding(GroupKind::Text);
        let exts = rotate_external_senders_extensions(&b, ExternalSendersExtension::new()).unwrap();
        assert!(exts.required_capabilities().is_some());
        assert!(exts.external_senders().is_some());
        assert_eq!(DillaBinding::from_extensions(&exts).unwrap(), b);
    }

    #[test]
    fn the_create_config_pins_the_wire_and_padding_policy() {
        let cfg = create_config(&binding(GroupKind::Text), None).unwrap();
        assert_eq!(cfg.ciphersuite(), CIPHERSUITE);
        assert_eq!(leaf_capabilities().ciphersuites().len(), 3);
        assert_eq!(leaf_capabilities().ciphersuites()[0], CIPHERSUITE.into());
        let join = cfg.join_config();
        assert_eq!(join.padding_size(), PADDING_SIZE);
        // `MlsGroupJoinConfig` has no `use_ratchet_tree_extension()` accessor in 0.9.0 (the field
        // is private); `MlsGroupCreateConfig` does, and it is the same flag — the create config
        // copies it into the join config it builds.
        assert!(!cfg.use_ratchet_tree_extension(), "the DS serves the tree");
        assert_eq!(join.wire_format_policy(), PURE_PLAINTEXT_WIRE_FORMAT_POLICY);
    }

    #[test]
    fn constants_match_the_protocol_documents() {
        assert_eq!(DILLA_BINDING_ID, 0xF001);
        assert_eq!(PADDING_SIZE, 256);
        assert_eq!(KEY_PACKAGE_LIFETIME_DAYS, 90);
        assert_eq!(INACTIVITY_REMOVE_DAYS, 90); // R15, not the spec's chaos-list "30 days"
        assert_eq!(MAX_ADDS_PER_COMMIT, 256);
        assert_eq!(
            CIPHERSUITE,
            Ciphersuite::MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519
        );
    }
}
