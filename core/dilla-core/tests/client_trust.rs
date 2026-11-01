#![cfg(not(target_arch = "wasm32"))]
mod client_support;
use client_support::*;
use dilla_core::cbor::decode_strict;

type Pending = ([u8; 16], u32, [u8; 16], u64, String);

fn pending(bytes: &[u8]) -> Vec<Pending> {
    decode_strict(bytes, |d| {
        let n = d.array_len()?;
        let mut out = Vec::new();
        for _ in 0..n {
            d.array(5)?;
            let group = d.bytes_exact::<16>()?;
            let leaf = u32::try_from(d.uint()?).expect("leaf index");
            out.push((
                group,
                leaf,
                d.bytes_exact::<16>()?,
                d.uint()?,
                d.text()?.to_owned(),
            ));
        }
        Ok(out)
    })
    .expect("pending leaves CBOR")
}

/// trust_state rows other than the own account's pin (seeded verified from the own credential).
fn contact_rows(core: &Core) -> usize {
    let state = core.core.trust_state().expect("state");
    decode_strict(&state, |d| {
        let n = d.array_len()?;
        let mut contacts = 0;
        for _ in 0..n {
            d.array(6)?;
            if d.bytes_exact::<16>()? != core.user {
                contacts += 1;
            }
            for _ in 0..5 {
                d.skip()?;
            }
        }
        Ok(contacts)
    })
    .expect("trust state CBOR")
}

#[test]
fn pending_leaves_are_core_owned_and_bounded() {
    let (_, relay, alice, bob) = alice_and_bob();
    let rows = pending(&alice.core.pending_leaves().expect("pending"));
    assert!(rows.len() <= 4096);
    assert!(
        rows.iter()
            .any(|r| r.0 == GROUP && r.2 == bob.user && r.1 == relay.leaf_of(bob.device))
    );
    assert!(
        rows.iter()
            .all(|r| ["ready-refresh", "unknown", "changed"].contains(&r.4.as_str()))
    );
}

#[test]
fn pending_leaf_group_ids_are_bounded_before_loading_group_state() {
    let (_, _, alice, _) = alice_and_bob();
    {
        let db = alice.probe.lock().unwrap();
        for n in 0..4097u32 {
            let mut group = [0x91u8; 16];
            group[12..].copy_from_slice(&n.to_be_bytes());
            db.execute(
                "INSERT INTO app_groups(group_id,kind,target_id,state) VALUES(?1,0,?2,2)",
                rusqlite::params![group.as_slice(), group.as_slice()],
            )
            .unwrap();
        }
    }
    assert_eq!(code(alice.core.pending_leaves()), "E_CORE_LIMIT");
}

#[test]
fn malformed_history_cannot_pin_a_real_current_leaf() {
    let (_, relay, mut alice, bob) = alice_and_bob();
    let leaf = relay.leaf_of(bob.device);
    let err = alice
        .core
        .device_list_ingest(&GROUP, leaf, &bob.user, &[0x80], &[0x80], NOW)
        .expect_err("empty history");
    assert!(["E_DEVICE_LIST_STALE", "E_CREDENTIAL"].contains(&err.code));
    assert_eq!(contact_rows(&alice), 0, "only the own pin");
}

#[test]
fn another_users_requested_id_never_creates_a_pin() {
    let (_, relay, mut alice, bob) = alice_and_bob();
    let wrong = [0xee; 16];
    let err = alice
        .core
        .device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &wrong,
            &[0x80],
            &[0x80],
            NOW,
        )
        .expect_err("wrong user");
    assert_eq!(err.code, "E_CREDENTIAL");
    assert_eq!(contact_rows(&alice), 0, "only the own pin");
}

#[test]
fn missing_shared_contact_has_no_safety_number() {
    let (_, _, alice, _) = alice_and_bob();
    assert_eq!(
        code(alice.core.safety_number_for(&[0xee; 16], &[0xee; 32])),
        "E_CORE_NOT_FOUND"
    );
}
use dilla_core::cbor::Encoder;
use dilla_core::envelope::EnvelopeType;
use dilla_core::identity::{
    DeviceEntry, DeviceList, DeviceListUnsigned, SskSigner, Tier, UmkSigner,
};
use dilla_core::ids::{DeviceId, UserId};

fn list_wire(peer: &Core) -> (Vec<u8>, Vec<u8>) {
    let put = peer.core.device_list_body().expect("own PUT body");
    let (version, blob, sig, prev) = decode_strict(&put, |d| {
        d.array(4)?;
        Ok((
            d.uint()?,
            d.bytes()?.to_vec(),
            d.bytes_exact::<64>()?,
            d.bytes_exact::<32>()?,
        ))
    })
    .expect("PUT [version,blob,signature,prev_hash]");
    let mut history = Encoder::new();
    history
        .array(1)
        .array(4)
        .uint(version)
        .bytes(&blob)
        .bytes(&sig)
        .bytes(&prev);
    (history.into_vec(), blob)
}

#[test]
fn exact_signed_history_pins_real_leaf_and_formats_pairwise_number() {
    let (_, relay, mut alice, bob) = alice_and_bob();
    let (history, head) = list_wire(&bob);
    let answer = alice
        .core
        .device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &bob.user,
            &history,
            &head,
            NOW,
        )
        .expect("ingest");
    let (version, changed, quarantined) = decode_strict(&answer, |d| {
        d.array(3)?;
        Ok((d.uint()?, d.uint()?, d.uint()?))
    })
    .expect("verdict");
    assert_eq!((version, changed, quarantined), (1, 0, 0));
    let number = alice
        .core
        .safety_number_for(&bob.user, &sec_cached_umk(&alice, &bob.user))
        .expect("number");
    assert_eq!(number.split(' ').count(), 12);
    assert!(
        number
            .split(' ')
            .all(|g| g.len() == 5 && g.bytes().all(|b| b.is_ascii_digit()))
    );
}

#[test]
fn trust_state_refuses_conflicting_security_registry_keys_for_one_user() {
    let (_, relay, mut alice, bob) = alice_and_bob();
    ingest_core_list(&mut alice, &relay, &bob);
    // One pin per user: a second, disagreeing registry key for Bob cannot be stored, so
    // trust_state cannot combine two keys into one apparent record.
    assert!(
        alice
            .probe
            .lock()
            .unwrap()
            .execute(
                "INSERT INTO app_trust_pins(user_id,umk_pub,first_seen,verified) VALUES(?1,?2,1,1)",
                rusqlite::params![bob.user.as_slice(), [0xe2u8; 32].as_slice()],
            )
            .is_err()
    );
    assert_eq!(contact_rows(&alice), 1);
    assert_ne!(sec_state_of(&alice, &bob.user).unwrap().0, [0xe2; 32]);
}

#[test]
fn wrong_head_bytes_and_missing_group_do_not_pin() {
    let (_, relay, mut alice, bob) = alice_and_bob();
    let (history, mut head) = list_wire(&bob);
    head[0] ^= 1;
    assert_eq!(
        code(alice.core.device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &bob.user,
            &history,
            &head,
            NOW
        )),
        "E_DEVICE_LIST_STALE"
    );
    assert_eq!(
        code(
            alice
                .core
                .device_list_ingest(&OTHER_GROUP, 0, &bob.user, &history, &head, NOW)
        ),
        "E_CORE_NOT_FOUND"
    );
    assert_eq!(contact_rows(&alice), 0, "only the own pin");
}

#[test]
fn a_sender_without_signed_history_stays_pending_and_unsearchable() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob();
    let (_, seq) = bob.send(&mut relay, &GROUP, "untrusted comet", NOW + 1);
    alice.sync(&relay);
    let row = alice.row_at(&GROUP, seq);
    assert_eq!(
        (row.status, row.reason.as_str(), row.body.as_str()),
        (1, "E_DEVICE_LIST_STALE", "")
    );
    assert!(
        alice
            .core
            .search(2, None, "untrusted comet", None, 50)
            .expect("search")
            == [0x80]
    );
}

/// web3.5 R1: a message released from pending history takes its mention flag by the role view
/// known at release: `<@everyone>` from a sender who may not send it counts only with a direct
/// mention.
#[test]
fn a_released_pending_message_applies_the_broadcast_gate() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob();
    let (_, plain) = bob.send(&mut relay, &GROUP, "<@everyone> late", NOW + 1);
    let direct = format!("<@everyone> and <@{}>", "a1".repeat(16));
    let (_, named) = bob.send(&mut relay, &GROUP, &direct, NOW + 2);
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, plain).status, 1);
    alice
        .core
        .authority_set(&GROUP, &[], &[], &[])
        .expect("bob may not broadcast");
    let (history, head) = list_wire(&bob);
    alice
        .core
        .device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &bob.user,
            &history,
            &head,
            NOW + 3,
        )
        .expect("ingest");
    assert_eq!(
        alice
            .core
            .revalidate_pending(&GROUP, relay.leaf_of(bob.device), 256)
            .expect("revalidate"),
        2
    );
    let (p, n) = (alice.row_at(&GROUP, plain), alice.row_at(&GROUP, named));
    assert_eq!((p.status, n.status), (0, 0), "both released");
    assert_eq!(
        (p.mention, n.mention),
        (0, 1),
        "the broadcast is gated; the direct mention counts"
    );
}

/// The same release from a sender the view allows `mention_everyone` counts.
#[test]
fn a_released_pending_broadcast_from_an_allowed_sender_counts() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob();
    let (_, seq) = bob.send(&mut relay, &GROUP, "<@everyone> late", NOW + 1);
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, seq).status, 1);
    alice
        .core
        .authority_set(&GROUP, &[], &bob.user, &[])
        .expect("bob may broadcast");
    let (history, head) = list_wire(&bob);
    alice
        .core
        .device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &bob.user,
            &history,
            &head,
            NOW + 2,
        )
        .expect("ingest");
    alice
        .core
        .revalidate_pending(&GROUP, relay.leaf_of(bob.device), 256)
        .expect("revalidate");
    let row = alice.row_at(&GROUP, seq);
    assert_eq!((row.status, row.mention), (0, 1));
}

#[test]
fn untrusted_message_does_not_abort_a_batch_with_an_honest_message() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob();
    let (_, bad_seq) = bob.send(&mut relay, &GROUP, "untrusted first", NOW + 1);
    let (_, good_seq) = alice.send(&mut relay, &GROUP, "honest second", NOW + 2);
    let applied = alice.sync(&relay);
    assert!(applied.next_seq > good_seq);
    let bad = alice.row_at(&GROUP, bad_seq);
    assert_eq!(
        (bad.status, bad.reason.as_str(), bad.body.as_str()),
        (1, "E_DEVICE_LIST_STALE", "")
    );
    assert_eq!(alice.row_at(&GROUP, good_seq).body, "honest second");
}

#[test]
fn signed_history_revalidates_a_previously_neutral_message() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob();
    let (_, seq) = bob.send(&mut relay, &GROUP, "late signed comet", NOW + 1);
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, seq).status, 1);
    let (history, head) = list_wire(&bob);
    alice
        .core
        .device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &bob.user,
            &history,
            &head,
            NOW + 2,
        )
        .expect("ingest");
    assert_eq!(
        alice
            .core
            .revalidate_pending(&GROUP, relay.leaf_of(bob.device), 256)
            .expect("revalidate"),
        1
    );
    let row = alice.row_at(&GROUP, seq);
    assert_eq!((row.status, row.body.as_str()), (0, "late signed comet"));
    assert_ne!(
        alice
            .core
            .search(2, None, "late signed", None, 50)
            .expect("search"),
        [0x80]
    );
}

#[test]
fn one_history_ingest_revalidates_more_than_one_page_past_an_unrecoverable_row() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob();
    let leaf = relay.leaf_of(bob.device);
    {
        let db = alice.probe.lock().expect("db");
        db.execute("INSERT INTO app_messages(group_id,seq,epoch,recv_ts,status,reason,sender_device,franking_tag) VALUES(?1,0,1,1,1,'E_DEVICE_LIST_STALE',?2,?3)",
            rusqlite::params![GROUP.as_slice(), bob.device.as_slice(), [0x44u8; 32].as_slice()]).expect("unrecoverable row");
        db.execute("INSERT INTO app_pending_history(group_id,user_id,device_id,seq,ciphertext,envelope,sender_leaf,sender_credential) VALUES(?1,?2,?3,0,x'80',x'80',?4,x'80')",
            rusqlite::params![GROUP.as_slice(),bob.user.as_slice(),bob.device.as_slice(), i64::from(leaf)]).expect("retained invalid evidence");
    }
    let mut last = 0;
    for n in 0..257 {
        let (_, seq) = bob.send(
            &mut relay,
            &GROUP,
            &format!("late batch comet {n}"),
            NOW + 1 + n,
        );
        last = seq;
    }
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, last).status, 1);
    let (history, head) = list_wire(&bob);
    alice
        .core
        .device_list_ingest(&GROUP, leaf, &bob.user, &history, &head, NOW + 300)
        .expect("ingest");
    assert_eq!(
        alice
            .core
            .revalidate_pending(&GROUP, leaf, 256)
            .expect("drain"),
        257
    );
    assert_eq!(
        (
            alice.row_at(&GROUP, last).status,
            alice.row_at(&GROUP, last).body.as_str()
        ),
        (0, "late batch comet 256")
    );
    assert_ne!(
        alice
            .core
            .search(2, None, "batch comet 256", None, 50)
            .expect("search"),
        [0x80]
    );
    let db = alice.probe.lock().expect("db");
    let status: i64 = db
        .query_row(
            "SELECT status FROM app_messages WHERE group_id=?1 AND seq=0",
            [GROUP.as_slice()],
            |r| r.get(0),
        )
        .expect("invalid row stays neutral");
    assert_eq!(status, 1);
}

#[test]
fn pending_leaf_revalidation_uses_the_leaf_sequence_index() {
    let (_, _, alice, _) = alice_and_bob();
    let db = alice.probe.lock().expect("db");
    let plan: Vec<String> = db.prepare("EXPLAIN QUERY PLAN SELECT p.seq,p.envelope,p.sender_leaf,p.sender_credential,m.epoch,m.recv_ts,m.sender_device,m.franking_tag \
        FROM app_pending_history p INDEXED BY app_pending_history_by_leaf_seq JOIN app_messages m ON m.group_id=p.group_id AND m.seq=p.seq \
        WHERE p.group_id=?1 AND p.user_id=?2 AND p.device_id=?3 AND p.sender_leaf=?4 AND p.seq>?5 AND m.status=1 AND m.reason='E_DEVICE_LIST_STALE' ORDER BY p.seq LIMIT ?6")
        .expect("plan").query_map(rusqlite::params![GROUP.as_slice(), [0xb2u8;16].as_slice(), [0xb2u8;16].as_slice(), 1_i64, -1_i64, 256_i64], |r| r.get(3))
        .expect("rows").collect::<Result<_, _>>().expect("plan rows");
    assert!(
        plan.iter()
            .any(|step| step.contains("app_pending_history_by_leaf_seq")
                && step.contains("group_id=?")
                && step.contains("user_id=?")
                && step.contains("device_id=?")
                && step.contains("sender_leaf=?")
                && step.contains("seq>?")),
        "pending query must seek by leaf and sequence: {plan:?}"
    );
}

#[test]
fn deleting_a_pending_message_discards_its_retained_ciphertext() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob();
    let (_, seq) = bob.send(&mut relay, &GROUP, "pending deleted comet", NOW + 1);
    alice.sync(&relay);
    alice
        .core
        .message_deleted(&GROUP, seq)
        .expect("delete pending row");
    let db = alice.probe.lock().expect("db");
    let retained: i64 = db
        .query_row(
            "SELECT count(*) FROM app_pending_history WHERE group_id=?1 AND seq=?2",
            rusqlite::params![GROUP.as_slice(), seq as i64],
            |r| r.get(0),
        )
        .expect("retained count");
    assert_eq!(retained, 0);
}

#[test]
fn revalidation_is_scoped_to_the_leaf_whose_history_arrived() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob();
    let (_, seq) = bob.send(&mut relay, &GROUP, "late leaf specific", NOW + 1);
    alice.sync(&relay);
    let bob_leaf = relay.leaf_of(bob.device);
    {
        let db = alice.probe.lock().expect("db");
        db.execute("INSERT INTO app_messages(group_id,seq,epoch,recv_ts,status,reason,sender_device,franking_tag) VALUES(?1,0,1,1,1,'E_DEVICE_LIST_STALE',?2,?3)",
            rusqlite::params![GROUP.as_slice(), [0x33u8; 16].as_slice(), [0x44u8; 32].as_slice()]).expect("earlier pending row");
        db.execute("INSERT INTO app_pending_history(group_id,user_id,device_id,seq,ciphertext,envelope,sender_leaf,sender_credential) VALUES(?1,?2,?3,0,x'80',x'80',999,x'80')",
            rusqlite::params![GROUP.as_slice(),[0x98u8;16].as_slice(),[0x99u8;16].as_slice()]).expect("earlier other leaf");
    }
    let (history, head) = list_wire(&bob);
    alice
        .core
        .device_list_ingest(&GROUP, bob_leaf, &bob.user, &history, &head, NOW + 2)
        .expect("ingest");
    assert_eq!(
        alice
            .core
            .revalidate_pending(&GROUP, bob_leaf, 1)
            .expect("bounded revalidate"),
        1
    );
    assert_eq!(alice.row_at(&GROUP, seq).body, "late leaf specific");
}

#[test]
fn authenticated_candidate_history_allows_first_contact_add() {
    let (instance, mut relay, mut alice, bob) = alice_and_bob_trusted();
    let mut carol = ready_core(0xc3, "carol");
    instance.propose_add(&mut relay, &carol.first_key_package());
    let proposals = relay.proposals_body();
    assert_eq!(
        code(alice.core.commit_build(&GROUP, &proposals)),
        "E_DEVICE_LIST_STALE"
    );
    let candidates = alice
        .core
        .proposed_adds(&GROUP, &proposals)
        .expect("authenticated candidates");
    let rows = decode_strict(&candidates, |d| {
        let n = d.array_len()?;
        let mut out = Vec::new();
        for _ in 0..n {
            d.array(3)?;
            out.push((d.uint()?, d.bytes_exact::<16>()?, d.uint()?));
        }
        Ok(out)
    })
    .expect("candidates");
    assert_eq!(rows, vec![(0, carol.user, 0)]);
    let (other_history, other_head) = list_wire(&bob);
    assert_eq!(
        code(alice.core.device_list_ingest_add(
            &GROUP,
            &proposals,
            0,
            &carol.user,
            &other_history,
            &other_head,
            NOW
        )),
        "E_CREDENTIAL"
    );
    let (history, head) = list_wire(&carol);
    alice
        .core
        .device_list_ingest_add(&GROUP, &proposals, 0, &carol.user, &history, &head, NOW)
        .expect("pre-add ingest");
    alice
        .core
        .commit_build(&GROUP, &proposals)
        .expect("first contact add");
}

#[test]
fn later_sender_verdict_retracts_visible_history_and_advances_generation() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob_trusted();
    let before_generation = alice.core.trust_generation().expect("generation");
    let (_, first) = bob.send(&mut relay, &GROUP, "previously visible comet", NOW + 1);
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, first).body, "previously visible comet");
    let sender_key: Vec<u8> = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT sender_sig_key FROM app_messages WHERE group_id=?1 AND seq=?2",
            rusqlite::params![GROUP.as_slice(), first as i64],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        sender_key.len(),
        32,
        "accepted history retains the authenticated leaf key"
    );
    assert_ne!(
        alice
            .core
            .search(2, None, "previously visible", None, 50)
            .unwrap(),
        [0x80]
    );

    // A signed list can be revoked or replaced between two application rows.
    // Force the cached verdict to fail while retaining the actual MLS sender.
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_device_lists SET list=x'80' WHERE user_id=?1",
            [bob.user.as_slice()],
        )
        .unwrap();
    let (_, second) = bob.send(&mut relay, &GROUP, "later comet", NOW + 2);
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, second).status, 1);
    assert!(alice.core.trust_generation().expect("generation") > before_generation);
    let old = alice.row_at(&GROUP, first);
    assert_eq!((old.status, old.body.as_str()), (1, ""));
    assert_eq!(
        alice
            .core
            .search(2, None, "previously visible", None, 50)
            .unwrap(),
        [0x80]
    );
    let generation: Vec<u8> = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT v FROM app_meta WHERE k='trust_generation'",
            [],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(
        dilla_core::cbor::decode_strict(&generation, |d| d.uint()).unwrap(),
        1
    );
}

#[test]
fn signed_revocation_retracts_earlier_text_and_search_hits() {
    // Bob is a fixture peer with known keys, so the survivor is a second device under the same
    // UMK and SSK and v3 is Bob's own signed revocation. (The earlier fixture paired a core Bob
    // with a survivor under a different UMK, so its v2 was a key change, not a revocation.)
    let (_, mut relay, mut alice, mut bob, v1) = sec_raw_bob();
    let seq = bob.send_envelope(
        &mut relay,
        &fold_envelope(
            [0x61; 16],
            EnvelopeType::Message,
            None,
            "revoked visible comet",
        ),
    );
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, seq).body, "revoked visible comet");
    let legacy_seq = bob.send_envelope(
        &mut relay,
        &fold_envelope(
            [0x62; 16],
            EnvelopeType::Message,
            None,
            "legacy revoked comet",
        ),
    );
    alice.sync(&relay);
    assert_eq!(
        alice.row_at(&GROUP, legacy_seq).body,
        "legacy revoked comet"
    );
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_messages SET sender_sig_key=NULL WHERE group_id=?1 AND seq=?2",
            rusqlite::params![GROUP.as_slice(), legacy_seq as i64],
        )
        .unwrap();
    assert_ne!(
        alice
            .core
            .search(2, None, "revoked visible", None, 50)
            .unwrap(),
        [0x80]
    );
    let mut survivor = RawPeer::new_with_keys(0xb2, 0xd4, 0xb2, 0xf2);
    survivor.join_external(&mut relay);
    alice.sync(&relay);
    let second = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&survivor)],
        0xf2,
    );
    sec_ingest(
        &mut alice,
        &relay,
        &bob.user,
        survivor.device,
        &[&v1, &second],
    );
    assert_eq!(alice.row_at(&GROUP, seq).body, "revoked visible comet");
    let third = sec_next(&second, vec![sec_entry(&survivor)], 0xf2);
    sec_ingest(
        &mut alice,
        &relay,
        &bob.user,
        survivor.device,
        &[&second, &third],
    );
    let row = alice.row_at(&GROUP, seq);
    assert_eq!((row.status, row.body.as_str()), (1, ""));
    let legacy_row = alice.row_at(&GROUP, legacy_seq);
    assert_eq!((legacy_row.status, legacy_row.body.as_str()), (1, ""));
    assert_eq!(
        alice
            .core
            .search(2, None, "legacy revoked", None, 50)
            .unwrap(),
        [0x80]
    );
    assert_eq!(
        alice
            .core
            .search(2, None, "revoked visible", None, 50)
            .unwrap(),
        [0x80]
    );
}

#[test]
fn merged_commit_quarantines_a_leaf_that_fails_its_cached_signed_list() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob_trusted();
    let (_, first) = bob.send(&mut relay, &GROUP, "commit visible comet", NOW + 1);
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, first).body, "commit visible comet");
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_device_lists SET list=x'80' WHERE user_id=?1",
            [bob.user.as_slice()],
        )
        .unwrap();
    bob.commit(&mut relay);
    alice.sync(&relay);
    assert_eq!(
        (
            alice.row_at(&GROUP, first).status,
            alice.row_at(&GROUP, first).body
        ),
        (1, String::new())
    );
    let blocks: i64 = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT count(*) FROM app_quarantine WHERE group_id=?1 AND device_id=?2",
            rusqlite::params![GROUP.as_slice(), bob.device.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(blocks, 1);
}

#[test]
fn external_join_audits_known_bad_existing_leaf() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob_trusted();
    let (_, first) = bob.send(&mut relay, &GROUP, "join visible comet", NOW + 1);
    alice.sync(&relay);
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_device_lists SET list=x'80' WHERE user_id=?1",
            [bob.user.as_slice()],
        )
        .unwrap();
    alice
        .core
        .group_join_external(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &relay.info_body(),
            &relay.tree_body(),
        )
        .unwrap();
    assert_eq!(
        (
            alice.row_at(&GROUP, first).status,
            alice.row_at(&GROUP, first).body
        ),
        (1, String::new())
    );
}

#[test]
fn welcome_join_audits_known_bad_existing_leaf() {
    let (instance, mut relay, mut alice, mut bob) = alice_and_bob_trusted();
    let (_, first) = bob.send(&mut relay, &GROUP, "welcome visible comet", NOW + 1);
    alice.sync(&relay);
    instance.propose_remove(&mut relay, alice.device);
    bob.commit(&mut relay);
    alice.sync(&relay);
    let package = alice.first_key_package();
    instance.propose_add(&mut relay, &package);
    bob.commit(&mut relay);
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_device_lists SET list=x'80' WHERE user_id=?1",
            [bob.user.as_slice()],
        )
        .unwrap();
    let results = decode_outcomes(
        &alice
            .core
            .welcomes_apply(
                &relay.welcomes_body(alice.device),
                &expected_body(&[(GROUP, COMMUNITY, CHANNEL, POLICY)]),
            )
            .unwrap(),
    );
    assert_eq!(results.last().unwrap().outcome, 0);
    assert_eq!(
        (
            alice.row_at(&GROUP, first).status,
            alice.row_at(&GROUP, first).body
        ),
        (1, String::new())
    );
}

#[test]
fn another_users_bad_cached_leaf_does_not_quarantine_an_honest_commit_author() {
    let (_, mut relay, mut alice, bob) = alice_and_bob_trusted();
    let mut carol = ready_core(0xc3, "carol");
    carol.join_external(&mut relay);
    alice.sync(&relay);
    ingest_core_list(&mut alice, &relay, &carol);
    let (_, prior) = carol.send(&mut relay, &GROUP, "author visible comet", NOW + 1);
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, prior).body, "author visible comet");
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_device_lists SET list=x'80' WHERE user_id=?1",
            [bob.user.as_slice()],
        )
        .unwrap();
    carol.commit(&mut relay);
    let forged_sender = relay.leaf_of(bob.device);
    relay.handshakes.last_mut().unwrap().sender = Some(forged_sender as u64);
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, prior).body, "author visible comet");
    let blocked: i64 = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT count(*) FROM app_quarantine WHERE group_id=?1 AND device_id=?2",
            rusqlite::params![GROUP.as_slice(), carol.device.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(blocked, 0);
    let bad_leaf_blocked: i64 = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT count(*) FROM app_quarantine WHERE group_id=?1 AND device_id=?2",
            rusqlite::params![GROUP.as_slice(), bob.device.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(bad_leaf_blocked, 1);
    let falsely_credited: i64 = alice.probe.lock().unwrap().query_row(
        "SELECT count(*) FROM app_trust_audit WHERE group_id=?1 AND user_id=?2 AND device_id=?3 AND added_by_user IS NOT NULL",
        rusqlite::params![GROUP.as_slice(),bob.user.as_slice(),bob.device.as_slice()],
        |r|r.get(0),
    ).unwrap();
    assert_eq!(
        falsely_credited, 0,
        "an existing failed leaf was not added by this committer"
    );
}

#[test]
fn own_commit_refuses_an_add_without_a_validated_signed_list() {
    let (instance, mut relay, mut alice, _) = alice_and_bob();
    let mut carol = ready_core(0xc3, "carol");
    let package = carol.first_key_package();
    instance.propose_add(&mut relay, &package);
    assert_eq!(
        code(alice.core.commit_build(&GROUP, &relay.proposals_body())),
        "E_DEVICE_LIST_STALE"
    );
}

#[test]
fn exact_key_verification_rechecks_the_signed_cache_and_releases_changed_key_blocks() {
    let (_, relay, mut alice, bob) = alice_and_bob();
    ingest_core_list(&mut alice, &relay, &bob);
    let expected: [u8; 32] = alice
        .probe
        .lock()
        .expect("db")
        .query_row(
            "SELECT umk_pub FROM app_device_lists WHERE user_id=?1",
            [bob.user.as_slice()],
            |r| r.get(0),
        )
        .expect("validated cached key");
    let bob_signature_key = DeviceList::decode(
        &alice
            .probe
            .lock()
            .unwrap()
            .query_row(
                "SELECT list FROM app_device_lists WHERE user_id=?1",
                [bob.user.as_slice()],
                |r| r.get::<_, Vec<u8>>(0),
            )
            .unwrap(),
    )
    .unwrap()
    .unsigned
    .entries[0]
        .dsk_pub;
    {
        let db = alice.probe.lock().expect("db");
        db.execute(
            "UPDATE app_trust_pins SET umk_pub=?2,changed_umk=?3,verified=0 WHERE user_id=?1",
            rusqlite::params![
                bob.user.as_slice(),
                [0xeeu8; 32].as_slice(),
                expected.as_slice()
            ],
        )
        .expect("simulate pending changed key");
        for group in [GROUP, OTHER_GROUP] {
            db.execute(
                "INSERT INTO app_quarantine(group_id,user_id,device_id,signature_key,code) VALUES(?1,?2,?3,?4,'E_UMK_CHANGED')",
                rusqlite::params![group.as_slice(), bob.user.as_slice(), bob.device.as_slice(),bob_signature_key.as_slice()],
            )
            .expect("block");
        }
    }
    assert!(alice.core.trust_verify(&bob.user, &[0xdd; 32]).is_err());
    alice
        .core
        .trust_verify(&bob.user, &expected)
        .expect("exact validated key");
    let db = alice.probe.lock().expect("db");
    let blocks: i64 = db
        .query_row(
            "SELECT count(*) FROM app_quarantine WHERE group_id=?1 AND device_id=?2",
            rusqlite::params![GROUP.as_slice(), bob.device.as_slice()],
            |r| r.get(0),
        )
        .expect("blocks");
    let verified: i64 = db
        .query_row(
            "SELECT verified FROM app_trust_pins WHERE user_id=?1",
            [bob.user.as_slice()],
            |r| r.get(0),
        )
        .expect("pin");
    assert_eq!((blocks, verified), (0, 1));
}

#[test]
fn first_seen_signed_contact_can_be_verified_after_comparison() {
    let (_, relay, mut alice, bob) = alice_and_bob();
    ingest_core_list(&mut alice, &relay, &bob);
    let expected: [u8; 32] = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT umk_pub FROM app_device_lists WHERE user_id=?1",
            [bob.user.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    alice
        .core
        .trust_verify(&bob.user, &expected)
        .expect("verified first sight");
    let verified: i64 = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT verified FROM app_trust_pins WHERE user_id=?1",
            [bob.user.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(verified, 1);
}

#[test]
fn verify_refuses_a_pending_cached_key_after_real_mls_remove() {
    let (instance, mut relay, mut alice, bob) = alice_and_bob();
    ingest_core_list(&mut alice, &relay, &bob);
    let expected: [u8; 32] = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT umk_pub FROM app_device_lists WHERE user_id=?1",
            [bob.user.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_trust_pins SET umk_pub=?2,changed_umk=?3,verified=0 WHERE user_id=?1",
            rusqlite::params![
                bob.user.as_slice(),
                [0xeeu8; 32].as_slice(),
                expected.as_slice()
            ],
        )
        .unwrap();
    instance.propose_remove(&mut relay, bob.device);
    alice.commit(&mut relay);
    assert_eq!(
        code(alice.core.trust_verify(&bob.user, &expected)),
        "E_DEVICE_LIST_STALE"
    );
}

#[test]
fn real_rejoin_with_rotated_umk_requires_exact_current_leaf_at_verify() {
    let (instance, mut relay, mut alice, bob) = alice_and_bob();
    let (old_history, old_blob) = list_wire(&bob);
    ingest_core_list(&mut alice, &relay, &bob);
    let old_number = alice
        .core
        .safety_number_for(&bob.user, &sec_cached_umk(&alice, &bob.user))
        .unwrap();
    instance.propose_remove(&mut relay, bob.device);
    alice.commit(&mut relay);
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    let old = DeviceList::decode(&old_blob).unwrap();
    let ssk = SskSigner::from_bytes(&[0xd6; 32]);
    let unsigned = DeviceListUnsigned {
        v: 1,
        user_id: UserId::from_bytes(bob.user),
        version: 2,
        prev_hash: old.hash(),
        entries: vec![DeviceEntry {
            device_id: DeviceId::from_bytes(rotated.device),
            dsk_pub: rotated.signer.public().try_into().unwrap(),
            tier: Tier::Native,
            added_at: NOW + 2,
            revoked_at: None,
        }],
    };
    let new = DeviceList {
        sig_ssk: ssk.sign_device_list(&unsigned),
        unsigned,
    };
    let (v1, blob1, sig1, prev1) = decode_strict(&old_history, |d| {
        d.array(1)?;
        d.array(4)?;
        Ok((
            d.uint()?,
            d.bytes()?.to_vec(),
            d.bytes_exact::<64>()?,
            d.bytes_exact::<32>()?,
        ))
    })
    .unwrap();
    let mut history = Encoder::new();
    history
        .array(2)
        .array(4)
        .uint(v1)
        .bytes(&blob1)
        .bytes(&sig1)
        .bytes(&prev1)
        .array(4)
        .uint(2)
        .bytes(&new.encode())
        .bytes(&new.sig_ssk)
        .bytes(&new.unsigned.prev_hash);
    let expected = UmkSigner::from_bytes(&[0xd5; 32]).public();
    let answer = alice
        .core
        .device_list_ingest(
            &GROUP,
            relay.leaf_of(rotated.device),
            &bob.user,
            &history.into_vec(),
            &new.encode(),
            NOW + 3,
        )
        .unwrap();
    let (_, changed, _) = decode_strict(&answer, |d| {
        d.array(3)?;
        Ok((d.uint()?, d.uint()?, d.uint()?))
    })
    .unwrap();
    assert_eq!(changed, 1);
    assert!(
        !alice.core.trust_state().unwrap().is_empty(),
        "revoked old device pins must not make the changed contact state ambiguous"
    );
    assert_eq!(
        code(
            alice
                .core
                .send_prepare(&GROUP, &request(0, None, "blocked comet", &[]), NOW + 3)
        ),
        "E_UMK_CHANGED",
        "a signed changed leaf still in this group blocks sending"
    );
    let pending_number = alice.core.safety_number_for(&bob.user, &expected).unwrap();
    assert_ne!(
        pending_number, old_number,
        "comparison must show the candidate key"
    );
    assert_eq!(pending_number.split(' ').count(), 12);
    assert!(
        alice.probe.lock().unwrap().execute(
            "INSERT INTO app_trust_pins(user_id,umk_pub,first_seen,verified,changed_umk) VALUES(?1,?2,1,0,?3)",
            rusqlite::params![bob.user.as_slice(),[0x90u8;32].as_slice(),[0x91u8;32].as_slice()],
        ).is_err(),
        "an older pending pin cannot be stored beside the current one"
    );
    assert_eq!(
        alice.core.safety_number_for(&bob.user, &expected).unwrap(),
        pending_number,
        "an older pending pin cannot replace the current signed candidate"
    );
    assert_eq!(
        code(alice.core.trust_verify(&bob.user, &[0xee; 32])),
        "E_DEVICE_LIST_STALE"
    );
    instance.propose_remove(&mut relay, rotated.device);
    alice.commit(&mut relay);
    assert_eq!(
        code(alice.core.trust_verify(&bob.user, &expected)),
        "E_DEVICE_LIST_STALE"
    );
    let mut substitute = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    substitute.join_external(&mut relay);
    alice.sync(&relay);
    assert_eq!(
        code(alice.core.trust_verify(&bob.user, &expected)),
        "E_DEVICE_LIST_STALE"
    );
    instance.propose_remove(&mut relay, substitute.device);
    alice.commit(&mut relay);
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    alice.core.trust_verify(&bob.user, &expected).unwrap();
    assert_eq!(
        sec_send(&mut alice),
        "OK",
        "exact-key verification releases the verified leaf's block"
    );
    let state = alice.core.trust_state().unwrap();
    let bob_state = decode_strict(&state, |d| {
        let n = d.array_len()?;
        let mut found = None;
        for _ in 0..n {
            d.array(6)?;
            let user = d.bytes_exact::<16>()?;
            let umk = d.bytes_exact::<32>()?;
            d.uint()?;
            let verified = d.uint()?;
            let changed = d.opt_bytes_exact::<32>()?;
            d.skip()?;
            if user == bob.user {
                found = Some((umk, verified, changed));
            }
        }
        Ok(found)
    })
    .unwrap()
    .unwrap();
    assert_eq!(
        bob_state,
        (expected, 1, None),
        "all pair pins reflect the verified user key"
    );
}
#[test]
fn another_users_signed_list_cannot_clear_a_victims_block() {
    let (_, relay, mut alice, bob) = alice_and_bob();
    ingest_core_list(&mut alice, &relay, &bob);
    let attacker = bob.user;
    let victim_device = bob.device;
    let victim = [0xc3u8; 16];
    let group = [0xf6u8; 16];
    let db = alice.probe.lock().unwrap();
    let key: [u8; 32] = db
        .query_row(
            "SELECT umk_pub FROM app_device_lists WHERE user_id=?1",
            [attacker.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    db.execute(
        "UPDATE app_trust_pins SET umk_pub=?2,changed_umk=?3 WHERE user_id=?1",
        rusqlite::params![attacker.as_slice(), [0x66u8; 32].as_slice(), key.as_slice()],
    )
    .unwrap();
    db.execute(
        "INSERT INTO app_quarantine(group_id,user_id,device_id,code) VALUES(?1,?2,?3,'E_UMK_CHANGED')",
        rusqlite::params![group.as_slice(), victim.as_slice(), victim_device.as_slice()],
    )
    .unwrap();
    db.execute(
        "INSERT INTO app_trust_pins(user_id,umk_pub,first_seen,verified,changed_umk) VALUES(?1,?2,1,0,?3)",
        rusqlite::params![victim.as_slice(), [0x55u8;32].as_slice(), [0x44u8;32].as_slice()],
    ).unwrap();
    drop(db);
    let _ = alice.core.trust_verify(&attacker, &key);
    let blocks: i64 = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT count(*) FROM app_quarantine WHERE group_id=?1 AND device_id=?2",
            rusqlite::params![group.as_slice(), victim_device.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(blocks, 1, "another user's list released a victim's block");
    let victim_changed: Vec<u8> = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT changed_umk FROM app_trust_pins WHERE user_id=?1",
            [victim.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(victim_changed, [0x44; 32]);
}

#[test]
fn oversized_candidate_head_is_rejected_before_decode() {
    use dilla_core::identity::{DeviceEntry, DeviceList, Tier};
    use dilla_core::ids::DeviceId;
    let (_, relay, mut alice, bob) = alice_and_bob();
    let (history, head) = list_wire(&bob);
    let mut huge = DeviceList::decode(&head).unwrap();
    let entry = DeviceEntry {
        device_id: DeviceId::from_bytes([0x99; 16]),
        dsk_pub: [0x88; 32],
        tier: Tier::Native,
        added_at: 1,
        revoked_at: None,
    };
    huge.unsigned.entries.resize(90_000, entry);
    let oversized = huge.encode();
    assert!(oversized.len() > 4 * 1024 * 1024);
    assert_eq!(
        code(alice.core.device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &bob.user,
            &history,
            &oversized,
            NOW
        )),
        "E_CORE_LIMIT"
    );
}

#[test]
fn overfull_candidate_head_is_rejected_before_entries_are_decoded() {
    use dilla_core::identity::{DeviceEntry, DeviceList, Tier};
    use dilla_core::ids::DeviceId;
    let (_, relay, mut alice, bob) = alice_and_bob();
    let (history, head) = list_wire(&bob);
    let mut huge = DeviceList::decode(&head).unwrap();
    let entry = DeviceEntry {
        device_id: DeviceId::from_bytes([0x99; 16]),
        dsk_pub: [0x88; 32],
        tier: Tier::Native,
        added_at: 1,
        revoked_at: None,
    };
    huge.unsigned.entries.resize(10_001, entry);
    let oversized = huge.encode();
    assert!(oversized.len() < 1024 * 1024);
    assert_eq!(
        code(alice.core.device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &bob.user,
            &history,
            &oversized,
            NOW
        )),
        "E_CORE_LIMIT"
    );
}

#[test]
fn overfull_signed_history_is_rejected_before_any_row_is_decoded() {
    let (_, relay, mut alice, bob) = alice_and_bob();
    let (_, head) = list_wire(&bob);
    let mut history = Encoder::new();
    history.array(1025);
    for _ in 0..1025 {
        history.null();
    }
    assert_eq!(
        code(alice.core.device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &bob.user,
            &history.into_vec(),
            &head,
            NOW,
        )),
        "E_CORE_LIMIT"
    );
}

#[test]
fn oversized_signed_history_is_rejected_before_decode() {
    let (_, relay, mut alice, bob) = alice_and_bob();
    let (_, head) = list_wire(&bob);
    let huge = vec![0xff; 4 * 1024 * 1024 + 1];
    assert_eq!(
        code(alice.core.device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &bob.user,
            &huge,
            &head,
            NOW,
        )),
        "E_CORE_LIMIT"
    );
}

// ---- Claude security review (sec-trust): regressions for T2-LAST-01..03 and leads (p)(q)(r). ----

type SecState = ([u8; 32], u64, Option<[u8; 32]>);

fn sec_state_of(alice: &Core, user: &[u8; 16]) -> Option<SecState> {
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

fn sec_bob_v1(alice: &mut Core, relay: &Relay, bob: &Core) -> DeviceList {
    let (_, blob) = list_wire(bob);
    ingest_core_list(alice, relay, bob);
    DeviceList::decode(&blob).unwrap()
}

fn sec_send(alice: &mut Core) -> &'static str {
    match alice
        .core
        .send_prepare(&GROUP, &request(0, None, "probe comet", &[]), NOW + 5)
    {
        Ok(_) => "OK",
        Err(e) => e.code,
    }
}

/// Alice's group with a fixture Bob whose UMK (seed 0xb2) and SSK (seed 0xf2) are known, so a
/// second device under the same key can be built; Bob's v1 list is ingested.
fn sec_raw_bob() -> (Instance, Relay, Core, RawPeer, DeviceList) {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut alice = ready_core(0xa1, "alice");
    alice.create_and_register(&mut relay, &instance);
    let mut bob = RawPeer::new(0xb2, 0xb3);
    bob.join_external(&mut relay);
    alice.sync(&relay);
    let body = bob.device_list_body();
    ingest_list_body(&mut alice, &relay, bob.user, bob.device, &body);
    let blob = decode_strict(&body, |d| {
        d.array(4)?;
        d.uint()?;
        let blob = d.bytes()?.to_vec();
        d.skip()?;
        d.skip()?;
        Ok(blob)
    })
    .unwrap();
    (
        instance,
        relay,
        alice,
        bob,
        DeviceList::decode(&blob).unwrap(),
    )
}

/// T2-LAST-01: a later signed list that drops a changed-key leaf must not release the
/// group's send block while that leaf is still a member; lead (r): nor may a second device of
/// the same unverified key be accepted as trusted.
#[test]
fn sec_signed_revocation_cannot_release_a_changed_leaf_still_in_the_group() {
    let (_, mut relay, mut alice, bob) = alice_and_bob();
    let v1 = sec_bob_v1(&mut alice, &relay, &bob);
    let original = sec_cached_umk(&alice, &bob.user);
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    let v2 = sec_next(&v1, vec![sec_entry(&rotated)], 0xd6);
    let (_, changed, _) = sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v1, &v2]);
    assert_eq!(changed, 1);
    assert_eq!(sec_send(&mut alice), "E_UMK_CHANGED");
    let mut second = RawPeer::new_with_keys(0xb2, 0xd7, 0xd5, 0xd6);
    second.join_external(&mut relay);
    alice.sync(&relay);
    let v3 = sec_next(&v2, vec![sec_entry(&second)], 0xd6);
    sec_ingest(&mut alice, &relay, &bob.user, second.device, &[&v2, &v3]);
    assert_eq!(
        sec_send(&mut alice),
        "E_UMK_CHANGED",
        "the revoked changed-key leaf and the new unverified one are still members"
    );
    let seq = second.send(&mut relay, "unverified second comet");
    alice.sync(&relay);
    let row = alice.row_at(&GROUP, seq);
    assert_eq!(
        (row.status, row.body.as_str()),
        (1, ""),
        "a device of the unverified changed key is not trusted"
    );
    let (pinned, verified, pending) = sec_state_of(&alice, &bob.user).expect("bob state");
    assert_eq!(pinned, original);
    assert_eq!((verified, pending.is_some()), (0, true));
}

/// T2-LAST-02: a verified contact who adds a device under the same key stays verified and
/// trust_state keeps answering for everyone.
#[test]
fn sec_same_key_device_addition_keeps_a_verified_contact_readable() {
    let (_, mut relay, mut alice, mut bob, v1) = sec_raw_bob();
    let key = UmkSigner::from_bytes(&[0xb2; 32]).public();
    assert_eq!(sec_cached_umk(&alice, &bob.user), key);
    alice.core.trust_verify(&bob.user, &key).expect("verify");
    let mut added = RawPeer::new_with_keys(0xb2, 0xd4, 0xb2, 0xf2);
    let join = added.join_external(&mut relay);
    alice.sync(&relay);
    bob.apply_commit(&relay, join);
    let v2 = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&added)],
        0xf2,
    );
    sec_ingest(&mut alice, &relay, &bob.user, added.device, &[&v1, &v2]);
    assert_eq!(sec_state_of(&alice, &bob.user), Some((key, 1, None)));
    let seq = added.send_envelope(
        &mut relay,
        &fold_envelope(
            [0x63; 16],
            EnvelopeType::Message,
            None,
            "second device comet",
        ),
    );
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, seq).body, "second device comet");
    let seq = bob.send_envelope(
        &mut relay,
        &fold_envelope(
            [0x64; 16],
            EnvelopeType::Message,
            None,
            "first device comet",
        ),
    );
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, seq).body, "first device comet");
    assert_eq!(sec_send(&mut alice), "OK");
}

/// Lead (p): an honest device turnover under an unchanged key must not erase the contact's
/// verification.
#[test]
fn sec_device_turnover_under_the_same_key_keeps_verification() {
    let (instance, mut relay, mut alice, bob, v1) = sec_raw_bob();
    let key = UmkSigner::from_bytes(&[0xb2; 32]).public();
    alice.core.trust_verify(&bob.user, &key).expect("verify");
    instance.propose_remove(&mut relay, bob.device);
    alice.commit(&mut relay);
    let mut added = RawPeer::new_with_keys(0xb2, 0xd4, 0xb2, 0xf2);
    added.join_external(&mut relay);
    alice.sync(&relay);
    let v2 = sec_next(&v1, vec![sec_entry(&added)], 0xf2);
    sec_ingest(&mut alice, &relay, &bob.user, added.device, &[&v1, &v2]);
    assert_eq!(sec_state_of(&alice, &bob.user), Some((key, 1, None)));
}

/// Lead (p): a list signed under a new, unverified key must not erase history that was
/// accepted under the pinned key.
#[test]
fn sec_unverified_key_change_cannot_erase_history_accepted_under_the_pinned_key() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob_trusted();
    let (_, seq) = bob.send(&mut relay, &GROUP, "pinned honest comet", NOW + 1);
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, seq).body, "pinned honest comet");
    let (_, legacy) = bob.send(&mut relay, &GROUP, "pinned legacy comet", NOW + 2);
    alice.sync(&relay);
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_messages SET sender_sig_key=NULL WHERE group_id=?1 AND seq=?2",
            rusqlite::params![GROUP.as_slice(), legacy as i64],
        )
        .unwrap();
    let (_, blob) = list_wire(&bob);
    let v1 = DeviceList::decode(&blob).unwrap();
    let mut forged = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    forged.join_external(&mut relay);
    alice.sync(&relay);
    bob.sync(&relay);
    let v2 = sec_next(&v1, vec![sec_entry(&forged)], 0xd6);
    let (_, changed, _) = sec_ingest(&mut alice, &relay, &bob.user, forged.device, &[&v1, &v2]);
    assert_eq!(changed, 1);
    let row = alice.row_at(&GROUP, seq);
    assert_eq!(
        (row.status, row.body.as_str()),
        (0, "pinned honest comet"),
        "an unverified key cannot revoke what the pinned key's device said"
    );
    assert_eq!(
        alice.row_at(&GROUP, legacy).body,
        "pinned legacy comet",
        "nor a row stored before the leaf key was kept"
    );
    assert_ne!(
        alice
            .core
            .search(2, None, "pinned honest", None, 50)
            .unwrap(),
        [0x80]
    );
    assert_eq!(sec_send(&mut alice), "E_UMK_CHANGED");
}

/// Lead (r): the changed-key gate holds at encryption, not only when the row was queued.
#[test]
fn sec_send_encrypt_rechecks_the_changed_key_block() {
    let (_, mut relay, mut alice, bob) = alice_and_bob();
    let v1 = sec_bob_v1(&mut alice, &relay, &bob);
    let msg = alice.prepare(&GROUP, "queued before the change", NOW + 1);
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    let v2 = sec_next(&v1, vec![sec_entry(&rotated)], 0xd6);
    sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v1, &v2]);
    assert_eq!(code(alice.core.send_encrypt(&msg)), "E_UMK_CHANGED");
}

/// Lead (q): a Remove this device commits itself is a validated Remove too; it must release
/// that group's changed-key block instead of locking the group.
#[test]
fn sec_own_commit_remove_releases_the_changed_key_block() {
    let (instance, mut relay, mut alice, bob) = alice_and_bob();
    let v1 = sec_bob_v1(&mut alice, &relay, &bob);
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    let v2 = sec_next(&v1, vec![sec_entry(&rotated)], 0xd6);
    sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v1, &v2]);
    assert_eq!(sec_send(&mut alice), "E_UMK_CHANGED");
    instance.propose_remove(&mut relay, rotated.device);
    alice.commit(&mut relay);
    assert_eq!(sec_send(&mut alice), "OK");
    let (_, verified, pending) = sec_state_of(&alice, &bob.user).unwrap();
    assert_eq!(
        (verified, pending.is_some()),
        (0, true),
        "a Remove approves nothing"
    );
}

/// T2-LAST-01 (precedence): a later list back under the pinned key turns the changed-key leaf
/// into a failed one, but its send block stays until that leaf is removed or its key verified.
#[test]
fn sec_a_list_back_under_the_pinned_key_keeps_a_changed_leaf_blocked() {
    let (_, mut relay, mut alice, bob, v1) = sec_raw_bob();
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    let v2 = sec_next(&v1, vec![sec_entry(&rotated)], 0xd6);
    let (_, changed, _) = sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v1, &v2]);
    assert_eq!(changed, 1);
    let v3 = sec_next(&v2, vec![v1.unsigned.entries[0].clone()], 0xf2);
    sec_ingest(&mut alice, &relay, &bob.user, bob.device, &[&v2, &v3]);
    let key = UmkSigner::from_bytes(&[0xb2; 32]).public();
    assert_eq!(
        sec_state_of(&alice, &bob.user),
        Some((key, 0, None)),
        "the pinned key's own newest list ends the pending change"
    );
    assert_eq!(
        sec_send(&mut alice),
        "E_UMK_CHANGED",
        "the changed-key leaf is still a member"
    );
    alice
        .core
        .trust_verify(&bob.user, &key)
        .expect("verify the pinned key");
    assert_eq!(
        sec_send(&mut alice),
        "E_UMK_CHANGED",
        "verifying another key does not release it"
    );
}

/// A failed leaf left in the tree is audited once, not once per epoch.
#[test]
fn sec_a_failed_leaf_left_in_the_tree_is_audited_once() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob_trusted();
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_device_lists SET list=x'80' WHERE user_id=?1",
            [bob.user.as_slice()],
        )
        .unwrap();
    for _ in 0..3 {
        bob.commit(&mut relay);
        alice.sync(&relay);
    }
    let rows: i64 = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT count(*) FROM app_trust_audit WHERE group_id=?1 AND user_id=?2 AND device_id=?3",
            rusqlite::params![GROUP.as_slice(), bob.user.as_slice(), bob.device.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(rows, 1);
}

// ---- Controller rulings 2 and 3 (2026-10-13): failed leaves block sending; failed Adds are
// left out of this device's commit. ----

fn sec_v1_of(peer: &RawPeer, ssk_seed: u8) -> DeviceList {
    let unsigned = DeviceListUnsigned {
        v: 1,
        user_id: UserId::from_bytes(peer.user),
        version: 1,
        prev_hash: [0; 32],
        entries: vec![sec_entry(peer)],
    };
    DeviceList {
        sig_ssk: SskSigner::from_bytes(&[ssk_seed; 32]).sign_device_list(&unsigned),
        unsigned,
    }
}

fn sec_key_package(peer: &RawPeer) -> Vec<u8> {
    use tls_codec::Serialize as _;
    let bundle = dilla_core::mls::build_key_package(
        &peer.provider,
        &peer.signer,
        peer.credential.clone(),
        false,
    )
    .expect("key package");
    openmls::prelude::MlsMessageOut::from(bundle.key_package().clone())
        .tls_serialize_detached()
        .expect("serialize")
}

fn sec_send_in(alice: &mut Core, group: &[u8; 16]) -> &'static str {
    match alice
        .core
        .send_prepare(group, &request(0, None, "probe comet", &[]), NOW + 5)
    {
        Ok(_) => "OK",
        Err(e) => e.code,
    }
}

/// Ruling 2: a leaf whose verdict failed (here a forged key for a pinned user, whose list the
/// instance never serves) blocks sending in that group, at queueing and at encryption, until a
/// validated Remove takes it out; the committer that added it is not blocked elsewhere.
#[test]
fn sec_a_failed_leaf_blocks_its_group_until_removed_and_only_that_group() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut alice = ready_core(0xa1, "alice");
    let mut bob = ready_core(0xb2, "bob");
    alice.create_and_register(&mut relay, &instance);
    bob.join_external(&mut relay);
    alice.sync(&relay);
    ingest_core_list(&mut alice, &relay, &bob);
    // Xavier's honest device; Alice pins his key.
    let mut xavier = RawPeer::new(0xd1, 0xd2);
    xavier.join_external(&mut relay);
    alice.sync(&relay);
    bob.sync(&relay);
    ingest_list_body(
        &mut alice,
        &relay,
        xavier.user,
        xavier.device,
        &xavier.device_list_body(),
    );
    // A second group shared by Alice and Bob (the committer below).
    let mut other = Relay::new(OTHER_GROUP);
    alice.create_and_register_in(&mut other, &instance, Some(&COMMUNITY), &OTHER_CHANNEL);
    bob.join_external_in(&mut other, Some(&COMMUNITY), &OTHER_CHANNEL);
    alice.sync(&other);
    let queued = alice.prepare(&GROUP, "queued before the forged leaf", NOW + 1);
    // Bob was shown a different first-sight list for Xavier and commits its device.
    let forged = RawPeer::new_with_keys(0xd1, 0xd3, 0xd5, 0xd6);
    instance.propose_add(&mut relay, &sec_key_package(&forged));
    let proposals = relay.proposals_body();
    let forged_v1 = sec_v1_of(&forged, 0xd6);
    bob.core
        .device_list_ingest_add(
            &GROUP,
            &proposals,
            0,
            &xavier.user,
            &sec_history(&[&forged_v1]),
            &forged_v1.encode(),
            NOW,
        )
        .expect("Bob's first sight of the forged key");
    bob.commit(&mut relay);
    alice.sync(&relay);
    assert_eq!(sec_send_in(&mut alice, &GROUP), "E_CREDENTIAL");
    assert_eq!(code(alice.core.send_encrypt(&queued)), "E_CREDENTIAL");
    assert_eq!(
        sec_send_in(&mut alice, &OTHER_GROUP),
        "OK",
        "the committer's other groups are unaffected"
    );
    let committer_rows: i64 = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT count(*) FROM app_quarantine WHERE user_id=?1",
            [bob.user.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(committer_rows, 0, "the committer is not quarantined");
    let credited: i64 = alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT count(*) FROM app_trust_audit WHERE group_id=?1 AND device_id=?2 AND added_by_user=?3",
            rusqlite::params![GROUP.as_slice(), [0xd3u8; 16].as_slice(), bob.user.as_slice()],
            |r| r.get(0),
        )
        .unwrap();
    assert_eq!(credited, 1, "the audit names who added the leaf");
    instance.propose_remove(&mut relay, forged.device);
    alice.commit(&mut relay);
    assert_eq!(sec_send_in(&mut alice, &GROUP), "OK");
}

/// Ruling 2, honest path: a same-key device that joins before its list is ingested is failed
/// (unlisted) and blocks the group only until the list naming it is ingested.
#[test]
fn sec_a_listed_device_ends_its_own_failed_verdict() {
    let (_, mut relay, mut alice, bob, v1) = sec_raw_bob();
    let mut added = RawPeer::new_with_keys(0xb2, 0xd4, 0xb2, 0xf2);
    added.join_external(&mut relay);
    alice.sync(&relay);
    assert_eq!(sec_send(&mut alice), "E_DEVICE_UNLISTED");
    let v2 = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&added)],
        0xf2,
    );
    sec_ingest(&mut alice, &relay, &bob.user, added.device, &[&v1, &v2]);
    assert_eq!(sec_send(&mut alice), "OK");
}

/// Ruling 3: a proposed Add whose leaf fails validation is left out of this device's commit
/// instead of refusing (and so stalling) every commit.
#[test]
fn sec_a_failed_proposed_add_is_left_out_of_this_devices_commit() {
    let (instance, mut relay, mut alice, bob) = alice_and_bob();
    ingest_core_list(&mut alice, &relay, &bob);
    let mut xavier = RawPeer::new(0xd1, 0xd2);
    xavier.join_external(&mut relay);
    alice.sync(&relay);
    ingest_list_body(
        &mut alice,
        &relay,
        xavier.user,
        xavier.device,
        &xavier.device_list_body(),
    );
    let forged = RawPeer::new_with_keys(0xd1, 0xd3, 0xd5, 0xd6);
    instance.propose_add(&mut relay, &sec_key_package(&forged));
    let body = alice
        .core
        .commit_build(&GROUP, &relay.proposals_body())
        .expect("the failed Add does not refuse the commit");
    relay.commit(&body).expect("commit accepted");
    alice.core.commit_confirm(&GROUP).expect("commit_confirm");
    let members = pending(&alice.core.pending_leaves().unwrap())
        .into_iter()
        .filter(|row| row.0 == GROUP)
        .count();
    assert_eq!(
        members, 3,
        "Alice, Bob and Xavier only: the failed leaf was not committed"
    );
    assert_eq!(sec_send(&mut alice), "OK");
}

/// Ruling 3, the usual order: the instance's Add reaches this device as a handshake row first
/// (`group_apply` queues it), and only then does this device build a commit. A failed Add that
/// is already queued must be left out as well: `self_update` consumes the whole queue, so it
/// would otherwise commit the failed leaf and address a Welcome to its device.
#[test]
fn sec_a_failed_add_queued_by_sync_is_left_out_of_this_devices_commit() {
    let (instance, mut relay, mut alice, bob) = alice_and_bob();
    ingest_core_list(&mut alice, &relay, &bob);
    let mut xavier = RawPeer::new(0xd1, 0xd2);
    xavier.join_external(&mut relay);
    alice.sync(&relay);
    ingest_list_body(
        &mut alice,
        &relay,
        xavier.user,
        xavier.device,
        &xavier.device_list_body(),
    );
    let forged = RawPeer::new_with_keys(0xd1, 0xd3, 0xd5, 0xd6);
    instance.propose_add(&mut relay, &sec_key_package(&forged));
    alice.sync(&relay);
    let body = alice
        .core
        .commit_build(&GROUP, &relay.proposals_body())
        .expect("the failed Add does not refuse the commit");
    let welcomes = decode_strict(&body, |d| {
        d.array(5)?;
        d.uint()?;
        d.bytes()?;
        d.bytes()?;
        let n = d.array_len()?;
        for _ in 0..n {
            d.array(2)?;
            d.bytes()?;
            d.bytes()?;
        }
        d.null()?;
        Ok(n)
    })
    .expect("commit_build body");
    assert_eq!(welcomes, 0, "no Welcome is addressed to the failed device");
    relay.commit(&body).expect("commit accepted");
    alice.core.commit_confirm(&GROUP).expect("commit_confirm");
    let members = pending(&alice.core.pending_leaves().unwrap())
        .into_iter()
        .filter(|row| row.0 == GROUP)
        .count();
    assert_eq!(
        members, 3,
        "Alice, Bob and Xavier only: the failed leaf was not committed"
    );
    assert_eq!(sec_send(&mut alice), "OK");
}

// ---------------------------------------------------------------------------------------------
// Fix wave A (whole-branch security review, 2026-10-13).

fn fixa_try_ingest(
    alice: &mut Core,
    relay: &Relay,
    user: &[u8; 16],
    via: [u8; 16],
    lists: &[&DeviceList],
) -> Result<(u64, u64, u64), &'static str> {
    let head = lists.last().unwrap().encode();
    alice
        .core
        .device_list_ingest(
            &relay.group_id,
            relay.leaf_of(via),
            user,
            &sec_history(lists),
            &head,
            NOW + 3,
        )
        .map(|answer| {
            decode_strict(&answer, |d| {
                d.array(3)?;
                Ok((d.uint()?, d.uint()?, d.uint()?))
            })
            .unwrap()
        })
        .map_err(|e| e.code)
}

fn fixa_cached(alice: &Core, user: &[u8; 16]) -> ([u8; 32], [u8; 32], Vec<u8>) {
    alice
        .probe
        .lock()
        .unwrap()
        .query_row(
            "SELECT umk_pub,ssk_pub,list FROM app_device_lists WHERE user_id=?1",
            [user.as_slice()],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        )
        .unwrap()
}

/// F1: a history that holds only the cached anchor must still be signed by the ingested leaf's
/// SSK. Bob rotates his UMK and keeps a device key his pinned list names; the instance serves the
/// unchanged head only. The ingest is refused, the cache stays bound to the key that signed it,
/// and the honest K2 list, its verification and later ingests all keep working.
#[test]
fn fixa_an_anchor_only_history_is_checked_against_the_leafs_ssk() {
    let (_, mut relay, mut alice, bob, v1) = sec_raw_bob();
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    let v2 = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&rotated)],
        0xf2,
    );
    sec_ingest(&mut alice, &relay, &bob.user, bob.device, &[&v1, &v2]);
    let k1 = UmkSigner::from_bytes(&[0xb2; 32]).public();
    let k2 = UmkSigner::from_bytes(&[0xd5; 32]).public();
    assert_eq!(
        fixa_try_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v2]),
        Err("E_CREDENTIAL"),
        "no list signed under the K2 leaf's SSK was presented"
    );
    let (umk, ssk, list) = fixa_cached(&alice, &bob.user);
    assert_eq!(umk, k1, "the cache stays bound to the key that signed it");
    assert!(DeviceList::decode(&list).unwrap().verify(&ssk).is_ok());
    assert_eq!(sec_state_of(&alice, &bob.user).map(|s| s.2), Some(None));
    // The honest K2 list chaining to v2 still ingests and becomes the pending change.
    let v3 = sec_next(&v2, vec![sec_entry(&rotated)], 0xd6);
    let (_, changed, _) = sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v2, &v3]);
    assert_eq!(changed, 1);
    // The pinned key's leaf cannot end the change with a head its key never signed.
    assert_eq!(
        fixa_try_ingest(&mut alice, &relay, &bob.user, bob.device, &[&v3]),
        Err("E_CREDENTIAL")
    );
    assert_eq!(
        sec_state_of(&alice, &bob.user).map(|s| s.2),
        Some(Some(k2)),
        "the pending change is not ended by an unsigned anchor"
    );
    alice
        .core
        .trust_verify(&bob.user, &k2)
        .expect("the changed key can be verified");
    let v4 = sec_next(&v3, vec![sec_entry(&rotated)], 0xd6);
    sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v3, &v4]);
    sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v4]);
    let state = sec_state_of(&alice, &bob.user).unwrap();
    assert_eq!((state.0, state.1, state.2), (k2, 1, None));
}

/// A trust_state row: (umk, verified, changed, blocked).
type FixaRow = ([u8; 32], u64, Option<[u8; 32]>, Option<String>);

fn fixa_row(alice: &Core, user: &[u8; 16]) -> Option<FixaRow> {
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
            let blocked = if d.try_null()? {
                None
            } else {
                Some(d.text()?.to_owned())
            };
            if candidate == *user {
                found = Some((umk, verified, changed, blocked));
            }
        }
        Ok(found)
    })
    .unwrap()
}

/// F4: a verified contact with a stored failed verdict on a leaf still in a group is reported as
/// blocked by the core itself, also after a reload, until that leaf leaves the group.
#[test]
fn fixa_trust_state_reports_a_stored_failed_verdict_per_user() {
    let (instance, mut relay, mut alice, bob) = alice_and_bob();
    ingest_core_list(&mut alice, &relay, &bob);
    let umk = sec_cached_umk(&alice, &bob.user);
    alice.core.trust_verify(&bob.user, &umk).expect("verified");
    assert_eq!(fixa_row(&alice, &bob.user), Some((umk, 1, None, None)));
    // The instance joins a leaf claiming Bob's id under another key.
    let mut forged = RawPeer::new_with_keys(0xb2, 0xd3, 0xd5, 0xd6);
    forged.join_external(&mut relay);
    alice.sync(&relay);
    assert_eq!(sec_send(&mut alice), "E_CREDENTIAL");
    let expected = Some((umk, 1, None, Some("E_CREDENTIAL".to_owned())));
    assert_eq!(fixa_row(&alice, &bob.user), expected);
    let mut alice = alice.reopen();
    assert_eq!(
        fixa_row(&alice, &bob.user),
        expected,
        "the verdict is read from the store, not from a session's ingests"
    );
    instance.propose_remove(&mut relay, forged.device);
    alice.commit(&mut relay);
    assert_eq!(
        fixa_row(&alice, &bob.user),
        Some((umk, 1, None, None)),
        "a verdict of a leaf no longer in any group is not reported"
    );
}

/// Alice's real UMK, as Bob pins it from her signed list.
fn fixa_own_umk(alice: &Core, bob: &mut Core, relay: &Relay) -> [u8; 32] {
    ingest_core_list(bob, relay, alice);
    sec_cached_umk(bob, &alice.user)
}

/// F3: the own account's UMK is pinned verified from this device's own credential, at signup and
/// again at every open (a store whose own pin is missing or names another key is repaired).
#[test]
fn fixa_the_own_key_is_pinned_verified_from_the_own_credential() {
    let (_, relay, alice, mut bob) = alice_and_bob();
    let own = fixa_own_umk(&alice, &mut bob, &relay);
    assert_eq!(fixa_row(&alice, &alice.user), Some((own, 1, None, None)));
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_trust_pins SET umk_pub=?2,verified=0,changed_umk=?3 WHERE user_id=?1",
            rusqlite::params![
                alice.user.as_slice(),
                [0xe2u8; 32].as_slice(),
                [0xe3u8; 32].as_slice()
            ],
        )
        .unwrap();
    let alice = alice.reopen();
    assert_eq!(fixa_row(&alice, &alice.user), Some((own, 1, None, None)));
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "DELETE FROM app_trust_pins WHERE user_id=?1",
            [alice.user.as_slice()],
        )
        .unwrap();
    let alice = alice.reopen();
    assert_eq!(fixa_row(&alice, &alice.user), Some((own, 1, None, None)));
}

/// F3: a second browser enrolled by recovery key pins the same own key, verified.
#[test]
fn fixa_an_enrolled_device_pins_the_own_key_verified() {
    let (first, recovery_key) = ready_core_with_key(0xa1, "alice");
    let second = enrolled_core(&first, &recovery_key, NOW + 1);
    let own = fixa_row(&first, &first.user).expect("own row").0;
    assert_eq!(fixa_row(&second, &second.user), Some((own, 1, None, None)));
}

/// F3: a leaf that claims the own user id under another UMK (an external join the instance
/// crafts) fails validation before any list is served: it blocks sending in its group like any
/// failed leaf, trust_state reports it on the own row, and a list the instance serves for it is
/// refused and changes no pin. A Remove releases the group.
#[test]
fn fixa_an_own_user_leaf_under_another_key_fails_and_is_reported() {
    let (instance, mut relay, mut alice, mut bob) = alice_and_bob();
    let own = fixa_own_umk(&alice, &mut bob, &relay);
    let mut impostor = RawPeer::new_with_keys(0xa1, 0xe5, 0xe2, 0xe1);
    impostor.join_external(&mut relay);
    alice.sync(&relay);
    assert_eq!(sec_send(&mut alice), "E_UMK_CHANGED");
    let reported = Some((own, 1, None, Some("E_UMK_CHANGED".to_owned())));
    assert_eq!(fixa_row(&alice, &alice.user), reported);
    let body = impostor.device_list_body();
    let (version, blob, sig, prev) = decode_strict(&body, |d| {
        d.array(4)?;
        Ok((
            d.uint()?,
            d.bytes()?.to_vec(),
            d.bytes_exact::<64>()?,
            d.bytes_exact::<32>()?,
        ))
    })
    .unwrap();
    let mut history = Encoder::new();
    history
        .array(1)
        .array(4)
        .uint(version)
        .bytes(&blob)
        .bytes(&sig)
        .bytes(&prev);
    assert_eq!(
        code(alice.core.device_list_ingest(
            &GROUP,
            relay.leaf_of(impostor.device),
            &alice.user,
            &history.into_vec(),
            &blob,
            NOW + 3,
        )),
        "E_UMK_CHANGED",
        "the instance cannot pin another key for the own account"
    );
    assert_eq!(fixa_row(&alice, &alice.user), reported);
    let mut alice = alice.reopen();
    assert_eq!(sec_send(&mut alice), "E_UMK_CHANGED");
    instance.propose_remove(&mut relay, impostor.device);
    alice.commit(&mut relay);
    assert_eq!(sec_send(&mut alice), "OK");
    assert_eq!(fixa_row(&alice, &alice.user), Some((own, 1, None, None)));
}

/// F6: the safety number is bound to the key the page will approve; a number for any other key
/// (a stale snapshot, or the pinned key while a change is pending) is refused.
#[test]
fn fixa_the_safety_number_is_bound_to_the_key_the_page_approves() {
    let (_, mut relay, mut alice, bob, v1) = sec_raw_bob();
    let k1 = sec_cached_umk(&alice, &bob.user);
    let number = alice
        .core
        .safety_number_for(&bob.user, &k1)
        .expect("the pinned key is the comparison key");
    assert_eq!(number.split(' ').count(), 12);
    assert_eq!(
        code(alice.core.safety_number_for(&bob.user, &[0xee; 32])),
        "E_DEVICE_LIST_STALE"
    );
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    let v2 = sec_next(&v1, vec![sec_entry(&rotated)], 0xd6);
    sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v1, &v2]);
    let k2 = UmkSigner::from_bytes(&[0xd5; 32]).public();
    assert_eq!(
        code(alice.core.safety_number_for(&bob.user, &k1)),
        "E_DEVICE_LIST_STALE",
        "while a change is pending the pinned key is not the one compared"
    );
    let changed = alice
        .core
        .safety_number_for(&bob.user, &k2)
        .expect("the pending key");
    assert_ne!(changed, number);
}

/// W-01: the page blocks the composer from the core's own per-group send gate, the same verdict
/// `send_prepare` and `send_encrypt` refuse with.
#[test]
fn fixa_the_send_block_code_is_readable_per_group() {
    let (_, mut relay, mut alice, bob, v1) = sec_raw_bob();
    assert_eq!(alice.core.send_block_code(&GROUP).unwrap(), None);
    let mut added = RawPeer::new_with_keys(0xb2, 0xd4, 0xb2, 0xf2);
    added.join_external(&mut relay);
    alice.sync(&relay);
    assert_eq!(
        alice.core.send_block_code(&GROUP).unwrap(),
        Some("E_DEVICE_UNLISTED")
    );
    assert_eq!(sec_send(&mut alice), "E_DEVICE_UNLISTED");
    assert_eq!(alice.core.send_block_code(&OTHER_GROUP).unwrap(), None);
    let v2 = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&added)],
        0xf2,
    );
    sec_ingest(&mut alice, &relay, &bob.user, added.device, &[&v1, &v2]);
    assert_eq!(alice.core.send_block_code(&GROUP).unwrap(), None);
}

/// Product P2: messages a contact's new key sent while the change was pending (and before its
/// list arrived) wait, unreadable, and become readable when exact-key verification approves that
/// key. Nothing is released before the approval.
#[test]
fn fixa_messages_under_a_changed_key_become_readable_after_verification() {
    let (_, mut relay, mut alice, bob, v1) = sec_raw_bob();
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    let early = rotated.send(&mut relay, "sent before my new list");
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, early).status, 1);
    let v2 = sec_next(&v1, vec![sec_entry(&rotated)], 0xd6);
    let (_, changed, _) = sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v1, &v2]);
    assert_eq!(changed, 1);
    let pending = rotated.send(&mut relay, "sent while the change is pending");
    alice.sync(&relay);
    let row = alice.row_at(&GROUP, pending);
    assert_eq!(
        (row.status, row.reason.as_str(), row.body.as_str()),
        (1, "E_UMK_CHANGED", "")
    );
    alice
        .core
        .revalidate_pending(&GROUP, relay.leaf_of(rotated.device), 256)
        .expect("revalidate");
    assert_eq!(
        alice.row_at(&GROUP, pending).status,
        1,
        "an unverified changed key releases nothing"
    );
    let k2 = UmkSigner::from_bytes(&[0xd5; 32]).public();
    alice.core.trust_verify(&bob.user, &k2).expect("approve K2");
    for (seq, body) in [
        (early, "sent before my new list"),
        (pending, "sent while the change is pending"),
    ] {
        let row = alice.row_at(&GROUP, seq);
        assert_eq!((row.status, row.body.as_str()), (0, body));
        assert_eq!(row.sender_user, Some(bob.user));
    }
    let retained: i64 = alice
        .probe
        .lock()
        .unwrap()
        .query_row("SELECT count(*) FROM app_pending_history", [], |r| r.get(0))
        .unwrap();
    assert_eq!(retained, 0, "released rows drop their retained evidence");
}

/// A same-key device that sent before the list naming it was ingested: its message waits and is
/// released by the revalidation that follows the ingest.
#[test]
fn fixa_a_listed_devices_earlier_message_is_released_by_revalidation() {
    let (_, mut relay, mut alice, bob, v1) = sec_raw_bob();
    let mut added = RawPeer::new_with_keys(0xb2, 0xd4, 0xb2, 0xf2);
    added.join_external(&mut relay);
    alice.sync(&relay);
    let early = added.send(&mut relay, "from my new laptop");
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, early).reason, "E_DEVICE_UNLISTED");
    let v2 = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&added)],
        0xf2,
    );
    sec_ingest(&mut alice, &relay, &bob.user, added.device, &[&v1, &v2]);
    let released = alice
        .core
        .revalidate_pending(&GROUP, relay.leaf_of(added.device), 256)
        .expect("revalidate");
    assert_eq!(released, 1);
    assert_eq!(alice.row_at(&GROUP, early).body, "from my new laptop");
}

/// Ruling 3, never-cached user: a proposed Add whose signed history was fetched and failed is
/// left out of this device's commit instead of reading as an unfetched history, which refused
/// every commit (an instance, or a member proposing a device under a fresh fake user, could stall
/// all members' commits). An Add whose history was never fetched still asks for the fetch.
#[test]
fn fixa_a_fetched_and_failed_add_of_a_never_cached_user_does_not_stall_commits() {
    let (instance, mut relay, mut alice, bob) = alice_and_bob();
    ingest_core_list(&mut alice, &relay, &bob);
    let forged = RawPeer::new_with_keys(0xd7, 0xd8, 0xd5, 0xd6);
    let listed = RawPeer::new_with_keys(0xd7, 0xd9, 0xd5, 0xd6);
    instance.propose_add(&mut relay, &sec_key_package(&forged));
    alice.sync(&relay);
    let proposals = relay.proposals_body();
    assert_eq!(
        code(alice.core.commit_build(&GROUP, &proposals)),
        "E_DEVICE_LIST_STALE",
        "an unfetched history asks for the fetch"
    );
    let v1 = sec_v1_of(&listed, 0xd6);
    assert_eq!(
        code(alice.core.device_list_ingest_add(
            &GROUP,
            &proposals,
            0,
            &forged.user,
            &sec_history(&[&v1]),
            &v1.encode(),
            NOW + 3,
        )),
        "E_DEVICE_UNLISTED"
    );
    let body = alice
        .core
        .commit_build(&GROUP, &proposals)
        .expect("the failed Add does not stall the commit");
    let welcomes = decode_strict(&body, |d| {
        d.array(5)?;
        d.uint()?;
        d.bytes()?;
        d.bytes()?;
        let n = d.array_len()?;
        for _ in 0..n {
            d.array(2)?;
            d.bytes()?;
            d.bytes()?;
        }
        d.null()?;
        Ok(n)
    })
    .expect("commit_build body");
    assert_eq!(welcomes, 0, "no Welcome is addressed to the failed device");
    relay.commit(&body).expect("commit accepted");
    alice.core.commit_confirm(&GROUP).expect("commit_confirm");
    assert_eq!(sec_send(&mut alice), "OK");
}

/// The bytes a group id leaves inside the openmls tables' `group_id` key (client_migrate.rs).
fn fixr_stored_id(id: &[u8; 16]) -> Vec<u8> {
    id.iter().flat_map(|b| [0x18, *b]).collect()
}

/// R3: one active group whose MLS state does not load no longer fails trust_state as a whole
/// (the page then kept its last slice, which could read verified). That group answers from its
/// stored verdict rows, as send_block_code does, so a blocked contact stays blocked (fail closed).
#[test]
fn fixr_trust_state_reads_an_unloadable_groups_stored_verdicts() {
    let (_, mut relay, mut alice, bob) = alice_and_bob();
    ingest_core_list(&mut alice, &relay, &bob);
    let umk = sec_cached_umk(&alice, &bob.user);
    alice.core.trust_verify(&bob.user, &umk).expect("verified");
    let mut forged = RawPeer::new_with_keys(0xb2, 0xd3, 0xd5, 0xd6);
    forged.join_external(&mut relay);
    alice.sync(&relay);
    assert_eq!(sec_send(&mut alice), "E_CREDENTIAL");
    let alice = alice.reopen();
    let changed = alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE openmls_group_data SET group_data = x'00' \
             WHERE data_type = 'group_state' AND instr(group_id, ?1) > 0",
            [fixr_stored_id(&GROUP)],
        )
        .unwrap();
    assert_eq!(changed, 1, "GROUP's group_state row");
    assert_eq!(
        fixa_row(&alice, &bob.user),
        Some((umk, 1, None, Some("E_CREDENTIAL".to_owned()))),
        "trust_state answers, and the stored verdict of the unloadable group still blocks Bob"
    );
    assert_eq!(
        alice.core.send_block_code(&GROUP).unwrap(),
        Some("E_CREDENTIAL")
    );
}

/// R4: once the approval is saved, trust_verify answers success. A revalidation of held messages
/// that fails afterwards (here the store's pending-history index is gone) is left to the next
/// revalidation; before, the call answered an error although the key was already verified.
#[test]
fn fixr_trust_verify_succeeds_once_saved_when_revalidation_fails() {
    let (_, mut relay, mut alice, bob, v1) = sec_raw_bob();
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    let v2 = sec_next(&v1, vec![sec_entry(&rotated)], 0xd6);
    sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v1, &v2]);
    let held = rotated.send(&mut relay, "sent while the change is pending");
    alice.sync(&relay);
    assert_eq!(alice.row_at(&GROUP, held).reason, "E_UMK_CHANGED");
    alice
        .probe
        .lock()
        .unwrap()
        .execute("DROP INDEX app_pending_history_by_leaf_seq", [])
        .unwrap();
    let k2 = UmkSigner::from_bytes(&[0xd5; 32]).public();
    alice
        .core
        .trust_verify(&bob.user, &k2)
        .expect("the saved approval is reported as done");
    let (umk, verified, changed, _) = fixa_row(&alice, &bob.user).expect("Bob's row");
    assert_eq!(
        (umk, verified, changed),
        (k2, 1, None),
        "the approval is saved"
    );
    assert_eq!(
        alice.row_at(&GROUP, held).status,
        1,
        "the failed revalidation released nothing"
    );
}

/// The own user's cached device list rows, by UMK.
fn fixr_own_lists(alice: &Core) -> Vec<[u8; 32]> {
    let db = alice.probe.lock().unwrap();
    let mut stmt = db
        .prepare("SELECT umk_pub FROM app_device_lists WHERE user_id=?1")
        .unwrap();
    stmt.query_map([alice.user.as_slice()], |r| r.get(0))
        .unwrap()
        .collect::<Result<_, _>>()
        .unwrap()
}

/// R5: the open-time repair of the own pin also drops an own device list cached under another
/// UMK (an impostor's, pinned by a store attacked before F3). Otherwise every real own device
/// failed against the cached impostor key, and ingest anchored on the impostor's head. A cached
/// own list under the own key is kept.
#[test]
fn fixr_the_own_pin_repair_drops_an_impostors_cached_own_list() {
    let (_, relay, alice, mut bob) = alice_and_bob();
    let own = fixa_own_umk(&alice, &mut bob, &relay);
    let insert = |alice: &Core, umk: [u8; 32]| {
        alice
            .probe
            .lock()
            .unwrap()
            .execute(
                "INSERT OR REPLACE INTO app_device_lists(user_id,version,list,umk_pub,ssk_pub) \
                 VALUES(?1,7,x'00',?2,?3)",
                rusqlite::params![
                    alice.user.as_slice(),
                    umk.as_slice(),
                    [0xe4u8; 32].as_slice()
                ],
            )
            .unwrap();
    };
    insert(&alice, [0xe2; 32]);
    alice
        .probe
        .lock()
        .unwrap()
        .execute(
            "UPDATE app_trust_pins SET umk_pub=?2,verified=0 WHERE user_id=?1",
            rusqlite::params![alice.user.as_slice(), [0xe2u8; 32].as_slice()],
        )
        .unwrap();
    let alice = alice.reopen();
    assert_eq!(fixa_row(&alice, &alice.user), Some((own, 1, None, None)));
    assert_eq!(
        fixr_own_lists(&alice),
        Vec::<[u8; 32]>::new(),
        "the impostor's own list is dropped"
    );
    // Also when the pin itself was already right.
    insert(&alice, [0xe2; 32]);
    let alice = alice.reopen();
    assert_eq!(fixr_own_lists(&alice), Vec::<[u8; 32]>::new());
    insert(&alice, own);
    let alice = alice.reopen();
    assert_eq!(
        fixr_own_lists(&alice),
        vec![own],
        "a list under the own key stays"
    );
}

/// R1: the page names the own account as the reason for a send block only when the leaves that
/// hold the block's code are all the own user's (an own-user leaf under another key); a
/// contact's failed or changed leaf is never read as the own account's.
#[test]
fn fixr_send_block_own_names_an_own_account_block() {
    let (instance, mut relay, mut alice, mut bob) = alice_and_bob();
    fixa_own_umk(&alice, &mut bob, &relay);
    ingest_core_list(&mut alice, &relay, &bob);
    assert!(
        !alice.core.send_block_own(&GROUP).unwrap(),
        "nothing blocks"
    );
    let mut forged = RawPeer::new_with_keys(0xb2, 0xd3, 0xd5, 0xd6);
    forged.join_external(&mut relay);
    alice.sync(&relay);
    assert_eq!(
        alice.core.send_block_code(&GROUP).unwrap(),
        Some("E_CREDENTIAL")
    );
    assert!(
        !alice.core.send_block_own(&GROUP).unwrap(),
        "a contact's failed leaf"
    );
    let mut impostor = RawPeer::new_with_keys(0xa1, 0xe5, 0xe2, 0xe1);
    impostor.join_external(&mut relay);
    alice.sync(&relay);
    assert_eq!(
        alice.core.send_block_code(&GROUP).unwrap(),
        Some("E_UMK_CHANGED")
    );
    assert!(
        alice.core.send_block_own(&GROUP).unwrap(),
        "the changed-key block is held by an own-user leaf only"
    );
    let mut alice = alice.reopen();
    assert!(
        alice.core.send_block_own(&GROUP).unwrap(),
        "read from the store"
    );
    instance.propose_remove(&mut relay, impostor.device);
    alice.commit(&mut relay);
    assert_eq!(
        alice.core.send_block_code(&GROUP).unwrap(),
        Some("E_CREDENTIAL")
    );
    assert!(!alice.core.send_block_own(&GROUP).unwrap());
}

/// Task-10 concern 1, the other old-key paths: while Bob's key change is pending, his old-key
/// device keeps sending and committing in the text group, and its list is refreshed. None of it
/// (sync of its message, processing its commit, the list refresh, the send gate) ends the pending
/// change or rebinds the cached list away from the changed key; the changed key stays verifiable.
#[test]
fn sec_old_key_traffic_and_refresh_never_end_a_pending_change() {
    let (_, mut relay, mut alice, mut bob, v1) = sec_raw_bob();
    let old_key = UmkSigner::from_bytes(&[0xb2; 32]).public();
    let new_key = UmkSigner::from_bytes(&[0xd5; 32]).public();
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xf2);
    let join = rotated.join_external(&mut relay);
    bob.apply_commit(&relay, join);
    alice.sync(&relay);
    let v2 = sec_next(
        &v1,
        vec![v1.unsigned.entries[0].clone(), sec_entry(&rotated)],
        0xf2,
    );
    let (_, changed, _) = sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v1, &v2]);
    assert_eq!(changed, 1);
    let pending = Some((old_key, 0, Some(new_key)));
    assert_eq!(sec_state_of(&alice, &bob.user), pending);

    bob.send(&mut relay, "still the old device");
    alice.sync(&relay);
    assert_eq!(sec_state_of(&alice, &bob.user), pending, "a message");
    let update = bob.self_update(&mut relay);
    rotated.apply_commit(&relay, update);
    alice.sync(&relay);
    assert_eq!(sec_state_of(&alice, &bob.user), pending, "a commit");
    assert_eq!(sec_send(&mut alice), "E_UMK_CHANGED");
    assert_eq!(sec_state_of(&alice, &bob.user), pending, "the send gate");
    let (_, changed, _) = sec_ingest(&mut alice, &relay, &bob.user, bob.device, &[&v2]);
    assert_eq!(changed, 0);
    assert_eq!(sec_state_of(&alice, &bob.user), pending, "the list refresh");
    assert_eq!(sec_send(&mut alice), "E_UMK_CHANGED");

    alice
        .core
        .trust_verify(&bob.user, &new_key)
        .expect("the changed key stays verifiable");
    assert_eq!(sec_state_of(&alice, &bob.user), Some((new_key, 1, None)));
}

// ---- Task S1 (Ruling AW): the account-kind attestation is independent of the key ----

fn attestation_for(user: &[u8; 16], kind: u8) -> Vec<u8> {
    use ed25519_dalek::Signer;
    let key = ed25519_dalek::SigningKey::from_bytes(&[0x41; 32]);
    assert_eq!(key.verifying_key().to_bytes(), INSTANCE_SIGNER_PUB);
    let message = dilla_core::identity::account_kind_message(&INSTANCE, user, kind);
    dilla_core::identity::AccountKindAttestation {
        key_id: [0x53; 16],
        kind,
        signature: key.sign(&message).to_bytes(),
    }
    .encode()
}

fn quarantined(answer: &[u8]) -> u64 {
    decode_strict(answer, |d| {
        d.array(3)?;
        d.uint()?;
        d.uint()?;
        d.uint()
    })
    .expect("verdict")
}

#[test]
fn a_human_attested_as_a_human_is_trusted_and_one_attested_as_a_bot_is_refused() {
    for (kind, want) in [(0u8, 0u64), (1, 1)] {
        let (_, relay, mut alice, bob) = alice_and_bob();
        alice
            .core
            .account_kind_ingest(&bob.user, &attestation_for(&bob.user, kind))
            .expect("attested");
        assert!(alice.core.account_kind_seen().expect("seen"));
        let (history, head) = list_wire(&bob);
        let answer = alice
            .core
            .device_list_ingest(
                &GROUP,
                relay.leaf_of(bob.device),
                &bob.user,
                &history,
                &head,
                NOW,
            )
            .expect("ingest");
        assert_eq!(
            quarantined(&answer),
            want,
            "kind {kind}: E_CREDENTIAL only for the wrong kind"
        );
    }
}

#[test]
fn once_seen_a_user_without_an_attestation_stays_pending() {
    let (_, mut relay, mut alice, mut bob) = alice_and_bob();
    // Seen through another account's attestation.
    alice
        .core
        .account_kind_ingest(&[0x5c; 16], &attestation_for(&[0x5c; 16], 0))
        .expect("own attestation");
    let (history, head) = list_wire(&bob);
    alice
        .core
        .device_list_ingest(
            &GROUP,
            relay.leaf_of(bob.device),
            &bob.user,
            &history,
            &head,
            NOW,
        )
        .expect("ingest");
    let (_, seq) = bob.send(&mut relay, &GROUP, "no attestation for me", NOW + 1);
    alice.sync(&relay);
    assert_eq!(
        alice.row_at(&GROUP, seq).status,
        1,
        "pending: no attestation for bob"
    );
}

/// A rotated user (A -> B): a contact who pinned A still goes through the key-change flow, and the
/// same attestation (it binds no key) holds for B's leaves once B is approved. A new contact who
/// never saw A trusting B's leaves with the attestation is pinned by
/// `trust::tests::the_attestation_applies_to_every_key_of_the_user` and end to end by
/// e2e/voice/blocked.spec.ts (a).
#[test]
fn a_rotated_user_changes_key_for_an_old_contact_and_the_attestation_holds_for_the_new_key() {
    let (_, mut relay, mut alice, bob) = alice_and_bob();
    let v1 = sec_bob_v1(&mut alice, &relay, &bob);
    let original = sec_cached_umk(&alice, &bob.user);
    alice
        .core
        .account_kind_ingest(&bob.user, &attestation_for(&bob.user, 0))
        .expect("attested");
    let mut rotated = RawPeer::new_with_keys(0xb2, 0xd4, 0xd5, 0xd6);
    let changed_umk = UmkSigner::from_bytes(&[0xd5; 32]).public();
    rotated.join_external(&mut relay);
    alice.sync(&relay);
    // The old contact: the same attestation; the key change is recorded and B waits for approval.
    alice
        .core
        .account_kind_ingest(&bob.user, &attestation_for(&bob.user, 0))
        .expect("attested again");
    let v2 = sec_next(&v1, vec![sec_entry(&rotated)], 0xd6);
    let (_, changed, _) = sec_ingest(&mut alice, &relay, &bob.user, rotated.device, &[&v1, &v2]);
    assert_eq!(changed, 1, "the key change is recorded");
    assert_eq!(sec_send(&mut alice), "E_UMK_CHANGED");
    let (pinned, verified, pending) = sec_state_of(&alice, &bob.user).expect("bob state");
    assert_eq!(
        (pinned, verified, pending),
        (original, 0, Some(changed_umk))
    );
    let seq = rotated.send(&mut relay, "under B before approval");
    alice.sync(&relay);
    assert_eq!(
        alice.row_at(&GROUP, seq).status,
        1,
        "pending while B is unapproved"
    );
    alice
        .core
        .trust_verify(&bob.user, &changed_umk)
        .expect("approve B");
    let held = alice.row_at(&GROUP, seq);
    assert_eq!(
        (held.status, held.body.as_str()),
        (0, "under B before approval"),
        "released by the approval, the attestation holding for B"
    );
}
