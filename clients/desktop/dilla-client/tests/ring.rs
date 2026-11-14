//! Task 18's client tests for the ring area (standard procedure S2).
//!
//! The first half ports the ring, outgoing and AFK logic tests of `packages/client-core/src/calls/controller.test.ts`
//! ("rings (L-TS-56 step 8)", "a ring follows whether this device is busy", m8) and of the AFK roster patch
//! (`awayPatched`, routes.ts `getAFK`) against the pure engine, so the same inputs give the same outputs.
//!
//! The second half runs against a real dillad (`#[ignore]`d): the browser is a Rust [`Client`] signed up the way the app
//! signs up, and the caller is the testkit's native peer (`dilla-testkit web-driver`, the one the Playwright suites
//! drive). They port `e2e/voice/ring.spec.ts` (answer, decline, busy, blocked DM, timeout) and `e2e/voice/blocked.spec.ts`
//! (a) at the client's level: the banner, the dock and the stage are the desktop's (tests/ring.rs of dilla-desktop).
//! Needs `target/dilla-testhost`, the WASI core and `target/debug/dilla-testkit` (or `DILLA_TESTKIT`).

use std::collections::{HashMap, HashSet};
use std::time::Duration;

use dilla_client::features::ring::{
    AFK_RETRY, AfkBook, AfkRead, DEFERRED_MAX, DMS_REREAD, DeferredRings, Expiry, RingBook,
    RingDeps, away_patched, changed_user, decode_afk, decode_changed, dms_read_due, dms_read_wait,
    is_builtin_afk, outgoing_declined, outgoing_shrink, outgoing_start,
};
use dilla_client::gateway::frames::{AfkAwayFrame, CallRingFrame};
use dilla_client::state::ring::{AFK_AWAY_MAX, AfkSnapshot, Away, RING, RINGS_MAX};
use dilla_client::state::{ChannelId, CommunityId, UserId};
use dilla_core::cbor::Encoder;
use tokio::time::Instant;

const ME: UserId = UserId([0xa1; 16]);
const PEER: UserId = UserId([0xb2; 16]);
const THIRD: UserId = UserId([0xc3; 16]);
const DM: ChannelId = ChannelId([0xd4; 16]);
const CALL: [u8; 16] = [0xd4; 16];

/// The worker's view of one client, as controller.test.ts's harness builds it: one DM with bob (more with `extra`).
#[derive(Default)]
struct World {
    dms: HashMap<ChannelId, Vec<UserId>>,
    blocked: HashSet<ChannelId>,
    busy: bool,
    muted: HashSet<ChannelId>,
}

impl World {
    fn dm() -> Self {
        let mut w = Self::default();
        w.dms.insert(DM, vec![ME, PEER]);
        w
    }
}

impl RingDeps for World {
    fn own_user(&self) -> Option<UserId> {
        Some(ME)
    }
    fn dm_members(&self, channel: ChannelId) -> Option<Vec<UserId>> {
        self.dms.get(&channel).cloned()
    }
    fn blocked(&self, channel: ChannelId) -> bool {
        self.blocked.contains(&channel)
    }
    fn busy(&self) -> bool {
        self.busy
    }
    fn quiet(&self, channel: ChannelId) -> bool {
        self.muted.contains(&channel)
    }
    fn name_of(&self, user: UserId) -> String {
        if user == PEER {
            "bob".into()
        } else {
            user.hex()[..8].to_owned()
        }
    }
}

/// controller.test.ts `ring(state, { channel, user, expires })`.
fn ring(state: u8, channel: ChannelId, user: UserId, expires: u64) -> CallRingFrame {
    CallRingFrame {
        call_id: channel.0,
        channel_id: channel.0,
        user_id: user.0,
        state,
        expires_ts: expires,
    }
}

fn bob(state: u8) -> CallRingFrame {
    ring(state, DM, PEER, 1_700_000_030)
}

#[test]
fn a_ring_for_a_dm_shows_once_busy_while_in_a_call_quiet_when_muted_and_ends_on_state_0() {
    let mut w = World::dm();
    let mut book = RingBook::default();
    let now = Instant::now();
    let step = book.frame(&bob(1), now, &w);
    assert!(step.changed);
    let r = &book.list()[0];
    assert_eq!(
        (r.channel, r.caller, r.caller_name.as_str(), r.ring_ts),
        (DM, PEER, "bob", 1_700_000_030)
    );
    assert_eq!((r.call_id, r.busy, r.quiet), (CALL, false, false));
    assert!(book.frame(&bob(0), now, &w).changed);
    assert!(book.list().is_empty());
    w.muted.insert(DM);
    book.frame(&bob(1), now, &w);
    assert!(book.list()[0].quiet && !book.list()[0].busy);
    book.frame(&bob(0), now, &w);
    // In a call elsewhere, a ring is busy.
    w.busy = true;
    book.frame(&bob(1), now, &w);
    assert!(book.list()[0].busy);
}

/// Attacker: a contact whose identity changed calls; the ring must not tempt a join the core would refuse.
#[test]
fn no_ring_entry_at_all_for_a_dm_whose_text_or_call_group_is_send_blocked() {
    let mut w = World::dm();
    let mut book = RingBook::default();
    w.blocked.insert(DM);
    assert!(!book.frame(&bob(1), Instant::now(), &w).changed);
    assert!(book.list().is_empty());
    // The control: unblocked, the same ring shows.
    w.blocked.clear();
    book.frame(&bob(1), Instant::now(), &w);
    assert_eq!(book.list().len(), 1);
    // An earlier ring goes when a later frame finds the DM blocked.
    w.blocked.insert(DM);
    assert!(
        book.frame(&ring(1, DM, PEER, 1_700_000_060), Instant::now(), &w)
            .changed
    );
    assert!(book.list().is_empty());
}

/// Attacker: the instance rings in the name of a channel the person does not share with the caller, or as themself.
#[test]
fn drops_a_ring_whose_caller_is_not_another_participant_of_a_known_dm_and_one_for_an_unknown_channel()
 {
    let w = World::dm();
    let mut book = RingBook::default();
    let now = Instant::now();
    book.frame(&ring(1, DM, THIRD, 1), now, &w);
    book.frame(&ring(1, DM, ME, 1), now, &w);
    book.frame(&ring(1, ChannelId([0x77; 16]), PEER, 1), now, &w);
    assert!(book.list().is_empty());
    book.frame(&bob(1), now, &w);
    assert_eq!(book.list().len(), 1);
}

#[test]
fn keeps_one_ring_per_channel_and_at_most_eight_in_total() {
    let mut w = World::dm();
    let extra: Vec<(ChannelId, UserId)> = (0..9u8)
        .map(|i| (ChannelId([0x10 + i; 16]), UserId([0x40 + i; 16])))
        .collect();
    for (c, u) in &extra {
        w.dms.insert(*c, vec![ME, *u]);
    }
    let mut book = RingBook::default();
    let now = Instant::now();
    book.frame(&ring(1, DM, PEER, 100), now, &w);
    book.frame(&ring(1, DM, PEER, 130), now, &w);
    assert_eq!(book.list().len(), 1);
    assert_eq!(book.list()[0].ring_ts, 130);
    for (c, u) in &extra {
        book.frame(&ring(1, *c, *u, 1), now, &w);
    }
    assert_eq!(book.list().len(), RINGS_MAX);
    assert!(!book.list().iter().any(|r| r.channel == extra[8].0));
}

#[test]
fn a_decline_of_an_older_ring_keeps_the_newer_ring_of_the_same_dm() {
    let w = World::dm();
    let mut book = RingBook::default();
    book.frame(&ring(1, DM, PEER, 130), Instant::now(), &w);
    // A second call of the same DM (another call id) with a later expires_ts.
    let mut later = ring(1, DM, PEER, 160);
    later.call_id = [0xee; 16];
    book.frame(&later, Instant::now(), &w);
    assert!(
        !book.remove_call(CALL, 130),
        "the newer ring replaced the older one"
    );
    assert_eq!(book.list()[0].ring_ts, 160);
    assert!(book.remove_call([0xee; 16], 160));
    assert!(book.list().is_empty());
}

#[test]
fn a_ring_expires_locally_at_its_deadline_when_no_state_0_arrives() {
    let w = World::dm();
    let mut book = RingBook::default();
    let received = Instant::now();
    // A lying instance: five minutes. The ring ends 30 s after it arrived.
    let step = book.frame(&ring(1, DM, PEER, 1_700_000_300), received, &w);
    let arm = step.arm.expect("a timer is armed");
    assert_eq!(arm.at, received + Duration::from_secs(30));
    assert_eq!(book.list()[0].expires_at, received + RING);
    assert!(book.expire(arm));
    assert!(book.list().is_empty());
}

/// concern 5: a page clock 60 s ahead of the instance (expires_ts already past here) still rings 30 s from arrival.
#[test]
fn a_ring_lasts_30_s_from_its_arrival_whatever_this_clock_says_of_its_expires_ts() {
    let w = World::dm();
    let mut book = RingBook::default();
    let received = Instant::now();
    book.frame(&ring(1, DM, PEER, 1), received, &w);
    assert_eq!(book.list()[0].ring_ts, 1);
    assert_eq!(book.list()[0].expires_at, received + RING);
}

/// f10app m8: a repeated state 1 of the same ring is that ring again, not a new 30 s.
#[test]
fn a_repeated_ring_frame_with_the_same_expires_ts_keeps_its_first_30_s_a_new_expires_ts_starts_its_own()
 {
    let w = World::dm();
    let mut book = RingBook::default();
    let t0 = Instant::now();
    let first = book.frame(&bob(1), t0, &w).arm.unwrap();
    let again = book.frame(&bob(1), t0 + Duration::from_secs(20), &w);
    assert_eq!(
        again,
        Default::default(),
        "nothing changes and no timer is armed"
    );
    assert_eq!(book.list()[0].expires_at, t0 + RING);
    assert!(book.expire(first));
    // A later ring of the same DM (another expires_ts) has its own 30 s; the earlier timer cannot end it.
    let t1 = t0 + Duration::from_secs(60);
    let a = book
        .frame(&ring(1, DM, PEER, 1_700_000_090), t1, &w)
        .arm
        .unwrap();
    let b = book
        .frame(
            &ring(1, DM, PEER, 1_700_000_120),
            t1 + Duration::from_secs(20),
            &w,
        )
        .arm
        .unwrap();
    assert!(!book.expire(a), "the replaced ring's timer ends nothing");
    assert_eq!(book.list().len(), 1);
    assert!(book.expire(b));
}

#[test]
fn a_ring_that_arrived_during_a_call_is_no_longer_busy_once_the_call_is_left_and_busy_again_in_the_next()
 {
    let mut w = World::dm();
    w.busy = true;
    let mut book = RingBook::default();
    book.frame(&bob(1), Instant::now(), &w);
    assert!(book.list()[0].busy);
    assert!(book.refresh_busy(false));
    assert!(!book.list()[0].busy);
    assert!(!book.refresh_busy(false));
    assert!(book.refresh_busy(true));
    assert!(book.list()[0].busy);
}

#[test]
fn a_ring_ends_when_its_dm_becomes_send_blocked_mid_ring() {
    let mut w = World::dm();
    let other = ChannelId([0x99; 16]);
    w.dms.insert(other, vec![ME, THIRD]);
    let mut book = RingBook::default();
    book.frame(&bob(1), Instant::now(), &w);
    book.frame(&ring(1, other, THIRD, 1), Instant::now(), &w);
    // The control: a refresh with no block keeps both rings.
    assert!(!book.retain_unblocked(|_| false));
    assert_eq!(book.list().len(), 2);
    assert!(book.retain_unblocked(|c| c == DM));
    assert_eq!(
        book.list().iter().map(|r| r.channel).collect::<Vec<_>>(),
        vec![other]
    );
}

#[test]
fn joining_the_dm_call_clears_its_ring() {
    let w = World::dm();
    let mut book = RingBook::default();
    book.frame(&bob(1), Instant::now(), &w);
    assert!(book.remove(DM));
    assert!(book.list().is_empty());
}

#[test]
fn an_outgoing_dm_call_lists_who_is_ringing_and_appends_decliners_from_state_2() {
    let members = [ME, PEER, THIRD];
    let mut out = outgoing_start(DM, &members, Some(ME), &HashSet::new());
    assert_eq!(out.ringing, vec![PEER, THIRD]);
    assert!(outgoing_shrink(&mut out, &HashSet::from([THIRD])));
    assert_eq!(out.ringing, vec![PEER]);
    assert!(outgoing_declined(
        &mut out,
        &members,
        Some(ME),
        &ring(2, DM, PEER, 0)
    ));
    assert_eq!((out.ringing.len(), out.declined.clone()), (0, vec![PEER]));
    // A decline by someone outside the DM, by this user, or of another channel changes nothing.
    assert!(!outgoing_declined(
        &mut out,
        &members,
        Some(ME),
        &ring(2, DM, UserId([9; 16]), 0)
    ));
    assert!(!outgoing_declined(
        &mut out,
        &members,
        Some(ME),
        &ring(2, DM, ME, 0)
    ));
    assert!(!outgoing_declined(
        &mut out,
        &members,
        Some(ME),
        &ring(2, ChannelId([1; 16]), THIRD, 0)
    ));
}

#[test]
fn the_changed_contact_is_named_only_among_the_calls_own_participants() {
    let changed = [THIRD, PEER, ME];
    assert_eq!(
        changed_user(&changed, Some(ME), &[ME, PEER], None),
        Some(PEER)
    );
    assert_eq!(changed_user(&changed, Some(ME), &[ME], None), None);
    assert_eq!(
        changed_user(&changed, Some(ME), &[PEER, THIRD], None),
        Some(PEER.min(THIRD))
    );
    assert_eq!(changed_user(&[], Some(ME), &[PEER], None), None);
}

/// Review I1 (controller.ts:325): with no candidates known (a call tree member who never entered the room, and no leaf
/// named by the core), nobody is named: never a contact of another conversation, however many are changed.
#[test]
fn with_no_candidates_and_no_reported_leaf_nobody_is_named() {
    let changed = [THIRD, PEER];
    assert_eq!(changed_user(&changed, Some(ME), &[], None), None);
}

/// controller.ts `changedUser(.., reported)`: the leaf the core's refusal named comes first while the trust state still
/// shows it changed; a reported leaf no longer changed, or the own user, falls back to the candidates.
#[test]
fn the_leaf_the_core_named_comes_first_while_it_is_still_changed() {
    let changed = [THIRD, PEER];
    assert_eq!(
        changed_user(&changed, Some(ME), &[], Some(THIRD)),
        Some(THIRD)
    );
    assert_eq!(
        changed_user(&changed, Some(ME), &[PEER], Some(THIRD)),
        Some(THIRD)
    );
    assert_eq!(
        changed_user(&[PEER], Some(ME), &[PEER], Some(THIRD)),
        Some(PEER)
    );
    assert_eq!(changed_user(&[ME], Some(ME), &[], Some(ME)), None);
}

#[test]
fn the_trust_state_names_changed_and_umk_blocked_contacts() {
    let mut e = Encoder::new();
    e.array(3);
    // Unchanged.
    e.array(6)
        .bytes(&ME.0)
        .bytes(&[1; 32])
        .uint(1)
        .uint(0)
        .null()
        .null();
    // A pending key.
    e.array(6)
        .bytes(&PEER.0)
        .bytes(&[2; 32])
        .uint(1)
        .uint(0)
        .bytes(&[3; 32])
        .null();
    // A leaf blocked E_UMK_CHANGED.
    e.array(6)
        .bytes(&THIRD.0)
        .bytes(&[4; 32])
        .uint(1)
        .uint(1)
        .null()
        .text("E_UMK_CHANGED");
    assert_eq!(decode_changed(&e.into_vec()).unwrap(), vec![PEER, THIRD]);
}

// ---- AFK ----

fn away(user: UserId, since: u64) -> Away {
    Away { user, since }
}

#[test]
fn an_away_change_patches_the_roster_and_a_stale_or_empty_one_changes_nothing() {
    let roster = vec![away(PEER, 10)];
    assert_eq!(
        away_patched(&roster, &away(ME, 5)),
        Some(vec![away(ME, 5), away(PEER, 10)])
    );
    assert_eq!(
        away_patched(&roster, &away(PEER, 10)),
        None,
        "no newer than held"
    );
    assert_eq!(away_patched(&roster, &away(PEER, 9)), None);
    assert_eq!(
        away_patched(&roster, &away(PEER, 11)),
        Some(vec![away(PEER, 11)])
    );
    assert_eq!(away_patched(&roster, &away(PEER, 0)), Some(vec![]));
    assert_eq!(
        away_patched(&roster, &away(THIRD, 0)),
        None,
        "a clear of someone not away"
    );
}

fn afk_body(
    channel: Option<[u8; 16]>,
    timeout: u64,
    reason: u64,
    rows: &[(UserId, u64)],
) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(5);
    match channel {
        Some(c) => e.bytes(&c),
        None => e.null(),
    };
    e.uint(timeout).array(0).uint(reason).array(rows.len());
    for (u, s) in rows {
        e.array(2).bytes(&u.0).uint(*s);
    }
    e.into_vec()
}

#[test]
fn the_afk_read_skips_cleared_and_repeated_rows_and_reads_the_pending_reason() {
    let body = afk_body(
        Some([7; 16]),
        300,
        1,
        &[(PEER, 20), (ME, 0), (PEER, 30), (THIRD, 5)],
    );
    let read = decode_afk(&body).unwrap();
    assert_eq!(read.channel, Some(ChannelId([7; 16])));
    assert_eq!(read.timeout_seconds, 300);
    assert_eq!(read.reason, 1);
    assert_eq!(read.away, vec![away(PEER, 20), away(THIRD, 5)]);
    // routes.ts `pendingReason`: an unknown reason reads as 2; a web3.9 host's three-element answer has none.
    assert_eq!(decode_afk(&afk_body(None, 60, 7, &[])).unwrap().reason, 2);
    let mut e = Encoder::new();
    e.array(3).null().uint(60).array(0);
    let old = decode_afk(&e.into_vec()).unwrap();
    assert_eq!((old.reason, old.away.len()), (0, 0));
}

#[test]
fn frames_received_while_an_afk_read_is_in_flight_are_replayed_onto_it_and_each_landed_read_rises()
{
    let c = CommunityId([5; 16]);
    let mut book = AfkBook::default();
    let token = book.begin(c, Instant::now()).unwrap();
    // No snapshot held yet: the frame is kept for the read only.
    let f = AfkAwayFrame {
        community_id: c.0,
        user_id: ME.0,
        since: 50,
    };
    assert_eq!(book.frame(None, &f), None);
    let read = AfkRead {
        channel: Some(ChannelId([7; 16])),
        timeout_seconds: 60,
        reason: 1,
        away: vec![away(PEER, 10)],
    };
    let s = book.land(c, token, read.clone()).unwrap();
    assert_eq!(s.away, vec![away(ME, 50), away(PEER, 10)]);
    assert!(s.pending);
    assert_eq!(s.seq, 1);
    // The next read takes the next seq; a token that is not the read in flight lands nothing.
    let next = book.begin(c, Instant::now()).unwrap();
    assert_eq!(book.land(c, next + 1, read.clone()), None);
    assert_eq!(book.land(c, next, read).unwrap().seq, 2);
    // op 55 on a held snapshot patches it in place, keeping its seq.
    let held = AfkSnapshot {
        seq: 2,
        away: vec![away(PEER, 10)],
        ..AfkSnapshot::default()
    };
    let clear = AfkAwayFrame {
        community_id: c.0,
        user_id: PEER.0,
        since: 0,
    };
    let patched = book.frame(Some(&held), &clear).unwrap();
    assert_eq!((patched.seq, patched.away.len()), (2, 0));
}

#[test]
fn ten_renders_asking_for_the_afk_snapshot_start_one_read_and_a_failure_holds_reads_for_5_s() {
    let c = CommunityId([5; 16]);
    let mut book = AfkBook::default();
    let t0 = Instant::now();
    // The AFK page asks on every frame (screens::ring::afk_page): ten frames, one read.
    let started: Vec<u64> = (0..10).filter_map(|_| book.begin(c, t0)).collect();
    assert_eq!(started.len(), 1, "one read for ten asks: {started:?}");
    // Another community reads on its own.
    assert!(book.begin(CommunityId([6; 16]), t0).is_some());
    // The read fails: no new one for AFK_RETRY, then one.
    book.fail(c, started[0], t0);
    assert_eq!(
        book.begin(c, t0 + AFK_RETRY - Duration::from_millis(1)),
        None
    );
    let again = (0..10)
        .filter_map(|_| book.begin(c, t0 + AFK_RETRY))
        .count();
    assert_eq!(again, 1);
}

#[test]
fn op_55_never_grows_the_away_roster_or_the_read_buffer_past_the_reads_bound() {
    let c = CommunityId([5; 16]);
    let mut book = AfkBook::default();
    let held = AfkSnapshot {
        away: (0..AFK_AWAY_MAX as u32)
            .map(|i| {
                let mut u = [0u8; 16];
                u[..4].copy_from_slice(&i.to_be_bytes());
                away(UserId(u), 1)
            })
            .collect(),
        ..AfkSnapshot::default()
    };
    let one_more = AfkAwayFrame {
        community_id: c.0,
        user_id: [0xff; 16],
        since: 9,
    };
    assert_eq!(
        book.frame(Some(&held), &one_more),
        None,
        "a full roster takes no one more"
    );
    // A refresh of someone already listed still applies.
    let refresh = AfkAwayFrame {
        community_id: c.0,
        user_id: held.away[0].user.0,
        since: 9,
    };
    assert!(book.frame(Some(&held), &refresh).is_some());
    // A read in flight keeps at most AFK_AWAY_MAX frames: the flood past it is dropped, the read lands bounded.
    let token = book.begin(c, Instant::now()).unwrap();
    for i in 0..(AFK_AWAY_MAX as u32 + 100) {
        let mut u = [0u8; 16];
        u[..4].copy_from_slice(&i.to_be_bytes());
        book.frame(
            None,
            &AfkAwayFrame {
                community_id: c.0,
                user_id: u,
                since: 5,
            },
        );
    }
    let read = AfkRead {
        channel: None,
        timeout_seconds: 60,
        reason: 0,
        away: vec![],
    };
    assert_eq!(book.land(c, token, read).unwrap().away.len(), AFK_AWAY_MAX);
}

#[test]
fn the_dm_list_is_read_again_at_most_once_per_5_s() {
    let mut last = None;
    let t0 = Instant::now();
    let reads = (0..20).filter(|_| dms_read_due(&mut last, t0)).count();
    assert_eq!(reads, 1);
    assert!(!dms_read_due(
        &mut last,
        t0 + DMS_REREAD - Duration::from_millis(1)
    ));
    assert!(dms_read_due(&mut last, t0 + DMS_REREAD));
}

/// Task 28 (`blocked_dm`'s control): a read asked for inside the window waits for the rest of it, never dropped.
#[test]
fn a_dm_list_read_inside_the_window_waits_for_its_end() {
    let t0 = Instant::now();
    assert_eq!(dms_read_wait(None, t0), Duration::ZERO);
    assert_eq!(
        dms_read_wait(Some(t0), t0 + Duration::from_secs(2)),
        DMS_REREAD - Duration::from_secs(2)
    );
    assert_eq!(dms_read_wait(Some(t0), t0 + DMS_REREAD), Duration::ZERO);
}

/// t28-bot-review I3: a ring for an unknown DM waits for the re-read on its own; a later ring frame of its channel
/// (an end, or the ring again) makes it stale, other channels' frames do not, and the waiting rings are bounded.
#[test]
fn a_deferred_ring_is_checked_once_only_while_no_later_frame_of_its_channel_came() {
    let (a, b) = (ChannelId([1; 16]), ChannelId([2; 16]));
    let mut d = DeferredRings::default();
    let first = d.defer(a).expect("room");
    d.frame(b);
    assert!(
        d.take(a, first),
        "another channel's frame leaves it current"
    );
    assert!(!d.take(a, first), "checked once");

    let stale = d.defer(a).expect("room");
    d.frame(a);
    assert!(!d.take(a, stale), "ended meanwhile (state 0)");

    let older = d.defer(a).expect("room");
    let newer = d.defer(a).expect("the same channel always has room");
    assert!(!d.take(a, older), "rung again: only the newer is checked");
    assert!(d.take(a, newer));

    for i in 0..DEFERRED_MAX {
        assert!(d.defer(ChannelId([10 + i as u8; 16])).is_some());
    }
    assert_eq!(d.defer(b), None, "bounded");
}

#[test]
fn the_builtin_afk_channel_is_found_by_id_else_by_the_reservation() {
    let afk = ChannelId([7; 16]);
    assert!(is_builtin_afk(afk, 3, Some(afk)));
    assert!(!is_builtin_afk(ChannelId([8; 16]), 1 << 30, Some(afk)));
    assert!(is_builtin_afk(ChannelId([8; 16]), 1 << 30, None));
    assert!(!is_builtin_afk(ChannelId([8; 16]), 3, None));
}

#[allow(dead_code)]
fn _expiry_is_copy(e: Expiry) -> (Expiry, Expiry) {
    (e, e)
}

// ---- against a real dillad ----

// Task 3's call world: dilla-testhost with its in-process LiveKit (a call start needs the SFU).
#[path = "support/call_world.rs"]
mod world;

mod real {
    use std::io::{BufRead, BufReader, Read, Write};
    use std::path::PathBuf;
    use std::process::{Command, Stdio};
    use std::sync::{Arc, Mutex, mpsc};
    use std::time::{Duration, Instant};

    use dilla_client::Client;
    use dilla_client::state::{AppState, ChannelId, CommunityId, UserId};
    use serde_json::{Value, json};

    pub use super::world::{Host, Person, clear_session, repo_root, sign_up, start, start_host};

    /// Waits (at most `within`) until `pred` holds for the client's state, answering that state.
    pub async fn wait_for(
        client: &Client,
        within: Duration,
        what: &str,
        pred: impl Fn(&AppState) -> bool,
    ) -> Arc<AppState> {
        let mut rx = client.state();
        let deadline = Instant::now() + within;
        loop {
            {
                let s = rx.borrow_and_update().clone();
                if pred(&s) {
                    return s;
                }
            }
            let left = deadline.saturating_duration_since(Instant::now());
            assert!(!left.is_zero(), "timed out waiting for {what}");
            let _ = tokio::time::timeout(left, rx.changed()).await;
        }
    }

    /// Moves the host's fake clock `seconds` forward (`POST <control>/debug/clock`, blocking).
    pub fn advance_clock(host: &Host, seconds: u64) {
        let addr = host.control.trim_start_matches("http://").to_owned();
        let body = format!("{{\"seconds\":{seconds}}}");
        let mut s = std::net::TcpStream::connect(&addr).unwrap();
        write!(
            s,
            "POST /debug/clock HTTP/1.1\r\nHost: {addr}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
            body.len()
        )
        .unwrap();
        let mut answer = String::new();
        s.read_to_string(&mut answer).unwrap();
        assert!(
            answer.starts_with("HTTP/1.1 2"),
            "the clock did not move: {answer}"
        );
    }

    pub const WAIT: Duration = Duration::from_secs(60);

    fn testkit_binary() -> PathBuf {
        if let Some(bin) = std::env::var_os("DILLA_TESTKIT") {
            return PathBuf::from(bin);
        }
        let root = repo_root();
        let release = root.join("target/release/dilla-testkit");
        if release.exists() {
            return release;
        }
        let debug = root.join("target/debug/dilla-testkit");
        assert!(
            debug.exists(),
            "dilla-testkit missing: cargo build -p dilla-testkit --bin dilla-testkit"
        );
        debug
    }

    /// The testkit's native peer (`dilla-testkit web-driver`), one JSON request per line.
    pub struct Peer {
        tx: mpsc::Sender<(Value, tokio::sync::oneshot::Sender<Value>)>,
    }

    impl Peer {
        pub fn start(host: &Host, seed: u64) -> Peer {
            let mut child = Command::new(testkit_binary())
                .args([
                    "web-driver",
                    "--ds",
                    &host.public,
                    "--seed",
                    &seed.to_string(),
                ])
                .env("DILLA_TESTKIT_INVITE", host.invites.join(","))
                .env("DILLA_TESTKIT_CONTROL", &host.control)
                .stdin(Stdio::piped())
                .stdout(Stdio::piped())
                .stderr(Stdio::inherit())
                .spawn()
                .expect("the web driver starts");
            let mut stdin = child.stdin.take().unwrap();
            let mut stdout = BufReader::new(child.stdout.take().unwrap());
            let (tx, rx) = mpsc::channel::<(Value, tokio::sync::oneshot::Sender<Value>)>();
            std::thread::spawn(move || {
                for (req, answer) in rx {
                    if writeln!(stdin, "{req}")
                        .and_then(|()| stdin.flush())
                        .is_err()
                    {
                        break;
                    }
                    let mut line = String::new();
                    if stdout.read_line(&mut line).unwrap_or(0) == 0 {
                        break;
                    }
                    let _ = answer.send(serde_json::from_str(&line).unwrap_or(Value::Null));
                }
                let _ = child.kill();
                let _ = child.wait();
            });
            Peer { tx }
        }

        pub async fn call(&self, op: &str, mut args: Value) -> Value {
            args["op"] = json!(op);
            let (tx, rx) = tokio::sync::oneshot::channel();
            self.tx.send((args, tx)).expect("the peer thread runs");
            let v = rx.await.expect("the peer answers");
            assert_eq!(v["ok"], json!(true), "peer {op}: {v}");
            v
        }

        /// Syncs until the text group's roster holds `count` devices.
        pub async fn wait_for_devices(&self, count: usize, channel: &str) {
            let deadline = Instant::now() + WAIT;
            loop {
                self.call("sync", json!({ "channel_id": channel })).await;
                let v = self.call("members", json!({ "channel_id": channel })).await;
                if v["devices"].as_array().map_or(0, Vec::len) == count {
                    return;
                }
                assert!(
                    Instant::now() < deadline,
                    "the group never held {count} devices: {v}"
                );
                tokio::time::sleep(Duration::from_millis(300)).await;
            }
        }

        /// The devices of `channel`'s call group, as the peer's member state holds them.
        pub async fn call_devices(&self, channel: ChannelId) -> Vec<String> {
            let v = self
                .call(
                    "call_members",
                    json!({ "actor": "peer", "channel": channel.hex() }),
                )
                .await;
            v["devices"]
                .as_array()
                .unwrap()
                .iter()
                .map(|d| d.as_str().unwrap().to_owned())
                .collect()
        }
    }

    pub fn hex16(s: &str) -> [u8; 16] {
        let mut out = [0u8; 16];
        for (i, b) in out.iter_mut().enumerate() {
            *b = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).unwrap();
        }
        out
    }

    pub fn hex(b: &[u8]) -> String {
        b.iter().map(|x| format!("{x:02x}")).collect()
    }

    /// One person, their client, and the peer's setup they joined.
    pub struct Pair {
        pub host: Host,
        pub me: Person,
        pub client: Client,
        pub peer: Peer,
        pub setup: Value,
    }

    impl Pair {
        pub fn peer_user(&self) -> UserId {
            UserId(hex16(self.setup["user_id"].as_str().unwrap()))
        }
        pub fn community(&self) -> CommunityId {
            CommunityId(hex16(self.setup["community_id"].as_str().unwrap()))
        }
        pub fn voice(&self) -> ChannelId {
            ChannelId(hex16(self.setup["voice_channel_id"].as_str().unwrap()))
        }
        pub fn me(&self) -> UserId {
            UserId(self.me.user_id)
        }
    }

    /// The person joins the peer's community (with a voice channel) and opens its text channel, so they share its
    /// group and this device pins the peer's key; the peer sees both devices.
    pub async fn pair(tag: &str, seed: u64) -> Pair {
        let tag_owned = tag.to_owned();
        let host = tokio::task::spawn_blocking(move || start_host(&tag_owned))
            .await
            .unwrap();
        let invite = host.invites.last().unwrap().clone();
        let me = sign_up(&host, &invite, "bob", tag).await;
        clear_session(&me);
        let client = start(&me).await;
        let peer = Peer::start(&host, seed);
        let setup = peer
            .call(
                "setup",
                json!({ "community": "ring", "channel": "general", "voice": true }),
            )
            .await;
        peer.call("register", json!({})).await;
        join_and_open(&host, &me, &client, &setup).await;
        peer.wait_for_devices(2, setup["channel_id"].as_str().unwrap())
            .await;
        let pair = Pair {
            host,
            me,
            client,
            peer,
            setup,
        };
        // This device pins the peer's key (first seen) before anything rotates it: otherwise the rotated key is the
        // first it sees, which is trust on first use, not a change (the Playwright suite pins it while joining).
        trust_is(&pair.client, pair.peer_user(), false).await;
        pair
    }

    /// Refreshes this device's trust until the core holds `user` pinned, changed or not as `changed` says.
    pub async fn trust_is(client: &Client, user: UserId, changed: bool) {
        let ring = client.ring();
        let deadline = Instant::now() + WAIT;
        while ring.contact_trust(user).await.unwrap().unwrap() != Some(changed) {
            assert!(
                Instant::now() < deadline,
                "the trust state never held {user:?} changed={changed}"
            );
            let _ = ring.refresh_blocks().await;
            tokio::time::sleep(Duration::from_millis(300)).await;
        }
    }

    pub async fn join_and_open(host: &Host, person: &Person, client: &Client, setup: &Value) {
        let community = hex16(setup["community_id"].as_str().unwrap());
        let channel = ChannelId(hex16(setup["channel_id"].as_str().unwrap()));
        let code = setup["invite_code"].as_str().unwrap().to_owned();
        let (public, device, token) = (host.public.clone(), person.device_id, person.token.clone());
        tokio::task::spawn_blocking(move || {
            use dilla_core::ids::DeviceId;
            let mut ds =
                dilla_testkit::HttpDs::with_session(&public, DeviceId::from_bytes(device), token)
                    .unwrap();
            ds.join_community(&community, &code).unwrap();
        })
        .await
        .unwrap();
        client.refresh();
        wait_for(client, WAIT, "the peer's channel", |s| {
            s.channel(channel).is_some()
        })
        .await;
        let deadline = Instant::now() + WAIT;
        loop {
            match client.open_channel(channel).await.unwrap() {
                Ok(()) => break,
                Err(e) if Instant::now() < deadline => {
                    eprintln!("open retried: {e}");
                    tokio::time::sleep(Duration::from_millis(500)).await;
                }
                Err(e) => panic!("never opened the channel: {e}"),
            }
        }
    }

    /// The peer opens a DM with `user`: its channel.
    pub async fn open_dm(peer: &Peer, user: UserId) -> ChannelId {
        let v = peer.call("open_dm", json!({ "user_id": user.hex() })).await;
        ChannelId(hex16(v["channel_id"].as_str().unwrap()))
    }

    /// The peer joins `channel`'s call group and starts its call: the instance rings the DM's other members.
    pub async fn peer_calls(peer: &Peer, channel: ChannelId) -> [u8; 16] {
        peer.call(
            "call_join",
            json!({ "actor": "peer", "channel": channel.hex() }),
        )
        .await;
        let started = peer
            .call(
                "call_start",
                json!({ "actor": "peer", "channel": channel.hex() }),
            )
            .await;
        hex16(started["call"].as_str().unwrap())
    }

    pub fn ringing(s: &AppState, channel: ChannelId) -> bool {
        s.ring.ring_of(channel).is_some()
    }

    /// Every channel a ring was ever listed for, as the published states show them.
    pub fn record_rings(
        client: &Client,
    ) -> (Arc<Mutex<Vec<ChannelId>>>, tokio::task::JoinHandle<()>) {
        let seen = Arc::new(Mutex::new(Vec::new()));
        // Every snapshot, not the coalesced latest: a ring listed for a moment is seen too.
        let mut rx = client.snapshots();
        let out = seen.clone();
        let task = tokio::spawn(async move {
            while let Some(s) = rx.recv().await {
                let mut seen = out.lock().unwrap();
                for r in &s.ring.rings {
                    if !seen.contains(&r.channel) {
                        seen.push(r.channel);
                    }
                }
            }
        });
        (seen, task)
    }
}

use real::{WAIT, advance_clock, hex, open_dm, pair, peer_calls, record_rings, ringing, wait_for};
use serde_json::json;

/// Joins `channel`'s call through the ring area's gate (a DM when `community` is `None`) and waits for its outcome.
async fn join(
    client: &dilla_client::Client,
    channel: ChannelId,
    community: Option<CommunityId>,
) -> Result<
    Result<dilla_client::calls::Joined, dilla_client::calls::JoinRefused>,
    dilla_client::state::ring::CallBlock,
> {
    match client
        .ring()
        .join(channel, community, String::new())
        .await
        .unwrap()
        .expect("client")
    {
        Ok(gated) => Ok(gated.outcome().await.expect("client")),
        Err(block) => Err(block),
    }
}

/// e2e/voice/ring.spec.ts "answer": the peer calls; this device rings (incoming, not busy, the caller named), answers by
/// joining the DM's call, and the ring goes; the peer's call group lists this device.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn answer() {
    let p = pair("t18-answer", 0x1801).await;
    let dm = open_dm(&p.peer, p.me()).await;
    peer_calls(&p.peer, dm).await;
    let s = wait_for(&p.client, WAIT, "the ring", |s| ringing(s, dm)).await;
    let r = s.ring.ring_of(dm).unwrap();
    assert_eq!(r.caller, p.peer_user());
    assert!(!r.busy && !r.quiet);
    let joined = join(&p.client, dm, None)
        .await
        .expect("not blocked")
        .expect("joined");
    assert!(
        !joined.started.created,
        "the peer's call is joined, not a new one"
    );
    wait_for(&p.client, WAIT, "the ring gone", |s| {
        s.ring.rings.is_empty()
    })
    .await;
    let devices = p.peer.call_devices(dm).await;
    assert!(devices.contains(&hex(&p.me.device_id)), "{devices:?}");
}

/// e2e/voice/ring.spec.ts "decline": this device calls the peer, who declines: the outgoing ring says so (op 53 state
/// 2). Then the peer calls and this device declines: its ring goes and the instance answers the decline.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn decline() {
    let p = pair("t18-decline", 0x1802).await;
    let dm = open_dm(&p.peer, p.me()).await;
    let joined = join(&p.client, dm, None)
        .await
        .expect("not blocked")
        .expect("joined");
    assert!(joined.started.created);
    let s = wait_for(&p.client, WAIT, "the outgoing ring", |s| {
        s.ring.outgoing.is_some()
    })
    .await;
    assert_eq!(
        s.ring.outgoing.as_ref().unwrap().ringing,
        vec![p.peer_user()]
    );
    p.peer
        .call(
            "call_decline",
            json!({ "actor": "peer", "call": hex(&joined.started.call_id) }),
        )
        .await;
    wait_for(&p.client, WAIT, "the decline", |s| {
        s.ring
            .outgoing
            .as_ref()
            .is_some_and(|o| o.declined == vec![p.peer_user()] && o.ringing.is_empty())
    })
    .await;
    // This device leaves (alone, so the call ends), and the peer calls it.
    p.client
        .calls()
        .leave(joined.group_id, joined.started.call_id, true)
        .await
        .unwrap()
        .expect("left");
    p.client.ring().set_in_call(None);
    peer_calls(&p.peer, dm).await;
    let s = wait_for(&p.client, WAIT, "the ring", |s| ringing(s, dm)).await;
    let r = s.ring.ring_of(dm).unwrap().clone();
    let answered = p.client.ring().decline(r.call_id, r.ring_ts);
    wait_for(&p.client, WAIT, "the ring gone", |s| {
        s.ring.rings.is_empty()
    })
    .await;
    answered
        .await
        .unwrap()
        .expect("the instance answered the decline");
}

/// Wave 2 review I-G1 (useCall.tsx `ringExpired`): this device calls the peer, who never answers. The outgoing ring
/// still rings at first; 30 s after the call's start it is marked expired (the box then says "No answer."), and the
/// call's end takes the outgoing ring with it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn an_unanswered_outgoing_ring_expires_after_30_s() {
    let p = pair("w2f3-ring-expiry", 0x1812).await;
    let dm = open_dm(&p.peer, p.me()).await;
    let started = std::time::Instant::now();
    let joined = join(&p.client, dm, None)
        .await
        .expect("not blocked")
        .expect("joined");
    let s = wait_for(&p.client, WAIT, "the outgoing ring", |s| {
        s.ring.outgoing.is_some()
    })
    .await;
    let out = s.ring.outgoing.clone().unwrap();
    assert_eq!(out.ringing, vec![p.peer_user()]);
    assert!(!out.expired, "it rings at first");
    let s = wait_for(&p.client, Duration::from_secs(45), "the ring's 30 s", |s| {
        s.ring.outgoing.as_ref().is_some_and(|o| o.expired)
    })
    .await;
    assert!(
        started.elapsed() >= dilla_client::state::ring::RING,
        "not before 30 s ({:?})",
        started.elapsed()
    );
    assert_eq!(
        s.ring.outgoing.as_ref().unwrap().ringing,
        vec![p.peer_user()]
    );
    p.client
        .calls()
        .leave(joined.group_id, joined.started.call_id, true)
        .await
        .unwrap()
        .expect("left");
    p.client.ring().set_in_call(None);
    wait_for(&p.client, WAIT, "the outgoing ring gone", |s| {
        s.ring.outgoing.is_none()
    })
    .await;
}

/// e2e/voice/ring.spec.ts "busy": while this device is in a call, a ring shows busy and names where; once the call is
/// left it no longer is (WB client m6).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn busy() {
    let p = pair("t18-busy", 0x1803).await;
    let dm = open_dm(&p.peer, p.me()).await;
    let joined = join(&p.client, p.voice(), Some(p.community()))
        .await
        .expect("not blocked")
        .expect("joined");
    p.client.ring().set_in_call(Some("general-voice".into()));
    peer_calls(&p.peer, dm).await;
    let s = wait_for(&p.client, WAIT, "the ring", |s| ringing(s, dm)).await;
    assert!(s.ring.ring_of(dm).unwrap().busy);
    assert_eq!(s.ring.in_call.as_deref(), Some("general-voice"));
    p.client
        .calls()
        .leave(joined.group_id, joined.started.call_id, true)
        .await
        .unwrap()
        .expect("left");
    p.client.ring().set_in_call(None);
    wait_for(&p.client, WAIT, "not busy", |s| {
        s.ring.ring_of(dm).is_some_and(|r| !r.busy)
    })
    .await;
}

/// e2e/voice/ring.spec.ts "blocked DM": a ring of a DM whose contact's identity changed never shows; a trusted peer's
/// ring after it does (the positive control, on the very path the blocked ring would have taken).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn blocked_dm() {
    let p = pair("t18-blocked-dm", 0x1804).await;
    // The DM area runs as the shell starts it (dms/home.rs): the DM list, and this device in each DM's text group.
    p.client.dms().start();
    let (seen, recorder) = record_rings(&p.client);
    // The peer's identity is replaced, and its new device opens a DM with this person: its leaf carries the new key.
    p.peer.call("sync", json!({})).await;
    p.peer.call("rotate_umk", json!({})).await;
    // A message after the rotation brings its commit to this device (the e2e's changedSeen).
    p.peer
        .call("send", json!({ "body": "after rotation" }))
        .await;
    real::trust_is(&p.client, p.peer_user(), true).await;
    let dm = open_dm(&p.peer, p.me()).await;
    // This device holds the DM's text group (the e2e's syncDm until it has two members).
    let deadline = std::time::Instant::now() + WAIT;
    loop {
        let v = p
            .peer
            .call("sync_dm", json!({ "channel_id": dm.hex() }))
            .await;
        if v["members"].as_u64().unwrap_or(0) >= 2 {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "this device never joined the DM's group: {v}"
        );
        tokio::time::sleep(Duration::from_millis(300)).await;
    }
    let ring = p.client.ring();
    let deadline = std::time::Instant::now() + WAIT;
    while ring.dm_blocked(dm).await.unwrap().unwrap().as_deref() != Some("E_UMK_CHANGED") {
        assert!(
            std::time::Instant::now() < deadline,
            "the DM never became send-blocked"
        );
        let _ = ring.refresh_blocks().await;
        tokio::time::sleep(Duration::from_millis(300)).await;
    }
    peer_calls(&p.peer, dm).await;
    // The control: a second, trusted peer's DM call, after the blocked one.
    let other = real::Peer::start(&p.host, 0x1805);
    other
        .call(
            "setup",
            json!({ "community": "other", "channel": "general" }),
        )
        .await;
    let odm = open_dm(&other, p.me()).await;
    // As the e2e: the trusted DM holds both people before its call (this device joined its text group).
    let deadline = std::time::Instant::now() + WAIT;
    loop {
        let v = other
            .call("sync_dm", json!({ "channel_id": odm.hex() }))
            .await;
        if v["members"].as_u64().unwrap_or(0) >= 2 {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "this device never joined the trusted DM's group: {v}"
        );
        tokio::time::sleep(Duration::from_millis(300)).await;
    }
    // Open on this device's screen, as the e2e's openDm.
    p.client
        .dms()
        .open(odm)
        .await
        .unwrap()
        .expect("the trusted DM opens");
    peer_calls(&other, odm).await;
    wait_for(&p.client, WAIT, "the trusted ring", |s| ringing(s, odm)).await;
    // The recorder reads the snapshot taps on its own task: it may not have reached the snapshot `wait_for` saw yet.
    let deadline = std::time::Instant::now() + WAIT;
    while !seen.lock().unwrap().contains(&odm) {
        assert!(
            std::time::Instant::now() < deadline,
            "the recorder never saw the trusted ring"
        );
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    let listed = seen.lock().unwrap().clone();
    assert_eq!(listed, vec![odm], "the blocked DM never had a ring listed");
    // The blocked DM's call is refused before anything is sent, with the changed contact named.
    let block = join(&p.client, dm, None)
        .await
        .expect_err("the DM call is refused");
    assert_eq!(block.code, "E_UMK_CHANGED");
    assert_eq!(block.changed_user, Some(p.peer_user()));
    // Review M6: nothing of that refused call reached the instance: the peer's call group and its room never list
    // this device.
    let devices = p.peer.call_devices(dm).await;
    assert!(
        !devices.contains(&hex(&p.me.device_id)),
        "the DM's call group never saw this device: {devices:?}"
    );
    let room = p
        .peer
        .call("call_read", json!({ "actor": "peer", "channel": dm.hex() }))
        .await;
    assert!(
        !room["participants"]
            .as_array()
            .unwrap()
            .iter()
            .any(|x| x["device"] == hex(&p.me.device_id)),
        "the DM's room never saw this device: {room}"
    );
    recorder.abort();
}

/// e2e/voice/ring.spec.ts "timeout": the ring ends when the instance's 30 s pass (op 53 state 0).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn timeout() {
    let p = pair("t18-timeout", 0x1806).await;
    let dm = open_dm(&p.peer, p.me()).await;
    peer_calls(&p.peer, dm).await;
    wait_for(&p.client, WAIT, "the ring", |s| ringing(s, dm)).await;
    advance_clock(&p.host, 31);
    // Well inside this device's own 30 s: only the instance's state 0 can end it this soon.
    wait_for(&p.client, Duration::from_secs(20), "the ring's end", |s| {
        s.ring.rings.is_empty()
    })
    .await;
}

/// e2e/voice/blocked.spec.ts (a): a contact whose identity changed is in the call group, so the join is refused before
/// anything of it is posted: no leaf of this device in the call group or the room, and the block names the contact.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn blocked_contact_refuses_join_before_any_call_write() {
    let p = pair("t18-blocked-a", 0x1807).await;
    p.peer.call("sync", json!({})).await;
    p.peer.call("rotate_umk", json!({})).await;
    // A message after the rotation brings its commit to this device (the e2e's changedSeen).
    p.peer
        .call("send", json!({ "body": "after rotation" }))
        .await;
    real::trust_is(&p.client, p.peer_user(), true).await;
    p.peer
        .call(
            "call_join",
            json!({ "actor": "peer", "channel": p.voice().hex() }),
        )
        .await;
    let refused = join(&p.client, p.voice(), Some(p.community()))
        .await
        .expect("a voice channel has no DM gate")
        .expect_err("the join is refused");
    assert_eq!(refused.code, "E_UMK_CHANGED");
    assert_eq!(refused.phase, dilla_client::calls::join::Phase::Blocked);
    assert_eq!(refused.leave, None, "nothing of the join was accepted");
    let s = wait_for(&p.client, WAIT, "the block", |s| s.ring.blocked.is_some()).await;
    let block = s.ring.blocked.clone().unwrap();
    assert_eq!(
        (block.channel, block.code.as_str()),
        (p.voice(), "E_UMK_CHANGED")
    );
    assert_eq!(block.when, dilla_client::state::ring::BlockWhen::BeforeJoin);
    assert_eq!(
        block.changed_user,
        Some(p.peer_user()),
        "the notice names the changed contact"
    );
    let devices = p.peer.call_devices(p.voice()).await;
    assert!(
        !devices.contains(&hex(&p.me.device_id)),
        "the call group never saw this device: {devices:?}"
    );
    let room = p
        .peer
        .call(
            "call_read",
            json!({ "actor": "peer", "channel": p.voice().hex() }),
        )
        .await;
    assert!(
        !room["participants"]
            .as_array()
            .unwrap()
            .iter()
            .any(|x| x["device"] == hex(&p.me.device_id)),
        "the room never saw this device: {room}"
    );
}

/// Review I3: `Voice::join` goes through the ring area's gate. A contact whose identity changed is in the call group: the
/// voice join is refused before anything is posted (the same answer as the gate's own), and the block is published.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn a_voice_join_goes_through_the_ring_gate() {
    let p = pair("t18-voice-gate", 0x1809).await;
    p.peer.call("sync", json!({})).await;
    p.peer.call("rotate_umk", json!({})).await;
    p.peer
        .call("send", json!({ "body": "after rotation" }))
        .await;
    real::trust_is(&p.client, p.peer_user(), true).await;
    p.peer
        .call(
            "call_join",
            json!({ "actor": "peer", "channel": p.voice().hex() }),
        )
        .await;
    let refused = p
        .client
        .voice()
        .join(p.voice())
        .await
        .expect("client")
        .expect_err("the voice join is refused at the gate");
    assert!(
        matches!(&refused, dilla_client::ClientError::Sync(code) if code == "E_UMK_CHANGED"),
        "{refused:?}"
    );
    let s = wait_for(&p.client, WAIT, "the block", |s| s.ring.blocked.is_some()).await;
    let block = s.ring.blocked.clone().unwrap();
    assert_eq!(block.when, dilla_client::state::ring::BlockWhen::BeforeJoin);
    assert_eq!(
        s.voice.call.phase,
        dilla_client::calls::CallPhase::Idle,
        "no call"
    );
    let devices = p.peer.call_devices(p.voice()).await;
    assert!(
        !devices.contains(&hex(&p.me.device_id)),
        "the call group never saw this device: {devices:?}"
    );
}

/// Integration 3 (gpui2/fix-flakes, d3146979a): a refused join's call is idle before its block is published. In the
/// order the snapshots were published, the call goes back to idle no later than the first snapshot with the block, and
/// no snapshot shows the block beside a call still joining.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn a_refused_join_is_idle_before_its_block() {
    let p = pair("t18-refused-order", 0x180c).await;
    p.peer.call("sync", json!({})).await;
    p.peer.call("rotate_umk", json!({})).await;
    p.peer
        .call("send", json!({ "body": "after rotation" }))
        .await;
    real::trust_is(&p.client, p.peer_user(), true).await;
    p.peer
        .call(
            "call_join",
            json!({ "actor": "peer", "channel": p.voice().hex() }),
        )
        .await;
    let mut snapshots = p.client.snapshots();
    let refused = p
        .client
        .voice()
        .join(p.voice())
        .await
        .expect("client")
        .expect_err("the voice join is refused at the gate");
    assert!(
        matches!(&refused, dilla_client::ClientError::Sync(code) if code == "E_UMK_CHANGED"),
        "{refused:?}"
    );
    wait_for(&p.client, WAIT, "the block", |s| s.ring.blocked.is_some()).await;
    let mut seen = Vec::new();
    while let Ok(s) = snapshots.try_recv() {
        seen.push((
            s.voice.call.phase,
            s.voice.call.channel,
            s.ring.blocked.is_some(),
        ));
    }
    let first_block = seen
        .iter()
        .position(|(_, _, blocked)| *blocked)
        .expect("a snapshot with the block");
    let (phase, channel, _) = seen[first_block];
    assert_eq!(
        (phase, channel),
        (dilla_client::calls::CallPhase::Idle, None),
        "the call is idle in the first snapshot with the block: {seen:?}"
    );
    assert!(
        seen.iter()
            .all(|(phase, _, blocked)| !*blocked || *phase == dilla_client::calls::CallPhase::Idle),
        "no snapshot shows the block beside a call still joining: {seen:?}"
    );
}

/// Review I3: `Voice::join_gated` follows the join the ring area's gate let through (an answered ring): this device is
/// in the DM's call as its own, and the peer's call group lists it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn join_gated_follows_the_handed_join() {
    let p = pair("t18-join-gated", 0x180a).await;
    let dm = open_dm(&p.peer, p.me()).await;
    peer_calls(&p.peer, dm).await;
    wait_for(&p.client, WAIT, "the ring", |s| ringing(s, dm)).await;
    let gated = p
        .client
        .ring()
        .join(dm, None, String::new())
        .await
        .unwrap()
        .expect("client")
        .expect("a DM with a readable group is not blocked");
    p.client
        .voice()
        .join_gated(dm, None, gated)
        .await
        .expect("client")
        .expect("the handed join is followed");
    let s = wait_for(&p.client, WAIT, "in the DM's call", |s| {
        s.voice.call.phase == dilla_client::calls::CallPhase::In && s.voice.call.channel == Some(dm)
    })
    .await;
    assert!(s.voice.call.call_id.is_some());
    let devices = p.peer.call_devices(dm).await;
    assert!(devices.contains(&hex(&p.me.device_id)), "{devices:?}");
}

/// Review M6: answering a ring while this device is in another call (the window's Answer: `Voice::join_dm`) leaves the
/// old call before the ring area's gate posts anything of the DM's: this device ends in the DM's call only, and the
/// voice channel's call group no longer lists it.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad: target/dilla-testhost, the wasi core and dilla-testkit"]
async fn answering_a_ring_in_a_call_leaves_the_call_first() {
    let p = pair("t18-answer-in-call", 0x180b).await;
    p.client
        .voice()
        .join(p.voice())
        .await
        .expect("client")
        .expect("in the voice channel's call");
    wait_for(&p.client, WAIT, "in the voice call", |s| {
        s.voice.call.phase == dilla_client::calls::CallPhase::In
            && s.voice.call.channel == Some(p.voice())
    })
    .await;
    let dm = open_dm(&p.peer, p.me()).await;
    peer_calls(&p.peer, dm).await;
    wait_for(&p.client, WAIT, "the ring", |s| ringing(s, dm)).await;
    p.client
        .voice()
        .join_dm(dm)
        .await
        .expect("client")
        .expect("the answer joins the DM's call");
    wait_for(&p.client, WAIT, "in the DM's call", |s| {
        s.voice.call.phase == dilla_client::calls::CallPhase::In && s.voice.call.channel == Some(dm)
    })
    .await;
    let me = dilla_client::state::DeviceId(p.me.device_id);
    wait_for(
        &p.client,
        WAIT,
        "the old call's roster without this device",
        |s| {
            !s.voice
                .participants(p.voice())
                .iter()
                .any(|x| x.device_id == me)
        },
    )
    .await;
    let joined = p.peer.call_devices(dm).await;
    assert!(joined.contains(&hex(&p.me.device_id)), "{joined:?}");
}

/// Whether `user` is listed away in `community`'s AFK snapshot.
fn listed_away(s: &dilla_client::state::AppState, community: CommunityId, user: UserId) -> bool {
    s.ring
        .afk
        .get(&community)
        .is_some_and(|a| a.away.iter().any(|x| x.user == user))
}

/// e2e/web/afk.spec.ts, "8. Q2 (revised, AW2 final): his last connection closing keeps Björn away through the 30 s grace,
/// then takes him off the roster; back, nothing joins". Björn is away (he chose AFK); his only device stops. Ada still
/// lists him away while the close lands and 20 s into the grace (the host's clock); once the 30 s grace ends with no
/// connection the roster drops him. His device starts again: no call, and nobody listed away.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core"]
async fn the_last_connection_closing_keeps_a_member_away_through_the_grace_then_drops_him() {
    let tag = "t28-afk-grace";
    let host = tokio::task::spawn_blocking(move || world::start_host(tag))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let ada = world::sign_up(&host, &invite, "ada", tag).await;
    let bjorn = world::sign_up(&host, &invite, "bjorn", tag).await;
    let (host, ada, bjorn, community) = tokio::task::spawn_blocking(move || {
        let (c, _) = world::voice_community(&host, &ada, &[&bjorn]);
        world::clear_session(&ada);
        world::clear_session(&bjorn);
        (host, ada, bjorn, CommunityId(c))
    })
    .await
    .unwrap();
    let him = UserId(bjorn.user_id);
    let a = world::start(&ada).await;
    world::connected(&a).await;
    let b = world::start(&bjorn).await;
    world::connected(&b).await;
    b.ring()
        .go_afk(community)
        .await
        .unwrap()
        .expect("Björn goes AFK");
    a.ring()
        .load_afk(community)
        .await
        .unwrap()
        .expect("Ada reads the AFK roster");
    wait_for(&a, WAIT, "Björn listed away", |s| {
        listed_away(s, community, him)
    })
    .await;

    // His last connection closes: inside the grace he stays listed while the close lands, and 20 s into it.
    tokio::task::spawn_blocking(move || b.stop()).await.unwrap();
    tokio::time::sleep(Duration::from_secs(3)).await;
    assert!(
        listed_away(&a.state().borrow(), community, him),
        "still away inside the 30 s grace"
    );
    let h = tokio::task::spawn_blocking(move || {
        advance_clock(&host, 20);
        host
    })
    .await
    .unwrap();
    tokio::time::sleep(Duration::from_secs(1)).await;
    assert!(
        listed_away(&a.state().borrow(), community, him),
        "still away 20 s into the grace"
    );
    // The grace ends with no connection: the roster drops him.
    let host = tokio::task::spawn_blocking(move || {
        advance_clock(&h, 11);
        h
    })
    .await
    .unwrap();
    wait_for(&a, WAIT, "the roster without Björn", |s| {
        s.ring
            .afk
            .get(&community)
            .is_some_and(|x| x.away.is_empty())
    })
    .await;

    // Back: his device starts again, joins no call and lists nobody away.
    let b = world::start(&bjorn).await;
    world::connected(&b).await;
    b.ring()
        .load_afk(community)
        .await
        .unwrap()
        .expect("Björn reads the AFK roster");
    let s = wait_for(&b, WAIT, "Björn's AFK roster", |s| {
        s.ring.afk.contains_key(&community)
    })
    .await;
    assert!(
        s.ring.afk[&community].away.is_empty(),
        "nobody away: {:?}",
        s.ring.afk[&community]
    );
    tokio::time::sleep(Duration::from_secs(2)).await;
    let s = b.state().borrow().clone();
    assert_eq!(
        s.voice.call.phase,
        dilla_client::calls::CallPhase::Idle,
        "nothing joins"
    );
    assert!(!s.ring.afk[&community].pending, "no cut pending");
    drop(host);
}
