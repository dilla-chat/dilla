//! Task 2 (screens plan): the client's lifecycle against a real dillad. The lifecycle fixes that need no instance (the
//! Link's connect racing a disconnect, a renewal finishing after a ready, a panicking engine job, the server's timings)
//! are unit tests beside the code: `client::tests::{disconnect_during_connect_stays_offline,
//! renewed_token_after_ready_does_not_restart, engine_panic_reports_failed_not_stopped}` and
//! `gateway::tests::server_timings_are_clamped`.

// Its test needs `Client::snapshots`: run it with `--features testing`.
#![cfg(feature = "testing")]

#[path = "support/world.rs"]
mod world;

use std::time::Duration;

use dilla_client::state::{AccountPhase, ConnectionStatus};
use world::*;

/// An account that owes its second factor starts, and the gateway refuses it: `FactorSetup`. The factor is set up over
/// the instance's routes (as the factor screens do); `Client::factor_completed` then takes the same client out of
/// `FactorSetup` and online, and its ready does what the owed account could not (its KeyPackages).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[ignore = "needs dillad: target/dilla-testhost and the wasi core (the desktop-gpui CI job runs it with --ignored on Linux)"]
async fn factor_setup_clears_after_factor_completed() {
    let host = tokio::task::spawn_blocking(|| start_host("factor"))
        .await
        .unwrap();
    let invite = host.invites.last().unwrap().clone();
    let alice = sign_up_with(&host, &invite, "alice", "factor", true).await;
    clear_session(&alice);
    let (target, _) = key_package_limits(&host).await;
    let a = start(&alice).await;
    // Every snapshot from the start on: the owed account starts in FactorSetup with no gateway (no ready, never
    // online), and a completion without a factor (the instance still finds one owed) leaves it there.
    let mut snapshots = a.snapshots();
    let first = snapshots.recv().await.unwrap();
    assert_eq!(first.account.phase, AccountPhase::FactorSetup);
    a.factor_completed();
    tokio::time::sleep(Duration::from_secs(3)).await;
    let mut seen = 1;
    while let Ok(s) = snapshots.try_recv() {
        seen += 1;
        assert_eq!(
            s.account.phase,
            AccountPhase::FactorSetup,
            "snapshot {seen}"
        );
        assert_ne!(
            s.connection.status,
            ConnectionStatus::Online,
            "snapshot {seen}"
        );
        assert_eq!(s.connection.readies, 0, "snapshot {seen}");
    }
    assert_eq!(a.state().borrow().connection.readies, 0);
    // The factor screens add an authenticator app; then the same client goes on.
    enrol_totp(&host, &alice).await;
    a.factor_completed();
    let s = wait_for(&a, Duration::from_secs(30), "ready and online", |s| {
        s.account.phase == AccountPhase::Ready && s.connection.status == ConnectionStatus::Online
    })
    .await;
    assert!(s.connection.readies >= 1);
    poll(
        Duration::from_secs(20),
        "the key packages the ready publishes",
        || async { (key_package_count(&host, &alice).await == target).then_some(()) },
    )
    .await;
    // A completion outside FactorSetup does nothing.
    let readies = a.state().borrow().connection.readies;
    a.factor_completed();
    tokio::time::sleep(Duration::from_secs(1)).await;
    let s = a.state().borrow().clone();
    assert_eq!(s.account.phase, AccountPhase::Ready);
    assert_eq!(s.connection.readies, readies, "no reconnect");
}
