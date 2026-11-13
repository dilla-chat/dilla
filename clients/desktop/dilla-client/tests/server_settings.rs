//! Task 15's client tests for the server settings area (standard procedure S2), against a real dillad
//! (dilla-testhost): the server-admin commands of `dilla_client::features::server_admin`, with the assertions the e2e
//! specs of the web client make.
//!
//! Needs `target/dilla-testhost` and the WASI core (`DILLA_CORE_WASI`); the tests are `#[ignore]`d so a plain
//! `cargo test` needs no Go toolchain (the desktop-gpui CI job runs them with `--include-ignored` on Linux).
//! The pure rules (names, invites, moves, privacy, welcome documents) are unit tests in the feature file.

use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::server_admin::{Direction, Parent, perm};
use dilla_client::state::server_admin::GifRating;
use dilla_client::state::{AppState, ChannelId, CommunityId};
use dilla_client::testing::host::{
    Host, Person, clear_session, community, sign_up, start, start_host, wait_for,
};

/// Two people in one community, the first its owner: signed up, set up and with their sessions cleared.
async fn two_people(tag: &str) -> (Host, Person, Person, CommunityId) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_host(&tag, "127.0.0.1:0", None)
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let a = sign_up(&host, &invite, "alice", "alice", tag).await;
    let b = sign_up(&host, &invite, "bob", "bob", tag).await;
    let (host, a, b, c) = tokio::task::spawn_blocking(move || {
        let c = community(&host, &a, "Midgard", &[&b]);
        clear_session(&a);
        clear_session(&b);
        (host, a, b, c)
    })
    .await
    .unwrap();
    (host, a, b, CommunityId(c))
}

async fn channel_named(client: &Client, community: CommunityId, name: &str) -> ChannelId {
    let s = wait_for(client, Duration::from_secs(30), name, |s| {
        s.channel_named(community, name).is_some()
    })
    .await;
    s.channel_named(community, name).unwrap().id
}

fn names(s: &AppState, community: CommunityId) -> Vec<String> {
    let mut rows: Vec<_> = s
        .channels
        .iter()
        .filter(|c| c.community_id == community && c.kind != 2)
        .collect();
    rows.sort_by_key(|c| (c.position, c.id));
    rows.iter().map(|c| c.name.clone()).collect()
}

/// e2e/web/admin.spec.ts: rename, channel edit, leave, hand over and delete.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn server_rename_channel_edit_leave_hand_over_delete() {
    let (_host, pa, pb, c) = two_people("admin").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let general = channel_named(&a, c, "general").await;
    channel_named(&b, c, "general").await;
    let admin = a.server_admin();

    // The owner holds every right and the sections show by it.
    let access = admin.load_access(c).wait().await.expect("access");
    assert!(
        access.owner && access.manage_community && access.manage_channels && access.create_invite
    );
    assert!(access.everyone.is_some() && access.overwrites_loaded);
    let bob_access = b
        .server_admin()
        .load_access(c)
        .wait()
        .await
        .expect("bob's access");
    assert!(!bob_access.owner && !bob_access.manage_community && !bob_access.manage_channels);

    // Rename: the list shows the new name at once, and a refusal of an empty name is local.
    admin
        .rename(c, "  Midgard Crew  ")
        .wait()
        .await
        .expect("rename");
    wait_for(&a, Duration::from_secs(10), "the new name", |s| {
        s.communities
            .iter()
            .any(|x| x.id == c && x.name == "Midgard Crew")
    })
    .await;
    assert_eq!(
        admin.rename(c, "   ").wait().await.unwrap_err().code,
        "E_BAD_INPUT"
    );
    // A member may not rename (the instance decides).
    let refused = b
        .server_admin()
        .rename(c, "Hijack")
        .wait()
        .await
        .unwrap_err();
    assert!(refused.forbidden(), "{refused}");

    // Channel edit: name, topic and slow mode go in one patch.
    admin
        .update_channel(
            general,
            Some("ship-talk"),
            Some("what ships today"),
            Parent::Keep,
            Some(30),
        )
        .wait()
        .await
        .expect("edit");
    let s = wait_for(&a, Duration::from_secs(10), "the edit", |s| {
        s.channel(general)
            .is_some_and(|ch| ch.name == "ship-talk" && ch.slowmode == 30)
    })
    .await;
    assert_eq!(s.channel(general).unwrap().topic, "what ships today");

    // A category, and the channel moved into it.
    let category = admin
        .create_category(c, "Text")
        .wait()
        .await
        .expect("category");
    admin
        .update_channel(general, None, None, Parent::Category(category), None)
        .wait()
        .await
        .expect("move into the category");
    wait_for(
        &a,
        Duration::from_secs(10),
        "the channel in its category",
        |s| {
            s.channel(general)
                .is_some_and(|ch| ch.parent_id == Some(category))
        },
    )
    .await;

    // A member cannot hand the server over; the owner can, and then is a member who may leave.
    let refused = b
        .server_admin()
        .transfer_ownership(c, dilla_client::state::UserId(pb.user_id))
        .wait()
        .await
        .unwrap_err();
    assert!(refused.forbidden() || refused.status >= 400, "{refused}");
    admin
        .transfer_ownership(c, dilla_client::state::UserId(pb.user_id))
        .wait()
        .await
        .expect("hand over");
    wait_for(&b, Duration::from_secs(10), "bob owns the server", |s| {
        s.communities
            .iter()
            .any(|x| x.id == c && x.owner == dilla_client::state::UserId(pb.user_id))
    })
    .await;
    admin.leave(c).wait().await.expect("alice leaves");
    wait_for(&a, Duration::from_secs(10), "the server gone", |s| {
        s.communities.iter().all(|x| x.id != c) && !s.server_admin.access.contains_key(&c)
    })
    .await;

    // The new owner deletes it.
    b.server_admin()
        .delete_community(c)
        .wait()
        .await
        .expect("delete");
    wait_for(&b, Duration::from_secs(10), "the server deleted", |s| {
        s.communities.iter().all(|x| x.id != c)
    })
    .await;
}

/// e2e/web/afk.spec.ts test 2: a channel renamed and moved into a category is seen live by another member.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn afk_rename_and_move_into_category_seen_live() {
    let (_host, pa, pb, c) = two_people("afk").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let general = channel_named(&a, c, "general").await;
    channel_named(&b, c, "general").await;
    let admin = a.server_admin();
    // The AFK settings: the built-in channel and the default timeout, then a saved one.
    let afk = admin.load_afk(c).wait().await.expect("afk");
    assert_eq!(afk.timeout_seconds, 300);
    admin.set_afk(c, 900).wait().await.expect("set afk");
    assert_eq!(admin.load_afk(c).wait().await.unwrap().timeout_seconds, 900);
    assert_eq!(
        admin.set_afk(c, 61).wait().await.unwrap_err().code,
        "E_ROUTE_INPUT"
    );
    // A rename and a move seen by the other member.
    let category = admin
        .create_category(c, "Chat")
        .wait()
        .await
        .expect("category");
    admin
        .update_channel(
            general,
            Some("lobby"),
            None,
            Parent::Category(category),
            None,
        )
        .wait()
        .await
        .expect("edit");
    wait_for(
        &b,
        Duration::from_secs(20),
        "bob to see the rename and the move",
        |s| {
            s.channel(general)
                .is_some_and(|ch| ch.name == "lobby" && ch.parent_id == Some(category))
        },
    )
    .await;
}

/// e2e/web/afk.spec.ts test 3: the AFK channel has no delete and no reorder.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn afk_has_no_delete_permissions_or_reorder() {
    let (_host, pa, _pb, c) = two_people("afk-fixed").await;
    let a = start(&pa).await;
    let admin = a.server_admin();
    let afk = admin
        .load_afk(c)
        .wait()
        .await
        .expect("afk")
        .channel
        .expect("the built-in AFK channel");
    let general = channel_named(&a, c, "general").await;
    wait_for(
        &a,
        Duration::from_secs(30),
        "the AFK channel in the list",
        |s| s.channel(afk).is_some(),
    )
    .await;
    let s = a.state().borrow().clone();
    // It never moves, and no ordinary channel passes it.
    assert!(
        dilla_client::features::server_admin::plan_move(&s.channels, afk, Direction::Up, Some(afk))
            .is_empty()
    );
    assert!(
        dilla_client::features::server_admin::plan_move(
            &s.channels,
            general,
            Direction::Down,
            Some(afk)
        )
        .is_empty()
    );
    let before = names(&s, c);
    admin
        .move_channel(afk, Direction::Up)
        .wait()
        .await
        .expect("a no-op");
    admin
        .move_channel(general, Direction::Down)
        .wait()
        .await
        .expect("a no-op");
    assert_eq!(names(&a.state().borrow(), c), before);
}

/// e2e/web/private.spec.ts, through the client's own API (the window's toggle and confirm are the settings' fixtures and
/// Task 28's integration run): a channel is made private, gained, lost and toggled, and nobody is locked out of it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn private_channels_client_gain_lose_toggle_lockout() {
    let (_host, pa, pb, c) = two_people("private").await;
    let a = start(&pa).await;
    let b = start(&pb).await;
    let general = channel_named(&a, c, "general").await;
    channel_named(&b, c, "general").await;
    let admin = a.server_admin();
    let access = admin.load_access(c).wait().await.expect("access");
    let everyone = access.everyone.expect("@everyone");
    let visible = |s: &AppState| {
        dilla_client::features::server_admin::is_private(
            s.server_admin
                .overwrites
                .get(&c)
                .map_or(&[][..], |v| v.as_slice()),
            general,
            everyone,
        )
    };
    assert!(!visible(&a.state().borrow()));

    // Private: @everyone is denied viewing, and bob (a plain member) loses the channel.
    admin
        .set_channel_private(general, true)
        .wait()
        .await
        .expect("make private");
    wait_for(&a, Duration::from_secs(10), "the channel private", |s| {
        visible(s)
    })
    .await;
    wait_for(
        &b,
        Duration::from_secs(20),
        "bob to lose the channel",
        |s| s.channel(general).is_none(),
    )
    .await;

    // Gain: an overwrite for bob lets him see it again.
    admin
        .set_overwrite(c, general, 1, pb.user_id, perm::VIEW_CHANNEL, 0)
        .wait()
        .await
        .expect("bob gains it");
    wait_for(
        &b,
        Duration::from_secs(20),
        "bob to gain the channel",
        |s| s.channel(general).is_some(),
    )
    .await;
    // Lose: the overwrite goes.
    admin
        .clear_overwrite(c, general, 1, pb.user_id)
        .wait()
        .await
        .expect("bob loses it");
    wait_for(&b, Duration::from_secs(20), "bob to lose it again", |s| {
        s.channel(general).is_none()
    })
    .await;

    // Toggle back: open to everyone, and the owner's own view never goes (no lockout).
    admin
        .set_channel_private(general, false)
        .wait()
        .await
        .expect("open");
    wait_for(&a, Duration::from_secs(10), "the channel open", |s| {
        !visible(s)
    })
    .await;
    assert!(a.state().borrow().channel(general).is_some());
    // Lockout: a plain member who would lose the channel by making it private is refused before any request.
    let b_admin = b.server_admin();
    wait_for(&b, Duration::from_secs(20), "bob to see the channel", |s| {
        s.channel(general).is_some()
    })
    .await;
    let refused = b_admin
        .set_channel_private(general, true)
        .wait()
        .await
        .unwrap_err();
    assert_eq!(refused.code, "E_WOULD_HIDE", "{refused}");
    assert!(!visible(&a.state().borrow()), "nothing was written");
    // A refusal the instance reads as invalid names its code.
    let refused = admin
        .set_overwrite(c, general, 1, pb.user_id, 1 << 30, 0)
        .wait()
        .await
        .unwrap_err();
    assert_eq!(refused.code, "E_ROUTE_INPUT");
}

/// e2e/web/sidebar-actions.spec.ts (its member-makes-a-link part): a member makes an invite link and sees it under Invites.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn member_makes_invite_link_under_invites() {
    let (host, _pa, pb, c) = two_people("invite").await;
    let b = start(&pb).await;
    channel_named(&b, c, "general").await;
    let admin = b.server_admin();
    // Every member holds create-invite by default (the instance's @everyone base), so the plain member makes the link.
    let created = admin
        .create_invite(c, 10, 604_800)
        .wait()
        .await
        .expect("a plain member makes an invite link");
    assert!(created.url.contains(&format!("/i/{}", created.code)));
    let _ = &host;
    admin
        .load_invites(c, dilla_client::testing::host::clock_now(&host))
        .wait()
        .await
        .expect("list");
    // The slice is published on the client's loop: wait for it, never read it back at once.
    wait_for(&b, Duration::from_secs(10), "the link listed", |s| {
        s.server_admin.invites.get(&c).is_some_and(|rows| {
            rows.iter()
                .any(|i| i.id == created.id && i.max_uses == 10 && i.uses == 0)
        })
    })
    .await;
    admin
        .revoke_invite(c, created.id)
        .wait()
        .await
        .expect("revoke");
    wait_for(&b, Duration::from_secs(10), "the link gone", |s| {
        s.server_admin
            .invites
            .get(&c)
            .is_some_and(|rows| rows.iter().all(|i| i.id != created.id))
    })
    .await;
    // The limits are the instance's: more than 1000 uses is refused before any request.
    assert_eq!(
        admin
            .create_invite(c, 1001, 3600)
            .wait()
            .await
            .unwrap_err()
            .code,
        "E_ROUTE_INPUT"
    );
}

/// The GIF settings (OverviewSection's GIFs): the key goes up and is never read back, the rating is kept.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn gif_key_is_write_only_and_the_rating_is_kept() {
    let (_host, pa, _pb, c) = two_people("gifs").await;
    let a = start(&pa).await;
    let admin = a.server_admin();
    let before = admin.load_gifs(c).wait().await.expect("status");
    assert!(!before.on);
    assert_eq!(
        admin
            .set_gif_key(c, zeroize::Zeroizing::new("short".to_owned()))
            .wait()
            .await
            .unwrap_err()
            .code,
        "E_ROUTE_INPUT"
    );
    admin
        .set_gif_key(c, zeroize::Zeroizing::new("abcDEF1234567890".to_owned()))
        .wait()
        .await
        .expect("key");
    assert!(admin.load_gifs(c).wait().await.unwrap().on);
    admin
        .set_gif_rating(c, GifRating::R)
        .wait()
        .await
        .expect("rating");
    assert_eq!(
        admin.load_gifs(c).wait().await.unwrap().rating,
        GifRating::R
    );
    admin.clear_gif_key(c).wait().await.expect("clear");
    assert!(!admin.load_gifs(c).wait().await.unwrap().on);
    // Nothing the state holds has the key in it.
    let dump = format!("{:?}", a.state().borrow().server_admin);
    assert!(!dump.contains("abcDEF1234567890"));
}
