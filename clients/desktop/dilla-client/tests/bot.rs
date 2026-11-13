//! Task 20's client tests for the bot area (standard procedure S2), against a real dillad.
//!
//! Needs `target/dilla-testhost` and the WASI core, like `tests/sync.rs`; `#[ignore]`d so a plain `cargo test` needs no
//! Go toolchain. The pure logic (the slash parser, private results, reminder times, poll payloads) is unit-tested in
//! `src/features/{bot,utilities}/`.

use std::sync::Arc;
use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::bot::run::{Text, Texts, Zone};
use dilla_client::features::bot::{RunOutcome, not_a_command};
use dilla_client::state::bot::{
    Conversation, PRIVATE_RESULT_TTL_MS, PrivateResult, ReminderStatus, ResultProvider,
    ResultStatus,
};
use dilla_client::state::{AppState, ChannelId, CommunityId};
use dilla_client::testing::host::bot::{cap, enable_bot};
use dilla_client::testing::host::{
    Host, Person, clear_session, community, sign_up, start, start_bot_host, start_host, wait_for,
};

async fn two_people(tag: &str) -> (Host, Person, Person, [u8; 16]) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_host(&tag, "127.0.0.1:0", None)
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let a = sign_up(&host, &invite, "ada", "ada", tag).await;
    let b = sign_up(&host, &invite, "bjorn", "bjorn", tag).await;
    tokio::task::spawn_blocking(move || {
        let c = community(&host, &a, "midgard", &[&b]);
        clear_session(&a);
        clear_session(&b);
        (host, a, b, c)
    })
    .await
    .unwrap()
}

async fn general(client: &Client, community: [u8; 16]) -> ChannelId {
    let s = wait_for(client, Duration::from_secs(30), "the channel list", |s| {
        s.channel_named(CommunityId(community), "general").is_some()
    })
    .await;
    s.channel_named(CommunityId(community), "general")
        .unwrap()
        .id
}

fn bodies(s: &AppState, channel: ChannelId) -> Vec<String> {
    s.timelines
        .get(&channel)
        .map(|t| t.items.iter().map(|i| i.body.clone()).collect())
        .unwrap_or_default()
}

/// e2e/web/slash-commands.spec.ts "composer keeps private results private, publishes only explicit text, and retains
/// separate channel/thread drafts" (its private-result half; the drafts are the composer's, Task 9): `/help` and an
/// unknown `/xyz` answer with rows only this device holds; nothing reaches the other device's timeline or this one's;
/// `//shrug` posts `/shrug` literally; the rows are gone after a restart and go away by themselves after the TTL.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn private_results_stay_private_and_auto_dismiss() {
    let (_host, pa, pb, c) = two_people("private").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let channel = general(&a, c).await;
    general(&b, c).await;
    a.open_channel(channel)
        .await
        .unwrap()
        .expect("a opens general");
    b.open_channel(channel)
        .await
        .unwrap()
        .expect("b joins general");
    let here = Conversation {
        channel,
        thread: None,
    };
    let community = Some(CommunityId(c));

    let help = a.bot().run(here, community, "/help");
    let unknown = a.bot().run(here, community, "/xyz");
    assert!(matches!(help, RunOutcome::Private(_)), "{help:?}");
    assert!(matches!(unknown, RunOutcome::Private(_)), "{unknown:?}");
    let s = wait_for(&a, Duration::from_secs(5), "a's two private rows", |s| {
        s.bot.results(&here).len() == 2
    })
    .await;
    let rows = s.bot.results(&here);
    assert_eq!(
        (rows[0].status, rows[0].provider),
        (ResultStatus::Success, ResultProvider::Builtin)
    );
    assert!(rows[0].text.starts_with("/help [command]"));
    assert_eq!(rows[1].status, ResultStatus::Failed);
    assert_eq!(rows[1].text, not_a_command("xyz"));
    let settled = rows
        .iter()
        .map(|r| r.settled_at.expect("a finished row is stamped"))
        .max()
        .unwrap();

    // An explicit literal is the only thing that posts.
    let RunOutcome::Post(text) = a.bot().run(here, community, "//shrug") else {
        panic!("a double slash posts its text");
    };
    assert_eq!(text, "/shrug");
    a.send_text(channel, text).await.unwrap().expect("queued");
    let seen = wait_for(&b, Duration::from_secs(10), "b to see the literal", |s| {
        bodies(s, channel).contains(&"/shrug".to_owned())
    })
    .await;
    // The other device holds no private row, and neither timeline carries one.
    assert!(
        seen.bot.private_results.is_empty(),
        "b holds no private result"
    );
    assert_eq!(
        bodies(&seen, channel),
        ["/shrug"],
        "b's timeline holds only the explicit post"
    );
    let mine = a.state().borrow().clone();
    assert!(
        bodies(&mine, channel).iter().all(|b| b == "/shrug"),
        "a's timeline holds no private result: {:?}",
        bodies(&mine, channel)
    );

    // Not yet due: the rows stay; at the TTL they go.
    a.bot().sweep_at(settled + PRIVATE_RESULT_TTL_MS - 1);
    wait_for(
        &a,
        Duration::from_secs(5),
        "the rows to stay before the TTL",
        |s| s.bot.results(&here).len() == 2,
    )
    .await;
    a.bot().sweep_at(settled + PRIVATE_RESULT_TTL_MS);
    wait_for(
        &a,
        Duration::from_secs(5),
        "the rows to go at the TTL",
        |s| s.bot.results(&here).is_empty(),
    )
    .await;

    // A row made now is gone after a restart: nothing about it was stored.
    a.bot().run(here, community, "/help");
    wait_for(&a, Duration::from_secs(5), "a new row", |s| {
        s.bot.results(&here).len() == 1
    })
    .await;
    a.stop();
    let a = start(&pa).await;
    let s = wait_for(&a, Duration::from_secs(30), "a to sync again", |s| {
        s.channel(channel).is_some()
    })
    .await;
    assert!(
        s.bot.private_results.is_empty(),
        "a restart holds no private result"
    );
}

/// The sealed path against a real dillad with no Bot (this host runs none; Task 28's tests below run one): the catalog read
/// leaves nothing to seal to, so a Bot command is refused on this device (`E_BOT_OFFLINE`) before anything is sealed or
/// posted, and Task 21's operations over the same transport are refused the same way.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_bot_command_with_no_verified_key_is_refused_before_anything_is_sent() {
    use dilla_client::features::bot::sealed::{RunEnd, Submit};
    use dilla_client::features::bot_admin::{BotArg, BotOperations, BotRequest, OpError};

    let (_host, pa, _pb, c) = two_people("nobot").await;
    let a = start(&pa).await;
    let channel = general(&a, c).await;
    let community = CommunityId(c);
    // The instance answers for a community without a Bot (a 404, or a catalog with no key); either way no key is held.
    let _ = a.bot().sync_catalog(community).await;
    let sealed = a.bot().sealed();
    let submit = Submit {
        community,
        channel: Some(channel),
        operation: 80,
        args: vec![BotArg::Uint(3600), BotArg::Text("stretch".into())],
        request_id: [5; 16],
    };
    let started = std::time::Instant::now();
    let end = sealed.run(&submit, |_| {}).await;
    let RunEnd::Refused(r) = end else {
        panic!("refused locally, not {end:?}");
    };
    assert_eq!((r.code.as_str(), r.status), ("E_BOT_OFFLINE", 503));
    assert!(
        started.elapsed() < Duration::from_secs(5),
        "no wait for an answer that cannot come"
    );
    assert!(
        !sealed.holds([5; 16]),
        "no reply secret is kept for a command that was never sealed"
    );
    let err = BotOperations::new(community, a.bot().transport())
        .request(BotRequest {
            operation: 94,
            ..Default::default()
        })
        .await
        .unwrap_err();
    assert!(
        matches!(&err, OpError::Refused { code, .. } if code == "E_BOT_OFFLINE"),
        "{err:?}"
    );
}

// ---- Task 28: against dilla's real Bot (`testing::host::bot`) ----

/// [`two_people`] on a host that runs the Bot.
async fn two_people_with_bot(tag: &str) -> (Host, Person, Person, [u8; 16]) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_bot_host(&tag, "127.0.0.1:0")
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let a = sign_up(&host, &invite, "ada", "ada", tag).await;
    let b = sign_up(&host, &invite, "bjorn", "bjorn", tag).await;
    tokio::task::spawn_blocking(move || {
        let c = community(&host, &a, "midgard", &[&b]);
        clear_session(&a);
        clear_session(&b);
        (host, a, b, c)
    })
    .await
    .unwrap()
}

/// The result rows' copy: a test reads the Bot's own answers, so the client's texts are their debug names.
fn texts() -> Texts {
    Arc::new(|t: &Text| format!("{t:?}"))
}

fn utc() -> Zone {
    Zone {
        name: "UTC".into(),
        offset_at: Arc::new(|_| 0),
    }
}

/// Types `draft` in `here` and sends it as the picker does: a Bot command runs sealed; answers its private row's id.
fn run_bot(client: &Client, here: Conversation, community: CommunityId, draft: &str) -> u64 {
    match client.bot().run(here, Some(community), draft) {
        RunOutcome::Bot { definition, args } => client.bot().run_command(
            here,
            community,
            Some(here.channel),
            definition,
            args,
            &utc(),
            texts(),
        ),
        other => panic!("{draft} is a Bot command here, not {other:?}"),
    }
}

/// Waits for the private row `row` in `here` to finish (not pending), and answers it.
async fn settled(client: &Client, here: Conversation, row: u64, within: Duration) -> PrivateResult {
    let s = wait_for(client, within, "the Bot's answer", |s| {
        s.bot
            .results(&here)
            .iter()
            .any(|r| r.id == row && r.status != ResultStatus::Pending)
    })
    .await;
    s.bot
        .results(&here)
        .iter()
        .find(|r| r.id == row)
        .unwrap()
        .clone()
}

/// e2e/web/bot-commands.spec.ts "/remind round trip": with the Reminders capability on, `/remind in 1 minute <text>` is
/// one sealed command whose answer ("Saved…") opens on this device in a private row; the host's clock runs with the wall
/// clock, so a minute later the Bot lists the reminder as due (the inbox's Due tab).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
async fn remind_round_trip_through_sealed_commands() {
    let (_host, pa, _pb, c) = two_people_with_bot("t28-remind").await;
    let a = start(&pa).await;
    let channel = general(&a, c).await;
    a.open_channel(channel)
        .await
        .unwrap()
        .expect("a opens general");
    let community = CommunityId(c);
    enable_bot(&a, c, cap::REMINDERS).await;
    let here = Conversation {
        channel,
        thread: None,
    };
    let text = format!("sealed reminder {}", std::process::id());
    let row = run_bot(
        &a,
        here,
        community,
        &format!("/remind when:1m text:\"{text}\""),
    );
    let answer = settled(&a, here, row, Duration::from_secs(90)).await;
    assert_eq!(
        (answer.status, answer.provider),
        (ResultStatus::Success, ResultProvider::Bot),
        "{answer:?}"
    );
    assert!(answer.text.contains("Saved"), "{answer:?}");
    // Nothing of the command reached the channel.
    assert!(bodies(&a.state().borrow(), channel).is_empty());

    let upcoming = a.utilities().refresh_reminders(community).await.unwrap();
    let mine = upcoming
        .iter()
        .find(|r| r.text == text)
        .expect("the reminder is listed");
    assert_eq!(mine.state, ReminderStatus::Scheduled);
    let deadline = std::time::Instant::now() + Duration::from_secs(180);
    loop {
        let all = a.utilities().refresh_reminders(community).await.unwrap();
        if all
            .iter()
            .any(|r| r.text == text && r.state == ReminderStatus::Due)
        {
            break;
        }
        assert!(
            std::time::Instant::now() < deadline,
            "the reminder never fell due: {all:?}"
        );
        tokio::time::sleep(Duration::from_secs(5)).await;
    }
    a.stop();
}

/// G2: Enter to the first answer, at p95, within the connection's RTT plus this (bot-commands.spec.ts).
const G2_HEADROOM_MS: f64 = 100.0;
const G2_RUNS: usize = 20;

/// The p95 of `samples` as the web specs take it: the sorted sample at `ceil(n * 0.95) - 1`.
fn p95(samples: &mut [f64]) -> f64 {
    samples.sort_by(f64::total_cmp);
    samples[(samples.len() as f64 * 0.95).ceil() as usize - 1]
}

/// e2e/web/bot-commands.spec.ts "Enter to first answer is within RTT + 100 ms at p95": twenty `/remind` runs, each timed
/// from the send (the picker's Enter calls `run_command`) to the moment its private row first shows the Bot's answer (no
/// longer the pending copy). The RTT is this connection's socket round trip: the client keeps no heartbeat ping (the web's
/// `pingMs`), so it is the median of ten socket reads of the installation on the same connection.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
async fn enter_to_first_answer_is_within_rtt_plus_100ms_at_p95() {
    let (_host, pa, _pb, c) = two_people_with_bot("t28-g2").await;
    let a = start(&pa).await;
    let channel = general(&a, c).await;
    a.open_channel(channel)
        .await
        .unwrap()
        .expect("a opens general");
    let community = CommunityId(c);
    enable_bot(&a, c, cap::REMINDERS).await;
    let here = Conversation {
        channel,
        thread: None,
    };
    // A warm-up run: the first sealed command sets up what later ones reuse, as the spec's first list opening does.
    let row = run_bot(&a, here, community, "/remind when:10m text:warm");
    settled(&a, here, row, Duration::from_secs(90)).await;

    let mut rtts = Vec::new();
    for _ in 0..10 {
        let t = std::time::Instant::now();
        a.bot_admin()
            .refresh_installation(community)
            .await
            .unwrap()
            .unwrap();
        rtts.push(t.elapsed().as_secs_f64() * 1000.0);
    }
    rtts.sort_by(f64::total_cmp);
    let rtt = rtts[rtts.len() / 2];

    let mut samples = Vec::new();
    for run in 0..G2_RUNS {
        let mut rx = a.state();
        rx.borrow_and_update();
        let start = std::time::Instant::now();
        let row = run_bot(
            &a,
            here,
            community,
            &format!("/remind when:10m text:\"G2 probe {run}\""),
        );
        let pending = format!("{:?}", Text::Pending("remind".into()));
        let first = loop {
            let answered =
                rx.borrow_and_update().bot.results(&here).iter().any(|r| {
                    r.id == row && (r.status != ResultStatus::Pending || r.text != pending)
                });
            if answered {
                break start.elapsed();
            }
            tokio::time::timeout(Duration::from_secs(90), rx.changed())
                .await
                .expect("an answer within 90 s")
                .unwrap();
        };
        samples.push(first.as_secs_f64() * 1000.0);
        let r = settled(&a, here, row, Duration::from_secs(90)).await;
        assert_eq!(r.status, ResultStatus::Success, "{r:?}");
    }
    let shown: Vec<String> = samples.iter().map(|s| format!("{s:.0}")).collect();
    let p = p95(&mut samples);
    eprintln!(
        "G2 rtt {rtt:.1} ms, p95 {p:.1} ms, samples {}",
        shown.join(" ")
    );
    assert!(
        p <= rtt + G2_HEADROOM_MS,
        "G2: p95 {p:.1} ms over rtt {rtt:.1} ms + {G2_HEADROOM_MS} ms; samples {}",
        shown.join(" ")
    );
    a.stop();
}

/// e2e/web/bot-runtime.spec.ts "literal music command text never executes through the native runtime": with the Music
/// capability on and a member in the channel, `//music` and `//play northern lights` post their text literally; the Bot
/// runs neither (no Bot post on either device, no private row).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
async fn literal_music_command_text_never_executes() {
    let (_host, pa, pb, c) = two_people_with_bot("t28-literal").await;
    let a = start(&pa).await;
    let channel = general(&a, c).await;
    a.open_channel(channel)
        .await
        .unwrap()
        .expect("a opens general");
    enable_bot(&a, c, cap::MUSIC).await;
    let b = start(&pb).await;
    general(&b, c).await;
    b.open_channel(channel)
        .await
        .unwrap()
        .expect("b joins general");
    let here = Conversation {
        channel,
        thread: None,
    };
    let community = Some(CommunityId(c));
    for draft in ["//music", "//play northern lights"] {
        let RunOutcome::Post(text) = a.bot().run(here, community, draft) else {
            panic!("{draft} posts its text");
        };
        assert_eq!(text, &draft[1..]);
        a.send_text(channel, text).await.unwrap().expect("queued");
    }
    let want = vec!["/music".to_owned(), "/play northern lights".to_owned()];
    wait_for(&b, Duration::from_secs(30), "b to see both literals", |s| {
        bodies(s, channel) == want
    })
    .await;
    // Long enough for a Bot that had taken either as a command to answer in the channel.
    tokio::time::sleep(Duration::from_secs(5)).await;
    for (who, client) in [("a", &a), ("b", &b)] {
        let s = client.state().borrow().clone();
        let items = &s.timelines[&channel].items;
        assert!(
            items.iter().all(|i| !i.bot),
            "{who}: no Bot post: {:?}",
            bodies(&s, channel)
        );
        assert_eq!(bodies(&s, channel), want, "{who}");
        assert!(s.bot.private_results.is_empty(), "{who}: no private row");
    }
    a.stop();
    b.stop();
}

/// e2e/web/slash-commands.spec.ts "reminders sync across devices and remain due after Bot restart and reload": a
/// reminder set on one device is listed on the account's second device; across a Bot restart, and with the first device
/// gone, it falls due; the first device restarted lists it due; dismissed on the second, the first no longer lists it.
/// With the Bot stopped, a restarted device holds no answer of the sealed command and runs nothing again (the reminder
/// lives in the Bot alone).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
async fn reminders_sync_across_devices_and_stay_due_after_a_bot_restart() {
    use dilla_client::testing::host::bot::wait_for_catalog;
    use dilla_client::testing::host::second_device;
    let (host, mut pa, _pb, c) = two_people_with_bot("t28-remind2").await;
    let a = start(&pa).await;
    let channel = general(&a, c).await;
    a.open_channel(channel)
        .await
        .unwrap()
        .expect("a opens general");
    let community = CommunityId(c);
    enable_bot(&a, c, cap::REMINDERS).await;
    let here = Conversation {
        channel,
        thread: None,
    };
    let text = format!("cross-device reminder {}", std::process::id());
    let row = run_bot(
        &a,
        here,
        community,
        &format!("/remind when:1m text:\"{text}\""),
    );
    let answer = settled(&a, here, row, Duration::from_secs(90)).await;
    assert_eq!(answer.status, ResultStatus::Success, "{answer:?}");
    assert!(
        answer.text.starts_with("Saved. I’ll remind you ")
            && answer.text.ends_with(&format!(": {text}")),
        "{answer:?}"
    );

    let (a2, root2) = second_device(&host, &mut pa, "t28-remind2").await;
    wait_for_catalog(&a2, c, Duration::from_secs(90)).await;
    let listed = a2.utilities().refresh_reminders(community).await.unwrap();
    assert!(
        listed.iter().any(|r| r.text == text),
        "the second device lists it: {listed:?}"
    );

    let host = tokio::task::spawn_blocking(move || {
        host.restart_bot();
        host
    })
    .await
    .unwrap();
    a.stop();

    let deadline = std::time::Instant::now() + Duration::from_secs(180);
    let due = loop {
        let all = a2
            .utilities()
            .refresh_reminders(community)
            .await
            .unwrap_or_default();
        if let Some(r) = all
            .iter()
            .find(|r| r.text == text && r.state == ReminderStatus::Due)
        {
            break r.clone();
        }
        assert!(
            std::time::Instant::now() < deadline,
            "the reminder never fell due: {all:?}"
        );
        tokio::time::sleep(Duration::from_secs(5)).await;
    };

    let a = start(&pa).await;
    wait_for_catalog(&a, c, Duration::from_secs(90)).await;
    let first = a.utilities().refresh_reminders(community).await.unwrap();
    assert!(
        first
            .iter()
            .any(|r| r.text == text && r.state == ReminderStatus::Due),
        "the restarted first device lists it due: {first:?}"
    );
    a2.utilities()
        .settle_reminder(community, due.id, true)
        .await
        .unwrap();
    let first = a.utilities().refresh_reminders(community).await.unwrap();
    assert!(
        first.iter().all(|r| r.text != text),
        "dismissed on the second device, gone on the first: {first:?}"
    );

    // With the Bot stopped, a restarted device holds no answer of the sealed command and runs nothing again.
    let host = tokio::task::spawn_blocking(move || {
        host.stop_bot();
        host
    })
    .await
    .unwrap();
    a.stop();
    let a = start(&pa).await;
    wait_for(&a, Duration::from_secs(30), "a to sync again", |s| {
        s.channel(channel).is_some()
    })
    .await;
    tokio::time::sleep(Duration::from_secs(2)).await;
    let s = a.state().borrow().clone();
    assert!(
        s.bot.private_results.is_empty(),
        "no answer is kept across a restart"
    );
    assert!(
        bodies(&s, channel).is_empty(),
        "nothing of the command reached the channel"
    );
    a.stop();
    a2.stop();
    drop(host);
    let _ = std::fs::remove_dir_all(root2);
}
