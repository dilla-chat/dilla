// Native only, like tests/mls_roundtrip.rs: every test opens an in-memory SQLite connection.
#![cfg(not(target_arch = "wasm32"))]

//! Task 5: `ClientCore` groups, sending and reading, exercised by two and three cores exchanging
//! the bodies they produce through an in-test delivery service.

mod client_support;

use client_support::*;
use dilla_core::cbor::{Encoder, decode_strict};
use dilla_core::client::{ClientCore, ClientError};
use dilla_core::mls::DillaBinding;

/// The tree body with its tree hash's first byte flipped.
fn forge_tree_hash(tree_body: &[u8]) -> Vec<u8> {
    let (epoch, tree, mut hash) = decode_strict(tree_body, |d| {
        d.array(3)?;
        Ok((d.uint()?, d.bytes()?.to_vec(), d.bytes()?.to_vec()))
    })
    .expect("tree body");
    hash[0] ^= 0x01;
    let mut e = Encoder::new();
    e.array(3).uint(epoch).bytes(&tree).bytes(&hash);
    e.into_vec()
}

#[test]
fn a_created_group_registers_with_its_binding_and_lists_in_each_state() {
    let instance = Instance::generate();
    let mut relay = Relay::starting_at(GROUP, 10);
    let mut a = ready_core(0xa1, "alice");

    let body = a
        .core
        .group_create(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &instance.public(),
        )
        .expect("group_create");
    let (group_id, binding) = decode_strict(&body, |d| {
        d.array(4)?;
        let group_id = d.bytes_exact::<16>()?;
        let binding = d.bytes()?.to_vec();
        d.bytes()?;
        d.bytes()?;
        Ok((group_id, binding))
    })
    .expect("POST /v1/groups body");
    assert_eq!(group_id, GROUP);
    assert_eq!(
        DillaBinding::decode(&binding).expect("binding"),
        text_binding(CHANNEL)
    );
    assert_eq!(
        groups(&a.core),
        vec![GroupRow {
            group_id: GROUP,
            kind: 0,
            community_id: Some(COMMUNITY),
            target_id: CHANNEL,
            state: 0,
            epoch: 0,
            next_seq: 1,
            proposals_pending: 0,
            pending_commit: 0,
        }]
    );

    let created = relay
        .register(&body)
        .expect("the delivery service accepts the GroupInfo, the tree and the binding");
    let next_seq = decode_strict(&created, |d| {
        d.array(2)?;
        d.bytes_exact::<16>()?;
        d.uint()
    })
    .expect("201 body");
    assert_eq!(next_seq, 10);

    // While GROUP stands, neither another group for CHANNEL nor GROUP for another channel.
    assert_eq!(
        code(a.core.group_create(
            &OTHER_GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &instance.public()
        )),
        "E_CORE_STATE"
    );
    assert_eq!(
        code(a.core.group_create(
            &GROUP,
            Some(&COMMUNITY),
            &OTHER_CHANNEL,
            POLICY,
            &instance.public()
        )),
        "E_CORE_STATE"
    );

    assert_eq!(code(a.core.group_registered(&GROUP, 0)), "E_CORE_INPUT");
    a.core
        .group_registered(&GROUP, next_seq)
        .expect("group_registered");
    let row = a.group(&GROUP).expect("row");
    assert_eq!((row.state, row.next_seq, row.epoch), (2, 10, 0));
    assert_eq!(
        code(a.core.group_registered(&GROUP, next_seq)),
        "E_CORE_STATE"
    );
    assert_eq!(
        code(a.core.group_registered(&OTHER_GROUP, 1)),
        "E_CORE_NOT_FOUND"
    );
}

#[test]
fn a_registration_that_lost_is_discarded_and_leaves_nothing() {
    let instance = Instance::generate();
    let mut a = ready_core(0xa1, "alice");
    a.core
        .group_create(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &instance.public(),
        )
        .expect("create");
    a.core.group_discard(&GROUP).expect("discard");
    assert!(groups(&a.core).is_empty());
    assert_eq!(code(a.core.group_discard(&GROUP)), "E_CORE_NOT_FOUND");

    // The MLS group went with the row: the same group id can be created again.
    a.core
        .group_create(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &instance.public(),
        )
        .expect("the group id and the channel are free again");
    a.core.group_discard(&GROUP).expect("discard");

    let mut relay = Relay::new(OTHER_GROUP);
    a.create_and_register(&mut relay, &instance);
    assert_eq!(code(a.core.group_discard(&OTHER_GROUP)), "E_CORE_STATE");
    assert_eq!(a.group(&OTHER_GROUP).map(|g| g.state), Some(2));
}

/// web3.5 R1: a discarded group takes its role view with it, so a later group with the same id
/// starts under the former rule until the view is pushed again.
#[test]
fn a_discarded_group_leaves_no_authority_rows() {
    let instance = Instance::generate();
    let mut a = ready_core(0xa1, "alice");
    a.core
        .group_create(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &instance.public(),
        )
        .expect("create");
    a.core
        .authority_set(&GROUP, &[0xe5; 16], &[0xe5; 16], &[0x3c; 16])
        .expect("a view");
    a.core
        .authority_set(&OTHER_GROUP, &[0xe5; 16], &[], &[])
        .expect("another group's view");
    {
        let db = a.probe.lock().expect("lock");
        db.execute(
            "INSERT INTO app_pins (group_id, target, seq, by_user) VALUES (?1, ?2, 2, ?2)",
            [GROUP.as_slice(), [0x71u8; 16].as_slice()],
        )
        .expect("a pin");
        db.execute(
            "INSERT INTO app_reactions (group_id, target, user_id, emoji, seq) VALUES (?1, ?2, ?2, 'x', 3)",
            [GROUP.as_slice(), [0x71u8; 16].as_slice()],
        )
        .expect("a reaction");
    }
    a.core.group_discard(&GROUP).expect("discard");
    let rows = |sql: &str| -> i64 {
        a.probe
            .lock()
            .expect("lock")
            .query_row(sql, [GROUP.as_slice()], |r| r.get(0))
            .expect("count")
    };
    assert_eq!(
        rows("SELECT count(*) FROM app_authority WHERE group_id = ?1"),
        0
    );
    assert_eq!(
        rows("SELECT count(*) FROM app_authority_known WHERE group_id = ?1"),
        0
    );
    assert_eq!(
        rows("SELECT count(*) FROM app_authority_mentionable WHERE group_id = ?1"),
        0
    );
    assert_eq!(rows("SELECT count(*) FROM app_pins WHERE group_id = ?1"), 0);
    assert_eq!(
        rows("SELECT count(*) FROM app_reactions WHERE group_id = ?1"),
        0
    );
    let other: i64 = a
        .probe
        .lock()
        .expect("lock")
        .query_row(
            "SELECT (SELECT count(*) FROM app_authority) + (SELECT count(*) FROM app_authority_known)",
            [],
            |r| r.get(0),
        )
        .expect("count");
    assert_eq!(other, 2, "another group's view stays");
}

/// A DM has no roles: the core refuses a role view for a DM group.
#[test]
fn a_dm_group_takes_no_role_view() {
    let instance = Instance::generate();
    let mut a = ready_core(0xa1, "alice");
    a.core
        .group_create(&DM_GROUP, None, &DM_CHANNEL, POLICY, &instance.public())
        .expect("a DM group");
    assert_eq!(
        code(a.core.authority_set(&DM_GROUP, &[0xe5; 16], &[], &[])),
        "E_CORE_STATE"
    );
    let rows: i64 = a
        .probe
        .lock()
        .expect("lock")
        .query_row(
            "SELECT (SELECT count(*) FROM app_authority) + (SELECT count(*) FROM app_authority_known)",
            [],
            |r| r.get(0),
        )
        .expect("count");
    assert_eq!(rows, 0);
}

#[test]
fn an_external_join_reaches_active_and_both_sides_read_each_other() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut a = ready_core(0xa1, "alice");
    let mut b = ready_core(0xb2, "bob");
    a.create_and_register(&mut relay, &instance);
    b.join_external(&mut relay);

    let joined = b.group(&GROUP).expect("bob's row");
    assert_eq!((joined.state, joined.epoch), (2, 1));
    assert_eq!(
        joined.next_seq,
        relay.through() + 1,
        "the external commit's seq + 1"
    );

    let applied = a.sync(&relay);
    assert_eq!((applied.state, applied.epoch), (2, 1));
    assert_eq!(applied.flags & EPOCH_CHANGED, EPOCH_CHANGED);
    assert!(applied.new_seqs.is_empty());
    assert_eq!(applied.next_seq, relay.through() + 1);
    ingest_core_list(&mut b, &relay, &a);
    ingest_core_list(&mut a, &relay, &b);

    let (msg_id, seq) = a.send(
        &mut relay,
        &GROUP,
        "the wolf capes are in the chest",
        NOW + 1,
    );
    let got = b.sync(&relay);
    assert_eq!(got.new_seqs, vec![seq]);
    assert_eq!(got.flags, 0);
    let rows = b.timeline(&GROUP);
    assert_eq!(rows.len(), 1);
    let row = &rows[0];
    assert_eq!(row.seq, seq);
    assert_eq!(row.epoch, 1);
    assert_eq!(row.recv_ts, NOW + seq);
    assert_eq!(row.status, 0);
    assert_eq!(row.reason, "");
    assert_eq!(row.sender_user, Some(a.user));
    assert_eq!(row.sender_device, a.device);
    assert_eq!(row.sender_kind, Some(0));
    assert_eq!(row.sender_tier, Some(1), "a browser-tier device");
    assert_eq!(row.msg_id, Some(msg_id));
    assert_eq!(row.ty, Some(0));
    assert_eq!(row.body, "the wolf capes are in the chest");

    let (_, back) = b.send(&mut relay, &GROUP, "on my way", NOW + 2);
    let applied = a.sync(&relay);
    assert_eq!(
        applied.new_seqs,
        vec![back],
        "the own row at `seq` is skipped, not re-listed"
    );
    let rows = a.timeline(&GROUP);
    assert_eq!(
        rows.iter().map(|r| r.seq).collect::<Vec<_>>(),
        vec![seq, back]
    );
    assert_eq!(rows[0].sender_device, a.device);
    assert_eq!(rows[0].body, "the wolf capes are in the chest");
    assert_eq!(rows[1].sender_user, Some(b.user));
    assert_eq!(rows[1].sender_device, b.device);
    assert_eq!(rows[1].body, "on my way");
}

#[test]
fn an_external_join_with_info_and_tree_that_disagree_is_refused_and_leaves_nothing() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut a = ready_core(0xa1, "alice");
    a.create_and_register(&mut relay, &instance);
    let stale_tree = relay.tree_body(); // epoch 0
    let mut b = ready_core(0xb2, "bob");
    b.join_external(&mut relay); // the group moves to epoch 1

    let mut c = ready_core(0xc3, "carol");
    let join = |c: &mut Core, info: &[u8], tree: &[u8]| {
        c.core
            .group_join_external(&GROUP, Some(&COMMUNITY), &CHANNEL, POLICY, info, tree)
    };
    assert_eq!(
        code(join(&mut c, &relay.info_body(), &stale_tree)),
        "E_CORE_INPUT"
    );
    assert_eq!(
        code(join(
            &mut c,
            &relay.info_body(),
            &forge_tree_hash(&relay.tree_body())
        )),
        "E_CORE_INPUT"
    );
    assert_eq!(
        code(join(&mut c, &[0x80], &relay.tree_body())),
        "E_CORE_INPUT"
    );
    assert!(groups(&c.core).is_empty());

    c.join_external(&mut relay);
    assert_eq!(c.group(&GROUP).map(|g| (g.state, g.epoch)), Some((2, 2)));
}

#[test]
fn an_external_join_into_a_group_of_another_channel_is_refused_with_e_binding() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut a = ready_core(0xa1, "alice");
    a.create_and_register(&mut relay, &instance);

    let mut c = ready_core(0xc3, "carol");
    let err = c
        .core
        .group_join_external(
            &GROUP,
            Some(&COMMUNITY),
            &OTHER_CHANNEL,
            POLICY,
            &relay.info_body(),
            &relay.tree_body(),
        )
        .expect_err("the GroupInfo is bound to CHANNEL");
    assert_eq!(err.code, "E_BINDING");
    assert!(
        groups(&c.core).is_empty(),
        "a refused join leaves no row and no group"
    );

    c.join_external(&mut relay);
    assert_eq!(c.group(&GROUP).map(|g| g.state), Some(2));
}

#[test]
fn an_external_join_rejects_a_group_info_labelled_with_another_group_id() {
    let instance = Instance::generate();
    let mut relay = Relay::new(OTHER_GROUP);
    let mut creator = ready_core(0xa1, "alice");
    creator.create_and_register(&mut relay, &instance);
    let mut joiner = ready_core(0xb2, "bob");

    assert_eq!(
        code(joiner.core.group_join_external(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &relay.info_body(),
            &relay.tree_body(),
        )),
        "E_CORE_INPUT"
    );
    assert!(groups(&joiner.core).is_empty());
    assert!(groups(&joiner.reopen().core).is_empty());
}

#[test]
fn a_mislabelled_external_resync_preserves_the_existing_group() {
    let (_instance, _relay, _a, mut joiner) = alice_and_bob_trusted();
    let instance = Instance::generate();
    let mut other_relay = Relay::new(OTHER_GROUP);
    let mut creator = ready_core(0xc3, "carol");
    creator.create_and_register(&mut other_relay, &instance);
    // OTHER_GROUP's GroupInfo is at epoch 2, above the joiner's floor of 1: only the group id
    // tells it apart from a valid resync input.
    creator.commit(&mut other_relay);
    creator.commit(&mut other_relay);
    assert_eq!(other_relay.epoch(), 2);

    assert_eq!(
        code(joiner.core.group_join_external(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &other_relay.info_body(),
            &other_relay.tree_body(),
        )),
        "E_CORE_INPUT"
    );
    assert_eq!(
        joiner.group(&GROUP).map(|row| (row.state, row.epoch)),
        Some((2, 1))
    );
    let joiner = joiner.reopen();
    assert_eq!(
        joiner.group(&GROUP).map(|row| (row.state, row.epoch)),
        Some((2, 1))
    );
}

#[test]
fn a_resync_replaces_the_local_group_and_a_discarded_resync_returns_to_needs_resync() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    a.send(&mut relay, &GROUP, "before the resync", NOW + 1);
    b.sync(&relay);

    // Bob starts a resync and abandons it.
    b.core
        .group_join_external(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &relay.info_body(),
            &relay.tree_body(),
        )
        .expect("resync body");
    assert_eq!(b.group(&GROUP).map(|g| g.state), Some(1));
    assert_eq!(
        code(
            b.core
                .send_prepare(&GROUP, &request(0, None, "while joining", &[]), NOW + 2)
        ),
        "E_CORE_STATE"
    );
    assert_eq!(
        code(b.core.group_create(
            &OTHER_GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &[0x01; 32]
        )),
        "E_CORE_STATE"
    );
    b.core.group_discard(&GROUP).expect("discard");
    let row = b.group(&GROUP).expect("the row stays");
    assert_eq!(row.state, 3);
    assert_eq!(
        row.epoch, 0,
        "the old MLS group was deleted when the resync began"
    );
    assert_eq!(code(b.try_sync(&relay)), "E_CORE_STATE");

    // And completes one.
    b.join_external(&mut relay);
    let row = b.group(&GROUP).expect("row");
    assert_eq!((row.state, row.epoch), (2, 2));
    assert_eq!(a.sync(&relay).epoch, 2);
    a.send(&mut relay, &GROUP, "after the resync", NOW + 3);
    b.sync(&relay);
    let bodies: Vec<String> = b.timeline(&GROUP).into_iter().map(|r| r.body).collect();
    assert_eq!(bodies, vec!["before the resync", "after the resync"]);
}

#[test]
fn a_welcome_joins_against_the_expected_binding_and_reports_every_outcome() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut peer = RawPeer::new(0xe5, 0xe6);
    peer.create(&mut relay, &instance);
    let mut c = ready_core(0xc3, "carol");
    let mut d = ready_core(0xd4, "dave");
    let c_kp = c.first_key_package();
    let d_kp = d.first_key_package();
    peer.add(&mut relay, &[c_kp.as_slice(), d_kp.as_slice()]);
    let commit_seq = relay.welcomes[0].commit_seq;
    let expected = expected_body(&[(GROUP, COMMUNITY, CHANNEL, POLICY)]);

    let outcome = |welcome_id: u64, outcome: u64, reason: &str| WelcomeOutcome {
        welcome_id,
        group_id: GROUP,
        outcome,
        reason: reason.to_owned(),
    };

    // 3: a group the caller does not know is left alone.
    let unknown = c
        .core
        .welcomes_apply(&relay.welcomes_body(c.device), &expected_body(&[]))
        .expect("welcomes_apply");
    assert_eq!(decode_outcomes(&unknown), vec![outcome(1, 3, "")]);
    assert!(groups(&c.core).is_empty());

    // 0: joined, at the welcoming epoch, reading from the commit's seq + 1.
    let joined = c
        .core
        .welcomes_apply(&relay.welcomes_body(c.device), &expected)
        .expect("welcomes_apply");
    assert_eq!(decode_outcomes(&joined), vec![outcome(1, 0, "")]);
    let row = c.group(&GROUP).expect("row");
    assert_eq!(
        (
            row.state,
            row.epoch,
            row.next_seq,
            row.target_id,
            row.community_id,
            row.kind
        ),
        (2, 1, commit_seq + 1, CHANNEL, Some(COMMUNITY), 0)
    );

    // 1: already a member.
    let again = c
        .core
        .welcomes_apply(&relay.welcomes_body(c.device), &expected)
        .expect("welcomes_apply");
    assert_eq!(decode_outcomes(&again), vec![outcome(1, 1, "")]);

    // 2: a Welcome whose binding names another channel than the caller expects.
    let wrong = expected_body(&[(GROUP, COMMUNITY, OTHER_CHANNEL, POLICY)]);
    let refused = d
        .core
        .welcomes_apply(&relay.welcomes_body(d.device), &wrong)
        .expect("welcomes_apply");
    assert_eq!(decode_outcomes(&refused), vec![outcome(2, 2, "E_BINDING")]);
    assert!(
        groups(&d.core).is_empty(),
        "a refused Welcome leaves no row and no group"
    );
    // The refusal rolled back: the KeyPackage's private keys are still there to join with.
    let retried = d
        .core
        .welcomes_apply(&relay.welcomes_body(d.device), &expected)
        .expect("welcomes_apply");
    assert_eq!(decode_outcomes(&retried), vec![outcome(2, 0, "")]);

    assert_eq!(
        code(c.core.welcomes_apply(&[0xff], &expected)),
        "E_CORE_INPUT"
    );
    assert_eq!(
        code(
            c.core
                .welcomes_apply(&relay.welcomes_body(c.device), &[0x81, 0x80])
        ),
        "E_CORE_INPUT"
    );

    // Both read what the welcoming member sends.
    for member in [&mut c, &mut d] {
        ingest_list_body(
            member,
            &relay,
            peer.user,
            peer.device,
            &peer.device_list_body(),
        );
    }
    let seq = peer.send(&mut relay, "welcome aboard");
    for member in [&mut c, &mut d] {
        assert_eq!(member.sync(&relay).new_seqs, vec![seq]);
        let row = member.timeline(&GROUP).pop().expect("row");
        assert_eq!(row.status, 0, "reason {:?}", row.reason);
        assert_eq!(row.sender_user, Some(peer.user));
        assert_eq!(row.sender_device, peer.device);
        assert_eq!((row.sender_kind, row.sender_tier), (Some(0), Some(0)));
        assert_eq!(row.body, "welcome aboard");
    }
}

#[test]
fn a_welcome_labelled_with_another_group_id_preserves_the_stale_group() {
    let (_instance, _relay, _a, mut joiner) = alice_and_bob_trusted();
    let key_package = joiner.first_key_package();
    let instance = Instance::generate();
    let mut other_relay = Relay::new(OTHER_GROUP);
    let mut peer = RawPeer::new(0xe5, 0xe6);
    peer.create(&mut other_relay, &instance);
    peer.add(&mut other_relay, &[key_package.as_slice()]);
    other_relay.group_id = GROUP; // The delivery service mislabels OTHER_GROUP's Welcome.

    joiner
        .probe
        .lock()
        .expect("connection")
        .execute(
            "UPDATE app_groups SET state=3 WHERE group_id=?1",
            [GROUP.as_slice()],
        )
        .expect("mark the existing group as needing resync");
    let outcome = joiner
        .core
        .welcomes_apply(
            &other_relay.welcomes_body(joiner.device),
            &expected_body(&[(GROUP, COMMUNITY, CHANNEL, POLICY)]),
        )
        .expect("outcome");
    assert_eq!(
        decode_outcomes(&outcome),
        vec![WelcomeOutcome {
            welcome_id: 1,
            group_id: GROUP,
            outcome: 2,
            reason: "E_BINDING".to_owned(),
        }]
    );
    assert_eq!(
        joiner.group(&GROUP).map(|row| (row.state, row.epoch)),
        Some((3, 1))
    );
    let joiner = joiner.reopen();
    assert_eq!(
        joiner.group(&GROUP).map(|row| (row.state, row.epoch)),
        Some((3, 1))
    );
}

#[test]
fn a_mislabelled_upload_is_stored_unreadable_with_e_sender_mismatch() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    let msg_id = b.prepare(&GROUP, "who sent this", NOW + 1);
    let (_, body) = b.encrypt(&msg_id);
    let mislabel = [0x77; 16];
    let answer = relay.post_message(mislabel, &body).expect("upload");
    let seq = seq_of_answer(&answer);

    let applied = a.sync(&relay);
    assert_eq!(applied.new_seqs, vec![seq]);
    let row = a.timeline(&GROUP).pop().expect("row");
    assert_eq!(row.seq, seq);
    assert_eq!(row.status, 1);
    assert_eq!(row.reason, "E_SENDER_MISMATCH");
    assert_eq!(row.sender_device, mislabel);
    assert_eq!(row.sender_user, None);
    assert_eq!(
        (row.sender_kind, row.sender_tier, row.msg_id, row.ty),
        (None, None, None, None)
    );
    assert_eq!(row.body, "");

    // The group carries on: an honestly labelled message reads.
    b.core.send_confirm(&msg_id, &answer).expect("confirm");
    let (_, honest) = b.send(&mut relay, &GROUP, "it was me", NOW + 2);
    assert_eq!(a.sync(&relay).new_seqs, vec![honest]);
    let row = a.timeline(&GROUP).pop().expect("row");
    assert_eq!(
        (row.status, row.sender_device, row.body.as_str()),
        (0, b.device, "it was me")
    );
}

#[test]
fn a_sender_whose_credential_does_not_decode_is_unreadable_with_e_credential() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut peer =
        RawPeer::with_identity(b"not a dilla credential".to_vec(), [0xe5; 16], [0xe6; 16]);
    peer.create(&mut relay, &instance);
    let mut c = ready_core(0xc3, "carol");
    let kp = c.first_key_package();
    peer.add(&mut relay, &[kp.as_slice()]);
    let joined = c
        .core
        .welcomes_apply(
            &relay.welcomes_body(c.device),
            &expected_body(&[(GROUP, COMMUNITY, CHANNEL, POLICY)]),
        )
        .expect("welcomes_apply");
    assert_eq!(decode_outcomes(&joined)[0].outcome, 0);

    let seq = peer.send(&mut relay, "unsigned words");
    assert_eq!(c.sync(&relay).new_seqs, vec![seq]);
    let row = c.timeline(&GROUP).pop().expect("row");
    assert_eq!(row.status, 1);
    assert_eq!(row.reason, "E_CREDENTIAL");
    assert_eq!(
        row.sender_device, [0xe6; 16],
        "the uploader the delivery service names"
    );
    assert_eq!(row.sender_user, None);
    assert_eq!(row.body, "");
}

#[test]
fn deleted_pruned_and_unknown_own_rows_are_stored_without_a_body() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    // Alice's own confirmed message, deleted before Alice applies its seq.
    let (_, own) = a.send(&mut relay, &GROUP, "regret", NOW + 1);
    relay.delete_message(own);
    // Bob's message, deleted before Alice ever saw it.
    let (_, gone) = b.send(&mut relay, &GROUP, "never seen", NOW + 2);
    relay.delete_message(gone);
    // A row whose ciphertext the instance pruned.
    let epoch = relay.epoch();
    let pruned = seq_of_answer(&relay.push_message(b.device, epoch, None, false));
    // A row the instance says this device uploaded, with nothing in flight here.
    let unknown =
        seq_of_answer(&relay.push_message(a.device, epoch, Some(vec![0x00, 0x01]), false));

    let applied = a.sync(&relay);
    assert_eq!(
        applied.new_seqs,
        vec![own, gone, pruned, unknown],
        "rules 2 and 3 list the deleted seqs (changed and inserted) and the pruned seq"
    );
    assert_eq!(applied.flags & OWN_ADOPTED, 0);
    let rows = a.timeline(&GROUP);
    let view: Vec<(u64, u64, String, [u8; 16], String)> = rows
        .iter()
        .map(|r| {
            (
                r.seq,
                r.status,
                r.reason.clone(),
                r.sender_device,
                r.body.clone(),
            )
        })
        .collect();
    assert_eq!(
        view,
        vec![
            (own, 2, String::new(), a.device, String::new()),
            (gone, 2, String::new(), b.device, String::new()),
            (pruned, 1, "E_PRUNED".to_owned(), b.device, String::new()),
            (
                unknown,
                1,
                "E_OWN_UNKNOWN".to_owned(),
                a.device,
                String::new()
            ),
        ]
    );
    assert_eq!(
        rows[0].sender_user,
        Some(a.user),
        "the update keeps the own row's sender"
    );
    assert!(rows[0].msg_id.is_some(), "and its msg_id");
    assert_eq!((rows[1].sender_user, rows[1].msg_id), (None, None));
}

#[test]
fn the_own_echo_is_adopted_from_the_outbox_and_send_confirm_reads_it_back() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    let msg_id = a.prepare(&GROUP, "lost answer", NOW + 1);
    let (_, body) = a.encrypt(&msg_id);
    let answer = relay.post_message(a.device, &body).expect("upload");
    let seq = seq_of_answer(&answer);

    // The answer is lost; the catch-up brings the row back first.
    let applied = a.sync(&relay);
    assert_eq!(applied.flags & OWN_ADOPTED, OWN_ADOPTED);
    assert_eq!(applied.new_seqs, vec![seq]);
    assert!(a.outbox(&GROUP).is_empty());
    let row = a.timeline(&GROUP).pop().expect("row");
    assert_eq!(row.seq, seq);
    assert_eq!(row.status, 0, "reason {:?}", row.reason);
    assert_eq!(
        (row.sender_user, row.sender_device),
        (Some(a.user), a.device)
    );
    assert_eq!((row.sender_kind, row.sender_tier), (Some(0), Some(1)));
    assert_eq!(row.msg_id, Some(msg_id));
    assert_eq!(row.body, "lost answer");
    assert_eq!((row.epoch, row.recv_ts), (1, NOW + seq));

    // A late answer only reads the adopted row back.
    assert_eq!(
        decode_confirm(&a.core.send_confirm(&msg_id, &answer).expect("confirm")),
        (GROUP, seq)
    );
    assert_eq!(a.timeline(&GROUP).len(), 1);
    assert_eq!(
        code(a.core.send_confirm(&[0x42; 16], &answer)),
        "E_CORE_NOT_FOUND"
    );

    // The usual order: confirmed first, then the echo is skipped.
    let (_, next) = a.send(&mut relay, &GROUP, "answered", NOW + 2);
    let applied = a.sync(&relay);
    assert!(applied.new_seqs.is_empty());
    assert_eq!(applied.flags & OWN_ADOPTED, 0);
    assert_eq!(applied.next_seq, next + 1);

    b.sync(&relay);
    let bodies: Vec<String> = b.timeline(&GROUP).into_iter().map(|r| r.body).collect();
    assert_eq!(bodies, vec!["lost answer", "answered"]);
}

#[test]
fn a_failed_row_insert_rolls_back_the_ratchet_and_the_retry_decrypts() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    let (_, seq) = b.send(&mut relay, &GROUP, "survives the failure", NOW + 1);
    let before = a.group(&GROUP).expect("row").next_seq;

    a.probe
        .lock()
        .expect("probe")
        .execute_batch(
            "CREATE TEMP TRIGGER fail_message_insert BEFORE INSERT ON app_messages \
             BEGIN SELECT RAISE(ABORT, 'injected failure'); END;",
        )
        .expect("trigger");
    let err = a.try_sync(&relay).expect_err("the row insert fails");
    assert_eq!(err.code, "E_CORE_STORAGE");
    assert_eq!(
        a.group(&GROUP).expect("row").next_seq,
        before,
        "the row's unit rolled back"
    );
    assert!(a.timeline(&GROUP).is_empty());

    a.probe
        .lock()
        .expect("probe")
        .execute_batch("DROP TRIGGER temp.fail_message_insert;")
        .expect("drop trigger");
    let applied = a.sync(&relay);
    assert_eq!(applied.new_seqs, vec![seq]);
    let row = a.timeline(&GROUP).pop().expect("row");
    assert_eq!(
        row.status, 0,
        "the ratchet step rolled back with the insert; reason {:?}",
        row.reason
    );
    assert_eq!(row.body, "survives the failure");
}

#[test]
fn one_message_per_group_is_in_flight_and_the_outbox_walks_its_states() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut a = ready_core(0xa1, "alice");
    a.create_and_register(&mut relay, &instance);

    let first = a.prepare(&GROUP, "first", NOW + 1);
    let second = a.prepare(&GROUP, "second", NOW + 2);
    assert_eq!(
        a.outbox(&GROUP),
        vec![
            OutboxRow {
                msg_id: first,
                state: 0,
                error: String::new(),
                created: NOW + 1,
                body: "first".into(),
                ty: 0,
                reply_to: None,
                attachments: vec![]
            },
            OutboxRow {
                msg_id: second,
                state: 0,
                error: String::new(),
                created: NOW + 2,
                body: "second".into(),
                ty: 0,
                reply_to: None,
                attachments: vec![]
            },
        ]
    );

    a.encrypt(&first);
    assert_eq!(
        code(a.core.send_encrypt(&second)),
        "E_CORE_STATE",
        "one in flight per group"
    );
    assert_eq!(
        code(a.core.send_encrypt(&first)),
        "E_CORE_STATE",
        "already in flight"
    );
    assert_eq!(code(a.core.send_discard(&first)), "E_CORE_STATE");
    assert_eq!(code(a.core.send_retry(&first)), "E_CORE_STATE");
    a.core.send_requeue(&first).expect("requeue");
    assert_eq!(code(a.core.send_requeue(&first)), "E_CORE_STATE");

    a.encrypt(&second);
    a.core
        .send_fail(&second, "E_FORBIDDEN")
        .expect("fail an in-flight row");
    assert_eq!(
        code(a.core.send_fail(&first, &"x".repeat(65))),
        "E_CORE_INPUT"
    );
    a.core
        .send_fail(&first, &"x".repeat(64))
        .expect("64 bytes is the bound");
    let states: Vec<(u64, String)> = a
        .outbox(&GROUP)
        .into_iter()
        .map(|o| (o.state, o.error))
        .collect();
    assert_eq!(
        states,
        vec![(2, "x".repeat(64)), (2, "E_FORBIDDEN".to_owned())]
    );

    a.core.send_retry(&second).expect("retry");
    let retried = &a.outbox(&GROUP)[1];
    assert_eq!((retried.state, retried.error.as_str()), (0, ""));
    a.core.send_discard(&first).expect("discard a failed row");
    a.core.send_discard(&second).expect("discard a queued row");
    assert!(a.outbox(&GROUP).is_empty());

    assert_eq!(code(a.core.send_encrypt(&first)), "E_CORE_NOT_FOUND");
    assert_eq!(code(a.core.send_requeue(&first)), "E_CORE_NOT_FOUND");
    assert_eq!(code(a.core.send_retry(&first)), "E_CORE_NOT_FOUND");
    assert_eq!(code(a.core.send_discard(&first)), "E_CORE_NOT_FOUND");
    assert_eq!(code(a.core.send_fail(&first, "E_X")), "E_CORE_NOT_FOUND");
    assert_eq!(code(a.core.outbox(&OTHER_GROUP)), "E_CORE_NOT_FOUND");
}

#[test]
fn send_prepare_checks_the_body_the_group_and_the_phase() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut a = ready_core(0xa1, "alice");
    a.create_and_register(&mut relay, &instance);

    assert_eq!(
        code(a.core.send_prepare(&GROUP, &request(0, None, "", &[]), NOW)),
        "E_CORE_INPUT"
    );
    assert_eq!(
        code(
            a.core
                .send_prepare(&GROUP, &request(0, None, " \n\t ", &[]), NOW)
        ),
        "E_CORE_INPUT"
    );
    assert_eq!(
        code(
            a.core
                .send_prepare(&GROUP, &request(0, None, &"a".repeat(4001), &[]), NOW)
        ),
        "E_ENVELOPE_LIMIT"
    );
    assert_eq!(
        code(
            a.core
                .send_prepare(&GROUP, &request(0, None, &"€".repeat(1334), &[]), NOW)
        ),
        "E_ENVELOPE_LIMIT",
        "the bound is 4000 UTF-8 bytes, not characters"
    );
    a.prepare(&GROUP, &"a".repeat(4000), NOW);
    a.prepare(&GROUP, &"€".repeat(1333), NOW);
    assert_eq!(a.outbox(&GROUP).len(), 2);

    assert_eq!(
        code(
            a.core
                .send_prepare(&OTHER_GROUP, &request(0, None, "hi", &[]), NOW)
        ),
        "E_CORE_NOT_FOUND"
    );
    a.core
        .group_create(
            &OTHER_GROUP,
            Some(&COMMUNITY),
            &OTHER_CHANNEL,
            POLICY,
            &instance.public(),
        )
        .expect("create");
    assert_eq!(
        code(
            a.core
                .send_prepare(&OTHER_GROUP, &request(0, None, "hi", &[]), NOW)
        ),
        "E_CORE_STATE"
    );

    let mut fresh = ClientCore::open(memory()).expect("open");
    assert_eq!(fresh.groups().expect("groups"), vec![0x80]);
    assert_eq!(
        code(fresh.send_prepare(&GROUP, &request(0, None, "hi", &[]), NOW)),
        "E_CORE_NO_IDENTITY"
    );
    assert_eq!(
        code(fresh.group_create(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &instance.public()
        )),
        "E_CORE_NO_IDENTITY"
    );
    assert_eq!(
        code(fresh.welcomes_apply(&[0x80], &[0x80])),
        "E_CORE_NO_IDENTITY"
    );
    assert_eq!(
        code(fresh.group_apply(&GROUP, &[0x80], &[0x80], 0)),
        "E_CORE_NO_IDENTITY"
    );
}

#[test]
fn the_timeline_pages_backwards_from_a_seq() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    let mut seqs = Vec::new();
    for i in 0..5u64 {
        seqs.push(
            b.send(&mut relay, &GROUP, &format!("message {i}"), NOW + i)
                .1,
        );
    }
    a.sync(&relay);
    let page = |before: u64, limit: u32| -> Vec<u64> {
        decode_timeline(&a.core.timeline(&GROUP, before, limit).expect("timeline"))
            .into_iter()
            .map(|r| r.seq)
            .collect()
    };
    assert_eq!(page(0, 2), seqs[3..].to_vec());
    assert_eq!(page(u64::MAX, 2), seqs[3..].to_vec());
    assert_eq!(page(seqs[3], 2), seqs[1..3].to_vec());
    assert_eq!(page(seqs[1], 200), vec![seqs[0]]);
    assert!(page(seqs[0], 200).is_empty());
    assert_eq!(code(a.core.timeline(&GROUP, 0, 0)), "E_CORE_INPUT");
    assert_eq!(code(a.core.timeline(&GROUP, 0, 201)), "E_CORE_INPUT");
    assert_eq!(
        code(a.core.timeline(&OTHER_GROUP, 0, 10)),
        "E_CORE_NOT_FOUND"
    );
}

#[test]
fn group_apply_refuses_malformed_rows_and_groups_that_are_not_active() {
    let (instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    let (_, seq) = b.send(&mut relay, &GROUP, "first", NOW + 1);
    let through = relay.through();

    assert_eq!(
        code(
            a.core
                .group_apply(&GROUP, &[0xff], &relay.messages_from(1), through)
        ),
        "E_CORE_INPUT"
    );
    assert_eq!(
        code(
            a.core
                .group_apply(&GROUP, &relay.handshakes_from(1), &[0x81, 0x80], through)
        ),
        "E_CORE_INPUT"
    );
    let duplicate = HsRow {
        seq,
        epoch: 1,
        kind: 0,
        sender: None,
        blob: vec![0x00],
    };
    assert_eq!(
        code(a.core.group_apply(
            &GROUP,
            &encode_handshakes(&[&duplicate]),
            &relay.messages_from(seq),
            through
        )),
        "E_CORE_INPUT"
    );
    let odd_kind = HsRow {
        seq: seq + 100,
        epoch: 1,
        kind: 3,
        sender: None,
        blob: vec![0x00],
    };
    assert_eq!(
        code(
            a.core
                .group_apply(&GROUP, &encode_handshakes(&[&odd_kind]), &[0x80], through)
        ),
        "E_CORE_INPUT"
    );
    let overflowing_epoch = MsgRow {
        seq,
        epoch: u64::MAX,
        uploader: b.device,
        blob: None,
        commitment: None,
        franking_tag: [0; 32],
        recv_ts: NOW,
        deleted: false,
        class: None,
    };
    assert_eq!(
        code(a.core.group_apply(
            &GROUP,
            &[0x80],
            &encode_messages(&[&overflowing_epoch]),
            through,
        )),
        "E_CORE_INPUT",
        "an epoch that cannot fit SQLite INTEGER is refused"
    );
    let overflowing_timestamp = MsgRow {
        epoch: 1,
        recv_ts: u64::MAX,
        ..overflowing_epoch
    };
    assert_eq!(
        code(a.core.group_apply(
            &GROUP,
            &[0x80],
            &encode_messages(&[&overflowing_timestamp]),
            through,
        )),
        "E_CORE_INPUT",
        "a timestamp that cannot fit SQLite INTEGER is refused"
    );
    assert!(
        a.timeline(&GROUP).is_empty(),
        "a refused call applies nothing"
    );

    assert_eq!(
        code(a.core.group_apply(&OTHER_GROUP, &[0x80], &[0x80], 0)),
        "E_CORE_NOT_FOUND"
    );
    a.core
        .group_create(
            &OTHER_GROUP,
            Some(&COMMUNITY),
            &OTHER_CHANNEL,
            POLICY,
            &instance.public(),
        )
        .expect("create");
    assert_eq!(
        code(a.core.group_apply(&OTHER_GROUP, &[0x80], &[0x80], 0)),
        "E_CORE_STATE"
    );

    // Rows below next_seq are ignored: a repeated catch-up changes nothing.
    let first = a.sync(&relay);
    assert_eq!(first.new_seqs, vec![seq]);
    let again = decode_applied(
        &a.core
            .group_apply(
                &GROUP,
                &relay.handshakes_from(1),
                &relay.messages_from(1),
                relay.through(),
            )
            .expect("again"),
    );
    assert!(again.new_seqs.is_empty());
    assert_eq!(again.next_seq, first.next_seq);

    // A row above `through` waits for a call that declares it.
    let (_, later) = b.send(&mut relay, &GROUP, "second", NOW + 2);
    let held = decode_applied(
        &a.core
            .group_apply(&GROUP, &[0x80], &relay.messages_from(later), later - 1)
            .expect("held"),
    );
    assert!(held.new_seqs.is_empty());
    assert_eq!(a.sync(&relay).new_seqs, vec![later]);
}

#[test]
fn a_reopened_core_reloads_its_groups_and_keeps_the_timeline() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    a.send(&mut relay, &GROUP, "before the reload", NOW + 1);
    b.sync(&relay);
    let groups_before = groups(&b.core);
    let timeline_before = b.timeline(&GROUP);

    let mut b = b.reopen();
    assert_eq!(groups(&b.core), groups_before);
    assert_eq!(b.timeline(&GROUP), timeline_before);
    let (_, seq) = b.send(&mut relay, &GROUP, "after the reload", NOW + 2);
    a.sync(&relay);
    let last = a.timeline(&GROUP).pop().expect("row");
    assert_eq!(
        (last.seq, last.status, last.body.as_str()),
        (seq, 0, "after the reload")
    );
}

#[test]
fn a_dm_group_carries_a_null_community_from_creation_to_both_sides_reading() {
    let instance = Instance::generate();
    let mut relay = Relay::new(DM_GROUP);
    let mut a = ready_core(0xa1, "alice");
    let mut b = ready_core(0xb2, "bob");
    let body = a
        .core
        .group_create(&DM_GROUP, None, &DM_CHANNEL, POLICY, &instance.public())
        .expect("group_create");
    let binding = decode_strict(&body, |d| {
        d.array(4)?;
        d.bytes_exact::<16>()?;
        let binding = d.bytes()?.to_vec();
        d.bytes()?;
        d.bytes()?;
        Ok(binding)
    })
    .expect("POST /v1/groups body");
    assert_eq!(
        DillaBinding::decode(&binding).expect("binding"),
        dm_binding(DM_CHANNEL)
    );
    let created = relay
        .register(&body)
        .expect("a null-community binding registers");
    let next_seq = decode_strict(&created, |d| {
        d.array(2)?;
        d.bytes_exact::<16>()?;
        d.uint()
    })
    .expect("201 body");
    a.core
        .group_registered(&DM_GROUP, next_seq)
        .expect("group_registered");
    b.join_external_in(&mut relay, None, &DM_CHANNEL);
    a.sync(&relay);
    ingest_core_list(&mut b, &relay, &a);
    ingest_core_list(&mut a, &relay, &b);
    for core in [&a, &b] {
        let row = core.group(&DM_GROUP).expect("row");
        assert_eq!(
            (row.kind, row.community_id, row.target_id, row.state),
            (0, None, DM_CHANNEL, 2)
        );
    }
    a.send(&mut relay, &DM_GROUP, "dm from alice", NOW + 1);
    b.sync(&relay);
    b.send(&mut relay, &DM_GROUP, "dm from bob", NOW + 2);
    a.sync(&relay);
    for core in [&a, &b] {
        let bodies: Vec<String> = core
            .timeline(&DM_GROUP)
            .into_iter()
            .map(|r| r.body)
            .collect();
        assert_eq!(bodies, vec!["dm from alice", "dm from bob"]);
    }
    // The NULL column survives a reload and still matches the stored group (take_group's check).
    let mut b = b.reopen();
    let (_, seq) = a.send(&mut relay, &DM_GROUP, "after the reload", NOW + 3);
    assert_eq!(b.sync(&relay).new_seqs, vec![seq]);
    // A row bound to no community is not a community channel's text group.
    assert_eq!(
        b.core
            .group_join_external(
                &DM_GROUP,
                Some(&COMMUNITY),
                &DM_CHANNEL,
                POLICY,
                &relay.info_body(),
                &relay.tree_body()
            )
            .expect_err("rebound"),
        ClientError {
            code: "E_CORE_STATE",
            detail: "group is bound to another channel".into()
        }
    );
}

#[test]
fn a_welcome_joins_a_dm_only_against_a_null_community_and_a_channel_only_against_its_own() {
    let instance = Instance::generate();
    let mut relay = Relay::new(DM_GROUP);
    let mut a = ready_core(0xa1, "alice");
    a.create_and_register_in(&mut relay, &instance, None, &DM_CHANNEL);
    let mut c = ready_core(0xc3, "carol");
    let kp = c.first_key_package();
    instance.propose_add(&mut relay, &kp);
    a.sync(&relay);
    ingest_add_candidate_in(&mut a, &relay, &c, &DM_GROUP);
    a.commit(&mut relay);
    let welcomes = relay.welcomes_body(c.device);

    // Welcome-first (head L-TS-22 rule 7, the engine's onUnexpectedWelcome): a Welcome for a DM the
    // client does not expect yet is outcome 3 and stores nothing; applied again once the DM is
    // expected, it joins (below, `as_dm`).
    let not_named = expected_entries(&[(GROUP, Some(COMMUNITY), CHANNEL, POLICY)]);
    assert_eq!(
        decode_outcomes(
            &c.core
                .welcomes_apply(&welcomes, &not_named)
                .expect("welcomes_apply")
        ),
        vec![WelcomeOutcome {
            welcome_id: 1,
            group_id: DM_GROUP,
            outcome: 3,
            reason: String::new()
        }]
    );
    assert!(
        groups(&c.core).is_empty(),
        "an unexpected Welcome stores nothing"
    );

    let as_channel = expected_entries(&[(DM_GROUP, Some(COMMUNITY), DM_CHANNEL, POLICY)]);
    assert_eq!(
        decode_outcomes(
            &c.core
                .welcomes_apply(&welcomes, &as_channel)
                .expect("welcomes_apply")
        ),
        vec![WelcomeOutcome {
            welcome_id: 1,
            group_id: DM_GROUP,
            outcome: 2,
            reason: "E_BINDING".into()
        }]
    );
    assert!(
        groups(&c.core).is_empty(),
        "a refused Welcome leaves no row and no group"
    );
    let as_dm = expected_entries(&[(DM_GROUP, None, DM_CHANNEL, POLICY)]);
    assert_eq!(
        decode_outcomes(
            &c.core
                .welcomes_apply(&welcomes, &as_dm)
                .expect("welcomes_apply")
        )[0]
        .outcome,
        0,
        "the held Welcome and the refusal both left the KeyPackage: the expected DM joins"
    );
    let row = c.group(&DM_GROUP).expect("row");
    assert_eq!(
        (row.state, row.community_id, row.target_id),
        (2, None, DM_CHANNEL)
    );
    let (_, seq) = a.send(&mut relay, &DM_GROUP, "welcome to the dm", NOW + 1);
    assert_eq!(c.sync(&relay).new_seqs, vec![seq]);

    // The reverse: a community channel's Welcome to a device that expects a DM.
    let mut channel_relay = Relay::new(GROUP);
    let mut peer = RawPeer::new(0xe5, 0xe6);
    peer.create(&mut channel_relay, &instance);
    let mut d = ready_core(0xd4, "dave");
    let d_kp = d.first_key_package();
    peer.add(&mut channel_relay, &[d_kp.as_slice()]);
    let refused = d
        .core
        .welcomes_apply(
            &channel_relay.welcomes_body(d.device),
            &expected_entries(&[(GROUP, None, CHANNEL, POLICY)]),
        )
        .expect("welcomes_apply");
    let refused = decode_outcomes(&refused);
    assert_eq!(
        (refused[0].outcome, refused[0].reason.as_str()),
        (2, "E_BINDING")
    );
    assert!(groups(&d.core).is_empty());
    let mut bad = Encoder::new();
    bad.array(1)
        .array(4)
        .bytes(&GROUP)
        .bytes(&[0x22; 15])
        .bytes(&CHANNEL)
        .uint(POLICY);
    assert_eq!(
        code(
            d.core
                .welcomes_apply(&channel_relay.welcomes_body(d.device), &bad.into_vec())
        ),
        "E_CORE_INPUT",
        "a 15-byte community is malformed, not a DM"
    );
}

#[test]
fn an_external_join_of_a_dm_with_a_community_is_refused_with_e_binding() {
    let instance = Instance::generate();
    let mut relay = Relay::new(DM_GROUP);
    let mut a = ready_core(0xa1, "alice");
    a.create_and_register_in(&mut relay, &instance, None, &DM_CHANNEL);
    let mut b = ready_core(0xb2, "bob");
    assert_eq!(
        code(b.core.group_join_external(
            &DM_GROUP,
            Some(&COMMUNITY),
            &DM_CHANNEL,
            POLICY,
            &relay.info_body(),
            &relay.tree_body()
        )),
        "E_BINDING"
    );
    assert!(
        groups(&b.core).is_empty(),
        "a refused join leaves no row and no group"
    );
    b.join_external_in(&mut relay, None, &DM_CHANNEL);
    assert_eq!(
        b.group(&DM_GROUP).map(|g| (g.state, g.community_id)),
        Some((2, None))
    );
}

/// Bot v2 Task 23: kind-3 (interaction) groups are gone. A store that still holds one is cleaned at open: the row is
/// deleted (once, with one log line), every other group stays, and a later open finds nothing more to remove.
#[test]
fn a_stored_kind3_group_is_removed_at_open() {
    let conn: dilla_core::mls::ConnHandle = std::sync::Arc::new(std::sync::Mutex::new(
        rusqlite::Connection::open_in_memory().expect("sqlite"),
    ));
    drop(ClientCore::open(conn.clone()).expect("open"));
    {
        let c = conn.lock().unwrap();
        c.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state,interaction_installation_id,interaction_device_id) \
             VALUES(?1,3,?2,?3,2,?4,?5)",
            rusqlite::params![[0x31u8; 16].as_slice(), COMMUNITY.as_slice(), [0x32u8; 16].as_slice(), [0x33u8; 16].as_slice(), [0x34u8; 16].as_slice()],
        )
        .expect("kind-3 row");
        c.execute(
            "INSERT INTO app_groups(group_id,kind,community_id,target_id,state) VALUES(?1,0,?2,?3,2)",
            rusqlite::params![GROUP.as_slice(), COMMUNITY.as_slice(), CHANNEL.as_slice()],
        )
        .expect("text row");
    }
    let core = ClientCore::open(conn.clone()).expect("reopen");
    assert_eq!(core.interaction_groups_removed_at_open(), 1);
    drop(core);
    {
        let c = conn.lock().unwrap();
        let count = |kind: i64| -> i64 {
            c.query_row(
                "SELECT count(*) FROM app_groups WHERE kind=?1",
                [kind],
                |r| r.get(0),
            )
            .unwrap()
        };
        assert_eq!(count(3), 0, "the kind-3 row is gone");
        assert_eq!(count(0), 1, "the text group stays");
    }
    let core = ClientCore::open(conn).expect("open again");
    assert_eq!(core.interaction_groups_removed_at_open(), 0);
}
