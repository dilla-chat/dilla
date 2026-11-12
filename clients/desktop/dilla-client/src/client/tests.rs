//! The client's own decisions: how a refused session is renewed, where `start` may be called, and a poisoned core.

use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use super::*;
use crate::sync::api::ApiError;

#[test]
fn a_disconnect_during_a_renewal_keeps_the_connection_down() {
    // A refusal starts the one renewal; the app takes the connection down before it ends: the renewed session is kept
    // for the app's own reconnect, and no gateway is started against the disconnect.
    let mut gate = RenewGate::default();
    assert_eq!(gate.refused(), OnRefused::Renew);
    assert_eq!(gate.done(Renewal::Renewed, true), AfterRenewal::Stay);
}

#[test]
fn a_reconnect_overtaking_a_renewal_waits_for_its_verdict() {
    // disconnect + reconnect while the renewal is out: the new gateway, on the old token, is refused too. That refusal
    // waits (no "sign in again"); the renewal then restarts the gateway on the new session, and its ready re-arms.
    let mut gate = RenewGate::default();
    assert_eq!(gate.refused(), OnRefused::Renew);
    assert_eq!(gate.refused(), OnRefused::Wait);
    assert_eq!(gate.done(Renewal::Renewed, false), AfterRenewal::Restart);
    gate.ready();
    assert_eq!(gate.refused(), OnRefused::Renew, "one renewal per ready");
}

#[test]
fn a_second_refusal_before_the_next_ready_signs_out() {
    let mut gate = RenewGate::default();
    assert_eq!(gate.refused(), OnRefused::Renew);
    assert_eq!(gate.done(Renewal::Renewed, false), AfterRenewal::Restart);
    assert_eq!(gate.refused(), OnRefused::SignOut);
    let mut gate = RenewGate::default();
    gate.refused();
    assert_eq!(gate.done(Renewal::SignedOut, false), AfterRenewal::SignOut);
}

#[test]
fn a_ready_never_undoes_a_verdict() {
    assert_eq!(phase_on_ready(AccountPhase::Revoked), AccountPhase::Revoked);
    assert_eq!(phase_on_ready(AccountPhase::Failed), AccountPhase::Failed);
    assert_eq!(
        phase_on_ready(AccountPhase::FactorSetup),
        AccountPhase::FactorSetup
    );
    assert_eq!(phase_on_ready(AccountPhase::Loading), AccountPhase::Ready);
}

#[tokio::test]
async fn an_action_that_unwinds_answers_stopped() {
    let (tx, mut rx) = mpsc::unbounded_channel();
    let answer: Result<(), ClientError> = guarded(async { panic!("E_CORE_POISONED") }, &tx).await;
    assert!(matches!(answer, Err(ClientError::Stopped)), "{answer:?}");
    assert!(matches!(rx.try_recv(), Ok(Update::Panicked)));
    assert!(matches!(
        guarded(async { Ok::<_, ClientError>(7) }, &tx).await,
        Ok(7)
    ));
    assert!(rx.try_recv().is_err(), "an answer is not a failure");
}

/// An in-memory core, an API on an address nothing answers, and an engine whose hooks send to `tx`.
fn offline_parts(tx: mpsc::UnboundedSender<Update>) -> (CoreHandle, Arc<Api>, Arc<Engine>) {
    let conn = rusqlite::Connection::open_in_memory().unwrap();
    let core: CoreHandle = Arc::new(Mutex::new(
        dilla_core::client::ClientCore::open(Arc::new(Mutex::new(conn))).unwrap(),
    ));
    let auth = AuthToken::new("token");
    let http =
        ReqwestHttp::new(url::Url::parse("http://127.0.0.1:9").unwrap(), auth.clone()).unwrap();
    let api = Arc::new(Api::new(
        http,
        SocketRequests::new(),
        auth,
        core.clone(),
        Arc::new(|_: &str| {}),
    ));
    let instance = crate::sync::api::InstanceInfo {
        instance_id: [1; 16],
        policy_version: 1,
        external_sender_pub: [0; 32],
    };
    let engine = Engine::new(
        core.clone(),
        api.clone(),
        Arc::new(Hooks {
            tx,
            inner: Arc::default(),
        }),
        instance,
    );
    (core, api, engine)
}

fn offline_link(engine: Arc<Engine>, updates: mpsc::UnboundedSender<Update>) -> Arc<Link> {
    Arc::new(Link {
        engine,
        socket: SocketRequests::new(),
        auth: AuthToken::new("token"),
        server: url::Url::parse("http://127.0.0.1:9").unwrap(),
        options: GatewayOptions::default(),
        gateway: Mutex::new(None),
        updates,
        app_offline: AtomicBool::new(false),
    })
}

fn has_gateway(link: &Link) -> bool {
    link.gateway.lock().unwrap().is_some()
}

/// The Link TOCTOU (screens plan Task 2): a renewal's restart checked `app_offline` before the app's disconnect landed;
/// its connect then runs after the disconnect. The app's choice wins: the connect reads `app_offline` under the
/// gateway's lock and connects nothing.
#[tokio::test]
async fn disconnect_during_connect_stays_offline() {
    let (tx, _rx) = mpsc::unbounded_channel();
    let (_core, _api, engine) = offline_parts(tx.clone());
    let link = offline_link(engine, tx);
    link.connect();
    assert!(has_gateway(&link));
    // The restart's `disconnect` ran (its check of app_offline passed a moment before); then the app's disconnect
    // (`Client::disconnect`: the flag first, then the gateway); then the restart's `connect`.
    link.disconnect();
    link.app_offline.store(true, Ordering::Release);
    link.disconnect();
    link.connect();
    assert!(!has_gateway(&link), "the app took the connection down");
    link.restart();
    assert!(!has_gateway(&link));
    // The app's own reconnect connects.
    link.app_offline.store(false, Ordering::Release);
    link.connect();
    assert!(has_gateway(&link));
    link.disconnect();
}

/// A refusal starts a renewal; before it ends, a gateway reaches ready (the app reconnected on a session the HTTP
/// layer had already renewed). The renewal's verdict must not tear that working gateway down and connect again.
#[test]
fn renewed_token_after_ready_does_not_restart() {
    let mut gate = RenewGate::default();
    assert_eq!(gate.refused(), OnRefused::Renew);
    gate.ready();
    assert_eq!(gate.done(Renewal::Renewed, false), AfterRenewal::Stay);
    // The next refusal is a new one: this ready re-armed the renewal, and it restarts as usual.
    assert_eq!(gate.refused(), OnRefused::Renew);
    assert_eq!(gate.done(Renewal::Renewed, false), AfterRenewal::Restart);
    // A ready before a renewal starts is no reason to skip its restart.
    let mut gate = RenewGate::default();
    gate.ready();
    assert_eq!(gate.refused(), OnRefused::Renew);
    assert_eq!(gate.done(Renewal::Renewed, false), AfterRenewal::Restart);
    // A device the instance refused is signed out whatever came meanwhile.
    let mut gate = RenewGate::default();
    gate.refused();
    gate.ready();
    assert_eq!(gate.done(Renewal::SignedOut, false), AfterRenewal::SignOut);
}

/// A publisher of a `Ready` account on the offline parts, its update queue and its published state.
#[allow(clippy::type_complexity)]
fn offline_publisher() -> (
    Publisher,
    mpsc::UnboundedReceiver<Update>,
    watch::Receiver<Arc<AppState>>,
    CoreHandle,
    Arc<Engine>,
) {
    let (tx, rx) = mpsc::unbounded_channel();
    let (core, api, engine) = offline_parts(tx.clone());
    let link = offline_link(engine.clone(), tx.clone());
    let (state_tx, state_rx) = watch::channel(Arc::new(AppState::default()));
    let initial = AppState {
        account: crate::state::AccountState {
            phase: AccountPhase::Ready,
            ..Default::default()
        },
        ..AppState::default()
    };
    let upkeep = Upkeep::new(
        core.clone(),
        api.clone(),
        tokio::runtime::Handle::current(),
        [1; 16],
        [2; 16],
    );
    let publisher = Publisher {
        state: initial,
        tx: state_tx,
        core: core.clone(),
        engine: engine.clone(),
        api,
        link,
        updates: tx,
        own_user: [1; 16],
        own_device: [2; 16],
        focus: None,
        not_member: HashSet::new(),
        resyncing: HashSet::new(),
        renew: RenewGate::default(),
        upkeep,
        inner: Arc::default(),
        removals_due: false,
        removals_pending: false,
    };
    (publisher, rx, state_rx, core, engine)
}

/// The security fix of the ready's backup pass: a refreshed own device list that leaves this device out signs it out
/// (controller.ts `if (!own.listed) return this.enterRevoked()`), and the connection goes down.
#[tokio::test]
async fn an_unlisted_device_is_signed_out() {
    let (publisher, rx, mut state_rx, _core, _engine) = offline_publisher();
    let updates = publisher.updates.clone();
    let link = publisher.link.clone();
    link.connect();
    tokio::spawn(publisher.run(rx));
    updates.send(Update::Unlisted).unwrap();
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if state_rx.borrow_and_update().account.phase == AccountPhase::Revoked {
                return;
            }
            state_rx.changed().await.unwrap();
        }
    })
    .await
    .expect("the account is reported Revoked");
    assert!(!has_gateway(&link), "the gateway is down");
}

/// A panic in an engine job that does not poison the core (a bug in the engine, not the store): the engine stops and
/// the client reports `Failed`, as for a poisoned core, instead of answering `Stopped` with the account still `Ready`.
#[tokio::test]
async fn engine_panic_reports_failed_not_stopped() {
    let (publisher, rx, mut state_rx, core, engine) = offline_publisher();
    let running = tokio::spawn(publisher.run(rx));
    // A job on a lane, the way every frame, catch-up and commit runs.
    let job = engine.watched(async {
        let rows: Vec<u8> = Vec::new();
        let _ = rows[3];
    });
    tokio::spawn(job).await.unwrap();
    assert!(!core.is_poisoned(), "the panic did not hold the core");
    assert!(engine.stopped(), "nothing more runs on the engine");
    tokio::time::timeout(Duration::from_secs(5), async {
        loop {
            if state_rx.borrow_and_update().account.phase == AccountPhase::Failed {
                return;
            }
            state_rx.changed().await.unwrap();
        }
    })
    .await
    .expect("the account is reported Failed");
    // The publisher ends, as for a poisoned core.
    tokio::time::timeout(Duration::from_secs(5), running)
        .await
        .expect("the publisher stopped")
        .unwrap();
}

fn quick(_: u32) -> Duration {
    Duration::from_millis(5)
}

#[tokio::test]
async fn a_transient_failure_is_retried_until_the_instance_answers() {
    // The instance unreachable twice, then a 503, then it answers: the session is renewed, never signed out.
    let calls = AtomicU32::new(0);
    let outcome = renew(
        || {
            let n = calls.fetch_add(1, Ordering::AcqRel);
            async move {
                match n {
                    0 | 1 => Err(SessionError::Api(ApiError::local(
                        0,
                        "E_NETWORK",
                        "unreachable",
                    ))),
                    2 => Err(SessionError::Api(ApiError::local(503, "E_HTTP", ""))),
                    _ => Ok(()),
                }
            }
        },
        quick,
    )
    .await;
    assert_eq!(outcome, Renewal::Renewed);
    assert_eq!(calls.load(Ordering::Acquire), 4);
    // A store hiccup is not a verdict either.
    let calls = AtomicU32::new(0);
    let outcome = renew(
        || {
            let n = calls.fetch_add(1, Ordering::AcqRel);
            async move {
                if n == 0 {
                    Err(SessionError::Core("E_CORE_STORAGE".into()))
                } else {
                    Ok(())
                }
            }
        },
        quick,
    )
    .await;
    assert_eq!(outcome, Renewal::Renewed);
}

#[tokio::test]
async fn only_the_instances_refusal_signs_out() {
    for refusal in [
        SessionError::Revoked,
        SessionError::Scope,
        SessionError::NoIdentity,
    ] {
        let calls = AtomicU32::new(0);
        let outcome = renew(
            || {
                calls.fetch_add(1, Ordering::AcqRel);
                let e = refusal.clone();
                async move { Err(e) }
            },
            quick,
        )
        .await;
        assert_eq!(outcome, Renewal::SignedOut);
        assert_eq!(calls.load(Ordering::Acquire), 1, "no retry after a verdict");
    }
}

#[test]
fn renewal_backoff_doubles_to_thirty_seconds() {
    let waits: Vec<u64> = (1..=8).map(|n| renewal_backoff(n).as_secs()).collect();
    assert_eq!(waits, [1, 2, 4, 8, 16, 30, 30, 30]);
}

#[tokio::test]
async fn a_poisoned_core_stops_the_engine_and_is_never_used_again() {
    let conn = rusqlite::Connection::open_in_memory().unwrap();
    let core: CoreHandle = Arc::new(Mutex::new(
        dilla_core::client::ClientCore::open(Arc::new(Mutex::new(conn))).unwrap(),
    ));
    // A core call that panics while it holds the core.
    let panicked = crate::sync::api::try_core(&core, |_| panic!("a core call fails")).await;
    assert_eq!(panicked.err(), Some(crate::sync::api::CorePoisoned));
    assert!(core.is_poisoned());
    let ran = Arc::new(AtomicU32::new(0));
    let r = ran.clone();
    let again = crate::sync::api::try_core(&core, move |_| {
        r.fetch_add(1, Ordering::AcqRel);
    })
    .await;
    assert!(again.is_err());
    assert_eq!(
        ran.load(Ordering::Acquire),
        0,
        "nothing runs on a poisoned core"
    );

    let auth = AuthToken::new("token");
    let http =
        ReqwestHttp::new(url::Url::parse("http://127.0.0.1:9").unwrap(), auth.clone()).unwrap();
    let api = Arc::new(Api::new(
        http,
        SocketRequests::new(),
        auth,
        core.clone(),
        Arc::new(|_: &str| {}),
    ));
    let (tx, _rx) = mpsc::unbounded_channel();
    let instance = crate::sync::api::InstanceInfo {
        instance_id: [1; 16],
        policy_version: 1,
        external_sender_pub: [0; 32],
    };
    let engine = Engine::new(
        core,
        api,
        Arc::new(Hooks {
            tx,
            inner: Arc::default(),
        }),
        instance,
    );
    assert!(
        engine.stopped(),
        "the engine treats a poisoned core as stopped"
    );
    assert!(
        engine.groups().await.is_empty(),
        "the publisher's read answers nothing, it does not unwind"
    );
}
