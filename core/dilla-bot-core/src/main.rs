#![allow(unsafe_code)] // Linux process hardening and inherited-fd cleanup require libc calls.

use dilla_bot_core::{Helper, serve_fd3};
use std::path::PathBuf;

fn main() {
    if suppress_database_diagnostics().is_err() {
        std::process::exit(70)
    }
    if harden_process().is_err() {
        std::process::exit(70)
    }
    let mut args = std::env::args_os().skip(1);
    let state = match (args.next(), args.next(), args.next()) {
        (Some(flag), Some(path), None) if flag == "--state-dir" => PathBuf::from(path),
        _ => std::process::exit(64),
    };
    if serve_fd3(Helper::new(state)).is_err() {
        std::process::exit(70)
    }
}

fn suppress_database_diagnostics() -> Result<(), ()> {
    type SqliteLog = unsafe extern "C" fn(*mut libc::c_void, libc::c_int, *const libc::c_char);
    // A null SQLite log callback suppresses SQLite's default stderr diagnostics.
    let rc = unsafe {
        rusqlite::ffi::sqlite3_config(
            rusqlite::ffi::SQLITE_CONFIG_LOG,
            None::<SqliteLog>,
            std::ptr::null_mut::<libc::c_void>(),
        )
    };
    if rc == rusqlite::ffi::SQLITE_OK {
        Ok(())
    } else {
        Err(())
    }
}

fn harden_process() -> Result<(), ()> {
    unsafe {
        libc::umask(0o077);
        let lim = libc::rlimit {
            rlim_cur: 0,
            rlim_max: 0,
        };
        if libc::setrlimit(libc::RLIMIT_CORE, &lim) != 0 {
            return Err(());
        }
        if libc::prctl(libc::PR_SET_DUMPABLE, 0, 0, 0, 0) != 0 {
            return Err(());
        }
        if libc::syscall(libc::SYS_close_range, 4u32, u32::MAX, 0u32) != 0 {
            return Err(());
        }
    }
    Ok(())
}
