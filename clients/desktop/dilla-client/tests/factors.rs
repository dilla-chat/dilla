//! Task 26's client tests for the factors area (standard procedure S2), against a real dillad: the forced factor
//! set-up, step-up, backup codes, removing a factor, the password change, the sign-in order (password, factor, recovery
//! key; a backup code waits for the key) and the system-browser ceremony's routes.
//!
//! Needs `target/dilla-testhost` and the WASI core; `#[ignore]`d like every real-dillad suite (the desktop-gpui CI job
//! runs it with `--ignored` on Linux).

#[path = "support/world.rs"]
mod world;

use std::sync::{Arc, Mutex};
use std::time::Duration;

use dilla_client::features::browser_auth::{BrowserAuth, BrowserAuthError, CeremonyKind, Opener};
use dilla_client::features::factors::{FactorKind, StepUpMethod};
use dilla_client::instances::Instances;
use dilla_client::keychain::MemoryKeychain;
use dilla_client::signin::{Owed, SignIn, SignInError};
use dilla_client::state::{AccountPhase, AppState, ConnectionStatus};
use dilla_client::testing::host as h;
use zeroize::Zeroizing;

fn z(s: &str) -> Zeroizing<String> {
    Zeroizing::new(s.to_owned())
}

fn online(s: &AppState) -> bool {
    s.account.phase == AccountPhase::Ready && s.connection.status == ConnectionStatus::Online
}

/// A code the instance refuses now: the right one with its last digit changed.
fn wrong(code: &str) -> String {
    let last = code.as_bytes()[5];
    let other = if last == b'9' { b'0' } else { last + 1 };
    format!("{}{}", &code[..5], char::from(other))
}

/// required-factor.spec.ts (the forced screen's client half): an account that owes its factor starts in `FactorSetup`
/// with no gateway; the factors read owed; finishing before a factor is refused as still owed; a wrong code is 400
/// `E_INVALID_REQUEST`; the right one answers ten backup codes and publishes the factors; then the account goes on.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn forced_setup_adds_an_app_then_goes_ready() {
    let host = tokio::task::spawn_blocking(|| world::start_host("t26-forced"))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let ada = world::sign_up_with(&host, &invite, "ada", "t26", true).await;
    world::clear_session(&ada);
    let client = world::start(&ada).await;
    assert_eq!(
        client.state().borrow().account.phase,
        AccountPhase::FactorSetup
    );
    let factors = client.factors();
    let read = factors
        .refresh()
        .await
        .unwrap()
        .expect("factor setup holds the factors");
    assert!(read.owed && !read.holds_factor() && read.has_password);
    // Nothing added yet: the finish is refused as still owed.
    let e = factors.setup_done().await.unwrap_err();
    assert!(e.is(403, "E_FACTOR_REQUIRED"), "{e:?}");
    // A malformed code never leaves the client.
    assert_eq!(
        factors.totp_confirm("12345").await.unwrap_err().code,
        "E_BAD_INPUT"
    );
    let key = factors.totp_begin(None).await.unwrap();
    assert!(key.uri.starts_with("otpauth://totp/"));
    let right = h::totp_code(&key.secret, world::host_now(&host).await / 30);
    let e = factors.totp_confirm(&wrong(&right)).await.unwrap_err();
    assert!(e.is(400, "E_INVALID_REQUEST"), "{e:?}");
    let codes = factors.totp_confirm(&right).await.unwrap();
    assert_eq!(codes.0.len(), 10);
    let s = world::wait_for(&client, Duration::from_secs(10), "the factors slice", |s| {
        s.factors.factors.as_ref().is_some_and(|f| !f.owed)
    })
    .await;
    let f = s.factors.factors.clone().unwrap();
    assert!(f.totp.is_some());
    assert_eq!(f.backup_codes_left, 10);
    factors.setup_done().await.unwrap();
    client.factor_completed();
    world::wait_for(&client, Duration::from_secs(30), "ready and online", online).await;
    world::stop(client).await;
}

/// second-factor.spec.ts (Settings → Sign-in's client half): a wrong step-up is 403 `E_STEP_UP`; a code steps up; new
/// backup codes; the spent token is refused and the factors read again; a backup code steps up and is used up; the only
/// factor cannot be removed (409); the old password is the password change's step-up (a wrong one 403 `E_FORBIDDEN`);
/// the new password signs in.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn step_up_backup_codes_remove_and_password() {
    let host = tokio::task::spawn_blocking(|| h::start_host("t26-settings", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let mut ada = h::sign_up(&host, &invite, "ada", "Ada", "t26-settings").await;
    h::clear_session(&ada);
    let client = h::start(&ada).await;
    h::wait_for(&client, Duration::from_secs(30), "ready and online", online).await;
    let factors = client.factors();

    let right = ada.next_code(&host);
    let e = factors
        .step_up(StepUpMethod::Totp, z(&wrong(&right)))
        .await
        .unwrap_err();
    assert!(e.is_step_up(), "{e:?}");
    // Input checks before anything is sent.
    assert_eq!(
        factors
            .step_up(StepUpMethod::Backup, z("short"))
            .await
            .unwrap_err()
            .code,
        "E_BAD_INPUT"
    );
    let code = ada.next_code(&host);
    let token = factors.step_up(StepUpMethod::Totp, z(&code)).await.unwrap();
    let codes = factors
        .regenerate_backup_codes(token.clone())
        .await
        .unwrap();
    assert_eq!(codes.0.len(), 10);
    let f = factors.refresh().await.unwrap().unwrap();
    assert_eq!(f.backup_codes_left, 10);
    // A token is spent by its first use.
    let e = factors.regenerate_backup_codes(token).await.unwrap_err();
    assert!(e.is_step_up(), "{e:?}");

    // A backup code (typed with a dash and in lower case) steps up and is used up.
    let typed = format!("{}-{}", &codes.0[0][..13], &codes.0[0][13..]).to_lowercase();
    let token = factors
        .step_up(StepUpMethod::Backup, z(&typed))
        .await
        .unwrap();
    let f = factors.refresh().await.unwrap().unwrap();
    assert_eq!(f.backup_codes_left, 9);
    // The only factor cannot be removed; the factors are read again and still hold it.
    let e = factors.remove(token, FactorKind::Totp).await.unwrap_err();
    assert_eq!(e.status, 409, "{e:?}");
    let s = h::wait_for(&client, Duration::from_secs(5), "the re-read", |s| {
        s.factors.factors.as_ref().is_some_and(|f| f.totp.is_some())
    })
    .await;
    assert_eq!(s.factors.factors.as_ref().unwrap().count(), 1);

    // The password: the old one is its step-up.
    let e = factors
        .change_password(Some(z("not the password")), z("a new password 1"), None)
        .await
        .unwrap_err();
    assert!(e.is(403, "E_FORBIDDEN"), "{e:?}");
    factors
        .change_password(Some(z(h::PASSWORD)), z("a new password 1"), None)
        .await
        .unwrap();
    h::wait_for(&client, Duration::from_secs(5), "password changed", |s| {
        s.factors
            .factors
            .as_ref()
            .is_some_and(|f| f.password_changed.is_some())
    })
    .await;
    let root = h::repo_root().join(format!("target/t26-pw-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&root);
    let cfg = Instances::load(&root)
        .unwrap()
        .add(url::Url::parse(&host.public).unwrap())
        .unwrap();
    let mut flow = SignIn::new(cfg, Arc::new(MemoryKeychain::new())).unwrap();
    assert_eq!(
        flow.login("ada", h::PASSWORD).await.unwrap_err(),
        SignInError::LoginFailed
    );
    assert_eq!(
        flow.login("ada", "a new password 1").await.unwrap(),
        Owed::Code
    );
    let _ = std::fs::remove_dir_all(&root);
    world::stop(client).await;
}

/// signin-order.spec.ts (the client half): a new device logs in, gives its code (step 2), then the key (step 3); a wrong
/// key keeps the registration, so the right one finishes without a new login. A third device takes the backup-code
/// path: the code waits until the key is given, so the account still holds its codes until then; afterwards one fewer.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn password_then_factor_then_key_backup_code_waits() {
    let host = tokio::task::spawn_blocking(|| h::start_host("t26-order", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let mut ada = h::sign_up(&host, &invite, "ada", "Ada", "t26-order").await;
    h::clear_session(&ada);
    let first = h::start(&ada).await;
    h::wait_for(&first, Duration::from_secs(30), "ready and online", online).await;
    let code = ada.next_code(&host);
    let token = first
        .factors()
        .step_up(StepUpMethod::Totp, z(&code))
        .await
        .unwrap();
    let codes = first
        .factors()
        .regenerate_backup_codes(token)
        .await
        .unwrap();

    let root = h::repo_root().join(format!("target/t26-order-new-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&root);
    let server = url::Url::parse(&host.public).unwrap();

    // The second device: login, code, key.
    let mut instances = Instances::load(&root).unwrap();
    let cfg = instances.add(server.clone()).unwrap();
    let kc = Arc::new(MemoryKeychain::new());
    let mut flow = SignIn::new(cfg.clone(), kc.clone()).unwrap();
    assert_eq!(flow.login("ada", h::PASSWORD).await.unwrap(), Owed::Code);
    // A refused code spends the assertion: the person logs in again.
    let right = ada.next_code(&host);
    assert_eq!(
        flow.code(&wrong(&right)).await.unwrap_err(),
        SignInError::CodeFailed
    );
    assert_eq!(
        flow.code(&right).await.unwrap_err(),
        SignInError::NoAssertion
    );
    assert_eq!(flow.login("ada", h::PASSWORD).await.unwrap(), Owed::Code);
    flow.code(&ada.next_code(&host)).await.unwrap();
    // A backup code in the key field is the wrong length for a key.
    assert_eq!(
        flow.add_device(&codes.0[1]).await.unwrap_err(),
        SignInError::KeyLength
    );
    let bad_key: String = ada
        .recovery_key
        .chars()
        .rev()
        .filter(|c| !c.is_whitespace() && *c != '-')
        .collect();
    assert_eq!(
        flow.add_device(&bad_key).await.unwrap_err(),
        SignInError::WrongKey
    );
    flow.add_device(&ada.recovery_key).await.unwrap();
    let second = tokio::task::spawn_blocking({
        let (cfg, kc) = (cfg.clone(), kc.clone());
        move || dilla_client::Client::start(cfg, kc).expect("the new device starts")
    })
    .await
    .unwrap();
    h::wait_for(
        &second,
        Duration::from_secs(30),
        "the new device online",
        online,
    )
    .await;
    world::stop(second).await;

    // The third device: login, a backup code held, then the key.
    let cfg = instances.add(server).unwrap();
    let kc = Arc::new(MemoryKeychain::new());
    let mut flow = SignIn::new(cfg.clone(), kc.clone()).unwrap();
    assert_eq!(flow.login("ada", h::PASSWORD).await.unwrap(), Owed::Code);
    let before = first.factors().refresh().await.unwrap().unwrap();
    assert_eq!(
        before.backup_codes_left, 10,
        "the held code is not spent before the key"
    );
    flow.add_device_with_backup(&codes.0[2].to_lowercase(), &ada.recovery_key)
        .await
        .unwrap();
    let after = first.factors().refresh().await.unwrap().unwrap();
    assert_eq!(after.backup_codes_left, 9);
    // A spent code does not sign in again.
    assert_eq!(flow.login("ada", h::PASSWORD).await.unwrap(), Owed::Code);
    let root3 = h::repo_root().join(format!("target/t26-order-third-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&root3);
    let cfg3 = Instances::load(&root3)
        .unwrap()
        .add(url::Url::parse(&host.public).unwrap())
        .unwrap();
    let mut flow = SignIn::new(cfg3, Arc::new(MemoryKeychain::new())).unwrap();
    flow.login("ada", h::PASSWORD).await.unwrap();
    assert_eq!(
        flow.add_device_with_backup(&codes.0[2], &ada.recovery_key)
            .await
            .unwrap_err(),
        SignInError::BackupFailed
    );
    world::stop(first).await;
    let _ = std::fs::remove_dir_all(&root);
    let _ = std::fs::remove_dir_all(&root3);
}

/// The system-browser ceremony against dillad's `desktop-auth` routes: the instance takes the request (its challenge is
/// the verifier's SHA-256), the browser is opened on the request's page (which the instance serves), and the result is
/// asked for until the page finishes or the ceremony times out. The WebAuthn half runs in a real browser (Task 28's
/// passkey test); here the page never finishes.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn browser_ceremony_against_dillad() {
    struct Visit(Mutex<Vec<(String, u16)>>);
    impl Opener for Visit {
        fn open(&self, url: &str) -> Result<(), String> {
            let url = url.to_owned();
            let status = std::thread::spawn(move || {
                tokio::runtime::Runtime::new().unwrap().block_on(async {
                    reqwest::Client::builder()
                        .build()
                        .unwrap()
                        .get(&url)
                        .send()
                        .await
                        .map(|r| r.status().as_u16())
                        .unwrap_or(0)
                })
            })
            .join()
            .unwrap();
            self.0.lock().unwrap().push((String::new(), status));
            Ok(())
        }
    }
    let host = tokio::task::spawn_blocking(|| h::start_host("t26-browser", "127.0.0.1:0", None))
        .await
        .unwrap();
    let visit = Arc::new(Visit(Mutex::new(Vec::new())));
    let auth = BrowserAuth::new(visit.clone(), true)
        .with_timing(Duration::from_secs(3), Duration::from_millis(200));
    let server = url::Url::parse(&host.public).unwrap();
    let options = r#"{"publicKey":{"challenge":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}}"#;
    let e = auth
        .run(&server, CeremonyKind::Get, options)
        .await
        .unwrap_err();
    assert_eq!(e, BrowserAuthError::TimedOut);
    let visits = visit.0.lock().unwrap().clone();
    assert_eq!(visits.len(), 1);
    assert_eq!(visits[0].1, 200, "the instance serves the request's page");
    // Options without a challenge are refused by the instance before any browser opens.
    let e = auth
        .run(&server, CeremonyKind::Get, "{}")
        .await
        .unwrap_err();
    assert_eq!(e, BrowserAuthError::Unavailable);
    assert_eq!(visit.0.lock().unwrap().len(), 1);
}

/// controller.ts `forgetBrowser`, the server half: with the gateway down, the device's sessions are ended
/// (`DELETE /v1/devices/{id}/sessions`), so the session it held is refused afterwards; the client then halts through a
/// shared handle.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn forget_signs_the_device_out_on_the_instance() {
    use dilla_client::http::routes::{self as r, NoBody};
    use dilla_client::http::{AuthToken, ReqwestHttp};
    use dilla_client::seams::{Http, HttpError};
    let host = tokio::task::spawn_blocking(|| h::start_host("t26-forget", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let ada = h::sign_up(&host, &invite, "ada", "Ada", "t26-forget").await;
    // The client keeps the sign-up session, so the test holds the same token.
    let client = std::rc::Rc::new(h::start(&ada).await);
    h::wait_for(&client, Duration::from_secs(30), "ready and online", online).await;
    let held = ReqwestHttp::new(
        url::Url::parse(&host.public).unwrap(),
        AuthToken::new(ada.token.as_str()),
    )
    .unwrap();
    held.call::<r::GetFactors>(&[], NoBody)
        .await
        .expect("the session works before");
    client.disconnect();
    client.factors().sign_out_device().await.unwrap();
    let after = held.call::<r::GetFactors>(&[], NoBody).await.unwrap_err();
    assert!(
        matches!(after, HttpError::Status { status: 401, .. }),
        "{after:?}"
    );
    // The local half: an area handle still alive keeps the store open, so nothing is wiped.
    let client = std::rc::Rc::try_unwrap(client).ok().unwrap();
    let probe = client.store_probe();
    let cfg = ada.cfg.clone();
    let root = cfg
        .data_dir
        .parent()
        .unwrap()
        .parent()
        .unwrap()
        .to_path_buf();
    let kc = ada.kc.clone();
    let held = client.factors();
    let (root2, cfg2, kc2) = (root.clone(), cfg.clone(), kc.clone());
    let early = tokio::task::spawn_blocking(move || {
        dilla_client::features::factors::close_and_forget(
            client,
            &root2,
            &cfg2,
            kc2.as_ref(),
            Duration::from_millis(500),
        )
    })
    .await
    .unwrap();
    assert_eq!(
        early,
        Err(dilla_client::features::factors::ForgetError::StoreOpen)
    );
    assert!(
        cfg.data_dir.exists(),
        "nothing wiped while the store is open"
    );
    // Every handle gone: the store closes, then the wipe; nothing writes it again afterwards.
    drop(held);
    let mut waited = 0;
    while !probe.is_closed() && waited < 100 {
        tokio::time::sleep(Duration::from_millis(50)).await;
        waited += 1;
    }
    assert!(
        probe.is_closed(),
        "the store closes once every handle let go"
    );
    dilla_client::features::factors::forget_device(&root, &cfg, kc.as_ref()).unwrap();
    assert!(!cfg.data_dir.exists());
    tokio::time::sleep(Duration::from_secs(1)).await;
    assert!(
        !cfg.data_dir.exists(),
        "no task wrote the store after the wipe"
    );
}

/// The whole local half in one call, with no other handle: the store is closed before the wipe and the data dir goes.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn forget_wipes_only_a_closed_store() {
    let host = tokio::task::spawn_blocking(|| h::start_host("t26-wipe", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let ada = h::sign_up(&host, &invite, "ada", "Ada", "t26-wipe").await;
    let client = h::start(&ada).await;
    h::wait_for(&client, Duration::from_secs(30), "ready and online", online).await;
    client.disconnect();
    client.factors().sign_out_device().await.unwrap();
    let probe = client.store_probe();
    let cfg = ada.cfg.clone();
    let root = cfg
        .data_dir
        .parent()
        .unwrap()
        .parent()
        .unwrap()
        .to_path_buf();
    let kc = ada.kc.clone();
    let (root2, cfg2) = (root.clone(), cfg.clone());
    tokio::task::spawn_blocking(move || {
        dilla_client::features::factors::close_and_forget(
            client,
            &root2,
            &cfg2,
            kc.as_ref(),
            Duration::from_secs(10),
        )
    })
    .await
    .unwrap()
    .unwrap();
    assert!(probe.is_closed());
    assert!(!cfg.data_dir.exists());
    tokio::time::sleep(Duration::from_secs(1)).await;
    assert!(
        !cfg.data_dir.exists(),
        "no task wrote the store after the wipe"
    );
}

/// signInBack: a wrong key keeps the registration only for the same ceremony; Back resets it, so another account's
/// sign-in on the same device starts clean and works.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn back_after_a_wrong_key_lets_another_account_sign_in() {
    let host = tokio::task::spawn_blocking(|| h::start_host("t26-back", "127.0.0.1:0", None))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let mut ada = h::sign_up(&host, &invite, "ada", "Ada", "t26-back").await;
    let mut bea = h::sign_up(&host, &invite, "bea", "Bea", "t26-back").await;
    let root = h::repo_root().join(format!("target/t26-back-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&root);
    let cfg = Instances::load(&root)
        .unwrap()
        .add(url::Url::parse(&host.public).unwrap())
        .unwrap();
    let kc = Arc::new(MemoryKeychain::new());
    let mut flow = SignIn::new(cfg.clone(), kc.clone()).unwrap();
    assert_eq!(flow.login("ada", h::PASSWORD).await.unwrap(), Owed::Code);
    flow.code(&ada.next_code(&host)).await.unwrap();
    // Bea's key is a well-formed key, but not Ada's: registered, then refused.
    assert_eq!(
        flow.add_device(&bea.recovery_key).await.unwrap_err(),
        SignInError::WrongKey
    );
    flow.back();
    assert_eq!(flow.login("bea", h::PASSWORD).await.unwrap(), Owed::Code);
    flow.code(&bea.next_code(&host)).await.unwrap();
    flow.add_device(&bea.recovery_key).await.unwrap();
    let client = tokio::task::spawn_blocking(move || {
        dilla_client::Client::start(cfg, kc).expect("bea's device starts")
    })
    .await
    .unwrap();
    let s = h::wait_for(&client, Duration::from_secs(30), "bea online", online).await;
    assert_eq!(s.account.username, "bea");
    world::stop(client).await;
    let _ = ada;
    let _ = std::fs::remove_dir_all(&root);
}
