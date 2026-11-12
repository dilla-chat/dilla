//! The dilla hook is mandatory (core/vendor/webrtc-sys, DILLA PATCH in
//! `PeerConnectionFactory::create_peer_connection`): without it no PeerConnection is created, so
//! there is no fallback to LiveKit's own FrameCryptor or to no transform at all.
//!
//! One test in its own binary: the hook is process-wide and can only be installed once, so the
//! "before" and "after" halves run in order here.

use gpui_voice_proof::media::{Hook, ProofMedia};
use livekit::webrtc::peer_connection_factory::{PeerConnectionFactory, RtcConfiguration};

#[test]
fn a_peer_connection_without_the_hook_is_refused() {
    let factory = PeerConnectionFactory::default();
    let Err(refused) = factory.create_peer_connection(RtcConfiguration::default()) else {
        panic!("a PeerConnection must not be created without the dilla hook");
    };
    assert!(
        refused.message.contains("E_DILLA_NO_FRAME_HOOK"),
        "{refused:?}"
    );

    assert!(webrtc_sys::dilla_transform::install(Box::new(Hook(
        ProofMedia::new(0)
    ))));
    assert!(
        factory
            .create_peer_connection(RtcConfiguration::default())
            .is_ok(),
        "with the hook installed the same factory creates one"
    );
}
