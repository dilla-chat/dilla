//! Application audio on Windows through WASAPI process loopback (Windows 10 2004, build 19041, and later).
//!
//! Listing: the default render endpoint's audio sessions that are active (`IAudioSessionManager2`), one entry per
//! process, the system sounds session and Dilla's own process tree (from a Toolhelp process snapshot) left out. A
//! source's serial is its session's place in that enumeration (counted from 1); only its pid is used to capture. Its
//! label is the executable's product name, else the executable's file stem.
//!
//! Capture: `ActivateAudioInterfaceAsync` on the process-loopback virtual device for the chosen process and its child
//! processes (`PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE`): only that tree's playback, never an endpoint mix.
//! The client is asked for 48 kHz stereo f32 (the loopback client converts), event driven, on its own thread with its
//! own multithreaded COM apartment. The capture ends when the process exits or the client fails (`ended`, once), or
//! within 100 ms of the handle's drop.
#![allow(unsafe_code)]

use std::collections::{HashMap, HashSet};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, mpsc};
use std::time::Duration;

use windows::Wdk::System::SystemServices::RtlGetVersion;
use windows::Win32::Foundation::{
    CloseHandle, HANDLE, S_OK, WAIT_EVENT, WAIT_OBJECT_0, WAIT_TIMEOUT,
};
use windows::Win32::Media::Audio::{
    AUDCLNT_BUFFERFLAGS_SILENT, AUDCLNT_SHAREMODE_SHARED, AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM,
    AUDCLNT_STREAMFLAGS_EVENTCALLBACK, AUDCLNT_STREAMFLAGS_LOOPBACK, AUDIOCLIENT_ACTIVATION_PARAMS,
    AUDIOCLIENT_ACTIVATION_PARAMS_0, AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK,
    AUDIOCLIENT_PROCESS_LOOPBACK_PARAMS, ActivateAudioInterfaceAsync, AudioSessionStateActive,
    IActivateAudioInterfaceAsyncOperation, IActivateAudioInterfaceCompletionHandler,
    IActivateAudioInterfaceCompletionHandler_Impl, IAudioCaptureClient, IAudioClient,
    IAudioSessionControl2, IAudioSessionManager2, IMMDeviceEnumerator, MMDeviceEnumerator,
    PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE, VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK,
    WAVEFORMATEX, eConsole, eRender,
};
use windows::Win32::Storage::FileSystem::{
    GetFileVersionInfoSizeW, GetFileVersionInfoW, VerQueryValueW,
};
use windows::Win32::System::Com::StructuredStorage::PROPVARIANT;
use windows::Win32::System::Com::{
    BLOB, CLSCTX_ALL, COINIT_MULTITHREADED, CoCreateInstance, CoInitializeEx, CoUninitialize,
};
use windows::Win32::System::Diagnostics::ToolHelp::{
    CreateToolhelp32Snapshot, PROCESSENTRY32W, Process32FirstW, Process32NextW, TH32CS_SNAPPROCESS,
};
use windows::Win32::System::SystemInformation::OSVERSIONINFOW;
use windows::Win32::System::Threading::{
    CreateEventW, OpenProcess, PROCESS_NAME_WIN32, PROCESS_QUERY_LIMITED_INFORMATION,
    PROCESS_SYNCHRONIZE, QueryFullProcessImageNameW, WaitForMultipleObjects,
};
use windows::Win32::System::Variant::VT_BLOB;
use windows::core::{HRESULT, IUnknown, Interface, PCWSTR, PWSTR, implement, w};

use super::{
    AppAudioError, AppCapture, AppSource, CHANNELS, MAX_CHUNK_BYTES, RATE,
    frames::{SampleFrames, ancestors, exclude_tree_and_ancestors},
    in_tree, windows_build_supported,
};

/// mmreg.h `WAVE_FORMAT_IEEE_FLOAT` (kept here rather than enabling the whole `Win32_Media_Multimedia` module).
const WAVE_FORMAT_IEEE_FLOAT: u16 = 3;
/// At most this many sessions are looked at (beta.5 lists at most 256 applications).
const MAX_SESSIONS: i32 = 1024;
/// The client's buffer: 20 ms, in 100 ns units (Microsoft's ApplicationLoopback sample).
const BUFFER_100NS: i64 = 200_000;

fn server(e: impl std::fmt::Display) -> AppAudioError {
    AppAudioError::Server(e.to_string())
}

/// Below build 19041 there is no process loopback ([`windows_build_supported`]). Reads the version only.
pub(super) fn supported() -> Result<(), AppAudioError> {
    let mut info = OSVERSIONINFOW {
        dwOSVersionInfoSize: std::mem::size_of::<OSVERSIONINFOW>() as u32,
        ..Default::default()
    };
    // SAFETY: `info` is a valid OSVERSIONINFOW with its size set, as RtlGetVersion requires.
    let status = unsafe { RtlGetVersion(&mut info) };
    if status.0 >= 0 && windows_build_supported(info.dwMajorVersion, info.dwBuildNumber) {
        Ok(())
    } else {
        Err(AppAudioError::Unsupported)
    }
}

/// This thread in the multithreaded apartment until dropped (declared first, so every COM object goes before it). Not
/// `Send`: it is dropped on the thread that made it.
struct Com(std::marker::PhantomData<*const ()>);

impl Com {
    fn init() -> Result<Com, AppAudioError> {
        // SAFETY: no reserved pointer; paired with CoUninitialize in Drop on the same thread.
        unsafe { CoInitializeEx(None, COINIT_MULTITHREADED) }
            .ok()
            .map_err(server)?;
        Ok(Com(std::marker::PhantomData))
    }
}

impl Drop for Com {
    fn drop(&mut self) {
        // SAFETY: this thread's successful CoInitializeEx above.
        unsafe { CoUninitialize() }
    }
}

/// A handle closed on drop.
struct Owned(HANDLE);

impl Drop for Owned {
    fn drop(&mut self) {
        // SAFETY: a handle this module opened and nothing else closes.
        let _ = unsafe { CloseHandle(self.0) };
    }
}

/// One process in the snapshot.
struct Process {
    parent: u32,
    exe: String,
}

/// Every process's parent and executable name, from a Toolhelp snapshot (empty when the snapshot fails).
fn processes() -> HashMap<u32, Process> {
    let mut out = HashMap::new();
    // SAFETY: a process snapshot (no module list, so the pid argument is ignored); closed by `Owned`.
    let Ok(snapshot) = (unsafe { CreateToolhelp32Snapshot(TH32CS_SNAPPROCESS, 0) }) else {
        return out;
    };
    let snapshot = Owned(snapshot);
    let mut entry = PROCESSENTRY32W {
        dwSize: std::mem::size_of::<PROCESSENTRY32W>() as u32,
        ..Default::default()
    };
    // SAFETY: `entry` has its size set; the snapshot handle is open.
    let mut more = unsafe { Process32FirstW(snapshot.0, &mut entry) }.is_ok();
    while more && out.len() < 65_536 {
        let len = entry
            .szExeFile
            .iter()
            .position(|&c| c == 0)
            .unwrap_or(entry.szExeFile.len());
        out.insert(
            entry.th32ProcessID,
            Process {
                parent: entry.th32ParentProcessID,
                exe: String::from_utf16_lossy(&entry.szExeFile[..len]),
            },
        );
        // SAFETY: as above.
        more = unsafe { Process32NextW(snapshot.0, &mut entry) }.is_ok();
    }
    out
}

fn stem(path: &str) -> String {
    std::path::Path::new(path)
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default()
}

fn wide(s: &str) -> Vec<u16> {
    s.encode_utf16().chain(std::iter::once(0)).collect()
}

/// A process's executable path (`None` for a process this user may not query).
fn image_path(pid: u32) -> Option<String> {
    // SAFETY: plain query access; closed by `Owned`.
    let process =
        Owned(unsafe { OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, pid) }.ok()?);
    let mut buf = [0u16; 1024];
    let mut len = buf.len() as u32;
    // SAFETY: `buf` holds `len` characters; the call writes at most that many and sets `len` to the count written.
    unsafe {
        QueryFullProcessImageNameW(
            process.0,
            PROCESS_NAME_WIN32,
            PWSTR(buf.as_mut_ptr()),
            &mut len,
        )
    }
    .ok()?;
    Some(String::from_utf16_lossy(buf.get(..len as usize)?))
}

/// The `ProductName` of an executable's version resource, in its first listed language.
fn product_name(path: &str) -> Option<String> {
    let path = wide(path);
    // SAFETY: a NUL-terminated path.
    let size = unsafe { GetFileVersionInfoSizeW(PCWSTR(path.as_ptr()), None) };
    if size == 0 || size > 1 << 20 {
        return None;
    }
    let mut block = vec![0u8; size as usize];
    // SAFETY: `block` is `size` bytes, as GetFileVersionInfoSizeW asked for.
    unsafe { GetFileVersionInfoW(PCWSTR(path.as_ptr()), None, size, block.as_mut_ptr().cast()) }
        .ok()?;
    let query = |key: PCWSTR| -> Option<&[u8]> {
        let mut value = std::ptr::null_mut();
        let mut len = 0u32;
        // SAFETY: `block` is the version resource just read; VerQueryValueW points `value` into it.
        let found =
            unsafe { VerQueryValueW(block.as_ptr().cast(), key, &mut value, &mut len) }.as_bool();
        if !found || value.is_null() {
            return None;
        }
        let start = (value as usize).checked_sub(block.as_ptr() as usize)?;
        block.get(start..)
    };
    // `\VarFileInfo\Translation`: pairs of (language, code page), little-endian u16s.
    let translation = query(w!("\\VarFileInfo\\Translation"))?;
    let (lang, page) = (
        u16::from_le_bytes([*translation.first()?, *translation.get(1)?]),
        u16::from_le_bytes([*translation.get(2)?, *translation.get(3)?]),
    );
    let key = wide(&format!(
        "\\StringFileInfo\\{lang:04x}{page:04x}\\ProductName"
    ));
    let name: Vec<u16> = query(PCWSTR(key.as_ptr()))?
        .as_chunks::<2>()
        .0
        .iter()
        .map(|b| u16::from_le_bytes(*b))
        .take_while(|&c| c != 0)
        .take(super::MAX_LABEL * 2)
        .collect();
    let name = String::from_utf16_lossy(&name).trim().to_owned();
    (!name.is_empty()).then_some(name)
}

fn label(pid: u32, table: &HashMap<u32, Process>) -> String {
    let path = image_path(pid);
    path.as_deref()
        .and_then(product_name)
        .or_else(|| path.as_deref().map(stem))
        .filter(|s| !s.trim().is_empty())
        .or_else(|| table.get(&pid).map(|p| stem(&p.exe)))
        .unwrap_or_default()
}

/// The pids of the default render endpoint's active sessions, in enumeration order (the system sounds session and a
/// session of several processes left out).
fn active_session_pids() -> Result<Vec<u32>, AppAudioError> {
    // SAFETY: COM calls on this thread's multithreaded apartment (`Com` is held by the caller), each with valid
    // arguments; every interface returned is reference-counted by the bindings.
    unsafe {
        let devices: IMMDeviceEnumerator =
            CoCreateInstance(&MMDeviceEnumerator, None, CLSCTX_ALL).map_err(server)?;
        let device = devices
            .GetDefaultAudioEndpoint(eRender, eConsole)
            .map_err(server)?;
        let manager: IAudioSessionManager2 = device.Activate(CLSCTX_ALL, None).map_err(server)?;
        let sessions = manager.GetSessionEnumerator().map_err(server)?;
        let count = sessions.GetCount().map_err(server)?;
        let mut out = Vec::new();
        for i in 0..count.clamp(0, MAX_SESSIONS) {
            let Ok(control) = sessions.GetSession(i) else {
                continue;
            };
            if control.GetState().ok() != Some(AudioSessionStateActive) {
                continue;
            }
            let Ok(control) = control.cast::<IAudioSessionControl2>() else {
                continue;
            };
            if control.IsSystemSoundsSession() == S_OK {
                continue;
            }
            // AUDCLNT_S_NO_SINGLE_PROCESS is a success code with no single pid: such a session is skipped (pid 0).
            match control.GetProcessId() {
                Ok(pid) if pid != 0 => out.push(pid),
                _ => {}
            }
        }
        Ok(out)
    }
}

fn list(root: u32) -> Result<Vec<AppSource>, AppAudioError> {
    let _com = Com::init()?;
    let pids = active_session_pids()?;
    let table = processes();
    let parents: HashMap<u32, u32> = table.iter().map(|(&pid, p)| (pid, p.parent)).collect();
    let mut seen = HashSet::new();
    let sources: Vec<AppSource> = pids
        .into_iter()
        .enumerate()
        .filter(|(_, pid)| seen.insert(*pid))
        .map(|(index, pid)| AppSource {
            serial: index as u64 + 1,
            pid,
            label: String::new(),
        })
        .collect();
    // Loopback captures a chosen process's whole tree, so Dilla's ancestors are left out with its own tree.
    Ok(exclude_tree_and_ancestors(sources, &parents, root)
        .into_iter()
        .map(|mut s| {
            s.label = label(s.pid, &table);
            s
        })
        .collect())
}

pub(super) fn sources(root: u32) -> Result<Vec<AppSource>, AppAudioError> {
    supported()?;
    // A thread of its own: the caller's apartment (the UI thread's may be single-threaded) is never touched.
    std::thread::Builder::new()
        .name("dilla-app-audio-list".to_owned())
        .spawn(move || list(root))
        .map_err(server)?
        .join()
        .map_err(|_| AppAudioError::Server("the listing failed".to_owned()))?
}

/// The activation's completion: signals the waiting thread. Agile (the `implement` default), as
/// `ActivateAudioInterfaceAsync` requires.
#[implement(IActivateAudioInterfaceCompletionHandler)]
struct Activated(mpsc::SyncSender<()>);

impl IActivateAudioInterfaceCompletionHandler_Impl for Activated_Impl {
    fn ActivateCompleted(
        &self,
        _operation: windows::core::Ref<IActivateAudioInterfaceAsyncOperation>,
    ) -> windows::core::Result<()> {
        let _ = self.0.try_send(());
        Ok(())
    }
}

/// An `IAudioClient` on the process-loopback device for `pid` and its child processes.
fn activate(pid: u32) -> Result<IAudioClient, AppAudioError> {
    let params = AUDIOCLIENT_ACTIVATION_PARAMS {
        ActivationType: AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK,
        Anonymous: AUDIOCLIENT_ACTIVATION_PARAMS_0 {
            ProcessLoopbackParams: AUDIOCLIENT_PROCESS_LOOPBACK_PARAMS {
                TargetProcessId: pid,
                ProcessLoopbackMode: PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE,
            },
        },
    };
    // Never dropped: the bindings' `Drop for PROPVARIANT` calls PropVariantClear, which would CoTaskMemFree a VT_BLOB's
    // data, and this blob borrows `params` on the stack. A zeroed PROPVARIANT owns nothing, so not dropping it leaks
    // nothing.
    let mut prop = std::mem::ManuallyDrop::new(PROPVARIANT::default());
    // SAFETY: writing the VT_BLOB arm of a zeroed PROPVARIANT. The blob borrows `params`, which lives until this
    // function returns, after ActivateAudioInterfaceAsync (which reads the parameters during the call) has returned;
    // `prop` is in ManuallyDrop, so PropVariantClear never runs on the borrowed pointer.
    unsafe {
        let inner = &mut *prop.Anonymous.Anonymous;
        inner.vt = VT_BLOB;
        inner.Anonymous.blob = BLOB {
            cbSize: std::mem::size_of::<AUDIOCLIENT_ACTIVATION_PARAMS>() as u32,
            pBlobData: std::ptr::from_ref(&params).cast_mut().cast(),
        };
    }
    let (done_tx, done_rx) = mpsc::sync_channel(1);
    let handler: IActivateAudioInterfaceCompletionHandler = Activated(done_tx).into();
    // SAFETY: the virtual device's path, IAudioClient's IID, the PROPVARIANT above and a live handler.
    let operation = unsafe {
        ActivateAudioInterfaceAsync(
            VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK,
            &IAudioClient::IID,
            Some(std::ptr::from_ref::<PROPVARIANT>(&prop)),
            &handler,
        )
    }
    .map_err(server)?;
    done_rx
        .recv_timeout(Duration::from_secs(5))
        .map_err(|_| AppAudioError::Server("process loopback did not activate".to_owned()))?;
    let mut result = HRESULT(0);
    let mut client: Option<IUnknown> = None;
    // SAFETY: the operation completed (the handler ran); both out pointers are valid.
    unsafe { operation.GetActivateResult(&mut result, &mut client) }.map_err(server)?;
    result.ok().map_err(server)?;
    client
        .ok_or_else(|| AppAudioError::Server("no audio client".to_owned()))?
        .cast::<IAudioClient>()
        .map_err(server)
}

/// A started loopback client, its buffer event and the captured process's handle.
struct Stream {
    client: IAudioClient,
    capture: IAudioCaptureClient,
    event: Owned,
    process: Owned,
}

fn open(pid: u32) -> Result<Stream, AppAudioError> {
    // SAFETY: wait access to the process (its exit ends the capture); closed by `Owned`.
    let process = Owned(
        unsafe {
            OpenProcess(
                PROCESS_SYNCHRONIZE | PROCESS_QUERY_LIMITED_INFORMATION,
                false,
                pid,
            )
        }
        .map_err(|_| AppAudioError::Gone)?,
    );
    let client = activate(pid)?;
    let format = WAVEFORMATEX {
        wFormatTag: WAVE_FORMAT_IEEE_FLOAT,
        nChannels: CHANNELS as u16,
        nSamplesPerSec: RATE,
        nAvgBytesPerSec: RATE * CHANNELS * 4,
        nBlockAlign: (CHANNELS * 4) as u16,
        wBitsPerSample: 32,
        cbSize: 0,
    };
    // SAFETY: COM calls on this thread's apartment with valid arguments; the event is an auto-reset event this module
    // owns, alive for as long as the client (both in `Stream`, the client dropped first).
    unsafe {
        client
            .Initialize(
                AUDCLNT_SHAREMODE_SHARED,
                AUDCLNT_STREAMFLAGS_LOOPBACK
                    | AUDCLNT_STREAMFLAGS_EVENTCALLBACK
                    | AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM,
                BUFFER_100NS,
                0,
                &format,
                None,
            )
            .map_err(server)?;
        let event = Owned(CreateEventW(None, false, false, PCWSTR::null()).map_err(server)?);
        client.SetEventHandle(event.0).map_err(server)?;
        let capture: IAudioCaptureClient = client.GetService().map_err(server)?;
        client.Start().map_err(server)?;
        Ok(Stream {
            client,
            capture,
            event,
            process,
        })
    }
}

/// Hands every captured packet to `cut`.
fn drain(
    capture: &IAudioCaptureClient,
    cut: &mut SampleFrames,
    frames: &mut dyn FnMut(Vec<f32>),
) -> Result<(), AppAudioError> {
    loop {
        // SAFETY: COM calls on the capture thread; GetBuffer's out pointers are valid, and its buffer is read only
        // before the matching ReleaseBuffer.
        unsafe {
            if capture.GetNextPacketSize().map_err(server)? == 0 {
                return Ok(());
            }
            let mut data = std::ptr::null_mut();
            let mut count = 0u32;
            let mut flags = 0u32;
            capture
                .GetBuffer(&mut data, &mut count, &mut flags, None, None)
                .map_err(server)?;
            let n = count as usize * CHANNELS as usize;
            let pushed = if n > MAX_CHUNK_BYTES / 4 {
                Err(AppAudioError::Invalid("chunk"))
            } else if flags & AUDCLNT_BUFFERFLAGS_SILENT.0 as u32 != 0 || data.is_null() {
                cut.push(&vec![0.0; n], &mut *frames)
            } else {
                // The client delivers the format asked for: `count` frames of interleaved stereo f32.
                let samples: Vec<f32> = (0..n)
                    .map(|i| data.cast::<f32>().add(i).read_unaligned())
                    .collect();
                cut.push(&samples, &mut *frames)
            };
            capture.ReleaseBuffer(count).map_err(server)?;
            pushed?;
        }
    }
}

/// Until `stop`, the process's exit, or an error.
fn pump(
    stream: &Stream,
    mut frames: Box<dyn FnMut(Vec<f32>) + Send>,
    stop: &AtomicBool,
) -> Result<(), AppAudioError> {
    let mut cut = SampleFrames::default();
    let handles = [stream.event.0, stream.process.0];
    let exited = WAIT_EVENT(WAIT_OBJECT_0.0 + 1);
    let result = loop {
        if stop.load(Ordering::Acquire) {
            break Ok(());
        }
        // SAFETY: two open handles, owned by `stream`.
        let woke = unsafe { WaitForMultipleObjects(&handles, false, 100) };
        if woke == WAIT_OBJECT_0 {
            if let Err(e) = drain(&stream.capture, &mut cut, &mut *frames) {
                break Err(e);
            }
        } else if woke == exited {
            break Ok(());
        } else if woke != WAIT_TIMEOUT {
            break Err(AppAudioError::Server("waiting for audio failed".to_owned()));
        }
    };
    // SAFETY: a started client on this thread.
    let _ = unsafe { stream.client.Stop() };
    result
}

pub(super) fn start(
    source: &AppSource,
    root: u32,
    frames: Box<dyn FnMut(Vec<f32>) + Send>,
    ended: Box<dyn FnOnce() + Send>,
) -> Result<AppCapture, AppAudioError> {
    supported()?;
    let table = processes();
    let parents: HashMap<u32, u32> = table.iter().map(|(&pid, p)| (pid, p.parent)).collect();
    // Dilla's tree, or an ancestor whose tree (what loopback captures) holds Dilla: refused like a process that is gone.
    if in_tree(source.pid, root, |pid| parents.get(&pid).copied())
        || ancestors(root, &parents).contains(&source.pid)
    {
        return Err(AppAudioError::Gone);
    }
    let stop = Arc::new(AtomicBool::new(false));
    let (ready_tx, ready_rx) = mpsc::channel();
    let (pid, stop_c) = (source.pid, stop.clone());
    std::thread::Builder::new()
        .name("dilla-app-audio".to_owned())
        .spawn(move || {
            let com = match Com::init() {
                Ok(com) => com,
                Err(e) => {
                    let _ = ready_tx.send(Err(e));
                    return;
                }
            };
            let stream = match open(pid) {
                Ok(stream) => stream,
                Err(e) => {
                    let _ = ready_tx.send(Err(e));
                    return;
                }
            };
            let _ = ready_tx.send(Ok(()));
            if let Err(e) = pump(&stream, frames, &stop_c) {
                log::warn!("application audio ended: {e}");
            }
            drop(stream);
            drop(com);
            // The process exited or the client failed: once, unless the handle was dropped.
            if !stop_c.load(Ordering::Acquire) {
                ended();
            }
        })
        .map_err(server)?;
    match ready_rx.recv_timeout(Duration::from_secs(10)) {
        Ok(Ok(())) => Ok(AppCapture::new(stop, source.label.clone())),
        Ok(Err(e)) => Err(e),
        Err(_) => {
            stop.store(true, Ordering::Release);
            Err(AppAudioError::Server(
                "the capture did not start".to_owned(),
            ))
        }
    }
}
