//! The platform-independent half of application audio, shared by every capture (Linux, Windows, macOS): frame
//! cutting with carry-over, the finite check and clamp on every sample, Dilla's own process tree left out of a list,
//! and the source list's bounds. Pure: compiled and tested on every OS.

use std::collections::{HashMap, HashSet};

use super::{AppAudioError, AppSource, FRAME_SAMPLES, MAX_CHUNK_BYTES, MAX_LABEL, MAX_SOURCES};

/// One sample, checked finite and clamped to [-1, 1] (beta.5 stops the capture on an invalid sample).
pub(crate) fn checked(v: f32) -> Result<f32, AppAudioError> {
    if v.is_finite() {
        Ok(v.clamp(-1.0, 1.0))
    } else {
        Err(AppAudioError::Invalid("sample"))
    }
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
                frame.push(checked(f32::from_le_bytes(*raw))?);
            }
            emit(frame);
            at += FRAME_BYTES;
        }
        self.tail = bytes[at..].to_vec();
        Ok(())
    }
}

/// [`PcmFrames`] for a capture that hands over f32 samples (WASAPI, CoreAudio): interleaved stereo samples of any count
/// become exact [`FRAME_SAMPLES`]-sample frames, each sample checked finite and clamped; the rest waits for the next
/// buffer (never more than one frame's worth).
#[cfg_attr(not(any(target_os = "windows", target_os = "macos")), allow(dead_code))]
#[derive(Default)]
pub(crate) struct SampleFrames {
    carry: Vec<f32>,
}

#[cfg_attr(not(any(target_os = "windows", target_os = "macos")), allow(dead_code))]
impl SampleFrames {
    pub(crate) fn push(
        &mut self,
        samples: &[f32],
        mut emit: impl FnMut(Vec<f32>),
    ) -> Result<(), AppAudioError> {
        if samples.len() > MAX_CHUNK_BYTES / 4 {
            return Err(AppAudioError::Invalid("chunk"));
        }
        for &v in samples {
            self.carry.push(checked(v)?);
            if self.carry.len() == FRAME_SAMPLES {
                emit(std::mem::replace(
                    &mut self.carry,
                    Vec::with_capacity(FRAME_SAMPLES),
                ));
            }
        }
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

/// `sources` without Dilla's own process tree (`root`), given every process's parent (`parents`: pid to parent pid, a
/// snapshot of the system's process table).
#[cfg_attr(not(any(target_os = "windows", target_os = "macos")), allow(dead_code))]
pub(crate) fn exclude_tree(
    sources: Vec<AppSource>,
    parents: &HashMap<u32, u32>,
    root: u32,
) -> Vec<AppSource> {
    sources
        .into_iter()
        .filter(|s| !in_tree(s.pid, root, |pid| parents.get(&pid).copied()))
        .collect()
}

/// A source's label: at most [`MAX_LABEL`] characters, and a name for a source that gives none.
pub(crate) fn cap_label(label: &str, pid: u32) -> String {
    if label.trim().is_empty() {
        format!("Application {pid}")
    } else {
        label.chars().take(MAX_LABEL).collect()
    }
}

/// beta.5 `parseAudioSources`'s limits: at most [`MAX_SOURCES`], one per id, labels of 1 to [`MAX_LABEL`] characters.
pub fn cap_sources(sources: Vec<AppSource>) -> Vec<AppSource> {
    let mut seen = HashSet::new();
    sources
        .into_iter()
        .filter(|s| s.serial != 0 && s.pid != 0 && seen.insert((s.serial, s.pid)))
        .map(|mut s| {
            s.label = cap_label(&s.label, s.pid);
            s
        })
        .take(MAX_SOURCES)
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn le(samples: &[f32]) -> Vec<u8> {
        samples.iter().flat_map(|s| s.to_le_bytes()).collect()
    }

    fn source(serial: u64, pid: u32, label: &str) -> AppSource {
        AppSource {
            serial,
            pid,
            label: label.to_owned(),
        }
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

    /// A buffer of any length (WASAPI hands over what the period produced) becomes exact 960-sample frames in order,
    /// clamped, the rest carried into the next buffer.
    #[test]
    fn frames_are_cut_to_960_samples() {
        let mut f = SampleFrames::default();
        let mut out: Vec<Vec<f32>> = Vec::new();
        let stream: Vec<f32> = (0..FRAME_SAMPLES * 3 + 7)
            .map(|i| (i % 1000) as f32 / 1000.0)
            .collect();
        let mut fed = 0;
        for len in [1, 441, 959, 2, 1400, 3] {
            f.push(&stream[fed..fed + len], |fr| out.push(fr)).unwrap();
            fed += len;
        }
        f.push(&stream[fed..], |fr| out.push(fr)).unwrap();
        assert_eq!(out.len(), 3);
        assert!(out.iter().all(|fr| fr.len() == FRAME_SAMPLES));
        assert_eq!(
            out.concat(),
            stream[..FRAME_SAMPLES * 3],
            "in order, none lost"
        );
        assert_eq!(f.carry, stream[FRAME_SAMPLES * 3..], "seven samples wait");
        let mut f = SampleFrames::default();
        let mut out = Vec::new();
        let mut loud = vec![0.5f32; FRAME_SAMPLES];
        loud[0] = 4.0;
        loud[1] = -2.0;
        f.push(&loud, |fr| out.push(fr)).unwrap();
        assert_eq!(
            (out[0][0], out[0][1], out[0][2]),
            (1.0, -1.0, 0.5),
            "clamped"
        );
        assert!(f.carry.is_empty());
    }

    #[test]
    fn non_finite_samples_end_the_capture() {
        for bad in [f32::NAN, f32::INFINITY, f32::NEG_INFINITY] {
            assert_eq!(checked(bad), Err(AppAudioError::Invalid("sample")));
            let mut f = SampleFrames::default();
            let mut samples = vec![0.0f32; 10];
            samples[4] = bad;
            assert_eq!(
                f.push(&samples, |_| panic!("no frame")),
                Err(AppAudioError::Invalid("sample"))
            );
        }
        assert_eq!(checked(0.5), Ok(0.5));
        assert_eq!(checked(-9.0), Ok(-1.0));
        let mut p = PcmFrames::default();
        let mut bad = vec![0.0f32; FRAME_SAMPLES];
        bad[5] = f32::NAN;
        assert_eq!(
            p.push(&le(&bad), |_| {}),
            Err(AppAudioError::Invalid("sample"))
        );
        assert_eq!(
            PcmFrames::default().push(&vec![0; MAX_CHUNK_BYTES + 1], |_| {}),
            Err(AppAudioError::Invalid("chunk"))
        );
        assert_eq!(
            SampleFrames::default().push(&vec![0.0; MAX_CHUNK_BYTES / 4 + 1], |_| {}),
            Err(AppAudioError::Invalid("chunk"))
        );
    }

    /// beta.5 `excluded`, from a process-table snapshot: Dilla (100), its children and grandchildren are dropped; another
    /// application, one whose parent is gone, and one in a parent cycle stay.
    #[test]
    fn own_process_tree_is_excluded() {
        let parents: HashMap<u32, u32> = [
            (100, 1),
            (200, 100),
            (300, 200),
            (400, 1),
            (500, 4242),
            (5, 6),
            (6, 5),
        ]
        .into_iter()
        .collect();
        let sessions = vec![
            source(1, 100, "Dilla"),
            source(2, 300, "Dilla's audio child"),
            source(3, 400, "Music"),
            source(4, 500, "Orphan"),
            source(5, 5, "Cycle"),
            source(6, 200, "Dilla helper"),
        ];
        let kept: Vec<u32> = exclude_tree(sessions, &parents, 100)
            .iter()
            .map(|s| s.pid)
            .collect();
        assert_eq!(kept, vec![400, 500, 5]);
    }

    #[test]
    fn labels_are_capped_at_512() {
        let long = "å".repeat(600);
        assert_eq!(cap_label(&long, 7).chars().count(), MAX_LABEL);
        assert_eq!(cap_label(&"x".repeat(MAX_LABEL), 7), "x".repeat(MAX_LABEL));
        assert_eq!(cap_label("Spotify", 7), "Spotify");
        assert_eq!(cap_label("  ", 7), "Application 7");
        assert_eq!(cap_label("", 9), "Application 9");
    }
}
