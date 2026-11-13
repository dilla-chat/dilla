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
use dilla_client::testing::host::bot::enable_bot;
use dilla_client::testing::host::{
    Host, Person, clear_session, community, sign_up, start, start_bot_host, start_host, wait_for,
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

/// [`owner`] on a host that runs the Bot (Task 28, `testing::host::bot`).
async fn bot_owner(tag: &str) -> (Host, Person, [u8; 16]) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_bot_host(&tag, "127.0.0.1:0")
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

/// e2e/web/bot-runtime.spec.ts "a bot enabled through server settings reaches ready through the native runtime": no
/// installation at first; the owner enables the Bot (an empty capability mask) behind a password step-up; the operator
/// runtime lists the one installation ready (state 3), and the owner's read says ready ("admin.bot.status.ready").
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
async fn a_bot_enabled_in_server_settings_reaches_ready() {
    let (host, pa, c) = bot_owner("t28-ready").await;
    let a = start(&pa).await;
    let community = CommunityId(c);
    wait_for(&a, Duration::from_secs(30), "the community", |s| {
        s.members.contains_key(&community)
    })
    .await;
    assert_eq!(
        recv(a.bot_admin().refresh_installation(community)).await,
        None,
        "no Bot before it is enabled (admin.bot.none)"
    );
    let installation = enable_bot(&a, c, 0).await;
    assert_eq!((installation.state, installation.capabilities), (3, 0));
    let (host, listed) = tokio::task::spawn_blocking(move || {
        let rows = host.bot_installations();
        (host, rows)
    })
    .await
    .unwrap();
    assert_eq!(
        listed,
        vec![(c, 3)],
        "the runtime lists one ready installation"
    );
    let s = a.state().borrow().clone();
    assert_eq!(
        s.bot_admin.installation.get(&community).map(|i| i.state),
        Some(3),
        "the settings page reads ready"
    );
    a.stop();
    drop(host);
}

/// An invite code to `community`, made by `owner` (the invite link's code).
fn invite_code(host: &Host, owner: &Person, community: [u8; 16]) -> String {
    use dilla_core::ids::DeviceId;
    let mut ds = dilla_testkit::HttpDs::with_session(
        &host.public,
        DeviceId::from_bytes(owner.device_id),
        owner.token.clone(),
    )
    .unwrap();
    ds.create_invite(&community, 64, 3600).unwrap()
}

/// How many members of `community` hold `role`, as the owner's Roles page counts them.
fn holders(s: &dilla_client::state::AppState, community: CommunityId, role: [u8; 16]) -> usize {
    s.members.get(&community).map_or(0, |m| {
        m.iter().filter(|m| m.role_ids.contains(&role)).count()
    })
}

/// e2e/web/bot-runtime.spec.ts "a Roles-enabled bot applies the configured join role on join and rejoin": the owner makes
/// "Arrival Crew" (no members), enables the Bot with the Roles capability, sets it as the join role (read back the same);
/// Bea joins and the role counts one member; she leaves (zero) and joins again, and the Bot gives it again.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "needs dillad with its Bot: target/dilla-testhost, target/dillabot, dilla-bot-core and the wasi core"]
async fn a_roles_bot_applies_the_join_role_on_join_and_rejoin() {
    use dilla_client::testing::host::bot::cap;
    let (host, pa, c) = bot_owner("t28-joinrole").await;
    let invite = host.invites.last().unwrap().clone();
    let pb = sign_up(&host, &invite, "bea", "Bea", "t28-joinrole").await;
    clear_session(&pb);
    let a = start(&pa).await;
    let community = CommunityId(c);
    wait_for(&a, Duration::from_secs(30), "the community", |s| {
        s.members.contains_key(&community)
    })
    .await;
    let role = a
        .roles()
        .create_role(community, "Arrival Crew", 0, false, false, 0)
        .await
        .expect("the role");
    let s = wait_for(&a, Duration::from_secs(10), "the role listed", |s| {
        s.roles
            .roles
            .get(&community)
            .is_some_and(|r| r.iter().any(|r| r.id == role))
    })
    .await;
    assert_eq!(holders(&s, community, role), 0);
    let installation = enable_bot(&a, c, cap::ROLES).await;
    assert_eq!(
        a.roles().bot_join_role(community).await.unwrap(),
        Some((None, installation.id)),
        "no join role yet"
    );
    let saved = a
        .roles()
        .configure_bot_join_role(community, Some(role), installation.id)
        .await
        .expect("the join role is saved");
    assert_eq!(saved, installation.id);
    assert_eq!(
        a.roles().bot_join_role(community).await.unwrap(),
        Some((Some(role), installation.id)),
        "read back"
    );

    let code = invite_code(&host, &pa, c);
    let b = start(&pb).await;
    b.onboarding().join(&code).await.expect("bea joins");
    let bea = dilla_client::state::UserId(pb.user_id);
    wait_for(
        &a,
        Duration::from_secs(90),
        "the Bot gives bea the join role",
        |s| {
            s.members.get(&community).is_some_and(|m| {
                m.iter()
                    .any(|m| m.user_id == bea && m.role_ids.contains(&role))
            })
        },
    )
    .await;

    b.server_admin()
        .leave(community)
        .wait()
        .await
        .expect("bea leaves");
    wait_for(&a, Duration::from_secs(30), "bea gone", |s| {
        s.members
            .get(&community)
            .is_some_and(|m| m.iter().all(|m| m.user_id != bea))
            && holders(s, community, role) == 0
    })
    .await;

    b.onboarding().join(&code).await.expect("bea joins again");
    wait_for(
        &a,
        Duration::from_secs(90),
        "the join role again on rejoin",
        |s| {
            s.members.get(&community).is_some_and(|m| {
                m.iter()
                    .any(|m| m.user_id == bea && m.role_ids.contains(&role))
            })
        },
    )
    .await;
    a.stop();
    b.stop();
    drop(host);
}
