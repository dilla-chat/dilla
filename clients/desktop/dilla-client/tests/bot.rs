//! Task 20's client tests for the bot area (standard procedure S2), against a real dillad.
//!
//! Needs `target/dilla-testhost` and the WASI core, like `tests/sync.rs`; `#[ignore]`d so a plain `cargo test` needs no
//! Go toolchain. The pure logic (the slash parser, private results, reminder times, poll payloads) is unit-tested in
//! `src/features/{bot,utilities}/`.

use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::bot::{RunOutcome, not_a_command};
use dilla_client::state::bot::{Conversation, PRIVATE_RESULT_TTL_MS, ResultProvider, ResultStatus};
use dilla_client::state::{AppState, ChannelId, CommunityId};
use dilla_client::testing::host::{
    Host, Person, clear_session, community, sign_up, start, start_host, wait_for,
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
