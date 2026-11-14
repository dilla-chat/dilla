//! The sealed path against a fake Bot (catalog.test.ts, commands.test.ts): the real reply HPKE from dilla-core, a fake
//! core for the key check and the pin, and a fake instance that records what is posted.

use std::sync::Mutex;

use dilla_core::bot::command::seal_reply;

use super::*;

pub(crate) const COMMUNITY: CommunityId = CommunityId([7; 16]);
const BOT: [u8; 16] = [0xb0; 16];
const OTHER_BOT: [u8; 16] = [0xb1; 16];

/// What the fake core answers a key check.
type Verdict = Result<([u8; 16], String), Refusal>;

#[derive(Default)]
pub(crate) struct Fake {
    pub(crate) candidate: Mutex<Option<Candidate>>,
    /// The core store's pin.
    pub(crate) pinned: Mutex<Option<[u8; 16]>>,
    /// Users the instance attested as Bots.
    pub(crate) attested: Mutex<Vec<[u8; 16]>>,
    /// What `verify` answers, in order (the last one repeats).
    pub(crate) verify: Mutex<Vec<Verdict>>,
    pub(crate) verified: Mutex<u32>,
    pub(crate) device_list_state: Mutex<Option<u8>>,
    pub(crate) seal_refusals: Mutex<Vec<Refusal>>,
    /// The public half of each reply secret made, in order.
    pub(crate) reply_keys: Mutex<Vec<[u8; 32]>>,
    pub(crate) posted: Mutex<Vec<Vec<u8>>>,
    pub(crate) post_refusal: Mutex<Option<Refusal>>,
    pub(crate) catalog: Mutex<Option<Vec<u8>>>,
    pub(crate) catalog_reads: Mutex<u32>,
    pub(crate) reads_at_once: Mutex<u32>,
    pub(crate) max_reads_at_once: Mutex<u32>,
    pub(crate) published: Mutex<Vec<Option<BotCatalogView>>>,
    /// The client's runtime; the test's own when none is set.
    pub(crate) runtime: Mutex<Option<tokio::runtime::Handle>>,
    /// While set, each key check waits for a permit (a check in flight).
    pub(crate) verify_gate: Mutex<Option<Arc<tokio::sync::Semaphore>>>,
    /// While not empty, each key check takes the first entry: it waits for that permit, then answers that verdict.
    pub(crate) verify_each: Mutex<Vec<(Arc<tokio::sync::Semaphore>, Verdict)>>,
}

#[async_trait]
impl BotIo for Fake {
    async fn bot_user_of(&self, _: CommunityId) -> Candidate {
        lock(&self.candidate).unwrap_or(Candidate::None)
    }
    async fn ingest_attestation(&self, _: [u8; 16]) -> Result<(), Refusal> {
        Ok(())
    }
    async fn pin_bot_user(
        &self,
        _: CommunityId,
        user: [u8; 16],
    ) -> Result<(u8, [u8; 16]), Refusal> {
        if !lock(&self.attested).contains(&user) {
            return Err(Refusal::new("E_DEVICE_LIST_STALE", 0));
        }
        let mut pinned = lock(&self.pinned);
        Ok(match *pinned {
            None => {
                *pinned = Some(user);
                (0, user)
            }
            Some(p) if p == user => (1, p),
            Some(p) => (2, p),
        })
    }
    async fn pinned_bot_user(&self, _: CommunityId) -> Option<[u8; 16]> {
        *lock(&self.pinned)
    }
    async fn verify(&self, _: Vec<u8>, _: [u8; 16]) -> Result<([u8; 16], String), Refusal> {
        let each = {
            let mut each = lock(&self.verify_each);
            (!each.is_empty()).then(|| each.remove(0))
        };
        if let Some((gate, verdict)) = each {
            *lock(&self.verified) += 1;
            gate.acquire().await.unwrap().forget();
            return verdict;
        }
        let gate = lock(&self.verify_gate).clone();
        *lock(&self.verified) += 1;
        if let Some(gate) = gate {
            gate.acquire().await.unwrap().forget();
        }
        let mut v = lock(&self.verify);
        if v.len() > 1 {
            v.remove(0)
        } else {
            v[0].clone()
        }
    }
    async fn pin_device_list(&self, _: [u8; 16]) -> Result<u8, Refusal> {
        lock(&self.device_list_state).ok_or_else(|| Refusal::new("E_DEVICE_LIST_STALE", 0))
    }
    async fn seal(
        &self,
        _: Vec<u8>,
        _: [u8; 16],
        request: zeroize::Zeroizing<Vec<u8>>,
    ) -> Result<(Vec<u8>, ReplySecret), Refusal> {
        if let Some(r) = lock(&self.seal_refusals).pop() {
            return Err(r);
        }
        let secret = ReplySecret::generate().unwrap();
        lock(&self.reply_keys).push(secret.public_key());
        Ok((request.to_vec(), secret))
    }
    async fn post(&self, _: CommunityId, body: Vec<u8>, _: Vec<u8>) -> Result<(), Refusal> {
        if let Some(r) = lock(&self.post_refusal).clone() {
            return Err(r);
        }
        lock(&self.posted).push(body);
        Ok(())
    }
    async fn fetch_catalog(&self, _: CommunityId) -> Result<Vec<u8>, Refusal> {
        *lock(&self.catalog_reads) += 1;
        {
            let mut now = lock(&self.reads_at_once);
            *now += 1;
            let mut max = lock(&self.max_reads_at_once);
            *max = (*max).max(*now);
        }
        for _ in 0..10 {
            tokio::task::yield_now().await;
        }
        *lock(&self.reads_at_once) -= 1;
        lock(&self.catalog)
            .clone()
            .ok_or_else(|| Refusal::new("E_NOT_FOUND", 404))
    }
    async fn fetch_bot_channels(&self, _: CommunityId) -> Result<Vec<ChannelId>, Refusal> {
        Ok(vec![])
    }
    fn now_s(&self) -> u64 {
        1_800_000_000
    }
    fn random16(&self) -> [u8; 16] {
        [9; 16]
    }
    fn publish(&self, _: CommunityId, view: Option<BotCatalogView>) {
        lock(&self.published).push(view);
    }
    fn spawn(&self, task: Task) {
        let handle = lock(&self.runtime)
            .clone()
            .unwrap_or_else(tokio::runtime::Handle::current);
        handle.spawn(task);
    }
}

pub(crate) fn publication(community: [u8; 16]) -> Vec<u8> {
    Value::Array(vec![
        Value::Uint(1),
        Value::Bytes(community.to_vec()),
        Value::Bytes(vec![3; 16]),
    ])
    .encode()
}

pub(crate) fn frame(revision: u64, publication: Option<Vec<u8>>) -> BotCatalogFrame {
    let remind = Value::Array(vec![
        Value::Text("remind".into()),
        Value::Text("Set a reminder".into()),
        Value::Uint(80),
        Value::Array(vec![]),
        Value::Uint(0),
        Value::Uint(0),
        Value::Uint(0),
    ]);
    BotCatalogFrame {
        community_id: COMMUNITY.0,
        revision,
        commands: vec![remind],
        publication,
    }
}

/// `GET /bot/catalog`: `[revision, commands, publication, [ready, null]]`.
pub(crate) fn catalog_body(revision: u64) -> Vec<u8> {
    Value::Array(vec![
        Value::Uint(revision),
        Value::Array(vec![]),
        Value::Bytes(publication(COMMUNITY.0)),
        Value::Array(vec![Value::Uint(0), Value::Null]),
    ])
    .encode()
}

/// A Bot that checks out: attested, named by the server, its key verifying.
pub(crate) fn ready_fake() -> Arc<Fake> {
    let fake = Fake::default();
    *lock(&fake.candidate) = Some(Candidate::One(BOT));
    lock(&fake.attested).push(BOT);
    *lock(&fake.verify) = vec![Ok(([3; 16], "ABCDEFGHJKMN".into()))];
    Arc::new(fake)
}

pub(crate) async fn ready(fake: &Arc<Fake>) -> Arc<Sealed> {
    let sealed = Sealed::new(fake.clone());
    sealed
        .apply_frame(&frame(1, Some(publication(COMMUNITY.0))))
        .await;
    sealed
}

fn submit(request_id: [u8; 16]) -> Submit {
    Submit {
        community: COMMUNITY,
        channel: Some(ChannelId([2; 16])),
        operation: 80,
        args: vec![BotArg::Uint(3600), BotArg::Text("stretch".into())],
        request_id,
    }
}

/// The fake Bot's answer number `seq` to `request_id`, sealed to reply key `key`, as op 62 carries it.
pub(crate) fn reply(
    key: [u8; 32],
    request_id: [u8; 16],
    seq: u32,
    status: u16,
    body: &str,
    final_: bool,
) -> BotReplyFrame {
    BotReplyFrame {
        request_id,
        seq,
        sealed: seal_reply(
            &key,
            request_id,
            seq,
            &encode_answer(status, body.as_bytes(), final_),
        )
        .unwrap(),
        // op 62's own flag is the server's: deliberately the opposite of the sealed one.
        final_: !final_,
    }
}

/// Waits (yielding) until `n` commands were posted.
pub(crate) async fn posted(fake: &Fake, n: usize) {
    for _ in 0..1000 {
        if lock(&fake.posted).len() >= n {
            return;
        }
        tokio::task::yield_now().await;
    }
    panic!("{n} commands were not posted");
}

#[tokio::test]
async fn a_verified_key_is_pinned_ready_and_shows_its_fingerprint() {
    let fake = ready_fake();
    let sealed = ready(&fake).await;
    let view = sealed.view(COMMUNITY).unwrap();
    assert_eq!(view.status.state, BotStatusState::Ready);
    assert_eq!(view.fingerprint.as_deref(), Some("ABCDEFGHJKMN"));
    assert_eq!(view.bot_user, Some(UserId(BOT)));
    assert_eq!(*lock(&fake.pinned), Some(BOT));
}

#[tokio::test]
async fn seal_send_and_open_round_trip() {
    let fake = ready_fake();
    let sealed = ready(&fake).await;
    let rid = [5; 16];
    let run = {
        let sealed = sealed.clone();
        tokio::spawn(async move {
            let mut progress = Vec::new();
            let end = sealed
                .run(&submit(rid), |a| progress.push(a.body.clone()))
                .await;
            (end, progress)
        })
    };
    posted(&fake, 1).await;
    // The posted body is [request_id, sealed].
    let body = Value::decode(&lock(&fake.posted)[0]).unwrap();
    let parts = body.as_array().unwrap();
    assert_eq!(parts[0].as_bytes(), Some(&rid[..]));
    let key = lock(&fake.reply_keys)[0];
    sealed.on_reply(&reply(key, rid, 0, 200, "Searching…", false));
    sealed.on_reply(&reply(key, rid, 1, 200, "Reminder set.", true));
    let (end, progress) = run.await.unwrap();
    assert_eq!(progress, [b"Searching\xe2\x80\xa6".to_vec()]);
    let RunEnd::Final(answer) = end else {
        panic!("{end:?}")
    };
    assert_eq!(
        (answer.status, answer.body.as_slice(), answer.seq),
        (200, &b"Reminder set."[..], 1)
    );
    assert!(
        !sealed.holds(rid),
        "the reply secret goes with the final answer"
    );
}

#[tokio::test(start_paused = true)]
async fn forged_and_replayed_replies_are_dropped() {
    let fake = ready_fake();
    let sealed = ready(&fake).await;
    let rid = [5; 16];
    let run = {
        let sealed = sealed.clone();
        tokio::spawn(async move {
            let mut progress = 0;
            let end = sealed.run(&submit(rid), |_| progress += 1).await;
            (end, progress)
        })
    };
    posted(&fake, 1).await;
    let key = lock(&fake.reply_keys)[0];
    // Sealed to another key (not this device's reply secret).
    let stranger = ReplySecret::generate().unwrap().public_key();
    sealed.on_reply(&reply(stranger, rid, 0, 200, "forged", true));
    // Sealed to the right key but for another request id (the AAD binds it): relabelled as this request.
    let mut relabelled = reply(key, [6; 16], 0, 200, "other request", true);
    relabelled.request_id = rid;
    sealed.on_reply(&relabelled);
    // Sealed for another seq than the frame says.
    let mut reseq = reply(key, rid, 3, 200, "other seq", true);
    reseq.seq = 0;
    sealed.on_reply(&reseq);
    // For a request this device never sent.
    sealed.on_reply(&reply(key, [8; 16], 0, 200, "unknown", true));
    // A genuine progress answer, then the same again (a replay).
    sealed.on_reply(&reply(key, rid, 0, 200, "Searching…", false));
    sealed.on_reply(&reply(key, rid, 0, 200, "Searching…", false));
    let (end, progress) = run.await.unwrap();
    assert_eq!(progress, 1, "only the genuine answer, once");
    assert_eq!(end, RunEnd::Timeout, "no forged answer ended the run");
}

#[tokio::test(start_paused = true)]
async fn no_first_answer_in_five_seconds_is_a_timeout_and_retry_reuses_the_request_id() {
    let fake = ready_fake();
    let sealed = ready(&fake).await;
    let rid = [5; 16];
    let started = tokio::time::Instant::now();
    assert_eq!(sealed.run(&submit(rid), |_| {}).await, RunEnd::Timeout);
    assert_eq!(started.elapsed(), FIRST_ANSWER);
    assert!(sealed.holds(rid), "a late answer can still be opened");
    let retry = {
        let sealed = sealed.clone();
        tokio::spawn(async move { sealed.run(&submit(rid), |_| {}).await })
    };
    posted(&fake, 2).await;
    let posted_ids: Vec<Vec<u8>> = lock(&fake.posted)
        .iter()
        .map(|b| {
            Value::decode(b).unwrap().as_array().unwrap()[0]
                .as_bytes()
                .unwrap()
                .to_vec()
        })
        .collect();
    assert_eq!(posted_ids, [rid.to_vec(), rid.to_vec()]);
    // Only the newest send's secret opens: an answer sealed to the first one is dropped.
    let keys = lock(&fake.reply_keys).clone();
    let (first, second) = (keys[0], keys[1]);
    sealed.on_reply(&reply(first, rid, 0, 200, "old seal", true));
    sealed.on_reply(&reply(second, rid, 0, 200, "Done", true));
    let RunEnd::Final(a) = retry.await.unwrap() else {
        panic!()
    };
    assert_eq!(a.body, b"Done");
}

#[tokio::test]
async fn a_stale_key_answer_reads_the_catalog_once_and_resends() {
    let fake = ready_fake();
    let sealed = ready(&fake).await;
    *lock(&fake.catalog) = Some(catalog_body(2));
    let rid = [5; 16];
    let run = {
        let sealed = sealed.clone();
        tokio::spawn(async move { sealed.run(&submit(rid), |_| {}).await })
    };
    posted(&fake, 1).await;
    let key = lock(&fake.reply_keys)[0];
    sealed.on_reply(&reply(
        key,
        rid,
        0,
        KEY_STALE_STATUS,
        "E_BOT_KEY_STALE",
        true,
    ));
    posted(&fake, 2).await;
    assert_eq!(*lock(&fake.catalog_reads), 1);
    let key = lock(&fake.reply_keys)[1];
    sealed.on_reply(&reply(
        key,
        rid,
        0,
        KEY_STALE_STATUS,
        "E_BOT_KEY_STALE",
        true,
    ));
    assert_eq!(
        run.await.unwrap(),
        RunEnd::Refused(Refusal::new("E_BOT_KEY_STALE", KEY_STALE_STATUS)),
        "a second stale answer ends the run"
    );
}

#[tokio::test]
async fn another_bot_user_than_the_pinned_one_is_changed_and_refused() {
    let fake = ready_fake();
    *lock(&fake.pinned) = Some(OTHER_BOT);
    lock(&fake.attested).push(OTHER_BOT);
    *lock(&fake.catalog) = Some(catalog_body(1));
    // A catalog read looks the server's Bot user up again: it names BOT, but this device pinned OTHER_BOT.
    let sealed = Sealed::new(fake.clone());
    sealed.refresh(COMMUNITY).await.unwrap();
    assert_eq!(
        sealed.view(COMMUNITY).unwrap().status.state,
        BotStatusState::Changed
    );
    assert_eq!(
        *lock(&fake.pinned),
        Some(OTHER_BOT),
        "the pin is never replaced"
    );
    assert_eq!(
        sealed.run(&submit([5; 16]), |_| {}).await,
        RunEnd::Refused(Refusal::new("E_BOT_OFFLINE", 503))
    );
    assert!(
        lock(&fake.posted).is_empty(),
        "nothing is sealed to a changed Bot"
    );
    assert_eq!(*lock(&fake.verified), 0, "its key is not even checked");
}

#[tokio::test]
async fn a_held_key_change_of_the_bots_device_list_is_changed_and_refused() {
    let fake = ready_fake();
    // No device list yet: the list is read and pinned, and it holds a key change; the core then refuses the key.
    *lock(&fake.verify) = vec![
        Err(Refusal::new("E_DEVICE_LIST_STALE", 0)),
        Err(Refusal::new("E_UMK_CHANGED", 0)),
    ];
    *lock(&fake.device_list_state) = Some(1);
    let sealed = ready(&fake).await;
    assert_eq!(
        sealed.view(COMMUNITY).unwrap().status.state,
        BotStatusState::Changed
    );
    assert_eq!(
        sealed.run(&submit([5; 16]), |_| {}).await,
        RunEnd::Refused(Refusal::new("E_BOT_OFFLINE", 503))
    );
    assert!(lock(&fake.posted).is_empty());
}

/// Task 28 item 21 (catalog.ts `attestationsChanged`, review M6): a first pin refused for want of the Bot's attestation
/// is tried again once the trust refresh ingested attestations, not only on the next catalog read.
#[tokio::test]
async fn an_attestation_the_trust_refresh_brings_pins_the_waiting_bot() {
    let fake = ready_fake();
    lock(&fake.attested).clear();
    let sealed = ready(&fake).await;
    assert_eq!(*lock(&fake.pinned), None, "no attestation yet: no pin");
    let reads = *lock(&fake.catalog_reads);
    // The trust refresh ingested the Bot's attestation (from a leaf).
    lock(&fake.attested).push(BOT);
    sealed.attestations_changed().await;
    assert_eq!(
        *lock(&fake.pinned),
        Some(BOT),
        "the waiting pin is tried again"
    );
    assert_eq!(
        sealed.view(COMMUNITY).unwrap().status.state,
        BotStatusState::Ready
    );
    assert_eq!(*lock(&fake.catalog_reads), reads, "no catalog read for it");
}

#[tokio::test]
async fn a_human_named_as_the_bot_is_never_pinned() {
    let fake = ready_fake();
    lock(&fake.attested).clear();
    let sealed = ready(&fake).await;
    assert_eq!(*lock(&fake.pinned), None);
    assert_eq!(
        sealed.view(COMMUNITY).unwrap().status.state,
        BotStatusState::Unverified
    );
    assert!(matches!(
        sealed.run(&submit([5; 16]), |_| {}).await,
        RunEnd::Refused(_)
    ));
    assert!(lock(&fake.posted).is_empty());
}

#[tokio::test]
async fn a_publication_for_another_community_is_not_checked() {
    let fake = ready_fake();
    let sealed = Sealed::new(fake.clone());
    sealed
        .apply_frame(&frame(1, Some(publication([8; 16]))))
        .await;
    assert_eq!(*lock(&fake.verified), 0);
    assert_eq!(
        sealed.view(COMMUNITY).unwrap().status.state,
        BotStatusState::Unverified
    );
}

#[tokio::test]
async fn an_older_catalog_frame_is_ignored_and_no_publication_reads_offline() {
    let fake = ready_fake();
    let sealed = Sealed::new(fake.clone());
    sealed.apply_frame(&frame(3, None)).await;
    sealed
        .apply_frame(&frame(2, Some(publication(COMMUNITY.0))))
        .await;
    let view = sealed.view(COMMUNITY).unwrap();
    assert_eq!(view.revision, 3);
    assert_eq!(view.status.state, BotStatusState::Offline);
}

#[tokio::test]
async fn a_refused_post_forgets_the_reply_secret() {
    let fake = ready_fake();
    let sealed = ready(&fake).await;
    *lock(&fake.post_refusal) = Some(Refusal::new("E_BOT_OFFLINE", 503));
    assert_eq!(
        sealed.run(&submit([5; 16]), |_| {}).await,
        RunEnd::Refused(Refusal::new("E_BOT_OFFLINE", 503))
    );
    assert!(!sealed.holds([5; 16]));
}

#[tokio::test]
async fn the_admin_transport_maps_answers_and_refusals() {
    let fake = ready_fake();
    let sealed = ready(&fake).await;
    let transport = Transport(sealed.clone());
    let command = SealedCommand {
        operation: 94,
        channel: None,
        args: vec![],
        request_id: [4; 16],
    };
    let send = tokio::spawn(async move { transport.send(COMMUNITY, command).await });
    posted(&fake, 1).await;
    let key = lock(&fake.reply_keys)[0];
    sealed.on_reply(&reply(key, [4; 16], 0, 200, "ok", true));
    let answer = send.await.unwrap().unwrap();
    assert_eq!((answer.status, answer.body.as_slice()), (200, &b"ok"[..]));
    *lock(&fake.post_refusal) = Some(Refusal::new("E_FORBIDDEN", 403));
    let err = Transport(sealed)
        .send(
            COMMUNITY,
            SealedCommand {
                operation: 94,
                channel: None,
                args: vec![],
                request_id: [3; 16],
            },
        )
        .await
        .unwrap_err();
    assert_eq!(
        err,
        SendError::Refused {
            code: "E_FORBIDDEN".into(),
            status: 403
        }
    );
}

#[tokio::test]
async fn a_umk_change_refused_by_the_core_is_changed_and_refused() {
    let fake = ready_fake();
    *lock(&fake.verify) = vec![Err(Refusal::new("E_UMK_CHANGED", 0))];
    let sealed = ready(&fake).await;
    assert_eq!(
        sealed.view(COMMUNITY).unwrap().status.state,
        BotStatusState::Changed
    );
    assert_eq!(
        sealed.run(&submit([5; 16]), |_| {}).await,
        RunEnd::Refused(Refusal::new("E_BOT_OFFLINE", 503))
    );
    assert!(lock(&fake.posted).is_empty());
}

/// Task 28 (found by `tests/bot::enter_to_first_answer_is_within_rtt_plus_100ms_at_p95`): the web checks a key in the
/// same step that replaces the entry (its core call is synchronous), so a re-read of the catalog never shows it
/// unchecked. Here the check is awaited: while a re-read (a sync, a state change) or a push of the same publication is
/// checked again, the key stays verified, so a command sent then is not refused "unverified".
#[tokio::test]
async fn a_re_read_of_the_same_publication_stays_verified_while_it_is_checked_again() {
    let fake = ready_fake();
    *lock(&fake.catalog) = Some(catalog_body(1));
    let sealed = ready(&fake).await;
    assert!(sealed.view(COMMUNITY).unwrap().fingerprint.is_some());
    let gate = Arc::new(tokio::sync::Semaphore::new(0));
    *lock(&fake.verify_gate) = Some(gate.clone());
    let before = *lock(&fake.verified);
    let read = tokio::spawn({
        let sealed = sealed.clone();
        async move { sealed.refresh(COMMUNITY).await }
    });
    while *lock(&fake.verified) == before {
        tokio::task::yield_now().await;
    }
    assert_eq!(
        sealed.view(COMMUNITY).unwrap().fingerprint.as_deref(),
        Some("ABCDEFGHJKMN"),
        "verified while the same key is checked again"
    );
    gate.add_permits(1);
    read.await.unwrap().unwrap();
    assert!(sealed.view(COMMUNITY).unwrap().fingerprint.is_some());

    // A check that now fails takes the key away.
    *lock(&fake.verify) = vec![Err(Refusal::new("E_BOT_SIGNATURE", 0))];
    *lock(&fake.verify_gate) = None;
    sealed.refresh(COMMUNITY).await.unwrap();
    assert_eq!(sealed.view(COMMUNITY).unwrap().fingerprint, None);
}

#[tokio::test]
async fn catalog_reads_of_one_community_run_one_at_a_time() {
    let fake = ready_fake();
    *lock(&fake.catalog) = Some(catalog_body(1));
    let sealed = Sealed::new(fake.clone());
    let reads: Vec<_> = (0..3)
        .map(|_| {
            let sealed = sealed.clone();
            tokio::spawn(async move { sealed.refresh(COMMUNITY).await })
        })
        .collect();
    for r in reads {
        r.await.unwrap().unwrap();
    }
    assert_eq!(*lock(&fake.catalog_reads), 3);
    assert_eq!(
        *lock(&fake.max_reads_at_once),
        1,
        "never two reads of one community at once"
    );
}

#[tokio::test(start_paused = true)]
async fn a_late_answer_after_the_timeout_still_arrives() {
    let fake = ready_fake();
    let sealed = ready(&fake).await;
    let rid = [5; 16];
    assert_eq!(sealed.run(&submit(rid), |_| {}).await, RunEnd::Timeout);
    let mut late = sealed.late(rid).expect("the reply secret is held");
    let key = lock(&fake.reply_keys)[0];
    sealed.on_reply(&reply(key, rid, 0, 200, "Reminder set.", true));
    let a = late.recv().await.unwrap();
    assert_eq!(a.body, b"Reminder set.");
    assert!(!sealed.holds(rid));
}

/// Yields until `done`, failing after 10 s (a check that never starts must not spin forever).
async fn spin_until(done: impl Fn() -> bool) {
    tokio::time::timeout(std::time::Duration::from_secs(10), async {
        while !done() {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("the key check started");
}

/// t28-bot-review M1: a verdict is carried only for the byte-identical publication under the same pin: a different
/// publication, a Bot user problem, or a pin the core made again for another user (it lost its pin) is unchecked until
/// its own check answers.
#[tokio::test]
async fn a_verdict_is_never_carried_to_another_publication_a_problem_or_another_pin() {
    // Another publication of the same community.
    let fake = ready_fake();
    let sealed = ready(&fake).await;
    assert!(sealed.view(COMMUNITY).unwrap().fingerprint.is_some());
    let gate = Arc::new(tokio::sync::Semaphore::new(0));
    *lock(&fake.verify_gate) = Some(gate.clone());
    let before = *lock(&fake.verified);
    // The same community, another key id.
    let other = Value::Array(vec![
        Value::Uint(1),
        Value::Bytes(COMMUNITY.0.to_vec()),
        Value::Bytes(vec![4; 16]),
    ])
    .encode();
    let push = tokio::spawn({
        let sealed = sealed.clone();
        async move { sealed.apply_frame(&frame(2, Some(other))).await }
    });
    spin_until(|| *lock(&fake.verified) != before).await;
    assert_eq!(
        sealed.view(COMMUNITY).unwrap().fingerprint,
        None,
        "another key is unchecked while it is checked"
    );
    assert!(lock(&sealed.store).publication(COMMUNITY).is_none());
    gate.add_permits(1);
    push.await.unwrap();

    // A Bot user problem: the server names another Bot user than the pinned one.
    let fake = ready_fake();
    *lock(&fake.catalog) = Some(catalog_body(1));
    let sealed = ready(&fake).await;
    assert!(sealed.view(COMMUNITY).unwrap().fingerprint.is_some());
    *lock(&fake.candidate) = Some(Candidate::One(OTHER_BOT));
    lock(&fake.attested).push(OTHER_BOT);
    sealed.refresh(COMMUNITY).await.unwrap();
    let view = sealed.view(COMMUNITY).unwrap();
    assert_eq!(view.status.state, BotStatusState::Changed);
    assert_eq!(view.fingerprint, None);

    // The core lost its pin and pins another user now (answer 0): the old verdict is not carried under it.
    let fake = ready_fake();
    *lock(&fake.catalog) = Some(catalog_body(1));
    let sealed = ready(&fake).await;
    assert!(sealed.view(COMMUNITY).unwrap().fingerprint.is_some());
    *lock(&fake.pinned) = None;
    *lock(&fake.candidate) = Some(Candidate::One(OTHER_BOT));
    lock(&fake.attested).push(OTHER_BOT);
    let gate = Arc::new(tokio::sync::Semaphore::new(0));
    *lock(&fake.verify_gate) = Some(gate.clone());
    let before = *lock(&fake.verified);
    let read = tokio::spawn({
        let sealed = sealed.clone();
        async move { sealed.refresh(COMMUNITY).await }
    });
    spin_until(|| *lock(&fake.verified) != before).await;
    assert_eq!(sealed.view(COMMUNITY).unwrap().fingerprint, None);
    assert!(lock(&sealed.store).publication(COMMUNITY).is_none());
    gate.add_permits(1);
    read.await.unwrap().unwrap();
    assert!(
        sealed.view(COMMUNITY).unwrap().fingerprint.is_some(),
        "checked against the new pin"
    );
}

/// t28-bot-review M2: of two checks of one key in flight, the newer answers for it: an older failure landing late
/// does not clear the newer verdict, and an older `Ok` landing after a newer refusal does not verify it.
#[tokio::test]
async fn an_older_key_check_answering_late_changes_nothing() {
    let refused = || Err(Refusal::new("E_BOT_SIGNATURE", 0));
    let ok = || Ok(([3; 16], "ABCDEFGHJKMN".to_owned()));
    for (older, newer, verified) in [(refused(), ok(), true), (ok(), refused(), false)] {
        let fake = ready_fake();
        let sealed = ready(&fake).await;
        let (a, b) = (
            Arc::new(tokio::sync::Semaphore::new(0)),
            Arc::new(tokio::sync::Semaphore::new(0)),
        );
        *lock(&fake.verify_each) = vec![(a.clone(), older), (b.clone(), newer)];
        let before = *lock(&fake.verified);
        let first = tokio::spawn({
            let sealed = sealed.clone();
            async move { sealed.verify_entry(COMMUNITY).await }
        });
        spin_until(|| *lock(&fake.verified) != before).await;
        let second = tokio::spawn({
            let sealed = sealed.clone();
            async move { sealed.verify_entry(COMMUNITY).await }
        });
        spin_until(|| *lock(&fake.verified) != before + 1).await;
        b.add_permits(1);
        second.await.unwrap();
        a.add_permits(1);
        first.await.unwrap();
        assert_eq!(
            sealed.view(COMMUNITY).unwrap().fingerprint.is_some(),
            verified,
            "the newer check's answer stands"
        );
    }
}
