//! Screens and windows on macOS for Dilla's own share picker (GPUI gaps Task 45, D22), through webrtc-sys's
//! `dilla_screen` bridge (`dilla_screen_mac.mm`): `SCShareableContent` lists them with each window's owning process,
//! `SCScreenshotManager` takes the thumbnails on macOS 14+ (one captured frame each on 13, as X11 and Windows do), and
//! an own `SCStream` captures the chosen one into the same [`super::desktop::DesktopCapture`].
//!
//! Screen Recording: listing needs it, and without it nothing touches ScreenCaptureKit (which would prompt): the
//! listing is "denied". Only [`request_access`] (the picker's open) asks the system, once per process.
//!
//! Logs say what failed, never a title, an application name or a pid: they describe the person's desktop.

use std::sync::atomic::{AtomicBool, AtomicU32, Ordering};
use std::time::Duration;

use webrtc_sys::dilla_screen::{BgraView, DillaScreenSink, ffi};

use super::Latest;
use super::desktop::{DesktopSource, Snapshot, SourceKind, fit_rgba, thumbnail};
use super::mac_list::{MacRecord, to_sources};
use crate::capture_source::{Frame, PixelFormat};

/// How often the capture thread looks at its stop flag and the asked frame rate.
const POLL: Duration = Duration::from_millis(20);

/// The picker's open: asks for Screen Recording the first time in this process (the system's prompt follows the
/// person's press), and answers whether it is granted now.
pub(super) fn request_access() -> bool {
    static ASKED: AtomicBool = AtomicBool::new(false);
    ffi::dilla_screen_access(!ASKED.swap(true, Ordering::AcqRel))
}

/// The listing, mapped and filtered; `None` when the system refused it (no Screen Recording).
fn sources() -> Option<Vec<DesktopSource>> {
    let listed = ffi::dilla_screen_list();
    match listed.access {
        ffi::DillaScreenAccess::Granted => {}
        ffi::DillaScreenAccess::Denied => {
            log::info!("dilla-media: no Screen Recording permission: nothing listed");
            return None;
        }
        _ => {
            log::warn!("dilla-media: ScreenCaptureKit gave no list (an error or no answer in 5 s)");
            return Some(Vec::new());
        }
    }
    let records: Vec<MacRecord> = listed
        .records
        .into_iter()
        .map(|r| MacRecord {
            window: r.is_window,
            id: r.id,
            owner_pid: r.owner_pid,
            bundle_id: r.bundle_id,
            app_name: r.app_name,
            title: r.title,
            layer: r.layer,
            width: r.width,
            height: r.height,
            main: r.main,
        })
        .collect();
    let sources = to_sources(&records, std::process::id());
    log::debug!(
        "dilla-media: {} of {} listed screens and windows offered",
        sources.len(),
        records.len()
    );
    Some(sources)
}

/// The screens or the windows that can be shared now (empty without Screen Recording).
pub(super) fn list(kind: SourceKind) -> Vec<DesktopSource> {
    sources()
        .unwrap_or_default()
        .into_iter()
        .filter(|s| s.kind == kind)
        .collect()
}

/// The picker's enumeration: the offered sources, at most `max`, with a thumbnail each.
pub(super) fn snapshot(max: usize, max_w: u32, max_h: u32) -> Snapshot {
    let Some(mut offered) = sources() else {
        return Snapshot {
            sources: Vec::new(),
            denied: true,
        };
    };
    offered.truncate(max);
    let mut thumbnails: Vec<Option<Frame>> = vec![None; offered.len()];
    if objc2::available!(macos = 14.0) {
        let ids = |kind| -> Vec<u64> {
            offered
                .iter()
                .filter(|s| s.kind == kind)
                .map(|s| s.id)
                .collect()
        };
        let taken = ffi::dilla_screen_thumbnails(
            &ids(SourceKind::Screen),
            &ids(SourceKind::Window),
            max_w,
            max_h,
        );
        for t in taken {
            let kind = if t.is_window {
                SourceKind::Window
            } else {
                SourceKind::Screen
            };
            let Some(i) = offered.iter().position(|s| s.kind == kind && s.id == t.id) else {
                continue;
            };
            let frame = Frame {
                width: t.width,
                height: t.height,
                format: PixelFormat::Bgra,
                stride: t.width * 4,
                data: t.bgra,
            };
            if frame.is_whole() {
                thumbnails[i] = Some(fit_rgba(&frame, max_w, max_h));
            }
        }
        let missing = thumbnails.iter().filter(|t| t.is_none()).count();
        if missing > 0 {
            log::debug!("dilla-media: {missing} sources gave no thumbnail in time");
        }
    } else {
        // macOS 13 has no SCScreenshotManager: one captured frame each, as X11 and Windows take theirs.
        for (slot, source) in thumbnails.iter_mut().zip(&offered) {
            *slot = thumbnail(source, max_w, max_h);
        }
    }
    Snapshot {
        sources: offered.into_iter().zip(thumbnails).collect(),
        denied: false,
    }
}

/// `width` x `height` BGRA rows at `stride`, packed without their padding.
fn pack(v: &BgraView<'_>) -> Option<Frame> {
    let (w, h, stride) = (v.width as usize, v.height as usize, v.stride as usize);
    let row = w * 4;
    if w == 0 || h == 0 || stride < row {
        return None;
    }
    let mut data = Vec::with_capacity(row * h);
    for y in 0..h {
        data.extend_from_slice(v.bgra.get(y * stride..y * stride + row)?);
    }
    Some(Frame {
        width: v.width,
        height: v.height,
        format: PixelFormat::Bgra,
        stride: v.width * 4,
        data,
    })
}

/// The `dilla-screen` thread of a macOS capture: opens the stream (re-checking a window's owner), keeps its frame rate
/// on the asked one, and drops it (stopping the stream) when told to stop or when the source ended.
pub(super) fn run(source: &DesktopSource, latest: &Latest, stop: &AtomicBool, fps: &AtomicU32) {
    let (frames, ended) = (latest.clone(), latest.clone());
    let sink = DillaScreenSink::new(
        move |v| {
            if let Some(f) = pack(&v) {
                frames.put(f);
            }
        },
        move || ended.end(),
    );
    let mut current = fps.load(Ordering::Relaxed).max(1);
    let expect_pid = source
        .owner
        .and_then(|pid| i32::try_from(pid).ok())
        .unwrap_or(0);
    let capture = ffi::dilla_screen_open(
        source.kind == SourceKind::Window,
        source.id,
        expect_pid,
        current,
        true,
        sink,
    );
    if capture.is_null() {
        log::warn!(
            "dilla-media: the {:?} did not open (gone, no Screen Recording, or no start in 5 s)",
            source.kind
        );
        latest.end();
        return;
    }
    while !stop.load(Ordering::Acquire) && !latest.ended() {
        let asked = fps.load(Ordering::Relaxed).max(1);
        if asked != current {
            capture.set_fps(asked);
            current = asked;
        }
        std::thread::sleep(POLL);
    }
    // Stops the stream and drains its queue before it returns: no frame arrives after this.
    drop(capture);
    latest.end();
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_frame_is_packed_without_its_row_padding() {
        // 2 x 2 BGRA with 4 bytes of padding per row.
        let bgra = [
            1, 2, 3, 4, 5, 6, 7, 8, 0, 0, 0, 0, //
            9, 10, 11, 12, 13, 14, 15, 16, 0, 0, 0, 0,
        ];
        let f = pack(&BgraView {
            width: 2,
            height: 2,
            stride: 12,
            bgra: &bgra,
        })
        .expect("a frame");
        assert_eq!((f.width, f.height, f.stride), (2, 2, 8));
        assert_eq!(f.data, (1..=16).collect::<Vec<u8>>());
        assert!(
            pack(&BgraView {
                width: 4,
                height: 2,
                stride: 12,
                bgra: &bgra,
            })
            .is_none(),
            "a stride shorter than a row is refused"
        );
    }
}
