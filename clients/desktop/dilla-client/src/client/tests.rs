//! The client's own decisions: how a refused session is renewed, where `start` may be called, and a poisoned core.

use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use super::*;
use crate::sync::api::ApiError;

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
    let engine = Engine::new(core, api, Arc::new(Hooks { tx }), instance);
    assert!(
        engine.stopped(),
        "the engine treats a poisoned core as stopped"
    );
    assert!(
        engine.groups().await.is_empty(),
        "the publisher's read answers nothing, it does not unwind"
    );
}
