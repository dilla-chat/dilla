//! macOS (release plan 14b, D9: no Sparkle). The update is the release's `dilla-<v>-mac-arm64.zip` holding `Dilla.app`
//! (Task 12). It is downloaded into a staging directory beside the running bundle (the same volume), unpacked with
//! `ditto` (which keeps the code signature's extended attributes), and the unpacked bundle must pass
//! `SecStaticCodeCheckValidity` against [`requirement`]: Apple's anchor, a Developer ID Application certificate of
//! [`TEAM_ID`] and the bundle id `dev.dilla.desktop`. The check runs again right before the swap, so the bundle that
//! replaces the app is the one that passed. The swap is one `renamex_np(RENAME_SWAP)`: the bundle's path holds the old
//! app or the new one, never a mix. The old bundle (now in the staging directory) is removed and the new one opened.
//!
//! Only a Developer-ID-signed bundle the person can replace updates itself; any other copy (a debug build, a build
//! without [`TEAM_ID`], a bundle in a folder this user cannot write) shows the release page instead.

#![cfg_attr(not(target_os = "macos"), allow(dead_code))]

use std::path::{Path, PathBuf};
use std::process::Command;

use super::{Error, Installed, RELAUNCHED};

/// The Apple Developer team that signs Dilla (beta.5's team: Task 12 signs with the same certificate, so the bundle id
/// keeps its macOS permissions). The repository does not carry the team id (mobile-release keeps it in the
/// `APPLE_TEAM_ID` environment variable), so the signing workflow's Mac build passes that variable in as
/// `DILLA_APPLE_TEAM_ID`. A build without it never replaces itself.
pub const TEAM_ID: Option<&str> = option_env!("DILLA_APPLE_TEAM_ID");

/// The bundle id the update must carry (release plan D3).
pub const BUNDLE_ID: &str = "dev.dilla.desktop";

/// [`TEAM_ID`] when it has the shape of a team id (10 uppercase letters and digits).
pub fn team_id() -> Option<&'static str> {
    TEAM_ID.filter(|t| {
        t.len() == 10
            && t.bytes()
                .all(|b| b.is_ascii_uppercase() || b.is_ascii_digit())
    })
}

/// The code requirement an update must satisfy: the plan's `anchor apple generic and certificate leaf[subject.OU] =
/// "<TEAM_ID>"`, narrowed to a Developer ID Application certificate (the Developer ID CA's and the leaf's marker
/// extensions, as Apple's designated requirement for Developer ID) and to Dilla's bundle id.
pub fn requirement(team: &str) -> String {
    format!(
        "identifier \"{BUNDLE_ID}\" and anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] and certificate leaf[field.1.2.840.113635.100.6.1.13] and certificate leaf[subject.OU] = \"{team}\""
    )
}

/// The bundle of an executable at `<bundle>.app/Contents/MacOS/<binary>`.
pub fn bundle_of(exe: &Path) -> Option<PathBuf> {
    let macos = exe.parent()?;
    let contents = macos.parent()?;
    let bundle = contents.parent()?;
    let ok = macos.file_name()? == "MacOS"
        && contents.file_name()? == "Contents"
        && bundle.extension()? == "app";
    ok.then(|| bundle.to_owned())
}

fn running_bundle() -> Option<PathBuf> {
    bundle_of(&std::env::current_exe().ok()?)
}

/// How this copy is installed.
pub fn installed() -> Installed {
    if cfg!(debug_assertions) || team_id().is_none() {
        return Installed::Manual;
    }
    match running_bundle().and_then(|b| b.parent().map(Path::to_owned)) {
        Some(dir) => Installed::SelfUpdating { staging: dir },
        None => Installed::Manual,
    }
}

/// Unpacks the checked zip beside it and checks the bundle's signature; answers the unpacked `Dilla.app`.
pub fn prepare(zip: &Path) -> Result<PathBuf, Error> {
    let staging = zip
        .parent()
        .ok_or_else(|| Error::Apply("no staging directory".into()))?;
    let out = staging.join("unpacked");
    let status = Command::new("/usr/bin/ditto")
        .arg("-x")
        .arg("-k")
        .arg(zip)
        .arg(&out)
        .status()?;
    let _ = std::fs::remove_file(zip);
    if !status.success() {
        return Err(Error::Apply(format!("ditto failed: {status}")));
    }
    let entries: Vec<_> = std::fs::read_dir(&out)?
        .filter_map(Result::ok)
        .map(|e| e.file_name())
        .collect();
    if entries.len() != 1 || entries[0] != "Dilla.app" {
        return Err(Error::Apply(
            "the zip does not hold exactly Dilla.app".into(),
        ));
    }
    let app = out.join("Dilla.app");
    check(&app)?;
    Ok(app)
}

/// `SecStaticCodeCheckValidity` of `app` against [`requirement`].
fn check(app: &Path) -> Result<(), Error> {
    let team = team_id().ok_or_else(|| Error::Apply("no Developer ID team".into()))?;
    sys::check_validity(app, &requirement(team))
        .map_err(|status| Error::Apply(format!("the update's signature is not Dilla's ({status})")))
}

/// Checks `app` again, swaps it with the running bundle, removes the old one and opens the new one.
pub fn apply(app: &Path) -> Result<(), Error> {
    let bundle =
        running_bundle().ok_or_else(|| Error::Apply("not running from a bundle".into()))?;
    if app.parent().and_then(Path::parent).and_then(Path::parent) != bundle.parent() {
        return Err(Error::Apply(
            "the update is not staged beside the bundle".into(),
        ));
    }
    check(app)?;
    sys::swap(app, &bundle)?;
    // `app` now holds the old bundle.
    if let Some(staging) = app.parent().and_then(Path::parent) {
        let _ = std::fs::remove_dir_all(staging);
    }
    open(&bundle)
}

fn open(bundle: &Path) -> Result<(), Error> {
    Command::new("/usr/bin/open")
        .arg("-n")
        .arg(bundle)
        .arg("--args")
        .arg(RELAUNCHED)
        .spawn()?;
    Ok(())
}

/// Opens this copy again (an update that could not be applied: the app comes back as it was).
pub fn relaunch_current() {
    if let Some(bundle) = running_bundle() {
        let _ = open(&bundle);
    }
}

#[cfg(target_os = "macos")]
// Security.framework's static code check and `renamex_np` have no binding in the lock beyond raw FFI: every pointer
// handed over is to a buffer or CF object this module owns for the call's length, and every CF object it creates it
// releases.
#[allow(unsafe_code)]
mod sys {
    use std::ffi::{CString, c_void};
    use std::os::unix::ffi::OsStrExt as _;
    use std::path::Path;

    use super::Error;

    type CFTypeRef = *const c_void;
    type OSStatus = i32;

    const K_CF_STRING_ENCODING_UTF8: u32 = 0x0800_0100;
    /// kSecCSCheckAllArchitectures | kSecCSCheckNestedCode | kSecCSStrictValidate.
    const CHECK_FLAGS: u32 = (1 << 0) | (1 << 3) | (1 << 4);

    #[link(name = "CoreFoundation", kind = "framework")]
    unsafe extern "C" {
        fn CFURLCreateFromFileSystemRepresentation(
            allocator: CFTypeRef,
            buffer: *const u8,
            length: isize,
            is_directory: u8,
        ) -> CFTypeRef;
        fn CFStringCreateWithBytes(
            allocator: CFTypeRef,
            bytes: *const u8,
            length: isize,
            encoding: u32,
            is_external: u8,
        ) -> CFTypeRef;
        fn CFRelease(cf: CFTypeRef);
    }

    #[link(name = "Security", kind = "framework")]
    unsafe extern "C" {
        fn SecStaticCodeCreateWithPath(
            path: CFTypeRef,
            flags: u32,
            code: *mut CFTypeRef,
        ) -> OSStatus;
        fn SecRequirementCreateWithString(
            text: CFTypeRef,
            flags: u32,
            requirement: *mut CFTypeRef,
        ) -> OSStatus;
        fn SecStaticCodeCheckValidity(
            code: CFTypeRef,
            flags: u32,
            requirement: CFTypeRef,
        ) -> OSStatus;
    }

    /// Releases a CF object when dropped (null is skipped).
    struct Owned(CFTypeRef);

    impl Drop for Owned {
        fn drop(&mut self) {
            if !self.0.is_null() {
                // SAFETY: a CF object this module created and owns one reference to.
                unsafe { CFRelease(self.0) };
            }
        }
    }

    /// 0 when the code at `path` is valid and satisfies `requirement`, else the OSStatus.
    pub fn check_validity(path: &Path, requirement: &str) -> Result<(), OSStatus> {
        let bytes = path.as_os_str().as_bytes();
        // SAFETY: `bytes` lives across the call; the result is owned and released by `Owned`.
        let url = Owned(unsafe {
            CFURLCreateFromFileSystemRepresentation(
                std::ptr::null(),
                bytes.as_ptr(),
                bytes.len() as isize,
                1,
            )
        });
        // SAFETY: as above, for the requirement's UTF-8 bytes.
        let text = Owned(unsafe {
            CFStringCreateWithBytes(
                std::ptr::null(),
                requirement.as_ptr(),
                requirement.len() as isize,
                K_CF_STRING_ENCODING_UTF8,
                0,
            )
        });
        if url.0.is_null() || text.0.is_null() {
            return Err(-1);
        }
        let mut code: CFTypeRef = std::ptr::null();
        // SAFETY: `url` is a valid CFURL and `code` a valid out pointer; the created object is owned below.
        let status = unsafe { SecStaticCodeCreateWithPath(url.0, 0, &mut code) };
        let code = Owned(code);
        if status != 0 {
            return Err(status);
        }
        let mut req: CFTypeRef = std::ptr::null();
        // SAFETY: `text` is a valid CFString and `req` a valid out pointer; the created object is owned below.
        let status = unsafe { SecRequirementCreateWithString(text.0, 0, &mut req) };
        let req = Owned(req);
        if status != 0 {
            return Err(status);
        }
        // SAFETY: both are valid objects created above.
        match unsafe { SecStaticCodeCheckValidity(code.0, CHECK_FLAGS, req.0) } {
            0 => Ok(()),
            status => Err(status),
        }
    }

    /// Exchanges `a` and `b` in one step (`renamex_np` with `RENAME_SWAP`, same volume).
    pub fn swap(a: &Path, b: &Path) -> Result<(), Error> {
        let c = |p: &Path| {
            CString::new(p.as_os_str().as_bytes())
                .map_err(|_| Error::Apply("a path holds NUL".into()))
        };
        let (a, b) = (c(a)?, c(b)?);
        // SAFETY: both are NUL-terminated paths that live across the call.
        if unsafe { libc::renamex_np(a.as_ptr(), b.as_ptr(), libc::RENAME_SWAP) } != 0 {
            return Err(std::io::Error::last_os_error().into());
        }
        Ok(())
    }
}

#[cfg(not(target_os = "macos"))]
mod sys {
    use std::path::Path;

    use super::Error;

    pub fn check_validity(_: &Path, _: &str) -> Result<(), i32> {
        Err(-1)
    }

    pub fn swap(_: &Path, _: &Path) -> Result<(), Error> {
        Err(Error::Apply("not macOS".into()))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_bundle_is_found_from_its_executable() {
        assert_eq!(
            bundle_of(Path::new(
                "/Applications/Dilla.app/Contents/MacOS/dilla-desktop"
            )),
            Some(PathBuf::from("/Applications/Dilla.app"))
        );
        for bad in [
            "/usr/local/bin/dilla-desktop",
            "/Applications/Dilla/Contents/MacOS/dilla-desktop",
            "/Applications/Dilla.app/Contents/Resources/dilla-desktop",
            "dilla-desktop",
        ] {
            assert_eq!(bundle_of(Path::new(bad)), None, "{bad}");
        }
    }

    #[test]
    fn the_requirement_names_developer_id_the_team_and_the_bundle() {
        let r = requirement("ABCDE12345");
        assert!(r.starts_with("identifier \"dev.dilla.desktop\" and anchor apple generic and "));
        assert!(r.ends_with("certificate leaf[subject.OU] = \"ABCDE12345\""));
        assert!(r.contains("certificate 1[field.1.2.840.113635.100.6.2.6]"));
        assert!(r.contains("certificate leaf[field.1.2.840.113635.100.6.1.13]"));
    }
}
