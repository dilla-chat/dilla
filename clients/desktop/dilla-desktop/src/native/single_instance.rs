//! One process for all instances (Package 6, decision D2; beta.5 `app.requestSingleInstanceLock()` and
//! `second-instance`).
//!
//! **The lock.** [`claim`] locks `<data root>/single-instance.lock` (`File::try_lock`: `flock` on Unix, `LockFileEx`
//! on Windows; the OS lets go when the process ends, however it ends). The process that holds it is the app.
//!
//! **The handover.** The app listens on a per-user endpoint:
//! - Unix: a socket `<data root>/ipc/handover.sock`, in a directory of mode 0700, the socket of mode 0600, so only
//!   the same user reaches it;
//! - Windows: a named pipe `\\.\pipe\dilla-desktop-<hash of the data root>` (the data root is per user), inbound only
//!   and refusing remote clients, its first instance created by the app (`FILE_FLAG_FIRST_PIPE_INSTANCE`).
//!
//! A second launch finds the lock taken, sends its argv there and exits ([`Claim::HandedOver`]). The app accepts only
//! argv-shaped messages ([`decode`]: a fixed header, at most [`MAX_ARGS`] UTF-8 arguments, [`MAX_MESSAGE`] bytes in
//! all); anything else is dropped unread. On the UI thread a handed-over argv with a `dilla://` link opens the link
//! (beta.5: the first one), and one without brings the app forward ([`super::NativeShell::show_app`]: the oldest
//! window, else the manager).
//!
//! `--instance=<n>` in a debug build skips the lock: the developer's side-by-side copy (D2: "`--instance=n` becomes
//! debug-only").

use std::fs::{File, OpenOptions};
use std::io::{self, Read as _, Write as _};
use std::path::{Path, PathBuf};
use std::time::{Duration, Instant};

use gpui::App;
use tokio::sync::mpsc;

/// The lock file in the data root.
pub const LOCK_FILE: &str = "single-instance.lock";
/// The header every handover message starts with.
const MAGIC: &[u8] = b"dilla-argv/1\0";
/// The longest handover message, header included.
pub const MAX_MESSAGE: usize = 64 * 1024;
/// The most arguments a handover carries (the program name included).
pub const MAX_ARGS: usize = 64;
/// How long a second launch keeps trying to reach the app (it may be between taking the lock and listening).
const CONNECT_FOR: Duration = Duration::from_secs(3);
/// How long either side waits for the other's bytes.
const IO_TIMEOUT: Duration = Duration::from_secs(5);
/// What the app answers once it took a message (Unix; the Windows pipe is inbound only).
#[cfg(unix)]
const ACK: &[u8] = b"ok\n";

/// This process holds the lock: the lock and the endpoint second launches talk to.
#[derive(Default)]
pub struct Primary {
    /// Held for the process's life (dropped with it).
    _lock: Option<File>,
    endpoint: Option<Endpoint>,
}

impl std::fmt::Debug for Primary {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Primary")
            .field("locked", &self._lock.is_some())
            .field("listening", &self.endpoint.is_some())
            .finish()
    }
}

impl Primary {
    /// Serves second launches into `tx` without an app (the tests' [`install`]).
    #[cfg(test)]
    pub(crate) fn serve_for_test(mut self, tx: mpsc::UnboundedSender<Vec<String>>) {
        if let Some(endpoint) = self.endpoint.take() {
            endpoint.serve(tx);
        }
        std::mem::forget(self._lock.take());
    }
}

/// What [`claim`] found.
#[derive(Debug)]
pub enum Claim {
    /// This process is the app.
    Primary(Primary),
    /// The running app took this launch's argv: this process exits.
    HandedOver,
}

/// Before the run loop, with the whole command line (the program name first).
pub fn claim(argv: &[String]) -> Claim {
    if cfg!(debug_assertions) && argv.iter().any(|a| a.starts_with("--instance=")) {
        return Claim::Primary(Primary::default());
    }
    match dilla_client::instances::default_root() {
        Ok(root) => claim_at(&root, argv),
        // No data root: nothing to share either; the app shows why it cannot store anything.
        Err(_) => Claim::Primary(Primary::default()),
    }
}

/// [`claim`] in data root `root`.
pub fn claim_at(root: &Path, argv: &[String]) -> Claim {
    let lock = match open_lock(root) {
        Ok(lock) => lock,
        Err(e) => {
            log::warn!("dilla-desktop: the single-instance lock could not be opened: {e}");
            return Claim::Primary(Primary::default());
        }
    };
    match lock.try_lock() {
        Ok(()) => {
            let endpoint = Endpoint::listen(root)
                .map_err(|e| log::warn!("dilla-desktop: second launches cannot hand over: {e}"))
                .ok();
            Claim::Primary(Primary {
                _lock: Some(lock),
                endpoint,
            })
        }
        Err(std::fs::TryLockError::WouldBlock) => {
            if let Err(e) = hand_over(root, argv) {
                // beta.5's second instance quit whatever happened; the running app stays as it is.
                eprintln!(
                    "dilla-desktop: Dilla is already running and did not take this launch: {e}"
                );
            }
            Claim::HandedOver
        }
        Err(std::fs::TryLockError::Error(e)) => {
            log::warn!("dilla-desktop: the single-instance lock failed: {e}");
            Claim::Primary(Primary::default())
        }
    }
}

fn open_lock(root: &Path) -> io::Result<File> {
    create_private_dir(root)?;
    OpenOptions::new()
        .read(true)
        .write(true)
        .create(true)
        .truncate(false)
        .open(root.join(LOCK_FILE))
}

/// Creates `dir` (and its parents) if missing, private to the user on Unix.
fn create_private_dir(dir: &Path) -> io::Result<()> {
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

/// Called once by [`super::install`], last, with what [`claim`] took: second launches' argv reach the UI thread.
pub fn install(primary: Primary, cx: &mut App) {
    let Primary { _lock, endpoint } = primary;
    // The lock lives as long as the process (the run loop never returns).
    std::mem::forget(_lock);
    let Some(endpoint) = endpoint else {
        return;
    };
    let (tx, mut rx) = mpsc::unbounded_channel::<Vec<String>>();
    endpoint.serve(tx);
    cx.spawn(async move |cx| {
        while let Some(argv) = rx.recv().await {
            cx.update(|cx| second_launch(&argv, cx));
        }
    })
    .detach();
}

/// A second launch's argv (the program name first), on the UI thread: beta.5 `second-instance`.
pub fn second_launch(argv: &[String], cx: &mut App) {
    match argv.iter().skip(1).find_map(|a| super::links::parse(a)) {
        Some(link) => {
            super::NativeShell::open_link(link, cx);
        }
        None => super::NativeShell::show_app(cx),
    }
}

/// The message a second launch sends: the header, then each argument followed by a NUL. Arguments past [`MAX_ARGS`]
/// or [`MAX_MESSAGE`], or with a NUL inside, are left out.
pub fn encode(argv: &[String]) -> Vec<u8> {
    let mut out = MAGIC.to_vec();
    for arg in argv.iter().take(MAX_ARGS) {
        if arg.contains('\0') || out.len() + arg.len() + 1 > MAX_MESSAGE {
            continue;
        }
        out.extend_from_slice(arg.as_bytes());
        out.push(0);
    }
    out
}

/// The argv of a message, or `None` when it is not exactly argv-shaped: the header, then at least one and at most
/// [`MAX_ARGS`] NUL-terminated UTF-8 arguments, [`MAX_MESSAGE`] bytes in all.
pub fn decode(bytes: &[u8]) -> Option<Vec<String>> {
    if bytes.len() > MAX_MESSAGE {
        return None;
    }
    let body = bytes.strip_prefix(MAGIC)?;
    let body = body.strip_suffix(b"\0")?;
    let args: Vec<String> = body
        .split(|b| *b == 0)
        .map(|a| String::from_utf8(a.to_vec()).ok())
        .collect::<Option<_>>()?;
    (!args.is_empty() && args.len() <= MAX_ARGS).then_some(args)
}

/// Reads one message: at most [`MAX_MESSAGE`] bytes, until the sender is done.
fn read_message(from: impl io::Read) -> io::Result<Option<Vec<String>>> {
    let mut buf = Vec::new();
    from.take(MAX_MESSAGE as u64 + 1).read_to_end(&mut buf)?;
    Ok(decode(&buf))
}

// ---------------------------------------------------------------------------------------------------------------------
// Unix: a socket in a private directory
// ---------------------------------------------------------------------------------------------------------------------

#[cfg(unix)]
struct Endpoint {
    listener: std::os::unix::net::UnixListener,
}

/// The longest socket path every Unix takes (`sun_path` is 104 bytes on macOS, 108 on Linux, NUL included).
#[cfg(unix)]
const MAX_SOCKET_PATH: usize = 100;

/// FNV-1a of the data root: names the short socket directory and the Windows pipe, so two data roots never share one.
fn root_hash(root: &str) -> u64 {
    let mut hash: u64 = 0xcbf2_9ce4_8422_2325;
    for b in root.bytes() {
        hash ^= u64::from(b);
        hash = hash.wrapping_mul(0x0100_0000_01b3);
    }
    hash
}

/// `<data root>/ipc/handover.sock`, or, when that is too long for a socket address, `dilla-<hash>/handover.sock` in
/// `$XDG_RUNTIME_DIR` (the user's own) or else the temporary directory.
#[cfg(unix)]
fn socket_path(root: &Path) -> PathBuf {
    let path = root.join("ipc").join("handover.sock");
    if path.as_os_str().len() <= MAX_SOCKET_PATH {
        return path;
    }
    let base = std::env::var_os("XDG_RUNTIME_DIR")
        .map(PathBuf::from)
        .filter(|p| p.is_absolute())
        .unwrap_or_else(std::env::temp_dir);
    base.join(format!("dilla-{:016x}", root_hash(&root.to_string_lossy())))
        .join("handover.sock")
}

/// The socket's directory is this user's and nobody else's: owned by whoever owns the data root, mode 0700. Checked
/// by both ends, so a directory another user made first (the temporary directory is shared) is never used.
#[cfg(unix)]
fn check_private(dir: &Path, root: &Path) -> io::Result<()> {
    use std::os::unix::fs::MetadataExt as _;
    let (d, r) = (std::fs::symlink_metadata(dir)?, std::fs::metadata(root)?);
    if !d.is_dir() || d.uid() != r.uid() || d.mode() & 0o077 != 0 {
        return Err(io::Error::other(format!(
            "{} is not this user's private directory",
            dir.display()
        )));
    }
    Ok(())
}

#[cfg(unix)]
impl Endpoint {
    fn listen(root: &Path) -> io::Result<Self> {
        use std::os::unix::fs::PermissionsExt;
        let path = socket_path(root);
        let dir = path.parent().expect("the socket has a directory");
        create_private_dir(dir)?;
        // Made private even when it was there before (fails when another user owns it).
        std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700))?;
        check_private(dir, root)?;
        // A socket left by an app that ended: this process holds the lock, so nobody listens on it.
        match std::fs::remove_file(&path) {
            Err(e) if e.kind() != io::ErrorKind::NotFound => return Err(e),
            _ => {}
        }
        let listener = std::os::unix::net::UnixListener::bind(&path)?;
        std::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o600))?;
        Ok(Self { listener })
    }

    /// Accepts second launches on a thread of its own; each argv goes to `tx`.
    fn serve(self, tx: mpsc::UnboundedSender<Vec<String>>) {
        let spawned = std::thread::Builder::new()
            .name("dilla-handover".into())
            .spawn(move || {
                for stream in self.listener.incoming() {
                    let Ok(mut stream) = stream else { continue };
                    let _ = stream.set_read_timeout(Some(IO_TIMEOUT));
                    let _ = stream.set_write_timeout(Some(IO_TIMEOUT));
                    match read_message(&mut stream) {
                        Ok(Some(argv)) => {
                            if tx.send(argv).is_err() {
                                return;
                            }
                            let _ = stream.write_all(ACK);
                        }
                        Ok(None) => {
                            log::warn!("dilla-desktop: a handover that was not an argv was dropped")
                        }
                        Err(e) => log::warn!("dilla-desktop: a handover could not be read: {e}"),
                    }
                }
            });
        if let Err(e) = spawned {
            log::warn!("dilla-desktop: the handover listener did not start: {e}");
        }
    }
}

#[cfg(unix)]
fn hand_over(root: &Path, argv: &[String]) -> io::Result<()> {
    use std::os::unix::net::UnixStream;
    let path = socket_path(root);
    let deadline = Instant::now() + CONNECT_FOR;
    let dir = path.parent().expect("the socket has a directory");
    let mut stream = loop {
        match check_private(dir, root).and_then(|()| UnixStream::connect(&path)) {
            Ok(stream) => break stream,
            Err(e) if Instant::now() >= deadline => return Err(e),
            Err(_) => std::thread::sleep(Duration::from_millis(50)),
        }
    };
    stream.set_write_timeout(Some(IO_TIMEOUT))?;
    stream.set_read_timeout(Some(IO_TIMEOUT))?;
    stream.write_all(&encode(argv))?;
    stream.shutdown(std::net::Shutdown::Write)?;
    let mut ack = [0u8; ACK.len()];
    stream.read_exact(&mut ack)?;
    if ack == ACK {
        Ok(())
    } else {
        Err(io::Error::other("the running app did not acknowledge"))
    }
}

// ---------------------------------------------------------------------------------------------------------------------
// Windows: a named pipe per data root (so per user)
// ---------------------------------------------------------------------------------------------------------------------

#[cfg(windows)]
struct Endpoint {
    name: Vec<u16>,
    /// The first pipe instance, made at claim time so a name already taken is known before the run loop.
    first: windows_sys::Win32::Foundation::HANDLE,
}

// SAFETY: the handle is a kernel object handle, usable from any thread; only the serving thread uses it.
#[cfg(windows)]
#[allow(unsafe_code)]
unsafe impl Send for Endpoint {}

/// `\\.\pipe\dilla-desktop-<FNV-1a of the data root>`.
#[cfg(windows)]
fn pipe_name(root: &Path) -> String {
    let hash = root_hash(&root.to_string_lossy().to_lowercase());
    format!(r"\\.\pipe\dilla-desktop-{hash:016x}")
}

#[cfg(windows)]
#[allow(unsafe_code)]
fn create_pipe(name: &[u16], first: bool) -> io::Result<windows_sys::Win32::Foundation::HANDLE> {
    use windows_sys::Win32::Foundation::INVALID_HANDLE_VALUE;
    use windows_sys::Win32::Storage::FileSystem::{
        FILE_FLAG_FIRST_PIPE_INSTANCE, PIPE_ACCESS_INBOUND,
    };
    use windows_sys::Win32::System::Pipes::{
        CreateNamedPipeW, PIPE_READMODE_BYTE, PIPE_REJECT_REMOTE_CLIENTS, PIPE_TYPE_BYTE,
        PIPE_UNLIMITED_INSTANCES, PIPE_WAIT,
    };
    let open_mode = PIPE_ACCESS_INBOUND
        | if first {
            FILE_FLAG_FIRST_PIPE_INSTANCE
        } else {
            0
        };
    // SAFETY: `name` is NUL-terminated UTF-16; no security attributes (the default DACL: the creator, SYSTEM and
    // administrators write; others may only read, and the pipe is inbound, so they have nothing to read).
    let handle = unsafe {
        CreateNamedPipeW(
            name.as_ptr(),
            open_mode,
            PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT | PIPE_REJECT_REMOTE_CLIENTS,
            PIPE_UNLIMITED_INSTANCES,
            0,
            MAX_MESSAGE as u32,
            0,
            std::ptr::null(),
        )
    };
    if handle == INVALID_HANDLE_VALUE {
        Err(io::Error::last_os_error())
    } else {
        Ok(handle)
    }
}

#[cfg(windows)]
#[allow(unsafe_code)]
impl Endpoint {
    fn listen(root: &Path) -> io::Result<Self> {
        let name: Vec<u16> = pipe_name(root).encode_utf16().chain([0]).collect();
        let first = create_pipe(&name, true)?;
        Ok(Self { name, first })
    }

    fn serve(self, tx: mpsc::UnboundedSender<Vec<String>>) {
        let spawned = std::thread::Builder::new()
            .name("dilla-handover".into())
            .spawn(move || {
                use std::os::windows::io::FromRawHandle as _;
                use windows_sys::Win32::Foundation::ERROR_PIPE_CONNECTED;
                use windows_sys::Win32::System::Pipes::ConnectNamedPipe;
                let mut next = Some(self.first);
                loop {
                    let handle = match next.take() {
                        Some(h) => h,
                        None => match create_pipe(&self.name, false) {
                            Ok(h) => h,
                            Err(e) => {
                                log::warn!("dilla-desktop: the handover pipe stopped: {e}");
                                return;
                            }
                        },
                    };
                    // SAFETY: a pipe handle of ours, waited on synchronously.
                    let connected = unsafe { ConnectNamedPipe(handle, std::ptr::null_mut()) } != 0
                        || io::Error::last_os_error().raw_os_error()
                            == Some(ERROR_PIPE_CONNECTED as i32);
                    // SAFETY: the handle is ours and is closed once, by the File.
                    let pipe = unsafe { File::from_raw_handle(handle as _) };
                    if !connected {
                        continue;
                    }
                    match read_message(&pipe) {
                        Ok(Some(argv)) => {
                            if tx.send(argv).is_err() {
                                return;
                            }
                        }
                        Ok(None) => {
                            log::warn!("dilla-desktop: a handover that was not an argv was dropped")
                        }
                        Err(e) => log::warn!("dilla-desktop: a handover could not be read: {e}"),
                    }
                }
            });
        if let Err(e) = spawned {
            log::warn!("dilla-desktop: the handover listener did not start: {e}");
        }
    }
}

#[cfg(windows)]
fn hand_over(root: &Path, argv: &[String]) -> io::Result<()> {
    let name = pipe_name(root);
    let deadline = Instant::now() + CONNECT_FOR;
    let mut pipe = loop {
        match OpenOptions::new().write(true).open(&name) {
            Ok(pipe) => break pipe,
            Err(e) if Instant::now() >= deadline => return Err(e),
            Err(_) => std::thread::sleep(Duration::from_millis(50)),
        }
    };
    pipe.write_all(&encode(argv))?;
    pipe.flush()
}

#[cfg(test)]
mod tests {
    use std::cell::RefCell;
    use std::rc::Rc;

    use gpui::TestAppContext;

    use super::*;
    use crate::native::{Link, NativeShell};

    fn argv(args: &[&str]) -> Vec<String> {
        args.iter().map(|a| (*a).to_owned()).collect()
    }

    /// The handover's own wire: what a second launch sends is what the app reads; nothing else is an argv.
    #[test]
    fn only_argv_shaped_messages_are_accepted() {
        let sent = argv(&[
            "dilla-desktop",
            "dilla://invite/chat.example/CODE",
            "--hidden",
        ]);
        assert_eq!(decode(&encode(&sent)), Some(sent));
        for bad in [
            &b""[..],
            b"dilla-argv/1\0",
            b"dilla-argv/2\0x\0",
            b"dilla-argv/1\0no terminator",
            b"dilla-argv/1\0\xff\xfe\0",
            b"{\"argv\":[\"x\"]}",
        ] {
            assert_eq!(decode(bad), None, "{bad:?}");
        }
        let mut long = MAGIC.to_vec();
        long.extend(std::iter::repeat_n(b'a', MAX_MESSAGE));
        long.push(0);
        assert_eq!(decode(&long), None, "too long");
        let mut many = MAGIC.to_vec();
        for _ in 0..=MAX_ARGS {
            many.extend_from_slice(b"a\0");
        }
        assert_eq!(decode(&many), None, "too many arguments");
        assert_eq!(
            decode(&encode(&vec!["a".to_owned(); MAX_ARGS + 5])).map(|a| a.len()),
            Some(MAX_ARGS),
            "a launch with too many arguments sends the first {MAX_ARGS}"
        );
    }

    /// Takes the lock in `root` as the app would, and the handover's receiving end.
    fn primary_at(root: &Path) -> (Primary, std::sync::mpsc::Receiver<Vec<String>>) {
        let Claim::Primary(mut primary) = claim_at(root, &argv(&["dilla-desktop"])) else {
            panic!("the first launch is the app");
        };
        let endpoint = primary.endpoint.take().expect("the app listens");
        let (tx, mut rx) = mpsc::unbounded_channel();
        endpoint.serve(tx);
        let (out, received) = std::sync::mpsc::channel();
        std::thread::spawn(move || {
            while let Some(a) = rx.blocking_recv() {
                if out.send(a).is_err() {
                    return;
                }
            }
        });
        (primary, received)
    }

    #[cfg(unix)]
    #[test]
    fn the_endpoint_is_private() {
        use std::os::unix::fs::PermissionsExt;
        let root = super::super::links::tests::temp_root("single-private");
        let (_primary, _rx) = primary_at(&root);
        let mode = |p: &Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
        let socket = socket_path(&root);
        assert_eq!(mode(socket.parent().unwrap()), 0o700);
        assert_eq!(mode(&socket), 0o600);
    }

    /// A data root too deep for a socket address gets a short private directory of its own; one another user made
    /// (or one open to others) is refused by both ends.
    #[cfg(unix)]
    #[test]
    fn a_deep_data_root_gets_a_short_private_socket() {
        use std::os::unix::fs::PermissionsExt;
        let shallow = super::super::links::tests::temp_root("s");
        let deep = shallow.join("d".repeat(120));
        std::fs::create_dir_all(&deep).unwrap();
        let socket = socket_path(&deep);
        assert!(socket.as_os_str().len() <= MAX_SOCKET_PATH);
        assert_ne!(
            socket_path(&deep),
            socket_path(&shallow.join("e".repeat(120)))
        );
        let (_primary, received) = primary_at(&deep);
        assert!(matches!(
            claim_at(&deep, &argv(&["dilla-desktop"])),
            Claim::HandedOver
        ));
        assert!(
            received.recv_timeout(IO_TIMEOUT).is_ok(),
            "handed over through the short socket"
        );
        let dir = socket.parent().unwrap();
        std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o755)).unwrap();
        assert!(
            check_private(dir, &deep).is_err(),
            "a directory open to others is not used"
        );
        std::fs::set_permissions(dir, std::fs::Permissions::from_mode(0o700)).unwrap();
        let _ = std::fs::remove_dir_all(dir);
    }

    /// beta.5 `second-instance` with no link: the second launch hands over and exits, and the running app brings its
    /// oldest window forward.
    #[gpui::test]
    fn second_launch_focuses_the_running_app(cx: &mut TestAppContext) {
        let root = super::super::links::tests::temp_root("single-focus");
        let (_primary, received) = primary_at(&root);
        let second = argv(&["dilla-desktop"]);
        let launch = second.clone();
        let root2 = root.clone();
        let handed = std::thread::spawn(move || claim_at(&root2, &launch))
            .join()
            .unwrap();
        assert!(
            matches!(handed, Claim::HandedOver),
            "the second launch exits"
        );
        let got = received.recv_timeout(IO_TIMEOUT).expect("the app took it");
        assert_eq!(got, second);

        let oldest = cx.add_empty_window().update(|w, _| w.window_handle());
        let newest = cx.add_empty_window().update(|w, _| w.window_handle());
        cx.update(|cx| {
            NativeShell::window_opened(oldest.window_id(), None, cx);
            NativeShell::window_opened(newest.window_id(), None, cx);
        });
        newest.update(cx, |_, w, _| w.activate_window()).unwrap();
        cx.run_until_parked();
        assert_eq!(cx.update(|cx| cx.active_window()), Some(newest));
        cx.update(|cx| second_launch(&got, cx));
        cx.run_until_parked();
        assert_eq!(
            cx.update(|cx| cx.active_window()),
            Some(oldest),
            "the oldest window came forward"
        );
    }

    /// beta.5 `second-instance` with a `dilla://` link: the link goes to the running app's link handler (the first one,
    /// as beta.5's `argv.find`), and nothing is brought forward instead.
    #[gpui::test]
    fn second_launch_with_a_link_opens_the_invite(cx: &mut TestAppContext) {
        let root = super::super::links::tests::temp_root("single-link");
        let (_primary, received) = primary_at(&root);
        let second = argv(&[
            "dilla-desktop",
            "--hidden",
            "dilla://invite/chat.example:8443/CODE-1",
            "dilla://invite/other.example/CODE-2",
        ]);
        let launch = second.clone();
        let root2 = root.clone();
        let handed = std::thread::spawn(move || claim_at(&root2, &launch))
            .join()
            .unwrap();
        assert!(matches!(handed, Claim::HandedOver));
        let got = received.recv_timeout(IO_TIMEOUT).expect("the app took it");

        let opened = Rc::new(RefCell::new(Vec::new()));
        let seen = opened.clone();
        cx.update(|cx| {
            NativeShell::on_link(Box::new(move |l, _| seen.borrow_mut().push(l)), cx);
            second_launch(&got, cx);
        });
        assert_eq!(
            *opened.borrow(),
            [Link::Invite {
                host: "chat.example:8443".into(),
                code: "CODE-1".into()
            }]
        );
    }

    /// A launch that finds the lock but no app answering does not become a second app: it gives up and exits.
    #[test]
    fn a_silent_lock_holder_is_not_replaced() {
        let root = super::super::links::tests::temp_root("single-silent");
        let lock = open_lock(&root).unwrap();
        lock.try_lock().unwrap();
        let started = Instant::now();
        assert!(matches!(
            claim_at(&root, &argv(&["dilla-desktop"])),
            Claim::HandedOver
        ));
        assert!(started.elapsed() >= CONNECT_FOR);
    }
}
