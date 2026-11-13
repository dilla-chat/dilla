//! Call groups in the browser core: kind-aware holders, the media epoch and its send-block gate.
// Native only, like tests/client_trust.rs: every test opens an in-memory SQLite connection.
#![cfg(not(target_arch = "wasm32"))]
mod client_support;
use client_support::*;
use dilla_core::cbor::decode_strict;
use dilla_core::identity::UmkSigner;

/// A plain text message request (`client_support::request` with type 0, no reply, no attachment).
fn request(body: &str) -> Vec<u8> {
    client_support::request(0, None, body, &[])
}

#[test]
fn a_dm_call_group_never_collides_with_its_text_group() {
    let instance = Instance::generate();
    let mut text = Relay::new(GROUP);
    let mut call = Relay::new(CALL);
    let mut alice = ready_core(0xa1, "alice");
    alice.create_and_register_in(&mut text, &instance, None, &DM);
    alice.call_create_and_register(&mut call, &instance, None, &DM);
    assert_eq!(alice.group(&GROUP).unwrap().kind, 0);
    assert_eq!(alice.group(&CALL).unwrap().kind, 1);
    assert!(alice.timeline(&CALL).is_empty());
    let id = alice.prepare_request(&GROUP, &request("hello"), NOW);
    assert_eq!(id.len(), 16);
}

#[test]
fn the_media_epoch_is_the_exporter_key_roster_and_six_by_five_code() {
    let (call, alice, bob, _) = call_raw_bob();
    let m = decode_media_epoch(&alice.core.call_media_epoch(&CALL).expect("media epoch"));
    assert_eq!(
        m.base_key,
        bob.group
            .as_ref()
            .unwrap()
            .sframe_base_key(&bob.provider)
            .unwrap()
    );
    assert_eq!(m.roster.len(), 2);
    assert_eq!(
        m.code.split(' ').map(str::len).collect::<Vec<_>>(),
        vec![5; 6]
    );
    let _ = call;
}

/// Attacker: a malicious instance (or a member colluding with it) adds a leaf under a key no
/// validated list presents, so it would receive this device's call audio. The core reads the
/// verdict from its own live tree; neither the worker nor the instance can skip the check.
#[test]
fn no_media_key_while_a_leaf_of_the_call_group_fails_validation() {
    let (mut call, mut alice, bob, v1) = call_raw_bob();
    assert!(alice.core.call_media_epoch(&CALL).is_ok());
    let mut forged = RawPeer::new_with_keys(0xb2, 0xd4, 0xb2, 0xf2);
    forged.join_external_call(&mut call);
    alice.sync(&call);
    assert_eq!(
        code(alice.core.call_media_epoch(&CALL)),
        "E_DEVICE_UNLISTED"
    );
    let v2 = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&forged)],
        0xf2,
    );
    call_ingest(&mut alice, &call, &bob.user, forged.device, &[&v1, &v2]);
    assert!(alice.core.call_media_epoch(&CALL).is_ok());
}

#[test]
fn no_media_key_after_a_changed_identity_joins_the_call() {
    let (mut call, mut alice, bob, v1) = call_raw_bob();
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external_call(&mut call);
    alice.sync(&call);
    let v2 = sec_next(&v1, vec![sec_entry(&rotated)], 0xd6);
    call_ingest(&mut alice, &call, &bob.user, rotated.device, &[&v1, &v2]);
    assert_eq!(code(alice.core.call_media_epoch(&CALL)), "E_UMK_CHANGED");
}

/// Attacker: an instance serving a GroupInfo and tree that already holds a forged leaf. The join
/// fails before an external commit exists, so the forged leaf never gets an epoch this device enters.
#[test]
fn joining_a_call_group_that_already_holds_a_failed_leaf_is_refused_before_any_commit() {
    let instance = Instance::generate();
    let mut call = Relay::new(CALL);
    let mut bob = RawPeer::new(0xb2, 0xb3);
    bob.create_call(&mut call, &instance);
    let mut forged = RawPeer::new_with_keys(0xb2, 0xd4, 0xb2, 0xf2);
    forged.join_external_call(&mut call);
    let mut alice = ready_core(0xa1, "alice");
    pin_list(&mut alice, &bob);
    let before = call.handshakes_from(0).len();
    let r = alice.core.call_group_join_external(
        &CALL,
        Some(&COMMUNITY),
        &VOICE,
        POLICY,
        &call.info_body(),
        &call.tree_body(),
    );
    assert_eq!(code(r), "E_DEVICE_UNLISTED");
    assert!(alice.group(&CALL).is_none());
    assert_eq!(call.handshakes_from(0).len(), before);
}

/// L-CORE-52: a call group carries no messages. An application message a member uploads to it is
/// skipped (never decrypted, stored or folded) while `next_seq` moves past it, and sending to it
/// is refused before anything is queued.
#[test]
fn a_call_group_skips_messages_and_refuses_sending() {
    let (mut call, mut alice, mut bob, _) = call_raw_bob();
    let seq = bob.send(&mut call, "not a call payload");
    let applied = alice.sync(&call);
    assert!(applied.new_seqs.is_empty(), "nothing listed: {applied:?}");
    assert_eq!(applied.next_seq, seq + 1);
    assert!(alice.timeline(&CALL).is_empty());
    assert_eq!(
        code(alice.core.send_prepare(&CALL, &request("hello"), NOW)),
        "E_CORE_INPUT"
    );
    assert!(alice.outbox(&CALL).is_empty());
}

#[test]
fn leave_rotate_and_forget() {
    let (mut call, mut alice, _bob, _) = call_raw_bob();
    let p = alice.core.call_leave_proposal(&CALL).expect("proposal");
    assert_eq!(decode_epoch_proposal(&p).0, 1);
    let c = alice.core.call_rotate(&CALL).expect("rotate");
    call.commit(&c)
        .expect("the DS accepts an empty commit with an UpdatePath");
    alice.core.commit_confirm(&CALL).expect("confirm");
    alice.core.call_group_forget(&CALL).expect("forget");
    assert_eq!(alice.group(&CALL).unwrap().state, 4);
    assert_eq!(code(alice.core.call_media_epoch(&CALL)), "E_CORE_STATE");
}

/// Attacker: an instance that adds a leaf for a user whose signed list this device never fetched, hoping the pending verdict lets audio through.
#[test]
fn no_media_key_while_a_leaf_is_pending() {
    let (mut call, mut alice, _bob, _) = call_raw_bob();
    let mut carol = RawPeer::new(0xc3, 0xc4);
    carol.join_external_call(&mut call);
    alice.sync(&call);
    assert_eq!(
        code(alice.core.call_media_epoch(&CALL)),
        "E_DEVICE_LIST_STALE"
    );
    ingest_list_body(
        &mut alice,
        &call,
        carol.user,
        carol.device,
        &carol.device_list_body(),
    );
    assert!(alice.core.call_media_epoch(&CALL).is_ok());
}

/// Ruling RV-1's join path: a join over a tree with a pending leaf is staged and returns its
/// external commit unposted; the list refresh runs against the staged group (its leaf is listed by
/// `pending_leaves`), and only `call_join_check` passing lets the worker post it.
#[test]
fn a_staged_join_with_a_pending_leaf_posts_nothing_until_checked() {
    let instance = Instance::generate();
    let mut call = Relay::new(CALL);
    let mut bob = RawPeer::new(0xb2, 0xb3);
    bob.create_call(&mut call, &instance);
    let mut carol = RawPeer::new(0xc3, 0xc4);
    carol.join_external_call(&mut call);
    let mut alice = ready_core(0xa1, "alice");
    pin_list(&mut alice, &bob);
    let before = call.handshakes_from(0);
    let commit = alice
        .core
        .call_group_join_external(
            &CALL,
            Some(&COMMUNITY),
            &VOICE,
            POLICY,
            &call.info_body(),
            &call.tree_body(),
        )
        .expect("a pending leaf is not a hard verdict: the join is staged");
    assert_eq!(call.handshakes_from(0), before, "nothing is posted");
    assert_eq!(alice.group(&CALL).unwrap().state, 1, "staged");
    let pending = decode_strict(&alice.core.pending_leaves().expect("pending"), |d| {
        let n = d.array_len()?;
        let mut rows = Vec::new();
        for _ in 0..n {
            d.array(5)?;
            let group = d.bytes_exact::<16>()?;
            let leaf = d.uint()?;
            let user = d.bytes_exact::<16>()?;
            d.uint()?;
            d.text()?;
            rows.push((group, leaf, user));
        }
        Ok(rows)
    })
    .expect("pending_leaves shape");
    assert!(
        pending.contains(&(CALL, u64::from(call.leaf_of(carol.device)), carol.user)),
        "the staged call group's pending leaf is listed for the refresh: {pending:?}"
    );
    assert_eq!(
        code(alice.core.call_join_check(&VOICE)),
        "E_CORE_NOT_FOUND",
        "the channel ID is not the staged MLS group ID"
    );
    assert_eq!(
        code(alice.core.call_join_check(&CALL)),
        "E_DEVICE_LIST_STALE"
    );
    ingest_list_body(
        &mut alice,
        &call,
        carol.user,
        carol.device,
        &carol.device_list_body(),
    );
    alice
        .core
        .call_join_check(&CALL)
        .expect("every leaf validates");
    let answer = call
        .resync(&commit)
        .expect("the DS accepts the external commit");
    let seq = decode_strict(&answer, |d| {
        d.array(2)?;
        let seq = d.uint()?;
        d.uint()?;
        Ok(seq)
    })
    .expect("200 body");
    alice.core.group_joined(&CALL, seq).expect("group_joined");
    assert_eq!(code(alice.core.call_join_check(&CALL)), "E_CORE_STATE");
}

/// Alice holds the DM text group GROUP on DM with Bob, whose identity changed there (a leaf of a
/// new UMK joined and its list was ingested): the text group is send-blocked `E_UMK_CHANGED`.
fn alice_with_a_blocked_dm_text_group() -> (Instance, Core) {
    let instance = Instance::generate();
    let mut text = Relay::new(GROUP);
    let mut alice = ready_core(0xa1, "alice");
    alice.create_and_register_in(&mut text, &instance, None, &DM);
    let mut bob = RawPeer::new(0xb2, 0xb3);
    bob.join_external_as(&mut text, &dm_binding(DM));
    alice.sync(&text);
    let body = bob.device_list_body();
    ingest_list_body(&mut alice, &text, bob.user, bob.device, &body);
    let v1 = list_of(&body);
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external_as(&mut text, &dm_binding(DM));
    alice.sync(&text);
    let v2 = sec_next(&v1, vec![sec_entry(&rotated)], 0xd6);
    call_ingest(&mut alice, &text, &bob.user, rotated.device, &[&v1, &v2]);
    assert_eq!(
        alice.core.send_block_code(&GROUP).expect("send gate"),
        Some("E_UMK_CHANGED")
    );
    (instance, alice)
}

/// Attacker: a contact whose identity changed calls in our DM; decision 4 refuses the call key in the core, not only in the worker.
#[test]
fn a_dm_call_key_refuses_while_the_dm_text_group_is_blocked() {
    let (instance, mut alice) = alice_with_a_blocked_dm_text_group();
    let mut call = Relay::new(CALL);
    alice.call_create_and_register(&mut call, &instance, None, &DM);
    assert_eq!(alice.core.send_block_code(&CALL).expect("send gate"), None);
    assert_eq!(code(alice.core.call_media_epoch(&CALL)), "E_UMK_CHANGED");

    // A voice channel's call group (bound to a community, its own target): the text-group rule
    // applies to DMs only.
    let (instance, mut alice) = alice_with_a_blocked_dm_text_group();
    let mut voice = Relay::new(CALL);
    alice.call_create_and_register(&mut voice, &instance, Some(&COMMUNITY), &VOICE);
    assert!(alice.core.call_media_epoch(&CALL).is_ok());
}

/// Attacker (review M3): whoever creates a DM's call group binds it to a made-up community, hoping
/// the core then treats it as a voice channel and skips the DM's blocked text group. The core
/// finds the DM by the text group it holds for the target, whatever the call binding says, and
/// refuses a call group whose community disagrees with that text group's.
#[test]
fn a_dm_call_group_bound_to_a_made_up_community_is_refused() {
    let (instance, mut alice) = alice_with_a_blocked_dm_text_group();
    let mut call = Relay::new(CALL);
    alice.call_create_and_register(&mut call, &instance, Some(&COMMUNITY), &DM);
    assert_eq!(code(alice.core.call_media_epoch(&CALL)), "E_BINDING");

    // The same disagreement with a text group that is not blocked: still refused.
    let instance = Instance::generate();
    let mut text = Relay::new(GROUP);
    let mut call = Relay::new(CALL);
    let mut alice = ready_core(0xa1, "alice");
    alice.create_and_register_in(&mut text, &instance, None, &DM);
    alice.call_create_and_register(&mut call, &instance, Some(&COMMUNITY), &DM);
    assert_eq!(code(alice.core.call_media_epoch(&CALL)), "E_BINDING");
}

/// Web-3 task 10, concern 3: a voice join refused because a contact's identity changed names that
/// contact, whether or not they ever entered the room: the refusal's detail is the user id (hex) of
/// the tree's leaf whose pin holds a changed key, so the worker names them (`voice.blockedChanged`)
/// and offers the join again once that contact is verified.
#[test]
fn a_join_refused_for_a_changed_identity_names_the_changed_contact() {
    let (instance, mut alice) = alice_with_a_blocked_dm_text_group();
    let mut voice = Relay::new(CALL);
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.create_as(&mut voice, &instance, &call_binding(Some(COMMUNITY), VOICE));
    let e = alice
        .core
        .call_group_join_external(
            &CALL,
            Some(&COMMUNITY),
            &VOICE,
            POLICY,
            &voice.info_body(),
            &voice.tree_body(),
        )
        .expect_err("a changed identity refuses the join");
    assert_eq!(e.code, "E_UMK_CHANGED");
    let hex: String = rotated.user.iter().map(|b| format!("{b:02x}")).collect();
    assert_eq!(e.detail, hex);
    assert!(alice.group(&CALL).is_none());
}

/// Attacker (review M2): a contact whose identity changed starts a call in our DM. Joining its call
/// group is refused in the join's own unit (decision 4), before any external commit exists, also
/// when the worker would skip `call_join_check`.
#[test]
fn joining_a_dm_call_while_the_dm_text_group_is_blocked_is_refused_before_any_commit() {
    let (instance, mut alice) = alice_with_a_blocked_dm_text_group();
    let mut call = Relay::new(CALL);
    let mut bob = RawPeer::new(0xb2, 0xb3);
    bob.create_as(&mut call, &instance, &call_binding(None, DM));
    let before = call.handshakes_from(0);
    let r = alice.core.call_group_join_external(
        &CALL,
        None,
        &DM,
        POLICY,
        &call.info_body(),
        &call.tree_body(),
    );
    assert_eq!(code(r), "E_UMK_CHANGED");
    assert!(alice.group(&CALL).is_none());
    assert_eq!(call.handshakes_from(0), before);
}

/// Review M5: forgetting a call group deletes its stored verdict and audit rows with its MLS
/// state, so the per-call groups RV-2 creates leave no trust rows behind.
#[test]
fn forget_deletes_the_call_groups_verdict_rows() {
    let (mut call, mut alice, _bob, _) = call_raw_bob();
    let mut forged = RawPeer::new_with_keys(0xb2, 0xd4, 0xb2, 0xf2);
    forged.join_external_call(&mut call);
    alice.sync(&call);
    let rows = |alice: &Core, table: &str| -> i64 {
        alice
            .probe
            .lock()
            .unwrap()
            .query_row(
                &format!("SELECT COUNT(*) FROM {table} WHERE group_id=?1"),
                [CALL.as_slice()],
                |r| r.get(0),
            )
            .unwrap()
    };
    assert_eq!(
        rows(&alice, "app_quarantine"),
        1,
        "the forged leaf's verdict"
    );
    assert_eq!(rows(&alice, "app_trust_audit"), 1, "its audit row");
    alice.core.call_group_forget(&CALL).expect("forget");
    for table in ["app_quarantine", "app_trust_audit", "app_pending_history"] {
        assert_eq!(
            rows(&alice, table),
            0,
            "{table} rows of the forgotten call group"
        );
    }
}

/// No key byte is derived before the gate passes: with a blocking verdict present the exporter is
/// never reached. The counter is inside `DillaGroup::sframe_base_key` itself (review I1), so it
/// counts derivations, not the line that calls one.
#[cfg(feature = "test-hooks")]
#[test]
fn the_media_epoch_bytes_are_gated_before_any_key_derivation() {
    let test_media_key_derivations = dilla_core::mls::DillaGroup::test_sframe_base_key_calls;
    let (mut call, mut alice, bob, v1) = call_raw_bob();
    let mut forged = RawPeer::new_with_keys(0xb2, 0xd4, 0xb2, 0xf2);
    forged.join_external_call(&mut call);
    alice.sync(&call);
    let before = test_media_key_derivations();
    assert_eq!(
        code(alice.core.call_media_epoch(&CALL)),
        "E_DEVICE_UNLISTED"
    );
    assert_eq!(
        test_media_key_derivations(),
        before,
        "no sframe_base_key call"
    );
    let v2 = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&forged)],
        0xf2,
    );
    call_ingest(&mut alice, &call, &bob.user, forged.device, &[&v1, &v2]);
    alice.core.call_media_epoch(&CALL).expect("media epoch");
    assert_eq!(
        test_media_key_derivations(),
        before + 1,
        "the counter sees a derivation once the gate passes"
    );
}

/// (pinned UMK, verified, pending changed UMK) of `user` in Alice's `trust_state`.
fn pin_of(alice: &Core, user: &[u8; 16]) -> Option<([u8; 32], u64, Option<[u8; 32]>)> {
    let state = alice.core.trust_state().expect("trust state answers");
    decode_strict(&state, |d| {
        let n = d.array_len()?;
        let mut found = None;
        for _ in 0..n {
            d.array(6)?;
            let candidate = d.bytes_exact::<16>()?;
            let umk = d.bytes_exact::<32>()?;
            d.uint()?;
            let verified = d.uint()?;
            let changed = d.opt_bytes_exact::<32>()?;
            d.skip()?;
            if candidate == *user {
                found = Some((umk, verified, changed));
            }
        }
        Ok(found)
    })
    .unwrap()
}

/// RV-3 (task-10 concern 1): Bob's identity changes while his old-key device is still in the
/// call group. The rotated device keeps Bob's SSK (a new UMK cross-signs it), so the one list that
/// names the new device also names the old one and verifies through either leaf. Refreshing that
/// same list through the old-key leaf is no "later list back under the pinned key": it must not
/// end the pending change, or the new-key leaf turns `E_CREDENTIAL` (blocked for good) and the
/// person loses the compare-and-verify path. Media stays paused until the new key is verified.
#[test]
fn an_old_key_refresh_of_the_same_list_keeps_a_pending_change_in_a_call() {
    let (mut call, mut alice, bob, v1) = call_raw_bob();
    let old_key = UmkSigner::from_bytes(&[0xb2; 32]).public();
    let new_key = UmkSigner::from_bytes(&[0xd5; 32]).public();
    // Same SSK (seed 0xf2) as Bob's first device, new UMK (seed 0xd5).
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xf2);
    rotated.join_external_call(&mut call);
    alice.sync(&call);
    let v2 = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&rotated)],
        0xf2,
    );
    let (_, changed, _) = call_ingest(&mut alice, &call, &bob.user, rotated.device, &[&v1, &v2]);
    assert_eq!(changed, 1);
    assert_eq!(pin_of(&alice, &bob.user), Some((old_key, 0, Some(new_key))));
    assert_eq!(code(alice.core.call_media_epoch(&CALL)), "E_UMK_CHANGED");

    // The worker refreshes the old-key leaf's list: the instance serves the cached head again.
    call_ingest(&mut alice, &call, &bob.user, bob.device, &[&v2]);
    assert_eq!(
        pin_of(&alice, &bob.user),
        Some((old_key, 0, Some(new_key))),
        "an old-key observation of the list that raised the change must not end it"
    );
    assert_eq!(
        code(alice.core.call_media_epoch(&CALL)),
        "E_UMK_CHANGED",
        "media stays paused on the changed key, not failed as an unknown one"
    );
    // The same refresh in the other order (new-key leaf again) changes nothing either.
    call_ingest(&mut alice, &call, &bob.user, rotated.device, &[&v2]);
    assert_eq!(pin_of(&alice, &bob.user), Some((old_key, 0, Some(new_key))));

    // The compare-and-verify path is still there, for the new key only.
    assert_eq!(
        code(alice.core.trust_verify(&bob.user, &old_key)),
        "E_DEVICE_LIST_STALE"
    );
    alice
        .core
        .trust_verify(&bob.user, &new_key)
        .expect("verify the changed key");
    assert_eq!(pin_of(&alice, &bob.user), Some((new_key, 1, None)));
}

/// The rotation's seq in a `POST /commit` answer `[seq, epoch]`.
fn seq_of_commit_answer(answer: &[u8]) -> u64 {
    decode_strict(answer, |d| {
        d.array(2)?;
        let seq = d.uint()?;
        d.uint()?;
        Ok(seq)
    })
    .expect("[seq, epoch]")
}

/// A raw peer's media key of its current epoch.
fn base_key_of(peer: &RawPeer) -> [u8; 16] {
    *peer
        .group
        .as_ref()
        .expect("joined")
        .media_epoch(&peer.provider)
        .expect("media epoch")
        .base_key
}

/// Screens wave 1 Task 3b, the gap it closes (the recorded result): a rotation the instance applied
/// while its answer was lost, then aborted by the client (the re-write after an instance restart is
/// refused 409 `E_COMMIT_CONFLICT`, which the old `rotate_call` took as "not applied"). Catch-up then
/// serves the client its own commit with no pending commit to match it against: the core cannot
/// process it, marks the group 3 (needs resync) and stays at the old epoch with its cursor before the
/// commit. No fork and no wedge (the engine resyncs a state-3 group by external commit), but the call
/// loses its keys until that rejoin: a degraded path `rotate_call` must not take.
#[test]
fn an_aborted_rotation_the_instance_applied_needs_a_resync() {
    let (mut call, mut alice, mut bob, _) = call_raw_bob();
    let rotation = alice.core.call_rotate(&CALL).expect("rotate");
    let seq = seq_of_commit_answer(&call.commit(&rotation).expect("the DS applies it"));
    bob.apply_commit(&call, seq);
    alice.core.commit_abort(&CALL).expect("abort");
    let applied = alice.sync(&call);
    assert_eq!((applied.state, applied.epoch), (3, 1));
    let row = alice.group(&CALL).unwrap();
    assert_eq!(
        (row.state, row.epoch, row.next_seq, row.pending_commit),
        (3, 1, seq, 0)
    );
    assert_eq!(code(alice.core.call_media_epoch(&CALL)), "E_CORE_STATE");
    assert_eq!(
        code(alice.try_sync(&call)),
        "E_CORE_STATE",
        "nothing more is applied"
    );

    // No wedge: the call's rejoin (the call controller's external commit; the engine never resyncs a call group)
    // brings Alice back to an active group with Bob's key.
    let rejoin = alice
        .core
        .call_group_join_external(
            &CALL,
            Some(&COMMUNITY),
            &VOICE,
            POLICY,
            &call.info_body(),
            &call.tree_body(),
        )
        .expect("the rejoin is built over the instance's tree");
    alice
        .core
        .call_join_check(&CALL)
        .expect("every leaf validates");
    let seq = seq_of_commit_answer(&call.resync(&rejoin).expect("the DS accepts the rejoin"));
    alice.core.group_joined(&CALL, seq).expect("group_joined");
    bob.apply_commit(&call, seq);
    assert_eq!(alice.group(&CALL).unwrap().state, 2);
    let media = decode_media_epoch(&alice.core.call_media_epoch(&CALL).expect("keys"));
    assert_eq!(
        media.base_key,
        base_key_of(&bob),
        "the peer's key after the rejoin"
    );
}

/// The reconciliation Task 3b's `rotate_call` relies on: with the pending rotation KEPT, catch-up
/// decides. Its own commit is adopted as the pending commit (confirmed at the new epoch, with the
/// peer's media key); another member's commit at that epoch clears the pending one and is applied,
/// and the next rotation builds from the new epoch.
#[test]
fn a_kept_rotation_is_settled_by_catch_up_whichever_commit_won() {
    let (mut call, mut alice, mut bob, _) = call_raw_bob();
    let rotation = alice.core.call_rotate(&CALL).expect("rotate");
    let seq = seq_of_commit_answer(&call.commit(&rotation).expect("the DS applies it"));
    bob.apply_commit(&call, seq);
    let applied = alice.sync(&call);
    assert_eq!((applied.state, applied.epoch), (2, 2));
    let row = alice.group(&CALL).unwrap();
    assert_eq!(
        (row.epoch, row.next_seq, row.pending_commit),
        (2, seq + 1, 0)
    );
    let media = decode_media_epoch(&alice.core.call_media_epoch(&CALL).expect("keys"));
    assert_eq!(media.epoch, 2);
    assert_eq!(
        media.base_key,
        base_key_of(&bob),
        "the peer's key of epoch 2"
    );

    // Bob's commit wins epoch 2 while Alice's rotation of epoch 2 is pending (never applied).
    let _lost = alice.core.call_rotate(&CALL).expect("rotate");
    bob.self_update(&mut call);
    let applied = alice.sync(&call);
    assert_eq!((applied.state, applied.epoch), (2, 3));
    assert_eq!(alice.group(&CALL).unwrap().pending_commit, 0);
    let media = decode_media_epoch(&alice.core.call_media_epoch(&CALL).expect("keys"));
    assert_eq!(
        media.base_key,
        base_key_of(&bob),
        "the peer's key of epoch 3"
    );
    let next = alice
        .core
        .call_rotate(&CALL)
        .expect("the next rotation builds");
    call.commit(&next).expect("from epoch 3");
}
