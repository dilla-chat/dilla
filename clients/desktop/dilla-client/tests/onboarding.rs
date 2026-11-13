//! Task 25's client tests for the onboarding area (standard procedure S2), against a real dillad: sign-up
//! (`features::signup::Signup`), enrolling a second device (`features::enrol::Enrol`) and the running client's create
//! and join flows (`Client::onboarding()`). The e2e specs they port are named on each test; what a spec does in another
//! area (DMs, device removal, the Bot) is left to that area and to Task 28.
//!
//! Needs `target/dilla-testhost` and the WASI core (see `dilla_client::testing::host`).

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use dilla_client::Client;
use dilla_client::features::enrol::{Enrol, check_key};
use dilla_client::features::signup::{
    InstanceInfo, NewChannelKind, RegistrationMode, Resumed, Signup, SignupError, channel_name,
};
use dilla_client::http::routes::{self as r, Raw};
use dilla_client::http::{AuthToken, ReqwestHttp};
use dilla_client::instances::Instances;
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::{Http, InstanceConfig};
use dilla_client::signin::{Owed, SignInError};
use dilla_client::state::{
    AccountPhase, AppState, ChannelId, CommunityId, TimelineItemState, UserId,
};
use dilla_client::store::open_store;
use dilla_client::testing::host::{self, Host, PASSWORD, Person};
use dilla_core::cbor::{Encoder, decode_strict};
use zeroize::Zeroizing;

const IGNORE: &str = "needs dillad";
const WAIT: Duration = Duration::from_secs(60);

/// A data root of its own under `target/`, removed when dropped.
struct Root(PathBuf);

impl Drop for Root {
    fn drop(&mut self) {
        let _ = std::fs::remove_dir_all(&self.0);
    }
}

/// A new device of no account yet: its instance config and keychain.
fn new_device(host: &Host, tag: &str) -> (InstanceConfig, Arc<MemoryKeychain>, Root) {
    let root = host::repo_root().join(format!("target/t25-{tag}-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&root);
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances
        .add(url::Url::parse(&host.public).unwrap())
        .unwrap();
    (cfg, Arc::new(MemoryKeychain::new()), Root(root))
}

fn cbor(f: impl FnOnce(&mut Encoder)) -> Vec<u8> {
    let mut e = Encoder::new();
    f(&mut e);
    e.into_vec()
}

/// The session the sign-up stored, read back from the store (the factor screens use the running client's).
fn stored_token(cfg: &InstanceConfig, kc: &MemoryKeychain) -> Zeroizing<String> {
    let core = open_store(cfg, kc).unwrap();
    let raw = core.session().unwrap();
    Zeroizing::new(
        decode_strict(&raw, |d| {
            let len = d.array_len()?;
            let t = d.text()?.to_owned();
            for _ in 1..len {
                d.skip()?;
            }
            Ok(t)
        })
        .unwrap(),
    )
}

/// What the factor screens do for a new account (Task 26): an authenticator app, confirmed with the host's clock.
/// Answers its secret.
async fn pay_factor(host: &Host, cfg: &InstanceConfig, token: &str) -> String {
    let authed = ReqwestHttp::new(cfg.server.clone(), AuthToken::new(token)).unwrap();
    let Raw(enrol) = authed
        .call::<r::TotpEnroll>(
            &[],
            Raw(cbor(|e| {
                e.array(1).null();
            })),
        )
        .await
        .expect("totp enroll");
    let secret = decode_strict(&enrol, |d| {
        let len = d.array_len()?;
        let s = d.text()?.to_owned();
        for _ in 1..len {
            d.skip()?;
        }
        Ok(s)
    })
    .unwrap();
    let now = host_now(host).await;
    let code = host::totp_code(&secret, now / 30);
    authed
        .call::<r::TotpConfirm>(
            &[],
            Raw(cbor(|e| {
                e.array(1).text(&code);
            })),
        )
        .await
        .expect("totp confirm");
    secret
}

async fn host_now(host: &Host) -> u64 {
    let control = host.control.clone();
    tokio::task::spawn_blocking(move || {
        let body = std::thread::spawn(move || {
            let rt = tokio::runtime::Runtime::new().unwrap();
            rt.block_on(async {
                reqwest::get(format!("{control}/debug/clock"))
                    .await
                    .unwrap()
                    .bytes()
                    .await
                    .unwrap()
                    .to_vec()
            })
        })
        .join()
        .unwrap();
        serde_json::from_slice::<serde_json::Value>(&body).unwrap()["now"]
            .as_u64()
            .unwrap()
    })
    .await
    .unwrap()
}

async fn start_cfg(cfg: &InstanceConfig, kc: &Arc<MemoryKeychain>) -> Client {
    let (cfg, kc) = (cfg.clone(), kc.clone());
    tokio::task::spawn_blocking(move || Client::start(cfg, kc).expect("client starts"))
        .await
        .unwrap()
}

async fn stop(client: Client) {
    tokio::task::spawn_blocking(move || client.stop())
        .await
        .unwrap();
}

fn community_listed(s: &AppState, id: CommunityId) -> bool {
    s.communities.iter().any(|c| c.id == id)
}

async fn listed_channel(
    client: &Client,
    community: CommunityId,
    name: &str,
    kind: u8,
) -> ChannelId {
    client.refresh();
    let s = host::wait_for(client, WAIT, &format!("{name} listed"), |s| {
        s.channels
            .iter()
            .any(|c| c.community_id == community && c.name == name && c.kind == kind)
    })
    .await;
    s.channels
        .iter()
        .find(|c| c.community_id == community && c.name == name && c.kind == kind)
        .unwrap()
        .id
}

async fn reads(client: &Client, channel: ChannelId, from: UserId, body: &str) {
    host::wait_for(client, WAIT, &format!("`{body}` read"), |s| {
        s.timelines.get(&channel).is_some_and(|t| {
            t.items.iter().any(|i| {
                i.body == body && i.sender_user == Some(from) && i.state == TimelineItemState::Ok
            })
        })
    })
    .await;
}

/// Sends `body`. Right after a device joins a group, a send can meet its leaf before this device's copy of the device
/// list names it (`E_DEVICE_UNLISTED`); the engine refreshes the list on that verdict, and the send is tried again
/// (the web's outbox does the same by itself: reported to Task 9/28).
async fn send(client: &Client, channel: ChannelId, body: &str) {
    let deadline = std::time::Instant::now() + WAIT;
    loop {
        match client.send_text(channel, body.to_owned()).await.unwrap() {
            Ok(_) => return,
            Err(dilla_client::ClientError::Sync(code))
                if code == "E_DEVICE_UNLISTED" && std::time::Instant::now() < deadline =>
            {
                tokio::time::sleep(Duration::from_millis(500)).await;
            }
            Err(e) => panic!("sent: {e:?}"),
        }
    }
}

async fn open(client: &Client, channel: ChannelId) {
    client.open_channel(channel).await.unwrap().expect("opened");
}

/// An owner with a running client, a server with #general, and an invite to it made by the create flow.
async fn owner_with_server(host: &Host, tag: &str) -> (Person, Client, CommunityId, String) {
    let invite = host.invites.last().unwrap().clone();
    let owner = host::sign_up(host, &invite, "owner", "Owner", tag).await;
    host::clear_session(&owner);
    let client = host::start(&owner).await;
    host::wait_for(&client, WAIT, "the owner ready", |s| {
        s.account.phase == AccountPhase::Ready
    })
    .await;
    let community = client
        .onboarding()
        .create_server("Midgard")
        .await
        .expect("server");
    client.refresh();
    host::wait_for(&client, WAIT, "the server listed", |s| {
        community_listed(s, community)
    })
    .await;
    client
        .onboarding()
        .create_channel(community, NewChannelKind::Text, "general", None, &[])
        .await
        .expect("#general");
    let made = client
        .onboarding()
        .create_invite(community)
        .await
        .expect("invite");
    assert!(
        made.url.ends_with(&format!("/i/{}", made.code)),
        "{}",
        made.url
    );
    (owner, client, community, made.url)
}

/// `e2e/web/signup.spec.ts` "a server invite, a name and a written-down key make an account that lands in that server":
/// the instance is read, the keys made (a 52-character key in 13 groups), the account created with a server invite,
/// the factor paid, and the invite's server joined after it; the new account reads the owner there.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad"]
async fn invite_name_written_key_lands_in_server() {
    let _ = IGNORE;
    let host = tokio::task::spawn_blocking(|| host::start_host("t25-signup", "127.0.0.1:0", None))
        .await
        .unwrap();
    let (owner, owner_client, community, link) = owner_with_server(&host, "t25-signup").await;

    let (cfg, kc, _root) = new_device(&host, "signup-new");
    let mut signup = Signup::new(cfg.clone(), kc.clone()).unwrap();
    let info: InstanceInfo = signup.instance().await.expect("instance");
    assert_eq!(info.registration_mode, RegistrationMode::InviteOnly);
    assert!(info.password_signup);
    // A bad invite is refused before anything is created; the shown key is dropped and the next begin makes another.
    let first = signup.begin().await.expect("keys");
    assert_eq!(first.groups().count(), 13);
    assert_eq!(
        signup
            .submit("not-an-invite", "newbie", Some(PASSWORD))
            .await,
        Err(SignupError::InviteInvalid)
    );
    signup.reset().expect("reset");
    let key = signup.begin().await.expect("keys again");
    assert_ne!(
        key.hyphenated().as_str(),
        first.hyphenated().as_str(),
        "a shown key is never shown again"
    );
    // The link as pasted: the screen extracts the code.
    let code = dilla_client::features::signup::extract_invite_code(&link);
    // A taken username: nothing is created.
    assert_eq!(
        signup.submit(&code, "owner", Some(PASSWORD)).await,
        Err(SignupError::UsernameTaken)
    );
    signup.reset().expect("reset");
    let key = signup.begin().await.expect("keys a third time");
    let written = key.hyphenated();
    let done = signup
        .submit(&code, " NewBie ", Some(PASSWORD))
        .await
        .expect("account");
    assert_eq!(done.username, "newbie");
    assert_eq!(done.pending_join, Some(community));
    drop(signup);

    // The account owes its factor: the client starts in FactorSetup and joins nothing yet.
    let client = start_cfg(&cfg, &kc).await;
    host::wait_for(&client, WAIT, "factor setup", |s| {
        s.account.phase == AccountPhase::FactorSetup
    })
    .await;
    let token = stored_token(&cfg, &kc);
    let _secret = pay_factor(&host, &cfg, &token).await;
    client.factor_completed();
    host::wait_for(&client, WAIT, "ready after the factor", |s| {
        s.account.phase == AccountPhase::Ready
    })
    .await;
    let result = client.onboarding().finish_signup().await;
    assert_eq!(result.community, Some(community), "{:?}", result.join_error);
    // Spent once tried: a second call joins nothing.
    assert_eq!(client.onboarding().finish_signup().await.community, None);
    client.refresh();
    host::wait_for(&client, WAIT, "the server listed", |s| {
        community_listed(s, community)
    })
    .await;
    let general = listed_channel(&client, community, "general", 0).await;
    open(&client, general).await;
    let owner_general = listed_channel(&owner_client, community, "general", 0).await;
    open(&owner_client, owner_general).await;
    send(&owner_client, owner_general, "welcome aboard").await;
    reads(&client, general, UserId(owner.user_id), "welcome aboard").await;

    // A resume of the finished sign-up is Done, with nothing pending.
    stop(client).await;
    let mut again = Signup::new(cfg.clone(), kc.clone()).unwrap();
    assert!(matches!(again.resume().await, Ok(Resumed::Done(s)) if s.pending_join.is_none()));
    drop(again);
    // The written key opens the account on a new device.
    let (cfg2, kc2, _root2) = new_device(&host, "signup-second");
    let mut enrol = Enrol::new(cfg2, kc2).unwrap();
    assert_eq!(enrol.login("newbie", PASSWORD).await, Ok(Owed::Code));
    let code = host::totp_code(&_secret, host_now(&host).await / 30 + 1);
    advance(&host, 30).await;
    enrol.code(&code).await.expect("code");
    enrol
        .add_device(&written)
        .await
        .expect("the written key opens the account");
    stop(owner_client).await;
}

/// Fix round 1 (item 5): once the account POST is answered, a later failure (here an undecodable answer) is Stalled;
/// the keys are never reset and no second POST is sent (a second submit is refused before it), so the account the
/// first POST made stays the only one.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad"]
async fn an_answered_post_is_never_sent_again_nor_reset() {
    let host =
        tokio::task::spawn_blocking(|| host::start_host("t25-answered", "127.0.0.1:0", None))
            .await
            .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let (cfg, kc, _root) = new_device(&host, "answered");
    let mut signup = Signup::new(cfg.clone(), kc.clone()).unwrap();
    signup.begin().await.expect("keys");
    signup.fail_next_account_decode();
    assert_eq!(
        signup.submit(&invite, "garbled", Some(PASSWORD)).await,
        Err(SignupError::Stalled)
    );
    assert_eq!(
        signup.reset(),
        Err(SignupError::Stalled),
        "the keys are kept"
    );
    assert_eq!(
        signup.submit(&invite, "garbled", Some(PASSWORD)).await,
        Err(SignupError::Stalled),
        "never a second POST"
    );
    assert_eq!(signup.resume().await, Err(SignupError::Stalled));
    // The keys are still pending in the store.
    let phase = {
        let core = open_store(&cfg, kc.as_ref()).unwrap();
        decode_strict(&core.identity().unwrap(), |d| {
            let len = d.array_len()?;
            let p = d.uint()?;
            for _ in 1..len {
                d.skip()?;
            }
            Ok(p)
        })
        .unwrap()
    };
    assert_eq!(phase, 1);
    drop(signup);
    // The first POST made the account: the name is taken.
    let (cfg2, kc2, _root2) = new_device(&host, "answered-2");
    let mut other = Signup::new(cfg2, kc2).unwrap();
    other.begin().await.expect("keys");
    assert_eq!(
        other.submit(&invite, "garbled", Some(PASSWORD)).await,
        Err(SignupError::UsernameTaken)
    );
}

/// Fix round 1 (item 2): a client that starts in FactorSetup after a restart still finds the sign-up's kept join,
/// and joins it once the factor is in place.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad"]
async fn the_pending_join_survives_a_restart_at_the_factor() {
    let host = tokio::task::spawn_blocking(|| host::start_host("t25-restart", "127.0.0.1:0", None))
        .await
        .unwrap();
    let (_owner, owner_client, community, link) = owner_with_server(&host, "t25-restart").await;
    let (cfg, kc, _root) = new_device(&host, "restart");
    let mut signup = Signup::new(cfg.clone(), kc.clone()).unwrap();
    signup.begin().await.expect("keys");
    let code = dilla_client::features::signup::extract_invite_code(&link);
    signup
        .submit(&code, "later", Some(PASSWORD))
        .await
        .expect("account");
    drop(signup);
    let first = start_cfg(&cfg, &kc).await;
    host::wait_for(&first, WAIT, "factor setup", |s| {
        s.account.phase == AccountPhase::FactorSetup
    })
    .await;
    assert!(first.onboarding().has_pending_join().await);
    stop(first).await;
    // The app closed at the factor step and opened again.
    let client = start_cfg(&cfg, &kc).await;
    host::wait_for(&client, WAIT, "factor setup again", |s| {
        s.account.phase == AccountPhase::FactorSetup
    })
    .await;
    assert!(
        client.onboarding().has_pending_join().await,
        "kept across the restart"
    );
    let token = stored_token(&cfg, &kc);
    pay_factor(&host, &cfg, &token).await;
    client.factor_completed();
    host::wait_for(&client, WAIT, "ready", |s| {
        s.account.phase == AccountPhase::Ready
    })
    .await;
    let result = client.onboarding().finish_signup().await;
    assert_eq!(result.community, Some(community));
    assert_eq!(result.invite.as_deref(), Some(code.as_str()));
    assert!(!client.onboarding().has_pending_join().await, "spent");
    stop(client).await;
    stop(owner_client).await;
}

async fn advance(host: &Host, seconds: u64) {
    let control = host.control.clone();
    tokio::task::spawn_blocking(move || {
        std::thread::spawn(move || {
            let rt = tokio::runtime::Runtime::new().unwrap();
            rt.block_on(async {
                reqwest::Client::new()
                    .post(format!("{control}/debug/clock"))
                    .header("content-type", "application/json")
                    .body(format!("{{\"seconds\":{seconds}}}"))
                    .send()
                    .await
                    .unwrap();
            });
        })
        .join()
        .unwrap();
    })
    .await
    .unwrap();
}

/// `e2e/web/enrol.spec.ts` "a second browser joins the account by password and recovery key, and the first can remove
/// it" (its joining half; the removal is Settings → Devices, Task 16): the key as written down (13 groups, spaces), both
/// devices read a peer, and the peer and the first device read the second.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad"]
async fn enrol_by_password_and_recovery_key() {
    let host = tokio::task::spawn_blocking(|| host::start_host("t25-enrol", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let mut alice = host::sign_up(&host, &invite, "alice", "Alice", "t25-enrol").await;
    let bob = host::sign_up(&host, &invite, "bob", "Bob", "t25-enrol").await;
    let community = CommunityId(host::community(&host, &bob, "Peers", &[&alice]));
    host::clear_session(&alice);
    host::clear_session(&bob);
    let a = host::start(&alice).await;
    let b = host::start(&bob).await;
    let general_a = listed_channel(&a, community, "general", 0).await;
    let general_b = listed_channel(&b, community, "general", 0).await;
    open(&b, general_b).await;
    open(&a, general_a).await;

    let (cfg, kc, _root) = new_device(&host, "enrol-second");
    let mut enrol = Enrol::new(cfg.clone(), kc.clone()).unwrap();
    assert_eq!(enrol.login("alice", PASSWORD).await, Ok(Owed::Code));
    let code = alice.next_code(&host);
    enrol.code(&code).await.expect("code");
    let spaced: String = alice
        .recovery_key
        .as_bytes()
        .chunks(4)
        .map(|c| std::str::from_utf8(c).unwrap())
        .collect::<Vec<_>>()
        .join(" ");
    enrol.add_device(&spaced).await.expect("enrolled");
    drop(enrol);
    let a2 = start_cfg(&cfg, &kc).await;
    host::wait_for(&a2, WAIT, "the second device ready", |s| {
        s.account.phase == AccountPhase::Ready
    })
    .await;
    let general_a2 = listed_channel(&a2, community, "general", 0).await;
    open(&a2, general_a2).await;

    send(&b, general_b, "peer to both").await;
    reads(&a, general_a, UserId(bob.user_id), "peer to both").await;
    reads(&a2, general_a2, UserId(bob.user_id), "peer to both").await;
    send(&a2, general_a2, "from the second device").await;
    reads(
        &a,
        general_a,
        UserId(alice.user_id),
        "from the second device",
    )
    .await;
    reads(
        &b,
        general_b,
        UserId(alice.user_id),
        "from the second device",
    )
    .await;
    let s = b.state().borrow().clone();
    let from_second = s.timelines[&general_b]
        .items
        .iter()
        .find(|i| i.body == "from the second device")
        .unwrap()
        .sender_device;
    assert!(
        from_second.is_some_and(|d| d.0 != alice.device_id),
        "sent by the new device"
    );
    stop(a2).await;
    stop(a).await;
    stop(b).await;
}

/// `e2e/web/enrol.spec.ts` "a wrong recovery key is refused in place, and the right one typed in lower case with hyphens
/// works after it": a short key is counted and refused before anything is sent, a well-formed wrong key is refused after
/// the backup is read, and the right key in lower case with hyphens then enrols the device.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad"]
async fn wrong_recovery_key_refused_lowercase_hyphens_ok() {
    let host =
        tokio::task::spawn_blocking(|| host::start_host("t25-wrongkey", "127.0.0.1:0", None))
            .await
            .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let mut alice = host::sign_up(&host, &invite, "alice", "Alice", "t25-wrongkey").await;
    let (cfg, kc, _root) = new_device(&host, "wrongkey");
    let mut enrol = Enrol::new(cfg.clone(), kc.clone()).unwrap();

    let short = &alice.recovery_key[..51];
    assert_eq!(check_key(short).count, 51);
    assert_eq!(enrol.add_device(short).await, Err(SignInError::KeyLength));

    assert_eq!(enrol.login("alice", PASSWORD).await, Ok(Owed::Code));
    enrol.code(&alice.next_code(&host)).await.expect("code");
    // Well formed, 52 characters of the alphabet, and not this account's key.
    // The first character changed: its bits are the key's high bits, so the form stays valid.
    let first = if alice.recovery_key.starts_with('0') {
        '2'
    } else {
        '0'
    };
    let wrong: String = std::iter::once(first)
        .chain(alice.recovery_key.chars().skip(1))
        .collect();
    assert!(check_key(&wrong).well_formed);
    assert_eq!(enrol.add_device(&wrong).await, Err(SignInError::WrongKey));

    // The refused attempt spent the login; the person signs in again and pastes the right key as a person might.
    assert_eq!(enrol.login("alice", PASSWORD).await, Ok(Owed::Code));
    enrol.code(&alice.next_code(&host)).await.expect("code");
    let pasted = alice
        .recovery_key
        .to_lowercase()
        .as_bytes()
        .chunks(4)
        .map(|c| std::str::from_utf8(c).unwrap())
        .collect::<Vec<_>>()
        .join("-");
    assert!(check_key(&pasted).well_formed);
    enrol
        .add_device(&pasted)
        .await
        .expect("the right key enrols");
    drop(enrol);
    let a2 = start_cfg(&cfg, &kc).await;
    host::wait_for(&a2, WAIT, "ready", |s| {
        s.account.phase == AccountPhase::Ready
    })
    .await;
    stop(a2).await;
}

/// `e2e/web/enrol.spec.ts` "the peer’s enrolled device enters and leaves the group while the browser keeps talking"
/// (its entering half: the peer's revocation is Settings → Devices, Task 16, and goes to Task 28's list): a peer
/// enrols a second device into the group this device talks in; both directions keep reading.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad"]
async fn peer_enrolled_device_enters_and_leaves_group() {
    let host = tokio::task::spawn_blocking(|| host::start_host("t25-peer", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let alice = host::sign_up(&host, &invite, "alice", "Alice", "t25-peer").await;
    let mut bob = host::sign_up(&host, &invite, "bob", "Bob", "t25-peer").await;
    let community = CommunityId(host::community(&host, &bob, "Peers", &[&alice]));
    host::clear_session(&alice);
    host::clear_session(&bob);
    let a = host::start(&alice).await;
    let b = host::start(&bob).await;
    let general_a = listed_channel(&a, community, "general", 0).await;
    let general_b = listed_channel(&b, community, "general", 0).await;
    open(&b, general_b).await;
    open(&a, general_a).await;

    let (cfg, kc, _root) = new_device(&host, "peer-second");
    let mut enrol = Enrol::new(cfg.clone(), kc.clone()).unwrap();
    assert_eq!(enrol.login("bob", PASSWORD).await, Ok(Owed::Code));
    enrol.code(&bob.next_code(&host)).await.expect("code");
    enrol.add_device(&bob.recovery_key).await.expect("enrolled");
    drop(enrol);
    let b2 = start_cfg(&cfg, &kc).await;
    let general_b2 = listed_channel(&b2, community, "general", 0).await;
    open(&b2, general_b2).await;

    send(&b, general_b, "with three devices").await;
    reads(&a, general_a, UserId(bob.user_id), "with three devices").await;
    send(&a, general_a, "browser with three devices").await;
    reads(
        &b,
        general_b,
        UserId(alice.user_id),
        "browser with three devices",
    )
    .await;
    reads(
        &b2,
        general_b2,
        UserId(alice.user_id),
        "browser with three devices",
    )
    .await;
    send(&b2, general_b2, "from the enrolled device").await;
    reads(
        &a,
        general_a,
        UserId(bob.user_id),
        "from the enrolled device",
    )
    .await;
    stop(b2).await;
    stop(a).await;
    stop(b).await;
}

/// `e2e/web/create.spec.ts` "from an empty instance: start a server, invite a friend through a link, reply, DM and add
/// a channel" (the DM part is Task 13's): the server and its starter channels (#general and the voice channel general),
/// an invite link the friend joins with, a reply each way, and a channel added after.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad"]
async fn empty_instance_start_server_invite_reply_dm_add_channel() {
    let host = tokio::task::spawn_blocking(|| host::start_host("t25-create", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let ada = host::sign_up(&host, &invite, "ada", "Ada", "t25-create").await;
    let friend = host::sign_up(&host, &invite, "friend", "Friend", "t25-create").await;
    host::clear_session(&ada);
    host::clear_session(&friend);
    let a = host::start(&ada).await;
    let f = host::start(&friend).await;
    let s = host::wait_for(&a, WAIT, "ready", |s| {
        s.account.phase == AccountPhase::Ready
    })
    .await;
    assert!(s.communities.is_empty(), "an empty instance");

    // A refused name sends nothing.
    assert!(a.onboarding().create_server("   ").await.is_err());
    let community = a
        .onboarding()
        .create_server("  Midgard  ")
        .await
        .expect("server");
    for (kind, name) in [
        (NewChannelKind::Text, "general"),
        (NewChannelKind::Voice, "general"),
    ] {
        a.onboarding()
            .create_channel(community, kind, name, None, &[])
            .await
            .expect("starter");
    }
    a.refresh();
    let s = host::wait_for(&a, WAIT, "the server and its starters", |s| {
        let starters = |kind: u8| {
            s.channels
                .iter()
                .filter(|c| c.community_id == community && c.name == "general" && c.kind == kind)
                .count()
        };
        community_listed(s, community) && starters(0) == 1 && starters(1) == 1
    })
    .await;
    assert_eq!(
        s.communities
            .iter()
            .find(|c| c.id == community)
            .unwrap()
            .name,
        "Midgard"
    );
    let general = listed_channel(&a, community, "general", 0).await;
    let _voice = listed_channel(&a, community, "general", 1).await;
    open(&a, general).await;

    let made = a
        .onboarding()
        .create_invite(community)
        .await
        .expect("invite");
    // The slice keeps it for the session: a reopened dialog shows the same link.
    let held = host::wait_for(&a, WAIT, "the invite held", |s| {
        s.onboarding.invites.contains_key(&community)
    })
    .await;
    assert_eq!(held.onboarding.invites[&community].url, made.url);
    // An instance invite is not a server invite.
    assert_eq!(
        f.onboarding().join(&invite).await.map_err(|e| e.code),
        Err("E_INVITE_NOT_COMMUNITY".to_owned())
    );
    assert_eq!(
        f.onboarding().join("nope-nope").await.map_err(|e| e.code),
        Err("E_INVITE_INVALID".to_owned())
    );
    assert_eq!(f.onboarding().join(&made.url).await, Ok(community));
    f.refresh();
    let general_f = listed_channel(&f, community, "general", 0).await;
    open(&f, general_f).await;
    send(&a, general, "hello friend").await;
    reads(&f, general_f, UserId(ada.user_id), "hello friend").await;
    send(&f, general_f, "hi ada").await;
    reads(&a, general, UserId(friend.user_id), "hi ada").await;

    // A channel added after: a slug, at the next position.
    let name = channel_name(NewChannelKind::Text, "Ship Talk");
    let ship = a
        .onboarding()
        .create_channel(community, NewChannelKind::Text, &name, None, &[])
        .await
        .expect("channel");
    let listed = listed_channel(&a, community, "ship-talk", 0).await;
    assert_eq!(ship, listed);
    let s = a.state().borrow().clone();
    // The instance's built-in AFK channel sits at 2^30 and never counts.
    let max_before = s
        .channels
        .iter()
        .filter(|c| c.community_id == community && c.id != ship)
        .map(|c| c.position)
        .filter(|p| u64::from(*p) < dilla_client::features::signup::AFK_BUILTIN_POSITION)
        .max()
        .unwrap();
    assert_eq!(s.channel(ship).unwrap().position, max_before + 1);
    stop(a).await;
    stop(f).await;
}

/// `e2e/web/sidebar-actions.spec.ts` (its owner part) "the owner adds a channel with the "+"": every kind the dialog
/// offers, a category and a channel inside it; a member may not add one.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad"]
async fn owner_adds_channel_with_plus() {
    let host = tokio::task::spawn_blocking(|| host::start_host("t25-plus", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let owner = host::sign_up(&host, &invite, "owner", "Owner", "t25-plus").await;
    let member = host::sign_up(&host, &invite, "member", "Member", "t25-plus").await;
    let community = CommunityId(host::community(&host, &owner, "Midgard", &[&member]));
    host::clear_session(&owner);
    host::clear_session(&member);
    let o = host::start(&owner).await;
    let m = host::start(&member).await;
    let general_o = listed_channel(&o, community, "general", 0).await;
    let general_m = listed_channel(&m, community, "general", 0).await;
    open(&o, general_o).await;
    open(&m, general_m).await;
    assert!(o.onboarding().can_announce(community).await);
    assert!(!m.onboarding().can_announce(community).await);

    let flow = o.onboarding();
    let news = flow
        .create_channel(community, NewChannelKind::Announcement, "news", None, &[])
        .await
        .expect("announcement");
    let lounge = flow
        .create_channel(community, NewChannelKind::Voice, "Lounge", None, &[])
        .await
        .expect("voice");
    let category = flow
        .create_channel(community, NewChannelKind::Category, "Projects", None, &[])
        .await
        .expect("category");
    let inside = flow
        .create_channel(
            community,
            NewChannelKind::Text,
            "ship-talk",
            Some(category),
            &[],
        )
        .await
        .expect("inside the category");
    o.refresh();
    let s = host::wait_for(&o, WAIT, "the new channels", |s| {
        [news, lounge, category, inside]
            .iter()
            .all(|c| s.channel(*c).is_some())
    })
    .await;
    assert_eq!(s.channel(news).unwrap().kind, 0);
    assert_eq!(s.channel(lounge).unwrap().kind, 1);
    assert_eq!(s.channel(category).unwrap().kind, 2);
    assert_eq!(s.channel(inside).unwrap().parent_id, Some(category));
    // A category inside a category is refused here.
    assert!(
        flow.create_channel(
            community,
            NewChannelKind::Category,
            "Nested",
            Some(category),
            &[]
        )
        .await
        .is_err()
    );
    // A member may not add a channel.
    let refused = m
        .onboarding()
        .create_channel(community, NewChannelKind::Text, "mine", None, &[])
        .await
        .unwrap_err();
    assert_eq!(refused.code, "E_FORBIDDEN", "{refused:?}");

    // The announcement channel is read-only for @everyone: the owner posts, the member reads and cannot post.
    m.refresh();
    let news_m = listed_channel(&m, community, "news", 0).await;
    open(&o, news).await;
    send(&o, news, "big news").await;
    open(&m, news_m).await;
    reads(&m, news_m, UserId(owner.user_id), "big news").await;
    let sent = tokio::time::timeout(WAIT, m.send_text(news_m, "may I?".into()))
        .await
        .expect("the send settles")
        .expect("the client runs");
    if sent.is_ok() {
        // Queued: the instance's refusal marks the row failed, and the owner never reads it.
        host::wait_for(&m, WAIT, "the member's post refused", |s| {
            s.timelines[&news_m]
                .items
                .iter()
                .any(|i| i.body == "may I?" && i.state == TimelineItemState::Failed)
        })
        .await;
    }
    send(&o, news, "after the refusal").await;
    let s = o.state().borrow().clone();
    assert!(!s.timelines[&news].items.iter().any(|i| i.body == "may I?"));
    stop(o).await;
    stop(m).await;
}
