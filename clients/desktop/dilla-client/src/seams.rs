//! The interfaces the desktop client's later tasks implement. Their signatures are fixed: change one only
//! with the controller.

use std::path::PathBuf;

use zeroize::Zeroizing;

/// One running copy of the client (0..=31), so several instances can share a machine.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub struct InstanceId(pub u8);

#[derive(Clone, Debug)]
pub struct InstanceConfig {
    pub id: InstanceId,
    pub server: url::Url,
    pub data_dir: PathBuf,
}

#[derive(Debug, thiserror::Error)]
pub enum KeychainError {
    #[error("the platform keychain is unavailable")]
    Unavailable,
}

pub trait Keychain: Send + Sync {
    fn get(&self, account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError>;
    fn set(&self, account: &str, secret: &[u8]) -> Result<(), KeychainError>;
    fn delete(&self, account: &str) -> Result<(), KeychainError>;

    /// [`Keychain::get`] that never asks the person: no unlock dialog, no access prompt. A secret the store holds but
    /// would have to ask for (a locked keyring) is [`KeychainError::Unavailable`]. What a start with no window (an
    /// autostart at login) may use. A store that cannot tell whether it would ask answers `Unavailable` (the default).
    fn get_quietly(&self, _account: &str) -> Result<Option<Zeroizing<Vec<u8>>>, KeychainError> {
        Err(KeychainError::Unavailable)
    }
}

#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum HttpError {
    /// A status the route does not accept. `code` is the instance's `E_*` code from the refusal's body
    /// (`[code, detail, delay|null, ...extra]`, internal/server/errors.go, read as http/errors.ts `decodeErrorBody`
    /// reads it); `None` when the body is not a dilla error (errors.ts names that `E_HTTP`). `retry_after_ms` is the
    /// body's delay (errors.ts `retryAfterMs`), when it names one.
    #[error("the server answered with status {status} ({})", code.as_deref().unwrap_or("E_HTTP"))]
    Status {
        status: u16,
        code: Option<String>,
        retry_after_ms: Option<u64>,
    },
    #[error("transport failure: {0}")]
    Transport(String),
    #[error("undecodable response: {0}")]
    Decode(String),
    /// No answer, with the client's own `E_*` code for why (client.ts's status-0 errors): `E_NETWORK` (the request
    /// never reached the instance) or `E_OUTCOME_UNKNOWN` (a written mutation whose answer was lost: it may have
    /// been applied, so a caller must not undo what it staged for it).
    #[error("no answer ({code}): {detail}")]
    Unanswered { code: String, detail: String },
}

impl HttpError {
    /// `Status { status, code }`: the instance refused with `status` and the `E_*` code `code`.
    pub fn is(&self, status: u16, code: &str) -> bool {
        matches!(self, HttpError::Status { status: s, code: Some(c), .. } if *s == status && c == code)
    }

    /// How long the instance asked to wait before a retry (a refusal's delay), if it named one.
    pub fn retry_after_ms(&self) -> Option<u64> {
        match self {
            HttpError::Status { retry_after_ms, .. } => *retry_after_ms,
            _ => None,
        }
    }

    /// The refusal's status, if this is one.
    pub fn status(&self) -> Option<u16> {
        match self {
            HttpError::Status { status, .. } => Some(*status),
            _ => None,
        }
    }
}

pub trait Route {
    const METHOD: http::Method;
    const PATH: &'static str;
    type Request: serde::Serialize + Send;
    type Response: serde::de::DeserializeOwned + Send;
}

#[async_trait::async_trait]
pub trait Http: Send + Sync {
    async fn call<R: Route>(
        &self,
        path_args: &[&str],
        req: R::Request,
    ) -> Result<R::Response, HttpError>;
}

#[derive(Debug, thiserror::Error)]
#[non_exhaustive]
pub enum GatewayError {
    #[error("the gateway connection is closed")]
    Closed,
    /// Task 8: the frame is over the connection's bound (the hello's `max_frame_bytes`; a request frame's own cap), so
    /// it was refused before it was sent. The instance would close the socket on it (E_FRAME_LIMIT).
    #[error("the frame is {len} bytes, over the gateway's bound of {max}")]
    TooLarge { len: usize, max: usize },
    /// Task 9: the writer queue already holds `queued` bytes and this frame would pass its bound
    /// (`gateway::WRITER_QUEUE_CAP`); nothing was queued. The caller keeps the frame and sends it later.
    #[error("the gateway's writer queue is full ({queued} of {cap} bytes queued)")]
    Full { queued: usize, cap: usize },
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DisconnectReason {
    Network,
    Server(u16),
    Shutdown,
}

#[derive(Debug)]
pub enum GatewayEvent {
    Connected { session: Vec<u8> },
    Frame(Vec<u8>),
    Disconnected { reason: DisconnectReason },
}

#[async_trait::async_trait]
pub trait Gateway: Send + Sync {
    async fn send(&self, frame: Vec<u8>) -> Result<(), GatewayError>;
    fn events(&self) -> tokio::sync::mpsc::Receiver<GatewayEvent>;
}
