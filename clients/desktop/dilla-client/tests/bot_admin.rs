//! Task 21's client tests for the bot admin area (standard procedure S2), against a real dillad (dilla-testhost).
//!
//! Needs `target/dilla-testhost` and the WASI core, like `tests/sync.rs`; the tests are `#[ignore]`d so a plain
//! `cargo test` needs no Go toolchain (CI runs them with `--ignored`).
//!
//! What needs the Bot's own process (the sealed commands: status read 94, provider status 24, the moderation pages, the
//! music operations, a webhook post being delivered into the channel) goes through the `SealedBot` seam that Task 20's
//! transport fills at merge; those tests live in `features::bot_admin` and `features::music` with a fake Bot, and the
//! end-to-end ones are Task 28's list (`BEHAVIOUR-GATES.md`): `welcome_channel_greets_new_member`, `webhook_post_appears`,
//! `bot_enabled_reaches_ready`, `music_text_never_executes`, `moderation_log_across_bot_restart`,
//! `join_role_applied_on_join_and_rejoin`, `self_role_panel_reaction_grant`, `warning_retrievable_from_bot_settings` and the
//! six `e2e/voice/bot-music.spec.ts` tests.

use std::time::Duration;

use dilla_client::state::{ChannelId, CommunityId};
use dilla_client::testing::host::{
    Person, clear_session, community, sign_up, start, start_host, wait_for,
};

async fn owner(tag: &str) -> (dilla_client::testing::host::Host, Person, [u8; 16]) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_host(&tag, "127.0.0.1:0", None)
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let a = sign_up(&host, &invite, "alice", "alice", tag).await;
    tokio::task::spawn_blocking(move || {
        let c = community(&host, &a, "gpui", &[]);
        clear_session(&a);
        (host, a, c)
    })
    .await
    .unwrap()
}

async fn recv<T>(
    rx: tokio::sync::oneshot::Receiver<Result<T, dilla_client::client::ClientError>>,
) -> T {
    rx.await
        .expect("the client answered")
        .expect("the call succeeded")
}

/// S2: a webhook is made for a Bot channel, its address is answered once and never lands in the slice, the list holds
/// no secret, "allow everyone" and rotate and delete work, and a second rotate changes the address.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs target/dilla-testhost and the WASI core"]
async fn webhook_lifecycle_keeps_no_address() {
    let (_host, alice, c) = owner("botadmin-hooks").await;
    let client = start(&alice).await;
    let community_id = CommunityId(c);
    let state = wait_for(&client, Duration::from_secs(30), "the channel list", |s| {
        s.channel_named(community_id, "general").is_some()
    })
    .await;
    let general: ChannelId = state.channel_named(community_id, "general").unwrap().id;
    let admin = client.bot_admin();

    recv(admin.mark_bot_channel(community_id, general)).await;
    let made = recv(admin.create_webhook(community_id, general, "  CI  ".to_owned(), false)).await;
    assert!(
        made.url.starts_with("https://") && made.url.contains("/hooks/"),
        "an address"
    );
    let first_url = made.url.clone();

    let s = wait_for(&client, Duration::from_secs(10), "the webhook list", |s| {
        s.bot_admin
            .webhooks
            .get(&community_id)
            .is_some_and(|w| w.len() == 1)
    })
    .await;
    let hook = s.bot_admin.webhooks[&community_id][0].clone();
    assert_eq!(
        (hook.name.as_str(), hook.channel_id, hook.allow_everyone),
        ("CI", general, false)
    );
    // Nothing in the published state carries the secret.
    let secret = made.url.rsplit('/').next().unwrap().to_owned();
    assert!(
        !format!("{:?}", s.bot_admin).contains(&secret),
        "the slice holds no address"
    );
    assert!(
        !format!("{made:?}").contains(&secret),
        "the address is not in Debug output"
    );

    recv(admin.set_webhook_everyone(community_id, hook.webhook_id, true)).await;
    wait_for(&client, Duration::from_secs(10), "allow everyone on", |s| {
        s.bot_admin.webhooks[&community_id][0].allow_everyone
    })
    .await;

    let rotated = recv(admin.rotate_webhook(community_id, hook.webhook_id)).await;
    assert_ne!(
        *rotated.url, *first_url,
        "a new secret: the old address stops working"
    );
    wait_for(&client, Duration::from_secs(10), "the rotation", |s| {
        s.bot_admin.webhooks[&community_id][0].rotated_at.is_some()
    })
    .await;

    recv(admin.delete_webhook(community_id, hook.webhook_id)).await;
    wait_for(&client, Duration::from_secs(10), "the list to empty", |s| {
        s.bot_admin
            .webhooks
            .get(&community_id)
            .is_some_and(Vec::is_empty)
    })
    .await;
    recv(admin.unmark_bot_channel(community_id, general)).await;
    client.stop();
}

/// A name that is only blanks is refused before anything is sent.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs target/dilla-testhost and the WASI core"]
async fn a_blank_webhook_name_is_refused_locally() {
    let (_host, alice, c) = owner("botadmin-blank").await;
    let client = start(&alice).await;
    let community_id = CommunityId(c);
    let state = wait_for(&client, Duration::from_secs(30), "the channel list", |s| {
        s.channel_named(community_id, "general").is_some()
    })
    .await;
    let general = state.channel_named(community_id, "general").unwrap().id;
    let refused = client
        .bot_admin()
        .create_webhook(community_id, general, "   ".to_owned(), false)
        .await
        .unwrap();
    assert!(refused.is_err());
    client.stop();
}

/// Integration 3, final review I1 (controller.ts `markBotChannel`: the mark, then `botCatalog.refresh`): a mark or an
/// unmark that went through re-reads the community's catalog, so the Bot channels this device holds follow it at once
/// (op 54 carries no mark, and nothing else would read them again).
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs target/dilla-testhost and the WASI core"]
async fn a_mark_re_reads_the_bot_channels() {
    let (_host, alice, c) = owner("botadmin-mark").await;
    let client = start(&alice).await;
    let community_id = CommunityId(c);
    let state = wait_for(&client, Duration::from_secs(30), "the channel list", |s| {
        s.channel_named(community_id, "general").is_some()
    })
    .await;
    let general: ChannelId = state.channel_named(community_id, "general").unwrap().id;
    let listed = |s: &dilla_client::state::AppState| {
        s.bot
            .catalogs
            .get(&community_id)
            .and_then(|c| c.bot_channels.as_ref())
            .is_some_and(|ids| ids.contains(&general))
    };
    recv(client.bot_admin().mark_bot_channel(community_id, general)).await;
    wait_for(
        &client,
        Duration::from_secs(10),
        "general among the Bot channels",
        |s| listed(s),
    )
    .await;
    recv(client.bot_admin().unmark_bot_channel(community_id, general)).await;
    wait_for(
        &client,
        Duration::from_secs(10),
        "general no longer a Bot channel",
        |s| !listed(s),
    )
    .await;
    client.stop();
}
