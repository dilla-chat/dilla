//! "Start Dilla at login" (Package 5; beta.5 `autostart.ts`, `setAutostart`). A login start passes `--hidden`: the app
//! starts in the tray with no window ([`hidden_start`]).
//!
//! - **Linux**: `~/.config/autostart/dilla-desktop.desktop` (the directory 0700, the file 0600), with
//!   `Exec="<executable>" --hidden` quoted for the Desktop Entry `Exec` key ([`desktop_entry`], beta.5's quoting). The
//!   executable is the AppImage when the app runs from one (`$APPIMAGE`), else this binary.
//! - **macOS**: a LaunchAgent, `~/Library/LaunchAgents/<app id>.plist`, with `RunAtLoad` and the program's arguments
//!   `[<executable>, --hidden]` ([`launch_agent`]). beta.5's `setLoginItemSettings` registered a login item; a
//!   LaunchAgent needs no new framework binding and works for an unbundled build too. The system lists it under Login
//!   Items → "Allow in the background".
//! - **Windows**: the `HKCU\Software\Microsoft\Windows\CurrentVersion\Run` value `Dilla`, `"<executable>" --hidden`.
//!
//! The setting is what the system holds (the entry, the agent, the value): nothing is kept beside it. As beta.5, only
//! an installed build offers it: a debug build's toggle does nothing and says so in the log.

use std::path::{Path, PathBuf};

use gpui::App;

use super::{NativeShell, TrayItem, TraySection, TraySlot};

/// The tray row (beta.5 `refreshMenu`).
pub const START_AT_LOGIN: &str = "Start Dilla at login";
/// The flag a login start passes.
pub const HIDDEN: &str = "--hidden";

/// Whether this launch starts in the tray with no window (`--hidden`, autostart). `argv` is the whole command line,
/// the program name first. `main.rs` skips the first windows of a hidden start only while
/// [`super::NativeShell::stays_in_background`] is on (a tray to come back from).
pub fn hidden_start(argv: &[String]) -> bool {
    argv.iter().skip(1).any(|a| a == HIDDEN)
}

/// beta.5 `desktopEntry`: the autostart entry for `executable`, quoted for the `Exec` key (not a shell command): `%`
/// doubled, `\`, `"`, `` ` `` and `$` escaped with a backslash, then every backslash escaped again for the key-value
/// layer. A relative path, or one with a line break or NUL, is refused.
pub fn desktop_entry(executable: &str) -> Result<String, String> {
    if !Path::new(executable).is_absolute() || executable.contains(['\r', '\n', '\0']) {
        return Err("Invalid autostart executable".into());
    }
    let mut quoted = String::new();
    for ch in executable.replace('%', "%%").chars() {
        if matches!(ch, '\\' | '"' | '`' | '$') {
            quoted.push('\\');
        }
        quoted.push(ch);
    }
    let quoted = quoted.replace('\\', "\\\\");
    Ok(format!(
        "[Desktop Entry]\nType=Application\nName=Dilla\nExec=\"{quoted}\" --hidden\nTerminal=false\nStartupNotify=false\nX-GNOME-Autostart-enabled=true\n"
    ))
}

/// beta.5 `linuxAutostart`: writes (or removes) `<config>/autostart/dilla-desktop.desktop`.
pub fn linux_autostart(config: &Path, executable: &str, enabled: bool) -> std::io::Result<()> {
    let dir = config.join("autostart");
    let path = dir.join("dilla-desktop.desktop");
    if !enabled {
        return match std::fs::remove_file(&path) {
            Err(e) if e.kind() != std::io::ErrorKind::NotFound => Err(e),
            _ => Ok(()),
        };
    }
    let entry = desktop_entry(executable).map_err(std::io::Error::other)?;
    create_private_dir(&dir)?;
    write_private(&path, entry.as_bytes())
}

/// Whether `<config>/autostart/dilla-desktop.desktop` exists.
pub fn linux_enabled(config: &Path) -> bool {
    config.join("autostart/dilla-desktop.desktop").is_file()
}

/// XML text: `&`, `<`, `>`, `"` and `'` escaped.
fn xml(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&apos;")
}

/// The macOS LaunchAgent for `executable`: label `label`, run at load with `--hidden`.
pub fn launch_agent(label: &str, executable: &str) -> Result<String, String> {
    if !Path::new(executable).is_absolute() || executable.contains('\0') {
        return Err("Invalid autostart executable".into());
    }
    Ok(format!(
        "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n\
<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n\
<plist version=\"1.0\">\n<dict>\n\
\t<key>Label</key>\n\t<string>{}</string>\n\
\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>{}</string>\n\t\t<string>--hidden</string>\n\t</array>\n\
\t<key>RunAtLoad</key>\n\t<true/>\n\
\t<key>ProcessType</key>\n\t<string>Interactive</string>\n\
</dict>\n</plist>\n",
        xml(label),
        xml(executable)
    ))
}

/// The Windows `Run` value for `executable`: the path quoted, then `--hidden`.
pub fn run_value(executable: &str) -> Result<String, String> {
    if executable.contains(['"', '\r', '\n', '\0']) {
        return Err("Invalid autostart executable".into());
    }
    Ok(format!("\"{executable}\" --hidden"))
}

fn create_private_dir(dir: &Path) -> std::io::Result<()> {
    #[cfg(unix)]
    {
        use std::os::unix::fs::DirBuilderExt;
        std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(dir)
    }
    #[cfg(not(unix))]
    {
        std::fs::create_dir_all(dir)
    }
}

fn write_private(path: &Path, bytes: &[u8]) -> std::io::Result<()> {
    use std::io::Write;
    let mut options = std::fs::OpenOptions::new();
    options.write(true).create(true).truncate(true);
    #[cfg(unix)]
    {
        use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
        options.mode(0o600);
        let mut file = options.open(path)?;
        // An entry written before with wider bits is narrowed too (beta.5's writeFile mode applies on create only).
        file.set_permissions(std::fs::Permissions::from_mode(0o600))?;
        file.write_all(bytes)
    }
    #[cfg(not(unix))]
    {
        options.open(path)?.write_all(bytes)
    }
}

/// The program a login start runs: the AppImage this runs from, else this binary.
fn executable() -> Option<PathBuf> {
    if cfg!(target_os = "linux")
        && let Some(appimage) = std::env::var_os("APPIMAGE")
    {
        return Some(PathBuf::from(appimage));
    }
    std::env::current_exe().ok()
}

fn home() -> Option<PathBuf> {
    std::env::var_os("HOME").map(PathBuf::from)
}

/// `$XDG_CONFIG_HOME`, else `~/.config` (beta.5 `app.getPath('appData')`).
fn linux_config() -> Option<PathBuf> {
    std::env::var_os("XDG_CONFIG_HOME")
        .map(PathBuf::from)
        .filter(|p| p.is_absolute())
        .or_else(|| home().map(|h| h.join(".config")))
}

fn mac_agent() -> Option<PathBuf> {
    home().map(|h| h.join(format!("Library/LaunchAgents/{}.plist", crate::APP_ID)))
}

/// Whether Dilla starts at login now (what the system holds).
pub fn enabled() -> bool {
    if cfg!(target_os = "linux") {
        linux_config().is_some_and(|c| linux_enabled(&c))
    } else if cfg!(target_os = "macos") {
        mac_agent().is_some_and(|p| p.is_file())
    } else {
        windows::enabled()
    }
}

/// Turns the login start on or off.
pub fn set_enabled(on: bool) -> Result<(), String> {
    let exe = executable().ok_or("the executable is unknown")?;
    let exe = exe.to_str().ok_or("the executable's path is not UTF-8")?;
    if cfg!(target_os = "linux") {
        let config = linux_config().ok_or("no configuration directory")?;
        linux_autostart(&config, exe, on).map_err(|e| e.to_string())
    } else if cfg!(target_os = "macos") {
        let path = mac_agent().ok_or("no home directory")?;
        if !on {
            return match std::fs::remove_file(&path) {
                Err(e) if e.kind() != std::io::ErrorKind::NotFound => Err(e.to_string()),
                _ => Ok(()),
            };
        }
        let plist = launch_agent(crate::APP_ID, exe)?;
        if let Some(dir) = path.parent() {
            std::fs::create_dir_all(dir).map_err(|e| e.to_string())?;
        }
        write_private(&path, plist.as_bytes()).map_err(|e| e.to_string())
    } else {
        windows::set(on, exe)
    }
}

/// beta.5 `setAutostart`: only an installed build changes it.
fn toggle(cx: &mut App) {
    if cfg!(debug_assertions) {
        log::warn!("dilla-desktop: autostart is available in installed builds");
        return;
    }
    let want = !enabled();
    if let Err(e) = set_enabled(want) {
        log::warn!("dilla-desktop: could not change the startup preference: {e}");
    }
    NativeShell::refresh_tray(cx);
}

/// Called once by [`super::install`]: the tray's "Start Dilla at login" row.
pub fn install(cx: &mut App) {
    NativeShell::add_tray_section(
        TraySection::new(TraySlot::Autostart, |_| {
            vec![TrayItem::Check {
                label: START_AT_LOGIN.into(),
                checked: enabled(),
                run: std::rc::Rc::new(toggle),
            }]
        }),
        cx,
    );
}

#[cfg(target_os = "windows")]
// The registry Run key is reached only through windows-sys's FFI calls: each call's pointers are to buffers this
// module owns for the call's length, and every key it opens it closes.
#[allow(unsafe_code)]
mod windows {
    use windows_sys::Win32::Foundation::ERROR_SUCCESS;
    use windows_sys::Win32::System::Registry::{
        HKEY, HKEY_CURRENT_USER, KEY_QUERY_VALUE, KEY_SET_VALUE, REG_SZ, RegCloseKey,
        RegDeleteValueW, RegOpenKeyExW, RegQueryValueExW, RegSetValueExW,
    };

    const RUN: &str = r"Software\Microsoft\Windows\CurrentVersion\Run";
    const VALUE: &str = "Dilla";

    fn wide(s: &str) -> Vec<u16> {
        s.encode_utf16().chain(std::iter::once(0)).collect()
    }

    fn open(access: u32) -> Option<HKEY> {
        let mut key: HKEY = std::ptr::null_mut();
        let run = wide(RUN);
        // SAFETY: `run` is NUL-terminated and `key` is a valid out pointer.
        let status = unsafe { RegOpenKeyExW(HKEY_CURRENT_USER, run.as_ptr(), 0, access, &mut key) };
        (status == ERROR_SUCCESS).then_some(key)
    }

    pub fn enabled() -> bool {
        let Some(key) = open(KEY_QUERY_VALUE) else {
            return false;
        };
        let name = wide(VALUE);
        // SAFETY: a size query (null data) of a value of an open key.
        let status = unsafe {
            RegQueryValueExW(
                key,
                name.as_ptr(),
                std::ptr::null(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
                std::ptr::null_mut(),
            )
        };
        // SAFETY: `key` was opened above.
        unsafe { RegCloseKey(key) };
        status == ERROR_SUCCESS
    }

    pub fn set(on: bool, exe: &str) -> Result<(), String> {
        let key = open(KEY_SET_VALUE).ok_or("the Run key did not open")?;
        let name = wide(VALUE);
        let status = if on {
            let data = wide(&super::run_value(exe)?);
            // SAFETY: `data` is a NUL-terminated UTF-16 string; its byte length is passed.
            unsafe {
                RegSetValueExW(
                    key,
                    name.as_ptr(),
                    0,
                    REG_SZ,
                    data.as_ptr().cast(),
                    (data.len() * 2) as u32,
                )
            }
        } else {
            // SAFETY: deletes a named value of an open key.
            let s = unsafe { RegDeleteValueW(key, name.as_ptr()) };
            if s == windows_sys::Win32::Foundation::ERROR_FILE_NOT_FOUND {
                ERROR_SUCCESS
            } else {
                s
            }
        };
        // SAFETY: `key` was opened above.
        unsafe { RegCloseKey(key) };
        if status == ERROR_SUCCESS {
            Ok(())
        } else {
            Err(format!("the Run value was not written ({status})"))
        }
    }
}

#[cfg(not(target_os = "windows"))]
mod windows {
    pub fn enabled() -> bool {
        false
    }

    pub fn set(_on: bool, _exe: &str) -> Result<(), String> {
        Err("no login start on this platform".into())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn argv(args: &[&str]) -> Vec<String> {
        args.iter().map(|s| (*s).to_owned()).collect()
    }

    #[test]
    fn hidden_is_a_flag_after_the_program() {
        assert!(hidden_start(&argv(&["dilla-desktop", "--hidden"])));
        assert!(hidden_start(&argv(&[
            "dilla-desktop",
            "dilla://invite/a/b",
            "--hidden"
        ])));
        assert!(
            !hidden_start(&argv(&["--hidden"])),
            "the program name is not a flag"
        );
        assert!(!hidden_start(&argv(&["dilla-desktop", "--hidden=1"])));
    }

    /// beta.5 `test/autostart` (desktopEntry): Exec quoting, then the key-value layer's backslashes; relative paths and
    /// line breaks refused.
    #[test]
    fn desktop_entry_quotes_exec() {
        assert_eq!(
            desktop_entry("/opt/Dilla/dilla-desktop").unwrap(),
            "[Desktop Entry]\nType=Application\nName=Dilla\nExec=\"/opt/Dilla/dilla-desktop\" --hidden\nTerminal=false\nStartupNotify=false\nX-GNOME-Autostart-enabled=true\n"
        );
        let entry = desktop_entry("/home/a b/100%/$x`y\"z\\w/Dilla.AppImage").unwrap();
        assert!(
            entry.contains(
                "Exec=\"/home/a b/100%%/\\\\$x\\\\`y\\\\\"z\\\\\\\\w/Dilla.AppImage\" --hidden\n"
            ),
            "{entry}"
        );
        for bad in [
            "dilla-desktop",
            "./dilla",
            "/opt/a\nExec=evil",
            "/opt/a\r",
            "/opt/\0",
        ] {
            assert!(desktop_entry(bad).is_err(), "{bad:?}");
        }
    }

    #[test]
    fn the_linux_entry_is_private_and_removed_when_off() {
        let config = std::env::temp_dir().join(format!(
            "dilla-autostart-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        assert!(!linux_enabled(&config));
        linux_autostart(&config, "/opt/Dilla/dilla-desktop", true).unwrap();
        assert!(linux_enabled(&config));
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let mode = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
            assert_eq!(mode(&config.join("autostart")), 0o700);
            assert_eq!(mode(&config.join("autostart/dilla-desktop.desktop")), 0o600);
        }
        linux_autostart(&config, "/opt/Dilla/dilla-desktop", false).unwrap();
        assert!(!linux_enabled(&config));
        linux_autostart(&config, "/opt/Dilla/dilla-desktop", false).unwrap();
        std::fs::remove_dir_all(&config).unwrap();
    }

    #[test]
    fn the_launch_agent_and_the_run_value_pass_hidden() {
        let plist = launch_agent(
            "dev.dilla.desktop",
            "/Applications/Dilla & Co.app/Contents/MacOS/Dilla",
        )
        .unwrap();
        assert!(plist.contains("<string>/Applications/Dilla &amp; Co.app/Contents/MacOS/Dilla</string>\n\t\t<string>--hidden</string>"));
        assert!(plist.contains("<key>RunAtLoad</key>\n\t<true/>"));
        assert!(launch_agent("x", "Dilla").is_err());
        assert_eq!(
            run_value(r"C:\Program Files\Dilla\dilla.exe").unwrap(),
            r#""C:\Program Files\Dilla\dilla.exe" --hidden"#
        );
        assert!(run_value("C:\\a\"b").is_err());
    }
}
