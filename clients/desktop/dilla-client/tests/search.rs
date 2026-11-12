//! Task 23's client tests for the search area (standard procedure S2), against a real dillad (dilla-testhost).
//!
//! Needs `target/dilla-testhost` and the WASI core (see `tests/sync.rs`); the tests are `#[ignore]`d so a plain `cargo test`
//! needs no Go toolchain, and the desktop-gpui CI job runs them with `--ignored` on Linux.
//!
//! The search is local: `Search::query` asks the core's own store (scope everywhere) and publishes the hits into
//! `AppState::search`. These tests read what the second device sent, search it by a prefix without the accent, and check the
//! hit's channel, author and time, the stale-answer gate and `clear`.

use std::time::Duration;

use dilla_client::Client;
use dilla_client::state::{AppState, ChannelId, CommunityId, UserId};
use dilla_client::testing::host::{
    Host, Person, clear_session, community, sign_up, start, start_host, wait_for,
};

/// Two people in one community, signed up, set up and with their sessions cleared.
async fn two_people(tag: &str) -> (Host, Person, Person, [u8; 16]) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_host(&tag, "127.0.0.1:0", None)
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let a = sign_up(&host, &invite, "alice", "Alice", tag).await;
    let b = sign_up(&host, &invite, "bob", "Bob", tag).await;
    tokio::task::spawn_blocking(move || {
        let c = community(&host, &a, "gpui", &[&b]);
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

fn found(s: &AppState, snippet: &str) -> bool {
    s.search.hits.iter().any(|h| h.snippet == snippet)
}

/// e2e/web/search-settings.spec.ts `native edit is searched by prefix and deleted text disappears after a positive result`,
/// the part this task owns: a message the other device sent is found by a prefix of a word without its accent, the hit
/// names its channel, author and time, and a query for something absent finds nothing.
///
/// The edit and the delete themselves (the spec's `peer.edit` and `peer.deleteMessage`) need the messages area's edit and
/// delete commands (Task 9), which this branch does not have: the end-to-end half of the test, "the old text is gone from
/// the results after a delete", is on Task 28's list. The core's search (`core search.rs`) already drops a deleted row's
/// text from its index; this test checks the index follows a row's text.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn edit_found_by_prefix_deleted_text_gone() {
    let (_host, pa, pb, c) = two_people("search").await;
    let alice = start(&pa).await;
    let bob = start(&pb).await;
    let channel = general(&alice, c).await;
    general(&bob, c).await;
    alice
        .open_channel(channel)
        .await
        .unwrap()
        .expect("alice opens the channel");
    bob.open_channel(channel)
        .await
        .unwrap()
        .expect("bob joins the channel");
    bob.send_text(channel, "café lamp".into())
        .await
        .unwrap()
        .expect("queued");
    bob.send_text(channel, "lamp after".into())
        .await
        .unwrap()
        .expect("queued");
    wait_for(
        &alice,
        Duration::from_secs(20),
        "alice to hold both messages",
        |s| {
            s.timelines.get(&channel).is_some_and(|t| {
                t.items.iter().any(|i| i.body == "café lamp")
                    && t.items.iter().any(|i| i.body == "lamp after")
            })
        },
    )
    .await;

    // By a prefix, without the accent.
    let answered = alice.search().query("cafe");
    answered.await.unwrap().expect("the search answers");
    let s = wait_for(&alice, Duration::from_secs(10), "the hit", |s| {
        s.search.query == "cafe" && !s.search.loading && found(s, "café lamp")
    })
    .await;
    let hit = &s.search.hits[0];
    assert_eq!(hit.channel, channel);
    assert_eq!(hit.community, Some(CommunityId(c)));
    assert!(hit.ts.is_some(), "a readable row carries its time");
    assert_eq!(hit.thread_root, None);
    // The author is bob, by the name the members list gives.
    let bob_name = s
        .members
        .values()
        .flatten()
        .find(|m| m.user_id == UserId(pb.user_id))
        .map(|m| {
            if m.display.is_empty() {
                m.username.clone()
            } else {
                m.display.clone()
            }
        });
    assert!(bob_name.is_some(), "bob is a member");
    assert_eq!(hit.author, bob_name);

    // The newest message first, and both for the shared word.
    alice.search().query("lamp").await.unwrap().unwrap();
    let s = wait_for(&alice, Duration::from_secs(10), "both hits", |s| {
        s.search.query == "lamp" && s.search.hits.len() == 2
    })
    .await;
    assert_eq!(s.search.hits[0].snippet, "lamp after");
    assert_eq!(s.search.hits[1].snippet, "café lamp");

    // Something absent finds nothing, and says it is no longer loading.
    alice.search().query("lantern").await.unwrap().unwrap();
    let s = wait_for(&alice, Duration::from_secs(10), "no hits", |s| {
        s.search.query == "lantern" && !s.search.loading
    })
    .await;
    assert!(s.search.hits.is_empty());

    // Clearing forgets the query and its hits.
    alice.search().clear();
    let s = wait_for(&alice, Duration::from_secs(10), "cleared", |s| {
        s.search.query.is_empty()
    })
    .await;
    assert!(s.search.hits.is_empty() && !s.search.loading);
    alice.stop();
    bob.stop();
}

/// A request that a newer one overtook never publishes (the web's `searchGate`): two queries in a row leave the second's
/// hits, whichever the core answers first.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn a_newer_query_wins_over_an_older_answer() {
    let (_host, pa, pb, c) = two_people("search-gate").await;
    let alice = start(&pa).await;
    let bob = start(&pb).await;
    let channel = general(&alice, c).await;
    general(&bob, c).await;
    alice.open_channel(channel).await.unwrap().unwrap();
    bob.open_channel(channel).await.unwrap().unwrap();
    bob.send_text(channel, "one boat".into())
        .await
        .unwrap()
        .unwrap();
    bob.send_text(channel, "two oars".into())
        .await
        .unwrap()
        .unwrap();
    wait_for(&alice, Duration::from_secs(20), "both messages", |s| {
        s.timelines
            .get(&channel)
            .is_some_and(|t| t.items.iter().any(|i| i.body == "two oars"))
    })
    .await;
    let first = alice.search().query("boat");
    let second = alice.search().query("oars");
    let _ = first.await;
    second.await.unwrap().unwrap();
    let s = wait_for(&alice, Duration::from_secs(10), "the newer answer", |s| {
        s.search.query == "oars" && !s.search.loading
    })
    .await;
    assert_eq!(
        s.search
            .hits
            .iter()
            .map(|h| h.snippet.as_str())
            .collect::<Vec<_>>(),
        ["two oars"]
    );
    alice.stop();
    bob.stop();
}
