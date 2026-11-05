// Native only: every test opens an in-memory SQLite connection.
#![cfg(not(target_arch = "wasm32"))]

//! web-2b task 2 (L-CORE-33…35, L-CORE-39): envelope types 1..6 fold onto their targets in the
//! apply unit; the timeline shows displayable rows only, with the folded body, the reply
//! reference, reactions per user, the pin flag and attachment summaries; pins, own roles and role
//! mentions. Every peer row is a real MLS application message from a `RawPeer`.

mod client_support;

use client_support::*;
use dilla_core::client::{ClientCore, ClientError, mentions_me_with_roles};
use dilla_core::envelope::Attachment;
use dilla_core::envelope::EnvelopeType::{
    self, Delete, Edit, Message, Pin, ReactionAdd, ReactionRemove, Unpin,
};

const PEER_USER: [u8; 16] = [0xe5; 16];
const READER_USER: [u8; 16] = [0xd4; 16];
const TARGET: [u8; 16] = [0x71; 16];
const UNHELD: [u8; 16] = [0xee; 16];
const ROLE: [u8; 16] = [0x3c; 16];
const OTHER_ROLE: [u8; 16] = [0x3d; 16];

/// "me" (alice, 0xa1) created and registered GROUP; the raw peer (user 0xe5, device 0xe6) joined
/// by external commit at seq 1; me applied it (next_seq 2). The parity start of L-CORE-10.
fn fold_start() -> (Instance, Relay, Core, RawPeer) {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut me = ready_core(0xa1, "alice");
    me.create_and_register(&mut relay, &instance);
    let mut peer = RawPeer::new(0xe5, 0xe6);
    assert_eq!(
        peer.join_external(&mut relay),
        1,
        "the peer's external commit is seq 1"
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

/// Another member joins by external commit; the raw members already in merge it; me syncs.
fn join(
    relay: &mut Relay,
    me: &mut Core,
    members: &mut [&mut RawPeer],
    user: u8,
    device: u8,
) -> RawPeer {
    let mut joiner = RawPeer::new(user, device);
    let seq = joiner.join_external(relay);
    for m in members.iter_mut() {
        m.apply_commit(relay, seq);
    }
    me.sync(relay);
    if let Some(first) = members.iter().find(|member| member.user == joiner.user) {
        ingest_raw_peer_pair(me, relay, first, &joiner);
    } else {
        ingest_list_body(
            me,
            relay,
            joiner.user,
            joiner.device,
            &joiner.device_list_body(),
        );
    }
    joiner
}

/// One envelope from `peer` with msg id 16 × `msg`; answers its seq.
fn post(
    peer: &mut RawPeer,
    relay: &mut Relay,
    msg: u8,
    kind: EnvelopeType,
    reply_to: Option<[u8; 16]>,
    body: &str,
) -> u64 {
    peer.send_envelope(relay, &fold_envelope([msg; 16], kind, reply_to, body))
}

fn row_at(me: &Core, seq: u64) -> TimelineRow {
    me.timeline(&GROUP)
        .into_iter()
        .find(|r| r.seq == seq)
        .unwrap_or_else(|| panic!("no displayable row at seq {seq}"))
}

fn seqs(me: &Core) -> Vec<u64> {
    me.timeline(&GROUP).into_iter().map(|r| r.seq).collect()
}

fn chips(me: &Core, seq: u64) -> Vec<(String, u64, u64)> {
    row_at(me, seq).reactions
}

fn chip(emoji: &str, count: u64, mine: u64) -> (String, u64, u64) {
    (emoji.to_owned(), count, mine)
}

fn page(me: &Core, before: u64, limit: u32) -> Vec<u64> {
    decode_timeline(&me.core.timeline(&GROUP, before, limit).expect("timeline"))
        .into_iter()
        .map(|r| r.seq)
        .collect()
}

fn count(me: &Core, sql: &str) -> i64 {
    me.probe
        .lock()
        .expect("lock")
        .query_row(sql, [], |r| r.get(0))
        .expect("count")
}

fn pins(me: &Core) -> Vec<PinRow> {
    decode_pins(&me.core.pins(&GROUP).expect("pins"))
}

// ---------------------------------------------------------------------------------------------
// L-CORE-39: the fold parity fixture against the real core.

/// Builds one fold-fixture row; answers its seq. `target` is set by `peer-target`/`own-target`;
/// row `i` of a case carries msg id 16 × (0xa0 + i).
fn build_fold_row(
    what: &str,
    i: usize,
    relay: &mut Relay,
    me: &mut Core,
    peer: &mut RawPeer,
    target: &mut [u8; 16],
) -> u64 {
    let own = 0xa0 + i as u8;
    match what {
        "peer-target" => {
            *target = TARGET;
            post(peer, relay, 0x71, Message, None, "the target")
        }
        "own-target" => {
            let msg_id = me.prepare(&GROUP, "my target", NOW);
            *target = msg_id;
            let (_, body) = me.encrypt(&msg_id);
            seq_of_answer(&relay.post_message(me.device, &body).expect("upload"))
        }
        "peer-edit" => post(peer, relay, own, Edit, Some(*target), "edited by the peer"),
        "peer-react" => post(peer, relay, own, ReactionAdd, Some(*target), "👍"),
        "peer-unreact" => post(peer, relay, own, ReactionRemove, Some(*target), "👍"),
        "peer-delete" => post(peer, relay, own, Delete, Some(*target), ""),
        "peer-pin" => post(peer, relay, own, Pin, Some(*target), ""),
        "peer-react-unheld" => post(peer, relay, own, ReactionAdd, Some(UNHELD), "👍"),
        "own-react" => {
            me.sync(relay);
            let msg_id = me.prepare_request(&GROUP, &request(3, Some(&TARGET), "👍", &[]), NOW);
            let (_, body) = me.encrypt(&msg_id);
            seq_of_answer(&relay.post_message(me.device, &body).expect("upload"))
        }
        other => panic!("unknown row kind {other}"),
    }
}

#[test]
fn the_fold_parity_fixture_holds() {
    let fixture: serde_json::Value =
        serde_json::from_str(include_str!("fixtures/fold_parity.json")).expect("fixture JSON");
    assert_eq!(fixture["v"].as_u64(), Some(1));
    let cases = fixture["cases"].as_array().expect("cases");
    assert_eq!(cases.len(), 7, "the seven cases of L-CORE-39");
    for case in cases {
        let name = case["name"].as_str().expect("name");
        let (_instance, mut relay, mut me, mut peer) = fold_start();
        let mut target = [0u8; 16];
        for (i, row) in case["rows"].as_array().expect("rows").iter().enumerate() {
            let what = row["what"].as_str().expect("what");
            let seq = row["seq"].as_u64().expect("seq");
            assert_eq!(
                build_fold_row(what, i, &mut relay, &mut me, &mut peer, &mut target),
                seq,
                "case {name}: {what} lands at seq {seq}"
            );
        }
        let through = case["through"].as_u64().expect("through");
        me.core
            .group_apply(
                &GROUP,
                &relay.handshakes_from(2),
                &relay.messages_from(2),
                through,
            )
            .expect("group_apply");
        let rows = me.timeline(&GROUP);
        for v in case["view"].as_array().expect("view") {
            let v = v.as_array().expect("a view entry");
            let seq = v[0].as_u64().expect("seq");
            let row = rows
                .iter()
                .find(|r| r.seq == seq)
                .unwrap_or_else(|| panic!("case {name}: no row at seq {seq}"));
            let reactions: Vec<(String, u64, u64)> = v[4]
                .as_array()
                .expect("reactions")
                .iter()
                .map(|r| {
                    (
                        r[0].as_str().expect("emoji").to_owned(),
                        r[1].as_u64().expect("count"),
                        r[2].as_u64().expect("mine"),
                    )
                })
                .collect();
            assert_eq!(
                (
                    row.status,
                    row.body.as_str(),
                    row.edited_seq,
                    row.reactions.clone(),
                    row.pinned
                ),
                (
                    v[1].as_u64().expect("status"),
                    v[2].as_str().expect("body"),
                    v[3].as_u64().expect("edited_seq"),
                    reactions,
                    v[5].as_u64().expect("pinned")
                ),
                "case {name}: the row at seq {seq}"
            );
        }
        for h in case["hidden"].as_array().expect("hidden") {
            let seq = h.as_u64().expect("seq");
            assert!(
                rows.iter().all(|r| r.seq != seq),
                "case {name}: seq {seq} is hidden"
            );
        }
    }
}

// ---------------------------------------------------------------------------------------------
// L-CORE-34: the view.

#[test]
fn fold_rows_are_hidden_and_take_no_page_slot() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        2
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, ReactionAdd, Some(TARGET), "👍"),
        3
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa2, ReactionAdd, Some(TARGET), "🎉"),
        4
    );
    assert_eq!(post(&mut peer, &mut relay, 0xa3, Pin, Some(TARGET), ""), 5);
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x72,
            Message,
            None,
            "after the folds"
        ),
        6
    );
    me.sync(&relay);
    assert_eq!(
        page(&me, 0, 200),
        vec![2, 6],
        "only type-0 rows are displayable"
    );
    assert_eq!(
        page(&me, 0, 2),
        vec![2, 6],
        "the three fold rows between them take no slot"
    );
    assert_eq!(page(&me, 6, 1), vec![2]);
    assert_eq!(
        count(
            &me,
            "SELECT count(*) FROM app_messages WHERE type IN (3, 5)"
        ),
        3,
        "fold rows are stored"
    );
    // A row of NULL type (cannot read) stays displayable.
    let epoch = relay.epoch();
    let pruned = seq_of_answer(&relay.push_message(peer.device, epoch, None, false));
    me.sync(&relay);
    assert_eq!(page(&me, 0, 200), vec![2, 6, pruned]);
    assert_eq!(row_at(&me, pruned).status, 1);
}

/// L-CORE-33 step 3: the author is a user. Mutation: compare `sender_device` → red here.
#[test]
fn an_edit_from_the_authors_other_device_applies_and_the_highest_seq_wins() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut twin = join(&mut relay, &mut me, &mut [&mut peer], 0xe5, 0xe7);
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        3
    );
    assert_eq!(
        post(
            &mut twin,
            &mut relay,
            0xa1,
            Edit,
            Some(TARGET),
            "edited on the other device"
        ),
        4
    );
    me.sync(&relay);
    let row = row_at(&me, 3);
    assert_eq!(
        (row.body.as_str(), row.edited_seq),
        ("edited on the other device", 4)
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0xa2,
            Edit,
            Some(TARGET),
            "edited again"
        ),
        5
    );
    me.sync(&relay);
    let row = row_at(&me, 3);
    assert_eq!((row.body.as_str(), row.edited_seq), ("edited again", 5));
    assert_eq!(seqs(&me), vec![3]);
    // Ruling 4: an edit never raises a mention.
    let mention = format!("now <@{}>", "a1".repeat(16));
    assert_eq!(
        post(&mut peer, &mut relay, 0xa3, Edit, Some(TARGET), &mention),
        6
    );
    me.sync(&relay);
    assert_eq!((row_at(&me, 3).body, row_at(&me, 3).mention), (mention, 0));
    assert_eq!(
        count(&me, "SELECT count(*) FROM app_messages WHERE mention = 1"),
        0
    );
}

/// Attacker statement (L-CORE-33): an edit or a delete from anyone but the target's author
/// changes nothing; steps 2-3 compare the MLS-authenticated `sender_user`. Mutation: drop the
/// author comparison of step 2 → red here.
#[test]
fn a_delete_or_an_edit_by_another_user_is_ignored() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut mallory = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        3
    );
    assert_eq!(
        post(
            &mut mallory,
            &mut relay,
            0xa1,
            Edit,
            Some(TARGET),
            "hijacked"
        ),
        4
    );
    assert_eq!(
        post(&mut mallory, &mut relay, 0xa2, Delete, Some(TARGET), ""),
        5
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x72,
            Message,
            None,
            "the positive control"
        ),
        6
    );
    me.sync(&relay);
    assert_eq!(seqs(&me), vec![3, 6]);
    let row = row_at(&me, 3);
    assert_eq!(
        (row.status, row.body.as_str(), row.edited_seq),
        (0, "the target", 0)
    );
    assert_eq!(
        count(
            &me,
            "SELECT count(*) FROM app_messages WHERE seq = 4 AND status = 0 AND body = 'hijacked'"
        ),
        1,
        "the ignored edit is stored as received"
    );
}

#[test]
fn a_delete_by_the_author_removes_the_target_its_edits_reactions_and_pin() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        2
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, ReactionAdd, Some(TARGET), "👍"),
        3
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa2, Edit, Some(TARGET), "edited"),
        4
    );
    assert_eq!(post(&mut peer, &mut relay, 0xa3, Pin, Some(TARGET), ""), 5);
    me.sync(&relay);
    let row = row_at(&me, 2);
    assert_eq!(
        (
            row.body.as_str(),
            row.edited_seq,
            row.reactions.len(),
            row.pinned
        ),
        ("edited", 4, 1, 1)
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa4, Delete, Some(TARGET), ""),
        6
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa5, ReactionAdd, Some(TARGET), "🎉"),
        7
    );
    me.sync(&relay);
    let row = row_at(&me, 2);
    assert_eq!(
        (
            row.status,
            row.body.as_str(),
            row.edited_seq,
            row.reactions.clone(),
            row.pinned,
            row.attachments.clone()
        ),
        (2, "", 0, vec![], 0, vec![])
    );
    assert_eq!(seqs(&me), vec![2]);
    assert!(pins(&me).is_empty());
    assert_eq!(
        count(
            &me,
            "SELECT count(*) FROM app_messages WHERE seq IN (2, 4) AND envelope IS NULL AND body = ''"
        ),
        2,
        "the target and its edit lose their words"
    );
    assert_eq!(count(&me, "SELECT count(*) FROM app_reactions"), 0);
    assert_eq!(count(&me, "SELECT count(*) FROM app_pins"), 0);
}

/// L-CORE-33 step 4 and its attacker statement: a reaction is the user's, from any of their
/// devices; nobody removes another user's reaction. Mutation: key reactions by device → red here.
#[test]
fn reactions_are_kept_per_user() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut twin = join(&mut relay, &mut me, &mut [&mut peer], 0xe5, 0xe7);
    let mut reader = join(&mut relay, &mut me, &mut [&mut peer, &mut twin], 0xd4, 0xd5);
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        4
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, ReactionAdd, Some(TARGET), "👍"),
        5
    );
    assert_eq!(
        post(&mut twin, &mut relay, 0xa2, ReactionAdd, Some(TARGET), "👍"),
        6
    );
    me.sync(&relay);
    assert_eq!(
        chips(&me, 4),
        vec![chip("👍", 1, 0)],
        "two devices of one user are one reaction"
    );
    assert_eq!(
        post(
            &mut reader,
            &mut relay,
            0xa3,
            ReactionAdd,
            Some(TARGET),
            "🎉"
        ),
        7
    );
    assert_eq!(
        post(
            &mut reader,
            &mut relay,
            0xa4,
            ReactionAdd,
            Some(TARGET),
            "👍"
        ),
        8
    );
    me.sync(&relay);
    assert_eq!(chips(&me, 4), vec![chip("👍", 2, 0), chip("🎉", 1, 0)]);
    assert_eq!(
        post(
            &mut twin,
            &mut relay,
            0xa5,
            ReactionRemove,
            Some(TARGET),
            "👍"
        ),
        9
    );
    me.sync(&relay);
    assert_eq!(
        chips(&me, 4),
        vec![chip("🎉", 1, 0), chip("👍", 1, 0)],
        "the user's removal from the other device wins by seq; chips order by their first present seq"
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0xa6,
            ReactionRemove,
            Some(TARGET),
            "🎉"
        ),
        10
    );
    me.sync(&relay);
    assert_eq!(
        chips(&me, 4),
        vec![chip("🎉", 1, 0), chip("👍", 1, 0)],
        "nobody removes another user's reaction"
    );
    assert_eq!(
        post(
            &mut reader,
            &mut relay,
            0xa7,
            ReactionRemove,
            Some(TARGET),
            "🎉"
        ),
        11
    );
    assert_eq!(
        post(
            &mut reader,
            &mut relay,
            0xa8,
            ReactionAdd,
            Some(TARGET),
            "🎉"
        ),
        12
    );
    me.sync(&relay);
    assert_eq!(chips(&me, 4), vec![chip("👍", 1, 0), chip("🎉", 1, 0)]);
    assert_eq!(
        count(
            &me,
            "SELECT count(*) FROM app_reactions WHERE user_id = x'e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5'"
        ),
        0
    );
}

#[test]
fn a_reply_shows_its_target_held_missing_or_deleted() {
    const DOOMED: [u8; 16] = [0x74; 16];
    let (_i, mut relay, mut me, mut peer) = fold_start();
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x71,
            Message,
            None,
            "first line\nsecond\rthird"
        ),
        2
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x72,
            Message,
            Some(TARGET),
            "re held"
        ),
        3
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x73,
            Message,
            Some(UNHELD),
            "re missing"
        ),
        4
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0x74, Message, None, "doomed"),
        5
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x75,
            Message,
            Some(DOOMED),
            "re deleted"
        ),
        6
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, Delete, Some(DOOMED), ""),
        7
    );
    me.sync(&relay);
    assert_eq!(row_at(&me, 2).reply, None);
    assert_eq!(
        row_at(&me, 3).reply,
        Some(Reply {
            reply_to: TARGET,
            target_seq: Some(2),
            target_user: Some(PEER_USER),
            excerpt: "first line second third".into(),
            state: 0,
            thread_root: None
        })
    );
    assert_eq!(
        row_at(&me, 4).reply,
        Some(Reply {
            reply_to: UNHELD,
            target_seq: None,
            target_user: None,
            excerpt: String::new(),
            state: 1,
            thread_root: None
        })
    );
    assert_eq!(
        row_at(&me, 6).reply,
        Some(Reply {
            reply_to: DOOMED,
            target_seq: Some(5),
            target_user: Some(PEER_USER),
            excerpt: String::new(),
            state: 2,
            thread_root: None
        })
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0xa2,
            Edit,
            Some(TARGET),
            "edited target"
        ),
        8
    );
    me.sync(&relay);
    assert_eq!(
        row_at(&me, 3).reply.expect("a reply").excerpt,
        "edited target"
    );
}

#[test]
fn the_excerpt_is_the_first_120_scalar_values_with_line_breaks_as_spaces() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let long = format!("{}\n{}", "å".repeat(119), "ö".repeat(20));
    assert_eq!(post(&mut peer, &mut relay, 0x71, Message, None, &long), 2);
    assert_eq!(
        post(&mut peer, &mut relay, 0x72, Message, Some(TARGET), "re"),
        3
    );
    assert_eq!(post(&mut peer, &mut relay, 0xa1, Pin, Some(TARGET), ""), 4);
    me.sync(&relay);
    let want = format!("{} ", "å".repeat(119));
    let reply = row_at(&me, 3).reply.expect("a reply");
    assert_eq!(reply.excerpt, want);
    assert_eq!(reply.excerpt.chars().count(), 120);
    assert_eq!(pins(&me)[0].excerpt, want);
    assert_eq!(
        row_at(&me, 2).body,
        long,
        "the row itself shows its whole body"
    );
}

/// L-CORE-33 step 1: a fold whose target is not held is kept and hidden. A type 0 sequenced after
/// it with the id it names is a repeat of an original this device never held, never its target
/// (protocol/04 § Semantics, Target; lead "fold spoofing").
#[test]
fn a_fold_waits_hidden_and_a_later_type_0_with_its_id_is_no_target() {
    const LATE: [u8; 16] = [0x72; 16];
    let (_i, mut relay, mut me, mut peer) = fold_start();
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, ReactionAdd, Some(LATE), "👍"),
        2
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0xa2,
            Edit,
            Some(LATE),
            "edited before it arrived"
        ),
        3
    );
    me.sync(&relay);
    assert!(me.timeline(&GROUP).is_empty(), "nothing displayable yet");
    assert_eq!(count(&me, "SELECT count(*) FROM app_reactions"), 0);
    assert_eq!(
        count(
            &me,
            "SELECT count(*) FROM app_messages WHERE type IN (1, 3)"
        ),
        2
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0x72, Message, None, "late target"),
        4
    );
    me.sync(&relay);
    let row = row_at(&me, 4);
    assert_eq!(
        (row.body.as_str(), row.edited_seq, row.reactions.clone()),
        ("late target", 0, vec![])
    );
    assert_eq!(count(&me, "SELECT count(*) FROM app_reactions"), 0);
}

/// Card 20 of web-1 and (ruled: AI-4): a later type-0 row that repeats a msg id is its own
/// message and never a target.
#[test]
fn a_replayed_msg_id_is_its_own_message_and_never_a_target() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "original"),
        2
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "replay"),
        3
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, Edit, Some(TARGET), "edited"),
        4
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa2, ReactionAdd, Some(TARGET), "👍"),
        5
    );
    assert_eq!(post(&mut peer, &mut relay, 0xa3, Pin, Some(TARGET), ""), 6);
    me.sync(&relay);
    let first = row_at(&me, 2);
    assert_eq!(
        (
            first.body.as_str(),
            first.edited_seq,
            first.reactions.len(),
            first.pinned
        ),
        ("edited", 4, 1, 1)
    );
    let replay = row_at(&me, 3);
    assert_eq!(
        (
            replay.body.as_str(),
            replay.edited_seq,
            replay.reactions.len(),
            replay.pinned
        ),
        ("replay", 0, 0, 0)
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa4, Delete, Some(TARGET), ""),
        7
    );
    me.sync(&relay);
    assert_eq!(
        (
            row_at(&me, 2).status,
            row_at(&me, 3).status,
            row_at(&me, 3).body
        ),
        (2, 0, "replay".to_owned())
    );
}

/// Lead "fold spoofing": the original is deleted before this receiver fetched it, so it holds a
/// tombstone without a msg id; a member who saw the original repeats its msg id. The repeat is
/// shown as its own message and collects none of the reactions, pins or replies sent for the
/// original (protocol/04 § Semantics, Target).
#[test]
fn a_repeat_of_a_deleted_unfetched_original_collects_none_of_its_folds() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut mallory = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "original"),
        3
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, ReactionAdd, Some(TARGET), "👍"),
        4
    );
    assert_eq!(post(&mut peer, &mut relay, 0xa2, Pin, Some(TARGET), ""), 5);
    assert_eq!(
        post(&mut peer, &mut relay, 0xa3, Message, Some(TARGET), "agreed"),
        6
    );
    relay.delete_message(3);
    assert_eq!(
        post(
            &mut mallory,
            &mut relay,
            0x71,
            Message,
            None,
            "new payment address"
        ),
        7
    );
    me.sync(&relay);
    let repeat = row_at(&me, 7);
    assert_eq!(
        (repeat.body.as_str(), repeat.reactions.len(), repeat.pinned),
        ("new payment address", 0, 0),
        "the repeat collects no reaction or pin of the original"
    );
    assert!(pins(&me).is_empty(), "the original's pin lists nothing");
    let reply = row_at(&me, 6).reply.expect("a reply");
    assert_eq!(
        (reply.target_seq, reply.excerpt.as_str()),
        (None, ""),
        "the earlier reply does not quote the repeat"
    );
    assert_eq!(
        refusal(
            me.core
                .send_prepare(&GROUP, &request(3, Some(&TARGET), "👍", &[]), NOW)
        )
        .0,
        "E_CORE_NOT_FOUND",
        "the repeat is not a target to react to"
    );
}

/// The delivery service's deletion (gateway op 21) of a fold row or of the target refolds.
#[test]
fn a_delivery_service_delete_refolds_what_the_row_touched() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        2
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, ReactionAdd, Some(TARGET), "👍"),
        3
    );
    assert_eq!(post(&mut peer, &mut relay, 0xa2, Pin, Some(TARGET), ""), 4);
    assert_eq!(
        post(&mut peer, &mut relay, 0xa3, Edit, Some(TARGET), "edited"),
        5
    );
    me.sync(&relay);
    assert_eq!(
        decode_applied(&me.core.message_deleted(&GROUP, 3).expect("op 21")).new_seqs,
        vec![3]
    );
    assert!(
        chips(&me, 2).is_empty(),
        "the deleted reaction row no longer counts"
    );
    me.core.message_deleted(&GROUP, 5).expect("op 21");
    let row = row_at(&me, 2);
    assert_eq!(
        (row.body.as_str(), row.edited_seq, row.pinned),
        ("the target", 0, 1),
        "the original body shows again"
    );
    me.core.message_deleted(&GROUP, 2).expect("op 21");
    let row = row_at(&me, 2);
    assert_eq!(
        (row.status, row.pinned),
        (2, 0),
        "the tombstone deletes the target and its pin"
    );
    assert_eq!(count(&me, "SELECT count(*) FROM app_pins"), 0);
    assert_eq!(
        post(&mut peer, &mut relay, 0xa4, Delete, Some(TARGET), ""),
        6
    );
    me.sync(&relay);
    assert_eq!(
        row_at(&me, 2).status,
        2,
        "a type 2 after the tombstone changes nothing more"
    );
}

/// Rule 2 on an own row confirmed ahead of next_seq: the served deletion refolds its target.
#[test]
fn a_served_deletion_of_an_own_row_stored_ahead_deletes_its_folds() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mine = me.prepare(&GROUP, "mine", NOW);
    let (_, body) = me.encrypt(&mine);
    let answer = relay.post_message(me.device, &body).expect("upload");
    assert_eq!(seq_of_answer(&answer), 2);
    me.core.send_confirm(&mine, &answer).expect("send_confirm");
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, ReactionAdd, Some(mine), "👍"),
        3
    );
    relay.delete_message(2);
    me.sync(&relay);
    let row = row_at(&me, 2);
    assert_eq!((row.status, row.reactions.len()), (2, 0));
    assert_eq!(count(&me, "SELECT count(*) FROM app_reactions"), 0);
}

/// A fold sequenced before an own message (its id cannot be known before the delivery service
/// sequences it) never applies to it, also when the confirm stores the message after the fold
/// (protocol/04 § Semantics, Target; lead "fold spoofing").
#[test]
fn an_own_message_confirmed_after_a_fold_named_it_is_no_target_of_it() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mine = me.prepare(&GROUP, "mine", NOW);
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, ReactionAdd, Some(mine), "👍"),
        2
    );
    me.sync(&relay);
    assert!(seqs(&me).is_empty(), "the reaction waits, hidden");
    let (_, body) = me.encrypt(&mine);
    let answer = relay.post_message(me.device, &body).expect("upload");
    assert_eq!(seq_of_answer(&answer), 3);
    me.core.send_confirm(&mine, &answer).expect("send_confirm");
    assert_eq!(
        (row_at(&me, 3).body.as_str(), chips(&me, 3)),
        ("mine", vec![]),
        "the confirm shows the message without the earlier reaction"
    );
    assert_eq!(count(&me, "SELECT count(*) FROM app_reactions"), 0);
}

// ---------------------------------------------------------------------------------------------
// L-CORE-35: pins, own roles, role mentions.

/// Attacker statement (Q20): a pin or unpin from any member is accepted; `pins` names who pinned.
#[test]
fn pins_list_newest_first_and_name_who_pinned() {
    const A: [u8; 16] = [0x72; 16];
    const B: [u8; 16] = [0x73; 16];
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut reader = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    assert_eq!(
        post(&mut peer, &mut relay, 0x72, Message, None, "first pinned"),
        3
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0x73, Message, None, "second pinned"),
        4
    );
    assert_eq!(post(&mut reader, &mut relay, 0xa1, Pin, Some(A), ""), 5);
    assert_eq!(post(&mut peer, &mut relay, 0xa2, Pin, Some(B), ""), 6);
    me.sync(&relay);
    let a_ts = row_at(&me, 3).recv_ts;
    let pin_b = PinRow {
        target_seq: 4,
        msg_id: B,
        pinned_seq: 6,
        by_user: PEER_USER,
        author: Some(PEER_USER),
        excerpt: "second pinned".into(),
        target_ts: row_at(&me, 4).recv_ts,
        thread_root: None,
    };
    assert_eq!(
        pins(&me),
        vec![
            pin_b.clone(),
            PinRow {
                target_seq: 3,
                msg_id: A,
                pinned_seq: 5,
                by_user: READER_USER,
                author: Some(PEER_USER),
                excerpt: "first pinned".into(),
                target_ts: a_ts,
                thread_root: None,
            },
        ]
    );
    assert_eq!(post(&mut peer, &mut relay, 0xa3, Unpin, Some(A), ""), 7);
    me.sync(&relay);
    assert_eq!(pins(&me), vec![pin_b.clone()], "any member unpins");
    assert_eq!(row_at(&me, 3).pinned, 0);
    assert_eq!(post(&mut reader, &mut relay, 0xa4, Pin, Some(A), ""), 8);
    me.sync(&relay);
    assert_eq!(
        pins(&me),
        vec![
            PinRow {
                target_seq: 3,
                msg_id: A,
                pinned_seq: 8,
                by_user: READER_USER,
                author: Some(PEER_USER),
                excerpt: "first pinned".into(),
                target_ts: a_ts,
                thread_root: None,
            },
            pin_b,
        ]
    );
    assert_eq!(code(me.core.pins(&OTHER_GROUP)), "E_CORE_NOT_FOUND");
}

/// Q20, ruling 12: role mentions count for the roles learned before the row is applied; no backfill.
#[test]
fn own_roles_flag_role_mentions_from_then_on() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let role_body = format!("standup <@{}>", "3c".repeat(16));
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, &role_body),
        2
    );
    me.sync(&relay);
    me.core
        .own_roles_set(&COMMUNITY, &[ROLE, [0x3e; 16]].concat())
        .expect("own_roles_set");
    assert_eq!(
        row_at(&me, 2).mention,
        0,
        "a row applied before the roles were known keeps its flag"
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0x72, Message, None, &role_body),
        3
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x73,
            Message,
            None,
            &format!("ops <@{}>", "3d".repeat(16))
        ),
        4
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x74,
            Message,
            None,
            &format!("shout <@{}>", "3C".repeat(16))
        ),
        5
    );
    me.sync(&relay);
    assert_eq!(
        (
            row_at(&me, 3).mention,
            row_at(&me, 4).mention,
            row_at(&me, 5).mention
        ),
        (1, 0, 0)
    );
    assert_eq!(count(&me, "SELECT count(*) FROM app_roles"), 2);
    me.core.own_roles_set(&COMMUNITY, &[]).expect("cleared");
    assert_eq!(
        count(&me, "SELECT count(*) FROM app_roles"),
        0,
        "the set is replaced, not merged"
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0x75, Message, None, &role_body),
        6
    );
    me.sync(&relay);
    assert_eq!(row_at(&me, 6).mention, 0);
    me.core
        .own_roles_set(&[0x23; 16], &ROLE)
        .expect("another community");
    assert_eq!(
        post(&mut peer, &mut relay, 0x76, Message, None, &role_body),
        7
    );
    me.sync(&relay);
    assert_eq!(
        row_at(&me, 7).mention,
        0,
        "a role of another community does not count here"
    );
    for bad in [vec![0u8; 17], vec![0u8; 1040]] {
        assert_eq!(
            me.core.own_roles_set(&COMMUNITY, &bad),
            Err(ClientError {
                code: "E_CORE_INPUT",
                detail: "role_ids must be 0..=64 ids of 16 bytes".into()
            })
        );
    }
    let sixty_four: Vec<u8> = (0..64u8).flat_map(|i| [i; 16]).collect();
    me.core
        .own_roles_set(&COMMUNITY, &sixty_four)
        .expect("64 ids");
    assert_eq!(count(&me, "SELECT count(*) FROM app_roles"), 65);
}

#[test]
fn mentions_me_with_roles_matches_the_tokens_literally() {
    let me = [0xa1; 16];
    let roles = [ROLE, OTHER_ROLE];
    for (body, want) in [
        (format!("<@{}>", "a1".repeat(16)), true),
        (format!("hey <@{}> team", "3c".repeat(16)), true),
        (format!("<@{}>", "3d".repeat(16)), true),
        (format!("<@{}>", "3e".repeat(16)), false),
        (format!("<@{}>", "3C".repeat(16)), false),
        ("<@everyone>".to_owned(), true),
        ("<@here>".to_owned(), true),
        (format!("<@{}", "3c".repeat(16)), false),
        (String::new(), false),
    ] {
        assert_eq!(mentions_me_with_roles(&body, &me, &roles), want, "{body:?}");
    }
    assert!(!mentions_me_with_roles(
        &format!("<@{}>", "3c".repeat(16)),
        &me,
        &[]
    ));
}

#[test]
fn an_attachment_summary_is_read_from_the_envelope() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut env = fold_envelope([0x72; 16], Message, None, "");
    env.attachments = vec![
        Attachment {
            blob_id: [0x43; 32],
            key: [0x44; 32],
            nonce: [0x45; 12],
            size: 1234,
            mime: "image/png".into(),
            w: Some(640),
            h: Some(480),
            thumb: Some(vec![0x01; 42]),
            name: "map.png".into(),
        },
        Attachment {
            blob_id: [0x53; 32],
            key: [0x54; 32],
            nonce: [0x55; 12],
            size: 70_000,
            mime: "application/pdf".into(),
            w: None,
            h: None,
            thumb: None,
            name: String::new(),
        },
    ];
    let seq = peer.send_envelope(&mut relay, &env);
    me.sync(&relay);
    assert_eq!(
        row_at(&me, seq).attachments,
        vec![
            AttachmentSummary {
                index: 0,
                size: 1234,
                mime: "image/png".into(),
                w: Some(640),
                h: Some(480),
                has_thumb: 1,
                name: "map.png".into()
            },
            AttachmentSummary {
                index: 1,
                size: 70_000,
                mime: "application/pdf".into(),
                w: None,
                h: None,
                has_thumb: 0,
                name: String::new()
            },
        ],
        "an attachment-only message (empty body) shows its files"
    );
    me.core.message_deleted(&GROUP, seq).expect("op 21");
    assert!(row_at(&me, seq).attachments.is_empty());
}

#[test]
fn the_views_need_an_identity() {
    let mut core = ClientCore::open(memory()).expect("open");
    assert_eq!(code(core.timeline(&GROUP, 0, 10)), "E_CORE_NO_IDENTITY");
    assert_eq!(code(core.pins(&GROUP)), "E_CORE_NO_IDENTITY");
    assert_eq!(
        code(core.own_roles_set(&COMMUNITY, &[])),
        "E_CORE_NO_IDENTITY"
    );
    assert_eq!(
        code(core.authority_set(&GROUP, &[], &[], &[])),
        "E_CORE_NO_IDENTITY"
    );
}

/// FACTS-SECURITY-03 and lesson e: a device that never held the target (the delivery service's
/// tombstone reached it first) still drops the author's edits named by the author's delete. Only
/// a user's own delete blanks only that user's words: another user's edit keeps its body.
#[test]
fn a_delete_of_a_target_never_held_blanks_its_authors_edits() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut reader = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        3
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0xa1,
            Edit,
            Some(TARGET),
            "secret words"
        ),
        4
    );
    assert_eq!(
        post(
            &mut reader,
            &mut relay,
            0xa2,
            Edit,
            Some(TARGET),
            "reader words"
        ),
        5
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa3, Delete, Some(TARGET), ""),
        6
    );
    relay.delete_message(3);
    me.sync(&relay);
    assert_eq!(
        row_at(&me, 3).status,
        2,
        "the tombstoned target is a deleted row"
    );
    assert_eq!(
        count(
            &me,
            "SELECT count(*) FROM app_messages WHERE seq = 4 AND body = '' AND envelope IS NULL"
        ),
        1,
        "the author's edit of a target never held is blanked by the author's delete"
    );
    assert_eq!(
        count(
            &me,
            "SELECT count(*) FROM app_messages WHERE seq = 5 AND body = 'reader words' AND envelope IS NOT NULL"
        ),
        1,
        "another user's edit is not blanked by the author's delete"
    );
}

/// FACTS-SECURITY-02: the view bounds the chips a member can make; the most counted stay.
#[test]
fn the_view_shows_at_most_twenty_reactions_most_counted_first() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut reader = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        3
    );
    for i in 0..25u8 {
        post(
            &mut peer,
            &mut relay,
            0xb0 + i,
            ReactionAdd,
            Some(TARGET),
            &format!("r{i:02}"),
        );
    }
    post(
        &mut reader,
        &mut relay,
        0xa1,
        ReactionAdd,
        Some(TARGET),
        "r24",
    );
    me.sync(&relay);
    let mut want: Vec<(String, u64, u64)> =
        (0..19).map(|i| chip(&format!("r{i:02}"), 1, 0)).collect();
    want.push(chip("r24", 2, 0));
    assert_eq!(chips(&me, 3), want);
}

/// FACTS-SECURITY-02: a long catch-up of one member's reactions on one target folds to the
/// right state (the cost bound itself is `fold::tests::every_fold_statement_is_an_index_search`).
#[test]
fn a_thousand_reaction_rows_on_one_target_fold() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        2
    );
    for i in 0..1000u32 {
        let kind = if i % 2 == 0 {
            ReactionAdd
        } else {
            ReactionRemove
        };
        post(&mut peer, &mut relay, 0xb0, kind, Some(TARGET), "👍");
    }
    post(&mut peer, &mut relay, 0xb1, ReactionAdd, Some(TARGET), "👍");
    me.sync(&relay);
    assert_eq!(chips(&me, 2), vec![chip("👍", 1, 0)]);
    assert_eq!(count(&me, "SELECT count(*) FROM app_reactions"), 1);
}

/// S1: an attacker can park rows on an unheld id or a deleted message. Each later insert
/// writes only its own row and the cursor, independent of those parked rows.
#[test]
fn parked_and_deleted_targets_touch_at_most_three_rows_per_insert() {
    use rusqlite::params;
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut mallory = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "doomed"),
        3
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, Delete, Some(TARGET), ""),
        4
    );
    me.sync(&relay);
    {
        let c = me.probe.lock().expect("lock");
        for (which, target) in [TARGET, UNHELD].iter().enumerate() {
            for i in 0..200i64 {
                for ty in [1i64, 2] {
                    // A fold of the held TARGET is sequenced after it (a lower seq would make
                    // TARGET a repeat, no target); UNHELD's parked rows precede every live one.
                    let k = i * 2 + ty - 1;
                    let seq = if which == 0 { 1_000_000 + k } else { -1 - k };
                    c.execute("INSERT INTO app_messages (group_id,seq,epoch,recv_ts,status,sender_user,sender_device,type,reply_to,body,franking_tag) VALUES (?1,?2,0,0,0,?3,?4,?5,?6,?7,?8)",
                        params![GROUP.as_slice(), seq, READER_USER.as_slice(), mallory.device.as_slice(), ty, target.as_slice(), if ty == 1 { "ignored" } else { "" }, [0u8; 32].as_slice()]).expect("park a fold row");
                }
            }
        }
    }
    let before = me.probe.lock().expect("lock").total_changes();
    let edit_seq = post(
        &mut mallory,
        &mut relay,
        0xb3,
        Edit,
        Some(TARGET),
        "late edit",
    );
    me.sync(&relay);
    let c = me.probe.lock().expect("lock");
    let (body, envelope): (String, Option<Vec<u8>>) = c
        .query_row(
            "SELECT body, envelope FROM app_messages WHERE group_id = ?1 AND seq = ?2",
            params![GROUP.as_slice(), edit_seq as i64],
            |r| Ok((r.get(0)?, r.get(1)?)),
        )
        .expect("stored late edit");
    assert_eq!(body, "", "late edit of a deleted target is blanked");
    assert!(envelope.is_none(), "late edit envelope is blanked");
    let after = c.total_changes();
    drop(c);
    assert!(
        after - before <= 4,
        "deleted target edit changed {} rows",
        after - before
    );
    let before = after;
    post(
        &mut mallory,
        &mut relay,
        0xb1,
        ReactionAdd,
        Some(TARGET),
        "👍",
    );
    me.sync(&relay);
    let after = me.probe.lock().expect("lock").total_changes();
    assert!(
        after - before <= 3,
        "deleted target changed {} rows",
        after - before
    );
    let before = after;
    post(&mut mallory, &mut relay, 0xb2, Delete, Some(UNHELD), "");
    me.sync(&relay);
    let after = me.probe.lock().expect("lock").total_changes();
    assert!(
        after - before <= 3,
        "unheld target changed {} rows",
        after - before
    );
}

// ---------------------------------------------------------------------------------------------
// web-2b task 3: sending every type (L-CORE-36), the outbox row (L-CORE-37), attachment reads and
// purges (L-CORE-38), and the purge record of L-CORE-33 step 2.

fn refusal(r: Result<Vec<u8>, ClientError>) -> (&'static str, String) {
    let e = r.expect_err("expected a refusal");
    (e.code, e.detail)
}

/// Alice on two devices (`a`, then `a2` enrolled by recovery key) and Bob in GROUP: `a` creates,
/// Bob and then `a2` join by external commit, `a` and Bob apply both joins.
fn alice_twice_and_bob() -> (Relay, Core, Core, Core) {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let (mut a, rk) = ready_core_with_key(0xa1, "alice");
    let mut a2 = enrolled_core(&a, &rk, NOW);
    let mut b = ready_core(0xb2, "bob");
    a.create_and_register(&mut relay, &instance);
    b.join_external(&mut relay);
    a2.join_external(&mut relay);
    a.sync(&relay);
    b.sync(&relay);
    ingest_core_list(&mut a, &relay, &b);
    ingest_core_list(&mut a2, &relay, &b);
    ingest_core_list(&mut a2, &relay, &a);
    let first_body = a.core.device_list_body().expect("first list");
    let second_body = a2.core.device_list_body().expect("enrolled list");
    ingest_enrolled_pair_bodies(
        &mut a,
        &relay,
        a2.user,
        a2.device,
        &first_body,
        &second_body,
    );
    ingest_enrolled_pair(&mut b, &relay, &a, &a2);
    (relay, a, a2, b)
}

#[test]
fn every_type_sent_by_one_core_is_folded_by_the_other() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    let (m, s) = a.send_request(&mut relay, &GROUP, &request(0, None, "hello", &[]), NOW);
    b.sync(&relay);

    b.send_request(
        &mut relay,
        &GROUP,
        &request(3, Some(&m), "👍", &[]),
        NOW + 1,
    );
    a.sync(&relay);
    assert_eq!(a.row_at(&GROUP, s).reactions, vec![("👍".to_owned(), 1, 0)]);

    let (_, edit_seq) = a.send_request(
        &mut relay,
        &GROUP,
        &request(1, Some(&m), "hello, edited", &[]),
        NOW + 2,
    );
    b.sync(&relay);
    let row = b.row_at(&GROUP, s);
    assert_eq!(
        (row.body.as_str(), row.edited_seq),
        ("hello, edited", edit_seq)
    );
    assert_eq!(row.reactions, vec![("👍".to_owned(), 1, 1)]);

    let (_, pin_seq) = b.send_request(&mut relay, &GROUP, &request(5, Some(&m), "", &[]), NOW + 3);
    a.sync(&relay);
    assert_eq!(
        a.row_at(&GROUP, s).pinned,
        1,
        "a pin from any member is accepted (Q20)"
    );
    let pins = decode_pins(&a.core.pins(&GROUP).expect("pins"));
    assert_eq!(pins.len(), 1);
    assert_eq!(
        (
            pins[0].target_seq,
            pins[0].msg_id,
            pins[0].pinned_seq,
            pins[0].by_user,
            pins[0].author,
            pins[0].target_ts
        ),
        (
            s,
            m,
            pin_seq,
            b.user,
            Some(a.user),
            a.row_at(&GROUP, s).recv_ts
        )
    );

    a.send_request(&mut relay, &GROUP, &request(6, Some(&m), "", &[]), NOW + 4);
    b.sync(&relay);
    assert_eq!(b.row_at(&GROUP, s).pinned, 0);
    assert!(decode_pins(&b.core.pins(&GROUP).expect("pins")).is_empty());

    b.send_request(
        &mut relay,
        &GROUP,
        &request(4, Some(&m), "👍", &[]),
        NOW + 5,
    );
    a.sync(&relay);
    assert!(a.row_at(&GROUP, s).reactions.is_empty());

    a.send_request(&mut relay, &GROUP, &request(2, Some(&m), "", &[]), NOW + 6);
    b.sync(&relay);
    let row = b.row_at(&GROUP, s);
    assert_eq!((row.status, row.body.as_str(), row.edited_seq), (2, "", 0));

    for core in [&a, &b] {
        let seqs: Vec<u64> = core.timeline(&GROUP).iter().map(|r| r.seq).collect();
        assert_eq!(seqs, vec![s], "fold rows are stored but never displayed");
    }
}

/// Attacker (lesson e): none. `send_prepare` is reached only by this device's own caller with its
/// own request; nothing another member sends passes through it, so these refusals stop the honest
/// sender from building an envelope the receivers' fold would ignore, and write nothing.
#[test]
fn send_prepare_refuses_each_rule_and_writes_nothing() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    let (mine, _) = a.send_request(&mut relay, &GROUP, &request(0, None, "mine", &[]), NOW);
    let (theirs, _) = b.send_request(&mut relay, &GROUP, &request(0, None, "theirs", &[]), NOW);
    a.sync(&relay);
    let unknown = [0xee; 16];
    let mut long_name = attachment(0x31);
    long_name.name = "n".repeat(256);
    let eleven: Vec<Attachment> = (1..=11).map(attachment).collect();
    let cases: Vec<(Vec<u8>, &str, &str)> = vec![
        (vec![0x80], "E_CORE_INPUT", "request is malformed"),
        (
            vec![0x83, 0x00, 0xf6, 0x60],
            "E_CORE_INPUT",
            "request is malformed",
        ),
        (
            request(7, Some(&mine), "x", &[]),
            "E_CORE_INPUT",
            "type is out of range",
        ),
        (
            request(1, Some(&mine), "x", &[attachment(0x31)]),
            "E_CORE_INPUT",
            "only a message carries attachments",
        ),
        (
            request(0, None, " \n\t ", &[]),
            "E_CORE_INPUT",
            "body is empty",
        ),
        (
            request(0, Some(&unknown), "a reply", &[]),
            "E_CORE_NOT_FOUND",
            "target is not held",
        ),
        (
            request(3, None, "👍", &[]),
            "E_CORE_INPUT",
            "reply_to is required",
        ),
        (
            request(3, Some(&unknown), "👍", &[]),
            "E_CORE_NOT_FOUND",
            "target is not held",
        ),
        (
            request(1, Some(&theirs), "not mine", &[]),
            "E_CORE_INPUT",
            "only the author may edit or delete",
        ),
        (
            request(2, Some(&theirs), "", &[]),
            "E_CORE_INPUT",
            "only the author may edit or delete",
        ),
        (
            request(1, Some(&mine), "  ", &[]),
            "E_CORE_INPUT",
            "body is empty",
        ),
        (
            request(3, Some(&theirs), "", &[]),
            "E_CORE_INPUT",
            "emoji must be 1..=32 bytes",
        ),
        (
            request(4, Some(&theirs), &"a".repeat(33), &[]),
            "E_CORE_INPUT",
            "emoji must be 1..=32 bytes",
        ),
        (
            request(2, Some(&mine), "x", &[]),
            "E_CORE_INPUT",
            "body must be empty",
        ),
        (
            request(5, Some(&theirs), "x", &[]),
            "E_CORE_INPUT",
            "body must be empty",
        ),
        (
            request(6, Some(&theirs), "x", &[]),
            "E_CORE_INPUT",
            "body must be empty",
        ),
        (
            request(1, Some(&mine), &"a".repeat(4001), &[]),
            "E_ENVELOPE_LIMIT",
            "",
        ),
        (request(0, None, "eleven", &eleven), "E_ENVELOPE_LIMIT", ""),
        (request(0, None, "", &[long_name]), "E_ENVELOPE_LIMIT", ""),
    ];
    for (req, code, detail) in &cases {
        assert_eq!(
            refusal(a.core.send_prepare(&GROUP, req, NOW + 1)),
            (*code, (*detail).to_owned()),
            "case {detail:?} / {code}"
        );
    }
    assert!(
        a.outbox(&GROUP).is_empty(),
        "a refused request wrote an outbox row"
    );

    a.send_request(
        &mut relay,
        &GROUP,
        &request(2, Some(&mine), "", &[]),
        NOW + 2,
    );
    for (ty, body) in [(1u64, "again"), (3, "👍"), (5, "")] {
        assert_eq!(
            refusal(
                a.core
                    .send_prepare(&GROUP, &request(ty, Some(&mine), body, &[]), NOW + 3)
            ),
            ("E_CORE_STATE", "the target is deleted".to_owned()),
            "type {ty} on a deleted target"
        );
    }
    assert!(a.outbox(&GROUP).is_empty());
}

#[test]
fn only_the_author_may_edit_or_delete() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    let (theirs, s) = b.send_request(&mut relay, &GROUP, &request(0, None, "theirs", &[]), NOW);
    a.sync(&relay);
    for (ty, body) in [(1u64, "rewritten"), (2, "")] {
        assert_eq!(
            refusal(
                a.core
                    .send_prepare(&GROUP, &request(ty, Some(&theirs), body, &[]), NOW + 1)
            ),
            (
                "E_CORE_INPUT",
                "only the author may edit or delete".to_owned()
            )
        );
    }
    assert!(a.outbox(&GROUP).is_empty());
    b.send_request(
        &mut relay,
        &GROUP,
        &request(1, Some(&theirs), "rewritten", &[]),
        NOW + 2,
    );
    a.sync(&relay);
    assert_eq!(a.row_at(&GROUP, s).body, "rewritten");
}

#[test]
fn an_attachment_only_message_is_sent_and_its_descriptors_read_back() {
    let (_instance, mut relay, mut a, mut b) = alice_and_bob_trusted();
    let files = [attachment(0x31), attachment(0x32)];
    let msg = a.prepare_request(&GROUP, &request(0, None, "", &files), NOW);
    let out = a.outbox(&GROUP);
    assert_eq!(out.len(), 1);
    assert_eq!(
        (
            out[0].msg_id,
            out[0].ty,
            out[0].reply_to,
            out[0].body.as_str()
        ),
        (msg, 0, None, "")
    );
    assert_eq!(
        out[0].attachments,
        vec![
            (
                [0x31; 32],
                1049,
                "image/png".to_owned(),
                "file-31.png".to_owned()
            ),
            (
                [0x32; 32],
                1050,
                "image/png".to_owned(),
                "file-32.png".to_owned()
            ),
        ]
    );
    let (_, body) = a.encrypt(&msg);
    let answer = relay.post_message(a.device, &body).expect("upload");
    let (_, s) = decode_confirm(&a.core.send_confirm(&msg, &answer).expect("send_confirm"));
    b.sync(&relay);

    let row = b.row_at(&GROUP, s);
    assert_eq!(row.body, "");
    assert_eq!(
        row.attachments,
        vec![
            AttachmentSummary {
                index: 0,
                size: 1049,
                mime: "image/png".into(),
                w: Some(640),
                h: Some(480),
                has_thumb: 1,
                name: "file-31.png".into()
            },
            AttachmentSummary {
                index: 1,
                size: 1050,
                mime: "image/png".into(),
                w: Some(640),
                h: Some(480),
                has_thumb: 1,
                name: "file-32.png".into()
            },
        ]
    );
    let none = ("E_CORE_NOT_FOUND", "no such attachment".to_owned());
    for (core, who) in [(&a, "sender"), (&b, "receiver")] {
        for (i, want) in files.iter().enumerate() {
            let got = decode_attachment(
                &core
                    .core
                    .attachment_get(&GROUP, s, i as u32)
                    .expect("attachment_get"),
            );
            assert!(
                got == *want,
                "{who}: attachment {i} reads back with its key, nonce and thumb"
            );
        }
        assert_eq!(
            refusal(core.core.attachment_get(&GROUP, s, 2)),
            none,
            "{who}: index past the list"
        );
        assert_eq!(
            refusal(core.core.attachment_get(&GROUP, s + 100, 0)),
            none,
            "{who}: no row"
        );
        assert_eq!(
            refusal(core.core.attachment_get(&OTHER_GROUP, s, 0)),
            none,
            "{who}: unknown group"
        );
    }
    assert_eq!(
        refusal(b.core.attachment_get(&GROUP, u64::MAX, 0)),
        ("E_CORE_INPUT", "seq out of range".to_owned())
    );

    let (_, edit) = a.send_request(
        &mut relay,
        &GROUP,
        &request(1, Some(&msg), "now with words", &[]),
        NOW + 1,
    );
    b.sync(&relay);
    assert_eq!(
        refusal(b.core.attachment_get(&GROUP, edit, 0)),
        none,
        "a type-1 row carries no attachment"
    );

    a.send_request(
        &mut relay,
        &GROUP,
        &request(2, Some(&msg), "", &[]),
        NOW + 2,
    );
    b.sync(&relay);
    assert_eq!(
        refusal(b.core.attachment_get(&GROUP, s, 0)),
        none,
        "a deleted message keeps no attachment"
    );
}

#[test]
fn the_outbox_row_carries_the_type_the_target_and_attachment_summaries() {
    let (_instance, mut relay, mut a, _b) = alice_and_bob_trusted();
    let (m, _) = a.send_request(&mut relay, &GROUP, &request(0, None, "target", &[]), NOW);
    let react = a.prepare_request(&GROUP, &request(3, Some(&m), "👍", &[]), NOW + 1);
    let reply = a.prepare_request(
        &GROUP,
        &request(0, Some(&m), "with a file", &[attachment(0x51)]),
        NOW + 2,
    );
    assert_eq!(
        a.outbox(&GROUP),
        vec![
            OutboxRow {
                msg_id: react,
                state: 0,
                error: String::new(),
                created: NOW + 1,
                body: "👍".into(),
                ty: 3,
                reply_to: Some(m),
                attachments: vec![]
            },
            OutboxRow {
                msg_id: reply,
                state: 0,
                error: String::new(),
                created: NOW + 2,
                body: "with a file".into(),
                ty: 0,
                reply_to: Some(m),
                attachments: vec![([0x51; 32], 1081, "image/png".into(), "file-51.png".into())]
            },
        ]
    );
}

#[test]
fn a_delete_records_one_purge_on_the_sending_device() {
    let (mut relay, mut a, _a2, mut b) = alice_twice_and_bob();
    let (m, s) = a.send_request(
        &mut relay,
        &GROUP,
        &request(0, None, "two files", &[attachment(0x41), attachment(0x42)]),
        NOW,
    );
    b.sync(&relay);
    a.send_request(&mut relay, &GROUP, &request(2, Some(&m), "", &[]), NOW + 1);
    let want = vec![PurgeRow {
        group_id: GROUP,
        seq: s,
        channel_id: CHANNEL,
        blob_ids: vec![[0x41; 32], [0x42; 32]],
    }];
    assert_eq!(a.purges(), want);
    assert_eq!(a.row_at(&GROUP, s).status, 2);

    a.sync(&relay);
    assert_eq!(
        a.purges(),
        want,
        "the echo of the delete records nothing twice"
    );
    relay.delete_message(s);
    a.core.message_deleted(&GROUP, s).expect("message_deleted");
    assert_eq!(
        a.purges(),
        want,
        "the delivery service's tombstone records nothing"
    );

    b.sync(&relay);
    assert!(b.purges().is_empty(), "a receiving device holds no purge");
    assert_eq!(b.row_at(&GROUP, s).status, 2);

    a.core.purge_done(&GROUP, s).expect("purge_done");
    assert!(a.purges().is_empty());
    a.core
        .purge_done(&GROUP, s)
        .expect("a second purge_done is no error");
    a.core
        .purge_done(&OTHER_GROUP, 9)
        .expect("an unknown purge is no error");
    a.core
        .message_deleted(&GROUP, s)
        .expect("message_deleted again");
    assert!(a.purges().is_empty(), "a done purge never comes back");
}

/// TESTS-10: the purge condition's `T.status = 0` clause. A second delete of a message the first
/// delete already removed records nothing, so a done purge never comes back.
#[test]
fn a_second_delete_of_the_same_message_records_no_second_purge() {
    let (mut relay, mut a, _a2, _b) = alice_twice_and_bob();
    let (m, s) = a.send_request(
        &mut relay,
        &GROUP,
        &request(0, None, "once", &[attachment(0x41)]),
        NOW,
    );
    let first = a.prepare_request(&GROUP, &request(2, Some(&m), "", &[]), NOW + 1);
    let second = a.prepare_request(&GROUP, &request(2, Some(&m), "", &[]), NOW + 2);
    a.encrypt_and_confirm(&mut relay, &GROUP, &first);
    assert_eq!(a.purges().len(), 1);
    a.core.purge_done(&GROUP, s).expect("purge_done");
    a.encrypt_and_confirm(&mut relay, &GROUP, &second);
    assert!(
        a.purges().is_empty(),
        "the second delete meets a deleted target and records nothing"
    );
}

/// FACTS-SECURITY-06 and lesson e: an own target whose stored envelope this build's decoder
/// refuses (an eight-element attachment, a shape web-2a's core accepted) is still deleted, and its
/// purge is recorded from the structural walk instead of failing the unit.
#[test]
fn a_delete_of_an_undecodable_own_target_still_records_its_purge() {
    let (mut relay, mut a, _a2, _b) = alice_twice_and_bob();
    let (m, s) = a.send_request(&mut relay, &GROUP, &request(0, None, "legacy", &[]), NOW);
    let legacy = legacy_eight_element_envelope(&m, "legacy");
    a.probe
        .lock()
        .expect("lock")
        .execute(
            "UPDATE app_messages SET envelope = ?1 WHERE seq = ?2",
            rusqlite::params![legacy, s as i64],
        )
        .expect("seed the legacy envelope");
    a.send_request(&mut relay, &GROUP, &request(2, Some(&m), "", &[]), NOW + 1);
    assert_eq!(a.row_at(&GROUP, s).status, 2);
    assert_eq!(
        a.purges(),
        vec![PurgeRow {
            group_id: GROUP,
            seq: s,
            channel_id: CHANNEL,
            blob_ids: vec![[0x41; 32]]
        }]
    );
}

#[test]
fn a_delete_received_from_the_own_users_other_device_records_no_purge() {
    let (mut relay, mut a, mut a2, mut b) = alice_twice_and_bob();
    let (m, s) = a.send_request(
        &mut relay,
        &GROUP,
        &request(0, None, "from the laptop", &[attachment(0x43)]),
        NOW,
    );
    a2.sync(&relay);
    b.sync(&relay);
    a.send_request(&mut relay, &GROUP, &request(2, Some(&m), "", &[]), NOW + 1);
    a2.sync(&relay);
    assert_eq!(
        a2.row_at(&GROUP, s).status,
        2,
        "the other device honours its own user's delete"
    );
    assert!(
        a2.purges().is_empty(),
        "only the device that sent the delete holds the duty"
    );
    assert_eq!(a.purges().len(), 1);

    let (m2, s2) = a.send_request(
        &mut relay,
        &GROUP,
        &request(0, None, "second", &[]),
        NOW + 2,
    );
    a2.sync(&relay);
    a2.send_request(&mut relay, &GROUP, &request(2, Some(&m2), "", &[]), NOW + 3);
    a.sync(&relay);
    assert_eq!(a.row_at(&GROUP, s2).status, 2);
    assert_eq!(a.purges().len(), 1, "the laptop keeps only its own purge");
    assert_eq!(
        a2.purges(),
        vec![PurgeRow {
            group_id: GROUP,
            seq: s2,
            channel_id: CHANNEL,
            blob_ids: vec![]
        }]
    );
    b.sync(&relay);
    assert!(b.purges().is_empty());
}

#[test]
fn a_delete_whose_echo_arrives_first_records_the_purge_on_adoption() {
    let (_instance, mut relay, mut a, _b) = alice_and_bob_trusted();
    let (m, s) = a.send_request(
        &mut relay,
        &GROUP,
        &request(0, None, "short-lived", &[]),
        NOW,
    );
    let (del, _) = a.post_request(&mut relay, &GROUP, &request(2, Some(&m), "", &[]), NOW + 1);
    let applied = a.sync(&relay);
    assert_eq!(applied.flags & OWN_ADOPTED, OWN_ADOPTED);
    assert!(a.outbox(&GROUP).iter().all(|r| r.msg_id != del));
    assert_eq!(
        a.purges(),
        vec![PurgeRow {
            group_id: GROUP,
            seq: s,
            channel_id: CHANNEL,
            blob_ids: vec![]
        }]
    );
    assert_eq!(a.row_at(&GROUP, s).status, 2);
}

#[test]
fn the_new_calls_need_an_identity() {
    let mut fresh = ClientCore::open(memory()).expect("open");
    assert_eq!(
        code(fresh.send_prepare(&GROUP, &request(0, None, "x", &[]), NOW)),
        "E_CORE_NO_IDENTITY"
    );
    assert_eq!(
        code(fresh.attachment_get(&GROUP, 1, 0)),
        "E_CORE_NO_IDENTITY"
    );
    assert_eq!(code(fresh.purges()), "E_CORE_NO_IDENTITY");
    assert_eq!(code(fresh.purge_done(&GROUP, 1)), "E_CORE_NO_IDENTITY");
}

// ---------------------------------------------------------------------------------------------
// web3.5 R1: the role view the host serves gates pins and `<@everyone>`/`<@here>` (protocol/04
// § Semantics). The view is advisory input: it decides only what this device counts.

/// (msg id, pinned seq, pinned by) of each pin, newest first.
fn pin_keys(me: &Core) -> Vec<([u8; 16], u64, [u8; 16])> {
    pins(me)
        .into_iter()
        .map(|p| (p.msg_id, p.pinned_seq, p.by_user))
        .collect()
}

fn mentions_in(me: &Core, group: &[u8; 16]) -> u64 {
    activity(&me.core)
        .into_iter()
        .find(|a| &a.group_id == group)
        .expect("activity row")
        .mentions
}

#[test]
fn pins_count_only_from_members_allowed_to_pin_now() {
    const A: [u8; 16] = [0x72; 16];
    const B: [u8; 16] = [0x73; 16];
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut reader = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    assert_eq!(post(&mut peer, &mut relay, 0x72, Message, None, "a"), 3);
    me.sync(&relay);
    me.core
        .authority_set(&GROUP, &PEER_USER, &[], &[])
        .expect("the peer may pin");
    assert_eq!(post(&mut peer, &mut relay, 0xa1, Pin, Some(A), ""), 4);
    assert_eq!(post(&mut reader, &mut relay, 0xa2, Unpin, Some(A), ""), 5);
    assert_eq!(post(&mut peer, &mut relay, 0x73, Message, None, "b"), 6);
    assert_eq!(post(&mut reader, &mut relay, 0xa3, Pin, Some(B), ""), 7);
    me.sync(&relay);
    assert_eq!(
        pin_keys(&me),
        vec![(A, 4, PEER_USER)],
        "an unauthorized unpin leaves the pin; an unauthorized pin is ignored"
    );
    assert_eq!(row_at(&me, 6).pinned, 0);

    me.core
        .authority_set(&GROUP, &[], &[], &[])
        .expect("nobody may pin");
    assert_eq!(pin_keys(&me), vec![], "removing the pinner unpins");
    assert_eq!(row_at(&me, 3).pinned, 0);

    me.core
        .authority_set(&GROUP, &PEER_USER, &[], &[])
        .expect("the peer again");
    assert_eq!(pin_keys(&me), vec![(A, 4, PEER_USER)], "restoring re-pins");

    me.core
        .authority_set(&GROUP, &[PEER_USER, READER_USER].concat(), &[], &[])
        .expect("both may pin");
    assert_eq!(
        pin_keys(&me),
        vec![(B, 7, READER_USER)],
        "the reader's unpin and pin count once the reader may pin"
    );
    assert_eq!(
        count(
            &me,
            "SELECT count(*) FROM app_authority_known WHERE set_at = 0"
        ),
        1,
        "one known row per group, at the core's clock (none: 0)"
    );
}

#[test]
fn a_group_without_a_role_view_keeps_the_former_pin_rule() {
    const A: [u8; 16] = [0x72; 16];
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut reader = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    me.core
        .authority_set(&OTHER_GROUP, &PEER_USER, &PEER_USER, &[])
        .expect("another group's view");
    assert_eq!(post(&mut peer, &mut relay, 0x72, Message, None, "a"), 3);
    assert_eq!(post(&mut reader, &mut relay, 0xa1, Pin, Some(A), ""), 4);
    assert_eq!(
        post(&mut reader, &mut relay, 0x74, Message, None, "<@everyone>"),
        5
    );
    me.sync(&relay);
    assert_eq!(
        pin_keys(&me),
        vec![(A, 4, READER_USER)],
        "any member pins where no view is known"
    );
    assert_eq!(
        row_at(&me, 5).mention,
        1,
        "and any member's @everyone counts"
    );
}

#[test]
fn everyone_and_here_count_only_from_members_allowed_them() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let own_token = format!("<@{}>", "a1".repeat(16));
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x71,
            Message,
            None,
            "<@everyone> standup"
        ),
        2
    );
    me.sync(&relay);
    assert_eq!(row_at(&me, 2).mention, 1, "no view yet: the former rule");
    assert_eq!(mentions_in(&me, &GROUP), 1);

    me.core
        .authority_set(&GROUP, &[], &[], &[])
        .expect("the peer may not");
    assert_eq!(row_at(&me, 2).mention, 0, "a held row is recomputed");
    assert_eq!(mentions_in(&me, &GROUP), 0, "the badge follows");
    assert_eq!(
        post(&mut peer, &mut relay, 0x72, Message, None, "<@here> ping"),
        3
    );
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x73,
            Message,
            None,
            &format!("<@everyone> and {own_token}")
        ),
        4
    );
    me.sync(&relay);
    assert_eq!(
        (row_at(&me, 3).mention, row_at(&me, 4).mention),
        (0, 1),
        "a direct mention still counts"
    );
    assert_eq!(mentions_in(&me, &GROUP), 1);

    me.core
        .authority_set(&GROUP, &[], &PEER_USER, &[])
        .expect("the peer may");
    assert_eq!(
        (
            row_at(&me, 2).mention,
            row_at(&me, 3).mention,
            row_at(&me, 4).mention
        ),
        (1, 1, 1)
    );
    assert_eq!(mentions_in(&me, &GROUP), 3);

    me.core
        .authority_set(&GROUP, &[], &[], &[])
        .expect("the peer may not again");
    assert_eq!(
        (
            row_at(&me, 2).mention,
            row_at(&me, 3).mention,
            row_at(&me, 4).mention
        ),
        (0, 0, 1)
    );
    assert_eq!(mentions_in(&me, &GROUP), 1);

    me.core
        .authority_set(&GROUP, &[], &PEER_USER, &[])
        .expect("the peer may");
    assert_eq!(
        post(&mut peer, &mut relay, 0x74, Message, None, "<@everyone>"),
        5
    );
    me.sync(&relay);
    assert_eq!(row_at(&me, 5).mention, 1, "counted on arrival");
}

/// W2a final review finding 1: once a group's view is known, an own-role token counts only when
/// the view marks the role mentionable or the sender may mention @everyone there; a role I do
/// not hold never counts. A new view recomputes the held rows, as for @everyone.
#[test]
fn a_role_token_counts_only_if_mentionable_or_from_a_broadcaster() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let token = |role: [u8; 16]| {
        format!(
            "ops <@{}>",
            role.iter().map(|b| format!("{b:02x}")).collect::<String>()
        )
    };
    me.core
        .own_roles_set(&COMMUNITY, &[ROLE, OTHER_ROLE].concat())
        .expect("own roles");
    me.core
        .authority_set(&GROUP, &[], &[], &[])
        .expect("nobody may broadcast, no role is mentionable");
    for (n, role) in [ROLE, OTHER_ROLE, UNHELD].into_iter().enumerate() {
        assert_eq!(
            post(
                &mut peer,
                &mut relay,
                0x71 + n as u8,
                Message,
                None,
                &token(role)
            ),
            2 + n as u64
        );
    }
    me.sync(&relay);
    let flags = |me: &Core| {
        (
            row_at(me, 2).mention,
            row_at(me, 3).mention,
            row_at(me, 4).mention,
        )
    };
    assert_eq!(
        flags(&me),
        (0, 0, 0),
        "an unmentionable role from a plain member"
    );
    assert_eq!(mentions_in(&me, &GROUP), 0);

    me.core
        .authority_set(&GROUP, &[], &[], &[ROLE, UNHELD].concat())
        .expect("two roles become mentionable");
    assert_eq!(
        flags(&me),
        (1, 0, 0),
        "a mentionable role counts from anyone; one I do not hold never does"
    );
    assert_eq!(mentions_in(&me, &GROUP), 1, "the badge follows");

    me.core
        .authority_set(&GROUP, &[], &PEER_USER, &ROLE)
        .expect("the peer gains mention_everyone");
    assert_eq!(
        flags(&me),
        (1, 1, 0),
        "a broadcaster's token for any role I hold counts"
    );

    me.core
        .authority_set(&GROUP, &[], &[], &ROLE)
        .expect("the peer loses it");
    assert_eq!(flags(&me), (1, 0, 0));

    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x75,
            Message,
            None,
            &token(OTHER_ROLE)
        ),
        5
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0x76, Message, None, &token(ROLE)),
        6
    );
    me.sync(&relay);
    assert_eq!(
        (row_at(&me, 5).mention, row_at(&me, 6).mention),
        (0, 1),
        "decided the same way on arrival"
    );
}

#[test]
fn a_group_without_a_view_counts_every_own_role_token() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    me.core.own_roles_set(&COMMUNITY, &ROLE).expect("own roles");
    me.core
        .authority_set(&OTHER_GROUP, &[], &[], &[])
        .expect("another group's view");
    let body = format!("ops <@{}>", "3c".repeat(16));
    assert_eq!(post(&mut peer, &mut relay, 0x71, Message, None, &body), 2);
    me.sync(&relay);
    assert_eq!(row_at(&me, 2).mention, 1, "the former rule");
}

#[test]
fn authority_set_takes_at_most_4096_ids_of_16_bytes_each() {
    let (_i, _relay, mut me, _peer) = fold_start();
    let full: Vec<u8> = (0..4096u32)
        .flat_map(|i| {
            let mut id = [0u8; 16];
            id[..4].copy_from_slice(&i.to_be_bytes());
            id
        })
        .collect();
    me.core
        .authority_set(&GROUP, &full, &full, &full)
        .expect("4096 ids each");
    assert_eq!(count(&me, "SELECT count(*) FROM app_authority"), 8192);
    let mentionable = "SELECT count(*) FROM app_authority_mentionable";
    assert_eq!(count(&me, mentionable), 4096);
    let too_many = [full.as_slice(), &[0xff; 16]].concat();
    let refusal = Err(ClientError {
        code: "E_CORE_INPUT",
        detail: "pinners, broadcasters and mentionable must be 0..=4096 ids of 16 bytes".into(),
    });
    for (pinners, broadcasters, roles) in [
        (too_many.as_slice(), &[][..], &[][..]),
        (&[][..], too_many.as_slice(), &[][..]),
        (&[][..], &[][..], too_many.as_slice()),
        (&[0u8; 15][..], &[][..], &[][..]),
        (&[][..], &[0u8; 15][..], &[][..]),
        (&[][..], &[][..], &[0u8; 15][..]),
    ] {
        assert_eq!(
            me.core.authority_set(&GROUP, pinners, broadcasters, roles),
            refusal
        );
    }
    assert_eq!(
        count(&me, "SELECT count(*) FROM app_authority"),
        8192,
        "a refusal writes nothing"
    );
    assert_eq!(count(&me, mentionable), 4096);
    assert_eq!(
        count(
            &me,
            "SELECT count(*) FROM app_authority WHERE bit NOT IN (3, 18)"
        ),
        0
    );
}

fn changes(me: &Core) -> u64 {
    me.probe.lock().expect("lock").total_changes()
}

fn exec(me: &Core, sql: &str) {
    me.probe
        .lock()
        .expect("lock")
        .execute(sql, [])
        .expect("probe write");
}

/// D1 as ruled (2026-10-15): the fold counts by the senders' rights now, so an unpin by a member
/// who lost `pin_messages` no longer counts and the earlier pin returns.
#[test]
fn an_unpin_by_a_member_who_lost_pin_messages_no_longer_counts() {
    const A: [u8; 16] = [0x72; 16];
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut reader = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    assert_eq!(post(&mut peer, &mut relay, 0x72, Message, None, "a"), 3);
    assert_eq!(post(&mut peer, &mut relay, 0xa1, Pin, Some(A), ""), 4);
    assert_eq!(post(&mut reader, &mut relay, 0xa2, Unpin, Some(A), ""), 5);
    me.sync(&relay);
    me.core
        .authority_set(&GROUP, &[PEER_USER, READER_USER].concat(), &[], &[])
        .expect("both may pin");
    assert_eq!(pin_keys(&me), vec![], "the reader's unpin counts");
    me.core
        .authority_set(&GROUP, &PEER_USER, &[], &[])
        .expect("only the peer");
    assert_eq!(pin_keys(&me), vec![(A, 4, PEER_USER)], "pinned again");
}

/// The two bits are separate: a broadcaster who may not pin, and a pinner who may not broadcast.
#[test]
fn the_pin_and_broadcast_bits_are_held_apart() {
    const A: [u8; 16] = [0x72; 16];
    const B: [u8; 16] = [0x73; 16];
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut reader = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    me.core
        .authority_set(&GROUP, &READER_USER, &PEER_USER, &[])
        .expect("crossed");
    assert_eq!(post(&mut peer, &mut relay, 0x72, Message, None, "a"), 3);
    assert_eq!(post(&mut peer, &mut relay, 0x73, Message, None, "b"), 4);
    assert_eq!(post(&mut peer, &mut relay, 0xa1, Pin, Some(A), ""), 5);
    assert_eq!(post(&mut reader, &mut relay, 0xa2, Pin, Some(B), ""), 6);
    assert_eq!(
        post(&mut peer, &mut relay, 0x74, Message, None, "<@everyone>"),
        7
    );
    assert_eq!(
        post(&mut reader, &mut relay, 0x75, Message, None, "<@here>"),
        8
    );
    me.sync(&relay);
    assert_eq!(pin_keys(&me), vec![(B, 6, READER_USER)]);
    assert_eq!((row_at(&me, 7).mention, row_at(&me, 8).mention), (1, 0));
}

/// A view belongs to its group: another group's view grants nothing here.
#[test]
fn each_group_keeps_its_own_view() {
    const A: [u8; 16] = [0x72; 16];
    let (_i, mut relay, mut me, mut peer) = fold_start();
    me.core
        .authority_set(&OTHER_GROUP, &PEER_USER, &PEER_USER, &[])
        .expect("the peer may, there");
    me.core
        .authority_set(&GROUP, &[], &[], &[])
        .expect("nobody may, here");
    assert_eq!(post(&mut peer, &mut relay, 0x72, Message, None, "a"), 2);
    assert_eq!(post(&mut peer, &mut relay, 0xa1, Pin, Some(A), ""), 3);
    assert_eq!(
        post(&mut peer, &mut relay, 0x74, Message, None, "<@everyone>"),
        4
    );
    me.sync(&relay);
    assert_eq!(pin_keys(&me), vec![]);
    assert_eq!(row_at(&me, 4).mention, 0);
    assert_eq!(count(&me, "SELECT count(*) FROM app_authority_known"), 2);
}

/// The rescan reads only other users' readable rows: the own row and a deleted row keep their flag.
#[test]
fn the_broadcast_rescan_ignores_own_and_deleted_rows() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    assert_eq!(
        post(
            &mut peer,
            &mut relay,
            0x71,
            Message,
            None,
            "<@everyone> gone"
        ),
        2
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, Delete, Some([0x71; 16]), ""),
        3
    );
    me.sync(&relay);
    let (_, own_seq) = me.send(&mut relay, &GROUP, "<@everyone> mine", NOW);
    me.sync(&relay);
    let flag = |me: &Core, seq: u64| -> i64 {
        me.probe
            .lock()
            .expect("lock")
            .query_row(
                "SELECT mention FROM app_messages WHERE group_id = ?1 AND seq = ?2",
                rusqlite::params![GROUP.as_slice(), seq as i64],
                |r| r.get(0),
            )
            .expect("row")
    };
    let deleted_before = flag(&me, 2);
    me.core
        .authority_set(&GROUP, &[], &[], &[])
        .expect("the peer may not");
    assert_eq!(
        flag(&me, 2),
        deleted_before,
        "a deleted row is not rescanned"
    );
    me.core
        .authority_set(&GROUP, &[], &[0xa1; 16], &[])
        .expect("I may");
    assert_eq!(flag(&me, own_seq), 0, "the own row never counts");
}

/// A push equal to the stored view (in any order, with repeats) writes nothing.
#[test]
fn an_unchanged_view_writes_nothing() {
    const A: [u8; 16] = [0x72; 16];
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut reader = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    assert_eq!(post(&mut peer, &mut relay, 0x72, Message, None, "a"), 3);
    assert_eq!(post(&mut reader, &mut relay, 0xa1, Pin, Some(A), ""), 4);
    assert_eq!(
        post(&mut peer, &mut relay, 0x74, Message, None, "<@everyone>"),
        5
    );
    me.sync(&relay);
    me.core
        .authority_set(
            &GROUP,
            &[PEER_USER, READER_USER].concat(),
            &PEER_USER,
            &[ROLE, OTHER_ROLE].concat(),
        )
        .expect("a view");
    let before = changes(&me);
    me.core
        .authority_set(
            &GROUP,
            &[READER_USER, PEER_USER, READER_USER].concat(),
            &PEER_USER,
            &[OTHER_ROLE, ROLE, OTHER_ROLE].concat(),
        )
        .expect("the same view");
    assert_eq!(changes(&me), before, "nothing written");
}

/// A changed view refolds only the targets with a 5/6 from a user whose pin bit changed, and
/// rescans only the rows of senders whose broadcast bit changed. Rows outside both are left as
/// they are (here, deliberately stale, to show they are not touched).
#[test]
fn a_changed_view_touches_only_the_affected_targets_and_rows() {
    const A: [u8; 16] = [0x72; 16];
    const B: [u8; 16] = [0x73; 16];
    let (_i, mut relay, mut me, mut peer) = fold_start();
    let mut reader = join(&mut relay, &mut me, &mut [&mut peer], 0xd4, 0xd5);
    assert_eq!(post(&mut peer, &mut relay, 0x72, Message, None, "a"), 3);
    assert_eq!(post(&mut peer, &mut relay, 0x73, Message, None, "b"), 4);
    assert_eq!(post(&mut peer, &mut relay, 0xa1, Pin, Some(A), ""), 5);
    assert_eq!(post(&mut reader, &mut relay, 0xa2, Pin, Some(B), ""), 6);
    assert_eq!(
        post(&mut peer, &mut relay, 0x74, Message, None, "<@everyone> p"),
        7
    );
    assert_eq!(
        post(
            &mut reader,
            &mut relay,
            0x75,
            Message,
            None,
            "<@everyone> r"
        ),
        8
    );
    me.sync(&relay);
    let both = [PEER_USER, READER_USER].concat();
    me.core
        .authority_set(&GROUP, &both, &both, &[])
        .expect("both");
    assert_eq!(pin_keys(&me).len(), 2);
    // Make the peer's derived state stale; a change that concerns only the reader leaves it so.
    exec(
        &me,
        "DELETE FROM app_pins WHERE target = x'72727272727272727272727272727272'",
    );
    exec(&me, "UPDATE app_messages SET mention = 0 WHERE seq = 7");
    me.core
        .authority_set(&GROUP, &PEER_USER, &PEER_USER, &[])
        .expect("the reader loses both");
    assert_eq!(pin_keys(&me), vec![], "B unpinned; A not refolded");
    assert_eq!(
        (row_at(&me, 7).mention, row_at(&me, 8).mention),
        (0, 0),
        "the reader's row rescanned; the peer's not"
    );
}

// ---------------------------------------------------------------------------------------------
// web3.7 RB1 (R5, R46): the host relays the upload class it stored with every row. A class that
// disagrees with the decrypted type (a post sent as class 1, a fold sent as class 0) is neither
// shown nor folded; a legacy row without a class is accepted.

/// The class the relay serves for the row at `seq` (`None` serves the legacy eight elements).
fn serve_class(relay: &mut Relay, seq: u64, class: Option<u64>) {
    relay
        .messages
        .iter_mut()
        .find(|m| m.seq == seq)
        .expect("a stored row")
        .class = class;
}

/// (status, reason, type, msg_id is NULL, reply_to is NULL, mention) of the stored row at `seq`.
fn stored(me: &Core, seq: u64) -> (i64, String, Option<i64>, bool, bool, i64) {
    me.probe
        .lock()
        .expect("lock")
        .query_row(
            &format!(
                "SELECT status, reason, type, msg_id IS NULL, reply_to IS NULL, mention \
                 FROM app_messages WHERE seq = {seq}"
            ),
            [],
            |r| {
                Ok((
                    r.get(0)?,
                    r.get(1)?,
                    r.get(2)?,
                    r.get(3)?,
                    r.get(4)?,
                    r.get(5)?,
                ))
            },
        )
        .expect("a stored row")
}

/// (unread, mentions, last_seq) of GROUP's activity.
fn unread(me: &Core) -> (u64, u64, u64) {
    let a = activity(&me.core)
        .into_iter()
        .find(|a| a.group_id == GROUP)
        .expect("the group's activity");
    (a.unread, a.mentions, a.last_seq)
}

/// A post at seq 2 and a reaction to it at seq 3, served with the given classes; me syncs.
fn post_and_react(post_class: Option<u64>, react_class: Option<u64>) -> Core {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        2
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, ReactionAdd, Some(TARGET), "👍"),
        3
    );
    serve_class(&mut relay, 2, post_class);
    serve_class(&mut relay, 3, react_class);
    me.sync(&relay);
    me
}

#[test]
fn class_mismatch_is_neither_shown_nor_folded() {
    let (_i, mut relay, mut me, mut peer) = fold_start();
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "the target"),
        2
    );
    // A reply that mentions me, sent as a fold: without the rule it would show, count as an
    // unread mention and keep its reply_to.
    let mention = format!("hey <@{}> sent as a fold", "a1".repeat(16));
    assert_eq!(
        post(&mut peer, &mut relay, 0x72, Message, Some(TARGET), &mention),
        3
    );
    assert_eq!(
        post(&mut peer, &mut relay, 0xa1, ReactionAdd, Some(TARGET), "👍"),
        4
    );
    assert_eq!(post(&mut peer, &mut relay, 0xa2, Pin, Some(TARGET), ""), 5);
    assert_eq!(
        post(&mut peer, &mut relay, 0xa3, Edit, Some(TARGET), "edited"),
        6
    );
    serve_class(&mut relay, 2, Some(0));
    serve_class(&mut relay, 3, Some(1));
    for fold in 4..=6 {
        serve_class(&mut relay, fold, Some(0));
    }
    me.sync(&relay);
    let dropped = (1, "E_CLASS_MISMATCH".to_owned(), Some(255), true, true, 0);
    assert_eq!(
        stored(&me, 3),
        dropped,
        "a mentioning reply sent as class 1"
    );
    assert_eq!(stored(&me, 4), dropped, "a reaction sent as class 0");
    assert_eq!(stored(&me, 5), dropped, "a pin sent as class 0");
    assert_eq!(stored(&me, 6), dropped, "an edit sent as class 0");
    assert_eq!(seqs(&me), vec![2], "no lying row is shown");
    let target = row_at(&me, 2);
    assert_eq!(
        (target.body.as_str(), target.edited_seq, target.pinned),
        ("the target", 0, 0),
        "neither the edit nor the pin is folded"
    );
    assert!(chips(&me, 2).is_empty(), "the reaction is not folded");
    assert!(pins(&me).is_empty(), "the pin is not listed");
    assert_eq!(unread(&me), (1, 0, 2), "activity counts the target only");
}

#[test]
fn a_legacy_row_without_a_class_is_kept() {
    let me = post_and_react(None, None);
    assert_eq!(seqs(&me), vec![2]);
    assert_eq!(chips(&me, 2), vec![chip("👍", 1, 0)]);
    assert_eq!((stored(&me, 2).0, stored(&me, 2).2), (0, Some(0)));
    assert_eq!((stored(&me, 3).0, stored(&me, 3).2), (0, Some(3)));
    assert_eq!(unread(&me), (1, 0, 2));
}

/// prepare → encrypt → POST → confirm; answers the upload body's (element count, class).
fn upload_class(me: &mut Core, relay: &mut Relay, request: &[u8], now: u64) -> (usize, u64) {
    let msg_id = me.prepare_request(&GROUP, request, now);
    let (_, body) = me.encrypt(&msg_id);
    let shape = dilla_core::cbor::decode_strict(&body, |d| {
        let n = d.array_len()?;
        d.uint()?;
        d.bytes()?;
        Ok((n, d.uint()?))
    })
    .expect("[epoch, private_message, class]");
    let answer = relay.post_message(me.device, &body).expect("upload");
    me.core
        .send_confirm(&msg_id, &answer)
        .expect("send_confirm");
    shape
}

#[test]
fn an_upload_carries_its_class() {
    let (_i, mut relay, mut me, _peer) = fold_start();
    let post = request(0, None, "mine", &[]);
    assert_eq!(
        upload_class(&mut me, &mut relay, &post, NOW + 1),
        (3, 0),
        "a post"
    );
    let target = me
        .timeline(&GROUP)
        .first()
        .and_then(|r| r.msg_id)
        .expect("the post");
    for (ty, body, now) in [(1, "edited", NOW + 2), (3, "👍", NOW + 3), (5, "", NOW + 4)] {
        let fold = request(ty, Some(&target), body, &[]);
        assert_eq!(
            upload_class(&mut me, &mut relay, &fold, now),
            (3, 1),
            "type {ty}"
        );
    }
}

#[test]
fn a_matching_class_is_kept() {
    let me = post_and_react(Some(0), Some(1));
    assert_eq!(seqs(&me), vec![2]);
    assert_eq!(chips(&me, 2), vec![chip("👍", 1, 0)]);
    assert_eq!((stored(&me, 2).0, stored(&me, 2).2), (0, Some(0)));
    assert_eq!((stored(&me, 3).0, stored(&me, 3).2), (0, Some(3)));
    assert_eq!(unread(&me), (1, 0, 2));
}

/// The class is checked before the sender's trust: a lying row from a sender whose device list is
/// not loaded yet is never parked as pending history, so a later release cannot show it. The
/// honest row from the same sender, parked the same way, is released (the control).
#[test]
fn a_lying_row_from_an_unlisted_sender_is_never_released() {
    let instance = Instance::generate();
    let mut relay = Relay::new(GROUP);
    let mut me = ready_core(0xa1, "alice");
    me.create_and_register(&mut relay, &instance);
    let mut peer = RawPeer::new(0xe5, 0xe6);
    assert_eq!(peer.join_external(&mut relay), 1);
    me.sync(&relay);
    assert_eq!(
        post(&mut peer, &mut relay, 0x71, Message, None, "honest"),
        2
    );
    assert_eq!(post(&mut peer, &mut relay, 0x72, Message, None, "lying"), 3);
    serve_class(&mut relay, 2, Some(0));
    serve_class(&mut relay, 3, Some(1));
    me.sync(&relay);
    assert!(seqs(&me).iter().all(|s| *s != 3), "the lie is not shown");
    assert_eq!(
        count(&me, "SELECT count(*) FROM app_pending_history"),
        1,
        "only the honest row is parked"
    );
    ingest_list_body(
        &mut me,
        &relay,
        peer.user,
        peer.device,
        &peer.device_list_body(),
    );
    me.core
        .revalidate_pending(&GROUP, relay.leaf_of(peer.device), 256)
        .expect("revalidate_pending");
    assert_eq!(
        seqs(&me),
        vec![2],
        "the honest row is released, the lie is not"
    );
    assert_eq!(stored(&me, 2).0, 0);
    assert_eq!(
        stored(&me, 3),
        (1, "E_CLASS_MISMATCH".to_owned(), Some(255), true, true, 0)
    );
}
