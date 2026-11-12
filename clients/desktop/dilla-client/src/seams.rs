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
}

#[derive(Debug, thiserror::Error)]
pub enum HttpError {
    #[error("the server answered with status {0}")]
    Status(u16),
    #[error("transport failure: {0}")]
    Transport(String),
    #[error("undecodable response: {0}")]
    Decode(String),
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
