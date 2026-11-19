// DILLA PATCH (core/vendor/README.md): this whole file is dilla's, not LiveKit's.
//
// Test support for the macOS camera: the authorisation gate of `dilla_camera_mac.mm` with a forced
// status instead of the machine's TCC state. Built only with the `dilla-test-frames` feature on
// macOS, which only dilla-media's dev-dependency enables, so no shipping binary contains it.

use std::sync::{Mutex, MutexGuard};

#[cxx::bridge(namespace = "livekit_ffi")]
mod ffi {
    unsafe extern "C++" {
        include!("livekit/dilla_camera_testing.h");

        fn dilla_camera_test_force(status: i32, grants: bool);
        fn dilla_camera_test_requests() -> u64;
        fn dilla_camera_test_refusals() -> u64;
        fn dilla_camera_test_starts() -> u64;
    }
}

/// `AVAuthorizationStatus`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
#[repr(i32)]
pub enum Authorization {
    NotDetermined = 0,
    Restricted = 1,
    Denied = 2,
    Authorized = 3,
}

/// While a status is forced, this id opens a camera with no device: `start` is counted ([`Forced::starts`]) and fails.
pub const TEST_CAMERA: &str = "dilla-test-camera";

static FORCED: Mutex<()> = Mutex::new(());

/// The status forced while this lives (one at a time in the process); the real one after.
pub struct Forced {
    _one_at_a_time: MutexGuard<'static, ()>,
}

/// Forces `status` for every camera open in the process, a request answering `grants`.
pub fn force(status: Authorization, grants: bool) -> Forced {
    let guard = FORCED.lock().unwrap_or_else(|p| p.into_inner());
    ffi::dilla_camera_test_force(status as i32, grants);
    Forced {
        _one_at_a_time: guard,
    }
}

impl Forced {
    /// The system requests the gate made since [`force`].
    pub fn requests(&self) -> u64 {
        ffi::dilla_camera_test_requests()
    }

    /// The opens the gate refused since [`force`].
    pub fn refusals(&self) -> u64 {
        ffi::dilla_camera_test_refusals()
    }

    /// The cameras started (`DillaCamera::start`) since [`force`].
    pub fn starts(&self) -> u64 {
        ffi::dilla_camera_test_starts()
    }
}

impl Drop for Forced {
    fn drop(&mut self) {
        ffi::dilla_camera_test_force(-1, false);
    }
}
