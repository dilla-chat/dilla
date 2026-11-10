//! Past-epoch secret retention and the proposal-policy tables of protocol/01-groups.md.

use super::GroupKind;
use crate::error::ProtocolError;
use crate::ids::UserId;
use core::time::Duration;
// `PublicGroup`, `Sender`, `StagedCommit`, `LeafNodeIndex` and `BasicCredential` all come from the
// prelude; none of them touches storage, so this module compiles on every target, wasip1 included.
use openmls::prelude::*;

/// Text groups keep this many past epochs. **A plan decision, not a verified number**: the
/// protocol's 300-second window cannot be turned into an epoch count without the DS's worst-case
/// reordering window, which nothing measures in week 1.
pub const PAST_EPOCHS_TEXT: usize = 16;
/// Call groups keep exactly one.
pub const PAST_EPOCHS_CALL: usize = 1;
/// protocol/01: past epoch secrets are kept 300 s in text groups.
pub const TEXT_SWEEP: Duration = Duration::from_secs(300);
/// protocol/01: 10 s in call groups.
pub const CALL_SWEEP: Duration = Duration::from_secs(10);

/// The **count-based** policy is the real guarantee. `older_than_duration` compares the epoch's
/// *start* timestamp and short-circuits to "clear all" when the current epoch is itself older than
/// the window (gap-8 section 3.1), so the sweep is an upper bound, not a floor.
pub fn past_epoch_policy(kind: GroupKind) -> PastEpochDeletionPolicy {
    match kind {
        GroupKind::Text => PastEpochDeletionPolicy::MaxEpochs(PAST_EPOCHS_TEXT),
        GroupKind::Call => PastEpochDeletionPolicy::MaxEpochs(PAST_EPOCHS_CALL),
        // Structural: nothing is ever written, so nothing can be recovered.
        GroupKind::Pairing => PastEpochDeletionPolicy::MaxEpochs(0),
    }
}

/// The time-based sweep run after a merge. Never write
/// `PastEpochDeletion::delete_all().max_past_epochs(k)`: it silently ignores `k` (gap-8 3.2).
pub fn past_epoch_sweep(kind: GroupKind) -> Option<PastEpochDeletion> {
    match kind {
        GroupKind::Text => Some(PastEpochDeletion::older_than_duration(TEXT_SWEEP)),
        GroupKind::Call => Some(PastEpochDeletion::older_than_duration(CALL_SWEEP)),
        GroupKind::Pairing => None,
    }
}

/// The client-side proposal policy of protocol/01-groups.md, applied to a `StagedCommit` **before**
/// it is merged. OpenMLS offers no credential-validation callback on Add: the application inspects
/// the staged commit and aborts without merging (facts-openmls "Is there a credential-validation
/// hook on Add?").
///
/// - `Update` from a member, and the committer's UpdatePath: accept only if the leaf node keeps the
///   credential and the signature key the leaf holds before the commit (`E_CREDENTIAL`;
///   `leaf_credentials_unchanged`). An external commit by a device that already holds a leaf must
///   remove that leaf and keep its credential (one device, one leaf; same code).
/// - `Remove`: accept only when the target leaf belongs to the proposer's own user — the
///   committer's for a Remove carried by value, the proposing member's for a referenced member
///   proposal (a member leaving, DEV-47) — (`E_MEMBER_REMOVE_FORBIDDEN`). Removing other users is
///   the instance's job.
/// - `Add`: accept only in pairing groups; reject in text and call groups.
/// - An external commit's `Remove` must target only the joiner's own leaf
///   (`E_EXTERNAL_COMMIT_REMOVE`).
/// - `PreSharedKey`: reject.
/// - `GroupContextExtensions`: reject, from every sender including the instance's external sender.
///   The proposal replaces the whole extension set, so whoever lands one rewrites `dilla_binding`
///   and `required_capabilities`; the one case protocol/01 allows (an instance key rotation)
///   cannot be verified until protocol/03 says where its signature travels
///   (`extension_change_verdict`).
/// - `ReInit`: reject, from every sender. Dilla never re-initialises a group, and OpenMLS 0.9.0
///   does not implement the proposal either (messages/external_proposals.rs:5).
///
/// The sender each rule is measured against is the **proposal's** sender
/// (`QueuedProposal::sender()`, proposal_store.rs:187), not the committer's: a member may commit a
/// proposal the instance made, and that is the rotation path `rotate_external_senders_extensions`
/// exists for.
///
/// **This function is not optional and is not advisory.** `DillaGroup::process_message` calls it on
/// the `StagedCommitMessage` arm before handing the commit back, so a caller cannot merge a commit
/// the protocol forbids. It is the only enforcement point: OpenMLS validates the commit
/// cryptographically and structurally, never against dilla's role rules.
///
/// `tree` is the group's **pre-merge** membership view (`MlsGroup::public_group()`), and it is the
/// only place a removed leaf's credential can be read: `StagedCommit::credentials_to_verify()` is
/// the set of credentials the commit *introduces* — empty for a Remove-only commit.
/// `sender` is how an external commit is recognised (`Sender::NewMemberCommit`, RFC 9420
/// §12.4.3.2); an update path is not that signal, because every path-bearing member commit has one.
pub fn validate_staged_commit(
    kind: GroupKind,
    own_user: &UserId,
    committer_user: &UserId,
    sender: &Sender,
    tree: &PublicGroup,
    staged: &StagedCommit,
) -> Result<(), ProtocolError> {
    let external = matches!(sender, Sender::NewMemberCommit);

    // `own_user` is the receiving device's own user. No rule below is receiver-relative — every
    // check compares a proposal's sender with its target — but the contract carries it because only the
    // caller knows it and the role-snapshot rule of protocol/01 will need it. Asserted rather than
    // discarded with a `let _ =`.
    debug_assert_ne!(
        own_user.as_bytes(),
        &[0u8; 16],
        "own_user must be the receiver's user id"
    );

    // A member's leaf keeps the credential it joined with: the UpdatePath and every Update the
    // commit applies are measured against the pre-merge tree.
    leaf_credentials_unchanged(tree, sender, staged)?;

    // Each Add and Remove is judged by ITS OWN sender (protocol/01 has one table for proposals
    // from the external sender and one for proposals from members). The instance's own Add and
    // Remove are accepted in `text` and `call` groups - they are how an offline device joins and
    // how a user is removed - and a member commit that carries them by reference is the only way
    // they ever take effect, so judging the commit by its proposal types refused every membership
    // change the instance makes.
    let member_add = staged
        .add_proposals()
        .any(|add| !matches!(add.sender(), Sender::External(_)));
    if member_add && matches!(kind, GroupKind::Text | GroupKind::Call) {
        // NEEDS VERIFICATION item 24: protocol/01-groups.md states this rule ("`Add`: accept only
        // in `pairing` … and `interaction` groups; reject in `text` and `call`") but assigns it no
        // code — its published list is the six `E_*` strings at line 122, and
        // `E_MEMBER_REMOVE_FORBIDDEN` is not one of this rule's names. The **rejection** is what
        // week 1 depends on; the string is not yet part of the compatibility surface for this
        // rule, and settling it is a protocol/07-versioning.md change, not a code change.
        return Err(ProtocolError::MemberRemoveForbidden);
    }
    // `.next().is_some()`, not `!….next().is_none()`: `clippy::nonminimal_bool` is warn-by-default
    // and every task here runs clippy with `-D warnings`. Same NEEDS VERIFICATION item 24 applies
    // to the code this returns.
    if staged.psk_proposals().next().is_some() {
        return Err(ProtocolError::MemberRemoveForbidden);
    }
    // `StagedCommit` has no `group_context_ext_proposals()`/`reinit_proposals()` accessor in
    // 0.9.0 - staged_commit.rs:888-926 lists add/remove/update/psk and the untyped
    // `queued_proposals()` - so the two remaining rules are read off the proposal queue directly.
    commit_extensions_unchanged(staged)?;
    for queued in staged.queued_proposals() {
        if matches!(queued.proposal(), Proposal::ReInit(_)) {
            return Err(ProtocolError::MemberRemoveForbidden);
        }
    }
    for remove in staged.remove_proposals() {
        // An instance Remove is accepted outright in `text` and `call` groups (protocol/01, the
        // external-sender table). A `pairing` group carries no external sender at all
        // (`group_context_extensions` refuses one), so an `External` sender cannot occur there.
        if matches!(remove.sender(), Sender::External(_)) {
            continue;
        }
        let target = remove.remove_proposal().removed();
        let target_user = user_of_leaf(tree, target)?;
        // Measured against the proposal's own sender, like every rule here: a Remove the commit
        // carries by value is the committer's, and a member Remove proposal the commit references
        // is its proposer's - which is how a member leaves (protocol/01 "Leaving", DEV-47): it
        // proposes its own Remove and another member commits it.
        let owner = match remove.sender() {
            Sender::Member(leaf) => user_of_leaf(tree, *leaf)?,
            _ => *committer_user,
        };
        removal_verdict(external, &owner, &target_user)?;
    }
    Ok(())
}

/// The removal rule on its own, over plain user ids.
///
/// Split out so it is testable: a *malicious* external commit — one that removes a leaf belonging
/// to someone other than the joiner — cannot be produced by OpenMLS's own commit builders, so
/// there is no way to drive that branch end to end from this plan's tests. The member branch is
/// covered end to end by `mls_roundtrip.rs`.
pub(crate) fn removal_verdict(
    external: bool,
    committer_user: &UserId,
    target_user: &UserId,
) -> Result<(), ProtocolError> {
    if target_user == committer_user {
        return Ok(());
    }
    if external {
        // RFC 9420 §12.4.3.2 and protocol/01: an external commit may remove only the joiner's own
        // leaf, and the joiner is the committer.
        Err(ProtocolError::ExternalCommitRemove)
    } else {
        Err(ProtocolError::MemberRemoveForbidden)
    }
}

/// Who may change the group context extensions. **Nobody, on this branch.**
///
/// A `GroupContextExtensions` proposal replaces the **whole** extension set (gap-4 section 4.1),
/// so whoever lands one can drop or rewrite `dilla_binding` and `required_capabilities`, while
/// `DillaGroup` keeps serving the binding it cached at load - the divergence would only surface on
/// a later `DillaGroup::load`. protocol/01-groups.md's external-sender table allows exactly one
/// such proposal: from the instance, "only if the sole change is to `external_senders` and the new
/// instance key is signed by the old one (`03-identity.md`, 'Instance key rotation')". Until this
/// commit the check was only the sender: **any** `Sender::External(_)` proposal was accepted, so
/// the one sender dilla trusts for Adds and inactivity Removes could also silently rewrite
/// `dilla_binding`, which rule 3 of protocol/01 "dilla_binding" calls immutable for the life of
/// the group.
///
/// The conditional accept cannot be implemented here yet. protocol/03-identity.md defines the
/// rotation *preimage* - `sig_old(new_pub || "dilla instance rotate v1")`, the domain constant
/// `identity::DOMAIN_INSTANCE_ROTATE` - but no document in `protocol/` says **where that signature
/// travels**: not as a companion GroupContext extension, not in the proposal's
/// `authenticated_data`, nowhere. A verifier cannot check a signature it cannot locate, and
/// inventing a carrier here would freeze a wire format by accident. Per the final-fix brief's
/// item 5 STOP condition this is recorded as blocked and the safe default is taken instead:
/// **every** `GroupContextExtensions` proposal is refused, from every sender. The consequence is
/// that instance key rotation is not possible in a live group on this branch; a rotation needs a
/// new group until protocol/03 says where the signature rides.
///
/// What the eventual accept must check, once that is settled: (i) every extension other than
/// `external_senders` is byte-identical to the current group context's (`required_capabilities`,
/// `dilla_binding` 0xF001, and anything else present), (ii) `external_senders` still has exactly
/// one entry, and (iii) the new instance key is signed by the old one over the protocol/03
/// preimage.
///
/// Split out from `validate_staged_commit` so it is testable: OpenMLS keeps
/// `GroupContextExtensionProposal::new` and `ReInitProposal`'s fields `pub(crate)`
/// (openmls-0.9.0/src/messages/proposals.rs:700 and :562-567), so dilla cannot construct either
/// proposal, and the member branch is driven end to end from `tests/mls_roundtrip.rs` instead.
///
/// Same NEEDS VERIFICATION item 24 as the `Add` and PSK rules: protocol/01-groups.md states the
/// rule but assigns it no `E_*` code, so `MemberRemoveForbidden` is the placeholder the other two
/// already use and settling it is a protocol/07-versioning.md change.
pub(crate) fn extension_change_verdict(_proposal_sender: &Sender) -> Result<(), ProtocolError> {
    Err(ProtocolError::MemberRemoveForbidden)
}

/// The group context's extensions never change after creation (protocol/01 "Client policy for
/// proposals from members", protocol/02 invariant 4): a commit that carries a
/// `GroupContextExtensions` proposal, by value or by reference, is refused, whoever sent the
/// proposal (`extension_change_verdict`). Shared by both enforcement points so they cannot drift:
/// `validate_staged_commit` (a receiving member) and `DillaPublicGroup::process_with_policy` (the
/// delivery service, on `/commit`, `/resync` and every commit of a heal's replayed tail). Without the
/// delivery-service half a member could swap `external_senders` to its own key in an honestly
/// registered group and leave the instance unable to propose into it (DS-MEMBERSHIP-01).
///
/// `StagedCommit::queued_proposals()` is every proposal the commit applies, the inline ones and the
/// referenced ones alike (openmls-0.9.0 `staged_commit.rs`), so one walk covers both.
pub(crate) fn commit_extensions_unchanged(staged: &StagedCommit) -> Result<(), ProtocolError> {
    for queued in staged.queued_proposals() {
        if matches!(queued.proposal(), Proposal::GroupContextExtensions(_)) {
            extension_change_verdict(queued.sender())?;
        }
    }
    Ok(())
}

/// The same rule for a standalone proposal, before anyone queues it: a `GroupContextExtensions`
/// proposal is refused, so it can never sit in a queue waiting for some committer to carry it.
pub(crate) fn proposal_extensions_unchanged(
    proposal: &QueuedProposal,
) -> Result<(), ProtocolError> {
    if matches!(proposal.proposal(), Proposal::GroupContextExtensions(_)) {
        extension_change_verdict(proposal.sender())?;
    }
    Ok(())
}

/// Once a leaf is in a group its credential is immutable (protocol/01 "Client policy for proposals
/// from members"). The credential is what binds a leaf to a user id and a device id
/// (protocol/03 "Credential"), and a receiver stores a message's sender from it, so a member that
/// could swap it would write under another user's name from then on. MLS allows the swap - an
/// `Update` proposal's leaf node and a commit's UpdatePath leaf node may carry any credential the
/// group's capabilities admit (RFC 9420 §12.1.2, §12.4.2) - and OpenMLS 0.9.0 checks only that its
/// credential *type* is supported (`group/public_group/validation.rs:795-835`), never that it is
/// the leaf's old one: it hands the new credentials to the application in
/// `StagedCommit::credentials_to_verify()` and leaves the decision there.
///
/// So the comparison is made here, byte for byte (`Credential`'s `PartialEq` covers its type and
/// its serialized content, which is the TLS encoding), against the credential the leaf holds in
/// `tree`, the **pre-merge** view:
///
/// - a member commit's UpdatePath leaf node, against the committer's leaf;
/// - every `Update` the commit applies, against its proposer's leaf.
///
/// - a **resync** - an external commit whose inline `Remove` takes out a leaf of the joiner's own
///   device (the device the path leaf's credential names) - against the leaf it replaces: the
///   device keeps the credential it joined with across the resync (review residual R1).
///
/// The leaf's **signature key** is immutable in the same way (finding G1 of the second hardening
/// review): the UpdatePath leaf and every `Update` must carry the key the leaf holds. MLS lets a
/// leaf rotate it (`self_update_with_new_signer`, `propose_self_update_with_new_signer`), but a
/// dilla leaf is keyed by its device's registered key (`dsk_pub`, protocol/03), which never
/// changes, and no honest client rotates it (`DillaGroup::self_update` builds its path with
/// `LeafNodeParameters::default()`). A leaf moved to another key would also escape the resync rule
/// below: OpenMLS's external-commit builder removes the leaf that carries the joiner's key, and
/// would find none. Both violations are `E_CREDENTIAL`: the credential and the key together are
/// the leaf's identity, and protocol/01 names no other code for either.
///
/// **One device, one leaf.** An external commit whose new leaf names a device that already holds a
/// leaf must remove that leaf (`resync_keeps_credential`). OpenMLS accepts at most one `Remove` in
/// an external commit, so a device found at two leaves of the pre-commit tree is refused outright.
/// The device is read from the credentials in the pre-commit tree, never from the commit's own
/// claims about what it removes. An external commit by a device that holds no leaf is a first
/// join: its UpdatePath leaf is a new leaf, and binding it to its device is the Add and
/// external-join rules' job (the delivery service binds its key to the device's registered key).
///
/// Shared by both enforcement points so they cannot drift: `validate_staged_commit` (a receiving
/// member) and `DillaPublicGroup::process_message` (the delivery service, which runs no other
/// dilla-level commit policy).
pub(crate) fn leaf_credentials_unchanged(
    tree: &PublicGroup,
    sender: &Sender,
    staged: &StagedCommit,
) -> Result<(), ProtocolError> {
    match (sender, staged.update_path_leaf_node()) {
        (Sender::Member(committer), Some(path_leaf)) => {
            leaf_identity_unchanged(tree, *committer, path_leaf)?;
        }
        (Sender::NewMemberCommit, Some(path_leaf)) => {
            resync_keeps_credential(tree, staged, path_leaf.credential())?;
        }
        _ => {}
    }
    for update in staged.update_proposals() {
        update_credential_verdict(tree, update.sender(), update.update_proposal())?;
    }
    Ok(())
}

/// The same rule for a standalone proposal, before anyone queues it: an `Update` whose leaf node
/// carries another credential or another signature key than its proposer's leaf is refused, every
/// other proposal is left to the rules that govern it.
pub(crate) fn proposal_credential_verdict(
    tree: &PublicGroup,
    proposal: &QueuedProposal,
) -> Result<(), ProtocolError> {
    match proposal.proposal() {
        Proposal::Update(update) => update_credential_verdict(tree, proposal.sender(), update),
        _ => Ok(()),
    }
}

/// An `Update` is always a member's own leaf (RFC 9420 §12.1.2: the leaf of its sender), so the
/// leaf it changes is the sender's; any other sender is refused rather than assumed away.
fn update_credential_verdict(
    tree: &PublicGroup,
    sender: &Sender,
    update: &UpdateProposal,
) -> Result<(), ProtocolError> {
    match sender {
        Sender::Member(leaf) => leaf_identity_unchanged(tree, *leaf, update.leaf_node()),
        _ => Err(ProtocolError::Credential),
    }
}

/// The resync half of the rule. An external commit carries its proposals inline (RFC 9420
/// §12.4.3.2), and the leaf it replaces is the one its `Remove` names; OpenMLS's builder emits that
/// `Remove` for the leaf holding the joiner's signature key, but a hand-built commit may name any
/// leaf, so the leaf is recognised by what it IS - a leaf whose credential names the same device as
/// the new leaf - not by how the commit was built. Every such leaf must carry exactly the new
/// leaf's credential. A credential on either side that does not decode as a dilla identity is
/// refused rather than passed over.
///
/// And every leaf of the joiner's device must go (one device, one leaf; finding G1): the leaves are
/// found by the device their credentials name in the pre-commit `tree`. A leaf whose credential
/// does not decode names no device and is not one of them. Two such leaves cannot both be removed
/// by one external commit, so they refuse it; one must be among the commit's Removes.
fn resync_keeps_credential(
    tree: &PublicGroup,
    staged: &StagedCommit,
    new: &Credential,
) -> Result<(), ProtocolError> {
    let joiner = device_of_credential(new)?;
    let removed: Vec<LeafNodeIndex> = staged
        .remove_proposals()
        .map(|remove| remove.remove_proposal().removed())
        .collect();
    let mut own_leaves = tree.members().filter(|member| {
        device_of_credential(&member.credential).is_ok_and(|device| device == joiner)
    });
    match (own_leaves.next(), own_leaves.next()) {
        (Some(_), Some(_)) => return Err(ProtocolError::Credential),
        (Some(only), None) if !removed.contains(&only.index) => {
            return Err(ProtocolError::Credential);
        }
        _ => {}
    }
    for target in removed {
        let old = tree.leaf(target).ok_or(ProtocolError::Credential)?;
        if device_of_credential(old.credential())? == joiner && old.credential() != new {
            return Err(ProtocolError::Credential);
        }
    }
    Ok(())
}

fn device_of_credential(credential: &Credential) -> Result<crate::ids::DeviceId, ProtocolError> {
    let basic =
        BasicCredential::try_from(credential.clone()).map_err(|_| ProtocolError::Credential)?;
    Ok(crate::identity::CredentialIdentity::decode(basic.identity())?.device_id)
}

/// A member's new leaf node (an UpdatePath leaf or an `Update`'s) against the leaf it replaces in
/// the pre-commit `tree`: the same credential, byte for byte, and the same signature key.
fn leaf_identity_unchanged(
    tree: &PublicGroup,
    leaf: LeafNodeIndex,
    new: &LeafNode,
) -> Result<(), ProtocolError> {
    let old = tree.leaf(leaf).ok_or(ProtocolError::Credential)?;
    if old.credential() == new.credential() && old.signature_key() == new.signature_key() {
        Ok(())
    } else {
        Err(ProtocolError::Credential)
    }
}

/// The user a leaf belongs to, read from the group's own pre-merge tree.
///
/// `PublicGroup::leaf(LeafNodeIndex) -> Option<&LeafNode>` is verified (facts-openmls §6). A
/// `Remove` names an index in the tree as it stands *before* the merge, which is exactly this view.
fn user_of_leaf(tree: &PublicGroup, leaf: LeafNodeIndex) -> Result<UserId, ProtocolError> {
    let node = tree.leaf(leaf).ok_or(ProtocolError::Credential)?;
    let basic = BasicCredential::try_from(node.credential().clone())
        .map_err(|_| ProtocolError::Credential)?;
    let identity = crate::identity::CredentialIdentity::decode(basic.identity())?;
    Ok(identity.user_id)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn past_epoch_policy_is_count_based_per_group_kind() {
        assert_eq!(
            past_epoch_policy(GroupKind::Text),
            PastEpochDeletionPolicy::MaxEpochs(PAST_EPOCHS_TEXT)
        );
        assert_eq!(
            past_epoch_policy(GroupKind::Call),
            PastEpochDeletionPolicy::MaxEpochs(PAST_EPOCHS_CALL)
        );
        // Structural: nothing is ever written for a pairing group, so nothing can leak.
        assert_eq!(
            past_epoch_policy(GroupKind::Pairing),
            PastEpochDeletionPolicy::MaxEpochs(0)
        );
    }

    #[test]
    fn the_sweep_exists_only_where_past_epochs_do() {
        assert!(past_epoch_sweep(GroupKind::Text).is_some());
        assert!(past_epoch_sweep(GroupKind::Call).is_some());
        assert!(past_epoch_sweep(GroupKind::Pairing).is_none());
    }

    #[test]
    fn the_windows_are_the_documented_300_and_10_seconds() {
        assert_eq!(TEXT_SWEEP, core::time::Duration::from_secs(300));
        assert_eq!(CALL_SWEEP, core::time::Duration::from_secs(10));
        assert_eq!(PAST_EPOCHS_TEXT, 16);
        assert_eq!(PAST_EPOCHS_CALL, 1);
    }

    #[test]
    fn a_removal_is_allowed_only_within_the_committers_own_user() {
        use crate::ids::UserId;
        let alice = UserId::from_bytes([0xaa; 16]);
        let bob = UserId::from_bytes([0xbb; 16]);

        // own-user device revocation, both as a member and inside an external commit
        assert_eq!(removal_verdict(false, &alice, &alice), Ok(()));
        assert_eq!(removal_verdict(true, &alice, &alice), Ok(()));

        // someone else's leaf: two different codes, and neither is silently accepted
        assert_eq!(
            removal_verdict(false, &alice, &bob),
            Err(ProtocolError::MemberRemoveForbidden)
        );
        assert_eq!(
            removal_verdict(true, &alice, &bob),
            Err(ProtocolError::ExternalCommitRemove)
        );
    }

    /// Fix round 1, finding 3: the doc comment promised this rule; the body did not enforce it.
    /// Final-fix item 5: the body enforced only *who* sent it, so the instance's external sender
    /// could rewrite `dilla_binding` - immutable for the life of the group by protocol/01 rule 3 -
    /// under the name of an `external_senders` rotation. The conditional accept protocol/01
    /// describes cannot be verified while protocol/03 does not say where the rotation signature
    /// travels, so every sender is refused until it does.
    #[test]
    fn no_sender_may_change_the_group_context_extensions() {
        // The instance is the one external sender, at index 0 (`instance_sender_index`). It is
        // refused here too: nothing in this build can check the rotation signature protocol/01
        // conditions the accept on.
        assert_eq!(
            extension_change_verdict(&Sender::External(SenderExtensionIndex::new(0))),
            Err(ProtocolError::MemberRemoveForbidden)
        );
        // And every other sender - a member, a joiner's external commit, an external add proposal.
        assert_eq!(
            extension_change_verdict(&Sender::Member(LeafNodeIndex::new(0))),
            Err(ProtocolError::MemberRemoveForbidden)
        );
        assert_eq!(
            extension_change_verdict(&Sender::NewMemberCommit),
            Err(ProtocolError::MemberRemoveForbidden)
        );
        assert_eq!(
            extension_change_verdict(&Sender::NewMemberProposal),
            Err(ProtocolError::MemberRemoveForbidden)
        );
    }
}
