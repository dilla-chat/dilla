//! A scripted gateway socket for unit tests: it keeps every request frame written, and the test answers them.

use std::sync::Arc;
use std::sync::Mutex as StdMutex;
use std::time::Duration;

use dilla_core::cbor::Encoder;

use super::{RequestSink, SocketRequests};
use crate::gateway::frames;
use crate::seams::GatewayError;

/// A scripted socket: every frame written is kept, and the test answers it.
#[derive(Default)]
pub(crate) struct Script {
    frames: StdMutex<Vec<Vec<u8>>>,
    written: tokio::sync::Notify,
}

impl RequestSink for Script {
    fn send_now(&self, frame: Vec<u8>) -> Result<(), GatewayError> {
        self.frames.lock().unwrap().push(frame);
        self.written.notify_waiters();
        Ok(())
    }
}

impl Script {
    /// The `n`th request frame written (0-based), once it is: its id and its idempotency key.
    pub(crate) async fn request(&self, n: usize) -> (u64, Option<Vec<u8>>) {
        loop {
            let wait = self.written.notified();
            if let Some(f) = self.frames.lock().unwrap().get(n) {
                let f = frames::decode(f).expect("a request frame");
                assert_eq!(f.op, frames::Op::REQUEST);
                let id = f.payload[0].as_uint().unwrap();
                return (id, f.payload[4].as_bytes().map(<[u8]>::to_vec));
            }
            tokio::time::timeout(Duration::from_secs(5), wait)
                .await
                .expect("a request frame is written");
        }
    }

    /// The body of the `n`th request frame written.
    pub(crate) fn body(&self, n: usize) -> Vec<u8> {
        let frames = self.frames.lock().unwrap();
        let f = frames::decode(&frames[n]).expect("a request frame");
        f.payload[3].as_bytes().expect("a body").to_vec()
    }

    /// The route (path and query) of the `n`th request frame written.
    pub(crate) fn route(&self, n: usize) -> String {
        let frames = self.frames.lock().unwrap();
        let f = frames::decode(&frames[n]).expect("a request frame");
        f.payload[2].as_text().expect("a route").to_owned()
    }

    pub(crate) fn written(&self) -> usize {
        self.frames.lock().unwrap().len()
    }
}

pub(crate) fn response(id: u64, status: u16, body: &[u8]) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(4)
        .uint(u64::from(frames::Op::RESPONSE))
        .uint(0)
        .null()
        .array(3)
        .uint(id)
        .uint(u64::from(status))
        .bytes(body);
    e.into_vec()
}

pub(crate) fn refusal(code: &str, delay: Option<u64>) -> Vec<u8> {
    let mut e = Encoder::new();
    e.array(3).text(code).text("");
    match delay {
        Some(d) => e.uint(d),
        None => e.null(),
    };
    e.into_vec()
}

/// A transport over `script` whose socket is up.
pub(crate) fn up(script: &Arc<Script>) -> Arc<SocketRequests> {
    let socket = SocketRequests::new();
    socket.attach_sink(Some(script.clone() as Arc<dyn RequestSink>));
    socket.on_ready("incarnation".into());
    socket
}
