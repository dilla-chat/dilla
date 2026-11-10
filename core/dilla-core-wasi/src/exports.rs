//! The `(ptr, len) -> u64` handlers of interfaces §2.10 — the fifteen shipped at ABI v1, ABI
//! v2's `public_group_staged_discard`, and ABI v3's `device_list_entries` and
//! `public_group_staged_group_info_validate` — written as pure functions over
//! `&[u8]` so the native test build drives exactly the same encoders the wasm build does.

// `Decoder` is deliberately absent: nothing outside the `#[cfg(test)]` module (which has its own
// `use`) names the type, and `cargo clippy --all-targets -- -D warnings` at step 16 fails on an
// unused import.
use dilla_core::cbor::Encoder;
use dilla_core::identity::CredentialIdentity;
use dilla_core::public_group::{
    DillaPublicGroup, PublicProcessed, external_propose_add, external_propose_remove,
    validate_key_package,
};
// `BasicCredential` lives in `openmls::credentials` (facts-openmls §7), not in
// `openmls_basic_credential`, which exports only `SignatureKeyPair`. `ProposalRef` is
// `openmls::ciphersuite::hash_ref::ProposalRef` and is NOT re-exported by `openmls::prelude` in
// 0.9.0 (`messages::proposals` imports it privately) — the same finding `public_group::state`
// records.
use openmls::ciphersuite::hash_ref::ProposalRef;
// Named explicitly so the two types this file's ABI v2 code leans on are visible at the top of
// the file rather than arriving through the glob.
use openmls::group::StagedCommit;
use openmls::messages::proposals::Proposal;
use openmls::prelude::*;
use openmls_basic_credential::SignatureKeyPair;
use openmls_rust_crypto::RustCrypto;

use crate::abi::{self, AbiError, tls};
use crate::handles::{Table, with_table};

/// The only ciphersuite dilla speaks at e2ee_version 1 (`protocol/00-overview.md`).
const CIPHERSUITE_ID: u64 = 0x0001;

/// Routes one request by export name. Never panics and never returns an OpenMLS type.
pub fn dispatch(export: &str, req: &[u8]) -> Vec<u8> {
    let result = with_table(|t| match export {
        "dilla_abi" => abi_info(req, t),
        "vectors_check" => vectors_check(req, t),
        "public_group_create" => public_group_create(req, t),
        "public_group_import_state" => public_group_import_state(req, t),
        "public_group_export_state" => public_group_export_state(req, t),
        "public_group_close" => public_group_close(req, t),
        "public_group_process" => public_group_process(req, t),
        "public_group_merge" => public_group_merge(req, t),
        "public_group_staged_discard" => public_group_staged_discard(req, t),
        "public_group_tree" => public_group_tree(req, t),
        "public_group_state" => public_group_state(req, t),
        "public_group_proposal_put" => public_group_proposal_put(req, t),
        "public_group_proposal_list" => public_group_proposal_list(req, t),
        "public_group_group_info_validate" => public_group_group_info_validate(req, t),
        "public_group_staged_group_info_validate" => {
            public_group_staged_group_info_validate(req, t)
        }
        "public_group_proposal_inspect" => public_group_proposal_inspect(req, t),
        "private_message_aad" => private_message_aad(req, t),
        "validate_key_package" => validate_key_package_export(req, t),
        "external_propose_add" => external_propose_add_export(req, t),
        "external_propose_remove" => external_propose_remove_export(req, t),
        "device_list_entries" => device_list_entries_export(req, t),
        "verify_credential" => verify_credential_export(req),
        "public_group_roster_extract" => public_group_roster_extract(req),
        "welcome_key_package_refs" => welcome_key_package_refs_export(req),
        other => Err(AbiError::shape(format!("unknown export {other}"))),
    });
    match result {
        Ok(bytes) => bytes,
        Err(err) => abi::error_response(&err),
    }
}

fn abi_info(req: &[u8], _t: &mut Table) -> Result<Vec<u8>, AbiError> {
    abi::open(req, 1)?.finish()?;
    let mut e = Encoder::new();
    e.array(6)
        .uint(0)
        .uint(dilla_core::ABI_VERSION)
        .text(dilla_core::CORE_VERSION)
        .uint(dilla_core::E2EE_VERSION)
        .uint(dilla_core::MEDIA_VERSION)
        .array(1)
        .uint(CIPHERSUITE_ID);
    Ok(e.into_vec())
}

/// Verifies a strict dilla CredentialIdentity against the DSK supplied by its MLS leaf.
///
/// The credential is borrowed from the ABI request and checked against the size limit before any
/// decode or output allocation. Signature verification stays in dilla-core so the server uses the
/// same strict Ed25519 checks as MLS clients.
fn verify_credential_export(req: &[u8]) -> Result<Vec<u8>, AbiError> {
    const MAX_CREDENTIAL_BYTES: usize = 8192;

    let mut d = abi::open(req, 3)?;
    let credential_bytes = d.bytes()?;
    if credential_bytes.len() > MAX_CREDENTIAL_BYTES {
        return Err(AbiError::new(
            "E_CREDENTIAL",
            "credential verification failed",
        ));
    }
    let dsk_pub = d.bytes_exact::<32>()?;
    d.finish()?;

    let credential = CredentialIdentity::decode(credential_bytes)
        .map_err(|_| AbiError::new("E_CREDENTIAL", "credential verification failed"))?;
    if credential.is_provisional() || credential.verify_signatures(&dsk_pub).is_err() {
        return Err(AbiError::new(
            "E_CREDENTIAL",
            "credential verification failed",
        ));
    }

    let mut e = Encoder::new();
    e.array(2)
        .uint(0)
        .array(8)
        .bytes(&credential.umk_pub)
        .bytes(credential.user_id.as_bytes())
        .bytes(credential.device_id.as_bytes())
        .uint(u64::from(credential.kind.as_u8()))
        .uint(u64::from(credential.tier.as_u8()))
        .uint(u64::from(credential.signer_tier.as_u8()))
        .bytes(&credential.ssk_pub)
        .bytes(&credential.sig_umk_ssk);
    Ok(e.into_vec())
}

/// Extracts a bounded roster from a ratchet tree and GroupInfo after OpenMLS authenticates
/// their relationship. Every leaf credential is then strictly decoded and both identity-chain
/// signatures are checked against the DSK in that same leaf. Nothing is returned partially.
fn public_group_roster_extract(req: &[u8]) -> Result<Vec<u8>, AbiError> {
    const MAX_TREE_BYTES: usize = 4 * 1024 * 1024;
    const MAX_GROUP_INFO_BYTES: usize = 64 * 1024;
    const MAX_LEAVES: usize = 1500;
    let mut d = abi::open(req, 3)?;
    let tree_bytes = d.bytes()?;
    let group_info_bytes = d.bytes()?;
    d.finish()?;
    if tree_bytes.is_empty()
        || tree_bytes.len() > MAX_TREE_BYTES
        || group_info_bytes.is_empty()
        || group_info_bytes.len() > MAX_GROUP_INFO_BYTES
    {
        return Err(AbiError::new(
            "E_ROSTER_LIMIT",
            "roster input exceeds its bound",
        ));
    }
    let crypto = RustCrypto::default();
    let (group, _) = DillaPublicGroup::from_external(
        &crypto,
        tls::ratchet_tree_in(tree_bytes)?,
        tls::verifiable_group_info(group_info_bytes)?,
    )?;
    let members = group.members();
    if group.leaf_count() > MAX_LEAVES || members.len() != group.leaf_count() {
        return Err(AbiError::new(
            "E_ROSTER_LIMIT",
            "roster has too many leaves or contains an unsupported credential",
        ));
    }
    let mut e = Encoder::new();
    e.array(2).uint(0).array(members.len());
    for member in members {
        member
            .identity
            .verify_signatures(&member.signature_key)
            .map_err(|_| AbiError::new("E_CREDENTIAL", "roster credential verification failed"))?;
        e.array(5)
            .bytes(member.identity.user_id.as_bytes())
            .bytes(member.identity.device_id.as_bytes())
            .bytes(&member.signature_key)
            .uint(u64::from(member.identity.tier.as_u8()))
            .uint(u64::from(member.identity.kind.as_u8()));
    }
    Ok(e.into_vec())
}

/// Deviation A2-9: the report's elements are emitted after the `0`, not the whole
/// `VectorReport::encode` array nested inside it.
fn vectors_check(req: &[u8], _t: &mut Table) -> Result<Vec<u8>, AbiError> {
    abi::open(req, 1)?.finish()?;
    let report = dilla_core::vectors::run_all();
    let mut e = Encoder::new();
    e.array(4)
        .uint(0)
        .uint(u64::from(report.passed))
        .uint(u64::from(report.failed));
    e.array(report.suites.len());
    for suite in &report.suites {
        e.array(2).text(suite.name).array(suite.cases.len());
        for case in &suite.cases {
            e.array(5)
                .text(&case.case)
                .text(case.field)
                .uint(u64::from(case.ok))
                .text(&case.expected)
                .text(&case.actual);
        }
    }
    Ok(e.into_vec())
}

fn public_group_create(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 3)?;
    let tree = d.bytes()?;
    let group_info = d.bytes()?;
    d.finish()?;

    let crypto = RustCrypto::default();
    let (group, gi) = DillaPublicGroup::from_external(
        &crypto,
        tls::ratchet_tree_in(tree)?,
        tls::verifiable_group_info(group_info)?,
    )?;
    let gi_bytes = tls::group_info_out(&gi)?;
    let epoch = group.epoch();
    let group_id = group.group_id().as_slice().to_vec();
    let tree_hash = group.tree_hash();
    let handle = t.insert_group(group);

    let mut e = Encoder::new();
    e.array(6)
        .uint(0)
        .uint(u64::from(handle))
        .uint(epoch)
        .bytes(&group_id)
        .bytes(&tree_hash)
        .bytes(&gi_bytes);
    Ok(e.into_vec())
}

fn public_group_import_state(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 3)?;
    let state = d.bytes()?;
    let group_id = d.bytes()?;
    d.finish()?;

    let group = DillaPublicGroup::import_state(state, &GroupId::from_slice(group_id))?;
    let epoch = group.epoch();
    let handle = t.insert_group(group);

    let mut e = Encoder::new();
    e.array(3).uint(0).uint(u64::from(handle)).uint(epoch);
    Ok(e.into_vec())
}

fn public_group_export_state(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 2)?;
    let handle = abi::read_handle(&mut d)?;
    d.finish()?;

    let state = t.group(handle)?.export_state();
    let mut e = Encoder::new();
    e.array(2).uint(0).bytes(&state);
    Ok(e.into_vec())
}

fn public_group_close(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 2)?;
    let handle = abi::read_handle(&mut d)?;
    d.finish()?;

    t.close_group(handle)?;
    let mut e = Encoder::new();
    e.array(1).uint(0);
    Ok(e.into_vec())
}

fn public_group_process(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 3)?;
    let handle = abi::read_handle(&mut d)?;
    let message = d.bytes()?;
    d.finish()?;

    let crypto = RustCrypto::default();
    let pm = tls::protocol_message(message)?;
    // process_message takes &self and writes nothing (gap-1 §6), so the borrow ends here and the
    // staged commit can then be moved into the table.
    let (processed, epoch) = {
        let group = t.group(handle)?;
        (group.process_message(&crypto, pm)?, group.epoch())
    };

    // ABI v3: the leaf an external commit's joiner lands on. A commit whose sender is not a member
    // is a `NewMemberCommit` (RFC 9420 §12.4.3.2: an external sender may propose but never
    // commit), which names no leaf of its own, so without this element the delivery service could
    // not say which leaf signed the GroupInfo that came with it (task 25 C1, deviation B30(a)).
    // `PublicGroup::ext_commit_sender_index` is openmls's own answer: the leftmost free leaf once
    // the commit's Removes have been applied, which is where the merge puts the joiner.
    let mut new_leaf: Option<u32> = None;
    let (kind, sender_leaf, staged, proposal_ref) = match processed {
        PublicProcessed::Proposal {
            proposal_ref,
            sender_leaf,
        } => (0u64, sender_leaf, None, Some(proposal_ref)),
        PublicProcessed::ExternalJoinProposal { proposal_ref } => {
            (2, None, None, Some(proposal_ref))
        }
        p @ PublicProcessed::StagedCommit { .. } => {
            let sender = match &p {
                PublicProcessed::StagedCommit { sender_leaf, .. } => *sender_leaf,
                _ => unreachable!("matched StagedCommit one line above"),
            };
            // ABI v3's `new_leaf`, computed BEFORE the staged commit enters the table so that a
            // failure here leaves no handle behind (see `with_staged_or_release`).
            if let PublicProcessed::StagedCommit {
                staged,
                sender_leaf: None,
            } = &p
            {
                new_leaf = Some(t.group(handle)?.ext_commit_sender_index(staged)?.u32());
            }
            let staged_handle = t.insert_staged(p);
            (1, sender, Some(staged_handle), None)
        }
        // wire format 3: the DS never sees a PrivateMessage; the message is reported, not raised.
        PublicProcessed::Rejected(_) => (3, None, None, None),
    };

    // ABI v2: the applied list and the committer-update flag. Both elements are always present;
    // for anything but a commit they are the empty array and 0. The staged commit is read back
    // **by reference** (`Table::staged`), never taken: `public_group_merge` is the one consumer.
    // Building the list can fail on input a remote member controls, and the handle is already in
    // the table by then, so it goes through `with_staged_or_release`: see that function for why an
    // error must take the entry with it.
    let (applied, committer_updated) = match staged {
        Some(h) => with_staged_or_release(t, h, |p| match p {
            PublicProcessed::StagedCommit { staged, .. } => Ok((
                applied_proposals(staged)?,
                u64::from(staged.update_path_leaf_node().is_some()),
            )),
            _ => Ok((Vec::new(), 0)),
        })?,
        None => (Vec::new(), 0),
    };

    let mut e = Encoder::new();
    e.array(9).uint(0).uint(kind).uint(epoch);
    e.opt_uint(sender_leaf.map(u64::from));
    e.opt_uint(staged.map(u64::from));
    e.opt_bytes(proposal_ref.as_deref());
    e.array(applied.len());
    for item in &applied {
        e.array(6).bytes(&item.proposal_ref).uint(item.kind);
        e.opt_uint(item.sender_leaf.map(u64::from));
        e.opt_uint(item.target_leaf.map(u64::from));
        e.opt_bytes(item.credential_identity.as_deref());
        e.opt_bytes(item.signature_key.as_deref());
    }
    e.uint(committer_updated);
    e.opt_uint(new_leaf.map(u64::from));
    Ok(e.into_vec())
}

/// Reads the staged commit at `handle` by reference, and releases it if `f` fails.
///
/// `public_group_process` inserts the `StagedCommit` before it can know whether the applied list
/// will build, and `applied_proposals` has two failure modes a remote member controls: a proposal
/// type outside the seven `protocol/02-delivery-service.md` numbers (openmls 0.9.0 carries
/// `SelfRemove` and `Custom` unconditionally) and an Add whose credential is not a
/// `BasicCredential` (`E_CREDENTIAL`). An error frame carries no handle number, and `take_staged`
/// is the only remover, so an entry left behind on that path can never be reached again by
/// `public_group_merge` or `public_group_staged_discard` — for a 1500-leaf group that is the
/// unbounded growth the discard export exists to prevent, on a message the DS did not choose to
/// accept. So the handle is released before the error leaves this module.
fn with_staged_or_release<T>(
    t: &mut Table,
    handle: u32,
    f: impl FnOnce(&PublicProcessed) -> Result<T, AbiError>,
) -> Result<T, AbiError> {
    // An unknown handle is E_ABI_HANDLE and removes nothing: `f` never runs, so the release arm
    // below is not reached and no live entry is touched.
    let staged = t.staged(handle)?;
    match f(staged) {
        Ok(value) => Ok(value),
        Err(err) => {
            let _ = t.take_staged(handle);
            Err(err)
        }
    }
}

/// One entry of ABI v2 §3.1's `applied` array.
///
/// ABI v4: `signature_key` is the added leaf's signature key, for an Add only. The delivery
/// service compares it with the registered key of the device the credential names (invariant 4's
/// Add clause), which it cannot do from the credential alone: nothing in the credential is the
/// leaf's key, and Go does not parse MLS.
struct Applied {
    proposal_ref: Vec<u8>,
    kind: u64,
    sender_leaf: Option<u32>,
    target_leaf: Option<u32>,
    credential_identity: Option<Vec<u8>>,
    signature_key: Option<Vec<u8>>,
}

/// Every proposal the commit resolved, in the order `StagedCommit` reports them.
///
/// `kind` is the RFC 9420 proposal type as `protocol/02-delivery-service.md` numbers it —
/// 1 add, 2 update, 3 remove, 4 psk, 5 reinit, 6 external_init, 7 group_context_extensions.
/// Anything else (openmls 0.9.0 also has `SelfRemove`, `Custom` and the two `extensions-draft`
/// variants) is a shape failure rather than a silently dropped item: invariant 4 clause 1 is a
/// **set** comparison against the DS's outstanding proposals, and an item the DS cannot name is
/// exactly the case where discarding it would let an unreferenced proposal through.
fn applied_proposals(staged: &StagedCommit) -> Result<Vec<Applied>, AbiError> {
    let mut out = Vec::new();
    for queued in staged.queued_proposals() {
        let sender_leaf = match queued.sender() {
            Sender::Member(index) => Some(index.u32()),
            _ => None,
        };
        let proposal_ref = queued.proposal_reference_ref().as_slice().to_vec();
        let mut signature_key = None;
        let (kind, target_leaf, credential_identity) = match queued.proposal() {
            Proposal::Add(add) => {
                signature_key = Some(leaf_signature_key(add.key_package()));
                (1u64, None, Some(leaf_credential_bytes(add.key_package())?))
            }
            Proposal::Update(_) => (2, None, None),
            Proposal::Remove(remove) => (3, Some(remove.removed().u32()), None),
            Proposal::PreSharedKey(_) => (4, None, None),
            Proposal::ReInit(_) => (5, None, None),
            Proposal::ExternalInit(_) => (6, None, None),
            Proposal::GroupContextExtensions(_) => (7, None, None),
            other => {
                return Err(AbiError::shape(format!(
                    "commit applies an unsupported proposal type {:?}",
                    other.proposal_type()
                )));
            }
        };
        out.push(Applied {
            proposal_ref,
            kind,
            sender_leaf,
            target_leaf,
            credential_identity,
            signature_key,
        });
    }
    Ok(out)
}

fn public_group_merge(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 3)?;
    let handle = abi::read_handle(&mut d)?;
    let staged = abi::read_handle(&mut d)?;
    d.finish()?;

    let processed = t.take_staged(staged)?;
    let commit = match processed {
        PublicProcessed::StagedCommit { staged, .. } => staged,
        _ => {
            return Err(AbiError::state(format!(
                "handle {staged} is not a staged commit"
            )));
        }
    };
    let group = t.group_mut(handle)?;
    group.merge_commit(*commit)?;
    let epoch = group.epoch();

    let mut e = Encoder::new();
    e.array(2).uint(0).uint(epoch);
    Ok(e.into_vec())
}

/// Releases a staged commit the DS decided not to merge. `public_group_process` inserts the
/// `StagedCommit` before the delivery service has checked invariant 4, and every refusal path
/// (an omitted outstanding proposal, a committer Update, a cross-user Remove, a GroupInfo whose
/// epoch or signature is wrong) leaves that handle behind. `public_group_merge` consumes a handle
/// but also advances the group, which is exactly what a refusal must not do — so the ABI needs a
/// second, non-advancing consumer. Discarding an unknown handle is `E_ABI_HANDLE`, so a double
/// discard is a clean error rather than a silent success.
fn public_group_staged_discard(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 3)?;
    let handle = abi::read_handle(&mut d)?;
    let staged = abi::read_handle(&mut d)?;
    d.finish()?;
    // `handle` is validated and nothing more: it proves the caller named a *live group*, not that
    // this group is the one `public_group_process` staged `staged` against. The two handle classes
    // share one id space (`handles.rs`) but not an ownership link, so pairing group A's handle with
    // group B's staged handle still discards B's commit. That pairing is the host's to get right —
    // interfaces §2.10 defines `staged` as a flat per-instance handle, exactly as
    // `public_group_merge` takes it, and dillad issues both from the same call site.
    let _ = t.group(handle)?;
    t.take_staged(staged)?;
    let mut e = Encoder::new();
    e.array(1).uint(0);
    Ok(e.into_vec())
}

fn public_group_tree(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 2)?;
    let handle = abi::read_handle(&mut d)?;
    d.finish()?;

    let group = t.group(handle)?;
    let tree = tls::ratchet_tree_out(&group.export_ratchet_tree())?;
    let mut e = Encoder::new();
    e.array(4)
        .uint(0)
        .bytes(&tree)
        .bytes(&group.tree_hash())
        .uint(group.epoch());
    Ok(e.into_vec())
}

fn public_group_state(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 2)?;
    let handle = abi::read_handle(&mut d)?;
    d.finish()?;

    let group = t.group(handle)?;
    let binding = group.binding_bytes();
    let members = group.members();
    let senders = group.external_senders()?;

    // Deviation A2-13: element 5 is a CBOR byte string *wrapping* the 8-element dilla_binding array,
    // which is how §2.10's export table types it (`binding(bstr)`). The same section's field note
    // says "spliced verbatim"; that note is wrong, and splicing a bare array here would make
    // §2.13's mlswasi decoder read a major-4 head where it expects major 2. The Go side decodes the
    // bstr, then decodes the binding array out of those bytes.
    //
    // ABI v5: element 7 is `leaf_count`, every occupied leaf of the tree. The member list above
    // leaves out a leaf it cannot name (no dilla identity, or not a 32-byte key), so a host that
    // adopts a tree compares the two rather than trusting the list's length.
    //
    // ABI v6: element 8 is `external_senders`, the group context's entries in extension order, each
    // `[credential_type, credential, signature_key]` (empty when the extension is absent). The host
    // decides what a group of each kind may carry; the core only reports it (DS-MEMBERSHIP-01).
    let mut e = Encoder::new();
    e.array(8)
        .uint(0)
        .uint(group.epoch())
        .bytes(group.group_id().as_slice())
        .bytes(&group.tree_hash())
        .bytes(&binding);
    e.array(members.len());
    for m in &members {
        e.array(3)
            .uint(u64::from(m.leaf_index))
            .bytes(&m.signature_key)
            .bytes(&m.identity.encode());
    }
    e.uint(group.leaf_count() as u64);
    e.array(senders.len());
    for s in &senders {
        e.array(3)
            .uint(u64::from(s.credential_type))
            .bytes(&s.credential)
            .bytes(&s.signature_key);
    }
    Ok(e.into_vec())
}

fn public_group_proposal_put(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 4)?;
    let handle = abi::read_handle(&mut d)?;
    let op = d.uint()?;
    let mut e = Encoder::new();
    match op {
        0 => {
            let proposal = d.bytes()?;
            d.finish()?;
            let crypto = RustCrypto::default();
            // NV-4: framing and signature verification stay inside dilla-core. `queue_proposal`
            // (Plan A task 11) takes the received ProtocolMessage, verifies it against the group
            // context, stores the original bytes beside the QueuedProposal (deviation A2-11) and
            // returns the new proposal's reference bytes. The wasi crate never builds an
            // `AuthenticatedContent` or a `QueuedProposal` of its own.
            let pm = tls::protocol_message(proposal)?;
            let proposal_ref = t.group_mut(handle)?.queue_proposal(&crypto, pm)?;
            e.array(2).uint(0).bytes(&proposal_ref);
        }
        1 => {
            let raw = d.bytes()?;
            d.finish()?;
            let group = t.group_mut(handle)?;
            let target = group
                .queued_proposals()?
                .into_iter()
                .find(|(r, _)| proposal_ref_bytes(r) == raw)
                .map(|(r, _)| r)
                .ok_or_else(|| AbiError::state("no queued proposal with that ref"))?;
            group.remove_proposal(&target)?;
            e.array(2).uint(0).null();
        }
        2 => {
            d.null()?;
            d.finish()?;
            let group = t.group_mut(handle)?;
            for (r, _) in group.queued_proposals()? {
                group.remove_proposal(&r)?;
            }
            e.array(2).uint(0).null();
        }
        other => {
            return Err(AbiError::shape(format!(
                "proposal op {other}, expected 0, 1 or 2"
            )));
        }
    }
    Ok(e.into_vec())
}

fn public_group_proposal_list(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 2)?;
    let handle = abi::read_handle(&mut d)?;
    d.finish()?;

    // Deviation A2-11: the second element is the original MLSMessage the DS received, not a bare
    // `Proposal`. The DS hands this list straight back to clients in §2.12's CommitConflict and
    // CommitRequired, and a bare Proposal has lost its FramedContent and signature, so a client
    // could not process what it got back. `queued_proposals()` yields `(ProposalRef, Vec<u8>)` of
    // (reference, received bytes) — Plan A task 11 owns that, raised in step 1.
    let queued = t.group(handle)?.queued_proposals()?;
    let mut e = Encoder::new();
    e.array(2).uint(0).array(queued.len());
    for (r, message) in &queued {
        e.array(2).bytes(&proposal_ref_bytes(r)).bytes(message);
    }
    Ok(e.into_vec())
}

/// ABI v2 §3.2. A signature mismatch is `signature_ok = 0`, not an error frame; a signer leaf that
/// is not in the tree is `E_ABI_STATE`, because there is no key to check against.
///
/// The caller names the signer leaf because `VerifiableGroupInfo::signer()` is `pub(crate)` in
/// openmls 0.9.0 (`src/messages/group_info.rs:100`), so the DS cannot read it off the GroupInfo
/// itself; it knows it from the commit it is validating.
fn public_group_group_info_validate(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 4)?;
    let handle = abi::read_handle(&mut d)?;
    let group_info = d.bytes()?;
    let signer_leaf = abi::read_handle(&mut d)?;
    d.finish()?;

    let crypto = RustCrypto::default();
    let verifiable = tls::verifiable_group_info(group_info)?;
    let group = t.group(handle)?;
    let key = group
        .signature_key_of_leaf(LeafNodeIndex::new(signer_leaf))
        .ok_or_else(|| AbiError::state(format!("leaf {signer_leaf} is not in the tree")))?;
    let signature_ok = verifiable.verify_no_out(&crypto, &key).is_ok();
    let context = verifiable.group_context();

    let mut e = Encoder::new();
    e.array(6)
        .uint(0)
        .uint(verifiable.epoch().as_u64())
        .bytes(verifiable.group_id().as_slice())
        .bytes(context.tree_hash())
        .bytes(context.confirmed_transcript_hash())
        .uint(u64::from(signature_ok));
    Ok(e.into_vec())
}

/// ABI v3: `[abi, handle, staged, group_info]` -> the same six elements as
/// `public_group_group_info_validate`, with the signature checked under the key the COMMITTER of
/// `staged` holds in the epoch the commit produces (`DillaPublicGroup::staged_committer_key`).
///
/// Invariant 4 step (6) — "a GroupInfo at epoch n+1 signed by the committer" — is asked before
/// the merge, because the merge is irreversible. For a member commit the tree can answer by leaf
/// index; for an EXTERNAL commit it cannot, since the joiner occupies no leaf until the merge, and
/// its key exists only in the staged commit's UpdatePath. That is why `new_leaf` alone does not
/// make an external commit checkable, and why this export reads the key off the staged commit
/// rather than off the tree (task 27a, Ruling C, deviation B32).
fn public_group_staged_group_info_validate(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 4)?;
    let handle = abi::read_handle(&mut d)?;
    let staged = abi::read_handle(&mut d)?;
    let group_info = d.bytes()?;
    d.finish()?;

    let crypto = RustCrypto::default();
    let verifiable = tls::verifiable_group_info(group_info)?;
    let group = t.group(handle)?;
    let key = match t.staged(staged)? {
        PublicProcessed::StagedCommit {
            staged,
            sender_leaf,
        } => group.staged_committer_key(staged, *sender_leaf),
        _ => {
            return Err(AbiError::handle(format!(
                "handle {staged} is not a staged commit"
            )));
        }
    }
    .ok_or_else(|| AbiError::state("the staged commit names no committer key".to_owned()))?;
    let signature_ok = verifiable.verify_no_out(&crypto, &key).is_ok();
    let context = verifiable.group_context();

    let mut e = Encoder::new();
    e.array(6)
        .uint(0)
        .uint(verifiable.epoch().as_u64())
        .bytes(verifiable.group_id().as_slice())
        .bytes(context.tree_hash())
        .bytes(context.confirmed_transcript_hash())
        .uint(u64::from(signature_ok));
    Ok(e.into_vec())
}

/// ABI v2 §3.3. Called on `POST /v1/groups/{id}/proposal` only: for its own proposals the instance
/// knows kind and target by construction.
///
/// The detail comes from `DillaPublicGroup::queued_proposal_detail`, which reads the
/// `QueuedProposal` the group already holds. The message is **not** re-processed here: the
/// proposal is queued by the time this is called, and `process_message` on an already-queued
/// proposal is a validation failure, not a read.
fn public_group_proposal_inspect(req: &[u8], t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 3)?;
    let handle = abi::read_handle(&mut d)?;
    // Deviation from the brief: `ProposalRef::from_slice` is `#[cfg(any(feature = "test-utils",
    // test))]` in openmls 0.9.0 (`src/ciphersuite/hash_ref.rs:120`), so the reference travels as
    // the opaque bytes it already is and `queued_proposal_detail` compares slices.
    let wanted = d.bytes()?;
    d.finish()?;

    let group = t.group(handle)?;
    let (kind, sender_leaf, target_leaf, credential_identity) = group
        .queued_proposal_detail(wanted)?
        .ok_or_else(|| AbiError::state("no queued proposal for that reference".to_owned()))?;

    let mut e = Encoder::new();
    e.array(5).uint(0).uint(kind);
    e.opt_uint(sender_leaf.map(u64::from));
    e.opt_uint(target_leaf.map(u64::from));
    e.opt_bytes(credential_identity.as_deref());
    Ok(e.into_vec())
}

/// ABI v2 §3.4: the 32-byte franking commitment and the epoch, without a Go TLS parser and without
/// decrypting anything.
fn private_message_aad(req: &[u8], _t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 2)?;
    let bytes = d.bytes()?;
    d.finish()?;

    let header = crate::private_message::parse(bytes)?;
    if header.authenticated_data.len() != 32 {
        return Err(AbiError::shape(format!(
            "authenticated_data is {} bytes, want exactly 32",
            header.authenticated_data.len()
        )));
    }

    let mut e = Encoder::new();
    e.array(4)
        .uint(0)
        .bytes(&header.authenticated_data)
        .uint(header.epoch)
        .uint(u64::from(header.content_type));
    Ok(e.into_vec())
}

fn validate_key_package_export(req: &[u8], _t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 2)?;
    let bytes = d.bytes()?;
    d.finish()?;

    let crypto = RustCrypto::default();
    let kp = validate_key_package(&crypto, tls::key_package_in(bytes)?)?;
    let identity = CredentialIdentity::decode(&leaf_credential_bytes(&kp)?)?;

    // ABI v2 §3.5: the DS keys its KeyPackage table by the RFC 9420 `KeyPackageRef`, so the module
    // computes it here rather than leaving the Go side to re-serialise and hash an MLS object.
    let kp_ref = kp
        .hash_ref(&crypto)
        .map_err(|e| AbiError::state(format!("key package ref: {e}")))?;

    // ABI v4: the leaf's signature key, which the delivery service compares with the uploading
    // device's registered key (protocol/02 invariant 4, the KeyPackage route). ABI v8 appends
    // the exact validated leaf credential identity bytes for managed Bot admission.
    let mut e = Encoder::new();
    e.array(8)
        .uint(0)
        .bytes(identity.device_id.as_bytes())
        .bytes(identity.user_id.as_bytes())
        .uint(u64::from(is_last_resort(&kp)))
        .uint(lifetime_not_after(&kp))
        .bytes(kp_ref.as_slice())
        .bytes(&leaf_signature_key(&kp))
        .bytes(&leaf_credential_bytes(&kp)?);
    Ok(e.into_vec())
}

/// Returns the KeyPackageRefs named by a strictly TLS-decoded MLS Welcome.
/// This is a framing/type inspection only; the caller must bind each ref to its target device.
fn welcome_key_package_refs_export(req: &[u8]) -> Result<Vec<u8>, AbiError> {
    const MAX_WELCOME_BYTES: usize = 1024 * 1024;
    const MAX_RECIPIENTS: usize = 4096;

    let mut d = abi::open(req, 2)?;
    let bytes = d.bytes()?;
    d.finish()?;
    if bytes.is_empty() || bytes.len() > MAX_WELCOME_BYTES {
        return Err(AbiError::shape(
            "Welcome size is outside the accepted bounds",
        ));
    }
    let message = tls::mls_message_in(bytes)?;
    let MlsMessageBodyIn::Welcome(welcome) = message.extract() else {
        return Err(AbiError::shape("MLS message is not a Welcome"));
    };
    let secrets = welcome.secrets();
    if secrets.len() > MAX_RECIPIENTS {
        return Err(AbiError::shape("Welcome has too many recipients"));
    }

    let mut e = Encoder::new();
    e.array(2).uint(0).array(secrets.len());
    for secret in secrets {
        let reference = secret.new_member();
        if reference.as_slice().len() != 32 {
            return Err(AbiError::shape(
                "Welcome recipient KeyPackageRef is not 32 bytes",
            ));
        }
        e.bytes(reference.as_slice());
    }
    Ok(e.into_vec())
}

fn external_propose_add_export(req: &[u8], _t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 5)?;
    let group_id = d.bytes()?;
    let epoch = d.uint()?;
    let key_package = d.bytes()?;
    let signing_key = d.bytes_exact::<32>()?;
    d.finish()?;

    let crypto = RustCrypto::default();
    let kp = validate_key_package(&crypto, tls::key_package_in(key_package)?)?;
    let signer = instance_signer(&signing_key);
    let out = external_propose_add(
        kp,
        GroupId::from_slice(group_id),
        GroupEpoch::from(epoch),
        &signer,
    )?;

    let mut e = Encoder::new();
    e.array(2).uint(0).bytes(&tls::message_out(&out)?);
    Ok(e.into_vec())
}

fn external_propose_remove_export(req: &[u8], _t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 5)?;
    let group_id = d.bytes()?;
    let epoch = d.uint()?;
    let leaf = d.uint()?;
    let signing_key = d.bytes_exact::<32>()?;
    d.finish()?;

    let leaf = u32::try_from(leaf)
        .map_err(|_| AbiError::shape(format!("leaf_index {leaf} does not fit in u32")))?;
    let signer = instance_signer(&signing_key);
    let out = external_propose_remove(
        LeafNodeIndex::new(leaf),
        GroupId::from_slice(group_id),
        GroupEpoch::from(epoch),
        &signer,
    )?;

    let mut e = Encoder::new();
    e.array(2).uint(0).bytes(&tls::message_out(&out)?);
    Ok(e.into_vec())
}

/// ABI v3 (NV-B8): `[abi, list, ssk_pub(32), user_id(16)]` -> `[0, [[device_id(16), dsk_pub(32),
/// tier, revoked(0|1)], …]]`.
///
/// `list` is the user's signed device list as protocol/03-identity.md writes it — the 6-element
/// array, `sig_ssk` included — which is the `blob` of `PUT /v1/users/{id}/device-list`. The list
/// is decoded by `DeviceList::decode`, its `sig_ssk` verified against `ssk_pub` (the key the
/// instance holds in `users.ssk_pub`), and it must name `user_id`; any failure is `E_CREDENTIAL`.
/// So the delivery service never re-implements the format and cannot disagree with the client that
/// produced it, and a substring of the blob can never pass for membership.
///
/// Every entry is reported, revoked ones flagged rather than dropped: the caller decides what a
/// revoked entry means for its rule. No chain check is made against an earlier version (`accept`'s
/// `prev` half): the instance stores and serves the newest list and is not the verifier of record,
/// so this answers "what does the newest list the user signed say".
fn device_list_entries_export(req: &[u8], _t: &mut Table) -> Result<Vec<u8>, AbiError> {
    let mut d = abi::open(req, 4)?;
    let list = d.bytes()?;
    let ssk_pub = d.bytes_exact::<32>()?;
    let user_id = d.bytes_exact::<16>()?;
    d.finish()?;

    let list = dilla_core::identity::DeviceList::decode(list)?;
    list.verify(&ssk_pub)?;
    if list.unsigned.user_id.as_bytes() != &user_id {
        return Err(AbiError::new(
            "E_CREDENTIAL",
            "the device list names another user",
        ));
    }

    let entries = &list.unsigned.entries;
    let mut e = Encoder::new();
    e.array(2).uint(0).array(entries.len());
    for entry in entries {
        e.array(4)
            .bytes(entry.device_id.as_bytes())
            .bytes(&entry.dsk_pub)
            .uint(u64::from(entry.tier.as_u8()))
            .uint(u64::from(entry.revoked_at.is_some()));
    }
    Ok(e.into_vec())
}

/// Rebuilds the instance's Ed25519 signer from its 32-byte private half. `SskSigner` is used only
/// as dilla-core's verified Ed25519 public-key derivation; the key it holds is the instance key,
/// not an SSK.
///
/// NV-3c: `SignatureKeyPair::from_raw` stores `private` verbatim and
/// `openmls_rust_crypto`'s `sign` feeds it to `ed25519_dalek::SigningKey::try_from(&[u8])`, which
/// takes the 32-byte **seed** — so the 32 bytes of the request are exactly what goes in.
fn instance_signer(private: &[u8; 32]) -> SignatureKeyPair {
    let public = dilla_core::identity::SskSigner::from_bytes(private).public();
    SignatureKeyPair::from_raw(SignatureScheme::ED25519, private.to_vec(), public.to_vec())
}

/// NV-3b: the ProposalRef's opaque bytes, as `protocol/02-delivery-service.md` moves them.
fn proposal_ref_bytes(r: &ProposalRef) -> Vec<u8> {
    r.as_slice().to_vec()
}

/// NV-3: the leaf credential's identity bytes, which are dilla's `CredentialIdentity` CBOR.
///
/// Returns an owned `Vec`, never a leaked borrow. `BasicCredential::try_from` produces an owned
/// temporary, so `identity()` can only borrow from it; the earlier `Box::leak` form leaked one
/// allocation per call, and this export is the DS's per-KeyPackage-upload hot path, driven by remote
/// input, inside a wazero instance that dillad pools and keeps alive for the process lifetime (R9).
/// That is a remote memory-exhaustion path, not a style question. The single caller passes the slice
/// straight to `CredentialIdentity::decode(&…)`, so the allocation dies at the end of the call.
fn leaf_credential_bytes(kp: &KeyPackage) -> Result<Vec<u8>, AbiError> {
    let credential = kp.leaf_node().credential();
    BasicCredential::try_from(credential.clone())
        .map(|b| b.identity().to_vec())
        .map_err(|e| AbiError::new("E_CREDENTIAL", e.to_string()))
}

/// The KeyPackage leaf's signature key, the key both of the KeyPackage's signatures verified under
/// (`KeyPackageIn::validate`). It is what a dilla leaf's `sig_ssk_dev` covers and what the
/// delivery service binds to the device's registered key (ABI v4).
fn leaf_signature_key(kp: &KeyPackage) -> Vec<u8> {
    kp.leaf_node().signature_key().as_slice().to_vec()
}

/// NV-3: `last_resort` is a KeyPackage extension, not a leaf-node one.
fn is_last_resort(kp: &KeyPackage) -> bool {
    kp.last_resort()
}

/// NV-3: the upper bound of the KeyPackage's `Lifetime`, in seconds since the Unix epoch.
///
/// Deviation from the brief's snippet, forced by the vendored source: `LeafNode::life_time()` is
/// `pub(crate)` in openmls 0.9.0 (`src/treesync/node/leaf_node.rs:473`) and therefore unreachable
/// from this crate. The public accessor is `KeyPackage::life_time(&self) -> &Lifetime`
/// (`src/key_packages/mod.rs:492`), which is infallible — its doc comment records that a leaf
/// inside a KeyPackage always carries a lifetime, and `KeyPackageIn::validate` (the only way a
/// `KeyPackage` is produced here) rejects any other `LeafNodeSource` with
/// `KeyPackageVerifyError::InvalidLeafNodeSourceType`. So this returns `u64`, not
/// `Result<u64, AbiError>`: there is no error path left to report.
fn lifetime_not_after(kp: &KeyPackage) -> u64 {
    kp.life_time().not_after()
}

#[cfg(test)]
mod tests {
    use super::*;
    use dilla_core::cbor::{Decoder, Encoder, decode_strict};

    fn req(build: impl FnOnce(&mut Encoder)) -> Vec<u8> {
        let mut e = Encoder::new();
        build(&mut e);
        e.into_vec()
    }

    fn version_only() -> Vec<u8> {
        req(|e| {
            e.array(1).uint(dilla_core::ABI_VERSION);
        })
    }

    /// Returns (code, detail) of a failure frame, or panics if the frame is a success.
    fn failure(bytes: &[u8]) -> (String, String) {
        decode_strict(bytes, |d: &mut Decoder<'_>| {
            d.array(3)?;
            assert_eq!(d.uint()?, 1, "expected a failure frame");
            Ok((d.text()?.to_owned(), d.text()?.to_owned()))
        })
        .expect("failure frames are always valid deterministic CBOR")
    }

    #[test]
    fn dilla_abi_reports_the_pinned_versions_and_ciphersuite() {
        let out = dispatch("dilla_abi", &version_only());
        let got = decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(6)?;
            let ok = d.uint()?;
            let abi = d.uint()?;
            let core = d.text()?.to_owned();
            let e2ee = d.uint()?;
            let media = d.uint()?;
            d.array(1)?;
            let suite = d.uint()?;
            Ok((ok, abi, core, e2ee, media, suite))
        })
        .unwrap();
        assert_eq!(got.0, 0);
        assert_eq!(got.1, dilla_core::ABI_VERSION);
        assert_eq!(got.2, dilla_core::CORE_VERSION);
        assert_eq!(got.3, dilla_core::E2EE_VERSION);
        assert_eq!(got.4, dilla_core::MEDIA_VERSION);
        assert_eq!(
            got.5, 0x0001,
            "the only ciphersuite at v1 is MLS_128_DHKEMX25519_AES128GCM_SHA256_Ed25519"
        );
    }

    #[test]
    fn an_unknown_abi_version_is_an_error_frame_not_a_trap() {
        let bad = req(|e| {
            e.array(1).uint(dilla_core::ABI_VERSION + 1);
        });
        assert_eq!(
            failure(&dispatch("dilla_abi", &bad)).0,
            crate::abi::E_ABI_VERSION
        );
    }

    #[test]
    fn truncated_cbor_is_e_abi_shape_not_a_trap() {
        assert_eq!(
            failure(&dispatch("dilla_abi", &[0x82, 0x01])).0,
            crate::abi::E_ABI_SHAPE
        );
        assert_eq!(
            failure(&dispatch("dilla_abi", &[])).0,
            crate::abi::E_ABI_SHAPE
        );
    }

    #[test]
    fn a_map_request_is_rejected_by_the_strict_decoder() {
        // a0 — the empty map, which the dilla CBOR subset forbids outright.
        assert_eq!(
            failure(&dispatch("dilla_abi", &[0xa0])).0,
            crate::abi::E_ABI_SHAPE
        );
    }

    #[test]
    fn an_unknown_export_name_is_an_error_frame() {
        assert_eq!(
            failure(&dispatch("nope", &version_only())).0,
            crate::abi::E_ABI_SHAPE
        );
    }

    #[test]
    fn every_public_group_export_rejects_a_handle_that_was_never_issued() {
        for name in [
            "public_group_export_state",
            "public_group_close",
            "public_group_tree",
            "public_group_state",
            "public_group_proposal_list",
        ] {
            let r = req(|e| {
                e.array(2).uint(dilla_core::ABI_VERSION).uint(4_242);
            });
            assert_eq!(
                failure(&dispatch(name, &r)).0,
                crate::abi::E_ABI_HANDLE,
                "{name}"
            );
        }
    }

    #[test]
    fn merging_a_staged_handle_that_was_never_issued_is_e_abi_handle() {
        let r = req(|e| {
            e.array(3).uint(dilla_core::ABI_VERSION).uint(1).uint(7);
        });
        assert_eq!(
            failure(&dispatch("public_group_merge", &r)).0,
            crate::abi::E_ABI_HANDLE
        );
    }

    #[test]
    fn vectors_check_reports_no_failures() {
        let out = dispatch("vectors_check", &version_only());
        let (ok, passed, failed) = decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(4)?;
            let ok = d.uint()?;
            let passed = d.uint()?;
            let failed = d.uint()?;
            d.skip()?;
            Ok((ok, passed, failed))
        })
        .unwrap();
        assert_eq!(ok, 0);
        assert_eq!(
            failed, 0,
            "the wasi build must reproduce every committed vector"
        );
        assert!(passed > 0, "a zero-case report would pass vacuously");
    }

    #[test]
    fn the_vectors_report_carries_the_same_suites_as_the_native_runner() {
        let native = dilla_core::vectors::run_all();
        let out = dispatch("vectors_check", &version_only());
        let names = decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(4)?;
            d.uint()?;
            d.uint()?;
            d.uint()?;
            let n = d.array_len()?;
            let mut names = Vec::with_capacity(n);
            for _ in 0..n {
                d.array(2)?;
                names.push(d.text()?.to_owned());
                d.skip()?;
            }
            Ok(names)
        })
        .unwrap();
        let expected: Vec<String> = native.suites.iter().map(|s| s.name.to_string()).collect();
        assert_eq!(names, expected);
    }

    /// The committed 1,500-leaf fixture of Plan A task 13. `include_bytes!` is the only way to reach
    /// it from a `crate-type = ["cdylib"]` crate: no `tests/` integration target can link this
    /// library, so the fixture is compiled into the unit-test binary instead. The paths are relative
    /// to `core/dilla-core-wasi/src/`.
    const FIXTURE_TREE: &[u8] =
        include_bytes!("../../../testkit/fixtures/ds-1500/ratchet_tree.mls");
    const FIXTURE_GROUP_INFO: &[u8] =
        include_bytes!("../../../testkit/fixtures/ds-1500/group_info.mls");

    /// Makes a one-leaf MLS group whose tree and GroupInfo are both valid for their own pairing.
    /// A caller supplies the credential bytes so tests can exercise a Dilla-signature failure
    /// without corrupting the enclosing MLS leaf signature or GroupInfo authentication.
    fn small_authenticated_material(
        signer: &SignatureKeyPair,
        identity: &[u8],
    ) -> (Vec<u8>, Vec<u8>) {
        use openmls_rust_crypto::OpenMlsRustCrypto;
        use tls_codec::Serialize as _;

        let provider = OpenMlsRustCrypto::default();
        let binding = dilla_core::mls::DillaBinding {
            v: 1,
            instance_id: dilla_core::ids::InstanceId::from_bytes([0x11; 16]),
            community_id: None,
            target_id: [0x22; 16],
            kind: dilla_core::mls::GroupKind::Pairing,
            policy_version: 1,
            e2ee_version: 1,
            media_version: 0,
        };
        let config = dilla_core::mls::create_config(&binding, None).expect("config");
        let group_id = GroupId::from_slice(&[0x33; 16]);
        let credential = CredentialWithKey {
            credential: BasicCredential::new(identity.to_vec()).into(),
            signature_key: signer.public().into(),
        };
        let group = MlsGroup::new_with_group_id(&provider, signer, &config, group_id, credential)
            .expect("create one-leaf MLS group");
        let tree = group
            .export_ratchet_tree()
            .tls_serialize_detached()
            .expect("serialize tree");
        let group_info = group
            .export_group_info(provider.crypto(), signer, false)
            .expect("export GroupInfo")
            .tls_serialize_detached()
            .expect("serialize GroupInfo");
        (tree, group_info)
    }

    fn roster_test_identity(credential_dsk: &[u8; 32]) -> Vec<u8> {
        use dilla_core::identity::{
            CredentialIdentity, Kind, SignerTier, SskSigner, Tier, UmkSigner,
        };
        use dilla_core::ids::{DeviceId, UserId};

        let umk = UmkSigner::from_bytes(&[0x51; 32]);
        let ssk = SskSigner::from_bytes(&[0x52; 32]);
        let user_id = UserId::from_bytes([0x53; 16]);
        let device_id = DeviceId::from_bytes([0x54; 16]);
        CredentialIdentity {
            v: 1,
            umk_pub: umk.public(),
            user_id,
            device_id,
            kind: Kind::User,
            tier: Tier::Browser,
            signer_tier: SignerTier::Browser,
            ssk_pub: ssk.public(),
            sig_umk_ssk: umk.sign_ssk(&ssk.public()),
            sig_ssk_dev: ssk.sign_device(
                &device_id,
                credential_dsk,
                Kind::User,
                Tier::Browser,
                SignerTier::Browser,
            ),
        }
        .encode()
    }

    #[test]
    fn roster_extraction_authenticates_the_group_and_every_member_credential() {
        let out = dispatch(
            "public_group_roster_extract",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .bytes(FIXTURE_TREE)
                    .bytes(FIXTURE_GROUP_INFO);
            }),
        );
        let count = decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(2)?;
            assert_eq!(d.uint()?, 0);
            let n = d.array_len()?;
            assert_eq!(n, 1500);
            for _ in 0..n {
                d.array(5)?;
                assert_eq!(d.bytes()?.len(), 16);
                assert_eq!(d.bytes()?.len(), 16);
                assert_eq!(d.bytes()?.len(), 32);
                assert!(d.uint()? <= 1);
                assert!(d.uint()? <= 1);
            }
            Ok(n)
        })
        .expect("authenticated fixture roster");
        assert_eq!(count, 1500);

        let mut tampered = FIXTURE_GROUP_INFO.to_vec();
        let last = tampered.len() - 1;
        tampered[last] ^= 1;
        let (code, _) = failure(&dispatch(
            "public_group_roster_extract",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .bytes(FIXTURE_TREE)
                    .bytes(&tampered);
            }),
        ));
        assert_ne!(code, "");
    }

    #[test]
    fn roster_extraction_rejects_a_different_valid_tree_for_the_signed_group_info() {
        let signer = SignatureKeyPair::new(SignatureScheme::ED25519).expect("keygen");
        let dsk: [u8; 32] = signer.public().try_into().expect("Ed25519 public key");
        let identity = roster_test_identity(&dsk);
        let (tree_a, info_a) = small_authenticated_material(&signer, &identity);
        let (tree_b, _info_b) = small_authenticated_material(&signer, &identity);
        assert_ne!(tree_a, tree_b, "independent group trees should differ");

        let (code, detail) = failure(&dispatch(
            "public_group_roster_extract",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .bytes(&tree_b)
                    .bytes(&info_a);
            }),
        ));
        assert_eq!(code, crate::abi::E_ABI_STATE);
        assert!(detail.contains("TreeHashMismatch"), "{detail}");
    }

    #[test]
    fn roster_extraction_rejects_an_invalid_dilla_signature_in_authenticated_mls_material() {
        let signer = SignatureKeyPair::new(SignatureScheme::ED25519).expect("keygen");
        let wrong_dsk = [0xa5; 32];
        let identity = roster_test_identity(&wrong_dsk);
        let (tree, group_info) = small_authenticated_material(&signer, &identity);

        let (code, detail) = failure(&dispatch(
            "public_group_roster_extract",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .bytes(&tree)
                    .bytes(&group_info);
            }),
        ));
        assert_eq!(code, "E_CREDENTIAL", "{detail}");
    }

    #[test]
    fn roster_extraction_rejects_oversized_group_info_before_decoding() {
        let oversized = vec![0; 64 * 1024 + 1];
        let (code, detail) = failure(&dispatch(
            "public_group_roster_extract",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .bytes(FIXTURE_TREE)
                    .bytes(&oversized);
            }),
        ));
        assert_eq!(code, "E_ROSTER_LIMIT", "{detail}");
    }

    /// Creates the fixture group and returns `(handle, epoch, group_id, tree_hash)`.
    fn create_fixture_group() -> (u64, u64, Vec<u8>, Vec<u8>) {
        let r = req(|e| {
            e.array(3)
                .uint(dilla_core::ABI_VERSION)
                .bytes(FIXTURE_TREE)
                .bytes(FIXTURE_GROUP_INFO);
        });
        let out = dispatch("public_group_create", &r);
        decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(6)?;
            assert_eq!(
                d.uint()?,
                0,
                "public_group_create must accept the committed fixture"
            );
            let handle = d.uint()?;
            let epoch = d.uint()?;
            let group_id = d.bytes()?.to_vec();
            let tree_hash = d.bytes()?.to_vec();
            d.skip()?; // the re-serialised GroupInfo; its framing is asserted by Plan B task 3.
            Ok((handle, epoch, group_id, tree_hash))
        })
        .expect("the create response is deterministic CBOR")
    }

    /// interfaces §6 task 14: `public_group_export_state` → `public_group_import_state` →
    /// `public_group_state` reproduces the epoch and tree hash.
    #[test]
    fn state_round_trips_through_export_and_import_byte_for_byte() {
        let (handle, epoch, group_id, tree_hash) = create_fixture_group();

        let r = req(|e| {
            e.array(2).uint(dilla_core::ABI_VERSION).uint(handle);
        });
        let state = decode_strict(
            &dispatch("public_group_export_state", &r),
            |d: &mut Decoder<'_>| {
                d.array(2)?;
                assert_eq!(d.uint()?, 0);
                Ok(d.bytes()?.to_vec())
            },
        )
        .unwrap();
        assert!(
            !state.is_empty(),
            "an empty state blob would make the round-trip vacuous"
        );

        let r = req(|e| {
            e.array(3)
                .uint(dilla_core::ABI_VERSION)
                .bytes(&state)
                .bytes(&group_id);
        });
        let (imported, imported_epoch) = decode_strict(
            &dispatch("public_group_import_state", &r),
            |d: &mut Decoder<'_>| {
                d.array(3)?;
                assert_eq!(d.uint()?, 0);
                Ok((d.uint()?, d.uint()?))
            },
        )
        .unwrap();
        assert_eq!(imported_epoch, epoch);

        let r = req(|e| {
            e.array(2).uint(dilla_core::ABI_VERSION).uint(imported);
        });
        let (got_epoch, got_group_id, got_tree_hash, members, leaf_count, senders) = decode_strict(
            &dispatch("public_group_state", &r),
            |d: &mut Decoder<'_>| {
                d.array(8)?;
                assert_eq!(d.uint()?, 0);
                let epoch = d.uint()?;
                let group_id = d.bytes()?.to_vec();
                let tree_hash = d.bytes()?.to_vec();
                // Deviation A2-13: element 5 is a byte string wrapping the 8-element binding array.
                let binding = d.bytes()?.to_vec();
                let members = d.array_len()?;
                for _ in 0..members {
                    d.array(3)?;
                    d.uint()?;
                    d.bytes()?;
                    d.bytes()?;
                }
                assert!(
                    !binding.is_empty(),
                    "dilla_binding must survive the state round-trip"
                );
                // ABI v5: element 7, every occupied leaf of the tree.
                let leaf_count = d.uint()?;
                // ABI v6: element 8, the external senders as [credential_type, credential, key].
                let n = d.array_len()?;
                let mut senders = Vec::with_capacity(n);
                for _ in 0..n {
                    d.array(3)?;
                    let credential_type = d.uint()?;
                    let credential = d.bytes()?.to_vec();
                    let key = d.bytes()?.to_vec();
                    senders.push((credential_type, credential, key));
                }
                Ok((epoch, group_id, tree_hash, members, leaf_count, senders))
            },
        )
        .unwrap();
        assert_eq!(got_epoch, epoch);
        assert_eq!(got_group_id, group_id);
        assert_eq!(
            got_tree_hash, tree_hash,
            "the tree hash must survive export and import"
        );
        assert_eq!(members, 1_500, "the committed fixture has 1,500 leaves");
        assert_eq!(
            leaf_count, 1_500,
            "leaf_count counts every occupied leaf; every fixture leaf is a dilla identity"
        );
        // The fixture's one external sender survives the round-trip: a basic credential naming the
        // generator's instance, and its 32-byte key.
        assert_eq!(senders.len(), 1, "the fixture names one external sender");
        let (credential_type, credential, key) = &senders[0];
        assert_eq!(*credential_type, 1, "a basic credential");
        assert_eq!(
            credential,
            &dilla_core::mls::instance_credential_identity(
                &dilla_core::ids::InstanceId::from_bytes([0x11; 16])
            )
        );
        assert_eq!(key.len(), 32);
    }

    #[test]
    fn public_group_close_twice_is_a_bad_handle_the_second_time() {
        let (handle, _, _, _) = create_fixture_group();
        let close = req(|e| {
            e.array(2).uint(dilla_core::ABI_VERSION).uint(handle);
        });
        let first = dispatch("public_group_close", &close);
        decode_strict(&first, |d: &mut Decoder<'_>| {
            d.array(1)?;
            assert_eq!(d.uint()?, 0, "the first close succeeds");
            Ok(())
        })
        .unwrap();
        assert_eq!(
            failure(&dispatch("public_group_close", &close)).0,
            crate::abi::E_ABI_HANDLE
        );
        assert_eq!(
            failure(&dispatch("public_group_tree", &close)).0,
            crate::abi::E_ABI_HANDLE
        );
    }

    /// NV-3c's regression net. It proves the signer can be built and used without panicking, which is
    /// the failure mode a wrong `from_raw` private-key layout produces inside openmls_rust_crypto. It
    /// does **not** prove the signature verifies against a real group: that needs a group whose
    /// `external_senders` carries this public key, which is Plan A task 13's testkit scenario
    /// `external_propose_remove produces a message a member accepts as coming from sender index 0`.
    #[test]
    fn the_instance_signer_is_derived_from_the_private_half_and_signs_without_panicking() {
        let sk = [0x0bu8; 32];
        let signer = super::instance_signer(&sk);
        assert_eq!(
            signer.public(),
            dilla_core::identity::SskSigner::from_bytes(&sk).public(),
            "the public half must be derived, never taken from the request"
        );

        let r = req(|e| {
            e.array(5)
                .uint(dilla_core::ABI_VERSION)
                .bytes(&[0x11u8; 16])
                .uint(7)
                .uint(3)
                .bytes(&sk);
        });
        let message = decode_strict(
            &dispatch("external_propose_remove", &r),
            |d: &mut Decoder<'_>| {
                d.array(2)?;
                assert_eq!(
                    d.uint()?,
                    0,
                    "external_propose_remove must produce a success frame"
                );
                Ok(d.bytes()?.to_vec())
            },
        )
        .unwrap();
        assert!(
            !message.is_empty(),
            "an empty MLSMessage would pass vacuously"
        );
        crate::abi::tls::mls_message_in(&message)
            .expect("the output must be a well-formed MLSMessage");
    }

    // The fixture's own commits are the only committed multi-thousand-leaf MLS messages in the
    // tree, and all ten are alternatives at the same base epoch, so each applies cleanly to a
    // freshly created fixture group. `testkit/src/fixtures.rs:158-215` fixes which is which:
    //   commits/00..07 - `DillaGroup::add_members` over MAX_ADDS_PER_COMMIT (256) KeyPackages
    //   commits/08     - `DillaGroup::remove_members(&[LeafNodeIndex::new(1)])`
    //   commits/09     - `DillaGroup::self_update`
    // Naming the generator line in each constant is deliberate: a fixture regeneration that
    // reorders the ten commits must fail these tests loudly rather than silently assert nothing.

    /// `testkit/src/fixtures.rs:158-181` - 256 inline Add proposals, one per KeyPackage.
    const FIXTURE_COMMIT_ADD: &[u8] =
        include_bytes!("../../../testkit/fixtures/ds-1500/commits/00.mls");
    /// `testkit/src/fixtures.rs:189-199` - one Remove of leaf 1.
    const FIXTURE_COMMIT_REMOVE: &[u8] =
        include_bytes!("../../../testkit/fixtures/ds-1500/commits/08.mls");
    /// `testkit/src/fixtures.rs:207-215` - a self-update: no proposals at all.
    const FIXTURE_COMMIT_SELF_UPDATE: &[u8] =
        include_bytes!("../../../testkit/fixtures/ds-1500/commits/09.mls");

    /// Decodes the ABI v2 `public_group_process` response and returns
    /// `(kind, epoch, sender_leaf, staged, applied, committer_updated)`.
    #[allow(clippy::type_complexity)]
    fn process(
        handle: u64,
        message: &[u8],
    ) -> (u64, u64, Option<u64>, Option<u64>, Vec<AppliedItem>, u64) {
        let r = req(|e| {
            e.array(3)
                .uint(dilla_core::ABI_VERSION)
                .uint(handle)
                .bytes(message);
        });
        let out = dispatch("public_group_process", &r);
        decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(9)?;
            assert_eq!(
                d.uint()?,
                0,
                "public_group_process must accept the fixture commit"
            );
            let kind = d.uint()?;
            let epoch = d.uint()?;
            let sender_leaf = d.opt_uint()?;
            let staged = d.opt_uint()?;
            d.skip()?; // proposal_ref
            let n = d.array_len()?;
            let mut applied = Vec::with_capacity(n);
            for _ in 0..n {
                d.array(6)?;
                let proposal_ref = d.bytes()?.to_vec();
                let kind = d.uint()?;
                let sender_leaf = d.opt_uint()?;
                let target_leaf = d.opt_uint()?;
                let credential_identity = d.opt_bytes()?.map(<[u8]>::to_vec);
                let signature_key = d.opt_bytes()?.map(<[u8]>::to_vec);
                applied.push(AppliedItem {
                    proposal_ref,
                    kind,
                    sender_leaf,
                    target_leaf,
                    credential_identity,
                    signature_key,
                });
            }
            let committer_updated = d.uint()?;
            // Every commit this helper is fed is a MEMBER commit (the fixture's ten), and only an
            // external commit names the leaf its joiner lands on.
            assert_eq!(d.opt_uint()?, None, "a member commit reports no new_leaf");
            Ok((kind, epoch, sender_leaf, staged, applied, committer_updated))
        })
        .expect("the process response is deterministic CBOR")
    }

    #[derive(Debug)]
    struct AppliedItem {
        proposal_ref: Vec<u8>,
        kind: u64,
        sender_leaf: Option<u64>,
        target_leaf: Option<u64>,
        credential_identity: Option<Vec<u8>>,
        signature_key: Option<Vec<u8>>,
    }

    /// interfaces §3: a response-shape change moves `abi_version`. 2 when the process and
    /// validate_key_package responses grew; 3 since `public_group_process` grew `new_leaf` and the
    /// module grew `device_list_entries` (task 27a, Ruling C); 4 since `validate_key_package` and
    /// the applied items grew the leaf's `signature_key` (hardening C); 5 since
    /// `public_group_state` grew `leaf_count` (hardening G); 6 since it grew `external_senders`
    /// (DS-MEMBERSHIP-01); 7 since `verify_credential` was exported (DILLA-BOT-04); 8 since
    /// `validate_key_package` appended the leaf credential identity (DILLA-BOT-04 Task5); 9 since
    /// `public_group_roster_extract` was exported; 10 adds strict Welcome recipient-ref inspection.
    #[test]
    fn dilla_abi_reports_version_ten() {
        let out = dispatch("dilla_abi", &version_only());
        let abi = decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(6)?;
            assert_eq!(d.uint()?, 0);
            let abi = d.uint()?;
            // `decode_strict` insists the whole frame is consumed, so the four elements this test
            // does not assert (core_version, e2ee_version, media_version, ciphersuites) are
            // skipped rather than left as trailing bytes.
            d.skip()?; // core_version
            d.skip()?; // e2ee_version
            d.skip()?; // media_version
            d.skip()?; // the ciphersuite array
            Ok(abi)
        })
        .unwrap();
        assert_eq!(
            abi, 10,
            "ABI v10: welcome_key_package_refs exports strict Welcome recipient refs"
        );
        assert_eq!(dilla_core::ABI_VERSION, 10);
    }

    /// An older ABI request must now be refused outright — there is no compatibility
    /// shim (§3).
    #[test]
    fn an_abi_version_two_request_is_refused() {
        for old in [2u64, 3, 4, 5, 6, 7, 8, 9] {
            let r = req(|e| {
                e.array(1).uint(old);
            });
            let (code, detail) = failure(&dispatch("dilla_abi", &r));
            assert_eq!(code, crate::abi::E_ABI_VERSION);
            assert!(
                detail.contains(&old.to_string()) && detail.contains("10"),
                "detail: {detail}"
            );
        }
    }

    /// A signing key for the device lists below. The SSK is a plain Ed25519 key, and
    /// `SskSigner::sign_device_list` signs `"dilla devices v1" || [v, user_id, version, prev_hash,
    /// entries]` exactly as protocol/03-identity.md writes it.
    fn device_list_signer() -> dilla_core::identity::SskSigner {
        dilla_core::identity::SskSigner::from_bytes(&[0x5a; 32])
    }

    const LIST_USER: [u8; 16] = [0xd4; 16];

    /// A signed version-1 list of two devices, the second of them revoked.
    fn signed_device_list(user: [u8; 16]) -> Vec<u8> {
        use dilla_core::identity::{DeviceEntry, DeviceList, DeviceListUnsigned, Tier};
        use dilla_core::ids::{DeviceId, UserId};
        let unsigned = DeviceListUnsigned {
            v: 1,
            user_id: UserId::from_bytes(user),
            version: 1,
            prev_hash: [0u8; 32],
            entries: vec![
                DeviceEntry {
                    device_id: DeviceId::from_bytes([0x01; 16]),
                    dsk_pub: [0x11; 32],
                    tier: Tier::Native,
                    added_at: 1_758_659_640,
                    revoked_at: None,
                },
                DeviceEntry {
                    device_id: DeviceId::from_bytes([0x02; 16]),
                    dsk_pub: [0x22; 32],
                    tier: Tier::Browser,
                    added_at: 1_758_659_641,
                    revoked_at: Some(1_758_700_000),
                },
            ],
        };
        let sig_ssk = device_list_signer().sign_device_list(&unsigned);
        DeviceList { unsigned, sig_ssk }.encode()
    }

    fn device_list_request(list: &[u8], ssk_pub: &[u8; 32], user: &[u8; 16]) -> Vec<u8> {
        req(|e| {
            e.array(4)
                .uint(dilla_core::ABI_VERSION)
                .bytes(list)
                .bytes(ssk_pub)
                .bytes(user);
        })
    }

    /// NV-B8: the delivery service's view of a user's newest signed device list. Every entry is
    /// reported — the revoked one too, flagged — so the caller decides, and the order is the
    /// list's own.
    #[test]
    fn device_list_entries_reports_every_entry_of_a_verified_list() {
        let out = dispatch(
            "device_list_entries",
            &device_list_request(
                &signed_device_list(LIST_USER),
                &device_list_signer().public(),
                &LIST_USER,
            ),
        );
        let entries = decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(2)?;
            assert_eq!(d.uint()?, 0, "a verified list must be accepted");
            let n = d.array_len()?;
            let mut v = Vec::new();
            for _ in 0..n {
                d.array(4)?;
                v.push((
                    d.bytes()?.to_vec(),
                    d.bytes()?.to_vec(),
                    d.uint()?,
                    d.uint()?,
                ));
            }
            Ok(v)
        })
        .expect("the response is deterministic CBOR");
        // protocol/03-identity.md numbers the tiers native = 0, browser = 1.
        assert_eq!(
            entries,
            vec![
                (vec![0x01; 16], vec![0x11; 32], 0, 0),
                (vec![0x02; 16], vec![0x22; 32], 1, 1),
            ]
        );
    }

    /// The signature is the whole point: a list the user's SSK did not sign is refused, however
    /// well formed it is.
    #[test]
    fn device_list_entries_refuses_a_list_another_key_signed() {
        let other = dilla_core::identity::SskSigner::from_bytes(&[0x77; 32]).public();
        let (code, _) = failure(&dispatch(
            "device_list_entries",
            &device_list_request(&signed_device_list(LIST_USER), &other, &LIST_USER),
        ));
        assert_eq!(code, "E_CREDENTIAL");
    }

    /// A list signed by this user's SSK but naming another user is not this user's list.
    #[test]
    fn device_list_entries_refuses_a_list_naming_another_user() {
        let (code, _) = failure(&dispatch(
            "device_list_entries",
            &device_list_request(
                &signed_device_list([0xee; 16]),
                &device_list_signer().public(),
                &LIST_USER,
            ),
        ));
        assert_eq!(code, "E_CREDENTIAL");
    }

    /// A tampered list and a blob that is not a device list at all are refusals, not traps.
    #[test]
    fn device_list_entries_refuses_a_tampered_or_undecodable_list() {
        let mut tampered = signed_device_list(LIST_USER);
        let last = tampered.len() - 1;
        tampered[last] ^= 0x01;
        for blob in [tampered, b"not a device list".to_vec(), Vec::new()] {
            let (code, _) = failure(&dispatch(
                "device_list_entries",
                &device_list_request(&blob, &device_list_signer().public(), &LIST_USER),
            ));
            assert_eq!(code, "E_CREDENTIAL", "blob {blob:02x?}");
        }
    }

    /// Decodes the ABI v3 `public_group_process` response of an external commit and returns
    /// `(kind, sender_leaf, staged, new_leaf)`.
    fn process_external(
        handle: u64,
        message: &[u8],
    ) -> (u64, Option<u64>, Option<u64>, Option<u64>) {
        let r = req(|e| {
            e.array(3)
                .uint(dilla_core::ABI_VERSION)
                .uint(handle)
                .bytes(message);
        });
        let out = dispatch("public_group_process", &r);
        decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(9)?;
            assert_eq!(d.uint()?, 0, "the external commit must be accepted");
            let kind = d.uint()?;
            d.skip()?; // epoch
            let sender_leaf = d.opt_uint()?;
            let staged = d.opt_uint()?;
            d.skip()?; // proposal_ref
            d.skip()?; // applied
            d.skip()?; // committer_updated
            let new_leaf = d.opt_uint()?;
            Ok((kind, sender_leaf, staged, new_leaf))
        })
        .expect("the process response is deterministic CBOR")
    }

    /// Task 25 C1 / deviation B30(a): an external commit names no sender leaf, so the delivery
    /// service needs the leaf the joiner lands on to check the GroupInfo it signed. The joiner here
    /// is a fresh openmls client joining the committed 1,500-leaf fixture from its GroupInfo and
    /// tree, exactly as a resyncing device does from GET /info and GET /tree; the leaf the guest
    /// reports must be the one the joiner's own group says it occupies, and merging the staged
    /// commit must put the joiner's signature key at that leaf.
    #[test]
    fn an_external_commit_reports_the_leaf_the_joiner_lands_on() {
        use openmls_rust_crypto::OpenMlsRustCrypto;
        use tls_codec::Serialize as _;

        let (handle, base_epoch, _group_id, _tree_hash) = create_fixture_group();

        let provider = OpenMlsRustCrypto::default();
        let signer = SignatureKeyPair::new(SignatureScheme::ED25519).expect("keygen");
        let identity = {
            use dilla_core::identity::{
                CredentialIdentity, Kind, SignerTier, SskSigner, Tier, UmkSigner,
            };
            use dilla_core::ids::{DeviceId, UserId};
            let umk = UmkSigner::from_bytes(&[0x61; 32]);
            let ssk = SskSigner::from_bytes(&[0x62; 32]);
            CredentialIdentity {
                v: 1,
                umk_pub: umk.public(),
                user_id: UserId::from_bytes([0x63; 16]),
                device_id: DeviceId::from_bytes([0x64; 16]),
                kind: Kind::User,
                tier: Tier::Native,
                signer_tier: SignerTier::Native,
                ssk_pub: ssk.public(),
                sig_umk_ssk: umk.sign_ssk(&ssk.public()),
                sig_ssk_dev: [0u8; 64],
            }
            .encode()
        };
        let credential = CredentialWithKey {
            credential: BasicCredential::new(identity).into(),
            signature_key: signer.public().into(),
        };
        #[allow(deprecated)]
        let (joined, commit, _info) = MlsGroup::join_by_external_commit(
            &provider,
            &signer,
            Some(tls::ratchet_tree_in(FIXTURE_TREE).expect("tree")),
            tls::verifiable_group_info(FIXTURE_GROUP_INFO).expect("group info"),
            &MlsGroupJoinConfig::default(),
            Some(dilla_core::mls::leaf_capabilities()),
            None,
            &[],
            credential,
        )
        .expect("a fresh client joins the fixture by external commit");
        let commit = commit.tls_serialize_detached().expect("serialize");

        let (kind, sender_leaf, staged, new_leaf) = process_external(handle, &commit);
        assert_eq!(kind, 1, "an external commit is a commit");
        assert_eq!(sender_leaf, None, "an external commit has no member sender");
        let own = u64::from(joined.own_leaf_index().u32());
        assert_eq!(new_leaf, Some(own), "the joiner's own leaf");
        let staged = staged.expect("a staged handle");

        // Invariant 4 step (6) on the external path, BEFORE the merge: the GroupInfo the joiner
        // exports for the new epoch verifies under the key its commit brings, and the base
        // epoch's GroupInfo (signed by leaf 0) does not.
        let joiner_info = joined
            .export_group_info(provider.crypto(), &signer, false)
            .expect("the joiner exports its GroupInfo")
            .tls_serialize_detached()
            .expect("serialize");
        let (epoch, ok) = staged_group_info_validate(handle, staged, &joiner_info);
        assert_eq!(
            epoch,
            base_epoch + 1,
            "the joiner's GroupInfo names epoch n+1"
        );
        assert!(
            ok,
            "the joiner's GroupInfo verifies under the key its commit brings"
        );
        let (_, ok) = staged_group_info_validate(handle, staged, FIXTURE_GROUP_INFO);
        assert!(!ok, "a GroupInfo another leaf signed must not verify");

        // After the merge the leaf the guest named carries the joiner's signature key.
        let merged = dispatch(
            "public_group_merge",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .uint(handle)
                    .uint(staged);
            }),
        );
        decode_strict(&merged, |d: &mut Decoder<'_>| {
            d.array(2)?;
            assert_eq!(d.uint()?, 0, "the staged external commit merges");
            d.skip()?;
            Ok(())
        })
        .unwrap();
        let slot = u32::try_from(own).expect("u32");
        let state = t_with_group(handle, |g| {
            g.leaf(LeafNodeIndex::new(slot))
                .map(|leaf| leaf.signature_key().as_slice().to_vec())
        });
        assert_eq!(state.as_deref(), Some(signer.public()));
    }

    /// The leaf-credential rule as the delivery service meets it across the ABI (protocol/02
    /// invariant 4). A fresh client joins the fixture by external commit and then commits a
    /// self-update whose UpdatePath keeps its key and device id but names another user:
    /// `public_group_process` answers an `E_CREDENTIAL` failure frame - which dillad turns into
    /// `422 E_COMMIT_INVALID`, rule `structural` (`internal/ds/commit.go`, step 4) - and leaves no
    /// staged handle behind. The same member's honest self-update is processed as usual.
    #[test]
    fn a_member_commit_that_changes_its_leafs_credential_is_an_e_credential_frame() {
        use openmls_rust_crypto::OpenMlsRustCrypto;
        use tls_codec::Serialize as _;

        let (handle, _epoch, _group_id, _tree_hash) = create_fixture_group();
        let provider = OpenMlsRustCrypto::default();
        let signer = SignatureKeyPair::new(SignatureScheme::ED25519).expect("keygen");
        let identity = |user: u8| {
            use dilla_core::identity::{
                CredentialIdentity, Kind, SignerTier, SskSigner, Tier, UmkSigner,
            };
            use dilla_core::ids::{DeviceId, UserId};
            let umk = UmkSigner::from_bytes(&[0x61; 32]);
            let ssk = SskSigner::from_bytes(&[0x62; 32]);
            CredentialIdentity {
                v: 1,
                umk_pub: umk.public(),
                user_id: UserId::from_bytes([user; 16]),
                device_id: DeviceId::from_bytes([0x64; 16]),
                kind: Kind::User,
                tier: Tier::Native,
                signer_tier: SignerTier::Native,
                ssk_pub: ssk.public(),
                sig_umk_ssk: umk.sign_ssk(&ssk.public()),
                sig_ssk_dev: [0u8; 64],
            }
            .encode()
        };
        let with_key = |user: u8| CredentialWithKey {
            credential: BasicCredential::new(identity(user)).into(),
            signature_key: signer.public().into(),
        };
        #[allow(deprecated)]
        let (mut joined, commit, _info) = MlsGroup::join_by_external_commit(
            &provider,
            &signer,
            Some(tls::ratchet_tree_in(FIXTURE_TREE).expect("tree")),
            tls::verifiable_group_info(FIXTURE_GROUP_INFO).expect("group info"),
            // dilla's own join configuration: handshakes go out as PublicMessages, which is the
            // only form the delivery service parses (a PrivateMessage is wire format 3, rejected).
            &dilla_core::mls::join_config(dilla_core::mls::GroupKind::Text),
            Some(dilla_core::mls::leaf_capabilities()),
            None,
            &[],
            with_key(0x63),
        )
        .expect("a fresh client joins the fixture by external commit");
        joined
            .merge_pending_commit(&provider)
            .expect("the joiner merges its own external commit");
        let commit = commit.tls_serialize_detached().expect("serialize");
        let (_, _, staged, _) = process_external(handle, &commit);
        let merged = dispatch(
            "public_group_merge",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .uint(handle)
                    .uint(staged.expect("a staged handle"));
            }),
        );
        decode_strict(&merged, |d: &mut Decoder<'_>| {
            d.array(2)?;
            assert_eq!(d.uint()?, 0, "the external commit merges");
            d.skip()?;
            Ok(())
        })
        .unwrap();
        let staged_before = with_table(|t| t.staged_count());

        // The same signature key and device id, another user id.
        let (rotating, _, _) = joined
            .self_update(
                &provider,
                &signer,
                LeafNodeParameters::builder()
                    .with_credential_with_key(with_key(0x65))
                    .build(),
            )
            .expect("OpenMLS builds the commit; the delivery service must refuse it")
            .into_contents();
        let rotating = rotating.tls_serialize_detached().expect("serialize");
        let (code, detail) = failure(&dispatch(
            "public_group_process",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .uint(handle)
                    .bytes(&rotating);
            }),
        ));
        assert_eq!(code, "E_CREDENTIAL", "{detail}");
        assert_eq!(
            with_table(|t| t.staged_count()),
            staged_before,
            "a refused commit must not leave a staged handle"
        );

        // The honest self-update from the same epoch is still a commit the guest stages.
        joined
            .clear_pending_commit(provider.storage())
            .expect("clear the refused commit");
        let (honest, _, _) = joined
            .self_update(&provider, &signer, LeafNodeParameters::default())
            .expect("self_update")
            .into_contents();
        let honest = honest.tls_serialize_detached().expect("serialize");
        let (kind, _, sender_leaf, staged, _, committer_updated) = process(handle, &honest);
        assert_eq!(kind, 1, "a commit");
        assert!(sender_leaf.is_some(), "a member commit names its leaf");
        assert!(staged.is_some(), "the honest commit stages");
        assert_eq!(committer_updated, 1);
    }

    /// `public_group_staged_group_info_validate` -> `(epoch, signature_ok)`.
    fn staged_group_info_validate(handle: u64, staged: u64, group_info: &[u8]) -> (u64, bool) {
        let out = dispatch(
            "public_group_staged_group_info_validate",
            &req(|e| {
                e.array(4)
                    .uint(dilla_core::ABI_VERSION)
                    .uint(handle)
                    .uint(staged)
                    .bytes(group_info);
            }),
        );
        decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(6)?;
            assert_eq!(d.uint()?, 0, "the staged validation must answer");
            let epoch = d.uint()?;
            d.skip()?; // group_id
            d.skip()?; // tree_hash
            d.skip()?; // confirmed_transcript_hash
            Ok((epoch, d.uint()? == 1))
        })
        .expect("the validate response is deterministic CBOR")
    }

    /// The member path of the same export: `commits/09.mls` is leaf 0's self-update and
    /// `commits/09.group_info.mls` the GroupInfo leaf 0 signed for the epoch it produces, so the
    /// key the staged commit brings for its committer verifies it and the epoch is n+1.
    #[test]
    fn a_member_commits_group_info_verifies_under_the_key_its_commit_brings() {
        const MERGED_INFO: &[u8] =
            include_bytes!("../../../testkit/fixtures/ds-1500/commits/09.group_info.mls");
        let (handle, epoch, _group_id, _tree_hash) = create_fixture_group();
        let (_, _, _, staged, _, _) = process(handle, FIXTURE_COMMIT_SELF_UPDATE);
        let staged = staged.expect("a staged handle");
        assert_eq!(
            staged_group_info_validate(handle, staged, MERGED_INFO),
            (epoch + 1, true)
        );
    }

    /// A handle that is not a staged commit is `E_ABI_HANDLE`, never a trap.
    #[test]
    fn staged_group_info_validate_refuses_an_unknown_staged_handle() {
        let (handle, _epoch, _group_id, _tree_hash) = create_fixture_group();
        let (code, _) = failure(&dispatch(
            "public_group_staged_group_info_validate",
            &req(|e| {
                e.array(4)
                    .uint(dilla_core::ABI_VERSION)
                    .uint(handle)
                    .uint(9_999)
                    .bytes(FIXTURE_GROUP_INFO);
            }),
        ));
        assert_eq!(code, crate::abi::E_ABI_HANDLE);
    }

    /// Runs `f` against the live group behind `handle` in this thread's table.
    fn t_with_group<T>(handle: u64, f: impl FnOnce(&DillaPublicGroup) -> T) -> T {
        with_table(|t| {
            f(t.group(u32::try_from(handle).expect("u32"))
                .expect("live handle"))
        })
    }

    /// Kind 1. `DillaGroup::add_members` builds one inline Add proposal per KeyPackage and
    /// `StagedCommit::queued_proposals()` iterates the whole staged queue, inline proposals
    /// included (openmls-0.9.0 `staged_commit.rs:926`), so `applied` has exactly
    /// `MAX_ADDS_PER_COMMIT` items. `add_members` passes `force_self_update(true)` to the commit
    /// builder (openmls-0.9.0 `src/group/mls_group/membership.rs:59,165`), so the UpdatePath is
    /// present and `committer_updated` is 1.
    #[test]
    fn an_add_commit_reports_one_applied_item_per_key_package() {
        let (handle, epoch, _group_id, _tree_hash) = create_fixture_group();
        let (kind, got_epoch, sender_leaf, staged, applied, committer_updated) =
            process(handle, FIXTURE_COMMIT_ADD);
        assert_eq!(kind, 1, "a commit");
        assert_eq!(got_epoch, epoch, "process reports the pre-merge epoch");
        assert!(sender_leaf.is_some(), "a member commit names its leaf");
        assert!(staged.is_some(), "a commit yields a staged handle");
        assert_eq!(
            applied.len(),
            dilla_core::mls::MAX_ADDS_PER_COMMIT,
            "commits/00.mls carries one inline Add per KeyPackage"
        );
        for item in &applied {
            assert_eq!(item.kind, 1, "every item is an Add");
            assert_eq!(
                item.sender_leaf, sender_leaf,
                "an inline proposal is attributed to the committer's own leaf"
            );
            assert!(item.target_leaf.is_none(), "an Add names no leaf");
            assert!(!item.proposal_ref.is_empty(), "every item carries its ref");
            let identity = item
                .credential_identity
                .as_ref()
                .expect("an Add carries the joiner's credential identity");
            dilla_core::identity::CredentialIdentity::decode(identity)
                .expect("the credential identity is the core's 10-element CBOR array");
            // ABI v4: the added leaf's signature key travels with the Add.
            assert_eq!(
                item.signature_key.as_ref().map(Vec::len),
                Some(32),
                "an Add carries its leaf's Ed25519 signature key"
            );
        }
        assert_eq!(committer_updated, 1, "add_members forces a self-update");
    }

    /// Kind 3, with `target_leaf` set and no credential identity.
    #[test]
    fn a_remove_commit_reports_the_removed_leaf() {
        let (handle, _epoch, _group_id, _tree_hash) = create_fixture_group();
        let (kind, _got_epoch, _sender_leaf, staged, applied, committer_updated) =
            process(handle, FIXTURE_COMMIT_REMOVE);
        assert_eq!(kind, 1, "a commit");
        assert!(staged.is_some());
        assert_eq!(
            applied.len(),
            1,
            "commits/08.mls removes exactly one leaf: {applied:?}"
        );
        assert_eq!(applied[0].kind, 3, "a Remove");
        assert_eq!(
            applied[0].target_leaf,
            Some(1),
            "leaf 1, per fixtures.rs:195"
        );
        assert!(
            applied[0].credential_identity.is_none(),
            "a Remove carries no credential"
        );
        assert!(
            applied[0].signature_key.is_none(),
            "a Remove carries no leaf key"
        );
        assert_eq!(
            committer_updated, 1,
            "a Remove requires an UpdatePath (RFC 9420 §17.4)"
        );
    }

    /// The zero-proposal arm: a self-update commit stages, applies nothing, and sets the flag.
    #[test]
    fn a_self_update_commit_reports_no_proposals_and_the_committer_update_flag() {
        let (handle, epoch, _group_id, _tree_hash) = create_fixture_group();
        let (kind, got_epoch, sender_leaf, staged, applied, committer_updated) =
            process(handle, FIXTURE_COMMIT_SELF_UPDATE);
        assert_eq!(kind, 1, "a commit");
        assert_eq!(got_epoch, epoch, "process reports the pre-merge epoch");
        assert!(sender_leaf.is_some(), "a member commit names its leaf");
        assert!(staged.is_some(), "a commit yields a staged handle");
        assert!(
            applied.is_empty(),
            "commits/09.mls references no proposals: {applied:?}"
        );
        assert_eq!(
            committer_updated, 1,
            "a self-update always carries an UpdatePath"
        );
    }

    /// The staged handle survives the applied read and is consumed exactly once by merge.
    #[test]
    fn the_staged_handle_from_a_v2_process_still_merges_exactly_once() {
        let (handle, _epoch, _group_id, _tree_hash) = create_fixture_group();
        let (_kind, _epoch, _sender, staged, _applied, _updated) =
            process(handle, FIXTURE_COMMIT_SELF_UPDATE);
        let staged = staged.expect("a commit stages");
        let merge = req(|e| {
            e.array(3)
                .uint(dilla_core::ABI_VERSION)
                .uint(handle)
                .uint(staged);
        });
        let epoch = decode_strict(
            &dispatch("public_group_merge", &merge),
            |d: &mut Decoder<'_>| {
                d.array(2)?;
                assert_eq!(d.uint()?, 0);
                d.uint()
            },
        )
        .unwrap();
        assert!(epoch > 0);
        let (code, _) = failure(&dispatch("public_group_merge", &merge));
        assert_eq!(
            code,
            crate::abi::E_ABI_HANDLE,
            "the staged handle is consumed by merge"
        );
    }

    /// A refused commit must not leak its staged handle. `public_group_process` inserts the
    /// `StagedCommit` before the DS has run invariant 4, and the DS refuses commits routinely
    /// (an omitted outstanding proposal, a committer Update, a cross-user Remove, a wrong
    /// GroupInfo). `public_group_merge` is the only other consumer, so without a discard a
    /// client that retries a refused commit in a loop grows the handle table without bound.
    #[test]
    fn a_staged_handle_can_be_discarded_without_merging() {
        let (handle, _epoch, _group_id, _tree_hash) = create_fixture_group();
        let (_kind, _epoch, _sender, staged, _applied, _updated) =
            process(handle, FIXTURE_COMMIT_SELF_UPDATE);
        let staged = staged.expect("a commit stages");
        let discard = req(|e| {
            e.array(3)
                .uint(dilla_core::ABI_VERSION)
                .uint(handle)
                .uint(staged);
        });
        decode_strict(
            &dispatch("public_group_staged_discard", &discard),
            |d: &mut Decoder<'_>| {
                d.array(1)?;
                assert_eq!(d.uint()?, 0);
                Ok(())
            },
        )
        .expect("discard answers [0]");
        let (code, _) = failure(&dispatch("public_group_staged_discard", &discard));
        assert_eq!(
            code,
            crate::abi::E_ABI_HANDLE,
            "the handle is gone after one discard"
        );
        let (code, _) = failure(&dispatch("public_group_merge", &discard));
        assert_eq!(
            code,
            crate::abi::E_ABI_HANDLE,
            "and merge cannot resurrect it"
        );
    }

    /// A proposal, not a commit: `applied` is empty and `committer_updated` is 0 even though the
    /// two elements are always present (fixed positions, §3.1).
    #[test]
    fn a_non_commit_still_carries_the_two_new_elements() {
        let (handle, _epoch, _group_id, _tree_hash) = create_fixture_group();
        // A truncated message is rejected by the TLS decoder, which is an error frame, so the
        // shape assertion uses the fixture commit's own staged response instead: both new
        // elements are present for every accepted message, and `a_commit_with_an_update_path…`
        // above covers the commit arm. This test pins the *rejected* arm's shape.
        let r = req(|e| {
            e.array(3)
                .uint(dilla_core::ABI_VERSION)
                .uint(handle)
                .bytes(&[]);
        });
        let (code, _detail) = failure(&dispatch("public_group_process", &r));
        assert_eq!(
            code,
            crate::abi::E_ABI_SHAPE,
            "an empty message is a shape failure, never a trap"
        );
    }

    /// `public_group_process` inserts the `StagedCommit` and *then* builds the applied list, and
    /// building it can fail on input a remote member controls: a proposal type outside the seven
    /// numbered ones (openmls 0.9.0 has `SelfRemove` and `Custom` unconditionally), or an Add whose
    /// credential is not a `BasicCredential`. The error frame carries no handle number and
    /// `take_staged` is the only remover, so an entry left behind is unreachable for ever — the
    /// unbounded growth `public_group_staged_discard` exists to prevent, reached without a discard
    /// ever being callable. The failure is injected here rather than driven through a fixture
    /// because no committed fixture carries an unsupported proposal; what is under test is the
    /// handle accounting, which is the half that leaks.
    #[test]
    fn a_staged_handle_is_released_when_the_applied_list_fails_to_build() {
        let mut t = Table::new();
        let h = t.insert_staged(PublicProcessed::Rejected(
            dilla_core::ProtocolError::Binding,
        ));
        assert_eq!(t.staged_count(), 1);
        let err = with_staged_or_release(&mut t, h, |_| {
            Err::<(), _>(AbiError::shape(
                "commit applies an unsupported proposal type SelfRemove",
            ))
        })
        .unwrap_err();
        assert_eq!(err.code, crate::abi::E_ABI_SHAPE);
        assert_eq!(
            t.staged_count(),
            0,
            "a process call that fails after staging must not strand the handle"
        );
    }

    /// The other half: the success path must leave the handle for `public_group_merge` (or
    /// `public_group_staged_discard`), which is why the applied list is read by reference at all.
    #[test]
    fn a_staged_handle_survives_an_applied_list_that_builds() {
        let mut t = Table::new();
        let h = t.insert_staged(PublicProcessed::Rejected(
            dilla_core::ProtocolError::Binding,
        ));
        assert_eq!(with_staged_or_release(&mut t, h, |_| Ok(7u64)).unwrap(), 7);
        assert_eq!(t.staged_count(), 1);
        assert!(t.take_staged(h).is_ok(), "merge can still consume it");
    }

    /// An unknown staged handle is `E_ABI_HANDLE` and removes nothing — the release path must not
    /// turn a bad handle into a second, silent removal.
    #[test]
    fn with_staged_or_release_rejects_an_unknown_handle() {
        let mut t = Table::new();
        let h = t.insert_staged(PublicProcessed::Rejected(
            dilla_core::ProtocolError::Binding,
        ));
        let err = with_staged_or_release(&mut t, h + 1, |_| Ok(())).unwrap_err();
        assert_eq!(err.code, crate::abi::E_ABI_HANDLE);
        assert_eq!(t.staged_count(), 1, "the live handle is untouched");
    }

    /// §3.2: the fixture's own GroupInfo verifies against the leaf that signed it, and the
    /// response carries the epoch, group id, tree hash and confirmed transcript hash.
    #[test]
    fn a_group_info_signed_by_the_named_leaf_verifies() {
        let (handle, epoch, group_id, tree_hash) = create_fixture_group();
        let signer = fixture_group_info_signer();
        let r = req(|e| {
            e.array(4)
                .uint(dilla_core::ABI_VERSION)
                .uint(handle)
                .bytes(FIXTURE_GROUP_INFO)
                .uint(u64::from(signer));
        });
        let out = dispatch("public_group_group_info_validate", &r);
        let (got_epoch, got_group_id, got_tree_hash, transcript, ok) =
            decode_strict(&out, |d: &mut Decoder<'_>| {
                d.array(6)?;
                assert_eq!(d.uint()?, 0, "validation must not be an error frame");
                Ok((
                    d.uint()?,
                    d.bytes()?.to_vec(),
                    d.bytes()?.to_vec(),
                    d.bytes()?.to_vec(),
                    d.uint()?,
                ))
            })
            .unwrap();
        assert_eq!(got_epoch, epoch);
        assert_eq!(got_group_id, group_id);
        assert_eq!(got_tree_hash, tree_hash);
        assert_eq!(transcript.len(), 32);
        assert_eq!(
            ok, 1,
            "the fixture GroupInfo is signed by its own committer"
        );
    }

    /// A signature mismatch is `signature_ok = 0`, never an error frame: dillad turns it into
    /// `422 E_COMMIT_INVALID` with `rule = "group_info_signature"` (§3.2).
    #[test]
    fn a_group_info_checked_against_another_leaf_reports_signature_ok_zero() {
        let (handle, _epoch, _group_id, _tree_hash) = create_fixture_group();
        let wrong = fixture_group_info_signer() + 1;
        let r = req(|e| {
            e.array(4)
                .uint(dilla_core::ABI_VERSION)
                .uint(handle)
                .bytes(FIXTURE_GROUP_INFO)
                .uint(u64::from(wrong));
        });
        let ok = decode_strict(
            &dispatch("public_group_group_info_validate", &r),
            |d: &mut Decoder<'_>| {
                d.array(6)?;
                assert_eq!(d.uint()?, 0);
                d.skip()?;
                d.skip()?;
                d.skip()?;
                d.skip()?;
                d.uint()
            },
        )
        .unwrap();
        assert_eq!(ok, 0, "a mismatch is reported, not raised");
    }

    /// A leaf that is not in the tree at all is an error frame — there is no key to check against.
    #[test]
    fn a_signer_leaf_outside_the_tree_is_an_error_frame() {
        let (handle, _epoch, _group_id, _tree_hash) = create_fixture_group();
        let r = req(|e| {
            e.array(4)
                .uint(dilla_core::ABI_VERSION)
                .uint(handle)
                .bytes(FIXTURE_GROUP_INFO)
                .uint(1_000_000);
        });
        let (code, _detail) = failure(&dispatch("public_group_group_info_validate", &r));
        assert_eq!(code, crate::abi::E_ABI_STATE);
    }

    /// `validate_key_package` returns the KeyPackageRef, leaf signature key, and exact credential
    /// bytes validated from this same KeyPackage.
    #[test]
    fn validate_key_package_returns_the_key_package_ref() {
        let out = dispatch(
            "validate_key_package",
            &req(|e| {
                e.array(2)
                    .uint(dilla_core::ABI_VERSION)
                    .bytes(FIXTURE_KEY_PACKAGE);
            }),
        );
        let (device_id, user_id, last_resort, not_after, kp_ref, signature_key, credential) =
            decode_strict(&out, |d: &mut Decoder<'_>| {
                d.array(8)?;
                assert_eq!(
                    d.uint()?,
                    0,
                    "the committed KeyPackage fixture must validate"
                );
                Ok((
                    d.bytes()?.to_vec(),
                    d.bytes()?.to_vec(),
                    d.uint()?,
                    d.uint()?,
                    d.bytes()?.to_vec(),
                    d.bytes()?.to_vec(),
                    d.bytes()?.to_vec(),
                ))
            })
            .unwrap();
        let kp = validate_key_package(
            &RustCrypto::default(),
            tls::key_package_in(FIXTURE_KEY_PACKAGE).expect("the fixture decodes"),
        )
        .expect("the fixture validates natively");
        assert_eq!(
            signature_key,
            kp.leaf_node().signature_key().as_slice(),
            "the seventh element is the leaf's own signature key"
        );
        assert_eq!(
            credential,
            leaf_credential_bytes(&kp).expect("validated leaf credential bytes"),
            "the eighth element is the exact leaf credential from the validated KeyPackage"
        );
        assert_eq!(device_id.len(), 16);
        assert_eq!(user_id.len(), 16);
        assert!(last_resort <= 1);
        assert!(not_after > 0);
        assert_eq!(kp_ref.len(), 32, "the ciphersuite's hash is SHA-256");
        assert_eq!(
            kp_ref,
            hex_bytes(&fixture_manifest_field("key_package_ref_hex")),
            "kp_ref must equal KeyPackage::hash_ref computed natively for the same fixture"
        );
    }

    #[test]
    fn welcome_key_package_refs_rejects_malformed_and_non_welcome_inputs() {
        for bytes in [&[0u8, 1][..], FIXTURE_KEY_PACKAGE] {
            let (code, _) = failure(&dispatch(
                "welcome_key_package_refs",
                &req(|e| {
                    e.array(2).uint(dilla_core::ABI_VERSION).bytes(bytes);
                }),
            ));
            assert_eq!(code, crate::abi::E_ABI_SHAPE);
        }
    }

    /// §3.4: exactly 32 bytes of `authenticated_data` is the franking commitment; anything else
    /// is E_ABI_SHAPE, which dillad maps to 422 E_COMMITMENT_INVALID.
    #[test]
    fn private_message_aad_reports_thirty_two_bytes_and_refuses_anything_else() {
        for len in [31usize, 32, 33] {
            let message = crate::private_message::tests_support::message(&vec![5u8; len], 7, 1);
            let out = dispatch(
                "private_message_aad",
                &req(|e| {
                    e.array(2).uint(dilla_core::ABI_VERSION).bytes(&message);
                }),
            );
            if len == 32 {
                let (aad, epoch, content_type) = decode_strict(&out, |d: &mut Decoder<'_>| {
                    d.array(4)?;
                    assert_eq!(d.uint()?, 0);
                    Ok((d.bytes()?.to_vec(), d.uint()?, d.uint()?))
                })
                .unwrap();
                assert_eq!(aad, vec![5u8; 32]);
                assert_eq!(epoch, 7);
                assert_eq!(content_type, 1);
            } else {
                let (code, detail) = failure(&out);
                assert_eq!(code, crate::abi::E_ABI_SHAPE, "len {len}");
                assert!(
                    detail.contains("32"),
                    "detail must name the required length: {detail}"
                );
            }
        }
    }

    /// §3.4 against a `PrivateMessage` **openmls itself framed**, not one this crate encoded.
    ///
    /// `private_message_aad_reports_thirty_two_bytes_and_refuses_anything_else` above feeds the
    /// parser bytes from `private_message::tests_support::message`, an encoder written in the same
    /// module from the same reading of RFC 9420 §6.3.2 as the decoder: a symmetric framing mistake
    /// — a field in the wrong order, a `<V>` header read as a fixed-width length, a `uint16`
    /// written little-endian — would satisfy both sides and be invisible. Since this decode is
    /// hand-written by controller ruling B2, runs on unauthenticated remote input and is the DS's
    /// only reader of the franking commitment, it is checked here against a real encoder:
    /// `testkit/fixtures/ds-1500/application_message.mls` is what `DillaGroup::create_message`
    /// produced through openmls' own `TlsSerialize` derives, with the MLS `authenticated_data` set
    /// to the envelope's 32-byte franking commitment (`mls/group.rs`'s `set_aad`). The manifest
    /// records that commitment and the epoch the message was sent at, both computed natively.
    #[test]
    fn private_message_aad_reads_a_message_openmls_framed() {
        const MESSAGE: &[u8] =
            include_bytes!("../../../testkit/fixtures/ds-1500/application_message.mls");
        let out = dispatch(
            "private_message_aad",
            &req(|e| {
                e.array(2).uint(dilla_core::ABI_VERSION).bytes(MESSAGE);
            }),
        );
        let (aad, epoch, content_type) = decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(4)?;
            assert_eq!(d.uint()?, 0, "a real application message must parse");
            Ok((d.bytes()?.to_vec(), d.uint()?, d.uint()?))
        })
        .unwrap();
        assert_eq!(
            aad,
            hex_bytes(&fixture_manifest_field(
                "application_message_commitment_hex"
            )),
            "the AAD must be the envelope's 32-byte franking commitment"
        );
        assert_eq!(
            epoch,
            fixture_manifest_field("application_message_epoch")
                .parse::<u64>()
                .expect("the manifest's message epoch is a decimal u64"),
            "the epoch must be the one the message was framed at"
        );
        assert_eq!(content_type, 1, "ContentType::Application");
    }

    /// §3.3: an inspected proposal is distinguished by kind and target. The fixture group carries
    /// an `ExternalSenders` extension naming the instance at `instance_sender_index()`, which is
    /// what makes the committed external Remove queueable at all.
    #[test]
    fn proposal_inspect_names_the_kind_and_target_of_a_queued_remove() {
        let (handle, _epoch, _group_id, _tree_hash) = create_fixture_group();
        let proposal_ref = queue_fixture_remove_proposal(handle);
        let out = dispatch(
            "public_group_proposal_inspect",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .uint(handle)
                    .bytes(&proposal_ref);
            }),
        );
        let (kind, _sender, target, _identity) = decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(5)?;
            assert_eq!(d.uint()?, 0);
            Ok((
                d.uint()?,
                d.opt_uint()?,
                d.opt_uint()?,
                d.opt_bytes()?.map(<[u8]>::to_vec),
            ))
        })
        .unwrap();
        assert_eq!(kind, 3, "a Remove");
        assert_eq!(target, Some(0), "the fixture Remove targets leaf 0");
    }

    /// An unknown ref is an error frame, never an empty success.
    #[test]
    fn proposal_inspect_refuses_an_unknown_ref() {
        let (handle, _epoch, _group_id, _tree_hash) = create_fixture_group();
        let (code, _detail) = failure(&dispatch(
            "public_group_proposal_inspect",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .uint(handle)
                    .bytes(&[0u8; 32]);
            }),
        ));
        assert_eq!(code, crate::abi::E_ABI_STATE);
    }

    /// The 1,500-leaf fixture's GroupInfo is signed by the leaf the manifest records.
    fn fixture_group_info_signer() -> u32 {
        fixture_manifest_field("group_info_signer_leaf")
            .parse()
            .expect("the manifest's signer leaf is a decimal u32")
    }

    /// Reads one scalar out of `testkit/fixtures/ds-1500/manifest.json` without a JSON dependency:
    /// the file is generated by `dilla-testkit gen-public-group` with one key per line.
    fn fixture_manifest_field(key: &str) -> String {
        const MANIFEST: &str = include_str!("../../../testkit/fixtures/ds-1500/manifest.json");
        let needle = format!("\"{key}\"");
        let line = MANIFEST
            .lines()
            .find(|l| l.contains(&needle))
            .unwrap_or_else(|| {
                panic!(
                    "manifest.json carries no {key}; regenerate the fixture with \
                     `cargo run -p dilla-testkit -- gen-public-group --out testkit/fixtures/ds-1500`"
                )
            });
        let value = line
            .split_once(':')
            .expect("a manifest line is \"key\": value")
            .1
            .trim()
            .trim_end_matches(',')
            .trim_matches('"');
        value.to_owned()
    }

    fn hex_bytes(s: &str) -> Vec<u8> {
        assert!(
            s.len().is_multiple_of(2),
            "hex must have an even length: {s}"
        );
        (0..s.len())
            .step_by(2)
            .map(|i| u8::from_str_radix(&s[i..i + 2], 16).expect("hex"))
            .collect()
    }

    /// Every committed fixture file lives under `testkit/fixtures/ds-1500/`, which is what
    /// `gen-public-group --out …/testkit/fixtures/ds-1500` writes. Nothing is written to
    /// `testkit/fixtures/` itself, so no reader may point there.
    const FIXTURE_KEY_PACKAGE: &[u8] =
        include_bytes!("../../../testkit/fixtures/ds-1500/key_package.mls");

    /// Queues the fixture's committed Remove proposal against the handle and returns its ref.
    ///
    /// `public_group_proposal_put` op 0 answers `[0, bstr]` — the reference is a plain byte
    /// string, not an optional one (`exports.rs`'s op-0 arm writes `.bytes(&proposal_ref)`), so it
    /// is read back with `bytes()`.
    fn queue_fixture_remove_proposal(handle: u64) -> Vec<u8> {
        const REMOVE: &[u8] = include_bytes!("../../../testkit/fixtures/ds-1500/remove_leaf0.mls");
        let out = dispatch(
            "public_group_proposal_put",
            &req(|e| {
                e.array(4)
                    .uint(dilla_core::ABI_VERSION)
                    .uint(handle)
                    .uint(0)
                    .bytes(REMOVE);
            }),
        );
        decode_strict(&out, |d: &mut Decoder<'_>| {
            d.array(2)?;
            assert_eq!(d.uint()?, 0, "queueing the fixture Remove must succeed");
            Ok(d.bytes()?.to_vec())
        })
        .unwrap()
    }

    fn signed_native_bot_credential() -> (CredentialIdentity, [u8; 32]) {
        use dilla_core::identity::{Kind, SignerTier, SskSigner, Tier, UmkSigner};
        use dilla_core::ids::{DeviceId, UserId};

        let umk = UmkSigner::from_bytes(&[0x11; 32]);
        let ssk = SskSigner::from_bytes(&[0x22; 32]);
        let dsk = [0x33; 32];
        let device_id = DeviceId::from_bytes([0x44; 16]);
        (
            CredentialIdentity {
                v: 1,
                umk_pub: umk.public(),
                user_id: UserId::from_bytes([0; 16]),
                device_id,
                kind: Kind::Bot,
                tier: Tier::Native,
                signer_tier: SignerTier::Native,
                ssk_pub: ssk.public(),
                sig_umk_ssk: umk.sign_ssk(&ssk.public()),
                sig_ssk_dev: ssk.sign_device(
                    &device_id,
                    &dsk,
                    Kind::Bot,
                    Tier::Native,
                    SignerTier::Native,
                ),
            },
            dsk,
        )
    }

    fn credential_request(credential: &[u8], dsk: &[u8], version: u64) -> Vec<u8> {
        req(|e| {
            e.array(3).uint(version).bytes(credential).bytes(dsk);
        })
    }

    #[test]
    fn verify_credential_refuses_bad_signatures_provisional_and_small_order_key_material() {
        let (credential, dsk) = signed_native_bot_credential();
        let mut cases = Vec::new();

        let mut bad_umk_signature = credential.clone();
        bad_umk_signature.sig_umk_ssk[0] ^= 1;
        cases.push(("umk-ssk signature", bad_umk_signature));

        let mut bad_device_signature = credential.clone();
        bad_device_signature.sig_ssk_dev[0] ^= 1;
        cases.push(("ssk-device signature", bad_device_signature));

        let provisional = CredentialIdentity::provisional(
            credential.umk_pub,
            credential.user_id,
            credential.device_id,
            credential.kind,
            credential.tier,
        );
        cases.push(("provisional", provisional));

        let mut small_order_key = credential.clone();
        small_order_key.umk_pub = [0; 32];
        cases.push(("small-order public key", small_order_key));

        let mut small_order_signature = credential.clone();
        small_order_signature.sig_umk_ssk = [0; 64];
        cases.push(("small-order signature encoding", small_order_signature));

        for (name, invalid) in cases {
            let response = dispatch(
                "verify_credential",
                &credential_request(&invalid.encode(), &dsk, dilla_core::ABI_VERSION),
            );
            assert_eq!(failure(&response).0, "E_CREDENTIAL", "{name}");
        }
        let mut wrong_dsk = dsk;
        wrong_dsk[0] ^= 1;
        let response = dispatch(
            "verify_credential",
            &credential_request(&credential.encode(), &wrong_dsk, dilla_core::ABI_VERSION),
        );
        assert_eq!(failure(&response).0, "E_CREDENTIAL", "wrong leaf DSK");
    }

    #[test]
    fn verify_credential_rejects_noncanonical_trailing_truncated_and_oversized_inputs() {
        let (credential, dsk) = signed_native_bot_credential();
        let valid = credential.encode();
        let mut trailing = valid.clone();
        trailing.push(0);
        let mut nonminimal = valid.clone();
        nonminimal.splice(1..2, [0x18, 0x01]);
        let mut array_key_instead_of_bytes = valid.clone();
        assert_eq!(
            array_key_instead_of_bytes[2], 0x58,
            "UMK key uses a 32-byte bstr"
        );
        array_key_instead_of_bytes[2] = 0x98;
        let truncated = valid[..valid.len() - 1].to_vec();
        let oversized = vec![0x41; 8193];

        for (name, bytes) in [
            ("trailing", trailing),
            ("nonminimal", nonminimal),
            ("array instead of bstr", array_key_instead_of_bytes),
            ("truncated", truncated),
            ("oversized", oversized),
        ] {
            let response = dispatch(
                "verify_credential",
                &credential_request(&bytes, &dsk, dilla_core::ABI_VERSION),
            );
            assert_eq!(failure(&response).0, "E_CREDENTIAL", "{name}");
        }

        let wrong_abi = dispatch(
            "verify_credential",
            &credential_request(&valid, &dsk, dilla_core::ABI_VERSION - 1),
        );
        assert_eq!(failure(&wrong_abi).0, crate::abi::E_ABI_VERSION);

        let wrong_dsk_shape = dispatch(
            "verify_credential",
            &credential_request(&valid, &[0; 31], dilla_core::ABI_VERSION),
        );
        assert_eq!(failure(&wrong_dsk_shape).0, crate::abi::E_ABI_SHAPE);
    }

    #[test]
    fn verify_credential_accepts_a_valid_native_bot_credential() {
        let (credential, dsk) = signed_native_bot_credential();
        let credential_bytes = credential.encode();
        let response = dispatch(
            "verify_credential",
            &req(|e| {
                e.array(3)
                    .uint(dilla_core::ABI_VERSION)
                    .bytes(&credential_bytes)
                    .bytes(&dsk);
            }),
        );
        let got = decode_strict(&response, |d: &mut Decoder<'_>| {
            d.array(2)?;
            assert_eq!(d.uint()?, 0, "verification succeeds");
            d.array(8)?;
            Ok((
                d.bytes()?.to_vec(),
                d.bytes()?.to_vec(),
                d.bytes()?.to_vec(),
                d.uint()?,
                d.uint()?,
                d.uint()?,
                d.bytes()?.to_vec(),
                d.bytes()?.to_vec(),
            ))
        })
        .expect("success response is strict CBOR");
        assert_eq!(got.0, credential.umk_pub);
        assert_eq!(got.1, [0; 16]);
        assert_eq!(got.2, [0x44; 16]);
        assert_eq!((got.3, got.4, got.5), (1, 0, 0));
        assert_eq!(got.6, credential.ssk_pub);
        assert_eq!(got.7, credential.sig_umk_ssk);
    }
}
