//! Application audio on macOS through CoreAudio process taps (macOS 14.2 and later).
//!
//! Listing: the HAL's process objects (`kAudioHardwarePropertyProcessObjectList`) that are playing
//! (`kAudioProcessPropertyIsRunningOutput`), Dilla's own process tree (from the kernel's process table) left out. A
//! source's serial is its process object's id; only its pid is used to capture. Its label is the application's
//! `NSRunningApplication.localizedName`, else the process's name.
//!
//! Capture: a private `CATapDescription` mixing the chosen process (and those of its descendants that play, never one
//! in Dilla's tree) down to stereo, unmuted, so the user still hears it; `AudioHardwareCreateProcessTap`; a private
//! aggregate device that holds the tap (clocked by the default output device when there is one), and an IOProc on it.
//! The IOProc only copies the tap's samples out; a thread of its own converts them to 48 kHz stereo f32 with an
//! `AudioConverter` when the device runs at another rate, cuts them into 10 ms frames and hands them over. The capture
//! ends when the process exits or the capture fails (`ended`, once), or within 100 ms of the handle's drop; either way
//! the IOProc is stopped and destroyed, then the aggregate device, then the tap.
//!
//! The first tap asks the user for the "system audio recording" permission (TCC AudioCapture,
//! `NSAudioCaptureUsageDescription` in the bundle's Info.plist); until it is granted the tap delivers silence.
//!
//! `AudioHardwareCreateProcessTap` and `AudioHardwareDestroyProcessTap` are looked up at run time (`dlsym`): the app
//! runs on macOS 13, whose CoreAudio has neither, and a link-time reference would stop it from launching there.
#![allow(unsafe_code)]

use std::collections::HashMap;
use std::ffi::{CStr, c_void};
use std::ptr::NonNull;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, mpsc};
use std::time::{Duration, Instant};

use objc2::rc::{Retained, autoreleasepool};
use objc2::runtime::AnyObject;
use objc2::{AnyThread, available};
use objc2_app_kit::NSRunningApplication;
use objc2_audio_toolbox::{
    AudioConverterDispose, AudioConverterFillComplexBuffer, AudioConverterNew, AudioConverterRef,
};
use objc2_core_audio::{
    AudioDeviceCreateIOProcID, AudioDeviceDestroyIOProcID, AudioDeviceIOProcID, AudioDeviceStart,
    AudioDeviceStop, AudioHardwareCreateAggregateDevice, AudioHardwareDestroyAggregateDevice,
    AudioObjectGetPropertyData, AudioObjectGetPropertyDataSize, AudioObjectID,
    AudioObjectPropertyAddress, AudioObjectPropertySelector, CATapDescription, CATapMuteBehavior,
    kAudioAggregateDeviceIsPrivateKey, kAudioAggregateDeviceIsStackedKey,
    kAudioAggregateDeviceMainSubDeviceKey, kAudioAggregateDeviceNameKey,
    kAudioAggregateDeviceSubDeviceListKey, kAudioAggregateDeviceTapAutoStartKey,
    kAudioAggregateDeviceTapListKey, kAudioAggregateDeviceUIDKey, kAudioDevicePropertyDeviceUID,
    kAudioDevicePropertyNominalSampleRate, kAudioHardwarePropertyDefaultSystemOutputDevice,
    kAudioHardwarePropertyProcessObjectList, kAudioObjectPropertyElementMain,
    kAudioObjectPropertyScopeGlobal, kAudioObjectSystemObject,
    kAudioProcessPropertyIsRunningOutput, kAudioProcessPropertyPID, kAudioSubDeviceUIDKey,
    kAudioSubTapDriftCompensationKey, kAudioSubTapUIDKey, kAudioTapPropertyFormat,
};
use objc2_core_audio_types::{
    AudioBuffer, AudioBufferList, AudioStreamBasicDescription, AudioStreamPacketDescription,
    AudioTimeStamp, kAudioFormatFlagIsFloat, kAudioFormatFlagIsPacked, kAudioFormatLinearPCM,
};
use objc2_core_foundation::CFDictionary;
use objc2_foundation::{NSArray, NSDictionary, NSNumber, NSString};

use super::{
    AppAudioError, AppCapture, AppSource, CHANNELS, MAX_CHUNK_BYTES, RATE,
    frames::{SampleFrames, cap_label, exclude_tree},
    in_tree,
};

/// At most this many process objects are looked at (beta.5 lists at most 256 applications).
const MAX_OBJECTS: usize = 4096;
/// Buffers waiting between the IOProc and the capture thread (each one IO cycle, typically 10 ms): beyond this the
/// IOProc drops a buffer rather than block the audio thread.
const QUEUE: usize = 64;
/// How long the capture thread waits for a buffer before it looks at `stop` again (the drop ends it within 100 ms).
const POLL: Duration = Duration::from_millis(20);
/// How often the captured process is checked for having exited.
const ALIVE_EVERY: Duration = Duration::from_millis(100);
/// The converter's input proc: no more input for now (any non-zero status; the converter keeps its state).
const NO_INPUT: i32 = 0x6e6f_6474; // 'nodt'

fn server(what: &str, status: i32) -> AppAudioError {
    AppAudioError::Server(format!("{what} ({status})"))
}

/// Below 14.2 there are no process taps.
fn supported() -> Result<(), AppAudioError> {
    if available!(macos = 14.2) {
        Ok(())
    } else {
        Err(AppAudioError::Unsupported)
    }
}

type CreateTap = unsafe extern "C-unwind" fn(*const CATapDescription, *mut AudioObjectID) -> i32;
type DestroyTap = unsafe extern "C-unwind" fn(AudioObjectID) -> i32;

/// CoreAudio's tap functions, found at run time (macOS 13 has neither).
struct TapApi {
    create: CreateTap,
    destroy: DestroyTap,
}

fn tap_api() -> Result<TapApi, AppAudioError> {
    let find = |name: &CStr| {
        // SAFETY: a NUL-terminated symbol name looked up in the images already loaded (CoreAudio is linked).
        let f = unsafe { libc::dlsym(libc::RTLD_DEFAULT, name.as_ptr()) };
        (!f.is_null())
            .then_some(f)
            .ok_or(AppAudioError::Unsupported)
    };
    let (create, destroy) = (
        find(c"AudioHardwareCreateProcessTap")?,
        find(c"AudioHardwareDestroyProcessTap")?,
    );
    // SAFETY: the symbols' C signatures (AudioHardwareTapping.h): `OSStatus (CATapDescription*, AudioObjectID*)` and
    // `OSStatus (AudioObjectID)`.
    unsafe {
        Ok(TapApi {
            create: std::mem::transmute::<*mut c_void, CreateTap>(create),
            destroy: std::mem::transmute::<*mut c_void, DestroyTap>(destroy),
        })
    }
}

fn address(selector: AudioObjectPropertySelector) -> AudioObjectPropertyAddress {
    AudioObjectPropertyAddress {
        mSelector: selector,
        mScope: kAudioObjectPropertyScopeGlobal,
        mElement: kAudioObjectPropertyElementMain,
    }
}

/// A fixed-size property of `object` (a `T` the HAL writes in full).
fn get<T: Copy>(
    object: AudioObjectID,
    selector: AudioObjectPropertySelector,
    zero: T,
) -> Result<T, i32> {
    let mut value = zero;
    let mut size = std::mem::size_of::<T>() as u32;
    let address = address(selector);
    // SAFETY: `value` is `size` bytes; the HAL writes at most that many and sets `size` to what it wrote.
    let status = unsafe {
        AudioObjectGetPropertyData(
            object,
            NonNull::from(&address),
            0,
            std::ptr::null(),
            NonNull::from(&mut size),
            NonNull::from(&mut value).cast(),
        )
    };
    if status != 0 {
        Err(status)
    } else if size as usize != std::mem::size_of::<T>() {
        Err(-1)
    } else {
        Ok(value)
    }
}

/// The system object's process objects (every process that has a CoreAudio client).
fn process_objects() -> Result<Vec<AudioObjectID>, AppAudioError> {
    let address = address(kAudioHardwarePropertyProcessObjectList);
    let system = kAudioObjectSystemObject as AudioObjectID;
    let mut size = 0u32;
    // SAFETY: the system object and a valid address; `size` is written.
    let status = unsafe {
        AudioObjectGetPropertyDataSize(
            system,
            NonNull::from(&address),
            0,
            std::ptr::null(),
            NonNull::from(&mut size),
        )
    };
    if status != 0 {
        return Err(server("the process list", status));
    }
    let count = (size as usize / std::mem::size_of::<AudioObjectID>()).min(MAX_OBJECTS);
    if count == 0 {
        return Ok(Vec::new());
    }
    let mut out = vec![0 as AudioObjectID; count];
    let mut size = (count * std::mem::size_of::<AudioObjectID>()) as u32;
    // SAFETY: `out` is `size` bytes; the HAL writes at most that many and sets `size` to what it wrote.
    let status = unsafe {
        AudioObjectGetPropertyData(
            system,
            NonNull::from(&address),
            0,
            std::ptr::null(),
            NonNull::from(&mut size),
            NonNull::new(out.as_mut_ptr()).expect("non-empty").cast(),
        )
    };
    if status != 0 {
        return Err(server("the process list", status));
    }
    out.truncate(size as usize / std::mem::size_of::<AudioObjectID>());
    Ok(out)
}

/// A process object's pid (0 when it has none).
fn pid_of(object: AudioObjectID) -> u32 {
    get::<libc::pid_t>(object, kAudioProcessPropertyPID, 0)
        .ok()
        .and_then(|p| u32::try_from(p).ok())
        .unwrap_or(0)
}

/// A process's kernel record (`None` once it is gone or for a process this user may not see).
fn bsd_info(pid: u32) -> Option<libc::proc_bsdinfo> {
    let pid = libc::c_int::try_from(pid).ok()?;
    // SAFETY: a zeroed plain C struct.
    let mut info: libc::proc_bsdinfo = unsafe { std::mem::zeroed() };
    let size = std::mem::size_of::<libc::proc_bsdinfo>() as libc::c_int;
    // SAFETY: `info` is `size` bytes, as PROC_PIDTBSDINFO writes.
    let wrote = unsafe {
        libc::proc_pidinfo(
            pid,
            libc::PROC_PIDTBSDINFO,
            0,
            std::ptr::from_mut(&mut info).cast(),
            size,
        )
    };
    (wrote == size).then_some(info)
}

fn parent(pid: u32) -> Option<u32> {
    bsd_info(pid).map(|i| i.pbi_ppid)
}

/// Whether `pid` still runs (not gone, not a zombie waiting for its parent).
fn alive(pid: u32) -> bool {
    bsd_info(pid).is_some_and(|i| i.pbi_status != libc::SZOMB)
}

/// `pids` and all their ancestors' parents (what [`exclude_tree`] walks).
fn parents(pids: impl IntoIterator<Item = u32>) -> HashMap<u32, u32> {
    let mut out = HashMap::new();
    for pid in pids {
        let mut pid = pid;
        for _ in 0..128 {
            if pid <= 1 || out.contains_key(&pid) {
                break;
            }
            let Some(up) = parent(pid) else { break };
            out.insert(pid, up);
            pid = up;
        }
    }
    out
}

/// The application's localized name, else the process's name.
fn label(pid: u32) -> String {
    let app = autoreleasepool(|_| {
        libc::pid_t::try_from(pid)
            .ok()
            .and_then(NSRunningApplication::runningApplicationWithProcessIdentifier)
            .and_then(|app| app.localizedName())
            .map(|name| name.to_string())
    });
    if let Some(name) = app.filter(|n| !n.trim().is_empty()) {
        return name;
    }
    let mut buf = [0u8; 256];
    // SAFETY: `buf` is 256 bytes; proc_name writes at most that many and returns the length.
    let len = unsafe {
        libc::proc_name(
            pid as libc::c_int,
            buf.as_mut_ptr().cast(),
            buf.len() as u32,
        )
    };
    usize::try_from(len)
        .ok()
        .and_then(|len| buf.get(..len))
        .map(|name| String::from_utf8_lossy(name).into_owned())
        .unwrap_or_default()
}

pub(super) fn sources(root: u32) -> Result<Vec<AppSource>, AppAudioError> {
    supported()?;
    let mut seen = std::collections::HashSet::new();
    let playing: Vec<AppSource> = process_objects()?
        .into_iter()
        .filter(|&o| get::<u32>(o, kAudioProcessPropertyIsRunningOutput, 0).is_ok_and(|r| r != 0))
        .map(|o| AppSource {
            serial: u64::from(o),
            pid: pid_of(o),
            label: String::new(),
        })
        .filter(|s| s.pid != 0 && seen.insert(s.pid))
        .collect();
    let table = parents(playing.iter().map(|s| s.pid));
    Ok(exclude_tree(playing, &table, root)
        .into_iter()
        .map(|mut s| {
            s.label = cap_label(&label(s.pid), s.pid);
            s
        })
        .collect())
}

/// The tap's process objects: `pid`'s and those of its descendants that have one, none in Dilla's tree (`root`).
fn tapped_objects(pid: u32, root: u32) -> Result<Vec<AudioObjectID>, AppAudioError> {
    let objects: Vec<(AudioObjectID, u32)> = process_objects()?
        .into_iter()
        .map(|o| (o, pid_of(o)))
        .filter(|&(_, p)| p != 0)
        .collect();
    let table = parents(objects.iter().map(|&(_, p)| p));
    let up = |p: u32| table.get(&p).copied();
    let chosen: Vec<AudioObjectID> = objects
        .iter()
        .filter(|&&(_, p)| in_tree(p, pid, up) && !in_tree(p, root, up))
        .map(|&(o, _)| o)
        .collect();
    if objects.iter().any(|&(_, p)| p == pid) {
        Ok(chosen)
    } else {
        Err(AppAudioError::Gone)
    }
}

fn ns(key: &CStr) -> Retained<NSString> {
    NSString::from_str(key.to_str().unwrap_or_default())
}

fn dict(entries: &[(&CStr, &AnyObject)]) -> Retained<NSDictionary<NSString, AnyObject>> {
    let keys: Vec<Retained<NSString>> = entries.iter().map(|(k, _)| ns(k)).collect();
    let keys: Vec<&NSString> = keys.iter().map(|k| &**k).collect();
    let values: Vec<&AnyObject> = entries.iter().map(|&(_, v)| v).collect();
    NSDictionary::from_slices(&keys, &values)
}

/// The default output device's UID (the aggregate's clock), if there is one.
fn output_uid() -> Option<Retained<NSString>> {
    let device = get::<AudioObjectID>(
        kAudioObjectSystemObject as AudioObjectID,
        kAudioHardwarePropertyDefaultSystemOutputDevice,
        0,
    )
    .ok()
    .filter(|&d| d != 0)?;
    let uid =
        get::<*mut NSString>(device, kAudioDevicePropertyDeviceUID, std::ptr::null_mut()).ok()?;
    // SAFETY: the property is a CFStringRef the caller owns (+1), toll-free bridged to NSString.
    unsafe { Retained::from_raw(uid) }
}

/// The IOProc's side: where the tap's samples go.
struct Io {
    tx: mpsc::SyncSender<Vec<f32>>,
}

/// The tap's input buffers as interleaved stereo (a mono buffer is doubled; channels past the second are dropped).
fn stereo(buffers: &[AudioBuffer]) -> Vec<f32> {
    // SAFETY (each slice): the HAL's buffers hold `mDataByteSize` bytes of native f32 for the IO cycle.
    let view = |b: &AudioBuffer| -> &[f32] {
        if b.mData.is_null() {
            &[]
        } else {
            unsafe {
                std::slice::from_raw_parts(b.mData.cast::<f32>(), b.mDataByteSize as usize / 4)
            }
        }
    };
    match buffers {
        [] => Vec::new(),
        [one] => {
            let (data, ch) = (view(one), one.mNumberChannels.max(1) as usize);
            data.chunks_exact(ch)
                .flat_map(|f| [f[0], *f.get(1).unwrap_or(&f[0])])
                .collect()
        }
        [left, right, ..] => {
            let (l, r) = (view(left), view(right));
            l.iter().zip(r).flat_map(|(&l, &r)| [l, r]).collect()
        }
    }
}

/// The IOProc: copies the tap's input out (never blocks); the output, if the clock device has one, stays silent.
unsafe extern "C-unwind" fn io_proc(
    _device: AudioObjectID,
    _now: NonNull<AudioTimeStamp>,
    input: NonNull<AudioBufferList>,
    _input_time: NonNull<AudioTimeStamp>,
    output: NonNull<AudioBufferList>,
    _output_time: NonNull<AudioTimeStamp>,
    client: *mut c_void,
) -> i32 {
    // SAFETY: `client` is the `Io` this module boxed, alive until the IOProc is destroyed; the buffer lists are the
    // HAL's for this cycle, `mNumberBuffers` long.
    unsafe {
        let io = &*client.cast::<Io>();
        let list = input.as_ref();
        let buffers =
            std::slice::from_raw_parts(list.mBuffers.as_ptr(), list.mNumberBuffers as usize);
        let samples = stereo(buffers);
        if !samples.is_empty() {
            let _ = io.tx.try_send(samples);
        }
        let out = output.as_ptr();
        let outs = std::slice::from_raw_parts_mut(
            (&raw mut (*out).mBuffers).cast::<AudioBuffer>(),
            (*out).mNumberBuffers as usize,
        );
        for b in outs {
            if !b.mData.is_null() {
                std::ptr::write_bytes(b.mData.cast::<u8>(), 0, b.mDataByteSize as usize);
            }
        }
    }
    0
}

/// Everything CoreAudio holds for one capture, torn down in reverse on drop: the IOProc stopped (the HAL waits for a
/// running cycle) and destroyed, the aggregate device destroyed, the tap destroyed, then the IOProc's state freed.
struct Tap {
    api: TapApi,
    tap: AudioObjectID,
    aggregate: AudioObjectID,
    proc_id: AudioDeviceIOProcID,
    started: bool,
    io: *mut Io,
}

impl Drop for Tap {
    fn drop(&mut self) {
        // SAFETY: objects this module created and nothing else destroys, in reverse order; the IOProc is stopped and
        // destroyed before its state is freed.
        unsafe {
            if self.started {
                AudioDeviceStop(self.aggregate, self.proc_id);
            }
            if self.proc_id.is_some() {
                AudioDeviceDestroyIOProcID(self.aggregate, self.proc_id);
            }
            if self.aggregate != 0 {
                AudioHardwareDestroyAggregateDevice(self.aggregate);
            }
            if self.tap != 0 {
                (self.api.destroy)(self.tap);
            }
            if !self.io.is_null() {
                drop(Box::from_raw(self.io));
            }
        }
    }
}

/// The tap's format: linear PCM f32 is all an IOProc gets, anything else is refused.
fn check_format(tap: AudioObjectID) -> Result<(), AppAudioError> {
    // SAFETY: a zeroed plain C struct.
    let zero: AudioStreamBasicDescription = unsafe { std::mem::zeroed() };
    let f = get(tap, kAudioTapPropertyFormat, zero).map_err(|s| server("the tap's format", s))?;
    if f.mFormatID == kAudioFormatLinearPCM
        && f.mFormatFlags & kAudioFormatFlagIsFloat != 0
        && f.mBitsPerChannel == 32
        && f.mChannelsPerFrame >= 1
    {
        Ok(())
    } else {
        Err(AppAudioError::Server(format!(
            "the tap's format is not f32 PCM ({:#x}, {:#x}, {} bits)",
            f.mFormatID, f.mFormatFlags, f.mBitsPerChannel
        )))
    }
}

/// Taps `pid` (and its playing descendants) and starts an aggregate device on it; its samples go to `tx`. Returns the
/// running tap and the device's sample rate.
fn open(pid: u32, root: u32, tx: mpsc::SyncSender<Vec<f32>>) -> Result<(Tap, f64), AppAudioError> {
    let api = tap_api()?;
    let objects = tapped_objects(pid, root)?;
    let mut tap = Tap {
        api,
        tap: 0,
        aggregate: 0,
        proc_id: None,
        started: false,
        io: std::ptr::null_mut(),
    };
    let uuid = autoreleasepool(|_| -> Result<String, AppAudioError> {
        let numbers: Vec<Retained<NSNumber>> =
            objects.iter().map(|&o| NSNumber::new_u32(o)).collect();
        let processes = NSArray::from_retained_slice(&numbers);
        // SAFETY: a fresh allocation initialised with an array of NSNumbers holding process object ids, as the
        // initializer takes; the setters take plain values.
        let description = unsafe {
            let d = CATapDescription::initStereoMixdownOfProcesses(
                CATapDescription::alloc(),
                &processes,
            );
            d.setName(&NSString::from_str("Dilla application audio"));
            d.setPrivate(true);
            d.setMuteBehavior(CATapMuteBehavior::Unmuted);
            d
        };
        // SAFETY: a valid description and an out pointer.
        let status = unsafe { (tap.api.create)(Retained::as_ptr(&description), &mut tap.tap) };
        if status != 0 || tap.tap == 0 {
            tap.tap = 0;
            return Err(server("the process tap", status));
        }
        // SAFETY: a plain getter on a live description.
        Ok(unsafe { description.UUID() }.UUIDString().to_string())
    })?;
    check_format(tap.tap)?;

    autoreleasepool(|_| -> Result<(), AppAudioError> {
        let tap_uid = NSString::from_str(&uuid);
        let sub_tap = dict(&[
            (kAudioSubTapUIDKey, &tap_uid),
            (kAudioSubTapDriftCompensationKey, &NSNumber::new_bool(true)),
        ]);
        let taps = NSArray::from_retained_slice(&[sub_tap]);
        let uid = NSString::from_str(&format!("dev.dilla.desktop.app-audio.{uuid}"));
        let name = NSString::from_str("Dilla application audio");
        let (yes, no) = (NSNumber::new_bool(true), NSNumber::new_bool(false));
        let mut entries: Vec<(&CStr, &AnyObject)> = vec![
            (kAudioAggregateDeviceUIDKey, &uid),
            (kAudioAggregateDeviceNameKey, &name),
            (kAudioAggregateDeviceIsPrivateKey, &yes),
            (kAudioAggregateDeviceIsStackedKey, &no),
            (kAudioAggregateDeviceTapAutoStartKey, &yes),
            (kAudioAggregateDeviceTapListKey, &taps),
        ];
        let output = output_uid();
        let subs = output
            .as_ref()
            .map(|out| NSArray::from_retained_slice(&[dict(&[(kAudioSubDeviceUIDKey, &**out)])]));
        if let (Some(out), Some(subs)) = (&output, &subs) {
            entries.push((kAudioAggregateDeviceMainSubDeviceKey, out));
            entries.push((kAudioAggregateDeviceSubDeviceListKey, subs));
        }
        let description = dict(&entries);
        // SAFETY: NSDictionary is toll-free bridged to CFDictionary; it outlives the call.
        let description = unsafe { &*Retained::as_ptr(&description).cast::<CFDictionary>() };
        // SAFETY: a valid description and an out pointer.
        let status = unsafe {
            AudioHardwareCreateAggregateDevice(description, NonNull::from(&mut tap.aggregate))
        };
        if status != 0 || tap.aggregate == 0 {
            tap.aggregate = 0;
            return Err(server("the aggregate device", status));
        }
        Ok(())
    })?;

    let rate = get::<f64>(tap.aggregate, kAudioDevicePropertyNominalSampleRate, 0.0)
        .map_err(|s| server("the device's rate", s))?;
    if !(8_000.0..=384_000.0).contains(&rate) {
        return Err(AppAudioError::Server(format!(
            "the device runs at {rate} Hz"
        )));
    }
    tap.io = Box::into_raw(Box::new(Io { tx }));
    // SAFETY: a live device, the IOProc above and its state (freed only after the IOProc is destroyed).
    let status = unsafe {
        AudioDeviceCreateIOProcID(
            tap.aggregate,
            Some(io_proc),
            tap.io.cast(),
            NonNull::from(&mut tap.proc_id),
        )
    };
    if status != 0 || tap.proc_id.is_none() {
        tap.proc_id = None;
        return Err(server("the IOProc", status));
    }
    // SAFETY: the IOProc just created on this device.
    let status = unsafe { AudioDeviceStart(tap.aggregate, tap.proc_id) };
    if status != 0 {
        return Err(server("starting the device", status));
    }
    tap.started = true;
    Ok((tap, rate))
}

/// Interleaved stereo f32 at `from` Hz to 48 kHz (`AudioConverter`), keeping its state between buffers.
pub(super) struct Resampler {
    converter: AudioConverterRef,
    /// Waiting for the converter.
    pending: Vec<f32>,
    /// Handed to the converter by the input proc: it reads it until the proc is called again.
    lent: Vec<f32>,
}

// SAFETY: the converter is used from one thread at a time (`&mut self`), as AudioConverter allows.
unsafe impl Send for Resampler {}

fn stereo_f32(rate: f64) -> AudioStreamBasicDescription {
    AudioStreamBasicDescription {
        mSampleRate: rate,
        mFormatID: kAudioFormatLinearPCM,
        mFormatFlags: kAudioFormatFlagIsFloat | kAudioFormatFlagIsPacked,
        mBytesPerPacket: CHANNELS * 4,
        mFramesPerPacket: 1,
        mBytesPerFrame: CHANNELS * 4,
        mChannelsPerFrame: CHANNELS,
        mBitsPerChannel: 32,
        mReserved: 0,
    }
}

unsafe extern "C-unwind" fn feed(
    _converter: AudioConverterRef,
    packets: NonNull<u32>,
    data: NonNull<AudioBufferList>,
    _descriptions: *mut *mut AudioStreamPacketDescription,
    state: *mut c_void,
) -> i32 {
    // SAFETY: `state` is the `Resampler` whose `push` is running; the converter is done with the last lent buffer
    // when it asks again, and reads the new one until the next call (it stays in `lent`).
    unsafe {
        let r = &mut *state.cast::<Resampler>();
        r.lent.clear();
        if r.pending.is_empty() {
            *packets.as_ptr() = 0;
            return NO_INPUT;
        }
        std::mem::swap(&mut r.lent, &mut r.pending);
        let list = &mut *data.as_ptr();
        list.mNumberBuffers = 1;
        list.mBuffers[0] = AudioBuffer {
            mNumberChannels: CHANNELS,
            mDataByteSize: (r.lent.len() * 4) as u32,
            mData: r.lent.as_mut_ptr().cast(),
        };
        *packets.as_ptr() = (r.lent.len() / CHANNELS as usize) as u32;
    }
    0
}

impl Resampler {
    pub(super) fn new(from: f64) -> Result<Resampler, AppAudioError> {
        let (source, target) = (stereo_f32(from), stereo_f32(f64::from(RATE)));
        let mut converter: AudioConverterRef = std::ptr::null_mut();
        // SAFETY: two valid formats and an out pointer.
        let status = unsafe {
            AudioConverterNew(
                NonNull::from(&source),
                NonNull::from(&target),
                NonNull::from(&mut converter),
            )
        };
        if status != 0 || converter.is_null() {
            return Err(server("the sample-rate converter", status));
        }
        Ok(Resampler {
            converter,
            pending: Vec::new(),
            lent: Vec::new(),
        })
    }

    /// `input` (interleaved stereo, whole frames) in, what the converter has ready at 48 kHz out.
    pub(super) fn push(&mut self, input: &[f32]) -> Result<Vec<f32>, AppAudioError> {
        self.pending.extend_from_slice(input);
        let mut out = Vec::new();
        let mut chunk = vec![0f32; 2048];
        loop {
            let capacity = (chunk.len() / CHANNELS as usize) as u32;
            let mut frames = capacity;
            let mut list = AudioBufferList {
                mNumberBuffers: 1,
                mBuffers: [AudioBuffer {
                    mNumberChannels: CHANNELS,
                    mDataByteSize: (chunk.len() * 4) as u32,
                    mData: chunk.as_mut_ptr().cast(),
                }],
            };
            // SAFETY: a live converter; `feed` gets this resampler (no other borrow of it is used during the call) and
            // `list` describes `chunk`, `frames` frames long.
            let status = unsafe {
                AudioConverterFillComplexBuffer(
                    self.converter,
                    Some(feed),
                    std::ptr::from_mut(self).cast(),
                    NonNull::from(&mut frames),
                    NonNull::from(&mut list),
                    std::ptr::null_mut(),
                )
            };
            let got = (frames.min(capacity) as usize) * CHANNELS as usize;
            out.extend_from_slice(&chunk[..got]);
            if status == NO_INPUT || frames == 0 {
                return Ok(out);
            }
            if status != 0 {
                return Err(server("the sample-rate converter", status));
            }
            if out.len() > MAX_CHUNK_BYTES {
                return Err(AppAudioError::Invalid("chunk"));
            }
        }
    }
}

impl Drop for Resampler {
    fn drop(&mut self) {
        // SAFETY: the converter this resampler made; nothing uses it after.
        unsafe { AudioConverterDispose(self.converter) };
    }
}

/// Until `stop`, the process's exit, or an error: the tap's buffers, at 48 kHz, cut into frames.
fn pump(
    pid: u32,
    rx: &mpsc::Receiver<Vec<f32>>,
    rate: f64,
    mut frames: Box<dyn FnMut(Vec<f32>) + Send>,
    stop: &AtomicBool,
) -> Result<(), AppAudioError> {
    let mut resampler = if rate == f64::from(RATE) {
        None
    } else {
        Some(Resampler::new(rate)?)
    };
    let mut cut = SampleFrames::default();
    let mut checked = Instant::now();
    loop {
        if stop.load(Ordering::Acquire) {
            return Ok(());
        }
        match rx.recv_timeout(POLL) {
            Ok(samples) => {
                let samples = match resampler.as_mut() {
                    Some(r) => r.push(&samples)?,
                    None => samples,
                };
                for part in samples.chunks(MAX_CHUNK_BYTES / 4) {
                    cut.push(part, &mut *frames)?;
                }
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {}
            Err(mpsc::RecvTimeoutError::Disconnected) => {
                return Err(AppAudioError::Server("the tap went away".to_owned()));
            }
        }
        if checked.elapsed() >= ALIVE_EVERY {
            checked = Instant::now();
            if !alive(pid) {
                return Ok(());
            }
        }
    }
}

pub(super) fn start(
    source: &AppSource,
    root: u32,
    frames: Box<dyn FnMut(Vec<f32>) + Send>,
    ended: Box<dyn FnOnce() + Send>,
) -> Result<AppCapture, AppAudioError> {
    supported()?;
    let table = parents([source.pid]);
    if in_tree(source.pid, root, |pid| table.get(&pid).copied()) {
        return Err(AppAudioError::Gone);
    }
    let stop = Arc::new(AtomicBool::new(false));
    let (ready_tx, ready_rx) = mpsc::channel();
    let (pid, stop_c) = (source.pid, stop.clone());
    std::thread::Builder::new()
        .name("dilla-app-audio".to_owned())
        .spawn(move || {
            let (tx, rx) = mpsc::sync_channel(QUEUE);
            let (tap, rate) = match open(pid, root, tx) {
                Ok(opened) => opened,
                Err(e) => {
                    let _ = ready_tx.send(Err(e));
                    return;
                }
            };
            let _ = ready_tx.send(Ok(()));
            if let Err(e) = pump(pid, &rx, rate, frames, &stop_c) {
                log::warn!("application audio ended: {e}");
            }
            drop(tap);
            // The process exited or the capture failed: once, unless the handle was dropped.
            if !stop_c.load(Ordering::Acquire) {
                ended();
            }
        })
        .map_err(|e| AppAudioError::Server(e.to_string()))?;
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

#[cfg(test)]
mod tests {
    use super::*;

    /// The strongest frequency of `mono` (48 kHz) between 100 and 2000 Hz, in 5 Hz steps (Goertzel).
    fn peak_hz(mono: &[f32]) -> f64 {
        let power = |hz: f64| {
            let k = 2.0 * (std::f64::consts::TAU * hz / 48_000.0).cos();
            let (mut s1, mut s2) = (0.0f64, 0.0f64);
            for &x in mono {
                let s0 = f64::from(x) + k * s1 - s2;
                s2 = s1;
                s1 = s0;
            }
            s1 * s1 + s2 * s2 - k * s1 * s2
        };
        (20..=400)
            .map(|step| f64::from(step) * 5.0)
            .map(|hz| (hz, power(hz)))
            .fold(
                (0.0, f64::MIN),
                |best, (hz, p)| if p > best.1 { (hz, p) } else { best },
            )
            .0
    }

    /// A 44.1 kHz device: a 440 Hz tone fed in uneven buffers comes out at 48 kHz, still 440 Hz, about 48/44.1 as
    /// many frames, nothing lost between buffers.
    #[test]
    fn a_44_1_khz_tap_is_converted_to_48_khz() {
        let input: Vec<f32> = (0..44_100 * 2)
            .flat_map(|n| {
                let v =
                    (std::f64::consts::TAU * 440.0 * f64::from(n) / 44_100.0).sin() as f32 * 0.5;
                [v, v]
            })
            .collect();
        let mut r = Resampler::new(44_100.0).unwrap();
        let mut out = Vec::new();
        let mut at = 0;
        for len in [882usize, 2, 1024, 512, 4410].iter().cycle() {
            if at >= input.len() {
                break;
            }
            let end = (at + len).min(input.len());
            out.extend(r.push(&input[at..end]).unwrap());
            at = end;
        }
        let frames = out.len() / 2;
        assert!(
            (95_000..=96_000).contains(&frames),
            "two seconds at 48 kHz, less the converter's latency: {frames}"
        );
        let mono: Vec<f32> = out
            .as_chunks::<2>()
            .0
            .iter()
            .map(|[l, r]| (l + r) / 2.0)
            .collect();
        let peak = peak_hz(&mono[24_000..72_000]);
        assert!((peak - 440.0).abs() <= 5.0, "peak at {peak} Hz");
        let max = mono[24_000..72_000]
            .iter()
            .fold(0f32, |m, x| m.max(x.abs()));
        assert!((0.45..=0.55).contains(&max), "level kept: {max}");
    }

    #[test]
    fn tap_buffers_become_interleaved_stereo() {
        let mut inter = [0.1f32, 0.2, 0.3, 0.4, 0.5, 0.6];
        let one = AudioBuffer {
            mNumberChannels: 3,
            mDataByteSize: 24,
            mData: inter.as_mut_ptr().cast(),
        };
        assert_eq!(stereo(&[one]), vec![0.1, 0.2, 0.4, 0.5]);
        let mut mono = [0.7f32, 0.8];
        let m = AudioBuffer {
            mNumberChannels: 1,
            mDataByteSize: 8,
            mData: mono.as_mut_ptr().cast(),
        };
        assert_eq!(stereo(&[m]), vec![0.7, 0.7, 0.8, 0.8]);
        let (mut l, mut r) = ([1.0f32, 2.0], [3.0f32, 4.0]);
        let planar = [
            AudioBuffer {
                mNumberChannels: 1,
                mDataByteSize: 8,
                mData: l.as_mut_ptr().cast(),
            },
            AudioBuffer {
                mNumberChannels: 1,
                mDataByteSize: 8,
                mData: r.as_mut_ptr().cast(),
            },
        ];
        assert_eq!(stereo(&planar), vec![1.0, 3.0, 2.0, 4.0]);
        assert!(stereo(&[]).is_empty());
    }

    /// The kernel's process table: this process runs, its parent is its parent, and a pid that never was is gone.
    #[test]
    fn the_process_table_answers() {
        let me = std::process::id();
        assert!(alive(me));
        assert_eq!(parent(me), Some(std::os::unix::process::parent_id()));
        assert!(!alive(u32::MAX >> 1));
        let table = parents([me]);
        assert!(in_tree(me, me, |p| table.get(&p).copied()));
        assert!(!label(me).is_empty());
    }
}
