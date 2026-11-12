//! Voice media for the desktop client: a LiveKit call whose every frame is `dilla-sframe/1`
//! (protocol/05), sealed and opened by `dilla_core::sframe` behind the encoded-frame hook of the
//! vendored webrtc-sys (`core/vendor/webrtc-sys`, `dilla_transform`).
//!
//! - [`Call::join`] connects to the call's LiveKit room with the token dillad minted for this
//!   device, installs every epoch key the call group hands it ([`EpochKeys`]) and publishes the
//!   microphone; [`Call::set_mic`], [`Call::set_muted`], [`Call::leave`],
//!   [`Call::remote_audio_levels`].
//! - [`transform`]: the per-frame transform (the browser media worker's pipeline in Rust).
//! - [`devices`]: the microphones and speakers.
//! - [`noise`]: the microphone's noise suppression modes (off, standard, enhanced).
//!
//! Fail closed everywhere: no PeerConnection exists without the hook, a frame that cannot be sealed
//! is dropped, a frame that does not authenticate is never decoded.

pub mod devices;
pub mod ffi;
pub mod noise;
pub mod room;
pub mod transform;

pub use room::{Call, CallOptions, LiveKitToken, Mic, RemoteLevels};

/// The call group's media keys, as `ClientCore::call_media_epoch` gives them for each epoch.
pub trait EpochKeys: Send + Sync {
    /// Registers `f`, called with `(epoch, base_key, roster as (leaf, device_id), own_leaf)` for
    /// the current epoch at once (when one is known) and then for every later one. The call stops
    /// publishing until the first call.
    #[allow(clippy::type_complexity)]
    fn on_epoch(&self, f: Box<dyn Fn(u64, [u8; 16], Vec<(u16, [u8; 16])>, u16) + Send + Sync>);

    /// As [`on_epoch`](EpochKeys::on_epoch), for a registration that ends: `f` answers false once
    /// its call has ended, and an implementation that can drops it then ([`EpochFeed`] does). The
    /// default keeps it registered and ignores the answer.
    #[allow(clippy::type_complexity)]
    fn on_epoch_while(
        &self,
        f: Box<dyn Fn(u64, [u8; 16], Vec<(u16, [u8; 16])>, u16) -> bool + Send + Sync>,
    ) {
        self.on_epoch(Box::new(move |e, k, r, l| {
            f(e, k, r, l);
        }));
    }
}

type EpochCallback = Box<dyn Fn(u64, [u8; 16], Vec<(u16, [u8; 16])>, u16) + Send + Sync>;
type WhileCallback = Box<dyn Fn(u64, [u8; 16], Vec<(u16, [u8; 16])>, u16) -> bool + Send + Sync>;

/// One epoch's media keys: `ClientCore::call_media_epoch` decoded (dilla-client's `calls`).
#[derive(Clone)]
pub struct MediaEpoch {
    pub epoch: u64,
    pub base_key: zeroize::Zeroizing<[u8; 16]>,
    pub roster: Vec<(u16, [u8; 16])>,
    pub own_leaf: u16,
}

/// The plain [`EpochKeys`]: whoever follows the call group (dilla-client's call controller)
/// [`publish`](EpochFeed::publish)es each epoch, and every registered call installs it. A call that
/// registers late gets the newest epoch at once. Epochs only move forward: an older one is ignored.
#[derive(Default)]
pub struct EpochFeed {
    inner: std::sync::Mutex<(Option<MediaEpoch>, Vec<WhileCallback>)>,
}

impl EpochFeed {
    pub fn new() -> std::sync::Arc<Self> {
        std::sync::Arc::default()
    }

    pub fn publish(&self, epoch: MediaEpoch) {
        let mut inner = self.inner.lock().unwrap_or_else(|p| p.into_inner());
        if inner.0.as_ref().is_some_and(|e| e.epoch >= epoch.epoch) {
            return;
        }
        // A registration whose call has ended answers false and is dropped.
        let (_, callbacks) = &mut *inner;
        callbacks.retain(|f| {
            f(
                epoch.epoch,
                *epoch.base_key,
                epoch.roster.clone(),
                epoch.own_leaf,
            )
        });
        inner.0 = Some(epoch);
    }

    /// How many registrations are live.
    pub fn registrations(&self) -> usize {
        self.inner.lock().unwrap_or_else(|p| p.into_inner()).1.len()
    }

    /// The newest epoch published.
    pub fn current(&self) -> Option<u64> {
        self.inner
            .lock()
            .unwrap_or_else(|p| p.into_inner())
            .0
            .as_ref()
            .map(|e| e.epoch)
    }
}

impl EpochKeys for EpochFeed {
    fn on_epoch(&self, f: EpochCallback) {
        self.on_epoch_while(Box::new(move |e, k, r, l| {
            f(e, k, r, l);
            true
        }));
    }

    fn on_epoch_while(&self, f: WhileCallback) {
        let mut inner = self.inner.lock().unwrap_or_else(|p| p.into_inner());
        if let Some(e) = &inner.0
            && !f(e.epoch, *e.base_key, e.roster.clone(), e.own_leaf)
        {
            return;
        }
        inner.1.push(f);
    }
}

#[derive(Debug, thiserror::Error)]
pub enum MediaError {
    /// Another encoded-frame hook was installed in this process first.
    #[error("E_DILLA_HOOK: another frame hook owns this process")]
    Hook,
    /// Another call is live in this process (one publisher per device and call, protocol/05 N2).
    #[error("E_CALL_BUSY: another call is live in this process")]
    Busy,
    /// The call has ended.
    #[error("E_CALL_CLOSED: the call has ended")]
    Closed,
    /// A transformer handle was bound twice.
    #[error("E_ALREADY_BOUND: transformer {0} is already bound")]
    AlreadyBound(u64),
    /// A key could not be installed.
    #[error("E_KEYS: {0}")]
    Keys(String),
    /// No key arrived in time: nothing was published.
    #[error("E_NO_EPOCH: no media key arrived")]
    NoEpoch,
    /// LiveKit refused or lost the connection.
    #[error("E_ROOM: {0}")]
    Room(String),
    /// An audio device or processor failed.
    #[error("E_AUDIO: {0}")]
    Audio(String),
    /// The request is not possible on this call (for example switching between microphone kinds,
    /// which would replace a sender's transformer).
    #[error("E_UNSUPPORTED: {0}")]
    Unsupported(String),
}
