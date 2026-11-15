//! The Windows end of the second-launch handover ([`super::single_instance`]): a named pipe only this user can open,
//! whose server a second launch checks before it writes.
//!
//! - The name carries the user's SID and a hash of the data root: `\\.\pipe\dilla-desktop-<SID>-<hash>`.
//! - The app makes the pipe with a protected DACL that allows the current user only (`D:P(A;;GA;;;<SID>)`), its first
//!   instance with `FILE_FLAG_FIRST_PIPE_INSTANCE` (a name someone else made first fails visibly), inbound only and
//!   refusing remote clients.
//! - A second launch opens it, then asks who serves it (`GetNamedPipeServerProcessId`, that process's token user) and
//!   writes only when that is its own SID.
//!
//! Only `std` and `windows-sys` are used here, so the file checks on its own for the Windows target.

#![allow(unsafe_code)]

use std::fs::{File, OpenOptions};
use std::io;
use std::os::windows::io::{AsRawHandle as _, FromRawHandle as _};

use windows_sys::Win32::Foundation::{
    CloseHandle, ERROR_PIPE_CONNECTED, HANDLE, INVALID_HANDLE_VALUE, LocalFree,
};
use windows_sys::Win32::Security::Authorization::{
    ConvertSidToStringSidW, ConvertStringSecurityDescriptorToSecurityDescriptorW, SDDL_REVISION_1,
};
use windows_sys::Win32::Security::{
    GetTokenInformation, PSECURITY_DESCRIPTOR, SECURITY_ATTRIBUTES, TOKEN_QUERY, TOKEN_USER,
    TokenUser,
};
use windows_sys::Win32::Storage::FileSystem::{FILE_FLAG_FIRST_PIPE_INSTANCE, PIPE_ACCESS_INBOUND};
use windows_sys::Win32::System::Pipes::{
    ConnectNamedPipe, CreateNamedPipeW, GetNamedPipeServerProcessId, PIPE_READMODE_BYTE,
    PIPE_REJECT_REMOTE_CLIENTS, PIPE_TYPE_BYTE, PIPE_UNLIMITED_INSTANCES, PIPE_WAIT,
};
use windows_sys::Win32::System::Threading::{
    GetCurrentProcess, OpenProcess, OpenProcessToken, PROCESS_QUERY_LIMITED_INFORMATION,
};

/// A handle closed when dropped.
struct Owned(HANDLE);

impl Drop for Owned {
    fn drop(&mut self) {
        // SAFETY: a handle this struct owns, closed once.
        unsafe { CloseHandle(self.0) };
    }
}

/// The string SID of the user `process` runs as.
fn process_sid(process: HANDLE) -> io::Result<String> {
    let mut token: HANDLE = std::ptr::null_mut();
    // SAFETY: `process` is a valid process handle; `token` receives a handle we own.
    if unsafe { OpenProcessToken(process, TOKEN_QUERY, &mut token) } == 0 {
        return Err(io::Error::last_os_error());
    }
    let token = Owned(token);
    let mut len = 0u32;
    // SAFETY: a size query with no buffer.
    unsafe { GetTokenInformation(token.0, TokenUser, std::ptr::null_mut(), 0, &mut len) };
    // u64 words keep TOKEN_USER's pointer alignment.
    let mut buf = vec![0u64; (len as usize).div_ceil(8).max(1)];
    // SAFETY: `buf` holds at least `len` bytes.
    if unsafe { GetTokenInformation(token.0, TokenUser, buf.as_mut_ptr().cast(), len, &mut len) }
        == 0
    {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: the call filled `buf` with a TOKEN_USER whose SID points inside `buf`.
    let sid = unsafe { (*buf.as_ptr().cast::<TOKEN_USER>()).User.Sid };
    let mut text: *mut u16 = std::ptr::null_mut();
    // SAFETY: a valid SID; `text` receives a LocalAlloc'd NUL-terminated string we free below.
    if unsafe { ConvertSidToStringSidW(sid, &mut text) } == 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: `text` is NUL-terminated.
    let out = unsafe {
        let mut n = 0;
        while *text.add(n) != 0 {
            n += 1;
        }
        String::from_utf16_lossy(std::slice::from_raw_parts(text, n))
    };
    // SAFETY: allocated by ConvertSidToStringSidW.
    unsafe { LocalFree(text.cast()) };
    Ok(out)
}

/// The current user's string SID.
pub fn user_sid() -> io::Result<String> {
    // SAFETY: the pseudo handle of this process needs no closing.
    process_sid(unsafe { GetCurrentProcess() })
}

/// `\\.\pipe\dilla-desktop-<sid>-<hash>`.
pub fn pipe_name(sid: &str, root_hash: u64) -> String {
    format!(r"\\.\pipe\dilla-desktop-{sid}-{root_hash:016x}")
}

fn wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain([0]).collect()
}

/// The app's end: makes pipe instances only `sid` may open, one connection at a time.
pub struct Listener {
    name: Vec<u16>,
    descriptor: PSECURITY_DESCRIPTOR,
    first: Option<HANDLE>,
}

// SAFETY: the descriptor and the handle are process-wide kernel and heap objects; only the serving thread uses them.
unsafe impl Send for Listener {}

impl Drop for Listener {
    fn drop(&mut self) {
        if let Some(h) = self.first.take() {
            drop(Owned(h));
        }
        // SAFETY: allocated by ConvertStringSecurityDescriptorToSecurityDescriptorW.
        unsafe { LocalFree(self.descriptor.cast()) };
    }
}

impl Listener {
    /// Makes the first instance of `name` for user `sid`; fails when the name exists already.
    pub fn listen(name: &str, sid: &str) -> io::Result<Self> {
        let sddl = wide(&format!("D:P(A;;GA;;;{sid})"));
        let mut descriptor: PSECURITY_DESCRIPTOR = std::ptr::null_mut();
        // SAFETY: a NUL-terminated SDDL string; `descriptor` receives a LocalAlloc'd descriptor freed on drop.
        if unsafe {
            ConvertStringSecurityDescriptorToSecurityDescriptorW(
                sddl.as_ptr(),
                SDDL_REVISION_1,
                &mut descriptor,
                std::ptr::null_mut(),
            )
        } == 0
        {
            return Err(io::Error::last_os_error());
        }
        let mut listener = Self {
            name: wide(name),
            descriptor,
            first: None,
        };
        listener.first = Some(listener.create(true)?);
        Ok(listener)
    }

    fn create(&self, first: bool) -> io::Result<HANDLE> {
        let attributes = SECURITY_ATTRIBUTES {
            nLength: std::mem::size_of::<SECURITY_ATTRIBUTES>() as u32,
            lpSecurityDescriptor: self.descriptor,
            bInheritHandle: 0,
        };
        let open_mode = PIPE_ACCESS_INBOUND
            | if first {
                FILE_FLAG_FIRST_PIPE_INSTANCE
            } else {
                0
            };
        // SAFETY: a NUL-terminated name and attributes that outlive the call.
        let handle = unsafe {
            CreateNamedPipeW(
                self.name.as_ptr(),
                open_mode,
                PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT | PIPE_REJECT_REMOTE_CLIENTS,
                PIPE_UNLIMITED_INSTANCES,
                0,
                64 * 1024,
                0,
                &attributes,
            )
        };
        if handle == INVALID_HANDLE_VALUE {
            Err(io::Error::last_os_error())
        } else {
            Ok(handle)
        }
    }

    /// Waits for the next second launch and hands its end of the pipe over as a `File` (read it to the end).
    pub fn accept(&mut self) -> io::Result<File> {
        let handle = match self.first.take() {
            Some(h) => h,
            None => self.create(false)?,
        };
        // SAFETY: our pipe handle, waited on synchronously.
        let connected = unsafe { ConnectNamedPipe(handle, std::ptr::null_mut()) } != 0
            || io::Error::last_os_error().raw_os_error() == Some(ERROR_PIPE_CONNECTED as i32);
        // SAFETY: the handle is ours and is closed once, by the File.
        let pipe = unsafe { File::from_raw_handle(handle as _) };
        if connected {
            Ok(pipe)
        } else {
            Err(io::Error::last_os_error())
        }
    }
}

/// A second launch's end: opens `name` and returns it only when the process serving it runs as `sid`.
pub fn connect(name: &str, sid: &str) -> io::Result<File> {
    let pipe = OpenOptions::new().write(true).open(name)?;
    let denied = |e: io::Error| io::Error::new(io::ErrorKind::PermissionDenied, e);
    server_sid(&pipe).map_err(denied).and_then(|server| {
        if server == sid {
            Ok(pipe)
        } else {
            Err(denied(io::Error::other(
                "the handover pipe is served by another user's process",
            )))
        }
    })
}

/// The SID of the process serving `pipe`.
fn server_sid(pipe: &File) -> io::Result<String> {
    let mut pid = 0u32;
    // SAFETY: an open pipe handle.
    if unsafe { GetNamedPipeServerProcessId(pipe.as_raw_handle() as HANDLE, &mut pid) } == 0 {
        return Err(io::Error::last_os_error());
    }
    // SAFETY: a query-only open of the server process; closed by `Owned`.
    let process = unsafe { OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, 0, pid) };
    if process.is_null() {
        return Err(io::Error::last_os_error());
    }
    let process = Owned(process);
    process_sid(process.0)
}
