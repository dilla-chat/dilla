//! The process's one call-media slot, shared by the real-instance call tests (tests/voice.rs, tests/share.rs; each
//! includes this file as `media_slot`).
//!
//! `dilla_media`'s frame hook routes one live call per process (N2): a second window's media in the same process fails
//! with `E_CALL_BUSY`, and the desktop then leaves that call (its media did not start). The real-instance tests run on
//! parallel threads of one test binary, so a test whose media lost the slot to another test's left its own call; alone
//! in it, the leave ended the call and closed its group, and bob's join built on that group was refused
//! (`E_COMMIT_INVALID`: the delivery service refuses a commit to a closed group). Each real-instance test holds the slot
//! for its whole run, and first waits for an earlier test's media to be gone (a window's media session can outlive its
//! test's world).

#![allow(dead_code)]

use std::sync::{Mutex, MutexGuard};
use std::time::{Duration, Instant};

static MEDIA_SLOT: Mutex<()> = Mutex::new(());

/// How long a test waits for an earlier test's call media to end.
pub const MEDIA_FREE_WAIT: Duration = Duration::from_secs(30);

/// A held [`MEDIA_SLOT`].
pub struct MediaSlot {
    _guard: MutexGuard<'static, ()>,
}

/// Takes the process's media slot ([`MEDIA_SLOT`]) once no call media is live in this process: the router is free and
/// no PeerConnection is left either (an earlier test's room may still be closing on its session's `dilla-call-end`
/// thread after the router was freed).
pub fn media_slot() -> MediaSlot {
    let guard = MEDIA_SLOT.lock().unwrap_or_else(|p| p.into_inner());
    assert!(
        media_free_within(MEDIA_FREE_WAIT),
        "an earlier test's call media is still live in this process after {MEDIA_FREE_WAIT:?}"
    );
    let deadline = Instant::now() + MEDIA_FREE_WAIT;
    loop {
        let live = dilla_media::ffi::live_transformers();
        if live == 0 {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "{live} frame transformers of an earlier test's call are still alive after {MEDIA_FREE_WAIT:?}"
        );
        std::thread::sleep(Duration::from_millis(50));
    }
    MediaSlot { _guard: guard }
}

/// True once no call is current in this process's media router, polled until `wait` has passed. The probe is the
/// router's own claim, released at once: only a holder of [`MEDIA_SLOT`] probes, and every test that starts call media
/// holds it.
pub fn media_free_within(wait: Duration) -> bool {
    let router = dilla_media::transform::Router::get().expect("the dilla frame hook installs");
    let deadline = Instant::now() + wait;
    loop {
        let probe = dilla_media::transform::CallMedia::new();
        match router.begin(probe.clone()) {
            Ok(()) => {
                router.end(&probe);
                return true;
            }
            Err(dilla_media::MediaError::Busy) => {}
            Err(e) => panic!("the media router refused the probe: {e}"),
        }
        if Instant::now() >= deadline {
            return false;
        }
        std::thread::sleep(Duration::from_millis(50));
    }
}
