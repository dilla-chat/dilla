//! A `cargo` command for a test that builds or runs something itself.
//!
//! Cargo gives every test process the per-crate variables of the crate under test
//! (`CARGO_MANIFEST_DIR`, `CARGO_PKG_*`, `CARGO_CRATE_NAME`, ...). A nested `cargo build` that
//! inherits them records them in the fingerprint of every build script that tracks one: `ring`'s
//! build script tracks `CARGO_MANIFEST_DIR`. The next outer `cargo build` then sees a different
//! value, re-runs `ring`'s build script and rebuilds everything above it, the vendored webrtc C++
//! build included (about 40 minutes), and the outer and nested builds keep invalidating each other.
//! [`command`] removes those variables and keeps the user's real cargo configuration
//! (`CARGO`, `CARGO_HOME`, `CARGO_TARGET_DIR`, `CARGO_BUILD_*`, `RUSTFLAGS`, `CC`, ...).
//!
//! `dilla-parity` carries a copy of this (`clients/desktop/parity/src/driver.rs`): it has no
//! dependency on the testkit, and gains none for a helper this small. Keep the two in step.

use std::ffi::OsStr;
use std::process::Command;

/// Whether cargo sets `name` for a running test, build script or `cargo run` child, rather than the
/// user setting it.
fn is_per_crate(name: &OsStr) -> bool {
    let Some(name) = name.to_str() else {
        return false;
    };
    const EXACT: &[&str] = &[
        "CARGO_MANIFEST_DIR",
        "CARGO_MANIFEST_PATH",
        "CARGO_MANIFEST_LINKS",
        "CARGO_CRATE_NAME",
        "CARGO_BIN_NAME",
        "CARGO_TARGET_TMPDIR",
        "CARGO_PRIMARY_PACKAGE",
        "OUT_DIR",
    ];
    const PREFIXES: &[&str] = &["CARGO_PKG_", "CARGO_BIN_EXE_", "CARGO_CFG_"];
    EXACT.contains(&name) || PREFIXES.iter().any(|p| name.starts_with(p))
}

/// `$CARGO` (or `cargo`) with the per-crate variables of the calling test removed, so a nested build
/// does not invalidate the outer build's cache. See the module docs.
pub fn command() -> Command {
    let mut cargo = Command::new(std::env::var_os("CARGO").unwrap_or_else(|| "cargo".into()));
    for (name, _) in std::env::vars_os() {
        if is_per_crate(&name) {
            cargo.env_remove(name);
        }
    }
    cargo
}

#[cfg(test)]
mod tests {
    use super::is_per_crate;
    use std::ffi::OsStr;

    #[test]
    fn per_crate_variables_go_and_the_users_configuration_stays() {
        for gone in [
            "CARGO_MANIFEST_DIR",
            "CARGO_PKG_VERSION",
            "CARGO_BIN_EXE_dilla-bot-core",
            "CARGO_CRATE_NAME",
            "CARGO_TARGET_TMPDIR",
            "OUT_DIR",
            "CARGO_CFG_TARGET_OS",
        ] {
            assert!(is_per_crate(OsStr::new(gone)), "{gone}");
        }
        for kept in [
            "CARGO",
            "CARGO_HOME",
            "CARGO_TARGET_DIR",
            "CARGO_BUILD_JOBS",
            "CARGO_INCREMENTAL",
            "CARGO_NET_OFFLINE",
            "RUSTFLAGS",
            "CC",
            "CXX",
        ] {
            assert!(!is_per_crate(OsStr::new(kept)), "{kept}");
        }
    }
}
