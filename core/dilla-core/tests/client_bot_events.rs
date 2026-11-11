#![cfg(not(target_arch = "wasm32"))]

mod client_support;

use client_support::{
    INSTANCE_SIGNER_PUB, Instance, MsgRow, RawPeer, Relay, dm_binding, fold_envelope, request,
    sec_entry, sec_next, text_binding,
};
use dilla_core::cbor::{Encoder, decode_strict};
use dilla_core::client::ClientCore;
use dilla_core::envelope::EnvelopeType;
use dilla_core::identity::DeviceList;
use dilla_core::mls::{ConnHandle, DillaBinding, GroupKind};
use std::sync::{Arc, Mutex};

const INSTANCE: [u8; 16] = [0x11; 16];
const COMMUNITY: [u8; 16] = [0x22; 16];
const INSTALLATION: [u8; 16] = [0x33; 16];
const USER: [u8; 16] = [0x44; 16];
const PEER_USER: [u8; 16] = [0x71; 16];
const PEER_DEVICE: [u8; 16] = [0x72; 16];
const GROUP: [u8; 16] = [0x73; 16];
const CHANNEL: [u8; 16] = [0x74; 16];
const POLICY: u64 = 1;
const NOW: u64 = 1_800_000_000;

fn conn() -> ConnHandle {
    Arc::new(Mutex::new(
        rusqlite::Connection::open_in_memory().expect("sqlite"),
    ))
}

fn ready_bot() -> (ClientCore, ConnHandle) {
    let connection = conn();
    let mut core = ClientCore::open(connection.clone()).expect("open");
    core.bot_bootstrap_begin(&INSTANCE, &COMMUNITY, &INSTALLATION, &[0x77; 32])
        .expect("begin");
    core.bot_bootstrap_complete(&USER, "event-bot", NOW)
        .expect("complete");
    (core, connection)
}

fn peer_history(peer: &RawPeer) -> (Vec<u8>, Vec<u8>) {
    let body = peer.device_list_body();
    let (version, list, signature, previous) = decode_strict(&body, |d| {
        d.array(4)?;
        Ok((
            d.uint()?,
            d.bytes()?.to_vec(),
            d.bytes()?.to_vec(),
            d.bytes()?.to_vec(),
        ))
    })
    .expect("signed device list");
    let mut history = Encoder::new();
    history
        .array(1)
        .array(4)
        .uint(version)
        .bytes(&list)
        .bytes(&signature)
        .bytes(&previous);
    (history.into_vec(), list)
}

fn bot_with_trusted_peer() -> (ClientCore, ConnHandle, Relay, RawPeer) {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut peer = RawPeer::new(PEER_USER[0], PEER_DEVICE[0]);
    peer.create_as(&mut relay, &instance, &text_binding(CHANNEL));
    let (mut bot, connection) = ready_bot();
    let join = bot
        .group_join_external(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &relay.info_body(),
            &relay.tree_body(),
        )
        .expect("join bot");
    let joined = relay.resync(&join).expect("relay resync");
    let seq = decode_strict(&joined, |d| {
        d.array(2)?;
        let seq = d.uint()?;
        d.uint()?;
        Ok(seq)
    })
    .expect("resync response");
    bot.group_joined(&GROUP, seq).expect("join complete");
    peer.apply_commit(&relay, seq);
    let (history, head) = peer_history(&peer);
    bot.device_list_ingest(
        &GROUP,
        relay.leaf_of(PEER_DEVICE),
        &PEER_USER,
        &history,
        &head,
        NOW,
    )
    .expect("trust signed MLS sender");
    (bot, connection, relay, peer)
}

fn bot_with_bound_peer(
    group_id: [u8; 16],
    channel: [u8; 16],
    community: Option<[u8; 16]>,
    mut binding: DillaBinding,
) -> (ClientCore, Relay, RawPeer) {
    let instance = Instance::generate();
    let mut relay = Relay::new(group_id);
    let mut peer = RawPeer::new(PEER_USER[0], PEER_DEVICE[0]);
    binding.target_id = channel;
    binding.community_id = community.map(dilla_core::ids::CommunityId::from_bytes);
    peer.create_as(&mut relay, &instance, &binding);
    let (mut bot, _) = ready_bot();
    let join = if binding.kind == GroupKind::Call {
        bot.call_group_join_external(
            &group_id,
            community.as_ref(),
            &channel,
            POLICY,
            &relay.info_body(),
            &relay.tree_body(),
        )
    } else {
        bot.group_join_external(
            &group_id,
            community.as_ref(),
            &channel,
            POLICY,
            &relay.info_body(),
            &relay.tree_body(),
        )
    }
    .expect("join bound group");
    let joined = relay.resync(&join).expect("relay resync");
    let seq = decode_strict(&joined, |d| {
        d.array(2)?;
        let seq = d.uint()?;
        d.uint()?;
        Ok(seq)
    })
    .expect("resync response");
    bot.group_joined(&group_id, seq).expect("join complete");
    peer.apply_commit(&relay, seq);
    let (history, head) = peer_history(&peer);
    bot.device_list_ingest(
        &group_id,
        relay.leaf_of(PEER_DEVICE),
        &PEER_USER,
        &history,
        &head,
        NOW,
    )
    .expect("trust signed MLS sender");
    (bot, relay, peer)
}

fn encode_rows(rows: &[&MsgRow]) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(rows.len());
    for row in rows {
        e.array(if row.class.is_some() { 9 } else { 8 })
            .uint(row.seq)
            .uint(row.epoch)
            .bytes(&row.uploader)
            .opt_bytes(row.blob.as_deref())
            .opt_bytes(row.commitment.as_ref().map(|v| v.as_slice()))
            .bytes(&row.franking_tag)
            .uint(row.recv_ts)
            .uint(u64::from(row.deleted));
        if let Some(class) = row.class {
            e.uint(class);
        }
    }
    e.into_vec()
}

fn decode_events(body: &[u8]) -> Vec<(u64, u64, u64, String)> {
    decode_strict(body, |d| {
        d.array(3)?;
        let count = d.array_len()?;
        let mut rows = Vec::with_capacity(count);
        for _ in 0..count {
            d.array(17)?;
            let delivery = d.uint()?;
            d.bytes_exact::<16>()?;
            d.bytes_exact::<16>()?;
            d.bytes_exact::<16>()?;
            let seq = d.uint()?;
            d.uint()?;
            d.uint()?;
            d.bytes_exact::<16>()?;
            d.bytes_exact::<16>()?;
            d.uint()?;
            d.uint()?;
            d.uint()?;
            d.bytes_exact::<16>()?;
            let kind = d.uint()?;
            let text = d.text()?.to_owned();
            d.opt_bytes()?;
            d.opt_bytes()?;
            rows.push((delivery, seq, kind, text));
        }
        d.uint()?;
        d.uint()?;
        Ok(rows)
    })
    .expect("event page")
}

fn decode_event_senders(body: &[u8]) -> Vec<(u64, [u8; 16], [u8; 16])> {
    decode_strict(body, |d| {
        d.array(3)?;
        let count = d.array_len()?;
        let mut senders = Vec::with_capacity(count);
        for _ in 0..count {
            d.array(17)?;
            let _delivery = d.uint()?;
            d.bytes_exact::<16>()?;
            d.bytes_exact::<16>()?;
            d.bytes_exact::<16>()?;
            let seq = d.uint()?;
            d.uint()?;
            d.uint()?;
            let user = d.bytes_exact::<16>()?;
            let device = d.bytes_exact::<16>()?;
            d.uint()?;
            d.uint()?;
            d.uint()?;
            d.bytes_exact::<16>()?;
            d.uint()?;
            d.text()?;
            d.opt_bytes()?;
            d.opt_bytes()?;
            senders.push((seq, user, device));
        }
        d.uint()?;
        d.uint()?;
        Ok(senders)
    })
    .expect("event senders")
}

#[test]
fn human_identity_cannot_read_or_ack_bot_events() {
    let mut core = ClientCore::open(conn()).expect("open");
    core.signup_begin(&INSTANCE).expect("signup");
    core.signup_complete(&USER, "human", 1_800_000_000, &INSTANCE_SIGNER_PUB)
        .expect("complete");

    assert_eq!(
        core.bot_events_read(0, 10).unwrap_err().code,
        "E_CORE_STATE"
    );
    assert_eq!(core.bot_events_ack(0).unwrap_err().code, "E_CORE_STATE");
}

#[test]
fn native_bot_event_api_starts_empty_and_rejects_invalid_limits_and_ack() {
    let mut core = ClientCore::open(conn()).expect("open");
    core.bot_bootstrap_begin(&INSTANCE, &COMMUNITY, &INSTALLATION, &[0x77; 32])
        .expect("begin");
    core.bot_bootstrap_complete(&USER, "event-bot", 1_800_000_000)
        .expect("complete");

    let page = core.bot_events_read(0, 10).expect("empty page");
    assert_eq!(page, [0x83, 0x80, 0x00, 0x00]);
    assert_eq!(core.bot_events_read(0, 0).unwrap_err().code, "E_CORE_INPUT");
    assert_eq!(
        core.bot_events_read(0, 101).unwrap_err().code,
        "E_CORE_INPUT"
    );
    assert_eq!(core.bot_events_ack(1).unwrap_err().code, "E_CORE_INPUT");
}

#[test]
fn signed_int64_max_cursor_and_ack_are_admitted_but_max_plus_one_is_refused() {
    let (mut bot, connection) = ready_bot();
    let max = i64::MAX as u64;
    connection
        .lock()
        .expect("lock")
        .execute(
            "INSERT INTO app_bot_events(delivery_id,group_id,source_seq,community_id,channel_id,epoch,recv_ts,sender_user,sender_device,sender_kind,sender_tier,sender_leaf,sender_credential,sender_sig_key,message_id,envelope_type,original_body,disposition) \
             VALUES(?1,?2,1,?3,?4,0,0,?5,?6,1,0,0,x'01',zeroblob(32),?7,0,NULL,1)",
            rusqlite::params![
                max as i64,
                GROUP.as_slice(),
                COMMUNITY.as_slice(),
                CHANNEL.as_slice(),
                PEER_USER.as_slice(),
                PEER_DEVICE.as_slice(),
                [0x7fu8; 16].as_slice(),
            ],
        )
        .expect("synthetic maximum delivery marker without an impossible row count");

    let page = bot
        .bot_events_read(max, 10)
        .expect("max cursor is representable");
    let (count, next, high) = decode_strict(&page, |d| {
        d.array(3)?;
        let count = d.array_len()?;
        let next = d.uint()?;
        let high = d.uint()?;
        Ok((count, next, high))
    })
    .expect("page cursor tuple");
    assert_eq!((count, next, high), (0, max, max));
    assert_eq!(
        bot.bot_events_read(max + 1, 10).unwrap_err().code,
        "E_CORE_INPUT"
    );
    bot.bot_events_ack(max).expect("max ack is representable");
    assert_eq!(
        bot.bot_events_ack(max + 1).unwrap_err().code,
        "E_CORE_INPUT"
    );
}

#[test]
fn accepted_mls_event_survives_reopen_and_ack_clears_only_its_body() {
    let (mut bot, connection, mut relay, mut peer) = bot_with_trusted_peer();

    let source_seq = peer.send(&mut relay, "journal original");
    let message = relay.messages.last().expect("uploaded message").clone();
    connection.lock().expect("lock").execute_batch(
        "CREATE TRIGGER fail_event_snapshot BEFORE INSERT ON app_bot_events BEGIN SELECT RAISE(ABORT,'journal fault'); END",
    ).expect("install rollback fault");
    assert_eq!(
        bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&message]), source_seq)
            .expect_err("event snapshot failure rolls back accepted message")
            .code,
        "E_CORE_STORAGE",
    );
    {
        let db = connection.lock().expect("lock");
        let (source, event): (i64, i64) = db.query_row(
            "SELECT (SELECT count(*) FROM app_messages WHERE group_id=?1 AND seq=?2),(SELECT count(*) FROM app_bot_events WHERE group_id=?1 AND source_seq=?2)",
            rusqlite::params![GROUP.as_slice(), source_seq as i64],
            |r| Ok((r.get(0)?, r.get(1)?)),
        ).expect("rollback counts");
        assert_eq!((source, event), (0, 0));
        db.execute_batch("DROP TRIGGER fail_event_snapshot")
            .expect("remove rollback fault");
    }
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&message]), source_seq)
        .expect("retry after rollback");
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&message]), source_seq)
        .expect("duplicate apply");
    let stored: i64 = connection
        .lock()
        .expect("lock")
        .query_row(
            "SELECT count(*) FROM app_bot_events WHERE group_id=?1 AND source_seq=?2",
            rusqlite::params![GROUP.as_slice(), source_seq as i64],
            |r| r.get(0),
        )
        .expect("one deduplicated source");
    assert_eq!(stored, 1);

    drop(bot); // Simulate losing the response after the core transaction committed.
    let mut bot = ClientCore::open(connection.clone()).expect("restart after commit");
    let page = bot.bot_events_read(0, 10).expect("read durable event");
    let (delivery_id, body, next, high) = decode_strict(&page, |d| {
        d.array(3)?;
        d.array(1)?;
        d.array(17)?;
        let delivery = d.uint()?;
        d.bytes_exact::<16>()?;
        d.bytes_exact::<16>()?;
        d.bytes_exact::<16>()?;
        d.uint()?;
        d.uint()?;
        d.uint()?;
        d.bytes_exact::<16>()?;
        d.bytes_exact::<16>()?;
        d.uint()?;
        d.uint()?;
        d.uint()?;
        d.bytes_exact::<16>()?;
        d.uint()?;
        let body = d.text()?.to_owned();
        d.opt_bytes()?;
        d.opt_bytes()?;
        let next = d.uint()?;
        let high = d.uint()?;
        Ok((delivery, body, next, high))
    })
    .expect("event page");
    assert_eq!(body, "journal original");
    assert_eq!((next, high), (delivery_id, delivery_id));

    bot.bot_events_ack(delivery_id).expect("ack");
    drop(bot);
    let mut bot = ClientCore::open(connection).expect("restart after ack");
    let page = bot.bot_events_read(0, 10).expect("read after ack");
    let (events, next, high) = decode_strict(&page, |d| {
        d.array(3)?;
        let n = d.array_len()?;
        let next = d.uint()?;
        let high = d.uint()?;
        Ok((n, next, high))
    })
    .expect("empty after ack");
    assert_eq!(events, 0);
    assert_eq!(next, delivery_id);
    assert_eq!(high, delivery_id);
}

#[test]
fn page_limit_and_ack_cursor_keep_later_events_readable() {
    let (mut bot, _connection, mut relay, mut peer) = bot_with_trusted_peer();
    let first_seq = peer.send(&mut relay, "first page event");
    let first = relay.messages.last().expect("first message").clone();
    let second_seq = peer.send(&mut relay, "second page event");
    let second = relay.messages.last().expect("second message").clone();
    bot.group_apply(
        &GROUP,
        &[0x80],
        &encode_rows(&[&first, &second]),
        second_seq,
    )
    .expect("accept both messages");

    let page = bot.bot_events_read(0, 1).expect("one event page");
    let rows = decode_events(&page);
    assert_eq!(rows.len(), 1);
    assert_eq!(
        (rows[0].1, rows[0].3.as_str()),
        (first_seq, "first page event")
    );
    let first_delivery = rows[0].0;
    let second_delivery = decode_events(
        &bot.bot_events_read(first_delivery, 10)
            .expect("second page"),
    )[0]
    .0;
    bot.bot_events_ack(first_delivery).expect("ack first event");
    bot.bot_events_ack(first_delivery)
        .expect("equal ack is idempotent");
    let after_ack = decode_events(&bot.bot_events_read(0, 10).expect("clamped to ack"));
    assert_eq!(after_ack.len(), 1);
    assert_eq!(
        (after_ack[0].0, after_ack[0].1, after_ack[0].3.as_str()),
        (second_delivery, second_seq, "second page event")
    );
    assert!(
        bot.bot_events_ack(first_delivery - 1).is_err(),
        "backward ack is invalid"
    );
}

#[test]
fn original_post_and_edit_events_are_snapshotted_and_author_delete_suppresses_target() {
    let (mut bot, _connection, mut relay, mut peer) = bot_with_trusted_peer();
    let target = [0x66; 16];
    let post_seq = peer.send_envelope(
        &mut relay,
        &fold_envelope(target, EnvelopeType::Message, None, "before edit"),
    );
    let edit_seq = peer.send_envelope(
        &mut relay,
        &fold_envelope([0x67; 16], EnvelopeType::Edit, Some(target), "after edit"),
    );
    let reaction_seq = peer.send_envelope(
        &mut relay,
        &fold_envelope([0x68; 16], EnvelopeType::ReactionAdd, Some(target), "🔥"),
    );
    let rows = relay.messages.iter().collect::<Vec<_>>();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&rows), reaction_seq)
        .expect("apply post, edit and reaction");
    assert_eq!(
        decode_events(&bot.bot_events_read(0, 10).expect("read snapshots"))
            .into_iter()
            .map(|row| (row.1, row.2, row.3))
            .collect::<Vec<_>>(),
        vec![
            (post_seq, 0, "before edit".to_owned()),
            (edit_seq, 1, "after edit".to_owned()),
            (reaction_seq, 3, "🔥".to_owned()),
        ],
        "folding the current timeline does not rewrite accepted event snapshots"
    );
    let senders = decode_event_senders(&bot.bot_events_read(0, 10).expect("sender provenance"));
    assert_eq!(
        senders,
        vec![
            (post_seq, PEER_USER, PEER_DEVICE),
            (edit_seq, PEER_USER, PEER_DEVICE),
            (reaction_seq, PEER_USER, PEER_DEVICE),
        ],
        "every snapshot identifies the authenticated actor/device, including reaction actors"
    );

    let delete_seq = peer.send_envelope(
        &mut relay,
        &fold_envelope([0x69; 16], EnvelopeType::Delete, Some(target), ""),
    );
    let delete = relay.messages.last().expect("author delete");
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[delete]), delete_seq)
        .expect("apply author delete");
    assert_eq!(
        decode_events(&bot.bot_events_read(0, 10).expect("read after deletion"))
            .into_iter()
            .map(|row| (row.1, row.2, row.3))
            .collect::<Vec<_>>(),
        vec![
            (reaction_seq, 3, "🔥".to_owned()),
            (delete_seq, 2, "".to_owned()),
        ],
        "the delete remains while the target and edits whose source rows were blanked are suppressed"
    );
}

#[test]
fn reaction_event_uses_the_reactors_authenticated_identity() {
    let (mut bot, _connection, mut relay, mut author) = bot_with_trusted_peer();
    let target = [0x65; 16];
    let post_seq = author.send_envelope(
        &mut relay,
        &fold_envelope(target, EnvelopeType::Message, None, "target author"),
    );
    let post = relay.messages.last().expect("target post").clone();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&post]), post_seq)
        .expect("accept target post");

    let mut reactor = RawPeer::new(0x75, 0x76);
    let join_seq = reactor.join_external_as(&mut relay, &text_binding(CHANNEL));
    author.apply_commit(&relay, join_seq);
    bot.group_apply(
        &GROUP,
        &relay.handshakes_from(join_seq),
        &encode_rows(&[]),
        join_seq,
    )
    .expect("apply second authenticated member");
    let (history, head) = peer_history(&reactor);
    bot.device_list_ingest(
        &GROUP,
        relay.leaf_of(reactor.device),
        &reactor.user,
        &history,
        &head,
        NOW,
    )
    .expect("trust second signed MLS sender");
    let reaction_seq = reactor.send_envelope(
        &mut relay,
        &fold_envelope([0x66; 16], EnvelopeType::ReactionAdd, Some(target), "🎯"),
    );
    let reaction = relay.messages.last().expect("reaction").clone();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&reaction]), reaction_seq)
        .expect("accept reaction");
    assert_eq!(
        decode_event_senders(&bot.bot_events_read(0, 10).expect("event actors")),
        vec![
            (post_seq, PEER_USER, PEER_DEVICE),
            (reaction_seq, reactor.user, reactor.device),
        ],
        "reaction actor/device comes from its own MLS leaf, separate from the target author"
    );
}

#[test]
fn non_community_and_non_text_bindings_never_populate_the_journal() {
    let mut call_binding = text_binding([0x7b; 16]);
    call_binding.kind = GroupKind::Call;
    call_binding.media_version = GroupKind::Call.media_version();
    let cases = [
        (
            [0x75; 16],
            [0x76; 16],
            Some([0x77; 16]),
            text_binding([0x76; 16]),
            "foreign text",
        ),
        ([0x78; 16], [0x79; 16], None, dm_binding([0x79; 16]), "DM"),
        (
            [0x7a; 16],
            [0x7b; 16],
            Some(COMMUNITY),
            call_binding,
            "call",
        ),
    ];
    for (group, channel, community, binding, label) in cases {
        let (mut bot, mut relay, mut peer) =
            bot_with_bound_peer(group, channel, community, binding);
        let seq = peer.send(&mut relay, label);
        let row = relay.messages.last().expect("non-feed source").clone();
        bot.group_apply(&group, &[0x80], &encode_rows(&[&row]), seq)
            .expect("accept authenticated source without exporting non-feed binding");
        assert!(
            decode_events(&bot.bot_events_read(0, 10).expect("empty feed")).is_empty(),
            "{label} group must not populate Bot journal"
        );
    }
}

#[test]
fn uploader_or_class_mismatch_cannot_create_a_journal_event() {
    let (mut bot, connection, mut relay, mut peer) = bot_with_trusted_peer();
    let source_seq = peer.send(&mut relay, "authenticated row");
    let source = relay.messages.last().expect("message").clone();
    let mut bad_uploader = source.clone();
    bad_uploader.uploader = [0x99; 16];
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&bad_uploader]), source_seq)
        .expect("mismatched uploader is handled as a refused source");
    let class_seq = peer.send(&mut relay, "wrong upload class");
    let mut bad_class = relay.messages.last().expect("second message").clone();
    bad_class.class = Some(1);
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&bad_class]), class_seq)
        .expect("mismatched class is handled as a refused source");
    let n: i64 = connection
        .lock()
        .expect("lock")
        .query_row("SELECT count(*) FROM app_bot_events", [], |r| r.get(0))
        .expect("no event from mismatched delivery metadata");
    assert_eq!(n, 0);
}

#[test]
fn corrupt_real_mls_ciphertext_never_creates_a_journal_event() {
    let (mut bot, connection, mut relay, mut peer) = bot_with_trusted_peer();
    let source_seq = peer.send(&mut relay, "ciphertext that will be corrupted");
    let mut source = relay.messages.last().expect("real MLS message").clone();
    let blob = source.blob.as_mut().expect("encrypted MLS application");
    let last = blob.len() - 1;
    blob[last] ^= 0x01;
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&source]), source_seq)
        .expect("undecryptable MLS source is retained as unreadable");

    let journal_count: i64 = connection
        .lock()
        .expect("lock")
        .query_row(
            "SELECT count(*) FROM app_bot_events WHERE group_id=?1 AND source_seq=?2",
            rusqlite::params![GROUP.as_slice(), source_seq as i64],
            |r| r.get(0),
        )
        .expect("journal count");
    let status: i64 = connection
        .lock()
        .expect("lock")
        .query_row(
            "SELECT status FROM app_messages WHERE group_id=?1 AND seq=?2",
            rusqlite::params![GROUP.as_slice(), source_seq as i64],
            |r| r.get(0),
        )
        .expect("unreadable source marker");
    assert_eq!(journal_count, 0);
    assert_eq!(status, 1, "the source is stored as unreadable");
    assert!(decode_events(&bot.bot_events_read(0, 10).expect("drain")).is_empty());
}

#[test]
fn native_bot_outbox_echo_is_not_a_remote_event() {
    let (mut bot, connection, mut relay, _peer) = bot_with_trusted_peer();
    let identity: Vec<u8> = connection
        .lock()
        .expect("lock")
        .query_row("SELECT v FROM app_meta WHERE k='identity'", [], |r| {
            r.get(0)
        })
        .expect("native identity");
    let bot_device = decode_strict(&identity, |d| {
        d.array(13)?;
        d.uint()?;
        d.bytes_exact::<16>()?;
        d.bytes_exact::<16>()?;
        let device = d.bytes_exact::<16>()?;
        for _ in 0..9 {
            d.skip()?;
        }
        Ok(device)
    })
    .expect("device id");
    let prepared = bot
        .send_prepare(&GROUP, &request(0, None, "own bot message", &[]), NOW)
        .expect("prepare native Bot message");
    let msg_id = decode_strict(&prepared, |d| {
        d.array(1)?;
        d.bytes_exact::<16>()
    })
    .expect("message id");
    let encrypted = bot
        .send_encrypt(&msg_id)
        .expect("encrypt native Bot message");
    let message_body = decode_strict(&encrypted, |d| {
        d.array(2)?;
        d.bytes_exact::<16>()?;
        d.bytes().map(ToOwned::to_owned)
    })
    .expect("upload body");
    let response = relay
        .post_message(bot_device, &message_body)
        .expect("relay own message");
    bot.send_confirm(&msg_id, &response)
        .expect("confirm own outbox echo");
    let own_row = relay.messages.last().expect("own upload").clone();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&own_row]), own_row.seq)
        .expect("apply echoed own upload");
    assert!(decode_events(&bot.bot_events_read(0, 10).expect("remote events only")).is_empty());
}

#[test]
fn late_pending_revalidation_gets_a_new_delivery_id_after_a_higher_ack() {
    // The Bot has not received the sender's list yet, so its real MLS message stays pending.
    let (mut bot, connection) = ready_bot();
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut peer = RawPeer::new(PEER_USER[0], PEER_DEVICE[0]);
    peer.create_as(&mut relay, &instance, &text_binding(CHANNEL));
    let join = bot
        .group_join_external(
            &GROUP,
            Some(&COMMUNITY),
            &CHANNEL,
            POLICY,
            &relay.info_body(),
            &relay.tree_body(),
        )
        .expect("join bot");
    let joined = relay.resync(&join).expect("relay resync");
    let commit_seq = decode_strict(&joined, |d| {
        d.array(2)?;
        let seq = d.uint()?;
        d.uint()?;
        Ok(seq)
    })
    .expect("resync response");
    bot.group_joined(&GROUP, commit_seq).expect("join complete");
    peer.apply_commit(&relay, commit_seq);

    let pending_seq = peer.send(&mut relay, "late lower source");
    let pending = relay.messages.last().expect("pending message").clone();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&pending]), pending_seq)
        .expect("retain untrusted sender evidence");
    let (history, head) = peer_history(&peer);
    let leaf = relay.leaf_of(PEER_DEVICE);
    bot.device_list_ingest(&GROUP, leaf, &PEER_USER, &history, &head, NOW)
        .expect("trust sender after original sync");

    let higher_seq = peer.send(&mut relay, "higher source first");
    let higher = relay.messages.last().expect("higher message").clone();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&higher]), higher_seq)
        .expect("accept higher source first");
    let high_page = bot.bot_events_read(0, 10).expect("read higher event");
    let first = decode_events(&high_page);
    assert_eq!(first.len(), 1);
    assert_eq!(
        (first[0].1, first[0].3.as_str()),
        (higher_seq, "higher source first")
    );
    let acknowledged = first[0].0;
    bot.bot_events_ack(acknowledged).expect("ack higher event");

    assert_eq!(
        bot.revalidate_pending(&GROUP, leaf, 1)
            .expect("late revalidation"),
        1
    );
    let late = decode_events(
        &bot.bot_events_read(acknowledged, 10)
            .expect("read late event"),
    );
    assert_eq!(late.len(), 1);
    assert!(
        late[0].0 > acknowledged,
        "delivery id is newer than the ack"
    );
    assert_eq!(
        (late[0].1, late[0].3.as_str()),
        (pending_seq, "late lower source")
    );
    drop(bot);
    let mut bot = ClientCore::open(connection).expect("reopen late event");
    assert_eq!(
        decode_events(
            &bot.bot_events_read(acknowledged, 10)
                .expect("recover late event")
        ),
        late
    );
}

#[test]
fn explicit_message_deleted_suppresses_an_unacknowledged_event() {
    let (mut bot, _connection, mut relay, mut peer) = bot_with_trusted_peer();
    let source_seq = peer.send(&mut relay, "must be withdrawn");
    let source = relay.messages.last().expect("message").clone();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&source]), source_seq)
        .expect("accept event");
    assert_eq!(
        decode_events(&bot.bot_events_read(0, 10).expect("queued")).len(),
        1
    );

    bot.message_deleted(&GROUP, source_seq)
        .expect("late tombstone");
    assert!(decode_events(&bot.bot_events_read(0, 10).expect("suppressed")).is_empty());
}

#[test]
fn group_apply_server_tombstone_clears_a_queued_body_and_advances_the_marker() {
    let (mut bot, connection, mut relay, mut peer) = bot_with_trusted_peer();
    let source_seq = peer.send(&mut relay, "server tombstone source");
    let source = relay.messages.last().expect("message").clone();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&source]), source_seq)
        .expect("accept event");
    let delivery_id = decode_events(&bot.bot_events_read(0, 10).expect("queued"))[0].0;

    // Rewind only the fixture's applied cursor so the same server sequence is presented as the
    // next row in a resumed sync. The row itself is an actual delivery-service tombstone.
    connection
        .lock()
        .expect("lock")
        .execute(
            "UPDATE app_groups SET next_seq=?2 WHERE group_id=?1",
            rusqlite::params![GROUP.as_slice(), source_seq as i64],
        )
        .expect("stage resumed sync cursor");
    relay.delete_message(source_seq);
    let tombstone = relay
        .messages
        .iter()
        .find(|row| row.seq == source_seq)
        .expect("delivery-service tombstone row")
        .clone();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&tombstone]), source_seq)
        .expect("apply server tombstone row");
    drop(bot);

    let mut bot = ClientCore::open(connection.clone()).expect("reopen after tombstone");
    let page = bot.bot_events_read(0, 10).expect("read retained marker");
    let (count, next, high) = decode_strict(&page, |d| {
        d.array(3)?;
        let count = d.array_len()?;
        let next = d.uint()?;
        let high = d.uint()?;
        Ok((count, next, high))
    })
    .expect("suppressed cursor page");
    assert_eq!((count, next, high), (0, delivery_id, delivery_id));
    let retained: (Option<String>, i64, i64) = connection
        .lock()
        .expect("lock")
        .query_row(
            "SELECT original_body,disposition,(SELECT status FROM app_messages WHERE group_id=?1 AND seq=?2) FROM app_bot_events WHERE group_id=?1 AND source_seq=?2",
            rusqlite::params![GROUP.as_slice(), source_seq as i64],
            |r| Ok((r.get(0)?, r.get(1)?, r.get(2)?)),
        )
        .expect("tombstone leaves compact journal metadata");
    assert_eq!(retained, (None, 1, 2));
}

#[test]
fn transient_trust_store_failure_keeps_the_queued_event_retryable() {
    let (mut bot, connection, mut relay, mut peer) = bot_with_trusted_peer();
    let source_seq = peer.send(&mut relay, "revoked before delivery");
    let source = relay.messages.last().expect("message").clone();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&source]), source_seq)
        .expect("accept event");
    assert_eq!(
        decode_events(&bot.bot_events_read(0, 10).expect("queued")).len(),
        1
    );

    connection
        .lock()
        .expect("lock")
        .execute_batch("ALTER TABLE app_device_lists RENAME TO app_device_lists_unavailable")
        .expect("simulate transient trust storage failure");
    assert_eq!(
        bot.bot_events_read(0, 10)
            .expect_err("storage failure must be surfaced")
            .code,
        "E_CORE_STORAGE"
    );
    let retained: (i64, Option<String>) = connection.lock().expect("lock").query_row(
        "SELECT disposition,original_body FROM app_bot_events WHERE group_id=?1 AND source_seq=?2",
        rusqlite::params![GROUP.as_slice(), source_seq as i64],
        |r| Ok((r.get(0)?,r.get(1)?)),
    ).expect("journal still live after failed trust check");
    assert_eq!(retained, (0, Some("revoked before delivery".to_owned())));
    connection
        .lock()
        .expect("lock")
        .execute_batch("ALTER TABLE app_device_lists_unavailable RENAME TO app_device_lists")
        .expect("restore trust storage");
    assert_eq!(
        decode_events(&bot.bot_events_read(0, 10).expect("retry after recovery")).len(),
        1
    );
}

#[test]
fn signed_device_revocation_suppresses_a_queued_event_before_drain() {
    let (mut bot, connection, mut relay, mut peer) = bot_with_trusted_peer();
    let source_seq = peer.send(&mut relay, "revoked before delivery");
    let source = relay.messages.last().expect("message").clone();
    bot.group_apply(&GROUP, &[0x80], &encode_rows(&[&source]), source_seq)
        .expect("accept event");

    let (_, first_bytes) = peer_history(&peer);
    let first = DeviceList::decode(&first_bytes).expect("initial signed list");
    let mut revoked = sec_entry(&peer);
    revoked.revoked_at = Some(NOW + 5);
    let second = sec_next(&first, vec![revoked], PEER_USER[0].wrapping_add(0x40));
    connection
        .lock()
        .expect("lock")
        .execute(
            "UPDATE app_device_lists SET version=2,list=?2 WHERE user_id=?1",
            rusqlite::params![PEER_USER.as_slice(), second.encode()],
        )
        .expect("install the newer, validly signed revocation list");
    assert!(decode_events(&bot.bot_events_read(0, 10).expect("recheck revoked sender")).is_empty());
    let (body, disposition): (Option<String>, i64) = connection.lock().expect("lock").query_row(
        "SELECT original_body,disposition FROM app_bot_events WHERE group_id=?1 AND source_seq=?2",
        rusqlite::params![GROUP.as_slice(), source_seq as i64],
        |r| Ok((r.get(0)?,r.get(1)?)),
    ).expect("retracted journal metadata");
    assert_eq!((body, disposition), (None, 1));
}

/// One journal marker with no body (already redacted), so a read pages past it without an MLS recheck.
fn journal_marker(connection: &ConnHandle, delivery_id: i64) {
    let marker = [delivery_id as u8; 16];
    connection
        .lock()
        .expect("lock")
        .execute(
            "INSERT INTO app_bot_events(delivery_id,group_id,source_seq,community_id,channel_id,epoch,recv_ts,sender_user,sender_device,sender_kind,sender_tier,sender_leaf,sender_credential,sender_sig_key,message_id,envelope_type,original_body,disposition) \
             VALUES(?1,?2,?1,?3,?4,0,0,?5,?6,0,0,0,x'01',zeroblob(32),?7,0,NULL,1)",
            rusqlite::params![
                delivery_id,
                GROUP.as_slice(),
                COMMUNITY.as_slice(),
                CHANNEL.as_slice(),
                PEER_USER.as_slice(),
                PEER_DEVICE.as_slice(),
                marker.as_slice(),
            ],
        )
        .expect("journal marker");
}

/// A journal marker inserted as the core journals an event: without a delivery id, so SQLite AUTOINCREMENT picks
/// it. Returns that id.
fn journal_marker_auto(connection: &ConnHandle, source_seq: i64) -> i64 {
    let db = connection.lock().expect("lock");
    db.execute(
        "INSERT INTO app_bot_events(group_id,source_seq,community_id,channel_id,epoch,recv_ts,sender_user,sender_device,sender_kind,sender_tier,sender_leaf,sender_credential,sender_sig_key,message_id,envelope_type,original_body,disposition) \
         VALUES(?1,?2,?3,?4,0,0,?5,?6,0,0,0,x'01',zeroblob(32),?7,0,NULL,1)",
        rusqlite::params![
            GROUP.as_slice(),
            source_seq,
            COMMUNITY.as_slice(),
            CHANNEL.as_slice(),
            PEER_USER.as_slice(),
            PEER_DEVICE.as_slice(),
            [source_seq as u8; 16].as_slice(),
        ],
    )
    .expect("auto journal marker");
    db.last_insert_rowid()
}

fn helper_event_page_vector(group: &str, name: &str) -> Vec<u8> {
    let corpus: serde_json::Value =
        serde_json::from_str(include_str!("../../../protocol/vectors/bot.json"))
            .expect("bot vectors");
    let case = corpus["helper_event_pages"][group]
        .as_array()
        .expect("helper event page vectors")
        .iter()
        .find(|case| case["name"] == name)
        .expect("named helper event page vector");
    hex::decode(case["hex"].as_str().expect("hex")).expect("vector hex")
}

/// A Bot v1 installation journaled interaction-group events with the newest delivery ids and acked through them;
/// Bot v2's remove_interaction_groups deletes those rows and keeps the ack. The page still keeps next <= high water
/// (the shared vector Go decodes), and acknowledging that next is the idempotent re-ack, not a refusal.
#[test]
fn events_page_after_newest_rows_deleted() {
    let (mut bot, connection) = ready_bot();
    for delivery in 1..=5 {
        journal_marker(&connection, delivery);
    }
    bot.bot_events_ack(5).expect("ack the whole journal");
    connection
        .lock()
        .expect("lock")
        .execute("DELETE FROM app_bot_events WHERE delivery_id>=3", [])
        .expect("delete the newest journal rows");

    let page = bot.bot_events_read(0, 100).expect("read after deletion");
    assert_eq!(
        page,
        helper_event_page_vector(
            "accepts",
            "acked journal whose newest rows were deleted reports the ack as high water"
        )
    );
    assert_eq!(page, [0x83, 0x80, 0x05, 0x05]);
    bot.bot_events_ack(5)
        .expect("acknowledging next is accepted");
    assert_eq!(bot.bot_events_ack(6).unwrap_err().code, "E_CORE_INPUT");
    assert_eq!(bot.bot_events_ack(4).unwrap_err().code, "E_CORE_INPUT");

    // AUTOINCREMENT never reuses a deleted id: the next journaled row lands above the ack (the self-heal).
    let healed = journal_marker_auto(&connection, 50);
    assert!(healed > 5, "new delivery id {healed} is not above the ack");
    assert_eq!(
        bot.bot_events_read(0, 100)
            .expect("read the healed journal"),
        [0x83, 0x80, healed as u8, healed as u8]
    );
    connection
        .lock()
        .expect("lock")
        .execute("DELETE FROM app_bot_events WHERE delivery_id=?1", [healed])
        .expect("drop the healing row again");

    // Every row gone (the journal held only interaction-group events): the ack is still the high water.
    connection
        .lock()
        .expect("lock")
        .execute("DELETE FROM app_bot_events", [])
        .expect("delete every journal row");
    assert_eq!(
        bot.bot_events_read(0, 100)
            .expect("read an emptied journal"),
        [0x83, 0x80, 0x05, 0x05]
    );
    bot.bot_events_ack(5).expect("re-ack an emptied journal");

    // A newly journaled event takes a delivery id above the ack and pages normally.
    journal_marker(&connection, 6);
    assert_eq!(
        bot.bot_events_read(0, 100).expect("read the new marker"),
        [0x83, 0x80, 0x06, 0x06]
    );
    bot.bot_events_ack(6).expect("ack the new marker");
}

/// A healthy journal (ack <= MAX(delivery_id)): the page bytes are the ones the core always produced.
#[test]
fn events_page_unchanged_when_healthy() {
    let (mut bot, connection) = ready_bot();
    assert_eq!(
        bot.bot_events_read(0, 100).expect("empty journal"),
        helper_event_page_vector("accepts", "empty journal")
    );
    for delivery in 1..=5 {
        journal_marker(&connection, delivery);
    }
    bot.bot_events_ack(3).expect("ack part of the journal");
    // The cursor is clamped to the ack and no live row follows it: the page runs to the high water.
    assert_eq!(
        bot.bot_events_read(0, 100)
            .expect("read the healthy journal"),
        [0x83, 0x80, 0x05, 0x05]
    );
    // One row per page: next is the row after the ack, the high water the newest row.
    assert_eq!(
        bot.bot_events_read(0, 1).expect("read one row"),
        [0x83, 0x80, 0x04, 0x05]
    );
    bot.bot_events_ack(5).expect("ack the rest");
    assert_eq!(bot.bot_events_ack(6).unwrap_err().code, "E_CORE_INPUT");
}

/// The production shape after the v1 upgrade on an installation that holds no Bot channel group: no journal row
/// at all (MAX(delivery_id) is NULL) under a durable ack of 5. The page is empty with next and high water at the
/// ack, and acknowledging that value is accepted.
#[test]
fn events_page_with_no_rows_under_a_durable_ack() {
    let (mut bot, connection) = ready_bot();
    connection
        .lock()
        .expect("lock")
        .execute(
            "UPDATE app_bot_event_state SET ack_delivery_id=5 WHERE singleton=1",
            [],
        )
        .expect("ack left by the deleted v1 interaction rows");
    let page = bot.bot_events_read(0, 100).expect("read a rowless journal");
    assert_eq!(
        page,
        helper_event_page_vector(
            "accepts",
            "journal with no rows left under ack 5 reports the ack as next and high water"
        )
    );
    assert_eq!(page, [0x83, 0x80, 0x05, 0x05]);
    bot.bot_events_ack(5)
        .expect("acknowledging the ack is accepted");
    assert_eq!(bot.bot_events_ack(6).unwrap_err().code, "E_CORE_INPUT");
    assert_eq!(bot.bot_events_ack(4).unwrap_err().code, "E_CORE_INPUT");
    assert_eq!(
        bot.bot_events_read(0, 100).expect("read again"),
        [0x83, 0x80, 0x05, 0x05]
    );
}

/// A live journal row whose group the Bot no longer holds (removed or never rejoined) is redacted on read and
/// paged past: the page is well formed and empty, not an error that fails every later poll.
#[test]
fn events_page_skips_a_row_whose_group_is_gone() {
    let (mut bot, connection) = ready_bot();
    connection
        .lock()
        .expect("lock")
        .execute(
            "INSERT INTO app_bot_events(delivery_id,group_id,source_seq,community_id,channel_id,epoch,recv_ts,sender_user,sender_device,sender_kind,sender_tier,sender_leaf,sender_credential,sender_sig_key,message_id,envelope_type,original_body,disposition) \
             VALUES(1,?1,1,?2,?3,0,0,?4,?5,0,0,0,x'01',zeroblob(32),?6,0,'gone group text',0)",
            rusqlite::params![
                GROUP.as_slice(),
                COMMUNITY.as_slice(),
                CHANNEL.as_slice(),
                PEER_USER.as_slice(),
                PEER_DEVICE.as_slice(),
                [0x01u8; 16].as_slice(),
            ],
        )
        .expect("live row of a group the Bot does not hold");
    assert_eq!(
        bot.bot_events_read(0, 100)
            .expect("read past the gone group"),
        [0x83, 0x80, 0x01, 0x01]
    );
    let (body, disposition): (Option<String>, i64) = connection
        .lock()
        .expect("lock")
        .query_row(
            "SELECT original_body,disposition FROM app_bot_events WHERE delivery_id=1",
            [],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .expect("redacted row");
    assert_eq!((body, disposition), (None, 1));
    bot.bot_events_ack(1).expect("ack past the gone group");
}

/// A read cursor above the clamped high water is refused, so the core never emits a page whose next exceeds its
/// high water (the page Go refuses as "event cursors").
#[test]
fn events_read_after_above_high_water_is_refused() {
    let (mut bot, connection) = ready_bot();
    assert_eq!(bot.bot_events_read(1, 10).unwrap_err().code, "E_CORE_INPUT");
    for delivery in 1..=5 {
        journal_marker(&connection, delivery);
    }
    assert_eq!(
        bot.bot_events_read(5, 10)
            .expect("cursor at the high water"),
        [0x83, 0x80, 0x05, 0x05]
    );
    assert_eq!(bot.bot_events_read(6, 10).unwrap_err().code, "E_CORE_INPUT");
    bot.bot_events_ack(5).expect("ack the journal");
    connection
        .lock()
        .expect("lock")
        .execute("DELETE FROM app_bot_events", [])
        .expect("delete every journal row");
    assert_eq!(
        bot.bot_events_read(5, 10)
            .expect("cursor at the clamped high water"),
        [0x83, 0x80, 0x05, 0x05]
    );
    assert_eq!(bot.bot_events_read(6, 10).unwrap_err().code, "E_CORE_INPUT");
}
