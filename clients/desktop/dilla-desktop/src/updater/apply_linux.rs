//! Linux: only an AppImage replaces itself (release plan 14b). `$APPIMAGE` names the running image; the update is
//! downloaded into a staging directory beside it (the same file system), made executable, checked again and renamed
//! over it in one step (`rename(2)` is atomic: the path holds the old image or the new one, never part of either), then
//! the new image is started. The running process keeps its own mounted image until it exits.
//!
//! A deb, rpm, AUR or tar.gz install belongs to its package manager or to the person who unpacked it: the updater only
//! says that a new version is available ([`super::Installed::PackageManager`], [`super::Installed::Manual`]).

#![cfg_attr(any(target_os = "macos", target_os = "windows"), allow(dead_code))]

use std::path::{Path, PathBuf};
use std::process::Command;

use super::{Error, Installed, RELAUNCHED};

/// Where the deb and rpm packages install (release plan D3: `/opt/Dilla`, `/usr/bin/dilla-desktop`).
const PACKAGED: [&str; 2] = ["/opt/Dilla/", "/usr/"];

/// The running AppImage: `$APPIMAGE`, when it is an absolute path to a file.
fn appimage() -> Option<PathBuf> {
    let path = PathBuf::from(std::env::var_os("APPIMAGE")?);
    (path.is_absolute() && path.is_file()).then_some(path)
}

/// How this copy is installed.
pub fn installed() -> Installed {
    if let Some(image) = appimage()
        && let Some(dir) = image.parent()
    {
        return Installed::SelfUpdating {
            staging: dir.to_owned(),
        };
    }
    let packaged =
        std::env::current_exe().is_ok_and(|exe| PACKAGED.iter().any(|p| exe.starts_with(p)));
    if packaged {
        Installed::PackageManager
    } else {
        Installed::Manual
    }
}

/// The checked download becomes executable (0755, as the release's AppImage).
pub fn prepare(downloaded: &Path) -> Result<PathBuf, Error> {
    use std::os::unix::fs::PermissionsExt;
    std::fs::set_permissions(downloaded, std::fs::Permissions::from_mode(0o755))?;
    Ok(downloaded.to_owned())
}

/// Renames `staged` over the running AppImage and starts the new one.
pub fn apply(staged: &Path) -> Result<(), Error> {
    let target = appimage().ok_or_else(|| Error::Apply("$APPIMAGE is gone".into()))?;
    replace(staged, &target)?;
    Command::new(&target).arg(RELAUNCHED).spawn()?;
    Ok(())
}

/// The atomic step: `staged` (in a staging directory beside `target`) takes `target`'s place, the directory entry is
/// flushed, and the empty staging directory is removed.
pub fn replace(staged: &Path, target: &Path) -> Result<(), Error> {
    let dir = target
        .parent()
        .ok_or_else(|| Error::Apply("the AppImage has no directory".into()))?;
    if staged.parent().and_then(Path::parent) != Some(dir) {
        return Err(Error::Apply(
            "the update is not staged beside the AppImage".into(),
        ));
    }
    std::fs::rename(staged, target)?;
    if let Ok(d) = std::fs::File::open(dir) {
        let _ = d.sync_all();
    }
    if let Some(staging) = staged.parent() {
        let _ = std::fs::remove_dir(staging);
    }
    Ok(())
}

/// Starts this copy again (an update that could not be applied: the app comes back as it was).
pub fn relaunch_current() {
    let program = appimage().or_else(|| std::env::current_exe().ok());
    if let Some(program) = program {
        let _ = Command::new(program).arg(RELAUNCHED).spawn();
    }
}

#[cfg(test)]
mod tests {
    use std::os::unix::fs::PermissionsExt;

    use super::*;

    #[test]
    fn the_staged_image_replaces_the_running_one_in_one_rename() {
        let dir = crate::native::links::tests::temp_root("updater-linux");
        let target = dir.join("Dilla.AppImage");
        std::fs::write(&target, b"old").unwrap();
        let staging = dir.join(".dilla-update-test");
        std::fs::create_dir(&staging).unwrap();
        let staged = staging.join("dilla-0.2.0-alpha.2-linux-x64.AppImage");
        std::fs::write(&staged, b"new").unwrap();
        prepare(&staged).unwrap();
        replace(&staged, &target).unwrap();
        assert_eq!(std::fs::read(&target).unwrap(), b"new");
        assert_eq!(
            std::fs::metadata(&target).unwrap().permissions().mode() & 0o777,
            0o755
        );
        assert!(!staging.exists(), "the staging directory is removed");
        // A file staged anywhere else is refused (it could be on another file system).
        let elsewhere = crate::native::links::tests::temp_root("updater-linux-other").join("x");
        std::fs::write(&elsewhere, b"x").unwrap();
        assert!(replace(&elsewhere, &target).is_err());
        assert_eq!(std::fs::read(&target).unwrap(), b"new");
    }
}
