// Native only: every test opens an in-memory SQLite connection.
#![cfg(not(target_arch = "wasm32"))]

//! web3.7 TB1: public threads in the local store (app schema 5). A type-0 envelope's `thread_id`
//! names a held type-0 root that has none; a reply whose root is held leaves the main timeline
//! and is read through `thread_timeline`; the root carries its thread summary; `threads` lists the
//! group's threads (protocol/04 § Threads). Every peer row is a real MLS application message.

mod client_support;

use client_support::*;
use dilla_core::envelope::Envelope;
use dilla_core::envelope::EnvelopeType::{self, Delete, Message, Pin, ReactionAdd};
use dilla_core::ids::MsgId;

const ROOT: [u8; 16] = [0x71; 16];
const UNHELD: [u8; 16] = [0xee; 16];

/// "me" (alice, 0xa1) created and registered GROUP; the raw peer (user 0xe5, device 0xe6) joined
/// by external commit at seq 1; me applied it and ingested the peer's device list.
fn start() -> (Instance, Relay, Core, RawPeer) {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut me = ready_core(0xa1, "alice");
    me.create_and_register(&mut relay, &instance);
    let mut peer = RawPeer::new(0xe5, 0xe6);
    assert_eq!(peer.join_external(&mut relay), 1);
    me.sync(&relay);
    ingest_list_body(
        &mut me,
        &relay,
        peer.user,
        peer.device,
        &peer.device_list_body(),
    );
    (instance, relay, me, peer)
}

/// One envelope from `peer` with msg id 16 × `msg`, in thread `thread`; answers its seq.
fn post_in(
    peer: &mut RawPeer,
    relay: &mut Relay,
    msg: u8,
    kind: EnvelopeType,
    reply_to: Option<[u8; 16]>,
    thread: Option<[u8; 16]>,
    body: &str,
) -> u64 {
    let mut env: Envelope = fold_envelope([msg; 16], kind, reply_to, body);
    env.thread_id = thread.map(MsgId::from_bytes);
    peer.send_envelope(relay, &env)
}

fn seqs(me: &Core) -> Vec<u64> {
    me.timeline(&GROUP).into_iter().map(|r| r.seq).collect()
}

fn thread_of(
    me: &Core,
    root: &[u8; 16],
    before: u64,
    limit: u32,
) -> (Option<TimelineRow>, Vec<TimelineRow>) {
    decode_thread_timeline(
        &me.core
            .thread_timeline(&GROUP, root, before, limit)
            .expect("thread_timeline"),
    )
}

fn threads_of(me: &Core) -> Vec<ThreadRow> {
    decode_threads(&me.core.threads(&GROUP).expect("threads"))
}

fn stored_thread(me: &Core, seq: u64) -> Option<Vec<u8>> {
    me.probe
        .lock()
        .expect("lock")
        .query_row(
            "SELECT thread_id FROM app_messages WHERE group_id = ?1 AND seq = ?2",
            rusqlite::params![GROUP.as_slice(), seq as i64],
            |r| r.get(0),
        )
        .expect("thread_id")
}

fn outbox_thread(me: &Core, msg_id: &[u8; 16]) -> Option<MsgId> {
    let bytes: Vec<u8> = me
        .probe
        .lock()
        .expect("lock")
        .query_row(
            "SELECT envelope FROM app_outbox WHERE msg_id = ?1",
            [msg_id.as_slice()],
            |r| r.get(0),
        )
        .expect("outbox envelope");
    Envelope::decode(&bytes).expect("envelope").thread_id
}

// ---------------------------------------------------------------------------------------------
// Step 2: send_prepare.

#[test]
fn a_thread_reply_names_a_held_post() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root_seq = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    me.sync(&relay);
    let msg_id = me.prepare_request(
        &GROUP,
        &thread_request(0, None, "in the thread", Some(&ROOT)),
        NOW,
    );
    assert_eq!(
        outbox_thread(&me, &msg_id),
        Some(MsgId::from_bytes(ROOT)),
        "the envelope carries the thread id"
    );
    let seq = me.encrypt_and_confirm(&mut relay, &GROUP, &msg_id);
    assert_eq!(
        stored_thread(&me, seq),
        Some(ROOT.to_vec()),
        "the own row keeps the thread id"
    );
    assert_eq!(stored_thread(&me, root_seq), None);
}

#[test]
fn a_thread_on_a_reply_is_refused() {
    let (_i, mut relay, mut me, mut peer) = start();
    post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(ROOT),
        "a reply",
    );
    me.sync(&relay);
    let refused = me
        .core
        .send_prepare(
            &GROUP,
            &thread_request(0, None, "nested", Some(&[0x72; 16])),
            NOW,
        )
        .unwrap_err();
    assert_eq!(
        (refused.code, refused.detail.as_str()),
        ("E_CORE_INPUT", "the thread root is a thread reply")
    );
    assert!(me.outbox(&GROUP).is_empty(), "the refusal writes nothing");
    // The control: the same request naming the root itself queues.
    me.prepare_request(&GROUP, &thread_request(0, None, "nested", Some(&ROOT)), NOW);
    assert_eq!(me.outbox(&GROUP).len(), 1);
}

#[test]
fn a_thread_on_a_non_post_is_refused() {
    let (_i, mut relay, mut me, mut peer) = start();
    post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    post_in(
        &mut peer,
        &mut relay,
        0x73,
        ReactionAdd,
        Some(ROOT),
        None,
        "👍",
    );
    me.sync(&relay);
    // The root is a reaction row.
    let refused = me
        .core
        .send_prepare(
            &GROUP,
            &thread_request(0, None, "x", Some(&[0x73; 16])),
            NOW,
        )
        .unwrap_err();
    assert_eq!(refused.code, "E_CORE_INPUT");
    // An unheld id.
    let unheld = me
        .core
        .send_prepare(&GROUP, &thread_request(0, None, "x", Some(&UNHELD)), NOW)
        .unwrap_err();
    assert_eq!(
        (unheld.code, unheld.detail.as_str()),
        ("E_CORE_NOT_FOUND", "target is not held")
    );
    // A thread id on types 1 to 6.
    for (ty, body) in [
        (1u64, "edited"),
        (2, ""),
        (3, "👍"),
        (4, "👍"),
        (5, ""),
        (6, ""),
    ] {
        let e = me
            .core
            .send_prepare(
                &GROUP,
                &thread_request(ty, Some(&ROOT), body, Some(&ROOT)),
                NOW,
            )
            .unwrap_err();
        assert_eq!(
            (e.code, e.detail.as_str()),
            ("E_CORE_INPUT", "only a message starts or joins a thread"),
            "type {ty}"
        );
        // The control: the same request with no thread id is accepted (or refused for its own
        // reason, never for the thread): a reaction queues.
        if ty == 3 {
            me.prepare_request(&GROUP, &thread_request(ty, Some(&ROOT), body, None), NOW);
        }
    }
    assert_eq!(me.outbox(&GROUP).len(), 1, "only the control queued");
}

#[test]
fn a_four_element_request_still_sends_with_no_thread() {
    let (_i, mut relay, mut me, _peer) = start();
    let (msg_id, seq) = me.send(&mut relay, &GROUP, "plain", NOW);
    assert_eq!(stored_thread(&me, seq), None);
    assert_eq!(
        me.timeline(&GROUP).last().map(|r| r.msg_id),
        Some(Some(msg_id))
    );
}

// ---------------------------------------------------------------------------------------------
// Step 3: ingest.

#[test]
fn a_peers_thread_reply_arrives_with_its_thread_id() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    let reply = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(ROOT),
        "a reply",
    );
    // A fold that carries a thread id stores none (type 0 only).
    let react = post_in(
        &mut peer,
        &mut relay,
        0x73,
        ReactionAdd,
        Some(ROOT),
        Some(ROOT),
        "👍",
    );
    me.sync(&relay);
    assert_eq!(stored_thread(&me, root), None);
    assert_eq!(stored_thread(&me, reply), Some(ROOT.to_vec()));
    assert_eq!(stored_thread(&me, react), None);
}

// ---------------------------------------------------------------------------------------------
// Step 4: the main timeline and the thread summary.

#[test]
fn a_reply_with_a_held_root_leaves_the_main_timeline() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    let first = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(ROOT),
        "one",
    );
    let second = post_in(
        &mut peer,
        &mut relay,
        0x74,
        Message,
        None,
        Some(ROOT),
        "two",
    );
    me.sync(&relay);
    assert_eq!(
        seqs(&me),
        vec![root],
        "the main timeline shows the root only"
    );
    let (_, rows) = thread_of(&me, &ROOT, 0, 50);
    let later = rows
        .iter()
        .find(|r| r.seq == second)
        .expect("second")
        .recv_ts;
    assert!(rows.iter().any(|r| r.seq == first));
    assert_eq!(me.row_at(&GROUP, root).thread, Some((2, later)));
}

#[test]
fn a_reply_with_an_unheld_root_stays_in_the_channel() {
    let (_i, mut relay, mut me, mut peer) = start();
    let reply = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(UNHELD),
        "orphan",
    );
    me.sync(&relay);
    assert_eq!(seqs(&me), vec![reply]);
    assert_eq!(me.row_at(&GROUP, reply).thread, None);
}

#[test]
fn a_nested_thread_shows_in_the_channel() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    let reply = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(ROOT),
        "a reply",
    );
    // A non-conforming peer names the reply as a thread root.
    let nested = post_in(
        &mut peer,
        &mut relay,
        0x75,
        Message,
        None,
        Some([0x72; 16]),
        "nested",
    );
    me.sync(&relay);
    assert_eq!(
        seqs(&me),
        vec![root, nested],
        "the reply is in its thread, the nested row in the channel"
    );
    assert_eq!(me.row_at(&GROUP, nested).thread, None);
    assert_eq!(me.row_at(&GROUP, root).thread.map(|t| t.0), Some(1));
    let (_, rows) = thread_of(&me, &ROOT, 0, 50);
    assert_eq!(rows.iter().map(|r| r.seq).collect::<Vec<_>>(), vec![reply]);
}

#[test]
fn a_deleted_reply_is_not_counted() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(ROOT),
        "one",
    );
    post_in(
        &mut peer,
        &mut relay,
        0x74,
        Message,
        None,
        Some(ROOT),
        "two",
    );
    me.sync(&relay);
    assert_eq!(me.row_at(&GROUP, root).thread.map(|t| t.0), Some(2));
    post_in(
        &mut peer,
        &mut relay,
        0x76,
        Delete,
        Some([0x74; 16]),
        None,
        "",
    );
    me.sync(&relay);
    assert_eq!(
        me.row_at(&GROUP, root).thread.map(|t| t.0),
        Some(1),
        "the tombstone is not counted"
    );
    assert_eq!(
        seqs(&me),
        vec![root],
        "the tombstone stays out of the channel"
    );
}

#[test]
fn a_root_without_replies_has_no_summary() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "alone");
    me.sync(&relay);
    assert_eq!(me.row_at(&GROUP, root).thread, None);
}

// ---------------------------------------------------------------------------------------------
// Step 5: the reads.

#[test]
fn thread_timeline_returns_the_root_and_the_replies_newest_first() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    let mut replies = Vec::new();
    for i in 0..5u8 {
        replies.push(post_in(
            &mut peer,
            &mut relay,
            0x80 + i,
            Message,
            None,
            Some(ROOT),
            &format!("r{i}"),
        ));
    }
    // A post outside the thread and a reaction on a reply are not thread rows.
    post_in(
        &mut peer,
        &mut relay,
        0x90,
        Message,
        None,
        None,
        "elsewhere",
    );
    post_in(
        &mut peer,
        &mut relay,
        0x91,
        ReactionAdd,
        Some([0x80; 16]),
        None,
        "👍",
    );
    me.sync(&relay);
    let (head, rows) = thread_of(&me, &ROOT, 0, 50);
    let head = head.expect("the root is held");
    assert_eq!((head.seq, head.body.as_str()), (root, "the root"));
    assert_eq!(head.thread.map(|t| t.0), Some(5));
    let newest_first: Vec<u64> = replies.iter().rev().copied().collect();
    assert_eq!(rows.iter().map(|r| r.seq).collect::<Vec<_>>(), newest_first);
    assert!(rows.iter().all(|r| r.ty == Some(0) && r.thread.is_none()));
    assert_eq!(
        rows.last().expect("oldest").reactions,
        vec![("👍".to_owned(), 1, 0)]
    );
    // Paging: before_seq bounds the page, limit sizes it.
    let (_, page) = thread_of(&me, &ROOT, replies[3], 2);
    assert_eq!(
        page.iter().map(|r| r.seq).collect::<Vec<_>>(),
        vec![replies[2], replies[1]]
    );
    let (_, rest) = thread_of(&me, &ROOT, replies[1], 50);
    assert_eq!(
        rest.iter().map(|r| r.seq).collect::<Vec<_>>(),
        vec![replies[0]]
    );
    // The limit's range is the timeline's.
    for bad in [0u32, 201] {
        assert_eq!(
            code(me.core.thread_timeline(&GROUP, &ROOT, 0, bad)),
            "E_CORE_INPUT"
        );
    }
    assert_eq!(
        code(me.core.thread_timeline(&OTHER_GROUP, &ROOT, 0, 50)),
        "E_CORE_NOT_FOUND"
    );
}

#[test]
fn an_unheld_root_has_a_null_root_row() {
    let (_i, mut relay, mut me, mut peer) = start();
    let reply = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(UNHELD),
        "orphan",
    );
    me.sync(&relay);
    let (head, rows) = thread_of(&me, &UNHELD, 0, 50);
    assert!(head.is_none());
    assert_eq!(rows.iter().map(|r| r.seq).collect::<Vec<_>>(), vec![reply]);
    // Fix round 1 (M1): a reply whose root is not held is no thread; it shows in the channel.
    assert_eq!(threads_of(&me), vec![]);
}

#[test]
fn threads_lists_the_groups_threads_by_their_last_reply() {
    let (_i, mut relay, mut me, mut peer) = start();
    let a = post_in(
        &mut peer,
        &mut relay,
        0x71,
        Message,
        None,
        None,
        "root a\nsecond line",
    );
    let b = post_in(&mut peer, &mut relay, 0x72, Message, None, None, "root b");
    post_in(
        &mut peer,
        &mut relay,
        0x73,
        Message,
        None,
        Some([0x71; 16]),
        "a1",
    );
    post_in(
        &mut peer,
        &mut relay,
        0x75,
        Message,
        None,
        Some([0x71; 16]),
        "a2",
    );
    // A nested thread (its root is a reply) is not a thread.
    post_in(
        &mut peer,
        &mut relay,
        0x76,
        Message,
        None,
        Some([0x73; 16]),
        "nested",
    );
    // b's reply is the latest: b lists first although its id sorts after a's.
    post_in(
        &mut peer,
        &mut relay,
        0x74,
        Message,
        None,
        Some([0x72; 16]),
        "b1",
    );
    me.sync(&relay);
    let rows = threads_of(&me);
    let (_, a_rows) = thread_of(&me, &[0x71; 16], 0, 50);
    let (_, b_rows) = thread_of(&me, &[0x72; 16], 0, 50);
    assert!(b_rows[0].recv_ts > a_rows[0].recv_ts);
    assert_eq!(
        rows,
        vec![
            ThreadRow {
                root: [0x72; 16],
                root_seq: Some(b),
                count: 1,
                last_ts: b_rows[0].recv_ts,
                excerpt: "root b".into()
            },
            ThreadRow {
                root: [0x71; 16],
                root_seq: Some(a),
                count: 2,
                last_ts: a_rows[0].recv_ts,
                excerpt: "root a second line".into()
            },
        ]
    );
}

#[test]
fn threads_returns_at_most_50() {
    let (_i, mut relay, mut me, mut peer) = start();
    for i in 0..51u8 {
        post_in(&mut peer, &mut relay, i, Message, None, None, "root");
        post_in(
            &mut peer,
            &mut relay,
            0x80 + i,
            Message,
            None,
            Some([i; 16]),
            "r",
        );
    }
    me.sync(&relay);
    let rows = threads_of(&me);
    assert_eq!(rows.len(), 50);
    assert!(
        rows.windows(2).all(|w| w[0].last_ts >= w[1].last_ts),
        "by last_ts descending"
    );
    assert_eq!(code(me.core.threads(&OTHER_GROUP)), "E_CORE_NOT_FOUND");
}

// ---------------------------------------------------------------------------------------------
// Step 6: counts.

#[test]
fn a_thread_reply_counts_toward_unread_and_mentions() {
    let (_i, mut relay, mut me, mut peer) = start();
    let (root, _) = me.send(&mut relay, &GROUP, "my root", NOW);
    let me_hex: String = me.user.iter().map(|b| format!("{b:02x}")).collect();
    post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(root),
        &format!("hey <@{me_hex}>"),
    );
    me.sync(&relay);
    let row = activity(&me.core)
        .into_iter()
        .find(|a| a.group_id == GROUP)
        .expect("activity");
    assert_eq!((row.unread, row.mentions), (1, 1));
    assert_eq!(
        seqs(&me).len(),
        1,
        "the reply is in its thread, not the channel"
    );
}

// ---------------------------------------------------------------------------------------------
// Fix round 1 (I1, M1, M2, M3): a thread belongs to the first root before it.

fn hits(bytes: &[u8]) -> Vec<(u64, Option<[u8; 16]>)> {
    dilla_core::cbor::decode_strict(bytes, |d| {
        let n = d.array_len()?;
        let mut out = Vec::new();
        for _ in 0..n {
            d.array(5)?;
            d.bytes_exact::<16>()?;
            let seq = d.uint()?;
            d.text()?;
            d.uint()?;
            out.push((seq, d.opt_bytes_exact::<16>()?));
        }
        Ok(out)
    })
    .expect("search hits")
}

/// X6: Mallory repeats the msg id of Bob's root after its replies; her row neither takes the
/// thread nor shows its summary.
#[test]
fn a_later_row_with_the_root_id_neither_takes_the_thread_nor_its_summary() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(
        &mut peer,
        &mut relay,
        0x71,
        Message,
        None,
        None,
        "bob's root",
    );
    let r1 = post_in(&mut peer, &mut relay, 0x72, Message, None, Some(ROOT), "r1");
    let r2 = post_in(&mut peer, &mut relay, 0x73, Message, None, Some(ROOT), "r2");
    let repeat = post_in(
        &mut peer,
        &mut relay,
        0x71,
        Message,
        None,
        None,
        "mallory reuses the id",
    );
    // A reply after the repeat still joins the first root.
    let r3 = post_in(&mut peer, &mut relay, 0x74, Message, None, Some(ROOT), "r3");
    me.sync(&relay);
    assert_eq!(seqs(&me), vec![root, repeat]);
    assert_eq!(me.row_at(&GROUP, root).thread.map(|t| t.0), Some(3));
    assert_eq!(
        me.row_at(&GROUP, repeat).thread,
        None,
        "the repeat borrows no summary"
    );
    let (head, rows) = thread_of(&me, &ROOT, 0, 50);
    assert_eq!(head.map(|h| h.seq), Some(root));
    assert_eq!(
        rows.iter().map(|r| r.seq).collect::<Vec<_>>(),
        vec![r3, r2, r1]
    );
    assert_eq!(
        threads_of(&me)
            .iter()
            .map(|t| (t.root_seq, t.count))
            .collect::<Vec<_>>(),
        vec![(Some(root), 3)]
    );
}

/// X2: a reply whose root this device does not hold stays in the channel when a later row
/// takes that msg id.
#[test]
fn a_later_root_does_not_capture_an_earlier_reply() {
    let (_i, mut relay, mut me, mut peer) = start();
    let reply = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(UNHELD),
        "orphan",
    );
    let forged = post_in(
        &mut peer,
        &mut relay,
        0xee,
        Message,
        None,
        None,
        "forged root",
    );
    me.sync(&relay);
    assert_eq!(seqs(&me), vec![reply, forged]);
    assert_eq!(me.row_at(&GROUP, forged).thread, None);
    let (_, rows) = thread_of(&me, &UNHELD, 0, 50);
    assert!(rows.is_empty(), "the forged root heads no earlier reply");
    assert_eq!(threads_of(&me), vec![]);
}

/// X3: repeating a reply's msg id as a root does not capture the nested reply under it.
#[test]
fn a_repeat_of_a_reply_id_does_not_capture_a_nested_reply() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "root");
    post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(ROOT),
        "reply",
    );
    let nested = post_in(
        &mut peer,
        &mut relay,
        0x75,
        Message,
        None,
        Some([0x72; 16]),
        "nested",
    );
    let repeat = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        None,
        "the reply's id as a root",
    );
    let later = post_in(
        &mut peer,
        &mut relay,
        0x76,
        Message,
        None,
        Some([0x72; 16]),
        "later",
    );
    me.sync(&relay);
    assert_eq!(
        seqs(&me),
        vec![root, nested, repeat, later],
        "the first row with 0x72 is a reply: no root"
    );
    assert_eq!(me.row_at(&GROUP, repeat).thread, None);
    assert!(thread_of(&me, &[0x72; 16], 0, 50).0.is_none());
}

/// X5: a reply before its root (by seq) and a reply naming its own msg id stay in the channel.
#[test]
fn a_reply_before_its_root_stays_in_the_channel() {
    let (_i, mut relay, mut me, mut peer) = start();
    let selfie = post_in(
        &mut peer,
        &mut relay,
        0x90,
        Message,
        None,
        Some([0x90; 16]),
        "self",
    );
    let early = post_in(
        &mut peer,
        &mut relay,
        0x91,
        Message,
        None,
        Some([0x92; 16]),
        "early",
    );
    let root = post_in(
        &mut peer,
        &mut relay,
        0x92,
        Message,
        None,
        None,
        "late root",
    );
    me.sync(&relay);
    assert_eq!(seqs(&me), vec![selfie, early, root]);
    assert_eq!(me.row_at(&GROUP, root).thread, None);
    let (head, rows) = thread_of(&me, &[0x92; 16], 0, 50);
    assert_eq!((head.map(|h| h.seq), rows.len()), (Some(root), 0));
}

/// M1: `threads` lists a thread only under a held root, never under an id a fold carries.
#[test]
fn threads_lists_only_held_roots() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "root");
    post_in(
        &mut peer,
        &mut relay,
        0x80,
        ReactionAdd,
        Some(ROOT),
        None,
        "👍",
    );
    let on_fold = post_in(
        &mut peer,
        &mut relay,
        0x81,
        Message,
        None,
        Some([0x80; 16]),
        "on a reaction id",
    );
    let orphan = post_in(
        &mut peer,
        &mut relay,
        0x82,
        Message,
        None,
        Some(UNHELD),
        "orphan",
    );
    post_in(
        &mut peer,
        &mut relay,
        0x83,
        Message,
        None,
        Some(ROOT),
        "real",
    );
    // A root whose msg id a fold row also carries is still listed (fix round 2, R1-I1): only a
    // held first type-0 root decides.
    let shared = post_in(
        &mut peer,
        &mut relay,
        0x77,
        Message,
        None,
        None,
        "shared id",
    );
    post_in(
        &mut peer,
        &mut relay,
        0x77,
        ReactionAdd,
        Some(ROOT),
        None,
        "🎉",
    );
    post_in(
        &mut peer,
        &mut relay,
        0x84,
        Message,
        None,
        Some([0x77; 16]),
        "under shared",
    );
    me.sync(&relay);
    assert_eq!(seqs(&me), vec![root, on_fold, orphan, shared]);
    assert_eq!(
        threads_of(&me).iter().map(|t| t.root).collect::<Vec<_>>(),
        vec![[0x77; 16], ROOT]
    );
}

/// Fix round 2 (R1-I1, probe Y1): a fold whose own msg id is a thread root's id (Mallory's reaction
/// on another message) does not delist the thread; the timeline, the summary and `threads` agree.
#[test]
fn a_fold_reusing_a_root_id_keeps_its_thread() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(
        &mut peer,
        &mut relay,
        0x71,
        Message,
        None,
        None,
        "bob's root",
    );
    let r1 = post_in(&mut peer, &mut relay, 0x72, Message, None, Some(ROOT), "r1");
    let other = post_in(&mut peer, &mut relay, 0x79, Message, None, None, "other");
    post_in(
        &mut peer,
        &mut relay,
        0x71,
        ReactionAdd,
        Some([0x79; 16]),
        None,
        "x",
    );
    me.sync(&relay);
    assert_eq!(seqs(&me), vec![root, other]);
    assert_eq!(me.row_at(&GROUP, root).thread.map(|t| t.0), Some(1));
    assert_eq!(
        thread_of(&me, &ROOT, 0, 50)
            .1
            .iter()
            .map(|r| r.seq)
            .collect::<Vec<_>>(),
        vec![r1]
    );
    assert_eq!(
        threads_of(&me)
            .iter()
            .map(|t| (t.root, t.root_seq))
            .collect::<Vec<_>>(),
        vec![(ROOT, Some(root))]
    );
}

/// M2: a deleted root keeps its replies in the thread and its tombstone carries the summary;
/// a member may still join it.
#[test]
fn a_deleted_root_keeps_its_summary() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "root");
    let reply = post_in(&mut peer, &mut relay, 0x72, Message, None, Some(ROOT), "r");
    post_in(&mut peer, &mut relay, 0x73, Delete, Some(ROOT), None, "");
    me.sync(&relay);
    let row = me.row_at(&GROUP, root);
    assert_eq!(row.status, 2);
    let reply_ts = thread_of(&me, &ROOT, 0, 50).1[0].recv_ts;
    assert_eq!(row.thread, Some((1, reply_ts)));
    assert_eq!(seqs(&me), vec![root]);
    assert_eq!(
        thread_of(&me, &ROOT, 0, 50)
            .1
            .iter()
            .map(|r| r.seq)
            .collect::<Vec<_>>(),
        vec![reply]
    );
    assert_eq!(
        threads_of(&me),
        vec![ThreadRow {
            root: ROOT,
            root_seq: Some(root),
            count: 1,
            last_ts: reply_ts,
            excerpt: String::new()
        }]
    );
    me.prepare_request(&GROUP, &thread_request(0, None, "late", Some(&ROOT)), NOW);
}

/// M3: search hits, pins and reply quotes name the thread root of a row shown in a thread, and
/// none for a channel row.
#[test]
fn a_thread_reply_is_reachable_from_search_pins_and_quotes() {
    let (_i, mut relay, mut me, mut peer) = start();
    let root = post_in(
        &mut peer,
        &mut relay,
        0x71,
        Message,
        None,
        None,
        "root lantern",
    );
    let reply = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(ROOT),
        "reply lantern",
    );
    post_in(&mut peer, &mut relay, 0x73, Pin, Some([0x72; 16]), None, "");
    post_in(&mut peer, &mut relay, 0x74, Pin, Some(ROOT), None, "");
    let quote = post_in(
        &mut peer,
        &mut relay,
        0x75,
        Message,
        Some([0x72; 16]),
        None,
        "quoting the reply",
    );
    let plain = post_in(
        &mut peer,
        &mut relay,
        0x76,
        Message,
        Some(ROOT),
        None,
        "quoting the root",
    );
    me.sync(&relay);
    let pins = decode_pins(&me.core.pins(&GROUP).expect("pins"));
    let pin_of = |seq: u64| {
        pins.iter()
            .find(|p| p.target_seq == seq)
            .expect("pin")
            .thread_root
    };
    assert_eq!((pin_of(reply), pin_of(root)), (Some(ROOT), None));
    let quoted = |seq: u64| me.row_at(&GROUP, seq).reply.expect("quote").thread_root;
    assert_eq!((quoted(quote), quoted(plain)), (Some(ROOT), None));
    let mut found = hits(
        &me.core
            .search(0, Some(&CHANNEL), "lantern", None, 50)
            .expect("search"),
    );
    found.sort();
    assert_eq!(found, vec![(root, None), (reply, Some(ROOT))]);
}

// ---------------------------------------------------------------------------------------------
// Wave C: threads apply to every text group, DMs and group DMs included. A DM thread reply is
// held out of the DM timeline like a channel's, and a DM send may name a root.

/// "me" (alice) created and registered DM_GROUP for DM_CHANNEL (no community); the raw peer
/// joined it by external commit and me ingested its device list.
fn start_dm() -> (Instance, Relay, Core, RawPeer) {
    let instance = Instance::generate();
    let mut relay = Relay::new(DM_GROUP);
    let mut me = ready_core(0xa1, "alice");
    me.create_and_register_in(&mut relay, &instance, None, &DM_CHANNEL);
    let mut peer = RawPeer::new(0xe5, 0xe6);
    assert_eq!(
        peer.join_external_as(&mut relay, &dm_binding(DM_CHANNEL)),
        1
    );
    me.sync(&relay);
    ingest_list_body(
        &mut me,
        &relay,
        peer.user,
        peer.device,
        &peer.device_list_body(),
    );
    (instance, relay, me, peer)
}

fn thread_of_dm(me: &Core, root: &[u8; 16]) -> (Option<TimelineRow>, Vec<TimelineRow>) {
    decode_thread_timeline(
        &me.core
            .thread_timeline(&DM_GROUP, root, 0, 50)
            .expect("thread_timeline"),
    )
}

#[test]
fn a_dm_thread_reply_is_stored_held_and_listed() {
    let (_i, mut relay, mut me, mut peer) = start_dm();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    let reply = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(ROOT),
        "a reply",
    );
    me.sync(&relay);
    let shown: Vec<u64> = me.timeline(&DM_GROUP).into_iter().map(|r| r.seq).collect();
    assert_eq!(
        shown,
        vec![root],
        "the reply is held out of the DM timeline"
    );
    assert_eq!(
        me.row_at(&DM_GROUP, root).thread.map(|t| t.0),
        Some(1),
        "the root counts its reply"
    );
    let listed = decode_threads(&me.core.threads(&DM_GROUP).expect("threads"));
    assert_eq!(listed.len(), 1, "threads lists the root");
    assert_eq!(listed[0].root_seq, Some(root));
    let (head, replies) = thread_of_dm(&me, &ROOT);
    assert_eq!(head.map(|h| h.seq), Some(root));
    assert_eq!(
        replies.iter().map(|r| r.seq).collect::<Vec<_>>(),
        vec![reply]
    );
    assert_eq!(
        stored_thread_in(&me, &DM_GROUP, reply),
        Some(ROOT.to_vec()),
        "a DM stores the thread id"
    );
}

fn stored_thread_in(me: &Core, group: &[u8; 16], seq: u64) -> Option<Vec<u8>> {
    me.probe
        .lock()
        .expect("lock")
        .query_row(
            "SELECT thread_id FROM app_messages WHERE group_id = ?1 AND seq = ?2",
            rusqlite::params![group.as_slice(), seq as i64],
            |r| r.get(0),
        )
        .expect("thread_id")
}

#[test]
fn send_prepare_in_a_dm_accepts_a_thread_id() {
    let (_i, mut relay, mut me, mut peer) = start_dm();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    me.sync(&relay);
    let msg_id = me.prepare_request(
        &DM_GROUP,
        &thread_request(0, None, "in a DM thread", Some(&ROOT)),
        NOW,
    );
    assert_eq!(me.outbox(&DM_GROUP).len(), 1, "the thread send queues");
    assert_eq!(
        outbox_thread(&me, &msg_id),
        Some(MsgId::from_bytes(ROOT)),
        "the queued envelope carries the thread id"
    );
    let seq = me.encrypt_and_confirm(&mut relay, &DM_GROUP, &msg_id);
    assert_eq!(
        stored_thread_in(&me, &DM_GROUP, seq),
        Some(ROOT.to_vec()),
        "the own row keeps the thread id"
    );
    let shown: Vec<u64> = me.timeline(&DM_GROUP).into_iter().map(|r| r.seq).collect();
    assert_eq!(
        shown,
        vec![root],
        "the own reply is held out of the DM timeline"
    );
    let (_, replies) = thread_of_dm(&me, &ROOT);
    assert_eq!(replies.iter().map(|r| r.seq).collect::<Vec<_>>(), vec![seq]);
}

#[test]
fn a_dm_thread_reply_counts_as_unread_and_its_root_resolves() {
    let (_i, mut relay, mut me, mut peer) = start_dm();
    let root = post_in(
        &mut peer,
        &mut relay,
        0x71,
        Message,
        None,
        None,
        "root lantern",
    );
    me.sync(&relay);
    let before = activity(&me.core)
        .into_iter()
        .find(|a| a.group_id == DM_GROUP)
        .expect("activity")
        .unread;
    let reply = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(ROOT),
        "reply lantern",
    );
    let quote = post_in(
        &mut peer,
        &mut relay,
        0x75,
        Message,
        Some([0x72; 16]),
        None,
        "quoting",
    );
    me.sync(&relay);
    let after = activity(&me.core)
        .into_iter()
        .find(|a| a.group_id == DM_GROUP)
        .expect("activity")
        .unread;
    assert_eq!(
        after,
        before + 2,
        "a held DM thread reply counts like a channel's"
    );
    let quoted = me
        .row_at(&DM_GROUP, quote)
        .reply
        .expect("quote")
        .thread_root;
    assert_eq!(quoted, Some(ROOT), "the quote names the thread root");
    let mut found = hits(
        &me.core
            .search(0, Some(&DM_CHANNEL), "lantern", None, 50)
            .expect("search"),
    );
    found.sort();
    assert_eq!(found, vec![(root, None), (reply, Some(ROOT))]);
}

#[test]
fn a_dm_thread_reply_before_its_root_is_held() {
    let (_i, mut relay, mut me, mut peer) = start_dm();
    let early = post_in(
        &mut peer,
        &mut relay,
        0x91,
        Message,
        None,
        Some([0x92; 16]),
        "early",
    );
    let root = post_in(
        &mut peer,
        &mut relay,
        0x92,
        Message,
        None,
        None,
        "late root",
    );
    let reply = post_in(
        &mut peer,
        &mut relay,
        0x93,
        Message,
        None,
        Some([0x92; 16]),
        "reply",
    );
    me.sync(&relay);
    let shown: Vec<u64> = me.timeline(&DM_GROUP).into_iter().map(|r| r.seq).collect();
    assert_eq!(
        shown,
        vec![early, root],
        "a reply before its root shows flat; the one after is held"
    );
    assert_eq!(me.row_at(&DM_GROUP, root).thread.map(|t| t.0), Some(1));
    let (head, rows) = thread_of_dm(&me, &[0x92; 16]);
    assert_eq!(
        (
            head.map(|h| h.seq),
            rows.iter().map(|r| r.seq).collect::<Vec<_>>()
        ),
        (Some(root), vec![reply])
    );
}

#[test]
fn a_pre_release_dm_row_without_thread_id_stays_in_the_timeline() {
    let (_i, mut relay, mut me, mut peer) = start_dm();
    let root = post_in(&mut peer, &mut relay, 0x71, Message, None, None, "the root");
    let reply = post_in(
        &mut peer,
        &mut relay,
        0x72,
        Message,
        None,
        Some(ROOT),
        "a reply",
    );
    me.sync(&relay);
    me.probe
        .lock()
        .expect("lock")
        .execute(
            "UPDATE app_messages SET thread_id = NULL WHERE group_id = ?1 AND seq = ?2",
            rusqlite::params![DM_GROUP.as_slice(), reply as i64],
        )
        .expect("plant a pre-release row");
    let shown: Vec<u64> = me.timeline(&DM_GROUP).into_iter().map(|r| r.seq).collect();
    assert_eq!(shown, vec![root, reply], "a NULL thread_id row stays flat");
    assert_eq!(me.row_at(&DM_GROUP, root).thread, None);
}
