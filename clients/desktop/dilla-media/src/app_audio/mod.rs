//! Application audio (GPUI parity plan Package 3; beta.5 `native-audio.ts` and `packages/desktop/native/*`, ported
//! in-process): the sound of one chosen application, captured from that application's own playback streams only,
//! never a sink or the system monitor, and never Dilla's own process tree (its call audio). The capture is 48 kHz
//! stereo f32, handed over in 10 ms frames ([`FRAME_SAMPLES`] interleaved samples), each sample checked finite and
//! clamped to [-1, 1] ([`PcmFrames`]). [`crate::Call::publish_app_audio`] publishes it as the screen-audio slot,
//! sealed like every other frame; a sender that cannot be bound to that slot sends nothing.
//!
//! - Linux ([`linux`]): PipeWire. The capture stream (`dilla-application-capture`) does not connect on its own: it is
//!   linked port by port to the chosen process's `Stream/Output/Audio` nodes (and to any it opens later).
//! - macOS (14.2+, CoreAudio process taps) and Windows (WASAPI process loopback): not built yet; [`sources`] answers
//!   [`AppAudioError::Unsupported`] there.

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

#[cfg(target_os = "linux")]
mod linux;

/// 48 kHz, stereo.
pub const RATE: u32 = 48_000;
pub const CHANNELS: u32 = 2;
/// One 10 ms frame: 480 sample frames, interleaved stereo (beta.5 `PCMFrames`: 960 samples).
pub const FRAME_SAMPLES: usize = 960;
/// beta.5 `parseAudioSources`: at most 256 applications, a label of at most 512 characters.
pub const MAX_SOURCES: usize = 256;
pub const MAX_LABEL: usize = 512;
/// beta.5's chunk bound: a larger chunk is refused.
pub const MAX_CHUNK_BYTES: usize = 1024 * 1024;

/// One application that is playing: its first playback stream's serial and its process id (beta.5's `serial:pid`), and
/// the name it gives itself.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub struct AppSource {
    pub serial: u64,
    pub pid: u32,
    pub label: String,
}

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum AppAudioError {
    /// This system has no application capture (macOS and Windows for now).
    #[error("application audio is not available on this system")]
    Unsupported,
    /// The audio server refused or could not be reached.
    #[error("the audio server: {0}")]
    Server(String),
    /// The chosen application is gone.
    #[error("the application is no longer playing")]
    Gone,
    /// A malformed frame or chunk: the capture ends (beta.5 stops on an invalid sample).
    #[error("invalid audio: {0}")]
    Invalid(&'static str),
}

/// The applications that are playing now, Dilla's own process tree left out (`excluded`: this process).
pub fn sources() -> Result<Vec<AppSource>, AppAudioError> {
    #[cfg(target_os = "linux")]
    {
        linux::sources(std::process::id()).map(cap_sources)
    }
    #[cfg(not(target_os = "linux"))]
    {
        Err(AppAudioError::Unsupported)
    }
}

/// Starts capturing `source` (its whole process: every playback stream it has or opens). Each 10 ms frame goes to
/// `frames`; when the application goes away or the server ends the capture, `ended` is called once. Dropping the handle
/// stops the capture.
pub fn start(
    source: &AppSource,
    frames: Box<dyn FnMut(Vec<f32>) + Send>,
    ended: Box<dyn FnOnce() + Send>,
) -> Result<AppCapture, AppAudioError> {
    #[cfg(target_os = "linux")]
    {
        linux::start(source, std::process::id(), frames, ended)
    }
    #[cfg(not(target_os = "linux"))]
    {
        let _ = (source, frames, ended);
        Err(AppAudioError::Unsupported)
    }
}

/// A running application capture. Dropping it stops it (the capture thread ends within 100 ms).
pub struct AppCapture {
    stop: Arc<AtomicBool>,
    label: String,
}

impl AppCapture {
    #[cfg_attr(not(target_os = "linux"), allow(dead_code))]
    pub(crate) fn new(stop: Arc<AtomicBool>, label: String) -> Self {
        Self { stop, label }
    }

    /// The application's name (the tray's "Sharing application sound: {label}").
    pub fn label(&self) -> &str {
        &self.label
    }
}

impl Drop for AppCapture {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
    }
}

/// beta.5 `parseAudioSources`'s limits: at most [`MAX_SOURCES`], one per id, labels of 1 to [`MAX_LABEL`] characters.
pub fn cap_sources(sources: Vec<AppSource>) -> Vec<AppSource> {
    let mut seen = std::collections::HashSet::new();
    sources
        .into_iter()
        .filter(|s| s.serial != 0 && s.pid != 0 && seen.insert((s.serial, s.pid)))
        .map(|mut s| {
            if s.label.trim().is_empty() {
                s.label = format!("Application {}", s.pid);
            }
            if s.label.chars().count() > MAX_LABEL {
                s.label = s.label.chars().take(MAX_LABEL).collect();
            }
            s
        })
        .take(MAX_SOURCES)
        .collect()
}

/// beta.5 `PCMFrames`: assembles little-endian f32 stereo bytes into [`FRAME_SAMPLES`]-sample frames, each sample checked
/// finite and clamped to [-1, 1]. It never keeps more than one frame's worth of bytes between chunks.
#[derive(Default)]
pub struct PcmFrames {
    tail: Vec<u8>,
}

impl PcmFrames {
    pub fn push(
        &mut self,
        chunk: &[u8],
        mut emit: impl FnMut(Vec<f32>),
    ) -> Result<(), AppAudioError> {
        if chunk.len() > MAX_CHUNK_BYTES {
            return Err(AppAudioError::Invalid("chunk"));
        }
        const FRAME_BYTES: usize = FRAME_SAMPLES * 4;
        let mut bytes = std::mem::take(&mut self.tail);
        bytes.extend_from_slice(chunk);
        let mut at = 0;
        while at + FRAME_BYTES <= bytes.len() {
            let mut frame = Vec::with_capacity(FRAME_SAMPLES);
            for raw in bytes[at..at + FRAME_BYTES].as_chunks::<4>().0 {
                let v = f32::from_le_bytes(*raw);
                if !v.is_finite() {
                    return Err(AppAudioError::Invalid("sample"));
                }
                frame.push(v.clamp(-1.0, 1.0));
            }
            emit(frame);
            at += FRAME_BYTES;
        }
        self.tail = bytes[at..].to_vec();
        Ok(())
    }
}

/// Whether `pid` is `root` or one of its descendants (beta.5 `excluded`: the audio service may run in a child of the
/// app, so the whole tree is left out), walking at most 128 parents through `parent_of`.
pub fn in_tree(pid: u32, root: u32, parent_of: impl Fn(u32) -> Option<u32>) -> bool {
    let mut pid = pid;
    for _ in 0..128 {
        if pid <= 1 {
            return false;
        }
        if pid == root {
            return true;
        }
        match parent_of(pid) {
            Some(parent) if parent != pid => pid = parent,
            _ => return false,
        }
    }
    false
}

/// A process's parent from `/proc/<pid>/stat` (the field after the command's closing parenthesis and the state).
pub fn parent_from_stat(stat: &str) -> Option<u32> {
    let end = stat.rfind(')')?;
    let mut fields = stat.get(end + 2..)?.split_whitespace();
    let _state = fields.next()?;
    fields.next()?.parse().ok()
}

/// Little-endian f32 samples to 16-bit PCM (the encoder's input), clamped.
pub fn to_i16(samples: &[f32]) -> Vec<i16> {
    samples
        .iter()
        .map(|s| (s.clamp(-1.0, 1.0) * f32::from(i16::MAX)).round() as i16)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn le(samples: &[f32]) -> Vec<u8> {
        samples.iter().flat_map(|s| s.to_le_bytes()).collect()
    }

    /// audio.test.ts / app-audio-lifetime.test.ts (beta.5 `PCMFrames`): frames are 960 samples, split chunks join, the
    /// samples are clamped, and a partial frame waits for its rest.
    #[test]
    fn pcm_chunks_become_clamped_960_sample_frames() {
        let mut p = PcmFrames::default();
        let mut out = Vec::new();
        let mut samples = vec![0.25f32; FRAME_SAMPLES * 2 + 10];
        samples[1] = 3.0;
        samples[2] = -7.5;
        let bytes = le(&samples);
        // Split mid-sample: the tail carries over.
        p.push(&bytes[..1001], |f| out.push(f)).unwrap();
        assert!(out.is_empty());
        p.push(&bytes[1001..], |f| out.push(f)).unwrap();
        assert_eq!(out.len(), 2);
        assert!(out.iter().all(|f| f.len() == FRAME_SAMPLES));
        assert_eq!((out[0][1], out[0][2]), (1.0, -1.0), "clamped");
        assert_eq!(p.tail.len(), 40, "ten samples wait for their frame");
    }

    #[test]
    fn a_non_finite_sample_or_an_oversized_chunk_ends_the_capture() {
        let mut p = PcmFrames::default();
        let mut bad = vec![0.0f32; FRAME_SAMPLES];
        bad[5] = f32::NAN;
        assert_eq!(
            p.push(&le(&bad), |_| {}),
            Err(AppAudioError::Invalid("sample"))
        );
        let mut p = PcmFrames::default();
        bad[5] = f32::INFINITY;
        assert!(p.push(&le(&bad), |_| {}).is_err());
        assert_eq!(
            PcmFrames::default().push(&vec![0; MAX_CHUNK_BYTES + 1], |_| {}),
            Err(AppAudioError::Invalid("chunk"))
        );
    }

    /// native-audio.ts `parseAudioSources`: one entry per id, a name for every source, at most 256, labels at most 512.
    #[test]
    fn the_source_list_is_bounded_and_named() {
        let s = |serial, pid, label: &str| AppSource {
            serial,
            pid,
            label: label.to_owned(),
        };
        let long = "x".repeat(600);
        let out = cap_sources(vec![
            s(1, 10, "Music"),
            s(1, 10, "Music again"),
            s(2, 11, " "),
            s(3, 12, &long),
            s(0, 13, "no serial"),
            s(4, 0, "no pid"),
        ]);
        assert_eq!(out.len(), 3);
        assert_eq!(out[1].label, "Application 11");
        assert_eq!(out[2].label.chars().count(), MAX_LABEL);
        let many: Vec<AppSource> = (1..400).map(|i| s(i, i as u32, "a")).collect();
        assert_eq!(cap_sources(many).len(), MAX_SOURCES);
    }

    /// beta.5 `excluded`: Dilla's whole process tree is left out, and only it.
    #[test]
    fn dillas_process_tree_is_excluded() {
        // 1 <- 100 (dilla) <- 200 (its audio child) <- 300; 400 is another app under 1.
        let parent = |pid| match pid {
            100 => Some(1),
            200 => Some(100),
            300 => Some(200),
            400 => Some(1),
            _ => None,
        };
        assert!(in_tree(100, 100, parent));
        assert!(in_tree(300, 100, parent));
        assert!(!in_tree(400, 100, parent));
        assert!(!in_tree(1, 100, parent));
        let cycle = |pid| Some(if pid == 5 { 6 } else { 5 });
        assert!(!in_tree(5, 100, cycle), "a cycle ends the walk");
        assert_eq!(
            parent_from_stat("4242 (pw (play)) S 4100 4242 4100 0"),
            Some(4100)
        );
        assert_eq!(parent_from_stat("garbage"), None);
        assert_eq!(
            to_i16(&[1.0, -1.0, 0.0, 2.0]),
            vec![32767, -32767, 0, 32767]
        );
    }
}
