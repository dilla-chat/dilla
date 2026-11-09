//! The delivery service's view of a group: the public tree, the epoch, the extensions and the leaf
//! credentials. It holds no group secret and cannot decrypt.

use super::{PublicStore, PublicStoreError};
use crate::error::ProtocolError;
use crate::identity::CredentialIdentity;
use crate::mls::{
    DILLA_BINDING, DillaBinding, commit_extensions_unchanged, instance_sender_index,
    leaf_credentials_unchanged, proposal_credential_verdict, proposal_extensions_unchanged,
};
// None of these four is re-exported by `openmls::prelude` in 0.9.0 (the same finding as
// `mls::group`: the prelude re-exports `hash_ref::KeyPackageRef` but not `ProposalRef`,
// `treesync::RatchetTreeIn` but not `RatchetTree`, and nothing from `messages::group_info`).
use openmls::ciphersuite::hash_ref::ProposalRef;
use openmls::messages::group_info::{GroupInfo, VerifiableGroupInfo};
use openmls::prelude::*;
use openmls::treesync::RatchetTree;
use openmls_traits::signatures::Signer;
use tls_codec::{Deserialize as _, Serialize as _};

#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum PublicGroupError {
    #[error(transparent)]
    Protocol(#[from] ProtocolError),
    #[error(transparent)]
    Store(#[from] PublicStoreError),
    #[error("openmls: {0}")]
    OpenMls(String),
    /// `PublicGroup::load` returns `Ok(None)` on a torn write - indistinguishable from "absent"
    /// (gap-1 section 6.1 hazard 1). The wrapper turns that into this error, never into a new
    /// group: re-bootstrapping over live state would silently fork every member.
    #[error("public group state is absent or torn")]
    StateMissing,
}

fn openmls<E: core::fmt::Debug>(e: E) -> PublicGroupError {
    PublicGroupError::OpenMls(format!("{e:?}"))
}

#[derive(Clone, PartialEq, Eq, Debug)]
pub struct MemberInfo {
    pub leaf_index: u32,
    pub signature_key: [u8; 32],
    pub identity: CredentialIdentity,
}

/// One entry of the group context's `external_senders` extension (RFC 9420 §12.1.8.1), as the
/// delivery service reads it: the credential's type and content, and the signature key OpenMLS
/// verifies an external proposal from that sender under.
#[derive(Clone, PartialEq, Eq, Debug)]
pub struct ExternalSenderInfo {
    /// `CredentialType` as its RFC 9420 code point (1 = basic).
    pub credential_type: u16,
    /// The credential's content: for a basic credential, its identity bytes.
    pub credential: Vec<u8>,
    pub signature_key: Vec<u8>,
}

#[derive(Debug)]
pub enum PublicProcessed {
    Proposal {
        proposal_ref: Vec<u8>,
        sender_leaf: Option<u32>,
    },
    ExternalJoinProposal {
        proposal_ref: Vec<u8>,
    },
    StagedCommit {
        staged: Box<StagedCommit>,
        sender_leaf: Option<u32>,
    },
    /// The DS never sees an application message: `process_message` refuses a `PrivateMessage`.
    Rejected(ProtocolError),
}

pub struct DillaPublicGroup {
    group: PublicGroup,
    store: PublicStore,
    binding: PublicGroupBinding,
}

/// The binding the public tree carries: the eight-field v1 binding. The typed v2 binding of the Bot
/// interaction groups is gone with them (Bot v2 Task 23); a v2 GroupContext is refused.
#[derive(Clone, Debug)]
enum PublicGroupBinding {
    Legacy(DillaBinding),
}

impl PublicGroupBinding {
    fn from_group_context(ctx: &GroupContext) -> Result<Self, ProtocolError> {
        let raw = ctx
            .extensions()
            .unknown(crate::mls::DILLA_BINDING_ID)
            .ok_or(ProtocolError::Binding)?;
        let mut decoder = crate::cbor::Decoder::new(&raw.0);
        decoder.array_len().map_err(|_| ProtocolError::Binding)?;
        let version = decoder.uint().map_err(|_| ProtocolError::Binding)?;
        match version {
            1 => Ok(Self::Legacy(DillaBinding::from_group_context(ctx)?)),
            _ => Err(ProtocolError::Binding),
        }
    }

    fn encode(&self) -> Vec<u8> {
        match self {
            Self::Legacy(binding) => binding.encode(),
        }
    }
}

/// Hand-written because neither `PublicGroup` nor `PublicStore` is `Debug`, and because a debug
/// dump of the DS view must never print a tree or a credential into a log line. The two fields
/// here are what identifies the view in an error message.
impl core::fmt::Debug for DillaPublicGroup {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        f.debug_struct("DillaPublicGroup")
            .field("group_id", &self.group.group_id().as_slice())
            .field("epoch", &self.epoch())
            .finish_non_exhaustive()
    }
}

fn sender_leaf(sender: &Sender) -> Option<u32> {
    match sender {
        Sender::Member(index) => Some(index.u32()),
        _ => None,
    }
}

/// Re-frames a `ProtocolMessage` as the `MLSMessage` it arrived in.
///
/// **Deviation from the brief, forced by the crate.** The brief's line was
/// `MlsMessageIn::from(message)`, with a fallback through `MlsMessageIn::from(PublicMessageIn)`.
/// Neither exists in openmls 0.9.0: `src/framing/message_in.rs` declares `MlsMessageIn`'s two
/// fields `pub(crate)` and defines no constructor, `src/framing/message_out.rs:208` has only
/// `From<MlsMessageOut> for MlsMessageIn`, and the one route out of `PublicMessageIn` -
/// `impl From<PublicMessageIn> for PublicMessage` at `src/framing/public_message_in.rs:291` - is
/// `#[cfg(any(feature = "test-utils", test))]` with a comment saying it MUST NOT be available
/// outside tests. So the two-field `MLSMessage` header is written here instead, from public items:
/// `ProtocolVersion` and `WireFormat` both serialise as the `u16` the RFC 9420 presentation
/// language gives them (`versions.rs:48`, `framing/mod.rs:125-136`), and `PublicMessageIn`
/// implements `tls_codec::Serialize` publicly (`framing/public_message_in.rs:264`). The
/// `MlsMessageBodyIn::PublicMessage` discriminant is 1, the same value as
/// `WireFormat::PublicMessage`.
///
/// `queue_proposal`'s byte-for-byte test over a real external Remove proposal is what keeps this
/// honest.
fn reframe_mls_message(message: &ProtocolMessage) -> Result<Vec<u8>, PublicGroupError> {
    let public = match message {
        ProtocolMessage::PublicMessage(public) => public,
        // The DS holds no group secret and must never be asked to decrypt.
        ProtocolMessage::PrivateMessage(_) => return Err(ProtocolError::EnvelopeShape.into()),
    };
    let mut out = Vec::new();
    ProtocolVersion::Mls10
        .tls_serialize(&mut out)
        .map_err(openmls)?;
    WireFormat::PublicMessage
        .tls_serialize(&mut out)
        .map_err(openmls)?;
    public.tls_serialize(&mut out).map_err(openmls)?;
    Ok(out)
}

impl DillaPublicGroup {
    /// Seeds the view from the first committer's GroupInfo and the ratchet tree, and verifies
    /// `dilla_binding` before returning. There is no tree-less variant: the DS must be seeded once
    /// and maintains the tree itself afterwards.
    pub fn from_external(
        crypto: &impl OpenMlsCrypto,
        ratchet_tree: RatchetTreeIn,
        group_info: VerifiableGroupInfo,
    ) -> Result<(Self, GroupInfo), PublicGroupError> {
        let store = PublicStore::new();
        let (group, info) = PublicGroup::from_external(
            crypto,
            &store,
            ratchet_tree,
            group_info,
            ProposalStore::new(),
        )
        .map_err(openmls)?;
        let binding = PublicGroupBinding::from_group_context(group.group_context())?;
        Ok((
            Self {
                group,
                store,
                binding,
            },
            info,
        ))
    }

    pub fn import_state(state: &[u8], group_id: &GroupId) -> Result<Self, PublicGroupError> {
        let store = PublicStore::import(state)?;
        // Two shapes of a torn write reach here and both mean the same thing to the DS, so both
        // become `StateMissing` and neither becomes a new group:
        //   * `PublicGroup::load` returns `Ok(None)` when one of the four entities is absent - it
        //     discards the other three and reports "absent" (gap-1 section 6.1 hazard 1);
        //   * it returns `Err` when an entity is present but does not decode into the type OpenMLS
        //     expects, which is the same damage seen one layer down.
        // The blob itself is a separate question: a body that is not this module's CBOR at all
        // fails in `PublicStore::import` above and surfaces as `Store(Codec(..))`.
        let group = PublicGroup::load(&store, group_id)
            .map_err(|_| PublicGroupError::StateMissing)?
            .ok_or(PublicGroupError::StateMissing)?;
        let binding = PublicGroupBinding::from_group_context(group.group_context())?;
        Ok(Self {
            group,
            store,
            binding,
        })
    }

    pub fn export_state(&self) -> Vec<u8> {
        self.store.export()
    }

    /// `&self`: structural validation writes nothing (facts-openmls section 6). A `PrivateMessage`
    /// is refused outright - the DS holds no secret and must never be asked to decrypt.
    pub fn process_message(
        &self,
        crypto: &impl OpenMlsCrypto,
        message: ProtocolMessage,
    ) -> Result<PublicProcessed, PublicGroupError> {
        if matches!(message, ProtocolMessage::PrivateMessage(_)) {
            return Ok(PublicProcessed::Rejected(ProtocolError::EnvelopeShape));
        }
        let processed = self.process_with_policy(crypto, message)?;
        let leaf = sender_leaf(processed.sender());
        Ok(match processed.into_content() {
            ProcessedMessageContent::ProposalMessage(p) => PublicProcessed::Proposal {
                proposal_ref: p.proposal_reference_ref().as_slice().to_vec(),
                sender_leaf: leaf,
            },
            ProcessedMessageContent::ExternalJoinProposalMessage(p) => {
                PublicProcessed::ExternalJoinProposal {
                    proposal_ref: p.proposal_reference_ref().as_slice().to_vec(),
                }
            }
            ProcessedMessageContent::StagedCommitMessage(staged) => PublicProcessed::StagedCommit {
                staged,
                sender_leaf: leaf,
            },
            _ => PublicProcessed::Rejected(ProtocolError::EnvelopeShape),
        })
    }

    /// `PublicGroup::process_message` followed by the two dilla-level rules the delivery service
    /// enforces inside the core: a member's leaf keeps its credential (protocol/01, protocol/02
    /// invariant 4; `mls::leaf_credentials_unchanged`), and the group context's extensions never
    /// change (`mls::commit_extensions_unchanged` and `proposal_extensions_unchanged`, the helpers a
    /// receiving member runs too: a `GroupContextExtensions` proposal could swap `external_senders`
    /// and leave a group the instance cannot propose into, DS-MEMBERSHIP-01). It runs on every
    /// handshake the DS parses - `process_message` for `POST /commit`, `/resync`, `/heal` (each
    /// replayed tail item) and `/proposal`, and `queue_proposal` for the queue itself - so such a
    /// commit or proposal is refused before a staged handle exists or anything is queued, stored or
    /// fanned out. Read-only like the call it wraps: `self.group` is the pre-merge tree.
    fn process_with_policy(
        &self,
        crypto: &impl OpenMlsCrypto,
        message: ProtocolMessage,
    ) -> Result<ProcessedMessage, PublicGroupError> {
        let processed = self
            .group
            .process_message(crypto, message)
            .map_err(openmls)?;
        match processed.content() {
            ProcessedMessageContent::StagedCommitMessage(staged) => {
                leaf_credentials_unchanged(&self.group, processed.sender(), staged)?;
                commit_extensions_unchanged(staged)?;
            }
            ProcessedMessageContent::ProposalMessage(proposal) => {
                proposal_credential_verdict(&self.group, proposal)?;
                proposal_extensions_unchanged(proposal)?;
            }
            _ => {}
        }
        Ok(processed)
    }

    /// `PublicGroup::merge_commit` calls `clear_proposal_queue` (gap-1 section 4's call table), and
    /// under ledger ruling A the received `MLSMessage` bytes live in the same queue entries, so
    /// they are dropped with the queue. Without that the map would grow for the lifetime of the
    /// wazero instance - which R9 keeps alive for the whole dillad process - driven purely by
    /// remote input. That is a memory-exhaustion path, not a tidiness question.
    ///
    /// The cached `binding` is re-derived here. `PublicGroup::merge_commit` replaces the group
    /// context wholesale (`merge_diff`, vendored `group/public_group/mod.rs:362-367`), so a
    /// structurally valid GroupContextExtensions commit moves the real `dilla_binding` underneath
    /// a cache that was only ever filled in `from_external`/`import_state`. `process_with_policy`
    /// now refuses every such commit (fix wave C), so none should reach a merge; the re-derivation
    /// stays as defence in depth, because `binding_bytes()` is exactly what
    /// interfaces section 2.10 export 12 (`public_group_state`) hands to clients. A stale cache
    /// would serve a binding that contradicts the DS's own stored state.
    ///
    /// The new value is derived from `staged.group_context()`, which is the post-commit context
    /// (`group/mls_group/staged_commit.rs:1001-1006`), **before** the merge: a commit whose context
    /// carries no valid `dilla_binding` is then refused with nothing written, rather than leaving a
    /// merged group behind an error.
    pub fn merge_commit(&mut self, staged: StagedCommit) -> Result<(), PublicGroupError> {
        let binding = PublicGroupBinding::from_group_context(staged.group_context())?;
        self.group
            .merge_commit(&self.store, staged)
            .map_err(openmls)?;
        self.binding = binding;
        Ok(())
    }

    pub fn add_proposal(&mut self, proposal: QueuedProposal) -> Result<Vec<u8>, PublicGroupError> {
        let reference = proposal.proposal_reference_ref().as_slice().to_vec();
        self.group
            .add_proposal(&self.store, proposal)
            .map_err(openmls)?;
        Ok(reference)
    }

    /// Frames, verifies and queues a proposal the DS received, keeping the original `MLSMessage`
    /// bytes beside it (NV-4 and deviation A2-11, both settled by ruling). Returns the new
    /// proposal's reference bytes.
    ///
    /// This is the **only** route from received bytes to a queued proposal, and it lives here
    /// rather than in `dilla-core-wasi` because building a `QueuedProposal` needs the group context
    /// and signature verification: the wasi crate has no public OpenMLS 0.9.0 route from bytes to
    /// `AuthenticatedContent` and must not invent one. Task 14's `public_group_proposal_put` op 0
    /// is its caller.
    ///
    /// The kept bytes are re-framed from the `ProtocolMessage` rather than passed in beside it: TLS
    /// presentation encoding is canonical, so re-framing reproduces exactly what arrived on the
    /// wire, and there is no second copy for a caller to get wrong. Ledger ruling A puts them in
    /// `PublicStore`, so `export_state()` carries them and a dillad restart does not lose a pending
    /// proposal.
    pub fn queue_proposal(
        &mut self,
        crypto: &impl OpenMlsCrypto,
        message: ProtocolMessage,
    ) -> Result<Vec<u8>, PublicGroupError> {
        // Refuses a `PrivateMessage` exactly as `process_message` refuses it.
        let received = reframe_mls_message(&message)?;

        let processed = self.process_with_policy(crypto, message)?;
        let queued = match processed.into_content() {
            ProcessedMessageContent::ProposalMessage(p)
            | ProcessedMessageContent::ExternalJoinProposalMessage(p) => *p,
            // A Commit goes through `process_message` + `merge_commit`, never through the queue.
            _ => return Err(ProtocolError::EnvelopeShape.into()),
        };
        let proposal_ref = queued.proposal_reference_ref().clone();
        let reference = proposal_ref.as_slice().to_vec();
        self.group
            .add_proposal(&self.store, queued)
            .map_err(openmls)?;
        self.store
            .set_received(self.group.group_id(), &proposal_ref, received)?;
        Ok(reference)
    }

    pub fn remove_proposal(&mut self, proposal_ref: &ProposalRef) -> Result<(), PublicGroupError> {
        // The kept `MLSMessage` bytes live in the same queue entry, so they go with it.
        self.group
            .remove_proposal(&self.store, proposal_ref)
            .map_err(openmls)
    }

    /// Each pair is the proposal's reference and the original `MLSMessage` bytes the DS received -
    /// deviation A2-11. The second element is deliberately **not** a bare `Proposal`: interfaces
    /// section 2.12 hands this list
    /// straight back to clients in `CommitConflict { proposals }` and `CommitRequired { proposals }`,
    /// and a bare `Proposal` has lost its `FramedContent` and its signature, so the client that
    /// receives it cannot process what it got back.
    ///
    /// Under ledger ruling A the bytes live in `PublicStore` and are therefore carried by
    /// `export_state()`, so a handle rebuilt with `import_state` reports them unchanged. A proposal
    /// queued through `add_proposal`, which never saw a wire message, has none, and that is
    /// reported as `StateMissing` rather than by inventing a re-serialised `Proposal` or handing
    /// back an empty byte string - either would be exactly the interoperability defect A2-11 exists
    /// to prevent.
    pub fn queued_proposals(&self) -> Result<Vec<(ProposalRef, Vec<u8>)>, PublicGroupError> {
        let queued = self.group.queued_proposals(&self.store).map_err(openmls)?;
        queued
            .into_iter()
            .map(|(reference, _queued): (ProposalRef, QueuedProposal)| {
                let message = self
                    .store
                    .received(self.group.group_id(), &reference)?
                    .ok_or(PublicGroupError::StateMissing)?;
                Ok((reference, message))
            })
            .collect()
    }

    pub fn export_ratchet_tree(&self) -> RatchetTree {
        self.group.export_ratchet_tree()
    }

    /// `PublicGroup` has no `tree_hash()`; it lives on the group context.
    pub fn tree_hash(&self) -> Vec<u8> {
        self.group.group_context().tree_hash().to_vec()
    }

    pub fn group_id(&self) -> &GroupId {
        self.group.group_id()
    }

    pub fn epoch(&self) -> u64 {
        self.group.group_context().epoch().as_u64()
    }

    /// The group's v1 binding.
    pub fn binding(&self) -> Option<&DillaBinding> {
        match &self.binding {
            PublicGroupBinding::Legacy(binding) => Some(binding),
        }
    }

    /// The binding exactly as carried in the authenticated GroupContext, for consumers that persist
    /// or compare the protocol bytes.
    pub fn binding_bytes(&self) -> Vec<u8> {
        self.binding.encode()
    }

    /// The number of occupied leaves in the tree, every one of them: `members()` leaves out a leaf
    /// whose credential is not a dilla identity or whose key is not 32 bytes, so its length is not
    /// how many leaves the group holds. A caller that adopts a tree (registration, heal's reseed)
    /// compares the two, so a tree cannot carry a leaf nobody can name.
    pub fn leaf_count(&self) -> usize {
        self.group.members().count()
    }

    /// The group context's `external_senders` entries, in extension order; empty when the extension
    /// is absent. A caller that adopts a tree (registration, heal's reseed) compares them with the
    /// instance's own key and credential: an external proposal is verified against exactly these
    /// entries, so a group that names another key is one the instance can never propose into.
    ///
    /// OpenMLS 0.9.0 keeps `ExternalSender::credential()` and `signature_key()` `pub(crate)`
    /// (`extensions/external_sender_extension.rs:41-47`), so each entry is read back out of its
    /// public TLS encoding, `signature_key` then `credential` (RFC 9420 §12.1.8.1), with no byte
    /// left over.
    pub fn external_senders(&self) -> Result<Vec<ExternalSenderInfo>, PublicGroupError> {
        let Some(senders) = self.group.group_context().extensions().external_senders() else {
            return Ok(Vec::new());
        };
        senders
            .iter()
            .map(|sender| {
                let bytes = sender.tls_serialize_detached().map_err(openmls)?;
                let mut rest = bytes.as_slice();
                let key = SignaturePublicKey::tls_deserialize(&mut rest).map_err(openmls)?;
                let credential = Credential::tls_deserialize(&mut rest).map_err(openmls)?;
                if !rest.is_empty() {
                    return Err(PublicGroupError::OpenMls(
                        "an external sender's encoding has trailing bytes".to_owned(),
                    ));
                }
                Ok(ExternalSenderInfo {
                    credential_type: credential.credential_type().into(),
                    credential: credential.serialized_content().to_vec(),
                    signature_key: key.as_slice().to_vec(),
                })
            })
            .collect()
    }

    pub fn members(&self) -> Vec<MemberInfo> {
        self.group
            .members()
            .filter_map(|m| {
                let basic = BasicCredential::try_from(m.credential.clone()).ok()?;
                let identity = CredentialIdentity::decode(basic.identity()).ok()?;
                let key = m.signature_key.as_slice();
                let mut signature_key = [0u8; 32];
                if key.len() != 32 {
                    return None;
                }
                signature_key.copy_from_slice(key);
                Some(MemberInfo {
                    leaf_index: m.index.u32(),
                    signature_key,
                    identity,
                })
            })
            .collect()
    }

    pub fn leaf(&self, index: LeafNodeIndex) -> Option<&LeafNode> {
        self.group.leaf(index)
    }

    /// The signature public key of a leaf, as OpenMLS's verification API wants it.
    ///
    /// ABI v2 §3.2 needs it because `VerifiableGroupInfo::signer()` is `pub(crate)` in openmls
    /// 0.9.0 (`src/messages/group_info.rs:100`), so the caller names the expected signer and the
    /// key comes from the tree the DS already maintains.
    ///
    /// No import is added: this file already has `use openmls::prelude::*;`, and the prelude
    /// re-exports `crate::ciphersuite::signature::*` (`openmls-0.9.0/src/prelude.rs:16`), which is
    /// where `OpenMlsSignaturePublicKey` lives (`src/ciphersuite/signature.rs:118`).
    /// `from_signature_key` is the right constructor: `::new` takes a `VLBytes` and returns a
    /// `Result`, while the tree hands back a `SignaturePublicKey` (`signature.rs:157`).
    pub fn signature_key_of_leaf(&self, index: LeafNodeIndex) -> Option<OpenMlsSignaturePublicKey> {
        let leaf = self.group.leaf(index)?;
        Some(OpenMlsSignaturePublicKey::from_signature_key(
            leaf.signature_key().clone(),
            self.group.ciphersuite().signature_algorithm(),
        ))
    }

    /// The kind, sender, target and credential of one queued proposal — ABI v2 §3.3.
    ///
    /// This lives in `dilla-core`, not in `dilla-core-wasi`, because the two openmls APIs a
    /// wasi-side re-parse would need are both unreachable from outside the crate:
    /// `PublicMessageIn::content()` is `pub(crate)` (`src/framing/public_message_in.rs:43`) and
    /// `impl From<ProposalIn> for Proposal` (`src/messages/proposals_in.rs:384`) is gated behind
    /// `#[cfg(any(feature = "test-utils", test))]`. `PublicGroup::queued_proposals` hands back the
    /// already-parsed `QueuedProposal`, which [`Self::queued_proposals`] above deliberately
    /// discards in favour of the wire bytes (deviation A2-11); this is the other half of the same
    /// read, so nothing is re-parsed and, crucially, nothing is re-processed.
    ///
    /// `Ok(None)` means "no queued proposal carries that reference", which the export turns into
    /// `E_ABI_STATE` rather than an empty success. `kind` uses `protocol/02-delivery-service.md`'s
    /// numbering: 1 add, 2 update, 3 remove, 4 psk, 5 reinit, 6 external_init,
    /// 7 group_context_extensions.
    ///
    /// **Deviation from the brief, forced by the vendored source.** The brief typed `wanted` as
    /// `&ProposalRef`. `ProposalRef` is `HashReference`, whose only constructor from opaque bytes
    /// is `HashReference::from_slice`, and that is `#[cfg(any(feature = "test-utils", test))]`
    /// (`openmls-0.9.0/src/ciphersuite/hash_ref.rs:120-125`) — so the wasi crate cannot build one
    /// from a request at all. The brief's own fallback is taken: the parameter is the reference
    /// bytes, which is what the comparison already used.
    #[allow(clippy::type_complexity)]
    pub fn queued_proposal_detail(
        &self,
        wanted: &[u8],
    ) -> Result<Option<(u64, Option<u32>, Option<u32>, Option<Vec<u8>>)>, PublicGroupError> {
        let queued = self.group.queued_proposals(&self.store).map_err(openmls)?;
        for (reference, proposal) in queued {
            if reference.as_slice() != wanted {
                continue;
            }
            let sender_leaf = sender_leaf(proposal.sender());
            let detail = match proposal.proposal() {
                Proposal::Add(add) => (
                    1u64,
                    sender_leaf,
                    None,
                    Some(credential_identity_bytes(add.key_package())?),
                ),
                Proposal::Update(_) => (2, sender_leaf, None, None),
                Proposal::Remove(remove) => (3, sender_leaf, Some(remove.removed().u32()), None),
                Proposal::PreSharedKey(_) => (4, sender_leaf, None, None),
                Proposal::ReInit(_) => (5, sender_leaf, None, None),
                Proposal::ExternalInit(_) => (6, sender_leaf, None, None),
                Proposal::GroupContextExtensions(_) => (7, sender_leaf, None, None),
                other => {
                    // openmls 0.9.0 also has SelfRemove, Custom and the two extensions-draft
                    // variants. Dropping one silently would let invariant 4's set comparison
                    // pass over a proposal the DS cannot name, so it is an error.
                    return Err(PublicGroupError::OpenMls(format!(
                        "unsupported proposal type {:?}",
                        other.proposal_type()
                    )));
                }
            };
            return Ok(Some(detail));
        }
        Ok(None)
    }

    pub fn required_capabilities(&self) -> Option<&RequiredCapabilitiesExtension> {
        self.group
            .group_context()
            .extensions()
            .required_capabilities()
    }

    /// The signature key the committer of `staged` holds in the epoch the commit produces, which is
    /// the key the GroupInfo of that epoch is signed under.
    ///
    /// A commit with an UpdatePath carries the committer's new leaf node, whose key may differ from
    /// the one the tree holds today, and an EXTERNAL commit always carries one: the joiner is in no
    /// leaf of the current tree, so `signature_key_of_leaf` has nothing to answer for it. A member
    /// commit without a path leaves the committer's leaf untouched, so its key is the tree's.
    /// `None` is a commit that names no sender and brings no leaf node, which openmls refuses to
    /// stage; it is reported rather than assumed away.
    pub fn staged_committer_key(
        &self,
        staged: &StagedCommit,
        sender_leaf: Option<u32>,
    ) -> Option<OpenMlsSignaturePublicKey> {
        if let Some(leaf) = staged.update_path_leaf_node() {
            return Some(OpenMlsSignaturePublicKey::from_signature_key(
                leaf.signature_key().clone(),
                self.group.ciphersuite().signature_algorithm(),
            ));
        }
        sender_leaf.and_then(|l| self.signature_key_of_leaf(LeafNodeIndex::new(l)))
    }

    pub fn ext_commit_sender_index(
        &self,
        staged: &StagedCommit,
    ) -> Result<LeafNodeIndex, PublicGroupError> {
        self.group.ext_commit_sender_index(staged).map_err(openmls)
    }
}

/// The raw `credential_identity` bytes of a KeyPackage's leaf. `BasicCredential` is in
/// `openmls::prelude`, which this file already imports; these are the same two lines
/// [`DillaPublicGroup::members`] uses.
fn credential_identity_bytes(kp: &KeyPackage) -> Result<Vec<u8>, PublicGroupError> {
    let basic = BasicCredential::try_from(kp.leaf_node().credential().clone()).map_err(openmls)?;
    Ok(basic.identity().to_vec())
}

/// `KeyPackageIn::validate` plus dilla's own checks: the leaf must advertise 0xF001, and the
/// credential identity must decode. Both are invariant 6's "before proposing" gate.
pub fn validate_key_package(
    crypto: &impl OpenMlsCrypto,
    kp: KeyPackageIn,
) -> Result<KeyPackage, PublicGroupError> {
    let validated = kp
        .validate(crypto, ProtocolVersion::Mls10)
        .map_err(openmls)?;
    let leaf = validated.leaf_node();
    if !leaf.capabilities().extensions().contains(&DILLA_BINDING) {
        return Err(ProtocolError::Binding.into());
    }
    let basic = BasicCredential::try_from(leaf.credential().clone()).map_err(openmls)?;
    CredentialIdentity::decode(basic.identity())?;
    Ok(validated)
}

/// The `Provider` type parameter of `ExternalProposal::new_*` is used for **nothing but its
/// error type** (`ProposeAddMemberError<Provider::StorageError>`; gap-16 §"External proposal
/// signing", facts-wazero §6: "all three constructors are generic over `Provider: OpenMlsProvider`
/// even though only the signer is used"). It must therefore not be `DillaProvider`, which exists
/// only on the two targets that have `rusqlite`: this module is the one the wasi tier is built
/// for, and naming `DillaProvider` here would drag SQLite back into the `wasm32-wasip1` build.
/// `openmls_rust_crypto::OpenMlsRustCrypto` is a full `OpenMlsProvider` over `MemoryStorage`
/// (facts-openmls §3), is already in the dependency graph on every target, and no instance of it
/// is ever created.
pub fn external_propose_add(
    kp: KeyPackage,
    group_id: GroupId,
    epoch: GroupEpoch,
    signer: &impl Signer,
) -> Result<MlsMessageOut, PublicGroupError> {
    ExternalProposal::new_add::<openmls_rust_crypto::OpenMlsRustCrypto>(
        kp,
        group_id,
        epoch,
        signer,
        instance_sender_index(),
    )
    .map_err(openmls)
}

pub fn external_propose_remove(
    removed: LeafNodeIndex,
    group_id: GroupId,
    epoch: GroupEpoch,
    signer: &impl Signer,
) -> Result<MlsMessageOut, PublicGroupError> {
    // Same provider-as-error-type-only parameter as `external_propose_add` above.
    ExternalProposal::new_remove::<openmls_rust_crypto::OpenMlsRustCrypto>(
        removed,
        group_id,
        epoch,
        signer,
        instance_sender_index(),
    )
    .map_err(openmls)
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::mls::test_entities::TVal;
    use openmls_rust_crypto::RustCrypto;
    use openmls_traits::public_storage::PublicStorageProvider as _;
    // `MlsMessageIn::tls_deserialize_exact` resolves through the module's own
    // `use tls_codec::{Deserialize as _, ..}` above, which `external_senders` needs too.

    /// A torn write - one of the four entities missing - must surface as `StateMissing`, never as
    /// "this group does not exist" (gap-1 section 6.1 hazard 1).
    ///
    /// The key here is a real `openmls::group::GroupId`, which OpenMLS implements
    /// `traits::GroupId` for; only the entity side needs a local newtype.
    #[test]
    fn a_torn_state_is_reported_as_missing_not_as_absent() {
        let store = PublicStore::new();
        let group_id = openmls::group::GroupId::from_slice(&[0x44; 16]);
        // Three of the four entities present: exactly the shape a crash mid-`store()` leaves.
        store.write_tree(&group_id, &TVal(1)).unwrap();
        store.write_context(&group_id, &TVal(2)).unwrap();
        store.write_confirmation_tag(&group_id, &TVal(3)).unwrap();
        let blob = store.export();

        let err = DillaPublicGroup::import_state(&blob, &group_id)
            .expect_err("a torn state must not import");
        assert!(matches!(err, PublicGroupError::StateMissing), "{err:?}");
    }

    #[test]
    fn an_empty_state_is_reported_as_missing() {
        let group_id = openmls::group::GroupId::from_slice(&[0x44; 16]);
        let blob = PublicStore::new().export();
        assert!(matches!(
            DillaPublicGroup::import_state(&blob, &group_id),
            Err(PublicGroupError::StateMissing)
        ));
    }

    // The committed 1,500-leaf fixture of Plan A task 13 — the same files `dilla-core-wasi`'s
    // export tests read. `include_bytes!` is relative to this file's own directory, so the four
    // `..` climb `public_group` -> `src` -> `dilla-core` -> `core` -> the repository root. There
    // is no `tests/` alternative that would be cheaper: the fixture is what makes these tests run
    // against a real 1,500-leaf tree rather than a hand-built toy.
    const FIXTURE_TREE: &[u8] =
        include_bytes!("../../../../testkit/fixtures/ds-1500/ratchet_tree.mls");
    const FIXTURE_GROUP_INFO: &[u8] =
        include_bytes!("../../../../testkit/fixtures/ds-1500/group_info.mls");
    /// An external Remove of leaf 0, signed by the instance key the fixture's `external_senders`
    /// extension names — which is the only reason a `DillaPublicGroup` will queue it at all.
    const FIXTURE_EXTERNAL_REMOVE: &[u8] =
        include_bytes!("../../../../testkit/fixtures/ds-1500/remove_leaf0.mls");

    fn fixture_public_group() -> DillaPublicGroup {
        let tree = RatchetTreeIn::tls_deserialize_exact(FIXTURE_TREE)
            .expect("the committed ratchet tree decodes");
        let group_info = match MlsMessageIn::tls_deserialize_exact(FIXTURE_GROUP_INFO)
            .expect("the committed GroupInfo decodes")
            .extract()
        {
            MlsMessageBodyIn::GroupInfo(info) => info,
            other => panic!("expected a GroupInfo, got {other:?}"),
        };
        DillaPublicGroup::from_external(&RustCrypto::default(), tree, group_info)
            .expect("the committed fixture seeds the DS view")
            .0
    }

    /// DS-MEMBERSHIP-01: the view reports the group context's external senders as the fixture's
    /// generator put them there - one entry, a basic credential `[1, "instance", instance_id]`
    /// naming the fixture's instance, and a 32-byte Ed25519 key. The fixture's external Remove,
    /// signed by the generator's instance key, still queues: the extension holds that one sender.
    #[test]
    fn external_senders_reports_the_fixtures_one_instance_entry() {
        let mut public = fixture_public_group();
        let senders = public.external_senders().expect("the extension decodes");
        assert_eq!(senders.len(), 1, "{senders:?}");
        let sender = &senders[0];
        assert_eq!(sender.credential_type, 1, "a basic credential");
        assert_eq!(
            sender.credential,
            crate::mls::instance_credential_identity(&crate::ids::InstanceId::from_bytes(
                [0x11; 16]
            )),
        );
        assert_eq!(sender.signature_key.len(), 32, "Ed25519");
        let message = MlsMessageIn::tls_deserialize_exact(FIXTURE_EXTERNAL_REMOVE)
            .expect("the committed external Remove decodes")
            .try_into_protocol_message()
            .expect("an external proposal is a handshake message");
        public
            .queue_proposal(&RustCrypto::default(), message)
            .expect("the external Remove verifies under the group's one external sender");
    }

    #[test]
    fn legacy_binding_accessor_preserves_v1_binding_behavior() {
        let public = fixture_public_group();
        let binding = public
            .binding()
            .expect("v1 group exposes the established binding API");
        assert_eq!(binding.v, 1);
        assert_eq!(DillaBinding::decode(&binding.encode()).unwrap(), *binding);
        assert_eq!(public.binding_bytes(), binding.encode());
    }

    /// ABI v2 §3.2's key source: a leaf inside the tree yields a verification key tagged with the
    /// group's own signature scheme, and a leaf outside it yields `None` rather than a panic or a
    /// zero key.
    #[test]
    fn signature_key_of_leaf_answers_for_a_member_and_none_for_a_stranger() {
        let public = fixture_public_group();
        let key = public
            .signature_key_of_leaf(LeafNodeIndex::new(0))
            .expect("leaf 0 is the creator");
        assert_eq!(key.as_slice().len(), 32, "Ed25519");
        assert_eq!(key.signature_scheme(), SignatureScheme::ED25519);
        assert_eq!(
            key.as_slice(),
            public
                .leaf(LeafNodeIndex::new(0))
                .unwrap()
                .signature_key()
                .as_slice(),
            "the key must come from the tree, not be invented"
        );
        assert!(
            public
                .signature_key_of_leaf(LeafNodeIndex::new(1_000_000))
                .is_none(),
            "a leaf outside the tree has no key to check a GroupInfo against"
        );
    }

    /// ABI v2 §3.3. The brief's snippet asserted `target_leaf == Some(1)`; the committed fixture
    /// the generator now writes is `remove_leaf0.mls`, so the target asserted here is leaf 0.
    #[test]
    fn queued_proposal_detail_names_a_removes_target_leaf() {
        let crypto = RustCrypto::default();
        let mut public = fixture_public_group();
        let message = MlsMessageIn::tls_deserialize_exact(FIXTURE_EXTERNAL_REMOVE)
            .expect("the committed external Remove decodes")
            .try_into_protocol_message()
            .expect("an external proposal is a handshake message");
        let reference = public
            .queue_proposal(&crypto, message)
            .expect("the instance is the group's external sender");
        let (kind, sender_leaf, target_leaf, identity) = public
            .queued_proposal_detail(&reference)
            .expect("the read succeeds")
            .expect("the proposal was just queued");
        assert_eq!(kind, 3);
        assert_eq!(target_leaf, Some(0), "remove_leaf0.mls removes leaf 0");
        assert!(
            sender_leaf.is_none(),
            "an external sender is not a member leaf"
        );
        assert!(identity.is_none());
        assert!(
            public.queued_proposal_detail(&[0u8; 32]).unwrap().is_none(),
            "an unknown reference is None, never a wrong proposal"
        );
    }
}
