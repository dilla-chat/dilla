//! Negative cases of the trust refresh after PR #72 (web3.12.2).
//!
//! In one pass the worker ingests one user's history for the first of that user's leaves. Every
//! later leaf of the same user then gets the history sliced from the version that ingest cached
//! (client-core `historyFrom`). Every slice still goes through `ingest_leaf`, so the core must
//! tell a slice that was "correctly relaxed" from one that was "relaxed too far". Each test here
//! asserts a refusal (or, for the equal boundary, the one acceptance a no-op refresh needs).
//!
//! Bob (UMK 0xb2, SSK 0xf2) has these leaves in GROUP, all joined before Alice caches any list:
//! - `bob`: device 0xb3, in v1 and v2;
//! - `bob2`: device 0xb4, added in v2 (the second device that reloads mid-sync);
//! - `rogue`: device 0xb5, a credential cross-signed by Bob's own SSK, in no signed list;
//! - `other`: device 0xb6, listed in v2, but its credential is under UMK 0xc1 / SSK 0xc2, which
//!   signed no list;
//! - `rot`: device 0xb7, UMK 0xb2 with a rotated SSK 0xf9, in no list Alice has cached.
#![cfg(not(target_arch = "wasm32"))]
mod client_support;
use client_support::*;
use dilla_core::cbor::{Encoder, decode_strict};
use dilla_core::identity::{DeviceList, SskSigner};

const BOB: [u8; 16] = [0xb3; 16];
const BOB2: [u8; 16] = [0xb4; 16];
const ROGUE: [u8; 16] = [0xb5; 16];
const OTHER: [u8; 16] = [0xb6; 16];
const ROT: [u8; 16] = [0xb7; 16];

struct World {
    relay: Relay,
    alice: Core,
    bob: RawPeer,
    bob2: RawPeer,
    rogue: RawPeer,
    rot: RawPeer,
    v1: DeviceList,
    v2: DeviceList,
    /// bob2's message and the rogue device's, both held by Alice for Bob's signed list.
    bob2_seq: u64,
    rogue_seq: u64,
}

fn world() -> World {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut alice = ready_core(0xa1, "alice");
    alice.create_and_register(&mut relay, &instance);
    let mut bob = RawPeer::new(0xb2, 0xb3);
    bob.join_external(&mut relay);
    let mut bob2 = RawPeer::new_with_keys(0xb2, 0xb4, 0xb2, 0xf2);
    bob2.join_external(&mut relay);
    let mut rogue = RawPeer::new_with_keys(0xb2, 0xb5, 0xb2, 0xf2);
    let join = rogue.join_external(&mut relay);
    bob2.apply_commit(&relay, join);
    let mut other = RawPeer::new_with_keys(0xb2, 0xb6, 0xc1, 0xc2);
    let join = other.join_external(&mut relay);
    bob2.apply_commit(&relay, join);
    rogue.apply_commit(&relay, join);
    let mut rot = RawPeer::new_with_keys(0xb2, 0xb7, 0xb2, 0xf9);
    let join = rot.join_external(&mut relay);
    bob2.apply_commit(&relay, join);
    rogue.apply_commit(&relay, join);
    alice.sync(&relay);
    let body = bob.device_list_body();
    let v1 = DeviceList::decode(
        &decode_strict(&body, |d| {
            d.array(4)?;
            d.uint()?;
            let blob = d.bytes()?.to_vec();
            d.skip()?;
            d.skip()?;
            Ok(blob)
        })
        .unwrap(),
    )
    .unwrap();
    let v2 = sec_next(
        &v1,
        vec![
            v1.unsigned.entries[0].clone(),
            sec_entry(&bob2),
            sec_entry(&other),
        ],
        0xf2,
    );
    let bob2_seq = bob2.send(&mut relay, "second device comet");
    let rogue_seq = rogue.send(&mut relay, "rogue comet");
    alice.sync(&relay);
    for seq in [bob2_seq, rogue_seq] {
        let row = alice.row_at(&GROUP, seq);
        assert_eq!(
            (row.status, row.reason.as_str()),
            (1, "E_DEVICE_LIST_STALE"),
            "held for Bob's signed list before any pass"
        );
    }
    World {
        relay,
        alice,
        bob,
        bob2,
        rogue,
        rot,
        v1,
        v2,
        bob2_seq,
        rogue_seq,
    }
}

type Verdict = (u64, u64, u64);
/// Alice's cached row for Bob: version, signed list, UMK, SSK.
type CachedRow = (i64, Vec<u8>, [u8; 32], [u8; 32]);

fn ingest_body(
    w: &mut World,
    via: [u8; 16],
    history: &[u8],
    head: &DeviceList,
) -> Result<Verdict, &'static str> {
    let leaf = w.relay.leaf_of(via);
    w.alice
        .core
        .device_list_ingest(&GROUP, leaf, &w.bob.user, history, &head.encode(), NOW + 3)
        .map(|answer| {
            decode_strict(&answer, |d| {
                d.array(3)?;
                Ok((d.uint()?, d.uint()?, d.uint()?))
            })
            .unwrap()
        })
        .map_err(|e| e.code)
}

/// One pass's ingest of `lists` (the history read, or a slice of it) for the leaf of `via`.
fn ingest(w: &mut World, via: [u8; 16], lists: &[&DeviceList]) -> Result<Verdict, &'static str> {
    let head = (*lists.last().expect("a nonempty history")).clone();
    ingest_body(w, via, &sec_history(lists), &head)
}

fn revalidate(w: &mut World, via: [u8; 16]) -> u32 {
    let leaf = w.relay.leaf_of(via);
    w.alice
        .core
        .revalidate_pending(&GROUP, leaf, 256)
        .expect("revalidate")
}

fn cached(w: &World) -> Option<CachedRow> {
    w.alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT version,list,umk_pub,ssk_pub FROM app_device_lists WHERE user_id=?1",
            [w.bob.user.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?, r.get(3)?)),
        )
        .ok()
}

/// The first leaf of the pass ingests [v1, v2] and caches v2: every case below starts here. The
/// verdict's quarantine count is Bob's three leaves no signed list backs (rogue, other, rot).
fn cache_v2(w: &mut World) -> CachedRow {
    let (v1, v2) = (w.v1.clone(), w.v2.clone());
    assert_eq!(ingest(w, BOB2, &[&v1, &v2]), Ok((2, 0, 3)));
    let row = cached(w).expect("v2 cached");
    assert_eq!((row.0, row.1.as_slice()), (2, w.v2.encode().as_slice()));
    row
}

/// The rogue leaf's verdict is E_DEVICE_UNLISTED (Alice's quarantine row for exactly that leaf),
/// and its held message stays unreadable. The row was held for Bob's list (E_DEVICE_LIST_STALE:
/// wait for the list); once the leaf is judged, it carries the verdict (E_DEVICE_UNLISTED: the
/// device must be added to the list), as a message the leaf sends after the verdict does
/// (`assert_rogue_refused_now`).
fn assert_rogue_unreadable(w: &World) {
    let row = w.alice.row_at(&GROUP, w.rogue_seq);
    assert_eq!(
        (row.status, row.reason.as_str(), row.body.as_str()),
        (1, "E_DEVICE_UNLISTED", ""),
        "the unlisted device's held message stays unreadable, for the reason it is unreadable"
    );
    let verdict: Vec<String> = {
        let db = w.alice.probe.lock().unwrap();
        let mut stmt = db
            .prepare(
                "SELECT code FROM app_quarantine WHERE group_id=?1 AND user_id=?2 AND device_id=?3",
            )
            .unwrap();
        stmt.query_map(
            rusqlite::params![GROUP.as_slice(), w.bob.user.as_slice(), ROGUE.as_slice()],
            |r| r.get(0),
        )
        .unwrap()
        .collect::<Result<_, _>>()
        .unwrap()
    };
    assert_eq!(
        verdict,
        ["E_DEVICE_UNLISTED"],
        "the leaf's verdict names why"
    );
}

/// A message the rogue device sends after the list is cached: unreadable, E_DEVICE_UNLISTED.
fn assert_rogue_refused_now(w: &mut World, text: &str) {
    let seq = w.rogue.send(&mut w.relay, text);
    w.alice.sync(&w.relay);
    let row = w.alice.row_at(&GROUP, seq);
    assert_eq!(
        (row.status, row.reason.as_str(), row.body.as_str()),
        (1, "E_DEVICE_UNLISTED", "")
    );
    assert_eq!(revalidate(w, ROGUE), 0);
}

fn assert_bob2_readable(w: &World) {
    let row = w.alice.row_at(&GROUP, w.bob2_seq);
    assert_eq!((row.status, row.body.as_str()), (0, "second device comet"));
}

// 1. An unauthorised device, in both orders within one pass.

#[test]
fn an_unlisted_device_is_refused_on_the_slice_after_its_sibling_cached_the_list() {
    let mut w = world();
    cache_v2(&mut w);
    assert_eq!(revalidate(&mut w, BOB2), 1);
    assert_bob2_readable(&w);
    let v2 = w.v2.clone();
    // The slice the worker hands the rogue leaf after the sibling's ingest.
    assert_eq!(ingest(&mut w, ROGUE, &[&v2]), Err("E_DEVICE_UNLISTED"));
    // Even a worker that revalidated the refused leaf releases nothing.
    assert_eq!(revalidate(&mut w, ROGUE), 0);
    assert_rogue_unreadable(&w);
    // The refusal cached nothing and the next legitimate leaf of the pass still ingests the slice.
    assert_eq!(cached(&w).unwrap().0, 2);
    assert_eq!(ingest(&mut w, BOB, &[&v2]), Ok((2, 0, 3)));
    assert_eq!(revalidate(&mut w, ROGUE), 0);
    assert_rogue_unreadable(&w);
    assert_rogue_refused_now(&mut w, "rogue after the slice");
}

#[test]
fn an_unlisted_device_first_in_the_pass_is_refused_and_then_refused_again_on_the_slice() {
    let mut w = world();
    let (v1, v2) = (w.v1.clone(), w.v2.clone());
    // The rogue leaf comes first: the full history from version 0.
    assert_eq!(ingest(&mut w, ROGUE, &[&v1, &v2]), Err("E_DEVICE_UNLISTED"));
    assert!(cached(&w).is_none(), "a refused ingest caches nothing");
    assert_eq!(revalidate(&mut w, ROGUE), 0);
    // Its legitimate sibling next, with the same full history: accepted, and it is released.
    cache_v2(&mut w);
    assert_eq!(revalidate(&mut w, BOB2), 1);
    assert_bob2_readable(&w);
    assert_rogue_unreadable(&w);
    // A later pass (or a retry) hands the rogue leaf the slice from the cached version: refused.
    assert_eq!(ingest(&mut w, ROGUE, &[&v2]), Err("E_DEVICE_UNLISTED"));
    assert_eq!(revalidate(&mut w, ROGUE), 0);
    assert_rogue_unreadable(&w);
    assert_rogue_refused_now(&mut w, "rogue after the slice");
}

// 2. A forked or substituted history after v2 is cached.

#[test]
fn a_correctly_signed_v2_that_is_not_the_cached_v2_is_refused_as_stale() {
    let mut w = world();
    let before = cache_v2(&mut w);
    let v1 = w.v1.clone();
    // A fork signed by Bob's own SSK, one that would authorise the rogue device.
    let fork = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&w.rogue)],
        0xf2,
    );
    assert_ne!(fork.encode(), w.v2.encode());
    let fork3 = sec_next(&fork, fork.unsigned.entries.clone(), 0xf2);
    // A single-row slice: the first-row requirement carries the whole weight.
    assert_eq!(ingest(&mut w, ROGUE, &[&fork]), Err("E_DEVICE_LIST_STALE"));
    assert_eq!(
        ingest(&mut w, ROGUE, &[&fork, &fork3]),
        Err("E_DEVICE_LIST_STALE")
    );
    assert_eq!(
        ingest(&mut w, ROGUE, &[&v1, &fork]),
        Err("E_DEVICE_LIST_STALE")
    );
    // Through a listed leaf too: the slice must carry the cached v2, not a different one.
    assert_eq!(ingest(&mut w, BOB, &[&fork]), Err("E_DEVICE_LIST_STALE"));
    assert_eq!(cached(&w), Some(before));
    assert_eq!(revalidate(&mut w, ROGUE), 0);
    assert_rogue_unreadable(&w);
}

/// The first row byte-equal to the cached blob, but the cached key that must have signed it is
/// another one (a cache row whose key column was rebound). The byte-equality half holds, so only
/// the signature half can refuse it.
#[test]
fn a_byte_equal_first_row_not_signed_under_the_cached_key_is_refused() {
    let mut w = world();
    cache_v2(&mut w);
    let rotated = SskSigner::from_bytes(&[0xf9; 32]).public();
    w.alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_device_lists SET ssk_pub=?1 WHERE user_id=?2",
            rusqlite::params![rotated.as_slice(), w.bob.user.as_slice()],
        )
        .unwrap();
    let v2 = w.v2.clone();
    let mut entries = v2.unsigned.entries.clone();
    entries.push(sec_entry(&w.rot));
    let v3 = sec_next(&v2, entries, 0xf9);
    assert_eq!(ingest(&mut w, ROT, &[&v2, &v3]), Err("E_CREDENTIAL"));
    // The single-row slice of the same: refused as well.
    assert_eq!(ingest(&mut w, ROT, &[&v2]), Err("E_CREDENTIAL"));
    assert_eq!(cached(&w).unwrap().0, 2);
}

/// Same UMK, another SSK: a leaf under a rotated SSK cannot extend the list the old SSK signed.
#[test]
fn a_leaf_under_another_ssk_of_the_cached_umk_cannot_extend_the_cached_list() {
    let mut w = world();
    let before = cache_v2(&mut w);
    let v2 = w.v2.clone();
    let mut entries = v2.unsigned.entries.clone();
    entries.push(sec_entry(&w.rot));
    let v3 = sec_next(&v2, entries, 0xf9);
    assert_eq!(ingest(&mut w, ROT, &[&v2, &v3]), Err("E_CREDENTIAL"));
    assert_eq!(cached(&w), Some(before));
}

/// The served row's own fields must match the signed list it carries: each one on its own.
#[test]
fn a_served_row_whose_fields_disagree_with_its_signed_list_is_refused_one_field_at_a_time() {
    let mut w = world();
    let before = cache_v2(&mut w);
    let v2 = w.v2.clone();
    let row = |version: u64, sig: [u8; 64], prev: [u8; 32]| {
        let mut e = Encoder::new();
        e.array(1)
            .array(4)
            .uint(version)
            .bytes(&v2.encode())
            .bytes(&sig)
            .bytes(&prev);
        e.into_vec()
    };
    let (sig, prev) = (v2.sig_ssk, v2.unsigned.prev_hash);
    assert_eq!(
        ingest_body(&mut w, BOB, &row(2, sig, prev), &v2),
        Ok((2, 0, 3))
    );
    for (what, body) in [
        ("version", row(3, sig, prev)),
        ("signature", row(2, [0; 64], prev)),
        ("prev_hash", row(2, sig, [0; 32])),
    ] {
        assert_eq!(
            ingest_body(&mut w, ROGUE, &body, &v2),
            Err("E_DEVICE_LIST_STALE"),
            "a served row with another {what}"
        );
        assert_eq!(
            ingest_body(&mut w, BOB, &body, &v2),
            Err("E_DEVICE_LIST_STALE"),
            "a served row with another {what}, through a listed leaf"
        );
    }
    assert_eq!(cached(&w), Some(before));
}

/// A cache row whose version column disagrees with its blob: the version half of the anchor check
/// on its own (the blob half holds).
#[test]
fn a_cache_row_whose_version_disagrees_with_its_blob_anchors_nothing() {
    let mut w = world();
    cache_v2(&mut w);
    w.alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_device_lists SET version=3 WHERE user_id=?1",
            [w.bob.user.as_slice()],
        )
        .unwrap();
    let v2 = w.v2.clone();
    let v3 = sec_next(&v2, v2.unsigned.entries.clone(), 0xf2);
    assert_eq!(ingest(&mut w, BOB, &[&v2, &v3]), Err("E_DEVICE_LIST_STALE"));
}

/// A zeroed cache row (version 0) whose blob is a well-formed version-0 list, served back as the
/// first row of a history: the version-column guard on its own (the anchor version, the anchor
/// bytes and the head check all see a match).
#[test]
fn a_zeroed_cache_row_anchors_nothing() {
    let mut w = world();
    cache_v2(&mut w);
    let v1 = w.v1.clone();
    let mut v0 = v1.clone();
    v0.unsigned.version = 0;
    v0.sig_ssk = SskSigner::from_bytes(&[0xf2; 32]).sign_device_list(&v0.unsigned);
    let next = sec_next(&v0, v1.unsigned.entries.clone(), 0xf2);
    w.alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_device_lists SET version=0,list=?1 WHERE user_id=?2",
            rusqlite::params![v0.encode(), w.bob.user.as_slice()],
        )
        .unwrap();
    let before = cached(&w);
    assert_eq!(
        ingest(&mut w, BOB, &[&v0, &next]),
        Err("E_DEVICE_LIST_STALE")
    );
    assert_eq!(cached(&w), before);
}

// 3. Rollback.

#[test]
fn a_history_whose_head_is_below_the_cached_version_is_refused() {
    let mut w = world();
    let before = cache_v2(&mut w);
    let (v1, v2) = (w.v1.clone(), w.v2.clone());
    assert_eq!(ingest(&mut w, BOB, &[&v1]), Err("E_DEVICE_LIST_STALE"));
    // The cached v2 as the first row, a head below it.
    assert_eq!(
        ingest_body(&mut w, BOB, &sec_history(&[&v2]), &v1),
        Err("E_DEVICE_LIST_STALE")
    );
    assert_eq!(cached(&w), Some(before));
}

/// The equal boundary: a refresh that finds nothing new (head at the cached version, the slice of
/// one row) is accepted and changes nothing.
#[test]
fn a_head_exactly_at_the_cached_version_is_accepted() {
    let mut w = world();
    let before = cache_v2(&mut w);
    let v2 = w.v2.clone();
    assert_eq!(ingest(&mut w, BOB, &[&v2]), Ok((2, 0, 3)));
    assert_eq!(ingest(&mut w, BOB2, &[&v2]), Ok((2, 0, 3)));
    assert_eq!(cached(&w), Some(before));
}

// 5. A head not signed by the leaf's key.

#[test]
fn a_head_the_leafs_ssk_did_not_sign_is_refused_also_when_it_is_the_cached_anchor() {
    let mut w = world();
    let (v1, v2) = (w.v1.clone(), w.v2.clone());
    // No cache: the chain is checked under the leaf's SSK.
    assert_eq!(ingest(&mut w, OTHER, &[&v1, &v2]), Err("E_CREDENTIAL"));
    let before = cache_v2(&mut w);
    // The single-row slice: the loop over newer rows has nothing to do, so the head's own
    // signature check against the leaf's SSK carries the whole weight.
    assert_eq!(ingest(&mut w, OTHER, &[&v2]), Err("E_CREDENTIAL"));
    assert_eq!(cached(&w), Some(before));
}

// Vacuous input.

#[test]
fn an_empty_history_is_refused_with_and_without_a_cached_list() {
    let mut w = world();
    let mut empty = Encoder::new();
    empty.array(0);
    let empty = empty.into_vec();
    let v2 = w.v2.clone();
    assert_eq!(
        ingest_body(&mut w, BOB2, &empty, &v2),
        Err("E_DEVICE_LIST_STALE")
    );
    assert!(cached(&w).is_none());
    let before = cache_v2(&mut w);
    assert_eq!(
        ingest_body(&mut w, BOB, &empty, &v2),
        Err("E_DEVICE_LIST_STALE")
    );
    assert_eq!(cached(&w), Some(before));
}

// The held row's reason after the verdict.

/// A row held for the list and re-labelled E_DEVICE_UNLISTED when its leaf was judged is still
/// held, not retracted: once a later signed list names the device, it is released.
#[test]
fn a_held_row_labelled_unlisted_is_released_once_a_later_list_names_the_device() {
    let mut w = world();
    cache_v2(&mut w);
    assert_rogue_unreadable(&w);
    let v2 = w.v2.clone();
    let mut entries = v2.unsigned.entries.clone();
    entries.push(sec_entry(&w.rogue));
    let v3 = sec_next(&v2, entries, 0xf2);
    assert_eq!(ingest(&mut w, ROGUE, &[&v2, &v3]), Ok((3, 0, 2)));
    assert_eq!(revalidate(&mut w, ROGUE), 1);
    let row = w.alice.row_at(&GROUP, w.rogue_seq);
    assert_eq!((row.status, row.body.as_str()), (0, "rogue comet"));
}

/// The re-label is the judged leaf's only: a sibling's row held for the same list, whose leaf the
/// same ingest found valid, keeps waiting for its own revalidation.
#[test]
fn only_the_judged_leafs_held_rows_take_its_verdict() {
    let mut w = world();
    cache_v2(&mut w);
    let bob2_row = w.alice.row_at(&GROUP, w.bob2_seq);
    assert_eq!(
        (bob2_row.status, bob2_row.reason.as_str()),
        (1, "E_DEVICE_LIST_STALE"),
        "a valid leaf's held row waits for its revalidation, not a failure"
    );
    assert_eq!(revalidate(&mut w, BOB2), 1);
    assert_bob2_readable(&w);
    assert_rogue_unreadable(&w);
}

fn retained(w: &World, seq: u64) -> i64 {
    w.alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT count(*) FROM app_pending_history WHERE group_id=?1 AND seq=?2",
            rusqlite::params![GROUP.as_slice(), seq as i64],
            |r| r.get(0),
        )
        .unwrap()
}

/// A device the sender revoked (its entry carries `revoked_at`) is not a device the sender never
/// added: its held message takes E_DEVICE_REVOKED, its retained evidence is dropped, and it never
/// opens, not even if a later list names the device unrevoked. A message it sends after the
/// revocation is refused the same way and not retained.
#[test]
fn a_revoked_devices_held_message_says_revoked_and_never_opens() {
    let mut w = world();
    let (v1, v2) = (w.v1.clone(), w.v2.clone());
    let mut entries = v2.unsigned.entries.clone();
    assert_eq!(entries[1].device_id.as_bytes(), &BOB2);
    entries[1].revoked_at = Some(NOW + 4);
    let v3 = sec_next(&v2, entries, 0xf2);
    assert_eq!(ingest(&mut w, BOB, &[&v1, &v2, &v3]).map(|v| v.0), Ok(3));
    let row = w.alice.row_at(&GROUP, w.bob2_seq);
    assert_eq!(
        (row.status, row.reason.as_str(), row.body.as_str()),
        (1, "E_DEVICE_REVOKED", "")
    );
    assert_eq!(
        retained(&w, w.bob2_seq),
        0,
        "a revoked device's held message is final"
    );
    assert_eq!(revalidate(&mut w, BOB2), 0);
    assert_eq!(ingest(&mut w, BOB2, &[&v3]), Err("E_DEVICE_REVOKED"));

    let after = w.bob2.send(&mut w.relay, "after the revocation");
    w.alice.sync(&w.relay);
    let row = w.alice.row_at(&GROUP, after);
    assert_eq!(
        (row.status, row.reason.as_str(), row.body.as_str()),
        (1, "E_DEVICE_REVOKED", "")
    );
    assert_eq!(retained(&w, after), 0);

    // A later list that names the device unrevoked: the leaf validates again, the old rows stay shut.
    let mut entries = v3.unsigned.entries.clone();
    entries[1].revoked_at = None;
    let v4 = sec_next(&v3, entries, 0xf2);
    assert_eq!(ingest(&mut w, BOB2, &[&v3, &v4]).map(|v| v.0), Ok(4));
    assert_eq!(revalidate(&mut w, BOB2), 0);
    for seq in [w.bob2_seq, after] {
        let row = w.alice.row_at(&GROUP, seq);
        assert_eq!((row.status, row.reason.as_str()), (1, "E_DEVICE_REVOKED"));
    }
}

/// v3: v2 with `device` named revoked (added to it first when v2 does not name it).
fn revoking(v2: &DeviceList, device: &RawPeer) -> DeviceList {
    let mut entries = v2.unsigned.entries.clone();
    if !entries
        .iter()
        .any(|e| e.device_id.as_bytes() == &device.device)
    {
        entries.push(sec_entry(device));
    }
    for entry in entries
        .iter_mut()
        .filter(|e| e.device_id.as_bytes() == &device.device)
    {
        entry.revoked_at = Some(NOW + 4);
    }
    sec_next(v2, entries, 0xf2)
}

/// A row held as unlisted (its device was never on the list) whose device a later list then names
/// revoked: the row takes E_DEVICE_REVOKED and keeps no evidence (the unlisted-then-revoked branch).
#[test]
fn a_row_held_unlisted_takes_revoked_when_a_later_list_revokes_its_device() {
    let mut w = world();
    cache_v2(&mut w);
    assert_rogue_unreadable(&w);
    assert_eq!(
        retained(&w, w.rogue_seq),
        1,
        "an unlisted row keeps its evidence"
    );
    let v2 = w.v2.clone();
    let v3 = revoking(&v2, &w.rogue);
    assert_eq!(ingest(&mut w, BOB, &[&v2, &v3]).map(|v| v.0), Ok(3));
    let row = w.alice.row_at(&GROUP, w.rogue_seq);
    assert_eq!(
        (row.status, row.reason.as_str(), row.body.as_str()),
        (1, "E_DEVICE_REVOKED", "")
    );
    assert_eq!(retained(&w, w.rogue_seq), 0);
    assert_eq!(revalidate(&mut w, ROGUE), 0);
}

/// A row held under any other releasable reason (a changed key, a failed credential, a tier
/// mismatch) whose device is then revoked: it takes E_DEVICE_REVOKED with its evidence gone, never
/// a releasable reason without evidence ("Compare the number" forever). Reaching each reason
/// through real keys needs a key change and its verification first; the held row's stored state is
/// what the revocation acts on, so it is set directly.
#[test]
fn a_row_held_for_any_releasable_reason_takes_revoked_when_its_device_is_revoked() {
    for reason in ["E_UMK_CHANGED", "E_CREDENTIAL", "E_TIER_MISMATCH"] {
        let mut w = world();
        w.alice
            .probe
            .lock()
            .unwrap()
            .execute(
                "UPDATE app_messages SET reason=?3 WHERE group_id=?1 AND seq=?2",
                rusqlite::params![GROUP.as_slice(), w.bob2_seq as i64, reason],
            )
            .unwrap();
        assert_eq!(retained(&w, w.bob2_seq), 1);
        let (v1, v2) = (w.v1.clone(), w.v2.clone());
        let v3 = revoking(&v2, &w.bob2);
        assert_eq!(ingest(&mut w, BOB, &[&v1, &v2, &v3]).map(|v| v.0), Ok(3));
        let row = w.alice.row_at(&GROUP, w.bob2_seq);
        assert_eq!(
            (row.status, row.reason.as_str(), row.body.as_str()),
            (1, "E_DEVICE_REVOKED", ""),
            "a row held as {reason}"
        );
        assert_eq!(retained(&w, w.bob2_seq), 0, "a row held as {reason}");
    }
}

// Bot v2 Task 16: membership trust from pushed device lists. A Bot's cache changes only by a
// push (op 24), so `pending_leaves` reports a leaf only when its user's list (or pinned key)
// changed since that leaf was last judged, plus leaves whose user has no list yet. It never
// reports `ready-refresh` for a Bot: a current leaf costs nothing.
mod pushed {
    use super::*;
    use dilla_core::bot::device_list_push::{DeviceListPush, DeviceListRow};
    use dilla_core::client::ClientCore;
    use dilla_core::identity::{DeviceEntry, DeviceListUnsigned, Tier, UmkSigner};
    use dilla_core::ids::{DeviceId, UserId};
    use std::sync::Arc;

    const INSTALLATION: [u8; 16] = [0x5c; 16];
    const BOT_USER: [u8; 16] = [0xe0; 16];

    /// A Bot core (bootstrap, then completion as `BOT_USER`): its own credential's kind is Bot.
    fn bot_core() -> Core {
        let conn = memory();
        let probe = Arc::clone(&conn);
        let mut core = ClientCore::open(conn).expect("open");
        core.bot_bootstrap_begin(&INSTANCE, &COMMUNITY, &INSTALLATION, &[0x77; 32])
            .expect("bot bootstrap");
        core.bot_bootstrap_complete(&BOT_USER, "music-bot", NOW)
            .expect("bot completion");
        let identity = core.identity().expect("identity");
        let device = decode_strict(&identity, |d| {
            d.array(6)?;
            d.uint()?;
            d.opt_bytes_exact::<16>()?;
            d.opt_bytes_exact::<16>()?;
            let device = d.opt_bytes_exact::<16>()?;
            d.text()?;
            d.uint()?;
            Ok(device)
        })
        .expect("identity shape")
        .expect("a device id");
        Core {
            core,
            probe,
            user: BOT_USER,
            device,
        }
    }

    fn ssk_of(peer: &RawPeer) -> SskSigner {
        SskSigner::from_bytes(&[peer.user[0].wrapping_add(0x40); 32])
    }

    /// The user's signed v1 list naming `peer`'s device, under the keys `RawPeer::new` derives.
    fn v1_of(peer: &RawPeer) -> DeviceList {
        let unsigned = DeviceListUnsigned {
            v: 1,
            user_id: UserId::from_bytes(peer.user),
            version: 1,
            prev_hash: [0; 32],
            entries: vec![DeviceEntry {
                device_id: DeviceId::from_bytes(peer.device),
                dsk_pub: peer.signer.public().try_into().unwrap(),
                tier: Tier::Native,
                added_at: NOW,
                revoked_at: None,
            }],
        };
        DeviceList {
            sig_ssk: ssk_of(peer).sign_device_list(&unsigned),
            unsigned,
        }
    }

    /// dillad's op 65 device_list push of `lists` for `peer`'s user.
    fn push_of(peer: &RawPeer, lists: &[&DeviceList]) -> Vec<u8> {
        let umk = UmkSigner::from_bytes(&[peer.user[0]; 32]);
        let ssk_pub = ssk_of(peer).public();
        DeviceListPush {
            umk_pub: umk.public(),
            ssk_pub,
            sig_umk_ssk: umk.sign_ssk(&ssk_pub),
            history: lists
                .iter()
                .map(|l| DeviceListRow {
                    version: l.unsigned.version,
                    blob: l.encode(),
                    ssk_signature: l.sig_ssk,
                    prev_hash: l.unsigned.prev_hash,
                })
                .collect(),
        }
        .encode()
    }

    type Pending = Vec<([u8; 16], u64, [u8; 16], u64, String)>;

    fn pending(core: &Core) -> Pending {
        let rows = core.core.pending_leaves().expect("pending leaves");
        decode_strict(&rows, |d| {
            let n = d.array_len()?;
            let mut out = Vec::new();
            for _ in 0..n {
                d.array(5)?;
                out.push((
                    d.bytes_exact::<16>()?,
                    d.uint()?,
                    d.bytes_exact::<16>()?,
                    d.uint()?,
                    d.text()?.to_owned(),
                ));
            }
            Ok(out)
        })
        .expect("pending leaves shape")
    }

    struct Fleet {
        bot: Core,
        peers: Vec<RawPeer>,
        lists: Vec<DeviceList>,
    }

    /// A Bot channel group of 50 users (one device each). Every user's v1 list is pushed and
    /// each leaf is judged once (`revalidate_pending`, what `trust.Changed` calls per leaf).
    fn fleet() -> Fleet {
        let instance = Instance::generate();
        let mut relay = Relay::new(GROUP);
        let mut bot = bot_core();
        bot.create_and_register(&mut relay, &instance);
        let mut peers = Vec::new();
        for i in 0..50u8 {
            let mut peer = RawPeer::new(0x10 + i, 0x60 + i);
            peer.join_external(&mut relay);
            peers.push(peer);
        }
        bot.sync(&relay);
        let before = pending(&bot);
        assert_eq!(before.len(), 50, "every member's leaf, not the Bot's own");
        assert!(before.iter().all(|r| r.4 == "unknown"), "{before:?}");
        let lists: Vec<DeviceList> = peers.iter().map(v1_of).collect();
        for (peer, list) in peers.iter().zip(&lists) {
            bot.core
                .ingest_device_list(&peer.user, &push_of(peer, &[list]), NOW + 1)
                .expect("first push pins and caches");
        }
        for (group, leaf, ..) in pending(&bot) {
            bot.core
                .revalidate_pending(&group, leaf as u32, 256)
                .expect("judge the leaf");
        }
        Fleet { bot, peers, lists }
    }

    #[test]
    fn pending_leaves_omits_current_leaves() {
        let f = fleet();
        assert_eq!(
            pending(&f.bot),
            Vec::new(),
            "50 members whose lists did not change since their leaves were judged: nothing to do"
        );
    }

    #[test]
    fn pending_leaves_reports_only_the_changed_user() {
        let mut f = fleet();
        let peer = &f.peers[7];
        let v1 = f.lists[7].clone();
        let v2 = sec_next(
            &v1,
            v1.unsigned.entries.clone(),
            peer.user[0].wrapping_add(0x40),
        );
        f.bot
            .core
            .ingest_device_list(&peer.user, &push_of(peer, &[&v1, &v2]), NOW + 2)
            .expect("a newer list");
        let rows = pending(&f.bot);
        assert_eq!(rows.len(), 1, "only the changed user's leaf: {rows:?}");
        assert_eq!(
            (rows[0].2, rows[0].3, rows[0].4.as_str()),
            (peer.user, 2, "changed")
        );
        f.bot
            .core
            .revalidate_pending(&rows[0].0, rows[0].1 as u32, 256)
            .expect("judge it again");
        assert_eq!(
            pending(&f.bot),
            Vec::new(),
            "judged under v2: current again"
        );
    }

    #[test]
    fn a_revoked_device_is_judged_on_its_users_push_and_leaves_others_current() {
        let mut f = fleet();
        let peer = &f.peers[3];
        let v1 = f.lists[3].clone();
        let mut entries = v1.unsigned.entries.clone();
        entries[0].revoked_at = Some(NOW + 2);
        let v2 = sec_next(&v1, entries, peer.user[0].wrapping_add(0x40));
        f.bot
            .core
            .ingest_device_list(&peer.user, &push_of(peer, &[&v1, &v2]), NOW + 2)
            .expect("a revocation");
        let rows = pending(&f.bot);
        assert_eq!(rows.len(), 1, "{rows:?}");
        f.bot
            .core
            .revalidate_pending(&rows[0].0, rows[0].1 as u32, 256)
            .expect("judge it");
        let code: String = f
            .bot
            .probe
            .lock()
            .unwrap()
            .query_row(
                "SELECT code FROM app_quarantine WHERE group_id=?1 AND user_id=?2",
                [GROUP.as_slice(), peer.user.as_slice()],
                |r| r.get(0),
            )
            .expect("the revoked leaf is quarantined");
        assert_eq!(code, "E_DEVICE_REVOKED");
        assert_eq!(pending(&f.bot), Vec::new());
    }

    #[test]
    fn a_person_still_refreshes_current_leaves_on_ready() {
        // A person's cache changes only when it reads a list, so its current leaves stay
        // `ready-refresh`: only a Bot's pushed cache omits them.
        let mut w = world();
        cache_v2(&mut w);
        let rows = pending(&w.alice);
        assert!(rows.iter().any(|r| r.4 == "ready-refresh"), "{rows:?}");
    }

    /// Fix round 1, M-4: a judgement is bound to the leaf's credential. A row recorded for
    /// another credential at the same leaf and signature key (one changed by an Update) does
    /// not make the leaf current: it is judged again.
    #[test]
    fn a_leaf_judged_under_another_credential_is_reported_again() {
        let f = fleet();
        let peer = &f.peers[5];
        let changed = f
            .bot
            .probe
            .lock()
            .unwrap()
            .execute(
                "UPDATE app_leaf_checked SET credential_hash=zeroblob(32) WHERE user_id=?1",
                [peer.user.as_slice()],
            )
            .unwrap();
        assert_eq!(changed, 1);
        let rows = pending(&f.bot);
        assert_eq!(rows.len(), 1, "{rows:?}");
        assert_eq!((rows[0].2, rows[0].4.as_str()), (peer.user, "changed"));
    }

    /// Fix round 1, M-4: a store with the first Task 16 table (no credential hash) gets the
    /// new table on open; the old rows go, so their leaves are judged again.
    #[test]
    fn a_first_shape_leaf_checked_table_is_replaced_on_open() {
        let bot = bot_core();
        {
            let db = bot.probe.lock().unwrap();
            db.execute_batch(
                "DROP TABLE app_leaf_checked;
                 CREATE TABLE app_leaf_checked(group_id BLOB NOT NULL,leaf_index INTEGER NOT NULL,signature_key BLOB NOT NULL,user_id BLOB NOT NULL,umk_pub BLOB NOT NULL,version INTEGER NOT NULL,PRIMARY KEY(group_id,leaf_index)) WITHOUT ROWID;
                 INSERT INTO app_leaf_checked VALUES(zeroblob(16),1,zeroblob(32),zeroblob(16),zeroblob(32),1);",
            )
            .unwrap();
        }
        let bot = bot.reopen();
        let db = bot.probe.lock().unwrap();
        let has_hash: i64 = db
            .query_row(
                "SELECT count(*) FROM pragma_table_info('app_leaf_checked') WHERE name='credential_hash'",
                [],
                |r| r.get(0),
            )
            .unwrap();
        let rows: i64 = db
            .query_row("SELECT count(*) FROM app_leaf_checked", [], |r| r.get(0))
            .unwrap();
        assert_eq!((has_hash, rows), (1, 0));
    }
}
