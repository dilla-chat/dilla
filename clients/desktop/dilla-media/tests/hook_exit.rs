//! The C++ glue survives the process's own exit (core/vendor/webrtc-sys, DILLA PATCH). The hook's
//! hold timer (`dilla-hold-tick`) used to be a detached thread that walked the transformer registry
//! every 20 ms, and the registry was a namespace-scope `std::map` that `exit()` destroyed: with a
//! PeerConnection still alive at exit (its transformers still registered), the timer walked freed
//! nodes, a SIGSEGV after every test had passed (dilla-desktop `--test voice`, about 1 run in 3 under
//! load).
//!
//! Each run is a child process (this binary again, `DILLA_HOOK_EXIT_CHILD` naming the test it runs)
//! that installs the hook, leaves a PeerConnection with many transceivers alive and returns. On Linux
//! its executable's `.fini_array` holds the process in `_dl_fini`, after every C++ static destructor
//! has run, for several timer ticks: the window in which the voice tests crashed, held open on
//! purpose. The parent requires every child to exit cleanly.
//!
//! A timer thread that cannot start (Linux: a soft `RLIMIT_NPROC` of 0 around the install) leaves the
//! hook installed and working, and the process still exits cleanly.
//!
//! The hook is process-wide and installs once: every test does its work in its own child.

use std::process::{Command, Stdio};
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

/// Whether a thread named `dilla-hold-tick` runs in this process (Linux: `/proc/self/task/*/comm`).
#[cfg(target_os = "linux")]
fn hold_timer_running() -> bool {
    std::fs::read_dir("/proc/self/task").unwrap().any(|task| {
        std::fs::read_to_string(task.unwrap().path().join("comm"))
            .is_ok_and(|comm| comm.trim() == "dilla-hold-tick")
    })
}

/// A call's PeerConnection, alive at exit as one is when the app quits mid-call: its transformers
/// stay registered while the process tears down.
fn leave_a_call_alive() {
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
    std::mem::forget(pc);
    std::mem::forget(factory);
    ARMED.store(true, Ordering::SeqCst);
}

/// Runs `test` of this binary in a child `RUNS` times; every child must exit cleanly. The children's
/// output stays out of this test's; a failing child's stderr is in the failure.
fn every_child_exits_cleanly(test: &str) {
    let exe = std::env::current_exe().unwrap();
    for run in 0..RUNS {
        let out = Command::new(&exe)
            .args(["--exact", test, "--test-threads=1", "--nocapture"])
            .env(CHILD, test)
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::piped())
            .output()
            .unwrap();
        assert!(
            out.status.success(),
            "run {run}: the child exited with {}; its stderr:\n{}",
            out.status,
            String::from_utf8_lossy(&out.stderr)
        );
    }
}

fn in_child(test: &str) -> bool {
    std::env::var_os(CHILD).is_some_and(|t| t == test)
}

#[test]
fn the_process_exits_cleanly_with_the_hold_timer_running_and_a_call_alive() {
    const TEST: &str = "the_process_exits_cleanly_with_the_hold_timer_running_and_a_call_alive";
    if !in_child(TEST) {
        return every_child_exits_cleanly(TEST);
    }
    assert!(webrtc_sys::dilla_transform::install(Box::new(Nothing)));
    #[cfg(target_os = "linux")]
    assert!(hold_timer_running(), "the hold timer started");
    leave_a_call_alive();
}

/// Linux only: the thread limit is how the timer is kept from starting.
#[cfg(target_os = "linux")]
#[test]
fn a_hold_timer_that_cannot_start_leaves_the_hook_installed_and_the_exit_clean() {
    const TEST: &str =
        "a_hold_timer_that_cannot_start_leaves_the_hook_installed_and_the_exit_clean";
    if !in_child(TEST) {
        return every_child_exits_cleanly(TEST);
    }
    #[allow(unsafe_code)]
    let set_soft_limit = |soft: libc::rlim_t| {
        let mut limit = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        // SAFETY: getrlimit/setrlimit on a valid, initialized rlimit of this stack frame.
        unsafe {
            assert_eq!(libc::getrlimit(libc::RLIMIT_NPROC, &mut limit), 0);
            let before = limit.rlim_cur;
            limit.rlim_cur = soft;
            assert_eq!(libc::setrlimit(libc::RLIMIT_NPROC, &limit), 0);
            before
        }
    };
    let before = set_soft_limit(0);
    // With CAP_SYS_RESOURCE (root) the limit is not enforced and the check below says nothing.
    let enforced = std::thread::Builder::new().spawn(|| {}).is_err();
    let installed = webrtc_sys::dilla_transform::install(Box::new(Nothing));
    set_soft_limit(before);
    assert!(installed, "the hook is installed without its timer");
    assert!(webrtc_sys::dilla_transform::installed());
    if enforced {
        assert!(!hold_timer_running(), "no timer thread could start");
    }
    leave_a_call_alive();
}
