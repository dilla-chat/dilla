//! The C++ glue survives the process's own exit (core/vendor/webrtc-sys, DILLA PATCH). The hook's
//! hold timer (`dilla-hold-tick`) used to be a detached thread that walked the transformer registry
//! every 20 ms, and the registry was a namespace-scope `std::map` that `exit()` destroyed: with a
//! PeerConnection still alive at exit (its transformers still registered), the timer walked freed
//! nodes, a SIGSEGV after every test had passed (dilla-desktop `--test voice`, about 1 run in 3 under
//! load).
//!
//! Each run is a child process (this binary again, `DILLA_HOOK_EXIT_CHILD`) that installs the hook,
//! leaves a PeerConnection with many transceivers alive and returns. On Linux its executable's
//! `.fini_array` holds the process in `_dl_fini`, after every C++ static destructor has run, for
//! several timer ticks: the window in which the voice tests crashed, held open on purpose. The parent
//! requires every child to exit cleanly.
//!
//! One test in its own binary: the hook is process-wide and installs once.

use std::process::Command;
use std::sync::atomic::{AtomicBool, Ordering};

use dilla_media::ffi::{FrameInfo, FrameTransform, Verdict};
use livekit::webrtc::MediaType;
use livekit::webrtc::peer_connection_factory::{PeerConnectionFactory, RtcConfiguration};
use livekit::webrtc::prelude::{RtpTransceiverDirection, RtpTransceiverInit};

const CHILD: &str = "DILLA_HOOK_EXIT_CHILD";
const RUNS: usize = 5;
const TRANSCEIVERS: usize = 64;

/// Set in a child once its hook and transformers are in place; the `.fini_array` wait runs only then.
static ARMED: AtomicBool = AtomicBool::new(false);

/// Runs in `_dl_fini`, after `exit()` has run the C++ static destructors: the hold timer, had it
/// survived them, would tick several times here. The only unsafe here is the section placement: a
/// `.fini_array` entry is a plain `extern "C" fn()` the loader calls once.
#[cfg(target_os = "linux")]
#[allow(unsafe_code)]
#[used]
#[unsafe(link_section = ".fini_array")]
static AFTER_STATIC_DESTRUCTORS: extern "C" fn() = {
    extern "C" fn wait_past_the_destructors() {
        if ARMED.load(Ordering::SeqCst) {
            std::thread::sleep(webrtc_sys::dilla_transform::HOLD_TICK * 10);
        }
    }
    wait_past_the_destructors
};

struct Nothing;

impl FrameTransform for Nothing {
    fn attached(&self, _handle: u64, _send: bool, _video: bool, _track_id: &str) {}
    fn detached(&self, _handle: u64) {}
    fn transform(&self, _info: &FrameInfo<'_>, _data: &[u8], _out: &mut Vec<u8>) -> Verdict {
        Verdict::Drop
    }
    fn hold_expired(&self, _handle: u64, _send: bool) {}
    fn pass_refused(&self, _handle: u64, _send: bool) {}
}

fn child() {
    assert!(webrtc_sys::dilla_transform::install(Box::new(Nothing)));
    let factory = PeerConnectionFactory::default();
    let pc = factory
        .create_peer_connection(RtcConfiguration::default())
        .unwrap();
    for _ in 0..TRANSCEIVERS {
        pc.add_transceiver_for_media(
            MediaType::Audio,
            RtpTransceiverInit {
                direction: RtpTransceiverDirection::SendRecv,
                stream_ids: Vec::new(),
                send_encodings: Vec::new(),
            },
        )
        .unwrap();
    }
    // Alive at exit, as a call's PeerConnection is when the app quits mid-call: its transformers
    // stay registered while the process tears down.
    std::mem::forget(pc);
    std::mem::forget(factory);
    ARMED.store(true, Ordering::SeqCst);
}

#[test]
fn the_process_exits_cleanly_with_the_hold_timer_running_and_a_call_alive() {
    if std::env::var_os(CHILD).is_some() {
        child();
        return;
    }
    let exe = std::env::current_exe().unwrap();
    for run in 0..RUNS {
        let status = Command::new(&exe)
            .args([
                "--exact",
                "the_process_exits_cleanly_with_the_hold_timer_running_and_a_call_alive",
                "--test-threads=1",
            ])
            .env(CHILD, "1")
            .status()
            .unwrap();
        assert!(
            status.success(),
            "run {run}: the child exited with {status}"
        );
    }
}
