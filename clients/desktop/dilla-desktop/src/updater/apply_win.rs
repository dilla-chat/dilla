//! Windows (release plan 14b, D10): the update is the release's per-user Inno Setup installer
//! (`dilla-<v>-win-x64-setup.exe`, Task 13). It is downloaded into a fresh staging directory under the user's
//! temporary directory, its size and SHA-512 are checked again right before it runs, and it runs with
//! `/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /relaunch=1` while the app exits. The installer replaces the files under
//! `%LOCALAPPDATA%\Programs\Dilla` and, for `/relaunch=1`, starts the new app (Task 13's `[Run]` entry).
//!
//! Only the per-user install updates itself; any other copy (a debug build, a copy run from elsewhere) shows the release
//! page instead.

#![cfg_attr(not(target_os = "windows"), allow(dead_code))]

use std::path::{Path, PathBuf};
use std::process::Command;

use super::{Error, Installed, RELAUNCHED};

/// The installer's arguments: silent, no message boxes, no reboot, and start Dilla again when done.
pub const SETUP_ARGS: [&str; 4] = [
    "/VERYSILENT",
    "/SUPPRESSMSGBOXES",
    "/NORESTART",
    "/relaunch=1",
];

/// Whether `exe` is the per-user install's binary: `<local app data>\Programs\Dilla\dilla-desktop.exe`.
pub fn per_user_install(exe: &Path, local_app_data: &Path) -> bool {
    exe.parent()
        .is_some_and(|dir| dir == local_app_data.join("Programs").join("Dilla"))
}

/// How this copy is installed.
pub fn installed() -> Installed {
    let local = std::env::var_os("LOCALAPPDATA").map(PathBuf::from);
    let ours = match (std::env::current_exe(), local) {
        (Ok(exe), Some(local)) => per_user_install(&exe, &local),
        _ => false,
    };
    if ours && !cfg!(debug_assertions) {
        Installed::SelfUpdating {
            staging: std::env::temp_dir(),
        }
    } else {
        Installed::Manual
    }
}

/// The checked installer runs as it is.
pub fn prepare(downloaded: &Path) -> Result<PathBuf, Error> {
    Ok(downloaded.to_owned())
}

/// Starts the installer; the app is already quitting.
pub fn apply(setup: &Path) -> Result<(), Error> {
    Command::new(setup).args(SETUP_ARGS).spawn()?;
    Ok(())
}

/// Starts this copy again (an update that could not be applied: the app comes back as it was).
pub fn relaunch_current() {
    if let Ok(exe) = std::env::current_exe() {
        let _ = Command::new(exe).arg(RELAUNCHED).spawn();
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn only_the_per_user_install_updates_itself() {
        let local = Path::new("/home/u/AppData/Local");
        assert!(per_user_install(
            &local
                .join("Programs")
                .join("Dilla")
                .join("dilla-desktop.exe"),
            local
        ));
        assert!(!per_user_install(
            &local
                .join("Programs")
                .join("Other")
                .join("dilla-desktop.exe"),
            local
        ));
        assert!(!per_user_install(
            Path::new("/tmp/dilla-desktop.exe"),
            local
        ));
    }

    #[test]
    fn the_installer_runs_silently_and_relaunches() {
        assert_eq!(SETUP_ARGS[0], "/VERYSILENT");
        assert!(SETUP_ARGS.contains(&"/SUPPRESSMSGBOXES"));
        assert!(SETUP_ARGS.contains(&"/relaunch=1"));
    }
}
