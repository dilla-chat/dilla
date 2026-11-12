//! [`SocketHttp`] over a scripted socket ([`RequestSink`]): what it writes, when, and what it answers.

use std::sync::Mutex as StdMutex;

use super::script::{Script, refusal, response};
use super::*;
use crate::http::routes::{GetChannelCall, NoBody, PostGroup, Raw};

/// An HTTP side that counts what it is handed and refuses it.
#[derive(Clone, Default)]
struct Counting(Arc<StdMutex<Vec<String>>>);

#[async_trait::async_trait]
impl Http for Counting {
    async fn call<R: Route>(&self, _a: &[&str], _r: R::Request) -> Result<R::Response, HttpError> {
        self.0
            .lock()
            .unwrap()
            .push(format!("{} {}", R::METHOD, R::PATH));
        Err(HttpError::Transport("counted".into()))
    }
}

use super::script::up;

const GROUP: &str = "0123456789abcdef0123456789abcdef";

/// A 429 is waited out and the request written again with the same idempotency key, so the instance's replay record
/// answers the second write instead of applying it twice.
#[tokio::test]
async fn a_429_retry_writes_the_same_idempotency_key() {
    let script = Arc::new(Script::default());
    let socket = up(&script);
    let http = SocketHttp::new(socket.clone(), Counting::default());
    let call = tokio::spawn(async move { http.call::<PostGroup>(&[], Raw(vec![0x80])).await });
    let (id1, idem1) = script.request(0).await;
    socket.on_response(&response(id1, 429, &refusal("E_RATE_LIMITED", Some(50))));
    let (id2, idem2) = script.request(1).await;
    assert_ne!(id1, id2, "a new frame");
    let idem1 = idem1.expect("a POST carries an idempotency key");
    assert_eq!(idem1.len(), 16);
    assert_eq!(Some(idem1), idem2, "the same key on both attempts");
    socket.on_response(&response(id2, 201, b"\x81\x01"));
    let Raw(body) = call.await.unwrap().expect("answered");
    assert_eq!(body, b"\x81\x01");
}

/// A socket route while the socket is down waits for it: nothing goes to HTTP, nothing is written until ready.
#[tokio::test]
async fn a_socket_route_while_down_waits_for_ready_and_never_uses_http() {
    let script = Arc::new(Script::default());
    let socket = SocketRequests::new();
    socket.attach_sink(Some(script.clone() as Arc<dyn RequestSink>));
    let counting = Counting::default();
    let http = SocketHttp::new(socket.clone(), counting.clone());
    let call = tokio::spawn(async move { http.call::<GetChannelCall>(&[GROUP], NoBody).await });
    tokio::time::sleep(Duration::from_millis(50)).await;
    assert_eq!(script.written(), 0, "nothing written while down");
    assert!(counting.0.lock().unwrap().is_empty(), "no HTTP fallback");
    socket.on_ready("incarnation".into());
    let (id, idem) = script.request(0).await;
    assert_eq!(idem, None, "a GET carries no key");
    socket.on_response(&response(id, 200, b"\x80"));
    assert_eq!(call.await.unwrap().expect("answered").0, b"\x80");
    assert!(counting.0.lock().unwrap().is_empty());
}

/// A refusal carries the instance's code; a written mutation whose answer is lost keeps its E_OUTCOME_UNKNOWN.
#[tokio::test]
async fn refusals_and_lost_answers_keep_their_codes() {
    let script = Arc::new(Script::default());
    let socket = up(&script);
    let http = Arc::new(SocketHttp::new(socket.clone(), Counting::default()));
    let h = http.clone();
    let call = tokio::spawn(async move { h.call::<GetChannelCall>(&[GROUP], NoBody).await });
    let (id, _) = script.request(0).await;
    socket.on_response(&response(id, 403, &refusal("E_PERM", None)));
    match call.await.unwrap() {
        Err(HttpError::Status { status: 403, code }) => {
            assert_eq!(code.as_deref(), Some("E_PERM"));
        }
        other => panic!("{other:?}"),
    }

    let h = http.clone();
    let call = tokio::spawn(async move { h.call::<PostGroup>(&[], Raw(vec![0x80])).await });
    script.request(1).await;
    socket.on_stopped(ApiError::local(401, "E_UNAUTHENTICATED", "revoked"));
    match call.await.unwrap() {
        Err(HttpError::Unanswered { code, .. }) => assert_eq!(code, "E_OUTCOME_UNKNOWN"),
        other => panic!("{other:?}"),
    }
}
