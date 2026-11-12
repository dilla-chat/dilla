//! Task 16's client tests for the user settings area (standard procedure S2), against a real dillad.
//!
//! Needs `target/dilla-testhost` and the WASI core (see `tests/sync.rs`); the tests are `#[ignore]`d so a plain `cargo test`
//! needs no Go toolchain. They run with `cargo test -p dilla-client --test user_settings -- --ignored`.
//!
//! The first account is made the way the app makes one (`testing::host`). A second device of the same account signs in the
//! way a person does on a new device: password, authenticator code, recovery key ([`SignIn`]).

use std::sync::Arc;
use std::time::Duration;

use dilla_client::Client;
use dilla_client::ClientError;
use dilla_client::features::devices::{order_devices, suggest_device_name};
use dilla_client::features::profile::ProfilePatch;
use dilla_client::instances::Instances;
use dilla_client::keychain::MemoryKeychain;
use dilla_client::seams::InstanceConfig;
use dilla_client::signin::{Owed, SignIn};
use dilla_client::state::profile::{DensitySetting, DeviceEntry, MotionSetting};
use dilla_client::state::{AppState, DeviceId};
use dilla_client::testing::host::{
    Host, PASSWORD, Person, clear_session, repo_root, sign_up, start, start_host, wait_for,
};
use zeroize::Zeroizing;

const IGNORE: &str = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)";

async fn account(tag: &str) -> (Host, Person) {
    let host = tokio::task::spawn_blocking({
        let tag = tag.to_owned();
        move || start_host(&tag, "127.0.0.1:0", None)
    })
    .await
    .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let alice = sign_up(&host, &invite, "alice", "Alice", tag).await;
    let alice = tokio::task::spawn_blocking(move || {
        clear_session(&alice);
        alice
    })
    .await
    .unwrap();
    (host, alice)
}

/// A second device of the account: password, authenticator code and the recovery key, then a running client.
async fn second_device(host: &Host, person: &mut Person, tag: &str) -> (Client, DeviceId) {
    let root = repo_root().join(format!(
        "target/testing-client-{tag}-second-{}",
        std::process::id()
    ));
    let _ = std::fs::remove_dir_all(&root);
    let base = url::Url::parse(&host.public).unwrap();
    let cfg: InstanceConfig = Instances::load(&root).unwrap().add(base).unwrap();
    let kc = Arc::new(MemoryKeychain::new());
    let mut sign_in = SignIn::new(cfg.clone(), kc.clone()).expect("sign-in");
    let owes = sign_in
        .login(&person.username, PASSWORD)
        .await
        .expect("password login");
    assert_eq!(owes, Owed::Code);
    let code = person.next_code(host);
    sign_in.code(&code).await.expect("authenticator code");
    sign_in
        .add_device(&person.recovery_key)
        .await
        .expect("the recovery key adds the device");
    drop(sign_in);
    let client =
        tokio::task::spawn_blocking(move || Client::start(cfg, kc).expect("second client"))
            .await
            .unwrap();
    let state = wait_for(
        &client,
        Duration::from_secs(30),
        "the second device's account",
        |s| s.account.device_id.is_some(),
    )
    .await;
    let id = state.account.device_id.unwrap();
    (client, id)
}

fn live(s: &AppState) -> Vec<&DeviceEntry> {
    s.profile
        .devices
        .iter()
        .filter(|d| d.revoked_at.is_none())
        .collect()
}

async fn ok<T>(rx: tokio::sync::oneshot::Receiver<Result<T, ClientError>>) -> T {
    rx.await
        .expect("the client answers")
        .expect("the call succeeds")
}

async fn refused<T: std::fmt::Debug>(
    rx: tokio::sync::oneshot::Receiver<Result<T, ClientError>>,
) -> String {
    match rx.await.expect("the client answers") {
        Err(ClientError::Sync(code)) => code,
        other => panic!("expected a refusal, got {other:?}"),
    }
}

/// Settings -> Profile saves only the fields that changed ('' clears one); another device reads what was saved.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn profile_saves_changed_fields_and_another_device_reads_them() {
    let _ = IGNORE;
    let (host, mut alice) = account("us-profile").await;
    let a = start(&alice).await;
    ok(a.profile().load()).await;
    let s = wait_for(&a, Duration::from_secs(30), "the profile", |s| {
        s.profile.me.is_some()
    })
    .await;
    assert_eq!(s.profile.me.as_ref().unwrap().display, "Alice");

    // A field that fails the rule is refused before any request, and nothing changes.
    let bad = ProfilePatch {
        display: Some("a".repeat(65)),
        ..ProfilePatch::default()
    };
    assert_eq!(refused(a.profile().update(bad)).await, "E_BAD_INPUT");

    ok(a.profile().update(ProfilePatch {
        status_msg: Some("  at the desk  ".into()),
        bio: Some("one\r\ntwo".into()),
        ..ProfilePatch::default()
    }))
    .await;
    let s = wait_for(&a, Duration::from_secs(10), "the saved profile", |s| {
        s.profile.saves == 1
    })
    .await;
    let me = s.profile.me.clone().unwrap();
    assert_eq!(
        (me.display.as_str(), me.status_msg.as_str(), me.bio.as_str()),
        ("Alice", "at the desk", "one\ntwo"),
        "the display name was left alone, the others are stored trimmed"
    );

    // '' clears a field.
    ok(a.profile().update(ProfilePatch {
        bio: Some(String::new()),
        ..ProfilePatch::default()
    }))
    .await;

    let (b, _) = second_device(&host, &mut alice, "us-profile").await;
    ok(b.profile().load()).await;
    let s = wait_for(
        &b,
        Duration::from_secs(30),
        "the profile on the second device",
        |s| s.profile.me.is_some(),
    )
    .await;
    let me = s.profile.me.clone().unwrap();
    assert_eq!(me.status_msg, "at the desk");
    assert_eq!(me.bio, "");
}

/// Appearance: density and reduced motion are kept in the store and read back.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn appearance_settings_are_kept_in_the_store() {
    let (_host, alice) = account("us-appearance").await;
    let a = start(&alice).await;
    ok(a.profile().set_density(DensitySetting::Compact)).await;
    ok(a.profile().set_reduce_motion(MotionSetting::On)).await;
    ok(a.profile().load_appearance()).await;
    let s = a.state().borrow().clone();
    assert_eq!(s.profile.density, DensitySetting::Compact);
    assert_eq!(s.profile.reduce_motion, MotionSetting::On);
}

/// e2e/web/enrol.spec.ts "a second browser joins the account by password and recovery key, and the first can remove it":
/// the list shows both devices, this one first; a wrong key is a field error and changes nothing; the right key removes the
/// other device, and its row is marked removed.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn remove_other_device_from_devices() {
    let (host, mut alice) = account("us-remove").await;
    let a = start(&alice).await;
    let (b, b_id) = second_device(&host, &mut alice, "us-remove").await;

    ok(a.devices().refresh()).await;
    let s = wait_for(&a, Duration::from_secs(30), "both devices", |s| {
        live(s).len() == 2
    })
    .await;
    let order = order_devices(&s.profile.devices);
    assert!(order[0].own, "this device is listed first");
    assert!(
        order.iter().all(|d| d.listed),
        "both are in the signed list"
    );
    assert!(order.iter().any(|d| d.id == b_id && !d.own));

    // A key of the wrong shape is refused before any request; a well-formed wrong key is the key refusal. Nothing changes.
    let short = Some(Zeroizing::new("ABCD".to_owned()));
    assert_eq!(
        refused(a.devices().revoke(b_id, short)).await,
        "E_BAD_INPUT"
    );
    let wrong = Some(Zeroizing::new("A".repeat(52)));
    assert_eq!(
        refused(a.devices().revoke(b_id, wrong)).await,
        "E_RECOVERY_KEY"
    );
    ok(a.devices().refresh()).await;
    assert_eq!(live(&a.state().borrow()).len(), 2);

    // The right key removes it.
    let key = Some(Zeroizing::new(alice.recovery_key.clone()));
    ok(a.devices().revoke(b_id, key)).await;
    let s = wait_for(
        &a,
        Duration::from_secs(30),
        "the other device removed",
        |s| {
            s.profile
                .devices
                .iter()
                .any(|d| d.id == b_id && d.revoked_at.is_some())
        },
    )
    .await;
    assert_eq!(live(&s).len(), 1);
    assert!(live(&s)[0].own);
    drop(b);
}

/// e2e/web/enrol.spec.ts "sign out and remove takes this browser out of the account and out of the group": the second
/// device signs itself out with the key; it says so, and the first device's list shows it removed and itself still live.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn sign_out_and_remove_leaves_account_and_group() {
    let (host, mut alice) = account("us-signout").await;
    let a = start(&alice).await;
    let (b, b_id) = second_device(&host, &mut alice, "us-signout").await;

    let key = Zeroizing::new(alice.recovery_key.clone());
    assert_eq!(
        refused(
            b.devices()
                .sign_out_and_remove(Zeroizing::new("A".repeat(52)))
        )
        .await,
        "E_RECOVERY_KEY"
    );
    assert!(
        !b.state().borrow().profile.signed_out,
        "a wrong key signs nothing out"
    );
    ok(b.devices().sign_out_and_remove(key)).await;
    wait_for(&b, Duration::from_secs(10), "the sign-out", |s| {
        s.profile.signed_out
    })
    .await;

    ok(a.devices().refresh()).await;
    let s = wait_for(&a, Duration::from_secs(30), "the removed device", |s| {
        s.profile
            .devices
            .iter()
            .any(|d| d.id == b_id && d.revoked_at.is_some())
    })
    .await;
    let own: Vec<_> = s.profile.devices.iter().filter(|d| d.own).collect();
    assert_eq!(own.len(), 1);
    assert!(
        own[0].revoked_at.is_none(),
        "this device is still in the account"
    );
}

/// A rename is the server's: no key, no list change; a bad name is refused before any request.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn rename_a_device_and_clear_its_name() {
    let (_host, alice) = account("us-rename").await;
    let a = start(&alice).await;
    ok(a.devices().refresh()).await;
    let me = a.state().borrow().account.device_id.unwrap();
    ok(a.devices().rename(me, Some("  Desk  ".into()))).await;
    let s = wait_for(&a, Duration::from_secs(10), "the new name", |s| {
        s.profile
            .devices
            .iter()
            .any(|d| d.label.as_deref() == Some("Desk"))
    })
    .await;
    assert!(s.profile.devices[0].own);
    assert_eq!(
        refused(a.devices().rename(me, Some("a".repeat(65)))).await,
        "E_BAD_INPUT"
    );
    ok(a.devices().rename(me, None)).await;
    wait_for(&a, Duration::from_secs(10), "the cleared name", |s| {
        s.profile.devices.iter().all(|d| d.label.is_none())
    })
    .await;
}

/// Pure: this device's suggested name has the shape the web client's has.
#[test]
fn the_suggestion_names_browser_and_system() {
    let p = suggest_device_name(
        "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0",
    )
    .unwrap();
    assert_eq!((p.browser, p.os), ("Firefox", "Linux"));
}
