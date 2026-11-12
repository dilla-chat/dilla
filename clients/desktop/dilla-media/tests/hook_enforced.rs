//! The C++ glue's own guarantees, whatever the Rust hook answers (core/vendor/webrtc-sys, DILLA
//! PATCH), with a deliberately wrong hook that answers `PassUnchanged` to every frame:
//!
//! - no PeerConnection is created before a hook is installed (`E_DILLA_NO_FRAME_HOOK`);
//! - `PassUnchanged` forwards only a zero-byte Opus frame on an audio transformer; every other frame
//!   so answered is dropped (and reported), so a broken hook cannot put plaintext on the wire;
//! - every transformer reaches Rust with a fresh handle, and its end is reported.
//!
//! One test in its own binary: the hook is process-wide and installs once.

use std::collections::BTreeSet;
use std::sync::Mutex;

use dilla_media::ffi::testing;
use dilla_media::ffi::{FrameInfo, FrameTransform, Verdict};
use livekit::webrtc::peer_connection_factory::{PeerConnectionFactory, RtcConfiguration};

#[derive(Default)]
struct Seen {
    attached: BTreeSet<u64>,
    detached: BTreeSet<u64>,
    refused: u64,
}

static SEEN: Mutex<Option<Seen>> = Mutex::new(None);

/// What a broken transform would do: pass everything unchanged.
struct PassEverything;

impl FrameTransform for PassEverything {
    fn attached(&self, handle: u64, _send: bool, _video: bool, _track_id: &str) {
        SEEN.lock()
            .unwrap()
            .get_or_insert_default()
            .attached
            .insert(handle);
    }
    fn detached(&self, handle: u64) {
        SEEN.lock()
            .unwrap()
            .get_or_insert_default()
            .detached
            .insert(handle);
    }
    fn transform(&self, _info: &FrameInfo<'_>, _data: &[u8], _out: &mut Vec<u8>) -> Verdict {
        Verdict::PassUnchanged
    }
    fn hold_expired(&self, _handle: u64, _send: bool) {}
    fn pass_refused(&self, _handle: u64, _send: bool) {
        SEEN.lock().unwrap().get_or_insert_default().refused += 1;
    }
}

#[test]
fn the_glue_refuses_a_peer_connection_without_a_hook_and_any_content_bearing_pass() {
    // RED evidence (Task 2): without the check the PeerConnection was created.
    let factory = PeerConnectionFactory::default();
    let Err(refused) = factory.create_peer_connection(RtcConfiguration::default()) else {
        panic!("a PeerConnection must not be created without the dilla hook");
    };
    assert!(
        refused.message.contains("E_DILLA_NO_FRAME_HOOK"),
        "{refused:?}"
    );
    assert_eq!(
        testing::attach(true, false, "mic"),
        0,
        "no transformer without a hook"
    );

    assert!(webrtc_sys::dilla_transform::install(Box::new(
        PassEverything
    )));
    assert!(
        !webrtc_sys::dilla_transform::install(Box::new(PassEverything)),
        "exactly one hook for the process life"
    );
    assert!(
        factory
            .create_peer_connection(RtcConfiguration::default())
            .is_ok()
    );

    for send in [true, false] {
        let audio = testing::attach(send, false, "mic");
        let video = testing::attach(send, true, "cam");
        assert!(audio != 0 && video != 0 && audio != video);

        // Content on an audio transformer: dropped although the hook said pass.
        testing::frame(audio, "audio/opus", &[0xfc, 0xff, 0xfe, 1, 2, 3], false);
        // A zero-byte frame of a codec with no prefix rule: dropped.
        testing::frame(audio, "audio/red", &[], false);
        // A zero-byte Opus frame (DTX) carries no content: forwarded.
        testing::frame(audio, "audio/opus", &[], false);
        assert_eq!(
            testing::take_emitted(audio),
            vec![Vec::<u8>::new()],
            "send={send}"
        );

        // Video, empty or not, is never passed unchanged.
        testing::frame(
            video,
            "video/VP8",
            &[0x50, 0x02, 0x00, 0x9d, 0x01, 0x2a],
            true,
        );
        testing::frame(video, "video/VP8", &[], false);
        assert!(testing::take_emitted(video).is_empty(), "send={send}");

        testing::release(audio);
        testing::release(video);
        let seen = SEEN.lock().unwrap();
        let seen = seen.as_ref().unwrap();
        assert!(seen.attached.contains(&audio) && seen.detached.contains(&audio));
        assert!(seen.attached.contains(&video) && seen.detached.contains(&video));
    }
    // Four refused passes per direction: the Opus frame with content, the empty RED frame and both
    // video frames.
    assert_eq!(SEEN.lock().unwrap().as_ref().unwrap().refused, 8);
}
