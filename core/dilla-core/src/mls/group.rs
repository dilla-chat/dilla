//! The transactional `MlsGroup` wrapper.
//!
//! Every state-changing OpenMLS call runs inside one `BEGIN IMMEDIATE ... COMMIT`, because the
//! `StorageProvider` trait has no transaction hook and `merge_staged_commit` alone performs up to
//! 15 writes (gap-7 section 2.1). After a rollback the in-memory `MlsGroup` is invalid, so the
//! wrapper returns `MlsError::NeedsReload` and the caller reloads.

use super::{
    DillaBinding, DillaProvider, DillaStorage, GroupKind, StorageError, TxError, create_config,
    join_config, past_epoch_sweep, proposal_credential_verdict, proposal_extensions_unchanged,
    validate_staged_commit,
};
use crate::envelope::Envelope;
use crate::error::ProtocolError;
use crate::ids::{DeviceId, UserId};
use crate::sframe::NK;
use openmls::extensions::ExternalSendersExtension;
// Not in `openmls::prelude` in 0.9.0 (verified: `prelude.rs` re-exports `messages::{external_proposals,
// proposals, proposals_in}::*` but not `messages::group_info`, and `treesync::RatchetTreeIn` but not
// `treesync::RatchetTree`), so both are named by their own paths.
use openmls::messages::group_info::{GroupInfo, VerifiableGroupInfo};
use openmls::prelude::*;
use openmls::treesync::RatchetTree;
use openmls_basic_credential::SignatureKeyPair;

#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum MlsError {
    #[error(transparent)]
    Protocol(ProtocolError),
    #[error(transparent)]
    Storage(#[from] StorageError),
    #[error("transaction: {0}")]
    Tx(String),
    #[error("openmls: {0}")]
    OpenMls(String),
    #[error("group handle is stale after a rollback; reload it")]
    NeedsReload,
    #[error("group not found")]
    NotFound,
    /// A Welcome whose group context's epoch or tree hash is not the one the delivery service
    /// labelled it with (`WelcomeLabel`): it does not belong to the group's history at that point.
    #[error("the Welcome does not match its delivery-service label")]
    WelcomeLabel,
}

/// What the delivery service says about a Welcome it serves (`GET /v1/welcomes`, protocol/02):
/// the group it was uploaded for, and the epoch and tree hash of that group right after the commit
/// it was uploaded with, read from the instance's own public group.
#[derive(Clone, Copy, Debug)]
pub struct WelcomeLabel<'a> {
    pub group_id: &'a [u8],
    pub epoch: u64,
    pub tree_hash: &'a [u8],
}

/// Lives here rather than in `binding.rs`: `MlsError` is defined in this module, and `binding` is
/// ungated so that `wasm32-wasip1` — which has no `rusqlite` and therefore no `group` module —
/// still gets `DillaBinding`. An `impl` in `binding.rs` would not resolve there.
impl From<ProtocolError> for MlsError {
    fn from(e: ProtocolError) -> Self {
        MlsError::Protocol(e)
    }
}

/// A rollback caused by the **database** leaves the in-memory `MlsGroup` invalid (gap-7 section 4
/// item 1): the caller must drop the handle and `DillaGroup::load` again, which is what
/// `NeedsReload` says. A rollback caused by a **protocol** rejection - dilla's own binding check,
/// or an OpenMLS validation failure such as `InsufficientCapabilities` - wrote nothing and must
/// reach the caller as itself; collapsing it into `NeedsReload` would hide every refusal behind
/// "reload me". `mls_err` below is what puts a storage failure into `MlsError::Storage` so this
/// can tell the two apart.
///
/// **Which call site is which, so the list stays auditable.** *Storage-typed* - the rollback
/// reaches this impl already carrying `MlsError::Storage`, because `mls_err` could pull the
/// provider's own error out of the OpenMLS enum: `create`, `join_from_welcome`,
/// `join_by_external_commit`, `add_members`, `remove_members`, `self_update`,
/// `merge_pending_commit`, `merge_staged_commit`, `process_message`, `sweep_past_epochs`, and
/// `delete`/`clear_pending_commit`, which call the storage provider directly. *Rollback-fatal* -
/// the OpenMLS error type has no storage variant to pull out, so the method does not use this
/// impl and maps every `RolledBack(_)` to `NeedsReload` itself: `create_message` (see the note
/// there). A new method belongs in one of those two lists before it is merged.
impl From<TxError<MlsError>> for MlsError {
    fn from(e: TxError<MlsError>) -> Self {
        match e {
            TxError::RolledBack(MlsError::Storage(_)) => MlsError::NeedsReload,
            TxError::RolledBack(inner) => inner,
            other => MlsError::Tx(format!("{other:?}")),
        }
    }
}

/// What a committer uploads. `group_info` is **without** the ratchet tree (DS invariant 2).
///
/// OpenMLS produces one `Welcome` addressed to every member added by the commit; dilla's DS API
/// wants it per device, so the same blob is listed once per added device and the DS fans it out.
///
/// `Debug` because the tests `expect_err` on `Result<CommitBundle, MlsError>`, which requires it.
#[derive(Debug)]
pub struct CommitBundle {
    pub commit: MlsMessageOut,
    pub welcomes: Vec<(DeviceId, MlsMessageOut)>,
    pub group_info: Option<GroupInfo>,
}

/// What one staged commit proposes: the three shapes `DillaGroup` builds.
enum CommitShape<'a> {
    Add(&'a [KeyPackage]),
    Remove(&'a [LeafNodeIndex]),
    Update,
}

/// An application sender authenticated to its MLS leaf. Validation against signed device lists
/// belongs to the later device-list work (DEV-W20).
pub struct ReceivedApplication {
    pub envelope: Envelope,
    pub sender_leaf: u32,
    pub sender: crate::identity::CredentialIdentity,
    pub epoch: u64,
}

impl core::fmt::Debug for ReceivedApplication {
    fn fmt(&self, f: &mut core::fmt::Formatter<'_>) -> core::fmt::Result {
        f.debug_struct("ReceivedApplication")
            .field("sender_leaf", &self.sender_leaf)
            .field("sender_device", &self.sender.device_id)
            .field("epoch", &self.epoch)
            .field("msg_id", &self.envelope.msg_id)
            .finish()
    }
}

#[derive(Debug)]
pub enum DillaProcessed {
    Application(Box<ReceivedApplication>),
    Proposal(Box<QueuedProposal>),
    ExternalJoinProposal(Box<QueuedProposal>),
    StagedCommit(Box<StagedCommit>, ([u8; 16], [u8; 16])),
    OwnPendingCommit,
    OwnPrivateMessage,
}

/// `Debug` because the tests `expect_err` on `Result<DillaGroup, MlsError>`, which requires it.
/// `MlsGroup` itself derives `Debug` in 0.9.0, and nothing here prints a secret: the derive only
/// reaches OpenMLS's own redacted formatting.
#[derive(Debug)]
pub struct DillaGroup {
    group: MlsGroup,
    binding: DillaBinding,
}

/// One leaf of a call group as the media key ring needs it.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct RosterEntry {
    pub leaf_index: u32,
    pub device_id: DeviceId,
    pub user_id: UserId,
}

/// One call-group epoch's media keys: the SFrame base key, this device's leaf and the roster,
/// all read from the same epoch (`DillaGroup::media_epoch`).
pub struct MediaEpoch {
    pub epoch: u64,
    pub base_key: zeroize::Zeroizing<[u8; NK]>,
    pub own_leaf: u32,
    pub roster: Vec<RosterEntry>,
}

fn openmls<E: core::fmt::Debug>(e: E) -> MlsError {
    MlsError::OpenMls(format!("{e:?}"))
}

/// OpenMLS names the provider's own failure in a plain `StorageError(_)` variant: no `#[from]`,
/// no `#[source]`, so `std::error::Error::source()` never reaches it and `format!("{e:?}")`
/// flattens it into a string. A typed match is the only way the wrapper can tell "the database
/// refused the write" from "the protocol refused the operation"; see the `From<TxError<MlsError>>`
/// impl above for why the difference matters.
trait SplitStorage: Sized {
    fn split_storage(self) -> Result<StorageError, Self>;
}

macro_rules! split_storage {
    ($($ty:ident),* $(,)?) => {$(
        impl SplitStorage for $ty<StorageError> {
            // `DeletePastEpochSecretsError` has exactly one variant, so its `other` arm is
            // unreachable; every other enum in the list needs it.
            #[allow(unreachable_patterns)]
            fn split_storage(self) -> Result<StorageError, Self> {
                match self {
                    $ty::StorageError(e) => Ok(e),
                    other => Err(other),
                }
            }
        }
    )*};
}

split_storage!(
    NewGroupError,
    WelcomeError,
    ExternalCommitError,
    ProcessMessageError,
    DeletePastEpochSecretsError,
    MergeCommitError,
);

/// The three commit builders wrap theirs one level deeper, in
/// `CommitBuilderStageError::KeyStoreError` - the only storage variant that enum has.
macro_rules! split_storage_commit {
    ($($ty:ident),* $(,)?) => {$(
        impl SplitStorage for $ty<StorageError> {
            fn split_storage(self) -> Result<StorageError, Self> {
                match self {
                    $ty::StorageError(e)
                    | $ty::CommitBuilderStageError(CommitBuilderStageError::KeyStoreError(e)) => {
                        Ok(e)
                    }
                    other => Err(other),
                }
            }
        }
    )*};
}

split_storage_commit!(AddMembersError, RemoveMembersError, SelfUpdateError);

impl SplitStorage for MergePendingCommitError<StorageError> {
    fn split_storage(self) -> Result<StorageError, Self> {
        match self {
            MergePendingCommitError::MergeCommitError(MergeCommitError::StorageError(e)) => Ok(e),
            other => Err(other),
        }
    }
}

/// `openmls`, but keeping a storage failure typed.
fn mls_err<E: SplitStorage + core::fmt::Debug>(e: E) -> MlsError {
    match e.split_storage() {
        Ok(storage) => MlsError::Storage(storage),
        Err(other) => openmls(other),
    }
}

/// The user a credential belongs to, read from its `CredentialIdentity`.
fn user_of_credential(credential: &Credential) -> Result<UserId, MlsError> {
    let basic = BasicCredential::try_from(credential.clone()).map_err(openmls)?;
    crate::identity::CredentialIdentity::decode(basic.identity())
        .map(|id| id.user_id)
        .map_err(MlsError::Protocol)
}

/// The device a KeyPackage belongs to, read from its leaf credential.
fn device_of(kp: &KeyPackage) -> Result<DeviceId, MlsError> {
    // Verified in step 1: `KeyPackage::leaf_node(&self) -> &LeafNode`
    // (openmls-0.9.0/src/key_packages/mod.rs:477).
    let credential = kp.leaf_node().credential().clone();
    let basic = BasicCredential::try_from(credential).map_err(openmls)?;
    let identity = crate::identity::CredentialIdentity::decode(basic.identity())
        .map_err(MlsError::Protocol)?;
    Ok(identity.device_id)
}

#[cfg(feature = "test-hooks")]
thread_local! {
    static SFRAME_BASE_KEY_CALLS: core::cell::Cell<u64> = const { core::cell::Cell::new(0) };
}

impl DillaGroup {
    /// T1 create: 8 writes in one transaction.
    pub fn create(
        provider: &DillaProvider,
        signer: &SignatureKeyPair,
        credential: CredentialWithKey,
        group_id: GroupId,
        binding: DillaBinding,
        ext_senders: Option<ExternalSendersExtension>,
    ) -> Result<Self, MlsError> {
        let config = create_config(&binding, ext_senders)?;
        let group = provider.storage().transaction(|| {
            MlsGroup::new_with_group_id(provider, signer, &config, group_id, credential)
                .map_err(mls_err)
        })?;
        Ok(Self { group, binding })
    }

    pub fn load(provider: &DillaProvider, group_id: &GroupId) -> Result<Option<Self>, MlsError> {
        Self::load_stored(provider.storage(), group_id)
    }

    /// `load` from the storage alone: loading is a read, so it needs no crypto provider. The app
    /// migration (`client::migrate_app`) is handed only the one `DillaStorage` and must not build
    /// a second one over the same connection.
    pub(crate) fn load_stored(
        storage: &DillaStorage,
        group_id: &GroupId,
    ) -> Result<Option<Self>, MlsError> {
        let Some(group) = MlsGroup::load(storage, group_id).map_err(MlsError::Storage)? else {
            return Ok(None);
        };
        let context = group.public_group().group_context();
        let binding = DillaBinding::from_group_context(context).map_err(MlsError::Protocol)?;
        Ok(Some(Self { group, binding }))
    }

    /// T2 welcome join. The binding is checked on the **staged** welcome, before `into_group`
    /// writes anything: a mismatched Welcome must leave no group behind.
    pub fn join_from_welcome(
        provider: &DillaProvider,
        welcome: Welcome,
        ratchet_tree: RatchetTreeIn,
        expected: &DillaBinding,
    ) -> Result<Self, MlsError> {
        Self::join_welcome(provider, welcome, ratchet_tree, expected, None)
    }

    /// `join_from_welcome`, and the staged group context must also match the delivery service's
    /// label: its group id (`ProtocolError::Binding`), its epoch and its tree hash
    /// (`MlsError::WelcomeLabel`). The checks run in this order, before `into_group`.
    ///
    /// The id and the binding are public, so a member can build a private group that carries
    /// both and address its Welcome to a device the real group adds; OpenMLS prefers the
    /// Welcome's own `ratchet_tree` extension over the served tree, so the tree argument does
    /// not constrain it either. The label is the instance's view of the real group after the
    /// commit that added the device, which such a Welcome cannot match.
    pub fn join_from_welcome_labelled(
        provider: &DillaProvider,
        welcome: Welcome,
        ratchet_tree: RatchetTreeIn,
        expected: &DillaBinding,
        label: WelcomeLabel<'_>,
    ) -> Result<Self, MlsError> {
        Self::join_welcome(provider, welcome, ratchet_tree, expected, Some(label))
    }

    fn join_welcome(
        provider: &DillaProvider,
        welcome: Welcome,
        ratchet_tree: RatchetTreeIn,
        expected: &DillaBinding,
        label: Option<WelcomeLabel<'_>>,
    ) -> Result<Self, MlsError> {
        let group = provider.storage().transaction(|| {
            let staged = StagedWelcome::new_from_welcome(
                provider,
                &join_config(expected.kind),
                welcome,
                Some(ratchet_tree),
            )
            .map_err(mls_err)?;
            let context = staged.group_context();
            let binding = DillaBinding::from_group_context(context).map_err(MlsError::Protocol)?;
            binding.matches(expected).map_err(MlsError::Protocol)?;
            if let Some(label) = label {
                if context.group_id().as_slice() != label.group_id {
                    return Err(MlsError::Protocol(ProtocolError::Binding));
                }
                if context.epoch().as_u64() != label.epoch || context.tree_hash() != label.tree_hash
                {
                    return Err(MlsError::WelcomeLabel);
                }
            }
            staged.into_group(provider).map_err(mls_err)
        })?;
        Ok(Self {
            group,
            binding: expected.clone(),
        })
    }

    /// T3 external join: 8 writes.
    ///
    /// `MlsGroup::join_by_external_commit` is `#[deprecated]` in 0.9.0 in favour of
    /// `MlsGroup::external_commit_builder`. The brief pins this call, and replacing it is a
    /// contract change, not an implementation detail, so the deprecation is allowed here and
    /// nowhere else; `-D warnings` would otherwise reject the crate.
    #[allow(deprecated)]
    pub fn join_by_external_commit(
        provider: &DillaProvider,
        signer: &SignatureKeyPair,
        credential: CredentialWithKey,
        group_info: VerifiableGroupInfo,
        ratchet_tree: RatchetTreeIn,
        expected: &DillaBinding,
    ) -> Result<(Self, MlsMessageOut, Option<GroupInfo>), MlsError> {
        let (group, binding, commit, info) = provider.storage().transaction(|| {
            let (group, commit, info) = MlsGroup::join_by_external_commit(
                provider,
                signer,
                Some(ratchet_tree),
                group_info,
                &join_config(expected.kind),
                Some(super::leaf_capabilities()),
                None,
                &[],
                credential,
            )
            .map_err(mls_err)?;
            // Inside the closure, exactly where `join_from_welcome` puts it: the call above has
            // already written the joiner's whole group state through the provider, so a check made
            // after the transaction has committed refuses the join but leaves the group row
            // behind, and the next `DillaGroup::load` hands back a group this client rejected.
            // Failing here rolls every one of those writes back.
            let binding = DillaBinding::from_group_context(group.public_group().group_context())
                .map_err(MlsError::Protocol)?;
            binding.matches(expected).map_err(MlsError::Protocol)?;
            Ok((group, binding, commit, info))
        })?;
        Ok((Self { group, binding }, commit, info))
    }

    /// T4 create commit.
    pub fn add_members(
        &mut self,
        provider: &DillaProvider,
        signer: &SignatureKeyPair,
        key_packages: &[KeyPackage],
    ) -> Result<CommitBundle, MlsError> {
        if key_packages.len() > super::MAX_ADDS_PER_COMMIT {
            return Err(MlsError::Protocol(ProtocolError::Binding));
        }
        let devices: Vec<DeviceId> = key_packages
            .iter()
            .map(device_of)
            .collect::<Result<_, _>>()?;
        let bundle = self.stage_commit(provider, signer, CommitShape::Add(key_packages))?;
        let welcome = bundle
            .to_welcome_msg()
            .ok_or_else(|| MlsError::OpenMls("an Add commit produced no Welcome".into()))?;
        let (commit, _, group_info) = bundle.into_contents();
        Ok(CommitBundle {
            commit,
            welcomes: devices.into_iter().map(|d| (d, welcome.clone())).collect(),
            group_info,
        })
    }

    /// T4 create commit.
    pub fn remove_members(
        &mut self,
        provider: &DillaProvider,
        signer: &SignatureKeyPair,
        members: &[LeafNodeIndex],
    ) -> Result<CommitBundle, MlsError> {
        if members.is_empty() {
            return Err(MlsError::OpenMls("a Remove commit names no member".into()));
        }
        let bundle = self.stage_commit(provider, signer, CommitShape::Remove(members))?;
        let (commit, welcome, group_info) = bundle.into_contents();
        // `CommitBundle.welcomes` is the DS's per-device fan-out key. OpenMLS emits a Welcome only
        // for a commit that adds members, so this is always `None` here; inventing an all-zero
        // `DeviceId` for it would address a Welcome to a device that does not exist. If a future
        // commit path ever both adds and removes, resolve the added devices the way `add_members`
        // does rather than reinstating a placeholder.
        debug_assert!(welcome.is_none(), "a remove-only commit emits no Welcome");
        Ok(CommitBundle {
            commit,
            welcomes: Vec::new(),
            group_info,
        })
    }

    /// T4 create commit.
    pub fn self_update(
        &mut self,
        provider: &DillaProvider,
        signer: &SignatureKeyPair,
    ) -> Result<CommitBundle, MlsError> {
        let bundle = self.stage_commit(provider, signer, CommitShape::Update)?;
        // A self-update also commits every proposal in the queue, and an instance Add queued by
        // `store_pending_proposal` (invariant 6) makes the commit carry a Welcome. It is addressed
        // to exactly the devices those Adds name, read off the staged commit, as `add_members`
        // addresses its own: dropping it would add a leaf whose device can never join.
        let devices: Vec<DeviceId> = match self.group.pending_commit() {
            Some(staged) => staged
                .add_proposals()
                .map(|add| device_of(add.add_proposal().key_package()))
                .collect::<Result<_, _>>()?,
            None => Vec::new(),
        };
        let welcome = bundle.to_welcome_msg();
        let (commit, _, group_info) = bundle.into_contents();
        let welcomes = match welcome {
            Some(w) => devices.into_iter().map(|d| (d, w.clone())).collect(),
            None => Vec::new(),
        };
        Ok(CommitBundle {
            commit,
            welcomes,
            group_info,
        })
    }

    /// Proposes the removal of this device's own leaf (protocol/01 "Leaving", DEV-47). A device
    /// cannot commit its own removal — OpenMLS refuses that commit with `CannotRemoveSelf` — so it
    /// posts this proposal to `POST /v1/groups/{id}/proposal` and another member's commit applies
    /// it. The device keeps its group state until that commit arrives.
    pub fn leave(
        &mut self,
        provider: &DillaProvider,
        signer: &SignatureKeyPair,
    ) -> Result<MlsMessageOut, MlsError> {
        let group = &mut self.group;
        Ok(provider
            .storage()
            .transaction(|| group.leave_group(provider, signer).map_err(openmls))?)
    }

    /// Builds and stages one commit through `MlsGroup::commit_builder`, asking it for the
    /// GroupInfo of the epoch the commit creates.
    ///
    /// Invariant 4 accepts a commit only with the GroupInfo of epoch n + 1. `add_members`,
    /// `remove_members` and `self_update` on `MlsGroup` return a GroupInfo only when the group uses
    /// the ratchet-tree extension, which dilla's never do (invariant 2: the DS serves the tree),
    /// and exporting one after `merge_pending_commit` is too late: a refused commit cannot be
    /// taken back once merged. `create_group_info(true)` makes the builder sign the n + 1
    /// GroupInfo while it stages the commit - without the tree, with the external public key
    /// (openmls-0.9.0/src/group/mls_group/commit_builder.rs:1036-1133).
    ///
    /// Each shape is the one the `MlsGroup` convenience method it replaces builds
    /// (membership.rs:169-175, :246-251, updates.rs:43-49). Those methods also refuse a group
    /// with a pending commit or one it was removed from (`is_operational`, which is crate-private);
    /// the same two refusals are made here before the builder is touched.
    fn stage_commit(
        &mut self,
        provider: &DillaProvider,
        signer: &SignatureKeyPair,
        shape: CommitShape<'_>,
    ) -> Result<CommitMessageBundle, MlsError> {
        if self.group.pending_commit().is_some() {
            return Err(openmls(MlsGroupStateError::PendingCommit));
        }
        if !self.group.is_active() {
            return Err(openmls(MlsGroupStateError::UseAfterEviction));
        }
        let group = &mut self.group;
        let bundle = provider.storage().transaction(|| {
            let builder = group.commit_builder();
            let builder = match shape {
                CommitShape::Add(kps) => builder
                    .propose_adds(kps.iter().cloned())
                    .force_self_update(true),
                CommitShape::Remove(members) => builder.propose_removals(members.iter().copied()),
                // Verified in step 1: `LeafNodeParameters` derives `Default`
                // (openmls-0.9.0/src/treesync/node/leaf_node.rs:70).
                CommitShape::Update => builder
                    .leaf_node_parameters(LeafNodeParameters::default())
                    .consume_proposal_store(true),
            };
            builder
                .load_psks(provider.storage())
                .map_err(openmls)?
                .create_group_info(true)
                .build(provider.rand(), provider.crypto(), signer, |_| true)
                .map_err(openmls)?
                .stage_commit(provider)
                .map_err(|e| match e {
                    CommitBuilderStageError::KeyStoreError(storage) => MlsError::Storage(storage),
                    other => openmls(other),
                })
        })?;
        Ok(bundle)
    }

    /// T11 housekeeping: drop a staged commit that will never be merged.
    ///
    /// Two callers need this. The DS 409 loser — "the loser clears its pending commit" (spec line
    /// 591, `protocol/02-delivery-service.md` `commit_conflict`) — and task 13's fixture
    /// generator, which produces ten **alternative** commits at one epoch. Without it the group
    /// stays in `MlsGroupState::PendingCommit` after the first staged commit and every later
    /// `add_members` / `remove_members` / `self_update` fails with
    /// `MlsGroupStateError::PendingCommit`.
    ///
    /// `MlsGroup::clear_pending_commit` takes the **storage**, not the provider (facts-openmls
    /// §4.4, verified from source), and writes `group_state`, so it runs inside a transaction like
    /// every other state change (gap-7 §4, T11).
    pub fn clear_pending_commit(&mut self, provider: &DillaProvider) -> Result<(), MlsError> {
        let group = &mut self.group;
        provider.storage().transaction(|| {
            group
                .clear_pending_commit(provider.storage())
                .map_err(MlsError::Storage)
        })?;
        Ok(())
    }

    /// T5 merge: 13-15 writes.
    pub fn merge_pending_commit(&mut self, provider: &DillaProvider) -> Result<(), MlsError> {
        let group = &mut self.group;
        provider
            .storage()
            .transaction(|| group.merge_pending_commit(provider).map_err(mls_err))?;
        self.sweep_past_epochs(provider)
    }

    /// T5 merge: 13-15 writes.
    ///
    /// Call this **only** with a `StagedCommit` that `process_message` handed back: that is where
    /// the proposal policy of protocol/01 is enforced (`validate_staged_commit`), and this method
    /// has neither the commit's `Sender` nor its pre-merge tree to re-check it.
    pub fn merge_staged_commit(
        &mut self,
        provider: &DillaProvider,
        staged: StagedCommit,
    ) -> Result<(), MlsError> {
        let group = &mut self.group;
        provider
            .storage()
            .transaction(|| group.merge_staged_commit(provider, staged).map_err(mls_err))?;
        self.sweep_past_epochs(provider)
    }

    /// T6 send. The MLS `authenticated_data` is set to the envelope's 32-byte commitment before
    /// framing, which is what the DS reads and what the receiver checks.
    pub fn create_message(
        &mut self,
        provider: &DillaProvider,
        signer: &SignatureKeyPair,
        envelope: &Envelope,
    ) -> Result<MlsMessageOut, MlsError> {
        let commitment = envelope.commitment().map_err(MlsError::Protocol)?;
        let body = envelope.encode().map_err(MlsError::Protocol)?;
        let group = &mut self.group;
        // `transaction` returns `Result<T, TxError<MlsError>>`, not `Result<T, MlsError>`; every
        // other call site here ends in `?`, which applies `impl From<TxError<MlsError>> for
        // MlsError`. This one must not: see the `rollback-fatal` note on that impl. In the default
        // (non-`virtual-clients-draft`) build `CreateMessageError` has **no** `StorageError`
        // variant at all (group/mls_group/errors.rs:200-207) - `create_message_internal` flattens
        // every `MessageEncryptionError`, the `StorageError` of `write_message_secrets`
        // (mod.rs:834-838) included, into `LibraryError::custom("Malformed plaintext")`
        // (application.rs:104-110). So `mls_err` cannot tell a database failure apart here, while
        // the in-memory `MlsGroup` has already ratcheted one application generation past the
        // committed state. Any rollback is therefore fatal to this handle.
        let result = provider.storage().transaction(|| {
            // Verified in step 1: `MlsGroup::set_aad(&mut self, aad: Vec<u8>)`
            // (openmls-0.9.0/src/group/mls_group/mod.rs:329).
            group.set_aad(commitment.to_vec());
            group
                .create_message(provider, signer, &body)
                .map_err(openmls)
        });
        match result {
            Ok(out) => Ok(out),
            Err(TxError::RolledBack(_)) => Err(MlsError::NeedsReload),
            Err(other) => Err(MlsError::Tx(format!("{other:?}"))),
        }
    }

    /// T7 receive. A `PublicMessage` writes nothing; a `PrivateMessage` writes the secret tree
    /// exactly once (gap-7 section 1.2).
    pub fn process_message(
        &mut self,
        provider: &DillaProvider,
        message: ProtocolMessage,
    ) -> Result<DillaProcessed, MlsError> {
        let group = &mut self.group;
        let processed = provider
            .storage()
            .transaction(|| group.process_message(provider, message).map_err(mls_err))?;
        let aad = processed.aad().to_vec();
        // Read before `into_content` consumes the message. `sender()` and `credential()` are
        // verified accessors (facts-openmls section 4.10). The credential is only *cloned* here:
        // decoding it as a dilla `CredentialIdentity` happens in the application and commit arms,
        // which carry a member's leaf. For `Sender::External(_)` OpenMLS fills `credential` from the
        // ExternalSenders extension (public_group/process.rs:93-97 and :302) - dilla's own
        // instance credential, whose identity is the three-element array `[1, "instance", id]`,
        // not the ten-element `CredentialIdentity`. Decoding unconditionally refused every
        // instance-sent proposal, the inactivity-Remove path included.
        let sender = processed.sender().clone();
        let credential = processed.credential().clone();
        let epoch = processed.epoch().as_u64();
        Ok(match processed.into_content() {
            ProcessedMessageContent::ApplicationMessage(app) => {
                let Sender::Member(leaf) = sender else {
                    return Err(MlsError::OpenMls(
                        "application message from a non-member".into(),
                    ));
                };
                let basic = BasicCredential::try_from(credential)
                    .map_err(|_| MlsError::Protocol(ProtocolError::Credential))?;
                let identity = crate::identity::CredentialIdentity::decode(basic.identity())
                    .map_err(MlsError::Protocol)?;
                let envelope = Envelope::decode(&app.into_bytes()).map_err(MlsError::Protocol)?;
                envelope
                    .verify_commitment(&aad)
                    .map_err(MlsError::Protocol)?;
                DillaProcessed::Application(Box::new(ReceivedApplication {
                    envelope,
                    sender_leaf: leaf.u32(),
                    sender: identity,
                    epoch,
                }))
            }
            ProcessedMessageContent::ProposalMessage(p) => {
                // The policy table is enforced on a standalone proposal too, not only on the
                // commit that carries it: a proposal handed back here is what the caller queues,
                // and a rule checked only in `validate_staged_commit` lets a forbidden proposal
                // sit in the queue until some other client commits it. `GroupContextExtensions`
                // is the one proposal type whose sender rule dilla can evaluate on its own -
                // Add/Remove need the role snapshot and the leaf credential, which
                // `validate_staged_commit` reads off the staged commit. The sender is the
                // proposal's own, as everywhere else in this policy. The helper is the one the
                // delivery service's public group runs too (`mls::policy`).
                proposal_extensions_unchanged(&p).map_err(MlsError::Protocol)?;
                // An `Update` keeps its proposer's credential (protocol/01), read against the
                // current tree - the same rule `validate_staged_commit` applies to the commit that
                // would carry it.
                proposal_credential_verdict(self.group.public_group(), &p)
                    .map_err(MlsError::Protocol)?;
                DillaProcessed::Proposal(p)
            }
            ProcessedMessageContent::ExternalJoinProposalMessage(p) => {
                DillaProcessed::ExternalJoinProposal(p)
            }
            ProcessedMessageContent::StagedCommitMessage(c) => {
                // A commit's sender is always `Member` or `NewMemberCommit`, both of which carry a
                // real dilla leaf credential, so this is the other arm that may decode it.
                let committer_user = user_of_credential(&credential)?;
                // The proposal policy of protocol/01-groups.md is enforced here, before the caller
                // ever sees the commit: `merge_staged_commit` is a separate call, and a caller
                // that skipped this check would install a commit the protocol forbids.
                // `public_group()` is the tree as it stands *before* the merge, which is the state
                // a `Remove` names.
                validate_staged_commit(
                    self.binding.kind,
                    &self.own_user()?,
                    &committer_user,
                    &sender,
                    self.group.public_group(),
                    c.as_ref(),
                )
                .map_err(MlsError::Protocol)?;
                let basic = BasicCredential::try_from(credential).map_err(openmls)?;
                let identity = crate::identity::CredentialIdentity::decode(basic.identity())
                    .map_err(MlsError::Protocol)?;
                DillaProcessed::StagedCommit(
                    c,
                    (*identity.user_id.as_bytes(), *identity.device_id.as_bytes()),
                )
            }
            ProcessedMessageContent::OwnPendingCommit => DillaProcessed::OwnPendingCommit,
            ProcessedMessageContent::OwnPrivateMessage => DillaProcessed::OwnPrivateMessage,
        })
    }

    /// Queues a proposal `process_message` handed back as `DillaProcessed::Proposal`, so the next
    /// commit this client builds (`self_update` consumes the queue) covers it. Invariant 4 refuses a
    /// commit that does not reference every outstanding instance proposal, so a member that drops
    /// the proposals it receives can never commit while the instance has one outstanding.
    ///
    /// `MlsGroup::store_pending_proposal` takes the **storage** and writes the queued proposal, so it
    /// runs inside a transaction like every other state change.
    ///
    /// A member's `Remove` never displaces the instance's `Remove` of the same leaf (protocol/01,
    /// "Client policy for proposals from members"). When both are queued, OpenMLS commits only the
    /// LATER of the two in this queue's order (`ProposalQueue::filter_proposals` keeps one proposal
    /// per removed leaf, and a later `Remove` has priority over an earlier one), and the delivery
    /// service refuses a commit that leaves the instance's out. The delivery service's own order is
    /// always the member's first — it refuses a member's `Remove` of a leaf the instance is already
    /// removing — but a client may receive them out of that order (a `GET /proposals` listing, a
    /// replay), so a member's `Remove` that arrives after the instance's is queued in front of it:
    /// the instance's is taken out and queued again behind it. Both stay queued, so a commit that
    /// references either one can still be processed.
    pub fn store_pending_proposal(
        &mut self,
        provider: &DillaProvider,
        proposal: QueuedProposal,
    ) -> Result<(), MlsError> {
        let behind: Vec<QueuedProposal> = match (proposal.proposal(), proposal.sender()) {
            (Proposal::Remove(remove), Sender::Member(_)) => self
                .group
                .pending_proposals()
                .filter(|p| {
                    matches!(p.sender(), Sender::External(_))
                        && matches!(p.proposal(), Proposal::Remove(r) if r.removed() == remove.removed())
                })
                .cloned()
                .collect(),
            _ => Vec::new(),
        };
        let group = &mut self.group;
        provider.storage().transaction(|| {
            for p in &behind {
                group
                    .remove_pending_proposal(provider.storage(), p.proposal_reference_ref())
                    .map_err(openmls)?;
            }
            group
                .store_pending_proposal(provider.storage(), proposal)
                .map_err(MlsError::Storage)?;
            for p in behind {
                group
                    .store_pending_proposal(provider.storage(), p)
                    .map_err(MlsError::Storage)?;
            }
            Ok::<(), MlsError>(())
        })?;
        Ok(())
    }

    /// Testkit only (feature `testkit`): re-queues every member `Remove` that shares its leaf with a
    /// queued instance `Remove` BEHIND that instance `Remove`, bypassing `store_pending_proposal`'s
    /// ordering rule. It is the queue a committer not built on this crate may hold — a replay, or a
    /// client that queues in arrival order from a listing — under which OpenMLS commits the
    /// member's `Remove` instead of the instance's; the delivery service must accept that commit
    /// (protocol/02 invariant 4 clause 1). Answers how many proposals it moved.
    #[cfg(feature = "testkit")]
    pub fn requeue_member_removes_last(
        &mut self,
        provider: &DillaProvider,
    ) -> Result<usize, MlsError> {
        let instance_leaves: Vec<LeafNodeIndex> = self
            .group
            .pending_proposals()
            .filter(|p| matches!(p.sender(), Sender::External(_)))
            .filter_map(|p| match p.proposal() {
                Proposal::Remove(r) => Some(r.removed()),
                _ => None,
            })
            .collect();
        let moved: Vec<QueuedProposal> = self
            .group
            .pending_proposals()
            .filter(|p| {
                matches!(p.sender(), Sender::Member(_))
                    && matches!(p.proposal(), Proposal::Remove(r) if instance_leaves.contains(&r.removed()))
            })
            .cloned()
            .collect();
        let group = &mut self.group;
        provider.storage().transaction(|| {
            for p in &moved {
                group
                    .remove_pending_proposal(provider.storage(), p.proposal_reference_ref())
                    .map_err(openmls)?;
            }
            for p in moved.iter().cloned() {
                group
                    .store_pending_proposal(provider.storage(), p)
                    .map_err(MlsError::Storage)?;
            }
            Ok::<(), MlsError>(())
        })?;
        Ok(moved.len())
    }

    /// Takes this device's own pending `Remove` of its own leaf (`leave`) back out of its queue. A
    /// delivery service refuses that proposal with `409 E_REMOVE_PENDING` when the instance is
    /// already removing the leaf (protocol/02 invariant 6):
    /// the device is being removed, and the refused proposal, which no other member holds, must not
    /// stay queued — OpenMLS refuses to send an application message while a proposal is pending.
    pub fn withdraw_leave(&mut self, provider: &DillaProvider) -> Result<(), MlsError> {
        let own = self.group.own_leaf_index();
        let refs: Vec<openmls::ciphersuite::hash_ref::ProposalRef> = self
            .group
            .pending_proposals()
            .filter(|p| {
                matches!(p.sender(), Sender::Member(l) if *l == own)
                    && matches!(p.proposal(), Proposal::Remove(r) if r.removed() == own)
            })
            .map(|p| p.proposal_reference_ref().clone())
            .collect();
        let group = &mut self.group;
        provider.storage().transaction(|| {
            for r in &refs {
                group
                    .remove_pending_proposal(provider.storage(), r)
                    .map_err(openmls)?;
            }
            Ok::<(), MlsError>(())
        })?;
        Ok(())
    }

    /// Takes every queued `Add` that `keep` refuses back out of the queue, so the next
    /// `self_update` (which consumes the whole queue) does not commit it. `keep` answers
    /// `Ok(false)` to drop an Add and an error to refuse; on an error nothing is removed.
    pub fn drop_queued_adds<E: From<MlsError>>(
        &mut self,
        provider: &DillaProvider,
        mut keep: impl FnMut(&KeyPackage) -> Result<bool, E>,
    ) -> Result<(), E> {
        let mut refs: Vec<openmls::ciphersuite::hash_ref::ProposalRef> = Vec::new();
        for p in self.group.pending_proposals() {
            if let Proposal::Add(add) = p.proposal()
                && !keep(add.key_package())?
            {
                refs.push(p.proposal_reference_ref().clone());
            }
        }
        if refs.is_empty() {
            return Ok(());
        }
        let group = &mut self.group;
        provider
            .storage()
            .transaction(|| {
                for r in &refs {
                    group
                        .remove_pending_proposal(provider.storage(), r)
                        .map_err(openmls)?;
                }
                Ok::<(), MlsError>(())
            })
            .map_err(MlsError::from)?;
        Ok(())
    }

    /// How many members the group has, as this client's tree holds it.
    pub fn member_count(&self) -> usize {
        self.group.members().count()
    }

    /// T8 delete: 14 writes.
    pub fn delete(&mut self, provider: &DillaProvider) -> Result<(), MlsError> {
        let group = &mut self.group;
        provider
            .storage()
            .transaction(|| group.delete(provider.storage()).map_err(MlsError::Storage))?;
        Ok(())
    }

    /// T11 housekeeping. A no-op for pairing groups, which keep no past epochs.
    pub fn sweep_past_epochs(&mut self, provider: &DillaProvider) -> Result<(), MlsError> {
        let Some(policy) = past_epoch_sweep(self.binding.kind) else {
            return Ok(());
        };
        let group = &mut self.group;
        provider.storage().transaction(|| {
            group
                .delete_past_epoch_secrets(provider, policy)
                .map_err(mls_err)
        })?;
        Ok(())
    }

    /// The group's binding, as its authenticated GroupContext carries it.
    pub fn binding(&self) -> Result<&DillaBinding, MlsError> {
        Ok(&self.binding)
    }

    pub fn kind(&self) -> GroupKind {
        self.binding.kind
    }

    pub fn group_id(&self) -> &GroupId {
        self.group.group_id()
    }

    pub fn epoch(&self) -> u64 {
        self.group.epoch().as_u64()
    }

    /// Authenticated members of the current MLS tree, including each signature key.
    pub fn members(&self) -> Vec<Member> {
        self.group.members().collect()
    }

    /// One authenticated current-tree member by leaf index.
    pub fn member_at(&self, leaf_index: u32) -> Option<Member> {
        self.group.member_at(LeafNodeIndex::new(leaf_index))
    }

    /// Hash from the signed GroupInfo for this current authenticated tree.
    pub fn tree_hash(
        &self,
        provider: &DillaProvider,
        signer: &SignatureKeyPair,
    ) -> Result<[u8; 32], MlsError> {
        let info = self.export_group_info(provider, signer)?;
        let openmls::framing::MlsMessageBodyOut::GroupInfo(info) = info.body() else {
            return Err(MlsError::OpenMls("export was not GroupInfo".into()));
        };
        info.group_context()
            .tree_hash()
            .try_into()
            .map_err(|_| MlsError::OpenMls("tree hash is not 32 bytes".into()))
    }

    /// Whether a staged commit awaits a merge or abort.
    pub fn has_pending_commit(&self) -> bool {
        self.group.pending_commit().is_some()
    }

    pub fn own_leaf_index(&self) -> LeafNodeIndex {
        self.group.own_leaf_index()
    }

    /// The user this device belongs to, read from its own leaf credential. `own_leaf_node` is in
    /// the 0.9.0 `MlsGroup` method index (facts-openmls §4.11); `LeafNode::credential()` is
    /// "Needs verification" item 9.
    fn own_user(&self) -> Result<UserId, MlsError> {
        let leaf = self.group.own_leaf_node().ok_or(MlsError::NotFound)?;
        user_of_credential(leaf.credential())
    }

    pub fn epoch_authenticator(&self) -> Result<[u8; 32], MlsError> {
        // Verified in step 1: `EpochAuthenticator::as_slice(&self) -> &[u8]`
        // (openmls-0.9.0/src/schedule/mod.rs:209).
        let raw = self.group.epoch_authenticator().as_slice();
        if raw.len() != 32 {
            return Err(MlsError::OpenMls(format!(
                "epoch authenticator is {} bytes",
                raw.len()
            )));
        }
        let mut out = [0u8; 32];
        out.copy_from_slice(raw);
        Ok(out)
    }

    /// Test builds only: how many times `sframe_base_key` ran on this thread. Counted inside the
    /// derivation, so a caller cannot derive a key without the count seeing it.
    #[cfg(feature = "test-hooks")]
    pub fn test_sframe_base_key_calls() -> u64 {
        SFRAME_BASE_KEY_CALLS.with(core::cell::Cell::get)
    }

    /// `MLS-Exporter("SFrame 1.0 Base Key", "", 16)` - the spec's corrected definition.
    pub fn sframe_base_key(&self, provider: &DillaProvider) -> Result<[u8; NK], MlsError> {
        use openmls_traits::OpenMlsProvider as _;
        #[cfg(feature = "test-hooks")]
        SFRAME_BASE_KEY_CALLS.with(|n| n.set(n.get() + 1));
        // OpenMLS now returns an ExportedSecret whose inner bytes are zeroized on drop.
        let raw = self
            .group
            .export_secret(provider.crypto(), crate::sframe::LABEL_BASE_KEY, &[], NK)
            .map_err(openmls)?;
        let mut out = [0u8; NK];
        out.copy_from_slice(raw.as_ref());
        Ok(out)
    }

    /// Every leaf of this client's tree whose credential decodes, as `(leaf, device, user)`, in
    /// leaf order. A device's leaf is not stable across epochs (a resync lands at the leftmost
    /// free index; a Remove frees an index for the next joiner), so a roster is only meaningful
    /// together with the epoch it was read in.
    pub fn roster(&self) -> Vec<RosterEntry> {
        self.group
            .members()
            .filter_map(|m| {
                let basic = BasicCredential::try_from(m.credential).ok()?;
                let id = crate::identity::CredentialIdentity::decode(basic.identity()).ok()?;
                Some(RosterEntry {
                    leaf_index: m.index.u32(),
                    device_id: id.device_id,
                    user_id: id.user_id,
                })
            })
            .collect()
    }

    /// The leaves `d` holds in this epoch: empty when it is not a member. More than one is legal
    /// for OpenMLS and refused by the media key ring.
    pub fn leaf_of_device(&self, d: &DeviceId) -> Vec<u32> {
        self.roster()
            .into_iter()
            .filter(|e| &e.device_id == d)
            .map(|e| e.leaf_index)
            .collect()
    }

    /// What the media worker installs per call-group epoch, read in one call so the base key and
    /// the roster are of the same epoch: snapshot it before merging the next commit.
    pub fn media_epoch(&self, provider: &DillaProvider) -> Result<MediaEpoch, MlsError> {
        Ok(MediaEpoch {
            epoch: self.epoch(),
            base_key: zeroize::Zeroizing::new(self.sframe_base_key(provider)?),
            own_leaf: self.own_leaf_index().u32(),
            roster: self.roster(),
        })
    }

    /// The GroupInfo a committer uploads: **without** the ratchet tree, because the DS serves the
    /// tree from its own `PublicGroup` (DS invariant 2).
    ///
    /// Returns the `MlsMessageOut` that carries it: `MlsGroup::export_group_info` returns a
    /// message, not a bare `GroupInfo` (facts-openmls section 4.7).
    pub fn export_group_info(
        &self,
        provider: &DillaProvider,
        signer: &SignatureKeyPair,
    ) -> Result<MlsMessageOut, MlsError> {
        use openmls_traits::OpenMlsProvider as _;
        self.group
            .export_group_info(provider.crypto(), signer, false)
            .map_err(openmls)
    }

    pub fn export_ratchet_tree(&self) -> RatchetTree {
        self.group.export_ratchet_tree()
    }
}

/// The roster and the media epoch over a real call group: create, Add, Remove, external join.
/// Native only, like `tests/mls_roundtrip.rs`: the provider is an in-memory SQLite connection.
#[cfg(all(test, not(target_arch = "wasm32")))]
mod media_tests {
    use super::*;
    use crate::identity::{CredentialIdentity, Kind, SignerTier, SskSigner, Tier, UmkSigner};
    use crate::ids::InstanceId;
    use crate::mls::{CIPHERSUITE, build_key_package};
    use std::sync::{Arc, Mutex};
    use tls_codec::{Deserialize as _, Serialize as _};

    fn provider() -> DillaProvider {
        let conn = rusqlite::Connection::open_in_memory().expect("sqlite");
        let p = DillaProvider::new(Arc::new(Mutex::new(conn)));
        p.storage().migrate().expect("migrate");
        p
    }

    fn member(user: u8, device: u8) -> (DillaProvider, SignatureKeyPair, CredentialWithKey) {
        let umk = UmkSigner::from_bytes(&[user; 32]);
        let ssk = SskSigner::from_bytes(&[user.wrapping_add(0x40); 32]);
        let identity = CredentialIdentity {
            v: 1,
            umk_pub: umk.public(),
            user_id: UserId::from_bytes([user; 16]),
            device_id: DeviceId::from_bytes([device; 16]),
            kind: Kind::User,
            tier: Tier::Native,
            signer_tier: SignerTier::Native,
            ssk_pub: ssk.public(),
            sig_umk_ssk: umk.sign_ssk(&ssk.public()),
            sig_ssk_dev: [0u8; 64],
        };
        let keys = SignatureKeyPair::new(CIPHERSUITE.signature_algorithm()).expect("keygen");
        let credential = CredentialWithKey {
            credential: BasicCredential::new(identity.encode()).into(),
            signature_key: keys.public().into(),
        };
        let p = provider();
        keys.store(p.storage()).expect("store signer");
        (p, keys, credential)
    }

    fn call_binding() -> DillaBinding {
        DillaBinding {
            v: 1,
            instance_id: InstanceId::from_bytes([0x11; 16]),
            community_id: None,
            target_id: [0x33; 16],
            kind: GroupKind::Call,
            policy_version: 1,
            e2ee_version: 1,
            media_version: GroupKind::Call.media_version(),
        }
    }

    fn wire(message: MlsMessageOut) -> MlsMessageBodyIn {
        let bytes = message.tls_serialize_detached().expect("serialize");
        MlsMessageIn::tls_deserialize_exact(&bytes)
            .expect("deserialize")
            .extract()
    }

    fn rows(g: &DillaGroup) -> Vec<(u32, u8)> {
        g.roster()
            .iter()
            .map(|e| (e.leaf_index, e.device_id.as_bytes()[0]))
            .collect()
    }

    /// The upstream export result owns and zeroizes the secret after we copy the base key.
    #[test]
    fn the_base_key_exporter_output_is_zeroized() {
        fn assert_zeroize_on_drop<T: zeroize::ZeroizeOnDrop>() {}
        assert_zeroize_on_drop::<openmls::group::ExportedSecret<openmls::group::GroupExport>>();
    }

    #[test]
    fn the_roster_follows_add_remove_and_an_external_join() {
        let (alice_p, alice_s, alice_c) = member(0xaa, 0x01);
        let (bob_p, bob_s, bob_c) = member(0xbb, 0x02);
        let (carol_p, carol_s, carol_c) = member(0xcc, 0x03);
        let b = call_binding();
        let mut alice = DillaGroup::create(
            &alice_p,
            &alice_s,
            alice_c,
            GroupId::from_slice(&[0x44; 16]),
            b.clone(),
            None,
        )
        .expect("create");
        assert_eq!(rows(&alice), [(0, 0x01)]);
        assert_eq!(alice.roster()[0].user_id, UserId::from_bytes([0xaa; 16]));

        let bob_kp = build_key_package(&bob_p, &bob_s, bob_c, false).expect("key package");
        alice
            .add_members(&alice_p, &alice_s, &[bob_kp.key_package().clone()])
            .expect("add");
        alice.merge_pending_commit(&alice_p).expect("merge");
        assert_eq!(rows(&alice), [(0, 0x01), (1, 0x02)]);
        assert_eq!(alice.leaf_of_device(&DeviceId::from_bytes([0x02; 16])), [1]);

        alice
            .remove_members(&alice_p, &alice_s, &[LeafNodeIndex::new(1)])
            .expect("remove");
        alice.merge_pending_commit(&alice_p).expect("merge");
        assert_eq!(rows(&alice), [(0, 0x01)]);
        assert!(
            alice
                .leaf_of_device(&DeviceId::from_bytes([0x02; 16]))
                .is_empty()
        );

        // Carol joins by external commit and lands on the index Bob's Remove freed.
        let MlsMessageBodyIn::GroupInfo(info) = wire(
            alice
                .export_group_info(&alice_p, &alice_s)
                .expect("group info"),
        ) else {
            panic!("not a GroupInfo");
        };
        let (carol, _, _) = DillaGroup::join_by_external_commit(
            &carol_p,
            &carol_s,
            carol_c,
            info,
            alice.export_ratchet_tree().into(),
            &b,
        )
        .expect("external join");
        assert_eq!(rows(&carol), [(0, 0x01), (1, 0x03)]);
        assert_eq!(carol.own_leaf_index().u32(), 1);
        assert!(
            carol
                .leaf_of_device(&DeviceId::from_bytes([0x99; 16]))
                .is_empty()
        );
    }

    #[test]
    fn the_media_epoch_is_the_exporter_key_own_leaf_and_roster_of_one_epoch() {
        let (alice_p, alice_s, alice_c) = member(0xaa, 0x01);
        let alice = DillaGroup::create(
            &alice_p,
            &alice_s,
            alice_c,
            GroupId::from_slice(&[0x45; 16]),
            call_binding(),
            None,
        )
        .expect("create");
        let m = alice.media_epoch(&alice_p).expect("media epoch");
        assert_eq!(m.epoch, alice.epoch());
        assert_eq!(
            *m.base_key,
            alice.sframe_base_key(&alice_p).expect("base key")
        );
        assert_eq!(m.own_leaf, 0);
        assert_eq!(m.roster, alice.roster());
    }

    /// DEV-47: a member leaves by a Remove *proposal* of its own leaf, which another member commits;
    /// OpenMLS refuses a commit that removes its own committer.
    #[test]
    fn a_leave_is_a_remove_proposal_another_member_commits() {
        let (alice_p, alice_s, alice_c) = member(0xaa, 0x01);
        let (bob_p, bob_s, bob_c) = member(0xbb, 0x02);
        let b = call_binding();
        let mut alice = DillaGroup::create(
            &alice_p,
            &alice_s,
            alice_c,
            GroupId::from_slice(&[0x45; 16]),
            b.clone(),
            None,
        )
        .expect("create");
        let bob_kp = build_key_package(&bob_p, &bob_s, bob_c, false).expect("key package");
        let bundle = alice
            .add_members(&alice_p, &alice_s, &[bob_kp.key_package().clone()])
            .expect("add");
        alice.merge_pending_commit(&alice_p).expect("merge");
        let MlsMessageBodyIn::Welcome(welcome) = wire(bundle.welcomes[0].1.clone()) else {
            panic!("not a Welcome");
        };
        let mut bob =
            DillaGroup::join_from_welcome(&bob_p, welcome, alice.export_ratchet_tree().into(), &b)
                .expect("join");

        let proposal = bob.leave(&bob_p, &bob_s).expect("leave");
        let bytes = proposal.tls_serialize_detached().expect("serialize");
        let message = MlsMessageIn::tls_deserialize_exact(&bytes)
            .expect("deserialize")
            .try_into_protocol_message()
            .expect("a protocol message");
        let DillaProcessed::Proposal(queued) =
            alice.process_message(&alice_p, message).expect("process")
        else {
            panic!("bob's leave is not a proposal");
        };
        match queued.proposal() {
            Proposal::Remove(r) => assert_eq!(r.removed().u32(), 1),
            other => panic!("bob's leave proposes {other:?}"),
        }
        alice
            .store_pending_proposal(&alice_p, *queued)
            .expect("queue");
        let commit = alice.self_update(&alice_p, &alice_s).expect("commit");
        alice.merge_pending_commit(&alice_p).expect("merge");
        assert_eq!(rows(&alice), [(0, 0x01)]);

        // The receiver's policy measures a referenced member Remove against its proposer (bob),
        // not the committer (alice): before that change every member refused this commit with
        // E_MEMBER_REMOVE_FORBIDDEN, so a member could never leave.
        let bytes = commit.commit.tls_serialize_detached().expect("serialize");
        let message = MlsMessageIn::tls_deserialize_exact(&bytes)
            .expect("deserialize")
            .try_into_protocol_message()
            .expect("a protocol message");
        let processed = bob
            .process_message(&bob_p, message)
            .expect("bob accepts the commit that applies his own leave");
        assert!(matches!(processed, DillaProcessed::StagedCommit(_, _)));
    }

    /// One device of a group, with what it signs with.
    struct Party {
        p: DillaProvider,
        s: SignatureKeyPair,
        g: DillaGroup,
    }

    fn protocol(message: &MlsMessageOut) -> ProtocolMessage {
        let bytes = message.tls_serialize_detached().expect("serialize");
        MlsMessageIn::tls_deserialize_exact(&bytes)
            .expect("deserialize")
            .try_into_protocol_message()
            .expect("a protocol message")
    }

    fn received(party: &mut Party, message: &MlsMessageOut) -> QueuedProposal {
        match party.g.process_message(&party.p, protocol(message)) {
            Ok(DillaProcessed::Proposal(q)) => *q,
            other => panic!("not a proposal: {other:?}"),
        }
    }

    /// alice (leaf 0), bob (leaf 1) and carol (leaf 2) in a call group whose external sender is the
    /// instance, with bob's own Remove of his leaf (`leave`, already in bob's own queue) and the
    /// instance's Remove of the same leaf, both at the current epoch.
    fn two_removes_of_bob() -> (Party, Party, Party, MlsMessageOut, MlsMessageOut) {
        let (alice_p, alice_s, alice_c) = member(0xaa, 0x01);
        let (bob_p, bob_s, bob_c) = member(0xbb, 0x02);
        let (carol_p, carol_s, carol_c) = member(0xcc, 0x03);
        let instance = SignatureKeyPair::new(CIPHERSUITE.signature_algorithm()).expect("keygen");
        let b = call_binding();
        let senders = crate::mls::external_senders(instance.public().into(), &b.instance_id);
        let mut alice = DillaGroup::create(
            &alice_p,
            &alice_s,
            alice_c,
            GroupId::from_slice(&[0x46; 16]),
            b.clone(),
            Some(senders),
        )
        .expect("create");
        let bob_kp = build_key_package(&bob_p, &bob_s, bob_c, false).expect("key package");
        let carol_kp = build_key_package(&carol_p, &carol_s, carol_c, false).expect("key package");
        let bundle = alice
            .add_members(
                &alice_p,
                &alice_s,
                &[bob_kp.key_package().clone(), carol_kp.key_package().clone()],
            )
            .expect("add");
        alice.merge_pending_commit(&alice_p).expect("merge");
        let tree = alice.export_ratchet_tree();
        let mut joined = bundle.welcomes.iter().map(|(_, w)| {
            let MlsMessageBodyIn::Welcome(welcome) = wire(w.clone()) else {
                panic!("not a Welcome");
            };
            welcome
        });
        let (bob_w, carol_w) = (
            joined.next().expect("bob's"),
            joined.next().expect("carol's"),
        );
        let mut bob = DillaGroup::join_from_welcome(&bob_p, bob_w, tree.clone().into(), &b)
            .expect("bob joins");
        let carol =
            DillaGroup::join_from_welcome(&carol_p, carol_w, tree.into(), &b).expect("carol joins");
        assert_eq!(bob.own_leaf_index().u32(), 1);

        let leave = bob.leave(&bob_p, &bob_s).expect("leave");
        let kick = crate::public_group::external_propose_remove(
            LeafNodeIndex::new(1),
            alice.group_id().clone(),
            GroupEpoch::from(alice.epoch()),
            &instance,
        )
        .expect("the instance's Remove");
        (
            Party {
                p: alice_p,
                s: alice_s,
                g: alice,
            },
            Party {
                p: bob_p,
                s: bob_s,
                g: bob,
            },
            Party {
                p: carol_p,
                s: carol_s,
                g: carol,
            },
            leave,
            kick,
        )
    }

    /// The Removes a commit this party staged carries, by sender.
    fn staged_removes(party: &Party) -> Vec<Sender> {
        party
            .g
            .group
            .pending_commit()
            .expect("a staged commit")
            .queued_proposals()
            .filter(|q| matches!(q.proposal(), Proposal::Remove(_)))
            .map(|q| q.sender().clone())
            .collect()
    }

    /// The fact the delivery service's dedupe rests on (task-9 security fix, B): with two Removes of
    /// one leaf queued, OpenMLS 0.9.0 does not refuse to commit — it commits exactly one of them, the
    /// LATER in queue order, and leaves the other out. Measured on the raw `MlsGroup` queue, in both
    /// orders.
    #[test]
    fn openmls_commits_only_the_later_of_two_removes_of_one_leaf() {
        for member_first in [true, false] {
            let (mut alice, _bob, _carol, leave, kick) = two_removes_of_bob();
            let member = received(&mut alice, &leave);
            let instance = received(&mut alice, &kick);
            let order = if member_first {
                [member, instance]
            } else {
                [instance, member]
            };
            let group = &mut alice.g.group;
            let storage = alice.p.storage();
            storage
                .transaction(|| {
                    for q in order {
                        group
                            .store_pending_proposal(storage, q)
                            .map_err(MlsError::Storage)?;
                    }
                    Ok::<(), MlsError>(())
                })
                .expect("queue");
            alice.g.self_update(&alice.p, &alice.s).expect("commit");
            let removes = staged_removes(&alice);
            assert_eq!(removes.len(), 1, "one Remove of the leaf is committed");
            assert_eq!(
                matches!(removes[0], Sender::External(_)),
                member_first,
                "the later Remove is the one committed (member first: {member_first})"
            );
        }
    }

    /// protocol/01's client rule over that fact: a member's Remove never displaces the instance's
    /// Remove of the same leaf, whichever this client received first, and a receiver holding both
    /// accepts the commit and drops the leaf.
    #[test]
    fn the_instance_remove_is_committed_over_a_member_remove_in_either_arrival_order() {
        for member_first in [true, false] {
            let (mut alice, _bob, mut carol, leave, kick) = two_removes_of_bob();
            let member = received(&mut alice, &leave);
            let instance = received(&mut alice, &kick);
            let order = if member_first {
                [member, instance]
            } else {
                [instance, member]
            };
            for q in order {
                alice.g.store_pending_proposal(&alice.p, q).expect("queue");
            }
            let commit = alice.g.self_update(&alice.p, &alice.s).expect("commit");
            let removes = staged_removes(&alice);
            assert_eq!(removes.len(), 1);
            assert!(
                matches!(removes[0], Sender::External(_)),
                "the instance's Remove is committed (member first: {member_first})"
            );

            for message in [&leave, &kick] {
                let q = received(&mut carol, message);
                carol.g.store_pending_proposal(&carol.p, q).expect("queue");
            }
            let DillaProcessed::StagedCommit(staged, _) = carol
                .g
                .process_message(&carol.p, protocol(&commit.commit))
                .expect("carol accepts the commit")
            else {
                panic!("not a commit");
            };
            carol
                .g
                .merge_staged_commit(&carol.p, *staged)
                .expect("merge");
            assert_eq!(rows(&carol.g), [(0, 0x01), (2, 0x03)]);
        }
    }

    /// A leaver whose own Remove the delivery service refused ("a removal of this leaf is already
    /// pending") takes that proposal back and keeps the instance's.
    #[test]
    fn withdraw_leave_takes_back_only_the_devices_own_remove() {
        let (_alice, mut bob, _carol, _leave, kick) = two_removes_of_bob();
        let instance = received(&mut bob, &kick);
        bob.g
            .store_pending_proposal(&bob.p, instance)
            .expect("queue");
        assert_eq!(bob.g.group.pending_proposals().count(), 2);
        bob.g.withdraw_leave(&bob.p).expect("withdraw");
        let left: Vec<_> = bob.g.group.pending_proposals().collect();
        assert_eq!(left.len(), 1);
        assert!(matches!(left[0].sender(), Sender::External(_)));
    }
}
